/**
 * @module background/captureHandler
 * 当前页采集编排 —— 运行于 background Service Worker。
 *
 * 职责：把一次「采集当前页」请求串成完整闭环：
 *   1. 注入抓取脚本：deps.injectCapture 在活动标签执行 capturePageContent
 *   2. 归一化：toIngestSource 把原始 RawCapture 裁剪、组装为后端 IngestSource
 *   3. 提交：调 WebTagClient.ingest（POST /api/ingest）
 *   4. 轮询：调 WebTagClient.getLink（GET /api/links/{link_id}）直到 done / failed / 超时
 *   5. 保存原文：解析完成后调 WebTagClient.saveLinkContent，持久化浏览器快照
 *   6. 回报：每次状态推进通过 deps.sendSnapshot 把 CaptureSnapshot 推送给 popup，
 *      并把最近一份快照 + 进行中任务持久化到 deps.store（chrome.storage.session）
 *
 * MV3 持久化设计（本次加固的核心）：
 *   Chrome MV3 的 Service Worker 空闲约 30s 后会被回收。轮询循环靠
 *   setTimeout 驱动、状态活在 controller 闭包里——SW 一旦被回收，循环消失，
 *   popup 永远卡在「解析中」。两条腿的混合方案修复它：
 *     - **SW 存活时的快速轮询**：仍用 ~2s 的 setTimeout 循环保证响应性，
 *       但每轮都把进度（attempt）落盘到 deps.store。
 *     - **看门狗恢复**：capture-messaging 注册一个 ~1 分钟周期的 chrome.alarms。
 *       alarm 触发时调 controller.resumeIfNeeded()：若 store 里有进行中任务、
 *       且本 SW 实例没有正在跑的轮询循环 → 从持久化的 attempt 断点续跑。
 *       这样 SW 中途被回收的采集会在 ~1 分钟内被恢复。
 *     - getLatestSnapshot 从 deps.store 读——重唤醒的 SW 返回真实进度而非 idle。
 *
 * 触发入口（两条，行为一致，备注不同）：popup CaptureForm 按钮（带备注、
 * 带 resolved tabId）、右键菜单（contextMenu.ts，无备注，带点击事件 tabId）。
 *
 * 消费者：src/background/main.ts（创建 controller、注册消息监听与右键菜单）、
 *         src/background/contextMenu.ts（右键菜单调用 controller.startCapture）、
 *         src/background/capture-messaging.ts（看门狗 alarm 调 resumeIfNeeded）。
 */

import {
  createIdempotencyKey,
  type ApiResult,
  type WebTagClient,
} from '@/api/webtag-client'
import type { RawCapture } from '@/contentScripts/capture'
import type {
  IngestRequest,
  IngestSource,
  Link,
  SubmitResponse,
} from '@/api/types'
import {
  DEFAULT_CAPTURE_DESTINATION,
  type CaptureDestination,
  type CaptureRequestDestination,
} from '@/api/capture-preferences'
import {
  captureOwnersEqual,
  deriveExtensionCapabilityPolicy,
  type CaptureActivationLease,
  type CaptureOwner,
} from '@/api/capabilities'
import { toIngestSource } from './capture-normalize'
import {
  MAX_POLL_ATTEMPTS,
  POLL_INTERVAL_MS,
  decidePollOutcome,
} from './capture-poll'
import {
  IDLE_CAPTURE_SNAPSHOT,
  type CaptureSnapshot,
  type RequestedLibraryKind,
} from './capture-protocol'
import {
  STALE_INFLIGHT_MARGIN_MS,
  isInFlightStale,
  type CaptureStore,
  type InFlightCapture,
  type InFlightCaptureMetadata,
} from './capture-store'

/**
 * 一次采集的最大墙钟预算（毫秒）—— 用于判定持久化 in-flight 记录是否陈旧。
 *
 * = 轮询间隔 × 最大轮询次数（理论纯轮询耗时）+ 余量（覆盖 SW 被回收的空档、
 * 看门狗唤醒延迟）。超出此预算的持久化 in-flight 记录被视为 STALE：
 *   - 续跑路径忽略它（不再 getLink 续跑一个早已结束 / 卡死的任务）
 *   - 并发去重路径忽略它（不再阻塞新采集）
 * 这让一个因 storage 故障被丢弃的 clearInFlight 留下的僵尸记录，会在
 * 预算到期后自动失效，而非锁死整个浏览器会话的后续采集。
 */
export const MAX_CAPTURE_AGE_MS =
  POLL_INTERVAL_MS * MAX_POLL_ATTEMPTS + STALE_INFLIGHT_MARGIN_MS

// ── 注入依赖 ────────────────────────────────────────────────

/**
 * 注入抓取脚本的归一化结果。
 *
 * restricted 与 failed 的区分：
 *   - restricted：目标页是 chrome:// / 应用商店 / 扩展页等受限页面，
 *     executeScript 必定抛错，属于「这个页面本就不能采集」。
 *   - failed：注入真实失败（无活动标签、脚本执行异常、空结果等）。
 * startCapture 据此映射不同 errorKind，popup 给用户不同提示。
 */
export type CaptureInjectResult =
  | { ok: true; data: RawCapture }
  | { ok: false; reason: 'restricted' | 'failed'; message?: string }

/** A verified connection snapshot used for one capture activation. */
export interface CaptureActivation {
  client: WebTagClient
  owner: CaptureOwner
  lease: CaptureActivationLease
  /** Absent when the connection has no secret suitable for private recovery. */
  recoveryKey?: CryptoKey
}

/** A retryable identity/configuration probe failure that must preserve recovery state. */
export interface TransientCaptureActivationFailure {
  readonly status: 'transient-unavailable'
}

export const TRANSIENT_CAPTURE_ACTIVATION_FAILURE: TransientCaptureActivationFailure =
  Object.freeze({ status: 'transient-unavailable' })

