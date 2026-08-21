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

// TranslationJobKind is the current durable River wire protocol. The stored
// kind keeps its v2 suffix because changing a River kind would itself require
// another runtime branch.
const TranslationJobKind = "translate_link_v2"

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
	// belongs to. Nil is reserved for current summary identities.
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
// Items so an empty list still has authoritative identities for both domains.
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
	// caller observed. It is required for saved-content translations.
	ExpectedContentRevision *int64
	// ExpectedSourceHash identifies the summary source block, which does not
	// share the saved-content generation lifecycle.
	ExpectedSourceHash *string
	Force              bool
}
