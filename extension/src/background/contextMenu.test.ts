/**
 * contextMenu.test.ts — 右键菜单可测试核心单元测试。
 *
 * 测两个纯函数 + setupContextMenu 的安全网接线：
 *   - resolveMenuTitle：按 UI 语言字符串选中文 / 英文菜单标题
 *   - handleMenuClick：id 守卫 + 触发无备注采集
 *   - setupContextMenu：必须同时在 onInstalled 与 onStartup 注册菜单创建
 *     （onInstalled 不在浏览器重启时触发，缺 onStartup 会让采集入口消失）
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ApiResult, WebTagClient } from '@/api/webtag-client'
import type { CapabilitiesResponse } from '@/api/types'
import { WEBTAG_SETTINGS_STORAGE_KEY } from '@/composables/useSettings'
import {
  handleMenuClick,
  probeSiteCaptureCapability,
  resolveMenuItemTitle,
  resolveMenuTitle,
  setupContextMenu,
} from './contextMenu'
import type { CaptureController } from './captureHandler'

const CAPTURE_MENU_SITE_ID = 'webtag-capture-site'

function capabilities(
  libraryKinds: boolean,
  siteLibrary: boolean,
  siteManagement: boolean,
): CapabilitiesResponse {
  return {
    library_kinds: libraryKinds,
    site_library: siteLibrary,
    site_management: siteManagement,
  } as unknown as CapabilitiesResponse
}

function capabilityClient(
  result: Promise<ApiResult<CapabilitiesResponse | null>>,
): Pick<WebTagClient, 'getCapabilities'> {
  return { getCapabilities: vi.fn(() => result) }
}

function createdMenuIDs(): unknown[] {
  return vi
    .mocked(chrome.contextMenus.create)
    .mock.calls.map(([properties]) => properties.id)
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(chrome.contextMenus.removeAll).mockImplementation((callback) => {
    callback?.()
  })
  vi.mocked(chrome.contextMenus.create).mockImplementation(
    (properties) => properties.id ?? 'mock-menu-id',
  )
})

describe('resolveMenuTitle', () => {
  it('zh-CN 返回中文标题', () => {
    expect(resolveMenuTitle('zh-CN')).toBe('采集此页到 WebTag')
  })

  it('zh-TW 等其它中文变体同样返回中文标题', () => {
    expect(resolveMenuTitle('zh-TW')).toBe('采集此页到 WebTag')
    expect(resolveMenuTitle('zh-HK')).toBe('采集此页到 WebTag')
  })

  it('en-US 返回英文标题', () => {
    expect(resolveMenuTitle('en-US')).toBe('Capture this page to WebTag')
  })

  it('其它非中文语言回退英文标题', () => {
    expect(resolveMenuTitle('ja')).toBe('Capture this page to WebTag')
    expect(resolveMenuTitle('fr-FR')).toBe('Capture this page to WebTag')
    expect(resolveMenuTitle('')).toBe('Capture this page to WebTag')
  })
})

describe('handleMenuClick', () => {
  /** 右键菜单项的唯一 id（与 contextMenu.ts 内 CAPTURE_MENU_ID 保持一致）。 */
  const CAPTURE_MENU_ID = 'webtag-capture-page'

  it('菜单 id 不匹配时不触发采集', () => {
    const startCapture = vi.fn()
    handleMenuClick('some-other-menu-id', 42, startCapture)
    expect(startCapture).not.toHaveBeenCalled()
  })

  it('数字类型的非匹配 id 同样被守卫拦截', () => {
    const startCapture = vi.fn()
    handleMenuClick(999, 42, startCapture)
    expect(startCapture).not.toHaveBeenCalled()
  })

  it('菜单 id 匹配时以空备注 + tabId 触发采集', () => {
    const startCapture = vi.fn()
    handleMenuClick(CAPTURE_MENU_ID, 7, startCapture)
    expect(startCapture).toHaveBeenCalledTimes(1)
    expect(startCapture).toHaveBeenCalledWith('', 7)
  })

  it('tabId 为 undefined 时仍触发采集，tabId 透传 undefined', () => {
    const startCapture = vi.fn()
    handleMenuClick(CAPTURE_MENU_ID, undefined, startCapture)
    expect(startCapture).toHaveBeenCalledTimes(1)
    expect(startCapture).toHaveBeenCalledWith('', undefined)
  })
})

