/**
 * captureHandler.test.ts — 当前页采集编排单元测试。
 *
 * 测试隔离策略：
 *   - 不 vi.mock 任何模块：createCaptureController 接受纯 stub CaptureDeps，
 *     测试自己提供 activateConnection / injectCapture / sendSnapshot / store。
 *   - 每个用例新建独立 controller + 独立内存 store，天然隔离。
 *   - 不给 chrome.* 打补丁：脚本注入经 injectCapture stub，持久化经内存 store。
 *
 * 覆盖 startCapture 的关键分支：
 *   - 抓取脚本注入失败（restricted / failed）→ failed(对应 errorKind)
 *   - 后端未配置 → failed(not-configured)
 *   - ingest 失败（401）→ failed(unauthorized)
 *   - ingest 命中已 done 链接 → done
 *   - ingest 成功 + 轮询到 done / failed
 *   - 并发守卫：进行中再次 startCapture 返回当前快照、不重复触发
 *   - try/catch 防死锁：toIngestSource 路径意外抛出被兜底为 failed，
 *     且不留下永久死锁
 * 以及 MV3 持久化加固：
 *   - publishSnapshot 落盘、终态清除 in-flight
 *   - getLatestSnapshot 从 store 读（重唤醒 SW 返回真实状态）
 *   - resumeIfNeeded 看门狗续跑：有 in-flight 且本实例无轮询 → 续跑
 *   - resumeIfNeeded 预算耗尽临界点：attempt 已 >= MAX_POLL_ATTEMPTS 时直接走
 *     still-processing 终态收尾并清 in-flight（不进空循环干等陈旧兜底）
 */
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ApiResult, WebTagClient } from '@/api/webtag-client'
import type {
  CapabilitiesResponse,
  IngestRequest,
  IngestSource,
  Link,
  SubmitResponse,
} from '@/api/types'
import type { RawCapture } from '@/contentScripts/capture'
import {
  CaptureActivationLease,
  createCaptureRecoveryKey,
  type CaptureOwner,
} from '@/api/capabilities'
import { MAX_POLL_ATTEMPTS, POLL_INTERVAL_MS } from './capture-poll'
import {
  MAX_CAPTURE_AGE_MS,
  createCaptureController,
  type CaptureDeps,
  type CaptureInjectResult,
} from './captureHandler'
import type { CaptureSnapshot } from './capture-protocol'
import { IDLE_CAPTURE_SNAPSHOT } from './capture-protocol'
import {
  createSessionCaptureStore,
  STORAGE_KEY_INFLIGHT,
  STORAGE_KEY_SNAPSHOT,
  type CaptureStore,
  type InFlightCapture,
  type SessionStorageArea,
} from './capture-store'

// ── 测试夹具 ────────────────────────────────────────────────

/** 一个最小可用的 RawCapture 形状（capturePageContent 在目标页执行后的返回）。 */
const FAKE_RAW_CAPTURE: RawCapture = {
  url: 'https://example.com/article',
  title: '示例文章',
  text: '正文内容',
  html: '<html><body>正文内容</body></html>',
  imageUrls: [],
  metadata: { capture_source: 'browser_extension' },
}

const OWNER_A: CaptureOwner = {
  fingerprint: 'a'.repeat(64),
  revision: 1,
}
const OWNER_B: CaptureOwner = {
  fingerprint: 'b'.repeat(64),
  revision: 2,
}
const TEST_RECOVERY_KEY = await createCaptureRecoveryKey(
  'capture-handler-test-token',
  OWNER_A,
)

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data })
const fail = (kind: string, message = 'err'): ApiResult<never> => ({
  ok: false,
  error: { kind: kind as never, message },
})

function linkWithStatus(
  status: Link['status'],
  overrides: Partial<Link> = {},
): Link {
  return {
    id: 'link-1',
    url: 'https://example.com/article',
    title: '示例文章',
    summary: null,
    description: null,
    tags: [],
    content_type: 'article',
    status,
    domain: 'example.com',
    path_depth: 1,
    parent_id: null,
    parent_path: '/',
    fetcher_type: null,
    is_low_confidence: false,
    has_content: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    metadata_revision: 1,
    ...overrides,
  }
}

/** 构造一个 done 状态的 Link。 */
function doneLink(): Link {
  return linkWithStatus('done')
}

function doneReadingLink(): Link {
  return linkWithStatus('done', { library_kind: 'reading' })
}

function doneSiteLink(): Link {
  return linkWithStatus('done', { library_kind: 'site' })
}

/** 构造一个 failed 状态的 Link。 */
function failedLink(
  errorMsg: string | null,
  errorCategory: string | null,
): Link {
  return linkWithStatus('failed', {
    error_category: errorCategory,
    error_msg: errorMsg,
  })
}

/** 构造一个 processing 状态的 Link（永远解析中，用于覆盖轮询预算耗尽路径）。 */
function processingLink(): Link {
  return linkWithStatus('processing')
}

/**
 * makeMemoryStore 的可选行为开关 —— 模拟 storage 故障被生产 store 吞掉的效果。
 *
 * 生产 createSessionCaptureStore 在写失败时 console.warn 后吞掉异常、resolve
 * void（fail-safe）：对调用方而言「写没生效」== 「写操作正常返回但持久层无变化」。
 * 这里用 dropSetInFlight / dropClearInFlight 精确复刻这个观察效果：
 *   - dropSetInFlight：setInFlight 不写入、直接 resolve（持久层留空）
 *   - dropClearInFlight：clearInFlight 不删除、直接 resolve（僵尸记录留存）
 */
interface MemoryStoreOptions {
  /** 为 true 时 setInFlight 静默丢弃（模拟 storage 写失败被 fail-safe 吞掉）。 */
  dropSetInFlight?: boolean
  /** 为 true 时 clearInFlight 静默丢弃（模拟 storage 删失败被 fail-safe 吞掉）。 */
  dropClearInFlight?: boolean
}

/**
 * 内存版 CaptureStore —— 测试用持久化存储。
 * 直接操作内存对象，模拟 chrome.storage.session 的行为。
 *
 * dropSetInFlight / dropClearInFlight 用于复刻「storage 写/删失败被生产
 * store fail-safe 吞掉」的观察效果，验证 controller 的两层 fail-safe 兜底。
 */
function makeMemoryStore(opts: MemoryStoreOptions = {}): CaptureStore & {
  _snapshot: () => CaptureSnapshot
  _inFlight: () => InFlightCapture | null
} {
  let snapshot: CaptureSnapshot = { ...IDLE_CAPTURE_SNAPSHOT }
  let inFlight: InFlightCapture | null = null
  return {
    getSnapshotOwner: () => Promise.resolve(snapshot.owner ?? null),
    getSnapshot: () => Promise.resolve(snapshot),
    setSnapshot: (s) => {
      snapshot = s
      return Promise.resolve()
    },
    getInFlight: () => Promise.resolve(inFlight),
    getInFlightMetadata: () =>
      Promise.resolve(
        inFlight
          ? {
              owner: inFlight.owner,
              ...(inFlight.phase ? { phase: inFlight.phase } : {}),
              attempt: inFlight.attempt,
              startedAt: inFlight.startedAt,
            }
          : null,
      ),
    setInFlight: (f) => {
      // 模拟 storage 写失败：生产 store fail-safe 吞掉异常、resolve void，
      // 持久层无变化——这里直接 resolve、不落 inFlight。
      if (opts.dropSetInFlight) return Promise.resolve()
      inFlight = f
      return Promise.resolve()
    },
    clearInFlight: () => {
      // 模拟 storage 删失败：僵尸记录留存——这里直接 resolve、不清 inFlight。
      if (opts.dropClearInFlight) return Promise.resolve()
      inFlight = null
      return Promise.resolve()
    },
    _snapshot: () => snapshot,
    _inFlight: () => inFlight,
  }
}

