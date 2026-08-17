// Command migrate 是数据库 schema 迁移的独立入口：从 DATABASE_URL 打开连接池，
// 调用 internal/migrate.Run 把迁移应用到目标库。设计为一次性运行：迁移完成即退出，
// 部署流水线在服务启动前调用，避免 webtag server 在线热迁移。
//
// # Environment
//
//	DATABASE_URL                  required.
//	MIGRATION_TARGET              ""    default release-gated run (stops before
//	                                    the first pending manual step);
//	                              fresh  compatibility alias for an empty
//	                                    database, same plan as "";
//	                              <id>   exact step ID — migrate through that
//	                                    step and not one step further.
//	MIGRATION_ALLOW_MANUAL        "true" lets an exact-target run cross a
//	                                    release-gated manual step. This is the
//	                                    human operator's approval; the deploy
//	                                    helper must never set it.
//	MIGRATION_RIVER_LEDGER_TARGET the River ledger version the release manifest
//	                                    declares. Defaults to this binary's
//	                                    River bundle head.
//	MIGRATION_REPORT_JSON         "true" is equivalent to --report-json.
//
// # Flags
//
//	--version                 print identity and exit.
//	--repair-reader-todos     rebuild every TODO projection; runs no migration.
//	--report-json             emit exactly one JSON object on stdout and route
//	                          all human text to stderr.
//
// # Exit contract
//
// A run is silent-on-success no longer: both the human and the JSON form report
// the start version, the end version and the exact step IDs applied, and the
// run fails closed when either ledger did not land on its declared target.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/buildinfo"
	"webtag/internal/database"
	"webtag/internal/migrate"
	"webtag/internal/repository"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

// repairTodoProjectionsFlag is the explicit operator entry point for TODO
// projection drift. Drift repair is deliberately not reachable from any HTTP
// read: rebuilding the whole installation is what the read path stopped doing.
const repairTodoProjectionsFlag = "--repair-reader-todos"

// reportJSONFlag switches stdout to a single machine-readable object. The
// deploy helper consumes that object; see migrationReport for the contract.
const reportJSONFlag = "--report-json"

// reportSchemaVersion is bumped whenever a field in migrationReport changes
// meaning or disappears. The helper must refuse a report whose schema_version
// it does not recognise rather than guess.
const reportSchemaVersion = 1

func execute(args []string, stdout, stderr io.Writer) error {
	if handled, err := buildinfo.PrintVersion(args, stdout); handled {
		return err
	}
	return run(stdout, stderr, args)
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if strings.TrimSpace(arg) == flag {
			return true
		}
	}
	return false
}

func wantsTodoProjectionRepair(args []string) bool {
	return hasFlag(args, repairTodoProjectionsFlag)
}

func wantsJSONReport(args []string) bool {
	return hasFlag(args, reportJSONFlag) || strings.EqualFold(strings.TrimSpace(os.Getenv("MIGRATION_REPORT_JSON")), "true")
}

// migrationReport is the machine-readable contract between this command and the
// root-owned deploy helper. It is written as exactly one JSON object on stdout
// when --report-json / MIGRATION_REPORT_JSON=true is set, and it is written on
// BOTH the success and the failure path so a helper never has to infer a HOLD
// reason from an exit code alone. The process still exits non-zero on failure.
type migrationReport struct {
	SchemaVersion int    `json:"schema_version"`
	Tool          string `json:"tool"`
	ToolVersion   string `json:"tool_version"`
	ToolCommit    string `json:"tool_commit"`
	OK            bool   `json:"ok"`

	Mode            string   `json:"mode"`
	Target          string   `json:"target"`
	AllowManual     bool     `json:"allow_manual"`
	StartVersion    string   `json:"start_version"`
	EndVersion      string   `json:"end_version"`
	Applied         []string `json:"applied"`
	AlreadyAtTarget bool     `json:"already_at_target"`
	StoppedAtManual string   `json:"stopped_at_manual"`

	// OnlineUpdate is evaluated against the ledger as it stood BEFORE this run
	// and describes whether the range was eligible for a page-triggered
	// update. It is informational here: this command migrates whatever it is
	// told to, and the helper decides what a page may authorise.
	OnlineUpdate *migrate.OnlineUpdatePlan `json:"online_update,omitempty"`
	// Ledgers reconciles schema_migrations and river_migration against their
	// declared targets after the run. A run whose ledgers do not reconcile
	// fails closed.
	Ledgers *migrate.LedgerReconciliation `json:"ledgers,omitempty"`
	// TodoProjectionBackfill reports the one-shot Reader TODO backfill.
	TodoProjectionBackfill *todoBackfillReport `json:"todo_projection_backfill,omitempty"`

	Error *reportError `json:"error,omitempty"`
}

