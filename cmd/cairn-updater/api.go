package main

import "time"

// APISchemaVersion versions every response body below. The Reader is shipped
// from the same release as the helper, but a held update means the two can be
// one version apart for as long as the hold lasts, so the UI needs to be able
// to notice that it is reading a document it does not understand.
const APISchemaVersion = 1

// HelperIdentity describes the running helper. It is reported even when
// everything else fails, because "which helper is this" is the first question
// of any deployment incident.
type HelperIdentity struct {
	Protocol  int    `json:"protocol"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// CoreIdentity is what the application currently reports about itself.
//
// Reachable is separate from the fields so the settings page can distinguish
// "the Core is stopped" from "the Core is running an unknown version". During a
// held update the first is normal and the second would be alarming.
type CoreIdentity struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Error     string `json:"error,omitempty"`
}

// VersionResponse is GET /api/deploy/system/version.
type VersionResponse struct {
	SchemaVersion int            `json:"schema_version"`
	Helper        HelperIdentity `json:"helper"`
	Repo          string         `json:"repo"`
	InstallMode   string         `json:"install_mode"`
	// Eligible says whether this installation can be updated from the page at
	// all. A source or Docker installation is permanently ineligible and the
	// page must show manual instructions instead of an update button.
	Eligible         bool         `json:"eligible"`
	IneligibleReason string       `json:"ineligible_reason,omitempty"`
	Current          CoreIdentity `json:"current"`
	// ActiveJobID lets a page that was reloaded mid-update find the job it lost
	// without having to remember an id across a restart of the browser.
	ActiveJobID string `json:"active_job_id,omitempty"`
}

// Candidate is the exact release the operator would be confirming.
//
// Everything here comes out of a manifest whose signature has already verified,
// so the tag, commit and schema target the page displays are the same three
// values the helper would act on. ManifestSHA256 is the digest of the exact
// bytes that verified, so an operator can compare it against the release page.
type Candidate struct {
	Tag                   string `json:"tag"`
	Version               string `json:"version"`
	Commit                string `json:"commit"`
	BuildTime             string `json:"build_time"`
	ManifestSHA256        string `json:"manifest_sha256"`
	SignatureKeyID        string `json:"signature_key_id"`
	SchemaTarget          string `json:"schema_target"`
	RiverLedgerTarget     int    `json:"river_ledger_target"`
	MinimumHelperProtocol int    `json:"minimum_helper_protocol"`
	CoreArchive           string `json:"core_archive"`
	CoreSHA256            string `json:"core_sha256"`
	CoreSizeBytes         int64  `json:"core_size_bytes"`
	ReaderArchive         string `json:"reader_archive"`
	ReaderSHA256          string `json:"reader_sha256"`
	ReaderSizeBytes       int64  `json:"reader_size_bytes"`
	// OnlineUpdateCompatible and RollbackCompatible are the release's own
	// declarations. The UI shows both before asking for confirmation: an
	// operator agreeing to an update that cannot be undone by swapping binaries
	// back should be told so in the same sentence they agree to.
	OnlineUpdateCompatible bool   `json:"online_update_compatible"`
	OnlineUpdateReason     string `json:"online_update_reason"`
	RollbackCompatible     bool   `json:"rollback_compatible"`
	RollbackReason         string `json:"rollback_reason"`
}

// CheckUpdatesResponse is GET /api/deploy/system/check-updates.
type CheckUpdatesResponse struct {
	SchemaVersion int          `json:"schema_version"`
	CheckedAt     time.Time    `json:"checked_at"`
	Cached        bool         `json:"cached"`
	Current       CoreIdentity `json:"current"`
	// Candidate is nil when discovery failed or the newest release did not
	// verify. A nil candidate with a populated DiscoveryError is the read-only
	// degraded state: the page still shows the running version.
	Candidate       *Candidate `json:"candidate,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	// CanUpdate is the single boolean the update button binds to.
	CanUpdate      bool   `json:"can_update"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	DiscoveryError string `json:"discovery_error,omitempty"`
}

// SubmitJobRequest is the POST /api/deploy/system/jobs body.
//
// One field, and it is an exact tag. There is no URL, no path, no channel, no
// "latest", and no options object that a future field could smuggle behaviour
// into.
type SubmitJobRequest struct {
	Target string `json:"target"`
}

// SubmitJobResponse is returned immediately, before any work happens.
type SubmitJobResponse struct {
	SchemaVersion int      `json:"schema_version"`
	JobID         string   `json:"job_id"`
	Target        string   `json:"target"`
	State         JobState `json:"state"`
	Phase         Phase    `json:"phase"`
	// Deduplicated is true when this request joined a job that was already
	// running for the same target instead of creating one. A retrying browser
	// must be able to tell that it did not start a second deployment.
	Deduplicated bool `json:"deduplicated"`
}

// JobResponse is GET /api/deploy/system/jobs/{id}.
type JobResponse struct {
	SchemaVersion int      `json:"schema_version"`
	JobID         string   `json:"job_id"`
	State         JobState `json:"state"`
	Phase         Phase    `json:"phase"`
	// Order is the full phase sequence so the UI can render progress without
	// hard-coding it and without guessing what comes next after a hold.
	Order             []Phase       `json:"order"`
	Target            string        `json:"target"`
	TargetCommit      string        `json:"target_commit,omitempty"`
	SchemaTarget      string        `json:"schema_target,omitempty"`
	RiverLedgerTarget int           `json:"river_ledger_target,omitempty"`
	ManifestSHA256    string        `json:"manifest_sha256,omitempty"`
	SignatureKeyID    string        `json:"signature_key_id,omitempty"`
	FromVersion       string        `json:"from_version,omitempty"`
	FromCommit        string        `json:"from_commit,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
	FinishedAt        *time.Time    `json:"finished_at,omitempty"`
	Phases            []PhaseRecord `json:"phases"`
	Hold              *Hold         `json:"hold,omitempty"`
	BackupPath        string        `json:"backup_path,omitempty"`
}

