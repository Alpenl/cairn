package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type envConfig struct {
	BaseURL         string
	APIKey          string
	Model           string
	GitHubToken     string
	YtdlpBinaryPath string
	YtdlpTimeout    time.Duration
}

func loadEnvConfig() (envConfig, error) {
	cfg := envConfig{
		BaseURL:         strings.TrimSpace(os.Getenv("AI_BASE_URL")),
		APIKey:          strings.TrimSpace(os.Getenv("AI_API_KEY")),
		Model:           strings.TrimSpace(os.Getenv("AI_MODEL")),
		GitHubToken:     strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		YtdlpBinaryPath: strings.TrimSpace(os.Getenv("YTDLP_BINARY_PATH")),
	}
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return cfg, fmt.Errorf("AI_BASE_URL and AI_API_KEY must be set (export them or source .env)")
	}
	if cfg.Model == "" {
		cfg.Model = "grok-4.3-fast"
	}
	if v := strings.TrimSpace(os.Getenv("YTDLP_TIMEOUT_MS")); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("YTDLP_TIMEOUT_MS=%q: %w", v, err)
		}
		cfg.YtdlpTimeout = time.Duration(ms) * time.Millisecond
	}
	return cfg, nil
}
