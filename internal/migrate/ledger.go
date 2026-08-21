package migrate

import (
	"context"
	"fmt"
	"slices"

	"webtag/internal/database"
)

// RiverLedgerLine is the migration line River records its versions under in
// river_migration. riverpgxv5 only ever uses the main line.
const RiverLedgerLine = "main"

// LedgerTargets is the pair of ledger positions a signed release manifest
// declares: the application's own schema step and River's bundled version.
// Cairn keeps two independent ledgers — schema_migrations for the steps in this
// package, river_migration for River's own schema — and a Core update is only
// complete when BOTH sit exactly on their declared target.
type LedgerTargets struct {
	// SchemaTarget is a step ID from this package's plan.
	SchemaTarget string `json:"schema_target"`
	// RiverLedgerTarget is the River bundle version, e.g. 7. Zero means "no
	// River schema expected" and is only meaningful for a database that has
	// never run River.
	RiverLedgerTarget int `json:"river_ledger_target"`
}

// LedgerProblemKind is the machine-readable failure class the deploy UI keys on.
type LedgerProblemKind string

const (
	// LedgerProblemBehind means the ledger has not reached its target.
	LedgerProblemBehind LedgerProblemKind = "behind"
	// LedgerProblemAhead means the ledger overshot its target. For a
	// forward-only migration system this cannot be corrected by migrating;
	// it means the wrong release already migrated this database.
	LedgerProblemAhead LedgerProblemKind = "ahead"
	// LedgerProblemUnknownTarget means the declared target is not a version
	// this binary can produce.
	LedgerProblemUnknownTarget LedgerProblemKind = "unknown_target"
	// LedgerProblemMissingTable means the ledger table itself is absent.
	LedgerProblemMissingTable LedgerProblemKind = "missing_table"
)

// LedgerProblem is one operator-facing reason a reconciliation failed.
type LedgerProblem struct {
	// Ledger is "schema_migrations" or "river_migration".
	Ledger string            `json:"ledger"`
	Kind   LedgerProblemKind `json:"kind"`
	Detail string            `json:"detail"`
}

// SchemaLedgerState is schema_migrations measured against its declared target.
type SchemaLedgerState struct {
	Present bool     `json:"present"`
	Target  string   `json:"target"`
	Head    string   `json:"head"`
	Applied []string `json:"applied"`
	// Missing lists plan steps at or before the target that are not recorded.
	Missing []string `json:"missing"`
	// Extra lists recorded versions past the target, plus any version this
	// binary does not define at all.
	Extra    []string `json:"extra"`
	AtTarget bool     `json:"at_target"`
}

// RiverLedgerState is river_migration measured against its declared target.
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

// LedgerReconciliation is the structured answer to "did this database land on
// exactly the manifest's two ledger targets?". OK is true only when both
// ledgers reached their target and neither went past it.
type LedgerReconciliation struct {
	OK       bool              `json:"ok"`
	Schema   SchemaLedgerState `json:"schema"`
	River    RiverLedgerState  `json:"river"`
	Problems []LedgerProblem   `json:"problems"`
}

// ReconcileLedgers checks both ledgers against the manifest's declared targets.
//
// It returns an error only when the database cannot be interrogated. A database
// that is simply in the wrong state comes back as OK=false with Problems
// populated, because that is a report the deploy UI renders rather than a
// transport failure.
func ReconcileLedgers(ctx context.Context, db database.Querier, targets LedgerTargets) (LedgerReconciliation, error) {
	if db == nil {
		return LedgerReconciliation{}, fmt.Errorf("nil querier")
	}
	result := LedgerReconciliation{Problems: make([]LedgerProblem, 0)}

	schema, schemaProblems, err := reconcileSchemaLedger(ctx, db, targets.SchemaTarget)
	if err != nil {
		return LedgerReconciliation{}, err
	}
	result.Schema = schema
	result.Problems = append(result.Problems, schemaProblems...)

	river, riverProblems, err := reconcileRiverLedger(ctx, db, targets.RiverLedgerTarget)
	if err != nil {
		return LedgerReconciliation{}, err
	}
	result.River = river
	result.Problems = append(result.Problems, riverProblems...)

	result.OK = len(result.Problems) == 0
	return result, nil
}

