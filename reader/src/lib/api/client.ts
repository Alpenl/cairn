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
  buildQueryString,
  normalizeHttpError as normalizeSharedHttpError,
  normalizeThrownError,
  parseRetryAfter as parseRetryAfterValue,
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
  isDiscoverFeedsResponse,
  isDomainTreeSummaryEnvelope,
  isFeedFolder,
  isFeedItem,
  isFeedItemAnalyzeResponse,
  isFeedSubscription,
  isLinkContentResponse,
	isConversionPreviewResponse,
	isConversionExecuteResponse,
	isGroupedSearchResponse,
  isHealthResponse,
  isLinkResponse,
  isOPMLImportResponse,
  isPaginatedFeedItems,
  isPaginatedLinks,
	isPaginatedSites,
	isSiteDetail,
	isSiteEntryDeleteResponse,
	isSiteMergePreviewResponse,
	isSiteMergeExecuteResponse,
	isSiteSplitPreviewResponse,
	isSiteSplitExecuteResponse,
  isSubscriptionsResponse,
  isSubmitResponse,
  isTagCountArray,
  isTranslationListResponse,
  isTranslationResponse,
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
  isReaderEngagementResponse,
  isReaderFeedFeedbackResponse,
  isReaderFeedResponse,
  isReaderHomeResponse,
  isReaderLinkMetadataResponse,
  hasCanonicalSafeLinkMetadataRevisionTokens,
  isReaderRelatedTagsResponse,
  isReaderActivityResponse,
  isReaderAIResponse,
  normalizeCapabilitiesResponse,
} from './guards'
import {
  ARCHIVE_V2_MAX_BYTES,
  ARCHIVE_V2_MIME_TYPE,
  ARCHIVE_V2_TOO_LARGE_ERROR_CODE,
  ARCHIVE_V2_VALIDATION_ERROR_CODE,
  ArchiveV2ValidationError,
  archiveV2Sections,
  fullArchiveV2Selection,
  isArchiveV2ByteArray,
  validateArchiveV2Bytes,
  type ArchiveV2Selection,
} from './archive-v2'
import { err, ok, type ApiError, type ApiResult } from '@webtag/api'
import type { IdentityLease, IdentityOperationContext, IdentityOwnership } from '../identity'

/** 默认请求超时（毫秒）。 */
export const DEFAULT_TIMEOUT = 15000

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

/**
 * 会话模式下每个请求必带的自定义头（与后端 internal/session.HeaderName 对齐）。
 *
 * 它是 CSRF 防线，不是装饰：跨站的表单 / <img> / <script> 都能让浏览器自动
 * 带上 cookie，但都加不了自定义头。少了它，后端会忽略 cookie 并回 401。
 */
export const SESSION_HEADER = 'X-WebTag-Session'
export const DATA_NAMESPACE_HEADER = 'X-WebTag-Data-Namespace'

export type { ListLinksParams } from './types'
export { buildLinksQuery }
export {
  archiveV2Sections,
  fullArchiveV2Selection,
  type ArchiveV2Selection,
} from './archive-v2'

export interface ReaderReadOptions {
  /** Optional cancellation owned by the caller. */
  signal?: AbortSignal
}

/** Per-call cancellation for request paths that are scheduled by a component. */
export interface ReaderRequestOptions extends ReaderReadOptions {
  readonly signal?: AbortSignal
	/** Stable identity reused only when replaying the same mutation intent. */
	readonly idempotencyKey?: string
}

export interface SessionLoginAttempt {
  readonly result: ApiResult<{ expiresAt: string; clientDataNamespace: string }>
  /** A successful HTTP response may already have replaced the browser cookie. */
  readonly sessionMayExist: boolean
}

export type ReaderActivityKind = 'all' | 'tag' | 'domain'

export interface ReaderActivityRequestOptions extends ReaderRequestOptions {
  readonly kind?: ReaderActivityKind
  readonly after?: string
}

function readerIdempotencyHeaders(options: ReaderRequestOptions): Record<string, string> | undefined {
	const key = options.idempotencyKey?.trim()
	return key ? { 'Idempotency-Key': key } : undefined
}

/** Build the RSS item filter query without emitting empty/default values. */
export function buildFeedItemsQuery(params: ListFeedItemsParams): string {
  const normalize = (value: string | number | undefined): string | undefined => {
    if (value === undefined) return undefined
    const normalized = String(value).trim()
    return normalized || undefined
  }
  return buildQueryString(
    {
      view: normalize(params.view),
      subscription_id: normalize(params.subscription_id),
      folder_id: normalize(params.folder_id),
      q: normalize(params.q),
      page:
        params.page !== undefined && params.page > 1 ? params.page : undefined,
      limit:
        params.limit !== undefined && params.limit > 0
          ? params.limit
          : undefined,
    },
    true,
  )
}

function buildReaderQuery(values: Record<string, string | number | readonly string[] | undefined>): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(values)) {
    if (value === undefined) continue
    if (Array.isArray(value)) {
      for (const candidate of value) {
        const normalized = String(candidate).trim()
        if (normalized) query.append(key, normalized)
      }
      continue
    }
    const normalized = String(value).trim()
    if (normalized) query.set(key, normalized)
  }
  return query.size > 0 ? `?${query.toString()}` : ''
}

const READER_FEED_SOURCE_ORDER = ['inbox', 'reading', 'subscription'] as const
type ReaderFeedSource = typeof READER_FEED_SOURCE_ORDER[number]

function normalizeReaderFeedSources(values: readonly string[] | undefined): ReaderFeedSource[] | undefined {
  if (values === undefined) return undefined
  const selected = new Set<ReaderFeedSource>()
  for (const value of values) {
    for (const part of value.split(',')) {
      const normalized = part.trim().toLowerCase()
      const source: ReaderFeedSource | null = normalized === 'pending'
        ? 'inbox'
        : normalized === 'saved'
          ? 'reading'
          : normalized === 'inbox' || normalized === 'reading' || normalized === 'subscription'
            ? normalized
            : null
      if (source) selected.add(source)
    }
  }
  return READER_FEED_SOURCE_ORDER.filter((source) => selected.has(source))
}