type todoBackfillReport struct {
	AlreadyComplete bool   `json:"already_complete"`
	ProjectedCount  int    `json:"projected_count"`
	CompletedAt     string `json:"completed_at,omitempty"`
}

// reportError gives the helper a stable discriminator for the HOLD reasons the
// state machine has to distinguish, instead of matching on message text.
type reportError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Stable error kinds. Anything unclassified is "failed".
const (
	errorKindUnknownTarget      = "unknown_target"
	errorKindLedgerAhead        = "ledger_ahead"
	errorKindTargetBehindLedger = "target_behind_ledger"
	errorKindManualStep         = "manual_step_in_range"
	errorKindLedgerMismatch     = "ledger_mismatch"
	errorKindFailed             = "failed"
)

func classifyError(err error) string {
	switch {
	case errors.Is(err, migrate.ErrUnknownTarget):
		return errorKindUnknownTarget
	case errors.Is(err, migrate.ErrLedgerAhead):
		return errorKindLedgerAhead
	case errors.Is(err, migrate.ErrTargetBehindLedger):
		return errorKindTargetBehindLedger
	case errors.Is(err, migrate.ErrManualStepInRange):
		return errorKindManualStep
	case errors.Is(err, errLedgerMismatch):
		return errorKindLedgerMismatch
	default:
		return errorKindFailed
	}
}

var errLedgerMismatch = errors.New("migration ledgers did not reach their declared targets")

// run 把启动逻辑从 main 抽离，使 defer（signal stop / pool.Close）能在
// 任何失败路径上正常执行——main 里 log.Fatal 会直接 os.Exit，defer 不
// 会跑（gocritic exitAfterDefer）。把错误冒泡到 main 由 main 一次性
// Fatal，是 Go 的标准 idiom。
func run(stdout, stderr io.Writer, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	pool, err := database.Open(ctx, databaseURL, database.Options{})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if wantsTodoProjectionRepair(args) {
		return repairReaderTodoProjections(ctx, stdout, pool)
	}

	jsonReport := wantsJSONReport(args)
	// With --report-json stdout carries exactly one object, so every human
	// line moves to stderr. Mixing the two would make the contract unparsable.
	human := stderr
	if !jsonReport {
		human = stdout
	}

	report, runErr := migrateAndVerify(ctx, human, pool)
	if jsonReport {
		if encodeErr := writeReport(stdout, report); encodeErr != nil {
			return errors.Join(runErr, encodeErr)
		}
	}
	return runErr
}

func writeReport(stdout io.Writer, report migrationReport) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	return nil
}

