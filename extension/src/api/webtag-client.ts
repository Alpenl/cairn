/**
 * @module api/webtag-client
 * WebTag 后端 API 客户端。
 *
 * 基于 native fetch 封装（不依赖 axios，避免把 ~40 kB 的 axios 打进
 * background Service Worker），从扩展设置（src/composables/useSettings.ts
 * 持久化的配置）注入 baseURL 与 `Authorization: Bearer <token>`。
 *
 * 鉴权使用单安装后端配置的一把静态 Bearer token；扩展不管理用户身份
 * 或令牌刷新流程。
 * 提供与后端契约对应的方法：
 *   - getTree   GET  /api/links（分页拉取后前端按 URL 现算树）
 *   - getLinks  GET  /api/links
 *   - getTags   GET  /api/tags
 *   - getLink   GET  /api/links/{link_id}
 *   - ingest    POST /api/ingest
 *   - saveLinkContent POST /api/links/{id}/content
 *   - refreshLink POST /api/links/{id}/refresh
 *   - findByUrl   GET  /api/links?url=（v1.1 精确已存检测，带 feature-detect）
 *   - findSubscriptionByUrl GET /api/subscriptions?url=（RSS 已订阅检测）
 *   - createSubscription POST /api/subscriptions（仅由用户明确点击触发）
 *   - testConnection  轻量鉴权探活（复用 GET /api/tags）
 *
 * 错误归一化（核心设计）：所有方法返回判别式联合 ApiResult<T>，调用方
 * 用 result.ok 分支，失败时 result.error.kind 精确区分五类错误：
 *   - unauthorized        401 / 403，Token 缺失或不匹配
 *   - network-unreachable 后端不可达（DNS 失败、连接拒绝、CORS、离线）
 *   - timeout             请求超时
 *   - rate-limited        429 限流 / 冷却中（refresh 冷却窗 cooldown_active、
 *                         全局 rate_limit_exceeded）。如后端给出 Retry-After 头，
 *                         其秒数落入 error.retryAfterSeconds，供「x 秒后可重试」文案。
 *   - other              其它（其余 4xx/5xx、配置缺失、响应解析失败、响应体形状不符等）
 * 调用方据此展示精确的中文提示，无需自行解析底层错误对象。
 *
 * 运行时响应校验（健壮性设计）：HTTP 200 不代表响应体可信——后端 bug、
 * 字段缺失、200 携带错误体、反代登录页（非 JSON 的 200）都可能流入 UI 导致
 * 渲染崩溃。因此每个 typed 方法在 HTTP 调用后用手写类型守卫校验响应形状，
 * 遵循「失败关闭」（fail closed）原则：
 *   - 顶层必需字段缺失/类型错（links.items 非数组、
 *     links.total/page/limit 非 number、200 实为 ApiErrorResponse、非对象/非 JSON）
 *     → 归一化为 { ok: false, error: { kind: 'other' } }，不冒充合法数据。
 *     绝不把缺字段的响应伪装成「空数据成功」——否则真实故障会被掩盖成数据丢失。
 *   - Link / Tag / SubmitResponse 在 wire 边界按 generated contract 完整
 *     校验；不补缺失字段、不过滤残缺数组项、不把非法 enum 强转为合法类型。
 *
 * 消费者：测试连接 UI（Task 2A）、知识库桌面（Phase 3 3A）、采集编排（Phase 3 3B）。
 */

import {
  buildQueryString,
  normalizeHttpError as normalizeSharedHttpError,
  normalizeThrownError as normalizeSharedThrownError,
  parseRetryAfter as parseRetryAfterValue,
  type ApiError,
  type ApiResult,
} from '@webtag/api'
import type {
  CapabilitiesResponse,
  ApiErrorResponse,
  GetTreeParams,
  IngestRequest,
  Link,
  LinkContentResponse,
  ListLinksParams,
  PaginatedLinksResponse,
  SessionIdentity,
  SubmitResponse,
  SubscriptionSummary,
  Tag,
  TreeResponse,
} from './types'
import { buildTreeFromLinks } from './tree-builder'
import {
  isWireCapabilitiesResponse,
  isWireLink,
  isWireLinkContentResponse,
  isWirePaginatedLinksResponse,
  isWireSubmitResponse,
  isWireTag,
} from './wire-guards'

// ── 错误归一化类型 ──────────────────────────────────────────

export type { ApiError, ApiResult } from '@webtag/api'