describe('resolveMenuItemTitle', () => {
  it('按中文 UI 语言生成三个显式子项文案', () => {
    expect(resolveMenuItemTitle('zh-CN', 'auto')).toBe('自动分类')
    expect(resolveMenuItemTitle('zh-CN', 'reading')).toBe('保存为阅读')
    expect(resolveMenuItemTitle('zh-CN', 'site')).toBe('收藏为网站')
  })

  it('非中文 UI 语言使用英文子项文案', () => {
    expect(resolveMenuItemTitle('en-US', 'auto')).toBe('Auto classify')
    expect(resolveMenuItemTitle('en-US', 'reading')).toBe('Save as reading')
    expect(resolveMenuItemTitle('en-US', 'site')).toBe('Save as website')
  })
})

describe('probeSiteCaptureCapability', () => {
  it.each([
    [false, false, false, false],
    [false, false, true, false],
    [false, true, false, false],
    [false, true, true, false],
    [true, false, false, false],
    [true, false, true, false],
    [true, true, false, false],
    [true, true, true, true],
  ] as const)(
    'requires library_kinds=%s site_library=%s site_management=%s to yield %s',
    async (libraryKinds, siteLibrary, siteManagement, expected) => {
      const result: ApiResult<CapabilitiesResponse | null> = {
        ok: true,
        data: capabilities(libraryKinds, siteLibrary, siteManagement),
      }
      await expect(
        probeSiteCaptureCapability(async () =>
          capabilityClient(Promise.resolve(result)),
        ),
      ).resolves.toBe(expected)
    },
  )

  it('fails closed for missing configuration, API errors, and thrown probes', async () => {
    await expect(probeSiteCaptureCapability(async () => null)).resolves.toBe(
      false,
    )
    await expect(
      probeSiteCaptureCapability(async () =>
        capabilityClient(
          Promise.resolve({
            ok: false,
            error: { kind: 'unauthorized', message: 'denied' },
          }),
        ),
      ),
    ).resolves.toBe(false)
    await expect(
      probeSiteCaptureCapability(async () =>
        capabilityClient(Promise.reject(new Error('offline'))),
      ),
    ).resolves.toBe(false)
  })
})

