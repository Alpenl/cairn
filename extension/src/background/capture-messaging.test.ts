/**
 * capture-messaging.test.ts — 采集生产侧 IO 接线单元测试。
 *
 * 覆盖本模块对外暴露的两个函数：
 *   - createCaptureDeps：组装的 injectCapture 适配器对受限页面 / 真实失败的
 *     区分，store 适配器接通 chrome.storage.session
 *   - setupCaptureMessaging：注册 chrome.alarms 看门狗、alarm 触发时调
 *     controller.resumeIfNeeded
 *
 * 隔离策略：vi.mock 掉 webext-bridge/background（避免触碰真实消息总线）与
 * 配置读取依赖；chrome.* 由 test/setup.ts 的全局 mock 提供。
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CaptureController } from './captureHandler'
import type { RawCapture } from '@/contentScripts/capture'
import { MSG_START_CAPTURE, type StartCapturePayload } from './capture-protocol'
import type { WebTagSettings } from '@/composables/useSettings'
import {
  createCaptureOwnerFingerprint,
  createCaptureRecoveryKey,
} from '@/api/capabilities'

// ── chrome mock 类型辅助 ────────────────────────────────────

/**
 * chrome.tabs.get / chrome.tabs.query / chrome.scripting.executeScript 在
 * @types/chrome 里是带回调与 Promise 重载的函数。test/setup.ts 把它们 mock 成
 * 纯 Promise 形态，但 vi.mocked() 解析重载时会落到 callback 重载（返回 void），
 * 导致 mockResolvedValueOnce 类型不匹配。下面用一个最小可调用形状收口：
 * 把目标 mock 取出为「接受任意入参、返回 Promise」的函数，再 mockResolvedValueOnce。
 */
type AsyncMock<T> = ReturnType<typeof vi.fn<(...args: unknown[]) => Promise<T>>>

/** 把 chrome.tabs.get 视作返回 Promise<Tab> 的 mock。 */
const tabsGet = chrome.tabs.get as unknown as AsyncMock<chrome.tabs.Tab>
/** 把 chrome.scripting.executeScript 视作返回 Promise<InjectionResult[]> 的 mock。 */
const execScript = chrome.scripting.executeScript as unknown as AsyncMock<
  chrome.scripting.InjectionResult[]
>

// ── 依赖 mock ───────────────────────────────────────────────

// webext-bridge/background：onMessage / sendMessage 用 stub，不触碰真实总线。
const onMessage = vi.fn<(id: string, handler: unknown) => void>()
const sendMessage = vi.fn<
  (id: string, data: unknown, target: string) => Promise<unknown>
>(() => Promise.resolve(undefined))
vi.mock('webext-bridge/background', () => ({
  onMessage: (id: string, handler: unknown) => onMessage(id, handler),
  sendMessage: (id: string, data: unknown, target: string) =>
    sendMessage(id, data, target),
}))

const runtimeMocks = vi.hoisted(() => ({
  buildWebTagClientFromSettings: vi.fn(),
  getWebTagSettings: vi.fn(),
}))

vi.mock('@/api/webtag-client', () => ({
  buildWebTagClientFromSettings: runtimeMocks.buildWebTagClientFromSettings,
}))
vi.mock('@/composables/useSettings', () => ({
  getWebTagSettings: runtimeMocks.getWebTagSettings,
}))

const { createCaptureDeps, setupCaptureMessaging, CAPTURE_WATCHDOG_ALARM } =
  await import('./capture-messaging')

const DEFAULT_SETTINGS: WebTagSettings = {
  backendUrl: '',
  readerUrl: '',
  accessToken: '',
  connectionOwnerFingerprint: '',
  connectionRevision: 0,
  linkOpenBehavior: 'current',
}

// ── 测试夹具 ────────────────────────────────────────────────

beforeEach(() => {
  onMessage.mockClear()
  sendMessage.mockClear()
  runtimeMocks.buildWebTagClientFromSettings.mockReset()
  runtimeMocks.buildWebTagClientFromSettings.mockReturnValue(null)
  runtimeMocks.getWebTagSettings.mockReset()
  runtimeMocks.getWebTagSettings.mockResolvedValue({
    ...DEFAULT_SETTINGS,
  })
})

afterEach(() => {
  vi.clearAllMocks()
})

// ── injectCapture 适配器 ────────────────────────────────────

