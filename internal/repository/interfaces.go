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

// ErrInvalidReaderFeedReason marks scoring evidence that cannot produce the
// frozen, machine-readable reason tuple required by a Feed snapshot.
var ErrInvalidReaderFeedReason = errors.New("invalid reader feed reason")

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

// ErrReaderInboxJobNotRunnable means a durable inbox job has already reached
// a terminal state. River workers treat it as an idempotent no-op.
var ErrReaderInboxJobNotRunnable = errors.New("reader inbox job is no longer runnable")

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
// legal lifecycle transition, such as confirming a discarded item or
// discarding an already confirmed saved link.
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

// ErrInvalidReaderCategoryMembership marks an empty or unsupported polymorphic
// category member identity.
var ErrInvalidReaderCategoryMembership = errors.New("invalid reader category membership")

// ErrParseJobNotRunnable means the exact immutable parse attempt has already
// reached a terminal state (including being superseded by a newer capture).
// Workers treat it as a clean no-op instead of retrying stale work.
var ErrParseJobNotRunnable = errors.New("parse job is no longer runnable")

// ReaderArchiveReader streams installation-owned Reader rows as JSON objects. The
// callback receives one already-encoded row at a time so archive responses do
// not buffer notes, thoughts, or saved content in memory. Implementations expose
// only the frozen archive projection and omit storage-only fields.
type ReaderArchiveReader interface {
	StreamReaderArchiveSection(context.Context, string, func([]byte) error) error
}

// ErrSiteEntryNotFound distinguishes a missing entry from a revision conflict
// on its owning site. Management services map it to the stable entry 404
// contract without exposing storage-layer details.
var ErrSiteEntryNotFound = errors.New("site entry not found")

// ErrRevisionConflict means a revision-guarded aggregate write observed a
// newer site state. Services deliberately map this to a stable 409 rather
// than treating it as a missing object.
var ErrRevisionConflict = errors.New("revision conflict")

// ErrLibraryKindLocked 表示写入被 library_kind_locked 挡下：链接已由用户裁决
// 归属，自动分类不得改写。
//
// 它与 ErrNotFound 必须分开——两者在 UPDATE 里都表现为零行，但含义相反：
// ErrNotFound 是数据不见了（要查），本错误是锁正常生效（不用查）。
var ErrLibraryKindLocked = errors.New("library kind locked by user")

// ErrLibraryIntentChanged means a terminal parse transaction observed a newer
// committed capture intent than the pipeline used to choose its branch. The
// pipeline must reload the intent and recompute the final partition.
var ErrLibraryIntentChanged = errors.New("requested library intent changed")

// LinkDetailProjection is the persisted read model exposed by link detail and
// list responses. Capture payloads, worker identity, and maintenance timestamps
// are deliberately absent: adding one of those columns to a point lookup would
// make the projection pay the TOAST/scan cost without a response consumer.
type LinkDetailProjection struct {
	ID                        uuid.UUID                `db:"id"`
	URL                       string                   `db:"url"`
	Title                     *string                  `db:"title"`
	Summary                   *string                  `db:"summary"`
	Tags                      []string                 `db:"tags"`
	FetcherType               *string                  `db:"fetcher_type"`
	IsLowConfidence           bool                     `db:"is_low_confidence"`
	LowConfidenceReason       *string                  `db:"low_confidence_reason"`
	Status                    model.LinkStatus         `db:"status"`
	ErrorMsg                  *string                  `db:"error_msg"`
	Description               *string                  `db:"description"`
	Domain                    *string                  `db:"domain"`
	ContentType               *string                  `db:"content_type"`
	LibraryKind               *model.LibraryKind       `db:"library_kind"`
	LibraryKindSource         *model.LibraryKindSource `db:"library_kind_source"`
	LibraryKindLocked         bool                     `db:"library_kind_locked"`
	PredictedLibraryKind      *model.LibraryKind       `db:"predicted_library_kind"`
	ClassificationConfidence  *float32                 `db:"classification_confidence"`
	ClassificationReason      *string                  `db:"classification_reason"`
	ClassificationExplanation *string                  `db:"classification_explanation"`
	ClassifierVersion         *string                  `db:"classifier_version"`
	ContentRevision           int64                    `db:"content_revision"`
	MetadataRevision          int64                    `db:"metadata_revision"`
	ContentSource             model.ContentSource      `db:"content_source"`
	HasContent                bool                     `db:"has_content"`
	ContentCJKChars           int                      `db:"content_cjk_chars"`
	ContentWords              int                      `db:"content_words"`
	PathDepth                 *int                     `db:"path_depth"`
	ParentPath                *string                  `db:"parent_path"`
	ParentID                  *uuid.UUID               `db:"parent_id"`
	CreatedAt                 time.Time                `db:"created_at"`
	UpdatedAt                 time.Time                `db:"updated_at"`
}