function makeSessionArea(): SessionStorageArea & {
  _data: Record<string, unknown>
} {
  const data: Record<string, unknown> = {}
  return {
    _data: data,
    get: async (keys) => {
      const result: Record<string, unknown> = {}
      for (const key of Array.isArray(keys) ? keys : [keys]) {
        if (key in data) result[key] = data[key]
      }
      return result
    },
    set: async (items) => {
      Object.assign(data, items)
    },
    remove: async (keys) => {
      for (const key of Array.isArray(keys) ? keys : [keys]) delete data[key]
    },
  }
}

/**
 * 测试用 deps 构造器。各分支只覆盖需要的字段，其余给出可用默认。
 */
interface StubOptions {
  /** 为 true 时 activateConnection 返回 null，模拟「后端未配置」。 */
  notConfigured?: boolean
  inject?: CaptureInjectResult
  ingest?: ApiResult<SubmitResponse>
  getLink?: ApiResult<Link>
  getLinkFactory?: () => Promise<ApiResult<Link>>
  /** 注入一个会抛错的 ingest，验证 try/catch 兜底。 */
  ingestThrows?: boolean
  /** injectCapture 调用计数注入点。 */
  onInject?: () => void
  /** ingest 调用计数注入点。 */
  onIngest?: (body: unknown) => void
  /** ingest options 注入点，用于检查幂等 key 是否被传递。 */
  onIngestOptions?: (options: unknown) => void
  /** 覆盖 capability 探测结果。 */
  capabilities?: ApiResult<CapabilitiesResponse | null>
  /** 模拟 capability 探测抛出网络/运行时异常。 */
  capabilitiesThrows?: boolean
  /** 模拟没有 capability 方法的旧客户端替身。 */
  withoutCapabilities?: boolean
  /** 生产 destination 设置的注入值。 */
  captureDestination?: 'inbox' | 'library'
  /** 自动保存原文调用计数注入点。 */
  onSaveLinkContent?: (linkId: string) => void
  /** 透传给内存 store 的故障开关（模拟 storage 写/删失败被 fail-safe 吞掉）。 */
  storeOptions?: MemoryStoreOptions
  /**
   * 时间源覆盖。默认固定 1_700_000_000_000；陈旧自愈类用例传入可推进的
   * 时间源（如读取外部可变变量），模拟采集预算耗尽后的墙钟推进。
   */
  now?: () => number
  owner?: CaptureOwner | (() => CaptureOwner)
  leaseCurrent?: (owner: CaptureOwner) => boolean | Promise<boolean>
}

