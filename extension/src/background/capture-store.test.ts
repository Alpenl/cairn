/**
 * capture-store.test.ts — 采集状态持久化层单元测试。
 *
 * createSessionCaptureStore 把一个 SessionStorageArea（生产是
 * chrome.storage.session）适配为 CaptureStore。本测试注入一个内存版
 * SessionStorageArea，验证：
 *   - 快照 / in-flight 的读写与清除
 *   - 空态回退（未写过 → idle 快照 / null in-flight）
 *   - storage 抛错时的兜底（读回退空态、写不再静默吞掉而是 console.warn 留痕）
 *   - isInFlightStale 陈旧判定（带 startedAt 时间戳的失效自愈）
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  STALE_INFLIGHT_MARGIN_MS,
  STORAGE_KEY_INFLIGHT,
  STORAGE_KEY_SNAPSHOT,
  createSessionCaptureStore,
  isInFlightStale,
  type CaptureStore,
  type InFlightCapture,
  type SessionStorageArea,
} from './capture-store'
import { IDLE_CAPTURE_SNAPSHOT, type CaptureSnapshot } from './capture-protocol'
import { createCaptureRecoveryKey } from '@/api/capabilities'

// ── 内存版 SessionStorageArea ───────────────────────────────

/** 用普通对象模拟 chrome.storage.session 的 get/set/remove。 */
function makeMemoryArea(): SessionStorageArea & {
  _data: Record<string, unknown>
} {
  const data: Record<string, unknown> = {}
  return {
    _data: data,
    get: (keys) => {
      const keyList = Array.isArray(keys) ? keys : [keys]
      const result: Record<string, unknown> = {}
      for (const key of keyList) {
        if (key in data) result[key] = data[key]
      }
      return Promise.resolve(result)
    },
    set: (items) => {
      Object.assign(data, items)
      return Promise.resolve()
    },
    remove: (keys) => {
      const keyList = Array.isArray(keys) ? keys : [keys]
      for (const key of keyList) delete data[key]
      return Promise.resolve()
    },
  }
}

/** 一个会对所有操作抛错的 SessionStorageArea。 */
function makeThrowingArea(): SessionStorageArea {
  return {
    get: () => Promise.reject(new Error('storage 不可用')),
    set: () => Promise.reject(new Error('storage 不可用')),
    remove: () => Promise.reject(new Error('storage 不可用')),
  }
}

// ── 测试夹具 ────────────────────────────────────────────────

function makeSnapshot(
  overrides: Partial<CaptureSnapshot> = {},
): CaptureSnapshot {
  return {
    stage: 'parsing',
    url: 'https://example.com/x',
    title: '示例',
    owner: makeOwner(),
    updatedAt: 1_700_000_000_000,
    ...overrides,
  }
}

function makeOwner(overrides: Partial<InFlightCapture['owner']> = {}) {
  return {
    fingerprint: 'a'.repeat(64),
    revision: 1,
    ...overrides,
  }
}

const TEST_RECOVERY_KEY = await createCaptureRecoveryKey(
  'capture-store-test-token',
  makeOwner(),
)
const OTHER_RECOVERY_KEY = await createCaptureRecoveryKey(
  'different-capture-store-token',
  makeOwner(),
)

function createTestCaptureStore(
  area: SessionStorageArea,
  recoveryKey: CryptoKey = TEST_RECOVERY_KEY,
): CaptureStore {
  const store = createSessionCaptureStore(area)
  return {
    ...store,
    getSnapshot: () => store.getSnapshot(recoveryKey),
    setSnapshot: (snapshot) => store.setSnapshot(snapshot, recoveryKey),
    getInFlight: () => store.getInFlight(recoveryKey),
    setInFlight: (inFlight) => store.setInFlight(inFlight, recoveryKey),
  }
}

function makeInFlight(
  overrides: Partial<InFlightCapture> = {},
): InFlightCapture {
  return {
    owner: makeOwner(),
    jobId: 'job-1',
    url: 'https://example.com/x',
    title: '示例',
    note: '备注',
    attempt: 3,
    snapshot: makeSnapshot(),
    startedAt: 1_700_000_000_000,
    ...overrides,
  }
}

// ── 快照读写 ────────────────────────────────────────────────

