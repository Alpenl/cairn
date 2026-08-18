package migrate

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// withPlan temporarily replaces the package migration plan so a test can build
// the step shapes the shipped plan does not currently contain — a manual gate,
// an unreviewed step.
//
// A test using it must NOT call t.Parallel. Go releases parallel tests only
// once every sequential test in the binary has finished, so a sequential test
// owns the global for its whole duration; a parallel one would not.
func withPlan(t *testing.T, plan []Step) {
	t.Helper()
	original := steps
	steps = plan
	t.Cleanup(func() { steps = original })
}

// TestEveryStepDeclaresAnOnlineUpdateReview is the contract that makes the
// default deny real. A new migration step added without an OnlineUpdate review
// gets the zero value, and the zero value fails here — the author has to answer
// "can the previous release's binary keep serving once this lands?" before the
// build is green.
func TestEveryStepDeclaresAnOnlineUpdateReview(t *testing.T) {
	t.Parallel()

	for _, step := range Steps() {
		switch step.OnlineUpdate.Compatibility {
		case OnlineUpdateCompatible, OnlineUpdateIncompatible:
		case OnlineUpdateUnclassified:
			t.Errorf("migration %q declares no online-update review; set OnlineUpdate to OnlineCompatible(...) "+
				"or OnlineIncompatible(...) after deciding whether the previous release's binary survives it", step.ID)
		default:
			t.Errorf("migration %q has unknown online-update compatibility %q", step.ID, step.OnlineUpdate.Compatibility)
		}
		if strings.TrimSpace(step.OnlineUpdate.Note) == "" {
			t.Errorf("migration %q has an empty online-update note; the deploy UI renders this string to the operator", step.ID)
		}
	}
}

// TestShippedPlanOnlineUpdateReviewConclusions pins the reviewed verdict of
// every step in the plan. Changing a historical step's verdict is a deliberate
// act — it changes which page-triggered updates are allowed and which binary
// rollbacks are safe — so it has to be changed here too.
func TestShippedPlanOnlineUpdateReviewConclusions(t *testing.T) {
	t.Parallel()

	want := map[string]OnlineUpdateCompatibility{
		TranslationSourceContractMigrationID:       OnlineUpdateIncompatible,
		translationTerminalHistoryIndexMigrationID: OnlineUpdateCompatible,
		readerThoughtTombstoneSnapshotMigrationID:  OnlineUpdateIncompatible,
		integrityRepairMigrationID:                 OnlineUpdateIncompatible,
		historicalRepairMigrationID:                OnlineUpdateIncompatible,
		conceptMergeAuditRepairMigrationID:         OnlineUpdateIncompatible,
		lifecycleRepairMigrationID:                 OnlineUpdateIncompatible,
		readerThoughtSearchTrigramMigrationID:      OnlineUpdateCompatible,
		ReaderTodoProjectionLedgerMigrationID:      OnlineUpdateCompatible,
		ReaderInboxCaptureDocumentMigrationID:      OnlineUpdateCompatible,
	}
	plan := Steps()
	if len(plan) != len(want) {
		t.Fatalf("plan has %d steps, reviewed conclusions cover %d", len(plan), len(want))
	}
	for _, step := range plan {
		expected, ok := want[step.ID]
		if !ok {
			t.Errorf("migration %q has no recorded review conclusion", step.ID)
			continue
		}
		if step.OnlineUpdate.Compatibility != expected {
			t.Errorf("migration %q online-update compatibility = %q, reviewed conclusion is %q",
				step.ID, step.OnlineUpdate.Compatibility, expected)
		}
	}
}

// TestPlanOnlineUpdateAcrossShippedPlanIsRefused proves the practical answer for
// the range a first page-triggered update would face: the shipped plan contains
// reviewed-incompatible repairs, so a full run is not online-eligible and the
// operator is told exactly which steps refuse.
func TestPlanOnlineUpdateAcrossShippedPlanIsRefused(t *testing.T) {
	t.Parallel()

	plan, err := PlanOnlineUpdate(nil, ReaderTodoProjectionLedgerMigrationID)
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if plan.Allowed {
		t.Fatal("full-plan online update reported allowed; the fresh-install and repair steps are reviewed incompatible")
	}
	blocked := make(map[string]OnlineUpdateBlockReason, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		blocked[blocker.StepID] = blocker.Reason
		if strings.TrimSpace(blocker.Detail) == "" {
			t.Errorf("blocker for %q has no operator-facing detail", blocker.StepID)
		}
	}
	for _, want := range []string{
		TranslationSourceContractMigrationID,
		readerThoughtTombstoneSnapshotMigrationID,
		integrityRepairMigrationID,
		historicalRepairMigrationID,
		conceptMergeAuditRepairMigrationID,
		lifecycleRepairMigrationID,
	} {
		if got := blocked[want]; got != OnlineUpdateBlockIncompatible {
			t.Errorf("blocker reason for %q = %q, want %q", want, got, OnlineUpdateBlockIncompatible)
		}
	}
	for _, unwanted := range []string{
		translationTerminalHistoryIndexMigrationID,
		readerThoughtSearchTrigramMigrationID,
		ReaderTodoProjectionLedgerMigrationID,
		ReaderInboxCaptureDocumentMigrationID,
	} {
		if _, blocked := blocked[unwanted]; blocked {
			t.Errorf("reviewed-compatible migration %q was reported as a blocker", unwanted)
		}
	}
}

