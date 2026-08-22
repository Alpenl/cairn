package model

import (
	"time"

	"github.com/google/uuid"
)

// Site is the aggregation root for one website. URLs remain SiteEntry rows so
// a user can preserve several useful paths without duplicating a card.
type Site struct {
	ID               uuid.UUID
	SiteKey          string
	Name             string
	Intro            string
	HomepageURL      *string
	IconURL          *string
	UserNote         string
	Pinned           bool
	PrimaryEntryID   *uuid.UUID
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
	Purpose           string
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
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type SiteIdentity struct {
	IdentityKey string
	SiteID      uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