/**
 * findByUrl 的三态判别结果（v1.1 popup 已存检测的 feature-detect 载体）。
 *
 * 把「后端是否支持 url= 参数」与「是否命中」一并表达，让调用方无需自行解析
 * 列表形状即可安全分支：
 *   - { supported: false }                后端不支持 url= （旧后端）。调用方静默
 *                                         隐藏已存检测，绝不据此报「已在库中」。
 *   - { supported: true; link: Link }     命中：该 URL 已在库中。
 *   - { supported: true; link: null }     支持但未命中：该 URL 不在库中。
 */
export type FindByUrlResult =
  { supported: false } | { supported: true; link: Link | null }

// ── 客户端配置 ──────────────────────────────────────────────

/** 创建客户端所需的运行时配置，由调用方从 useSettings 读取后传入。 */
export interface WebTagClientConfig {
  /** 后端基础地址，如 http://localhost:8080。尾部斜杠会被规整。 */
  baseURL: string
  /** 单安装后端的静态访问 Token。空串时不注入 Authorization 头。 */
  token: string
}

/** 默认请求超时（毫秒）。与 NaiveTab 既有 api/request.ts 的 8s 保持一致。 */
const DEFAULT_TIMEOUT = 8000

// ── 内部工具 ────────────────────────────────────────────────

/** 去掉 baseURL 尾部斜杠，避免与各 path 的前导斜杠拼出双斜杠。 */
function normalizeBaseURL(raw: string): string {
  return raw.trim().replace(/\/+$/, '')
}

/** 类型守卫：判断值是否为非空普通对象（用于后续逐字段校验的前置判定）。 */
function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** 类型守卫：判断响应体是否为后端统一错误体形状。 */
function isApiErrorResponse(data: unknown): data is ApiErrorResponse {
  return (
    isPlainObject(data) &&
    'error' in data &&
    isPlainObject((data as { error: unknown }).error)
  )
}

/**
 * 从响应头解析 Retry-After 秒数。后端对 429（冷却 / 限流）以「整数秒」形式
 * 写 Retry-After（见后端 handler/errors.go）。这里只解析「纯整数秒」格式：
 *   - 头缺失 / 非正整数 / HTTP-date 形式 → 返回 undefined（调用方退回通用文案）。
 * 不实现 HTTP-date 解析：后端只发整数秒，且过度兼容不值得为此引入时区/解析风险。
 */
function parseRetryAfter(headers: Headers): number | undefined {
  return parseRetryAfterValue(headers.get('Retry-After'))
}

// ── 网络层错误 ──────────────────────────────────────────────

/**
 * 把请求过程中抛出的异常归一化为 ApiError。
 * - AbortError（AbortController 触发的超时）→ timeout
 * - TypeError（fetch 在网络层失败，浏览器统一抛 "Failed to fetch"）→ network-unreachable
 * - 其它同步/异步异常 → other
 */
function normalizeThrownError(err: unknown): ApiError {
  return normalizeSharedThrownError(err, DEFAULT_TIMEOUT)
}

/**
 * 把非 2xx 的 HTTP 响应归一化为 ApiError。
 * - 401 / 403 → unauthorized
 * - 429       → rate-limited（refresh per-link 冷却窗 cooldown_active、
 *               全局限流 rate_limit_exceeded 等都走此类），并在拿到 Retry-After
 *               秒数时一并带上，供调用方渲染「x 秒后可重试」。
 * - 其它非 2xx → other
 * 都尝试从响应体（若为后端统一错误体）提取 error_code 与 message。
 *
 * @param status            HTTP 状态码
 * @param body              已解析的响应体（解析失败时为 undefined）
 * @param retryAfterSeconds 从 Retry-After 响应头解析出的秒数（无 / 非法时为 undefined）；
 *                          仅在 429 时落入返回的 ApiError.retryAfterSeconds。
 */
function normalizeHttpError(
  status: number,
  body: unknown,
  retryAfterSeconds?: number,
): ApiError {
  return normalizeSharedHttpError(status, body, retryAfterSeconds)
}

// ── 运行时响应校验 ───────────────────────────────────────────
//
// HTTP 200 不等于响应体可信。generated response 必须完整匹配；残缺字段、
// 非法 enum 或坏数组项一律判为 other，不修补后冒充合法 wire data。

/** 形状不符时统一构造的 ApiError。message 指明哪个接口、哪里不对。 */
function shapeError(detail: string): ApiError {
  return { kind: 'other', message: `响应体格式不符：${detail}` }
}