/** 构造一条合法 RawCapture 形状的 executeScript 注入结果。 */
function injectionResult(raw: RawCapture): chrome.scripting.InjectionResult[] {
  return [{ result: raw } as chrome.scripting.InjectionResult]
}

/** 让 chrome.tabs.query 下一次返回指定标签集。 */
function stubActiveTab(tabs: Partial<chrome.tabs.Tab>[]): void {
  vi.mocked(chrome.tabs.query).mockImplementationOnce(((
    _q: unknown,
    cb?: (t: chrome.tabs.Tab[]) => void,
  ) => {
    const result = tabs as chrome.tabs.Tab[]
    if (cb) cb(result)
    return Promise.resolve(result)
  }) as typeof chrome.tabs.query)
}

describe('createCaptureDeps — injectCapture 受限页面判定', () => {
  it('传入 tabId 指向受限页面（chrome://）时返回 restricted', async () => {
    // chrome.tabs.get 返回受限 URL。
    tabsGet.mockResolvedValueOnce({
      id: 9,
      url: 'chrome://settings',
    } as chrome.tabs.Tab)

    const deps = createCaptureDeps()
    const result = await deps.injectCapture(9)

    expect(result.ok).toBe(false)
    if (result.ok) throw new Error('期望注入失败')
    expect(result.reason).toBe('restricted')
    // 受限页面提前判定，不应再调 executeScript。
    expect(execScript).not.toHaveBeenCalled()
  })

  it('活动标签为应用商店页面时返回 restricted', async () => {
    stubActiveTab([
      { id: 5, url: 'https://chrome.google.com/webstore/detail/x' },
    ])

    const deps = createCaptureDeps()
    const result = await deps.injectCapture()

    expect(result.ok).toBe(false)
    if (result.ok) throw new Error('期望注入失败')
    expect(result.reason).toBe('restricted')
  })

  it('无活动标签时返回 failed（非 restricted）', async () => {
    stubActiveTab([])

    const deps = createCaptureDeps()
    const result = await deps.injectCapture()

    expect(result.ok).toBe(false)
    if (result.ok) throw new Error('期望注入失败')
    expect(result.reason).toBe('failed')
  })

  it('注入成功但结果为空时返回 failed（非 restricted）', async () => {
    tabsGet.mockResolvedValueOnce({
      id: 7,
      url: 'https://example.com/article',
    } as chrome.tabs.Tab)
    // executeScript 返回空结果数组。
    execScript.mockResolvedValueOnce([])

    const deps = createCaptureDeps()
    const result = await deps.injectCapture(7)

    expect(result.ok).toBe(false)
    if (result.ok) throw new Error('期望注入失败')
    expect(result.reason).toBe('failed')
  })

  it('普通页面注入返回合法 RawCapture 时 ok=true', async () => {
    tabsGet.mockResolvedValueOnce({
      id: 7,
      url: 'https://example.com/article',
    } as chrome.tabs.Tab)
    execScript.mockResolvedValueOnce(
      injectionResult({
        url: 'https://example.com/article',
        title: '文章',
        text: '正文',
        html: '<html></html>',
        imageUrls: [],
        metadata: {},
      }),
    )

    const deps = createCaptureDeps()
    const result = await deps.injectCapture(7)

    expect(result.ok).toBe(true)
    if (!result.ok) throw new Error('期望注入成功')
    expect(result.data.url).toBe('https://example.com/article')
  })

  it('executeScript 抛错时归类为 restricted', async () => {
    tabsGet.mockResolvedValueOnce({
      id: 7,
      url: 'https://example.com/article',
    } as chrome.tabs.Tab)
    execScript.mockRejectedValueOnce(
      new Error('Cannot access contents of the page'),
    )

    const deps = createCaptureDeps()
    const result = await deps.injectCapture(7)

    expect(result.ok).toBe(false)
    if (result.ok) throw new Error('期望注入失败')
    expect(result.reason).toBe('restricted')
  })
})

// ── store 适配器 ────────────────────────────────────────────

