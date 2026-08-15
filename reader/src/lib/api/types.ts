/**
 * Reader-facing names for wire shapes generated from the authoritative
 * OpenAPI document. Runtime response validation remains in guards.ts.
 */
import type {
  paths as WirePaths,
  DomainTreeSummaryEnvelope as WireDomainTreeSummaryEnvelope,
  DomainTreeSummaryResponse as WireDomainTreeSummaryResponse,
  ErrorDetail as WireErrorDetail,
  ErrorResponse as WireErrorResponse,
  ContentEditRequest as WireContentEditRequest,
  LinkContentResponse as WireLinkContentResponse,
  LinkCreateRequest as WireLinkCreateRequest,
	ConversionPreviewRequest as WireConversionPreviewRequest,
	ConversionPreviewResponse as WireConversionPreviewResponse,
	ConversionExecuteRequest as WireConversionExecuteRequest,
	ConversionExecuteResponse as WireConversionExecuteResponse,
	ClassificationRuleCreateRequest as WireClassificationRuleCreateRequest,
	ClassificationRuleUpdateRequest as WireClassificationRuleUpdateRequest,
	ClassificationRuleResponse as WireClassificationRuleResponse,
	LibraryReviewResolveRequest as WireLibraryReviewResolveRequest,
	LibraryReviewResponse as WireLibraryReviewResponse,
	GroupedSearchResponse as WireGroupedSearchResponse,
	ReaderThoughtSearchResponse as WireReaderThoughtSearchResponse,
	ReaderNoteSearchResponse as WireReaderNoteSearchResponse,
  CapabilitiesResponse as WireCapabilitiesResponse,
  ReaderCapabilitiesResponse as WireReaderCapabilitiesResponse,
  LinkResponse as WireLinkResponse,
  OpmlImportResponse as WireOPMLImportResponse,
  PaginatedLinksResponse as WirePaginatedLinksResponse,
	SessionIdentity as WireSessionIdentity,
	PaginatedSitesResponse as WirePaginatedSitesResponse,
	SiteDetailResponse as WireSiteDetailResponse,
	SiteEntryDeleteResponse as WireSiteEntryDeleteResponse,
	SiteEntryResponse as WireSiteEntryResponse,
	SiteEntryUpdateRequest as WireSiteEntryUpdateRequest,
	SiteListItemResponse as WireSiteListItemResponse,
	SiteUpdateRequest as WireSiteUpdateRequest,
	SiteMergePreviewRequest as WireSiteMergePreviewRequest,
	SiteMergePreviewResponse as WireSiteMergePreviewResponse,
	SiteMergeExecuteRequest as WireSiteMergeExecuteRequest,
	SiteMergeExecuteResponse as WireSiteMergeExecuteResponse,
	SiteSplitRequest as WireSiteSplitRequest,
	SiteSplitPreviewResponse as WireSiteSplitPreviewResponse,
	SiteSplitExecuteResponse as WireSiteSplitExecuteResponse,
  SubmitResponse as WireSubmitResponse,
  TagCountResponse as WireTagCountResponse,
  TranslationCreateRequest as WireTranslationCreateRequest,
  TranslationListResponse as WireTranslationListResponse,
  TranslationResponse as WireTranslationResponse,
  TranslationSourceIdentity as WireTranslationSourceIdentity,
  ReaderThoughtOpRequest as WireReaderThoughtOpRequest,
  ReaderThoughtOpsRequest as WireReaderThoughtOpsRequest,
  ReaderThoughtAckResponse as WireReaderThoughtAckResponse,
  ReaderThoughtResponse as WireReaderThoughtResponse,
  ReaderThoughtsResponse as WireReaderThoughtsResponse,
  ReaderThoughtConflictOperationResponse as WireReaderThoughtSupersessionOperationResponse,
  ReaderThoughtConflictResponse as WireReaderThoughtSupersessionEventResponse,
  ReaderThoughtConflictsResponse as WireReaderThoughtSupersessionEventsResponse,
  ReaderThoughtReattachRequest as WireReaderThoughtReattachRequest,
  ReaderNoteCreateRequest as WireReaderNoteCreateRequest,
  ReaderNoteDraftRequest as WireReaderNoteDraftRequest,
  ReaderNotePublishRequest as WireReaderNotePublishRequest,
	ReaderNoteRestoreRequest as WireReaderNoteRestoreRequest,
  ReaderNoteResponse as WireReaderNoteResponse,
  ReaderNotesResponse as WireReaderNotesResponse,
  ReaderNoteHistoryResponse as WireReaderNoteHistoryResponse,
  ReaderHostLifecycleResponse as WireReaderHostLifecycleResponse,
  ReaderHostPurgeRequest as WireReaderHostPurgeRequest,
  ReaderTrashItemResponse as WireReaderTrashItemResponse,
  ReaderTrashResponse as WireReaderTrashResponse,
  ReaderInboxCreateRequest as WireReaderInboxCreateRequest,
  ReaderInboxPatchRequest as WireReaderInboxPatchRequest,
  ReaderInboxResponse as WireReaderInboxResponse,
  ReaderInboxResponsePage as WireReaderInboxResponsePage,
  ReaderInboxConfirmAiProposalsRequest as WireReaderInboxConfirmAIProposalsRequest,
  ReaderInboxBulkRequest as WireReaderInboxBulkRequest,
  ReaderInboxBulkItemResponse as WireReaderInboxBulkItemResponse,
  ReaderInboxBulkResponse as WireReaderInboxBulkResponse,
  ReaderInboxConfirmAiProposalsResponse as WireReaderInboxConfirmAIProposalsResponse,
  ReaderInboxJobResponse as WireReaderInboxJobResponse,
  ReaderConfirmResponse as WireReaderConfirmResponse,
  ReaderCategoryRequest as WireReaderCategoryRequest,
  ReaderCategoryResponse as WireReaderCategoryResponse,
  ReaderCategoriesResponse as WireReaderCategoriesResponse,
  ReaderCategoryMembershipRequest as WireReaderCategoryMembershipRequest,
  ReaderTodoCreateRequest as WireReaderTodoCreateRequest,
  ReaderTodoPatchRequest as WireReaderTodoPatchRequest,
  ReaderTodoResponse as WireReaderTodoResponse,
  ReaderTodosResponse as WireReaderTodosResponse,
  ReaderEngagementRequest as WireReaderEngagementRequest,
  ReaderEngagementResponse as WireReaderEngagementResponse,
  ReaderFeedAction as WireReaderFeedAction,
  ReaderFeedResponse as WireReaderFeedResponse,
  ReaderFeedSectionResponse as WireReaderFeedSectionResponse,
  ReaderFeedSourceResponse as WireReaderFeedSourceResponse,
  ReaderFeedFeedbackRequest as WireReaderFeedFeedbackRequest,
  ReaderFeedFeedbackResponse as WireReaderFeedFeedbackResponse,
  ReaderHomeResponse as WireReaderHomeResponse,
  ReaderLinkMetadataRequest as WireReaderLinkMetadataRequest,
  ReaderLinkMetadataResponse as WireReaderLinkMetadataResponse,
  ReaderContentHistoryResponse as WireReaderContentHistoryResponse,
  ReaderContentHistoryRestoreRequest as WireReaderContentHistoryRestoreRequest,
  ReaderContentHistoryRestoreResponse as WireReaderContentHistoryRestoreResponse,
  ReaderRelatedTagsResponse as WireReaderRelatedTagsResponse,
  ReaderActivityResponse as WireReaderActivityResponse,
  ReaderTagActivityResponse as WireReaderTagActivityResponse,
  ReaderDomainActivityResponse as WireReaderDomainActivityResponse,
  ReaderAiRequest as WireReaderAiRequest,
  ReaderAiResponse as WireReaderAiResponse,
} from '@webtag/api/generated'