func newJobResponse(job *Job) JobResponse {
	return JobResponse{
		SchemaVersion:     APISchemaVersion,
		JobID:             job.ID,
		State:             job.State,
		Phase:             job.Phase,
		Order:             Phases,
		Target:            job.Target,
		TargetCommit:      job.TargetCommit,
		SchemaTarget:      job.SchemaTarget,
		RiverLedgerTarget: job.RiverLedgerTarget,
		ManifestSHA256:    job.ManifestSHA256,
		SignatureKeyID:    job.SignatureKeyID,
		FromVersion:       job.FromVersion,
		FromCommit:        job.FromCommit,
		CreatedAt:         job.CreatedAt,
		UpdatedAt:         job.UpdatedAt,
		FinishedAt:        job.FinishedAt,
		Phases:            job.Phases,
		Hold:              job.Hold,
		BackupPath:        job.BackupPath,
	}
}

// ErrorResponse is the single error shape every route uses.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError carries a stable machine code beside the human message, so the UI
// can branch on "this token is wrong" versus "this target is malformed"
// without matching on prose.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable error codes.
const (
	// CodeUnauthorized is the only answer an unauthenticated caller ever gets.
	// It is deliberately identical for a missing header, a wrong scheme, an
	// empty token and a wrong token: telling the caller which of those it was
	// is telling it how to make progress.
	CodeUnauthorized = "unauthorized"
	// CodeInvalidTarget is a target that is not an exact formal release tag.
	CodeInvalidTarget = "invalid_target"
	// CodeInvalidRequest is a malformed body.
	CodeInvalidRequest = "invalid_request"
	// CodeOperationInProgress is the operation lock refusing a second target.
	CodeOperationInProgress = "operation_in_progress"
	// CodeNotFound is an unknown job id.
	CodeNotFound = "not_found"
	// CodeIneligible is a source or Docker installation.
	CodeIneligible = "ineligible"
	// CodeInternal is an unexpected helper failure.
	CodeInternal = "internal"
)