describe('createCaptureDeps — store 适配器', () => {
  it('store 接通 chrome.storage.session：写入后能读回', async () => {
    const deps = createCaptureDeps()
    const owner = { fingerprint: 'a'.repeat(64), revision: 1 }
    const recoveryKey = await createCaptureRecoveryKey(
      'test-token-for-recovery',
      owner,
    )
    const snapshot = {
      stage: 'done' as const,
      url: 'https://private.example.test/alice',
      title: 'private title',
      owner,
      updatedAt: 123,
    }
    await deps.store.setSnapshot(snapshot, recoveryKey)

    const raw = await chrome.storage.session.get('webtag:capture-snapshot')
    expect(JSON.stringify(raw)).not.toContain(snapshot.url)
    expect(JSON.stringify(raw)).not.toContain(snapshot.title)

    const restartedDeps = createCaptureDeps()
    expect(await restartedDeps.store.getSnapshot(recoveryKey)).toEqual(snapshot)
  })
})

describe('createCaptureDeps — confirmed activation', () => {
  const SETTINGS_A: WebTagSettings = {
    ...DEFAULT_SETTINGS,
    backendUrl: 'https://api-a.example.test/private/base',
    accessToken: 'token-a',
    connectionRevision: 7,
  }

  it('creates a non-secret owner and revokes the lease when confirmed settings change', async () => {
    const identity = {
      client_data_namespace: 'raw-namespace-a',
      representation_contract: 'v3' as const,
    }
    const fingerprint = await createCaptureOwnerFingerprint(
      SETTINGS_A.backendUrl,
      identity,
    )
    const confirmedA = {
      ...SETTINGS_A,
      connectionOwnerFingerprint: fingerprint,
    }
    const client = {
      getSessionIdentity: vi.fn(async () => ({
        ok: true as const,
        data: identity,
      })),
    }
    runtimeMocks.buildWebTagClientFromSettings.mockReturnValue(client)
    runtimeMocks.getWebTagSettings
      .mockResolvedValueOnce(confirmedA)
      .mockResolvedValueOnce(confirmedA)
      .mockResolvedValue({
        ...confirmedA,
        connectionOwnerFingerprint: '',
      })

    const activation = await createCaptureDeps().activateConnection()

    expect(activation).not.toBeNull()
    if (!activation || !('owner' in activation)) {
      throw new Error('expected a verified activation')
    }
    expect(activation.owner).toEqual({
      fingerprint: expect.stringMatching(/^[a-f0-9]{64}$/),
      revision: 7,
    })
    const serializedOwner = JSON.stringify(activation.owner)
    expect(serializedOwner).not.toContain('token-a')
    expect(serializedOwner).not.toContain('raw-namespace-a')
    expect(serializedOwner).not.toContain('/private/base')
    await expect(activation.lease.isCurrent()).resolves.toBe(true)
    await expect(activation.lease.isCurrent()).resolves.toBe(false)
  })

  it('keeps blank-token capture working without deriving a publicly reproducible recovery key', async () => {
    const identity = {
      client_data_namespace: 'public-blank-token-namespace',
      representation_contract: 'v3' as const,
    }
    const fingerprint = await createCaptureOwnerFingerprint(
      SETTINGS_A.backendUrl,
      identity,
    )
    const confirmed = {
      ...SETTINGS_A,
      accessToken: '',
      connectionOwnerFingerprint: fingerprint,
    }
    runtimeMocks.getWebTagSettings.mockResolvedValue(confirmed)
    runtimeMocks.buildWebTagClientFromSettings.mockReturnValue({
      getSessionIdentity: vi.fn(async () => ({
        ok: true as const,
        data: identity,
      })),
    })
    const deps = createCaptureDeps()

    const activation = await deps.activateConnection()

    expect(activation).not.toBeNull()
    if (!activation || !('owner' in activation))
      throw new Error('expected a verified blank-token activation')
    expect(activation.recoveryKey).toBeUndefined()
    await deps.store.setSnapshot(
      {
        stage: 'done',
        url: 'https://private.example.test/blank-token',
        title: 'private blank-token title',
        owner: activation.owner,
        updatedAt: 123,
      },
      activation.recoveryKey,
    )
    expect(
      (await chrome.storage.session.get('webtag:capture-snapshot'))[
        'webtag:capture-snapshot'
      ],
    ).toBeUndefined()
  })

  it('classifies a transient session identity failure without claiming definitive mismatch', async () => {
    runtimeMocks.getWebTagSettings.mockResolvedValue(SETTINGS_A)
    runtimeMocks.buildWebTagClientFromSettings.mockReturnValue({
      getSessionIdentity: vi.fn(async () => ({
        ok: false as const,
        error: {
          kind: 'network-unreachable' as const,
          message: 'temporary network failure',
        },
      })),
    })

    await expect(createCaptureDeps().activateConnection()).resolves.toEqual({
      status: 'transient-unavailable',
    })
  })

  it('fails closed when identity probing fails or the confirmed owner hash disagrees', async () => {
    runtimeMocks.getWebTagSettings.mockResolvedValue(SETTINGS_A)
    runtimeMocks.buildWebTagClientFromSettings.mockReturnValue({
      getSessionIdentity: vi.fn(async () => ({
        ok: false as const,
        error: { kind: 'identity-mismatch' as const, message: 'mismatch' },
      })),
    })
    await expect(createCaptureDeps().activateConnection()).resolves.toBeNull()

    runtimeMocks.getWebTagSettings.mockResolvedValue({
      ...SETTINGS_A,
      connectionOwnerFingerprint: 'f'.repeat(64),
    })
    runtimeMocks.buildWebTagClientFromSettings.mockReturnValue({
      getSessionIdentity: vi.fn(async () => ({
        ok: true as const,
        data: {
          client_data_namespace: 'raw-namespace-a',
          representation_contract: 'v3',
        },
      })),
    })
    await expect(createCaptureDeps().activateConnection()).resolves.toBeNull()
  })
})