export type ListLinksParams = NonNullable<
  WirePaths['/api/links']['get']['parameters']['query']
>

export type LinkResponse = WireLinkResponse
export type LinkStatus = LinkResponse['status']
export type ContentSource = NonNullable<LinkResponse['content_source']>
export type LinkContentResponse = WireLinkContentResponse
export type ContentEditRequest = WireContentEditRequest
export type LinkCreateRequest = WireLinkCreateRequest
export type ConversionPreviewRequest = WireConversionPreviewRequest
export type ConversionPreviewResponse = WireConversionPreviewResponse
export type ConversionExecuteRequest = WireConversionExecuteRequest
export type ConversionExecuteResponse = WireConversionExecuteResponse
export type ClassificationRuleCreateRequest = WireClassificationRuleCreateRequest
export type ClassificationRuleUpdateRequest = WireClassificationRuleUpdateRequest
export type ClassificationRuleResponse = WireClassificationRuleResponse
export type LibraryReviewResolveRequest = WireLibraryReviewResolveRequest
export type LibraryReviewResponse = WireLibraryReviewResponse
export type GroupedSearchResponse = WireGroupedSearchResponse
export type LibrarySearchGroup = WireGroupedSearchResponse['reading']
export type SiteSearchGroup = WireGroupedSearchResponse['sites']
export type SiteSearchResultResponse = WireGroupedSearchResponse['sites']['items'][number]
export type SiteSearchEntryResponse = SiteSearchResultResponse['matched_entries'][number]
export type ReaderThoughtSearchResponse = WireReaderThoughtSearchResponse
export type ReaderNoteSearchResponse = WireReaderNoteSearchResponse
export type SubmitResponse = WireSubmitResponse
export type PaginatedLinksResponse = WirePaginatedLinksResponse
export type SessionIdentity = WireSessionIdentity
export type SiteListItemResponse = WireSiteListItemResponse
export type SiteDetailResponse = WireSiteDetailResponse
export type PaginatedSitesResponse = WirePaginatedSitesResponse
export type SiteUpdateRequest = WireSiteUpdateRequest
export type SiteEntryUpdateRequest = WireSiteEntryUpdateRequest
export type SiteEntryDeleteResponse = WireSiteEntryDeleteResponse
export type SiteEntryResponse = WireSiteEntryResponse
export type SiteMergePreviewRequest = WireSiteMergePreviewRequest
export type SiteMergePreviewResponse = WireSiteMergePreviewResponse
export type SiteMergeExecuteRequest = WireSiteMergeExecuteRequest
export type SiteMergeExecuteResponse = WireSiteMergeExecuteResponse
export type SiteSplitRequest = WireSiteSplitRequest
export type SiteSplitPreviewResponse = WireSiteSplitPreviewResponse
export type SiteSplitExecuteResponse = WireSiteSplitExecuteResponse
export interface ListSitesParams {
  view?: 'all' | 'pinned' | 'recent' | 'review'
  tags?: string
  recentCutoff?: string
  page?: number
  limit?: number
}
export type TagCountResponse = WireTagCountResponse
export type DomainTreeSummaryResponse = WireDomainTreeSummaryResponse
export type DomainTreeSummaryEnvelope = WireDomainTreeSummaryEnvelope
export type TranslationCreateRequest = WireTranslationCreateRequest
export type TranslationResponse = WireTranslationResponse
export type TranslationListResponse = WireTranslationListResponse
export type TranslationSourceIdentity = WireTranslationSourceIdentity
export type ErrorDetail = WireErrorDetail
export type ErrorResponse = WireErrorResponse
export type OPMLImportResponse = WireOPMLImportResponse
export type CapabilitiesResponse = WireCapabilitiesResponse
export type ReaderCapabilitiesResponse = WireReaderCapabilitiesResponse

