// Package releasetrust is the release trust root for the page-triggered Core
// update path (issue #41).
//
// It is deliberately a zero-dependency leaf: the root-owned cairn-updater
// helper links this package directly, so everything the helper needs to decide
// "is this release authentic and installable here" lives in one importable
// place instead of being spread across release shell scripts.
//
// The package owns four things:
//
//  1. The canonical JSON encoding (canonical.go). A signature is only stable if
//     the signed bytes are stable, so the manifest has exactly one byte
//     representation and a manifest that is not already in that representation
//     is rejected rather than normalised.
//
//  2. The release manifest schema (manifest.go). Every field the helper is
//     allowed to act on is declared, validated, and cross-checked here.
//
//  3. The Ed25519 trust root and its rotation rules (keys.go, sign.go). Public
//     keys are compiled in with validity windows; private keys never enter the
//     repository and are supplied to the signer through the environment.
//
//  4. The verification path (verify.go, archive.go, checksums.go, download.go).
//     Signature, repository, tag shape, archive hash, in-archive provenance,
//     executable identity, host architecture, and the helper protocol floor are
//     all checked from the signed manifest.
//
// SHA256SUMS is intentionally *not* a trust root here. It ships in the same
// GitHub Release as the archives, so an attacker who can replace one can
// replace both; checksums.go exists only so the helper can keep using it as a
// corruption / wrong-file check.
package releasetrust
