package contentdoc

import (
	"crypto/sha256"
	"encoding/hex"
)

const renderedSourceBlockPersistenceDomain = "webtag:rendered-source-block:v1\x00"

// RenderedSourceBlockPersistenceHash returns the durable identity for a
// canonical rendered source block. It is deliberately domain-separated from
// the raw SHA-256 exposed as expected_source_hash/current_identity on the wire,
// and includes the canonical block key so equal text in different blocks does
// not share a persistence identity.
func RenderedSourceBlockPersistenceHash(blockKey, renderedProjection string) string {
	sum := sha256.Sum256([]byte(
		renderedSourceBlockPersistenceDomain + blockKey + "\x00" + renderedProjection,
	))
	return hex.EncodeToString(sum[:])
}
