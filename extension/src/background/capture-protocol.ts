/**
 * @module background/capture-protocol
 * 当前页采集功能的 popup ↔ background 消息协议与状态模型。
 *
 * 单独抽出协议层的原因：popup（CaptureForm.vue）与 background（captureHandler.ts）
 * 都要引用同一组消息 ID 与状态形状。集中定义避免字符串字面量散落、保证两端类型一致。
 *
 * 通信链路（webext-bridge）：
 *   popup --[START_CAPTURE]--> background        触发一次采集，返回初始快照
 *   popup --[GET_CAPTURE_STATUS]--> background   popup（重新）打开时拉取最近一次采集快照
 *   background --[CAPTURE_STATUS_UPDATE]--> popup 采集推进时主动推送最新快照
 *
 * 为什么不是单次「请求-长等待响应」：采集要轮询后端解析任务，可能耗时数十秒，
 * 期间 popup 可能被用户关闭、SW 也可能因空闲被回收。改为「触发即返回 +
 * 主动推送 + 可重新拉取」三段式：background 把最近一次采集快照与进行中任务
 * 持久化到 chrome.storage.session（见 capture-store.ts），popup 关闭后重开、
 * 甚至 SW 被回收重建后，仍能通过 GET_CAPTURE_STATUS 看到真实最新进度。
 *
 * 消费者：src/background/captureHandler.ts、src/background/contextMenu.ts、
 *         src/popup/components/CaptureForm.vue。
 */

import type { ApiErrorKind } from '@/api/types'
import type { CaptureOwner } from '@/api/capabilities'

export type RequestedLibraryKind = 'auto' | 'reading' | 'site'
export type FinalLibraryKind = 'reading' | 'site'

// ── 消息 ID ─────────────────────────────────────────────────

/** popup → background：以可选备注触发一次当前页采集。 */
export const MSG_START_CAPTURE = 'webtag:start-capture'
/** popup → background：拉取 background 持有的最近一次采集快照。 */
export const MSG_GET_CAPTURE_STATUS = 'webtag:get-capture-status'
/** background → popup：采集状态推进时推送最新快照。 */
export const MSG_CAPTURE_STATUS_UPDATE = 'webtag:capture-status-update'

// ── 采集阶段 ────────────────────────────────────────────────

/**
 * 面向用户的采集阶段。在已提交 / 解析中 / 完成 / 失败四态之外，额外加三个
 * 本地阶段：
 * `idle`（尚无采集记录）、`capturing`（抓取页面中，覆盖「提交到后端之前」
 * 的本地抓取窗口）与 `still-processing`（轮询预算耗尽但任务仍在后端解析）
 * ——共七态。
 *
 * 与后端 Link.status 的映射关系（见 capture-poll.ts mapLinkStatus）：
 *   - 后端 pending             → submitted（已提交）
 *   - 后端 processing          → parsing（解析中）
 *   - 后端 done                → done（完成）
 *   - 后端 failed              → failed（失败）
 *   - 命中已 done 链接 → done（完成）
 *
 * still-processing 与 failed 的区分（本次 UX 加固的核心）：
 *   扩展轮询预算（POLL_INTERVAL_MS × MAX_POLL_ATTEMPTS）耗尽时，若后端任务
 *   仍是 pending / processing（而非后端明确返回 failed），这不是失败——LLM 慢
 *   解析常超出扩展的轮询窗口。此前一律归 failed 会误导用户「采集失败」。
 *   改为独立的 still-processing 终态：popup 用中性色提示「仍在解析中，稍后可
 *   在知识库桌面查看」，引导用户去知识库的「处理中」分区跟进，而非红色报错。
 *   只有后端明确返回 failed 才走 failed（capture.error.job-failed）。
 */
export type CaptureStage =
  | 'idle' // 尚无采集记录
  | 'capturing' // 正在抓取当前页 DOM/正文/配图
  | 'submitted' // 已 POST /api/ingest，后端任务排队中
  | 'parsing' // 后端正在解析
  | 'done' // 解析完成
  | 'still-processing' // 扩展轮询预算耗尽，后端仍在解析（非失败，引导去知识库查看）
  | 'failed' // 采集 / 解析失败

