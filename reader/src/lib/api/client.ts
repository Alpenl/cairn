/**
 * Reader 后端 HTTP 客户端。
 *
 * 基于 fetch + AbortController（超时），把网络异常 / 非 2xx / 响应体解析失败统一
 * 收敛为判别式 ApiResult，不抛异常。Bearer token 从连接配置注入（空则不带）。
 *
 * 纯契约、错误归一化与查询编码来自 @webtag/api；session、CSRF、identity
 * ownership 与缓存协商仍由 Reader adapter 负责。
 *
 * 消费者：连接引导 testConnection（R1）、主界面数据流（R2）、⌘K 搜索（R3）、
 * refresh 触发（R4）。
 */
import {
  buildLinksQuery,
  type ApiResult,
  ok,
} from '@webtag/api'
import { isRecord } from '../records'
import type {
  DiscoverFeedsResponse,
  FeedFolder,
  FeedItem,
  FeedItemAnalyzeResponse,
  FeedItemStatePatch,
  FeedSubscription,
  ContentEditRequest,
  LinkContentResponse,
  LinkCreateRequest,
	ConversionPreviewRequest,
	ConversionPreviewResponse,
	ConversionExecuteRequest,
	ConversionExecuteResponse,
	GroupedSearchResponse,
  DomainTreeSummaryEnvelope,
  LinkResponse,
  ListFeedItemsParams,
  ListLinksParams,
  OPMLImportResponse,
  PaginatedFeedItemsResponse,
  PaginatedLinksResponse,
	SessionIdentity,
	PaginatedSitesResponse,
	SiteDetailResponse,
	SiteEntryDeleteResponse,
	SiteEntryUpdateRequest,
	SiteUpdateRequest,
	SiteMergePreviewRequest,
	SiteMergePreviewResponse,
	SiteMergeExecuteRequest,
	SiteMergeExecuteResponse,
	SiteSplitRequest,
	SiteSplitPreviewResponse,
	SiteSplitExecuteResponse,
	ListSitesParams,
  SubscriptionsResponse,
  SubmitResponse,
  TagCountResponse,
  TranslationCreateRequest,
  TranslationListResponse,
  TranslationResponse,
  ReaderThoughtOpsRequest,
  ReaderThoughtAckResponse,
  ReaderThoughtResponse,
  ReaderThoughtsResponse,
  ReaderThoughtSupersessionEventsResponse,
  ReaderNoteCreateRequest,
  ReaderNoteDraftRequest,
  ReaderNotePublishRequest,
	ReaderNoteRestoreRequest,
  ReaderNoteResponse,
  ReaderNotesResponse,
  ReaderNoteHistoryResponse,
  ReaderHostKind,
  ReaderHostLifecycleResponse,
  ReaderHostPurgeRequest,
  ReaderTrashResponse,
  ReaderInboxCreateRequest,
  ReaderInboxPatchRequest,
  ReaderInboxResponse,
  ReaderInboxResponsePage,
  ReaderInboxPartition,
  ReaderInboxConfirmAIProposalsRequest,
  ReaderInboxBulkRequest,
  ReaderInboxBulkResponse,
  ReaderInboxConfirmAIProposalsResponse,
  ReaderConfirmResponse,
  ReaderTodoCreateRequest,
  ReaderTodoPatchRequest,
  ReaderTodoResponse,
  ReaderTodosResponse,
  ReaderEngagementRequest,
  ReaderEngagementResponse,
  ReaderFeedResponse,
  ReaderFeedFeedbackRequest,
  ReaderFeedFeedbackResponse,
  ReaderHomeResponse,
  ReaderLinkMetadataRequest,
  ReaderLinkMetadataResponse,
  ReaderRelatedTagsResponse,
  ReaderActivityResponse,
  ReaderAIRequest,
  ReaderAIResponse,
  CapabilitiesResponse,
  HealthResponse,
} from './types'
import {
  isReaderThoughtAckResponse,
  isReaderThoughtResponse,
  isReaderThoughtsResponse,
  isReaderThoughtSupersessionEventsResponse,
  isReaderNoteResponse,
  isReaderNotesResponse,
  isReaderNoteHistoryResponse,
  isReaderHostLifecycleResponse,
  isReaderTrashResponse,
  isReaderInboxResponse,
  isReaderInboxResponsePage,
  isReaderInboxBulkResponse,
  isReaderInboxConfirmAIProposalsResponse,
  isReaderConfirmResponse,
  isReaderTodoResponse,
  isReaderTodosResponse,
  isReaderHomeResponse,
  isReaderAIResponse,
  normalizeCapabilitiesResponse,
} from './guards'
import {
  fullArchiveV2Selection,
  type ArchiveV2Selection,
} from './archive-v2'
import {
  ReaderHttpTransport,
  shapeMismatch,
} from './transport'
import {
  buildReaderQuery,
  readerIdempotencyHeaders,
  readerLimit,
  type ReaderFeedSource,
} from './endpoint-helpers'
import * as librarySitesEndpoints from './library-sites-endpoints'
import * as subscriptionsFeedEndpoints from './subscriptions-feed-endpoints'
import type {
  ReaderReadOptions,
  ReaderRequestOptions,
  SessionLoginAttempt,
} from './transport'
import type { IdentityLease, IdentityOwnership } from '../identity'

