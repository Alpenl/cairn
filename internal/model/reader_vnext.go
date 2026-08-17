package model

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// ReaderThoughtContractVersion identifies the wire/archive contract that
	// carries a complete Lamport winner key.
	ReaderThoughtContractVersion = 1
	// ReaderThoughtMaxLogicalClock is the largest integer JavaScript can round
	// trip without losing precision.
	ReaderThoughtMaxLogicalClock int64 = 9_007_199_254_740_991
	// LinkMetadataMaxRevision is the largest metadata CAS revision JavaScript
	// can round trip without losing precision.
	LinkMetadataMaxRevision int64 = ReaderThoughtMaxLogicalClock
)

// ReaderThoughtOp is the durable, replayable unit used by the Reader's
// local-first annotation store. The server stores the payload without
// interpreting anchor offsets. Sequence is an append/replay cursor only;
// LogicalClock, DeviceID, and OpID form the deterministic materialization key.
type ReaderThoughtOp struct {
	Sequence      int64
	OpID          string
	DeviceID      string
	LogicalClock  int64
	OperationKind string
	AnnotationID  string
	HostKind      string
	HostID        string
	Target        json.RawMessage
	Payload       json.RawMessage
	// RecoveryOf and ExpectedWinnerKey are present only for an explicit
	// supersession recovery. They make the recovery's provenance durable and
	// turn it into a compare-and-swap against the live winner.
	RecoveryOf        *ReaderThoughtVersionKey
	ExpectedWinnerKey *ReaderThoughtVersionKey
	// Reattach marks a client-owned update that must be rebuilt from the
	// immutable lifecycle snapshot before it enters the operation log. It is
	// command metadata only; the persisted payload contains the marker plus
	// the server-owned frozen content used for materialization.
	Reattach  *ReaderThoughtReattachOperation
	CreatedAt time.Time
}

// ReaderThoughtVersionKey is the total-order key for one operation on one
// annotation. A larger logical clock wins; equal clocks use the stable device
// and operation identifiers as tie-breakers. The operation kind is deliberately
// not part of the key, so add/update/delete all obey the same replay rule.
type ReaderThoughtVersionKey struct {
	LogicalClock int64
	DeviceID     string
	OpID         string
}

func (key ReaderThoughtVersionKey) Compare(other ReaderThoughtVersionKey) int {
	if key.LogicalClock < other.LogicalClock {
		return -1
	}
	if key.LogicalClock > other.LogicalClock {
		return 1
	}
	if compared := bytes.Compare([]byte(key.DeviceID), []byte(other.DeviceID)); compared != 0 {
		return compared
	}
	return bytes.Compare([]byte(key.OpID), []byte(other.OpID))
}

// ReaderThoughtConflictOperation is one side of a durable thought merge
// conflict. Sequence identifies the append-log row; it is not the winner key.
type ReaderThoughtConflictOperation struct {
	Sequence          int64
	OpID              string
	DeviceID          string
	LogicalClock      int64
	OperationKind     string
	AnnotationID      string
	HostKind          string
	HostID            string
	Target            json.RawMessage
	Payload           json.RawMessage
	RecoveryOf        *ReaderThoughtVersionKey
	ExpectedWinnerKey *ReaderThoughtVersionKey
	CreatedAt         time.Time
}

// ReaderThoughtConflict is derived from the durable operation log. Keeping
// both operations readable lets a client or user decide how to recover a
// losing edit without changing the existing push ack or replay cursor.
type ReaderThoughtConflict struct {
	// Sequence is the immutable supersession event sequence. It must never be
	// inferred from the losing operation's append sequence: an old winner can
	// become a loser after a client has already moved beyond that cursor.
	Sequence     int64
	AnnotationID string
	Winner       ReaderThoughtConflictOperation
	Loser        ReaderThoughtConflictOperation
}

type ReaderThoughtAck struct {
	OpID         string
	Sequence     int64
	Disposition  string
	SubmittedKey ReaderThoughtVersionKey
	WinnerKey    ReaderThoughtVersionKey
}

