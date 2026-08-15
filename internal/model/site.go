package model

import (
	"time"

	"github.com/google/uuid"
)

// FieldSource tracks whether a mutable profile field came from analysis, a
// user action, or a reviewed historical migration.
type FieldSource string

const (
	FieldSourceAuto      FieldSource = "auto"
	FieldSourceUser      FieldSource = "user"
	FieldSourceMigration FieldSource = "migration"
)

// Site is the aggregation root for one website. URLs remain SiteEntry rows so
// a user can preserve several useful paths without duplicating a card.
type Site struct {
	ID               uuid.UUID
	SiteKey          string
	Name             string
	NameSource       FieldSource
	Intro            string
	IntroSource      FieldSource
	HomepageURL      *string
	HomepageSource   *FieldSource
	IconURL          *string
	IconSource       *FieldSource
	UserNote         string
	Pinned           bool
	PrimaryEntryID   *uuid.UUID
	PrimarySource    FieldSource
	GroupingLocked   bool
	NeedsReview      bool
	Revision         int64
	FirstCollectedAt time.Time
	LastCollectedAt  time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SiteEntry is a single collected URL assigned to a Site.
type SiteEntry struct {
	ID                uuid.UUID
	SiteID            uuid.UUID
	LinkID            uuid.UUID
	EntryName         string
	EntryNameSource   FieldSource
	Purpose           string
	PurposeSource     FieldSource
	NormalizedURL     string
	FirstCollectedAt  time.Time
	LastRecollectedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type SiteTag struct {
	SiteID        uuid.UUID
	Tag           string
	NormalizedTag string
	Source        FieldSource
	ConceptID     *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type SiteIdentity struct {
	IdentityKey string
	SiteID      uuid.UUID
	Source      string
	Locked      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