// LinkParseInput contains the source material and state needed to run one
// parse, compare an ingest capture, or save the captured original. It excludes
// every presentation/enrichment field produced by parsing.
type LinkParseInput struct {
	ID                         uuid.UUID                        `db:"id"`
	URL                        string                           `db:"url"`
	SourceKind                 string                           `db:"source_kind"`
	SourceKey                  string                           `db:"source_key"`
	InputTitle                 *string                          `db:"input_title"`
	InputText                  *string                          `db:"input_text"`
	InputHTML                  *string                          `db:"input_html"`
	InputImages                []string                         `db:"input_images"`
	SourceMetadata             map[string]any                   `db:"source_metadata"`
	Description                *string                          `db:"description"`
	Status                     model.LinkStatus                 `db:"status"`
	RequestedLibraryKind       model.RequestedLibraryKind       `db:"requested_library_kind"`
	RequestedLibraryKindSource model.RequestedLibraryKindSource `db:"requested_library_kind_source"`
	LibraryKind                *model.LibraryKind               `db:"library_kind"`
	LibraryKindLocked          bool                             `db:"library_kind_locked"`
	ContentRevision            int64                            `db:"content_revision"`
	UpdatedAt                  time.Time                        `db:"updated_at"`
}

// LinkLifecycleProjection is the small state/CAS view used by conversion and
// deletion commands. Classification provenance is included because conversion
// metrics must describe the pre-command decision, not the post-command row.
type LinkLifecycleProjection struct {
	ID                   uuid.UUID                `db:"id"`
	URL                  string                   `db:"url"`
	Status               model.LinkStatus         `db:"status"`
	LibraryKind          *model.LibraryKind       `db:"library_kind"`
	LibraryKindSource    *model.LibraryKindSource `db:"library_kind_source"`
	LibraryKindLocked    bool                     `db:"library_kind_locked"`
	ClassificationReason *string                  `db:"classification_reason"`
	ContentRevision      int64                    `db:"content_revision"`
	HasContent           bool                     `db:"has_content"`
	DeletedAt            *time.Time               `db:"deleted_at"`
}

// LinkSubmitLookup is the identity/state view used by ordinary URL submit and
// refresh. Multimodal ingest deliberately uses LinkParseInput because capture
// equality depends on the original input payload.
type LinkSubmitLookup struct {
	ID                         uuid.UUID                        `db:"id"`
	URL                        string                           `db:"url"`
	SourceKey                  string                           `db:"source_key"`
	Status                     model.LinkStatus                 `db:"status"`
	RequestedLibraryKind       model.RequestedLibraryKind       `db:"requested_library_kind"`
	RequestedLibraryKindSource model.RequestedLibraryKindSource `db:"requested_library_kind_source"`
	LibraryKind                *model.LibraryKind               `db:"library_kind"`
}