// TestPlanOnlineUpdateAllowsAReviewedCompatibleTail is the positive case: the
// tail of the shipped plan is three reviewed-compatible steps, so an
// installation already carrying the repairs is online-eligible.
func TestPlanOnlineUpdateAllowsAReviewedCompatibleTail(t *testing.T) {
	t.Parallel()

	applied := []string{
		TranslationSourceContractMigrationID,
		translationTerminalHistoryIndexMigrationID,
		readerThoughtTombstoneSnapshotMigrationID,
		integrityRepairMigrationID,
		historicalRepairMigrationID,
		conceptMergeAuditRepairMigrationID,
		lifecycleRepairMigrationID,
	}
	plan, err := PlanOnlineUpdate(applied, ReaderTodoProjectionLedgerMigrationID)
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if !plan.Allowed {
		t.Fatalf("reviewed-compatible tail reported blocked: %+v", plan.Blockers)
	}
	want := []string{readerThoughtSearchTrigramMigrationID, ReaderTodoProjectionLedgerMigrationID}
	if !slices.Equal(plan.Pending, want) {
		t.Fatalf("pending = %v, want %v", plan.Pending, want)
	}
}

func TestPlanOnlineUpdateAlreadyAtTargetIsAllowed(t *testing.T) {
	t.Parallel()

	applied := make([]string, 0, len(steps))
	for _, step := range Steps() {
		applied = append(applied, step.ID)
	}
	plan, err := PlanOnlineUpdate(applied, ReaderInboxCaptureDocumentMigrationID)
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if !plan.Allowed || len(plan.Pending) != 0 {
		t.Fatalf("already-at-target plan = %+v, want allowed with nothing pending", plan)
	}
}

// TestPlanOnlineUpdateRefusesUnreviewedStep is the whole point of the zero
// value: a step whose author wrote nothing is refused, and the operator is told
// it was never reviewed rather than being told it is unsafe.
func TestPlanOnlineUpdateRefusesUnreviewedStep(t *testing.T) {
	withPlan(t, []Step{
		{ID: "reviewed", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "unreviewed", SQL: []string{"SELECT 1"}},
	})

	plan, err := PlanOnlineUpdate(nil, "unreviewed")
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if plan.Allowed {
		t.Fatal("unreviewed step was allowed online; the zero value must deny")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].StepID != "unreviewed" ||
		plan.Blockers[0].Reason != OnlineUpdateBlockUnclassified {
		t.Fatalf("blockers = %+v, want a single unclassified blocker on \"unreviewed\"", plan.Blockers)
	}
}

func TestPlanOnlineUpdateRefusesManualStepEvenWhenReviewedCompatible(t *testing.T) {
	withPlan(t, []Step{
		{ID: "first", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "gate", SQL: []string{"SELECT 1"}, Manual: true, OnlineUpdate: OnlineCompatible("no-op")},
		{ID: "after", SQL: []string{"SELECT 1"}, OnlineUpdate: OnlineCompatible("no-op")},
	})

	plan, err := PlanOnlineUpdate([]string{"first"}, "after")
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if plan.Allowed {
		t.Fatal("a manual gate inside the range was crossed implicitly")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].StepID != "gate" ||
		plan.Blockers[0].Reason != OnlineUpdateBlockManual {
		t.Fatalf("blockers = %+v, want a single manual-gate blocker on \"gate\"", plan.Blockers)
	}
}

func TestPlanOnlineUpdateFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		applied []string
		target  string
		want    error
	}{
		{
			name:   "unknown target",
			target: "no-such-step",
			want:   ErrUnknownTarget,
		},
		{
			name:    "ledger written by a newer binary",
			applied: []string{TranslationSourceContractMigrationID, "future2027010101"},
			target:  ReaderTodoProjectionLedgerMigrationID,
			want:    ErrLedgerAhead,
		},
		{
			name:    "target behind the applied ledger",
			applied: []string{TranslationSourceContractMigrationID, translationTerminalHistoryIndexMigrationID},
			target:  TranslationSourceContractMigrationID,
			want:    ErrTargetBehindLedger,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := PlanOnlineUpdate(tt.applied, tt.target)
			if !errors.Is(err, tt.want) {
				t.Fatalf("PlanOnlineUpdate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRiverBundleTargetIsTheNewestBundledVersion(t *testing.T) {
	t.Parallel()

	versions := RiverBundleVersions()
	if len(versions) == 0 {
		t.Fatal("River bundle reported no versions; the manifest's river_ledger_target cannot be derived")
	}
	if !slices.IsSorted(versions) {
		t.Fatalf("River bundle versions = %v, want ascending", versions)
	}
	if got, want := RiverBundleTarget(), versions[len(versions)-1]; got != want {
		t.Fatalf("RiverBundleTarget() = %d, want %d", got, want)
	}
}
