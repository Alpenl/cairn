package config

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("READER_CURSOR_SIGNING_KEY", testReaderCursorKeyA); err != nil {
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
	if cfg.CursorSigningKey != "" {
		t.Fatalf("CursorSigningKey = %q, want blank Link cursor compatibility mode", cfg.CursorSigningKey)
	}
	if cfg.ReaderCursorSigningKey == "" {
		t.Fatal("ReaderCursorSigningKey is empty; default replicas need a stable signing key")
	}
	if cfg.ReaderCursorSigningKey != testReaderCursorKeyA {
		t.Fatalf("ReaderCursorSigningKey = %q, want dedicated explicit key", cfg.ReaderCursorSigningKey)
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

func TestLoadRejectsMissingReaderCursorSigningKeyOutsideDev(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("READER_CURSOR_SIGNING_KEY", "")
	t.Setenv("CURSOR_SIGNING_KEY", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "READER_CURSOR_SIGNING_KEY or CURSOR_SIGNING_KEY") {
		t.Fatalf("Load() error = %v, want explicit Reader cursor signing key requirement", err)
	}
}

func TestLoadKeepsPlaintextLinkCompatibilityWithDedicatedReaderKey(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("READER_CURSOR_SIGNING_KEY", testReaderCursorKeyA)
	t.Setenv("CURSOR_SIGNING_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CursorSigningKey != "" {
		t.Fatalf("CursorSigningKey = %q, want blank Link compatibility mode", cfg.CursorSigningKey)
	}
	if cfg.ReaderCursorSigningKey != testReaderCursorKeyA {
		t.Fatalf("ReaderCursorSigningKey = %q, want dedicated key", cfg.ReaderCursorSigningKey)
	}
}

func TestLoadAllowsDevelopmentReaderCursorFallback(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("READER_CURSOR_SIGNING_KEY", "")
	t.Setenv("CURSOR_SIGNING_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := deriveDevelopmentCursorSigningKey(cfg.DatabaseURL, cfg.Analyzer.APIKey)
	if cfg.ReaderCursorSigningKey != want {
		t.Fatalf("ReaderCursorSigningKey = %q, want development fallback %q", cfg.ReaderCursorSigningKey, want)
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
	t.Setenv("AI_ALLOW_UNSAFE_TARGETS", "true")

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
	if !cfg.Analyzer.AllowUnsafeTargets {
		t.Fatal("Analyzer.AllowUnsafeTargets = false, want true")
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

// TestLoadReadsStructuredOutputEscapeHatch covers the env→config leg of
// AI_DISABLE_STRUCTURED_OUTPUT. Without it the only coverage stops at
// OpenAIAnalyzerOptions, so a typo in the env name here would leave the
// escape hatch permanently dead with every test still green — and this is
// the switch an operator reaches for when a gateway breaks tagging.
func TestLoadReadsStructuredOutputEscapeHatch(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
		t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
		t.Setenv("AI_API_KEY", "test-key")
		t.Setenv("AI_MODEL", "gpt-4.1-mini")
	}

	t.Run("defaults to enabled", func(t *testing.T) {
		base(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Analyzer.DisableStructuredOutput {
			t.Fatal("DisableStructuredOutput = true with the env unset, want false (structured output on by default)")
		}
	})

	t.Run("opt out", func(t *testing.T) {
		base(t)
		t.Setenv("AI_DISABLE_STRUCTURED_OUTPUT", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.Analyzer.DisableStructuredOutput {
			t.Fatal("AI_DISABLE_STRUCTURED_OUTPUT=true did not reach Analyzer.DisableStructuredOutput")
		}
	})

	t.Run("rejects non-boolean", func(t *testing.T) {
		base(t)
		t.Setenv("AI_DISABLE_STRUCTURED_OUTPUT", "not-bool")
		if _, err := Load(); err == nil {
			t.Fatal("Load() error = nil, want invalid boolean error")
		}
	})
}

func TestLoadRejectsInvalidBooleanConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("AI_ALLOW_UNSAFE_TARGETS", "not-bool")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want invalid boolean configuration error")
	}
	if err.Error() != `AI_ALLOW_UNSAFE_TARGETS must be a valid boolean` {
		t.Fatalf("error = %q, want %q", err.Error(), "AI_ALLOW_UNSAFE_TARGETS must be a valid boolean")
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

// TestLoadOTelDefaults 锁定 OTel SDK 接入的三个 env 默认值：
// endpoint 空（走 noop tracer）、ratio=0.05（OTel 社区推荐生产档位）、
// insecure=true（本地 collector 友好；prod 应显式设 false）。
func TestLoadOTelDefaults(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OTELEndpoint != "" {
		t.Errorf("OTELEndpoint = %q, want empty (noop default)", cfg.OTELEndpoint)
	}
	if cfg.OTELSamplingRatio != 0.05 {
		t.Errorf("OTELSamplingRatio = %v, want 0.05", cfg.OTELSamplingRatio)
	}
	if !cfg.OTELInsecure {
		t.Error("OTELInsecure = false, want true (local-friendly default)")
	}
}

// TestLoadRejectsOTelSamplingOutOfRange 锁定 ratio 必须在 [0,1] 内。
// 双层防御中的外层（validateConfig 拒绝越界），内层是 InitTracer 内部
// clamp 兜底。
func TestLoadRejectsOTelSamplingOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"negative", "-0.1"},
		{"above_one", "1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("OTEL_SAMPLING_RATIO", tc.val)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want OTEL_SAMPLING_RATIO=%s rejection", tc.val)
			}
			if !strings.Contains(err.Error(), "OTEL_SAMPLING_RATIO") {
				t.Fatalf("error %q should mention OTEL_SAMPLING_RATIO", err.Error())
			}
		})
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
	t.Setenv("BATCH_SUBMIT_CONCURRENCY", "not-an-integer")
	t.Setenv("IDEMPOTENCY_CAPACITY", "not-an-integer")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() should ignore retired no-op knobs, got %v", err)
	}
}

// TestDeprecatedEnvsIgnoredButDetected locks the v3 retirement contract:
// retired concept-normalization env vars are still tolerated (Load
// does not fail-fast on a stale .env carrying them) but are surfaced in
// Config.DeprecatedEnvsSet so the boot path can WARN. Order matches
// declaration order in deprecatedEnvVars.
func TestDeprecatedEnvsIgnoredButDetected(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("WIKIDATA_ENABLED", "true")
	// A placeholder UA that the old validator would have rejected — proves
	// the retired validation no longer fires.
	t.Setenv("WIKIDATA_USER_AGENT", "MyApp/1.0 (placeholder)")
	t.Setenv("CONCEPT_JUDGE_ENABLED", "true")
	t.Setenv("CONCEPT_JUDGE_INTERVAL_MS", "0") // would have failed validation pre-v3
	t.Setenv("CONCEPT_RETRIEVE_TOP_K", "not-an-integer")
	t.Setenv("CONCEPT_AUTO_MERGE_THRESHOLD", "not-a-number")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load should ignore deprecated config, got err: %v", err)
	}

	want := map[string]bool{
		"WIKIDATA_ENABLED":             true,
		"WIKIDATA_USER_AGENT":          true,
		"CONCEPT_JUDGE_ENABLED":        true,
		"CONCEPT_JUDGE_INTERVAL_MS":    true,
		"CONCEPT_RETRIEVE_TOP_K":       true,
		"CONCEPT_AUTO_MERGE_THRESHOLD": true,
	}
	got := make(map[string]bool, len(cfg.DeprecatedEnvsSet))
	for _, name := range cfg.DeprecatedEnvsSet {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("DeprecatedEnvsSet missing %q; got %v", name, cfg.DeprecatedEnvsSet)
		}
	}
	// Sanity: unset deprecated vars must not appear.
	for _, name := range cfg.DeprecatedEnvsSet {
		if name == "WIKIDATA_BASE_URL" || name == "CONCEPT_JUDGE_BATCH_SIZE" {
			t.Errorf("DeprecatedEnvsSet reported %q which was never set", name)
		}
	}
}

