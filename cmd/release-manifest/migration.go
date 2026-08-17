package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"webtag/internal/migrate"
)

// Migration targets and compatibility classification.
//
// The manifest has to state, for one exact release, which migration tail the
// database must reach and whether the page-triggered updater is allowed to get
// it there. Both answers are derived from the migration plan compiled into this
// release, never typed in by hand at tag time.
//
// Classification is default deny. A step is only online-updatable once someone
// has explicitly said so on the internal/migrate side; until then it is
// unclassified and the whole release is marked online_update_compatible=false.
// The failure mode of the opposite default is a page click silently crossing a
// manual gate, which is exactly the stop condition issue #41 lists.
type onlineUpdateClass string

const (
	// classAutomatic is a step the updater may apply inside the maintenance
	// window without an operator decision.
	classAutomatic onlineUpdateClass = "automatic"
	// classManualGate is a release-gated step. It can never be crossed
	// implicitly by a page-triggered update.
	classManualGate onlineUpdateClass = "manual-gate"
	// classUnclassified is a step nobody has classified yet. Treated as deny.
	classUnclassified onlineUpdateClass = "unclassified"
)

type stepClassification struct {
	ID    string
	Class onlineUpdateClass
}

// classifyStep is the single seam the internal/migrate side will grow into.
// Today the only compatibility metadata a Step carries is Manual, which is
// enough to recognise the release-gated steps; everything else is unclassified
// and therefore denied. When Step gains an explicit online-update field, this
// function is the only place that has to learn about it.
func classifyStep(step migrate.Step) onlineUpdateClass {
	if step.Manual {
		return classManualGate
	}
	return classUnclassified
}

func classifySteps(steps []migrate.Step) []stepClassification {
	classifications := make([]stepClassification, 0, len(steps))
	for _, step := range steps {
		classifications = append(classifications, stepClassification{ID: step.ID, Class: classifyStep(step)})
	}
	return classifications
}

// schemaTarget is the exact terminal migration step id of this release. The
// helper migrates to this id, not to "everything pending", so a release that
// happens to be built from a tree with extra unmerged steps cannot widen what
// an update applies.
func schemaTarget(steps []migrate.Step) (string, error) {
	if len(steps) == 0 {
		return "", errors.New("the migration plan is empty")
	}
	target := steps[len(steps)-1].ID
	if strings.TrimSpace(target) == "" {
		return "", errors.New("the terminal migration step has no id")
	}
	return target, nil
}

// onlineUpdateDecision folds the per-step classification into the manifest
// flag plus the reason the UI shows the operator.
func onlineUpdateDecision(classifications []stepClassification) (bool, string) {
	var gated []string
	var unclassified []string
	for _, classification := range classifications {
		switch classification.Class {
		case classManualGate:
			gated = append(gated, classification.ID)
		case classUnclassified:
			unclassified = append(unclassified, classification.ID)
		case classAutomatic:
		}
	}
	switch {
	case len(gated) > 0:
		return false, fmt.Sprintf("release-gated migration step(s) %s must be applied by hand; "+
			"a page-triggered update may never cross a manual gate", strings.Join(gated, ", "))
	case len(unclassified) > 0:
		return false, fmt.Sprintf("%d migration step(s) carry no online-update classification "+
			"(first: %s); classification defaults to deny", len(unclassified), unclassified[0])
	default:
		return true, "every migration step in this release is classified as safe to apply inside the maintenance window"
	}
}

// rollbackDecision answers "can this release be undone by swapping binaries
// back". It can only be yes when the release changed neither ledger, and it is
// no whenever the generator was not told what the previous release targeted —
// an unknown predecessor is not a compatible one.
func rollbackDecision(target string, riverTarget int, previousTarget string, previousRiverTarget int) (bool, string) {
	if previousTarget == "" || previousRiverTarget == 0 {
		return false, "the previous release's migration targets were not supplied to the manifest generator, " +
			"so binary-only rollback cannot be proven safe"
	}
	if previousTarget != target {
		return false, fmt.Sprintf("this release advances the application schema target from %s to %s; "+
			"restoring the previous binaries would leave the database ahead of the code", previousTarget, target)
	}
	if previousRiverTarget != riverTarget {
		return false, fmt.Sprintf("this release advances the River ledger target from %d to %d; "+
			"restoring the previous binaries would leave the queue schema ahead of the code",
			previousRiverTarget, riverTarget)
	}
	return true, fmt.Sprintf("this release applies no new application or River migration step beyond %s / %d, "+
		"so the previous binaries can be restored without touching the database", target, riverTarget)
}

// riverLedgerTarget is the highest River migration version bundled with this
// release. River keeps its own river_migration ledger next to the application's
// schema_migrations table, so the helper has to reconcile both.
func riverLedgerTarget() (int, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(nil), nil)
	if err != nil {
		return 0, fmt.Errorf("construct river migrator: %w", err)
	}
	versions := migrator.AllVersions()
	if len(versions) == 0 {
		return 0, errors.New("the bundled River migration set is empty")
	}
	return versions[len(versions)-1].Version, nil
}
