package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"igsync/internal/config"
	"igsync/internal/scraper"
	"igsync/internal/store"
)

// Usage:
//
//	worker            fetch the latest posts of IG_PROFILE and save the new ones
//	worker run        same as above
//	worker import f   read Instagram post links from file f (first full import)
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg := config.Load()
	if cfg.MongoURI == "" || cfg.Profile == "" {
		return fmt.Errorf("set MONGODB_URI and IG_PROFILE (see .env.example)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st, err := store.New(ctx, cfg.MongoURI, cfg.DB)
	if err != nil {
		return fmt.Errorf("mongodb: %w", err)
	}
	defer st.Close(ctx)

	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "run":
		posts, fetchErr := scraper.FetchLatest(ctx, cfg.Profile, cfg.Proxy)
		if fetchErr != nil {
			_ = st.SaveStatus(ctx, cfg.Profile, 0, fetchErr)
			return fmt.Errorf("fetch failed (saved posts are untouched): %w", fetchErr)
		}
		added, err := st.AddPosts(ctx, posts)
		_ = st.SaveStatus(ctx, cfg.Profile, added, err)
		if err != nil {
			return err
		}
		log.Printf("%s: fetched %d posts, %d new", cfg.Profile, len(posts), added)

	case "import":
		if len(os.Args) < 3 {
			return fmt.Errorf("usage: worker import links.txt")
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			return err
		}
		codes := scraper.ExtractShortcodes(string(data))
		if len(codes) == 0 {
			return fmt.Errorf("no Instagram post links found in %s", os.Args[2])
		}
		// Links pasted from the profile page are newest first and carry no date.
		// Give them a fake early date that keeps that order; posts the scraper
		// later sees get their real date.
		base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		posts := make([]store.Post, 0, len(codes))
		for i, c := range codes {
			posts = append(posts, store.Post{
				Shortcode: c,
				Profile:   cfg.Profile,
				URL:       scraper.PostURL(c),
				PostedAt:  base.Add(time.Duration(len(codes)-i) * time.Second),
			})
		}
		added, err := st.AddPosts(ctx, posts)
		if err != nil {
			return err
		}
		log.Printf("%s: found %d links, %d new", cfg.Profile, len(codes), added)

	default:
		return fmt.Errorf("unknown command %q (use: run | import <file>)", cmd)
	}
	return nil
}
