package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type LibraryReviewKind string

const (
	LibraryReviewKindClassificationUncertain LibraryReviewKind = "classification_uncertain"
	LibraryReviewKindMigrationSuggestion     LibraryReviewKind = "migration_suggestion"
	LibraryReviewKindNoteConflict            LibraryReviewKind = "note_conflict"
	LibraryReviewKindMergeConflict           LibraryReviewKind = "merge_conflict"
)

type LibraryReviewStatus string

const (
	LibraryReviewStatusPending   LibraryReviewStatus = "pending"
	LibraryReviewStatusApplied   LibraryReviewStatus = "applied"
	LibraryReviewStatusDismissed LibraryReviewStatus = "dismissed"
)

// LibraryReviewItem contains only structured candidates and user-provided
// conflict text. Captured page payloads must never be placed in Payload.
type LibraryReviewItem struct {
	ID         uuid.UUID
	Kind       LibraryReviewKind
	LinkID     *uuid.UUID
	SiteID     *uuid.UUID
	Payload    json.RawMessage
	Status     LibraryReviewStatus
	Revision   int64
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// LibraryClassificationRule is an installation-wide, revision-guarded
// preference.
// A shared-platform adapter always requires a narrow PathPrefix; service
// validation enforces that invariant before persistence.
type LibraryClassificationRule struct {
	ID              uuid.UUID
	Host            string
	IdentityAdapter *string
	PathPrefix      *string
	TargetKind      LibraryKind
	Enabled         bool
	Revision        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