// migrateAndVerify performs the run and always returns a fully populated
// report, including on the failure paths.
func migrateAndVerify(ctx context.Context, human io.Writer, pool *pgxpool.Pool) (migrationReport, error) {
	target := strings.TrimSpace(os.Getenv("MIGRATION_TARGET"))
	allowManual := strings.EqualFold(strings.TrimSpace(os.Getenv("MIGRATION_ALLOW_MANUAL")), "true")

	exactTarget := target != "" && target != migrate.FreshInstallTarget
	report := migrationReport{
		SchemaVersion: reportSchemaVersion,
		Tool:          "cairn-migrate",
		ToolVersion:   buildinfo.Version,
		ToolCommit:    buildinfo.Commit,
		Mode:          requestedMode(target),
		AllowManual:   allowManual,
		Applied:       []string{},
	}
	if exactTarget {
		report.Target = target
		// Evaluate online-update eligibility against the pre-run ledger — the
		// only moment the answer is meaningful. It must not abort the run: a
		// helper that already quiesced writes may apply an incompatible range;
		// what it may not do is claim the range was compatible.
		report.OnlineUpdate = preRunOnlineUpdatePlan(ctx, pool, target)
	}

	result, err := migrate.Run(ctx, pool, migrate.RunRequest{Target: target, AllowManual: allowManual})
	report.StartVersion = result.StartVersion
	report.EndVersion = result.EndVersion
	report.AlreadyAtTarget = result.AlreadyAtTarget
	report.StoppedAtManual = result.StoppedAtManual
	if result.Applied != nil {
		report.Applied = result.Applied
	}
	if err != nil {
		report.Error = &reportError{Kind: classifyError(err), Message: err.Error()}
		writeHumanRunSummary(human, report)
		return report, fmt.Errorf("apply migrations: %w", err)
	}
	writeHumanRunSummary(human, report)

	reconciliation, err := reconcileLedgers(ctx, pool, result)
	if err != nil {
		report.Error = &reportError{Kind: errorKindFailed, Message: err.Error()}
		return report, err
	}
	report.Ledgers = &reconciliation
	writeHumanLedgerSummary(human, reconciliation)
	if !reconciliation.OK {
		details := make([]string, 0, len(reconciliation.Problems))
		for _, problem := range reconciliation.Problems {
			details = append(details, problem.Ledger+": "+problem.Detail)
		}
		joined := fmt.Errorf("%w: %s", errLedgerMismatch, strings.Join(details, "; "))
		report.Error = &reportError{Kind: errorKindLedgerMismatch, Message: joined.Error()}
		return report, joined
	}

	// The backfill writes to the ledger table a tail step creates, so an exact
	// target that stops short of that step must skip it rather than fail on a
	// missing relation. Deciding from the reconciled ledger keeps the rule
	// tied to what the database actually holds.
	if !slices.Contains(reconciliation.Schema.Applied, migrate.ReaderTodoProjectionLedgerMigrationID) {
		writeLine(human, "migrate: skipping reader TODO projection backfill; %s is not applied at this target\n",
			migrate.ReaderTodoProjectionLedgerMigrationID)
		report.OK = true
		return report, nil
	}

	backfill, err := backfillTodoProjections(ctx, human, pool)
	if err != nil {
		report.Error = &reportError{Kind: errorKindFailed, Message: err.Error()}
		return report, err
	}
	report.TodoProjectionBackfill = &backfill
	report.OK = true
	return report, nil
}

// requestedMode names the plan the environment asked for, independently of
// whether the run got far enough to report one.
func requestedMode(target string) string {
	switch target {
	case "":
		return "default"
	case migrate.FreshInstallTarget:
		return "fresh"
	default:
		return "target"
	}
}

// preRunOnlineUpdatePlan is best-effort: on a database that cannot answer yet
// (no ledger table, unreadable) the report simply omits the field rather than
// asserting an eligibility it could not compute.
func preRunOnlineUpdatePlan(ctx context.Context, pool *pgxpool.Pool, target string) *migrate.OnlineUpdatePlan {
	applied, err := migrate.AppliedVersions(ctx, pool)
	if err != nil {
		return nil
	}
	plan, err := migrate.PlanOnlineUpdate(applied, target)
	if err != nil {
		return nil
	}
	return &plan
}

// reconcileLedgers checks both ledgers landed exactly where this run claims.
// The schema target is the run's requested target for an exact-target run and
// its actual end version otherwise; the River target comes from the release
// manifest via MIGRATION_RIVER_LEDGER_TARGET, defaulting to this binary's
// bundle head.
func reconcileLedgers(ctx context.Context, pool *pgxpool.Pool, result migrate.RunResult) (migrate.LedgerReconciliation, error) {
	schemaTarget := result.EndVersion
	if result.Target != "" {
		schemaTarget = result.Target
	}
	riverTarget := migrate.RiverBundleTarget()
	if raw := strings.TrimSpace(os.Getenv("MIGRATION_RIVER_LEDGER_TARGET")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return migrate.LedgerReconciliation{}, fmt.Errorf("MIGRATION_RIVER_LEDGER_TARGET %q is not an integer: %w", raw, err)
		}
		riverTarget = parsed
	}
	return migrate.ReconcileLedgers(ctx, pool, migrate.LedgerTargets{
		SchemaTarget:      schemaTarget,
		RiverLedgerTarget: riverTarget,
	})
}

