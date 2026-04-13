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
	// Model is the model name to send in requests (cosmetic for Hermes).
	Model string
	// Pricing configuration (USD per million tokens)
	InputPricePerM  float64
	OutputPricePerM float64
	ReasoningPricePerM float64
}

func loadConfig() Config {
	return Config{
		BaseURL:            envOr("HERMES_URL", "http://localhost:8642"),
		APIKey:             os.Getenv("HERMES_API_KEY"),
		Model:              envOr("HERMES_MODEL", "hermes-agent"),
		InputPricePerM:     envFloat("HERMES_INPUT_PRICE_PER_M", 3.0),   // Default: $3/M input tokens
		OutputPricePerM:    envFloat("HERMES_OUTPUT_PRICE_PER_M", 15.0), // Default: $15/M output tokens
		ReasoningPricePerM: envFloat("HERMES_REASONING_PRICE_PER_M", 15.0), // Default: same as output
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
