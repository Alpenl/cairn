// Command cairn-updater is the root-owned deployment helper for issue #41.
//
// # Why it is a separate process
//
// The application user must never be able to replace the program that runs on
// the next boot. If `webtag` could write /opt/webtag/releases, an application
// RCE would own every future start of the service and could shred the backups
// on the way out. systemd's ProtectSystem=strict does not help: it does not
// protect a directory that has been explicitly granted. So the write authority
// lives in a second process that the application cannot reach — different user,
// different unit, a Unix socket only Caddy may open, and its own bearer token
// that never appears in the application's environment.
//
// # What it refuses to do
//
// The helper takes an exact formal tag (vX.Y.Z) and nothing else. It does not
// accept a URL, a path, a command, a shell fragment, a branch, "latest", or a
// prerelease. Asset URLs are constructed by releasetrust.AssetURL from the
// compiled-in repository and the confirmed tag; a redirect that leaves the
// allow-list aborts the download. The trust root is the Ed25519 public key set
// compiled into internal/releasetrust, and the signature is checked over the
// exact bytes received — never over a re-serialised copy.
//
// # Why every failure is a HOLD and not a retry
//
// The dangerous window of an update is between "stop accepting writes" and
// "the new binary answered /ready with the target commit". Inside it the
// database may already have moved forward through a non-transactional or
// separately-committed migration step, and forward-only migrations cannot be
// undone by putting the old files back. A helper that guesses in that window
// turns a recoverable stop into an unrecoverable one. So every step that can
// fail names its own HOLD point, records whether the service was left stopped,
// and says what a human has to look at. The job never retries itself.
//
// # The order that matters
//
// Everything that can refuse the update is evaluated *before* the service is
// stopped: signature, hashes, provenance, both binaries' identity, host
// architecture, helper protocol, free disk, database reachability, the online
// update plan, and rollback compatibility. By the time webtag is stopped the
// only remaining questions are ones that need the service to be down.
package main
