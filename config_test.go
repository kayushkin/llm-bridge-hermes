package main

import (
	"os"
	"testing"
)

func TestEnvOr(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TEST_ENV_OR", "custom")
		if got := envOr("TEST_ENV_OR", "default"); got != "custom" {
			t.Errorf("envOr = %q, want %q", got, "custom")
		}
	})

	t.Run("returns fallback when unset", func(t *testing.T) {
		os.Unsetenv("TEST_ENV_OR_UNSET")
		if got := envOr("TEST_ENV_OR_UNSET", "default"); got != "default" {
			t.Errorf("envOr = %q, want %q", got, "default")
		}
	})

	t.Run("returns fallback when empty", func(t *testing.T) {
		t.Setenv("TEST_ENV_OR_EMPTY", "")
		if got := envOr("TEST_ENV_OR_EMPTY", "default"); got != "default" {
			t.Errorf("envOr = %q, want %q", got, "default")
		}
	})
}

func TestEnvFloat(t *testing.T) {
	t.Run("parses valid float", func(t *testing.T) {
		t.Setenv("TEST_FLOAT", "42.5")
		if got := envFloat("TEST_FLOAT", 0); got != 42.5 {
			t.Errorf("envFloat = %f, want %f", got, 42.5)
		}
	})

	t.Run("returns fallback for invalid", func(t *testing.T) {
		t.Setenv("TEST_FLOAT_BAD", "notanumber")
		if got := envFloat("TEST_FLOAT_BAD", 3.0); got != 3.0 {
			t.Errorf("envFloat = %f, want %f", got, 3.0)
		}
	})

	t.Run("returns fallback when unset", func(t *testing.T) {
		os.Unsetenv("TEST_FLOAT_UNSET")
		if got := envFloat("TEST_FLOAT_UNSET", 15.0); got != 15.0 {
			t.Errorf("envFloat = %f, want %f", got, 15.0)
		}
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		// Clear all HERMES_ env vars to test defaults.
		for _, key := range []string{
			"HERMES_URL", "HERMES_API_KEY", "HERMES_MODEL",
			"HERMES_INPUT_PRICE_PER_M", "HERMES_OUTPUT_PRICE_PER_M",
			"HERMES_REASONING_PRICE_PER_M", "HERMES_PREFLIGHT",
		} {
			os.Unsetenv(key)
		}

		cfg := loadConfig()

		if cfg.BaseURL != "http://localhost:8642" {
			t.Errorf("BaseURL = %q, want default", cfg.BaseURL)
		}
		if cfg.APIKey != "" {
			t.Errorf("APIKey = %q, want empty", cfg.APIKey)
		}
		if cfg.Model != "" {
			t.Errorf("Model = %q, want empty", cfg.Model)
		}
		if cfg.ModelExplicit {
			t.Error("ModelExplicit should be false when HERMES_MODEL is unset")
		}
		if cfg.InputPricePerM != 3.0 {
			t.Errorf("InputPricePerM = %f, want 3.0", cfg.InputPricePerM)
		}
		if cfg.OutputPricePerM != 15.0 {
			t.Errorf("OutputPricePerM = %f, want 15.0", cfg.OutputPricePerM)
		}
		if cfg.ReasoningPricePerM != 15.0 {
			t.Errorf("ReasoningPricePerM = %f, want 15.0", cfg.ReasoningPricePerM)
		}
		if cfg.Preflight {
			t.Error("Preflight should be false by default")
		}
	})

	t.Run("explicit model", func(t *testing.T) {
		t.Setenv("HERMES_MODEL", "my-model")
		cfg := loadConfig()
		if cfg.Model != "my-model" {
			t.Errorf("Model = %q, want %q", cfg.Model, "my-model")
		}
		if !cfg.ModelExplicit {
			t.Error("ModelExplicit should be true when HERMES_MODEL is set")
		}
	})

	t.Run("preflight enabled", func(t *testing.T) {
		t.Setenv("HERMES_PREFLIGHT", "1")
		cfg := loadConfig()
		if !cfg.Preflight {
			t.Error("Preflight should be true when HERMES_PREFLIGHT=1")
		}
	})

	t.Run("custom pricing", func(t *testing.T) {
		t.Setenv("HERMES_INPUT_PRICE_PER_M", "5.5")
		t.Setenv("HERMES_OUTPUT_PRICE_PER_M", "20.0")
		t.Setenv("HERMES_REASONING_PRICE_PER_M", "25.0")
		cfg := loadConfig()
		if cfg.InputPricePerM != 5.5 {
			t.Errorf("InputPricePerM = %f, want 5.5", cfg.InputPricePerM)
		}
		if cfg.OutputPricePerM != 20.0 {
			t.Errorf("OutputPricePerM = %f, want 20.0", cfg.OutputPricePerM)
		}
		if cfg.ReasoningPricePerM != 25.0 {
			t.Errorf("ReasoningPricePerM = %f, want 25.0", cfg.ReasoningPricePerM)
		}
	})
}
