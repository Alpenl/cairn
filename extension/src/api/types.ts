/**
 * @module api/types
 * WebTag 后端 API 的 TypeScript 类型定义。
 *
 * 线缆形状由 internal/app/assets/openapi.json 自动生成。本模块只提供扩展侧
 * 领域命名。
 *
 * 消费者：src/api/webtag-client.ts、采集、RSS 订阅与 Reader compatibility probes。
 */

import type {
  CapabilitiesResponse as WireCapabilitiesResponse,
  ErrorResponse as WireErrorResponse,
  IngestRequest as WireIngestRequest,
  IngestSource as WireIngestSource,
  LinkContentResponse as WireLinkContentResponse,
  LinkResponse,
  SessionIdentity as WireSessionIdentity,
  SubmitResponse as WireSubmitResponse,
} from '@webtag/api/generated'

// ── 枚举类型 ────────────────────────────────────────────────

/** 链接内容形态。 */

/**
 * 采集来源类型。对应 internal/dto/request.go 的 IngestSource.Kind oneof。
 * 扩展采集当前页时固定使用 `browser_capture`。
 */

/**
 * 归一化后的 API 错误类别。调用方 switch 此字段决定提示文案。
 *
 * 放在中立的契约模块，让消息协议层（background/capture-protocol.ts）
 * 等不依赖重型 API 客户端模块的代码也能直接引用此类型。
 * webtag-client.ts 负责把 fetch 网络异常、非 2xx 响应与响应体形状不符
 * 归一化为此类别。
 *   - unauthorized        401 / 403，Token 缺失或不匹配
 *   - network-unreachable 后端不可达（DNS 失败、连接拒绝、CORS、离线）
 *   - timeout             请求超时
 *   - rate-limited        429 限流 / 冷却中（含 refresh per-link 冷却窗
 *                         cooldown_active、全局 rate_limit_exceeded）。
 *                         单列此类，让调用方能区分「冷却中（稍后会成功）」
 *                         与真失败，并据 ApiError.retryAfterSeconds 给出
 *                         「x 秒后可重试」的针对性提示。
 *   - other              其它（其余 4xx/5xx、配置缺失、响应解析失败等）
 */
export type { ApiErrorKind } from '@webtag/api'

/** Additive server capability flags used for progressive collection rollout. */
export type CapabilitiesResponse = WireCapabilitiesResponse

/** Authenticated data namespace used to partition extension-local caches. */
export type SessionIdentity = WireSessionIdentity

// ── 响应 DTO ────────────────────────────────────────────────

/** 单条链接视图。 */
export type Link = LinkResponse

/**
 * 提交结果。对应 internal/dto/response.go 的 SubmitResponse。
 * Library/Site 返回 link_id；Inbox 返回 inbox_id。
 */
export type SubmitResponse = WireSubmitResponse

/** POST /api/links/{link_id}/content 保存后的原文快照。 */
export type LinkContentResponse = WireLinkContentResponse

/**
 * Minimal subscription shape consumed by the extension popup. The backend's
 * canonical wire field is `feed_url`; WebTagClient normalizes it to `url` so
 * UI code never depends on storage naming.
 */
export interface SubscriptionSummary {
  id: string
  url: string
  title: string
}

/** 后端所有失败响应的统一外层包裹。对应 internal/dto/response.go 的 ErrorResponse。 */
export type ApiErrorResponse = WireErrorResponse

// ── 请求 DTO ────────────────────────────────────────────────

/**
 * 一条采集来源。对应 internal/dto/request.go 的 IngestSource。
 * 扩展采集当前页时 kind 固定为 `browser_capture`，携带 url/title、
 * 最多 512 KiB UTF-8 的脱敏可读文本、同样上限的脱敏正文 HTML 结构快照、
 * 基础 metadata 与可选 note。
 *
 * html 不是可选的锦上添花：正文的标题、段落、列表、代码块只存在于它里面，
 * text 是 innerText 压平的结果。后端靠 html 转出阅读用的 Markdown，
 * 不发它就只能存一堆文字。image_urls 仍固定为空。
 */
export type IngestSource = WireIngestSource

/**
 * POST /api/ingest 请求体。对应 internal/dto/request.go 的 IngestRequest。
 * sources 长度 1–64。
 */
export type IngestRequest = WireIngestRequest
