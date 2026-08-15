/**
 * @module background/contextMenu
 * 「采集此页到 WebTag」右键菜单 —— 运行于 background Service Worker。
 *
 * 在网页右键菜单注册一个 page 上下文菜单项，点击后等价于在 popup 中无备注提交：
 * 复用注入的 CaptureController.startCapture（note 传空串、tabId 取自点击事件）。
 *
 * Service Worker 注意事项：
 *   - 菜单项必须用 chrome.contextMenus.create 注册，但不能在 SW 顶层直接 create
 *     （SW 每次唤醒重跑顶层代码，重复 id 会报错）。注册时机有两个：
 *       · chrome.runtime.onInstalled —— 安装 / 更新时触发；
 *       · chrome.runtime.onStartup   —— 浏览器启动时触发。
 *     onInstalled **不会**在浏览器重启时触发，单靠它会让菜单项在重启后
 *     悄无声息地消失。两个事件都接上 createMenu（create 前先 removeAll，幂等），
 *     保证采集入口不会丢失。
 *   - onClicked 监听器在顶层同步注册（SW 唤醒即注册），其回调内做的异步采集
 *     交给 controller.startCapture。
 *
 * controller 由 background/main.ts 创建后注入：右键菜单与 popup 共用同一个
 * 采集控制器，保证「采集进行中」并发守卫与最近快照在两条触发入口间一致。
 *
 * 可测试性：标题选择与点击处理抽成两个纯函数 resolveMenuTitle / handleMenuClick
 * 并导出，单测直接传值断言；setupContextMenu 只保留无法单测的 addListener 接线。
 *
 * 消费者：src/background/main.ts（SW 启动时调用 setupContextMenu(controller)）、
 *         src/background/contextMenu.test.ts（直测 resolveMenuTitle / handleMenuClick）。
 */

import type { CaptureController } from './captureHandler'
import type { RequestedLibraryKind } from './capture-protocol'
import type { WebTagClient } from '@/api/webtag-client'
import { deriveExtensionCapabilityPolicy } from '@/api/capabilities'
import { WEBTAG_SETTINGS_STORAGE_KEY } from '@/composables/useSettings'

/** 右键菜单项的唯一 id。 */
const CAPTURE_MENU_ID = 'webtag-capture-page'
const CAPTURE_MENU_AUTO_ID = 'webtag-capture-auto'
const CAPTURE_MENU_READING_ID = 'webtag-capture-reading'
const CAPTURE_MENU_SITE_ID = 'webtag-capture-site'

type ContextMenuClient = Pick<WebTagClient, 'getCapabilities'>
export type ContextMenuClientFactory = () => Promise<ContextMenuClient | null>

/**
 * i18n key 与回退文案。
 *
 * background SW 没有 vue-i18n 运行时（vue-i18n 绑定在 Vue app 上），
 * 这里用 chrome.i18n.getUILanguage 选语言、内联文案表——与 contentScripts/toast.ts
 * 的轻量 t() 同一模式。新增的功能域 capture.* key 仍会在交付报告中列出，
 * 由主 Agent 统一并入 locale JSON（供 popup 等有 vue-i18n 的上下文使用）。
 */
const MENU_TITLE: Record<string, string> = {
  'zh-CN': '采集此页到 WebTag',
  'en-US': 'Capture this page to WebTag',
}

/**
 * 按浏览器 UI 语言取菜单标题 —— 纯函数。
 *
 * 入参为 UI 语言字符串（如 'zh-CN' / 'en-US'），而非内部调用
 * chrome.i18n.getUILanguage()，以便单测直接传值断言而无需 mock chrome API。
 * 调用方（setupContextMenu）负责取实际 UI 语言后传入。
 *
 * @param uiLanguage 浏览器 UI 语言字符串
 * @returns zh-* 语言返回中文标题，其余返回英文标题
 */
export function resolveMenuTitle(uiLanguage: string): string {
  if (uiLanguage.startsWith('zh')) return MENU_TITLE['zh-CN']
  return MENU_TITLE['en-US']
}

const MENU_KIND_TITLE: Record<
  'auto' | 'reading' | 'site',
  { zh: string; en: string }
> = {
  auto: { zh: '自动分类', en: 'Auto classify' },
  reading: { zh: '保存为阅读', en: 'Save as reading' },
  site: { zh: '收藏为网站', en: 'Save as website' },
}

/** Resolve an explicit child-menu label using the browser UI language. */
export function resolveMenuItemTitle(
  uiLanguage: string,
  kind: RequestedLibraryKind,
): string {
  const labels = MENU_KIND_TITLE[kind]
  return uiLanguage.startsWith('zh') ? labels.zh : labels.en
}

/**
 * 处理右键菜单点击 —— 与 chrome.contextMenus.onClicked 监听器解耦的可测试核心。
 *
 * 把「id 守卫 + 触发无备注采集」抽成纯函数：onClicked 回调只负责解构事件参数
 * 并转调本函数。单测可直接构造 menuItemId / tabId / 假 startCapture 验证逻辑。
 *
 * @param menuItemId 被点击菜单项的 id（来自 OnClickData.menuItemId）
 * @param tabId      被右键标签页的 id（来自 onClicked 第二参 tab?.id，可能为 undefined）
 * @param startCapture 采集触发函数，等价于 controller.startCapture
 */