function readerLimit(limit: number | undefined, fallback: number): number | undefined {
  if (limit === undefined) return fallback
  return limit > 0 ? Math.min(Math.floor(limit), 200) : fallback
}

const SESSION_IDENTITY_KEYS = new Set([
  'client_data_namespace',
  'representation_contract',
])
const SESSION_CREATED_KEYS = new Set([...SESSION_IDENTITY_KEYS, 'expires_at'])
const DATA_NAMESPACE_PATTERN = /^[A-Za-z0-9_-]{43}$/
const REPRESENTATION_CONTRACT = 'v3'
const RFC3339_DATE_TIME_PATTERN = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|[+-](\d{2}):(\d{2}))$/

function hasOnlyKeys(value: Record<string, unknown>, allowed: ReadonlySet<string>): boolean {
  const keys = Object.keys(value)
  return keys.length === allowed.size && keys.every((key) => allowed.has(key))
}

function hasSessionIdentityFields(
  value: Record<string, unknown>,
): value is Record<string, unknown> & SessionIdentity {
  return (
    typeof value.client_data_namespace === 'string' &&
    DATA_NAMESPACE_PATTERN.test(value.client_data_namespace) &&
    value.representation_contract === REPRESENTATION_CONTRACT
  )
}

function isRFC3339DateTime(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const match = RFC3339_DATE_TIME_PATTERN.exec(value)
  if (!match) return false

  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const hour = Number(match[4])
  const minute = Number(match[5])
  const second = Number(match[6])
  const offsetHour = match[7] === undefined ? 0 : Number(match[7])
  const offsetMinute = match[8] === undefined ? 0 : Number(match[8])
  if (month < 1 || month > 12) return false

  const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
  const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  return (
    day >= 1 &&
    day <= daysInMonth[month - 1] &&
    hour <= 23 &&
    minute <= 59 &&
    second <= 60 &&
    offsetHour <= 23 &&
    offsetMinute <= 59
  )
}

/** Strict RF2A v2 identity wire guard shared by bootstrap and the authenticated client. */
export function isSessionIdentity(value: unknown): value is SessionIdentity {
  return isRecord(value) && hasOnlyKeys(value, SESSION_IDENTITY_KEYS) && hasSessionIdentityFields(value)
}

function isSessionCreated(
  value: unknown,
): value is SessionIdentity & { readonly expires_at: string } {
  if (!isRecord(value)) return false
  const expiresAt = value.expires_at
  return (
    hasOnlyKeys(value, SESSION_CREATED_KEYS) &&
    hasSessionIdentityFields(value) &&
    isRFC3339DateTime(expiresAt)
  )
}

/** 从响应头解析 Retry-After 的「纯整数秒」格式（HTTP-date 格式不解析，返回 undefined）。 */
function parseRetryAfter(headers: Headers): number | undefined {
  return parseRetryAfterValue(headers.get('Retry-After'), { allowZero: true })
}

/** 把 fetch 抛出的异常归一化为 timeout / network-unreachable / other。 */
function normalizeThrown(e: unknown, timeoutMs: number): ApiError {
  return normalizeThrownError(e, timeoutMs)
}

/** 把非 2xx 响应（已解析的错误体）归一化为 ApiError。 */
function normalizeHttpError(
  status: number,
  body: unknown,
  retryAfterSeconds: number | undefined,
): ApiError {
  return normalizeSharedHttpError(status, body, retryAfterSeconds)
}

/** 响应体不符契约时的统一失败结果。 */
function shapeMismatch<T>(detail: string): ApiResult<T> {
  return err<T>({ kind: 'other', message: `响应体格式不符：${detail}` })
}

function archiveValidationFailure<T>(failure: unknown): ApiResult<T> {
  if (failure instanceof ArchiveV2ValidationError) {
    return err({
      kind: 'other',
      message: failure.message,
      errorCode: failure.errorCode,
    })
  }
  return err({
    kind: 'other',
    message: '归档完整性校验失败',
    errorCode: ARCHIVE_V2_VALIDATION_ERROR_CODE,
  })
}

function archiveTooLargeFailure<T>(): ApiResult<T> {
  return err({
    kind: 'other',
    message: '归档超过浏览器可验证的 64 MiB 上限',
    errorCode: ARCHIVE_V2_TOO_LARGE_ERROR_CODE,
  })
}

function archiveContentLengthExceedsBrowserLimit(headers: Headers): boolean {
  const raw = headers.get('Content-Length')?.trim()
  if (!raw || !/^\d+$/.test(raw)) return false
  const length = Number(raw)
  return !Number.isSafeInteger(length) || length > ARCHIVE_V2_MAX_BYTES
}

export class ReaderClient {
  private readonly baseURL: string
  private readonly installationToken: string
  private readonly session: boolean
  private readonly timeoutMs: number
  private readonly identity?: IdentityLease
  private readonly onIdentityMismatch?: () => void

  constructor(config: ReaderClientConfig) {
    this.baseURL = config.baseURL.replace(/\/+$/, '')
    this.installationToken = config.installationToken?.trim() ?? ''
    this.session = config.session ?? false
    this.timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT
    this.identity = config.identity
    this.onIdentityMismatch = config.onIdentityMismatch
  }

  /** The sole identity lease owned by this client, or null for bootstrap-only clients. */
  get identityLease(): IdentityLease | null {
    return this.identity ?? null
  }

  /** 鉴权相关的请求头 + fetch 选项，两条请求路径（send / 下载）共用。 */
  private authInit(): { headers: Record<string, string>; init: RequestInit } {
    if (this.session) {
      return {
        headers: { [SESSION_HEADER]: '1' },
        init: { credentials: 'include' },
      }
    }
    return {
      headers: this.installationToken
        ? { Authorization: `Bearer ${this.installationToken}` }
        : {},
      init: {},
    }
  }

