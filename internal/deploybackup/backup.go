// Package deploybackup takes the pre-migration database dump and proves it is
// restorable.
//
// It is a package rather than a few methods inside cmd/cairn-updater for one
// reason: this is the only part of the deployment helper whose correctness is a
// claim about PostgreSQL rather than about the helper's own logic. "pg_dump
// exited zero" is not evidence that a recoverable backup exists, and the only
// way to establish what is evidence is to run the real tools against a real
// server. A package can be tested that way from the opt-in integration module;
// an unexported method inside a main package cannot.
//
// The package is stdlib-only and holds no database driver. It never connects to
// PostgreSQL itself — it drives the client binaries, which is what the recovery
// runbook will also do.
package deploybackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
)

// Runner runs one external program to completion. The helper supplies its own
// implementation so every subprocess in the deployment path goes through one
// audited seam.
type Runner interface {
	Run(ctx context.Context, name string, args []string, env []string) (stdout []byte, stderr []byte, err error)
}

// Backup drives pg_dump and pg_restore against one database.
type Backup struct {
	runner      Runner
	pgDump      string
	pgRestore   string
	databaseURL string
}

// New binds the dump tooling to one database URL.
func New(runner Runner, pgDump, pgRestore, databaseURL string) *Backup {
	return &Backup{runner: runner, pgDump: pgDump, pgRestore: pgRestore, databaseURL: databaseURL}
}

// DumpArgs is the exact argument vector the dump uses.
//
// It is exported so the integration test can assert against the same vector the
// helper runs, rather than against a paraphrase of it that could drift.
//
// --format=custom is required rather than preferred. A plain SQL dump cannot be
// listed without replaying it and cannot be restored selectively, so a recovery
// runbook bound to one would have no way to check its own backup before the
// moment it needs it.
func DumpArgs(path, databaseURL string) []string {
	return []string{"--format=custom", "--no-password", "--file=" + path, databaseURL}
}

// ListArgs is the exact argument vector the readability check uses.
func ListArgs(path string) []string {
	return []string{"--list", path}
}

// ErrEmptyDump is the specific finding that a dump file has no content.
var ErrEmptyDump = errors.New("the dump is empty")

// ErrUnreadableDump is the specific finding that a dump cannot be listed.
var ErrUnreadableDump = errors.New("the dump is not a restorable archive")

// Dump writes a custom-format dump to path.
func (backup *Backup) Dump(ctx context.Context, path string) error {
	if _, stderr, err := backup.runner.Run(ctx, backup.pgDump, DumpArgs(path, backup.databaseURL), nil); err != nil {
		return fmt.Errorf("write the pre-migration dump to %s: %w (%s)", path, err, firstLine(stderr))
	}
	return nil
}

// Verify proves the dump at path is a restorable archive.
//
// The two checks are ordered on purpose. A zero-length file is what a full
// filesystem produces, and saying so directly is far more useful to an operator
// than whatever pg_restore reports about an empty file. Only once the file has
// content is its readability a meaningful question.
func (backup *Backup) Verify(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect the dump at %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: %s holds no bytes, so there is nothing to recover from", ErrEmptyDump, path)
	}
	stdout, stderr, err := backup.runner.Run(ctx, backup.pgRestore, ListArgs(path), nil)
	if err != nil {
		return fmt.Errorf("%w: %s could not be listed: %w (%s)", ErrUnreadableDump, path, err, firstLine(stderr))
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return fmt.Errorf("%w: %s listed no objects", ErrUnreadableDump, path)
	}
	return nil
}

// ToolVersions reports what pg_dump and pg_restore are, proving both exist and
// run. It is called in preflight: discovering a missing pg_dump after the
// service has been stopped would mean an installation that is down with no
// backup and no way to take one.
func (backup *Backup) ToolVersions(ctx context.Context) (dumpVersion, restoreVersion string, err error) {
	for _, tool := range []struct {
		path string
		into *string
	}{{backup.pgDump, &dumpVersion}, {backup.pgRestore, &restoreVersion}} {
		stdout, stderr, runErr := backup.runner.Run(ctx, tool.path, []string{"--version"}, nil)
		if runErr != nil {
			return "", "", fmt.Errorf("%s is not usable on this host: %w (%s)", tool.path, runErr, firstLine(stderr))
		}
		*tool.into = string(bytes.TrimSpace(stdout))
	}
	return dumpVersion, restoreVersion, nil
}

func firstLine(data []byte) string {
	text := bytes.TrimSpace(data)
	if len(text) == 0 {
		return "no stderr output"
	}
	if index := bytes.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return string(text)
}