type ReaderThought struct {
	ID              string
	HostKind        string
	HostID          string
	LinkID          *uuid.UUID
	Target          json.RawMessage
	Quote           json.RawMessage
	Body            string
	Source          string
	Deleted         bool
	LastSequence    int64
	WinnerKey       ReaderThoughtVersionKey
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LifecycleStatus string
	LifecycleReason *string
	TombstonedAt    *time.Time
	// OriginalHostSnapshot is immutable context captured with a lifecycle
	// tombstone. It is intentionally absent from live thoughts.
	OriginalHostSnapshot json.RawMessage
}

type ReaderThoughtReattachCommand struct {
	ThoughtID            string
	TargetHostKind       string
	TargetHostID         string
	ExpectedLastSequence int64
	ExpectedHostRevision int64
}

// ReaderThoughtReattachOperation carries the compare-and-swap expectations
// for a local-first reattach update. OpID, DeviceID, and LogicalClock remain
// on ReaderThoughtOp so retries keep one durable Lamport identity.
type ReaderThoughtReattachOperation struct {
	ExpectedLastSequence int64
	ExpectedHostRevision int64
}

// ReaderThoughtSearch is the deliberately narrow published search projection.
// It keeps full annotation payloads out of the grouped library-search path.
type ReaderThoughtSearch struct {
	ID              string
	HostKind        string
	HostID          string
	LinkID          *uuid.UUID
	Snippet         string
	UpdatedAt       time.Time
	LifecycleStatus string
	LifecycleReason *string
	HistoryDeepLink string
}

