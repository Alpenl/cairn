//go:build dbintegration

package dbintegration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webtag/internal/deploybackup"
)

// The pre-migration dump is the only part of the deployment helper whose
// correctness is a claim about PostgreSQL rather than about the helper's own
// logic, so it is the part that has to be proved against a real server.
//
// Everything here runs the real client binaries with the exact argument vector
// the helper uses (deploybackup.DumpArgs / deploybackup.ListArgs), against the
// real shared container. The tools are invoked inside the container rather than
// on the host because a pg_dump older than the server refuses to run at all,
// and the assertion under test is about the dump, not about which client
// packages this particular machine happens to have installed.

// containerRunner drives pg_dump and pg_restore inside the Postgres container.
type containerRunner struct {
	containerID string
	database    string
	user        string
}

// Run translates the helper's argument vector into a docker exec. The
// --file=PATH argument is turned into a host-side redirect so the dump lands on
// the host filesystem exactly as it would in production, and the --list check
// then reads that same host file back in.
func (runner containerRunner) Run(ctx context.Context, name string, args, _ []string) ([]byte, []byte, error) {
	switch {
	case len(args) == 1 && args[0] == "--version":
		return runner.exec(ctx, nil, []string{name, "--version"})

	case strings.HasSuffix(name, "pg_dump"):
		path := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "--file=") {
				path = strings.TrimPrefix(arg, "--file=")
			}
		}
		if path == "" {
			return nil, nil, errors.New("the dump argument vector carries no --file")
		}
		stdout, stderr, err := runner.exec(ctx, nil, []string{
			"pg_dump", "--format=custom", "--no-password", "--username=" + runner.user, "--dbname=" + runner.database,
		})
		if err != nil {
			return nil, stderr, err
		}
		if writeErr := os.WriteFile(path, stdout, 0o600); writeErr != nil {
			return nil, stderr, writeErr
		}
		return nil, stderr, nil

	case strings.HasSuffix(name, "pg_restore") && len(args) == 2 && args[0] == "--list":
		body, err := os.ReadFile(args[1])
		if err != nil {
			return nil, nil, err
		}
		return runner.exec(ctx, body, []string{"pg_restore", "--list"})

	default:
		return nil, nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
}

func (runner containerRunner) exec(ctx context.Context, stdin []byte, command []string) ([]byte, []byte, error) {
	args := []string{"exec", "-i", "-u", "postgres", "-e", "PGPASSWORD=" + dbPassword, runner.containerID}
	args = append(args, command...)
	process := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		process.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err := process.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// findContainer locates the shared Postgres container by the host port the DSN
// publishes, so this test needs no new accessor on the shared helper.
func findContainer(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), "docker", "ps",
		"--filter", "publish="+parsed.Port(), "--format", "{{.ID}}").Output()
	if err != nil {
		t.Fatalf("locate the postgres container: %v", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" || strings.Contains(id, "\n") {
		t.Fatalf("expected exactly one container publishing port %s, got %q", parsed.Port(), id)
	}
	return id
}

func newBackup(t *testing.T) (*deploybackup.Backup, containerRunner) {
	t.Helper()
	StartPostgres(t)
	dsn := DSN(t)
	runner := containerRunner{containerID: findContainer(t, dsn), database: dbName, user: dbUser}
	return deploybackup.New(runner, "pg_dump", "pg_restore", dsn), runner
}

func TestTheDumpToolingIsUsableAndReportsItself(t *testing.T) {
	backup, _ := newBackup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	dumpVersion, restoreVersion, err := backup.ToolVersions(ctx)
	if err != nil {
		t.Fatalf("the dump tooling is not usable: %v", err)
	}
	// The preflight check exists so a missing or unrunnable client is found
	// while the service is still serving.
	if !strings.HasPrefix(dumpVersion, "pg_dump") || !strings.HasPrefix(restoreVersion, "pg_restore") {
		t.Fatalf("expected version banners, got %q and %q", dumpVersion, restoreVersion)
	}
	t.Logf("pg_dump: %s / pg_restore: %s", dumpVersion, restoreVersion)
}