function makeDeps(opts: StubOptions = {}): {
  deps: CaptureDeps
  store: ReturnType<typeof makeMemoryStore>
  snapshots: CaptureSnapshot[]
  injectCalls: () => number
  ingestCalls: () => number
  saveLinkContentCalls: () => number
  getLinkCalls: () => number
} {
  const snapshots: CaptureSnapshot[] = []
  const store = makeMemoryStore(opts.storeOptions)
  let injectCount = 0
  let ingestCount = 0
  let saveLinkContentCount = 0
  let getLinkCount = 0

  const client: WebTagClient = {
    getCapabilities: (() => {
      if (opts.capabilitiesThrows) {
        return Promise.reject(new Error('capability probe unavailable'))
      }
      return Promise.resolve(
        opts.capabilities ??
          ok<CapabilitiesResponse>({
            library_kinds: true,
            site_library: true,
            site_management: true,
            site_advanced_management: true,
            archive_versions: [],
            reader_vnext: true,
            reader: {
              annotations: true,
              notes: true,
              inbox: true,
              todos: true,
              engagement: true,
              home: true,
              feed: true,
              ai: true,
              related_tags: true,
              activity: true,
              history: true,
              trash: true,
            },
          }),
      )
    }) as WebTagClient['getCapabilities'],
    ingest: ((body: unknown, options: unknown) => {
      ingestCount += 1
      opts.onIngest?.(body)
      opts.onIngestOptions?.(options)
      if (opts.ingestThrows) {
        throw new Error('意外的 ingest 异常')
      }
      return Promise.resolve(
        opts.ingest ?? ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
      )
    }) as WebTagClient['ingest'],
    getLink: (() => {
      getLinkCount += 1
      return (
        opts.getLinkFactory?.() ??
        Promise.resolve(opts.getLink ?? ok(doneLink()))
      )
    }) as WebTagClient['getLink'],
    saveLinkContent: (linkId: string) => {
      saveLinkContentCount += 1
      opts.onSaveLinkContent?.(linkId)
      return Promise.resolve(ok({}))
    },
  } as WebTagClient
  if (opts.withoutCapabilities) {
    delete (client as unknown as Record<string, unknown>).getCapabilities
  }

  const deps: CaptureDeps = {
    activateConnection: () => {
      if (opts.notConfigured) return Promise.resolve(null)
      const owner =
        typeof opts.owner === 'function'
          ? opts.owner()
          : (opts.owner ?? OWNER_A)
      return Promise.resolve({
        client,
        owner,
        lease: new CaptureActivationLease(
          owner,
          () => opts.leaseCurrent?.(owner) ?? true,
        ),
        recoveryKey: TEST_RECOVERY_KEY,
      })
    },
    injectCapture: () => {
      injectCount += 1
      opts.onInject?.()
      return Promise.resolve(
        opts.inject ?? { ok: true, data: FAKE_RAW_CAPTURE },
      )
    },
    sendSnapshot: (s) => {
      snapshots.push(s)
    },
    store,
    getCaptureDestination: () =>
      Promise.resolve(opts.captureDestination ?? 'inbox'),
    // 固定时间源，让快照 updatedAt 可预测；陈旧自愈类用例可覆盖为可推进的源。
    now: opts.now ?? (() => 1_700_000_000_000),
  }

  return {
    deps,
    store,
    snapshots,
    injectCalls: () => injectCount,
    ingestCalls: () => ingestCount,
    saveLinkContentCalls: () => saveLinkContentCount,
    getLinkCalls: () => getLinkCount,
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('createCaptureController — 抓取阶段', () => {
  it('注入受限页面（restricted）时返回 failed(capture-restricted)', async () => {
    const { deps } = makeDeps({
      inject: { ok: false, reason: 'restricted', message: 'chrome://settings' },
    })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('', undefined, 'site')
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('capture-restricted')
    expect(snapshot.requestedKind).toBe('site')
  })

  it('注入真实失败（failed）时返回 failed(capture-injection-failed)', async () => {
    const { deps } = makeDeps({
      inject: { ok: false, reason: 'failed', message: 'no-active-tab' },
    })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('', undefined, 'reading')
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('capture-injection-failed')
    expect(snapshot.requestedKind).toBe('reading')
  })
})

describe('createCaptureController — 配置校验', () => {
  it('后端未配置（activateConnection 返回 null）时返回 failed(not-configured)', async () => {
    const { deps, injectCalls } = makeDeps({ notConfigured: true })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('', undefined, 'site')
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('not-configured')
    expect(snapshot.url).toBe('')
    expect(snapshot.requestedKind).toBe('site')
    expect(injectCalls()).toBe(0)
  })
})

describe('createCaptureController — 提交阶段', () => {
  it('ingest 返回 401 时返回 failed(unauthorized)', async () => {
    const { deps } = makeDeps({ ingest: fail('unauthorized') })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('', undefined, 'site')
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('unauthorized')
    expect(snapshot.requestedKind).toBe('site')
  })

  it('ingest 命中已 done 链接时直接返回 done', async () => {
    const { deps } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'link-1', status: 'done' }),
    })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('')
    expect(snapshot.stage).toBe('done')
  })

  it('默认把扩展采集写入 Inbox，并接受只有 Inbox 身份的响应', async () => {
    let ingestBody: unknown
    const { deps, getLinkCalls, saveLinkContentCalls } = makeDeps({
      ingest: ok<SubmitResponse>({
        inbox_id: 'inbox-1',
        destination: 'inbox',
        status: 'pending',
      }),
      onIngest: (body) => {
        ingestBody = body
      },
    })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('保留')

    expect(snapshot.stage).toBe('done')
    expect(ingestBody).toMatchObject({ destination: 'inbox' })
    expect(ingestBody).not.toHaveProperty('link_id')
    expect(getLinkCalls()).toBe(0)
    expect(saveLinkContentCalls()).toBe(0)
  })

  it('显式网站采集忽略普通偏好并发送 site destination', async () => {
    let ingestBody: unknown
    const { deps } = makeDeps({
      captureDestination: 'library',
      onIngest: (body) => {
        ingestBody = body
      },
      ingest: ok<SubmitResponse>({ link_id: 'link-1', status: 'done' }),
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('', undefined, 'site')

    expect(snapshot.stage).toBe('done')
    expect(ingestBody).toMatchObject({
      destination: 'site',
    })
    expect(ingestBody).not.toHaveProperty('requested_library_kind')
  })

  it('直接入库的默认选择显式发送 auto 而不是伪造用户去向', async () => {
    let ingestBody: unknown
    const { deps } = makeDeps({
      captureDestination: 'library',
      onIngest: (body) => {
        ingestBody = body
      },
      ingest: ok<SubmitResponse>({ link_id: 'link-auto', status: 'done' }),
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('')

    expect(snapshot.stage).toBe('done')
    expect(ingestBody).toMatchObject({
      sources: [
        expect.objectContaining({
          kind: 'browser_capture',
          url: FAKE_RAW_CAPTURE.url,
        }),
      ],
      destination: 'library',
      requested_library_kind: 'auto',
    })
  })

  it('library_kinds 严格为 true 时保留显式 reading 选择', async () => {
    let ingestBody: unknown
    const { deps } = makeDeps({
      captureDestination: 'library',
      capabilities: ok<CapabilitiesResponse>({
        library_kinds: true,
        site_library: true,
        site_management: true,
        site_advanced_management: true,
        archive_versions: [],
        reader_vnext: false,
      } as unknown as CapabilitiesResponse),
      onIngest: (body) => {
        ingestBody = body
      },
      ingest: ok<SubmitResponse>({ link_id: 'legacy-link', status: 'done' }),
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('', undefined, 'reading')

    expect(snapshot.stage).toBe('done')
    expect(ingestBody).toMatchObject({
      destination: 'library',
      requested_library_kind: 'reading',
    })
  })

  it.each([
    [
      'library_kinds=false',
      {
        capabilities: ok<CapabilitiesResponse>({
          library_kinds: false,
        } as unknown as CapabilitiesResponse),
      },
    ],
    [
      'library_kinds 缺失',
      {
        capabilities: ok<CapabilitiesResponse>(
          {} as unknown as CapabilitiesResponse,
        ),
      },
    ],
    [
      'capability 返回失败',
      {
        capabilities: fail(
          'unauthorized',
        ) as ApiResult<CapabilitiesResponse | null>,
      },
    ],
    ['capability 探测抛错', { capabilitiesThrows: true }],
  ] as const)(
    '%s 时显式 reading 按旧后端兼容规则降为 library auto',
    async (_scenario, capabilityOptions) => {
      let ingestBody: unknown
      const { deps } = makeDeps({
        ...capabilityOptions,
        captureDestination: 'library',
        onIngest: (body) => {
          ingestBody = body
        },
        ingest: ok<SubmitResponse>({
          link_id: 'compat-reading-link',
          destination: 'library',
          status: 'done',
        }),
      })

      const snapshot = await createCaptureController(deps).startCapture(
        '',
        undefined,
        'reading',
      )

      expect(snapshot.stage).toBe('done')
      expect(ingestBody).toMatchObject({
        destination: 'library',
        requested_library_kind: 'auto',
      })
    },
  )

  it('capability 鉴权失败把 site 视为不可用，并在注入前停止', async () => {
    const { deps, injectCalls, ingestCalls } = makeDeps({
      capabilities: fail(
        'unauthorized',
      ) as ApiResult<CapabilitiesResponse | null>,
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('', undefined, 'site')

    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('other')
    expect(injectCalls()).toBe(0)
    expect(ingestCalls()).toBe(0)
  })

  it('capability 探测失败时普通保存回退到显式 library', async () => {
    let ingestBody: unknown
    const { deps, injectCalls, ingestCalls } = makeDeps({
      capabilities: fail(
        'unauthorized',
      ) as ApiResult<CapabilitiesResponse | null>,
      onIngest: (body) => {
        ingestBody = body
      },
      ingest: ok<SubmitResponse>({
        link_id: 'library-fallback',
        destination: 'library',
        status: 'done',
      }),
    })

    const snapshot = await createCaptureController(deps).startCapture('')

    expect(snapshot.stage).toBe('done')
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)
    expect(ingestBody).toMatchObject({
      destination: 'library',
      requested_library_kind: 'auto',
    })
  })

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
    'site path with library_kinds=%s site_library=%s site_management=%s is allowed=%s',
    async (libraryKinds, siteLibrary, siteManagement, allowed) => {
      let ingestBody: unknown
      const { deps, injectCalls, ingestCalls } = makeDeps({
        captureDestination: 'library',
        capabilities: ok({
          library_kinds: libraryKinds,
          site_library: siteLibrary,
          site_management: siteManagement,
          site_advanced_management: false,
          archive_versions: [],
          reader_vnext: false,
          reader: {
            annotations: false,
            notes: false,
            inbox: false,
            todos: false,
            engagement: false,
            home: false,
            feed: false,
            ai: false,
            related_tags: false,
            activity: false,
            history: false,
            trash: false,
          },
        }),
        onIngest: (body) => {
          ingestBody = body
        },
        ingest: ok<SubmitResponse>({
          link_id: 'site-matrix',
          destination: 'site',
          status: 'done',
        }),
      })

      const snapshot = await createCaptureController(deps).startCapture(
        '',
        undefined,
        'site',
      )

      expect(injectCalls()).toBe(allowed ? 1 : 0)
      expect(ingestCalls()).toBe(allowed ? 1 : 0)
      if (allowed) {
        expect(snapshot.stage).toBe('done')
        expect(ingestBody).toMatchObject({ destination: 'site' })
      } else {
        expect(snapshot.stage).toBe('failed')
        expect(ingestBody).toBeUndefined()
      }
    },
  )

  it('capability 探测意外抛出时 fail closed，不静默降级并提交', async () => {
    const { deps, ingestCalls } = makeDeps({
      capabilitiesThrows: true,
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('', undefined, 'site')

    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('other')
    expect(ingestCalls()).toBe(0)
  })

  it('重复 URL 的已有 failed library 状态不会被普通 ingest 隐式 retry', async () => {
    const { deps, getLinkCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'failed-link', status: 'failed' }),
    })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('')

    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('job-failed')
    expect(getLinkCalls()).toBe(0)
  })

  it('微信公众号采集完成时自动保存浏览器中已渲染的原文', async () => {
    let savedLinkId = ''
    const { deps, saveLinkContentCalls } = makeDeps({
      captureDestination: 'library',
      inject: {
        ok: true,
        data: {
          ...FAKE_RAW_CAPTURE,
          url: 'https://mp.weixin.qq.com/s/Y2Vp9sgIAmiqG_pegXoRWQ',
          title: '微信公众号原文',
          text: '浏览器中可见的公众号正文',
          html: '<div id="js_content"><p>浏览器中可见的公众号正文</p></div>',
        },
      },
      ingest: ok<SubmitResponse>({
        link_id: 'wechat-link-1',
        status: 'done',
      }),
      onSaveLinkContent: (linkId) => {
        savedLinkId = linkId
      },
    })

    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('')

    expect(snapshot.stage).toBe('done')
    // A duplicate done response has no final Link snapshot, so automatic
    // original persistence must fail closed rather than risk storing a site.
    expect(saveLinkContentCalls()).toBe(0)
  })

  it('ingest 返回 pending Link 时返回 submitted 初始快照', async () => {
    vi.useFakeTimers()
    const { deps } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'link-1',
        status: 'pending',
      }),
      getLink: ok(doneLink()),
    })
    const controller = createCaptureController(deps)
    const snapshot = await controller.startCapture('备注内容')
    expect(snapshot.stage).toBe('submitted')
    expect(snapshot.url).toBe(FAKE_RAW_CAPTURE.url)
    // 推进并放行后台轮询，避免它泄漏到下一个用例。
    await vi.advanceTimersByTimeAsync(3000)
  })

  it('将正文、脱敏结构、元数据和备注发送给 ingest', async () => {
    let ingestBody: unknown
    const { deps } = makeDeps({
      inject: {
        ok: true,
        data: {
          ...FAKE_RAW_CAPTURE,
          html: '<html><body>完整页面</body></html>',
          imageUrls: ['https://example.com/content.png'],
        },
      },
      ingest: ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
      onIngest: (body) => {
        ingestBody = body
      },
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('我的备注')
    const sources = (ingestBody as { sources: unknown[] }).sources
    expect(ingestBody).toMatchObject({ destination: 'inbox' })
    const first = sources[0] as {
      kind: string
      text: string
      html: string
      image_urls: string[]
      metadata: { note: string }
    }
    expect(first.kind).toBe('browser_capture')
    expect(first.text).toBe(FAKE_RAW_CAPTURE.text)
    expect(first.html).toBe('<html><body>完整页面</body></html>')
    expect(first.image_urls).toEqual([])
    expect(first.metadata.note).toBe('我的备注')
  })
})

describe('createCaptureController — 轮询阶段', () => {
  it('轮询到后端 done 时推送 done 快照', async () => {
    vi.useFakeTimers()
    let savedLinkId = ''
    const { deps, saveLinkContentCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(doneLink()),
      onSaveLinkContent: (linkId) => {
        savedLinkId = linkId
      },
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('')
    await vi.advanceTimersByTimeAsync(3000)

    expect((await controller.getLatestSnapshot()).stage).toBe('done')
    // The fixture intentionally has no final classification; only an explicit
    // final reading result is eligible for automatic original persistence.
    expect(saveLinkContentCalls()).toBe(0)
  })

  it('轮询到后端 failed 时推送 failed(job-failed) 快照并透传 error_msg', async () => {
    vi.useFakeTimers()
    const { deps } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(failedLink('抓取失败', 'fetch_error')),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('')
    await vi.advanceTimersByTimeAsync(3000)

    const snapshot = await controller.getLatestSnapshot()
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('job-failed')
    expect(snapshot.errorMessage).toBe('抓取失败')
  })

  it('仅在最终分类明确为阅读时保存浏览器原文', async () => {
    vi.useFakeTimers()
    const { deps, saveLinkContentCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'reading-link',
        status: 'pending',
      }),
      getLink: ok(doneReadingLink()),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('', undefined, 'auto')
    await vi.advanceTimersByTimeAsync(3000)
    expect(saveLinkContentCalls()).toBe(1)
    expect((await controller.getLatestSnapshot()).libraryKind).toBe('reading')
  })

  it('最终分类为网站时不保存浏览器原文', async () => {
    vi.useFakeTimers()
    const { deps, saveLinkContentCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'site-link',
        status: 'pending',
      }),
      getLink: ok(doneSiteLink()),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('', undefined, 'site')
    await vi.advanceTimersByTimeAsync(3000)
    expect(saveLinkContentCalls()).toBe(0)
    expect((await controller.getLatestSnapshot()).libraryKind).toBe('site')
  })

  it('轮询预算耗尽但后端仍 processing 时收尾为 still-processing（非 failed）', async () => {
    // UX 加固：LLM 慢解析常超出扩展轮询窗口。预算耗尽且后端仍 processing
    // 不再误报 failed，而是中性的 still-processing 终态，引导用户去知识库查看。
    vi.useFakeTimers()
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('')
    // 推进整个轮询预算（每轮 POLL_INTERVAL_MS，共 MAX_POLL_ATTEMPTS 轮）。
    await vi.advanceTimersByTimeAsync(
      POLL_INTERVAL_MS * (MAX_POLL_ATTEMPTS + 1),
    )

    const snapshot = await controller.getLatestSnapshot()
    expect(snapshot.stage).toBe('still-processing')
    // 中性态不带 errorKind / errorMessage。
    expect(snapshot.errorKind).toBeUndefined()
    expect(snapshot.errorMessage).toBeUndefined()
    // 终态：in-flight 已清除，看门狗不再续跑。
    expect(store._inFlight()).toBeNull()
  })
})

