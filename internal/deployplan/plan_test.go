package deployplan

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"webtag/internal/migrate"
)

// TestTheMirrorDecodesTheRealTypes is the reason this package is allowed to
// restate internal/migrate's wire shape instead of importing it.
//
// The helper runs as root and must not link the migration engine, pgx and
// River; but a hand-copied struct drifts silently, and the field it drifts on
// will be the one that decides whether a manual gate was crossed. So the copy
// is checked against the original mechanically: the real types are marshalled
// and this package is required to read every field back.
func TestTheMirrorDecodesTheRealTypes(t *testing.T) {
	real := migrate.OnlineUpdatePlan{
		Target:  "readertodoprojection2026081701",
		Pending: []string{"conceptbackfill2026072001", "readertodoprojection2026081701"},
		Allowed: false,
		Blockers: []migrate.OnlineUpdateBlocker{
			{StepID: "conceptbackfill2026072001", Reason: migrate.OnlineUpdateBlockManual, Detail: "release-gated"},
			{StepID: "readertodoprojection2026081701", Reason: migrate.OnlineUpdateBlockIncompatible, Detail: "rewrites live writes"},
		},
	}
	encoded, err := json.Marshal(real)
	if err != nil {
		t.Fatalf("marshal the real plan: %v", err)
	}
	var mirrored OnlineUpdatePlan
	if err := json.Unmarshal(encoded, &mirrored); err != nil {
		t.Fatalf("the mirror cannot decode the real plan: %v", err)
	}
	if mirrored.Target != real.Target || mirrored.Allowed != real.Allowed {
		t.Fatalf("the mirror lost a scalar: %+v", mirrored)
	}
	if !slices.Equal(mirrored.Pending, real.Pending) {
		t.Fatalf("the mirror lost the pending range: %v", mirrored.Pending)
	}
	if len(mirrored.Blockers) != len(real.Blockers) {
		t.Fatalf("the mirror lost blockers: %+v", mirrored.Blockers)
	}
	for index, blocker := range mirrored.Blockers {
		if blocker.StepID != real.Blockers[index].StepID ||
			blocker.Reason != string(real.Blockers[index].Reason) ||
			blocker.Detail != real.Blockers[index].Detail {
			t.Fatalf("blocker %d drifted: %+v vs %+v", index, blocker, real.Blockers[index])
		}
	}
}

// TestTheBlockReasonConstantsMatch keeps the classification strings identical.
// The helper decides "is this a manual gate" by comparing to one of these, and
// a renamed constant on either side would silently turn every manual gate into
// an ordinary blocker.
func TestTheBlockReasonConstantsMatch(t *testing.T) {
	pairs := map[string]migrate.OnlineUpdateBlockReason{
		BlockUnclassified: migrate.OnlineUpdateBlockUnclassified,
		BlockIncompatible: migrate.OnlineUpdateBlockIncompatible,
		BlockManualGate:   migrate.OnlineUpdateBlockManual,
	}
	for mirrored, real := range pairs {
		if mirrored != string(real) {
			t.Fatalf("block reason drifted: mirror %q, migrate %q", mirrored, real)
		}
	}
}

func TestTheLedgerProblemKindsMatch(t *testing.T) {
	pairs := map[string]migrate.LedgerProblemKind{
		ProblemBehind:        migrate.LedgerProblemBehind,
		ProblemAhead:         migrate.LedgerProblemAhead,
		ProblemUnknownTarget: migrate.LedgerProblemUnknownTarget,
		ProblemMissingTable:  migrate.LedgerProblemMissingTable,
	}
	for mirrored, real := range pairs {
		if mirrored != string(real) {
			t.Fatalf("ledger problem kind drifted: mirror %q, migrate %q", mirrored, real)
		}
	}
}

