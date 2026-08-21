package dto

import (
	"bytes"
	"encoding/json"
	"time"
)

type ReaderThoughtOpRequest struct {
	ContractVersion int             `json:"contract_version" binding:"required,oneof=1"`
	OpID            string          `json:"op_id" binding:"required,max=128"`
	DeviceID        string          `json:"device_id" binding:"required,max=128"`
	LogicalClock    int64           `json:"logical_clock" binding:"required,min=1"`
	OperationKind   string          `json:"operation_kind" binding:"required,oneof=add update delete"`
	AnnotationID    string          `json:"annotation_id" binding:"required,max=256"`
	HostKind        string          `json:"host_kind" binding:"required,max=32"`
	HostID          string          `json:"host_id" binding:"required,max=256"`
	Target          json.RawMessage `json:"target" binding:"required"`
	Payload         json.RawMessage `json:"payload" binding:"required"`
	// Recovery fields are an atomic pair. A recovery appends a new operation
	// only while expected_current_winner_key remains the materialized winner.
	RecoveryOf               *ReaderThoughtVersionKeyResponse `json:"recovery_of,omitempty"`
	ExpectedCurrentWinnerKey *ReaderThoughtVersionKeyResponse `json:"expected_current_winner_key,omitempty"`
}

type ReaderThoughtOpsRequest struct {
	Ops []ReaderThoughtOpRequest `json:"ops" binding:"required,min=1,max=200,dive"`
}

type ReaderThoughtAckResponse struct {
	ContractVersion  int                             `json:"contract_version"`
	OpID             string                          `json:"op_id"`
	Sequence         int64                           `json:"sequence"`
	Disposition      string                          `json:"disposition"`
	SubmittedKey     ReaderThoughtVersionKeyResponse `json:"submitted_key"`
	CurrentWinnerKey ReaderThoughtVersionKeyResponse `json:"current_winner_key"`
}

type ReaderThoughtVersionKeyResponse struct {
	LogicalClock int64  `json:"logical_clock"`
	DeviceID     string `json:"device_id"`
	OpID         string `json:"op_id"`
}

type ReaderThoughtResponse struct {
	ContractVersion      int                             `json:"contract_version"`
	ID                   string                          `json:"id"`
	HostKind             string                          `json:"host_kind"`
	HostID               string                          `json:"host_id"`
	LinkID               *string                         `json:"link_id,omitempty"`
	Target               json.RawMessage                 `json:"target"`
	Quote                json.RawMessage                 `json:"quote,omitempty"`
	Body                 string                          `json:"body"`
	Source               string                          `json:"source"`
	Deleted              bool                            `json:"deleted"`
	LastSequence         int64                           `json:"last_sequence"`
	WinnerKey            ReaderThoughtVersionKeyResponse `json:"winner_key"`
	CreatedAt            time.Time                       `json:"created_at"`
	UpdatedAt            time.Time                       `json:"updated_at"`
	LifecycleStatus      string                          `json:"lifecycle_status,omitempty"`
	LifecycleReason      *string                         `json:"lifecycle_reason,omitempty"`
	TombstonedAt         *time.Time                      `json:"tombstoned_at,omitempty"`
	OriginalHostSnapshot json.RawMessage                 `json:"original_host_snapshot,omitempty"`
}

type ReaderThoughtsResponse struct {
	ContractVersion int                     `json:"contract_version"`
	Items           []ReaderThoughtResponse `json:"items"`
	NextCursor      string                  `json:"next_cursor,omitempty"`
}

// ReaderThoughtConflictOperationResponse preserves the complete operation
// envelope on one side of a deterministic thought merge conflict. Sequence
// remains an append-log identity, not the materialization winner key.
type ReaderThoughtConflictOperationResponse struct {
	ContractVersion          int                              `json:"contract_version"`
	Sequence                 int64                            `json:"sequence"`
	OpID                     string                           `json:"op_id"`
	DeviceID                 string                           `json:"device_id"`
	LogicalClock             int64                            `json:"logical_clock"`
	OperationKind            string                           `json:"operation_kind"`
	AnnotationID             string                           `json:"annotation_id"`
	HostKind                 string                           `json:"host_kind"`
	HostID                   string                           `json:"host_id"`
	Target                   json.RawMessage                  `json:"target"`
	Payload                  json.RawMessage                  `json:"payload"`
	RecoveryOf               *ReaderThoughtVersionKeyResponse `json:"recovery_of,omitempty"`
	ExpectedCurrentWinnerKey *ReaderThoughtVersionKeyResponse `json:"expected_current_winner_key,omitempty"`
	CreatedAt                time.Time                        `json:"created_at"`
}

