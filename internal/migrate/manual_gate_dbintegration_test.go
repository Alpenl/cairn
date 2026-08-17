//go:build dbintegration

package migrate

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The shipped plan contains no manual step, so these tests install a synthetic
// three-step plan with a release gate in the middle. Every assertion below is
// still made against a real PostgreSQL: what is under test is that the refusal
// happens before anything commits, and only a real server can prove the ledger
// and the tables it would have created are genuinely untouched.
func manualGatePlan() []Step {
	return []Step{
		{
			ID:           "manualgateprobe_first",
			OnlineUpdate: OnlineCompatible("test fixture"),
			SQL:          []string{`CREATE TABLE IF NOT EXISTS public.manual_gate_probe_first(id integer)`},
		},
		{
			ID:           "manualgateprobe_gate",
			Manual:       true,
			OnlineUpdate: OnlineIncompatible("test fixture: release-gated"),
			SQL:          []string{`CREATE TABLE IF NOT EXISTS public.manual_gate_probe_gate(id integer)`},
		},
		{
			ID:           "manualgateprobe_after",
			OnlineUpdate: OnlineCompatible("test fixture"),
			SQL:          []string{`CREATE TABLE IF NOT EXISTS public.manual_gate_probe_after(id integer)`},
		},
	}
}

// TestExactTargetRefusesToCrossAManualGateAgainstRealPostgres is the rule that
// keeps issue #41's page-triggered update honest: the helper never sets
// AllowManual, so a release-gated step standing between the current ledger and
// the manifest's target stops the run instead of being crossed silently.
func TestExactTargetRefusesToCrossAManualGateAgainstRealPostgres(t *testing.T) {
	pool := isolatedMigrationPool(t)
	withPlan(t, manualGatePlan())
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	first, err := Run(ctx, pool, RunRequest{Target: "manualgateprobe_first"})
	if err != nil {
		t.Fatalf("Run(first) error = %v", err)
	}
	if want := []string{"manualgateprobe_first"}; !slices.Equal(first.Applied, want) {
		t.Fatalf("applied = %v, want %v", first.Applied, want)
	}

	_, err = Run(ctx, pool, RunRequest{Target: "manualgateprobe_after"})
	if !errors.Is(err, ErrManualStepInRange) {
		t.Fatalf("Run(after) error = %v, want ErrManualStepInRange", err)
	}
	if !strings.Contains(err.Error(), "manualgateprobe_gate") {
		t.Fatalf("refusal does not name the gate: %v", err)
	}
	if !strings.Contains(err.Error(), "MIGRATION_ALLOW_MANUAL=true") {
		t.Fatalf("refusal does not tell the operator how to proceed: %v", err)
	}

	// Nothing past the gate may have been created or recorded.
	assertLedger(t, pool, []string{"manualgateprobe_first"})
	assertRelation(t, pool, "public.manual_gate_probe_gate", false)
	assertRelation(t, pool, "public.manual_gate_probe_after", false)

	// The same refusal must be visible to a pre-flight check that never
	// touches the database, so the helper can decide before it quiesces.
	plan, err := PlanOnlineUpdate([]string{"manualgateprobe_first"}, "manualgateprobe_after")
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if plan.Allowed {
		t.Fatal("PlanOnlineUpdate allowed a range containing a manual gate")
	}
	// The gate is also reviewed incompatible, so both reasons must surface:
	// clearing the manual approval alone would not make the range online-safe,
	// and the operator has to see that.
	reasons := make(map[OnlineUpdateBlockReason]struct{}, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		if blocker.StepID != "manualgateprobe_gate" {
			t.Fatalf("unexpected blocker %+v", blocker)
		}
		reasons[blocker.Reason] = struct{}{}
	}
	for _, want := range []OnlineUpdateBlockReason{OnlineUpdateBlockManual, OnlineUpdateBlockIncompatible} {
		if _, ok := reasons[want]; !ok {
			t.Fatalf("blockers = %+v, want one with reason %q", plan.Blockers, want)
		}
	}
}

// TestExactTargetCrossesAManualGateWithOperatorApproval is the other half: an
// operator who has satisfied the rollout preconditions can still get through,
// which is what keeps the gate a gate rather than a wall.
func TestExactTargetCrossesAManualGateWithOperatorApproval(t *testing.T) {
	pool := isolatedMigrationPool(t)
	withPlan(t, manualGatePlan())
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	result, err := Run(ctx, pool, RunRequest{Target: "manualgateprobe_after", AllowManual: true})
	if err != nil {
		t.Fatalf("Run(AllowManual) error = %v", err)
	}
	want := []string{"manualgateprobe_first", "manualgateprobe_gate", "manualgateprobe_after"}
	if !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want %v", result.Applied, want)
	}
	assertLedger(t, pool, want)
	assertRelation(t, pool, "public.manual_gate_probe_gate", true)
	assertRelation(t, pool, "public.manual_gate_probe_after", true)
}

// TestDefaultRunStopsAtTheManualGateAndSaysSo covers the release-gated default
// path: it still stops successfully before the gate, but it no longer does so
// in silence.
func TestDefaultRunStopsAtTheManualGateAndSaysSo(t *testing.T) {
	pool := isolatedMigrationPool(t)
	withPlan(t, manualGatePlan())
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	result, err := Run(ctx, pool, RunRequest{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := []string{"manualgateprobe_first"}; !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want %v", result.Applied, want)
	}
	if result.StoppedAtManual != "manualgateprobe_gate" {
		t.Fatalf("StoppedAtManual = %q, want the pending gate", result.StoppedAtManual)
	}
	assertRelation(t, pool, "public.manual_gate_probe_gate", false)
}

func assertLedger(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	got, err := AppliedVersions(t.Context(), pool)
	if err != nil {
		t.Fatalf("AppliedVersions() error = %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("schema_migrations = %v, want %v", got, want)
	}
}

func assertRelation(t *testing.T, pool *pgxpool.Pool, qualifiedName string, want bool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, qualifiedName).Scan(&present); err != nil {
		t.Fatalf("probe %s: %v", qualifiedName, err)
	}
	if present != want {
		t.Fatalf("relation %s exists = %t, want %t", qualifiedName, present, want)
	}
}

// isolatedMigrationPool creates a throwaway database on the configured test
// server. A synthetic plan cannot share the shared database: its ledger already
// holds the real step IDs, and the exact-target contract rejects a ledger whose
// versions the running plan does not define.
func isolatedMigrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WEBTAG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WEBTAG_TEST_DATABASE_URL is not configured")
	}
	baseURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WEBTAG_TEST_DATABASE_URL: %v", err)
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	adminPool, err := pgxpool.New(t.Context(), adminURL.String())
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}

	databaseName := "manualgate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(t.Context(), `CREATE DATABASE `+quotedName); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, databaseName)
		if _, err := adminPool.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+quotedName); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	targetURL := *baseURL
	targetURL.Path = "/" + databaseName
	pool, err := pgxpool.New(t.Context(), targetURL.String())
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