export {
  DATA_NAMESPACE_HEADER,
  DEFAULT_TIMEOUT,
  isSessionIdentity,
  SESSION_HEADER,
} from './transport'
export type {
  ReaderReadOptions,
  ReaderRequestOptions,
  SessionLoginAttempt,
} from './transport'

/** 客户端构造配置。 */
export interface ReaderClientConfig {
  /** 后端基础地址，如 https://api.example.com 或 http://localhost:8080；末尾斜杠会被裁掉。 */
  baseURL: string
  /** 安装令牌，注入 Authorization: Bearer。空 / 缺省则不带鉴权头。 */
  installationToken?: string
  /**
   * 会话模式：凭证是后端下发的 httpOnly cookie，前端拿不到也存不下。
   * 为 true 时请求带 credentials: 'include' 与 CSRF 自定义头，不带 Authorization。
   *
   * 这是默认且首选的模式。安装令牌模式只对没有会话端点的旧后端，
   * 或 Reader 与后端跨源部署（cookie 送不过去）时作为回退。
   */
  session?: boolean
  /** 单请求超时，缺省 DEFAULT_TIMEOUT。 */
  timeoutMs?: number
  /** Installed authoritative identity. Private API responses are rejected without its marker. */
  identity?: IdentityLease
  /** Called once when a response proves that the browser session changed identity. */
  onIdentityMismatch?: () => void
}

export type IdentityBoundReaderClientConfig = Omit<ReaderClientConfig, 'identity'> & {
  readonly identity: IdentityLease
}

export type { ListLinksParams } from './types'
export { buildLinksQuery }
export { buildFeedItemsQuery } from './endpoint-helpers'
export {
  archiveV2Sections,
  fullArchiveV2Selection,
  type ArchiveV2Selection,
} from './archive-v2'

export type ReaderActivityKind = 'all' | 'tag' | 'domain'

export interface ReaderActivityRequestOptions extends ReaderRequestOptions {
  readonly kind?: ReaderActivityKind
  readonly after?: string
}

export class ReaderClient {
  private readonly transport: ReaderHttpTransport

  constructor(config: ReaderClientConfig) {
    this.transport = new ReaderHttpTransport(config)
  }

  /** The sole identity lease owned by this client, or null for bootstrap-only clients. */
  get identityLease(): IdentityLease | null {
    return this.transport.identityLease
  }

  /** Whether component work started with this client may still commit local side effects. */
  isIdentityCurrent(): boolean {
    return this.transport.isIdentityCurrent()
  }

  /** Capture the immutable lease/epoch owned by this client for a multi-step component action. */
  captureIdentity(logicalKey: string): IdentityOwnership | null {
    return this.transport.captureIdentity(logicalKey)
  }

  /** Fetch the authenticated identity snapshot without consulting any HTTP cache. */
  async getIdentity(signal?: AbortSignal): Promise<ApiResult<SessionIdentity>> {
    return this.transport.getIdentity(signal)
  }

  /**
   * Read the build identity the backend already publishes on `/health`.
   *
   * Deliberately bypasses `send`: the probe is unauthenticated and carries no
   * per-identity data, so requiring a lease would only make "which Core am I
   * talking to" unanswerable exactly when something is wrong. Nothing is sent
   * either — no credential is needed to read a version string.
   */
  async getHealth(signal?: AbortSignal): Promise<ApiResult<HealthResponse>> {
    return this.transport.getHealth(signal)
  }