export type ReaderThoughtOpRequest = WireReaderThoughtOpRequest
export type ReaderThoughtOpsRequest = WireReaderThoughtOpsRequest
export type ReaderThoughtAckResponse = WireReaderThoughtAckResponse
export type ReaderThoughtResponse = WireReaderThoughtResponse
export type ReaderThoughtsResponse = WireReaderThoughtsResponse
export type ReaderThoughtSupersessionOperationResponse = WireReaderThoughtSupersessionOperationResponse
export type ReaderThoughtSupersessionEventResponse = WireReaderThoughtSupersessionEventResponse
export type ReaderThoughtSupersessionEventsResponse = WireReaderThoughtSupersessionEventsResponse
export type ReaderThoughtReattachRequest = WireReaderThoughtReattachRequest
export type ReaderNoteCreateRequest = WireReaderNoteCreateRequest
export type ReaderNoteDraftRequest = WireReaderNoteDraftRequest
export type ReaderNotePublishRequest = WireReaderNotePublishRequest
export type ReaderNoteRestoreRequest = WireReaderNoteRestoreRequest
export type ReaderNoteResponse = WireReaderNoteResponse
export type ReaderNotesResponse = WireReaderNotesResponse
export type ReaderNoteHistoryResponse = WireReaderNoteHistoryResponse
export type ReaderHostLifecycleResponse = WireReaderHostLifecycleResponse
export type ReaderHostKind = ReaderHostLifecycleResponse['host_kind']
export type ReaderHostPurgeRequest = WireReaderHostPurgeRequest
export type ReaderTrashItemResponse = WireReaderTrashItemResponse
export type ReaderTrashResponse = WireReaderTrashResponse
export type ReaderInboxCreateRequest = WireReaderInboxCreateRequest
export type ReaderInboxPatchRequest = WireReaderInboxPatchRequest
export type ReaderInboxResponse = WireReaderInboxResponse
export type ReaderInboxResponsePage = WireReaderInboxResponsePage
export type ReaderInboxPartition = WireReaderInboxConfirmAIProposalsRequest['partition']
export type ReaderInboxConfirmAIProposalsRequest = WireReaderInboxConfirmAIProposalsRequest
export type ReaderInboxBulkRequest = WireReaderInboxBulkRequest
export type ReaderInboxBulkItemResponse = WireReaderInboxBulkItemResponse
export type ReaderInboxBulkResponse = WireReaderInboxBulkResponse
export type ReaderInboxConfirmAIProposalsResponse = WireReaderInboxConfirmAIProposalsResponse
export type ReaderInboxJobResponse = WireReaderInboxJobResponse
export type ReaderConfirmResponse = WireReaderConfirmResponse
export type ReaderCategoryRequest = WireReaderCategoryRequest
export type ReaderCategoryResponse = WireReaderCategoryResponse
export type ReaderCategoriesResponse = WireReaderCategoriesResponse
export type ReaderCategoryMembershipRequest = WireReaderCategoryMembershipRequest
export type ReaderTodoCreateRequest = WireReaderTodoCreateRequest
export type ReaderTodoPatchRequest = WireReaderTodoPatchRequest
export type ReaderTodoResponse = WireReaderTodoResponse
export type ReaderResponseFreshness = 'unknown' | 'fresh' | 'partial' | 'stale'
type ReaderHomeTodoResponseMetadata = {
  freshness?: ReaderResponseFreshness
  partial?: boolean
}
export type ReaderTodosResponse = WireReaderTodosResponse & ReaderHomeTodoResponseMetadata
export type ReaderEngagementRequest = WireReaderEngagementRequest
export type ReaderEngagementResponse = WireReaderEngagementResponse
export type ReaderFeedAction = WireReaderFeedAction
// /api/reader-feed returns the ranked item intersection. Home intentionally
// uses the base ReaderFeedItemResponse generated type through ReaderHomeResponse.
export type ReaderFeedItemResponse = WireReaderFeedResponse['items'][number]
export type ReaderFeedResponse = WireReaderFeedResponse
export type ReaderFeedSectionResponse = WireReaderFeedSectionResponse
export type ReaderFeedSourceResponse = WireReaderFeedSourceResponse
export type ReaderFeedFeedbackRequest = WireReaderFeedFeedbackRequest
export type ReaderFeedFeedbackResponse = WireReaderFeedFeedbackResponse
export type ReaderHomeResponse = WireReaderHomeResponse & ReaderHomeTodoResponseMetadata
export type ReaderHomeFreshness = NonNullable<WireReaderHomeResponse['freshness']>
export type ReaderLinkMetadataRequest = WireReaderLinkMetadataRequest
export type ReaderLinkMetadataResponse = WireReaderLinkMetadataResponse
export type ReaderContentHistoryResponse = WireReaderContentHistoryResponse
export type ReaderContentHistoryRestoreRequest = WireReaderContentHistoryRestoreRequest
export type ReaderContentHistoryRestoreResponse = WireReaderContentHistoryRestoreResponse
export type ReaderRelatedTagsResponse = WireReaderRelatedTagsResponse
export type ReaderActivityResponse = WireReaderActivityResponse
export type ReaderTagActivityResponse = WireReaderTagActivityResponse
export type ReaderDomainActivityResponse = WireReaderDomainActivityResponse
export type ReaderAIRequest = WireReaderAiRequest
export type ReaderAIResponse = WireReaderAiResponse

