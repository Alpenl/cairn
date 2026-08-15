/**
 * @module composables/useSettings
 * WebTag 扩展配置的读写 composable。
 *
 * 持久化目标：`chrome.storage.local`。
 * 选择 chrome.storage 而非 NaiveTab 既有 useStorageLocal（基于页面 localStorage）的原因：
 * WebTag 配置（后端地址 / Token / 链接打开行为）需要被 newtab、popup、
 * content script、background 多个上下文共享读取。localStorage 各上下文隔离、
 * content script 也无法访问 newtab 的 localStorage；chrome.storage 是扩展内
 * 唯一跨上下文可共享、且能监听 onChanged 的存储。manifest 已含 `storage` 权限。
 *
 * 设计沿用 NaiveTab useStorageLocal 的形态：返回响应式 ref，非连接字段对 ref
 * 的修改经防抖后自动落盘。连接字段只允许通过显式测试后的
 * confirmConnection 原子提交。差异：异步初始化（chrome.storage 是 Promise
 * API），并通过 chrome.storage.onChanged 监听其它上下文的写入做反向同步。
 *
 * 用法：
 *   const { settings, ready, update, confirmConnection, reload } = useWebTagSettings()
 *   await ready                       // 等待首次从 storage 加载完成
 *   const res = await update({ linkOpenBehavior: 'new' }) // 部分更新并立即落盘
 *   const confirmed = await confirmConnection(connection) // 原子激活已验证连接
 *   if (!res.ok) { /* 落盘失败，res.error 携带原因 *\/ }
 *
 * 消费者：后端连接设置 UI（Task 2A）、API 客户端构造方、知识库桌面、采集编排。
 */

import { ref, watch, type Ref } from 'vue'

// ── 配置模型 ────────────────────────────────────────────────

/** 知识库链接点击后的打开行为。 */
export type LinkOpenBehavior = 'current' | 'new'

/**
 * WebTag 扩展配置。仅存放本扩展自身的配置，
 * 不与 NaiveTab 既有的 localConfig（Widget 配置）混用。
 *
 * 注：深浅色主题复用 NaiveTab 自身的外观设置（currTheme），WebTag 不再
 * 维护独立的 theme 字段。
 */
export interface WebTagSettings {
  /** WebTag 后端基础地址，如 http://localhost:8080。空串表示未配置。 */
  backendUrl: string
  /** Reader 前端完整地址。与后端分开部署时应显式配置。 */
  readerUrl: string
  /** 后端访问 Token（Bearer）。空串表示未配置。 */
  accessToken: string
  /** Last verified installation owner hash. Never contains the raw namespace. */
  connectionOwnerFingerprint: string
  /** Monotonic activation revision advanced by each explicit confirmation. */
  connectionRevision: number
  /** 知识库链接打开行为：当前标签页 / 新标签页。 */
  linkOpenBehavior: LinkOpenBehavior
}

/** Immutable candidate committed only after its URL and token were verified together. */
export interface ConfirmedConnection {
  backendUrl: string
  readerUrl: string
  accessToken: string
  connectionOwnerFingerprint: string
}

/**
 * chrome.storage.local 中存放 WebTag 配置的键。
 * 单键存整个配置对象，避免与 NaiveTab 既有键冲突，也便于整体读写与监听。
 */
export const WEBTAG_SETTINGS_STORAGE_KEY = 'webtag_settings'

/** 配置默认值。首次安装或 storage 缺键时使用。 */
export const DEFAULT_WEBTAG_SETTINGS: WebTagSettings = {
  backendUrl: '',
  readerUrl: '',
  accessToken: '',
  connectionOwnerFingerprint: '',
  connectionRevision: 0,
  linkOpenBehavior: 'current',
}

/** 防抖落盘延迟（毫秒）。沿用 NaiveTab useStorageLocal 的 800ms。 */
const PERSIST_DEBOUNCE_MS = 800