describe('createSessionCaptureStore — 快照', () => {
  it('未写过快照时 getSnapshot 返回 idle', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    expect(await store.getSnapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
  })

  it('setSnapshot 写入后 getSnapshot 读回同一份', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    const snapshot = makeSnapshot({ stage: 'done' })
    await store.setSnapshot(snapshot)
    expect(await store.getSnapshot()).toEqual(snapshot)
  })

  it('快照写入 STORAGE_KEY_SNAPSHOT 键', async () => {
    const area = makeMemoryArea()
    const store = createTestCaptureStore(area)
    await store.setSnapshot(makeSnapshot())
    expect(area._data[STORAGE_KEY_SNAPSHOT]).toBeDefined()
  })

  it('带 URL/title 但缺 owner 的 legacy snapshot 返回 idle 并被隔离', async () => {
    const area = makeMemoryArea()
    const { owner: _owner, ...legacy } = makeSnapshot({ stage: 'done' })
    area._data[STORAGE_KEY_SNAPSHOT] = legacy
    const store = createTestCaptureStore(area)

    expect(await store.getSnapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
    expect(area._data[STORAGE_KEY_SNAPSHOT]).toBeUndefined()
  })

  it('ownerless failed snapshot with dynamic text is never persisted in plaintext', async () => {
    const area = makeMemoryArea()
    const store = createTestCaptureStore(area)

    await store.setSnapshot({
      stage: 'failed',
      url: '',
      title: '',
      errorKind: 'capture-unexpected',
      errorMessage: 'private-error-message',
      updatedAt: 1,
    })

    expect(area._data[STORAGE_KEY_SNAPSHOT]).toBeUndefined()
    await expect(store.getSnapshot()).resolves.toEqual(IDLE_CAPTURE_SNAPSHOT)
  })
})

// ── in-flight 读写 ──────────────────────────────────────────