// ── setupCaptureMessaging 看门狗 ───────────────────────────

describe('setupCaptureMessaging — 看门狗与消息注册', () => {
  /** 构造一个最小 stub controller。 */
  function makeController(): CaptureController & {
    resumeIfNeeded: ReturnType<typeof vi.fn>
  } {
    return {
      startCapture: vi.fn(),
      getLatestSnapshot: vi.fn(),
      resumeIfNeeded: vi.fn(() => Promise.resolve()),
    }
  }

  it('注册 START_CAPTURE / GET_CAPTURE_STATUS 两个消息监听', () => {
    setupCaptureMessaging(makeController())
    expect(onMessage).toHaveBeenCalledTimes(2)
  })

  it('将 popup 的 requestedKind 原样转交给采集控制器', async () => {
    const controller = makeController()
    const expected = {
      stage: 'submitted' as const,
      url: 'https://example.com/tool',
      title: 'Tool',
      updatedAt: 1,
    }
    vi.mocked(controller.startCapture).mockResolvedValue(expected)
    setupCaptureMessaging(controller)

    const registration = onMessage.mock.calls.find(
      ([id]) => id === MSG_START_CAPTURE,
    )
    if (!registration)
      throw new Error('START_CAPTURE handler was not registered')
    const handler = registration[1] as (message: {
      data?: StartCapturePayload
    }) => Promise<typeof expected>

    await expect(
      handler({
        data: { note: 'save for team', tabId: 42, requestedKind: 'site' },
      }),
    ).resolves.toEqual(expected)
    expect(controller.startCapture).toHaveBeenCalledWith(
      'save for team',
      42,
      'site',
    )
  })

  it('创建采集看门狗 alarm（带周期）', () => {
    setupCaptureMessaging(makeController())
    expect(chrome.alarms.create).toHaveBeenCalledWith(
      CAPTURE_WATCHDOG_ALARM,
      expect.objectContaining({ periodInMinutes: expect.any(Number) }),
    )
  })

  it('看门狗 alarm 触发时调 controller.resumeIfNeeded', () => {
    const controller = makeController()
    setupCaptureMessaging(controller)

    // 取出注册到 onAlarm 的监听器并以采集 alarm 触发。
    const listener = vi.mocked(chrome.alarms.onAlarm.addListener).mock
      .calls[0][0] as (alarm: chrome.alarms.Alarm) => void
    listener({ name: CAPTURE_WATCHDOG_ALARM } as chrome.alarms.Alarm)

    expect(controller.resumeIfNeeded).toHaveBeenCalledTimes(1)
  })

  it('其它名字的 alarm 触发时不调 resumeIfNeeded', () => {
    const controller = makeController()
    setupCaptureMessaging(controller)

    const listener = vi.mocked(chrome.alarms.onAlarm.addListener).mock
      .calls[0][0] as (alarm: chrome.alarms.Alarm) => void
    listener({ name: 'some-other-alarm' } as chrome.alarms.Alarm)

    expect(controller.resumeIfNeeded).not.toHaveBeenCalled()
  })
})