/**
 * 校验 GET /api/links 响应。
 *
 * 失败关闭（fail closed）：顶层必需字段缺失或类型错 → other 错误。
 *   - items 不是数组
 *   - total / page / limit 缺失或不是 number
 * 这四个字段都是分页契约的硬要求，缺任意一个都意味着响应不可信。
 * 不把缺字段的响应伪装成「空知识库」，否则真实故障会被掩盖成数据丢失。
 *
 * items 中每一项都必须满足 generated LinkResponse；任一残缺项都会让整个
 * 响应失败关闭。next_cursor 是 omitempty 可选字段，存在时必须为字符串。
 */
function validateLinksResponse(
  body: unknown,
): ApiResult<PaginatedLinksResponse> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (!isWirePaginatedLinksResponse(body)) {
    return {
      ok: false,
      error: shapeError('GET /api/links 响应体不是完整 PaginatedLinksResponse'),
    }
  }
  return { ok: true, data: body }
}

/**
 * 校验并归一化 GET /api/tags 响应。
 * 通过条件：body 是数组（后端契约 GET /api/tags 直接返回 Tag[]）。
 * 数组中每一项都必须满足 generated TagCountResponse；任一残缺项都会让
 * 整个响应失败关闭。
 */
function validateTagsResponse(body: unknown): ApiResult<Tag[]> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (!Array.isArray(body)) {
    return { ok: false, error: shapeError('GET /api/tags 响应体不是数组') }
  }
  if (!body.every(isWireTag)) {
    return {
      ok: false,
      error: shapeError('GET /api/tags 响应体包含残缺 Tag'),
    }
  }
  return { ok: true, data: body }
}

/**
 * 校验 GET /api/links/{id} 响应。
 */
function validateLinkResponse(body: unknown): ApiResult<Link> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (!isWireLink(body)) {
    return {
      ok: false,
      error: shapeError('GET /api/links/{id} 响应体不是完整 LinkResponse'),
    }
  }
  return { ok: true, data: body }
}

/**
 * 校验 POST /api/ingest 响应。
 * body 必须满足 generated SubmitResponse；status 仅允许 generated enum，
 * 且必须带有 link_id 或 inbox_id 之一。Inbox proposal 状态由
 * GET /api/inbox/{id} 提供。
 */
function validateSubmitResponse(body: unknown): ApiResult<SubmitResponse> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (!isWireSubmitResponse(body)) {
    return {
      ok: false,
      error: shapeError('POST /api/ingest 响应体不是完整 SubmitResponse'),
    }
  }
  const linkID = body.link_id?.trim()
  const inboxID = body.inbox_id?.trim()
  const hasLinkID = Boolean(linkID)
  const hasInboxID = Boolean(inboxID)

  // SubmitResponse is a closed union at the extension boundary even though
  // the generated schema keeps the two identities optional for legacy JSON.
  // Accepting both IDs, or an ID that contradicts destination, would make the
  // capture handler choose a durable target by accident.
  if (hasLinkID === hasInboxID) {
    return {
      ok: false,
      error: shapeError(
        'POST /api/ingest 必须且只能返回 link_id 或 inbox_id 之一',
      ),
    }
  }
  if (
    (body.link_id !== undefined && !hasLinkID) ||
    (body.inbox_id !== undefined && !hasInboxID)
  ) {
    return {
      ok: false,
      error: shapeError('SubmitResponse 的 durable identity 不能为空'),
    }
  }
  if (body.destination === 'library' && !hasLinkID) {
    return {
      ok: false,
      error: shapeError('library 响应必须带 link_id'),
    }
  }
  if (body.destination === 'inbox' && !hasInboxID) {
    return {
      ok: false,
      error: shapeError('inbox 响应必须带 inbox_id'),
    }
  }
  return { ok: true, data: body }
}

const REPRESENTATION_CONTRACT = 'v3'
const DATA_NAMESPACE_HEADER = 'X-WebTag-Data-Namespace'

function validateSessionIdentity(body: unknown): ApiResult<SessionIdentity> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (
    !isPlainObject(body) ||
    typeof body.client_data_namespace !== 'string' ||
    body.client_data_namespace.trim() === '' ||
    body.representation_contract !== REPRESENTATION_CONTRACT
  ) {
    return {
      ok: false,
      error: shapeError('GET /api/session 响应体不是完整 SessionIdentity'),
    }
  }
  return { ok: true, data: body as SessionIdentity }
}

/**
 * 只校验扩展徽标所需的活跃待确认数。`counts.inbox` 不包含已过期分区；
 * `counts.inbox_expired` 由 Reader 单独展示，扩展不扫描或合并 Inbox 记录。
 */