describe('createCaptureController — getLatestSnapshot', () => {
  it('新建 controller 的初始快照为 idle（store 为空）', async () => {
    const { deps } = makeDeps()
    const controller = createCaptureController(deps)
    expect((await controller.getLatestSnapshot()).stage).toBe('idle')
  })

  it('两个 controller 实例各用独立 store，互不共享快照', async () => {
    const a = createCaptureController(
      makeDeps({ captureDestination: 'library' }).deps,
    )
    const b = createCaptureController(
      makeDeps({ captureDestination: 'library' }).deps,
    )
    await a.startCapture('')
    expect((await a.getLatestSnapshot()).stage).toBe('done')
    expect((await b.getLatestSnapshot()).stage).toBe('idle')
  })

  it('重唤醒的 SW（新 controller 共用同一 store）读回真实快照', async () => {
    // 模拟 SW 被回收重建：同一个 store 喂给新 controller 实例。
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
    })
    const first = createCaptureController(deps)
    await first.startCapture('')
    expect((await first.getLatestSnapshot()).stage).toBe('done')

    // SW 被回收 → 新 controller，共用 store。
    const respawned = createCaptureController({ ...deps, store })
    // 重唤醒的 SW 不再返回 idle，而是 store 里的真实 done。
    expect((await respawned.getLatestSnapshot()).stage).toBe('done')
  })

  it('a failed owner peek does not let the no-key fallback delete a sealed snapshot', async () => {
    const backing = makeSessionArea()
    const writer = createSessionCaptureStore(backing)
    const snapshot: CaptureSnapshot = {
      stage: 'done',
      url: 'https://private.example.test/recover-after-read-failure',
      title: 'sealed snapshot',
      owner: OWNER_A,
      updatedAt: 1_700_000_000_000,
    }
    await writer.setSnapshot(snapshot, TEST_RECOVERY_KEY)
    let firstRead = true
    const flakyArea: SessionStorageArea = {
      ...backing,
      get: (keys) => {
        if (firstRead) {
          firstRead = false
          return Promise.reject(new Error('temporary storage read failure'))
        }
        return backing.get(keys)
      },
    }
    const { deps } = makeDeps()
    const controller = createCaptureController({
      ...deps,
      store: createSessionCaptureStore(flakyArea),
    })
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})

    await expect(controller.getLatestSnapshot()).resolves.toEqual(
      IDLE_CAPTURE_SNAPSHOT,
    )
    expect(backing._data[STORAGE_KEY_SNAPSHOT]).toBeDefined()
    await expect(controller.getLatestSnapshot()).resolves.toEqual(snapshot)
    warnSpy.mockRestore()
  })

  it('轮询中读取状态复用 live activation，不中断下一次 getLink', async () => {
    vi.useFakeTimers()
    const { deps, getLinkCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'link-a',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    expect((await controller.startCapture('')).stage).toBe('submitted')

    expect((await controller.getLatestSnapshot()).stage).toBe('submitted')
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS + 100)

    expect(getLinkCalls()).toBe(1)
    vi.clearAllTimers()
  })
})

