package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// jobIDPattern is what a job id may look like. Job ids arrive from the URL, and
// the store turns them into file paths, so the pattern is the boundary that
// keeps `GET /api/deploy/system/jobs/..%2f..%2fetc%2fshadow` from being a file
// read. Validating the shape is cheaper to get right than sanitising a path.
var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ErrJobNotFound is returned for an unknown job id.
var ErrJobNotFound = errors.New("no such job")

// ErrOperationInProgress is returned when a different target is submitted while
// a job is running.
var ErrOperationInProgress = errors.New("another update is already in progress")

// Store persists jobs and owns the global operation lock.
//
// Two properties matter more than performance here:
//
//   - Only one job may run at a time, process-wide. Two concurrent updates
//     would race on the same symlinks and the same migration ledger.
//   - A resubmission of the same target returns the running job rather than
//     creating a second one. The browser that lost its connection and retried
//     must not be able to start a second deployment, and the operator who
//     double-clicked must not see two ids for one action.
//
// The lock is an in-process mutex plus a persisted active-job pointer. An
// in-process mutex is sufficient because the systemd unit is the only writer
// and it is not permitted to run twice; the persisted pointer is what makes a
// helper restart during a hold still report an occupied installation instead of
// cheerfully starting a second update on top of the first.
type Store struct {
	dir string
	now func() time.Time

	mu sync.Mutex
	// active is the job currently holding the operation lock, or the last job
	// if it ended in a hold that has not been acknowledged.
	active *Job
}

// activePointerName is the file naming the job that owns the operation lock.
const activePointerName = "active.json"

// NewStore opens (and creates) the job directory and recovers the active
// pointer left behind by a previous process.
func NewStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create job directory %s: %w", dir, err)
	}
	store := &Store{dir: dir, now: now}
	if err := store.recover(); err != nil {
		return nil, err
	}
	return store, nil
}

// recover reloads the active job after a helper restart.
//
// A job that was still marked running when the helper died did not finish, and
// nothing is going to finish it: the goroutine that owned it is gone. Leaving
// it as "running" would be a lie that the UI would poll forever, so it is
// converted into a hold that says exactly that. The alternative — resuming —
// would mean re-entering a state machine at an unknown point with no knowledge
// of whether the interrupted step committed.
func (store *Store) recover() error {
	data, err := os.ReadFile(store.pointerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active job pointer: %w", err)
	}
	var pointer struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return fmt.Errorf("parse active job pointer: %w", err)
	}
	job, err := store.Load(pointer.ID)
	if errors.Is(err, ErrJobNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.State == JobRunning {
		now := store.now()
		job.State = JobHold
		job.FinishedAt = &now
		job.UpdatedAt = now
		job.Hold = &Hold{
			Phase: job.Phase,
			Class: HoldIntegrity,
			Reason: fmt.Sprintf("the helper restarted while this update was in phase %q, so the update did not complete "+
				"and cannot be resumed", job.Phase),
			ServiceStopped:   phaseIndex(job.Phase) >= phaseIndex(PhaseQuiesce),
			DatabaseMigrated: phaseIndex(job.Phase) > phaseIndex(PhaseMigrate),
			Switched:         phaseIndex(job.Phase) > phaseIndex(PhaseSwitch),
			BackupPath:       job.BackupPath,
			Remediation: "Inspect the host before doing anything else: check whether webtag is running, whether the " +
				"schema and River ledgers reached the target, and which of the current symlinks were switched. " +
				"The helper will not resume an interrupted update.",
		}
		if err := store.write(job); err != nil {
			return err
		}
	}
	store.active = job
	return nil
}

func phaseIndex(phase Phase) int {
	for index, candidate := range Phases {
		if candidate == phase {
			return index
		}
	}
	return -1
}

func (store *Store) pointerPath() string { return filepath.Join(store.dir, activePointerName) }

func (store *Store) jobPath(id string) (string, error) {
	if !jobIDPattern.MatchString(id) {
		return "", ErrJobNotFound
	}
	return filepath.Join(store.dir, id+".json"), nil
}

// Load reads one job record from disk.
func (store *Store) Load(id string) (*Job, error) {
	path, err := store.jobPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // The id is constrained to 32 hex characters by jobIDPattern.
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read job %s: %w", id, err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("parse job %s: %w", id, err)
	}
	if job.SchemaVersion != JobSchemaVersion {
		return nil, fmt.Errorf("job %s was written by schema version %d, this helper speaks %d",
			id, job.SchemaVersion, JobSchemaVersion)
	}
	return &job, nil
}