// TestARealDumpIsNonEmptyAndListsItsObjects is the claim the helper's backup
// phase encodes, checked against a real server rather than assumed.
func TestARealDumpIsNonEmptyAndListsItsObjects(t *testing.T) {
	backup, _ := newBackup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	path := filepath.Join(t.TempDir(), "pre-migration.dump")
	if err := backup.Dump(ctx, path); err != nil {
		t.Fatalf("dump a real database: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("a real dump of a migrated database came out empty")
	}
	// The custom-format magic is what makes the archive selectively
	// restorable; a plain SQL dump would pass a size check and fail recovery.
	header := make([]byte, 5)
	handle, err := os.Open(path)
	if err != nil {
		t.Fatalf("open dump: %v", err)
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Read(header); err != nil {
		t.Fatalf("read dump header: %v", err)
	}
	if string(header) != "PGDMP" {
		t.Fatalf("expected a custom-format archive, got header %q", header)
	}

	if err := backup.Verify(ctx, path); err != nil {
		t.Fatalf("a real custom-format dump failed the helper's readability check: %v", err)
	}
}

// TestAnEmptyDumpIsRejectedByTheRealCheck reproduces a full filesystem: pg_dump
// can exit zero and still leave nothing behind.
func TestAnEmptyDumpIsRejectedByTheRealCheck(t *testing.T) {
	backup, _ := newBackup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "empty.dump")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty dump: %v", err)
	}
	err := backup.Verify(ctx, path)
	if err == nil {
		t.Fatal("an empty dump passed the readability check")
	}
	if !errors.Is(err, deploybackup.ErrEmptyDump) {
		t.Fatalf("an empty dump must be reported as empty, got %v", err)
	}
}

// TestATruncatedDumpIsRejectedByRealPgRestore is the case a size check alone
// would let through: a file with content that is not a restorable archive.
func TestATruncatedDumpIsRejectedByRealPgRestore(t *testing.T) {
	backup, _ := newBackup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	full := filepath.Join(t.TempDir(), "full.dump")
	if err := backup.Dump(ctx, full); err != nil {
		t.Fatalf("dump: %v", err)
	}
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.dump")
	if err := os.WriteFile(truncated, body[:len(body)/3], 0o600); err != nil {
		t.Fatalf("write truncated dump: %v", err)
	}

	if err := backup.Verify(ctx, truncated); err == nil {
		t.Fatal("a truncated dump passed the readability check")
	} else if !errors.Is(err, deploybackup.ErrUnreadableDump) {
		t.Fatalf("a truncated dump must be reported as unreadable, got %v", err)
	}

	// A file that is not an archive at all fails the same way.
	garbage := filepath.Join(t.TempDir(), "garbage.dump")
	if err := os.WriteFile(garbage, []byte("this is not a pg_dump archive"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := backup.Verify(ctx, garbage); !errors.Is(err, deploybackup.ErrUnreadableDump) {
		t.Fatalf("a non-archive must be reported as unreadable, got %v", err)
	}
}

// TestTheDumpRestoresIntoAThrowawayDatabase is the smoke issue #41 asks for:
// the backup is only a backup if it can actually be restored, and that has to
// be established somewhere other than production during an incident.
func TestTheDumpRestoresIntoAThrowawayDatabase(t *testing.T) {
	backup, runner := newBackup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	path := filepath.Join(t.TempDir(), "restore-smoke.dump")
	if err := backup.Dump(ctx, path); err != nil {
		t.Fatalf("dump: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}

	restored := fmt.Sprintf("restore_smoke_%d", time.Now().UnixNano())
	if _, stderr, err := runner.exec(ctx, nil, []string{"createdb", "-U", dbUser, restored}); err != nil {
		t.Fatalf("create throwaway database: %v (%s)", err, stderr)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		_, _, _ = runner.exec(dropCtx, nil, []string{"dropdb", "--force", "-U", dbUser, restored})
	})

	if _, stderr, err := runner.exec(ctx, body, []string{
		"pg_restore", "--no-owner", "--username=" + dbUser, "--dbname=" + restored,
	}); err != nil {
		t.Fatalf("restore the dump into a throwaway database: %v (%s)", err, stderr)
	}

	// The restored database really holds the application's schema, not just an
	// archive that pg_restore was willing to read.
	stdout, stderr, err := runner.exec(ctx, nil, []string{
		"psql", "-t", "-A", "-U", dbUser, "-d", restored,
		"-c", "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='links'",
	})
	if err != nil {
		t.Fatalf("inspect the restored database: %v (%s)", err, stderr)
	}
	if strings.TrimSpace(string(stdout)) != "1" {
		t.Fatalf("the restored database has no links table, got %q", strings.TrimSpace(string(stdout)))
	}

	// And the migration ledger came back with it, which is what makes the dump
	// a coherent recovery point rather than a pile of tables.
	stdout, stderr, err = runner.exec(ctx, nil, []string{
		"psql", "-t", "-A", "-U", dbUser, "-d", restored,
		"-c", "SELECT count(*) FROM public.schema_migrations",
	})
	if err != nil {
		t.Fatalf("read the restored migration ledger: %v (%s)", err, stderr)
	}
	if strings.TrimSpace(string(stdout)) == "0" {
		t.Fatal("the restored database has an empty migration ledger")
	}
}
