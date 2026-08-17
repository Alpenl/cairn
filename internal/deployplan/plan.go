// Package deployplan drives the target release's own migrate binary.
//
// # Why the helper cannot answer these questions itself
//
// cairn-updater is the release that is being replaced. Its compiled-in
// migration plan is, by definition, the one that does not contain the steps the
// update would apply, so planning locally would always report an empty range
// and approve everything. The decision has to come from the binary that owns
// the steps, which means it comes back as a document rather than as a function
// call.
//
// # Why the document is mirrored rather than imported
//
// The types here restate the JSON shape internal/migrate emits instead of
// reusing those types directly. Importing them would link the migration engine,
// pgx and River into a daemon that runs as root and never touches a database
// itself — and it would quietly re-create the temptation to plan in-process.
// The cost of mirroring is drift, so the drift is closed mechanically:
// TestTheMirrorDecodesTheRealTypes marshals the real migrate types and requires
// this package to decode them field for field.
//
// # What "ok" does not mean
//
// A plan report's ok says only that the question was answerable. Eligibility
// lives in online_update.allowed and its blockers, and a caller that reads ok
// alone learns nothing. Every accessor here is written so that the fail-closed
// reading is the easy one.
package deployplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ReportSchemaVersion is the migrate report schema this package speaks. A
// report from a newer schema is refused rather than read for the fields that
// happen to still parse.
const ReportSchemaVersion = 1

// Environment variable names the migrate binary reads.
const (
	envDatabaseURL = "DATABASE_URL"
	envTarget      = "MIGRATION_TARGET"
	envReportJSON  = "MIGRATION_REPORT_JSON"
)

// PlanFlag asks the target release to evaluate the range without applying it.
const PlanFlag = "--plan-json"

// Runner runs one external program to completion.
type Runner interface {
	Run(ctx context.Context, name string, args []string, env []string) (stdout []byte, stderr []byte, err error)
}

// Report is the schema-version-1 document cmd/migrate emits on both paths.
type Report struct {
	SchemaVersion   int      `json:"schema_version"`
	Tool            string   `json:"tool"`
	ToolVersion     string   `json:"tool_version"`
	ToolCommit      string   `json:"tool_commit"`
	OK              bool     `json:"ok"`
	Mode            string   `json:"mode"`
	Target          string   `json:"target"`
	AllowManual     bool     `json:"allow_manual"`
	StartVersion    string   `json:"start_version"`
	EndVersion      string   `json:"end_version"`
	Applied         []string `json:"applied"`
	AlreadyAtTarget bool     `json:"already_at_target"`
	StoppedAtManual string   `json:"stopped_at_manual"`
	// PlanOnly marks a report produced by --plan-json. The helper requires it
	// on the plan path; see Plan.
	PlanOnly bool `json:"plan_only"`

	OnlineUpdate *OnlineUpdatePlan     `json:"online_update,omitempty"`
	Ledgers      *LedgerReconciliation `json:"ledgers,omitempty"`
	Error        *ReportError          `json:"error,omitempty"`
}

// OnlineUpdatePlan mirrors migrate.OnlineUpdatePlan.
type OnlineUpdatePlan struct {
	Target   string                `json:"target"`
	Pending  []string              `json:"pending"`
	Allowed  bool                  `json:"allowed"`
	Blockers []OnlineUpdateBlocker `json:"blockers"`
}