// LinkDetailReader owns response-shaped reads. ListDone retains model.Link for
// now because its existing linkListColumns projection is already independently
// narrow and also feeds the tree/export paths; RF9 changes point lookup payloads
// without coupling that stable list seam to the new detail scanner.
type LinkDetailReader interface {
	GetDetailByID(context.Context, uuid.UUID) (*LinkDetailProjection, error)
	GetDetailByURL(context.Context, string) (*LinkDetailProjection, error)
	ListDone(context.Context, ListLinksFilter) ([]model.Link, int, error)
}

type LinkParseInputReader interface {
	GetParseInputByID(context.Context, uuid.UUID) (*LinkParseInput, error)
	GetParseInputBySourceKeyOrURL(context.Context, string, string) (*LinkParseInput, error)
}

type LinkLifecycleReader interface {
	GetLifecycleByID(context.Context, uuid.UUID) (*LinkLifecycleProjection, error)
}

type LinkSubmitLookupReader interface {
	GetSubmitLookupByID(context.Context, uuid.UUID) (*LinkSubmitLookup, error)
	GetSubmitLookupByURL(context.Context, string) (*LinkSubmitLookup, error)
}

// LinkReader is the composition-root read capability. Feature modules should
// accept one of the smaller consumer-owned interfaces above wherever possible.
type LinkReader interface {
	LinkDetailReader
	LinkParseInputReader
	LinkLifecycleReader
	LinkSubmitLookupReader
}

// LinkWriter is the write-side slice (insert + state transitions + delete) used
// by submit and the parse pipeline. Splitting it from LinkReader lets a
// pipeline test fake skip the listing methods entirely.
type LinkWriter interface {
	Create(context.Context, CreateLinkParams) (*model.Link, error)
	UpdateState(context.Context, UpdateLinkStateParams) error
	UpdateAnalysis(context.Context, UpdateLinkAnalysisParams) error
	Delete(context.Context, uuid.UUID) error
}

// LibraryClassificationWriter owns final classification updates. It stays
// separate from LinkWriter because classification changes have stricter
// invariants than ordinary analysis writes and will be used by the site
// completion transaction.
type LibraryClassificationWriter interface {
	UpdateLibraryClassification(context.Context, UpdateLibraryClassificationParams) error
}

// SiteAggregator atomically binds a final site link to its stable identity
// and creates or refreshes the corresponding SiteEntry.
type SiteAggregator interface {
	Aggregate(context.Context, AggregateSiteParams) (SiteAggregateResult, error)
}

// SiteParseCompleter owns the terminal transaction for a parsed site link.
// It is intentionally separate from ParseStateStore because it additionally
// clears reading-only assets and aggregates a SiteEntry before exposing done.
type SiteParseCompleter interface {
	CompleteSiteParse(context.Context, CompleteSiteParseParams, uuid.UUID) (SiteAggregateResult, error)
}

type SiteReader interface {
	ListSites(context.Context, SiteListFilter) ([]SiteListItem, int, error)
	GetSite(context.Context, uuid.UUID) (*SiteDetail, error)
}

// SiteIdentityLookup is the tiny read surface used by conversion previews to
// advertise a CAS-ready existing aggregation target without listing sites.
type SiteIdentityLookup interface {
	FindByIdentityKey(context.Context, string) (*SiteConversionCandidate, error)
}

type SiteSearchStore interface {
	SearchSites(context.Context, string, int) ([]SiteSearchMatch, int, error)
}

// SiteSemanticSearchStore is optional so older stores retain their keyword
// search behavior while the service can use the profile-vector hybrid path.
type SiteSemanticSearchStore interface {
	SearchSitesSemantic(context.Context, string, []float32, string, int) ([]SiteSearchMatch, int, error)
}

type SiteEmbeddingCandidate struct {
	ID          uuid.UUID
	Revision    int64
	Name        string
	Intro       string
	DisplayHost string
	Tags        []string
	Entries     []SiteEmbeddingEntryCandidate
}

type SiteEmbeddingEntryCandidate struct {
	Name    string
	Purpose string
}

type SiteEmbeddingStore interface {
	ListSitesNeedingEmbedding(context.Context, string, uuid.UUID, int) ([]SiteEmbeddingCandidate, error)
	UpdateSiteEmbedding(context.Context, uuid.UUID, int64, []float32, string) (bool, error)
}