export function isTransientCaptureActivationFailure(
  value: CaptureActivation | TransientCaptureActivationFailure | null,
): value is TransientCaptureActivationFailure {
  return value !== null && 'status' in value
}

/**
 * createCaptureController 的注入依赖。生产实现在 capture-messaging.ts 接线，
 * 测试传入纯 stub。把所有 IO（chrome.* / 网络 / 配置读取 / 消息推送 / 持久化）
 * 收口为这一组显式依赖，让 controller 本身不直接 import 任何 IO 模块。
 */
export interface CaptureDeps {
  /**
   * 读取 confirmed 配置、验证会话身份并构造带 owner/lease 的客户端激活。
   * 未配置或确定的身份不匹配返回 null；可重试探测失败返回 transient
   * sentinel，controller 必须保留密封恢复状态。
   */
  activateConnection: () => Promise<
    CaptureActivation | TransientCaptureActivationFailure | null
  >
  /**
   * 向目标标签注入抓取脚本并取回 RawCapture（生产：chrome.scripting 适配器）。
   * tabId 为空时自动取当前窗口活动标签。这是唯一直面 Chrome API 的叶子。
   */
  injectCapture: (tabId?: number) => Promise<CaptureInjectResult>
  /**
   * 把快照推送给 popup（生产：webext-bridge sendMessage）。
   * popup 未打开时应静默失败，不抛错。
   */
  sendSnapshot: (snapshot: CaptureSnapshot) => void
  /**
   * 采集状态持久化存储（生产：chrome.storage.session 适配器）。
   * 快照 + 进行中任务落盘于此，让 SW 被回收后能恢复。
   */
  store: CaptureStore
  /** 当前时间戳源；默认 Date.now，测试可注入固定值。 */
  now?: () => number
  /** Read the shared extension destination preference; omitted means Inbox. */
  getCaptureDestination?: () => Promise<CaptureDestination>
  /** Refresh the global pending badge after a successful Inbox capture. */
  onInboxCapture?: () => void
}

/** createCaptureController 返回的采集控制器。 */
export interface CaptureController {
  /**
   * 执行一次完整采集。供 popup 消息处理与右键菜单共用。
   *
   * @param note   用户备注（右键菜单为空串）
   * @param tabId  目标标签 id；popup 传 resolved tabId，右键菜单传 info.tabId
   * @returns 触发后的初始快照（submitted / done / failed）。
   *          submitted 之后的 parsing/done/failed 通过 sendSnapshot 异步推送。
   */
  startCapture: (
    note: string,
    tabId?: number,
    requestedKind?: RequestedLibraryKind,
  ) => Promise<CaptureSnapshot>
  /**
   * 读取最近一次采集快照（供 GET_CAPTURE_STATUS 与右键菜单使用）。
   * 从持久化存储读取——重唤醒的 SW 也能返回真实的最后已知状态。
   */
  getLatestSnapshot: () => Promise<CaptureSnapshot>
  /**
   * 看门狗恢复入口：由 chrome.alarms 周期触发。
   * 若 store 里有进行中采集任务、且本 SW 实例没有正在跑的轮询循环，
   * 则从持久化的 attempt 断点续跑——恢复 SW 中途被回收的采集。
   */
  resumeIfNeeded: () => Promise<void>
}

/**
 * sleep 工具。
 *
 * 这是采集轮询里有意保留的 IO / 时间边界——把 setTimeout 等待收口在此处，
 * 让决策逻辑 decidePollOutcome 得以保持纯函数（无 await、无定时器、无副作用），
 * 从而可脱离 fake timer 独立单测。
 */
function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// ── 采集控制器工厂 ──────────────────────────────────────────

/**
 * 创建一个采集控制器。SW 启动时由 capture-messaging.ts 调用一次。
 *
 * 闭包内仅保留**本 SW 实例局部**的运行时标志（pollLoopRunning），
 * 不再持有跨 SW 唤醒需要的状态——后者全部落盘到 deps.store。
 * 所以 SW 被回收重建后，新 controller 读 store 即可拿回真实状态。
 */
