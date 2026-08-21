package config

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("CURSOR_SIGNING_KEY", testCursorKeyA); err != nil {
		panic(err)
	}
	if err := os.Setenv("EXTENSION_API_TOKEN", "test-installation-token"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	// Wave 5 P2 默认 30s 优雅停机预算。
	if cfg.Server.ShutdownTimeoutMS != 30000 {
		t.Fatalf("Server.ShutdownTimeoutMS = %d, want 30000", cfg.Server.ShutdownTimeoutMS)
	}

	if cfg.Server.ListenAddr != ":8000" {
		t.Fatalf("Server.ListenAddr = %q, want %q", cfg.Server.ListenAddr, ":8000")
	}
	if cfg.CursorSigningKey != testCursorKeyA {
		t.Fatalf("CursorSigningKey = %q, want explicit key", cfg.CursorSigningKey)
	}

	if len(cfg.Server.CORSOrigins) != 1 || cfg.Server.CORSOrigins[0] != "http://localhost:3000" {
		t.Fatalf("Server.CORSOrigins = %#v, want localhost default", cfg.Server.CORSOrigins)
	}
	if len(cfg.Server.TrustedProxyCIDRs) != 0 {
		t.Fatalf("Server.TrustedProxyCIDRs = %#v, want no trusted proxies by default", cfg.Server.TrustedProxyCIDRs)
	}

	if cfg.DB.ParseConcurrency != 3 {
		t.Fatalf("DB.ParseConcurrency = %d, want 3", cfg.DB.ParseConcurrency)
	}
	if cfg.Server.ReadHeaderTimeoutMS != 2000 {
		t.Fatalf("Server.ReadHeaderTimeoutMS = %d, want 2000", cfg.Server.ReadHeaderTimeoutMS)
	}
	if cfg.Server.ReadTimeoutMS != 10000 {
		t.Fatalf("Server.ReadTimeoutMS = %d, want 10000", cfg.Server.ReadTimeoutMS)
	}
	if cfg.Server.WriteTimeoutMS != 15000 {
		t.Fatalf("Server.WriteTimeoutMS = %d, want 15000", cfg.Server.WriteTimeoutMS)
	}
	if cfg.Server.IdleTimeoutMS != 60000 {
		t.Fatalf("Server.IdleTimeoutMS = %d, want 60000", cfg.Server.IdleTimeoutMS)
	}

	if cfg.Fetcher.RetryAttempts != 2 {
		t.Fatalf("Fetcher.RetryAttempts = %d, want 2", cfg.Fetcher.RetryAttempts)
	}
	if !cfg.Fetcher.URLDirect {
		t.Fatal("Fetcher.URLDirect = false, want true by default for Grok URL-direct analysis")
	}
	if cfg.Analyzer.RetryAttempts != 3 {
		t.Fatalf("Analyzer.RetryAttempts = %d, want 3", cfg.Analyzer.RetryAttempts)
	}
	if cfg.Analyzer.RequestTimeoutMS != 60000 {
		t.Fatalf("Analyzer.RequestTimeoutMS = %d, want 60000", cfg.Analyzer.RequestTimeoutMS)
	}
}

func TestLoadRejectsMissingCursorSigningKeyOutsideDev(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("CURSOR_SIGNING_KEY", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CURSOR_SIGNING_KEY") {
		t.Fatalf("Load() error = %v, want explicit cursor signing key requirement", err)
	}
}

func TestLoadUsesOneCursorSigningKey(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("CURSOR_SIGNING_KEY", testCursorKeyA)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CursorSigningKey != testCursorKeyA {
		t.Fatalf("CursorSigningKey = %q, want configured key", cfg.CursorSigningKey)
	}
}

