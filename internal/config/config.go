package config

import (
	"net/url"
	"os"
	"strings"
)

type Config struct {
	MongoURI string
	DB       string
	Profile  string // Instagram username (no @, no URL)
	Proxy    string // optional, e.g. http://user:pass@host:port or socks5://host:port
	APIKey   string
	Addr     string

	// PublicURL is the public address of this API, e.g.
	// https://insta-feed-2.onrender.com (no trailing slash).
	// Stored images are served from PublicURL + "/img/<shortcode>".
	PublicURL string
}

func Load() Config {
	loadDotEnv(".env")
	return Config{
		MongoURI: os.Getenv("MONGODB_URI"),
		DB:       getenv("MONGODB_DB", "igsync"),
		Profile:  Username(os.Getenv("IG_PROFILE")),
		Proxy:    os.Getenv("IG_PROXY"),
		APIKey:   os.Getenv("API_KEY"),
		Addr:     getenv("ADDR", ":8080"),

		PublicURL: strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
	}
}

// Username accepts "name", "@name" or a full profile URL and returns "name".
func Username(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "instagram.com") {
		if !strings.HasPrefix(s, "http") {
			s = "https://" + s
		}
		if u, err := url.Parse(s); err == nil {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) > 0 {
				return parts[0]
			}
		}
	}
	return strings.Trim(strings.TrimPrefix(s, "@"), "/")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// loadDotEnv reads KEY=VALUE lines into the environment (existing env vars win).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
