/**
 * @module background/capture-messaging
 * 当前页采集的生产侧 IO 接线 —— 运行于 background Service Worker。
 *
 * captureHandler.ts 的 createCaptureController 只认抽象 CaptureDeps，本模块
 * 提供 CaptureDeps 的真实实现，并把 controller 接到 webext-bridge 消息总线
 * 与 chrome.alarms 看门狗：
 *   - createCaptureDeps()：组装生产依赖（客户端构造 / 脚本注入 /
 *     快照推送 / 持久化存储 / 时间源）。其中 injectCapture 是唯一直面
 *     chrome.scripting / chrome.tabs 的「叶子适配器」。
 *   - setupCaptureMessaging(controller)：注册 START_CAPTURE / GET_CAPTURE_STATUS
 *     webext-bridge 监听，并注册 chrome.alarms 看门狗周期触发 resumeIfNeeded。
 *
 * MV3 看门狗：Service Worker 空闲约 30s 即被回收，轮询循环随之消失。
 * 注册一个 ~1 分钟周期的 chrome.alarms：alarm 触发会唤醒 SW 并跑监听器，
 * 调 controller.resumeIfNeeded() —— 若 storage.session 里有进行中采集任务、
 * 且本 SW 实例没在轮询，就从持久化的 attempt 断点续跑。
 *
 * 消费者：src/background/main.ts（SW 启动时组装 deps、创建 controller、注册监听）。
 */

import { onMessage, sendMessage } from 'webext-bridge/background'
import { capturePageContent } from '@/contentScripts/capture'
import { buildWebTagClientFromSettings } from '@/api/webtag-client'
import {
  CaptureActivationLease,
  createCaptureOwnerFingerprint,
  createCaptureRecoveryKey,
  isCaptureRecoverySecretEligible,
} from '@/api/capabilities'
import { getCaptureDestination } from '@/api/capture-preferences'
import { getWebTagSettings } from '@/composables/useSettings'
import { isForbiddenUrl } from '@/env'
import type { RawCapture } from '@/contentScripts/capture'
import type {
  CaptureActivation,
  CaptureController,
  CaptureDeps,
  CaptureInjectResult,
} from './captureHandler'
import { TRANSIENT_CAPTURE_ACTIVATION_FAILURE } from './captureHandler'
import {
  MSG_CAPTURE_STATUS_UPDATE,
  MSG_GET_CAPTURE_STATUS,
  MSG_START_CAPTURE,
} from './capture-protocol'
import {
  createSessionCaptureStore,
  type SessionStorageArea,
} from './capture-store'

// ── 看门狗常量 ──────────────────────────────────────────────

/** 采集恢复看门狗 alarm 的名称。 */
export const CAPTURE_WATCHDOG_ALARM = 'webtag:capture-watchdog'
/**
 * 看门狗 alarm 的周期（分钟）。chrome.alarms 周期最小 1 分钟。
 * SW 中途被回收的采集会在最多约 1 分钟内被这个 alarm 唤醒续跑。
 */
export const CAPTURE_WATCHDOG_PERIOD_MIN = 1

// ── 叶子适配器：注入抓取脚本到活动标签 ───────────────────────

/**
 * 向指定标签注入 capturePageContent 并取回结果。
 * tabId 为空时自动取当前窗口的活动标签。
 *
 * 这是采集流程中唯一直面 chrome.scripting / chrome.tabs 的函数——作为
 * CaptureDeps.injectCapture 注入给 controller，把 Chrome API 收口在一处。
 *
 * 失败原因区分：
 *   - restricted：目标标签 URL 命中 isForbiddenUrl（chrome:// / 应用商店 /
 *     扩展页等），executeScript 必定抛错。提前判定，避免把「页面本就不能采集」
 *     塞进泛化的 failed。executeScript 抛错时也兜底归类为 restricted——
 *     因为受限页面是注入抛错的绝对主因。
 *   - failed：无活动标签、空结果等真实失败。
 */
