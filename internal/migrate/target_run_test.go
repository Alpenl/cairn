package migrate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRunExactTargetStopsThereAndReportsTheSpan(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{TranslationSourceContractMigrationID}}}
	result, err := Run(context.Background(), db, RunRequest{Target: readerThoughtTombstoneSnapshotMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := []string{translationTerminalHistoryIndexMigrationID, readerThoughtTombstoneSnapshotMigrationID}
	if !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want %v", result.Applied, want)
	}
	if !slices.Equal(db.inserts, want) {
		t.Fatalf("recorded = %v, want %v (one step past the target would be a contract break)", db.inserts, want)
	}
	if result.Mode != "target" || result.Target != readerThoughtTombstoneSnapshotMigrationID {
		t.Fatalf("mode/target = %q/%q", result.Mode, result.Target)
	}
	if result.StartVersion != TranslationSourceContractMigrationID {
		t.Fatalf("start = %q, want %q", result.StartVersion, TranslationSourceContractMigrationID)
	}
	if result.EndVersion != readerThoughtTombstoneSnapshotMigrationID {
		t.Fatalf("end = %q, want %q", result.EndVersion, readerThoughtTombstoneSnapshotMigrationID)
	}
	if result.AlreadyAtTarget {
		t.Fatal("AlreadyAtTarget = true for a run that applied two steps")
	}
}

func TestRunExactTargetAlreadyThereIsASuccessfulNoOp(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{
		TranslationSourceContractMigrationID,
		translationTerminalHistoryIndexMigrationID,
	}}}
	result, err := Run(context.Background(), db, RunRequest{Target: translationTerminalHistoryIndexMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.AlreadyAtTarget || len(result.Applied) != 0 {
		t.Fatalf("result = %+v, want an idempotent no-op", result)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("recorded %v on a no-op run", db.inserts)
	}
}

func TestRunExactTargetRefusesRollback(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{
		TranslationSourceContractMigrationID,
		translationTerminalHistoryIndexMigrationID,
		readerThoughtTombstoneSnapshotMigrationID,
	}}}
	_, err := Run(context.Background(), db, RunRequest{Target: translationTerminalHistoryIndexMigrationID})
	if !errors.Is(err, ErrTargetBehindLedger) {
		t.Fatalf("Run() error = %v, want ErrTargetBehindLedger", err)
	}
	if !strings.Contains(err.Error(), "restoring the bound dump") {
		t.Fatalf("rollback error is not actionable: %v", err)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("a refused rollback still recorded %v", db.inserts)
	}
}

func TestRunExactTargetRefusesLedgerWrittenByANewerBinary(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{
		TranslationSourceContractMigrationID,
		"future2027010101",
	}}}
	_, err := Run(context.Background(), db, RunRequest{Target: translationTerminalHistoryIndexMigrationID})
	if !errors.Is(err, ErrLedgerAhead) {
		t.Fatalf("Run() error = %v, want ErrLedgerAhead", err)
	}
	if !strings.Contains(err.Error(), "future2027010101") {
		t.Fatalf("ledger-ahead error does not name the offending version: %v", err)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("a refused run still recorded %v", db.inserts)
	}
}

func TestRunRejectsUnknownTargetBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	_, err := Run(context.Background(), db, RunRequest{Target: "no-such-step"})
	if !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Run() error = %v, want ErrUnknownTarget", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("unknown target executed %d statements", len(db.execs))
	}
}

// TestRunExactTargetRefusesToCrossAManualGate is the rule the deploy helper
// depends on: a page-triggered update leaves AllowManual false, so a manual
// step standing in the range stops the run instead of being crossed silently.
func TestRunExactTargetRefusesToCrossAManualGate(t *testing.T) {
	withPlan(t, []Step{
		{ID: "first", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "gate", SQL: []string{"SELECT 1"}, Manual: true, OnlineUpdate: OnlineIncompatible("release-gated")},
		{ID: "after", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
	})

	db := &fakeQuerier{rows: &fakeRows{versions: []string{"first"}}}
	_, err := Run(context.Background(), db, RunRequest{Target: "after"})
	if !errors.Is(err, ErrManualStepInRange) {
		t.Fatalf("Run() error = %v, want ErrManualStepInRange", err)
	}
	if !strings.Contains(err.Error(), "MIGRATION_ALLOW_MANUAL=true") {
		t.Fatalf("manual-gate error does not tell the operator how to proceed: %v", err)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("a refused manual range still recorded %v", db.inserts)
	}
}

func TestRunExactTargetCrossesAManualGateOnlyWithExplicitApproval(t *testing.T) {
	withPlan(t, []Step{
		{ID: "first", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "gate", SQL: []string{"SELECT 1"}, Manual: true, OnlineUpdate: OnlineIncompatible("release-gated")},
		{ID: "after", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
	})

	db := &fakeQuerier{rows: &fakeRows{versions: []string{"first"}}}
	result, err := Run(context.Background(), db, RunRequest{Target: "after", AllowManual: true})
	if err != nil {
		t.Fatalf("Run(AllowManual) error = %v", err)
	}
	if want := []string{"gate", "after"}; !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want %v", result.Applied, want)
	}
}

// TestRunDefaultReportsTheManualGateItStoppedAt covers the other half of the
// silent-success problem: the default plan stops before a pending manual step,
// and used to do so without saying anything at all.
func TestRunDefaultReportsTheManualGateItStoppedAt(t *testing.T) {
	withPlan(t, []Step{
		{ID: "first", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "gate", SQL: []string{"SELECT 1"}, Manual: true, OnlineUpdate: OnlineIncompatible("release-gated")},
		{ID: "after", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
	})

	db := &fakeQuerier{rows: &fakeRows{}}
	result, err := Run(context.Background(), db, RunRequest{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Mode != "default" {
		t.Fatalf("mode = %q, want default", result.Mode)
	}
	if want := []string{"first"}; !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want %v", result.Applied, want)
	}
	if result.StoppedAtManual != "gate" {
		t.Fatalf("StoppedAtManual = %q, want \"gate\"", result.StoppedAtManual)
	}
	if result.EndVersion != "first" {
		t.Fatalf("end = %q, want \"first\"", result.EndVersion)
	}
}

// TestRunReplaysALedgerGapAndStillReportsTheRealHead covers the awkward case
// where an older step is missing while a newer one is recorded: the run has to
// replay the gap, and the end version must still name the newest step rather
// than the last one it happened to apply.
func TestRunReplaysALedgerGapAndStillReportsTheRealHead(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{
		TranslationSourceContractMigrationID,
		readerThoughtTombstoneSnapshotMigrationID,
	}}}
	result, err := Run(context.Background(), db, RunRequest{Target: readerThoughtTombstoneSnapshotMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := []string{translationTerminalHistoryIndexMigrationID}; !slices.Equal(result.Applied, want) {
		t.Fatalf("applied = %v, want the replayed gap %v", result.Applied, want)
	}
	if result.EndVersion != readerThoughtTombstoneSnapshotMigrationID {
		t.Fatalf("end = %q, want the newest recorded step %q", result.EndVersion, readerThoughtTombstoneSnapshotMigrationID)
	}
}

func TestUpStillAppliesTheWholeDefaultPlan(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := make([]string, 0, len(steps))
	for _, step := range Steps() {
		want = append(want, step.ID)
	}
	if !slices.Equal(db.inserts, want) {
		t.Fatalf("recorded = %v, want %v", db.inserts, want)
	}
}