function validateReaderPendingCount(body: unknown): ApiResult<number> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (
    !isPlainObject(body) ||
    !isPlainObject(body.counts) ||
    typeof body.counts.inbox !== 'number' ||
    !Number.isInteger(body.counts.inbox) ||
    body.counts.inbox < 0
  ) {
    return {
      ok: false,
      error: shapeError('GET /api/home 缺少合法的 counts.inbox'),
    }
  }
  return { ok: true, data: body.counts.inbox }
}

/** 校验 POST /api/links/{id}/content 的完整原文保存响应。 */
function validateLinkContentResponse(
  body: unknown,
): ApiResult<LinkContentResponse> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  if (!isWireLinkContentResponse(body)) {
    return {
      ok: false,
      error: shapeError(
        'POST /api/links/{id}/content 响应体不是完整 LinkContentResponse',
      ),
    }
  }
  return { ok: true, data: body }
}

function normalizeSubscription(value: unknown): SubscriptionSummary | null {
  if (!isPlainObject(value) || typeof value.id !== 'string' || !value.id) {
    return null
  }
  const url =
    typeof value.feed_url === 'string'
      ? value.feed_url
      : typeof value.url === 'string'
        ? value.url
        : ''
  if (!url) return null
  const title =
    typeof value.title === 'string'
      ? value.title
      : typeof value.name === 'string'
        ? value.name
        : ''
  return { id: value.id, url, title }
}

function subscriptionValues(body: unknown): unknown[] | null {
  if (Array.isArray(body)) return body
  if (!isPlainObject(body)) return null
  if (Array.isArray(body.items)) return body.items
  if (Array.isArray(body.subscriptions)) return body.subscriptions
  if ('subscription' in body) return [body.subscription]
  if ('id' in body) return [body]
  return null
}

function validateSubscriptionList(
  body: unknown,
): ApiResult<SubscriptionSummary[]> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  const values = subscriptionValues(body)
  if (!values) {
    return {
      ok: false,
      error: shapeError('GET /api/subscriptions 响应体形状无效'),
    }
  }
  const subscriptions = values.map(normalizeSubscription)
  if (subscriptions.some((value) => value === null)) {
    return {
      ok: false,
      error: shapeError('GET /api/subscriptions 响应包含残缺订阅'),
    }
  }
  return { ok: true, data: subscriptions as SubscriptionSummary[] }
}

function validateCreatedSubscription(
  body: unknown,
): ApiResult<SubscriptionSummary> {
  if (isApiErrorResponse(body)) {
    return { ok: false, error: normalizeHttpError(200, body) }
  }
  const values = subscriptionValues(body)
  const subscription =
    values?.length === 1 ? normalizeSubscription(values[0]) : null
  if (!subscription) {
    return {
      ok: false,
      error: shapeError('POST /api/subscriptions 响应体形状无效'),
    }
  }
  return { ok: true, data: subscription }
}

// ── 客户端 ──────────────────────────────────────────────────

/**
 * 内部统一的请求描述。
 *
 * query 取扩展实际消费的两个查询参数接口的并集（GetTreeParams / ListLinksParams）。
 * 二者字段值均为 `string | number | undefined`；buildURL 跳过 undefined、
 * 对其余值做 String() 转换。
 */
interface RequestSpec {
  method: 'GET' | 'POST'
  /** 相对路径，以 / 开头，如 /api/tree。 */
  path: string
  /** 查询参数。undefined 值跳过，其余值经 String() 转换。 */
  query?: GetTreeParams | ListLinksParams | { url: string }
  /** POST 请求体，会被 JSON.stringify。 */
  body?: unknown
}

interface RequestOptions {
  /** Stable key for replaying the exact same mutation after an ambiguous result. */
  idempotencyKey?: string
  /** Retry one network/timeout result with the same request and key. */
  retryOnAmbiguous?: boolean
}

/** Options accepted by mutation methods without changing their old call shape. */
export interface MutationRequestOptions {
  idempotencyKey?: string
}