async function injectCapture(tabId?: number): Promise<CaptureInjectResult> {
  let targetTabId = tabId
  let targetUrl: string | undefined
  if (targetTabId === undefined) {
    const [activeTab] = await chrome.tabs.query({
      active: true,
      currentWindow: true,
    })
    targetTabId = activeTab?.id
    targetUrl = activeTab?.url
  } else {
    // popup / 右键菜单传入 tabId 时，回查 URL 以便提前判定受限页面。
    try {
      const tab = await chrome.tabs.get(targetTabId)
      targetUrl = tab?.url
    } catch {
      // 查不到标签信息：不阻断，交由 executeScript 实际尝试。
    }
  }
  if (targetTabId === undefined) {
    return { ok: false, reason: 'failed', message: 'no-active-tab' }
  }
  // 受限页面提前判定：命中即返回 restricted，不浪费一次必失败的注入。
  if (targetUrl && isForbiddenUrl(targetUrl)) {
    return { ok: false, reason: 'restricted', message: targetUrl }
  }
  try {
    const results = await chrome.scripting.executeScript({
      target: { tabId: targetTabId },
      func: capturePageContent,
    })
    const raw = results[0]?.result as RawCapture | undefined
    if (!raw || typeof raw.url !== 'string' || raw.url.length === 0) {
      // 注入成功但结果为空：真实失败，非受限页面。
      return { ok: false, reason: 'failed', message: 'empty-result' }
    }
    return { ok: true, data: raw }
  } catch (err) {
    // executeScript 抛错绝大多数是受限页面（chrome:// / 应用商店 / 扩展页）。
    // 上面 URL 预判已拦掉大部分，仍抛错的（如预判时查不到 URL）归类 restricted。
    return {
      ok: false,
      reason: 'restricted',
      message: err instanceof Error ? err.message : String(err),
    }
  }
}

// ── storage.session 适配器 ──────────────────────────────────

/**
 * 把 chrome.storage.session 适配为 capture-store 的 SessionStorageArea。
 *
 * chrome.storage.session 的 get/set/remove 都返回 Promise（MV3 标准），
 * 直接转发即可。单独包一层是为了把 chrome 全局收口在本接线模块，
 * captureHandler / capture-store 不直接 import chrome。
 */
function createSessionStorageArea(): SessionStorageArea {
  // chrome.storage.session 在 MV3 中可用；类型定义里偶有缺失，显式断言。
  const session = chrome.storage.session
  return {
    get: (keys) => session.get(keys),
    set: (items) => session.set(items),
    remove: (keys) => session.remove(keys),
  }
}

// ── 生产依赖组装 ────────────────────────────────────────────

/**
 * 组装 createCaptureController 所需的生产侧 CaptureDeps。
 *
 * - activateConnection：读取 confirmed 配置、验证会话身份并构造带 owner/lease
 *   的客户端激活；未配置或确定不匹配返回 null，可重试失败返回 transient
 *   sentinel，空或过短 token 仍可采集但不生成私密恢复密钥
 * - injectCapture：chrome.scripting 叶子适配器（见上）
 * - sendSnapshot：经 webext-bridge 推送给 popup；popup 已关闭时 reject 被
 *   catch 吞掉——快照已落 storage.session，popup 重开会经 GET_CAPTURE_STATUS 拉取
 * - store：chrome.storage.session 持久化存储（采集状态跨 SW 回收的关键）
 * - now：Date.now
 */
export interface CaptureDepsOptions {
  /** Called after an Inbox response becomes durable. */
  onInboxCapture?: () => void
}

