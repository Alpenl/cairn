// Package migrate provisions the current Cairn schema and upgrades the one
// production baseline supported by this release. It is forward-only: an empty
// database installs the fresh baseline schema, the exact v0.1.17 ledger runs one
// aggregate upgrade, and the current single-head ledger is a no-op.
package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/database"
)

// Step is the schema target exposed to release tooling. The shipped plan has
// one transactional step; production-only conversion SQL is selected from the
// verified ledger state and is not exposed as additional targets.
type Step struct {
	ID           string
	SQL          []string
	OnlineUpdate OnlineUpdateReview
}

type txBeginner interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// FreshInstallTarget explicitly requires an empty application schema ledger.
const FreshInstallTarget = "fresh"

// Steps returns a copy of the single-head release plan.
func Steps() []Step {
	return slices.Clone(steps)
}

// Up installs or upgrades the database to CurrentSchemaMigrationID.
func Up(ctx context.Context, db database.Querier) error {
	_, err := Run(ctx, db, RunRequest{})
	return err
}

// UpFreshInstall provisions a known-empty database and refuses every non-empty
// application schema ledger.
func UpFreshInstall(ctx context.Context, db database.Querier) error {
	_, err := Run(ctx, db, RunRequest{Target: FreshInstallTarget})
	return err
}

// RunRequest selects the default, explicit fresh, or exact current target.
type RunRequest struct {
	Target string
}

// RunResult is the machine-readable evidence emitted by cmd/migrate.
type RunResult struct {
	Mode            string   `json:"mode"`
	Target          string   `json:"target"`
	StartVersion    string   `json:"start_version"`
	EndVersion      string   `json:"end_version"`
	Applied         []string `json:"applied"`
	AlreadyAtTarget bool     `json:"already_at_target"`
}

// Run validates the target and ledger before River or application DDL runs.
func Run(ctx context.Context, db database.Querier, request RunRequest) (RunResult, error) {
	target := strings.TrimSpace(request.Target)
	result := RunResult{Mode: "default"}
	switch target {
	case "":
	case FreshInstallTarget:
		result.Mode = "fresh"
	case CurrentSchemaMigrationID:
		result.Mode = "target"
		result.Target = target
	default:
		return RunResult{}, fmt.Errorf("%w %q; known target: %s", ErrUnknownTarget, target, CurrentSchemaMigrationID)
	}
	if db == nil {
		return RunResult{}, fmt.Errorf("nil querier")
	}

	err := withMigrationRunnerSession(ctx, db, func(sessionDB database.Querier) error {
		return runLocked(ctx, sessionDB, result.Mode == "fresh", &result)
	})
	return result, err
}

func runLocked(ctx context.Context, db database.Querier, freshOnly bool, result *RunResult) error {
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	ledger, err := classifySchemaLedger(applied)
	if err != nil {
		return err
	}
	if freshOnly && ledger.kind != schemaLedgerFresh {
		return fmt.Errorf("%w: fresh install requires an empty schema ledger; found %s",
			ErrLedgerAhead, versionLabel(ledger.startVersion))
	}
	result.StartVersion = ledger.startVersion
	result.EndVersion = ledger.startVersion
	if ledger.kind == schemaLedgerCurrent {
		result.AlreadyAtTarget = result.Mode == "target"
	}

	// River owns its own ledger. Run it even when the Cairn schema is current so
	// a separately interrupted River migration can converge on the next retry.
	if err := maybeRunRiverMigrations(ctx, db); err != nil {
		return err
	}
	if ledger.kind == schemaLedgerCurrent {
		return nil
	}

	step := steps[0]
	if ledger.kind == schemaLedgerProductionBaseline {
		step.SQL = productionUpgradeSQL()
	}
	txDB, _ := db.(txBeginner)
	if err := applyStep(ctx, db, txDB, step); err != nil {
		return err
	}
	result.Applied = []string{CurrentSchemaMigrationID}
	result.EndVersion = CurrentSchemaMigrationID
	return nil
}

func versionLabel(version string) string {
	if version == "" {
		return "an empty ledger"
	}
	return version
}

// AppliedVersions returns the raw application ledger in a stable order. The
// exact production baseline retains its deployed order; every other shape is
// sorted so fail-closed reports remain deterministic.
func AppliedVersions(ctx context.Context, db database.Querier) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("nil querier")
	}
	present, err := tolerateMissingLedger(ctx, db)
	if err != nil {
		return nil, err
	}
	if !present {
		return []string{}, nil
	}
	applied, err := loadAppliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	if versionSetEquals(applied, productionBaselineLedger) {
		return ProductionBaselineVersions(), nil
	}
	versions := make([]string, 0, len(applied))
	for version := range applied {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions, nil
}

func tolerateMissingLedger(ctx context.Context, db database.Querier) (bool, error) {
	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	return present, nil
}

func loadAppliedMigrations(ctx context.Context, db database.Querier) (map[string]struct{}, error) {
	rows, err := db.Query(ctx, `SELECT version FROM public.schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

func applyStep(ctx context.Context, db database.Querier, txDB txBeginner, step Step) error {
	if txDB == nil {
		return execStepStatements(ctx, db, step)
	}
	tx, err := txDB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx for migration %s: %w", step.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := execStepStatements(ctx, tx, step); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", step.ID, err)
	}
	return nil
}

type stepExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func execStepStatements(ctx context.Context, executor stepExecutor, step Step) error {
	for index, statement := range step.SQL {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := executor.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %s statement %d: %w", step.ID, index+1, err)
		}
	}
	if _, err := executor.Exec(ctx,
		`INSERT INTO public.schema_migrations(version) VALUES ($1)`, step.ID); err != nil {
		return fmt.Errorf("record migration %s: %w", step.ID, err)
	}
	return nil
}