describe('createCaptureController — 持久化', () => {
  it('提交进入轮询时 store 写入 in-flight 记录', async () => {
    vi.useFakeTimers()
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      // 轮询保持 processing，让 in-flight 记录留存便于断言。
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('备注')

    const inFlight = store._inFlight()
    expect(inFlight).not.toBeNull()
    expect(inFlight?.linkId).toBe('l')
    expect(inFlight?.note).toBe('备注')
    expect(inFlight?.attempt).toBe(0)

    vi.clearAllTimers()
  })

  it('采集到终态（done）后 store 的 in-flight 记录被清除', async () => {
    vi.useFakeTimers()
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(doneLink()),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('')
    await vi.advanceTimersByTimeAsync(3000)

    expect(store._inFlight()).toBeNull()
  })

  it('同步终态（命中已 done）也清除 in-flight 记录', async () => {
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
    })
    const controller = createCaptureController(deps)
    await controller.startCapture('')
    expect(store._inFlight()).toBeNull()
  })
})

describe('createCaptureController — resumeIfNeeded 看门狗', () => {
  it('store 无 in-flight 时 resumeIfNeeded 不做任何事', async () => {
    const { deps, getLinkCalls } = makeDeps()
    const controller = createCaptureController(deps)
    await controller.resumeIfNeeded()
    expect(getLinkCalls()).toBe(0)
  })

  it('store 有 in-flight 且本实例无轮询时，resumeIfNeeded 从持久 attempt 续跑到 done', async () => {
    vi.useFakeTimers()
    // 预置一个进行中任务（模拟上一个被回收的 SW 留下的状态）。
    const { deps, store } = makeDeps({ getLink: ok(doneLink()) })
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-1',
      url: 'https://example.com/x',
      title: '续跑页',
      note: '',
      attempt: 5,
      snapshot: {
        stage: 'parsing',
        url: 'https://example.com/x',
        title: '续跑页',
        owner: OWNER_A,
        updatedAt: 1,
      },
      // 固定时间源恒为 1_700_000_000_000，startedAt 取同值 → 不陈旧。
      startedAt: 1_700_000_000_000,
    })

    // 新 controller（模拟重建的 SW），共用同一 store。
    const controller = createCaptureController(deps)
    void controller.resumeIfNeeded()
    await vi.advanceTimersByTimeAsync(3000)

    // 续跑命中 done：快照终态、in-flight 清除。
    expect((await controller.getLatestSnapshot()).stage).toBe('done')
    expect(store._inFlight()).toBeNull()
  })

  it('重放持久化 site 提交时保留完整请求体和幂等 key，且不重新探测 capability', async () => {
    const area = makeSessionArea()
    const store = createSessionCaptureStore(area)
    const source: IngestSource = {
      kind: 'browser_capture',
      url: 'https://private.docs.example.test/alice?draft=1',
      title: 'Persisted site capture',
      text: 'Exact captured text',
      html: '<main>Exact captured HTML</main>',
      image_urls: ['https://docs.example.test/cover.png'],
      metadata: {
        capture_source: 'browser_extension',
        canonical_url: 'https://private.docs.example.test/alice?draft=1',
        client_data_namespace: 'raw-private-namespace',
      },
    }
    const ingestRequest: IngestRequest = {
      sources: [source],
      destination: 'site',
    }
    const idempotencyKey = 'persisted-site-idempotency-key'
    const persistedCapture: InFlightCapture = {
      owner: OWNER_A,
      linkId: '',
      url: source.url ?? '',
      title: source.title ?? '',
      note: 'keep the persisted note',
      phase: 'submitting',
      destination: 'site',
      requestedKind: 'site',
      idempotencyKey,
      source,
      ingestRequest,
      attempt: 0,
      snapshot: {
        stage: 'capturing',
        url: source.url ?? '',
        title: source.title ?? '',
        requestedKind: 'site',
        owner: OWNER_A,
        updatedAt: 1_700_000_000_000,
      },
      startedAt: 1_700_000_000_000,
    }
    await store.setInFlight(persistedCapture, TEST_RECOVERY_KEY)

    const rawStorage = JSON.stringify(area._data[STORAGE_KEY_INFLIGHT])
    for (const privateValue of [
      source.url ?? '',
      source.text ?? '',
      source.metadata?.client_data_namespace ?? '',
      persistedCapture.note,
      idempotencyKey,
    ]) {
      expect(rawStorage).not.toContain(privateValue)
    }

    const getCapabilities = vi.fn(() =>
      Promise.resolve(
        fail('other', 'a persisted mutation must not re-probe capabilities'),
      ),
    )
    const ingest = vi.fn(
      (body: IngestRequest, options?: { idempotencyKey?: string }) =>
        Promise.resolve(
          ok<SubmitResponse>({
            link_id: 'site-link-after-restart',
            destination: 'site',
            status: 'done',
          }),
        ),
    )
    const injectCapture = vi.fn<CaptureDeps['injectCapture']>(() =>
      Promise.resolve({ ok: true, data: FAKE_RAW_CAPTURE }),
    )
    const client = {
      getCapabilities,
      ingest,
    } as unknown as WebTagClient
    const controller = createCaptureController({
      activateConnection: () =>
        Promise.resolve({
          client,
          owner: OWNER_A,
          lease: new CaptureActivationLease(OWNER_A, () => true),
          recoveryKey: TEST_RECOVERY_KEY,
        }),
      injectCapture,
      sendSnapshot: () => {},
      store,
      now: () => 1_700_000_000_000,
    })

    await controller.resumeIfNeeded()

    expect(injectCapture).not.toHaveBeenCalled()
    expect(getCapabilities).not.toHaveBeenCalled()
    expect(ingest).toHaveBeenCalledTimes(1)
    expect(ingest.mock.calls[0]?.[0]).toEqual(ingestRequest)
    expect(ingest.mock.calls[0]?.[0]).toMatchObject({ destination: 'site' })
    expect(ingest.mock.calls[0]?.[0]).not.toHaveProperty(
      'requested_library_kind',
    )
    expect(ingest.mock.calls[0]?.[1]).toEqual({ idempotencyKey })
    expect(await store.getInFlightMetadata()).toBeNull()
  })

  it('in-flight 存在但配置已被清空时，resumeIfNeeded 收尾为 failed(not-configured)', async () => {
    const { deps, store } = makeDeps({ notConfigured: true })
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-1',
      url: 'https://example.com/x',
      title: '续跑页',
      note: '',
      requestedKind: 'site',
      attempt: 3,
      snapshot: {
        stage: 'parsing',
        url: 'https://example.com/x',
        title: '续跑页',
        owner: OWNER_A,
        updatedAt: 1,
      },
      // 固定时间源恒为 1_700_000_000_000，startedAt 取同值 → 记录未陈旧，
      // resumeIfNeeded 才会走到 activateConnection 分支并隔离旧任务。
      startedAt: 1_700_000_000_000,
    })
    const controller = createCaptureController(deps)
    await controller.resumeIfNeeded()

    const snapshot = await controller.getLatestSnapshot()
    expect(snapshot).toEqual(IDLE_CAPTURE_SNAPSHOT)
    expect(store._inFlight()).toBeNull()
  })

  it('续跑前 attempt 已达 MAX_POLL_ATTEMPTS（预算耗尽临界点）：收尾为 still-processing 并清 in-flight，不进轮询', async () => {
    // 复刻「SW 恰在最后一轮落盘 attempt=MAX_POLL_ATTEMPTS 后、收尾前被回收」：
    // 若直接续跑，runCapturePolling 的 for 循环条件一开始即为 false，循环体不执行，
    // 既不发布终态也不清 in-flight，记录会残留到 MAX_CAPTURE_AGE_MS 陈旧兜底。
    // 修复后应直接走与活体预算耗尽一致的 still-processing 终态收尾。
    const { deps, store, getLinkCalls } = makeDeps()
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-1',
      url: 'https://example.com/x',
      title: '临界页',
      note: '',
      attempt: MAX_POLL_ATTEMPTS,
      snapshot: {
        stage: 'parsing',
        url: 'https://example.com/x',
        title: '临界页',
        owner: OWNER_A,
        updatedAt: 1,
      },
      // 固定时间源恒为 1_700_000_000_000，startedAt 取同值 → 记录未陈旧，
      // 才能走到 attempt 临界点分支（而非被陈旧判定先清除）。
      startedAt: 1_700_000_000_000,
    })

    const controller = createCaptureController(deps)
    await controller.resumeIfNeeded()

    // 收尾为中性的 still-processing 终态，不进轮询（getLink 零调用）。
    const snapshot = await controller.getLatestSnapshot()
    expect(snapshot.stage).toBe('still-processing')
    expect(snapshot.errorKind).toBeUndefined()
    expect(getLinkCalls()).toBe(0)
    // in-flight 已清除，不再阻塞后续新采集。
    expect(store._inFlight()).toBeNull()
  })

  it('续跑前 attempt 超过 MAX_POLL_ATTEMPTS 时同样走 still-processing 收尾', async () => {
    const { deps, store, getLinkCalls } = makeDeps()
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-1',
      url: 'https://example.com/y',
      title: '超界页',
      note: '',
      attempt: MAX_POLL_ATTEMPTS + 3,
      snapshot: {
        stage: 'parsing',
        url: 'https://example.com/y',
        title: '超界页',
        owner: OWNER_A,
        updatedAt: 1,
      },
      startedAt: 1_700_000_000_000,
    })

    const controller = createCaptureController(deps)
    await controller.resumeIfNeeded()

    expect((await controller.getLatestSnapshot()).stage).toBe(
      'still-processing',
    )
    expect(getLinkCalls()).toBe(0)
    expect(store._inFlight()).toBeNull()
  })

  it('A 的 submitting capture 在 B 恢复时零 ingest，并允许 B 独立采集', async () => {
    const { deps, store, ingestCalls, getLinkCalls, saveLinkContentCalls } =
      makeDeps({
        owner: OWNER_B,
        captureDestination: 'library',
        ingest: ok<SubmitResponse>({ link_id: 'link-b', status: 'done' }),
      })
    const source: IngestSource = {
      kind: 'browser_capture',
      url: 'https://private-a.example.test/draft',
      title: 'Private A title',
      text: 'private A body',
    }
    await store.setInFlight({
      owner: OWNER_A,
      linkId: '',
      url: source.url ?? '',
      title: source.title ?? '',
      note: '',
      phase: 'submitting',
      source,
      ingestRequest: { sources: [source], destination: 'library' },
      destination: 'library',
      requestedKind: 'auto',
      idempotencyKey: 'owner-a-key',
      attempt: 0,
      snapshot: {
        owner: OWNER_A,
        stage: 'capturing',
        url: source.url ?? '',
        title: source.title ?? '',
        updatedAt: 1_700_000_000_000,
      },
      startedAt: 1_700_000_000_000,
    })
    const decryptSpy = vi.spyOn(store, 'getInFlight')
    const controller = createCaptureController(deps)

    await controller.resumeIfNeeded()

    expect(decryptSpy).not.toHaveBeenCalled()
    expect(ingestCalls()).toBe(0)
    expect(getLinkCalls()).toBe(0)
    expect(saveLinkContentCalls()).toBe(0)
    expect(await controller.getLatestSnapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
    expect(store._inFlight()).toBeNull()

    const captureB = await controller.startCapture('B capture')
    expect(captureB.stage).toBe('done')
    expect(captureB.owner).toEqual(OWNER_B)
    expect(ingestCalls()).toBe(1)
  })

  it('a transient activation failure preserves sealed recovery state for a later retry', async () => {
    const { deps, store, injectCalls } = makeDeps()
    const persisted = {
      owner: OWNER_A,
      linkId: 'link-a',
      url: 'https://private-a.example.test/transient',
      title: 'Private A title',
      note: '',
      phase: 'polling' as const,
      attempt: 1,
      snapshot: {
        owner: OWNER_A,
        stage: 'parsing' as const,
        url: 'https://private-a.example.test/transient',
        title: 'Private A title',
        updatedAt: 1_700_000_000_000,
      },
      startedAt: 1_700_000_000_000,
    }
    await store.setInFlight(persisted)
    deps.activateConnection = () =>
      Promise.resolve({ status: 'transient-unavailable' } as never)
    const controller = createCaptureController(deps)

    await expect(controller.startCapture('retry later')).resolves.toEqual(
      IDLE_CAPTURE_SNAPSHOT,
    )
    expect(injectCalls()).toBe(0)
    expect(store._inFlight()).toEqual(persisted)
  })

  it('一次 B startCapture 会隔离已有 A 记录并立即执行 B 采集', async () => {
    const { deps, store, ingestCalls } = makeDeps({
      owner: OWNER_B,
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'link-b', status: 'done' }),
    })
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-a',
      url: 'https://private-a.example.test/article',
      title: 'Private A title',
      note: '',
      phase: 'polling',
      attempt: 1,
      snapshot: {
        owner: OWNER_A,
        stage: 'parsing',
        url: 'https://private-a.example.test/article',
        title: 'Private A title',
        updatedAt: 1_700_000_000_000,
      },
      startedAt: 1_700_000_000_000,
    })
    const controller = createCaptureController(deps)

    const result = await controller.startCapture('new B capture')

    expect(result.stage).toBe('done')
    expect(result.owner).toEqual(OWNER_B)
    expect(ingestCalls()).toBe(1)
    expect(store._inFlight()).toBeNull()
  })

  it.each([
    ['different installation', OWNER_B],
    [
      'same installation after reactivation',
      { fingerprint: OWNER_A.fingerprint, revision: OWNER_A.revision + 1 },
    ],
  ] as const)(
    'A polling capture is quarantined for %s with zero getLink/save',
    async (_name, currentOwner) => {
      const { deps, store, ingestCalls, getLinkCalls, saveLinkContentCalls } =
        makeDeps({ owner: currentOwner })
      await store.setInFlight({
        owner: OWNER_A,
        linkId: 'link-a',
        url: 'https://private-a.example.test/article',
        title: 'Private A title',
        note: '',
        phase: 'polling',
        attempt: 2,
        snapshot: {
          owner: OWNER_A,
          stage: 'parsing',
          url: 'https://private-a.example.test/article',
          title: 'Private A title',
          updatedAt: 1_700_000_000_000,
        },
        startedAt: 1_700_000_000_000,
      })
      const controller = createCaptureController(deps)

      await controller.resumeIfNeeded()

      expect(ingestCalls()).toBe(0)
      expect(getLinkCalls()).toBe(0)
      expect(saveLinkContentCalls()).toBe(0)
      expect(await controller.getLatestSnapshot()).toEqual(
        IDLE_CAPTURE_SNAPSHOT,
      )
      expect(store._inFlight()).toBeNull()
    },
  )

  it('revoked lease stops before the next getLink continuation', async () => {
    vi.useFakeTimers()
    let current = true
    const { deps, store, getLinkCalls } = makeDeps({
      captureDestination: 'library',
      leaseCurrent: () => current,
      ingest: ok<SubmitResponse>({
        link_id: 'link-a',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    expect((await controller.startCapture('')).stage).toBe('submitted')

    current = false
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS + 100)

    expect(getLinkCalls()).toBe(0)
    expect(store._inFlight()).toBeNull()
    expect(store._snapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
  })

  it('revoked lease after getLink returns prevents save and hides old snapshot', async () => {
    vi.useFakeTimers()
    let current = true
    const pendingLink = deferred<ApiResult<Link>>()
    const { deps, store, getLinkCalls, saveLinkContentCalls } = makeDeps({
      captureDestination: 'library',
      leaseCurrent: () => current,
      ingest: ok<SubmitResponse>({
        link_id: 'link-a',
        status: 'pending',
      }),
      getLinkFactory: () => pendingLink.promise,
    })
    const controller = createCaptureController(deps)
    expect((await controller.startCapture('')).stage).toBe('submitted')
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS + 100)
    expect(getLinkCalls()).toBe(1)

    current = false
    pendingLink.resolve(ok(doneReadingLink()))
    await vi.waitFor(() => expect(store._inFlight()).toBeNull())

    expect(saveLinkContentCalls()).toBe(0)
    expect(store._snapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
  })

  it('a late A poll cannot clear the newer B in-flight capture', async () => {
    vi.useFakeTimers()
    let currentOwner = OWNER_A
    const { deps, store, getLinkCalls } = makeDeps({
      owner: () => currentOwner,
      leaseCurrent: (owner) => owner === currentOwner,
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'link',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    expect((await controller.startCapture('A')).owner).toEqual(OWNER_A)

    currentOwner = OWNER_B
    const captureB = await controller.startCapture('B')
    expect(captureB.stage).toBe('submitted')
    expect(captureB.owner).toEqual(OWNER_B)

    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS + 100)

    expect(getLinkCalls()).toBe(0)
    expect(store._inFlight()?.owner).toEqual(OWNER_B)
    expect(store._snapshot().owner).toEqual(OWNER_B)
  })

  it('a deferred A finalization cannot clear or revoke a newer B capture', async () => {
    vi.useFakeTimers()
    let currentOwner = OWNER_A
    const finalWriteStarted = deferred<void>()
    const releaseFinalWrite = deferred<void>()
    const { deps, store } = makeDeps({
      owner: () => currentOwner,
      leaseCurrent: (owner) => owner === currentOwner,
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'link',
        status: 'pending',
      }),
      getLink: ok(doneLink()),
    })
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-a',
      url: 'https://private-a.example.test/deferred',
      title: 'Private A title',
      note: '',
      phase: 'polling',
      attempt: 0,
      snapshot: {
        owner: OWNER_A,
        stage: 'submitted',
        url: 'https://private-a.example.test/deferred',
        title: 'Private A title',
        updatedAt: 1_700_000_000_000,
      },
      startedAt: 1_700_000_000_000,
    })
    const setSnapshot = store.setSnapshot.bind(store)
    store.setSnapshot = async (snapshot, recoveryKey) => {
      if (snapshot.owner === OWNER_A && snapshot.stage === 'done') {
        finalWriteStarted.resolve()
        await releaseFinalWrite.promise
      }
      await setSnapshot(snapshot, recoveryKey)
    }
    const controller = createCaptureController(deps)
    const resumeA = controller.resumeIfNeeded()
    await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS + 100)
    await finalWriteStarted.promise

    currentOwner = OWNER_B
    const captureB = controller.startCapture('B')
    await vi.advanceTimersByTimeAsync(0)
    releaseFinalWrite.resolve()
    await resumeA
    const resultB = await captureB

    expect(resultB.stage).toBe('submitted')
    expect(resultB.owner).toEqual(OWNER_B)
    expect(store._inFlight()?.owner).toEqual(OWNER_B)
    expect(store._snapshot().owner).toEqual(OWNER_B)
    expect((await controller.getLatestSnapshot()).owner).toEqual(OWNER_B)
  })

  it('本实例已有轮询在跑时，resumeIfNeeded 不重复启动轮询', async () => {
    vi.useFakeTimers()
    const { deps, getLinkCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)
    // 启动一次采集 → 本实例已有轮询循环在跑。
    await controller.startCapture('')
    await vi.advanceTimersByTimeAsync(2100)
    const callsAfterFirstPoll = getLinkCalls()

    // 看门狗触发：本实例 pollLoopRunning=true，应直接跳过。
    await controller.resumeIfNeeded()
    // 仅靠原轮询自然推进，没有第二个循环叠加调用。
    await vi.advanceTimersByTimeAsync(2100)
    expect(getLinkCalls()).toBe(callsAfterFirstPoll + 1)

    vi.clearAllTimers()
  })
})