/**
 * Reader-tolerant RSS view types. Exact wire types now live in generated.ts;
 * these keep compatibility aliases accepted by the runtime guards.
 */
export type FeedView = 'all' | 'unread' | 'starred' | 'later'

export interface FeedFolder {
  id: string
  name: string
  created_at?: string
  updated_at?: string
}

export interface FeedSubscription {
  id: string
  /** Canonical feed URL. `url` is accepted for compatibility with early API builds. */
  feed_url?: string
  url?: string
  site_url?: string | null
  title?: string | null
  name?: string | null
  description?: string | null
  folder_id?: string | null
  unread_count?: number
  item_count?: number
  active?: boolean
  refreshing?: boolean
  last_success_at?: string | null
  last_fetched_at?: string | null
  next_fetch_at?: string | null
  last_error?: string | null
  fetch_error?: string | null
  failure_count?: number
  created_at?: string
  updated_at?: string
}

export interface FeedCounts {
  all: number
  unread: number
  starred: number
  later: number
}

export interface SubscriptionsResponse {
  folders: FeedFolder[]
  subscriptions: FeedSubscription[]
  counts: FeedCounts
}

export interface DiscoveredFeed {
  feed_url?: string
  url?: string
  title?: string | null
  type?: string | null
}

export interface DiscoverFeedsResponse {
  feeds: DiscoveredFeed[]
}

