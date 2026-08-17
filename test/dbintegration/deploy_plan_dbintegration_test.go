//go:build dbintegration

package dbintegration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/deployplan"
	"webtag/internal/migrate"
)

// The deployment helper's preflight asks the *target release's* migrate binary
// whether a range may be applied, and refuses before the service is stopped if
// the answer is no. Everything in this file drives that exact path for real: a
// freshly built cmd/migrate binary, invoked as a subprocess by
// internal/deployplan, against a real PostgreSQL container.
//
// The property that matters most is the one a fixture script can only assert
// about itself — that --plan-json changes nothing. It is checked here by
// measuring the database before and after.

var (
	migrateBinaryOnce sync.Once
	migrateBinaryPath string
	migrateBinaryDir  string
	migrateBinaryErr  error
)

// TestMain removes the shared build once every case has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if migrateBinaryDir != "" {
		_ = os.RemoveAll(migrateBinaryDir)
	}
	os.Exit(code)
}

// buildMigrateBinary compiles the real cmd/migrate the way a release would.
// Building it once per test binary keeps the cost off each individual case.
func buildMigrateBinary(t *testing.T) string {
	t.Helper()
	migrateBinaryOnce.Do(func() {
		// Deliberately not t.TempDir: that directory is removed when the first
		// case finishes, and this binary is built once and shared by every case
		// in the package.
		dir, err := os.MkdirTemp("", "cairn-migrate-under-test-")
		if err != nil {
			migrateBinaryErr = fmt.Errorf("create build directory: %w", err)
			return
		}
		migrateBinaryDir = dir
		path := filepath.Join(dir, "migrate")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		build := exec.CommandContext(ctx, "go", "build", "-o", path, "./cmd/migrate")
		build.Dir = "../.."
		if output, err := build.CombinedOutput(); err != nil {
			migrateBinaryErr = fmt.Errorf("build cmd/migrate: %w\n%s", err, output)
			return
		}
		migrateBinaryPath = path
	})
	if migrateBinaryErr != nil {
		t.Fatalf("%v", migrateBinaryErr)
	}
	return migrateBinaryPath
}

// execRunner is the deployplan.Runner the helper uses in production, restated
// here because the helper's own copy lives in a main package.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args, env []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		command.Env = env
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

func terminalStep(t *testing.T) string {
	t.Helper()
	steps := migrate.Steps()
	if len(steps) == 0 {
		t.Fatal("the compiled migration plan is empty")
	}
	return steps[len(steps)-1].ID
}

// databaseFingerprint is everything a plan run must leave untouched.
type databaseFingerprint struct {
	ledger    []string
	tables    int
	riverHead int
}

func fingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) databaseFingerprint {
	t.Helper()
	applied, err := migrate.AppliedVersions(ctx, pool)
	if err != nil {
		t.Fatalf("read applied versions: %v", err)
	}
	var tables int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	var riverHead int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM public.river_migration`).Scan(&riverHead); err != nil {
		t.Fatalf("read river ledger: %v", err)
	}
	return databaseFingerprint{ledger: applied, tables: tables, riverHead: riverHead}
}

// TestThePlanChangesNothing is the claim the whole preflight ordering rests on.
// The helper runs this against production before it stops the service, so a
// plan that quietly migrated would be an unannounced maintenance window.
func TestThePlanChangesNothing(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	before := fingerprint(t, ctx, pool)
	planner := deployplan.New(execRunner{}, buildMigrateBinary(t), DSN(t))

	report, err := planner.Plan(ctx, terminalStep(t))
	if err != nil {
		t.Fatalf("plan against a real database: %v", err)
	}
	if !report.PlanOnly {
		t.Fatal("the real binary did not mark its answer as plan-only")
	}
	if len(report.Applied) != 0 {
		t.Fatalf("a plan run reported applying %v", report.Applied)
	}
	if report.Changed() {
		t.Fatal("a plan run reported changing the database")
	}

	after := fingerprint(t, ctx, pool)
	if strings.Join(before.ledger, ",") != strings.Join(after.ledger, ",") {
		t.Fatalf("the plan moved schema_migrations: %v -> %v", before.ledger, after.ledger)
	}
	if before.tables != after.tables {
		t.Fatalf("the plan changed the table count: %d -> %d", before.tables, after.tables)
	}
	if before.riverHead != after.riverHead {
		t.Fatalf("the plan moved the River ledger: %d -> %d", before.riverHead, after.riverHead)
	}
}

// TestAPlanOnAMigratedDatabaseIsAllowedAndEmpty is the "already at target"
// case: nothing is pending, so nothing can be unsafe.
func TestAPlanOnAMigratedDatabaseIsAllowedAndEmpty(t *testing.T) {
	StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	planner := deployplan.New(execRunner{}, buildMigrateBinary(t), DSN(t))
	report, err := planner.Plan(ctx, terminalStep(t))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if report.Error != nil {
		t.Fatalf("the plan reported an error: %+v", report.Error)
	}
	blocked, blockers := report.Blocked()
	if blocked {
		t.Fatalf("a database already at target was refused: %+v", blockers)
	}
	if len(report.OnlineUpdate.Pending) != 0 {
		t.Fatalf("expected an empty pending range, got %v", report.OnlineUpdate.Pending)
	}
	// The plan also reconciles the ledgers as they stand. A fully migrated
	// database is exactly at both targets and has overshot neither.
	if report.Ledgers == nil {
		t.Fatal("the plan produced no ledger reconciliation")
	}
	if report.Ledgers.Overshot() {
		t.Fatalf("a freshly migrated database was reported as overshot: %+v", report.Ledgers)
	}
	if !report.Ledgers.Schema.AtTarget || !report.Ledgers.River.AtTarget {
		t.Fatalf("a freshly migrated database is not at target: %+v", report.Ledgers)
	}
}

// TestAPlanOnAnEmptyDatabaseIsRefusedByTheRealClassification is the refusal the
// helper must land on before quiesce. The fresh-install step is reviewed
// OnlineIncompatible, so a page-triggered update may never apply it.
func TestAPlanOnAnEmptyDatabaseIsRefusedByTheRealClassification(t *testing.T) {
	StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	emptyDSN := createScratchDatabase(t, ctx)
	planner := deployplan.New(execRunner{}, buildMigrateBinary(t), emptyDSN)

	report, err := planner.Plan(ctx, terminalStep(t))
	if err != nil {
		t.Fatalf("plan against an empty database: %v", err)
	}
	if !report.PlanOnly || len(report.Applied) != 0 {
		t.Fatalf("the plan was not plan-only: %+v", report)
	}
	blocked, blockers := report.Blocked()
	if !blocked {
		t.Fatal("an empty database was declared eligible for a page-triggered update")
	}
	if len(blockers) == 0 {
		t.Fatal("a refusal must name the steps that caused it")
	}
	// Every blocker carries a real classification and a real explanation, which
	// is what the settings page renders verbatim.
	for _, blocker := range blockers {
		switch blocker.Reason {
		case deployplan.BlockIncompatible, deployplan.BlockManualGate, deployplan.BlockUnclassified:
		default:
			t.Fatalf("blocker %s carries an unknown reason %q", blocker.StepID, blocker.Reason)
		}
		if strings.TrimSpace(blocker.Detail) == "" {
			t.Fatalf("blocker %s has no explanation for the operator", blocker.StepID)
		}
	}
	t.Logf("real blockers: %+v", blockers)

	// And the plan still changed nothing: the empty database is still empty.
	var tables int
	pool, err := pgxpool.New(ctx, emptyDSN)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 0 {
		t.Fatalf("planning against an empty database created %d tables", tables)
	}
}

// TestAnUnknownTargetIsRefusedByTheRealBinary is the mis-built-release case:
// the signed manifest names a schema target the shipped binary cannot produce.
func TestAnUnknownTargetIsRefusedByTheRealBinary(t *testing.T) {
	StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	planner := deployplan.New(execRunner{}, buildMigrateBinary(t), DSN(t))
	report, err := planner.Plan(ctx, "astepthisbinarydoesnotdefine2099010101")
	// The binary exits non-zero, but the report is what the helper classifies.
	if report == nil {
		t.Fatalf("an unknown target produced no report: %v", err)
	}
	if report.Error == nil {
		t.Fatal("an unknown target was not reported as an error")
	}
	if report.Error.Kind != deployplan.ErrorUnknownTarget {
		t.Fatalf("expected %q, got %q (%s)", deployplan.ErrorUnknownTarget, report.Error.Kind, report.Error.Message)
	}
	if report.Changed() {
		t.Fatal("a refused plan reported changing the database")
	}
}

// TestAnUnreachableDatabaseIsAPlanFailureNotASilentPass proves the helper's
// environment hold has something real behind it.
func TestAnUnreachableDatabaseIsAPlanFailureNotASilentPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	planner := deployplan.New(execRunner{}, buildMigrateBinary(t),
		"postgres://nobody:nothing@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=2")
	report, err := planner.Plan(ctx, terminalStep(t))
	if err == nil && (report == nil || report.Error == nil) {
		t.Fatalf("an unreachable database produced no failure: %+v", report)
	}
	// The failure has to be about the database, not about the helper failing to
	// run the binary at all — otherwise this test would pass for the wrong
	// reason on a machine where the build silently produced nothing.
	message := ""
	if report != nil && report.Error != nil {
		message = report.Error.Message
	} else if err != nil {
		message = err.Error()
	}
	if strings.Contains(message, "no such file or directory") && strings.Contains(message, "migrate") {
		t.Fatalf("the migrate binary was not runnable, so this proved nothing: %s", message)
	}
	t.Logf("unreachable database reported as: %s", message)
}

// createScratchDatabase makes an empty database in the shared container and
// returns a DSN for it.
func createScratchDatabase(t *testing.T, ctx context.Context) string {
	t.Helper()
	name := fmt.Sprintf("plan_scratch_%d", time.Now().UnixNano())
	adminDSN := DSN(t)
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		dropPool, err := pgxpool.New(dropCtx, adminDSN)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}
