package model

import (
	"time"

	"github.com/google/uuid"
)

// FeedFolder is an installation-owned, single-level grouping for subscriptions.
type FeedFolder struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// FeedSubscription is the public subscription view plus the private refresh
// lease fields needed by the scheduler. URL and FeedURL intentionally carry
// the same value so older extension clients and the Reader can coexist.
type FeedSubscription struct {
	ID            uuid.UUID  `json:"id"`
	URL           string     `json:"url"`
	FeedURL       string     `json:"feed_url"`
	SiteURL       *string    `json:"site_url,omitempty"`
	Title         string     `json:"title"`
	Description   *string    `json:"description,omitempty"`
	FolderID      *uuid.UUID `json:"folder_id,omitempty"`
	UnreadCount   int        `json:"unread_count"`
	ItemCount     int        `json:"item_count"`
	Active        bool       `json:"active"`
	LastFetchedAt *time.Time `json:"last_fetched_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	NextFetchAt   time.Time  `json:"next_fetch_at"`
	FetchError    *string    `json:"fetch_error,omitempty"`
	LastError     *string    `json:"last_error,omitempty"`
	FailureCount  int        `json:"failure_count"`
	Refreshing    bool       `json:"refreshing"`
	CreatedAt     time.Time  `json:"created_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at,omitempty"`

	ETag              string     `json:"-"`
	LastModified      string     `json:"-"`
	SyncBoundaryID    string     `json:"-"`
	RefreshClaimToken *uuid.UUID `json:"-"`
	RefreshClaimUntil *time.Time `json:"-"`
}

// FeedCounts backs the four smart views in the subscription sidebar.
type FeedCounts struct {
	All     int `json:"all"`
	Unread  int `json:"unread"`
	Starred int `json:"starred"`
	Later   int `json:"later"`
}

type FeedSubscriptionsResponse struct {
	Folders       []FeedFolder       `json:"folders"`
	Subscriptions []FeedSubscription `json:"subscriptions"`
	Counts        FeedCounts         `json:"counts"`
}

// FeedItem stores sanitized feed content independently from links. LinkID is
// populated only after an explicit AI analysis request.
type FeedItem struct {
	ID                uuid.UUID  `json:"id"`
	SubscriptionID    uuid.UUID  `json:"subscription_id"`
	SubscriptionTitle string     `json:"subscription_title"`
	Title             string     `json:"title"`
	URL               string     `json:"url"`
	Author            *string    `json:"author,omitempty"`
	Summary           *string    `json:"summary,omitempty"`
	Content           *string    `json:"content,omitempty"`
	ContentHTML       *string    `json:"content_html,omitempty"`
	PublishedAt       *time.Time `json:"published_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	Read              bool       `json:"read"`
	ReadAt            *time.Time `json:"read_at,omitempty"`
	Starred           bool       `json:"starred"`
	ReadLater         bool       `json:"read_later"`
	LinkID            *uuid.UUID `json:"link_id,omitempty"`
	AnalysisStatus    *string    `json:"analysis_status,omitempty"`

	ExternalID string `json:"-"`
}

type PaginatedFeedItems struct {
	Items []FeedItem `json:"items"`
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Limit int        `json:"limit"`
}

type FeedCandidate struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

type FeedDiscoveryResponse struct {
	Feeds []FeedCandidate `json:"feeds"`
}

type OPMLImportResponse struct {
	Imported int      `json:"imported"`
	Folders  int      `json:"folders"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// ParsedFeed is the normalized output of RSS 2.0, Atom, or RSS 1.0 parsing.
type ParsedFeed struct {
	Title       string
	Description string
	SiteURL     string
	FeedType    string
	Items       []FeedItem
}