// TestDeprecatedEnvsEmptyByDefault confirms a clean environment reports no
// deprecated vars (so the boot path stays quiet).
func TestDeprecatedEnvsEmptyByDefault(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DeprecatedEnvsSet) != 0 {
		t.Errorf("DeprecatedEnvsSet = %v, want empty on a clean env", cfg.DeprecatedEnvsSet)
	}
}

// TestIdempotencyDefaultsEnabled locks the live idempotency defaults.
func TestIdempotencyDefaultsEnabled(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IdempotencyEnabled {
		t.Error("IdempotencyEnabled = false, want true (default-on per round-2 audit)")
	}
	if cfg.IdempotencyTTLMS != 0 {
		t.Errorf("IdempotencyTTLMS = %d, want 0 (use middleware default)", cfg.IdempotencyTTLMS)
	}
}

// TestIdempotencyAcceptsExplicitOverrides 锁定 env 覆盖路径。
func TestIdempotencyAcceptsExplicitOverrides(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("IDEMPOTENCY_ENABLED", "false")
	t.Setenv("IDEMPOTENCY_TTL_MS", "60000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdempotencyEnabled {
		t.Error("IdempotencyEnabled = true after explicit false")
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

func TestSiteMigrationDefaultsDisabledAndDryRun(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SiteMigrationEnabled || !cfg.SiteMigrationDryRun || cfg.SiteMigrationBatch != 100 || cfg.SiteMigrationIntervalMS != 900000 {
		t.Fatalf("site migration defaults = %#v", cfg)
	}
}

func TestWebsiteCollectionFeatureFlagsDefaultEnabledAndParseOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LibraryKindAPIEnabled || !cfg.SiteLibraryWriteEnabled || !cfg.SiteAutoClassificationEnabled || !cfg.SiteAdvancedManagementEnabled {
		t.Fatalf("website flags should default enabled: %#v", cfg)
	}
	t.Setenv("SITE_LIBRARY_WRITE_ENABLED", "false")
	t.Setenv("SITE_AUTO_CLASSIFICATION_ENABLED", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SiteLibraryWriteEnabled || cfg.SiteAutoClassificationEnabled {
		t.Fatalf("website flag overrides ignored: %#v", cfg)
	}
}

// TestPublicAPIOpenDefaultsToFalse 钉住本轮三个 P0 里最要害的那个默认值。
//
// 在此之前 MODE=single（默认）+ 未配 EXTENSION_API_TOKEN + 库里零把 key =
// 公开 API 对所有人敞开。改成 fail-closed 之后，唯一的证人一度只有
// scripts/container_smoke.sh 里那条 `assert_status /api/links 401`——而它是
// Docker 门控的 job。把默认值改回 true，`go test ./internal/...` 全绿。
//
// 按本仓库审查日志收尾时立的规矩：「判定标准只有一条——去掉它，有没有测试
// 会红。」这条就是那个会红的测试。
func TestPublicAPIOpenDefaultsToFalse(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("AI_BASE_URL", "http://ai.internal/v1")
	t.Setenv("AI_API_KEY", "k")
	t.Setenv("AI_MODEL", "m")
	// 显式清空，避免开发机 .env 或 CI 环境里残留的值让这条测试失去意义。
	t.Setenv("PUBLIC_API_OPEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PublicAPIOpen {
		t.Fatal("PUBLIC_API_OPEN 未设置时必须为 false——默认开放意味着端口一暴露全库可读可删")
	}
}

// 显式设为 true 时必须真的生效，否则这个开关本身就是装饰品。
func TestPublicAPIOpenHonoursExplicitTrue(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("AI_BASE_URL", "http://ai.internal/v1")
	t.Setenv("AI_API_KEY", "k")
	t.Setenv("AI_MODEL", "m")
	t.Setenv("PUBLIC_API_OPEN", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.PublicAPIOpen {
		t.Fatal("PUBLIC_API_OPEN=true 未生效")
	}
}

// TestGzipDefaults 锁定压缩默认值：开启 + 1024 字节下限。
//
// 默认必须是"开"：反代层的 encode 只覆盖有反代的部署，直连 Go 二进制的
// 自托管用户拿不到任何压缩。默认值改动会让那部分用户静默退回裸奔。
func TestGzipDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if !cfg.GzipEnabled {
		t.Fatal("GzipEnabled = false, want true by default")
	}
	if cfg.GzipMinLength != 1024 {
		t.Fatalf("GzipMinLength = %d, want 1024", cfg.GzipMinLength)
	}
}

// TestGzipReadsExplicitOverrides 覆盖两个 env 的解析路径。
func TestGzipReadsExplicitOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("GZIP_ENABLED", "false")
	t.Setenv("GZIP_MIN_LENGTH", "4096")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.GzipEnabled {
		t.Fatal("GzipEnabled = true, want false from GZIP_ENABLED=false")
	}
	if cfg.GzipMinLength != 4096 {
		t.Fatalf("GzipMinLength = %d, want 4096", cfg.GzipMinLength)
	}
}

// TestGzipRejectsNonPositiveMinLength 覆盖 validate.go 里新增的校验分支。
// minLength <= 0 会让中间件回退到默认值，而运维以为自己关掉了阈值——
// fail-fast 比静默回退好。
func TestGzipRejectsNonPositiveMinLength(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")
	t.Setenv("GZIP_MIN_LENGTH", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want failure on GZIP_MIN_LENGTH=0")
	}
	if !strings.Contains(err.Error(), "GZIP_MIN_LENGTH") {
		t.Fatalf("error = %v, want it to name GZIP_MIN_LENGTH", err)
	}
}

// TestTreeCacheDefaultsAndValidation 覆盖域名摘要缓存 TTL 的默认值与校验分支。
func TestTreeCacheDefaultsAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/webtag")
	t.Setenv("AI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("AI_API_KEY", "test-key")
	t.Setenv("AI_MODEL", "gpt-4.1-mini")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.TreeCacheTTLMS != 300000 {
		t.Fatalf("TreeCacheTTLMS = %d, want 300000", cfg.TreeCacheTTLMS)
	}

	t.Setenv("TREE_CACHE_TTL_MS", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TREE_CACHE_TTL_MS") {
		t.Fatalf("Load() error = %v, want it to reject TREE_CACHE_TTL_MS=0", err)
	}
}
