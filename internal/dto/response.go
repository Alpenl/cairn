package dto

import "time"

// SubmitResponse 是单条链接提交后回给客户端的结果，描述本次提交进入的状态以及对应的 link/job 标识。
type SubmitResponse struct {
	// JobID is omitted on submissions that hit an already-done link: there is
	// no parse_jobs row to report and the previous "synthesize a fake done
	// job" workaround now goes away. The pointer + omitempty combo keeps
	// the JSON shape identical for the common pending/processing path
	// (string job_id) while making the absence explicit on done.
	JobID       *string `json:"job_id,omitempty"`
	LinkID      string  `json:"link_id,omitempty"`
	InboxID     string  `json:"inbox_id,omitempty"`
	Destination string  `json:"destination,omitempty"`
	Status      string  `json:"status"`
}

// BatchItemResponse carries the per-item outcome of POST /api/links/batch.
// Result and Error are mutually exclusive: one of them is set per item. The
// item position is implied by the array index — no explicit Index field is
// surfaced because the response order matches the request order one-to-one.
// Error is a short, sanitized client-facing message; internal cause chains
// stay in the structured logs.
type BatchItemResponse struct {
	Result *SubmitResponse `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// BatchSubmitResponse is returned by POST /api/links/batch as a parallel
// array of per-item submission outcomes. Per-item errors surface alongside
// successes (partial-success contract); a request only fails outright when
// validation rejects the envelope itself (empty / >100). Aggregate
// success/failure counters are omitted because the client can derive them
// from Results in O(n) without server bookkeeping.
type BatchSubmitResponse struct {
	Results []BatchItemResponse `json:"results"`
}

// CapabilitiesResponse lets independently deployed clients enable additive
// collection features without inferring support from a server version.
type CapabilitiesResponse struct {
	LibraryKinds           bool                       `json:"library_kinds"`
	SiteLibrary            bool                       `json:"site_library"`
	SiteAutoClassification bool                       `json:"site_auto_classification"`
	SiteManagement         bool                       `json:"site_management"`
	SiteAdvancedManagement bool                       `json:"site_advanced_management"`
	ArchiveVersions        []int                      `json:"archive_versions"`
	ReaderVNext            bool                       `json:"reader_vnext"`
	Reader                 ReaderCapabilitiesResponse `json:"reader"`
}

type SitePrimaryEntryResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// ConversionPreviewResponse is intentionally factual rather than advisory:
// execute uses ExpectedContentRevision as its optimistic-concurrency token.
// The asset fields report what a reading-to-site conversion would remove.
type ConversionPreviewResponse struct {
	LinkID                  string                           `json:"link_id"`
	CurrentKind             string                           `json:"current_kind"`
	TargetKind              string                           `json:"target_kind"`
	ExpectedContentRevision int64                            `json:"expected_content_revision"`
	Destructive             bool                             `json:"destructive"`
	SavedOriginal           bool                             `json:"saved_original"`
	TranslationCount        int                              `json:"translation_count"`
	ReparseRequired         bool                             `json:"reparse_required"`
	AnnotationPolicy        string                           `json:"annotation_policy,omitempty"`
	MatchingSite            *ConversionSiteCandidateResponse `json:"matching_site,omitempty"`
}

type ConversionSiteCandidateResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

// ConversionExecuteResponse is the durable state after a conversion. Reader
// uses ReaderTarget to replace a stale detail selection without guessing URLs.
type ConversionExecuteResponse struct {
	LinkID          string       `json:"link_id"`
	LibraryKind     string       `json:"library_kind"`
	ContentRevision int64        `json:"content_revision"`
	Status          string       `json:"status"`
	SiteID          string       `json:"site_id,omitempty"`
	SiteRevision    int64        `json:"site_revision,omitempty"`
	EntryID         string       `json:"entry_id,omitempty"`
	ReparseRequired bool         `json:"reparse_required"`
	ParseJobID      string       `json:"parse_job_id,omitempty"`
	ReaderTarget    ReaderTarget `json:"reader_target"`
}

// ReaderTarget deliberately carries identifiers only. Clients combine it with
// their configured Reader base URL instead of accepting an origin from the
// backend response.
type ReaderTarget struct {
	View    string `json:"view"`
	LinkID  string `json:"link_id,omitempty"`
	SiteID  string `json:"site_id,omitempty"`
	EntryID string `json:"entry_id,omitempty"`
}
type SiteListItemResponse struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Intro            string                    `json:"intro"`
	DisplayHost      string                    `json:"display_host"`
	HomepageURL      *string                   `json:"homepage_url"`
	IconURL          *string                   `json:"icon_url"`
	Tags             []string                  `json:"tags"`
	EntryCount       int                       `json:"entry_count"`
	Pinned           bool                      `json:"pinned"`
	NeedsReview      bool                      `json:"needs_review"`
	PrimaryEntry     *SitePrimaryEntryResponse `json:"primary_entry"`
	Revision         int64                     `json:"revision"`
	FirstCollectedAt time.Time                 `json:"first_collected_at"`
	LastCollectedAt  time.Time                 `json:"last_collected_at"`
}
type PaginatedSitesResponse struct {
	Items        []SiteListItemResponse `json:"items"`
	Total        int                    `json:"total"`
	Page         int                    `json:"page"`
	Limit        int                    `json:"limit"`
	RecentCutoff *time.Time             `json:"recent_cutoff,omitempty"`
}
type SiteTagResponse struct {
	Tag    string `json:"tag"`
	Source string `json:"source"`
}
type SiteEntryResponse struct {
	ID                string     `json:"id"`
	LinkID            string     `json:"link_id"`
	Name              string     `json:"name"`
	NameSource        string     `json:"name_source"`
	Purpose           string     `json:"purpose"`
	PurposeSource     string     `json:"purpose_source"`
	URL               string     `json:"url"`
	FirstCollectedAt  time.Time  `json:"first_collected_at"`
	LastRecollectedAt *time.Time `json:"last_recollected_at,omitempty"`
}
type RelatedReadingResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}
type SiteDetailResponse struct {
	SiteListItemResponse
	UserNote        string                   `json:"user_note"`
	GroupingLocked  bool                     `json:"grouping_locked"`
	TagsWithSource  []SiteTagResponse        `json:"tags_with_source"`
	Entries         []SiteEntryResponse      `json:"entries"`
	RelatedReadings []RelatedReadingResponse `json:"related_readings"`
}

type SiteEntryDeleteResponse struct {
	DeletedSite bool `json:"deleted_site"`
}

// ListLinksRequest carries the GET /api/links query parameters in a single
// transport-friendly shape. The handler parses URL query strings into this
// type and the service layer consumes it directly, so no adapter is needed
// between them.
//
// Cursor is true when the client has opted into cursor mode by
// supplying ?after= (with any value, including empty for "start of
// stream"). After is the opaque cursor token; an empty After plus
// Cursor=true means "first page". Cursor mode skips the windowed
// COUNT(*) so listing past 100k rows stays O(limit). Mutually
// exclusive with Page>1 — the service rejects that combination
// with 422.
type ListLinksRequest struct {
	Tags          string
	Domain        string
	ContentType   string
	LibraryKind   string
	Page          int
	Limit         int
	Cursor        bool
	After         string
	CreatedFrom   string
	CreatedBefore string

	// Status is the raw, comma-separated status filter from the query
	// string (e.g. "pending,processing,failed"). Empty means every saved-link
	// state (pending/processing/failed/done), so a new or failed bookmark stays
	// visible throughout enrichment. The service layer splits and validates
	// explicit values against that whitelist, rejecting any other token with
	// 400 so a typo never silently returns an empty page.
	Status string

	// Query is the raw ?q= value (Phase 9 hybrid search). Blank / whitespace
	// is treated as "not supplied" (back-compat: falls through to the normal
	// list path). Non-blank switches to semantic+ILIKE hybrid search (or pure
	// ILIKE when EMBEDDING_MODEL is off / vectorization fails): top-50, no
	// pagination — page/after are ignored. Over-length q returns 422.
	Query string

	// LowConfidence is the raw ?low_confidence= value. "true" / "false" filter
	// on links.is_low_confidence; empty means no filter. Any other value is
	// rejected with 422 rather than silently ignored — a typo that quietly
	// returns the unfiltered list is how a review queue ends up looking empty.
	LowConfidence string

	// URL is the raw ?url= value (Phase 9 existence check). Non-blank does an
	// exact normalized-URL match returning 0 or 1 link, no pagination. Takes
	// precedence over Query when both are supplied (q ignored).
	URL string
}

// TranslationSourceIdentity describes the authoritative source observed when
// a translation source-CAS request conflicts.
type TranslationSourceIdentity struct {
	ContentRevision *int64  `json:"content_revision,omitempty"`
	BlockKey        string  `json:"block_key,omitempty"`
	SourceHash      *string `json:"source_hash,omitempty"`
}

// ErrorDetail 描述统一错误体中的具体错误信息。
//
// 字段含义：
//   - Code (json: "code") —— HTTP 状态码的数值表示，保留以兼容历史客户端。
//   - ErrorCode (json: "error_code") —— Wave 6 新增的机器可读 slug，
//     如 "link_not_found" / "cooldown_active"，比 HTTP status 信息更细。
//     客户端 SDK 可基于 slug 做稳定的分支判断，避免对 message 做字符串
//     匹配。空字符串走 omitempty 省略，保留旧响应形状的兼容性；服务端
//     middleware.JSONError / JSONErrorWithSlug 总是会写入一个非空 slug
//     （未提供时退回 "default_<status>"）。
//   - Message —— 面向最终用户的简短描述，可能本地化或脱敏，不适合做
//     程序判断。
//   - CurrentIdentity —— translation source CAS 冲突时的权威当前身份；其他
//     错误省略。saved content 使用 content_revision，summary 使用 source_hash；
//     已下线的 deep-research 只保留历史可读能力，不接受新的创建请求。
type ErrorDetail struct {
	Code            int                        `json:"code"`                       // HTTP 状态码或业务码
	ErrorCode       string                     `json:"error_code,omitempty"`       // 机器可读 slug，例如 link_not_found
	Message         string                     `json:"message"`                    // 面向客户端的简短错误描述
	CurrentIdentity *TranslationSourceIdentity `json:"current_identity,omitempty"` // translation source CAS 的当前身份
}

// ErrorResponse 是所有失败响应的统一外层包裹，保证客户端可以稳定从 .error 取错误。
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// LinkResponse 是 /api/links 系列接口对外暴露的链接视图，包含解析结果、状态及低置信度 / 错误诊断字段。
type LinkResponse struct {
	ID                        string   `json:"id"`
	URL                       string   `json:"url"`
	Title                     *string  `json:"title"`
	Summary                   *string  `json:"summary"`
	Description               *string  `json:"description"`
	Tags                      []string `json:"tags"`
	ContentType               *string  `json:"content_type"` // 内容形态：article / listing / homepage 等
	LibraryKind               *string  `json:"library_kind,omitempty"`
	LibraryKindSource         *string  `json:"library_kind_source,omitempty"`
	LibraryKindLocked         bool     `json:"library_kind_locked,omitempty"`
	PredictedLibraryKind      *string  `json:"predicted_library_kind,omitempty"`
	ClassificationConfidence  *float32 `json:"classification_confidence,omitempty"`
	ClassificationReason      *string  `json:"classification_reason,omitempty"`
	ClassificationExplanation *string  `json:"classification_explanation,omitempty"`
	ClassifierVersion         *string  `json:"classifier_version,omitempty"`
	ContentRevision           int64    `json:"content_revision,omitempty"`
	MetadataRevision          int64    `json:"metadata_revision"`
	// ContentSource is detail-only metadata. List/search projections leave it
	// nil so the API does not expand their wire contract or query payload.
	ContentSource       *string   `json:"content_source,omitempty"`
	Status              string    `json:"status"`     // 链接处理状态：pending / processing / done / failed
	Domain              *string   `json:"domain"`     // 链接所属域名（用于聚合 / 过滤）
	PathDepth           *int      `json:"path_depth"` // URL 路径深度，用于树形展示
	ParentID            *string   `json:"parent_id"`  // 父链接 ID，构成域名内的链接树
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	FetcherType         *string   `json:"fetcher_type"`          // 实际使用的抓取器类型（http / yt-dlp 等）
	IsLowConfidence     bool      `json:"is_low_confidence"`     // 解析结果是否被判定为低置信度
	LowConfidenceReason *string   `json:"low_confidence_reason"` // 低置信度原因，便于前端给出提示
	ErrorCategory       *string   `json:"error_category"`        // 失败分类（upstream_http / parse_error 等）
	ErrorMsg            *string   `json:"error_msg"`             // 失败时的简短客户端可读消息
	ParentPath          *string   `json:"parent_path"`           // 父路径字符串，方便前端展示来源链路

	// HasContent 说明这条链接是否已有「保存原文」快照，但不携带正文本身。
	// Reader 把原文做成默认折叠区之后，进详情页只需要知道「有没有」来决定显示
	// 折叠头还是「保存原文」按钮；正文等用户展开时再走
	// GET /api/links/{id}/content 单独取。
	HasContent bool `json:"has_content"`

	// ContentCJKChars / ContentWords 是已保存原文的两项计数：CJK 字符数与
	// 西文词数。详情端为了判定 has_content 本来就要读一次正文，顺手数一遍是
	// 零额外 I/O。客户端用它们估阅读时长——**估算公式仍然只在客户端一份**，
	// 服务端只负责数数，这样正文没展开时和展开后套的是同一个公式、同样的输入，
	// 数字不会在展开瞬间跳变。没有已保存原文时都为 0（omitempty 省略）。
	ContentCJKChars int `json:"content_cjk_chars,omitempty"`
	ContentWords    int `json:"content_words,omitempty"`

	// Content 是用户按需「保存原文」后保存的网页完整正文；未保存为 null。
	// 仅详情接口在 include_content=true（默认）时返回；列表永远不带。
	// Reader 传 include_content=false 把整篇正文挪到展开时按需请求。
	Content         *string `json:"content,omitempty"`
	ContentDocument *string `json:"content_document,omitempty"`
	ContentFormat   *string `json:"content_format,omitempty"`
}

// LinkContentResponse 是「保存原文」POST 接口的返回：保存后的原文 + 来源抓取器类型。
type LinkContentResponse struct {
	LinkID          string  `json:"link_id"`
	Content         string  `json:"content"`
	ContentDocument *string `json:"content_document,omitempty"`
	ContentFormat   string  `json:"content_format"`
	FetcherType     string  `json:"fetcher_type"`
	ContentSource   string  `json:"content_source"`
	// ContentRevision 是这份正文的代次，与列表/详情里的同名字段同源。
	// 没有它，客户端要等下一次列表刷新才知道自己刚写的正文换了代次，而那段
	// 窗口里新建的正文划线会在刷新后被判定失配丢掉。不加 omitempty：0 是
	// 「这条 link 从未写过正文」的合法取值，省略会让客户端读到 undefined。
	ContentRevision int64 `json:"content_revision"`
}

type TranslationResponse struct {
	ID             string  `json:"id"`
	LinkID         string  `json:"link_id"`
	Scope          string  `json:"scope"`
	BlockKey       string  `json:"block_key"`
	StartOffset    int     `json:"start_offset"`
	EndOffset      int     `json:"end_offset"`
	SourceText     string  `json:"source_text"`
	TranslatedText *string `json:"translated_text"`
	SourceFormat   string  `json:"source_format"`
	TargetLanguage string  `json:"target_language"`
	// SourceContentRevision is null for summary translations, historical
	// retired deep-research rows, and legacy-unverified rows. It is
	// intentionally not omitted so clients can distinguish those rows without
	// guessing from response shape.
	SourceContentRevision *int64    `json:"source_content_revision"`
	Status                string    `json:"status"`
	Model                 *string   `json:"model"`
	ErrorMsg              *string   `json:"error_msg"`
	Stale                 bool      `json:"stale"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type TranslationListResponse struct {
	CurrentContentRevision int64 `json:"current_content_revision"`
	// CurrentSummarySourceHash is the wire CAS identity, not the
	// domain-separated hash persisted on verified translation rows.
	CurrentSummarySourceHash *string               `json:"current_summary_source_hash"`
	Items                    []TranslationResponse `json:"items"`
}

// JobResponse 是 /api/jobs 等接口对外暴露的解析任务视图，可选地内嵌当前 Link 快照。
type JobResponse struct {
	ID            string        `json:"id"`
	LinkID        string        `json:"link_id"`
	Status        string        `json:"status"`
	ErrorCategory *string       `json:"error_category"`
	ErrorMsg      *string       `json:"error_msg"`
	Link          *LinkResponse `json:"link"` // 关联的链接快照；列表场景下可能为 nil
}

// PaginatedLinksResponse always carries Items + Limit. In offset mode
// (the default, ?page=N), Total and Page reflect the windowed count
// and the requested page index. In cursor mode (?after=<token>), Total
// and Page are 0 because the count is intentionally not computed (the
// reason cursor mode exists is to avoid that scan); NextCursor carries
// the continuation token. NextCursor is omitted only when the response
// is shorter than Limit — a full page emits NextCursor even when it is
// effectively the last; the client confirms termination by issuing the
// follow-up call and seeing an empty Items array.
type PaginatedLinksResponse struct {
	Items      []LinkResponse `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	Limit      int            `json:"limit"`
	NextCursor *string        `json:"next_cursor,omitempty"`
}

// TagCountResponse 是标签聚合接口的单条结果：标签名及其被引用次数。
type TagCountResponse struct {
	Tag          string `json:"tag"`
	Count        int    `json:"count"`
	ReadingCount *int   `json:"reading_count,omitempty"`
	SiteCount    *int   `json:"site_count,omitempty"`
}

type LibrarySearchGroup struct {
	Items     []LinkResponse `json:"items"`
	TotalHint int            `json:"total_hint"`
}

type SiteSearchEntryResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type SiteSearchResultResponse struct {
	ID             string                    `json:"id"`
	Name           string                    `json:"name"`
	MatchedEntries []SiteSearchEntryResponse `json:"matched_entries"`
}

type SiteSearchGroup struct {
	Items     []SiteSearchResultResponse `json:"items"`
	TotalHint int                        `json:"total_hint"`
}

type ReaderThoughtSearchResponse struct {
	ID              string    `json:"id"`
	HostKind        string    `json:"host_kind"`
	HostID          string    `json:"host_id"`
	LinkID          *string   `json:"link_id,omitempty"`
	Snippet         string    `json:"snippet"`
	UpdatedAt       time.Time `json:"updated_at"`
	LifecycleStatus string    `json:"lifecycle_status,omitempty"`
	LifecycleReason *string   `json:"lifecycle_reason,omitempty"`
	HistoryDeepLink string    `json:"history_deep_link,omitempty"`
}

type ReaderThoughtSearchGroup struct {
	Items      []ReaderThoughtSearchResponse `json:"items"`
	TotalHint  int                           `json:"total_hint"`
	NextCursor string                        `json:"next_cursor,omitempty"`
}

type ReaderNoteSearchResponse struct {
	ID                string    `json:"id"`
	Title             string    `json:"title"`
	Snippet           string    `json:"snippet"`
	PublishedRevision int64     `json:"published_revision"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type ReaderNoteSearchGroup struct {
	Items     []ReaderNoteSearchResponse `json:"items"`
	TotalHint int                        `json:"total_hint"`
}

type GroupedSearchResponse struct {
	Reading  LibrarySearchGroup        `json:"reading"`
	Sites    SiteSearchGroup           `json:"sites"`
	Thoughts *ReaderThoughtSearchGroup `json:"thoughts,omitempty"`
	Notes    *ReaderNoteSearchGroup    `json:"notes,omitempty"`
}

type ClassificationRuleResponse struct {
	ID              string    `json:"id"`
	Host            string    `json:"host"`
	IdentityAdapter *string   `json:"identity_adapter,omitempty"`
	PathPrefix      *string   `json:"path_prefix,omitempty"`
	TargetKind      string    `json:"target_kind"`
	Enabled         bool      `json:"enabled"`
	Revision        int64     `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type LibraryReviewResponse struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	LinkID     *string        `json:"link_id,omitempty"`
	SiteID     *string        `json:"site_id,omitempty"`
	Payload    map[string]any `json:"payload"`
	Status     string         `json:"status"`
	Revision   int64          `json:"revision"`
	CreatedAt  time.Time      `json:"created_at"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty"`
}

type SiteMergePreviewEntryResponse struct {
	ID        string `json:"id"`
	SiteID    string `json:"site_id"`
	LinkID    string `json:"link_id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Duplicate bool   `json:"duplicate"`
}

// SiteMergeFieldConflictResponse is emitted only for competing user-owned
// values. Auto fields are recomputable and never force the user through a
// resolution dialog.
type SiteMergeFieldConflictResponse struct {
	Field        string `json:"field"`
	TargetValue  string `json:"target_value"`
	SourceSiteID string `json:"source_site_id"`
	SourceValue  string `json:"source_value"`
}

type SiteMergePreviewResponse struct {
	TargetSiteID       string                           `json:"target_site_id"`
	TargetRevision     int64                            `json:"target_revision"`
	Entries            []SiteMergePreviewEntryResponse  `json:"entries"`
	UserTags           []string                         `json:"user_tags"`
	IdentityKeys       []string                         `json:"identity_keys"`
	FieldConflicts     []SiteMergeFieldConflictResponse `json:"field_conflicts"`
	RequiresResolution bool                             `json:"requires_resolution"`
}

type SiteMergeExecuteResponse struct {
	SiteID       string `json:"site_id"`
	Revision     int64  `json:"revision"`
	MovedEntries int    `json:"moved_entries"`
	DeletedLinks int    `json:"deleted_duplicate_links"`
}

type SiteSplitPreviewResponse struct {
	SourceSiteID   string                          `json:"source_site_id"`
	SourceRevision int64                           `json:"source_revision"`
	Payload        SiteSplitRequest                `json:"payload"`
	Entries        []SiteMergePreviewEntryResponse `json:"entries"`
	Identities     []SiteSplitIdentityResponse     `json:"identities"`
	UserTags       []string                        `json:"user_tags"`
}

type SiteSplitIdentityResponse struct {
	IdentityKey        string `json:"identity_key"`
	EligibleForNewSite bool   `json:"eligible_for_new_site"`
	Owner              string `json:"owner"`
}

type SiteSplitExecuteResponse struct {
	SourceSiteID    string `json:"source_site_id"`
	SourceRevision  int64  `json:"source_revision"`
	NewSiteID       string `json:"new_site_id"`
	NewSiteRevision int64  `json:"new_site_revision"`
	MovedEntries    int    `json:"moved_entries"`
}

// DomainTreeSummaryResponse is the lightweight domain-level entry returned by
// GET /api/tree?view=domains for the debug domain summary.
type DomainTreeSummaryResponse struct {
	Domain string `json:"domain"`
	Count  int    `json:"count"`
}

// DomainTreeSummaryEnvelope returns visible domain buckets plus the complete
// done-link total. Total includes rows whose domain is null or empty; those
// rows cannot form a selectable domain bucket but still belong to the corpus.
type DomainTreeSummaryEnvelope struct {
	Domains     []DomainTreeSummaryResponse `json:"domains"`
	Total       int                         `json:"total"`
	LibraryKind *string                     `json:"library_kind,omitempty"`
}

// TreeNodeResponse 是按域名 / 路径组织的链接树节点视图，
// 字段与 LinkResponse 对应；额外包含子节点数组和 Truncated 标记。
type TreeNodeResponse struct {
	ID                  string    `json:"id"`
	URL                 string    `json:"url"`
	Title               *string   `json:"title"`
	Summary             *string   `json:"summary"`
	Description         *string   `json:"description"`
	Tags                []string  `json:"tags"`
	ContentType         *string   `json:"content_type"`
	Status              string    `json:"status"`
	Domain              *string   `json:"domain"`
	PathDepth           *int      `json:"path_depth"`
	ParentID            *string   `json:"parent_id"`
	FetcherType         *string   `json:"fetcher_type"`
	IsLowConfidence     bool      `json:"is_low_confidence"`
	LowConfidenceReason *string   `json:"low_confidence_reason"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// Truncated is true when the subtree under this node was clipped at
	// the maxTreeDepth render cap. The actual children exist in the
	// dataset; the response just omits them to keep the JSON bounded.
	// Clients that need the deeper rows should re-query with a domain
	// or parent filter that scopes the second request smaller.
	Truncated bool               `json:"truncated,omitempty"`
	Children  []TreeNodeResponse `json:"children"`
}

// TreeResponse 是链接树接口的整体响应，Total 是命中的根节点总数。
type TreeResponse struct {
	Nodes []TreeNodeResponse `json:"nodes"`
	Total int                `json:"total"`
}
