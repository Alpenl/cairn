/**
 * @module api/types
 * WebTag 后端 API 的 TypeScript 类型定义。
 *
 * 线缆形状由 internal/app/assets/openapi.json 自动生成。本模块只提供扩展侧
 * 领域命名，并补充 URL 树构建器使用的本地虚拟节点字段。
 *
 * 消费者：src/api/webtag-client.ts、知识库桌面与采集功能（Phase 3+）。
 */

import type {
  paths as WirePaths,
  CapabilitiesResponse as WireCapabilitiesResponse,
  ErrorDetail as WireErrorDetail,
  ErrorResponse as WireErrorResponse,
  IngestRequest as WireIngestRequest,
  IngestSource as WireIngestSource,
  JobResponse,
  LinkContentResponse as WireLinkContentResponse,
  LinkResponse,
  PaginatedLinksResponse as WirePaginatedLinksResponse,
  SessionIdentity as WireSessionIdentity,
  SubmitResponse as WireSubmitResponse,
  TagCountResponse,
  TreeNodeResponse,
  TreeResponse as WireTreeResponse,
} from '@webtag/api/generated'

// ── 枚举类型 ────────────────────────────────────────────────

/** 链接内容形态。 */

/**
 * 链接处理状态。对应 openapi LinkResponse.status 枚举。
 * `skeleton` 仅为兼容历史数据保留；新的树层级不再依赖后端占位祖先行。
 */

/** 解析任务状态。对应 openapi JobResponse.status / SubmitResponse.status 枚举。 */
export type JobStatus = WireSubmitResponse['status']

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
 * URL 层级树节点。对应 internal/dto/response.go 的 TreeNodeResponse。
 * 字段与 Link 基本对应，额外带 `children` 子节点数组与 `truncated` 截断标记。
 * 注意 TreeNodeResponse 没有 parent_path / error_category / error_msg。
 */
export type TreeNode = Omit<TreeNodeResponse, 'children' | 'status'> & {
  /** 前端按 URL 现算出的虚拟路径节点，不对应数据库链接行。 */
  virtual?: boolean
  /** `virtual` 只存在于本地 URL 补位节点，不属于后端 wire enum。 */
  status: TreeNodeResponse['status'] | 'virtual'
  children: TreeNode[]
}

/**
 * getTree 的树响应。客户端由 /api/links 分页结果按 URL 现算；形状兼容 openapi TreeResponse。
 * `total` 是命中的真实链接数，不含虚拟路径节点。
 */
export interface TreeResponse extends Omit<WireTreeResponse, 'nodes'> {
  nodes: TreeNode[]
}

/**
 * 标签聚合项。对应 internal/dto/response.go 的 TagCountResponse。
 * GET /api/tags 返回 Tag[]（按 count 降序，上限 1000 条）。
 */
export type Tag = TagCountResponse

/**
 * GET /api/links 分页响应。对应 internal/dto/response.go 的 PaginatedLinksResponse。
 * offset 模式（page=）下 total/page 有效；cursor 模式（after=）下二者为 0，
 * 用 next_cursor 续传。next_cursor 仅在响应长度等于 limit（满页）时出现。
 */
export type PaginatedLinksResponse = WirePaginatedLinksResponse

/**
 * 提交结果。对应 internal/dto/response.go 的 SubmitResponse。
 * `job_id` 在命中已 done 链接时缺席（后端 omitempty），故为可选。
 */
export type SubmitResponse = WireSubmitResponse

/** POST /api/links/{link_id}/content 保存后的原文快照。 */
export type LinkContentResponse = WireLinkContentResponse

/**
 * 解析任务视图。对应 internal/dto/response.go 的 JobResponse。
 * status=done 时 `link` 内嵌完整 Link 快照，其余状态下可能为 null。
 */
export type Job = JobResponse

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

/**
 * 后端统一错误体的内层细节。对应 internal/dto/response.go 的 ErrorDetail。
 * 客户端应基于稳定的 `error_code` slug 分支，而非解析 `message`。
 */

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

// ── 查询参数 ────────────────────────────────────────────────

/** GET /api/links 查询参数直接派生自 generated path contract。 */
export type ListLinksParams = NonNullable<
  WirePaths['/api/links']['get']['parameters']['query']
>

/**
 * getTree 查询参数。当前客户端将 domain 转换为 /api/links 的 domain 参数。
 * 后端还支持 view=domains 的域名树视图，但扩展 v1 不消费，故此处不收录该参数。
 */
export interface GetTreeParams {
  domain?: string
}