export function handleMenuClick(
  menuItemId: string | number,
  tabId: number | undefined,
  startCapture: (
    note: string,
    tabId?: number,
    requestedKind?: RequestedLibraryKind,
  ) => void,
): void {
  const kindByID: Record<string, RequestedLibraryKind> = {
    [CAPTURE_MENU_ID]: 'auto',
    [CAPTURE_MENU_AUTO_ID]: 'auto',
    [CAPTURE_MENU_READING_ID]: 'reading',
    [CAPTURE_MENU_SITE_ID]: 'site',
  }
  const requestedKind = kindByID[String(menuItemId)]
  if (!requestedKind) return
  // 右键菜单等价于「无备注提交」。tabId 取自被右键的标签页。
  // Preserve the legacy parent item wire shape; the three explicit children
  // always carry a concrete requested kind.
  if (menuItemId === CAPTURE_MENU_ID) startCapture('', tabId)
  else startCapture('', tabId, requestedKind)
}

/**
 * 创建右键菜单项。在 onInstalled 中调用。
 * 先 removeAll 再 create，避免扩展更新后旧菜单项残留导致 id 冲突。
 */
export async function probeSiteCaptureCapability(
  buildClient: ContextMenuClientFactory,
): Promise<boolean> {
  try {
    const client = await buildClient()
    if (!client || typeof client.getCapabilities !== 'function') return false
    const result = await client.getCapabilities()
    return (
      result.ok && deriveExtensionCapabilityPolicy(result.data).site === true
    )
  } catch {
    return false
  }
}

function createMenu(
  buildClient: ContextMenuClientFactory,
  generationIsCurrent: () => boolean,
  onBasePhaseComplete: () => void,
): void {
  chrome.contextMenus.removeAll(() => {
    try {
      if (!generationIsCurrent()) return
      chrome.contextMenus.create({
        id: CAPTURE_MENU_ID,
        title: resolveMenuTitle(chrome.i18n.getUILanguage()),
        // 仅在普通网页（page 上下文）显示，不在链接 / 图片 / 选区等子上下文出现。
        contexts: ['page'],
        // 受限页面（chrome:// 等）无法采集，用 documentUrlPatterns 限定可注入的协议。
        documentUrlPatterns: ['http://*/*', 'https://*/*'],
      })
      for (const [id, kind] of [
        [CAPTURE_MENU_AUTO_ID, 'auto'],
        [CAPTURE_MENU_READING_ID, 'reading'],
      ] as const) {
        chrome.contextMenus.create({
          id,
          parentId: CAPTURE_MENU_ID,
          title: resolveMenuItemTitle(chrome.i18n.getUILanguage(), kind),
          contexts: ['page'],
          documentUrlPatterns: ['http://*/*', 'https://*/*'],
        })
      }

      // Keep the site entry absent while probing. A failed, stale, or denied
      // probe never creates it; captureHandler independently rechecks before
      // page injection in case capability changes after menu construction.
      void probeSiteCaptureCapability(buildClient).then((enabled) => {
        if (!enabled || !generationIsCurrent()) return
        chrome.contextMenus.create({
          id: CAPTURE_MENU_SITE_ID,
          parentId: CAPTURE_MENU_ID,
          title: resolveMenuItemTitle(chrome.i18n.getUILanguage(), 'site'),
          contexts: ['page'],
          documentUrlPatterns: ['http://*/*', 'https://*/*'],
        })
      })
    } finally {
      onBasePhaseComplete()
    }
  })
}

/**
 * 注册右键菜单与点击监听。由 background/main.ts 在 SW 启动时调用一次。
 *
 * - onInstalled：安装 / 更新时创建菜单项
 * - onStartup：浏览器启动时同样创建菜单项 —— onInstalled 不在重启时触发，
 *   单靠它会让采集入口在浏览器重启后消失，onStartup 是必须的安全网
 * - onClicked：点击采集菜单项时触发无备注采集
 *
 * @param controller 采集控制器，与 popup 共用同一实例
 */
export function setupContextMenu(
  controller: CaptureController,
  buildClient: ContextMenuClientFactory = async () => null,
): void {
  let menuGeneration = 0
  let basePhaseInFlight = false
  let rebuildPending = false

  const runPendingRebuild = () => {
    if (basePhaseInFlight || !rebuildPending) return

    rebuildPending = false
    basePhaseInFlight = true
    const generation = menuGeneration
    createMenu(
      buildClient,
      () => generation === menuGeneration,
      () => {
        basePhaseInFlight = false
        runPendingRebuild()
      },
    )
  }

  const rebuildMenu = () => {
    menuGeneration += 1
    rebuildPending = true
    runPendingRebuild()
  }

  // 安装 / 更新时创建菜单。SW 顶层不能直接 create（重复 id 报错）。
  chrome.runtime.onInstalled.addListener(() => {
    rebuildMenu()
  })

  // 浏览器启动时也创建菜单。onInstalled 不在重启时触发，缺这个监听
  // 会让右键采集入口在浏览器重启后悄无声息地消失。createMenu 先 removeAll
  // 再 create，幂等，与 onInstalled 同时触发也不会冲突。
  chrome.runtime.onStartup.addListener(() => {
    rebuildMenu()
  })

  // First install normally creates the menu before the user has configured a
  // backend. Rebuild as soon as the connection identity changes so a newly
  // granted site capability does not require a browser restart.
  chrome.storage.onChanged.addListener((changes, areaName) => {
    if (areaName !== 'local' || !(WEBTAG_SETTINGS_STORAGE_KEY in changes)) {
      return
    }
    rebuildMenu()
  })

  // 点击菜单项 → 触发采集。监听器只解构事件参数，逻辑委托给 handleMenuClick。
  chrome.contextMenus.onClicked.addListener((info, tab) => {
    handleMenuClick(info.menuItemId, tab?.id, (note, tabId, requestedKind) => {
      void controller.startCapture(note, tabId, requestedKind)
    })
  })
}
