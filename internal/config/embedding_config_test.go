package config

import (
	"strings"
	"testing"
)

func TestLoadEmbeddingDefaults(t *testing.T) {
	setBaseConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// EMBEDDING_MODEL defaults to empty (= retrieval path disabled).
	if cfg.Embedding.Model != "" {
		t.Fatalf("Embedding.Model = %q, want empty default", cfg.Embedding.Model)
	}
	if cfg.Embedding.Dimensions != 1536 {
		t.Fatalf("Embedding.Dimensions = %d, want 1536", cfg.Embedding.Dimensions)
	}
	// BaseURL / APIKey inherit AI_BASE_URL / AI_API_KEY when not set.
	if cfg.Embedding.BaseURL != cfg.Analyzer.BaseURL {
		t.Fatalf("Embedding.BaseURL = %q, want inherited %q", cfg.Embedding.BaseURL, cfg.Analyzer.BaseURL)
	}
	if cfg.Embedding.APIKey != cfg.Analyzer.APIKey {
		t.Fatalf("Embedding.APIKey = %q, want inherited %q", cfg.Embedding.APIKey, cfg.Analyzer.APIKey)
	}
	// SSRF + retry knobs inherit from the analyzer config.
	if cfg.Embedding.AllowUnsafeTargets != cfg.Analyzer.AllowUnsafeTargets {
		t.Fatalf("Embedding.AllowUnsafeTargets = %v, want inherited %v", cfg.Embedding.AllowUnsafeTargets, cfg.Analyzer.AllowUnsafeTargets)
	}
	if cfg.Embedding.RetryAttempts != cfg.Analyzer.RetryAttempts {
		t.Fatalf("Embedding.RetryAttempts = %d, want inherited %d", cfg.Embedding.RetryAttempts, cfg.Analyzer.RetryAttempts)
	}
	if cfg.Embedding.RequestTimeoutMS != cfg.Analyzer.RequestTimeoutMS {
		t.Fatalf("Embedding.RequestTimeoutMS = %d, want inherited %d", cfg.Embedding.RequestTimeoutMS, cfg.Analyzer.RequestTimeoutMS)
	}
}

func TestLoadEmbeddingOverrides(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("EMBEDDING_MODEL", "text-embedding-3-small")
	t.Setenv("EMBEDDING_BASE_URL", "https://embeddings.example.com/v1")
	t.Setenv("EMBEDDING_API_KEY", "embed-key")
	t.Setenv("EMBEDDING_DIMENSIONS", "768")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Embedding.Model != "text-embedding-3-small" {
		t.Fatalf("Embedding.Model = %q", cfg.Embedding.Model)
	}
	if cfg.Embedding.BaseURL != "https://embeddings.example.com/v1" {
		t.Fatalf("Embedding.BaseURL = %q", cfg.Embedding.BaseURL)
	}
	if cfg.Embedding.APIKey != "embed-key" {
		t.Fatalf("Embedding.APIKey = %q", cfg.Embedding.APIKey)
	}
	if cfg.Embedding.Dimensions != 768 {
		t.Fatalf("Embedding.Dimensions = %d, want 768", cfg.Embedding.Dimensions)
	}
}

func TestLoadEmbeddingValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		needle string
	}{
		{
			name:   "dimensions too small",
			env:    map[string]string{"EMBEDDING_DIMENSIONS": "32"},
			needle: "EMBEDDING_DIMENSIONS",
		},
		{
			name:   "dimensions too large",
			env:    map[string]string{"EMBEDDING_DIMENSIONS": "9000"},
			needle: "EMBEDDING_DIMENSIONS",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("Load() err = %v, want error containing %q", err, tc.needle)
			}
		})
	}
}