type ReaderNote struct {
	ID                uuid.UUID
	Title             string
	PublishedContent  string
	PublishedRevision int64
	DraftContent      *string
	DraftRevision     int64
	DraftUpdatedAt    *time.Time
	DeletedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ReaderNoteSearch contains only fields that are safe for search results.
// In particular, draft_content is intentionally absent from this projection.
type ReaderNoteSearch struct {
	ID                uuid.UUID
	Title             string
	Snippet           string
	PublishedRevision int64
	UpdatedAt         time.Time
}

type ReaderNoteHistory struct {
	ID          int64
	NoteID      uuid.UUID
	Revision    int64
	Title       string
	Content     string
	ReanchorOps json.RawMessage
	CreatedAt   time.Time
}

type ReaderHostKind string

const (
	ReaderHostLink  ReaderHostKind = "link"
	ReaderHostInbox ReaderHostKind = "inbox"
	ReaderHostNote  ReaderHostKind = "note"
)

func (kind ReaderHostKind) Valid() bool {
	switch kind {
	case ReaderHostLink, ReaderHostInbox, ReaderHostNote:
		return true
	default:
		return false
	}
}

type ReaderHostState string

const (
	ReaderHostLive    ReaderHostState = "live"
	ReaderHostTrashed ReaderHostState = "trashed"
)

type ReaderHostLifecycleResult struct {
	HostKind ReaderHostKind
	HostID   uuid.UUID
	State    ReaderHostState
	Changed  bool
}

// ReaderTrashItem is a deliberately narrow projection. Host content remains
// in its owning table while the trash surface exposes only enough metadata to
// identify and restore or purge an item.
type ReaderTrashItem struct {
	HostKind  ReaderHostKind
	HostID    uuid.UUID
	Title     *string
	URL       *string
	TrashedAt time.Time
}

type ReaderInbox struct {
	ID               uuid.UUID
	URL              string
	IdentityKey      string
	SourceKind       string
	Title            *string
	Body             string
	Note             string
	Summary          *string
	SuggestedTags    []string
	ProposalSignals  json.RawMessage
	ProposalStatus   string
	Tags             []string
	CategoryIDs      []uuid.UUID
	Status           string
	MetadataRevision int64
	JobID            *uuid.UUID
	// ExpiresAt is the authoritative deadline for the pending Inbox item. A
	// nil value means the item has no expiry policy; it is never inferred from
	// created_at or updated_at.
	ExpiresAt *time.Time
	// ExpiredAt records that the expiry worker observed ExpiresAt and moved the
	// item into the expired partition. It does not replace ExpiresAt as the
	// expiry predicate.
	ExpiredAt *time.Time
	DeletedAt *time.Time
	Expired   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReaderInboxListItem is the queue projection of an Inbox capture. It carries
// only what a queue card renders, what the batch actions address, the revision
// the CAS writes compare, and the keyset cursor fields — never the capture
// body, the user note, the raw AI proposal payload, or the detail-only
// category memberships. A capture may hold a 4 MiB body and a 1 MiB note; the
// list is read on every Inbox open, so those belong to GET /api/inbox/{id}
// alone. Preview is the already-bounded card text, not a prefix of the body
// the client may expand.
type ReaderInboxListItem struct {
	ID               uuid.UUID
	URL              string
	SourceKind       string
	Title            *string
	Preview          string
	Tags             []string
	Status           string
	MetadataRevision int64
	Expired          bool
	UpdatedAt        time.Time
}

// ReaderInboxPartition is the stable, server-owned split of pending Inbox
// items. Expiry never changes the pending lifecycle: it only moves an item
// from Active to Expired after the maintenance worker records ExpiredAt.
type ReaderInboxPartition string

const (
	ReaderInboxPartitionActive  ReaderInboxPartition = "active"
	ReaderInboxPartitionExpired ReaderInboxPartition = "expired"
)

func (partition ReaderInboxPartition) Valid() bool {
	return partition == ReaderInboxPartitionActive || partition == ReaderInboxPartitionExpired
}

// ReaderInboxBulkResult is the internal result of an idempotent inbox batch
// transition. It is intentionally a model-level result until the HTTP/shared
// wire contract for partial batch failures is finalized.
type ReaderInboxBulkResult struct {
	ID     uuid.UUID
	Status string
	LinkID *uuid.UUID
}

// ReaderInboxBulkConfirmation carries the exact revision a caller reviewed.
// A nil revision preserves the legacy idempotent batch behavior for callers
// that do not edit Inbox drafts.
type ReaderInboxBulkConfirmation struct {
	ID               uuid.UUID
	ExpectedRevision *int64
}

// ReaderInboxAIProposalConfirmation is one server-selected confirmation
// batch. Items is all-or-nothing; RemainingCount is the authoritative number
// of still eligible rows in the requested partition after this transaction.
type ReaderInboxAIProposalConfirmation struct {
	Items          []ReaderInboxBulkResult
	RemainingCount int
}

type ReaderCategory struct {
	ID        uuid.UUID
	Name      string
	Count     int
	CreatedAt time.Time
}

type ReaderTodo struct {
	ID             uuid.UUID
	Text           string
	DueAt          *time.Time
	Done           bool
	OriginKind     string
	OriginHostKind *string
	OriginHostID   *string
	OriginRef      json.RawMessage
	HostRevision   int64
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Expired        bool
}

type ReaderTodoPage struct {
	Items []ReaderTodo
	Next  string
}

type ReaderEngagement struct {
	LinkID     uuid.UUID
	Read       bool
	Progress   float32
	ReadLater  bool
	LastOpened *time.Time
	UpdatedAt  time.Time
}

type ReaderFeedReasonCode string

const (
	ReaderFeedReasonPendingConfirmation   ReaderFeedReasonCode = "pending_confirmation"
	ReaderFeedReasonSavedLibrary          ReaderFeedReasonCode = "saved_library"
	ReaderFeedReasonSubscriptionRecent    ReaderFeedReasonCode = "subscription_recent"
	ReaderFeedReasonUnread                ReaderFeedReasonCode = "unread"
	ReaderFeedReasonReadLater             ReaderFeedReasonCode = "read_later"
	ReaderFeedReasonChronologicalFallback ReaderFeedReasonCode = "chronological_fallback"
)

// ReaderFeedReasonParams carries only evidence consumed by the scoring pass.
// Exactly one field is present for each reason code.
type ReaderFeedReasonParams struct {
	Source    *string    `json:"source,omitempty"`
	Read      *bool      `json:"read,omitempty"`
	ReadLater *bool      `json:"read_later,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// ReaderFeedScoreContributions is deliberately fixed-width so snapshots and
// clients can audit every ranking signal without relying on map iteration.
type ReaderFeedScoreContributions struct {
	PendingConfirmation   int `json:"pending_confirmation"`
	SavedLibrary          int `json:"saved_library"`
	SubscriptionRecent    int `json:"subscription_recent"`
	Unread                int `json:"unread"`
	ReadLater             int `json:"read_later"`
	ChronologicalFallback int `json:"chronological_fallback"`
}

type ReaderFeedItem struct {
	Key    string
	Source string
	// ResourceKey identifies the durable resource represented by the card.
	// ActionKey identifies the command target within this installation. They are kept
	// separate because a subscription item can act on its feed-item identity
	// while pointing at an already-saved link resource.
	ResourceKey         string
	ActionKey           string
	DedupeKey           string
	SectionID           string
	Actions             []string
	Title               string
	Summary             string
	URL                 string
	LinkID              *uuid.UUID
	InboxID             *uuid.UUID
	FeedItemID          *uuid.UUID
	Read                bool
	ReadLater           bool
	Saved               bool
	Score               int
	ScoreContributions  ReaderFeedScoreContributions
	EnabledScoreSignals []ReaderFeedReasonCode
	ReasonCode          ReaderFeedReasonCode
	ReasonParams        ReaderFeedReasonParams
	ReasonContribution  int
	ReasonText          string
	PublishedAt         *time.Time
	CreatedAt           time.Time
}

// ReaderFeedSaveAssociation is the stable installation-local identity of a
// subscription item's saved-reading association. It intentionally carries the
// creator bit because that bit governs the last-association trash transition.
type ReaderFeedSaveAssociation struct {
	FeedItemID  uuid.UUID
	LinkID      uuid.UUID
	CreatedLink bool
}

type ReaderFeedFeedback struct {
	ItemKey     string
	Action      string
	Saved       bool
	Association *ReaderFeedSaveAssociation
}

// VisibleEventAt is the single timestamp used for Feed chronology and card
// display. A published instant wins when present; every other item falls back
// to its creation instant.
func (item ReaderFeedItem) VisibleEventAt() time.Time {
	if item.PublishedAt != nil {
		return *item.PublishedAt
	}
	return item.CreatedAt
}

type ReaderFeedSection struct {
	ID           string
	Source       string
	Label        string
	Count        int
	Capabilities []string
}

type ReaderFeedSource struct {
	ID           string
	Label        string
	Enabled      bool
	Count        int
	Capabilities []string
}

type ReaderFeedPage struct {
	Items        []ReaderFeedItem
	NextCursor   string
	SnapshotID   string
	Mode         string
	Capabilities []string
	Sections     []ReaderFeedSection
	Sources      []ReaderFeedSource
}

// ResourceIdentity returns the stable resource identity while retaining the
// old Key-based representation for snapshots created before WP-24.
func (item ReaderFeedItem) ResourceIdentity() string {
	if strings.TrimSpace(item.ResourceKey) != "" {
		return strings.TrimSpace(item.ResourceKey)
	}
	if item.LinkID != nil {
		return "link:" + item.LinkID.String()
	}
	if item.InboxID != nil {
		return "inbox:" + item.InboxID.String()
	}
	if item.FeedItemID != nil {
		return "feed_item:" + item.FeedItemID.String()
	}
	return strings.TrimSpace(item.Key)
}

// ActionIdentity is the canonical installation-local key accepted by Feed action
// endpoints. Key remains its legacy wire alias.
func (item ReaderFeedItem) ActionIdentity() string {
	if strings.TrimSpace(item.ActionKey) != "" {
		return strings.TrimSpace(item.ActionKey)
	}
	if strings.TrimSpace(item.Key) != "" {
		return strings.TrimSpace(item.Key)
	}
	return item.ResourceIdentity()
}

// DedupeIdentity keeps URL-level dedupe explicit without replacing resource
// identity. The repository uses the same value before snapshot creation.
func (item ReaderFeedItem) DedupeIdentity() string {
	if strings.TrimSpace(item.DedupeKey) != "" {
		return strings.TrimSpace(item.DedupeKey)
	}
	if url := strings.TrimSpace(item.URL); url != "" {
		return "url:" + url
	}
	return item.ResourceIdentity()
}

func (item ReaderFeedItem) SectionIdentity() string {
	if strings.TrimSpace(item.SectionID) != "" {
		return strings.TrimSpace(item.SectionID)
	}
	return strings.TrimSpace(item.Source)
}

// ActionCapabilities is deliberately a closed, source-derived list. A
// non-nil empty Actions slice is an explicit capability-off result and must
// not be replaced by inferred defaults.
func (item ReaderFeedItem) ActionCapabilities() []string {
	if item.Actions != nil {
		cloned := make([]string, len(item.Actions))
		copy(cloned, item.Actions)
		return cloned
	}
	var actions []string
	switch item.Source {
	case "inbox", "pending":
		actions = []string{"confirm", "discard", "hide", "not_interested", "open"}
	case "reading", "saved":
		actions = []string{"read", "read_later", "hide", "not_interested", "open"}
	case "subscription", "feed":
		actions = []string{"read", "read_later", "hide", "not_interested", "open"}
		if item.Saved {
			actions = append(actions, "unsave")
		} else {
			actions = append(actions, "save")
		}
	default:
		actions = []string{}
	}
	if item.LinkID != nil {
		actions = append(actions, "open_workspace")
	}
	return actions
}

const (
	ReaderActivityKindAll    = "all"
	ReaderActivityKindTag    = "tag"
	ReaderActivityKindDomain = "domain"
)

// ReaderActivity is one row in the shared tag/domain activity ordering.
// NormalizedKey is the case-folded seek key; Key preserves the display value
// and is the final stabilizer when two display values normalize identically.
type ReaderActivity struct {
	Kind          string
	Key           string
	NormalizedKey string
	LastAt        time.Time
}

type ReaderActivityCursor struct {
	LastAt        time.Time
	Kind          string
	NormalizedKey string
	Key           string
}

type ReaderActivityQuery struct {
	Kind  string
	After *ReaderActivityCursor
	Limit int
}

type ReaderActivityPage struct {
	Items   []ReaderActivity
	HasMore bool
}

type ReaderContentHistory struct {
	ID              int64
	LinkID          uuid.UUID
	Revision        int64
	Content         *string
	ContentDocument *string
	ContentFormat   string
	ContentSource   string
	CreatedAt       time.Time
}

type ReaderInboxJob struct {
	ID                       uuid.UUID
	InboxID                  uuid.UUID
	ExpectedMetadataRevision int64
	Status                   string
	Attempts                 int
	ErrorMessage             *string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	StartedAt                *time.Time
	FinishedAt               *time.Time
}

type ReaderNoteDraftCommand struct {
	NoteID                uuid.UUID
	Content               string
	ExpectedDraftRevision int64
}

// ReaderNoteDiscardDraftCommand clears only the unpublished draft. The
// published revision/content remain unchanged; the draft revision is bumped
// so an in-flight autosave cannot recreate a draft after the user discarded it.
type ReaderNoteDiscardDraftCommand struct {
	NoteID                uuid.UUID
	ExpectedDraftRevision int64
}

type ReaderNotePublishCommand struct {
	NoteID                    uuid.UUID
	ExpectedDraftRevision     int64
	ExpectedPublishedRevision int64
	ReanchorOps               []json.RawMessage
}

type ReaderNoteRestoreCommand struct {
	NoteID                    uuid.UUID
	Revision                  int64
	ExpectedDraftRevision     int64
	ExpectedPublishedRevision int64
	ReanchorOps               []json.RawMessage
}

type ReaderInboxPatch struct {
	ID               uuid.UUID
	Title            *string
	Body             *string
	Note             *string
	Summary          *string
	Tags             []string
	ExpectedRevision int64
}

type ReaderTodoPatch struct {
	ID                      uuid.UUID
	Text                    *string
	DueAt                   *time.Time
	DueAtSet                bool
	Done                    *bool
	ExpectedHostRevision    *int64
	ExpectedHostRevisionSet bool
}

type ReaderEngagementPatch struct {
	LinkID    uuid.UUID
	Read      *bool
	Progress  *float32
	ReadLater *bool
}

// ReaderAIContext is a bounded installation-local context projection for the AI
// proxy. It deliberately contains no draft/private fields outside the link's
// published projection and a small set of live thought snippets.
type ReaderAIContext struct {
	LinkID   uuid.UUID
	Content  string
	Summary  string
	Tags     []string
	Thoughts []ReaderAIThoughtContext
}

type ReaderAIThoughtContext struct {
	Body string
}

type ReaderLinkMetadataPatch struct {
	LinkID           uuid.UUID
	Title            *string
	Summary          *string
	Tags             []string
	ExpectedRevision int64
}

// ReaderLinkMetadataUpdate describes the observable outcome of one complete
// metadata replacement. TagsChanged lets the service invalidate only tag
// projections that the command actually changed.
type ReaderLinkMetadataUpdate struct {
	MetadataRevision int64
	TagsChanged      bool
}
