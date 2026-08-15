package model

import (
	"time"

	"github.com/google/uuid"
)

type TranslationScope string

const (
	TranslationScopeSelection TranslationScope = "selection"
	TranslationScopeFull      TranslationScope = "full"
)

type TranslationStatus string

const (
	TranslationStatusPending    TranslationStatus = "pending"
	TranslationStatusProcessing TranslationStatus = "processing"
	TranslationStatusDone       TranslationStatus = "done"
	TranslationStatusFailed     TranslationStatus = "failed"
)

type TranslationFormat string

const (
	TranslationFormatPlain    TranslationFormat = "plain"
	TranslationFormatMarkdown TranslationFormat = "markdown"
)

const TranslationTargetChinese = "zh-CN"

const (
	// TranslationJobKindLegacy is the pre-RF6A River kind retained only for
	// compat/drain processing during the rolling protocol cutover.
	TranslationJobKindLegacy = "translate_link_content"
	// TranslationJobKindV2 is the strict current-attempt River protocol.
	TranslationJobKindV2 = "translate_link_v2"
)

// TranslationJobsRolloutStage coordinates old and v2 River workers with the
// API scheduling gate during a rolling deployment.
type TranslationJobsRolloutStage string

const (
	TranslationJobsRolloutCompatV1 TranslationJobsRolloutStage = "compat-v1"
	TranslationJobsRolloutDrainV1  TranslationJobsRolloutStage = "drain-v1"
	TranslationJobsRolloutStrictV2 TranslationJobsRolloutStage = "strict-v2"
)

// TranslationJobScheduleProtocol is the durable River wire format selected by
// a rollout policy. Callers derive it from TranslationJobsRolloutStage rather
// than accepting it as independent configuration.
type TranslationJobScheduleProtocol string

const (
	TranslationJobScheduleLegacy TranslationJobScheduleProtocol = "legacy"
	TranslationJobSchedulePaused TranslationJobScheduleProtocol = "paused"
	TranslationJobScheduleV2     TranslationJobScheduleProtocol = "v2"
)

// TranslationJobsPolicy is the authoritative worker-registration and
// scheduling behavior for one rolling-deployment stage.
type TranslationJobsPolicy struct {
	RegisterLegacyWorker    bool
	RegisterV2Worker        bool
	ScheduleProtocol        TranslationJobScheduleProtocol
	AllowExistingReschedule bool
}

// Valid reports whether the stage has a defined worker and scheduling policy.
func (s TranslationJobsRolloutStage) Valid() bool {
	_, ok := s.jobPolicy()
	return ok
}

// JobPolicy returns the stage's authoritative rollout behavior. Unknown stages
// fail closed by pausing scheduling and registering no translation workers.
func (s TranslationJobsRolloutStage) JobPolicy() TranslationJobsPolicy {
	policy, ok := s.jobPolicy()
	if !ok {
		return TranslationJobsPolicy{ScheduleProtocol: TranslationJobSchedulePaused}
	}
	return policy
}

func (s TranslationJobsRolloutStage) jobPolicy() (TranslationJobsPolicy, bool) {
	switch s {
	case TranslationJobsRolloutCompatV1:
		return TranslationJobsPolicy{
			RegisterLegacyWorker: true,
			RegisterV2Worker:     true,
			ScheduleProtocol:     TranslationJobScheduleLegacy,
		}, true
	case TranslationJobsRolloutDrainV1:
		return TranslationJobsPolicy{
			RegisterLegacyWorker: true,
			RegisterV2Worker:     true,
			ScheduleProtocol:     TranslationJobSchedulePaused,
		}, true
	case TranslationJobsRolloutStrictV2:
		return TranslationJobsPolicy{
			RegisterV2Worker:        true,
			ScheduleProtocol:        TranslationJobScheduleV2,
			AllowExistingReschedule: true,
		}, true
	default:
		return TranslationJobsPolicy{}, false
	}
}