// write persists a job record atomically.
//
// Write-to-temp then rename is not decoration. The status file is read by a
// browser while the Core is stopped; a torn write would be an unparseable
// status at exactly the moment the operator most needs to read it. The parent
// directory is fsynced too, so the rename itself survives a host that loses
// power during the maintenance window.
func (store *Store) write(job *Job) error {
	path, err := store.jobPath(job.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job %s: %w", job.ID, err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// setActivePointer records which job owns the operation lock.
func (store *Store) setActivePointer(id string) error {
	if id == "" {
		if err := os.Remove(store.pointerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear active job pointer: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(struct {
		ID string `json:"id"`
	}{ID: id})
	if err != nil {
		return fmt.Errorf("encode active job pointer: %w", err)
	}
	return writeFileAtomic(store.pointerPath(), append(data, '\n'), 0o600)
}

// Submit takes the operation lock for an exact target.
//
// The three outcomes are the whole concurrency contract:
//
//	(job, false, nil)                  a new job now owns the lock
//	(job, true, nil)                   this exact target is already running
//	(nil, false, ErrOperationInProgress) something else owns the lock
//
// The returned job is always a snapshot, never the record the runner is
// mutating. A deduplicated submission arrives while the state machine is
// already advancing that record, so handing the live pointer to an HTTP
// handler would be a read of a struct another goroutine is writing.
func (store *Store) Submit(target string) (*Job, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.active != nil && !store.active.terminal() {
		if store.active.Target == target {
			return snapshotOf(store.active), true, nil
		}
		return nil, false, fmt.Errorf("%w: job %s is updating to %s",
			ErrOperationInProgress, store.active.ID, store.active.Target)
	}
	// A previous job that ended in a hold still blocks new work. The
	// installation is in a state a human has not looked at yet, and starting a
	// second update on top of an unexamined half-migrated database is the
	// single worst thing this helper could do.
	if store.active != nil && store.active.State == JobHold {
		return nil, false, fmt.Errorf("%w: job %s is holding at phase %q and must be resolved by hand first",
			ErrOperationInProgress, store.active.ID, store.active.Hold.Phase)
	}

	id, err := newJobID()
	if err != nil {
		return nil, false, err
	}
	now := store.now()
	job := &Job{
		SchemaVersion: JobSchemaVersion,
		ID:            id,
		State:         JobRunning,
		Phase:         PhaseQueued,
		Target:        target,
		CreatedAt:     now,
		UpdatedAt:     now,
		Phases:        []PhaseRecord{},
	}
	if err := store.write(job); err != nil {
		return nil, false, err
	}
	if err := store.setActivePointer(id); err != nil {
		return nil, false, err
	}
	store.active = job
	return snapshotOf(job), false, nil
}

// snapshotOf copies a job record so a caller outside the lock can read it.
func snapshotOf(job *Job) *Job {
	copied := *job
	copied.Phases = append([]PhaseRecord(nil), job.Phases...)
	return &copied
}

// Get returns a job by id, preferring the in-memory active job so a reader
// never observes a status older than the one the runner has already recorded.
func (store *Store) Get(id string) (*Job, error) {
	store.mu.Lock()
	if store.active != nil && store.active.ID == id {
		snapshot := snapshotOf(store.active)
		store.mu.Unlock()
		return snapshot, nil
	}
	store.mu.Unlock()
	return store.Load(id)
}

// Active returns a snapshot of the job holding the operation lock, if any.
func (store *Store) Active() *Job {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.active == nil {
		return nil
	}
	return snapshotOf(store.active)
}

// Update applies a mutation to the active job and persists it.
func (store *Store) Update(id string, mutate func(*Job)) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.active == nil || store.active.ID != id {
		return ErrJobNotFound
	}
	mutate(store.active)
	store.active.UpdatedAt = store.now()
	return store.write(store.active)
}

// Release clears the operation lock after a job finished successfully. A job
// that ended in a hold keeps the lock on purpose; see Submit.
func (store *Store) Release(id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.active == nil || store.active.ID != id {
		return nil
	}
	if store.active.State == JobHold {
		return nil
	}
	if err := store.setActivePointer(""); err != nil {
		return err
	}
	store.active = nil
	return nil
}

func newJobID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, then renames it into place and fsyncs the directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if err := writeAndSync(temp, data, mode); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tempName, path, err)
	}
	return syncDir(dir)
}

func writeAndSync(file *os.File, data []byte, mode os.FileMode) error {
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("set mode on %s: %w", file.Name(), err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", file.Name(), err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", file.Name(), err)
	}
	return nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // The directory is helper-owned state, not caller input.
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
