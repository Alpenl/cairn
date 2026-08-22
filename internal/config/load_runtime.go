package config

import (
	"os"
	"strings"
)

func loadRuntimeConfig() (Config, error) { //nolint:gocyclo // 逐项解析运行时配置，分支数等于配置项数
	server, err := loadServerConfig()
	if err != nil {
		return Config{}, err
	}
	db, err := loadDBConfig()
	if err != nil {
		return Config{}, err
	}
	fetcher, err := loadFetcherConfig()
	if err != nil {
		return Config{}, err
	}
	analyzer, err := loadAnalyzerConfig()
	if err != nil {
		return Config{}, err
	}
	idempotencyTTLMS, err := loadIdempotencyTTLMS()
	if err != nil {
		return Config{}, err
	}
	riverTerminalRetentionMS, err := envInt("RIVER_TERMINAL_RETENTION_MS", 604800000)
	if err != nil {
		return Config{}, err
	}
	sessionSigningKey, sessionSigningKeyEphemeral, err := resolveSessionSigningKey(envString("SESSION_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	appEnv := strings.ToLower(envString("APP_ENV", "prod"))
	cursorSigningKey, err := resolveCursorSigningKey(
		appEnv,
		envString("CURSOR_SIGNING_KEY", ""),
		databaseURL,
		analyzer.APIKey,
	)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Server:   server,
		DB:       db,
		Fetcher:  fetcher,
		Analyzer: analyzer,

		DatabaseURL: databaseURL,

		LogLevel:                   envString("LOG_LEVEL", "info"),
		LogFormat:                  envString("LOG_FORMAT", "json"),
		GitHubToken:                envString("GITHUB_TOKEN", ""),
		ExtensionAPIToken:          envString("EXTENSION_API_TOKEN", ""),
		SessionSigningKey:          sessionSigningKey,
		SessionSigningKeyEphemeral: sessionSigningKeyEphemeral,
		RiverTerminalRetentionMS:   riverTerminalRetentionMS,
		AppEnv:                     appEnv,
		CursorSigningKey:           cursorSigningKey,
		IdempotencyTTLMS:           idempotencyTTLMS,
	}, nil
}