type LinkTranslation struct {
	ID             uuid.UUID
	LinkID         uuid.UUID
	Scope          TranslationScope
	BlockKey       string
	StartOffset    int
	EndOffset      int
	SourceText     string
	TranslatedText *string
	SourceFormat   TranslationFormat
	TargetLanguage string
	SourceHash     string
	// SourceContentRevision is the saved-content generation this translation
	// belongs to. Nil is reserved for summary identities, historical retired
	// deep-research rows, and compatibility-window legacy-unverified rows.
	SourceContentRevision *int64
	Status                TranslationStatus
	Model                 *string
	ErrorMsg              *string
	// AttemptGeneration identifies the monotonically increasing scheduling
	// attempt that currently owns this reusable translation row. Generation 0
	// is reserved for historical attempts that predate the v2 River protocol.
	AttemptGeneration int64
	// CurrentRiverJobID is the exact River row allowed to project this
	// attempt. It intentionally has no database FK because River cleans
	// terminal rows on an independent retention schedule.
	CurrentRiverJobID *int64
	// Stale is a read-time projection for full-article translations whose
	// source hash no longer matches the currently saved original. It is not
	// persisted in link_translations.
	Stale     bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TranslationList is the source-aware read result for one link. The current
// saved-content generation and summary CAS hash are carried independently from
// Items so an empty or entirely legacy-unverified list still has authoritative
// identities for both source domains.
type TranslationList struct {
	CurrentContentRevision   int64
	CurrentSummarySourceHash *string
	Items                    []LinkTranslation
}

// TranslationAttempt is the complete ownership and source identity a worker
// must present before it may project processing or terminal state into a
// reusable translation row.
type TranslationAttempt struct {
	TranslationID         uuid.UUID
	AttemptGeneration     int64
	RiverJobID            int64
	SourceHash            string
	SourceContentRevision *int64
}

// TranslationAttemptRejectionReason is the closed set of observable reasons
// why a River row may not project state into a translation product row.
type TranslationAttemptRejectionReason string

const (
	TranslationAttemptRejectionInvalidIdentity          TranslationAttemptRejectionReason = "invalid_identity"
	TranslationAttemptRejectionTranslationNotFound      TranslationAttemptRejectionReason = "translation_not_found"
	TranslationAttemptRejectionAmbiguousCandidates      TranslationAttemptRejectionReason = "ambiguous_candidates"
	TranslationAttemptRejectionNotCurrent               TranslationAttemptRejectionReason = "not_current"
	TranslationAttemptRejectionGenerationMismatch       TranslationAttemptRejectionReason = "generation_mismatch"
	TranslationAttemptRejectionExplicitRecoveryRequired TranslationAttemptRejectionReason = "explicit_recovery_required"
	TranslationAttemptRejectionSourceStatusMismatch     TranslationAttemptRejectionReason = "source_status_mismatch"
	TranslationAttemptRejectionInvalidGeneration        TranslationAttemptRejectionReason = "invalid_generation"
	TranslationAttemptRejectionIdentityMismatch         TranslationAttemptRejectionReason = "identity_mismatch"
	TranslationAttemptRejectionInvalidRiverJobID        TranslationAttemptRejectionReason = "invalid_river_job_id"
	TranslationAttemptRejectionKindMismatch             TranslationAttemptRejectionReason = "kind_mismatch"
	TranslationAttemptRejectionMalformedArgs            TranslationAttemptRejectionReason = "malformed_args"
	TranslationAttemptRejectionMissingTranslationID     TranslationAttemptRejectionReason = "missing_translation_id"
	TranslationAttemptRejectionInvalidAttemptGeneration TranslationAttemptRejectionReason = "invalid_attempt_generation"
)

// String returns the stable value used in structured rejection logs.
func (r TranslationAttemptRejectionReason) String() string { return string(r) }

// TranslationAttemptResolution is the proof result shared by repositories,
// workers, terminal handlers, logs, and tests. Rejection is empty only when
// Attempt contains the exact current owner allowed to project state.
type TranslationAttemptResolution struct {
	Attempt   TranslationAttempt
	Rejection TranslationAttemptRejectionReason
}

// Rejected reports whether proof denied this River attempt.
func (r TranslationAttemptResolution) Rejected() bool { return r.Rejection != "" }

// TranslationAttemptSeed is the immutable product, attempt, and source identity
// known before the River row is inserted. The scheduler returns the River ID
// that completes an Attempt.
type TranslationAttemptSeed struct {
	TranslationID         uuid.UUID
	AttemptGeneration     int64
	SourceHash            string
	SourceContentRevision *int64
}

// TranslationScheduleCommand describes one atomic River scheduling change.
// Previous is present only when a still-current attempt is being superseded;
// the transactional scheduler must cancel it before inserting Seed.
type TranslationScheduleCommand struct {
	Seed     TranslationAttemptSeed
	Previous *TranslationAttempt
}

// TranslationRequest is the domain input for creating or reusing a
// persistent translation. The initial product surface always targets zh-CN;
// callers therefore cannot choose a target language.
type TranslationRequest struct {
	Scope       TranslationScope
	BlockKey    string
	StartOffset int
	EndOffset   int
	SourceText  string
	// ExpectedContentRevision identifies the saved-content generation the
	// caller observed. Nil is intentionally distinct from zero so requests
	// from clients in the compatibility window remain legacy-unverified.
	ExpectedContentRevision *int64
	// ExpectedSourceHash identifies the summary source block, which does not
	// share the saved-content generation lifecycle. Retired deep-research
	// blocks remain readable as history but cannot be scheduled.
	ExpectedSourceHash *string
	Force              bool
}
