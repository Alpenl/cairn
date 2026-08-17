package migrate

import (
	"errors"
	"fmt"
	"slices"
)

// OnlineUpdateCompatibility records, per migration step, the reviewed answer to
// exactly one question:
//
//	If this step is applied while the PREVIOUS release's binary is still the
//	one serving traffic, does the previous binary keep working correctly?
//
// Two consumers depend on that single property:
//
//   - The page-triggered ("online") update. Issue #41 lets an operator start a
//     controlled Core update from the Reader settings page. A range that is not
//     compatible must not be reachable that way; it belongs to the manual
//     runbook with its own maintenance window and bound dump.
//   - Binary-only rollback. The helper may switch back to the previous release
//     without restoring a dump only when every step it applied leaves that
//     previous binary functional — which is the same property.
//
// The zero value is OnlineUpdateUnclassified, i.e. DENIED. A step author who
// writes nothing gets no online-update permission, and TestEveryStepDeclaresAn
// OnlineUpdateReview fails the build until the step is reviewed explicitly.
type OnlineUpdateCompatibility string

const (
	// OnlineUpdateUnclassified is the zero value: nobody has reviewed this
	// step yet. Treated as denied everywhere.
	OnlineUpdateUnclassified OnlineUpdateCompatibility = ""
	// OnlineUpdateIncompatible means the review concluded the previous
	// release's binary cannot keep serving correctly once this step lands.
	OnlineUpdateIncompatible OnlineUpdateCompatibility = "incompatible"
	// OnlineUpdateCompatible means the review concluded the previous
	// release's binary is unaffected by this step.
	OnlineUpdateCompatible OnlineUpdateCompatibility = "compatible"
)

// OnlineUpdateReview is one step's reviewed compatibility conclusion plus the
// operator-facing sentence explaining it. The Note is data rather than a Go
// comment because the deploy UI has to show the operator *why* a range was
// refused; a comment cannot be rendered.
type OnlineUpdateReview struct {
	Compatibility OnlineUpdateCompatibility
	Note          string
}

// OnlineCompatible declares a reviewed step safe to apply under the previous
// release's binary. The note must state what makes it safe.
func OnlineCompatible(note string) OnlineUpdateReview {
	return OnlineUpdateReview{Compatibility: OnlineUpdateCompatible, Note: note}
}

// OnlineIncompatible declares a reviewed step unsafe to apply under the
// previous release's binary. The note must state what breaks.
func OnlineIncompatible(note string) OnlineUpdateReview {
	return OnlineUpdateReview{Compatibility: OnlineUpdateIncompatible, Note: note}
}

// OnlineUpdateBlockReason classifies why a step refuses an online update.
type OnlineUpdateBlockReason string

const (
	// OnlineUpdateBlockUnclassified is the fail-closed default: the step
	// carries no review at all.
	OnlineUpdateBlockUnclassified OnlineUpdateBlockReason = "unclassified"
	// OnlineUpdateBlockIncompatible is an explicit reviewed refusal.
	OnlineUpdateBlockIncompatible OnlineUpdateBlockReason = "incompatible"
	// OnlineUpdateBlockManual marks a release-gated manual step. A manual step
	// can never be crossed implicitly by a page-triggered update, regardless of
	// its compatibility review.
	OnlineUpdateBlockManual OnlineUpdateBlockReason = "manual_gate"
)

// OnlineUpdateBlocker names one step that refuses the online update and why.
type OnlineUpdateBlocker struct {
	StepID string                  `json:"step_id"`
	Reason OnlineUpdateBlockReason `json:"reason"`
	Detail string                  `json:"detail"`
}

// OnlineUpdatePlan answers "may a page-triggered update carry this database
// from its current ledger state to targetID?" with the evidence attached.
//
// Allowed is deliberately not the whole answer: the deploy UI has to render
// Blockers so the operator sees which exact step forces the manual runbook.
type OnlineUpdatePlan struct {
	Target string `json:"target"`
	// Pending lists, in plan order, the step IDs this range would apply.
	Pending []string `json:"pending"`
	// Allowed is true only when Pending is non-blocking in its entirety. An
	// empty Pending (already at target) is allowed: nothing is applied.
	Allowed bool `json:"allowed"`
	// Blockers is empty exactly when Allowed is true.
	Blockers []OnlineUpdateBlocker `json:"blockers"`
}

// Fail-closed sentinels shared by the planner and the exact-target runner.
var (
	// ErrUnknownTarget means the requested target is not in this binary's plan.
	ErrUnknownTarget = errors.New("unknown migration target")
	// ErrLedgerAhead means schema_migrations records steps this binary does
	// not know about: some newer migrate binary already ran here.
	ErrLedgerAhead = errors.New("migration ledger is ahead of this binary")
	// ErrTargetBehindLedger means the requested target is older than what is
	// already applied. This migration system has no down direction, so the
	// request can only be satisfied by a restore, never by running forward.
	ErrTargetBehindLedger = errors.New("migration target is behind the applied ledger")
	// ErrManualStepInRange means the range contains a release-gated manual
	// step that was not explicitly approved for this run.
	ErrManualStepInRange = errors.New("migration range contains a manual step")
)