describe('createSessionCaptureStore — in-flight', () => {
  it('未写过 in-flight 时 getInFlight 返回 null', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    expect(await store.getInFlight()).toBeNull()
  })

  it('setInFlight 写入后 getInFlight 读回同一份', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    const inFlight = makeInFlight()
    await store.setInFlight(inFlight)
    expect(await store.getInFlight()).toEqual(inFlight)
  })

  it('clearInFlight 后 getInFlight 返回 null', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    await store.setInFlight(makeInFlight())
    await store.clearInFlight()
    expect(await store.getInFlight()).toBeNull()
  })

  it('in-flight 写入 STORAGE_KEY_INFLIGHT 键', async () => {
    const area = makeMemoryArea()
    const store = createTestCaptureStore(area)
    await store.setInFlight(makeInFlight())
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeDefined()
  })

  it('缺少 installation owner 的 legacy in-flight fail closed', async () => {
    const area = makeMemoryArea()
    const { owner: _owner, ...legacy } = makeInFlight()
    legacy.snapshot = { ...legacy.snapshot, owner: undefined }
    area._data[STORAGE_KEY_INFLIGHT] = legacy
    const store = createTestCaptureStore(area)

    expect(await store.getInFlight()).toBeNull()
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
  })

  it('owner record rejects token, body, raw namespace, and owner disagreement', async () => {
    const area = makeMemoryArea()
    const store = createTestCaptureStore(area)
    const unsafe = makeInFlight()
    ;(unsafe.owner as unknown as Record<string, unknown>).token = 'secret'
    ;(unsafe.owner as unknown as Record<string, unknown>).body = {
      text: 'private body',
    }
    ;(unsafe.owner as unknown as Record<string, unknown>).namespace =
      'raw-namespace'
    await store.setInFlight(unsafe)
    expect(await store.getInFlight()).toBeNull()

    await store.setInFlight(
      makeInFlight({
        owner: makeOwner({ revision: 2 }),
        snapshot: makeSnapshot({ owner: makeOwner({ revision: 1 }) }),
      }),
    )
    expect(await store.getInFlight()).toBeNull()
  })

  it('round-trips an exact persisted site request for service-worker replay', async () => {
    const store = createTestCaptureStore(makeMemoryArea())
    const inFlight = makeInFlight({
      jobId: '',
      phase: 'submitting',
      destination: 'site',
      requestedKind: 'site',
      idempotencyKey: 'site-capture-key',
      source: {
        kind: 'browser_capture',
        url: 'https://example.com/x',
      },
      ingestRequest: {
        sources: [{ kind: 'browser_capture', url: 'https://example.com/x' }],
        destination: 'site',
      },
    })

    await store.setInFlight(inFlight)

    expect(await store.getInFlight()).toEqual(inFlight)
  })

  it('keeps sensitive submitting payload out of raw session storage while a new store recovers it exactly', async () => {
    const area = makeMemoryArea()
    const privateUrl =
      'https://private.example.test/account/alice?session=private-query'
    const privateTitle = 'Alice private draft title'
    const privateNote = 'alice-private-note'
    const privateBody = 'alice-private-capture-body'
    const privateNamespace = 'alice-raw-private-namespace'
    const privateToken = 'alice-private-access-token'
    const idempotencyKey = 'alice-private-idempotency-key'
    const source = {
      kind: 'browser_capture' as const,
      url: privateUrl,
      title: privateTitle,
      text: privateBody,
      metadata: { client_data_namespace: privateNamespace },
    }
    const inFlight = makeInFlight({
      jobId: '',
      phase: 'submitting',
      url: privateUrl,
      title: privateTitle,
      note: privateNote,
      idempotencyKey,
      source,
      ingestRequest: { sources: [source], destination: 'library' },
      snapshot: makeSnapshot({
        stage: 'capturing',
        url: privateUrl,
        title: privateTitle,
      }),
    })
    const recoveryKey = await createCaptureRecoveryKey(
      privateToken,
      inFlight.owner,
    )
    const firstWorkerStore = createTestCaptureStore(area, recoveryKey)

    await firstWorkerStore.setSnapshot(inFlight.snapshot)
    await firstWorkerStore.setInFlight(inFlight)

    expect(area._data[STORAGE_KEY_SNAPSHOT]).toMatchObject({
      format: 'webtag-capture-sealed-v1',
      kind: 'snapshot',
      owner: inFlight.owner,
      iv: expect.any(String),
      ciphertext: expect.any(String),
    })
    expect(area._data[STORAGE_KEY_INFLIGHT]).toMatchObject({
      format: 'webtag-capture-sealed-v1',
      kind: 'in-flight',
      owner: inFlight.owner,
      phase: 'submitting',
      attempt: inFlight.attempt,
      startedAt: inFlight.startedAt,
      iv: expect.any(String),
      ciphertext: expect.any(String),
    })
    for (const forbiddenField of [
      'url',
      'title',
      'note',
      'source',
      'ingestRequest',
      'idempotencyKey',
      'snapshot',
    ]) {
      expect(area._data[STORAGE_KEY_INFLIGHT]).not.toHaveProperty(
        forbiddenField,
      )
    }

    const rawStorage = JSON.stringify(area._data)
    for (const privateValue of [
      privateToken,
      privateUrl,
      privateTitle,
      privateNote,
      privateBody,
      privateNamespace,
      idempotencyKey,
    ]) {
      expect(rawStorage).not.toContain(privateValue)
    }

    const restartedWorkerStore = createTestCaptureStore(area, recoveryKey)
    await expect(restartedWorkerStore.getSnapshot()).resolves.toEqual(
      inFlight.snapshot,
    )
    await expect(restartedWorkerStore.getInFlight()).resolves.toEqual(inFlight)
  })

  it('fails closed and purges a sealed payload when the recovery key changes', async () => {
    const area = makeMemoryArea()
    const writer = createSessionCaptureStore(area)
    const inFlight = makeInFlight()
    await writer.setInFlight(inFlight, TEST_RECOVERY_KEY)

    const restartedWithAnotherSecret = createSessionCaptureStore(area)
    await expect(
      restartedWithAnotherSecret.getInFlight(OTHER_RECOVERY_KEY),
    ).resolves.toBeNull()
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
  })

  it('fails closed and purges an in-flight payload when authenticated metadata changes', async () => {
    const area = makeMemoryArea()
    const store = createSessionCaptureStore(area)
    const inFlight = makeInFlight()
    await store.setInFlight(inFlight, TEST_RECOVERY_KEY)
    const sealed = area._data[STORAGE_KEY_INFLIGHT] as Record<string, unknown>
    area._data[STORAGE_KEY_INFLIGHT] = {
      ...sealed,
      attempt: inFlight.attempt + 1,
    }

    await expect(store.getInFlight(TEST_RECOVERY_KEY)).resolves.toBeNull()
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
  })

  it('fails closed and purges an in-flight payload when ciphertext changes', async () => {
    const area = makeMemoryArea()
    const store = createSessionCaptureStore(area)
    await store.setInFlight(makeInFlight(), TEST_RECOVERY_KEY)
    const sealed = area._data[STORAGE_KEY_INFLIGHT] as Record<string, unknown>
    const ciphertext = sealed.ciphertext as string
    area._data[STORAGE_KEY_INFLIGHT] = {
      ...sealed,
      ciphertext: `${ciphertext.startsWith('A') ? 'B' : 'A'}${ciphertext.slice(1)}`,
    }

    await expect(store.getInFlight(TEST_RECOVERY_KEY)).resolves.toBeNull()
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
  })

  it('purges plaintext upgrade records without copying private values to storage or logs', async () => {
    const area = makeMemoryArea()
    const privateUrl = 'https://legacy-private.example.test/alice'
    const privateBody = 'legacy-private-body'
    const privateNamespace = 'legacy-private-namespace'
    area._data[STORAGE_KEY_SNAPSHOT] = makeSnapshot({
      url: privateUrl,
      title: privateBody,
    })
    area._data[STORAGE_KEY_INFLIGHT] = makeInFlight({
      url: privateUrl,
      title: privateBody,
      source: {
        kind: 'browser_capture',
        url: privateUrl,
        text: privateBody,
        metadata: { client_data_namespace: privateNamespace },
      },
    })
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const store = createTestCaptureStore(area)

    await expect(store.getSnapshotOwner()).resolves.toBeNull()
    await expect(store.getInFlightMetadata()).resolves.toBeNull()

    expect(area._data[STORAGE_KEY_SNAPSHOT]).toBeUndefined()
    expect(area._data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
    const remainingStorage = JSON.stringify(area._data)
    const logged = JSON.stringify(warnSpy.mock.calls)
    for (const privateValue of [privateUrl, privateBody, privateNamespace]) {
      expect(remainingStorage).not.toContain(privateValue)
      expect(logged).not.toContain(privateValue)
    }
    warnSpy.mockRestore()
  })
})