export function createCaptureDeps(
  options: CaptureDepsOptions = {},
): CaptureDeps {
  return {
    activateConnection: async () => {
      const confirmed = await getWebTagSettings()
      const client = buildWebTagClientFromSettings(confirmed)
      if (!client) return null

      let identity: Awaited<ReturnType<typeof client.getSessionIdentity>>
      try {
        identity = await client.getSessionIdentity()
      } catch {
        return TRANSIENT_CAPTURE_ACTIVATION_FAILURE
      }
      if (!identity.ok) {
        if (
          identity.error.kind === 'network-unreachable' ||
          identity.error.kind === 'timeout' ||
          identity.error.kind === 'rate-limited' ||
          identity.error.status === 408 ||
          (identity.error.status !== undefined && identity.error.status >= 500)
        ) {
          return TRANSIENT_CAPTURE_ACTIVATION_FAILURE
        }
        return null
      }
      let fingerprint: string
      try {
        fingerprint = await createCaptureOwnerFingerprint(
          confirmed.backendUrl,
          identity.data,
        )
      } catch {
        return null
      }
      if (
        confirmed.connectionOwnerFingerprint &&
        confirmed.connectionOwnerFingerprint !== fingerprint
      ) {
        return null
      }

      const owner = {
        fingerprint,
        revision: confirmed.connectionRevision,
      }
      let recoveryKey: CryptoKey | undefined
      if (isCaptureRecoverySecretEligible(confirmed.accessToken)) {
        try {
          recoveryKey = await createCaptureRecoveryKey(
            confirmed.accessToken,
            owner,
          )
        } catch {
          return TRANSIENT_CAPTURE_ACTIVATION_FAILURE
        }
      }
      const activationIdentity = JSON.stringify([
        confirmed.backendUrl.trim(),
        confirmed.accessToken.trim(),
        confirmed.connectionRevision,
        confirmed.connectionOwnerFingerprint,
      ])
      const lease = new CaptureActivationLease(owner, async () => {
        const current = await getWebTagSettings()
        const currentIdentity = JSON.stringify([
          current.backendUrl.trim(),
          current.accessToken.trim(),
          current.connectionRevision,
          current.connectionOwnerFingerprint,
        ])
        return currentIdentity === activationIdentity
      })
      return {
        client,
        owner,
        lease,
        ...(recoveryKey ? { recoveryKey } : {}),
      } satisfies CaptureActivation
    },
    getCaptureDestination,
    onInboxCapture: options.onInboxCapture,
    injectCapture,
    sendSnapshot: (snapshot) => {
      void sendMessage(MSG_CAPTURE_STATUS_UPDATE, snapshot, 'popup').catch(
        () => {
          /* popup 未打开，忽略 */
        },
      )
    },
    store: createSessionCaptureStore(createSessionStorageArea()),
    now: Date.now,
  }
}

// ── 消息监听与看门狗注册 ────────────────────────────────────

/**
 * 注册采集相关的 webext-bridge 消息监听与 chrome.alarms 看门狗。
 * 由 background/main.ts 在 SW 启动时调用一次。
 *
 * - START_CAPTURE：触发采集，await 到「抓取+提交」完成后返回初始快照
 * - GET_CAPTURE_STATUS：异步返回 storage.session 持有的最近一次快照
 * - chrome.alarms 看门狗：~1 分钟周期，触发时调 resumeIfNeeded 续跑被
 *   SW 回收打断的采集
 *
 * @param controller 由 createCaptureController 创建的唯一采集控制器
 */
export function setupCaptureMessaging(controller: CaptureController): void {
  // ProtocolMap 已登记 webtag:* 协议，data / 返回值均为强类型，无需断言。
  onMessage(MSG_START_CAPTURE, async ({ data }) => {
    return controller.startCapture(
      data?.note ?? '',
      data?.tabId,
      data?.requestedKind,
    )
  })

  // getLatestSnapshot 现为异步（从 storage.session 读），直接返回 Promise。
  onMessage(MSG_GET_CAPTURE_STATUS, () => controller.getLatestSnapshot())

  // 看门狗 alarm：周期触发，唤醒 SW 并续跑被回收打断的采集轮询。
  chrome.alarms.create(CAPTURE_WATCHDOG_ALARM, {
    periodInMinutes: CAPTURE_WATCHDOG_PERIOD_MIN,
  })
  chrome.alarms.onAlarm.addListener((alarm) => {
    if (alarm.name !== CAPTURE_WATCHDOG_ALARM) return
    void controller.resumeIfNeeded()
  })
}