func TestTheMirrorDecodesTheRealLedgerReconciliation(t *testing.T) {
	real := migrate.LedgerReconciliation{
		OK: false,
		Schema: migrate.SchemaLedgerState{
			Present: true, Target: "readertodoprojection2026081701", Head: "somethingnewer2026090101",
			Applied: []string{"a", "b"}, Missing: []string{"c"}, Extra: []string{"somethingnewer2026090101"},
			AtTarget: false,
		},
		River: migrate.RiverLedgerState{
			Present: true, Line: "main", Target: 7, Head: 9,
			Applied: []int{1, 7, 9}, Missing: []int{}, Extra: []int{9}, AtTarget: false,
		},
		Problems: []migrate.LedgerProblem{
			{Ledger: "schema_migrations", Kind: migrate.LedgerProblemAhead, Detail: "overshot"},
		},
	}
	encoded, err := json.Marshal(real)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var mirrored LedgerReconciliation
	if err := json.Unmarshal(encoded, &mirrored); err != nil {
		t.Fatalf("the mirror cannot decode the real reconciliation: %v", err)
	}
	if !slices.Equal(mirrored.Schema.Extra, real.Schema.Extra) || !slices.Equal(mirrored.River.Extra, real.River.Extra) {
		t.Fatalf("the mirror lost the overshoot evidence: %+v", mirrored)
	}
	if mirrored.River.Line != real.River.Line || mirrored.River.Head != real.River.Head {
		t.Fatalf("the mirror lost River state: %+v", mirrored.River)
	}
	if !mirrored.Overshot() {
		t.Fatal("the mirror decoded an overshoot but does not report one")
	}
}

// --- decision helpers -------------------------------------------------------

func TestAnAbsentDecisionCountsAsBlocked(t *testing.T) {
	// The default has to be refusal: an update that proceeded because the
	// eligibility answer was missing is exactly the failure the classification
	// exists to prevent.
	var nilReport *Report
	if blocked, _ := nilReport.Blocked(); !blocked {
		t.Fatal("a nil report was treated as permission")
	}
	if blocked, _ := (&Report{}).Blocked(); !blocked {
		t.Fatal("a report with no online_update was treated as permission")
	}
	// And ok alone is never permission.
	if blocked, _ := (&Report{OK: true}).Blocked(); !blocked {
		t.Fatal("ok=true was read as eligibility")
	}
}

func TestBlockedReturnsTheBlockersVerbatim(t *testing.T) {
	report := &Report{OnlineUpdate: &OnlineUpdatePlan{
		Allowed:  false,
		Blockers: []OnlineUpdateBlocker{{StepID: "s1", Reason: BlockManualGate, Detail: "gated"}},
	}}
	blocked, blockers := report.Blocked()
	if !blocked || len(blockers) != 1 || blockers[0].Reason != BlockManualGate {
		t.Fatalf("expected the manual gate verbatim, got %v %+v", blocked, blockers)
	}
}

func TestAnEmptyPendingRangeIsAllowed(t *testing.T) {
	// Already at target: nothing is applied, so nothing can be unsafe.
	report := &Report{OnlineUpdate: &OnlineUpdatePlan{Pending: []string{}, Allowed: true, Blockers: nil}}
	if blocked, _ := report.Blocked(); blocked {
		t.Fatal("an empty range was refused")
	}
}

func TestChangedReadsTheReportNotTheExitStatus(t *testing.T) {
	if (&Report{Applied: []string{"s1"}}).Changed() != true {
		t.Fatal("an applied step is a change")
	}
	if (&Report{StartVersion: "a", EndVersion: "b"}).Changed() != true {
		t.Fatal("a moved head is a change")
	}
	if (&Report{StartVersion: "a", EndVersion: "a", Applied: []string{}}).Changed() != false {
		t.Fatal("an unchanged head with nothing applied is not a change")
	}
	if (&Report{Applied: []string{}}).Changed() != false {
		t.Fatal("an empty plan report is not a change")
	}
}

// --- the subprocess contract ------------------------------------------------

type scriptedRunner struct {
	calls  [][]string
	envs   [][]string
	stdout []byte
	stderr []byte
	err    error
}

func (runner *scriptedRunner) Run(_ context.Context, name string, args, env []string) ([]byte, []byte, error) {
	runner.calls = append(runner.calls, append([]string{name}, args...))
	runner.envs = append(runner.envs, env)
	return runner.stdout, runner.stderr, runner.err
}