/** Generate an opaque key when a caller does not need to manage persistence itself. */
export function createIdempotencyKey(): string {
  try {
    if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) {
      return crypto.randomUUID()
    }
  } catch {
    // Fall through to the non-cryptographic browser-compatible fallback.
  }
  return `webtag-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

function isAmbiguousResult(result: ApiResult<unknown>): boolean {
  return (
    !result.ok &&
    (result.error.kind === 'network-unreachable' ||
      result.error.kind === 'timeout' ||
      result.error.status === undefined)
  )
}

/**
 * WebTag API 客户端。一个实例对应一组后端地址 + Token。
 * 设置变更后应重新创建实例（见 createWebTagClient）。
 */
export class WebTagClient {
  private readonly baseURL: string
  /** 单安装后端的静态 token，无则空串。 */
  private readonly staticToken: string

  constructor(config: WebTagClientConfig) {
    this.baseURL = normalizeBaseURL(config.baseURL)
    this.staticToken = config.token.trim()
  }

  /** 构造请求头：注入 Authorization 与可选幂等 key。 */
  private buildHeaders(
    token: string,
    idempotencyKey?: string,
  ): Record<string, string> {
    const headers: Record<string, string> = {}
    if (token) headers.Authorization = `Bearer ${token}`
    if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey
    return headers
  }

  /** 把 baseURL + path + query 拼成完整 URL。query 用 URLSearchParams 编码。 */
  private buildURL(spec: RequestSpec): string {
    let url = `${this.baseURL}${spec.path}`
    if (spec.query) {
      const qs = buildQueryString({ ...spec.query })
      if (qs) url += `?${qs}`
    }
    return url
  }

  /**
   * 发一次请求（不含 401 重试）：用 fetch + AbortController（DEFAULT_TIMEOUT 超时）
   * 发请求，把网络异常 / 非 2xx / 响应体解析失败统一收敛为 ApiResult。
   * 不抛异常。校验响应「形状」是各 typed 方法通过 validate 回调完成的。
   *
   * 超时覆盖范围（关键）：AbortController 的超时计时器在 fetch 发起到
   * 响应体完整读取（res.text()）并 JSON 解析完成的整个区间保持活跃，
   * 只在请求彻底结束（成功或失败）后才在 finally 中 clearTimeout。
   * 因此后端「已回 headers 但 body 永远不来」的挂起场景：计时器到点触发
   * controller.abort()，正在进行的 res.text() 会抛出 AbortError，经
   * normalizeThrownError 归一化为 { kind: 'timeout' }，不会无限挂起。
   *
   * 成功路径返回 { ok: true, data: <已解析的原始 body> }，data 是 unknown；
   * 由调用方的 validate 回调进一步校验并归一化。
   *
   * @param spec  请求描述
   * @param token 本次请求使用的 Bearer token（由 request 解析后传入）
   * @returns 已解析 body 的 ApiResult<unknown>，或归一化的失败结果
   */
  private async sendOnce(
    spec: RequestSpec,
    token: string,
    idempotencyKey?: string,
  ): Promise<ApiResult<unknown>> {
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT)
    try {
      const init: RequestInit = {
        method: spec.method,
        headers: this.buildHeaders(token, idempotencyKey),
        signal: controller.signal,
      }
      if (spec.body !== undefined) {
        ;(init.headers as Record<string, string>)['Content-Type'] =
          'application/json'
        init.body = JSON.stringify(spec.body)
      }

      // 发请求。网络层失败（DNS、连接拒绝、CORS、离线）抛 TypeError；
      // 超时由 controller.abort() 触发，抛 AbortError。
      const res = await fetch(this.buildURL(spec), init)

      // 解析响应体。此处计时器仍活跃：若后端回了 headers 但 body 挂起，
      // 计时器到点会 abort，res.text() 抛 AbortError → 下方 catch 归一化为 timeout。
      // 非 JSON 的 200（如反代登录页）会在 JSON.parse 抛出，归为 other。
      let body: unknown
      try {
        const text = await res.text()
        body = text.length > 0 ? JSON.parse(text) : undefined
      } catch (parseErr) {
        // body 读取被超时 abort：归一化为 timeout，而非伪装成解析失败。
        if (
          parseErr instanceof DOMException &&
          parseErr.name === 'AbortError'
        ) {
          return { ok: false, error: normalizeThrownError(parseErr) }
        }
        if (parseErr instanceof Error && parseErr.name === 'AbortError') {
          return { ok: false, error: normalizeThrownError(parseErr) }
        }
        if (!res.ok) {
          // 非 2xx 且响应体非 JSON：按状态码归一化，不依赖响应体。
          // 429 仍可能带 Retry-After 头（与响应体是否为 JSON 无关），照样解析。
          return {
            ok: false,
            error: normalizeHttpError(
              res.status,
              undefined,
              parseRetryAfter(res.headers),
            ),
          }
        }
        // 2xx 但响应体不是 JSON（典型：反代登录页 HTML）→ other。
        return {
          ok: false,
          error: shapeError(
            `${spec.method} ${spec.path} 返回非 JSON 响应（HTTP ${res.status}）`,
          ),
        }
      }

      if (!res.ok) {
        // 非 2xx（JSON 错误体）：429 时附带 Retry-After 秒数（如有）。
        return {
          ok: false,
          error: normalizeHttpError(
            res.status,
            body,
            parseRetryAfter(res.headers),
          ),
        }
      }
      if (
        spec.method === 'GET' &&
        spec.path === '/api/session' &&
        isPlainObject(body) &&
        typeof body.client_data_namespace === 'string' &&
        body.client_data_namespace.trim() !== '' &&
        body.representation_contract === REPRESENTATION_CONTRACT
      ) {
        const marker = res.headers.get(DATA_NAMESPACE_HEADER)
        if (marker !== body.client_data_namespace) {
          return {
            ok: false,
            error: {
              kind: 'identity-mismatch',
              message: 'GET /api/session namespace marker mismatch',
            },
          }
        }
      }
      return { ok: true, data: body }
    } catch (err) {
      // 覆盖 fetch 抛错（TypeError / AbortError）与其它同步异常。
      return { ok: false, error: normalizeThrownError(err) }
    } finally {
      // 只在请求彻底结束（body 已读取并解析完，或已失败）后清计时器。
      clearTimeout(timer)
    }
  }

  /** 统一请求底座。静态安装令牌失效时直接返回 unauthorized。 */
  private async request(
    spec: RequestSpec,
    options: RequestOptions = {},
  ): Promise<ApiResult<unknown>> {
    const result = await this.sendOnce(
      spec,
      this.staticToken,
      options.idempotencyKey,
    )
    if (options.retryOnAmbiguous && isAmbiguousResult(result)) {
      return this.sendOnce(spec, this.staticToken, options.idempotencyKey)
    }
    return result
  }

  /**
   * 发请求并用 validate 回调对响应体做运行时形状校验 / 归一化。
   * 网络层 / HTTP 层失败直接透传；HTTP 成功时把原始 body 交给 validate。
   */
  private async requestValidated<T>(
    spec: RequestSpec,
    validate: (body: unknown) => ApiResult<T>,
    options?: RequestOptions,
  ): Promise<ApiResult<T>> {
    const res = await this.request(spec, options)
    if (!res.ok) return res
    return validate(res.data)
  }

  /**
   * getTree — 通过 /api/links 分页拉完当前范围的 done 链接，再在前端按 URL
   * 现算层级树。保留同名方法给上层调用，避免知识库桌面改契约。
   */
  async getTree(params?: GetTreeParams): Promise<ApiResult<TreeResponse>> {
    const items: Link[] = []
    let after = ''
    for (;;) {
      const page = await this.getLinks({
        ...params,
        status: 'done',
        limit: 100,
        after,
      })
      if (!page.ok) return { ok: false, error: page.error }
      items.push(...page.data.items)
      if (!page.data.next_cursor) break
      after = page.data.next_cursor
    }
    return { ok: true, data: buildTreeFromLinks(items) }
  }

  /**
   * GET /api/links — 按筛选与分页拉取已 done 链接。
   * 查询参数语义见 ListLinksParams；offset 与 cursor 模式互斥由调用方保证。
   *
   * 响应经运行时校验（失败关闭）：顶层 items 非数组、或 total/page/limit
   * 缺失/非 number → other 错误；items 中任一 Link 不满足 generated contract
   * 也让整个响应失败关闭。响应体非对象或为错误体 → other 错误。
   */
  getLinks(
    params?: ListLinksParams,
  ): Promise<ApiResult<PaginatedLinksResponse>> {
    return this.requestValidated(
      { method: 'GET', path: '/api/links', query: params },
      validateLinksResponse,
    )
  }

  /**
   * GET /api/tags — 获取标签聚合列表（按 count 降序，上限 1000 条）。
   *
   * 响应经运行时校验：必须是完整 TagCountResponse 数组；任一残缺项
   * 都让整个响应失败关闭。
   */
  getTags(): Promise<ApiResult<Tag[]>> {
    return this.requestValidated(
      { method: 'GET', path: '/api/tags' },
      validateTagsResponse,
    )
  }

  /** Probe additive collection capabilities. A 404 is an older backend, not an error. */
  async getCapabilities(): Promise<ApiResult<CapabilitiesResponse | null>> {
    const result = await this.requestValidated(
      { method: 'GET', path: '/api/capabilities' },
      (body) =>
        isWireCapabilitiesResponse(body)
          ? { ok: true, data: body }
          : {
              ok: false,
              error: {
                kind: 'other',
                message: 'invalid capabilities response',
              },
            },
    )
    if (!result.ok && result.error.status === 404)
      return { ok: true, data: null }
    return result
  }

  /** Read the authenticated namespace used to isolate extension-local caches. */
  getSessionIdentity(): Promise<ApiResult<SessionIdentity>> {
    return this.requestValidated(
      { method: 'GET', path: '/api/session' },
      validateSessionIdentity,
    )
  }

  /**
   * 读取活跃 Inbox 待确认数，不要求完整 Reader Home 响应。
   * 404 表示旧后端，返回 `data: null`，调用方可清除旧徽标而不把兼容性当作 API 失败。
   */
  async getReaderPendingCount(): Promise<ApiResult<number | null>> {
    const result = await this.requestValidated(
      { method: 'GET', path: '/api/home' },
      validateReaderPendingCount,
    )
    if (!result.ok && result.error.status === 404) {
      return { ok: true, data: null }
    }
    return result
  }

  /**
   * GET /api/links/{link_id} — 查询产品可见的解析状态与结果。
   */
  getLink(linkId: string): Promise<ApiResult<Link>> {
    return this.requestValidated(
      { method: 'GET', path: `/api/links/${encodeURIComponent(linkId)}` },
      validateLinkResponse,
    )
  }

  /**
   * POST /api/ingest — 多源采集提交。
   * 扩展采集当前页时 sources 内为单条 browser_capture。返回 SubmitResponse。
   *
   * 响应经运行时校验：必须满足 generated SubmitResponse，status 只能是
   * pending / processing / done / failed；library 响应带 link_id，Inbox
   * 响应带 inbox_id。
   */
  ingest(
    body: IngestRequest,
    options: MutationRequestOptions = {},
  ): Promise<ApiResult<SubmitResponse>> {
    const idempotencyKey = options.idempotencyKey ?? createIdempotencyKey()
    return this.requestValidated(
      { method: 'POST', path: '/api/ingest', body },
      validateSubmitResponse,
      { idempotencyKey, retryOnAmbiguous: true },
    )
  }

  /**
   * POST /api/links/{link_id}/content — 保存当前链接的原文快照。
   * 浏览器采集来源由后端优先使用已提交的脱敏 HTML / 可读文本，不重新抓 URL。
   */
  saveLinkContent(linkId: string): Promise<ApiResult<LinkContentResponse>> {
    return this.requestValidated(
      {
        method: 'POST',
        path: `/api/links/${encodeURIComponent(linkId)}/content`,
      },
      validateLinkContentResponse,
    )
  }

  /**
   * POST /api/links/{link_id}/refresh — 对已存在链接重新入队解析。
   * 知识库「失败」分区的「重试」按钮调用：对 failed 链接重新抓取 + 重新分析。
   * 后端返回 202 + SubmitResponse（含新的 job 状态），与 ingest 同形状，
   * 复用 validateSubmitResponse 校验；该 endpoint 的成功响应应含 link_id。
   *
   * link_id 经 encodeURIComponent 编码，防止特殊字符破坏路径。
   */
  refreshLink(
    linkId: string,
    options: MutationRequestOptions = {},
  ): Promise<ApiResult<SubmitResponse>> {
    const idempotencyKey = options.idempotencyKey ?? createIdempotencyKey()
    return this.requestValidated(
      {
        method: 'POST',
        path: `/api/links/${encodeURIComponent(linkId)}/refresh`,
      },
      validateSubmitResponse,
      { idempotencyKey, retryOnAmbiguous: true },
    )
  }

  /** Check whether a feed URL already has an active subscription. */
  async findSubscriptionByUrl(
    url: string,
  ): Promise<ApiResult<SubscriptionSummary | null>> {
    const result = await this.requestValidated(
      { method: 'GET', path: '/api/subscriptions', query: { url } },
      validateSubscriptionList,
    )
    if (!result.ok) {
      // Some implementations use a resource-style 404 for a missing URL.
      if (result.error.status === 404) return { ok: true, data: null }
      return result
    }
    return { ok: true, data: result.data[0] ?? null }
  }

  /** Subscribe only after an explicit user click in the popup. */
  createSubscription(url: string): Promise<ApiResult<SubscriptionSummary>> {
    return this.requestValidated(
      { method: 'POST', path: '/api/subscriptions', body: { url } },
      validateCreatedSubscription,
    )
  }

  /**
   * GET /api/links?url=<url> — 精确 URL 已存检测（v1.1 / 后端 Spec v3.1）。
   *
   * 用于 popup 关系面板的「已在库中」检测：传入当前页完整 URL，后端按 url 精确
   * 匹配返回 0 / 1 条。命中（恰一条且 URL 一致）→ 该页已在库中。
   *
   * ⚠️ feature-detect（核心）：`url` 参数由后端 Phase 9 实现。旧后端不识别它，
   * 会忽略该参数按普通列表返回（全量最新 done 链接）。本方法在客户端就把这种
   * 「不支持」判出来，向调用方返回三态判别结果 FindByUrlResult，绝不让旧后端
   * 的普通列表被误读成「已在库中」：
   *   - supported: false              后端不支持（返回多于 1 条，或唯一一条 URL
   *                                   与查询不一致）→ 视为旧后端，调用方静默隐藏。
   *   - supported: true, link: Link   命中：恰一条且 URL 与查询完全一致。
   *   - supported: true, link: null   支持但未命中：0 条。
   *
   * limit=2 而非 1：旧后端忽略 url 时普通列表通常 > 1 条，多取一条让「返回多于 1 条」
   * 这一不支持信号更易触发；新后端精确匹配至多 1 条，limit=2 不影响命中判定。
   *
   * 网络 / HTTP / 形状错误：透传底层 ApiResult.error（kind 见 ApiError），
   * 调用方据此 fail-soft（隐藏面板，不阻塞采集）。
   *
   * @param url 当前页完整 URL（规范化前的原始 URL，与后端存储键一致）
   */
  async findByUrl(url: string): Promise<ApiResult<FindByUrlResult>> {
    const res = await this.getLinks({ url, limit: 2 })
    if (!res.ok) return res
    const { items } = res.data
    // 旧后端忽略 url：普通列表返回 ≥ 2 条 → 判为不支持。
    if (items.length > 1) {
      return { ok: true, data: { supported: false } }
    }
    // 0 条：支持但未命中。
    if (items.length === 0) {
      return { ok: true, data: { supported: true, link: null } }
    }
    // 恰 1 条：URL 与查询一致才算命中（防旧后端恰好库里只有 1 条时误判）。
    // 后端存储/返回的是 Go ParseRequestURI().String() 归一化后的 URL——
    // 它把浏览器 tab.url 中的 "#"（fragment）当作路径字符编码为 "%23"，
    // 所以带锚点的页面用严格 === 会对真实命中误判"未命中"。比较时额外
    // 接受 "#"→"%23" 的归一化变体；该宽松不会引入误报——即使是旧后端
    // 凑巧返回的单条，URL 等于查询变体本身就意味着该页确实在库中。
    const only = items[0]
    const normalizedQuery = url.replace(/#/g, '%23')
    if (only.url === url || only.url === normalizedQuery) {
      return { ok: true, data: { supported: true, link: only } }
    }
    // 唯一一条但 URL 不一致 → 旧后端忽略了 url 参数，判为不支持。
    return { ok: true, data: { supported: false } }
  }

  /**
   * 测试连接 — 用当前 baseURL + Token 调一个轻量鉴权接口（GET /api/tags），
   * 验证地址可达且 Token 有效。
   * 成功返回 ok:true；失败时 error.kind 区分 401（Token 错）/ 不可达 / 超时 / 其它。
   */
  async testConnection(): Promise<ApiResult<true>> {
    const res = await this.getTags()
    if (res.ok) {
      return { ok: true, data: true }
    }
    return { ok: false, error: res.error }
  }
}

/**
 * 工厂函数：按配置创建一个新的 WebTagClient 实例。
 * 配置（后端地址 / Token）变更后应调用此函数重建，不要复用旧实例。
 */
export function createWebTagClient(config: WebTagClientConfig): WebTagClient {
  return new WebTagClient(config)
}

/**
 * 从扩展设置构造 WebTagClient —— 「未配置后端地址」的统一判定入口。
 *
 * 把多处调用点（useLibrary、background 采集依赖、BackendSettings、
 * useLibraryRelation、libraryReview）此前各自重复的「trim 地址 / Token + 空地址
 * 视为未配置」逻辑收口到一处：
 *   - 同时 trim backendUrl 与 accessToken；
 *   - backendUrl trim 后为空 → 返回 null（调用方据此报「未配置」，不发请求）；
 *   - 否则用 trim 后的值 createWebTagClient。
 *
 * 入参刻意取结构化的 settings（而非 import WebTagSettings），避免 api 层反向依赖
 * composables，防止模块环。
 */
export function buildWebTagClientFromSettings(settings: {
  backendUrl: string
  accessToken: string
}): WebTagClient | null {
  const baseURL = settings.backendUrl.trim()
  if (!baseURL) return null
  return createWebTagClient({
    baseURL,
    token: settings.accessToken.trim(),
  })
}