// ── 容错 ────────────────────────────────────────────────────

describe('createSessionCaptureStore — storage 抛错容错', () => {
  // 写失败不再静默吞掉而是 console.warn——逐用例 spy 掉，避免污染测试输出。
  let warnSpy: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})
  })

  afterEach(() => {
    warnSpy.mockRestore()
  })

  it('getSnapshot 抛错时回退到 idle 并 console.warn 留痕', async () => {
    const store = createTestCaptureStore(makeThrowingArea())
    expect(await store.getSnapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
    expect(warnSpy).toHaveBeenCalled()
  })

  it('getInFlight 抛错时回退到 null 并 console.warn 留痕', async () => {
    const store = createTestCaptureStore(makeThrowingArea())
    expect(await store.getInFlight()).toBeNull()
    expect(warnSpy).toHaveBeenCalled()
  })

  it('setSnapshot / setInFlight / clearInFlight 抛错时不抛出，但都 console.warn 留痕（不再静默吞掉）', async () => {
    const store = createTestCaptureStore(makeThrowingArea())
    await expect(store.setSnapshot(makeSnapshot())).resolves.toBeUndefined()
    await expect(store.setInFlight(makeInFlight())).resolves.toBeUndefined()
    await expect(store.clearInFlight()).resolves.toBeUndefined()
    // 三个写操作各失败一次 → 至少 3 次 warn。
    expect(warnSpy.mock.calls.length).toBeGreaterThanOrEqual(3)
  })

  it('encrypted replacement failure quarantines legacy plaintext snapshot and in-flight slots', async () => {
    const data: Record<string, unknown> = {
      [STORAGE_KEY_SNAPSHOT]: makeSnapshot({
        url: 'https://legacy-private.example.test/snapshot',
      }),
      [STORAGE_KEY_INFLIGHT]: makeInFlight({
        url: 'https://legacy-private.example.test/in-flight',
      }),
    }
    const area: SessionStorageArea = {
      get: async (keys) => {
        const result: Record<string, unknown> = {}
        for (const key of Array.isArray(keys) ? keys : [keys]) {
          if (key in data) result[key] = data[key]
        }
        return result
      },
      set: () => Promise.reject(new Error('set unavailable')),
      remove: async (keys) => {
        for (const key of Array.isArray(keys) ? keys : [keys]) delete data[key]
      },
    }
    const store = createSessionCaptureStore(area)

    await store.setSnapshot(makeSnapshot(), TEST_RECOVERY_KEY)
    await store.setInFlight(makeInFlight(), TEST_RECOVERY_KEY)

    expect(data[STORAGE_KEY_SNAPSHOT]).toBeUndefined()
    expect(data[STORAGE_KEY_INFLIGHT]).toBeUndefined()
  })

  it('setInFlight 失败的 warn 文案点出「跨 SW 续跑失效」语义', async () => {
    const store = createTestCaptureStore(makeThrowingArea())
    await store.setInFlight(makeInFlight())
    const messages = warnSpy.mock.calls.map((c) => String(c[0]))
    expect(messages.some((m) => m.includes('setInFlight'))).toBe(true)
  })

  it('clearInFlight 失败的 warn 文案点出「僵尸记录由陈旧判定自愈」语义', async () => {
    const store = createTestCaptureStore(makeThrowingArea())
    await store.clearInFlight()
    const messages = warnSpy.mock.calls.map((c) => String(c[0]))
    expect(messages.some((m) => m.includes('clearInFlight'))).toBe(true)
  })

  it('storage 返回非对象脏数据时 getSnapshot 回退 idle', async () => {
    const area: SessionStorageArea = {
      get: vi.fn(() =>
        Promise.resolve({ [STORAGE_KEY_SNAPSHOT]: 'not-an-object' }),
      ),
      set: vi.fn(() => Promise.resolve()),
      remove: vi.fn(() => Promise.resolve()),
    }
    const store = createTestCaptureStore(area)
    expect(await store.getSnapshot()).toEqual(IDLE_CAPTURE_SNAPSHOT)
  })
})

