package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"igsync/internal/store"
)

var codeRe = regexp.MustCompile(`instagram\.com/(?:[A-Za-z0-9_.]+/)?(?:p|reel|tv)/([A-Za-z0-9_-]+)`)

// ExtractShortcodes pulls unique post codes out of any text containing Instagram
// post/reel URLs, keeping the order they appear in.
func ExtractShortcodes(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range codeRe.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func PostURL(code string) string {
	return "https://www.instagram.com/p/" + code + "/"
}

type profileResp struct {
	Data struct {
		User struct {
			Media struct {
				Edges []struct {
					Node struct {
						Shortcode string `json:"shortcode"`
						TakenAt   int64  `json:"taken_at_timestamp"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"edge_owner_to_timeline_media"`
		} `json:"user"`
	} `json:"data"`
}

// FetchLatest returns the most recent posts (usually ~12) of a public profile.
// It makes ONE request. If Instagram blocks it, an error is returned and nothing
// else happens, so stored posts are never affected.
func FetchLatest(ctx context.Context, username, proxy string) ([]store.Post, error) {
	tr := &http.Transport{}
	if proxy != "" {
		pu, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("bad IG_PROXY: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	endpoint := "https://i.instagram.com/api/v1/users/web_profile_info/?username=" + url.QueryEscape(username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-ig-app-id", "936619743392459")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instagram returned HTTP %d (blocked or rate limited)", resp.StatusCode)
	}

	var r profileResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New("response was not valid JSON (probably a login wall)")
	}
	edges := r.Data.User.Media.Edges
	if len(edges) == 0 {
		return nil, errors.New("no posts returned (private profile, wrong username, or blocked)")
	}

	posts := make([]store.Post, 0, len(edges))
	for _, e := range edges {
		if e.Node.Shortcode == "" {
			continue
		}
		posts = append(posts, store.Post{
			Shortcode: e.Node.Shortcode,
			Profile:   username,
			URL:       PostURL(e.Node.Shortcode),
			PostedAt:  time.Unix(e.Node.TakenAt, 0).UTC(),
		})
	}
	return posts, nil
}