func planReport(t *testing.T, report Report) []byte {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func TestPlanPassesTheTargetAndNeverAllowsManual(t *testing.T) {
	runner := &scriptedRunner{stdout: planReport(t, Report{
		SchemaVersion: ReportSchemaVersion, OK: true, PlanOnly: true, Applied: []string{},
		OnlineUpdate: &OnlineUpdatePlan{Allowed: true},
	})}
	planner := New(runner, "/opt/webtag/releases/.incoming-v1.2.3/pkg/migrate", "postgres://x")

	if _, err := planner.Plan(context.Background(), "step2026081701"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !slices.Equal(runner.calls[0][1:], []string{PlanFlag}) {
		t.Fatalf("the plan must be the only flag, got %v", runner.calls[0])
	}
	environment := strings.Join(runner.envs[0], "\n")
	if !strings.Contains(environment, "MIGRATION_TARGET=step2026081701") {
		t.Fatalf("the exact target must be passed, got %v", runner.envs[0])
	}
	if !strings.Contains(environment, "DATABASE_URL=postgres://x") {
		t.Fatalf("the database URL must be passed, got %v", runner.envs[0])
	}
	// A helper able to set this flag would let a page click answer a question
	// that was reserved for a person.
	if strings.Contains(environment, "MIGRATION_ALLOW_MANUAL") {
		t.Fatalf("the helper must never set MIGRATION_ALLOW_MANUAL, got %v", runner.envs[0])
	}
}

func TestApplyNeverAllowsManualEither(t *testing.T) {
	runner := &scriptedRunner{stdout: planReport(t, Report{SchemaVersion: ReportSchemaVersion, OK: true})}
	planner := New(runner, "migrate", "postgres://x")
	if _, err := planner.Apply(context.Background(), "step2026081701"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(runner.calls[0]) != 1 {
		t.Fatalf("apply takes no flags, got %v", runner.calls[0])
	}
	if strings.Contains(strings.Join(runner.envs[0], "\n"), "MIGRATION_ALLOW_MANUAL") {
		t.Fatalf("the helper must never set MIGRATION_ALLOW_MANUAL, got %v", runner.envs[0])
	}
}

func TestAPlanWithoutTheMarkerIsRefused(t *testing.T) {
	// An older binary that does not know --plan-json would treat it as an
	// unrecognised argument and could run the migration for real.
	for _, report := range []Report{
		{SchemaVersion: ReportSchemaVersion, OK: true, PlanOnly: false, Applied: []string{}},
		{SchemaVersion: ReportSchemaVersion, OK: true, PlanOnly: true, Applied: []string{"step2026081701"}},
	} {
		runner := &scriptedRunner{stdout: planReport(t, report)}
		planner := New(runner, "migrate", "postgres://x")
		_, err := planner.Plan(context.Background(), "step2026081701")
		if !errors.Is(err, ErrPlanApplied) {
			t.Fatalf("expected ErrPlanApplied for %+v, got %v", report, err)
		}
	}
}

func TestAMigrationWithNoReportIsNotBelieved(t *testing.T) {
	// The trap this repository has already been bitten by: a zero exit status
	// with nothing to show for it.
	runner := &scriptedRunner{stdout: nil}
	planner := New(runner, "migrate", "postgres://x")
	if _, err := planner.Apply(context.Background(), "step2026081701"); !errors.Is(err, ErrNoReport) {
		t.Fatalf("expected ErrNoReport, got %v", err)
	}
}

func TestAReportFromANewerSchemaIsRefused(t *testing.T) {
	runner := &scriptedRunner{stdout: planReport(t, Report{SchemaVersion: ReportSchemaVersion + 1, OK: true})}
	planner := New(runner, "migrate", "postgres://x")
	if _, err := planner.Apply(context.Background(), "step"); err == nil {
		t.Fatal("a report from a newer schema was read as if it were understood")
	}
}

func TestAFailedRunStillYieldsItsReport(t *testing.T) {
	// On failure the report is the only thing that names which steps committed
	// before the failing one, which is exactly what a hold point has to record.
	runner := &scriptedRunner{
		stdout: planReport(t, Report{
			SchemaVersion: ReportSchemaVersion, OK: false,
			Applied: []string{"stepA"}, StartVersion: "start", EndVersion: "stepA",
			Error: &ReportError{Kind: ErrorLedgerMismatch, Message: "did not reach target"},
		}),
		err: errors.New("exit status 1"),
	}
	planner := New(runner, "migrate", "postgres://x")
	report, err := planner.Apply(context.Background(), "stepB")
	if err == nil {
		t.Fatal("a failing run was reported as success")
	}
	if report == nil {
		t.Fatal("a failing run must still surface its report")
	}
	if !report.Changed() {
		t.Fatal("a run that committed a step must report the database as changed")
	}
	if report.Error.Kind != ErrorLedgerMismatch {
		t.Fatalf("the failure kind must survive, got %+v", report.Error)
	}
}
