package deploybackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These are the argument- and ordering-level facts. Whether a real dump is
// actually restorable is a claim about PostgreSQL and is proved in
// test/dbintegration against a real server.

type recordingRunner struct {
	calls  [][]string
	stdout map[string][]byte
	stderr []byte
	err    error
	// onDump writes the file pg_dump would have written.
	onDump func(path string) error
}

func (runner *recordingRunner) Run(_ context.Context, name string, args, _ []string) ([]byte, []byte, error) {
	runner.calls = append(runner.calls, append([]string{name}, args...))
	if runner.onDump != nil && strings.HasSuffix(name, "pg_dump") && len(args) > 1 {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--file=") {
				if err := runner.onDump(strings.TrimPrefix(arg, "--file=")); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	if runner.err != nil {
		return nil, runner.stderr, runner.err
	}
	return runner.stdout[name], runner.stderr, nil
}

func TestTheDumpIsAlwaysCustomFormat(t *testing.T) {
	// A plain SQL dump cannot be listed without replaying it and cannot be
	// restored selectively, so the format is part of the contract rather than a
	// preference.
	args := DumpArgs("/opt/webtag/backups/v1.2.3.dump", "postgres://webtag@127.0.0.1:5433/webtag")
	if !slices.Contains(args, "--format=custom") {
		t.Fatalf("the dump must be custom-format, got %v", args)
	}
	if !slices.Contains(args, "--no-password") {
		t.Fatalf("the dump must never wait on a password prompt, got %v", args)
	}
	if !slices.Contains(args, "--file=/opt/webtag/backups/v1.2.3.dump") {
		t.Fatalf("the dump must be written to the recorded path, got %v", args)
	}
	// The connection string is passed as one argv element, so nothing inside it
	// can become a second argument.
	if args[len(args)-1] != "postgres://webtag@127.0.0.1:5433/webtag" {
		t.Fatalf("the database URL must be a single argument, got %v", args)
	}
}

func TestAnEmptyDumpIsNamedAsEmptyRatherThanUnreadable(t *testing.T) {
	// "Empty" is what a full filesystem produces, and saying so is far more
	// useful to an operator than whatever pg_restore says about zero bytes.
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.dump")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := &recordingRunner{}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")

	err := backup.Verify(context.Background(), path)
	if !errors.Is(err, ErrEmptyDump) {
		t.Fatalf("expected ErrEmptyDump, got %v", err)
	}
	// pg_restore is never even consulted: there is nothing to consult it about.
	if len(runner.calls) != 0 {
		t.Fatalf("an empty dump must not be handed to pg_restore, got %v", runner.calls)
	}
}

func TestADumpThatListsNothingIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quiet.dump")
	if err := os.WriteFile(path, []byte("PGDMP-ish"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// pg_restore exiting zero with no output is not proof of a usable backup.
	runner := &recordingRunner{stdout: map[string][]byte{"pg_restore": []byte("   \n")}}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")

	if err := backup.Verify(context.Background(), path); !errors.Is(err, ErrUnreadableDump) {
		t.Fatalf("expected ErrUnreadableDump, got %v", err)
	}
}

func TestAFailingListIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.dump")
	if err := os.WriteFile(path, []byte("not an archive"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := &recordingRunner{
		err:    errors.New("exit status 1"),
		stderr: []byte("pg_restore: error: did not find magic string in file header\n"),
	}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")

	err := backup.Verify(context.Background(), path)
	if !errors.Is(err, ErrUnreadableDump) {
		t.Fatalf("expected ErrUnreadableDump, got %v", err)
	}
	// The operator needs the tool's own first line, not just a classification.
	if !strings.Contains(err.Error(), "magic string") {
		t.Fatalf("the failure must carry the tool's own message, got %v", err)
	}
}

func TestAGoodDumpPassesAndUsesTheDeclaredVectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "good.dump")
	runner := &recordingRunner{
		stdout: map[string][]byte{"pg_restore": []byte(";     dbname: webtag\n215; 1259 TABLE public links\n")},
		onDump: func(path string) error { return os.WriteFile(path, []byte("PGDMP\x00content"), 0o600) },
	}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")
	ctx := context.Background()

	if err := backup.Dump(ctx, path); err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := backup.Verify(ctx, path); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected one dump and one list, got %v", runner.calls)
	}
	if !slices.Equal(runner.calls[0][1:], DumpArgs(path, "postgres://x")) {
		t.Fatalf("the dump used %v, not the declared vector", runner.calls[0])
	}
	if !slices.Equal(runner.calls[1][1:], ListArgs(path)) {
		t.Fatalf("the list used %v, not the declared vector", runner.calls[1])
	}
}

func TestMissingToolingIsReportedBeforeAnythingStops(t *testing.T) {
	runner := &recordingRunner{err: errors.New("executable file not found in $PATH")}
	backup := New(runner, "/usr/bin/pg_dump", "/usr/bin/pg_restore", "postgres://x")

	if _, _, err := backup.ToolVersions(context.Background()); err == nil {
		t.Fatal("missing tooling was reported as usable")
	} else if !strings.Contains(err.Error(), "/usr/bin/pg_dump") {
		t.Fatalf("the failure must name the tool, got %v", err)
	}
}

func TestToolVersionsReportsBothBanners(t *testing.T) {
	runner := &recordingRunner{stdout: map[string][]byte{
		"pg_dump":    []byte("pg_dump (PostgreSQL) 16.4\n"),
		"pg_restore": []byte("pg_restore (PostgreSQL) 16.4\n"),
	}}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")

	dump, restore, err := backup.ToolVersions(context.Background())
	if err != nil {
		t.Fatalf("tool versions: %v", err)
	}
	if dump != "pg_dump (PostgreSQL) 16.4" || restore != "pg_restore (PostgreSQL) 16.4" {
		t.Fatalf("got %q and %q", dump, restore)
	}
}

func TestAFailedDumpNamesTheDestination(t *testing.T) {
	runner := &recordingRunner{
		err:    errors.New("exit status 1"),
		stderr: []byte("pg_dump: error: connection to server failed\n"),
	}
	backup := New(runner, "pg_dump", "pg_restore", "postgres://x")
	err := backup.Dump(context.Background(), "/opt/webtag/backups/v1.2.3.dump")
	if err == nil {
		t.Fatal("a failed dump was reported as successful")
	}
	if !strings.Contains(err.Error(), "/opt/webtag/backups/v1.2.3.dump") ||
		!strings.Contains(err.Error(), "connection to server failed") {
		t.Fatalf("the failure must name the destination and the cause, got %v", err)
	}
}