  /**
   * Read additive server capabilities after identity is established.
   *
   * The endpoint predates Reader vNext on some self-hosted installations. A
   * legacy response is normalized to Reader capability-off rather than being
   * interpreted as support for routes the server does not expose.
   */
  async getCapabilities(signal?: AbortSignal): Promise<ApiResult<CapabilitiesResponse>> {
    const r = await this.send('GET', '/api/capabilities', undefined, undefined, undefined, signal)
    if (!r.ok) return r
    return ok(normalizeCapabilitiesResponse(r.data))
  }

  /** Session negotiation uses this to clean up malformed successful exchanges. */
  async loginWithMutationStatus(
    installationToken: string,
    signal?: AbortSignal,
  ): Promise<SessionLoginAttempt> {
    return this.transport.loginWithMutationStatus(installationToken, signal)
  }

  /** 结束会话：让后端清掉 cookie。失败不阻塞前端登出。 */
  async logout(): Promise<void> {
    return this.transport.logout()
  }

  /**
   * 发一次请求并把结果收敛为 ApiResult<unknown>（已解析 body）。
   * 网络异常 / 超时 / 非 2xx / body 解析失败统一归一化，不抛异常。
   */
  private async send(
    method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE',
    path: string,
    requestBody?: unknown,
    extraHeaders?: Record<string, string>,
    readOptions?: ReaderReadOptions,
    callerSignal?: AbortSignal,
    rawJSONContract?: 'link-metadata-revision',
  ): Promise<ApiResult<unknown>> {
    return this.transport.send(method, path, {
      body: requestBody,
      headers: extraHeaders,
      readOptions,
      signal: callerSignal,
      rawJSONContract,
    })
  }

  /**
   * Download and verify a selected v2 archive. No Blob is constructed until
   * headers, the full byte stream, JSON/schema/count/checksum validation, and
   * the active identity lease have all passed.
   */
  async downloadArchiveV2(
    selection: ArchiveV2Selection = fullArchiveV2Selection,
  ): Promise<ApiResult<Blob>> {
    return this.transport.downloadArchiveV2(selection)
  }

  /** GET /api/links —— 列表 / 搜索（q）/ 存在性检查（url）。 */
  async getLinks(
    params: ListLinksParams = {},
    options?: ReaderReadOptions,
  ): Promise<ApiResult<PaginatedLinksResponse>> {
    return librarySitesEndpoints.getLinks(this.transport, params, options)
  }

  /** POST /api/links -- submit one URL to the parse pipeline. */
  async submitLink(request: LinkCreateRequest): Promise<ApiResult<SubmitResponse>> {
    return librarySitesEndpoints.submitLink(this.transport, request)
  }

  /** GET /api/search —— grouped reading, website, and cursor-paged thought search. */
  async searchLibrary(
    q: string,
    readingLimit = 10,
    siteLimit = 10,
    thoughtLimit = 20,
    thoughtAfter = '',
  ): Promise<ApiResult<GroupedSearchResponse>> {
    return librarySitesEndpoints.searchLibrary(
      this.transport,
      q,
      readingLimit,
      siteLimit,
      thoughtLimit,
      thoughtAfter,
    )
  }

  /** GET /api/links/{id} —— 单条详情；包含按需保存的原文。 */
  /**
   * GET /api/links/{id} —— 单条详情。**不带已保存原文的正文**：Reader 的原文是
   * 默认折叠的，打开一篇文章不该顺带把整篇正文拖过网络。响应里的 has_content
   * 说明有没有原文，正文等用户展开时走 getContent 单独取。
   */
  async getLink(
    id: string,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<LinkResponse>> {
    return librarySitesEndpoints.getLink(this.transport, id, options)
  }

  /** GET /api/links/{id}/content —— 按需读已保存原文（不触发抓取）。 */
  async getContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    return librarySitesEndpoints.getContent(this.transport, id)
  }

  /** PATCH /api/links/{id}/content - replace saved content without fetching. */
  async editContent(
    id: string,
    request: ContentEditRequest,
  ): Promise<ApiResult<LinkContentResponse>> {
    return librarySitesEndpoints.editContent(this.transport, id, request)
  }

	async getSites(params: ListSitesParams = {}): Promise<ApiResult<PaginatedSitesResponse>> {
		return librarySitesEndpoints.getSites(this.transport, params)
	}

	async getSite(id: string): Promise<ApiResult<SiteDetailResponse>> {
		return librarySitesEndpoints.getSite(this.transport, id)
	}

	async updateSite(id: string, revision: number, patch: SiteUpdateRequest): Promise<ApiResult<SiteDetailResponse>> {
		return librarySitesEndpoints.updateSite(this.transport, id, revision, patch)
	}

