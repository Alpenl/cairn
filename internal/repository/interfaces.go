package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
)

// ErrNotFound 在 SQL UPDATE/DELETE 没有命中任何行（即目标记录不存在）时由仓储方法返回。
var ErrNotFound = errors.New("repository row not found")

// ErrInvalidReaderHostKind marks a lifecycle request whose polymorphic host
// kind is outside the frozen Link/Inbox/Note set.
var ErrInvalidReaderHostKind = errors.New("invalid reader host kind")

// ErrReaderHostNotTrashed prevents permanent purge from becoming an implicit
// hard delete. Callers must soft-delete the host before purging it.
var ErrReaderHostNotTrashed = errors.New("reader host is not trashed")

// ErrInvalidReaderCursor marks a malformed Reader pagination or replay token.
// Callers must use errors.Is instead of depending on repository-specific
// wording, because the same token contract is shared by notes, inbox,
// thoughts, and feed pages.
var ErrInvalidReaderCursor = errors.New("invalid reader cursor")

// ErrInvalidReaderReanchor marks a malformed opaque client reanchor batch.
// The repository validates the envelope and the host/revision invariants at
// the transaction boundary. Quote placement remains owned by the shared
// client kernel, but a server must never accept an operation for another
// host, note, or revision.
var ErrInvalidReaderReanchor = errors.New("invalid reader reanchor batch")

var ErrReaderNoteContentEmpty = errors.New("reader note content is empty")
var ErrReaderNoteDraftDirty = errors.New("reader note draft is dirty")
var ErrReaderNoteReanchorIncomplete = errors.New("reader note reanchor operations are incomplete")

// ErrInvalidReaderFeedItem marks an action that cannot be applied to the
// resource identity encoded by a mixed-feed item key.
var ErrInvalidReaderFeedItem = errors.New("invalid reader feed item")

// ErrReaderTodoProjectionImmutable marks an attempt to edit source-owned
// TODO fields through the projection surface. Projected items accept only a
// desired checkbox state; text and due dates belong to their host document.
var ErrReaderTodoProjectionImmutable = errors.New("reader todo projection is immutable")

// ErrReaderTodoHostRevisionNotApplicable marks a conditional host-revision
// request sent to a standalone TODO. Standalone TODOs have no host revision,
// so accepting that precondition would silently ignore a caller's CAS.
var ErrReaderTodoHostRevisionNotApplicable = errors.New("reader todo host revision is not applicable")

// ErrReaderTodoHostMissing means the projection still exists but its source
// host is gone or no longer addressable. It is intentionally distinct from a
// revision conflict so the Reader can offer history/manual reattach.
var ErrReaderTodoHostMissing = errors.New("reader todo host is missing")

// ErrReaderTodoAnchorNotFound means the stable block reference no longer
// resolves in the current host source.
var ErrReaderTodoAnchorNotFound = errors.New("reader todo anchor not found")

// ErrReaderTodoAnchorAmbiguous means a stable reference resolves to more than
// one source block and must not be guessed.
var ErrReaderTodoAnchorAmbiguous = errors.New("reader todo anchor is ambiguous")

// ErrReaderInboxProposalNotRunnable means the Inbox no longer accepts output
// from the current River attempt. Workers treat it as an idempotent no-op.
var ErrReaderInboxProposalNotRunnable = errors.New("reader inbox proposal is no longer runnable")

// ErrReaderThoughtOpConflict means an op_id was already accepted with a
// different immutable envelope. Retrying an operation is idempotent only when
// every envelope field has the same JSON meaning.
var ErrReaderThoughtOpConflict = errors.New("reader thought operation envelope conflicts")

// ErrReaderThoughtRecoveryConflict means a recovery candidate observed an old
// winner. The candidate remains client-side and must never overwrite the live
// projection after this compare-and-swap rejection.
var ErrReaderThoughtRecoveryConflict = errors.New("reader thought recovery winner conflicts")

