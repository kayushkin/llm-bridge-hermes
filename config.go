package main

import "os"

// Config holds environment-based configuration for the Hermes harness.
type Config struct {
	// BaseURL is the Hermes API server base URL.
	BaseURL string
	// APIKey is the bearer token for Hermes API authentication.
	APIKey string
	// Model is the model name to send in requests (cosmetic for Hermes).
	Model string
}

func loadConfig() Config {
	return Config{
		BaseURL: envOr("HERMES_URL", "http://localhost:8642"),
		APIKey:  os.Getenv("HERMES_API_KEY"),
		Model:   envOr("HERMES_MODEL", "hermes-agent"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