	async updateSiteEntry(siteID: string, entryID: string, revision: number, patch: SiteEntryUpdateRequest): Promise<ApiResult<SiteDetailResponse>> {
		return librarySitesEndpoints.updateSiteEntry(this.transport, siteID, entryID, revision, patch)
	}

	async setPrimarySiteEntry(siteID: string, entryID: string, revision: number): Promise<ApiResult<SiteDetailResponse>> {
		return librarySitesEndpoints.setPrimarySiteEntry(this.transport, siteID, entryID, revision)
	}

	async deleteSiteEntry(siteID: string, entryID: string, revision: number): Promise<ApiResult<SiteEntryDeleteResponse>> {
		return librarySitesEndpoints.deleteSiteEntry(this.transport, siteID, entryID, revision)
	}

	async deleteSite(id: string, revision: number, entryCount: number): Promise<ApiResult<void>> {
		return librarySitesEndpoints.deleteSite(this.transport, id, revision, entryCount)
	}

	async previewSiteMerge(request: SiteMergePreviewRequest): Promise<ApiResult<SiteMergePreviewResponse>> {
		return librarySitesEndpoints.previewSiteMerge(this.transport, request)
	}

	async executeSiteMerge(request: SiteMergeExecuteRequest): Promise<ApiResult<SiteMergeExecuteResponse>> {
		return librarySitesEndpoints.executeSiteMerge(this.transport, request)
	}

	async previewSiteSplit(siteID: string, request: SiteSplitRequest): Promise<ApiResult<SiteSplitPreviewResponse>> {
		return librarySitesEndpoints.previewSiteSplit(this.transport, siteID, request)
	}

	async executeSiteSplit(siteID: string, request: SiteSplitRequest): Promise<ApiResult<SiteSplitExecuteResponse>> {
		return librarySitesEndpoints.executeSiteSplit(this.transport, siteID, request)
	}

	/** POST /api/links/{id}/conversion-preview — pure conversion consequences. */
	async previewLinkConversion(id: string, request: ConversionPreviewRequest): Promise<ApiResult<ConversionPreviewResponse>> {
		return librarySitesEndpoints.previewLinkConversion(this.transport, id, request)
	}

	/** POST /api/links/{id}/convert — apply a confirmed revision-bound conversion. */
	async convertLink(id: string, request: ConversionExecuteRequest): Promise<ApiResult<ConversionExecuteResponse>> {
		return librarySitesEndpoints.convertLink(this.transport, id, request)
	}

  /** GET /api/tags —— 标签聚合；省略 scope 时返回全部资料库计数。 */
  async getTags(
    scope?: 'reading' | 'site' | 'all',
    options?: ReaderReadOptions,
  ): Promise<ApiResult<TagCountResponse[]>> {
    return librarySitesEndpoints.getTags(this.transport, scope, options)
  }

  /** GET /api/tree?view=domains —— truthful 域名聚合。 */
  async getDomainSummaries(
    scope?: 'reading' | 'site',
    options?: ReaderReadOptions,
  ): Promise<ApiResult<DomainTreeSummaryEnvelope>> {
    return librarySitesEndpoints.getDomainSummaries(this.transport, scope, options)
  }

  /** POST /api/links/{id}/refresh —— 重新入队解析。 */
  async refreshLink(id: string): Promise<ApiResult<SubmitResponse>> {
    return librarySitesEndpoints.refreshLink(this.transport, id)
  }

