package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is resolved from environment variables, then overridden by any flags
// that were explicitly passed on the command line.
type Config struct {
	Listen      string
	LibraryPath string

	// InitialStump seeds the Settings page on first run. Once a setting has
	// been saved there, the persisted file takes over and these env vars /
	// flags are no longer read.
	InitialStump StumpSettings

	AuthUsername string
	AuthPassword string

	GutendexURL string
	MaxBytes    int64

	// InitialZLibrary seeds the Settings page on first run, same rule as
	// InitialStump: once anything has been saved there, the file wins.
	InitialZLibrary ZLibrarySettings
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func LoadConfig() (*Config, error) {
	maxMB := int64(200)
	if v := os.Getenv("MAX_DOWNLOAD_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("MAX_DOWNLOAD_MB must be a positive integer, got %q", v)
		}
		maxMB = n
	}

	c := &Config{
		Listen:      envOr("LISTEN", ":8080"),
		LibraryPath: envOr("LIBRARY_PATH", ""),
		InitialStump: StumpSettings{
			URL:       strings.TrimRight(envOr("STUMP_URL", "http://localhost:10801"), "/"),
			APIKey:    envOr("STUMP_API_KEY", ""),
			Username:  envOr("STUMP_USERNAME", ""),
			Password:  envOr("STUMP_PASSWORD", ""),
			LibraryID: envOr("STUMP_LIBRARY_ID", ""),
		},

		AuthUsername: envOr("AUTH_USERNAME", ""),
		AuthPassword: envOr("AUTH_PASSWORD", ""),

		GutendexURL: strings.TrimRight(envOr("GUTENDEX_URL", "https://gutendex.com"), "/"),
		MaxBytes:    maxMB * 1024 * 1024,

		InitialZLibrary: ZLibrarySettings{
			BaseURL:  strings.TrimRight(envOr("ZLIBRARY_URL", ""), "/"),
			Email:    envOr("ZLIBRARY_EMAIL", ""),
			Password: os.Getenv("ZLIBRARY_PASSWORD"), // don't trim; whitespace can be part of it
		},
	}

	flag.StringVar(&c.Listen, "listen", c.Listen, "address to listen on")
	flag.StringVar(&c.LibraryPath, "library-path", c.LibraryPath, "local directory Stump scans; downloads are written here")
	flag.StringVar(&c.InitialStump.URL, "stump-url", c.InitialStump.URL, "base URL of the Stump server")
	flag.StringVar(&c.InitialStump.LibraryID, "stump-library-id", c.InitialStump.LibraryID, "Stump library id to rescan after a download")
	flag.Parse()

	c.InitialStump.URL = strings.TrimRight(c.InitialStump.URL, "/")

	if c.LibraryPath == "" {
		return nil, fmt.Errorf("LIBRARY_PATH (or -library-path) is required: the directory Stump scans")
	}

	if (c.AuthUsername == "") != (c.AuthPassword == "") {
		return nil, fmt.Errorf("AUTH_USERNAME and AUTH_PASSWORD must be set together")
	}

	abs, err := filepath.Abs(c.LibraryPath)
	if err != nil {
		return nil, fmt.Errorf("resolving LIBRARY_PATH: %w", err)
	}
	c.LibraryPath = abs

	info, err := os.Stat(c.LibraryPath)
	if err != nil {
		return nil, fmt.Errorf("LIBRARY_PATH %q is not readable: %w", c.LibraryPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("LIBRARY_PATH %q is not a directory", c.LibraryPath)
	}

	return c, nil
}

// AuthConfigured reports whether a login is required to use the app at all.
// Without it, every route below is served unauthenticated.
func (c *Config) AuthConfigured() bool {
	return c.AuthUsername != "" && c.AuthPassword != ""
}