export type FeedAnalysisStatus = 'none' | 'pending' | 'processing' | 'done' | 'failed'

export interface FeedItem {
  id: string
  subscription_id: string
  /** Stable source label, including when the subscription is no longer active. */
  subscription_title?: string
  title: string
  url: string
  guid?: string | null
  author?: string | null
  summary?: string | null
  content?: string | null
  content_html?: string | null
  published_at?: string | null
  created_at?: string
  updated_at?: string
  read_at?: string | null
  starred_at?: string | null
  read_later_at?: string | null
  /** Boolean aliases accepted from early API builds. */
  read?: boolean
  starred?: boolean
  read_later?: boolean
  link_id?: string | null
  analysis_status?: FeedAnalysisStatus
  link_status?: FeedAnalysisStatus
  analysis_error?: string | null
}

export interface PaginatedFeedItemsResponse {
  items: FeedItem[]
  total: number
  page: number
  limit: number
}

export interface ListFeedItemsParams {
  view?: FeedView
  subscription_id?: string
  folder_id?: string
  q?: string
  page?: number
  limit?: number
}

export interface FeedItemStatePatch {
  read?: boolean
  starred?: boolean
  read_later?: boolean
}

export interface FeedItemAnalyzeResponse {
  item?: FeedItem
  link_id?: string
  status?: FeedAnalysisStatus
}