export function createCaptureController(deps: CaptureDeps): CaptureController {
  const now = deps.now ?? Date.now
  let activeActivation: CaptureActivation | null = null

  /**
   * 本 SW 实例内是否已有一个轮询循环在跑。
   *
   * 它是「实例级」标志而非「全局状态」：SW 被回收时随闭包一起丢失——
   * 这正是期望行为。看门狗在新 SW 实例里看到它为 false，便知道
   * 「持久化里虽有进行中任务，但本实例没在轮询」→ 触发续跑。
   * 同时它兼任并发守卫：一个 SW 实例内不会有两个轮询循环。
   */
  let pollLoopRunning = false

  /**
   * 本 SW 实例的内存级 in-flight 记录 —— 并发去重的 **主** 防线。
   *
   * 为什么需要它（不能只靠持久化记录去重）：
   *   持久化的 chrome.storage.session 记录可能因 storage 故障写入失败
   *   （setInFlight 被丢弃）。若并发去重只读持久化记录，一次失败的
   *   setInFlight 会让第二次 startCapture 读不到 in-flight → 重复注入、
   *   重复提交同一页面到 /api/ingest。
   *
   * 改为：**本 SW 生命周期内**的并发去重以这个内存记录为准——只要它非空，
   * 第二次 startCapture 立即返回当前快照，与持久化是否成功无关。持久化记录
   * 退居「跨 SW 重建的恢复层」：SW 被回收时本变量随闭包丢失，新 SW 实例靠
   * 持久化记录（带陈旧判定）恢复。
   *
   * 生命周期：startCapture 进入轮询前置位、采集到达终态（finishCapture）
   * 时清空。SW 被回收 → 随闭包丢失（期望行为，新实例改读持久化记录）。
   */
  let inMemoryInFlight: InFlightCapture | null = null

  /**
   * Covers the gap before the submission lease is persisted. An async function
   * can otherwise be entered twice before the first request reaches ingest.
   */
  let startOperation: Promise<CaptureSnapshot> | null = null
  let resumeOperation: Promise<void> | null = null

  /** Serialize every state mutation so an older generation cannot finish over a newer one. */
  let stateMutationQueue: Promise<void> = Promise.resolve()

  function mutateState<T>(operation: () => Promise<T>): Promise<T> {
    const result = stateMutationQueue.then(operation, operation)
    stateMutationQueue = result.then(
      () => undefined,
      () => undefined,
    )
    return result
  }

  async function waitForStateMutations(): Promise<void> {
    await stateMutationQueue
  }

  async function clearMatchingInFlight(
    expected: InFlightCaptureMetadata,
  ): Promise<void> {
    await mutateState(async () => {
      const current = await deps.store.getInFlightMetadata()
      if (
        !current ||
        !captureOwnersEqual(current.owner, expected.owner) ||
        current.phase !== expected.phase ||
        current.attempt !== expected.attempt ||
        current.startedAt !== expected.startedAt
      ) {
        return
      }
      await deps.store.clearInFlight()
    })
  }

  async function clearMatchingSnapshot(
    expectedOwner: CaptureOwner,
  ): Promise<void> {
    await mutateState(async () => {
      const currentOwner = await deps.store.getSnapshotOwner()
      if (!currentOwner || !captureOwnersEqual(currentOwner, expectedOwner)) {
        return
      }
      await deps.store.setSnapshot({ ...IDLE_CAPTURE_SNAPSHOT })
    })
  }

  /** 构造一份带当前时间戳的快照。 */
  function makeSnapshot(
    partial: Omit<CaptureSnapshot, 'updatedAt'>,
  ): CaptureSnapshot {
    return {
      ...partial,
      ...(activeActivation && partial.stage !== 'idle'
        ? { owner: activeActivation.owner }
        : {}),
      updatedAt: now(),
    }
  }

  /**
   * 更新持久化快照并向 popup 推送（popup 未打开时推送静默失败）。
   * 快照先落盘再推送：即使 popup 没开，重唤醒的 SW 也能读回这份快照。
   */
  async function publishSnapshot(
    snapshot: CaptureSnapshot,
    expectedActivation: CaptureActivation | null,
  ): Promise<boolean> {
    return mutateState(async () => {
      if (activeActivation !== expectedActivation) return false
      await deps.store.setSnapshot(snapshot, expectedActivation?.recoveryKey)
      if (activeActivation !== expectedActivation) return false
      deps.sendSnapshot(snapshot)
      return true
    })
  }

  /**
   * 登记「进行中采集任务」记录 —— 同步内存记录 + 持久化记录。
   *
   * 内存记录（inMemoryInFlight）是本 SW 实例内并发去重的主防线，必先置位
   * 且不会失败；持久化记录（deps.store.setInFlight）是跨 SW 重建的恢复层，
   * 可能因 storage 故障失败，但 capture-store 已 console.warn 留痕，且
   * 失败不影响内存记录已生效的并发去重。
   *
   * 每轮轮询后调用，attempt 随之递增；startedAt 由调用方保持不变（衡量
   * 「采集开始至今」的墙钟时长，供陈旧判定使用）。
   */
  async function persistInFlight(
    inFlight: InFlightCapture,
    expectedActivation: CaptureActivation,
  ): Promise<boolean> {
    return mutateState(async () => {
      if (activeActivation !== expectedActivation) return false
      inMemoryInFlight = inFlight
      await deps.store.setInFlight(inFlight, expectedActivation.recoveryKey)
      return activeActivation === expectedActivation
    })
  }

  async function abandonCapture(
    expectedActivation?: CaptureActivation,
  ): Promise<CaptureSnapshot> {
    const idle = { ...IDLE_CAPTURE_SNAPSHOT }
    const capturedActivation = expectedActivation ?? activeActivation
    await mutateState(async () => {
      if (activeActivation !== capturedActivation) {
        expectedActivation?.lease.revoke()
        return
      }
      capturedActivation?.lease.revoke()
      activeActivation = null
      inMemoryInFlight = null
      await deps.store.setSnapshot(idle)
      await deps.store.clearInFlight()
      if (activeActivation === null) deps.sendSnapshot(idle)
    })
    return idle
  }

  async function activationIsCurrent(
    activation: CaptureActivation,
  ): Promise<boolean> {
    if (
      activeActivation !== activation ||
      !captureOwnersEqual(activeActivation.owner, activation.owner)
    ) {
      return false
    }
    if (!(await activation.lease.isCurrent())) return false
    return (
      activeActivation === activation &&
      captureOwnersEqual(activeActivation.owner, activation.owner)
    )
  }

  interface CaptureTarget {
    destination: CaptureRequestDestination
    requestedKind: RequestedLibraryKind
  }

  function submitMatchesTarget(
    submit: SubmitResponse,
    target: CaptureTarget,
  ): boolean {
    const hasLinkID =
      typeof submit.link_id === 'string' && submit.link_id !== ''
    const hasInboxID =
      typeof submit.inbox_id === 'string' && submit.inbox_id !== ''
    if (hasLinkID === hasInboxID) return false
    if (submit.destination && submit.destination !== target.destination) {
      return false
    }
    if (target.destination === 'inbox') {
      return hasInboxID && !hasLinkID
    }
    return hasLinkID && !hasInboxID
  }

  function targetFromIngestRequest(request: IngestRequest): CaptureTarget {
    const destination = request.destination ?? 'library'
    return {
      destination,
      requestedKind:
        destination === 'site'
          ? 'site'
          : (request.requested_library_kind ?? 'auto'),
    }
  }

  /**
   * Resolve the wire destination without allowing a missing/old capability
   * endpoint to masquerade as Reader Inbox support.
   */
  async function resolveCaptureTarget(
    client: WebTagClient,
    requestedKind: RequestedLibraryKind,
    desiredDestination: CaptureDestination,
  ): Promise<ApiResult<CaptureTarget>> {
    let capabilities: Awaited<
      ReturnType<WebTagClient['getCapabilities']>
    > | null = null
    try {
      if (typeof client.getCapabilities === 'function') {
        capabilities = await client.getCapabilities()
      }
    } catch {
      capabilities = null
    }

    const policy = deriveExtensionCapabilityPolicy(
      capabilities?.ok ? capabilities.data : undefined,
    )
    if (requestedKind === 'site') {
      return policy.site
        ? { ok: true, data: { destination: 'site', requestedKind: 'site' } }
        : {
            ok: false,
            error: {
              kind: 'other',
              message: 'site capture capability is unavailable',
            },
          }
    }

    const resolvedLibraryKind =
      requestedKind === 'reading' &&
      capabilities?.ok === true &&
      capabilities.data?.library_kinds === true
        ? 'reading'
        : 'auto'

    return {
      ok: true,
      data: {
        destination:
          desiredDestination === 'inbox' && policy.inbox ? 'inbox' : 'library',
        requestedKind: resolvedLibraryKind,
      },
    }
  }

  function buildIngestBody(
    source: IngestSource,
    target: CaptureTarget,
  ): IngestRequest {
    return {
      sources: [source],
      destination: target.destination,
      ...(target.destination === 'library'
        ? { requested_library_kind: target.requestedKind }
        : {}),
    }
  }

  function notifyInboxCapture(): void {
    try {
      deps.onInboxCapture?.()
    } catch {
      // Badge refresh is an accessory and must not change capture success.
    }
  }

  /** Finish the shared response handling for first submission and resumption. */
  async function handleSubmitResponse(
    activation: CaptureActivation,
    source: IngestSource,
    note: string,
    target: CaptureTarget,
    idempotencyKey: string,
    startedAt: number,
    submit: SubmitResponse,
  ): Promise<CaptureSnapshot> {
    if (!(await activationIsCurrent(activation)))
      return abandonCapture(activation)
    const url = source.url ?? ''
    const title = source.title ?? ''

    if (!submitMatchesTarget(submit, target)) {
      return finishCapture(
        makeSnapshot({
          stage: 'failed',
          url,
          title,
          errorKind: 'other',
          requestedKind: target.requestedKind,
        }),
        activation,
      )
    }

    // Inbox is durable without a library Link. Never invent that identity or
    // poll the library endpoint for this branch.
    if (submit.inbox_id) {
      const snapshot = await finishCapture(
        makeSnapshot({
          stage: 'done',
          url,
          title,
          requestedKind: target.requestedKind,
        }),
        activation,
      )
      if (snapshot.stage === 'done') notifyInboxCapture()
      return snapshot
    }

    // A duplicate library URL can return its existing failed state. It is not
    // an implicit retry; explicit retry remains refreshLink.
    if (submit.status === 'failed') {
      return finishCapture(
        makeSnapshot({
          stage: 'failed',
          url,
          title,
          errorKind: 'job-failed',
          requestedKind: target.requestedKind,
        }),
        activation,
      )
    }

    if (submit.status === 'done') {
      return finishCapture(
        makeSnapshot({
          stage: 'done',
          url,
          title,
          requestedKind: target.requestedKind,
        }),
        activation,
      )
    }

    const submittedSnapshot = makeSnapshot({
      stage: 'submitted',
      url,
      title,
      requestedKind: target.requestedKind,
    })
    const inFlight: InFlightCapture = {
      owner: activation.owner,
      linkId: submit.link_id ?? '',
      url,
      title,
      note,
      requestedKind: target.requestedKind,
      destination: target.destination,
      idempotencyKey,
      phase: 'polling',
      attempt: 0,
      snapshot: submittedSnapshot,
      startedAt,
    }
    await publishSnapshot(submittedSnapshot, activation)
    await persistInFlight(inFlight, activation)
    if (!(await activationIsCurrent(activation)))
      return abandonCapture(activation)
    void runCapturePolling(activation, inFlight, 0)
    return submittedSnapshot
  }

  /**
   * 采集到达终态：发布终态快照、清除内存与持久化的进行中任务。
   *
   * 「终态」指 done / failed——无后续轮询。无论是同步终态
   * （抓取失败 / 未配置 / 提交失败 / 命中已 done），还是轮询终态，
   * 都经此收尾。两份记录都清：
   *   - 内存记录（inMemoryInFlight）：释放本 SW 实例的并发去重锁。
   *   - 持久化记录（deps.store.clearInFlight）：让看门狗不再续跑。
   *     即使此处 clearInFlight 因 storage 故障失败，留下的僵尸记录也会被
   *     isInFlightStale 在采集预算到期后判定为陈旧而放行——不会永久锁死。
   */
  async function finishCapture(
    snapshot: CaptureSnapshot,
    expectedActivation: CaptureActivation | null,
  ): Promise<CaptureSnapshot> {
    const finished = await mutateState(async () => {
      if (activeActivation !== expectedActivation) {
        expectedActivation?.lease.revoke()
        return false
      }
      inMemoryInFlight = null
      await deps.store.setSnapshot(snapshot, expectedActivation?.recoveryKey)
      if (activeActivation !== expectedActivation) return false
      await deps.store.clearInFlight()
      if (activeActivation !== expectedActivation) return false
      expectedActivation?.lease.revoke()
      activeActivation = null
      deps.sendSnapshot(snapshot)
      return true
    })
    return finished ? snapshot : { ...IDLE_CAPTURE_SNAPSHOT }
  }

  /**
   * 在链接解析完成后自动保存浏览器采集到的原文。
   *
   * 这是附加动作：链接入库与解析已经成功，因此原文接口失败不能把整次采集
   * 反写成 failed。WebTagClient 正常不会抛错；try/catch 只防御测试替身或意外
   * 运行时异常。await 请求可让 MV3 Service Worker 在响应前保持活跃。
   */
  async function saveCapturedOriginal(
    activation: CaptureActivation,
    linkId: string,
    finalKind?: string | null,
  ): Promise<void> {
    if (finalKind !== 'reading') return
    if (!(await activationIsCurrent(activation))) return
    const client = activation.client
    if (typeof client.saveLinkContent !== 'function') return
    try {
      const result = await client.saveLinkContent(linkId)
      if (!result.ok) {
        console.warn(
          '[capture] automatic original-content save failed',
          result.error.kind,
        )
      }
    } catch {
      console.warn(
        '[capture] automatic original-content save failed unexpectedly',
      )
    }
  }

  /**
   * 轮询 getLink 直到任务 done / failed 或超过 MAX_POLL_ATTEMPTS。
   *
   * 从 startAttempt 开始计数——首次采集传 0，看门狗续跑传持久化的 attempt。
   * 每轮：延时 → getLink → decidePollOutcome 决策 → 终态则收尾返回，
   * 否则发布中间态、把递增后的 attempt 落盘后继续。
   *
   * pollLoopRunning 在进入时置 true、在 finally 置 false——保证一个 SW
   * 实例内只有一个轮询循环，看门狗据此判断是否需要续跑。
   *
   * @param inFlight     进行中任务记录（linkId / url / title / note / attempt）
   * @param startAttempt 起始轮询序号（首跑 0，续跑为持久化 attempt）
   */
  async function runCapturePolling(
    activation: CaptureActivation,
    inFlight: InFlightCapture,
    startAttempt: number,
  ): Promise<void> {
    if (pollLoopRunning) return
    pollLoopRunning = true
    const { linkId, url, title, note, requestedKind = 'auto' } = inFlight
    try {
      for (
        let attempt = startAttempt;
        attempt < MAX_POLL_ATTEMPTS;
        attempt += 1
      ) {
        await delay(POLL_INTERVAL_MS)
        if (!(await activationIsCurrent(activation))) {
          await abandonCapture(activation)
          return
        }
        const client = activation.client
        const result: ApiResult<Link> = await client.getLink(linkId)
        if (!(await activationIsCurrent(activation))) {
          await abandonCapture(activation)
          return
        }
        const outcome = decidePollOutcome(result, attempt, MAX_POLL_ATTEMPTS)

        if (outcome.action === 'done') {
          const libraryKind = result.ok ? result.data.library_kind : undefined
          if (result.ok) {
            await saveCapturedOriginal(activation, result.data.id, libraryKind)
            if (!(await activationIsCurrent(activation))) {
              await abandonCapture(activation)
              return
            }
          }
          await finishCapture(
            makeSnapshot({
              stage: 'done',
              url,
              title,
              requestedKind,
              ...(libraryKind ? { libraryKind } : {}),
            }),
            activation,
          )
          return
        }
        if (outcome.action === 'failed') {
          await finishCapture(
            makeSnapshot({
              stage: 'failed',
              url,
              title,
              requestedKind,
              errorKind: outcome.errorKind,
              ...(outcome.errorMessage
                ? { errorMessage: outcome.errorMessage }
                : {}),
            }),
            activation,
          )
          return
        }
        if (outcome.action === 'still-processing') {
          // 轮询预算耗尽、后端仍在解析：收尾为中性的 still-processing 终态。
          // 不带 errorKind（这不是错误），popup 用中性色文案展示并引导去知识库。
          await finishCapture(
            makeSnapshot({
              stage: 'still-processing',
              url,
              title,
              requestedKind,
            }),
            activation,
          )
          return
        }
        // retry：发布中间态，并把「已完成 attempt+1」连同新快照落盘。
        // 落盘 attempt+1（而非 attempt）是因为本轮已经查询完毕——
        // SW 此刻被回收，续跑应从下一轮开始，不重复本轮。
        // startedAt 沿用 inFlight 原始值（衡量「采集开始至今」墙钟时长，
        // 不随轮询推进刷新），陈旧判定才能正确反映总耗时。
        const midSnapshot = makeSnapshot({
          stage: outcome.stage,
          url,
          title,
          requestedKind,
        })
        await publishSnapshot(midSnapshot, activation)
        await persistInFlight(
          {
            owner: inFlight.owner,
            linkId,
            url,
            title,
            note,
            requestedKind,
            destination: inFlight.destination,
            idempotencyKey: inFlight.idempotencyKey,
            phase: 'polling',
            attempt: attempt + 1,
            snapshot: midSnapshot,
            startedAt: inFlight.startedAt,
          },
          activation,
        )
      }
    } finally {
      pollLoopRunning = false
    }
  }

  /**
   * 执行一次完整采集。
   *
   * 整个函数体被 try/catch 包裹：任何「意外」抛出（如 toIngestSource 抛错、
   * 持久化抛错）都会被兜底为一个 failed 快照并清理进行中任务——
   * 绝不让异常逃逸导致后续采集卡死。
   *
   * 并发去重（fail-safe，两层）：
   *   1. **内存级（主防线）**：inMemoryInFlight 非空 → 本 SW 实例内已有采集
   *      在跑，立即返回当前快照。它不依赖持久化是否成功——即使 setInFlight
   *      因 storage 故障被丢弃，第二次 startCapture 仍被内存记录挡住，
   *      不会重复注入 / 重复提交同一页面到 /api/ingest。
   *   2. **持久化级（跨 SW 恢复层）**：内存记录为空时（如 SW 刚重建，闭包
   *      变量已丢失），再读持久化的 in-flight 记录。读到且 **未陈旧** 才
   *      阻塞——用 isInFlightStale 判断：超出采集预算的僵尸记录（如
   *      clearInFlight 因 storage 故障被丢弃留下的）视为失效，放行新采集。
   *      这样一次丢失的 clearInFlight 会在预算到期后自愈，不会永久锁死会话。
   */
  async function runStartCapture(
    note: string,
    tabId?: number,
    requestedKind: RequestedLibraryKind = 'auto',
  ): Promise<CaptureSnapshot> {
    let activation: CaptureActivation | null = null
    await waitForStateMutations()

    // Existing state is returned only after proving that the currently
    // confirmed installation still owns it.
    if (inMemoryInFlight) {
      if (
        activeActivation &&
        captureOwnersEqual(activeActivation.owner, inMemoryInFlight.owner) &&
        (await activationIsCurrent(activeActivation))
      ) {
        return inMemoryInFlight.snapshot
      }
      const activationResult = await deps.activateConnection()
      if (isTransientCaptureActivationFailure(activationResult)) {
        return { ...IDLE_CAPTURE_SNAPSHOT }
      }
      activation = activationResult
      if (
        activation &&
        captureOwnersEqual(activation.owner, inMemoryInFlight.owner)
      ) {
        activeActivation = activation
        if (await activationIsCurrent(activation)) {
          return inMemoryInFlight.snapshot
        }
      }
      activation?.lease.revoke()
      await abandonCapture()
      activation = null
    }
    // 并发去重第 2 层（跨 SW 恢复）：读持久化记录，仅「未陈旧」才阻塞。
    const persistedMetadata = await deps.store.getInFlightMetadata()
    if (
      persistedMetadata &&
      !isInFlightStale(persistedMetadata, MAX_CAPTURE_AGE_MS, now())
    ) {
      if (!activation) {
        const activationResult = await deps.activateConnection()
        if (isTransientCaptureActivationFailure(activationResult)) {
          return { ...IDLE_CAPTURE_SNAPSHOT }
        }
        activation = activationResult
      }
      if (
        activation &&
        captureOwnersEqual(activation.owner, persistedMetadata.owner)
      ) {
        activeActivation = activation
        if (await activationIsCurrent(activation)) {
          const persistedInFlight = await deps.store.getInFlight(
            activation.recoveryKey,
          )
          if (persistedInFlight && (await activationIsCurrent(activation))) {
            inMemoryInFlight = persistedInFlight
            if (
              !persistedInFlight.linkId &&
              persistedInFlight.source &&
              persistedInFlight.idempotencyKey
            ) {
              void resumeIfNeeded()
            }
            return persistedInFlight.snapshot
          }
        }
      }
      activation?.lease.revoke()
      await abandonCapture()
      activation = null
    }
    // 持久化记录陈旧（或不存在）→ 清掉僵尸记录，继续发起新采集。
    if (persistedMetadata) {
      await clearMatchingInFlight(persistedMetadata)
    }

    try {
      // Resolve configuration and capabilities before page injection. A
      // forbidden explicit site path must perform neither injection nor ingest.
      if (!activation) {
        const activationResult = await deps.activateConnection()
        if (isTransientCaptureActivationFailure(activationResult)) {
          return { ...IDLE_CAPTURE_SNAPSHOT }
        }
        activation = activationResult
      }
      if (!activation) {
        return finishCapture(
          makeSnapshot({
            stage: 'failed',
            url: '',
            title: '',
            errorKind: 'not-configured',
            requestedKind,
          }),
          null,
        )
      }
      activeActivation = activation
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)
      const client = activation.client
      let desiredDestination = DEFAULT_CAPTURE_DESTINATION
      try {
        desiredDestination =
          (await deps.getCaptureDestination?.()) ?? DEFAULT_CAPTURE_DESTINATION
      } catch {
        desiredDestination = DEFAULT_CAPTURE_DESTINATION
      }
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)
      const target = await resolveCaptureTarget(
        client,
        requestedKind,
        desiredDestination,
      )
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)
      if (!target.ok) {
        return finishCapture(
          makeSnapshot({
            stage: 'failed',
            url: '',
            title: '',
            errorKind: target.error.kind,
            requestedKind,
          }),
          activation,
        )
      }

      // The popup publishes an optimistic capturing state while the probe is
      // pending; the authoritative background snapshot starts after approval.
      await publishSnapshot(
        makeSnapshot({ stage: 'capturing', url: '', title: '', requestedKind }),
        activation,
      )

      // 阶段 1：注入抓取脚本。
      // 受限页面（chrome:// 等）报 capture-restricted，其余注入失败报
      // capture-injection-failed，由 popup 经 vue-i18n 渲染对应文案。
      const captured = await deps.injectCapture(tabId)
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)
      if (!captured.ok) {
        return finishCapture(
          makeSnapshot({
            stage: 'failed',
            url: '',
            title: '',
            errorKind:
              captured.reason === 'restricted'
                ? 'capture-restricted'
                : 'capture-injection-failed',
            requestedKind,
          }),
          activation,
        )
      }
      const raw = captured.data

      // Persist the exact source, body, and key before POST. If
      // the worker disappears after this point, the next worker can replay the
      // same body instead of injecting/submitting a second capture.
      const source: IngestSource = toIngestSource(raw, note)
      const idempotencyKey = createIdempotencyKey()
      const ingestRequest = buildIngestBody(source, target.data)
      const startedAt = now()
      const submittingSnapshot = makeSnapshot({
        stage: 'capturing',
        url: raw.url,
        title: raw.title,
        requestedKind: target.data.requestedKind,
      })
      await publishSnapshot(submittingSnapshot, activation)
      const pendingSubmission: InFlightCapture = {
        owner: activation.owner,
        linkId: '',
        url: raw.url,
        title: raw.title,
        note,
        requestedKind: target.data.requestedKind,
        destination: target.data.destination,
        idempotencyKey,
        phase: 'submitting',
        source,
        ingestRequest,
        attempt: 0,
        snapshot: submittingSnapshot,
        startedAt,
      }
      await persistInFlight(pendingSubmission, activation)
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)

      // 阶段 3：归一化并提交。ingest internally retries only ambiguous
      // network/timeout results, reusing this exact key for both attempts.
      const ingestRes: ApiResult<SubmitResponse> = await client.ingest(
        ingestRequest,
        { idempotencyKey },
      )
      if (!(await activationIsCurrent(activation)))
        return abandonCapture(activation)
      if (!ingestRes.ok) {
        return finishCapture(
          makeSnapshot({
            stage: 'failed',
            url: raw.url,
            title: raw.title,
            errorKind: ingestRes.error.kind,
            requestedKind: target.data.requestedKind,
          }),
          activation,
        )
      }

      return handleSubmitResponse(
        activation,
        source,
        note,
        target.data,
        idempotencyKey,
        startedAt,
        ingestRes.data,
      )
    } catch (err) {
      // 任何意外抛出：兜底为 failed 快照并清理，绝不让异常逃逸卡死后续采集。
      const message = err instanceof Error ? err.message : String(err)
      return finishCapture(
        makeSnapshot({
          stage: 'failed',
          url: '',
          title: '',
          errorKind: 'capture-unexpected',
          errorMessage: message,
          requestedKind,
        }),
        activation,
      )
    }
  }

  /** Resume a worker that disappeared after the source was captured but before ingest returned. */
  async function resumePendingSubmission(
    activation: CaptureActivation,
    inFlight: InFlightCapture,
  ): Promise<void> {
    if (!(await activationIsCurrent(activation))) {
      await abandonCapture(activation)
      return
    }
    const client = activation.client
    const source = inFlight.source ?? inFlight.ingestRequest?.sources[0]
    if (!source || !inFlight.idempotencyKey) {
      await finishCapture(
        makeSnapshot({
          stage: 'failed',
          url: inFlight.url,
          title: inFlight.title,
          errorKind: 'capture-unexpected',
          requestedKind: inFlight.requestedKind ?? 'auto',
        }),
        activation,
      )
      return
    }

    let target: CaptureTarget
    let ingestRequest: IngestRequest
    if (inFlight.ingestRequest) {
      // The capability result may have changed while the worker was asleep.
      // Replaying a mutation must keep both its body and idempotency key stable.
      ingestRequest = inFlight.ingestRequest
      target = targetFromIngestRequest(ingestRequest)
    } else {
      const resolvedTarget = await resolveCaptureTarget(
        client,
        inFlight.destination === 'site'
          ? 'site'
          : (inFlight.requestedKind ?? 'auto'),
        inFlight.destination === 'inbox' ? 'inbox' : 'library',
      )
      if (!(await activationIsCurrent(activation))) {
        await abandonCapture(activation)
        return
      }
      if (!resolvedTarget.ok) {
        await finishCapture(
          makeSnapshot({
            stage: 'failed',
            url: inFlight.url,
            title: inFlight.title,
            errorKind: resolvedTarget.error.kind,
            requestedKind: inFlight.requestedKind ?? 'auto',
          }),
          activation,
        )
        return
      }
      target = resolvedTarget.data
      ingestRequest = buildIngestBody(source, target)
      await persistInFlight(
        {
          ...inFlight,
          destination: target.destination,
          requestedKind: target.requestedKind,
          ingestRequest,
        },
        activation,
      )
    }

    if (!(await activationIsCurrent(activation))) {
      await abandonCapture(activation)
      return
    }
    const result = await client.ingest(ingestRequest, {
      idempotencyKey: inFlight.idempotencyKey,
    })
    if (!(await activationIsCurrent(activation))) {
      await abandonCapture(activation)
      return
    }
    if (!result.ok) {
      await finishCapture(
        makeSnapshot({
          stage: 'failed',
          url: inFlight.url,
          title: inFlight.title,
          errorKind: result.error.kind,
          requestedKind: target.requestedKind,
        }),
        activation,
      )
      return
    }

    await handleSubmitResponse(
      activation,
      source,
      inFlight.note,
      target,
      inFlight.idempotencyKey,
      inFlight.startedAt,
      result.data,
    )
  }

  /** Public wrapper that installs an early Promise lock before the first await. */
  function startCapture(
    note: string,
    tabId?: number,
    requestedKind: RequestedLibraryKind = 'auto',
  ): Promise<CaptureSnapshot> {
    if (startOperation) return startOperation

    const operation = runStartCapture(note, tabId, requestedKind)
    startOperation = operation
    void operation.then(
      () => {
        if (startOperation === operation) startOperation = null
      },
      () => {
        if (startOperation === operation) startOperation = null
      },
    )
    return operation
  }

  /**
   * 看门狗恢复：由 chrome.alarms 周期触发。
   *
   * 续跑条件（全部满足）：
   *   - store 里有进行中采集任务（说明有采集没走到终态）
   *   - 该任务 **未陈旧**——isInFlightStale 判定其墙钟耗时未超出采集预算。
   *     陈旧记录（如 clearInFlight 因 storage 故障被丢弃留下的僵尸记录、
   *     或采集异常卡死）不再续跑，直接清掉，避免对一个早已结束 / 死掉的
   *     任务无限 getLink，也避免它永久占着持久化槽位阻塞新采集。
   *   - 本 SW 实例没有正在跑的轮询循环（pollLoopRunning=false）——
   *     说明轮询循环已随上一个被回收的 SW 一起消失
   * 满足则续跑，但分两种情况：
   *   - attempt 已 >= MAX_POLL_ATTEMPTS（预算耗尽临界点，SW 恰在最后一轮落盘后
   *     被回收）：进 runCapturePolling 的 for 循环会一次都不执行，导致终态不发布、
   *     in-flight 不清。故直接走与活体预算耗尽一致的 still-processing 终态收尾。
   *   - 否则：用持久化的 attempt 断点续跑。
   * 当前 confirmed connection 不存在、会话身份验证失败，或 owner/revision 与
   * 持久化任务不一致时，将旧任务隔离为空闲态并清除，避免跨安装续跑。
   */
  async function runResumeIfNeeded(): Promise<void> {
    if (pollLoopRunning) return
    await waitForStateMutations()
    const metadata = await deps.store.getInFlightMetadata()
    if (!metadata) return
    // 陈旧的僵尸记录：不续跑，直接清除持久化槽位（不再阻塞新采集）。
    if (isInFlightStale(metadata, MAX_CAPTURE_AGE_MS, now())) {
      await clearMatchingInFlight(metadata)
      return
    }
    const activationResult = await deps.activateConnection()
    if (isTransientCaptureActivationFailure(activationResult)) return
    const activation = activationResult
    if (!activation || !captureOwnersEqual(activation.owner, metadata.owner)) {
      activation?.lease.revoke()
      if (
        activeActivation &&
        !captureOwnersEqual(activeActivation.owner, metadata.owner)
      ) {
        await clearMatchingInFlight(metadata)
        return
      }
      await abandonCapture()
      return
    }
    activeActivation = activation
    if (!(await activationIsCurrent(activation))) {
      await abandonCapture(activation)
      return
    }
    const inFlight = await deps.store.getInFlight(activation.recoveryKey)
    if (
      !inFlight ||
      !captureOwnersEqual(inFlight.owner, activation.owner) ||
      !(await activationIsCurrent(activation))
    ) {
      await abandonCapture(activation)
      return
    }
    // 轮询预算临界点：持久化的 attempt 已 >= MAX_POLL_ATTEMPTS（SW 恰在最后一轮
    // 落盘后、收尾前被回收）。此时 runCapturePolling 的 for 循环条件
    // `attempt < MAX_POLL_ATTEMPTS` 一开始就为 false，循环体一次都不进 ——
    // 既不发布终态、也不清 in-flight，记录会一直残留到 MAX_CAPTURE_AGE_MS 陈旧
    // 兜底才被动清除（期间还会阻塞新采集）。这里直接走与「活体轮询预算耗尽」
    // 一致的收尾：发布中性的 still-processing 终态快照 + finishCapture 清 in-flight，
    // 不进循环干等。
    if (inFlight.attempt >= MAX_POLL_ATTEMPTS) {
      await finishCapture(
        makeSnapshot({
          stage: 'still-processing',
          url: inFlight.url,
          title: inFlight.title,
          requestedKind: inFlight.requestedKind ?? 'auto',
        }),
        activation,
      )
      return
    }
    if (!inFlight.linkId) {
      inMemoryInFlight = inFlight
      await resumePendingSubmission(activation, inFlight)
      return
    }
    // 续跑前把持久化记录登记为本 SW 实例的内存记录——续跑期间若有并发
    // startCapture，内存级主防线据此挡住，不会重复发起采集。
    inMemoryInFlight = inFlight
    await runCapturePolling(activation, inFlight, inFlight.attempt)
  }

  /** Public watchdog entry with a synchronous single-flight guard. */
  function resumeIfNeeded(): Promise<void> {
    if (pollLoopRunning) return Promise.resolve()
    if (resumeOperation) return resumeOperation

    const operation = runResumeIfNeeded().catch(() => {
      // Alarm callbacks are fire-and-forget. Keep a failed recovery from
      // becoming an unhandled rejection or leaving the local guard occupied.
      console.warn('[capture] recovery failed unexpectedly')
    })
    resumeOperation = operation
    void operation.then(() => {
      if (resumeOperation === operation) resumeOperation = null
    })
    return operation
  }

  return {
    startCapture,
    getLatestSnapshot: async () => {
      try {
        await waitForStateMutations()
        const snapshotOwner = await deps.store.getSnapshotOwner()
        if (snapshotOwner === undefined) return { ...IDLE_CAPTURE_SNAPSHOT }
        if (snapshotOwner === null) return deps.store.getSnapshot()
        if (
          activeActivation &&
          captureOwnersEqual(activeActivation.owner, snapshotOwner)
        ) {
          if (await activationIsCurrent(activeActivation)) {
            const snapshot = await deps.store.getSnapshot(
              activeActivation.recoveryKey,
            )
            if (
              snapshot.owner &&
              captureOwnersEqual(snapshot.owner, activeActivation.owner) &&
              (await activationIsCurrent(activeActivation))
            ) {
              return snapshot
            }
          }
          return abandonCapture(activeActivation)
        }
        const activationResult = await deps.activateConnection()
        if (isTransientCaptureActivationFailure(activationResult)) {
          return { ...IDLE_CAPTURE_SNAPSHOT }
        }
        const activation = activationResult
        if (
          !activation ||
          !captureOwnersEqual(activation.owner, snapshotOwner)
        ) {
          activation?.lease.revoke()
          if (
            activeActivation &&
            !captureOwnersEqual(activeActivation.owner, snapshotOwner)
          ) {
            await clearMatchingSnapshot(snapshotOwner)
            return { ...IDLE_CAPTURE_SNAPSHOT }
          }
          return abandonCapture()
        }
        activeActivation = activation
        if (!(await activationIsCurrent(activation)))
          return abandonCapture(activation)
        const snapshot = await deps.store.getSnapshot(activation.recoveryKey)
        if (
          !snapshot.owner ||
          !captureOwnersEqual(snapshot.owner, activation.owner) ||
          !(await activationIsCurrent(activation))
        ) {
          return abandonCapture(activation)
        }
        return snapshot
      } catch {
        return { ...IDLE_CAPTURE_SNAPSHOT }
      }
    },
    resumeIfNeeded,
  }
}
