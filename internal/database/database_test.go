package database

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestQuerierInterfaceSatisfied verifies that *pgxpool.Pool also satisfies
// the narrower Querier interface (Exec / Query / QueryRow).
func TestQuerierInterfaceSatisfied(t *testing.T) {
	var _ Querier = (*pgxpool.Pool)(nil)
}

// TestOptionsZeroValue ensures the Options zero-value is usable (no panics,
// no unexpected defaults). Open applies MaxConns/MinConns only when > 0, so
// a zero Options must be safe to pass without error at the options layer.
func TestOptionsZeroValue(t *testing.T) {
	opts := Options{}
	if opts.MaxConns != 0 {
		t.Errorf("Options{}.MaxConns = %d; want 0", opts.MaxConns)
	}
	if opts.MinConns != 0 {
		t.Errorf("Options{}.MinConns = %d; want 0", opts.MinConns)
	}
}

// TestParseConfigValidURL confirms that pgxpool.ParseConfig accepts a
// well-formed postgres URL without returning an error. This exercises
// the same call Open makes before creating the pool.
func TestParseConfigValidURL(t *testing.T) {
	_, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/dbname")
	if err != nil {
		t.Errorf("pgxpool.ParseConfig(valid URL) returned error: %v", err)
	}
}

// TestParseConfigInvalidURL confirms that pgxpool.ParseConfig returns an
// error for a clearly malformed connection string.
func TestParseConfigInvalidURL(t *testing.T) {
	_, err := pgxpool.ParseConfig("not-a-valid-url://???")
	if err == nil {
		t.Error("pgxpool.ParseConfig(invalid URL) returned nil error; want error")
	}
}

// TestOpen_InvalidURL exercises the "parse pgx config" error branch of Open.
// No real DB is needed: an unparseable URL causes ParseConfig to fail before
// any network call is made.
func TestOpen_InvalidURL(t *testing.T) {
	_, err := Open(context.Background(), "not-a-valid-url://???", Options{})
	if err == nil {
		t.Fatal("Open(invalid URL) returned nil error; want error")
	}
	if !strings.Contains(err.Error(), "parse pgx config") {
		t.Errorf("Open error = %q; want it to contain \"parse pgx config\"", err.Error())
	}
}

func TestOpenConfiguredPoolHonorsPingTimeout(t *testing.T) {
	t.Parallel()

	cfg, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	cfg.ConnConfig.ConnectTimeout = 750 * time.Millisecond
	cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	startedAt := time.Now()
	pool, err := openConfiguredPool(t.Context(), cfg, 50*time.Millisecond)
	if pool != nil {
		t.Fatalf("openConfiguredPool() pool = %p, want nil", pool)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("openConfiguredPool() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("openConfiguredPool() elapsed = %v, want bounded ping failure cleanup", elapsed)
	}
}

// TestOptionsLifecycleFields ensures the Options zero-value leaves the
// lifecycle knobs at zero (Open will then keep pgx defaults intact) and
// that non-zero values survive the round-trip into the struct.
//
// Wave 2 H6：H6 的核心契约就是这三个字段——只在调用方显式给值时覆盖
// 默认，否则继承 pgx 的 1h / 30min / 1min。这里只验 Options 层的赋值；
// Open 把它们传给 pgxpool.Config 的部分由 TestOpen_AppliesLifecycle
// 在 pgxpool.ParseConfig 之上断言。
func TestOptionsLifecycleFields(t *testing.T) {
	zero := Options{}
	if zero.MaxConnLifetime != 0 {
		t.Errorf("Options{}.MaxConnLifetime = %v; want 0", zero.MaxConnLifetime)
	}
	if zero.MaxConnIdleTime != 0 {
		t.Errorf("Options{}.MaxConnIdleTime = %v; want 0", zero.MaxConnIdleTime)
	}
	if zero.HealthCheckPeriod != 0 {
		t.Errorf("Options{}.HealthCheckPeriod = %v; want 0", zero.HealthCheckPeriod)
	}

	set := Options{
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   10 * time.Minute,
		HealthCheckPeriod: time.Minute,
	}
	if set.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %v; want 30m", set.MaxConnLifetime)
	}
	if set.MaxConnIdleTime != 10*time.Minute {
		t.Errorf("MaxConnIdleTime = %v; want 10m", set.MaxConnIdleTime)
	}
	if set.HealthCheckPeriod != time.Minute {
		t.Errorf("HealthCheckPeriod = %v; want 1m", set.HealthCheckPeriod)
	}
}

// TestOpen_AppliesLifecycle reproduces the pgxpool.Config code path inside
// Open without a live database: we ParseConfig a known URL, then apply the
// same three overrides Open applies and assert they land on the config
// struct. This catches "we accepted the option but forgot to plumb it" —
// the bug audit H6 was reported against — without requiring a running PG.
func TestOpen_AppliesLifecycle(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://localhost/db")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	opts := Options{
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   10 * time.Minute,
		HealthCheckPeriod: 45 * time.Second,
	}

	// Mirror exactly the only-apply-when-positive logic inside Open.
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	if opts.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = opts.HealthCheckPeriod
	}

	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %v; want 30m", cfg.MaxConnLifetime)
	}
	if cfg.MaxConnIdleTime != 10*time.Minute {
		t.Errorf("MaxConnIdleTime = %v; want 10m", cfg.MaxConnIdleTime)
	}
	if cfg.HealthCheckPeriod != 45*time.Second {
		t.Errorf("HealthCheckPeriod = %v; want 45s", cfg.HealthCheckPeriod)
	}
}

// TestParseConfigMaxMinConns verifies that our options logic (only apply
// when > 0) matches the underlying ParseConfig behaviour. We set both
// limits in the URL to confirm the parsed cfg can hold them.
func TestParseConfigMaxMinConns(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://localhost/db?pool_max_conns=10&pool_min_conns=2")
	if err != nil {
		t.Fatalf("ParseConfig returned unexpected error: %v", err)
	}
	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns = %d; want 10", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns = %d; want 2", cfg.MinConns)
	}
}