// ── 陈旧判定 ────────────────────────────────────────────────

describe('isInFlightStale — in-flight 记录陈旧判定', () => {
  /** 一个相对宽裕的采集墙钟预算（用于测试，足够大以单独控制陈旧条件）。 */
  const MAX_AGE = 60_000

  it('startedAt 在预算内的记录不陈旧', () => {
    const inFlight = makeInFlight({ startedAt: 1_000_000 })
    // now 距 startedAt 仅 10s，远小于 60s 预算。
    expect(isInFlightStale(inFlight, MAX_AGE, 1_010_000)).toBe(false)
  })

  it('startedAt 超出预算的记录陈旧', () => {
    const inFlight = makeInFlight({ startedAt: 1_000_000 })
    // now 距 startedAt 已 70s，超出 60s 预算 → 陈旧。
    expect(isInFlightStale(inFlight, MAX_AGE, 1_070_000)).toBe(true)
  })

  it('恰好等于预算边界时不陈旧（用 > 严格判定）', () => {
    const inFlight = makeInFlight({ startedAt: 1_000_000 })
    expect(isInFlightStale(inFlight, MAX_AGE, 1_000_000 + MAX_AGE)).toBe(false)
  })

  it('缺失 startedAt（老数据）的记录一律视为陈旧', () => {
    // 模拟旧版本写入、无 startedAt 字段的脏记录。
    const legacy = {
      jobId: 'job-1',
      url: 'https://example.com/x',
      title: '示例',
      note: '',
      attempt: 0,
      snapshot: makeSnapshot(),
    } as InFlightCapture
    expect(isInFlightStale(legacy, MAX_AGE, 9_999_999)).toBe(true)
  })

  it('非法 startedAt（0 / 负数）的记录视为陈旧', () => {
    expect(isInFlightStale(makeInFlight({ startedAt: 0 }), MAX_AGE, 1)).toBe(
      true,
    )
    expect(isInFlightStale(makeInFlight({ startedAt: -5 }), MAX_AGE, 1)).toBe(
      true,
    )
  })

  it('STALE_INFLIGHT_MARGIN_MS 导出为正数余量常量', () => {
    expect(STALE_INFLIGHT_MARGIN_MS).toBeGreaterThan(0)
  })
})