// OnlineUpdateBlocker mirrors migrate.OnlineUpdateBlocker.
type OnlineUpdateBlocker struct {
	StepID string `json:"step_id"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Block reasons, mirroring migrate.OnlineUpdateBlockReason.
const (
	// BlockUnclassified is the fail-closed default for an unreviewed step.
	BlockUnclassified = "unclassified"
	// BlockIncompatible is an explicit reviewed refusal.
	BlockIncompatible = "incompatible"
	// BlockManualGate is a release-gated step a page may never cross.
	BlockManualGate = "manual_gate"
)

// LedgerReconciliation mirrors migrate.LedgerReconciliation.
type LedgerReconciliation struct {
	OK       bool              `json:"ok"`
	Schema   SchemaLedgerState `json:"schema"`
	River    RiverLedgerState  `json:"river"`
	Problems []LedgerProblem   `json:"problems"`
}

// SchemaLedgerState mirrors migrate.SchemaLedgerState.
type SchemaLedgerState struct {
	Present  bool     `json:"present"`
	Target   string   `json:"target"`
	Head     string   `json:"head"`
	Applied  []string `json:"applied"`
	Missing  []string `json:"missing"`
	Extra    []string `json:"extra"`
	AtTarget bool     `json:"at_target"`
}

// RiverLedgerState mirrors migrate.RiverLedgerState.
type RiverLedgerState struct {
	Present  bool   `json:"present"`
	Line     string `json:"line"`
	Target   int    `json:"target"`
	Head     int    `json:"head"`
	Applied  []int  `json:"applied"`
	Missing  []int  `json:"missing"`
	Extra    []int  `json:"extra"`
	AtTarget bool   `json:"at_target"`
}

// LedgerProblem mirrors migrate.LedgerProblem.
type LedgerProblem struct {
	Ledger string `json:"ledger"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Ledger problem kinds, mirroring migrate.LedgerProblemKind.
const (
	// ProblemBehind means the ledger has not reached its target.
	ProblemBehind = "behind"
	// ProblemAhead means the ledger overshot. For a forward-only migration
	// system this cannot be corrected by migrating further.
	ProblemAhead = "ahead"
	// ProblemUnknownTarget means the declared target is not a version the
	// binary can produce.
	ProblemUnknownTarget = "unknown_target"
	// ProblemMissingTable means the ledger table itself is absent.
	ProblemMissingTable = "missing_table"
)

// ReportError is the migrate failure discriminator.
type ReportError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Error kinds cmd/migrate produces.
const (
	// ErrorUnknownTarget means the target is not in the binary's plan.
	ErrorUnknownTarget = "unknown_target"
	// ErrorLedgerAhead means some newer migrate binary already ran here.
	ErrorLedgerAhead = "ledger_ahead"
	// ErrorTargetBehindLedger means the request can only be satisfied by a
	// restore, never by running forward.
	ErrorTargetBehindLedger = "target_behind_ledger"
	// ErrorManualStep means the range contains a release-gated manual step.
	ErrorManualStep = "manual_step"
	// ErrorLedgerMismatch means the ledgers did not reach their targets.
	ErrorLedgerMismatch = "ledger_mismatch"
)

// ErrPlanApplied is the fail-closed guard against a binary that ignored the
// plan flag. It is the one failure in this package that means the maintenance
// window has already been entered without anyone deciding to.
var ErrPlanApplied = errors.New("the plan run reported applying migrations")

// ErrNoReport means the process produced nothing parseable.
var ErrNoReport = errors.New("the migration produced no JSON report")

// Overshot reports whether either ledger passed its declared target.
func (reconciliation *LedgerReconciliation) Overshot() bool {
	if reconciliation == nil {
		return false
	}
	for _, problem := range reconciliation.Problems {
		if problem.Kind == ProblemAhead {
			return true
		}
	}
	return len(reconciliation.Schema.Extra) > 0 || len(reconciliation.River.Extra) > 0
}

// Changed reports whether the run moved the database.
//
// It reads what the report says was applied rather than the exit status,
// because a migration that exits non-zero may still have committed several
// steps and a migration that exits zero has proved nothing until its own report
// says so.
func (report *Report) Changed() bool {
	if report == nil {
		return false
	}
	return len(report.Applied) > 0 || (report.EndVersion != "" && report.EndVersion != report.StartVersion)
}

// Blocked reports whether a plan refuses the online update, with its reasons.
//
// A nil or absent decision counts as blocked. The default has to be refusal:
// an update that proceeded because the eligibility answer was missing is
// exactly the failure the classification exists to prevent.
func (report *Report) Blocked() (bool, []OnlineUpdateBlocker) {
	if report == nil || report.OnlineUpdate == nil {
		return true, nil
	}
	if !report.OnlineUpdate.Allowed {
		return true, report.OnlineUpdate.Blockers
	}
	return false, nil
}

// Planner drives one staged migrate executable against one database.
type Planner struct {
	runner      Runner
	binary      string
	databaseURL string
}

// New binds a planner to a staged migrate binary and a database URL.
func New(runner Runner, binary, databaseURL string) *Planner {
	return &Planner{runner: runner, binary: binary, databaseURL: databaseURL}
}

// Plan evaluates eligibility without applying anything.
//
// The report is required to carry plan_only and an empty applied list. That is
// not belt-and-braces: an older migrate binary that does not know --plan-json
// would treat it as an unrecognised argument and could run the migration for
// real. Refusing a plan report that shows applied work turns that into a hold
// before the service is stopped instead of an unannounced maintenance window.
func (planner *Planner) Plan(ctx context.Context, target string) (*Report, error) {
	report, err := planner.invoke(ctx, []string{PlanFlag}, target)
	if report != nil && (!report.PlanOnly || len(report.Applied) > 0) {
		return report, fmt.Errorf("%w: %s answered %s without the plan_only marker (applied %v); "+
			"this binary may not understand %s and may have migrated the database",
			ErrPlanApplied, planner.binary, PlanFlag, report.Applied, PlanFlag)
	}
	return report, err
}

// Apply migrates to the exact target and returns the report.
//
// MIGRATION_ALLOW_MANUAL is never set. A manual gate exists precisely because
// someone decided a human has to be present for that step, so a deployment
// helper able to set the flag would be a way for a page click to answer a
// question that was reserved for a person.
func (planner *Planner) Apply(ctx context.Context, target string) (*Report, error) {
	return planner.invoke(ctx, nil, target)
}

func (planner *Planner) invoke(ctx context.Context, args []string, target string) (*Report, error) {
	environment := []string{
		envDatabaseURL + "=" + planner.databaseURL,
		envTarget + "=" + target,
		envReportJSON + "=true",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	stdout, stderr, runErr := planner.runner.Run(ctx, planner.binary, args, environment)

	// The report is parsed on both the success and the failure path. cmd/migrate
	// writes it either way, and on failure it is the only thing that names which
	// steps committed before the failing one — which is exactly what a hold
	// point has to record.
	report, parseErr := Parse(stdout)
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("%w (and its report was unreadable: %w)", runErr, parseErr)
		}
		return nil, fmt.Errorf("%w (stderr: %s)", parseErr, firstLine(stderr))
	}
	if runErr != nil {
		return report, runErr
	}
	return report, nil
}

// Parse reads a migrate report from a process's stdout.
func Parse(stdout []byte) (*Report, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w; its result cannot be established", ErrNoReport)
	}
	var report Report
	if err := json.Unmarshal(trimmed, &report); err != nil {
		return nil, fmt.Errorf("parse migration report: %w", err)
	}
	if report.SchemaVersion != ReportSchemaVersion {
		return nil, fmt.Errorf("the migration report is schema version %d, this helper speaks %d",
			report.SchemaVersion, ReportSchemaVersion)
	}
	return &report, nil
}

func firstLine(data []byte) string {
	text := bytes.TrimSpace(data)
	if len(text) == 0 {
		return "no stderr output"
	}
	if index := bytes.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return string(text)
}