// ErrReaderThoughtClockInvalid marks a clock outside the legacy zero or
// JavaScript-safe positive Lamport range.
var ErrReaderThoughtClockInvalid = errors.New("reader thought logical clock is invalid")

// ErrReaderThoughtClockExhausted means a server-derived writer cannot allocate
// winner_clock+1 without leaving the JavaScript-safe integer range.
var ErrReaderThoughtClockExhausted = errors.New("reader thought logical clock is exhausted")

// ErrReaderThoughtLinkMismatch means a thought's link host and payload link_id
// do not describe the same installation link.
var ErrReaderThoughtLinkMismatch = errors.New("reader thought link does not match host")

// ErrReaderInboxStateConflict means the requested inbox transition is not a
// legal lifecycle transition, such as confirming a trashed item or discarding
// an already confirmed saved link.
var ErrReaderInboxStateConflict = errors.New("reader inbox state transition conflicts")

// ErrReaderInboxTitleRequired prevents every confirmation entry point from
// materializing a library Link without a user-visible title.
var ErrReaderInboxTitleRequired = errors.New("reader inbox title is required")

// ErrInvalidReaderThought marks an invalid server-side thought envelope that
// cannot be materialized safely.
var ErrInvalidReaderThought = errors.New("invalid reader thought")

// ErrReaderThoughtReattachInvalidState means the source thought still exists
// but is not a historical tombstone that can be manually reattached. It is
// deliberately distinct from ErrNotFound and ErrRevisionConflict: clients
// must stop retrying an active thought even if their cached revisions change.
var ErrReaderThoughtReattachInvalidState = errors.New("reader thought reattach is not allowed in its current lifecycle state")

// ErrParseAttemptNotRunnable means the Link no longer accepts output from the
// immutable generation carried by a River job. Workers treat it as a clean
// no-op instead of retrying stale work.
var ErrParseAttemptNotRunnable = errors.New("parse attempt is no longer runnable")

// ErrSiteEntryNotFound distinguishes a missing entry from a revision conflict
// on its owning site. Management services map it to the stable entry 404
// contract without exposing storage-layer details.
var ErrSiteEntryNotFound = errors.New("site entry not found")

// ErrRevisionConflict means a revision-guarded aggregate write observed a
// newer site state. Services deliberately map this to a stable 409 rather
// than treating it as a missing object.
var ErrRevisionConflict = errors.New("revision conflict")

// ErrLibrarySelectionChanged means terminal parse completion observed a newer
// kind/lock pair than the pipeline used to choose its branch. The pipeline must
// reload the Link and recompute the final partition.
var ErrLibrarySelectionChanged = errors.New("library selection changed")

// LinkDetailProjection is the persisted read model exposed by link detail and
// list responses. Capture payloads, worker identity, and maintenance timestamps
// are deliberately absent: adding one of those columns to a point lookup would
// make the projection pay the TOAST/scan cost without a response consumer.
type LinkDetailProjection struct {
	ID                  uuid.UUID           `db:"id"`
	URL                 string              `db:"url"`
	Title               *string             `db:"title"`
	Summary             *string             `db:"summary"`
	Tags                []string            `db:"tags"`
	FetcherType         *string             `db:"fetcher_type"`
	IsLowConfidence     bool                `db:"is_low_confidence"`
	LowConfidenceReason *string             `db:"low_confidence_reason"`
	Status              model.LinkStatus    `db:"status"`
	ErrorMsg            *string             `db:"error_msg"`
	Description         *string             `db:"description"`
	Domain              *string             `db:"domain"`
	ContentType         *string             `db:"content_type"`
	LibraryKind         *model.LibraryKind  `db:"library_kind"`
	ContentRevision     int64               `db:"content_revision"`
	MetadataRevision    int64               `db:"metadata_revision"`
	ContentSource       model.ContentSource `db:"content_source"`
	HasContent          bool                `db:"has_content"`
	ContentCJKChars     int                 `db:"content_cjk_chars"`
	ContentWords        int                 `db:"content_words"`
	PathDepth           *int                `db:"path_depth"`
	ParentPath          *string             `db:"parent_path"`
	ParentID            *uuid.UUID          `db:"parent_id"`
	CreatedAt           time.Time           `db:"created_at"`
	UpdatedAt           time.Time           `db:"updated_at"`
}

