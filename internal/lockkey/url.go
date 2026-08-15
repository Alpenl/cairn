package lockkey

import (
	"crypto/sha256"
	"encoding/binary"
)

const (
	// ClassSubmit identifies the shared URL-mutation advisory namespace.
	// Its value is a cross-process protocol constant and must remain stable
	// across replicas and rolling upgrades.
	ClassSubmit int32 = 1
	// Classes 2 and 3 are retired namespaces. Their values stay reserved so
	// mixed-version processes cannot reinterpret an old lock as a new one.
	// ClassRepresentationWriteGate identifies the RF3B representation-write
	// protocol. Ordinary tracked writers take a shared lock while operations
	// that prelock multiple revision categories take the exclusive side. This
	// value is persisted in migration SQL and must not be reused.
	ClassRepresentationWriteGate int32 = 4
	// ObjectRepresentationWriteGate is the singleton object within the RF3B
	// advisory namespace. Together with ClassRepresentationWriteGate it forms
	// the stable PostgreSQL advisory key (4, 0).
	ObjectRepresentationWriteGate int32 = 0
	// ClassSchemaMigration identifies the migration-runner namespace. It
	// serializes whole migration runs against one database, so that a
	// CREATE INDEX CONCURRENTLY still building on one runner is never mistaken
	// for an abandoned invalid index by another runner's recovery probe. This
	// value is a cross-process protocol constant and must not be reused.
	ClassSchemaMigration int32 = 5
	// ObjectSchemaMigration is the singleton object within the migration
	// namespace, forming the stable advisory key (5, 0).
	ObjectSchemaMigration int32 = 0
)

// URL returns the shared 32-bit advisory-lock object id for one URL-like
// identity in this installation.
func URL(rawURL string) int32 {
	sum := sha256.Sum256([]byte(rawURL))
	// PostgreSQL advisory object ids compare the 32-bit bit pattern; signedness
	// does not change identity.
	return int32(binary.BigEndian.Uint32(sum[:4])) //nolint:gosec // intentional uint32 bit reinterpretation
}