// ── 内部工具 ────────────────────────────────────────────────

/**
 * 以默认值为模板合并存储值，过滤未知字段、补齐缺失字段。
 * 保证升级后新增字段对老用户也有默认值（与 NaiveTab mergeState 同理，单层即可）。
 */
function mergeWithDefaults(stored: unknown): WebTagSettings {
  if (typeof stored !== 'object' || stored === null) {
    return { ...DEFAULT_WEBTAG_SETTINGS }
  }
  const src = stored as Partial<WebTagSettings>
  return {
    backendUrl:
      typeof src.backendUrl === 'string'
        ? src.backendUrl
        : DEFAULT_WEBTAG_SETTINGS.backendUrl,
    readerUrl:
      typeof src.readerUrl === 'string'
        ? src.readerUrl
        : DEFAULT_WEBTAG_SETTINGS.readerUrl,
    accessToken:
      typeof src.accessToken === 'string'
        ? src.accessToken
        : DEFAULT_WEBTAG_SETTINGS.accessToken,
    connectionOwnerFingerprint:
      typeof src.connectionOwnerFingerprint === 'string'
        ? src.connectionOwnerFingerprint
        : DEFAULT_WEBTAG_SETTINGS.connectionOwnerFingerprint,
    connectionRevision:
      typeof src.connectionRevision === 'number' &&
      Number.isSafeInteger(src.connectionRevision) &&
      src.connectionRevision >= 0
        ? src.connectionRevision
        : DEFAULT_WEBTAG_SETTINGS.connectionRevision,
    linkOpenBehavior:
      src.linkOpenBehavior === 'current' || src.linkOpenBehavior === 'new'
        ? src.linkOpenBehavior
        : DEFAULT_WEBTAG_SETTINGS.linkOpenBehavior,
  }
}

/** 从 chrome.storage.local 读取并合并出完整配置。 */
async function readSettings(): Promise<WebTagSettings> {
  const result = await chrome.storage.local.get(WEBTAG_SETTINGS_STORAGE_KEY)
  return mergeWithDefaults(result[WEBTAG_SETTINGS_STORAGE_KEY])
}

/** 把完整配置写入 chrome.storage.local。 */
async function writeSettings(value: WebTagSettings): Promise<void> {
  await chrome.storage.local.set({ [WEBTAG_SETTINGS_STORAGE_KEY]: value })
}

// ── composable ──────────────────────────────────────────────

/**
 * update() 的结果。
 * - ok=true  落盘成功
 * - ok=false 落盘失败（如 storage 配额超限），error 携带原因
 *
 * update() 不抛异常，调用方据 ok 分支决定是否提示用户。
 */
export type UpdateResult = { ok: true } | { ok: false; error: Error }

/** useWebTagSettings 的返回结构。 */
export interface UseWebTagSettingsReturn {
  /** 响应式配置。非连接字段的直接修改会在防抖后自动持久化。 */
  settings: Ref<WebTagSettings>
  /** 首次从 storage 加载完成的 Promise。读取配置前应 await 它。 */
  ready: Promise<void>
  /**
   * 部分更新配置并立即落盘（绕过防抖），用于"保存"按钮等需要即时确认的场景。
   * 不抛异常：落盘失败时返回 { ok: false, error }，由调用方决定如何提示。
   */
  update: (patch: Partial<WebTagSettings>) => Promise<UpdateResult>
  /** Atomically activate a URL/token tuple after an explicit successful test. */
  confirmConnection: (connection: ConfirmedConnection) => Promise<UpdateResult>
  /** 重新从 storage 拉取并覆盖本地 ref（手动刷新）。 */
  reload: () => Promise<void>
  /** 解除 watch 与 chrome.storage.onChanged 监听。组件卸载时调用以避免泄漏。 */
  dispose: () => void
}

