package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"igsync/internal/store"
)

// ------------------------------------------------------------
// Settings
// ------------------------------------------------------------

const (
	// Largest image we accept or download.
	maxImageBytes = 8 << 20

	// At most this many images are downloaded per /ingest call, so a
	// big first import cannot time out. Posts that were skipped keep
	// their Instagram link and are downloaded on the next ingest.
	maxDownloadsPerCall = 40

	// Parallel downloads.
	downloadWorkers = 4
)

var shortcodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{5,40}$`)

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

// imageURLFor builds the permanent public URL of a stored image.
func imageURLFor(publicURL, shortcode string) string {
	return publicURL + "/img/" + shortcode
}

// Only Instagram's own image hosts may be downloaded.
func allowedImageHost(host string) bool {

	host = strings.ToLower(host)

	return strings.HasSuffix(host, ".cdninstagram.com") ||
		strings.HasSuffix(host, ".fbcdn.net")
}

// ------------------------------------------------------------
// fetchImage
//
// Downloads one image from Instagram's CDN while its link is
// still fresh.
// ------------------------------------------------------------

func fetchImage(
	ctx context.Context,
	rawURL string,
) ([]byte, error) {

	u, err := url.Parse(rawURL)

	if err != nil ||
		u.Scheme != "https" ||
		!allowedImageHost(u.Hostname()) {

		return nil, errors.New("not an Instagram image URL")
	}

	ctx, cancel := context.WithTimeout(
		ctx,
		20*time.Second,
	)

	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		rawURL,
		nil,
	)

	if err != nil {
		return nil, err
	}

	req.Header.Set(
		"User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36",
	)

	req.Header.Set(
		"Accept",
		"image/avif,image/webp,image/apng,image/*,*/*;q=0.8",
	)

	client := &http.Client{
		CheckRedirect: func(
			r *http.Request,
			via []*http.Request,
		) error {

			if len(via) >= 5 {
				return errors.New("too many redirects")
			}

			if !allowedImageHost(r.URL.Hostname()) {
				return errors.New("redirect to a disallowed host")
			}

			return nil
		},
	}

	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"instagram returned HTTP %d",
			resp.StatusCode,
		)
	}

	data, err := io.ReadAll(
		io.LimitReader(resp.Body, maxImageBytes+1),
	)

	if err != nil {
		return nil, err
	}

	if len(data) == 0 || len(data) > maxImageBytes {
		return nil, errors.New("image is empty or too large")
	}

	if !strings.HasPrefix(
		http.DetectContentType(data),
		"image/",
	) {
		return nil, errors.New("response was not an image")
	}

	return data, nil
}

// ------------------------------------------------------------
// prepareImages
//
// Call this on the posts from /ingest BEFORE saving them.
//
// For every post that has an Instagram image link:
//   - if we already hold a copy, imageUrl becomes our own URL
//   - otherwise the image is downloaded now (the link is fresh),
//     stored in MongoDB, and imageUrl becomes our own URL
//   - if the download fails, the Instagram link is kept as is
//     (it may stop working later) and the failure is logged
// ------------------------------------------------------------

func prepareImages(
	ctx context.Context,
	st *store.Store,
	publicURL string,
	posts []store.Post,
) {

	if publicURL == "" {
		log.Printf("[prepareImages] PUBLIC_URL is empty — skipping (%d posts)",
			len(posts))
		return
	}

	log.Printf("[prepareImages] start: %d posts, publicURL=%q", len(posts), publicURL)

	var wg sync.WaitGroup

	sem := make(chan struct{}, downloadWorkers)

	downloads := 0

	for i := range posts {

		p := &posts[i]

		if p.ImageURL == "" {
			log.Printf("[prepareImages]   %s: no imageUrl supplied — skipping",
				p.Shortcode)
			continue
		}

		ourURL := imageURLFor(publicURL, p.Shortcode)

		// Already stored: just point at our copy.
		if st.HasImage(ctx, p.Shortcode) {
			p.ImageURL = ourURL
			log.Printf("[prepareImages]   %s: already stored -> %s",
				p.Shortcode, ourURL)
			continue
		}

		// Over the per-call limit: keep the Instagram link for now.
		if downloads >= maxDownloadsPerCall {
			log.Printf("[prepareImages]   %s: hit maxDownloadsPerCall (%d) — deferring",
				p.Shortcode, maxDownloadsPerCall)
			continue
		}

		downloads++

		wg.Add(1)
		sem <- struct{}{}

		go func(p *store.Post, ourURL string) {

			defer wg.Done()
			defer func() { <-sem }()

			log.Printf("[prepareImages]   %s: downloading %s",
				p.Shortcode, shorten(p.ImageURL))

			data, err := fetchImage(ctx, p.ImageURL)

			if err != nil {
				log.Printf(
					"[prepareImages]   %s: DOWNLOAD FAILED: %v (keeping Instagram URL)",
					p.Shortcode,
					err,
				)
				return
			}

			log.Printf("[prepareImages]   %s: downloaded %d bytes, saving to GridFS",
				p.Shortcode, len(data))

			if err := st.SaveImage(
				ctx,
				p.Shortcode,
				data,
			); err != nil {
				log.Printf(
					"[prepareImages]   %s: SAVE FAILED: %v",
					p.Shortcode,
					err,
				)
				return
			}

			log.Printf("[prepareImages]   %s: saved -> %s", p.Shortcode, ourURL)
			p.ImageURL = ourURL

		}(p, ourURL)
	}

	wg.Wait()

	log.Printf("[prepareImages] done: %d downloads attempted", downloads)
}

// ------------------------------------------------------------
// Routes
//
//	GET /img/{shortcode}   public: serves the stored image
//	PUT /img/{shortcode}   needs X-API-Key: upload image bytes
//	                       (for when Go cannot download from
//	                       Instagram and the extension must
//	                       send the file instead)
// ------------------------------------------------------------

func registerImageRoutes(
	mux *http.ServeMux,
	apiKey string,
	publicURL string,
	st *store.Store,
) {

	mux.HandleFunc(
		"GET /img/{shortcode}",
		func(w http.ResponseWriter, r *http.Request) {

			code := strings.TrimSuffix(
				r.PathValue("shortcode"),
				".jpg",
			)

			if !shortcodeRe.MatchString(code) {
				log.Printf("[img] GET %s: bad shortcode", code)
				http.NotFound(w, r)
				return
			}

			data, err := st.ReadImage(r.Context(), code)

			if errors.Is(err, store.ErrNoImage) {
				log.Printf("[img] GET %s: 404 (not stored)", code)
				http.NotFound(w, r)
				return
			}

			if err != nil {
				log.Printf("[img] GET %s: db error: %v", code, err)
				http.Error(
					w,
					"database error",
					http.StatusInternalServerError,
				)
				return
			}

			log.Printf("[img] GET %s: served %d bytes", code, len(data))

			w.Header().Set(
				"Content-Type",
				http.DetectContentType(data),
			)

			w.Header().Set(
				"Content-Length",
				strconv.Itoa(len(data)),
			)

			w.Header().Set(
				"Cache-Control",
				"public, max-age=86400",
			)

			w.Header().Set(
				"Cross-Origin-Resource-Policy",
				"cross-origin",
			)

			_, _ = w.Write(data)
		},
	)

	mux.HandleFunc(
		"PUT /img/{shortcode}",
		auth(apiKey, func(
			w http.ResponseWriter,
			r *http.Request,
		) {

			code := strings.TrimSuffix(
				r.PathValue("shortcode"),
				".jpg",
			)

			if !shortcodeRe.MatchString(code) {
				log.Printf("[img] PUT %s: bad shortcode", code)
				writeJSON(
					w,
					http.StatusBadRequest,
					map[string]string{
						"error": "bad shortcode",
					},
				)
				return
			}

			r.Body = http.MaxBytesReader(
				w,
				r.Body,
				maxImageBytes,
			)

			data, err := io.ReadAll(r.Body)

			if err != nil || len(data) == 0 {
				log.Printf("[img] PUT %s: missing or too large body: %v",
					code, err)
				writeJSON(
					w,
					http.StatusBadRequest,
					map[string]string{
						"error": "missing or too large image",
					},
				)
				return
			}

			if !strings.HasPrefix(
				http.DetectContentType(data),
				"image/",
			) {
				log.Printf("[img] PUT %s: body is not an image (%d bytes)",
					code, len(data))
				writeJSON(
					w,
					http.StatusBadRequest,
					map[string]string{
						"error": "body is not an image",
					},
				)
				return
			}

			log.Printf("[img] PUT %s: received %d bytes from extension",
				code, len(data))

			if err := st.SaveImage(
				r.Context(),
				code,
				data,
			); err != nil {
				log.Printf("[img] PUT %s: SAVE FAILED: %v", code, err)
				writeJSON(
					w,
					http.StatusInternalServerError,
					map[string]string{
						"error": "database error",
					},
				)
				return
			}

			log.Printf("[img] PUT %s: saved to GridFS", code)

			// Point the post at the stored copy.
			if publicURL != "" {
				u := imageURLFor(publicURL, code)
				if err := st.SetImageURL(
					r.Context(),
					code,
					u,
				); err != nil {
					log.Printf("[img] PUT %s: SetImageURL failed: %v",
						code, err)
				} else {
					log.Printf("[img] PUT %s: imageUrl set to %s", code, u)
				}
			}

			writeJSON(
				w,
				http.StatusOK,
				map[string]string{
					"ok": "true",
				},
			)
		}),
	)
}
