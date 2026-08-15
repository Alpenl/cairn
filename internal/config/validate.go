package config

import (
	"fmt"

	"webtag/internal/model"
)

//nolint:gocyclo // reason: 集中的 config 校验函数本身就是一长串独立的字段校验 switch case，每条 case 业务上互不相关；拆成 validateRedis/validateRateLimit 等只会增加跳转成本，不会提高可读性。
func validateConfig(cfg Config) error {
	// RateLimitBurst==0 with RateLimit.RPS>0 is intentionally allowed:
	// middleware.RateLimit derives a sensible default burst (one second
	// of capacity) so single-knob configs like RATE_LIMIT_RPS=10 stay
	// valid. The validation switch only flags negative values.
	switch {
	case cfg.DatabaseURL == "":
		return fmt.Errorf("DATABASE_URL is required")
	case cfg.Analyzer.BaseURL == "":
		return fmt.Errorf("AI_BASE_URL is required")
	case cfg.Analyzer.APIKey == "":
		return fmt.Errorf("AI_API_KEY is required")
	case cfg.Analyzer.Model == "":
		return fmt.Errorf("AI_MODEL is required")
	case cfg.DB.ParseConcurrency <= 0:
		return fmt.Errorf("PARSE_CONCURRENCY must be greater than 0")
	case cfg.Server.ReadHeaderTimeoutMS <= 0:
		return fmt.Errorf("READ_HEADER_TIMEOUT_MS must be greater than 0")
	case cfg.Server.ReadTimeoutMS <= 0:
		return fmt.Errorf("READ_TIMEOUT_MS must be greater than 0")
	case cfg.Server.WriteTimeoutMS <= 0:
		return fmt.Errorf("WRITE_TIMEOUT_MS must be greater than 0")
	case cfg.Server.IdleTimeoutMS <= 0:
		return fmt.Errorf("IDLE_TIMEOUT_MS must be greater than 0")
	case cfg.Server.ShutdownTimeoutMS < 1000:
		return fmt.Errorf("SHUTDOWN_TIMEOUT_MS must be >= 1000 (sub-second budgets cannot drain workers gracefully)")
	case cfg.Fetcher.RetryAttempts <= 0:
		return fmt.Errorf("FETCH_RETRY_ATTEMPTS must be greater than 0")
	case cfg.Fetcher.RetryDelayMS <= 0:
		return fmt.Errorf("FETCH_RETRY_DELAY_MS must be greater than 0")
	case cfg.Analyzer.RetryAttempts <= 0:
		return fmt.Errorf("AI_RETRY_ATTEMPTS must be greater than 0")
	case cfg.Analyzer.RetryDelayMS <= 0:
		return fmt.Errorf("AI_RETRY_DELAY_MS must be greater than 0")
	case cfg.Analyzer.RequestTimeoutMS <= 0:
		return fmt.Errorf("AI_REQUEST_TIMEOUT_MS must be greater than 0")
	case cfg.YtdlpTimeoutMS <= 0:
		return fmt.Errorf("YTDLP_TIMEOUT_MS must be greater than 0")
	case cfg.TagCacheTTLMS <= 0:
		return fmt.Errorf("TAG_CACHE_TTL_MS must be greater than 0")
	case cfg.TreeCacheTTLMS <= 0:
		return fmt.Errorf("TREE_CACHE_TTL_MS must be greater than 0")
	case cfg.GzipMinLength <= 0:
		return fmt.Errorf("GZIP_MIN_LENGTH must be greater than 0")
	case cfg.DB.MaxConns <= 0:
		return fmt.Errorf("DB_MAX_CONNS must be greater than 0")
	case cfg.DB.MaxConns > 1000:
		return fmt.Errorf("DB_MAX_CONNS must be <= 1000")
	case cfg.DB.MinConns < 0:
		return fmt.Errorf("DB_MIN_CONNS must be >= 0")
	case cfg.DB.MinConns > 1000:
		return fmt.Errorf("DB_MIN_CONNS must be <= 1000")
	case cfg.DB.MinConns > cfg.DB.MaxConns:
		return fmt.Errorf("DB_MIN_CONNS must be <= DB_MAX_CONNS")
	case cfg.DB.MaxConnLifetimeMS < 0:
		return fmt.Errorf("DB_MAX_CONN_LIFETIME_MS must be >= 0 (0 keeps the pgx default of 1h)")
	case cfg.DB.MaxConnIdleTimeMS < 0:
		return fmt.Errorf("DB_MAX_CONN_IDLE_TIME_MS must be >= 0 (0 keeps the pgx default of 30min)")
	case cfg.DB.HealthCheckPeriodMS < 0:
		return fmt.Errorf("DB_HEALTH_CHECK_PERIOD_MS must be >= 0 (0 keeps the pgx default of 1min)")
	case cfg.RateLimit.RPS < 0:
		return fmt.Errorf("RATE_LIMIT_RPS must be >= 0")
	case cfg.RateLimit.Burst < 0:
		return fmt.Errorf("RATE_LIMIT_BURST must be >= 0")
	case cfg.IdempotencyTTLMS < 0:
		return fmt.Errorf("IDEMPOTENCY_TTL_MS must be >= 0 (0 keeps middleware default 24h)")
	case cfg.TranslationReconcileIntervalMS < 1000:
		return fmt.Errorf("TRANSLATION_RECONCILE_INTERVAL_MS must be >= 1000")
	case cfg.TranslationReconcileBatch < 1 || cfg.TranslationReconcileBatch > 1000:
		return fmt.Errorf("TRANSLATION_RECONCILE_BATCH must be in [1, 1000]")
	case cfg.TranslationReconcileRoundTimeoutMS < 1:
		return fmt.Errorf("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS must be >= 1")
	case cfg.TranslationReconcileMissingAfterMS <= cfg.TranslationReconcileRoundTimeoutMS:
		return fmt.Errorf("TRANSLATION_RECONCILE_MISSING_AFTER_MS must be greater than TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS")
	case cfg.ParseReconcileIntervalMS < 1000:
		return fmt.Errorf("PARSE_RECONCILE_INTERVAL_MS must be >= 1000")
	case cfg.ParseReconcileBatch < 1 || cfg.ParseReconcileBatch > 1000:
		return fmt.Errorf("PARSE_RECONCILE_BATCH must be in [1, 1000]")
	case cfg.ParseReconcileRoundTimeoutMS < 1:
		return fmt.Errorf("PARSE_RECONCILE_ROUND_TIMEOUT_MS must be >= 1")
	case cfg.ParseReconcileMissingAfterMS <= cfg.ParseReconcileRoundTimeoutMS:
		return fmt.Errorf("PARSE_RECONCILE_MISSING_AFTER_MS must be greater than PARSE_RECONCILE_ROUND_TIMEOUT_MS")
	case cfg.RiverMaxRecoveryDowntimeMS < 1000:
		return fmt.Errorf("RIVER_MAX_RECOVERY_DOWNTIME_MS must be >= 1000")
	case cfg.RiverTerminalRetentionMS < -1 || cfg.RiverTerminalRetentionMS == 0:
		return fmt.Errorf("RIVER_TERMINAL_RETENTION_MS must be -1 or a positive integer")
	case cfg.RiverTerminalRetentionMS != -1 && int64(cfg.RiverTerminalRetentionMS) <= minimumRiverTerminalRetentionMS(cfg):
		return fmt.Errorf(
			"RIVER_TERMINAL_RETENTION_MS must be greater than the recovery window (%d ms)",
			minimumRiverTerminalRetentionMS(cfg),
		)
	case cfg.SiteMigrationBatch < 1 || cfg.SiteMigrationBatch > 1000:
		return fmt.Errorf("SITE_MIGRATION_BATCH must be in [1, 1000]")
	case cfg.SiteMigrationIntervalMS < 1000:
		return fmt.Errorf("SITE_MIGRATION_INTERVAL_MS must be >= 1000")
	case cfg.OTELSamplingRatio < 0 || cfg.OTELSamplingRatio > 1:
		return fmt.Errorf("OTEL_SAMPLING_RATIO must be in [0, 1]: got %f", cfg.OTELSamplingRatio)
	case cfg.Embedding.Dimensions < 64 || cfg.Embedding.Dimensions > 8192:
		return fmt.Errorf("EMBEDDING_DIMENSIONS must be in [64, 8192]: got %d", cfg.Embedding.Dimensions)
	case !cfg.TranslationJobsRollout.Valid():
		return fmt.Errorf("TRANSLATION_JOBS_ROLLOUT must be %q, %q, or %q: got %q", model.TranslationJobsRolloutCompatV1, model.TranslationJobsRolloutDrainV1, model.TranslationJobsRolloutStrictV2, cfg.TranslationJobsRollout)
	case !cfg.TranslationSourceRollout.Valid():
		return fmt.Errorf("TRANSLATION_SOURCE_ROLLOUT must be %q or %q: got %q", TranslationSourceRolloutCompat, TranslationSourceRolloutStrict, cfg.TranslationSourceRollout)
	}

	return nil
}

func minimumRiverTerminalRetentionMS(cfg Config) int64 {
	missingAfter := max(int64(cfg.ParseReconcileMissingAfterMS), int64(cfg.TranslationReconcileMissingAfterMS))
	parseCycle := int64(cfg.ParseReconcileIntervalMS) + int64(cfg.ParseReconcileRoundTimeoutMS)
	translationCycle := int64(cfg.TranslationReconcileIntervalMS) + int64(cfg.TranslationReconcileRoundTimeoutMS)
	cycleBudget := max(parseCycle, translationCycle)
	return missingAfter + int64(cfg.RiverMaxRecoveryDowntimeMS) + cycleBudget
}