/**
 * 创建一个 WebTag 配置 composable 实例。
 *
 * 每次调用返回独立的响应式 ref。同一上下文内多次调用会各自维护副本，
 * 但都监听同一 chrome.storage 键，写入后通过 onChanged 互相同步。
 */
export function useWebTagSettings(): UseWebTagSettingsReturn {
  const settings = ref<WebTagSettings>({ ...DEFAULT_WEBTAG_SETTINGS })
  let committedSettings: WebTagSettings = { ...DEFAULT_WEBTAG_SETTINGS }

  let persistTimer: ReturnType<typeof setTimeout> | undefined
  // 程序化写入标记（onChanged → ref / reload / update）。
  // 程序化修改 settings.value 前置位，watch 回调"消费一次即清除"——
  // 标记的清除发生在 watch 回调内部，不再依赖 Promise 微任务与 Vue watch
  // 调度的相对顺序，因此不会出现旧实现中"微任务先于 watch 跑导致标记提前失效、
  // 程序化写入被当成用户修改又落盘一次"的竞态。
  let applyingProgrammatic = false

  /** 程序化方式赋值 settings.value：置标记 → 赋值，由 watch 回调消费标记。 */
  const assignProgrammatically = (value: WebTagSettings): void => {
    applyingProgrammatic = true
    committedSettings = { ...value }
    settings.value = { ...value }
  }

  const connectionChanged = (value: WebTagSettings): boolean =>
    value.backendUrl !== committedSettings.backendUrl ||
    value.readerUrl !== committedSettings.readerUrl ||
    value.accessToken !== committedSettings.accessToken ||
    value.connectionOwnerFingerprint !==
      committedSettings.connectionOwnerFingerprint ||
    value.connectionRevision !== committedSettings.connectionRevision

  // 防抖落盘：非连接字段的深层修改在 PERSIST_DEBOUNCE_MS 后写入 storage。
  //
  // ⚠️ deep: true 不可省略。WebTagSettings 虽是扁平对象，但消费方可能直接修改
  // `settings.value.linkOpenBehavior` 而非重新赋值整个 ref；
  // 字段级修改不会被浅 watch 捕获。连接字段若被直接修改则恢复 confirmed 值，
  // 只有 confirmConnection 可以持久化完整连接元组。
  const stopWatch = watch(
    settings,
    (value) => {
      // 程序化写入（reload / update / onChanged 同步）：在回调内消费标记并跳过落盘。
      if (applyingProgrammatic) {
        applyingProgrammatic = false
        return
      }
      // Connection fields are confirmed state, never an edit buffer. Restore
      // direct mutations so neither persistence nor capability probes can
      // observe a partially typed URL/token tuple.
      if (connectionChanged(value)) {
        if (persistTimer) clearTimeout(persistTimer)
        const restored: WebTagSettings = {
          ...value,
          backendUrl: committedSettings.backendUrl,
          readerUrl: committedSettings.readerUrl,
          accessToken: committedSettings.accessToken,
          connectionOwnerFingerprint:
            committedSettings.connectionOwnerFingerprint,
          connectionRevision: committedSettings.connectionRevision,
        }
        assignProgrammatically(restored)
        return
      }
      // 用户修改：防抖后落盘。
      if (persistTimer) clearTimeout(persistTimer)
      const pendingValue = { ...value }
      persistTimer = setTimeout(() => {
        // chrome.storage.local.set 可能因配额超限 / 瞬时故障 reject。
        // .catch 兜底：记录日志，避免静默丢失编辑且产生未处理的 Promise rejection。
        // 防抖路径无返回值，无法像 update() 那样把失败回传给调用方，故只能日志。
        void writeSettings(pendingValue).catch((err: unknown) => {
          console.warn('[WebTag] 防抖持久化扩展配置失败', err)
        })
      }, PERSIST_DEBOUNCE_MS)
    },
    { deep: true },
  )

  // 监听其它上下文（popup / background / 另一个标签页）对配置的写入，反向同步到本 ref。
  const handleStorageChange = (
    changes: Record<string, chrome.storage.StorageChange>,
    areaName: string,
  ) => {
    if (areaName !== 'local') return
    const change = changes[WEBTAG_SETTINGS_STORAGE_KEY]
    if (!change) return
    assignProgrammatically(mergeWithDefaults(change.newValue))
  }
  chrome.storage.onChanged.addListener(handleStorageChange)

  const reload = async (): Promise<void> => {
    const loaded = await readSettings()
    assignProgrammatically(loaded)
  }

  const update = async (
    patch: Partial<WebTagSettings>,
  ): Promise<UpdateResult> => {
    if (
      patch.backendUrl !== undefined ||
      patch.readerUrl !== undefined ||
      patch.accessToken !== undefined ||
      patch.connectionOwnerFingerprint !== undefined ||
      patch.connectionRevision !== undefined
    ) {
      return {
        ok: false,
        error: new Error('连接配置必须通过 confirmConnection 原子提交'),
      }
    }
    const next: WebTagSettings = { ...settings.value, ...patch }
    // 先取消待执行的防抖写入，避免与本次立即写入竞争。
    if (persistTimer) clearTimeout(persistTimer)
    assignProgrammatically(next)
    try {
      await writeSettings(next)
      return { ok: true }
    } catch (err) {
      // 落盘失败（如 storage 配额超限）：捕获并归一化为 UpdateResult，
      // 不向调用方抛出，避免未捕获的 Promise rejection。
      // 标记的清除已交由 watch 回调，不会因落盘失败而残留——
      // ref 已被程序化赋值，watch 仍会照常触发并消费标记。
      const error =
        err instanceof Error
          ? new Error(`写入扩展配置失败：${err.message}`, { cause: err })
          : new Error(`写入扩展配置失败：${String(err)}`)
      return { ok: false, error }
    }
  }

  const confirmConnection = async (
    connection: ConfirmedConnection,
  ): Promise<UpdateResult> => {
    if (!/^[a-f0-9]{64}$/.test(connection.connectionOwnerFingerprint)) {
      return {
        ok: false,
        error: new Error('连接 owner fingerprint 无效'),
      }
    }
    if (committedSettings.connectionRevision >= Number.MAX_SAFE_INTEGER) {
      return {
        ok: false,
        error: new Error('连接 activation revision 已耗尽'),
      }
    }
    if (persistTimer) clearTimeout(persistTimer)
    const next: WebTagSettings = {
      ...committedSettings,
      backendUrl: connection.backendUrl.trim(),
      readerUrl: connection.readerUrl.trim(),
      accessToken: connection.accessToken.trim(),
      connectionOwnerFingerprint: connection.connectionOwnerFingerprint,
      connectionRevision: committedSettings.connectionRevision + 1,
    }
    try {
      // storage.local stores the complete tuple under one key. Update the
      // reactive confirmed view only after the durable write succeeds.
      await writeSettings(next)
      assignProgrammatically(next)
      return { ok: true }
    } catch (err) {
      const error =
        err instanceof Error
          ? new Error(`写入扩展配置失败：${err.message}`, { cause: err })
          : new Error(`写入扩展配置失败：${String(err)}`)
      return { ok: false, error }
    }
  }

  const dispose = (): void => {
    if (persistTimer) clearTimeout(persistTimer)
    stopWatch()
    chrome.storage.onChanged.removeListener(handleStorageChange)
  }

  // 首次加载：异步从 storage 拉取初始值。
  const ready = reload()

  return { settings, ready, update, confirmConnection, reload, dispose }
}

/**
 * 一次性读取当前配置快照（不建立响应式订阅）。
 * 适合 background / content script 等只需读一次的场景。
 */
export async function getWebTagSettings(): Promise<WebTagSettings> {
  return readSettings()
}
