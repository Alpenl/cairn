// Package representation defines the version inputs shared by authenticated
// responses, conditional requests, and client data partitioning.
package representation

import "github.com/google/uuid"

// LibraryBase is the authoritative installation-owned part of a
// representation version. RepresentationNamespace is immutable; Revision
// advances when library data changes.
type LibraryBase struct {
	RepresentationNamespace uuid.UUID
	Revision                int64
}

// Valid reports whether the base can safely participate in a client namespace
// or cache validator. A missing namespace must fail open to the handler rather
// than produce an invalid installation validator.
func (b LibraryBase) Valid() bool {
	return b.RepresentationNamespace != uuid.Nil && b.Revision >= 0
}

// VersionBase is the authoritative result of one component-aware version
// lookup. The empty component vector is reserved for identity namespace reads.
type VersionBase struct {
	RepresentationNamespace uuid.UUID
	Components              []Component
}

// ValidFor reports whether the base has a non-zero namespace and exactly the
// canonical component set selected by route policy.
func (b VersionBase) ValidFor(selected ComponentSet) bool {
	return b.RepresentationNamespace != uuid.Nil && selected.matches(b.Components)
}
