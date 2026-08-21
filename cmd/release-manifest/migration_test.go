package main

import (
	"strings"
	"testing"

	"webtag/internal/migrate"
)

// The compatibility classification is default deny. These tests pin the three
// answers separately so that the day internal/migrate grows an explicit
// classification, the "automatic" branch is already covered and only
// classifyStep has to change.
func TestOnlineUpdateDecisionDefaultsToDeny(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		steps      []stepClassification
		compatible bool
		reason     string
	}{
		"every step classified automatic": {
			steps: []stepClassification{
				{ID: "a", Class: classAutomatic},
				{ID: "b", Class: classAutomatic},
			},
			compatible: true,
			reason:     "safe to apply inside the maintenance window",
		},
		"one unclassified step": {
			steps: []stepClassification{
				{ID: "a", Class: classAutomatic},
				{ID: "b", Class: classUnclassified},
			},
			compatible: false,
			reason:     "no online-update classification",
		},
		"one incompatible target": {
			steps: []stepClassification{
				{ID: "a", Class: classAutomatic},
				{ID: "b", Class: classIncompatible},
			},
			compatible: false,
			reason:     "incompatible with the previous Core binary",
		},
		"an incompatible target outranks an unclassified target": {
			steps: []stepClassification{
				{ID: "a", Class: classUnclassified},
				{ID: "b", Class: classIncompatible},
			},
			compatible: false,
			reason:     "incompatible with the previous Core binary",
		},
		"an empty plan": {
			steps:      nil,
			compatible: true,
			reason:     "safe to apply inside the maintenance window",
		},
	} {
		compatible, reason := onlineUpdateDecision(testCase.steps)
		if compatible != testCase.compatible {
			t.Errorf("%s: online_update_compatible = %t, want %t", name, compatible, testCase.compatible)
		}
		if !strings.Contains(reason, testCase.reason) {
			t.Errorf("%s: reason %q does not mention %q", name, reason, testCase.reason)
		}
	}
}

func TestClassifyStepUsesTheMigrationReview(t *testing.T) {
	t.Parallel()

	if got := classifyStep(migrate.Step{ID: "safe", OnlineUpdate: migrate.OnlineCompatible("additive")}); got != classAutomatic {
		t.Errorf("compatible step classified %q, want %q", got, classAutomatic)
	}
	if got := classifyStep(migrate.Step{ID: "unsafe", OnlineUpdate: migrate.OnlineIncompatible("contract")}); got != classIncompatible {
		t.Errorf("incompatible step classified %q, want %q", got, classIncompatible)
	}
	if got := classifyStep(migrate.Step{ID: "plain"}); got != classUnclassified {
		t.Errorf("plain step classified %q, want %q", got, classUnclassified)
	}
}

func TestRollbackDecisionRequiresProofBothLedgersAreUnchanged(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		previousTarget string
		previousRiver  int
		compatible     bool
		reason         string
	}{
		"unknown predecessor": {
			previousTarget: "", previousRiver: 0,
			compatible: false, reason: "were not supplied",
		},
		"known schema target but unknown River target": {
			previousTarget: "step-b", previousRiver: 0,
			compatible: false, reason: "were not supplied",
		},
		"the release advances the application schema": {
			previousTarget: "step-a", previousRiver: 7,
			compatible: false, reason: "advances the application schema target",
		},
		"the release advances the River ledger": {
			previousTarget: "step-b", previousRiver: 6,
			compatible: false, reason: "advances the River ledger target",
		},
		"neither ledger moved": {
			previousTarget: "step-b", previousRiver: 7,
			compatible: true, reason: "no new application or River migration step",
		},
	} {
		compatible, reason := rollbackDecision("step-b", 7, testCase.previousTarget, testCase.previousRiver)
		if compatible != testCase.compatible {
			t.Errorf("%s: rollback_compatible = %t, want %t", name, compatible, testCase.compatible)
		}
		if !strings.Contains(reason, testCase.reason) {
			t.Errorf("%s: reason %q does not mention %q", name, reason, testCase.reason)
		}
	}
}

// schema_target is the terminal step of the plan compiled into this release,
// so an update always lands on an exact id rather than on "everything pending".
func TestSchemaTargetIsTheTerminalStepOfTheCompiledPlan(t *testing.T) {
	t.Parallel()

	steps := migrate.Steps()
	target, err := schemaTarget(steps)
	if err != nil {
		t.Fatalf("schema target: %v", err)
	}
	if target != steps[len(steps)-1].ID {
		t.Fatalf("schema target %q is not the terminal step %q", target, steps[len(steps)-1].ID)
	}
	if _, err := schemaTarget(nil); err == nil {
		t.Error("an empty migration plan produced a schema target")
	}
	if _, err := schemaTarget([]migrate.Step{{ID: "  "}}); err == nil {
		t.Error("a blank step id produced a schema target")
	}
}

// The River ledger target has to come from the bundled migration set, not from
// a number typed into the workflow.
func TestRiverLedgerTargetComesFromTheBundledMigrationSet(t *testing.T) {
	t.Parallel()

	target, err := riverLedgerTarget()
	if err != nil {
		t.Fatalf("river ledger target: %v", err)
	}
	if target < 1 {
		t.Fatalf("river ledger target %d is not a real version", target)
	}
}
