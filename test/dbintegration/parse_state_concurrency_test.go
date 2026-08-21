package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func openNamedPool(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = postgresApplicationName(name)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open named pool %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// PostgreSQL stores application_name in a 64-byte field including its NUL
// terminator. Keep the observer and the connection startup parameter in sync
// when a test's descriptive prefix and UUID would otherwise exceed 63 bytes.
func postgresApplicationName(name string) string {
	const maxApplicationNameBytes = 63
	if len(name) <= maxApplicationNameBytes {
		return name
	}
	clipped := name[:maxApplicationNameBytes]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

func TestPostgresApplicationNamePreservesUTF8Boundary(t *testing.T) {
	name := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa你"
	got := postgresApplicationName(name)
	if len(got) != 62 || !utf8.ValidString(got) {
		t.Fatalf("postgresApplicationName(%q) = %q (%d bytes), want valid 62-byte prefix", name, got, len(got))
	}
}

func waitForPostgresLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applicationName string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	applicationName = postgresApplicationName(applicationName)
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE application_name = $1 AND wait_event_type = 'Lock'
			)`, applicationName).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe %s lock wait: %v", applicationName, err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never entered a PostgreSQL lock wait: %v", applicationName, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertNotDeadlock(t *testing.T, operation string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("%s hit a PostgreSQL deadlock: %v", operation, err)
	}
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s did not finish after lock release: %v", operation, err)
	}
}
