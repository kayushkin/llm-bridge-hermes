package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/kayushkin/aiauth"
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
	// CredentialID is the aiauth profile name to resolve into the bearer
	// token used for the Hermes /v1 API. Populated from LLMBRIDGE_CREDENTIAL_ID
	// and optionally overridden by the start message credential_id field.
	CredentialID string
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
		CredentialID:       os.Getenv("LLMBRIDGE_CREDENTIAL_ID"),
	}
}

// resolveCredentialKey looks up an aiauth profile by name and returns the
// raw secret value (api key, token, or oauth access token) for use as the
// Hermes bearer token. Empty credentialID returns ("", nil) so callers can
// fall back to an explicitly-set HERMES_API_KEY.
func resolveCredentialKey(credentialID string) (string, error) {
	if credentialID == "" {
		return "", nil
	}

	store := aiauth.DefaultStore()
	profiles := store.Profiles()
	cred, ok := profiles[credentialID]
	if !ok {
		return "", fmt.Errorf("credential profile %q not found in aiauth store", credentialID)
	}

	var key string
	switch cred.Type {
	case "api_key":
		key = cred.Key
	case "token":
		key = cred.Token
	case "oauth":
		key = cred.Access
	default:
		return "", fmt.Errorf("credential %q has unsupported type %q", credentialID, cred.Type)
	}
	if key == "" {
		return "", fmt.Errorf("credential %q is empty for type %q", credentialID, cred.Type)
	}
	return key, nil
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