  private validateResponseOwnership(
    response: Response,
    operation: IdentityOperationContext | undefined,
  ): ApiResult<never> | null {
    if (!this.identity || !operation) return null
    if (!this.identity.isCurrent(operation)) {
      return err({ kind: 'identity-mismatch', message: 'Authenticated response belongs to a stale identity epoch' })
    }
    if (response.headers.get(DATA_NAMESPACE_HEADER) === operation.serverClientDataNamespace) {
      return null
    }

    this.identity.revoke()
    this.onIdentityMismatch?.()
    return err({
      kind: 'identity-mismatch',
      message: 'Authenticated response identity marker changed',
    })
  }

  /** Whether component work started with this client may still commit local side effects. */
  isIdentityCurrent(): boolean {
    return this.captureIdentity('ReaderClient component continuation') !== null
  }

  /** Capture the immutable lease/epoch owned by this client for a multi-step component action. */
  captureIdentity(logicalKey: string): IdentityOwnership | null {
    const identity = this.identity
    if (!identity) return null
    const ownership = identity.captureOwnership(logicalKey)
    return identity.isOwnershipCurrent(ownership) ? ownership : null
  }

  /** Fetch the authenticated identity snapshot without consulting any HTTP cache. */
  async getIdentity(signal?: AbortSignal): Promise<ApiResult<SessionIdentity>> {
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const auth = this.authInit()
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}/api/session`, {
          ...auth.init,
          method: 'GET',
          cache: 'no-store',
          headers: { Accept: 'application/json', ...auth.headers },
          signal: controller.signal,
        })
      } catch (e) {
        return err(normalizeThrown(e, this.timeoutMs))
      }

      let body: unknown = null
      try {
        const text = await response.text()
        body = text ? JSON.parse(text) : null
      } catch (e) {
        if (controller.signal.aborted) return err(normalizeThrown(e, this.timeoutMs))
      }
      if (!response.ok) {
        return err(normalizeHttpError(response.status, body, undefined))
      }
      if (!isSessionIdentity(body)) return shapeMismatch('SessionIdentity')
      const marker = response.headers.get(DATA_NAMESPACE_HEADER)
      if (!marker || marker !== body.client_data_namespace) {
        return err({
          kind: 'identity-mismatch',
          message: 'Authenticated response identity marker is missing or inconsistent',
        })
      }
      return ok(body)
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
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
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}/health`, {
          method: 'GET',
          cache: 'no-store',
          headers: { Accept: 'application/json' },
          signal: controller.signal,
        })
      } catch (e) {
        return err(normalizeThrown(e, this.timeoutMs))
      }

      let body: unknown = null
      try {
        const text = await response.text()
        body = text ? JSON.parse(text) : null
      } catch (e) {
        if (controller.signal.aborted) return err(normalizeThrown(e, this.timeoutMs))
      }
      if (!response.ok) return err(normalizeHttpError(response.status, body, undefined))
      if (!isHealthResponse(body)) return shapeMismatch('HealthResponse')
      return ok(body)
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
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
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    try {
      let res: Response
      try {
        res = await fetch(`${this.baseURL}/api/session`, {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ token: installationToken }),
          signal: controller.signal,
        })
      } catch (e) {
        return {
          result: err(normalizeThrown(e, this.timeoutMs)),
          sessionMayExist: false,
        }
      }
      const text = await res.text().catch(() => '')
      let body: unknown = null
      if (text) {
        try {
          body = JSON.parse(text)
        } catch {
          body = null
        }
      }
      if (!res.ok) {
        return {
          result: err(normalizeHttpError(res.status, body, undefined)),
          sessionMayExist: false,
        }
      }
      if (!isSessionCreated(body)) {
        return { result: shapeMismatch('SessionCreated'), sessionMayExist: true }
      }
      const marker = res.headers.get(DATA_NAMESPACE_HEADER)
      if (!marker || marker !== body.client_data_namespace) {
        return {
          result: err({
            kind: 'identity-mismatch',
            message: 'Session creation identity marker is missing or inconsistent',
          }),
          sessionMayExist: true,
        }
      }
      return {
        result: ok({
          expiresAt: body.expires_at,
          clientDataNamespace: body.client_data_namespace,
        }),
        sessionMayExist: true,
      }
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
  }

  /** 结束会话：让后端清掉 cookie。失败不阻塞前端登出。 */
  async logout(): Promise<void> {
    try {
      await fetch(`${this.baseURL}/api/session`, {
        method: 'DELETE',
        credentials: 'include',
        headers: { [SESSION_HEADER]: '1' },
      })
    } catch {
      // 网络失败时本地照常清配置；cookie 会随过期自然失效。
    }
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
    const requestSignal = callerSignal ?? readOptions?.signal
    const identity = this.identity
    if (!identity) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private request requires an authoritative identity lease',
      })
    }
    const ownership = identity.capture(`${method} ${path}`)
    if (!identity.isCurrent(ownership)) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private request belongs to a stale identity epoch',
      })
    }
    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    const abortForCaller = () => controller.abort()
    if (ownership.signal.aborted) controller.abort()
    else ownership.signal.addEventListener('abort', abortForIdentity, { once: true })
    if (requestSignal?.aborted) controller.abort()
    else requestSignal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const cancellationFailure = (message: string): ApiResult<never> | null => {
      if (!identity.isCurrent(ownership)) {
        return err({ kind: 'identity-mismatch', message })
      }
      if (controller.signal.aborted) {
        return err(normalizeThrown(new DOMException('aborted', 'AbortError'), this.timeoutMs))
      }
      return null
    }
    const auth = this.authInit()
    const headers: Record<string, string> = { Accept: 'application/json', ...auth.headers }
    Object.assign(headers, extraHeaders)
    if (requestBody !== undefined) headers['Content-Type'] = 'application/json'

    // 超时计时器必须存活到响应体读完为止：服务端可能在发完响应头后于
    // body 阶段挂起，若 headers 一到就 clearTimeout，res.text() 将无限期
    // hang（本仓库 extension 客户端在 commit 54f943f 修过同一个洞）。
    // 因此 clearTimeout 收敛进 finally，包住 fetch + res.text() 全程。
    try {
      let res: Response
      try {
        res = await fetch(`${this.baseURL}${path}`, {
          ...auth.init,
          method,
          headers,
          ...(requestBody === undefined ? {} : { body: JSON.stringify(requestBody) }),
          signal: controller.signal,
        })
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated request was cancelled by an identity change',
          })
        }
        return err(normalizeThrown(e, this.timeoutMs))
      }

      const postFetchCancellation = cancellationFailure(
        'Authenticated request was cancelled by an identity change',
      )
      if (postFetchCancellation) return postFetchCancellation

      const ownershipFailure = this.validateResponseOwnership(res, ownership)
      if (ownershipFailure) return ownershipFailure

      // 解析 body（429 也可能带 JSON 错误体）。非 JSON 时 body 置 null。
      // abort 触发时 res.text() reject（AbortError）→ 归一化为 timeout。
      let responseBody: unknown = null
      let text: string
      try {
        text = await res.text()
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated response body was cancelled by an identity change',
          })
        }
        if (controller.signal.aborted) {
          return err(normalizeThrown(e, this.timeoutMs))
        }
        text = ''
      }
      const postBodyCancellation = cancellationFailure(
        'Authenticated response body was cancelled by an identity change',
      )
      if (postBodyCancellation) return postBodyCancellation
      const postBodyOwnershipFailure = this.validateResponseOwnership(res, ownership)
      if (postBodyOwnershipFailure) return postBodyOwnershipFailure
      if (text) {
        if (
          res.ok &&
          rawJSONContract === 'link-metadata-revision' &&
          !hasCanonicalSafeLinkMetadataRevisionTokens(text)
        ) {
          return shapeMismatch('Link metadata revision JSON')
        }
        try {
          responseBody = JSON.parse(text)
        } catch {
          // OPML export may be returned directly as XML instead of `{ opml }`.
          responseBody = text
        }
      }

      if (!res.ok) {
        const retryAfter = res.status === 429 ? parseRetryAfter(res.headers) : undefined
        return err(normalizeHttpError(res.status, responseBody, retryAfter))
      }
      return ok(responseBody)
    } finally {
      clearTimeout(timer)
      ownership.signal.removeEventListener('abort', abortForIdentity)
      requestSignal?.removeEventListener('abort', abortForCaller)
    }
  }

  /**
   * Read a response body without allowing a streamed archive to exceed the
   * browser verification bound. Identity is checked around every awaited
   * stream read, because an old response must not cross a lease transition.
   */
  private async readArchiveV2Bytes(
    response: Response,
    identity: IdentityLease,
    ownership: IdentityOperationContext,
    controller: AbortController,
  ): Promise<ApiResult<Uint8Array>> {
    if (archiveContentLengthExceedsBrowserLimit(response.headers)) {
      controller.abort()
      void response.body?.cancel().catch(() => undefined)
      return archiveTooLargeFailure()
    }

    const body = response.body
    if (!body) return ok(new Uint8Array())

    const reader = body.getReader()
    const chunks: Uint8Array[] = []
    let total = 0
    const cancelForIdentityChange = (): ApiResult<Uint8Array> => {
      controller.abort()
      void reader.cancel().catch(() => undefined)
      return err({
        kind: 'identity-mismatch',
        message: 'Authenticated archive stream was cancelled by an identity change',
      })
    }
    try {
      for (;;) {
        if (!identity.isCurrent(ownership)) {
          return cancelForIdentityChange()
        }

        let next: ReadableStreamReadResult<Uint8Array>
        try {
          next = await reader.read()
        } catch (e) {
          if (!identity.isCurrent(ownership)) {
            return cancelForIdentityChange()
          }
          return err(normalizeThrown(e, this.timeoutMs))
        }

        // This is intentionally after every awaited read, including `done`.
        if (!identity.isCurrent(ownership)) {
          return cancelForIdentityChange()
        }
        if (next.done) break
        if (!isArchiveV2ByteArray(next.value)) {
          return archiveValidationFailure(
            new ArchiveV2ValidationError('归档响应流不是字节流'),
          )
        }
        if (next.value.byteLength > ARCHIVE_V2_MAX_BYTES - total) {
          controller.abort()
          void reader.cancel().catch(() => undefined)
          return archiveTooLargeFailure()
        }
        chunks.push(next.value)
        total += next.value.byteLength
      }

      const bytes = new Uint8Array(total)
      let offset = 0
      for (const chunk of chunks) {
        bytes.set(chunk, offset)
        offset += chunk.byteLength
      }
      return ok(bytes)
    } finally {
      reader.releaseLock()
    }
  }

  /**
   * Download and verify a selected v2 archive. No Blob is constructed until
   * headers, the full byte stream, JSON/schema/count/checksum validation, and
   * the active identity lease have all passed.
   */
  async downloadArchiveV2(
    selection: ArchiveV2Selection = fullArchiveV2Selection,
  ): Promise<ApiResult<Blob>> {
    const identity = this.identity
    if (!identity) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private archive download requires an authoritative identity lease',
      })
    }
    let sections: string
    try {
      sections = archiveV2Sections(selection)
    } catch (e) {
      return archiveValidationFailure(e)
    }
    const path = `/api/export/v2?sections=${sections}`
    const ownership = identity.capture(`GET ${path}`)
    if (!identity.isCurrent(ownership)) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private archive download belongs to a stale identity epoch',
      })
    }
    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    if (ownership.signal.aborted) controller.abort()
    else ownership.signal.addEventListener('abort', abortForIdentity, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const cancellationFailure = (message: string): ApiResult<never> | null => {
      if (!identity.isCurrent(ownership)) {
        return err({ kind: 'identity-mismatch', message })
      }
      if (controller.signal.aborted) {
        return err(normalizeThrown(new DOMException('aborted', 'AbortError'), this.timeoutMs))
      }
      return null
    }
    const auth = this.authInit()
    const headers: Record<string, string> = { Accept: 'application/json', ...auth.headers }
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}${path}`, {
          ...auth.init,
          method: 'GET',
          headers,
          signal: controller.signal,
        })
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated archive request was cancelled by an identity change',
          })
        }
        return err(normalizeThrown(e, this.timeoutMs))
      }

      const postFetchCancellation = cancellationFailure(
        'Authenticated archive request was cancelled by an identity change',
      )
      if (postFetchCancellation) return postFetchCancellation

      // Fence one: the header marker and the captured lease must agree before
      // any response bytes are read.
      const headerOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (headerOwnershipFailure) return headerOwnershipFailure

      const bodyResult = await this.readArchiveV2Bytes(
        response,
        identity,
        ownership,
        controller,
      )
      if (!bodyResult.ok) return bodyResult

      const postStreamCancellation = cancellationFailure(
        'Authenticated archive stream was cancelled by an identity change',
      )
      if (postStreamCancellation) return postStreamCancellation
      const postStreamOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (postStreamOwnershipFailure) return postStreamOwnershipFailure

      if (!response.ok) {
        let body: unknown = null
        const text = new TextDecoder().decode(bodyResult.data)
        if (text) {
          try {
            body = JSON.parse(text)
          } catch {
            // A non-JSON error body still maps deterministically by status.
          }
        }
        return err(normalizeHttpError(
          response.status,
          body,
          response.status === 429 ? parseRetryAfter(response.headers) : undefined,
        ))
      }

      try {
        await validateArchiveV2Bytes(bodyResult.data, {
          clientDataNamespace: ownership.serverClientDataNamespace,
          selection,
        })
      } catch (e) {
        return archiveValidationFailure(e)
      }

      const postValidationCancellation = cancellationFailure(
        'Authenticated archive validation was cancelled by an identity change',
      )
      if (postValidationCancellation) return postValidationCancellation
      // Fence two: validation awaits SHA-256, so identity may have changed
      // after the stream completed even though its header was initially valid.
      const postValidationOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (postValidationOwnershipFailure) return postValidationOwnershipFailure

      // Fence three: keep the final check directly adjacent to the success
      // path so a stale archive cannot become a download after validation.
      const immediatelyBeforeSuccessCancellation = cancellationFailure(
        'Authenticated archive validation was cancelled by an identity change',
      )
      if (immediatelyBeforeSuccessCancellation) return immediatelyBeforeSuccessCancellation
      const immediatelyBeforeSuccess = this.validateResponseOwnership(response, ownership)
      if (immediatelyBeforeSuccess) return immediatelyBeforeSuccess
      try {
        // BlobPart deliberately requires an ArrayBuffer-backed view. This
        // copies raw bytes only; it never parses or reserializes the archive.
        const blobBytes = new Uint8Array(bodyResult.data.byteLength)
        blobBytes.set(bodyResult.data)
        return ok(new Blob([blobBytes.buffer], { type: ARCHIVE_V2_MIME_TYPE }))
      } catch (e) {
        return archiveValidationFailure(e)
      }
    } finally {
      clearTimeout(timer)
      ownership.signal.removeEventListener('abort', abortForIdentity)
    }
  }

  /** GET /api/links —— 列表 / 搜索（q）/ 存在性检查（url）。 */
  async getLinks(
    params: ListLinksParams = {},
    options?: ReaderReadOptions,
  ): Promise<ApiResult<PaginatedLinksResponse>> {
    const r = await this.send(
      'GET',
      `/api/links${buildLinksQuery(params)}`,
      undefined,
      undefined,
      options,
      undefined,
      'link-metadata-revision',
    )
    if (!r.ok) return r
    if (!isPaginatedLinks(r.data)) return shapeMismatch('PaginatedLinksResponse')
    return ok(r.data)
  }

  /** POST /api/links -- submit one URL to the parse pipeline. */
  async submitLink(request: LinkCreateRequest): Promise<ApiResult<SubmitResponse>> {
    const r = await this.send('POST', '/api/links', request)
    if (!r.ok) return r
    if (!isSubmitResponse(r.data)) return shapeMismatch('SubmitResponse')
    return ok(r.data)
  }

  /** GET /api/search —— grouped reading, website, and cursor-paged thought search. */
  async searchLibrary(
    q: string,
    readingLimit = 10,
    siteLimit = 10,
    thoughtLimit = 20,
    thoughtAfter = '',
  ): Promise<ApiResult<GroupedSearchResponse>> {
    const query = new URLSearchParams({
      q: q.trim(),
      reading_limit: String(readingLimit),
      site_limit: String(siteLimit),
      thought_limit: String(thoughtLimit),
    })
    if (thoughtAfter.trim()) query.set('thought_after', thoughtAfter)
    const r = await this.send(
      'GET',
      `/api/search?${query}`,
      undefined,
      undefined,
      undefined,
      undefined,
      'link-metadata-revision',
    )
    if (!r.ok) return r
    return isGroupedSearchResponse(r.data) ? ok(r.data) : shapeMismatch('GroupedSearchResponse')
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
    const r = await this.send(
      'GET',
      `/api/links/${encodeURIComponent(id)}?include_content=false`,
      undefined,
      undefined,
      undefined,
      options.signal,
      'link-metadata-revision',
    )
    if (!r.ok) return r
    if (!isLinkResponse(r.data)) return shapeMismatch('LinkResponse')
    return ok(r.data)
  }

  /** GET /api/links/{id}/content —— 按需读已保存原文（不触发抓取）。 */
  async getContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    const r = await this.send('GET', `/api/links/${encodeURIComponent(id)}/content`)
    if (!r.ok) return r
    if (!isLinkContentResponse(r.data)) return shapeMismatch('LinkContentResponse')
    return ok(r.data)
  }

  /** PATCH /api/links/{id}/content - replace saved content without fetching. */
  async editContent(
    id: string,
    request: ContentEditRequest,
  ): Promise<ApiResult<LinkContentResponse>> {
    const r = await this.send(
      'PATCH',
      `/api/links/${encodeURIComponent(id)}/content`,
      request,
    )
    if (!r.ok) return r
    if (!isLinkContentResponse(r.data)) return shapeMismatch('LinkContentResponse')
    return ok(r.data)
  }

	async getSites(params: ListSitesParams = {}): Promise<ApiResult<PaginatedSitesResponse>> {
		const query = new URLSearchParams()
		if (params.view) query.set('view', params.view)
		if (params.tags?.trim()) query.set('tags', params.tags.trim())
		if (params.recentCutoff?.trim()) query.set('recent_cutoff', params.recentCutoff.trim())
		if (params.page && params.page > 1) query.set('page', String(params.page))
		if (params.limit && params.limit > 0) query.set('limit', String(params.limit))
		const suffix = query.size ? `?${query}` : ''
		const r = await this.send('GET', `/api/sites${suffix}`)
		if (!r.ok) return r
		return isPaginatedSites(r.data) ? ok(r.data) : shapeMismatch('PaginatedSitesResponse')
	}

	async getSite(id: string): Promise<ApiResult<SiteDetailResponse>> {
		const r = await this.send('GET', `/api/sites/${encodeURIComponent(id)}`)
		if (!r.ok) return r
		return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
	}

	async updateSite(id: string, revision: number, patch: SiteUpdateRequest): Promise<ApiResult<SiteDetailResponse>> {
		const r = await this.send('PATCH', `/api/sites/${encodeURIComponent(id)}`, patch, { 'If-Match': `"${revision}"` })
		if (!r.ok) return r
		return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
	}

	async updateSiteEntry(siteID: string, entryID: string, revision: number, patch: SiteEntryUpdateRequest): Promise<ApiResult<SiteDetailResponse>> {
		const r = await this.send('PATCH', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}`, patch, { 'If-Match': `"${revision}"` })
		if (!r.ok) return r
		return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
	}

	async setPrimarySiteEntry(siteID: string, entryID: string, revision: number): Promise<ApiResult<SiteDetailResponse>> {
		const r = await this.send('POST', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}/set-primary`, undefined, { 'If-Match': `"${revision}"` })
		if (!r.ok) return r
		return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
	}

	async deleteSiteEntry(siteID: string, entryID: string, revision: number): Promise<ApiResult<SiteEntryDeleteResponse>> {
		const r = await this.send('DELETE', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}`, undefined, { 'If-Match': `"${revision}"` })
		if (!r.ok) return r
		return isSiteEntryDeleteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteEntryDeleteResponse')
	}

	async deleteSite(id: string, revision: number, entryCount: number): Promise<ApiResult<void>> {
		const query = new URLSearchParams({ confirm_entry_count: String(entryCount) })
		const r = await this.send('DELETE', `/api/sites/${encodeURIComponent(id)}?${query}`, undefined, { 'If-Match': `"${revision}"` })
		if (!r.ok) return r
		return ok(undefined)
	}

	async previewSiteMerge(request: SiteMergePreviewRequest): Promise<ApiResult<SiteMergePreviewResponse>> {
		const r = await this.send('POST', '/api/sites/merge-preview', request)
		if (!r.ok) return r
		return isSiteMergePreviewResponse(r.data) ? ok(r.data) : shapeMismatch('SiteMergePreviewResponse')
	}

	async executeSiteMerge(request: SiteMergeExecuteRequest): Promise<ApiResult<SiteMergeExecuteResponse>> {
		const r = await this.send('POST', '/api/sites/merge', request)
		if (!r.ok) return r
		return isSiteMergeExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteMergeExecuteResponse')
	}

	async previewSiteSplit(siteID: string, request: SiteSplitRequest): Promise<ApiResult<SiteSplitPreviewResponse>> {
		const r = await this.send('POST', `/api/sites/${encodeURIComponent(siteID)}/split-preview`, request)
		if (!r.ok) return r
		return isSiteSplitPreviewResponse(r.data) ? ok(r.data) : shapeMismatch('SiteSplitPreviewResponse')
	}

	async executeSiteSplit(siteID: string, request: SiteSplitRequest): Promise<ApiResult<SiteSplitExecuteResponse>> {
		const r = await this.send('POST', `/api/sites/${encodeURIComponent(siteID)}/split`, request)
		if (!r.ok) return r
		return isSiteSplitExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteSplitExecuteResponse')
	}

	/** POST /api/links/{id}/conversion-preview — pure conversion consequences. */
	async previewLinkConversion(id: string, request: ConversionPreviewRequest): Promise<ApiResult<ConversionPreviewResponse>> {
		const r = await this.send('POST', `/api/links/${encodeURIComponent(id)}/conversion-preview`, request)
		if (!r.ok) return r
		return isConversionPreviewResponse(r.data) ? ok(r.data) : shapeMismatch('ConversionPreviewResponse')
	}

	/** POST /api/links/{id}/convert — apply a confirmed revision-bound conversion. */
	async convertLink(id: string, request: ConversionExecuteRequest): Promise<ApiResult<ConversionExecuteResponse>> {
		const r = await this.send('POST', `/api/links/${encodeURIComponent(id)}/convert`, request)
		if (!r.ok) return r
		return isConversionExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('ConversionExecuteResponse')
	}

  /** GET /api/tags —— 标签聚合；省略 scope 时返回全部资料库计数。 */
  async getTags(
    scope?: 'reading' | 'site' | 'all',
    options?: ReaderReadOptions,
  ): Promise<ApiResult<TagCountResponse[]>> {
    const query = scope ? `?library_kind=${encodeURIComponent(scope)}` : ''
    const r = await this.send('GET', '/api/tags' + query, undefined, undefined, options)
    if (!r.ok) return r
    if (!isTagCountArray(r.data)) return shapeMismatch('TagCountResponse[]')
    // Scoped tag rows carry both partition components. Their presence lets
    // Reader reject an older server that silently ignores library_kind and
    // returns the legacy bare all-library counts.
    if (scope && r.data.some((tag) => {
      const readingCount = tag.reading_count
      const siteCount = tag.site_count
      if (typeof readingCount !== 'number' || typeof siteCount !== 'number') return true
      if (scope === 'reading') return tag.count !== readingCount
      if (scope === 'site') return tag.count !== siteCount
      return tag.count !== readingCount + siteCount
    })) {
      return shapeMismatch('scoped TagCountResponse[]')
    }
    return ok(r.data)
  }

  /** GET /api/tree?view=domains —— truthful 域名聚合。 */
  async getDomainSummaries(
    scope?: 'reading' | 'site',
    options?: ReaderReadOptions,
  ): Promise<ApiResult<DomainTreeSummaryEnvelope>> {
    const query = scope ? `?view=domains&library_kind=${encodeURIComponent(scope)}` : '?view=domains'
    const r = await this.send('GET', '/api/tree' + query, undefined, undefined, options)
    if (!r.ok) return r
    if (!isDomainTreeSummaryEnvelope(r.data)) {
      return shapeMismatch('DomainTreeSummaryEnvelope')
    }
    // A legacy server can ignore an additive query parameter and return a
    // plausible but unscoped all-library aggregate. The scoped response echo
    // is therefore part of the authority proof, not display metadata.
    if (scope && r.data.library_kind !== scope) {
      return shapeMismatch('scoped DomainTreeSummaryEnvelope')
    }
    return ok(r.data)
  }

  /** POST /api/links/{id}/content —— 抓取并保存网页原文，返回保存后的原文。 */
  async saveContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    const r = await this.send('POST', `/api/links/${encodeURIComponent(id)}/content`)
    if (!r.ok) return r
    if (!isLinkContentResponse(r.data)) {
      return shapeMismatch('LinkContentResponse')
    }
    return ok(r.data)
  }

  /** PUT /api/links/{id}/content —— 重新抓取并明确替换已保存原文。 */
  async replaceContent(id: string): Promise<ApiResult<LinkContentResponse>> {
    const r = await this.send('PUT', `/api/links/${encodeURIComponent(id)}/content`)
    if (!r.ok) return r
    if (!isLinkContentResponse(r.data)) {
      return shapeMismatch('LinkContentResponse')
    }
    return ok(r.data)
  }

  /** GET /api/links/{id}/translations —— 读取数据库中的选段与全文译文。 */
  async getTranslations(
    id: string,
  ): Promise<ApiResult<TranslationListResponse>> {
    const r = await this.send(
      'GET',
      `/api/links/${encodeURIComponent(id)}/translations`,
    )
    if (!r.ok) return r
    if (!isTranslationListResponse(r.data)) {
      return shapeMismatch('TranslationListResponse')
    }
    return ok(r.data)
  }

  /** POST /api/links/{id}/translations —— 创建或复用异步中文翻译。 */
  async createTranslation(
    id: string,
    request: TranslationCreateRequest,
  ): Promise<ApiResult<TranslationResponse>> {
    const r = await this.send(
      'POST',
      `/api/links/${encodeURIComponent(id)}/translations`,
      request,
    )
    if (!r.ok) return r
    if (!isTranslationResponse(r.data)) {
      return shapeMismatch('TranslationResponse')
    }
    return ok(r.data)
  }

  /** GET /api/subscriptions -- RSS navigation metadata and truthful counts. */
  async getSubscriptions(
    url?: string,
    options?: ReaderReadOptions,
  ): Promise<ApiResult<SubscriptionsResponse>> {
    const query = url?.trim() ? `?url=${encodeURIComponent(url.trim())}` : ''
    const r = await this.send('GET', `/api/subscriptions${query}`, undefined, undefined, options)
    if (!r.ok) return r
    if (!isSubscriptionsResponse(r.data)) return shapeMismatch('SubscriptionsResponse')
    return ok(r.data)
  }

  /** Resolve a feed URL directly or discover alternate feeds from an HTML page. */
  async discoverFeeds(url: string): Promise<ApiResult<DiscoverFeedsResponse>> {
    const r = await this.send('POST', '/api/subscriptions/discover', { url })
    if (!r.ok) return r
    if (!isDiscoverFeedsResponse(r.data)) return shapeMismatch('DiscoverFeedsResponse')
    return ok(r.data)
  }

  async createSubscription(input: {
    url: string
    folder_id?: string | null
  }): Promise<ApiResult<FeedSubscription>> {
    const r = await this.send('POST', '/api/subscriptions', input)
    if (!r.ok) return r
    const candidate = isRecord(r.data) && 'subscription' in r.data ? r.data.subscription : r.data
    if (!isFeedSubscription(candidate)) return shapeMismatch('FeedSubscription')
    return ok(candidate)
  }

  async updateSubscription(
    id: string,
    patch: { folder_id?: string | null },
  ): Promise<ApiResult<FeedSubscription | null>> {
    const r = await this.send('PUT', `/api/subscriptions/${encodeURIComponent(id)}`, patch)
    if (!r.ok) return r
    if (r.data === null) return ok(null)
    const candidate = isRecord(r.data) && 'subscription' in r.data ? r.data.subscription : r.data
    if (!isFeedSubscription(candidate)) return shapeMismatch('FeedSubscription')
    return ok(candidate)
  }

  async deleteSubscription(id: string): Promise<ApiResult<true>> {
    const r = await this.send('DELETE', `/api/subscriptions/${encodeURIComponent(id)}`)
    return r.ok ? ok(true) : r
  }

  async refreshSubscription(id: string): Promise<ApiResult<true>> {
    const r = await this.send('POST', `/api/subscriptions/${encodeURIComponent(id)}/refresh`)
    return r.ok ? ok(true) : r
  }

  async refreshSubscriptions(): Promise<ApiResult<true>> {
    const r = await this.send('POST', '/api/subscriptions/refresh')
    return r.ok ? ok(true) : r
  }

  async getFeedItems(
    params: ListFeedItemsParams = {},
    options?: ReaderReadOptions,
  ): Promise<ApiResult<PaginatedFeedItemsResponse>> {
    const r = await this.send('GET', `/api/feed-items${buildFeedItemsQuery(params)}`, undefined, undefined, options)
    if (!r.ok) return r
    if (!isPaginatedFeedItems(r.data)) return shapeMismatch('PaginatedFeedItemsResponse')
    return ok(r.data)
  }

  async getFeedItem(id: string): Promise<ApiResult<FeedItem>> {
    const r = await this.send('GET', `/api/feed-items/${encodeURIComponent(id)}`)
    if (!r.ok) return r
    const candidate = isRecord(r.data) && 'item' in r.data ? r.data.item : r.data
    if (!isFeedItem(candidate)) return shapeMismatch('FeedItem')
    return ok(candidate)
  }

  async updateFeedItem(
    id: string,
    patch: FeedItemStatePatch,
  ): Promise<ApiResult<FeedItem | null>> {
    const r = await this.send('PUT', `/api/feed-items/${encodeURIComponent(id)}/state`, patch)
    if (!r.ok) return r
    if (r.data === null) return ok(null)
    const candidate = isRecord(r.data) && 'item' in r.data ? r.data.item : r.data
    if (!isFeedItem(candidate)) return shapeMismatch('FeedItem')
    return ok(candidate)
  }

  async markFeedItemsRead(filters: ListFeedItemsParams): Promise<ApiResult<true>> {
    const r = await this.send('POST', '/api/feed-items/mark-read', filters)
    return r.ok ? ok(true) : r
  }

  async analyzeFeedItem(id: string): Promise<ApiResult<FeedItemAnalyzeResponse>> {
    const r = await this.send('POST', `/api/feed-items/${encodeURIComponent(id)}/analyze`)
    if (!r.ok) return r
    if (isFeedItem(r.data)) return ok({ item: r.data })
    if (!isFeedItemAnalyzeResponse(r.data)) return shapeMismatch('FeedItemAnalyzeResponse')
    return ok(r.data)
  }

  async createFeedFolder(name: string): Promise<ApiResult<FeedFolder>> {
    const r = await this.send('POST', '/api/feed-folders', { name })
    if (!r.ok) return r
    const candidate = isRecord(r.data) && 'folder' in r.data ? r.data.folder : r.data
    if (!isFeedFolder(candidate)) return shapeMismatch('FeedFolder')
    return ok(candidate)
  }

  async updateFeedFolder(id: string, name: string): Promise<ApiResult<FeedFolder | null>> {
    const r = await this.send('PUT', `/api/feed-folders/${encodeURIComponent(id)}`, { name })
    if (!r.ok) return r
    if (r.data === null) return ok(null)
    const candidate = isRecord(r.data) && 'folder' in r.data ? r.data.folder : r.data
    if (!isFeedFolder(candidate)) return shapeMismatch('FeedFolder')
    return ok(candidate)
  }

  async deleteFeedFolder(id: string): Promise<ApiResult<true>> {
    const r = await this.send('DELETE', `/api/feed-folders/${encodeURIComponent(id)}`)
    return r.ok ? ok(true) : r
  }

  async exportSubscriptionsOPML(): Promise<ApiResult<string>> {
    const r = await this.send('GET', '/api/subscriptions/opml')
    if (!r.ok) return r
    if (typeof r.data === 'string') return ok(r.data)
    if (isRecord(r.data) && typeof r.data.opml === 'string') return ok(r.data.opml)
    return shapeMismatch('OPML string or { opml }')
  }

  async importSubscriptionsOPML(opml: string): Promise<ApiResult<OPMLImportResponse>> {
    const r = await this.send('POST', '/api/subscriptions/opml', { opml })
    if (!r.ok) return r
    if (!isOPMLImportResponse(r.data)) return shapeMismatch('OPMLImportResponse')
    return ok(r.data)
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
    const r = await this.send('GET', `/api/engagement/${encodeURIComponent(linkID)}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderEngagementResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderEngagementResponse')
  }

  async patchEngagement(
    linkID: string,
    request: ReaderEngagementRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderEngagementResponse>> {
    const r = await this.send('PATCH', `/api/engagement/${encodeURIComponent(linkID)}`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderEngagementResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderEngagementResponse')
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
    const query = buildReaderQuery({
      mode: params.mode,
      source: normalizeReaderFeedSources(params.source),
      after: params.after,
      limit: readerLimit(params.limit, 50),
    })
    const r = await this.send('GET', `/api/reader-feed${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderFeedResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderFeedResponse')
  }

  async sendReaderFeedFeedback(
    itemKey: string,
    request: ReaderFeedFeedbackRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderFeedFeedbackResponse>> {
    const query = buildReaderQuery({ item_key: itemKey })
    const r = await this.send('POST', `/api/reader-feed/feedback${query}`, request, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderFeedFeedbackResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderFeedFeedbackResponse')
  }

  async patchLinkMetadata(
    linkID: string,
    revision: number,
    request: ReaderLinkMetadataRequest,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderLinkMetadataResponse>> {
    if (!Number.isSafeInteger(revision) || revision < 1) {
      return err({ kind: 'other', message: 'metadata revision must be a positive JavaScript-safe integer' })
    }
    const r = await this.send(
      'PATCH',
      `/api/links/${encodeURIComponent(linkID)}/metadata`,
      request,
      { 'If-Match': `"${revision}"` },
      undefined,
      options.signal,
      'link-metadata-revision',
    )
    if (!r.ok) return r
    return isReaderLinkMetadataResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderLinkMetadataResponse')
  }

  async getRelatedTags(
    linkID?: string,
    limit = 12,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<ReaderRelatedTagsResponse>> {
    const query = buildReaderQuery({ link_id: linkID, limit: readerLimit(limit, 12) })
    const r = await this.send('GET', `/api/reader/related-tags${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderRelatedTagsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderRelatedTagsResponse')
  }

  async getReaderActivity(
    limit = 100,
    options: ReaderActivityRequestOptions = {},
  ): Promise<ApiResult<ReaderActivityResponse>> {
    const query = buildReaderQuery({
      kind: options.kind,
      after: options.after,
      limit: readerLimit(limit, 100),
    })
    const r = await this.send('GET', `/api/reader/activity${query}`, undefined, undefined, undefined, options.signal)
    if (!r.ok) return r
    return isReaderActivityResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderActivityResponse')
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
