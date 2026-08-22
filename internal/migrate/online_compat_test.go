package migrate

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCurrentHeadDeclaresAnIncompatibleOnlineReview(t *testing.T) {
	t.Parallel()

	step := Steps()[0]
	if step.OnlineUpdate.Compatibility != OnlineUpdateIncompatible {
		t.Fatalf("current migration compatibility = %q, want %q", step.OnlineUpdate.Compatibility, OnlineUpdateIncompatible)
	}
	if strings.TrimSpace(step.OnlineUpdate.Note) == "" {
		t.Fatal("current migration has no operator-facing compatibility explanation")
	}
}

func TestPlanOnlineUpdateRefusesFreshAndProductionUpgrade(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		applied []string
	}{
		{name: "fresh"},
		{name: "v0.1.17 production", applied: ProductionBaselineVersions()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanOnlineUpdate(test.applied, CurrentSchemaMigrationID)
			if err != nil {
				t.Fatalf("PlanOnlineUpdate() error = %v", err)
			}
			if plan.Allowed {
				t.Fatal("schema-replacing migration was allowed online")
			}
			if !slices.Equal(plan.Pending, []string{CurrentSchemaMigrationID}) {
				t.Fatalf("pending = %v, want current head", plan.Pending)
			}
			if len(plan.Blockers) != 1 || plan.Blockers[0].StepID != CurrentSchemaMigrationID ||
				plan.Blockers[0].Reason != OnlineUpdateBlockIncompatible ||
				strings.TrimSpace(plan.Blockers[0].Detail) == "" {
				t.Fatalf("blockers = %+v, want one explained incompatibility", plan.Blockers)
			}
		})
	}
}

func TestPlanOnlineUpdateAlreadyCurrentIsAllowed(t *testing.T) {
	t.Parallel()

	plan, err := PlanOnlineUpdate([]string{CurrentSchemaMigrationID}, CurrentSchemaMigrationID)
	if err != nil {
		t.Fatalf("PlanOnlineUpdate() error = %v", err)
	}
	if !plan.Allowed || len(plan.Pending) != 0 || len(plan.Blockers) != 0 {
		t.Fatalf("already-current plan = %+v, want allowed no-op", plan)
	}
}

func TestPlanOnlineUpdateRejectsUnknownTargetAndInvalidLedger(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		applied []string
		target  string
		want    error
	}{
		{name: "unknown target", target: "no-such-step", want: ErrUnknownTarget},
		{name: "partial historical ledger", applied: ProductionBaselineVersions()[:3], target: CurrentSchemaMigrationID, want: ErrLedgerAhead},
		{name: "future ledger", applied: []string{CurrentSchemaMigrationID, "future2027010101"}, target: CurrentSchemaMigrationID, want: ErrLedgerAhead},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := PlanOnlineUpdate(test.applied, test.target)
			if !errors.Is(err, test.want) {
				t.Fatalf("PlanOnlineUpdate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRiverBundleTargetIsTheNewestBundledVersion(t *testing.T) {
	t.Parallel()

	versions := RiverBundleVersions()
	if len(versions) == 0 {
		t.Fatal("River bundle reported no versions")
	}
	if !slices.IsSorted(versions) {
		t.Fatalf("River bundle versions = %v, want ascending", versions)
	}
	if got, want := RiverBundleTarget(), versions[len(versions)-1]; got != want {
		t.Fatalf("RiverBundleTarget() = %d, want %d", got, want)
	}
}
