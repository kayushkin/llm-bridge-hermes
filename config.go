package main

import (
	"os"
	"strconv"
)

// Config holds environment-based configuration for the Hermes harness.
type Config struct {
	// BaseURL is the Hermes API server base URL.
	BaseURL string
	// APIKey is the bearer token for Hermes API authentication.
	APIKey string
	// Model is the model name to send in requests. Empty means the bridge
	// will query GET /v1/models on startup and use the server-advertised name.
	Model string
	// ModelExplicit records whether the user pinned HERMES_MODEL. When true,
	// model discovery must not overwrite it.
	ModelExplicit bool
	// Pricing configuration (USD per million tokens)
	InputPricePerM     float64
	OutputPricePerM    float64
	ReasoningPricePerM float64
	// Preflight controls whether the harness calls GET /health at startup.
	Preflight bool
	// DashboardURL is the Hermes web dashboard base URL (e.g. http://127.0.0.1:9119).
	// Required for session discovery (-discover). Empty disables dashboard features.
	DashboardURL string
	// DashboardKey is an optional auth token for the dashboard API.
	DashboardKey string
}

func loadConfig() Config {
	model := os.Getenv("HERMES_MODEL")
	return Config{
		BaseURL:            envOr("HERMES_URL", "http://localhost:8642"),
		APIKey:             os.Getenv("HERMES_API_KEY"),
		Model:              model,
		ModelExplicit:      model != "",
		InputPricePerM:     envFloat("HERMES_INPUT_PRICE_PER_M", 3.0),     // Default: $3/M input tokens
		OutputPricePerM:    envFloat("HERMES_OUTPUT_PRICE_PER_M", 15.0),   // Default: $15/M output tokens
		ReasoningPricePerM: envFloat("HERMES_REASONING_PRICE_PER_M", 15.0), // Default: same as output
		Preflight:          os.Getenv("HERMES_PREFLIGHT") == "1",
		DashboardURL:       os.Getenv("HERMES_DASHBOARD_URL"),
		DashboardKey:       os.Getenv("HERMES_DASHBOARD_KEY"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