describe('setupContextMenu — 安全网接线', () => {
  /** 构造一个最小 stub controller。 */
  function makeController(): CaptureController {
    return {
      startCapture: vi.fn(),
      getLatestSnapshot: vi.fn(),
      resumeIfNeeded: vi.fn(),
    }
  }

  it('在 onInstalled 注册菜单创建逻辑', () => {
    setupContextMenu(makeController())
    expect(chrome.runtime.onInstalled.addListener).toHaveBeenCalled()
  })

  it('在 onStartup 注册菜单创建逻辑（浏览器重启安全网）', () => {
    setupContextMenu(makeController())
    // 关键断言：onStartup 必须也被注册——onInstalled 不在重启时触发，
    // 缺这个监听会让右键采集入口在浏览器重启后悄无声息地消失。
    expect(chrome.runtime.onStartup.addListener).toHaveBeenCalled()
  })

  it('onStartup 触发时调用 chrome.contextMenus.create 创建菜单', () => {
    setupContextMenu(makeController())
    // 取出注册到 onStartup 的回调并触发，验证它确实创建菜单。
    const startupCb = vi.mocked(chrome.runtime.onStartup.addListener).mock
      .calls[0][0] as () => void
    startupCb()
    expect(chrome.contextMenus.removeAll).toHaveBeenCalled()
    expect(chrome.contextMenus.create).toHaveBeenCalled()
  })

  it('能力探测尚未返回时不创建 site 项，严格授权后才创建', async () => {
    const pending = deferred<ApiResult<CapabilitiesResponse | null>>()
    setupContextMenu(makeController(), async () =>
      capabilityClient(pending.promise),
    )
    const startupCb = vi.mocked(chrome.runtime.onStartup.addListener).mock
      .calls[0][0] as () => void

    startupCb()
    expect(createdMenuIDs()).not.toContain(CAPTURE_MENU_SITE_ID)

    pending.resolve({ ok: true, data: capabilities(true, true, true) })
    await vi.waitFor(() => {
      expect(createdMenuIDs()).toContain(CAPTURE_MENU_SITE_ID)
    })
  })

  it('连接设置保存后立即重建菜单并重新探测 site 能力', async () => {
    const buildClient = vi.fn(async () =>
      capabilityClient(
        Promise.resolve({
          ok: true,
          data: capabilities(true, true, true),
        }),
      ),
    )
    setupContextMenu(makeController(), buildClient)
    const storageListener = vi.mocked(chrome.storage.onChanged.addListener).mock
      .calls[0][0]

    storageListener(
      { [WEBTAG_SETTINGS_STORAGE_KEY]: { newValue: {} } },
      'local',
    )

    await vi.waitFor(() => {
      expect(buildClient).toHaveBeenCalledTimes(1)
      expect(createdMenuIDs()).toContain(CAPTURE_MENU_SITE_ID)
    })
  })

  it('旧菜单 generation 的迟到授权响应不能重新创建 site 项', async () => {
    const oldProbe = deferred<ApiResult<CapabilitiesResponse | null>>()
    const buildClient = vi
      .fn()
      .mockResolvedValueOnce(capabilityClient(oldProbe.promise))
      .mockResolvedValueOnce(
        capabilityClient(
          Promise.resolve({
            ok: true,
            data: capabilities(true, true, false),
          }),
        ),
      )
    setupContextMenu(makeController(), buildClient)
    const startupCb = vi.mocked(chrome.runtime.onStartup.addListener).mock
      .calls[0][0] as () => void
    const installedCb = vi.mocked(chrome.runtime.onInstalled.addListener).mock
      .calls[0][0] as () => void

    startupCb()
    installedCb()
    await vi.waitFor(() => expect(buildClient).toHaveBeenCalledTimes(2))
    vi.mocked(chrome.contextMenus.create).mockClear()

    oldProbe.resolve({ ok: true, data: capabilities(true, true, true) })
    await Promise.resolve()
    await Promise.resolve()

    expect(createdMenuIDs()).not.toContain(CAPTURE_MENU_SITE_ID)
  })

  it('串行化重叠重建，旧 removeAll 完成后仍由最新 generation 恢复基础菜单', () => {
    const pendingRemovals: Array<() => void> = []
    const liveMenuIDs = new Set<unknown>()
    vi.mocked(chrome.contextMenus.removeAll).mockImplementation((callback) => {
      pendingRemovals.push(() => {
        liveMenuIDs.clear()
        callback?.()
      })
    })
    vi.mocked(chrome.contextMenus.create).mockImplementation((properties) => {
      liveMenuIDs.add(properties.id)
      return properties.id ?? 'mock-menu-id'
    })

    setupContextMenu(makeController())
    const startupCb = vi.mocked(chrome.runtime.onStartup.addListener).mock
      .calls[0][0] as () => void
    const installedCb = vi.mocked(chrome.runtime.onInstalled.addListener).mock
      .calls[0][0] as () => void

    startupCb()
    installedCb()

    // The newer rebuild stays pending until the older removeAll completes, so
    // two destructive operations can never settle out of order.
    expect(chrome.contextMenus.removeAll).toHaveBeenCalledTimes(1)
    expect(pendingRemovals).toHaveLength(1)

    pendingRemovals[0]?.()

    expect(chrome.contextMenus.removeAll).toHaveBeenCalledTimes(2)
    expect(pendingRemovals).toHaveLength(2)
    expect(liveMenuIDs).toEqual(new Set())

    pendingRemovals[1]?.()

    expect(liveMenuIDs).toEqual(
      new Set([
        'webtag-capture-page',
        'webtag-capture-auto',
        'webtag-capture-reading',
      ]),
    )
  })
})