// LinkParseInput contains the source material and state needed to run one
// parse, compare an ingest capture, or save the captured original. It excludes
// every presentation/enrichment field produced by parsing.
type LinkParseInput struct {
	ID                uuid.UUID          `db:"id"`
	URL               string             `db:"url"`
	SourceKind        string             `db:"source_kind"`
	SourceKey         string             `db:"source_key"`
	InputTitle        *string            `db:"input_title"`
	InputText         *string            `db:"input_text"`
	InputHTML         *string            `db:"input_html"`
	InputImages       []string           `db:"input_images"`
	SourceMetadata    map[string]any     `db:"source_metadata"`
	Description       *string            `db:"description"`
	Status            model.LinkStatus   `db:"status"`
	LibraryKind       *model.LibraryKind `db:"library_kind"`
	LibraryKindLocked bool               `db:"library_kind_locked"`
	ContentRevision   int64              `db:"content_revision"`
	MetadataRevision  int64              `db:"metadata_revision"`
	ParseGeneration   int64              `db:"parse_generation"`
	UpdatedAt         time.Time          `db:"updated_at"`
}

// LinkLifecycleProjection is the small state/CAS view used by conversion and
// deletion commands.
type LinkLifecycleProjection struct {
	ID                uuid.UUID          `db:"id"`
	URL               string             `db:"url"`
	Status            model.LinkStatus   `db:"status"`
	LibraryKind       *model.LibraryKind `db:"library_kind"`
	LibraryKindLocked bool               `db:"library_kind_locked"`
	ContentRevision   int64              `db:"content_revision"`
	HasContent        bool               `db:"has_content"`
	DeletedAt         *time.Time         `db:"deleted_at"`
}

// LinkSubmitLookup is the identity/state view used by ordinary URL submit and
// refresh. Multimodal ingest deliberately uses LinkParseInput because capture
// equality depends on the original input payload.
type LinkSubmitLookup struct {
	ID                uuid.UUID          `db:"id"`
	URL               string             `db:"url"`
	SourceKey         string             `db:"source_key"`
	Status            model.LinkStatus   `db:"status"`
	LibraryKind       *model.LibraryKind `db:"library_kind"`
	LibraryKindLocked bool               `db:"library_kind_locked"`
	ParseRequestedAt  time.Time          `db:"parse_requested_at"`
}

type LinkLifecycleReader interface {
	GetLifecycleByID(context.Context, uuid.UUID) (*LinkLifecycleProjection, error)
}

// SiteParseCompleter owns the terminal transaction for a parsed site link.
// It is intentionally separate from ParseStateStore because it additionally
// clears reading-only assets and aggregates a SiteEntry before exposing done.
type SiteParseCompleter interface {
	CompleteSiteParse(context.Context, CompleteSiteParseParams) (SiteAggregateResult, error)
}

// SiteIdentityLookup is the tiny read surface used by conversion previews to
// advertise a CAS-ready existing aggregation target without listing sites.
type SiteIdentityLookup interface {
	FindByIdentityKey(context.Context, string) (*SiteConversionCandidate, error)
}

type SiteSearchMatch struct {
	SiteID         uuid.UUID
	SiteName       string
	MatchedEntries []SiteSearchEntry
}

type SiteSearchEntry struct {
	ID   uuid.UUID
	Name string
	URL  string
}

type SiteConversionCandidate struct {
	ID       uuid.UUID
	Name     string
	Revision int64
}

type RelatedReading struct {
	ID        uuid.UUID
	Title     string
	URL       string
	CreatedAt time.Time
}

type SiteMergeSource struct {
	ID       uuid.UUID
	Revision int64
}

