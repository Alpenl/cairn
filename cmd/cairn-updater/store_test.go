package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Job records exist on disk because the Core is stopped for part of every
// update, and a status a browser cannot read during the maintenance window is
// not a status. These tests are about that record surviving things.

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "jobs")
	store, err := NewStore(dir, time.Now)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, dir
}

func TestAJobRecordSurvivesTheProcessThatWroteIt(t *testing.T) {
	store, dir := newTestStore(t)
	job, deduplicated, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if deduplicated {
		t.Fatal("the first submission cannot be a duplicate")
	}
	if err := store.Update(job.ID, func(job *Job) {
		job.Phase = PhaseMigrate
		job.TargetCommit = fixtureCommit
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reopened, err := NewStore(dir, time.Now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	recovered, err := reopened.Load(job.ID)
	if err != nil {
		t.Fatalf("load after reopen: %v", err)
	}
	if recovered.Target != "v1.2.3" || recovered.TargetCommit != fixtureCommit {
		t.Fatalf("the record did not survive: %+v", recovered)
	}
}

// TestAnInterruptedJobBecomesAHoldRatherThanStayingRunning is the recovery
// contract. A job that was running when the helper died did not finish and
// nothing is going to finish it, so reporting "running" forever would be a lie
// the UI would poll indefinitely — and resuming would mean re-entering the
// state machine at an unknown point.
func TestAnInterruptedJobBecomesAHoldRatherThanStayingRunning(t *testing.T) {
	store, dir := newTestStore(t)
	job, _, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := store.Update(job.ID, func(job *Job) { job.Phase = PhaseMigrate }); err != nil {
		t.Fatalf("update: %v", err)
	}

	reopened, err := NewStore(dir, time.Now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	recovered, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if recovered.State != JobHold {
		t.Fatalf("expected an interrupted job to hold, got %s", recovered.State)
	}
	if recovered.Hold.Class != HoldIntegrity {
		t.Fatalf("an interrupted migration is an integrity hold, got %s", recovered.Hold.Class)
	}
	// It has to say what state the host might be in, phase by phase.
	if !recovered.Hold.ServiceStopped {
		t.Fatal("a job interrupted after quiesce must record that the service is stopped")
	}
	if recovered.Hold.Switched {
		t.Fatal("a job interrupted at the migration must not claim the switch happened")
	}
	if !strings.Contains(recovered.Hold.Remediation, "will not resume") {
		t.Fatalf("the remediation must say the helper will not resume, got %q", recovered.Hold.Remediation)
	}

	// And the operation lock is still held, so nothing starts a second update
	// on top of an unexamined half-migrated database.
	if _, _, err := reopened.Submit("v1.2.4"); err == nil {
		t.Fatal("a second update was allowed on top of an unresolved hold")
	}
}

func TestAnInterruptedDownloadHoldsWithoutClaimingAnOutage(t *testing.T) {
	store, dir := newTestStore(t)
	job, _, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := store.Update(job.ID, func(job *Job) { job.Phase = PhaseDownload }); err != nil {
		t.Fatalf("update: %v", err)
	}
	reopened, err := NewStore(dir, time.Now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	recovered, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if recovered.Hold.ServiceStopped {
		t.Fatal("a job interrupted before quiesce must not claim the service is stopped")
	}
}

func TestASucceededJobReleasesTheOperationLock(t *testing.T) {
	store, _ := newTestStore(t)
	first, _, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := store.Update(first.ID, func(job *Job) { job.State = JobSucceeded }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := store.Release(first.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, deduplicated, err := store.Submit("v1.2.4")
	if err != nil {
		t.Fatalf("a second update after success was refused: %v", err)
	}
	if deduplicated || second.ID == first.ID {
		t.Fatal("a new target after a completed job must get a new job")
	}
}

func TestAHeldJobKeepsTheOperationLock(t *testing.T) {
	store, _ := newTestStore(t)
	job, _, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := store.Update(job.ID, func(job *Job) {
		job.State = JobHold
		job.Hold = &Hold{Phase: PhaseMigrate, Class: HoldIntegrity}
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Release is a no-op for a hold on purpose: the installation is in a state
	// nobody has looked at.
	if err := store.Release(job.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, err := store.Submit("v1.2.4"); err == nil {
		t.Fatal("a held job released the lock")
	}
}

func TestJobIdentifiersCannotBecomePaths(t *testing.T) {
	store, dir := newTestStore(t)
	// The id arrives from a URL and is turned into a file path. Validating the
	// shape is the boundary; sanitising a path afterwards is not.
	secret := filepath.Join(filepath.Dir(dir), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"schema_version":1,"id":"x"}`), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	for _, id := range []string{
		"../secret", "..%2fsecret", "/etc/passwd", "abc", strings.Repeat("f", 33),
		"0123456789ABCDEF0123456789abcdef", "0123456789abcdef0123456789abcde.",
	} {
		if _, err := store.Load(id); err == nil {
			t.Fatalf("job id %q resolved to a file", id)
		}
	}
}

func TestARecordFromAnUnknownSchemaIsRefusedRatherThanGuessedAt(t *testing.T) {
	store, dir := newTestStore(t)
	id := "0123456789abcdef0123456789abcdef"
	future := map[string]any{"schema_version": JobSchemaVersion + 1, "id": id, "state": "running"}
	encoded, err := json.Marshal(future)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), encoded, 0o600); err != nil {
		t.Fatalf("write future record: %v", err)
	}
	if _, err := store.Load(id); err == nil {
		t.Fatal("a record from a newer schema was read as if it were understood")
	}
}

func TestTheJobRecordIsWrittenAtomically(t *testing.T) {
	store, dir := newTestStore(t)
	job, _, err := store.Submit("v1.2.3")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	for range 50 {
		if err := store.Update(job.ID, func(job *Job) {
			job.Phases = append(job.Phases, PhaseRecord{Phase: PhaseMigrate, StartedAt: time.Now()})
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	// No temporary files were left behind: a torn write during a maintenance
	// window is exactly when the status matters most.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read job directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("a temporary write artefact was left behind: %s", entry.Name())
		}
	}
	if _, err := store.Load(job.ID); err != nil {
		t.Fatalf("the record is unreadable after repeated writes: %v", err)
	}
}

func TestTheJobDirectoryIsOwnerOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("this suite targets Linux hosts")
	}
	_, dir := newTestStore(t)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat job directory: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the job directory is mode %04o and readable beyond its owner", info.Mode().Perm())
	}
}
