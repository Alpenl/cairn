/**
 * Vitest 全局测试环境初始化
 *
 * 职责：
 * 1. Mock Chrome Extension API（chrome.storage / chrome.tabs / chrome.runtime 等）
 * 2. 注入 window.$t / $message / $notification 等全局 API
 * 3. Polyfill CompressionStream / DecompressionStream
 * 4. 每个测试前清空 localStorage
 */

import { beforeEach, vi } from 'vitest'
import { CompressionStream, DecompressionStream } from 'node:stream/web'

// ── Chrome Extension API Mock ──

// chrome.storage.sync 模拟数据
const mockSyncData: Record<string, unknown> = {}

// chrome.storage.local 模拟数据：真实内存对象，set/remove 会就地变更。
const mockLocalData: Record<string, unknown> = {}

// chrome.storage.session 模拟数据：MV3 会话级内存存储，采集子系统持久化用。
const mockSessionData: Record<string, unknown> = {}

/**
 * 按 chrome.storage.get 的 keys 入参形态从给定数据源筛选结果。
 * 支持：null（全量）、string（单键）、string[]（多键）、
 * 以及对象形式（键 → 默认值，缺键时回退默认值）。
 */
function pickByKeys(
  source: Record<string, unknown>,
  keys: string | string[] | Record<string, unknown> | null | undefined,
): Record<string, unknown> {
  if (keys === null || keys === undefined) {
    return { ...source }
  }
  if (typeof keys === 'string') {
    return { [keys]: source[keys] }
  }
  if (Array.isArray(keys)) {
    const result: Record<string, unknown> = {}
    for (const key of keys) {
      result[key] = source[key]
    }
    return result
  }
  // 对象形态：键为字段名，值为该字段缺失时的默认值。
  const result: Record<string, unknown> = {}
  for (const [key, defaultValue] of Object.entries(keys)) {
    result[key] = key in source ? source[key] : defaultValue
  }
  return result
}

// chrome.storage.onChanged 的活跃监听器集合（addListener 注册、removeListener 注销）。
type StorageChangeListener = (
  changes: Record<string, chrome.storage.StorageChange>,
  areaName: string,
) => void
const storageChangeListeners = new Set<StorageChangeListener>()

/**
 * 触发 chrome.storage.onChanged 的所有活跃监听器。
 * set/remove/clear 改变 mockLocalData 后调用，模拟真实 onChanged 行为。
 */
function emitStorageChange(
  changes: Record<string, chrome.storage.StorageChange>,
  areaName: string,
): void {
  // 拷贝一份再遍历，避免监听器在回调中增删监听导致迭代异常。
  for (const listener of [...storageChangeListeners]) {
    listener(changes, areaName)
  }
}