type ExecuteSiteMergeParams struct {
	TargetID       uuid.UUID
	TargetRevision int64
	Sources        []SiteMergeSource
	Name           *string
	Intro          *string
	HomepageURL    *string
	IconURL        *string
	UserNote       *string
}

type SiteMergeResult struct {
	SiteID       uuid.UUID
	Revision     int64
	MovedEntries int
	DeletedLinks int
}

type ExecuteSiteSplitParams struct {
	SourceID              uuid.UUID
	SourceRevision        int64
	EntryIDs              []uuid.UUID
	Name                  string
	Intro                 *string
	HomepageURL           *string
	IconURL               *string
	UserNote              *string
	PrimaryEntryID        uuid.UUID
	IdentityKeyForNewSite *string
}

type SiteSplitResult struct {
	SourceID       uuid.UUID
	SourceRevision int64
	NewSiteID      uuid.UUID
	NewRevision    int64
	MovedEntries   int
}

type ConvertLinkParams struct {
	LinkID                  uuid.UUID
	TargetKind              model.LibraryKind
	ExpectedContentRevision int64
	TargetSiteID            *uuid.UUID
	ExpectedSiteRevision    *int64
	PreservedUserNote       *string
}

type ConvertLinkResult struct {
	LinkID          uuid.UUID
	Kind            model.LibraryKind
	ContentRevision int64
	Status          model.LinkStatus
	SiteID          *uuid.UUID
	SiteRevision    *int64
	EntryID         *uuid.UUID
	ParseAttempt    *model.ParseAttempt
}

type UpdateSiteProfileParams struct {
	ID          uuid.UUID
	Revision    int64
	Name        *string
	Intro       *string
	HomepageURL *string
	IconURL     *string
	UserNote    *string
	Pinned      *bool
	TagAdds     []SiteTagMutation
	TagRemovals []string
}

type SiteTagMutation struct {
	Tag           string
	NormalizedTag string
}

type UpdateSiteEntryParams struct {
	SiteID   uuid.UUID
	EntryID  uuid.UUID
	Revision int64
	Name     *string
	Purpose  *string
}

type SetSitePrimaryEntryParams struct {
	SiteID   uuid.UUID
	EntryID  uuid.UUID
	Revision int64
}

type DeleteSiteEntryParams struct {
	SiteID   uuid.UUID
	EntryID  uuid.UUID
	Revision int64
}

type SiteEntryDeleteResult struct {
	DeletedSite bool
}

type DeleteSiteParams struct {
	ID                uuid.UUID
	Revision          int64
	ConfirmEntryCount int
}

type SiteListFilter struct {
	View         string
	Tags         []string
	RecentCutoff *time.Time
	Limit        int
	Offset       int
}

type SiteListItem struct {
	model.Site
	DisplayHost     string
	Tags            []string
	EntryCount      int
	PrimaryEntryURL *string
}

type SiteDetail struct {
	SiteListItem
	Tags       []model.SiteTag
	Entries    []model.SiteEntry
	Identities []model.SiteIdentity
}

type ReadingParseCompleter interface {
	CompleteReadingParse(context.Context, CompleteReadingParseParams) (CompleteReadingParseResult, error)
}

// ParseStateStore owns the product-visible state transitions for one immutable
// River attempt. links.parse_generation is the only application-side attempt
// fence; River remains the only execution/retry ledger.
type ParseStateStore interface {
	MarkParseProcessing(context.Context, model.ParseAttempt) error
	MarkParseFailed(context.Context, model.ParseAttempt, string) error
}

// LinkSubmitResult is the product row and optional parse attempt produced by
// one durable submit transaction. A non-nil Attempt means the caller must
// enqueue it before commit; Restored tells the adapter to cancel stale work first.
type LinkSubmitResult struct {
	Link     *model.Link
	Attempt  *model.ParseAttempt // non-nil when this outcome created an attempt to enqueue
	Restored bool                // true when an existing Trash row was restored
}

// TranslationAttemptSeed is the immutable identity encoded into River job
// arguments. Product generation and source identity fence every projection;
// the business row does not mirror the River job ID.
type TranslationAttemptSeed = model.TranslationAttemptSeed