type ReaderThoughtConflictResponse struct {
	Sequence          int64                                  `json:"sequence"`
	AnnotationID      string                                 `json:"annotation_id"`
	WinnerAtDetection ReaderThoughtConflictOperationResponse `json:"winner_at_detection"`
	Loser             ReaderThoughtConflictOperationResponse `json:"loser"`
}

type ReaderThoughtConflictsResponse struct {
	ContractVersion int                             `json:"contract_version"`
	Items           []ReaderThoughtConflictResponse `json:"items"`
	NextCursor      string                          `json:"next_cursor,omitempty"`
}

type ReaderNoteCreateRequest struct {
	Title   string `json:"title,omitempty" binding:"omitempty,max=256"`
	Content string `json:"content,omitempty" binding:"max=1048576"`
}

type ReaderNoteDraftRequest struct {
	Content               string `json:"content" binding:"max=1048576"`
	ExpectedDraftRevision int64  `json:"expected_draft_revision" binding:"min=0"`
}

type ReaderNotePublishRequest struct {
	ExpectedDraftRevision     *int64            `json:"expected_draft_revision" binding:"required,min=0"`
	ExpectedPublishedRevision *int64            `json:"expected_published_revision" binding:"required,min=0"`
	ReanchorOps               []json.RawMessage `json:"reanchor_ops,omitempty"`
}

type ReaderNoteRestoreRequest struct {
	ExpectedDraftRevision     *int64            `json:"expected_draft_revision" binding:"required,min=0"`
	ExpectedPublishedRevision *int64            `json:"expected_published_revision" binding:"required,min=0"`
	ReanchorOps               []json.RawMessage `json:"reanchor_ops,omitempty"`
}