describe('createCaptureController — 并发守卫', () => {
  it('提交前的两个并发 startCapture 共享同一个操作，不重复注入/提交', async () => {
    const { deps, ingestCalls, injectCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
    })
    const controller = createCaptureController(deps)

    const first = controller.startCapture('first')
    const second = controller.startCapture('second')
    const [firstSnapshot, secondSnapshot] = await Promise.all([first, second])

    expect(firstSnapshot).toEqual(secondSnapshot)
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)
  })

  it('采集进行中再次 startCapture 返回当前快照、不重复注入/提交', async () => {
    vi.useFakeTimers()
    const { deps, ingestCalls, injectCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)

    const first = await controller.startCapture('')
    expect(first.stage).toBe('submitted')
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)

    // 第二次：store 里有 in-flight，应直接返回当前快照。
    const second = await controller.startCapture('')
    expect(second.stage).toBe('submitted')
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)

    vi.clearAllTimers()
  })

  it('采集完成后并发守卫释放，可再次发起采集', async () => {
    const { deps, injectCalls } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({ link_id: 'l', status: 'done' }),
    })
    const controller = createCaptureController(deps)

    await controller.startCapture('')
    expect(injectCalls()).toBe(1)

    // in-flight 已清除（同步终态），第二次采集可正常发起。
    await controller.startCapture('')
    expect(injectCalls()).toBe(2)
  })

  it('提交租约持久化幂等 key，供网络重试与 SW 重启复用', async () => {
    vi.useFakeTimers()
    let ingestOptions: unknown
    const { deps, store } = makeDeps({
      captureDestination: 'library',
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
      onIngestOptions: (options) => {
        ingestOptions = options
      },
    })
    const controller = createCaptureController(deps)

    await controller.startCapture('备注')

    const persisted = store._inFlight()
    expect(persisted?.idempotencyKey).toEqual(
      (ingestOptions as { idempotencyKey: string }).idempotencyKey,
    )
    expect(persisted?.idempotencyKey).toEqual(expect.any(String))
    vi.clearAllTimers()
  })

  it('SW 重启后复用已持久化的精确请求体和幂等 key，不重新探测 capability', async () => {
    const store = makeMemoryStore()
    const capabilities = vi.fn(() =>
      Promise.resolve(
        ok<CapabilitiesResponse>({
          library_kinds: true,
          site_library: true,
          site_management: true,
          site_advanced_management: true,
          archive_versions: [],
          reader_vnext: true,
          reader: {
            annotations: true,
            notes: true,
            inbox: true,
            todos: true,
            engagement: true,
            home: true,
            feed: true,
            ai: true,
            related_tags: true,
            activity: true,
            history: true,
            trash: true,
          },
        }),
      ),
    )
    const bodies: unknown[] = []
    const keys: string[] = []
    let unblockFirst: (result: ApiResult<SubmitResponse>) => void = () => {}
    let ingestCount = 0
    const client: WebTagClient = {
      getCapabilities: capabilities,
      ingest: (body, options) => {
        bodies.push(body)
        keys.push(options?.idempotencyKey ?? '')
        ingestCount += 1
        if (ingestCount === 1) {
          return new Promise((resolve) => {
            unblockFirst = resolve
          })
        }
        return Promise.resolve(
          ok<SubmitResponse>({
            inbox_id: 'inbox-after-restart',
            destination: 'inbox',
            status: 'pending',
          }),
        )
      },
    } as unknown as WebTagClient
    const deps: CaptureDeps = {
      activateConnection: () =>
        Promise.resolve({
          client,
          owner: OWNER_A,
          lease: new CaptureActivationLease(OWNER_A, () => true),
          recoveryKey: TEST_RECOVERY_KEY,
        }),
      injectCapture: () =>
        Promise.resolve({ ok: true, data: FAKE_RAW_CAPTURE }),
      sendSnapshot: () => {},
      store,
      getCaptureDestination: () => Promise.resolve('inbox'),
      now: () => 1_700_000_000_000,
    }

    const first = createCaptureController(deps)
    const firstStart = first.startCapture('keep this note')
    await vi.waitFor(() => expect(bodies).toHaveLength(1))
    expect(store._inFlight()?.ingestRequest).toEqual(bodies[0])

    const respawned = createCaptureController({ ...deps, store })
    await respawned.resumeIfNeeded()

    expect(capabilities).toHaveBeenCalledTimes(1)
    expect(bodies[1]).toEqual(bodies[0])
    expect(keys[1]).toBe(keys[0])

    unblockFirst(
      ok<SubmitResponse>({
        inbox_id: 'inbox-after-restart',
        destination: 'inbox',
        status: 'pending',
      }),
    )
    await firstStart
  })
})