// TranslationStore is the read and terminal-projection seam used by services
// and workers. Durable scheduling is owned by app/durablework instead.
type TranslationStore interface {
	FindByIdentity(context.Context, UpsertTranslationParams) (*model.LinkTranslation, error)
	ListByLink(context.Context, uuid.UUID) ([]model.LinkTranslation, error)
	GetByID(context.Context, uuid.UUID) (*model.LinkTranslation, error)
	MarkProcessing(context.Context, model.TranslationAttempt) (*model.LinkTranslation, error)
	Complete(context.Context, model.TranslationAttempt, string, string) (bool, error)
	Fail(context.Context, model.TranslationAttempt, string) (bool, error)
}

type ScopedTagCount struct {
	Tag          string
	Count        int
	ReadingCount int
	SiteCount    int
}

// TreeStore 提供链接树视图所需的只读操作。
type TreeStore interface {
	// LookupByURLs returns existing real links for the supplied URLs in a
	// single round trip. Missing URLs simply do not appear in the result map.
	LookupByURLs(context.Context, []string) (map[string]*model.Link, error)
	ListVisible(context.Context, *string) ([]model.Link, error)
	ListDomains(context.Context) (DomainTreeSummarySet, error)
	ListDomainsScoped(context.Context, model.LibraryKind) (DomainTreeSummarySet, error)
}

// ListLinksFilter 描述 ListDone 查询的过滤与分页参数，同时承载 offset / cursor 两种分页模式。
type ListLinksFilter struct {
	Tags          []string
	Domain        *string
	ContentType   *string
	LibraryKind   *model.LibraryKind
	CreatedFrom   *time.Time
	CreatedBefore *time.Time
	// LowConfidence 非 nil 时按 links.is_low_confidence 过滤。
	//
	// 存在的理由：Reader 的「低置信」视图此前是在前端过滤的——单次拉最新 100 条
	// 再筛。而那个视图的全部意义就是捞出需要人工复核的历史遗留，按时间截断等于
	// 让它只覆盖最新的一小段，越老越看不见，而老的恰恰是最该处理的。
	LowConfidence *bool
	Limit         int
	Offset        int

	// Query, when non-nil, switches ListDone into keyword-search mode. The
	// service trims and validates it before constructing this filter.
	Query *string

	// Statuses 限定要返回的 links.status 集合（如 ["pending","processing",
	// "failed"]）。仓储零值兼容路径仍把空切片解释为 done；公开 service 的
	// 缺省请求会显式传入四种可见状态，从而返回全部已保存 links。非空时使用
	// status = ANY($n) 集合过滤，让客户端按 done 主列表或活动分区收窄。
	// 取值由 service 层校验为 model.LinkStatus 白名单子集后才会流到这里。
	Statuses []string

	// Cursor=true switches the query to cursor mode: ORDER BY
	// (created_at DESC, id DESC) with no windowed COUNT(*). After,
	// when non-nil, additionally restricts the rows to those strictly
	// past the (CreatedAt, ID) tuple — i.e. continuation. After=nil
	// with Cursor=true is the "start of stream" call. Cursor=false
	// is the offset path that powers the existing UI's page=N controls.
	Cursor bool
	After  *ListLinksCursor
}

// ListLinksCursor represents a stable position in the done-link
// timeline. The (CreatedAt, ID) tuple uniquely orders rows even when
// timestamps collide, so cursor pagination cannot skip or duplicate
// rows under concurrent inserts.
type ListLinksCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// TagCount 是 ListCounts 返回的单个标签聚合结果。
type TagCount struct {
	Tag   string
	Count int
}

// DomainTreeSummary is the lightweight domain aggregation used by the tree
// landing view before the client drills into a specific domain.
type DomainTreeSummary struct {
	Domain string
	Count  int
}

// DomainTreeSummarySet keeps the complete done-link total separate from
// selectable domain buckets because valid links may have no domain.
type DomainTreeSummarySet struct {
	Domains []DomainTreeSummary
	Total   int
}

