package migrate

import (
	"errors"
	"fmt"
)

// OnlineUpdateCompatibility records whether the previous Core binary remains
// valid after the current schema target lands. The zero value denies updates.
type OnlineUpdateCompatibility string

const (
	OnlineUpdateUnclassified OnlineUpdateCompatibility = ""
	OnlineUpdateIncompatible OnlineUpdateCompatibility = "incompatible"
	OnlineUpdateCompatible   OnlineUpdateCompatibility = "compatible"
)

type OnlineUpdateReview struct {
	Compatibility OnlineUpdateCompatibility
	Note          string
}

func OnlineCompatible(note string) OnlineUpdateReview {
	return OnlineUpdateReview{Compatibility: OnlineUpdateCompatible, Note: note}
}

func OnlineIncompatible(note string) OnlineUpdateReview {
	return OnlineUpdateReview{Compatibility: OnlineUpdateIncompatible, Note: note}
}

type OnlineUpdateBlockReason string

const (
	OnlineUpdateBlockUnclassified OnlineUpdateBlockReason = "unclassified"
	OnlineUpdateBlockIncompatible OnlineUpdateBlockReason = "incompatible"
	// OnlineUpdateBlockManual remains part of report schema version 1 so older
	// updater fixtures still decode. The single-head runner never emits it.
	OnlineUpdateBlockManual OnlineUpdateBlockReason = "manual_gate"
)

type OnlineUpdateBlocker struct {
	StepID string                  `json:"step_id"`
	Reason OnlineUpdateBlockReason `json:"reason"`
	Detail string                  `json:"detail"`
}

type OnlineUpdatePlan struct {
	Target   string                `json:"target"`
	Pending  []string              `json:"pending"`
	Allowed  bool                  `json:"allowed"`
	Blockers []OnlineUpdateBlocker `json:"blockers"`
}

var (
	ErrUnknownTarget = errors.New("unknown migration target")
	ErrLedgerAhead   = errors.New("migration ledger is ahead of this binary")
)

func resolveTargetRange(applied []string, targetID string) ([]Step, error) {
	if targetID != CurrentSchemaMigrationID {
		return nil, fmt.Errorf("%w %q; known target: %s", ErrUnknownTarget, targetID, CurrentSchemaMigrationID)
	}
	ledger, err := classifySchemaVersions(applied)
	if err != nil {
		return nil, err
	}
	if ledger.kind == schemaLedgerCurrent {
		return []Step{}, nil
	}
	return []Step{steps[0]}, nil
}

// PlanOnlineUpdate answers whether the current target may be applied while the
// previous Core binary is still serving. Invalid targets and ledgers fail
// closed before a plan is returned.
func PlanOnlineUpdate(applied []string, targetID string) (OnlineUpdatePlan, error) {
	pending, err := resolveTargetRange(applied, targetID)
	if err != nil {
		return OnlineUpdatePlan{}, err
	}
	plan := OnlineUpdatePlan{
		Target:   targetID,
		Pending:  make([]string, 0, len(pending)),
		Blockers: make([]OnlineUpdateBlocker, 0, len(pending)),
	}
	for _, step := range pending {
		plan.Pending = append(plan.Pending, step.ID)
		if blocker, blocked := stepOnlineBlocker(step); blocked {
			plan.Blockers = append(plan.Blockers, blocker)
		}
	}
	plan.Allowed = len(plan.Blockers) == 0
	return plan, nil
}

func stepOnlineBlocker(step Step) (OnlineUpdateBlocker, bool) {
	switch step.OnlineUpdate.Compatibility {
	case OnlineUpdateCompatible:
		return OnlineUpdateBlocker{}, false
	case OnlineUpdateIncompatible:
		return OnlineUpdateBlocker{
			StepID: step.ID,
			Reason: OnlineUpdateBlockIncompatible,
			Detail: step.OnlineUpdate.Note,
		}, true
	default:
		return OnlineUpdateBlocker{
			StepID: step.ID,
			Reason: OnlineUpdateBlockUnclassified,
			Detail: "step carries no online-update review; the default is to refuse",
		}, true
	}
}
