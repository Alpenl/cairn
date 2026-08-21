package migrate

import (
	"fmt"
	"slices"
)

type schemaLedgerKind uint8

const (
	schemaLedgerFresh schemaLedgerKind = iota
	schemaLedgerProductionBaseline
	schemaLedgerCurrent
)

type schemaLedgerClassification struct {
	kind         schemaLedgerKind
	startVersion string
}

// ProductionBaselineVersions returns the only historical ledger accepted by
// this binary. It is evidence for one production upgrade edge, not a list of
// runnable migration steps.
func ProductionBaselineVersions() []string {
	return slices.Clone(productionBaselineLedger)
}

func classifySchemaLedger(applied map[string]struct{}) (schemaLedgerClassification, error) {
	switch {
	case len(applied) == 0:
		return schemaLedgerClassification{kind: schemaLedgerFresh}, nil
	case versionSetEquals(applied, productionBaselineLedger):
		return schemaLedgerClassification{
			kind:         schemaLedgerProductionBaseline,
			startVersion: ProductionBaselineMigrationID,
		}, nil
	case len(applied) == 1:
		if _, current := applied[CurrentSchemaMigrationID]; current {
			return schemaLedgerClassification{
				kind:         schemaLedgerCurrent,
				startVersion: CurrentSchemaMigrationID,
			}, nil
		}
	}

	versions := make([]string, 0, len(applied))
	for version := range applied {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return schemaLedgerClassification{}, fmt.Errorf(
		"%w: schema_migrations records %v; this release accepts only an empty ledger, the exact v0.1.17 production ledger, or %q",
		ErrLedgerAhead, versions, CurrentSchemaMigrationID,
	)
}

func classifySchemaVersions(applied []string) (schemaLedgerClassification, error) {
	versions := make(map[string]struct{}, len(applied))
	for _, version := range applied {
		versions[version] = struct{}{}
	}
	return classifySchemaLedger(versions)
}

func versionSetEquals(applied map[string]struct{}, expected []string) bool {
	if len(applied) != len(expected) {
		return false
	}
	for _, version := range expected {
		if _, ok := applied[version]; !ok {
			return false
		}
	}
	return true
}