type ClassificationRuleStore interface {
	ListClassificationRules(context.Context) ([]model.LibraryClassificationRule, error)
	CreateClassificationRule(context.Context, CreateClassificationRuleParams) (*model.LibraryClassificationRule, error)
	UpdateClassificationRule(context.Context, UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error)
	DeleteClassificationRule(context.Context, uuid.UUID, int64) (bool, error)
}

// LibraryReviewStore owns the durable review queue. Creation is intentionally
// internal: callers have already validated that payload contains only bounded
// structured candidates, never captured page data.
type LibraryReviewStore interface {
	ListLibraryReviews(context.Context, ListLibraryReviewsParams) ([]model.LibraryReviewItem, error)
	ResolveLibraryReview(context.Context, ResolveLibraryReviewParams) (*model.LibraryReviewItem, error)
}

type MigrationReviewActionExecutor interface {
	KeepHistoricalMigrationReading(context.Context, uuid.UUID, int64) (*model.LibraryReviewItem, error)
	MoveHistoricalMigrationToSite(context.Context, uuid.UUID, int64) (*model.LibraryReviewItem, error)
}

type ListLibraryReviewsParams struct {
	Status *model.LibraryReviewStatus
	Kind   *model.LibraryReviewKind
	Limit  int
	Offset int
}

type ResolveLibraryReviewParams struct {
	ID       uuid.UUID
	Revision int64
	Status   model.LibraryReviewStatus
}

type CreateClassificationRuleParams struct {
	Host            string
	IdentityAdapter *string
	PathPrefix      *string
	TargetKind      model.LibraryKind
	Enabled         bool
}