// targetRange is the resolved, validated span between an applied ledger and a
// requested exact target. Every fail-closed condition is decided here so the
// planner and the runner cannot drift apart.
type targetRange struct {
	targetIndex int
	startIndex  int // highest applied plan index, -1 when nothing is applied
	startID     string
	pending     []Step
}

// resolveTargetRange validates applied+target and returns the steps the run
// would apply. Ledger entries are matched by ID rather than by count, so a gap
// (an older step missing while a newer one is recorded) is replayed rather than
// silently skipped, exactly like Up does.
func resolveTargetRange(applied []string, targetID string) (targetRange, error) {
	targetIndex, err := stepIndex(targetID)
	if err != nil {
		return targetRange{}, err
	}

	appliedSet := make(map[string]struct{}, len(applied))
	unknown := make([]string, 0)
	beyond := make([]string, 0)
	startIndex := -1
	for _, version := range applied {
		appliedSet[version] = struct{}{}
		index, indexErr := stepIndex(version)
		if indexErr != nil {
			unknown = append(unknown, version)
			continue
		}
		if index > startIndex {
			startIndex = index
		}
		if index > targetIndex {
			beyond = append(beyond, version)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return targetRange{}, fmt.Errorf("%w: schema_migrations records %v, which this migrate binary does not define; "+
			"run the release whose migrate binary owns those versions", ErrLedgerAhead, unknown)
	}
	if len(beyond) > 0 {
		slices.Sort(beyond)
		return targetRange{}, fmt.Errorf("%w: target %q is already followed by applied %v; this migration system is "+
			"forward-only, so reaching %q again requires restoring the bound dump, not running migrate",
			ErrTargetBehindLedger, targetID, beyond, targetID)
	}

	resolved := targetRange{targetIndex: targetIndex, startIndex: startIndex}
	if startIndex >= 0 {
		resolved.startID = steps[startIndex].ID
	}
	for _, step := range steps[:targetIndex+1] {
		if _, ok := appliedSet[step.ID]; ok {
			continue
		}
		resolved.pending = append(resolved.pending, step)
	}
	return resolved, nil
}

// PlanOnlineUpdate reports whether every step between the applied ledger state
// and targetID may be applied by a page-triggered update, and when it may not,
// which steps refuse and why.
//
// applied is the raw contents of schema_migrations (see AppliedVersions); order
// does not matter. targetID is the exact step ID the signed release manifest
// names as its schema target.
//
// It returns an error only for the fail-closed conditions that make the
// question meaningless: unknown target, a ledger written by a newer binary, or
// a target that is behind the ledger. A blocked-but-answerable range comes back
// as a plan with Allowed=false, never as an error.
func PlanOnlineUpdate(applied []string, targetID string) (OnlineUpdatePlan, error) {
	resolved, err := resolveTargetRange(applied, targetID)
	if err != nil {
		return OnlineUpdatePlan{}, err
	}

	plan := OnlineUpdatePlan{
		Target:   targetID,
		Pending:  make([]string, 0, len(resolved.pending)),
		Blockers: make([]OnlineUpdateBlocker, 0),
	}
	for _, step := range resolved.pending {
		plan.Pending = append(plan.Pending, step.ID)
		plan.Blockers = append(plan.Blockers, stepOnlineBlockers(step)...)
	}
	plan.Allowed = len(plan.Blockers) == 0
	return plan, nil
}

// stepOnlineBlockers returns every reason one step refuses an online update. A
// manual step that is also reviewed incompatible reports both, because the
// operator needs to know the manual gate is not the only thing standing there.
func stepOnlineBlockers(step Step) []OnlineUpdateBlocker {
	blockers := make([]OnlineUpdateBlocker, 0, 2)
	if step.Manual {
		blockers = append(blockers, OnlineUpdateBlocker{
			StepID: step.ID,
			Reason: OnlineUpdateBlockManual,
			Detail: "release-gated manual step: it must be approved and run from the operator runbook, " +
				"never crossed implicitly by a page-triggered update",
		})
	}
	switch step.OnlineUpdate.Compatibility {
	case OnlineUpdateCompatible:
	case OnlineUpdateIncompatible:
		blockers = append(blockers, OnlineUpdateBlocker{
			StepID: step.ID,
			Reason: OnlineUpdateBlockIncompatible,
			Detail: step.OnlineUpdate.Note,
		})
	default:
		blockers = append(blockers, OnlineUpdateBlocker{
			StepID: step.ID,
			Reason: OnlineUpdateBlockUnclassified,
			Detail: "step carries no online-update review; the default is to refuse",
		})
	}
	return blockers
}
