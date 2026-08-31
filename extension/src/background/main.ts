/**
 * 精简后的 background Service Worker 入口。
 *
 * 当前扩展只保留采集链路：
 * - popup 采集
 * - 右键采集
 * - 采集状态轮询 / 恢复
 * - 首次安装打开设置页
 *
 * NaiveTab 的快捷键、Port 与旧 content script 接线已随 full build 删除，
 * 当前 background 入口不再导入它们。
 */
import { createCaptureController } from './captureHandler'
import { createCaptureDeps, setupCaptureMessaging } from './capture-messaging'
import { setupContextMenu } from './contextMenu'
import { setupRssDiscoveryMessaging } from './rss-discovery'
import { createProductionPendingBadgeController } from './pending-badge'

// ── 冷启动引导（首次安装打开设置页）──────────────────────────────────────
//
// WebTag 扩展依赖用户在设置页填写后端地址 + Token 才能采集 / 浏览知识库。
// 首次安装时若不引导，用户点扩展图标只会看到一个无法采集的 popup（后端未配置）。
// 这里在安装时主动打开设置页，让用户第一时间完成连接配置。
//
// MV3 SW 规范：onInstalled 监听器必须在 SW 同步模块加载阶段顶层注册——SW 被
// 唤醒重放安装事件时，监听器须已就位，否则事件丢失。reason 仅在 'install'
// （全新安装）时打开设置页；'update' / 'chrome_update' 不打扰已配置的老用户。
// openOptionsPage 返回 Promise，失败时 catch 兜底避免未处理 rejection。
chrome.runtime.onInstalled.addListener((details) => {
  if (details.reason === 'install') {
    chrome.runtime.openOptionsPage().catch((err: unknown) => {
      console.warn('[WebTag] 首次安装打开设置页失败', err)
    })
  }
})

// ── WebTag 当前页采集（Phase 3 Task 3B）────────────────────────────────────
//
// createCaptureController 用生产侧 IO 依赖（createCaptureDeps）创建唯一一个
// 采集控制器，闭包持有「最近快照」与「采集进行中」状态。
// setupCaptureMessaging 把 controller 接到 popup ↔ background 的采集消息监听
// （webext-bridge：START_CAPTURE / GET_CAPTURE_STATUS）。
// setupContextMenu 把同一个 controller 接到右键菜单「采集此页到 WebTag」，
// 保证两条触发入口共用同一份并发守卫与最近快照。
//
// 三者在 SW 启动的同步阶段注册 listener，与既有 Port / onMessage listener
// 同批完成，不依赖 waitInitialized()（采集功能不依赖 NaiveTab 键盘配置缓存）。
const pendingBadge = createProductionPendingBadgeController()
void pendingBadge.start()
chrome.storage.onChanged.addListener((changes, areaName) => {
  if (areaName === 'local' && 'webtag_settings' in changes) {
    void pendingBadge.refresh()
  }
})

const captureDeps = createCaptureDeps({
  onInboxCapture: () => {
    void pendingBadge.refresh()
  },
})
const captureController = createCaptureController(captureDeps)
setupCaptureMessaging(captureController)
setupContextMenu(captureController, async () => {
  const activation = await captureDeps.activateConnection()
  return activation && 'client' in activation ? activation.client : null
})
setupRssDiscoveryMessaging()
