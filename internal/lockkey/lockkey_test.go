package lockkey

import "testing"

// TestAdvisoryClassesAreUniqueAndReserveRetiredValues turns the "must not be
// reused" comments into a mechanical check.
//
// Advisory class ids are a cross-process protocol: replicas of different
// versions share one PostgreSQL lock space, so two namespaces that collide make
// unrelated operations serialize — or worse, let one of them believe it holds a
// lock the other owns. Class 2 belonged to the retired Deep Research namespace
// and stays reserved for the same reason: a mixed-version rolling upgrade must
// not have a new replica reinterpret an old replica's class-2 locks.
func TestAdvisoryClassesAreUniqueAndReserveRetiredValues(t *testing.T) {
	t.Parallel()

	classes := map[string]int32{
		"ClassSubmit":                  ClassSubmit,
		"ClassRepresentationWriteGate": ClassRepresentationWriteGate,
		"ClassSchemaMigration":         ClassSchemaMigration,
	}

	seen := make(map[int32]string, len(classes))
	for name, value := range classes {
		if previous, collides := seen[value]; collides {
			t.Errorf("advisory class %d used by both %s and %s; classes share one PostgreSQL lock space",
				value, previous, name)
			continue
		}
		seen[value] = name
	}

	for _, retiredClass := range []int32{2, 3} {
		if name, reused := seen[retiredClass]; reused {
			t.Errorf("%s reuses retired advisory class %d; a mixed-version process would reinterpret old locks",
				name, retiredClass)
		}
	}
}