type ReaderNoteResponse struct {
	ID                string     `json:"id"`
	Title             string     `json:"title"`
	PublishedContent  string     `json:"published_content"`
	PublishedRevision int64      `json:"published_revision"`
	DraftContent      *string    `json:"draft_content,omitempty"`
	DraftRevision     int64      `json:"draft_revision"`
	DraftUpdatedAt    *time.Time `json:"draft_updated_at,omitempty"`
	DeletedAt         *time.Time `json:"deleted_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Dirty             bool       `json:"dirty"`
}

type ReaderNotesResponse struct {
	Items      []ReaderNoteResponse `json:"items"`
	Count      int                  `json:"count"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type ReaderNoteHistoryResponse struct {
	ID          int64           `json:"id"`
	Revision    int64           `json:"revision"`
	Title       string          `json:"title"`
	Content     string          `json:"content"`
	ReanchorOps json.RawMessage `json:"reanchor_ops"`
	CreatedAt   time.Time       `json:"created_at"`
}

type ReaderHostLifecycleResponse struct {
	HostKind string `json:"host_kind"`
	HostID   string `json:"host_id"`
	State    string `json:"state"`
	Changed  bool   `json:"changed"`
}

type ReaderHostPurgeRequest struct {
	OperationID string `json:"operation_id" binding:"required,max=128"`
}

type ReaderTrashItemResponse struct {
	HostKind  string    `json:"host_kind"`
	HostID    string    `json:"host_id"`
	Title     *string   `json:"title,omitempty"`
	URL       *string   `json:"url,omitempty"`
	TrashedAt time.Time `json:"trashed_at"`
}

type ReaderTrashResponse struct {
	Items      []ReaderTrashItemResponse `json:"items"`
	Count      int                       `json:"count"`
	NextCursor string                    `json:"next_cursor,omitempty"`
}

type ReaderInboxCreateRequest struct {
	URL        string   `json:"url" binding:"required,url,max=2048"`
	SourceKind string   `json:"source_kind,omitempty" binding:"omitempty,max=32"`
	Title      *string  `json:"title,omitempty" binding:"omitempty,max=512"`
	Body       string   `json:"body,omitempty" binding:"max=4194304"`
	Note       string   `json:"note,omitempty" binding:"max=1048576"`
	Summary    *string  `json:"summary,omitempty" binding:"omitempty,max=4096"`
	Tags       []string `json:"tags,omitempty" binding:"max=50,dive,max=64"`
}

type ReaderInboxPatchRequest struct {
	Title   *string  `json:"title,omitempty" binding:"omitempty,max=512"`
	Body    *string  `json:"body,omitempty" binding:"omitempty,max=4194304"`
	Note    *string  `json:"note,omitempty" binding:"omitempty,max=1048576"`
	Summary *string  `json:"summary,omitempty" binding:"omitempty,max=4096"`
	Tags    []string `json:"tags,omitempty" binding:"max=50,dive,max=64"`
}

// ReaderInboxBulkRequest is the request envelope for the atomic Inbox batch
// actions. Duplicate IDs are accepted; the service keeps the first occurrence
// and returns outcomes in that de-duplicated input order.
type ReaderInboxBulkRequest struct {
	InboxIDs          []string         `json:"inbox_ids" binding:"required,min=1,max=100,dive,max=128"`
	ExpectedRevisions map[string]int64 `json:"expected_revisions,omitempty" binding:"omitempty,max=100"`
}

// ReaderInboxBulkItemResponse is one successful outcome from an Inbox batch
// action. A failed batch has no item response: the service transaction is
// atomic and the handler returns the mapped error instead.
type ReaderInboxBulkItemResponse struct {
	InboxID string  `json:"inbox_id"`
	Status  string  `json:"status"`
	LinkID  *string `json:"link_id,omitempty"`
}

// ReaderInboxBulkResponse makes the all-or-nothing behavior explicit on the
// wire. Items are ordered by the first occurrence of each requested ID.
type ReaderInboxBulkResponse struct {
	Atomic bool                          `json:"atomic"`
	Items  []ReaderInboxBulkItemResponse `json:"items"`
}

// ReaderInboxConfirmAIProposalsRequest deliberately has no client-supplied
// IDs. The server selects the stable, AI-ready batch for the requested
// partition so Reader and Feed cannot diverge on backlog filtering.
type ReaderInboxConfirmAIProposalsRequest struct {
	Partition string `json:"partition" binding:"required,oneof=active expired"`
}

// ReaderInboxConfirmAIProposalsResponse describes exactly one atomic batch.
// Clients call the same action again while RemainingCount is non-zero.
type ReaderInboxConfirmAIProposalsResponse struct {
	Atomic         bool                          `json:"atomic"`
	Items          []ReaderInboxBulkItemResponse `json:"items"`
	RemainingCount int                           `json:"remaining_count"`
}

type ReaderInboxResponse struct {
	ID               string     `json:"id"`
	URL              string     `json:"url"`
	SourceKind       string     `json:"source_kind"`
	Title            *string    `json:"title,omitempty"`
	Body             string     `json:"body"`
	Note             string     `json:"note"`
	Summary          *string    `json:"summary,omitempty"`
	SuggestedTags    []string   `json:"suggested_tags"`
	ProposalStatus   string     `json:"proposal_status"`
	Tags             []string   `json:"tags"`
	Status           string     `json:"status"`
	MetadataRevision int64      `json:"metadata_revision"`
	ExpiresAt        *time.Time `json:"expires_at"`
	Expired          bool       `json:"expired"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ReaderInboxListItemResponse is the queue card contract. It deliberately has
// no Body, Note or SuggestedTags field: a
// capture accepts a 4 MiB body and a 1 MiB note, and the list is read every
// time the Inbox opens. Clients that need those read GET /api/inbox/{id},
// whose contract is unchanged.
//
// Preview is the bounded card text the server already resolved from summary,
// then note, then body. It is a display string, not a truncated body — clients
// must not treat it as the beginning of the capture content.
type ReaderInboxListItemResponse struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	SourceKind string   `json:"source_kind"`
	Title      *string  `json:"title,omitempty"`
	Preview    string   `json:"preview"`
	Tags       []string `json:"tags"`
	Status     string   `json:"status"`
	// MetadataRevision is what the batch confirm/discard actions send back as
	// the expected revision, so the list must carry it.
	MetadataRevision int64 `json:"metadata_revision"`
	// Expired mirrors the detail contract and is computed by PostgreSQL from
	// ExpiresAt, never by the client clock.
	Expired bool `json:"expired"`
	// UpdatedAt is both the card timestamp and the keyset cursor field.
	UpdatedAt time.Time `json:"updated_at"`
}

type ReaderInboxResponsePage struct {
	Items        []ReaderInboxListItemResponse `json:"items"`
	NextCursor   string                        `json:"next_cursor,omitempty"`
	ActiveCount  int                           `json:"active_count"`
	ExpiredCount int                           `json:"expired_count"`
}

type ReaderTodoCreateRequest struct {
	Text  string     `json:"text" binding:"required,max=4096"`
	DueAt *time.Time `json:"due_at,omitempty"`
}

type ReaderTodoPatchRequest struct {
	Text                 *string    `json:"text,omitempty" binding:"omitempty,max=4096"`
	DueAt                *time.Time `json:"due_at,omitempty"`
	DueAtSet             bool       `json:"-"`
	Done                 *bool      `json:"done,omitempty"`
	ExpectedHostRevision *int64     `json:"expected_host_revision,omitempty"`
	// ExpectedHostRevisionSet distinguishes an omitted field from an explicit
	// JSON null. All JSON integer values and null must reach host/origin policy:
	// standalone TODOs reject explicit input, while projections treat a
	// nonmatching value as a revision conflict.
	ExpectedHostRevisionSet bool `json:"-"`
}

// UnmarshalJSON keeps the wire-level distinction between an omitted due_at
// and an explicit null. A nil *time.Time alone cannot represent both states.
func (r *ReaderTodoPatchRequest) UnmarshalJSON(data []byte) error {
	type rawReaderTodoPatchRequest struct {
		Text                 *string         `json:"text"`
		DueAt                json.RawMessage `json:"due_at"`
		Done                 *bool           `json:"done"`
		ExpectedHostRevision json.RawMessage `json:"expected_host_revision"`
	}
	var raw rawReaderTodoPatchRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ReaderTodoPatchRequest{
		Text: raw.Text,
		Done: raw.Done,
	}
	if raw.DueAt != nil {
		r.DueAtSet = true
		if !bytes.Equal(bytes.TrimSpace(raw.DueAt), []byte("null")) {
			if err := json.Unmarshal(raw.DueAt, &r.DueAt); err != nil {
				return err
			}
		}
	}
	if raw.ExpectedHostRevision != nil {
		r.ExpectedHostRevisionSet = true
		if !bytes.Equal(bytes.TrimSpace(raw.ExpectedHostRevision), []byte("null")) {
			if err := json.Unmarshal(raw.ExpectedHostRevision, &r.ExpectedHostRevision); err != nil {
				return err
			}
		}
	}
	return nil
}

type ReaderTodoResponse struct {
	ID             string          `json:"id"`
	Text           string          `json:"text"`
	DueAt          *time.Time      `json:"due_at,omitempty"`
	Done           bool            `json:"done"`
	OriginKind     string          `json:"origin_kind"`
	OriginHostKind *string         `json:"origin_host_kind,omitempty"`
	OriginHostID   *string         `json:"origin_host_id,omitempty"`
	OriginRef      json.RawMessage `json:"origin_ref,omitempty"`
	HostRevision   int64           `json:"host_revision"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	Expired        bool            `json:"expired"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	SourceHref     *string         `json:"source_href,omitempty"`
}

type ReaderTodosResponse struct {
	Items     []ReaderTodoResponse `json:"items"`
	NextAfter string               `json:"next_after,omitempty"`
}

type ReaderEngagementRequest struct {
	Read      *bool    `json:"read,omitempty"`
	Progress  *float32 `json:"progress,omitempty" binding:"omitempty,min=0,max=1"`
	ReadLater *bool    `json:"read_later,omitempty"`
}

type ReaderEngagementResponse struct {
	LinkID     string     `json:"link_id"`
	Read       bool       `json:"read"`
	Progress   float32    `json:"progress"`
	ReadLater  bool       `json:"read_later"`
	LastOpened *time.Time `json:"last_opened,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type ReaderFeedItemResponse struct {
	Key         string    `json:"key"`
	Source      string    `json:"source"`
	ResourceKey string    `json:"resource_key"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	URL         string    `json:"url"`
	LinkID      *string   `json:"link_id,omitempty"`
	InboxID     *string   `json:"inbox_id,omitempty"`
	FeedItemID  *string   `json:"feed_item_id,omitempty"`
	Read        bool      `json:"read"`
	ReadLater   bool      `json:"read_later"`
	Saved       bool      `json:"saved"`
	EventAt     time.Time `json:"event_at"`
}

type ReaderFeedResponse struct {
	Items      []ReaderFeedItemResponse `json:"items"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Mode       string                   `json:"mode"`
}

type ReaderFeedFeedbackRequest struct {
	Action string `json:"action" binding:"required,oneof=hide save unsave"`
}

type ReaderFeedFeedbackResponse struct {
	ItemKey string  `json:"item_key"`
	Action  string  `json:"action"`
	LinkID  *string `json:"link_id,omitempty"`
}

type ReaderHomeResponse struct {
	Today           string                   `json:"today"`
	Summary         string                   `json:"summary"`
	Counts          map[string]int           `json:"counts"`
	ContinueReading []ReaderFeedItemResponse `json:"continue_reading"`
	RecentThoughts  []ReaderThoughtResponse  `json:"recent_thoughts"`
	Todos           []ReaderTodoResponse     `json:"todos"`
	// Freshness is the explicit aggregate state. It is additive to Stale so
	// older Reader clients can continue using their boolean compatibility flag.
	Freshness string `json:"freshness"`
	// Partial distinguishes an incomplete/degraded aggregate from a complete
	// aggregate whose data is merely older than the preferred freshness window.
	Partial bool `json:"partial"`
	Stale   bool `json:"stale"`
}

const (
	ReaderHomeFreshnessFresh   = "fresh"
	ReaderHomeFreshnessPartial = "partial"
	ReaderHomeFreshnessStale   = "stale"
)

type ReaderAIRequest struct {
	Prompt       string `json:"prompt" binding:"required,max=16000"`
	Scope        string `json:"scope,omitempty" binding:"omitempty,oneof=general selection thought"`
	LinkID       string `json:"link_id,omitempty" binding:"omitempty,max=64"`
	SelectedText string `json:"selected_text,omitempty" binding:"omitempty,max=16000"`
}

type ReaderAIResponse struct {
	Enabled bool   `json:"enabled"`
	Answer  string `json:"answer"`
	Model   string `json:"model,omitempty"`
}

type ReaderRelatedTagsResponse struct {
	Items []string `json:"items"`
}

type ReaderActivityResponse struct {
	Kind       string                         `json:"kind"`
	Tags       []ReaderTagActivityResponse    `json:"tags"`
	Domains    []ReaderDomainActivityResponse `json:"domains"`
	NextCursor string                         `json:"next_cursor,omitempty"`
}

type ReaderTagActivityResponse struct {
	Tag    string    `json:"tag"`
	LastAt time.Time `json:"last_at"`
}

type ReaderDomainActivityResponse struct {
	Domain string    `json:"domain"`
	LastAt time.Time `json:"last_at"`
}

type ReaderLinkMetadataRequest struct {
	Title   *string  `json:"title"`
	Summary *string  `json:"summary"`
	Tags    []string `json:"tags"`

	// JSON null is a valid explicit replacement for Title and Summary, while
	// omitting either field is not. Keep presence separate from the nullable
	// value so the service can enforce the complete-replacement contract.
	titlePresent   bool
	summaryPresent bool
	tagsPresent    bool
}

// Complete reports whether this request explicitly supplied every member of
// the metadata replacement tuple. It deliberately does not validate values;
// the service owns semantic validation so direct callers and HTTP callers have
// one outcome.
func (r ReaderLinkMetadataRequest) Complete() bool {
	return r.titlePresent && r.summaryPresent && r.tagsPresent
}

// UnmarshalJSON preserves field presence for the metadata CAS command. The
// ordinary pointer/slice representation cannot distinguish an omitted field
// from JSON null, which would turn a partial PATCH into an unintended write.
func (r *ReaderLinkMetadataRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	*r = ReaderLinkMetadataRequest{}
	var ok bool
	var raw json.RawMessage
	if raw, ok = fields["title"]; ok {
		r.titlePresent = true
		if err := json.Unmarshal(raw, &r.Title); err != nil {
			return err
		}
	}
	if raw, ok = fields["summary"]; ok {
		r.summaryPresent = true
		if err := json.Unmarshal(raw, &r.Summary); err != nil {
			return err
		}
	}
	if raw, ok = fields["tags"]; ok {
		r.tagsPresent = true
		if err := json.Unmarshal(raw, &r.Tags); err != nil {
			return err
		}
	}
	return nil
}

type ReaderLinkMetadataResponse struct {
	LinkID           string `json:"link_id"`
	MetadataRevision int64  `json:"metadata_revision"`
}

type ReaderCapabilitiesResponse struct {
	Annotations bool `json:"annotations"`
	Notes       bool `json:"notes"`
	Inbox       bool `json:"inbox"`
	Todos       bool `json:"todos"`
	Engagement  bool `json:"engagement"`
	Home        bool `json:"home"`
	Feed        bool `json:"feed"`
	AI          bool `json:"ai"`
	RelatedTags bool `json:"related_tags"`
	Activity    bool `json:"activity"`
	History     bool `json:"history"`
	Trash       bool `json:"trash"`
}
