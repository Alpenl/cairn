/**
 * @module background/capture-poll
 * 采集任务轮询 —— 纯决策函数 + 薄轮询外壳。
 *
 * 拆分动机：原 runCapturePolling 把「等待 setTimeout」「发网络请求」
 * 「分类终态/重试」三件事缠在一个循环里，决策逻辑无法脱离 fake timer
 * 单测。这里把分类逻辑收口为纯函数 decidePollOutcome（无 await、无
 * setTimeout、无副作用），轮询外壳 runCapturePolling 退化为
 * 「延时 → 取任务 → 决策 → 终态则发布并返回」的薄命令式循环。
 *
 * 与后端 Link.status 的映射关系：
 *   - pending             → retry（已提交，继续轮询）
 *   - processing          → retry（解析中，继续轮询）
 *   - done                → done
 *   - failed              → failed
 *   - getLink 网络失败     → retry（临时抖动，继续重试），最后一次仍失败 → failed
 *   - 次数耗尽仍非终态     → failed
 *
 * 消费者：src/background/captureHandler.ts（采集编排）。
 */

import type { ApiResult } from '@/api/webtag-client'
import type { ApiErrorKind, Link } from '@/api/types'
import type { CaptureErrorKind, CaptureStage } from './capture-protocol'

// ── 调优常量 ────────────────────────────────────────────────

/** 轮询 getLink 的间隔（毫秒）。 */
export const POLL_INTERVAL_MS = 2000
/**
 * 轮询最大次数。attempt 计数跨 SW 回收持久化（见 capture-store.ts），
 * 看门狗续跑时从持久化的 attempt 继续递增，因此「POLL_INTERVAL_MS × 此值」
 * 是采集解析的累计轮询预算上限（不含 SW 被回收期间的空档），
 * 而非单次 SW 存活窗口内的墙钟时长。
 *
 * 取 60（× 2s = 120s 累计轮询预算）：LLM 慢解析常超出 60s，30 次（60s）的旧
 * 预算会过早判定「仍在处理」。即使预算耗尽，也不再误报 failed——超时且任务
 * 仍 pending/processing 时走 still-processing 中性终态（见 decidePollOutcome）。
 */
export const MAX_POLL_ATTEMPTS = 60

// ── 轮询决策结果 ────────────────────────────────────────────

/**
 * 一次轮询的决策结果。由纯函数 decidePollOutcome 产出，轮询外壳据此决定
 * 「发布终态快照并返回」还是「继续下一轮」。
 *
 * - done：任务完成，发布 done 快照
 * - failed：任务失败（后端明确返回 failed）或客户端侧错误（最后一次 getLink
 *   仍网络失败），发布 failed 快照
 *   - errorKind/errorMessage 区分客户端侧错误（只发 kind）与后端失败原因
 *     （动态文案，透传 message）
 * - still-processing：轮询预算耗尽但后端任务仍 pending/processing —— 这**不是**
 *   失败（LLM 慢解析常超出扩展轮询窗口），而是「扩展不再等了，任务仍在跑」。
 *   轮询外壳据此发布中性的 still-processing 终态快照（非红色 error），引导用户
 *   去知识库的「处理中」分区跟进。区别于 failed：failed 是真出错，
 *   still-processing 只是「比扩展轮询窗口慢」。
 * - retry：未到终态且预算未耗尽，发布中间态快照（stage）后继续轮询
 */
export type PollOutcome =
  | { action: 'done' }
  | {
      action: 'failed'
      errorKind: ApiErrorKind | CaptureErrorKind
      errorMessage?: string
    }
  | { action: 'still-processing' }
  | { action: 'retry'; stage: CaptureStage }

/** 后端 Link.status → 面向用户的 CaptureStage 映射。 */
export function mapLinkStatus(status: Link['status']): CaptureStage {
  switch (status) {
    case 'pending':
      return 'submitted'
    case 'processing':
      return 'parsing'
    case 'done':
      return 'done'
    case 'failed':
      return 'failed'
    default:
      return 'parsing'
  }
}

/**
 * 纯函数：根据单次 getLink 结果与当前轮询进度，决定下一步动作。
 *
 * 无 await、无 setTimeout、无副作用——仅由入参计算出 PollOutcome，
 * 因此可脱离 fake timer 独立单测所有分支。
 *
 * @param linkResult  本次 getLink 的归一化结果
 * @param attempt     当前轮询序号（从 0 计）
 * @param maxAttempts 轮询最大次数
 */
export function decidePollOutcome(
  linkResult: ApiResult<Link>,
  attempt: number,
  maxAttempts: number,
): PollOutcome {
  const isLastAttempt = attempt >= maxAttempts - 1

  if (!linkResult.ok) {
    // 单次查询失败不立刻判定采集失败——可能是临时网络抖动，继续重试。
    // 仅在最后一次仍失败时给出失败结果。
    if (isLastAttempt) {
      // 客户端侧错误（网络 / 超时 / 鉴权等）：只发 errorKind，
      // 由 popup 经 vue-i18n 渲染本地化文案。
      return { action: 'failed', errorKind: linkResult.error.kind }
    }
    return { action: 'retry', stage: 'parsing' }
  }

  const stage = mapLinkStatus(linkResult.data.status)
  if (stage === 'done') {
    return { action: 'done' }
  }
  if (stage === 'failed') {
    // 后端任务失败：error_msg / error_category 是后端返回的真实失败原因，
    // 属于动态文案而非 i18n key，原样透传给 popup 展示。两者皆空时不带
    // errorMessage，由 popup 回退到 capture.error.job-failed 的本地化文案。
    const backendMessage =
      linkResult.data.error_msg || linkResult.data.error_category
    return {
      action: 'failed',
      errorKind: 'job-failed',
      ...(backendMessage ? { errorMessage: backendMessage } : {}),
    }
  }
  // pending / processing：未到终态，继续轮询。
  if (isLastAttempt) {
    // 轮询预算耗尽但后端仍 pending/processing：这**不是**失败——LLM 慢解析常
    // 超出扩展的轮询窗口。归独立的 still-processing 终态（中性），让 popup 用
    // 中性提示引导用户去知识库「处理中」分区跟进，而非红色误报「采集失败」。
    // 只有后端明确返回 failed（上面的 stage === 'failed' 分支）才走 failed。
    return { action: 'still-processing' }
  }
  return { action: 'retry', stage }
}
