package config

import "fmt"

//nolint:gocyclo // reason: 集中的 config 校验函数本身就是一长串独立的字段校验 switch case，每条 case 业务上互不相关；拆开只会增加跳转成本，不会提高可读性。
func validateConfig(cfg Config) error {
	switch {
	case cfg.DatabaseURL == "":
		return fmt.Errorf("DATABASE_URL is required")
	case cfg.Analyzer.BaseURL == "":
		return fmt.Errorf("AI_BASE_URL is required")
	case cfg.Analyzer.APIKey == "":
		return fmt.Errorf("AI_API_KEY is required")
	case cfg.Analyzer.Model == "":
		return fmt.Errorf("AI_MODEL is required")
	case cfg.ExtensionAPIToken == "":
		return fmt.Errorf("EXTENSION_API_TOKEN is required")
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
	case cfg.IdempotencyTTLMS < 0:
		return fmt.Errorf("IDEMPOTENCY_TTL_MS must be >= 0 (0 keeps middleware default 24h)")
	case cfg.RiverTerminalRetentionMS < -1 || cfg.RiverTerminalRetentionMS == 0:
		return fmt.Errorf("RIVER_TERMINAL_RETENTION_MS must be -1 or a positive integer")
	}

	return nil
}