describe('createCaptureController — storage 故障下的 fail-safe', () => {
  // ── H1：setInFlight 被丢弃 → 内存守卫仍阻止重复 ingest ────────
  it('setInFlight 持久化被丢弃时，内存级守卫仍阻止第二次 startCapture 重复注入/提交', async () => {
    vi.useFakeTimers()
    // dropSetInFlight：模拟 storage 写失败被生产 store fail-safe 吞掉，
    // 持久层始终为空——若并发去重只看持久记录，第二次采集会重复 ingest。
    const { deps, store, ingestCalls, injectCalls } = makeDeps({
      captureDestination: 'library',
      storeOptions: { dropSetInFlight: true },
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(processingLink()),
    })
    const controller = createCaptureController(deps)

    const first = await controller.startCapture('')
    expect(first.stage).toBe('submitted')
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)
    // 持久记录确实没落盘（setInFlight 被丢弃）。
    expect(store._inFlight()).toBeNull()

    // 第二次 startCapture：持久层为空，但本 SW 实例的内存级 in-flight 守卫
    // 非空 → 立即返回当前快照，绝不重复注入 / 重复提交同一页面到 /api/ingest。
    const second = await controller.startCapture('')
    expect(second.stage).toBe('submitted')
    expect(injectCalls()).toBe(1)
    expect(ingestCalls()).toBe(1)

    vi.clearAllTimers()
  })

  // ── H1：clearInFlight 被丢弃 → 陈旧判定自愈，不锁死会话 ────────
  it('clearInFlight 被丢弃留下僵尸记录时，陈旧判定在采集预算耗尽后放行新采集（不锁死会话）', async () => {
    vi.useFakeTimers()
    // 可推进的时间源：模拟采集结束后墙钟继续走，最终超出采集预算。
    let nowMs = 1_700_000_000_000
    const { deps, store, injectCalls } = makeDeps({
      captureDestination: 'library',
      now: () => nowMs,
      // dropClearInFlight：轮询终态时 clearInFlight 被丢弃，僵尸记录留存。
      storeOptions: { dropClearInFlight: true },
      // 走 pending Link 轮询路径：persistInFlight 真实落盘一条 in-flight 记录，
      // 轮询到 done 后 finishCapture → clearInFlight 被丢弃 → 留下僵尸记录。
      ingest: ok<SubmitResponse>({
        link_id: 'l',
        status: 'pending',
      }),
      getLink: ok(doneLink()),
    })
    const controller = createCaptureController(deps)

    // 第一次采集：提交 → 轮询到 done → clearInFlight 被丢弃 → 持久层留僵尸记录。
    const first = await controller.startCapture('')
    expect(first.stage).toBe('submitted')
    await vi.advanceTimersByTimeAsync(3000)
    expect((await controller.getLatestSnapshot()).stage).toBe('done')
    // 僵尸记录确实残留在持久层（clearInFlight 被丢弃）。
    expect(store._inFlight()).not.toBeNull()
    vi.useRealTimers()

    // 墙钟推进到超出采集预算：僵尸记录被 isInFlightStale 判定为陈旧。
    nowMs += MAX_CAPTURE_AGE_MS + 1
    // 新 controller 模拟 SW 重建后再次发起采集，共用同一 store——
    // 内存守卫已随旧 SW 闭包丢失，去重只能依赖持久层的陈旧判定。
    const respawned = createCaptureController({ ...deps, store })
    const healed = await respawned.startCapture('')
    // 陈旧记录被放行 → 新采集正常发起、注入计数递增，会话未被永久锁死。
    expect(healed.stage).toBe('submitted')
    expect(injectCalls()).toBe(2)
  })

  // ── H1：看门狗对陈旧僵尸记录不续跑，直接清槽 ──────────────────
  it('resumeIfNeeded 遇陈旧僵尸记录时不续跑、直接清掉持久槽位', async () => {
    let nowMs = 1_700_000_000_000
    const { deps, store, getLinkCalls } = makeDeps({ now: () => nowMs })
    // 预置一条「采集早已开始」的 in-flight 记录。
    await store.setInFlight({
      owner: OWNER_A,
      linkId: 'link-zombie',
      url: 'https://example.com/x',
      title: '僵尸记录',
      note: '',
      attempt: 2,
      snapshot: {
        stage: 'parsing',
        url: 'https://example.com/x',
        title: '僵尸记录',
        owner: OWNER_A,
        updatedAt: 1,
      },
      startedAt: nowMs,
    })
    // 墙钟推进到超出采集预算 → 记录陈旧。
    nowMs += MAX_CAPTURE_AGE_MS + 1

    const controller = createCaptureController(deps)
    await controller.resumeIfNeeded()

    // 不对陈旧任务 getLink 续跑，且持久槽位被清空（不再阻塞后续新采集）。
    expect(getLinkCalls()).toBe(0)
    expect(store._inFlight()).toBeNull()
  })
})

describe('createCaptureController — try/catch 防死锁', () => {
  it('ingest 意外抛出时兜底为 failed(capture-unexpected)，不留下死锁', async () => {
    const { deps, store } = makeDeps({ ingestThrows: true })
    const controller = createCaptureController(deps)

    const snapshot = await controller.startCapture('', undefined, 'reading')
    // 意外异常被外层 try/catch 兜底为 failed。
    expect(snapshot.stage).toBe('failed')
    expect(snapshot.errorKind).toBe('capture-unexpected')
    expect(snapshot.requestedKind).toBe('reading')
    expect(snapshot.errorMessage).toContain('意外的 ingest 异常')
    // in-flight 记录被清理，没有永久死锁。
    expect(store._inFlight()).toBeNull()

    // 关键：再次发起采集仍能正常工作（守卫已释放）。
    const second = await controller.startCapture('')
    expect(second.stage).toBe('failed')
    expect(second.errorKind).toBe('capture-unexpected')
  })
})