// CreateLinkParams 是 Create / SubmitTx 写入 links 表所需的入参集合。
type CreateLinkParams struct {
	URL                     string
	SourceKind              string
	SourceKey               string
	InputTitle              *string
	InputText               *string
	InputHTML               *string
	InputImages             []string
	SourceMetadata          map[string]any
	Description             *string
	Status                  model.LinkStatus
	Domain                  *string
	ContentType             *string
	PathDepth               *int
	ParentPath              *string
	ParentID                *uuid.UUID
	RequestedLibraryKind    model.RequestedLibraryKind
	UserSelectedLibraryKind bool
}

type SetLibraryKindParams struct {
	ID       uuid.UUID
	Kind     model.LibraryKind
	Override bool
}

type SetLibraryKindResult struct {
	Status model.LinkStatus
}

// UpdateLinkStateParams 仅用于切换 links.status（及配套 error_msg），不动其他字段。
type UpdateLinkStateParams struct {
	ID       uuid.UUID
	Status   model.LinkStatus
	ErrorMsg *string
}

// UpdateLinkAnalysisParams 由解析管线在抓取/分析完成后调用，一次性写回标题、摘要、标签等分析产物。
type UpdateLinkAnalysisParams struct {
	ID uuid.UUID
	// ExpectedParseGeneration is the immutable Link generation carried by the
	// River job. A terminal write with an older generation is rejected in full.
	ExpectedParseGeneration int64
	// ExpectedMetadataRevision is the immutable revision captured on the
	// Link when this attempt was enqueued. It is consumed only by
	// terminal parse writers; ordinary UpdateAnalysis callers intentionally do
	// not apply this fence. Zero is the legacy safe-no-write sentinel.
	ExpectedMetadataRevision int64
	SourceKind               string
	SourceKey                string
	InputTitle               *string
	InputText                *string
	InputHTML                *string
	InputImages              []string
	SourceMetadata           map[string]any
	Title                    *string
	Summary                  *string
	Tags                     []string
	FetcherType              *string
	IsLowConfidence          bool
	LowConfidenceReason      *string
	Status                   model.LinkStatus
	ErrorMsg                 *string
	Domain                   *string
	ContentType              *string
	PathDepth                *int
	ParentPath               *string
	ParentID                 *uuid.UUID
}

type UpdateLibraryClassificationParams struct {
	ID     uuid.UUID
	Kind   model.LibraryKind
	Locked bool
}

// AggregateSiteParams contains analysis-derived site profile data. All
// sources are deliberately fixed to auto here; user-managed changes use the
// dedicated management service and must never be overwritten by aggregation.
type AggregateSiteParams struct {
	LinkID        uuid.UUID
	IdentityKey   string
	NormalizedURL string
	Name          string
	Intro         string
	EntryName     string
	Purpose       string
	HomepageURL   *string
	IconURL       *string
}

type SiteAggregateResult struct {
	SiteID       uuid.UUID
	EntryID      uuid.UUID
	CreatedSite  bool
	CreatedEntry bool
}

type CompleteSiteParseParams struct {
	Analysis                  UpdateLinkAnalysisParams
	Classification            UpdateLibraryClassificationParams
	Site                      AggregateSiteParams
	ExpectedLibraryKind       *model.LibraryKind
	ExpectedLibraryKindLocked bool
}

type CompleteReadingParseParams struct {
	Analysis                  UpdateLinkAnalysisParams
	Classification            UpdateLibraryClassificationParams
	ExpectedLibraryKind       *model.LibraryKind
	ExpectedLibraryKindLocked bool
}

// CompleteReadingParseResult reports whether this immutable parse attempt was
// still entitled to write the user-owned metadata tuple. Lifecycle and
// classification completion can succeed even when MetadataApplied is false;
// callers must then skip asynchronous enrichments derived from stale analyzer
// output.
type CompleteReadingParseResult struct {
	MetadataRevision int64
	MetadataApplied  bool
}