func reconcileSchemaLedger(ctx context.Context, db database.Querier, target string) (SchemaLedgerState, []LedgerProblem, error) {
	const ledger = "schema_migrations"
	state := SchemaLedgerState{
		Target:  target,
		Applied: []string{},
		Missing: []string{},
		Extra:   []string{},
	}
	problems := make([]LedgerProblem, 0)

	present, err := tolerateMissingLedger(ctx, db)
	if err != nil {
		return SchemaLedgerState{}, nil, err
	}
	state.Present = present
	if present {
		applied, appliedErr := AppliedVersions(ctx, db)
		if appliedErr != nil {
			return SchemaLedgerState{}, nil, appliedErr
		}
		state.Applied = applied
	} else {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemMissingTable,
			Detail: "schema_migrations does not exist; this database has never been migrated"})
	}

	if target != CurrentSchemaMigrationID {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemUnknownTarget,
			Detail: fmt.Sprintf("declared schema target %q is not the current target %q", target, CurrentSchemaMigrationID)})
		return state, problems, nil
	}

	ledgerState, classifyErr := classifySchemaVersions(state.Applied)
	if classifyErr != nil {
		state.Extra = slices.Clone(state.Applied)
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemAhead,
			Detail: classifyErr.Error()})
		return state, problems, nil //nolint:nilerr // invalid ledger contents are reconciliation findings, not query failures
	}
	state.Head = ledgerState.startVersion
	if ledgerState.kind != schemaLedgerCurrent {
		state.Missing = []string{CurrentSchemaMigrationID}
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemBehind,
			Detail: fmt.Sprintf("schema target %s not reached; missing %s", target, CurrentSchemaMigrationID)})
	}
	state.AtTarget = ledgerState.kind == schemaLedgerCurrent
	return state, problems, nil
}

func reconcileRiverLedger(ctx context.Context, db database.Querier, target int) (RiverLedgerState, []LedgerProblem, error) {
	const ledger = "river_migration"
	state := RiverLedgerState{
		Line:    RiverLedgerLine,
		Target:  target,
		Applied: []int{},
		Missing: []int{},
		Extra:   []int{},
	}
	problems := make([]LedgerProblem, 0)

	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.river_migration') IS NOT NULL`).Scan(&present); err != nil {
		return RiverLedgerState{}, nil, fmt.Errorf("probe river_migration: %w", err)
	}
	state.Present = present
	if present {
		applied, err := riverAppliedVersions(ctx, db)
		if err != nil {
			return RiverLedgerState{}, nil, err
		}
		state.Applied = applied
		if len(applied) > 0 {
			state.Head = applied[len(applied)-1]
		}
	} else {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemMissingTable,
			Detail: "river_migration does not exist; River's own migrations have never run against this database"})
	}

	bundle := RiverBundleVersions()
	if target != 0 && !slices.Contains(bundle, target) {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemUnknownTarget,
			Detail: fmt.Sprintf("declared River ledger target %d is not in this binary's River bundle %v", target, bundle)})
		return state, problems, nil
	}

	state.Missing, state.Extra = diffRiverLedger(state.Applied, bundle, target)

	if len(state.Missing) > 0 {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemBehind,
			Detail: fmt.Sprintf("River ledger target %d not reached; missing %v", target, state.Missing)})
	}
	if len(state.Extra) > 0 {
		problems = append(problems, LedgerProblem{Ledger: ledger, Kind: LedgerProblemAhead,
			Detail: fmt.Sprintf("River ledger is past target %d; unexpected %v", target, state.Extra)})
	}
	state.AtTarget = len(state.Missing) == 0 && len(state.Extra) == 0
	return state, problems, nil
}

// diffRiverLedger measures recorded River versions against the bundle prefix
// ending at target. A recorded version past the target, or one River itself
// does not ship, is extra.
func diffRiverLedger(applied, bundle []int, target int) (missing, extra []int) {
	missing, extra = []int{}, []int{}
	appliedSet := make(map[int]struct{}, len(applied))
	for _, version := range applied {
		appliedSet[version] = struct{}{}
	}
	for _, version := range bundle {
		if _, recorded := appliedSet[version]; version <= target && !recorded {
			missing = append(missing, version)
		}
	}
	bundled := make(map[int]struct{}, len(bundle))
	for _, version := range bundle {
		bundled[version] = struct{}{}
	}
	for _, version := range applied {
		if _, shipped := bundled[version]; version > target || !shipped {
			extra = append(extra, version)
		}
	}
	slices.Sort(extra)
	return missing, slices.Compact(extra)
}

func riverAppliedVersions(ctx context.Context, db database.Querier) ([]int, error) {
	rows, err := db.Query(ctx, `SELECT version FROM public.river_migration WHERE line = $1 ORDER BY version`, RiverLedgerLine)
	if err != nil {
		return nil, fmt.Errorf("query river_migration: %w", err)
	}
	defer rows.Close()

	versions := make([]int, 0, 8)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan river_migration: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate river_migration: %w", err)
	}
	return versions, nil
}