func TestLoadAllowsDevelopmentCursorFallback(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("CURSOR_SIGNING_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := deriveDevelopmentCursorSigningKey(cfg.DatabaseURL, cfg.Analyzer.APIKey)
	if cfg.CursorSigningKey != want {
		t.Fatalf("CursorSigningKey = %q, want development fallback %q", cfg.CursorSigningKey, want)
	}
}

func TestLoadValidatesTrustedProxyCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
		bad   bool
	}{
		{name: "explicit ingress networks", value: "10.20.0.0/16, 2001:db8::/48", want: []string{"10.20.0.0/16", "2001:db8::/48"}},
		{name: "deduplicates canonical networks", value: "10.20.1.2/16,10.20.0.0/16", want: []string{"10.20.0.0/16"}},
		{name: "invalid CIDR", value: "10.20.0.1", bad: true},
		{name: "blank ambiguity", value: "10.20.0.0/16, ", bad: true},
		{name: "IPv4 trust all", value: "0.0.0.0/0", bad: true},
		{name: "IPv6 trust all", value: "::/0", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("TRUSTED_PROXY_CIDRS", tc.value)
			cfg, err := Load()
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
					t.Fatalf("Load() error = %v, want trusted proxy validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if strings.Join(cfg.Server.TrustedProxyCIDRs, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("TrustedProxyCIDRs = %#v, want %#v", cfg.Server.TrustedProxyCIDRs, tc.want)
			}
		})
	}
}

func TestLoadReadsRetryConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("READ_HEADER_TIMEOUT_MS", "2500")
	t.Setenv("READ_TIMEOUT_MS", "11000")
	t.Setenv("WRITE_TIMEOUT_MS", "16000")
	t.Setenv("IDLE_TIMEOUT_MS", "65000")
	t.Setenv("FETCH_RETRY_ATTEMPTS", "4")
	t.Setenv("FETCH_RETRY_DELAY_MS", "150")
	t.Setenv("AI_RETRY_ATTEMPTS", "3")
	t.Setenv("AI_RETRY_DELAY_MS", "120")
	t.Setenv("AI_REQUEST_TIMEOUT_MS", "900")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Fetcher.RetryAttempts != 4 {
		t.Fatalf("Fetcher.RetryAttempts = %d, want 4", cfg.Fetcher.RetryAttempts)
	}
	if cfg.Server.ReadHeaderTimeoutMS != 2500 {
		t.Fatalf("Server.ReadHeaderTimeoutMS = %d, want 2500", cfg.Server.ReadHeaderTimeoutMS)
	}
	if cfg.Server.ReadTimeoutMS != 11000 {
		t.Fatalf("Server.ReadTimeoutMS = %d, want 11000", cfg.Server.ReadTimeoutMS)
	}
	if cfg.Server.WriteTimeoutMS != 16000 {
		t.Fatalf("Server.WriteTimeoutMS = %d, want 16000", cfg.Server.WriteTimeoutMS)
	}
	if cfg.Server.IdleTimeoutMS != 65000 {
		t.Fatalf("Server.IdleTimeoutMS = %d, want 65000", cfg.Server.IdleTimeoutMS)
	}
	if cfg.Fetcher.RetryDelayMS != 150 {
		t.Fatalf("Fetcher.RetryDelayMS = %d, want 150", cfg.Fetcher.RetryDelayMS)
	}
	if cfg.Analyzer.RetryAttempts != 3 {
		t.Fatalf("Analyzer.RetryAttempts = %d, want 3", cfg.Analyzer.RetryAttempts)
	}
	if cfg.Analyzer.RetryDelayMS != 120 {
		t.Fatalf("Analyzer.RetryDelayMS = %d, want 120", cfg.Analyzer.RetryDelayMS)
	}
	if cfg.Analyzer.RequestTimeoutMS != 900 {
		t.Fatalf("Analyzer.RequestTimeoutMS = %d, want 900", cfg.Analyzer.RequestTimeoutMS)
	}
}

func TestLoadUsesFetcherModeWhenExplicitlyRequested(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("PARSE_MODE", "fetcher")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Fetcher.URLDirect {
		t.Fatal("Fetcher.URLDirect = true, want false when PARSE_MODE=fetcher")
	}
}

func TestLoadRejectsUnknownParseMode(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("PARSE_MODE", "auto")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want invalid PARSE_MODE error")
	}
	if !strings.Contains(err.Error(), "PARSE_MODE") || !strings.Contains(err.Error(), "fetcher") || !strings.Contains(err.Error(), "grok_direct") {
		t.Fatalf("error = %q, want supported PARSE_MODE values", err.Error())
	}
}

func TestLoadRejectsInvalidIntegerConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("READ_TIMEOUT_MS", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want invalid integer configuration error")
	}
	if err.Error() != `READ_TIMEOUT_MS must be a valid integer` {
		t.Fatalf("error = %q, want %q", err.Error(), "READ_TIMEOUT_MS must be a valid integer")
	}
}

func TestLoadRejectsNonPositiveTimeoutAndConcurrencyConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("PARSE_CONCURRENCY", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want non-positive configuration error")
	}
	if err.Error() != `PARSE_CONCURRENCY must be greater than 0` {
		t.Fatalf("error = %q, want %q", err.Error(), "PARSE_CONCURRENCY must be greater than 0")
	}
}

func TestLoadAppliesPoolSizingDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.DB.MaxConns != 30 {
		t.Fatalf("DB.MaxConns = %d, want 30", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns != 5 {
		t.Fatalf("DB.MinConns = %d, want 5", cfg.DB.MinConns)
	}
}

func TestLoadReadsPoolSizingOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("DB_MAX_CONNS", "50")
	t.Setenv("DB_MIN_CONNS", "10")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.DB.MaxConns != 50 || cfg.DB.MinConns != 10 {
		t.Fatalf("pool sizing = (%d/%d), want (50/10)", cfg.DB.MaxConns, cfg.DB.MinConns)
	}
}

// TestLoadAcceptsCustomShutdownTimeout 锁定 Wave 5 P2 SHUTDOWN_TIMEOUT_MS
// 覆盖：自定义值能正常生效，且必须 >= 1000ms。
func TestLoadAcceptsCustomShutdownTimeout(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("SHUTDOWN_TIMEOUT_MS", "45000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.ShutdownTimeoutMS != 45000 {
		t.Fatalf("ShutdownTimeoutMS = %d, want 45000", cfg.Server.ShutdownTimeoutMS)
	}
}

// TestLoadRejectsTooShortShutdownTimeout 锁定下界：< 1000ms 的预算无法
// 完成 worker 排空（阶段 2 拿不到 300ms），Load 必须 fail-fast。
func TestLoadRejectsTooShortShutdownTimeout(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("SHUTDOWN_TIMEOUT_MS", "500")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want SHUTDOWN_TIMEOUT_MS lower bound rejection")
	}
	if !strings.Contains(err.Error(), "SHUTDOWN_TIMEOUT_MS") {
		t.Fatalf("error %q should mention SHUTDOWN_TIMEOUT_MS", err.Error())
	}
}

func TestLoadRejectsMinAboveMaxPoolSize(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("DB_MAX_CONNS", "10")
	t.Setenv("DB_MIN_CONNS", "20")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want DB_MIN_CONNS > DB_MAX_CONNS error")
	}
}

func setBaseConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
}

func TestLoadIgnoresRetiredNoOpKnobs(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("DB_MAX_CONNS", "4")
	t.Setenv("DB_MIN_CONNS", "1")
	t.Setenv("PARSE_CONCURRENCY", "3")
	t.Setenv("IDEMPOTENCY_CAPACITY", "not-an-integer")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() should ignore retired no-op knobs, got %v", err)
	}
}

func TestIdempotencyTTLDefaultsToMiddlewareValue(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdempotencyTTLMS != 0 {
		t.Errorf("IdempotencyTTLMS = %d, want 0 (use middleware default)", cfg.IdempotencyTTLMS)
	}
}

func TestIdempotencyTTLAcceptsExplicitOverride(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("IDEMPOTENCY_TTL_MS", "60000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdempotencyTTLMS != 60000 {
		t.Errorf("IdempotencyTTLMS = %d, want 60000", cfg.IdempotencyTTLMS)
	}
}

func TestIdempotencyRejectsNegativeTTL(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("IDEMPOTENCY_TTL_MS", "-1")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "IDEMPOTENCY_TTL_MS") {
		t.Fatalf("err = %v, want IDEMPOTENCY_TTL_MS validation failure", err)
	}
}

func TestLoadRequiresExtensionAPIToken(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("EXTENSION_API_TOKEN", "")

	_, err := Load()
	if err == nil || err.Error() != "EXTENSION_API_TOKEN is required" {
		t.Fatalf("Load() error = %v, want EXTENSION_API_TOKEN requirement", err)
	}
}