  /** POST /api/links/{id}/content —— 抓取并保存网页原文，返回保存后的原文。 */
  async saveContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    return librarySitesEndpoints.saveContent(this.transport, id)
  }

  /** PUT /api/links/{id}/content —— 重新抓取并明确替换已保存原文。 */
  async replaceContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    return librarySitesEndpoints.replaceContent(this.transport, id)
  }

  /** GET /api/links/{id}/translations —— 读取数据库中的选段与全文译文。 */
  async getTranslations(
    id: string,
  ): Promise<ApiResult<TranslationListResponse>> {
    return librarySitesEndpoints.getTranslations(this.transport, id)
  }

  /** POST /api/links/{id}/translations —— 创建或复用异步中文翻译。 */
  async createTranslation(
    id: string,
    request: TranslationCreateRequest,
  ): Promise<ApiResult<TranslationResponse>> {
    return librarySitesEndpoints.createTranslation(this.transport, id, request)
  }

  /** GET /api/subscriptions -- RSS navigation metadata and truthful counts. */
  async getSubscriptions(
    url?: string,
    options?: ReaderReadOptions,
  ): Promise<ApiResult<SubscriptionsResponse>> {
    return subscriptionsFeedEndpoints.getSubscriptions(this.transport, url, options)
  }

  /** Resolve a feed URL directly or discover alternate feeds from an HTML page. */
  async discoverFeeds(url: string): Promise<ApiResult<DiscoverFeedsResponse>> {
    return subscriptionsFeedEndpoints.discoverFeeds(this.transport, url)
  }

  async createSubscription(input: {
    url: string
    folder_id?: string | null
  }): Promise<ApiResult<FeedSubscription>> {
    return subscriptionsFeedEndpoints.createSubscription(this.transport, input)
  }

  async updateSubscription(
    id: string,
    patch: { folder_id?: string | null },
  ): Promise<ApiResult<FeedSubscription | null>> {
    return subscriptionsFeedEndpoints.updateSubscription(this.transport, id, patch)
  }

  async deleteSubscription(id: string): Promise<ApiResult<true>> {
    return subscriptionsFeedEndpoints.deleteSubscription(this.transport, id)
  }

  async refreshSubscription(id: string): Promise<ApiResult<true>> {
    return subscriptionsFeedEndpoints.refreshSubscription(this.transport, id)
  }

  async refreshSubscriptions(): Promise<ApiResult<true>> {
    return subscriptionsFeedEndpoints.refreshSubscriptions(this.transport)
  }

  async getFeedItems(
    params: ListFeedItemsParams = {},
    options?: ReaderReadOptions,
  ): Promise<ApiResult<PaginatedFeedItemsResponse>> {
    return subscriptionsFeedEndpoints.getFeedItems(this.transport, params, options)
  }

  async getFeedItem(id: string): Promise<ApiResult<FeedItem>> {
    return subscriptionsFeedEndpoints.getFeedItem(this.transport, id)
  }

  async updateFeedItem(
    id: string,
    patch: FeedItemStatePatch,
  ): Promise<ApiResult<FeedItem | null>> {
    return subscriptionsFeedEndpoints.updateFeedItem(this.transport, id, patch)
  }

  async markFeedItemsRead(filters: ListFeedItemsParams): Promise<ApiResult<true>> {
    return subscriptionsFeedEndpoints.markFeedItemsRead(this.transport, filters)
  }

  async analyzeFeedItem(id: string): Promise<ApiResult<FeedItemAnalyzeResponse>> {
    return subscriptionsFeedEndpoints.analyzeFeedItem(this.transport, id)
  }

  async createFeedFolder(name: string): Promise<ApiResult<FeedFolder>> {
    return subscriptionsFeedEndpoints.createFeedFolder(this.transport, name)
  }

  async updateFeedFolder(id: string, name: string): Promise<ApiResult<FeedFolder | null>> {
    return subscriptionsFeedEndpoints.updateFeedFolder(this.transport, id, name)
  }

  async deleteFeedFolder(id: string): Promise<ApiResult<true>> {
    return subscriptionsFeedEndpoints.deleteFeedFolder(this.transport, id)
  }

  async exportSubscriptionsOPML(): Promise<ApiResult<string>> {
    return subscriptionsFeedEndpoints.exportSubscriptionsOPML(this.transport)
  }

  async importSubscriptionsOPML(opml: string): Promise<ApiResult<OPMLImportResponse>> {
    return subscriptionsFeedEndpoints.importSubscriptionsOPML(this.transport, opml)
  }

  /** POST /api/annotations/ops -- append durable thought operations. */
  async pushThoughtOps(
    request: ReaderThoughtOpsRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderThoughtAckResponse[]>> {
    const r = await this.send('POST', '/api/annotations/ops', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    if (!isRecord(r.data) || !Array.isArray(r.data.items) || !r.data.items.every(isReaderThoughtAckResponse)) {
      return shapeMismatch('ReaderThoughtAckResponse[]')
    }
    return ok(r.data.items)
  }

  /** GET /api/annotations -- list materialized durable thoughts. */
  async listThoughts(
    params: { q?: string; after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderThoughtsResponse>> {
    const query = buildReaderQuery({
      q: params.q,
      after: params.after,
      limit: readerLimit(params.limit, 50),
    })
    const r = await this.send('GET', `/api/annotations${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
  }

  /** GET /api/annotations/sync -- replay server-ordered thoughts, including tombstones. */
  async syncThoughts(
    params: { after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderThoughtsResponse>> {
    const query = buildReaderQuery({
      after: params.after,
      limit: readerLimit(params.limit, 100),
    })
    const r = await this.send('GET', `/api/annotations/sync${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
  }

  /** GET /api/annotations/conflicts -- immutable superseded-version event log. */
  async listThoughtSupersessions(
    params: { after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderThoughtSupersessionEventsResponse>> {
    const query = buildReaderQuery({
      after: params.after,
      limit: readerLimit(params.limit, 100),
    })
    const r = await this.send(
      'GET',
      `/api/annotations/conflicts${query}`,
      undefined,
      undefined,
      undefined,
      options.signal,
    )
    if (!r.ok) return r
    return isReaderThoughtSupersessionEventsResponse(r.data)
      ? ok(r.data)
      : shapeMismatch('ReaderThoughtSupersessionEventsResponse')
  }

  /** GET /api/annotations/history -- list thoughts whose host is tombstoned. */
  async listThoughtHistory(
    params: { after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderThoughtsResponse>> {
    const query = buildReaderQuery({ after: params.after, limit: readerLimit(params.limit, 30) })
    const r = await this.send('GET', `/api/annotations/history${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
  }

  /** GET /api/annotations/{id} -- read one durable thought. */
  async getThought(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderThoughtResponse>> {
    const r = await this.send('GET', `/api/annotations/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderThoughtResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtResponse')
  }

  /** POST /api/notes -- create a note with an optional local draft. */
  async createNote(
    request: ReaderNoteCreateRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNoteResponse>> {
    const r = await this.send('POST', '/api/notes', request, readerIdempotencyHeaders(options), undefined, options.signal)
    if (!r.ok) return r
    return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
  }

  /** GET /api/notes -- list notes using the opaque server cursor. */
  async listNotes(
    params: { after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNotesResponse>> {
    const query = buildReaderQuery({ after: params.after, limit: readerLimit(params.limit, 30) })
    const r = await this.send('GET', `/api/notes${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderNotesResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNotesResponse')
  }

  async getNote(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderNoteResponse>> {
    const r = await this.send('GET', `/api/notes/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
  }

  /** PATCH /api/notes/{id}/draft -- optimistic draft save. */
  async saveNoteDraft(
    id: string,
    request: ReaderNoteDraftRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNoteResponse>> {
    const r = await this.send('PATCH', `/api/notes/${encodeURIComponent(id)}/draft`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
  }

  /** DELETE /api/notes/{id}/draft -- clear the unpublished draft with CAS. */
  async discardNoteDraft(
    id: string,
    expectedDraftRevision: number,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<true>> {
    const r = await this.send(
      'DELETE',
      `/api/notes/${encodeURIComponent(id)}/draft`,
      undefined,
      { 'If-Match': `"${expectedDraftRevision}"` },
      undefined,
      options.signal,
    )
    return r.ok ? ok(true) : r
  }

  /** POST /api/notes/{id}/publish -- publish the current draft with both CAS guards. */
  async publishNote(
    id: string,
    request: ReaderNotePublishRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNoteResponse>> {
    const r = await this.send('POST', `/api/notes/${encodeURIComponent(id)}/publish`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
  }

  async deleteNote(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderHostLifecycleResponse>> {
    const r = await this.send('DELETE', `/api/notes/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
  }

  async restoreNote(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderHostLifecycleResponse>> {
    const r = await this.send('POST', `/api/notes/${encodeURIComponent(id)}/restore`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
  }

  async listTrash(
    params: { hostKind?: ReaderHostKind; after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderTrashResponse>> {
    const query = buildReaderQuery({
      host_kind: params.hostKind,
      after: params.after,
      limit: readerLimit(params.limit, 50),
    })
    const r = await this.send('GET', `/api/trash${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderTrashResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTrashResponse')
  }

  /**
   * 把链接移入回收站。
   *
   * 后端是软删（置 deleted_at），条目随后出现在 /api/trash，可用 restoreLink
   * 撤销、或用 purgeHost('link', ...) 彻底清除。所以调用方给一次撤销机会就够，
   * 不必在删除前弹确认框——真正不可逆的是 purge，不是这里。
   */
  async deleteLink(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<true>> {
    const r = await this.send('DELETE', `/api/links/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    return r.ok ? ok(true) : r
  }

  async restoreLink(
    id: string,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderHostLifecycleResponse>> {
    const r = await this.send('POST', `/api/links/${encodeURIComponent(id)}/restore`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
  }

  async purgeHost(
    kind: ReaderHostKind,
    id: string,
    operationID: string,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<true>> {
    const collection = kind === 'link' ? 'links' : kind === 'note' ? 'notes' : 'inbox'
    const request: ReaderHostPurgeRequest = { operation_id: operationID }
    const r = await this.send(
      'DELETE',
      `/api/${collection}/${encodeURIComponent(id)}/purge`,
      request,
      undefined,
      undefined,
      options.signal,
    )
    return r.ok ? ok(true) : r
  }

  async listNoteHistory(
    id: string,
    limit = 50,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNoteHistoryResponse[]>> {
    const query = buildReaderQuery({ limit: readerLimit(limit, 50) })
    const r = await this.send('GET', `/api/notes/${encodeURIComponent(id)}/history${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    if (!isRecord(r.data) || !Array.isArray(r.data.items) || !r.data.items.every(isReaderNoteHistoryResponse)) {
      return shapeMismatch('ReaderNoteHistoryResponse[]')
    }
    return ok(r.data.items)
  }

  async restoreNoteRevision(
    id: string,
    revision: number,
	request: ReaderNoteRestoreRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderNoteResponse>> {
    const r = await this.send('POST', `/api/notes/${encodeURIComponent(id)}/history/${encodeURIComponent(String(revision))}/restore`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
  }

  /** POST /api/inbox -- create a capture that remains pending until confirmed. */
  async createInbox(
    request: ReaderInboxCreateRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxResponse>> {
    const r = await this.send('POST', '/api/inbox', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
  }

  async listInbox(
    params: { partition?: ReaderInboxPartition; after?: string; limit?: number } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxResponsePage>> {
    const query = buildReaderQuery({ partition: params.partition, after: params.after, limit: readerLimit(params.limit, 50) })
    const r = await this.send('GET', `/api/inbox${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxResponsePage(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponsePage')
  }

  async getInbox(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderInboxResponse>> {
    const r = await this.send('GET', `/api/inbox/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
  }

  /** PATCH /api/inbox/{id} -- metadata CAS uses the ETag revision. */
  async patchInbox(
    id: string,
    revision: number,
    request: ReaderInboxPatchRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxResponse>> {
    const r = await this.send(
      'PATCH',
      `/api/inbox/${encodeURIComponent(id)}`,
      request,
      { 'If-Match': `"${revision}"` },
      undefined,
      options.signal,
    )
    if (!r.ok) return r
    return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
  }

  async confirmInbox(id: string, revision?: number, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderConfirmResponse>> {
	const r = await this.send('POST', `/api/inbox/${encodeURIComponent(id)}/confirm`, undefined, { ...readerIdempotencyHeaders(options), ...(revision === undefined ? {} : { 'If-Match': `"${revision}"` }) }, undefined, options.signal)
    if (!r.ok) return r
    return isReaderConfirmResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderConfirmResponse')
  }

  async restoreInbox(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<true>> {
	const r = await this.send('POST', `/api/inbox/${encodeURIComponent(id)}/restore`, undefined, readerIdempotencyHeaders(options), undefined, options.signal)
    return r.ok ? ok(true) : r
  }

  async confirmInboxBulk(
    request: ReaderInboxBulkRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxBulkResponse>> {
    const r = await this.send('POST', '/api/inbox/bulk/confirm', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxBulkResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxBulkResponse')
  }

  async confirmAIProposals(
    request: ReaderInboxConfirmAIProposalsRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxConfirmAIProposalsResponse>> {
    const r = await this.send('POST', '/api/inbox/confirm-ai-proposals', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxConfirmAIProposalsResponse(r.data)
      ? ok(r.data)
      : shapeMismatch('ReaderInboxConfirmAIProposalsResponse')
  }

  async discardInboxBulk(
    request: ReaderInboxBulkRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderInboxBulkResponse>> {
    const r = await this.send('POST', '/api/inbox/bulk/discard', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderInboxBulkResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxBulkResponse')
  }

  async discardInbox(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<true>> {
	const r = await this.send('POST', `/api/inbox/${encodeURIComponent(id)}/discard`, undefined, readerIdempotencyHeaders(options), undefined, options.signal)
    return r.ok ? ok(true) : r
  }

	async resummarizeInbox(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderInboxResponse>> {
		const r = await this.send('POST', `/api/inbox/${encodeURIComponent(id)}/resummarize`, undefined, readerIdempotencyHeaders(options), undefined, options.signal)
		if (!r.ok) return r
		return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
	}

  async createTodo(
    request: ReaderTodoCreateRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderTodoResponse>> {
    const r = await this.send('POST', '/api/todos', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderTodoResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTodoResponse')
  }

  async listTodos(options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderTodosResponse>> {
    const items: ReaderTodoResponse[] = []
    const seen = new Set<string>()
    let after: string | undefined
    for (let pageIndex = 0; pageIndex < 100; pageIndex += 1) {
      const path = `/api/todos${buildReaderQuery({ limit: 200, after })}`
      const r = await this.send('GET', path, undefined, undefined, undefined, options.signal)
      if (!r.ok) return r
      if (!isReaderTodosResponse(r.data)) return shapeMismatch('ReaderTodosResponse')
      items.push(...r.data.items)
      const next = r.data.next_after?.trim()
      if (!next) {
        const response = { ...r.data, items }
        delete response.next_after
        return ok(response)
      }
      if (seen.has(next)) return shapeMismatch('ReaderTodosResponse cursor')
      seen.add(next)
      after = next
    }
    return shapeMismatch('ReaderTodosResponse page limit')
  }

  async patchTodo(
    id: string,
    request: ReaderTodoPatchRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderTodoResponse>> {
    const r = await this.send('PATCH', `/api/todos/${encodeURIComponent(id)}`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderTodoResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTodoResponse')
  }

  async deleteTodo(id: string, options: ReaderRequestOptions = {}): Promise<ApiResult<true>> {
    const r = await this.send('DELETE', `/api/todos/${encodeURIComponent(id)}`, undefined, undefined, undefined, options.signal)
    return r.ok ? ok(true) : r
  }

  async getEngagement(linkID: string, options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderEngagementResponse>> {
    return librarySitesEndpoints.getEngagement(this.transport, linkID, options)
  }

  async patchEngagement(
    linkID: string,
    request: ReaderEngagementRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderEngagementResponse>> {
    return librarySitesEndpoints.patchEngagement(this.transport, linkID, request, options)
  }

  async getHome(options: ReaderRequestOptions = {}): Promise<ApiResult<ReaderHomeResponse>> {
    const r = await this.send('GET', '/api/home', undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderHomeResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHomeResponse')
  }

  async getReaderFeed(
    params: {
      mode?: 'recommended' | 'chronological'
      source?: readonly ReaderFeedSource[]
      after?: string
      limit?: number
    } = {},
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderFeedResponse>> {
    return subscriptionsFeedEndpoints.getReaderFeed(this.transport, params, options)
  }

  async sendReaderFeedFeedback(
    itemKey: string,
    request: ReaderFeedFeedbackRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderFeedFeedbackResponse>> {
    return subscriptionsFeedEndpoints.sendReaderFeedFeedback(this.transport, itemKey, request, options)
  }

  async patchLinkMetadata(
    linkID: string,
    revision: number,
    request: ReaderLinkMetadataRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderLinkMetadataResponse>> {
    return librarySitesEndpoints.patchLinkMetadata(this.transport, linkID, revision, request, options)
  }

  async getRelatedTags(
    linkID?: string,
    limit = 12,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderRelatedTagsResponse>> {
    return librarySitesEndpoints.getRelatedTags(this.transport, linkID, limit, options)
  }

  async getReaderActivity(
    limit = 100,
    options: ReaderActivityRequestOptions = {},
  ): Promise<ApiResult<ReaderActivityResponse>> {
    return librarySitesEndpoints.getReaderActivity(this.transport, limit, options)
  }

  async completeReaderAI(
    request: ReaderAIRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderAIResponse>> {
    const r = await this.send('POST', '/api/ai', request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderAIResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderAIResponse')
  }

  /**
   * 测试连接：执行严格、无缓存的 identity handshake，不读取任何私有集合。
   * 成功 → ok；401/403 → unauthorized；不可达 → network-unreachable。
   */
  async testConnection(): Promise<ApiResult<true>> {
    const r = await this.getIdentity()
    if (!r.ok) return r
    return ok(true)
  }
}

/**
 * Private Reader data crosses the network/cache seam through one identity-bound client.
 * Bootstrap clients remain plain ReaderClient instances because no lease exists yet.
 */
export class IdentityBoundReaderClient extends ReaderClient {
  constructor(config: IdentityBoundReaderClientConfig) {
    super(config)
  }

  override get identityLease(): IdentityLease {
    const identity = super.identityLease
    if (!identity) throw new Error('IdentityBoundReaderClient requires an identity lease')
    return identity
  }
}