// writeHumanRunSummary prints what the run did. The repository has been burned
// by a migrate that succeeded silently: a deploy went on to rebuild containers
// on top of migrations that were never applied, and the exit code said nothing.
func writeHumanRunSummary(human io.Writer, report migrationReport) {
	writeLine(human, "migrate: mode=%s target=%s\n", orNone(report.Mode), orNone(report.Target))
	writeLine(human, "migrate: start=%s end=%s\n", orNone(report.StartVersion), orNone(report.EndVersion))
	if len(report.Applied) == 0 {
		writeLine(human, "migrate: applied 0 step(s); database already at %s\n", orNone(report.EndVersion))
	} else {
		writeLine(human, "migrate: applied %d step(s): %s\n", len(report.Applied), strings.Join(report.Applied, ", "))
	}
	if report.StoppedAtManual != "" {
		writeLine(human, "migrate: stopped before pending manual step %s\n", report.StoppedAtManual)
	}
	if report.Error != nil {
		writeLine(human, "migrate: FAILED (%s) %s\n", report.Error.Kind, report.Error.Message)
	}
}

func writeHumanLedgerSummary(human io.Writer, reconciliation migrate.LedgerReconciliation) {
	status := "ok"
	if !reconciliation.OK {
		status = "MISMATCH"
	}
	writeLine(human, "migrate: ledgers %s schema=%s river=%s:%d\n",
		status, orNone(reconciliation.Schema.Head), reconciliation.River.Line, reconciliation.River.Head)
	for _, problem := range reconciliation.Problems {
		writeLine(human, "migrate: ledger problem %s/%s: %s\n", problem.Ledger, problem.Kind, problem.Detail)
	}
}

// writeLine is the human/progress stream. Its errors are deliberately dropped:
// a broken stderr must not turn a completed migration into a failed deploy, and
// the machine-readable report on stdout is the checked channel.
func writeLine(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// backfillTodoProjections runs the one-shot TODO projection backfill after the
// schema is in place. It lives in the deploy-time migrate command rather than
// in the server because it must finish before the Reader starts serving TODO
// reads from the projection alone. A repeated deploy is a no-op: the backfill
// records completion in its own ledger and reports the earlier run.
func backfillTodoProjections(ctx context.Context, human io.Writer, pool *pgxpool.Pool) (todoBackfillReport, error) {
	result, err := repository.NewPGXReaderVNextRepository(pool).BackfillTodoProjections(ctx)
	if err != nil {
		return todoBackfillReport{}, fmt.Errorf("backfill reader TODO projections: %w", err)
	}
	report := todoBackfillReport{
		AlreadyComplete: result.AlreadyComplete,
		ProjectedCount:  result.ProjectedCount,
	}
	if result.AlreadyComplete {
		report.CompletedAt = result.CompletedAt.UTC().Format(time.RFC3339)
		writeLine(human, "reader TODO projection backfill already completed at %s (%d projections)\n",
			report.CompletedAt, result.ProjectedCount)
		return report, nil
	}
	writeLine(human, "reader TODO projection backfill completed (%d projections)\n", result.ProjectedCount)
	return report, nil
}

// repairReaderTodoProjections rebuilds every TODO projection from its sources.
// It skips migrations entirely because it is a data repair an operator reaches
// for after drift, not part of a deploy.
func repairReaderTodoProjections(ctx context.Context, stdout io.Writer, pool *pgxpool.Pool) error {
	projected, err := repository.NewPGXReaderVNextRepository(pool).RepairTodoProjections(ctx)
	if err != nil {
		return fmt.Errorf("repair reader TODO projections: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "reader TODO projection repair rebuilt %d projections\n", projected)
	return err
}
