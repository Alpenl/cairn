package main

import "time"

// Phase is a step of the update state machine.
//
// The phases are the ones issue #41 lists, collapsed to the boundaries that
// actually change what recovery looks like. The important boundary is
// PhaseQuiesce: everything before it is reversible by doing nothing, and
// everything from it onwards has left a mark a human may have to undo.
type Phase string

const (
	// PhaseQueued is the state a job is created in, before the operation lock
	// has been taken.
	PhaseQueued Phase = "queued"
	// PhaseVerifyManifest re-fetches and re-verifies the signed manifest for
	// the exact tag under the operation lock. The candidate shown at check time
	// is discovery; this is the decision.
	PhaseVerifyManifest Phase = "verify_manifest"
	// PhaseDownload fetches the Core and Reader archives into root-owned
	// staging under a size, host and redirect policy.
	PhaseDownload Phase = "download"
	// PhaseVerifyArtifacts checks hashes, provenance, archive shape, and both
	// executables' --version identity against the signed manifest.
	PhaseVerifyArtifacts Phase = "verify_artifacts"
	// PhasePreflight is the last phase that can refuse without consequences:
	// disk, database reachability, the online update plan, rollback stance.
	PhasePreflight Phase = "preflight"
	// PhaseQuiesce stops webtag. From here on the installation is degraded
	// until the job either finishes or a human resolves the hold.
	PhaseQuiesce Phase = "quiesce"
	// PhaseBackup writes the custom-format dump and proves it is readable.
	PhaseBackup Phase = "backup"
	// PhaseMigrate runs the target release's own migrate binary to the exact
	// schema target and reconciles both ledgers.
	PhaseMigrate Phase = "migrate"
	// PhaseSwitch atomically repoints the Core and Reader current symlinks.
	PhaseSwitch Phase = "switch"
	// PhaseStart starts webtag and requires /health to report the target commit
	// and /ready to succeed.
	PhaseStart Phase = "start"
	// PhaseAudit records the completed state.
	PhaseAudit Phase = "audit"
	// PhaseDone is terminal success.
	PhaseDone Phase = "done"
)

// Phases is the ordered state machine, exposed so the UI can render progress
// without hard-coding the order.
var Phases = []Phase{
	PhaseQueued,
	PhaseVerifyManifest,
	PhaseDownload,
	PhaseVerifyArtifacts,
	PhasePreflight,
	PhaseQuiesce,
	PhaseBackup,
	PhaseMigrate,
	PhaseSwitch,
	PhaseStart,
	PhaseAudit,
	PhaseDone,
}

// JobState is the coarse outcome the UI branches on.
type JobState string

const (
	// JobRunning means the helper is still working. It is not a promise that
	// progress is being made, only that no terminal state has been recorded.
	JobRunning JobState = "running"
	// JobSucceeded means the target commit answered /ready.
	JobSucceeded JobState = "succeeded"
	// JobHold means the job stopped at a named point and will not continue.
	// There is no automatic retry, ever: the failure modes worth retrying are
	// indistinguishable from the ones where retrying compounds the damage.
	JobHold JobState = "hold"
)

// HoldClass tells the UI what kind of sentence to write and whether a retry
// could conceivably be safe once a human has looked.
type HoldClass string

const (
	// HoldTrust is a signature, hash, provenance, identity, repository, tag, or
	// helper-protocol failure. The candidate is not what it claims to be.
	HoldTrust HoldClass = "trust"
	// HoldPolicy is a refusal by contract rather than by failure: an
	// unclassified or manual-gated migration step, or a release the manifest
	// declares not online-updatable.
	HoldPolicy HoldClass = "policy"
	// HoldEnvironment is a host problem: disk, database reachability, missing
	// tooling, a version-incompatible pg_dump.
	HoldEnvironment HoldClass = "environment"
	// HoldIntegrity is the dangerous class: the database or the installation is
	// in a state the helper cannot reason about, such as a ledger that overshot
	// its target or a dump that will not list. It always means a human reads
	// the runbook before anything else touches this host.
	HoldIntegrity HoldClass = "integrity"
)

// Hold is the machine-readable stop point.
type Hold struct {
	Phase Phase     `json:"phase"`
	Class HoldClass `json:"class"`
	// Reason is one sentence the UI can show verbatim.
	Reason string `json:"reason"`
	// Detail carries the underlying error text. It is not shown as the headline
	// because an operator reading "exec: pg_dump: executable file not found"
	// needs to be told what that means for their database first.
	Detail string `json:"detail,omitempty"`
	// Blockers is populated when the online update plan refused. Each entry is
	// a migration step the page may not cross.
	Blockers []Blocker `json:"blockers,omitempty"`
	// ServiceStopped records whether webtag was left stopped. It is the single
	// most important field on this struct: it is the difference between "your
	// update did not happen" and "your installation is down right now".
	ServiceStopped bool `json:"service_stopped"`
	// DatabaseMigrated records whether the migration step ran at all. Combined
	// with Switched it tells a human whether restoring the previous binaries is
	// still a coherent action.
	DatabaseMigrated bool `json:"database_migrated"`
	// Switched records whether the current symlinks were already repointed.
	Switched bool `json:"switched"`
	// BackupPath is the dump taken before the migration, when one was taken.
	// The recovery runbook is bound to this exact file.
	BackupPath string `json:"backup_path,omitempty"`
	// Remediation is the operator instruction for this hold.
	Remediation string `json:"remediation"`
}

// Blocker mirrors one refused migration step. It is a helper-owned wire type
// rather than a re-export so the JSON the UI reads stays stable even if the
// migration package renames its own fields.
type Blocker struct {
	StepID string `json:"step_id"`
	Class  string `json:"class"`
	Manual bool   `json:"manual"`
	Reason string `json:"reason"`
}

// PhaseRecord is one completed or attempted phase, for the progress display.
type PhaseRecord struct {
	Phase      Phase      `json:"phase"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	OK         bool       `json:"ok"`
	Note       string     `json:"note,omitempty"`
}

// Job is the persisted record of one update attempt.
//
// It is written to disk after every transition, not held in memory, because the
// whole reason this helper is a separate process is that it must be able to
// answer "what happened" while the Core is stopped — including after the helper
// itself has been restarted mid-hold.
type Job struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	State         JobState `json:"state"`
	Phase         Phase    `json:"phase"`

	// Target is the exact tag this job was authorised for.
	Target string `json:"target"`
	// TargetCommit, SchemaTarget and RiverLedgerTarget are copied out of the
	// signed manifest once it has verified, so the record of what was intended
	// survives independently of GitHub.
	TargetCommit      string `json:"target_commit,omitempty"`
	SchemaTarget      string `json:"schema_target,omitempty"`
	RiverLedgerTarget int    `json:"river_ledger_target,omitempty"`
	ManifestSHA256    string `json:"manifest_sha256,omitempty"`
	SignatureKeyID    string `json:"signature_key_id,omitempty"`

	// FromVersion and FromCommit are what was running when the job started.
	FromVersion string `json:"from_version,omitempty"`
	FromCommit  string `json:"from_commit,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	Phases     []PhaseRecord `json:"phases"`
	Hold       *Hold         `json:"hold,omitempty"`
	BackupPath string        `json:"backup_path,omitempty"`
}

// JobSchemaVersion versions the on-disk record. A helper that finds a record it
// does not understand reports it as unreadable rather than guessing, because
// guessing about a hold point is how a stopped installation gets restarted into
// a half-migrated database.
const JobSchemaVersion = 1

// terminal reports whether the job has stopped moving.
func (job *Job) terminal() bool {
	return job.State == JobSucceeded || job.State == JobHold
}