type UpdateClassificationRuleParams struct {
	ID              uuid.UUID
	Revision        int64
	Host            *string
	IdentityAdapter **string
	PathPrefix      **string
	TargetKind      *model.LibraryKind
	Enabled         *bool
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

type SiteProfileWriter interface {
	UpdateSiteProfile(context.Context, UpdateSiteProfileParams) (bool, error)
}

// SiteProfileTagWriter performs a revision-guarded profile and tag delta in a
// single transaction. It prevents a profile revision from being visible with
// only half of the requested tag patch applied.
type SiteProfileTagWriter interface {
	UpdateSiteProfileAndTags(context.Context, UpdateSiteProfileParams) (bool, error)
}

// SiteManagementWriter owns mutations that affect a site's entries or its
// aggregate lifecycle. Every operation is revision-guarded at the site root
// because entry actions can change the primary entry and entry count.
type SiteManagementWriter interface {
	UpdateSiteEntry(context.Context, UpdateSiteEntryParams) (bool, error)
	SetSitePrimaryEntry(context.Context, SetSitePrimaryEntryParams) (bool, error)
	DeleteSiteEntry(context.Context, DeleteSiteEntryParams) (SiteEntryDeleteResult, error)
	DeleteSite(context.Context, DeleteSiteParams) (bool, error)
}

type SiteMergeWriter interface {
	ExecuteSiteMerge(context.Context, ExecuteSiteMergeParams) (SiteMergeResult, error)
}

type SiteSplitWriter interface {
	ExecuteSiteSplit(context.Context, ExecuteSiteSplitParams) (SiteSplitResult, error)
}

// SiteRelatedReader deliberately returns reading links only. Site entries are
// excluded in SQL so a related item can never affect a site's lifecycle.
type SiteRelatedReader interface {
	ListRelatedReadings(context.Context, []string, []string, int) ([]RelatedReading, error)
}

type ArchiveV2Reader interface {
	StreamArchiveV2Section(context.Context, string, func([]byte) error) error
}

// ArchiveV2RuleReader keeps the versioned archive bounded even when the
// installation has accumulated many personal classification rules.
type ArchiveV2RuleReader interface {
	StreamArchiveV2Rules(context.Context, func([]byte) error) error
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
	ParseJobID      *uuid.UUID
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
	CompleteReadingParse(context.Context, CompleteReadingParseParams, uuid.UUID) (CompleteReadingParseResult, error)
}

// ParseStateStore owns the cross-table state machine for one immutable parse
// attempt. Each method updates the link and the exact parse_jobs row in one
// database transaction so observers can never see a terminal link paired with
// a stale job (or vice versa).
//
// 终态写入不在此接口上：解析成功必须落到 ReadingParseCompleter 或
// SiteParseCompleter，二者除写 links/parse_jobs 外还分别维护分类字段、
// payload 清理截止时间与站点聚合。此前 CompleteParse 也在这里，使得
// pipeline 有一条「只写 links 不写分类」的旁路——生产从不走它，测试却大量
// 走它。移出接口后该旁路在类型层面即不可达。
// PGXLinkRepository.CompleteParse 仍作为具体方法保留，供 dbintegration
// 直接验证底层 SQL 行为。
type ParseStateStore interface {
	MarkParseProcessing(context.Context, uuid.UUID, uuid.UUID) error
	MarkParseFailed(context.Context, uuid.UUID, uuid.UUID, string) error
}

// LinkSubmitResult is the per-item outcome of a SubmitBatch call. Inserted
// distinguishes a brand-new row from a re-submission of an already-known
// source_key. A non-nil Job always means the caller should enqueue: fresh rows
// receive their initial attempt, while restored pending/processing rows receive
// one replacement attempt. Restored reports that the existing Link came back
// from Trash in this transaction.
//
// Error is populated only when the batch transaction's fresh subset could not
// be admitted. In that case every provisional fresh insert is rolled back,
// while already-existing rows remain populated in the same 1:1 result slice so
// the service can still report those idempotent outcomes. Link and Job are nil
// for an errored item.
type LinkSubmitResult struct {
	Link     *model.Link
	Job      *model.ParseJob // non-nil when this outcome created an attempt to enqueue
	Inserted bool            // true = fresh insert; false = pre-existing row returned via ON CONFLICT
	Restored bool            // true = an existing Trash row was restored in this transaction
	Error    error           // non-nil when this fresh item was rejected before commit
}

// LinkStore is the union of the three role interfaces. PGXLinkRepository
// implements every method, so a single embedded value still satisfies the
// composite shape; consumers should depend on the smallest sub-interface
// that meets their needs.
type LinkStore interface {
	LinkReader
	LinkWriter
}

// LinkReadDeleter is the exact surface needed by service.LinkReadService:
// every read-side method plus Delete (the only write the read service
// performs, via the cache-invalidating delete path). Carved out so the
// service no longer declares an anonymous composite interface inline.
type LinkReadDeleter interface {
	LinkReader
	Delete(context.Context, uuid.UUID) error
}

// TranslationAttemptSeed is the immutable identity available before the River
// row exists. A scheduler must encode translation/product/generation plus the exact
// source hash and nullable saved-content revision in its job args, then return
// the inserted River ID to complete the current-attempt binding.
type TranslationAttemptSeed = model.TranslationAttemptSeed

// TranslationScheduleCommand carries both the new immutable attempt seed and
// the exact current attempt it supersedes. The queue applies both changes in
// the repository transaction so an old active River job cannot be orphaned.
type TranslationScheduleCommand = model.TranslationScheduleCommand

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

// TranslationListSnapshotReader owns the coherent read boundary used by the
// translation LIST service. Transaction details remain repository-internal.
type TranslationListSnapshotReader interface {
	ReadListSnapshot(context.Context, uuid.UUID) (*TranslationListSnapshot, error)
}

// JobStore 是 parse_jobs 表的仓储接口，提供任务创建、状态更新与查询。
type JobStore interface {
	Create(context.Context, uuid.UUID) (*model.ParseJob, error)
	GetByID(context.Context, uuid.UUID) (*model.ParseJob, error)
	ListByIDs(context.Context, []uuid.UUID) ([]model.ParseJob, error)
	GetLatestByLinkID(context.Context, uuid.UUID) (*model.ParseJob, error)
	UpdateState(context.Context, UpdateJobStateParams) error
}

// TagStore 提供已完成链接（status='done'）上 tags 字段的聚合视图，用于 /api/tags 等只读接口。
type TagStore interface {
	ListDistinct(context.Context) ([]string, error)
	ListCounts(context.Context) ([]TagCount, error)
}

// ScopedTagStore adds collection-aware counts without changing the legacy
// TagStore contract used by old clients and cache tests.
type ScopedTagStore interface {
	ListScopedCounts(context.Context, string) ([]ScopedTagCount, error)
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

	// Query, when non-nil, switches ListDone into hybrid-search mode (Phase 9
	// q= contract). The service layer sets it only after trimming/length-
	// validating the raw ?q= value; an empty string is never passed (the
	// service treats blank q as "not supplied"). When QueryEmbedding is also
	// set the repository runs the semantic-nearest ∪ ILIKE merge; with
	// QueryEmbedding nil it degrades to pure ILIKE(title/summary/tags). Either
	// way the result is top-N truncated (no pagination) and ordered
	// semantic-hits-first. Domain/Tags/ContentType/Statuses still apply to
	// both legs.
	Query *string
	// QueryEmbedding is the query vector for the semantic leg of hybrid
	// search. nil means "EMBEDDING_MODEL unset or query vectorization failed"
	// → pure ILIKE fallback. EmbeddingModel scopes the nearest-neighbour scan
	// to same-model rows (cross-model vectors live in an incompatible space).
	QueryEmbedding []float32
	EmbeddingModel string

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

// CreateLinkParams 是 Create / SubmitNew / SubmitBatch 写入 links 表所需的入参集合。
type CreateLinkParams struct {
	URL                        string
	SourceKind                 string
	SourceKey                  string
	InputTitle                 *string
	InputText                  *string
	InputHTML                  *string
	InputImages                []string
	SourceMetadata             map[string]any
	Description                *string
	Status                     model.LinkStatus
	Domain                     *string
	ContentType                *string
	PathDepth                  *int
	ParentPath                 *string
	ParentID                   *uuid.UUID
	RequestedLibraryKind       model.RequestedLibraryKind
	RequestedLibraryKindSource model.RequestedLibraryKindSource
	PredictedLibraryKind       *model.LibraryKind
}

type UpdateRequestedLibraryIntentParams struct {
	ID     uuid.UUID
	Kind   model.RequestedLibraryKind
	Source model.RequestedLibraryKindSource
}

type UpdateRequestedLibraryIntentResult struct {
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
	// ExpectedMetadataRevision is the immutable revision captured on the
	// parse_jobs row when this attempt was enqueued. It is consumed only by
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
	ID                uuid.UUID
	Kind              model.LibraryKind
	Source            model.LibraryKindSource
	Locked            bool
	PredictedKind     *model.LibraryKind
	Confidence        *float32
	Reason            *string
	Explanation       *string
	ClassifierVersion *string
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
	Analysis                           UpdateLinkAnalysisParams
	Classification                     UpdateLibraryClassificationParams
	Site                               AggregateSiteParams
	ExpectedRequestedLibraryKind       model.RequestedLibraryKind
	ExpectedRequestedLibraryKindSource model.RequestedLibraryKindSource
}

type CompleteReadingParseParams struct {
	Analysis                           UpdateLinkAnalysisParams
	Classification                     UpdateLibraryClassificationParams
	ExpectedRequestedLibraryKind       model.RequestedLibraryKind
	ExpectedRequestedLibraryKindSource model.RequestedLibraryKindSource
	DetachSiteEntry                    bool
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

// UpdateJobStateParams 仅承载 parse_jobs.status 切换所需的字段（含可选 error_msg）。
type UpdateJobStateParams struct {
	ID       uuid.UUID
	Status   model.JobStatus
	ErrorMsg *string
}