/**
 * 一次采集的状态快照。background 持有最新一份，并在每次推进时推送给 popup。
 *
 * 全部字段为 JSON 可序列化基础类型——webext-bridge 要求消息载荷为 JsonValue。
 */
export interface CaptureSnapshot {
  /** 当前阶段。 */
  stage: CaptureStage
  /** 被采集页面的 URL（抓取成功后填充，失败/idle 时可能为空串）。 */
  url: string
  /** 被采集页面的标题（同上）。 */
  title: string
  /** Verified installation activation that owns every non-empty capture. */
  owner?: CaptureOwner
  requestedKind?: RequestedLibraryKind
  libraryKind?: FinalLibraryKind
  /**
   * 失败原因文案 —— **仅** 后端任务失败时填充，存放后端返回的真实
   * error_msg / error_category（动态文案，非 i18n key）。popup 原样渲染。
   *
   * 客户端侧错误（注入失败 / 未配置 / 鉴权 / 网络 / 超时等）**不**带本字段：
   * 那类错误只下发 errorKind，由 popup 经 vue-i18n 取 capture.error.* 文案，
   * 避免在无 vue-i18n 的 SW 内拼死中文导致 en-US 用户看到中文。
   */
  errorMessage?: string
  /**
   * 失败原因的归一化类别（来自 API 客户端 ApiErrorKind，或采集侧自定义类别）。
   * popup 据此取 capture.error.<kind> 本地化文案，并决定是否额外提示
   * 「去设置页配置后端」（not-configured 时）。
   */
  errorKind?: ApiErrorKind | CaptureErrorKind
  /** 该快照生成的时间戳（毫秒）。用于 popup 判断是否为陈旧快照。 */
  updatedAt: number
}

/**
 * 采集流程自身（非 API 客户端）产生的错误类别。
 * 与 ApiErrorKind 合并构成 CaptureSnapshot.errorKind 的取值域。
 */
export type CaptureErrorKind =
  /** 后端地址 / Token 未配置。 */
  | 'not-configured'
  /**
   * 目标页是 chrome:// / 应用商店 / 扩展页等受限页面，无法注入抓取脚本。
   * 与 capture-injection-failed 区分：受限是「这个页面本就不能采集」，
   * popup 据此给出明确而非泛化的提示。
   */
  | 'capture-restricted'
  /** 注入抓取脚本真实失败（无活动标签、脚本执行异常、空结果等）。 */
  | 'capture-injection-failed'
  /** 后端解析任务最终状态为 failed。 */
  | 'job-failed'
  /**
   * 采集编排过程中发生未预期的异常（如归一化抛错）。
   * 由 startCapture 外层 try/catch 兜底产生，errorMessage 透传异常信息。
   */
  | 'capture-unexpected'

/** START_CAPTURE 消息的请求载荷。 */
export interface StartCapturePayload {
  /** 用户填写的可选备注；右键菜单触发时为空串。 */
  note: string
  requestedKind?: RequestedLibraryKind
  /**
   * popup 在打开时已解析的活动标签 id。
   *
   * 传它而非让 background 自行 re-query 活动标签——popup 展示的页面信息
   * 与 background 重新解析的活动标签可能不一致（用户在 popup 打开后切了标签）。
   * 由 popup 把「用户看到的那个标签」的 id 一并传来，保证采集到同一页面。
   * 右键菜单不经此载荷（直接走 controller.startCapture 的 tabId 参数）。
   */
  tabId?: number
}

/**
 * 处于「无采集记录」初始态的空快照常量。
 * popup 首次打开、background 尚未发生过采集时返回它。
 */
export const IDLE_CAPTURE_SNAPSHOT: CaptureSnapshot = {
  stage: 'idle',
  url: '',
  title: '',
  updatedAt: 0,
}
