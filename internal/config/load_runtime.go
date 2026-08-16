package config

import (
	"os"
	"strings"

	"webtag/internal/model"
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
	embedding, err := loadEmbeddingConfig(analyzer)
	if err != nil {
		return Config{}, err
	}
	rateLimit, err := loadRateLimitConfig()
	if err != nil {
		return Config{}, err
	}
	scalars, err := loadScalarKnobs()
	if err != nil {
		return Config{}, err
	}
	idem, err := loadIdempotencyConfig()
	if err != nil {
		return Config{}, err
	}
	// 默认 false = fail-closed。显式设 true 才恢复「零凭证即开放」的历史行为。
	publicAPIOpen, err := envBool("PUBLIC_API_OPEN", false)
	if err != nil {
		return Config{}, err
	}
	translationReconcileIntervalMS, err := envInt("TRANSLATION_RECONCILE_INTERVAL_MS", 60000)
	if err != nil {
		return Config{}, err
	}
	translationReconcileBatch, err := envInt("TRANSLATION_RECONCILE_BATCH", 100)
	if err != nil {
		return Config{}, err
	}
	translationReconcileRoundTimeoutMS, err := envInt("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", 30000)
	if err != nil {
		return Config{}, err
	}
	translationReconcileMissingAfterMS, err := envInt("TRANSLATION_RECONCILE_MISSING_AFTER_MS", 21600000)
	if err != nil {
		return Config{}, err
	}
	parseReconcileIntervalMS, err := envInt("PARSE_RECONCILE_INTERVAL_MS", 60000)
	if err != nil {
		return Config{}, err
	}
	parseReconcileBatch, err := envInt("PARSE_RECONCILE_BATCH", 100)
	if err != nil {
		return Config{}, err
	}
	parseReconcileRoundTimeoutMS, err := envInt("PARSE_RECONCILE_ROUND_TIMEOUT_MS", 30000)
	if err != nil {
		return Config{}, err
	}
	parseReconcileMissingAfterMS, err := envInt("PARSE_RECONCILE_MISSING_AFTER_MS", 21600000)
	if err != nil {
		return Config{}, err
	}
	riverTerminalRetentionMS, err := envInt("RIVER_TERMINAL_RETENTION_MS", 604800000)
	if err != nil {
		return Config{}, err
	}
	riverMaxRecoveryDowntimeMS, err := envInt("RIVER_MAX_RECOVERY_DOWNTIME_MS", 86400000)
	if err != nil {
		return Config{}, err
	}
	siteMigrationEnabled, err := envBool("SITE_MIGRATION_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	siteMigrationDryRun, err := envBool("SITE_MIGRATION_DRY_RUN", true)
	if err != nil {
		return Config{}, err
	}
	siteMigrationBatch, err := envInt("SITE_MIGRATION_BATCH", 100)
	if err != nil {
		return Config{}, err
	}
	siteMigrationIntervalMS, err := envInt("SITE_MIGRATION_INTERVAL_MS", 900000)
	if err != nil {
		return Config{}, err
	}
	libraryKindAPIEnabled, err := envBool("LIBRARY_KIND_API_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	siteLibraryWriteEnabled, err := envBool("SITE_LIBRARY_WRITE_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	siteAutoClassificationEnabled, err := envBool("SITE_AUTO_CLASSIFICATION_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	siteAdvancedManagementEnabled, err := envBool("SITE_ADVANCED_MANAGEMENT_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	otel, err := loadOTelConfig()
	if err != nil {
		return Config{}, err
	}
	sessionSigningKey, err := resolveSessionSigningKey(envString("SESSION_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	appEnv := strings.ToLower(envString("APP_ENV", "prod"))
	explicitCursorSigningKey := envString("CURSOR_SIGNING_KEY", "")
	readerCursorSigningKey, err := resolveReaderCursorSigningKey(
		appEnv,
		envString("READER_CURSOR_SIGNING_KEY", ""),
		explicitCursorSigningKey,
		databaseURL,
		analyzer.APIKey,
	)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Server:    server,
		DB:        db,
		Fetcher:   fetcher,
		Analyzer:  analyzer,
		Embedding: embedding,
		RateLimit: rateLimit,

		DatabaseURL: databaseURL,

		LogLevel:                           envString("LOG_LEVEL", "info"),
		LogFormat:                          envString("LOG_FORMAT", "json"),
		TagCacheTTLMS:                      scalars.tagCacheTTLMS,
		TreeCacheTTLMS:                     scalars.treeCacheTTLMS,
		GitHubToken:                        envString("GITHUB_TOKEN", ""),
		YtdlpBinaryPath:                    envString("YTDLP_BINARY_PATH", "yt-dlp"),
		YtdlpTimeoutMS:                     scalars.ytdlpTimeoutMS,
		GzipEnabled:                        scalars.gzipEnabled,
		GzipMinLength:                      scalars.gzipMinLength,
		MetricsAuthToken:                   envString("METRICS_AUTH_TOKEN", ""),
		AdminAuthToken:                     envString("ADMIN_AUTH_TOKEN", ""),
		ExtensionAPIToken:                  envString("EXTENSION_API_TOKEN", ""),
		PublicAPIOpen:                      publicAPIOpen,
		SessionSigningKey:                  sessionSigningKey,
		TranslationJobsRollout:             model.TranslationJobsRolloutStage(strings.ToLower(envString("TRANSLATION_JOBS_ROLLOUT", string(model.TranslationJobsRolloutStrictV2)))),
		TranslationSourceRollout:           TranslationSourceRolloutStage(strings.ToLower(envString("TRANSLATION_SOURCE_ROLLOUT", string(TranslationSourceRolloutStrict)))),
		TranslationReconcileIntervalMS:     translationReconcileIntervalMS,
		TranslationReconcileBatch:          translationReconcileBatch,
		TranslationReconcileRoundTimeoutMS: translationReconcileRoundTimeoutMS,
		TranslationReconcileMissingAfterMS: translationReconcileMissingAfterMS,
		ParseReconcileIntervalMS:           parseReconcileIntervalMS,
		ParseReconcileBatch:                parseReconcileBatch,
		ParseReconcileRoundTimeoutMS:       parseReconcileRoundTimeoutMS,
		ParseReconcileMissingAfterMS:       parseReconcileMissingAfterMS,
		RiverTerminalRetentionMS:           riverTerminalRetentionMS,
		RiverMaxRecoveryDowntimeMS:         riverMaxRecoveryDowntimeMS,
		AppEnv:                             appEnv,
		PProfEnabled:                       scalars.pprofEnabled,
		CursorSigningKey:                   explicitCursorSigningKey,
		ReaderCursorSigningKey:             readerCursorSigningKey,
		IdempotencyEnabled:                 idem.enabled,
		IdempotencyTTLMS:                   idem.ttlMS,
		SiteMigrationEnabled:               siteMigrationEnabled,
		SiteMigrationDryRun:                siteMigrationDryRun,
		SiteMigrationBatch:                 siteMigrationBatch,
		SiteMigrationIntervalMS:            siteMigrationIntervalMS,
		LibraryKindAPIEnabled:              libraryKindAPIEnabled,
		SiteLibraryWriteEnabled:            siteLibraryWriteEnabled,
		SiteAutoClassificationEnabled:      siteAutoClassificationEnabled,
		SiteAdvancedManagementEnabled:      siteAdvancedManagementEnabled,

		OTELEndpoint:      envString("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTELSamplingRatio: otel.samplingRatio,
		OTELInsecure:      otel.insecure,
	}, nil
}
