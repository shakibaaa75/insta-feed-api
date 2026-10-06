package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"igsync/internal/config"
	"igsync/internal/scraper"
	"igsync/internal/store"
)

func main() {
	cfg := config.Load()

	if cfg.MongoURI == "" {
		log.Fatal("set MONGODB_URI (see .env.example)")
	}

	if cfg.APIKey == "" {
		log.Fatal("set API_KEY so the API is not open to everyone")
	}

	log.Printf("config: PUBLIC_URL=%q profile=%q db=%q addr=%q",
		cfg.PublicURL, cfg.Profile, cfg.DB, cfg.Addr)
	if cfg.PublicURL == "" {
		log.Printf("WARNING: PUBLIC_URL is empty — stored images will not be " +
			"served from your own domain, and /posts will keep returning " +
			"Instagram CDN links that expire and 403.")
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		20*time.Second,
	)

	st, err := store.New(
		ctx,
		cfg.MongoURI,
		cfg.DB,
	)

	cancel()

	if err != nil {
		log.Fatalf("mongodb: %v", err)
	}

	defer st.Close(context.Background())

	mux := http.NewServeMux()

	// ------------------------------------------------------------
	// Image routes (MUST be registered before server starts)
	//
	//   GET /img/{shortcode}  — public, no API key needed
	//   PUT /img/{shortcode}  — needs API key (extension uploads)
	// ------------------------------------------------------------

	registerImageRoutes(mux, cfg.APIKey, cfg.PublicURL, st)

	// ------------------------------------------------------------
	// Health
	// ------------------------------------------------------------

	mux.HandleFunc("GET /healthz", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		writeJSON(
			w,
			http.StatusOK,
			map[string]string{
				"ok": "true",
			},
		)
	})

	// ------------------------------------------------------------
	// GET /posts?profile=name&limit=50
	//
	// FIX: imageUrl is rewritten at read time. Any post whose image
	// is present in GridFS is served from PUBLIC_URL/img/<shortcode>,
	// regardless of what string is stored in MongoDB. This is what
	// stops Instagram's expiring CDN links from leaking out to
	// WordPress and causing 403s.
	// ------------------------------------------------------------

	mux.HandleFunc(
		"GET /posts",
		auth(cfg.APIKey, func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			limit := int64(50)

			if v, err := strconv.Atoi(
				r.URL.Query().Get("limit"),
			); err == nil && v > 0 {
				limit = int64(min(v, 500))
			}

			profile := config.Username(
				r.URL.Query().Get("profile"),
			)

			if profile == "" {
				profile = cfg.Profile
			}

			log.Printf("[posts] request profile=%q limit=%d publicURL=%q",
				profile, limit, cfg.PublicURL)

			posts, err := st.List(
				r.Context(),
				profile,
				limit,
			)

			if err != nil {
				log.Printf("[posts] db error: %v", err)
				writeJSON(
					w,
					http.StatusInternalServerError,
					map[string]string{
						"error": "database error",
					},
				)
				return
			}

			// ---- READ-TIME URL REWRITE ----
			rewritten := 0
			for i := range posts {
				stored := posts[i].ImageURL
				if st.HasImage(r.Context(), posts[i].Shortcode) {
					if cfg.PublicURL != "" {
						posts[i].ImageURL = cfg.PublicURL + "/img/" + posts[i].Shortcode
						rewritten++
						log.Printf("[posts]   %s: has image, imageUrl %q -> %q",
							posts[i].Shortcode, shorten(stored), posts[i].ImageURL)
					} else {
						log.Printf("[posts]   %s: has image but PUBLIC_URL is empty — "+
							"leaving imageUrl as %q (will 403 when it expires)",
							posts[i].Shortcode, shorten(stored))
					}
				} else {
					log.Printf("[posts]   %s: no stored image, leaving imageUrl as %q",
						posts[i].Shortcode, shorten(stored))
				}
			}
			log.Printf("[posts] returning %d posts (%d rewritten to %s/img/...)",
				len(posts), rewritten, cfg.PublicURL)

			writeJSON(
				w,
				http.StatusOK,
				map[string]any{
					"posts": posts,
				},
			)
		}),
	)

	// ------------------------------------------------------------
	// GET /status
	// ------------------------------------------------------------

	mux.HandleFunc(
		"GET /status",
		auth(cfg.APIKey, func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			s, err := st.Statuses(
				r.Context(),
			)

			if err != nil {
				writeJSON(
					w,
					http.StatusInternalServerError,
					map[string]string{
						"error": "database error",
					},
				)
				return
			}

			writeJSON(
				w,
				http.StatusOK,
				map[string]any{
					"status": s,
				},
			)
		}),
	)

	// ------------------------------------------------------------
	// POST /ingest
	// ------------------------------------------------------------

	mux.HandleFunc(
		"POST /ingest",
		auth(cfg.APIKey, func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			var body struct {
				Profile string `json:"profile"`

				Posts []struct {
					Shortcode string `json:"shortcode"`
					URL       string `json:"url"`
					ImageURL  string `json:"imageUrl"`
				} `json:"posts"`

				// Backward compatibility.
				Links []string `json:"links"`

				Error string `json:"error"`
			}

			r.Body = http.MaxBytesReader(
				w,
				r.Body,
				2<<20,
			)

			if err := json.NewDecoder(
				r.Body,
			).Decode(&body); err != nil {
				log.Printf("[ingest] bad JSON: %v", err)
				writeJSON(
					w,
					http.StatusBadRequest,
					map[string]string{
						"error": "bad JSON",
					},
				)
				return
			}

			profile := config.Username(
				body.Profile,
			)

			if profile == "" {
				profile = cfg.Profile
			}

			if profile == "" {
				writeJSON(
					w,
					http.StatusBadRequest,
					map[string]string{
						"error": "profile is required",
					},
				)
				return
			}

			log.Printf("[ingest] start profile=%q posts=%d links=%d err=%q",
				profile, len(body.Posts), len(body.Links), body.Error)

			// Extension reports an error.
			if body.Error != "" {
				_ = st.SaveStatus(
					r.Context(),
					profile,
					0,
					errors.New(body.Error),
				)

				writeJSON(
					w,
					http.StatusOK,
					map[string]int{
						"found": 0,
						"added": 0,
					},
				)

				return
			}

			// ----------------------------------------------------
			// New posts format
			// ----------------------------------------------------

			if len(body.Posts) > 0 {
				now := time.Now().UTC()

				posts := make(
					[]store.Post,
					0,
					len(body.Posts),
				)

				for i, p := range body.Posts {
					shortcode := strings.TrimSpace(
						p.Shortcode,
					)

					// If shortcode wasn't supplied,
					// extract it from the URL.
					if shortcode == "" {
						codes := scraper.ExtractShortcodes(
							p.URL,
						)

						if len(codes) > 0 {
							shortcode = codes[0]
						}
					}

					if shortcode == "" {
						log.Printf("[ingest]   skip post with no shortcode (url=%q)",
							shorten(p.URL))
						continue
					}

					postURL := strings.TrimSpace(
						p.URL,
					)

					if postURL == "" {
						postURL = scraper.PostURL(
							shortcode,
						)
					}

					posts = append(
						posts,
						store.Post{
							Shortcode: shortcode,
							Profile:   profile,
							URL:       postURL,
							ImageURL: strings.TrimSpace(
								p.ImageURL,
							),
							PostedAt: now.Add(
								-time.Duration(i) *
									time.Second,
							),
						},
					)
				}

				log.Printf("[ingest] prepared %d posts, calling prepareImages (publicURL=%q)",
					len(posts), cfg.PublicURL)

				// For posts we already have, this just fixes the URL.
				// For new posts, Instagram will likely return 403
				// (their CDN blocks server-side requests), so the
				// extension uploads the image via PUT /img/{shortcode}
				// right after this /ingest call returns.
				prepareImages(
					r.Context(),
					st,
					cfg.PublicURL,
					posts,
				)

				added, err := st.AddPosts(
					r.Context(),
					posts,
				)

				_ = st.SaveStatus(
					r.Context(),
					profile,
					added,
					err,
				)

				if err != nil {
					log.Printf("[ingest] db error: %v", err)
					writeJSON(
						w,
						http.StatusInternalServerError,
						map[string]string{
							"error": "database error",
						},
					)
					return
				}

				log.Printf("[ingest] done found=%d added=%d", len(posts), added)

				writeJSON(
					w,
					http.StatusOK,
					map[string]int{
						"found": len(posts),
						"added": added,
					},
				)

				return
			}

			// ----------------------------------------------------
			// Old links format
			// ----------------------------------------------------

			codes := scraper.ExtractShortcodes(
				strings.Join(
					body.Links,
					"\n",
				),
			)

			now := time.Now().UTC()

			posts := make(
				[]store.Post,
				0,
				len(codes),
			)

			for i, c := range codes {
				posts = append(
					posts,
					store.Post{
						Shortcode: c,
						Profile:   profile,
						URL:       scraper.PostURL(c),
						PostedAt: now.Add(
							-time.Duration(i) *
								time.Second,
						),
					},
				)
			}

			added, err := st.AddNew(
				r.Context(),
				posts,
			)

			_ = st.SaveStatus(
				r.Context(),
				profile,
				added,
				err,
			)

			if err != nil {
				log.Printf("[ingest] db error: %v", err)
				writeJSON(
					w,
					http.StatusInternalServerError,
					map[string]string{
						"error": "database error",
					},
				)
				return
			}

			log.Printf("[ingest] done (links) found=%d added=%d", len(codes), added)

			writeJSON(
				w,
				http.StatusOK,
				map[string]int{
					"found": len(codes),
					"added": added,
				},
			)
		}),
	)

	// ------------------------------------------------------------

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf(
		"API listening on %s",
		cfg.Addr,
	)

	log.Fatal(
		srv.ListenAndServe(),
	)
}

// ------------------------------------------------------------
// API key authentication
// ------------------------------------------------------------

func auth(
	key string,
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		got := r.Header.Get(
			"X-API-Key",
		)

		if subtle.ConstantTimeCompare(
			[]byte(got),
			[]byte(key),
		) != 1 {
			log.Printf("[auth] rejected %s %s (bad API key)",
				r.Method, r.URL.Path)
			writeJSON(
				w,
				http.StatusUnauthorized,
				map[string]string{
					"error": "unauthorized",
				},
			)
			return
		}

		next(w, r)
	}
}

// ------------------------------------------------------------
// JSON helper
// ------------------------------------------------------------

func writeJSON(
	w http.ResponseWriter,
	code int,
	v any,
) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(code)

	_ = json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------
// shorten trims long CDN URLs for readable logs.
// ------------------------------------------------------------

func shorten(s string) string {
	if len(s) <= 70 {
		return s
	}
	return s[:70] + "..."
}
