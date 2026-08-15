package main

import "testing"

func TestLoadEnvConfigDefaultsToConfiguredGrokModel(t *testing.T) {
	t.Setenv("AI_BASE_URL", "http://example.test/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "")

	cfg, err := loadEnvConfig()
	if err != nil {
		t.Fatalf("loadEnvConfig() error = %v", err)
	}
	if cfg.Model != "grok-4.3-fast" {
		t.Fatalf("Model = %q, want grok-4.3-fast", cfg.Model)
	}
}