const chromeMock = {
  storage: {
    sync: {
      get: vi.fn((keys, callback) => {
        if (typeof keys === 'string' && callback === undefined) {
          // Promise form: return Promise
          return Promise.resolve({ [keys]: mockSyncData[keys] })
        }
        if (keys === null) {
          callback({ ...mockSyncData })
        } else if (typeof keys === 'string') {
          callback({ [keys]: mockSyncData[keys] })
        } else {
          const result: Record<string, unknown> = {}
          for (const key of keys) {
            result[key] = mockSyncData[key]
          }
          callback(result)
        }
      }),
      set: vi.fn((obj, callback) => {
        Object.assign(mockSyncData, obj)
        callback?.()
      }),
      remove: vi.fn((key, callback) => {
        delete mockSyncData[key]
        callback?.()
      }),
      clear: vi.fn((callback) => {
        Object.keys(mockSyncData).forEach((k) => delete mockSyncData[k])
        callback?.()
      }),
      getBytesInUse: vi.fn(() => Promise.resolve(100)),
      onChanged: {
        addListener: vi.fn(),
        removeListener: vi.fn(),
      },
    },
    local: {
      // 同时支持 Promise 风格（chrome.storage.local.get(keys) → Promise）
      // 和回调风格（chrome.storage.local.get(keys, callback)）。
      get: vi.fn(
        (
          keys?: unknown,
          callback?: (items: Record<string, unknown>) => void,
        ) => {
          const result = pickByKeys(
            mockLocalData,
            keys as
              | string
              | string[]
              | Record<string, unknown>
              | null
              | undefined,
          )
          if (typeof callback === 'function') {
            callback(result)
            return undefined
          }
          return Promise.resolve(result)
        },
      ),
      // set：写入内存，按变更触发 onChanged（areaName='local'），支持 Promise / 回调两种风格。
      set: vi.fn((obj: Record<string, unknown>, callback?: () => void) => {
        const changes: Record<string, chrome.storage.StorageChange> = {}
        for (const [key, newValue] of Object.entries(obj)) {
          const oldValue = mockLocalData[key]
          mockLocalData[key] = newValue
          changes[key] = { oldValue, newValue }
        }
        emitStorageChange(changes, 'local')
        if (typeof callback === 'function') {
          callback()
          return undefined
        }
        return Promise.resolve()
      }),
      // remove：删除内存键，按变更触发 onChanged，支持单键或多键、Promise / 回调风格。
      remove: vi.fn((keys: string | string[], callback?: () => void) => {
        const keyList = Array.isArray(keys) ? keys : [keys]
        const changes: Record<string, chrome.storage.StorageChange> = {}
        for (const key of keyList) {
          if (key in mockLocalData) {
            changes[key] = { oldValue: mockLocalData[key], newValue: undefined }
            delete mockLocalData[key]
          }
        }
        if (Object.keys(changes).length > 0) {
          emitStorageChange(changes, 'local')
        }
        if (typeof callback === 'function') {
          callback()
          return undefined
        }
        return Promise.resolve()
      }),
      clear: vi.fn((callback?: () => void) => {
        const changes: Record<string, chrome.storage.StorageChange> = {}
        for (const key of Object.keys(mockLocalData)) {
          changes[key] = { oldValue: mockLocalData[key], newValue: undefined }
          delete mockLocalData[key]
        }
        if (Object.keys(changes).length > 0) {
          emitStorageChange(changes, 'local')
        }
        if (typeof callback === 'function') {
          callback()
          return undefined
        }
        return Promise.resolve()
      }),
    },
    // chrome.storage.session —— MV3 内存级会话存储。采集子系统用它持久化
    // in-flight 任务与最近快照，让 Service Worker 被回收后仍能恢复
    // （src/background/capture-store.ts）。这里用独立内存对象模拟，
    // get/set/remove 均为 Promise 风格（MV3 标准）。
    session: {
      get: vi.fn((keys?: unknown) => {
        return Promise.resolve(
          pickByKeys(
            mockSessionData,
            keys as
              | string
              | string[]
              | Record<string, unknown>
              | null
              | undefined,
          ),
        )
      }),
      set: vi.fn((obj: Record<string, unknown>) => {
        Object.assign(mockSessionData, obj)
        return Promise.resolve()
      }),
      remove: vi.fn((keys: string | string[]) => {
        const keyList = Array.isArray(keys) ? keys : [keys]
        for (const key of keyList) delete mockSessionData[key]
        return Promise.resolve()
      }),
      clear: vi.fn(() => {
        Object.keys(mockSessionData).forEach((k) => delete mockSessionData[k])
        return Promise.resolve()
      }),
    },
    // chrome.storage.onChanged：addListener/removeListener 维护真实的活跃监听器集合，
    // local.set/remove/clear 变更数据后会触发集合内所有监听器（areaName='local'）。
    onChanged: {
      addListener: vi.fn((listener: StorageChangeListener) => {
        storageChangeListeners.add(listener)
      }),
      removeListener: vi.fn((listener: StorageChangeListener) => {
        storageChangeListeners.delete(listener)
      }),
    },
  },
  runtime: {
    lastError: null as chrome.runtime.LastError | null,
    id: 'test-extension-id',
    getURL: vi.fn((path: string) => `chrome-extension://test/${path}`),
    sendMessage: vi.fn(),
    // chrome.runtime.openOptionsPage —— LibraryDesktop 的「去设置」按钮调用，
    // 打开扩展设置页。测试只需断言被调用，故为空 mock。
    openOptionsPage: vi.fn(),
    onMessage: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
    // chrome.runtime.onInstalled —— setupContextMenu 在此注册右键菜单创建逻辑。
    onInstalled: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
    // chrome.runtime.onStartup —— setupContextMenu 在此注册浏览器启动时的
    // 菜单创建安全网（onInstalled 不在重启时触发）。
    onStartup: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
  tabs: {
    create: vi.fn(),
    sendMessage: vi.fn(),
    query: vi.fn((_queryInfo, callback) => {
      callback([])
    }),
    // chrome.tabs.get —— capture-messaging 的 injectCapture 回查标签 URL
    // 以判定受限页面。默认返回一个普通 https 页面。
    get: vi.fn(() => Promise.resolve({ id: 1, url: 'https://example.com' })),
    update: vi.fn(),
    remove: vi.fn(),
    onUpdated: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
    onRemoved: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
  action: {
    setBadgeText: vi.fn(() => Promise.resolve()),
    setBadgeBackgroundColor: vi.fn(() => Promise.resolve()),
  },
  // chrome.scripting —— capture-messaging 的 injectCapture 注入采集脚本。
  // 默认返回空结果数组，单测按需 mock 覆盖。
  scripting: {
    executeScript: vi.fn(() => Promise.resolve([])),
  },
  // chrome.alarms —— 采集恢复看门狗（capture-messaging）。
  alarms: {
    create: vi.fn(),
    clear: vi.fn(() => Promise.resolve(true)),
    onAlarm: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
  i18n: {
    getUILanguage: vi.fn(() => 'zh-CN'),
  },
  permissions: {
    request: vi.fn((_perms, callback) => {
      callback(true)
    }),
    contains: vi.fn((_perms, callback) => {
      callback(true)
    }),
  },
  notifications: {
    create: vi.fn(),
    clear: vi.fn(),
  },
  menus: {
    create: vi.fn(),
    update: vi.fn(),
    remove: vi.fn(),
    onClicked: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
  // chrome.contextMenus —— 右键菜单 API。源码 src/background/contextMenu.ts 用的是
  // contextMenus（Chrome 标准命名），而非 Firefox 系的 menus 别名，故单独提供。
  contextMenus: {
    removeAll: vi.fn((callback?: () => void) => {
      callback?.()
    }),
    create: vi.fn(),
    update: vi.fn(),
    remove: vi.fn(),
    onClicked: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
  commands: {
    onCommand: {
      addListener: vi.fn(),
      removeListener: vi.fn(),
    },
  },
}

globalThis.chrome = chromeMock as unknown as typeof chrome

// ── Window Globals Mock ──

globalThis.window.appVersion = '2.2.5'

globalThis.window.$t = vi.fn((key: string) => key)

globalThis.window.$message = {
  info: vi.fn(),
  warning: vi.fn(),
  error: vi.fn(),
  success: vi.fn(),
} as any

globalThis.window.$notification = {
  info: vi.fn(),
  warning: vi.fn(),
  error: vi.fn(),
  success: vi.fn(),
} as any

globalThis.window.$dialog = {} as any
globalThis.window.$loadingBar = {} as any

// ── Browser API Polyfills ──

// jsdom 的 Blob 没有可用的 .stream()，整体替换为 Node 原生 Blob
// 注意：这会影响 jsdom 中所有 Blob 相关操作，但测试环境不受影响
import { Blob as NodeBlob } from 'node:buffer'
globalThis.Blob = NodeBlob as any

// CompressionStream / DecompressionStream 在 jsdom 中不可用，从 Node 导入
if (typeof globalThis.CompressionStream === 'undefined') {
  globalThis.CompressionStream = CompressionStream as any
}
if (typeof globalThis.DecompressionStream === 'undefined') {
  globalThis.DecompressionStream = DecompressionStream as any
}

// ── Per-test Cleanup ──

beforeEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
  // 清空 chrome.storage.sync / local / session mock 数据
  Object.keys(mockSyncData).forEach((k) => delete mockSyncData[k])
  Object.keys(mockLocalData).forEach((k) => delete mockLocalData[k])
  Object.keys(mockSessionData).forEach((k) => delete mockSessionData[k])
  // 清空 onChanged 活跃监听器，避免跨测试泄漏
  storageChangeListeners.clear()
  chromeMock.runtime.lastError = null
})
