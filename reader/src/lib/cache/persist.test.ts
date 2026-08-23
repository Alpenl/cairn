/**
 * PF4：磁盘持久化与首屏 hydrate 的行为守卫。
 */
import 'fake-indexeddb/auto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ok } from '@webtag/api'
import { resourceStore } from './store'
import {
  CACHE_SCHEMA_VERSION,
  PAYLOAD_STORE_NAME,
  createPersistedRecord,
  idbAdvanceInvalidation,
  idbClear,
  idbGetAll,
  idbGetMetadata,
  idbPut,
  idbPutWithinQuota,
  resetDatabaseHandle,
  type PersistedRecord,
} from './idb'
import { invalidateLibrary } from './invalidate'
import { contentCacheKey, translationsKey } from './keys'
import {
  MAX_PERSISTED_BYTES,
  deletePersistedPrefix,
  getCacheMaintenanceStats,
  hydrateFromDisk,
  startPersistence,
} from './persist'
import { HYDRATE_TIMEOUT_MS, bootstrapCache, stopCachePersistence } from './bootstrap'
import { readerIdentity } from '../identity'
import { cacheStorageQueue } from './io-queue'

const LINKS_KEY = 'GET /api/links?limit=30'
const TEST_PHYSICAL_NAMESPACE = 'physical-test'

function installIdentity(server: string, physical: string) {
  const lease = readerIdentity.install({
    serverClientDataNamespace: server,
    physicalNamespace: physical,
  })
  resourceStore.activateIdentity(lease)
  return lease
}

beforeEach(async () => {
  resetDatabaseHandle()
  await idbClear()
  resourceStore.deactivateIdentity()
  installIdentity('server-test', TEST_PHYSICAL_NAMESPACE)
})

afterEach(() => {
  stopCachePersistence()
  readerIdentity.clear()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

/** 等待防抖落盘完成。 */
async function settle(ms = 600): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms))
}

describe('PF4 落盘与恢复', () => {
  it('deletes only a matching prefix in the active physical namespace', async () => {
    const leaseA = installIdentity('server-A', 'physical-A')
    await idbPut(createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-links' },
      updatedAt: 1,
      size: 10,
    }))
    await idbPut(createPersistedRecord('physical-A', 'GET /api/tags', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-tags' },
      updatedAt: 2,
      size: 10,
    }))
    await idbPut(createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-links' },
      updatedAt: 3,
      size: 10,
    }))

    await deletePersistedPrefix('GET /api/links', leaseA)

    expect(
      (await idbGetAll())
        .map((record) => `${record.namespace}:${record.logicalKey}`)
        .sort(),
    ).toEqual(['physical-A:GET /api/tags', 'physical-B:GET /api/links?owner=B'])
  })

  it('queues an A prefix delete and discards it after activating B', async () => {
    const leaseA = installIdentity('server-A', 'physical-A')
    await idbPut(createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-links' },
      updatedAt: 1,
      size: 10,
    }))
    await idbPut(createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-links' },
      updatedAt: 2,
      size: 10,
    }))

    let releaseBarrier!: () => void
    const barrier = new Promise<void>((resolve) => {
      releaseBarrier = resolve
    })
    const blocker = cacheStorageQueue.enqueue(leaseA, 'test prefix delete barrier', async () =>
      barrier,
    )
    await Promise.resolve()

    let deletionSettled = false
    const deletion = deletePersistedPrefix('GET /api/links', leaseA).then(() => {
      deletionSettled = true
    })
    await settle(30)
    const settledBeforeSwitch = deletionSettled

    installIdentity('server-B', 'physical-B')
    releaseBarrier()
    await blocker
    await deletion

    expect(settledBeforeSwitch).toBe(false)
    expect(
      (await idbGetAll())
        .map((record) => `${record.namespace}:${record.logicalKey}`)
        .sort(),
    ).toEqual([
      'physical-A:GET /api/links?owner=A',
      'physical-B:GET /api/links?owner=B',
    ])
  })

  it('removes disk-only rows through the production library invalidation path', async () => {
    const leaseA = installIdentity('server-A', 'physical-A')
    await idbPut(createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-links' },
      updatedAt: 1,
      size: 10,
    }))
    await idbPut(createPersistedRecord('physical-A', 'GET /api/subscriptions', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-subscriptions' },
      updatedAt: 2,
      size: 10,
    }))
    await idbPut(createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-links' },
      updatedAt: 3,
      size: 10,
    }))

    const stop = startPersistence({ lease: leaseA })
    invalidateLibrary()
    await cacheStorageQueue.enqueue(leaseA, 'await production invalidation', () => undefined)
    stop()

    expect(
      (await idbGetAll())
        .map((record) => `${record.namespace}:${record.logicalKey}`)
        .sort(),
    ).toEqual([
      'physical-A:GET /api/subscriptions',
      'physical-B:GET /api/links?owner=B',
    ])
  })

  it('drops a queued 500ms A flush after switching to B', async () => {
    const leaseA = installIdentity('server-A', 'physical-A')
    let releaseBarrier!: () => void
    const barrier = new Promise<void>((resolve) => {
      releaseBarrier = resolve
    })
    const blocker = cacheStorageQueue.enqueue(leaseA, 'test flush barrier', async () => barrier)
    await Promise.resolve()

    const stop = startPersistence({ lease: leaseA })
    resourceStore.set(LINKS_KEY, { items: [{ id: 'A-late' }] })
    await settle(520)

    installIdentity('server-B', 'physical-B')
    stop()
    resourceStore.clear()
    releaseBarrier()
    await blocker
    await settle(30)

    expect((await idbGetAll()).filter((record) => record.namespace === 'physical-A')).toEqual([])
  })

  it('finishes an in-flight write batch when persistence stops after the first put', async () => {
    class StrictBroadcastChannel extends EventTarget {
      readonly name: string
      onmessage: ((this: BroadcastChannel, event: MessageEvent) => unknown) | null = null
      onmessageerror: ((this: BroadcastChannel, event: MessageEvent) => unknown) | null = null
      private closed = false

      constructor(name: string) {
        super()
        this.name = name
      }

      postMessage(): void {
        if (this.closed) throw new DOMException('BroadcastChannel is closed', 'InvalidStateError')
      }

      close(): void {
        this.closed = true
      }
    }
    vi.stubGlobal('BroadcastChannel', StrictBroadcastChannel)

    const lease = readerIdentity.activeLease!
    const stop = startPersistence({ lease, debounceMs: 10 })
    await cacheStorageQueue.enqueue(lease, 'await persistence bootstrap', () => undefined)
    const originalPut = IDBObjectStore.prototype.put
    let stoppedDuringPut = false
    const putSpy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (
      this: IDBObjectStore,
      value: unknown,
      key?: IDBValidKey,
    ) {
      const request = originalPut.call(this, value, key)
      if (this.name === PAYLOAD_STORE_NAME && !stoppedDuringPut) {
        stoppedDuringPut = true
        stop()
      }
      return request
    })

    try {
      resourceStore.set('GET /api/links?batch=A', { batch: 'A' })
      resourceStore.set('GET /api/links?batch=B', { batch: 'B' })
      await settle(30)
      await cacheStorageQueue.enqueue(lease, 'await stopped persistence batch', () => undefined)
    } finally {
      stop()
      putSpy.mockRestore()
      vi.unstubAllGlobals()
    }

    expect(stoppedDuringPut).toBe(true)
    expect((await idbGetAll()).map((record) => record.logicalKey).sort()).toEqual([
      'GET /api/links?batch=A',
      'GET /api/links?batch=B',
    ])
  })

  it('hydrates only the current physical namespace and preserves the previous partition', async () => {
    installIdentity('server-A', 'physical-A')
    const stop = startPersistence({ debounceMs: 10 })
    resourceStore.set(LINKS_KEY, { items: [{ id: 'A-only' }] })
    await settle(80)
    stop()
    resourceStore.clear()

    installIdentity('server-B', 'physical-B')
    expect(await hydrateFromDisk()).toBe(0)
    expect(resourceStore.has(LINKS_KEY)).toBe(false)

    installIdentity('server-A', 'physical-A')
    expect(await hydrateFromDisk()).toBe(1)
    expect(resourceStore.peek<{ items: { id: string }[] }>(LINKS_KEY).data?.items[0].id).toBe(
      'A-only',
    )
  })

  it('does not delete another physical namespace stale records during hydrate', async () => {
    const staleA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION + 1,
      data: { owner: 'A' },
      updatedAt: 1,
      size: 10,
    })
    await idbPut(staleA)
    const leaseB = installIdentity('server-B', 'physical-B')

    expect(await hydrateFromDisk(undefined, leaseB)).toBe(0)
    expect(await idbGetAll()).toEqual([staleA])
  })

  it('参与持久化的键会被写进磁盘，并能被 hydrate 回内存', async () => {
    const stop = startPersistence({ debounceMs: 10 })
    await resourceStore.fetch(LINKS_KEY, async () => ok({ items: [{ id: 'L1' }], total: 1 }))
    await settle(80)
    stop()

    const records = await idbGetAll()
    expect(records.map((record) => record.logicalKey)).toContain(LINKS_KEY)

    // 模拟刷新：内存清空，只剩磁盘。
    resourceStore.clear()
    expect(resourceStore.has(LINKS_KEY)).toBe(false)

    const restored = await hydrateFromDisk()
    expect(restored).toBeGreaterThan(0)
    expect(resourceStore.has(LINKS_KEY)).toBe(true)
    expect(resourceStore.peek<{ items: { id: string }[] }>(LINKS_KEY).data?.items[0].id).toBe('L1')
  })

  // 已保存原文是自己一个键命名空间（不在 `GET /api/links` 之下，见 keys.ts）。
  // 换命名空间的时候很容易忘了同步落盘白名单——那样正文就只剩内存，F5 之后
  // 展开原文又要重新下载一遍，正是这次要修的症状换个触发方式再来一遍。
  it('已保存原文同样落盘，刷新后仍在', async () => {
    const contentKey = contentCacheKey('LC', 3)
    const stop = startPersistence({ debounceMs: 10 })
    await resourceStore.fetch(contentKey, async () => ok({ link_id: 'LC', content: '正文' }))
    await settle(80)
    stop()

    resourceStore.clear()
    await hydrateFromDisk()
    expect(resourceStore.peek<{ content: string }>(contentKey).data?.content).toBe('正文')
  })

  // 译文同理，而且它是**后搬进独立命名空间**的那一个：此前靠 `GET /api/links`
  // 前缀顺带落盘，搬走之后必须在白名单里显式列出。漏掉这条，译文会从「刷新后
  // 仍在」静默退化成纯内存缓存——退化得毫无症状，只是每次刷新都重取一遍。
  it('译文同样落盘，刷新后仍在', async () => {
    const key = translationsKey('LT')
    const stop = startPersistence({ debounceMs: 10 })
    await resourceStore.fetch(key, async () =>
      ok({
        current_content_revision: 7,
        current_summary_source_hash: null,
        items: [{ id: 'T1' }],
      }),
    )
    await settle(80)
    stop()

    resourceStore.clear()
    await hydrateFromDisk()
    expect(
      resourceStore.peek<{
        current_content_revision: number
        current_summary_source_hash: string | null
        items: { id: string }[]
      }>(key).data,
    ).toEqual({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [{ id: 'T1' }],
    })
  })

  it('不在白名单里的键不落盘（搜索、翻页这类一次性请求）', async () => {
    const stop = startPersistence({ debounceMs: 10 })
    await resourceStore.fetch('GET /api/search?q=foo', async () => ok({ items: [] }))
    await settle(80)
    stop()

    const records = await idbGetAll()
    expect(records.map((record) => record.logicalKey)).not.toContain('GET /api/search?q=foo')
  })

  it('失效之后磁盘上的那条也被删掉，不会在下次启动复活', async () => {
    const stop = startPersistence({ debounceMs: 10 })
    await resourceStore.fetch(LINKS_KEY, async () => ok({ items: [{ id: 'L1' }] }))
    await settle(80)
    expect((await idbGetAll()).some((record) => record.logicalKey === LINKS_KEY)).toBe(true)

    resourceStore.invalidate(LINKS_KEY)
    await settle(80)
    stop()

    expect((await idbGetAll()).some((record) => record.logicalKey === LINKS_KEY)).toBe(false)
  })

  it('clears a schema mismatch from the active namespace without hydrating it', async () => {
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, LINKS_KEY, {
      schema: CACHE_SCHEMA_VERSION + 1,
      data: { shape: 'from-the-future' },
      updatedAt: Date.now(),
      size: 10,
    }))

    const restored = await hydrateFromDisk()

    expect(restored).toBe(0)
    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(await idbGetAll()).toHaveLength(0)
  })

  it('deletes a schema-v4 translation payload instead of restoring its revisionless rows', async () => {
    const key = translationsKey('legacy-v4')
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, key, {
      schema: 4,
      data: {
        items: [{ id: 'T-legacy', scope: 'full', status: 'succeeded' }],
      },
      updatedAt: Date.now(),
      size: 10,
    }))

    const restored = await hydrateFromDisk()

    expect(restored).toBe(0)
    expect(resourceStore.has(key)).toBe(false)
    expect(await idbGetAll()).toHaveLength(0)
  })

  it('deletes a real pre-RF2B schema-v3 record without namespace metadata', async () => {
    await idbPut({
      key: LINKS_KEY,
      schema: 3,
      data: { shape: 'legacy-v3' },
      updatedAt: Date.now(),
      size: 10,
    } as PersistedRecord)

    const restored = await hydrateFromDisk()

    expect(restored).toBe(0)
    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(await idbGetAll()).toHaveLength(0)
  })

  it('does not hydrate a payload older than the durable invalidation generation', async () => {
    const lease = readerIdentity.activeLease!
    await cacheStorageQueue.enqueue(lease, 'seed durable invalidation', async (operation) => {
      await idbAdvanceInvalidation('GET /api/links', operation)
    })
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, LINKS_KEY, {
      schema: CACHE_SCHEMA_VERSION,
      data: { items: [{ id: 'stale' }] },
      updatedAt: 1,
      size: 10,
      generation: 0,
    }))

    expect(await hydrateFromDisk()).toBe(0)
    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(await idbGetAll()).toEqual([])
    expect(resourceStore.peek(LINKS_KEY).desiredGeneration).toBe(1)
  })

  it('reconciles a missed cross-tab invalidation from disk on visibilitychange', async () => {
    const lease = readerIdentity.activeLease!
    resourceStore.set(LINKS_KEY, { items: [{ id: 'cached' }] })
    const stop = startPersistence({ lease, debounceMs: 10 })
    await settle(40)
    expect(resourceStore.has(LINKS_KEY)).toBe(true)

    await cacheStorageQueue.enqueue(lease, 'simulate another tab tombstone', async (operation) => {
      await idbAdvanceInvalidation('GET /api/links', operation)
    })
    document.dispatchEvent(new Event('visibilitychange'))
    await cacheStorageQueue.enqueue(lease, 'await visibility reconciliation', () => undefined)

    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(resourceStore.peek(LINKS_KEY).desiredGeneration).toBe(1)
    stop()
  })

  it('does not let a high resource retry generation bypass a missed durable tombstone', async () => {
    const lease = readerIdentity.activeLease!
    const stop = startPersistence({ lease, debounceMs: 10 })
    await cacheStorageQueue.enqueue(lease, 'await initial invalidation reconciliation', () => undefined)

    await resourceStore.fetch(LINKS_KEY, async () => ok({ version: 0 }))
    for (let version = 1; version <= 3; version += 1) {
      await resourceStore.fetch(LINKS_KEY, async () => ok({ version }), { force: true })
    }
    await settle(40)
    expect(resourceStore.peek(LINKS_KEY).settledGeneration).toBe(3)

    // Another tab commits a tombstone while this page misses both the
    // BroadcastChannel wakeup and visibility reconciliation.
    await cacheStorageQueue.enqueue(lease, 'simulate missed durable tombstone', async (operation) => {
      await idbAdvanceInvalidation('GET /api/links', operation)
    })

    // A stale local continuation dirties the key after the tombstone. Its
    // resource retry generation is high, but its durable journal position is
    // still old and therefore must not allow the payload to reappear.
    resourceStore.set(LINKS_KEY, { version: 'stale-local-continuation' })
    await settle(40)
    stop()

    expect((await idbGetAll()).filter((record) => record.logicalKey === LINKS_KEY)).toEqual([])
  })
})

describe('PF4 容量治理', () => {
  it('restarts from the true metadata byte total and evicts before growing past quota', async () => {
    const almostFull = MAX_PERSISTED_BYTES - 8
    const oldKey = 'GET /api/links?old-near-quota'
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, oldKey, {
      schema: CACHE_SCHEMA_VERSION,
      data: { old: true },
      updatedAt: 1,
      size: almostFull,
    }))
    const lease = readerIdentity.activeLease!
    const stop = startPersistence({ lease, debounceMs: 10 })
    await cacheStorageQueue.enqueue(lease, 'await metadata bootstrap', () => undefined)
    expect(getCacheMaintenanceStats().knownBytes).toBe(almostFull)

    const storageOrder: string[] = []
    const originalDelete = IDBObjectStore.prototype.delete
    const originalPut = IDBObjectStore.prototype.put
    const deleteSpy = vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
      this: IDBObjectStore,
      query: IDBValidKey | IDBKeyRange,
    ) {
      if (this.name === PAYLOAD_STORE_NAME && query === createPersistedRecord(
        TEST_PHYSICAL_NAMESPACE,
        oldKey,
        { schema: CACHE_SCHEMA_VERSION, data: null, updatedAt: 0, size: 0 },
      ).key) {
        storageOrder.push('delete-old')
      }
      return originalDelete.call(this, query)
    })
    const putSpy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (
      this: IDBObjectStore,
      value: unknown,
      key?: IDBValidKey,
    ) {
      if (
        this.name === PAYLOAD_STORE_NAME &&
        value &&
        typeof value === 'object' &&
        (value as { key?: unknown }).key === createPersistedRecord(
          TEST_PHYSICAL_NAMESPACE,
          LINKS_KEY,
          { schema: CACHE_SCHEMA_VERSION, data: null, updatedAt: 0, size: 0 },
        ).key
      ) {
        storageOrder.push('put-new')
      }
      return originalPut.call(this, value, key)
    })

    resourceStore.set(LINKS_KEY, { items: [{ id: 'new-write' }] })
    await settle(80)
    stop()
    deleteSpy.mockRestore()
    putSpy.mockRestore()

    const metadata = await idbGetMetadata()
    expect(metadata.reduce((sum, record) => sum + record.size, 0)).toBeLessThanOrEqual(
      MAX_PERSISTED_BYTES,
    )
    expect(metadata.map((record) => record.logicalKey)).toContain(LINKS_KEY)
    expect(metadata.map((record) => record.logicalKey)).not.toContain(oldKey)
    expect(getCacheMaintenanceStats().knownBytes).toBe(
      metadata.reduce((sum, record) => sum + record.size, 0),
    )
    expect(storageOrder).toEqual(['delete-old', 'put-new'])
  })

  it('scans metadata for 1 MB and 50 MB payloads without reading either payload store value', async () => {
    const oneMiB = 'a'.repeat(1024 * 1024)
    const fiftyMiB = 'b'.repeat(50 * 1024 * 1024)
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, 'GET /api/links?one-mib', {
      schema: CACHE_SCHEMA_VERSION,
      data: oneMiB,
      updatedAt: 1,
      size: oneMiB.length,
    }))
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, 'GET /api/links?fifty-mib', {
      schema: CACHE_SCHEMA_VERSION,
      data: fiftyMiB,
      updatedAt: 2,
      size: fiftyMiB.length,
    }))
    const get = vi.spyOn(IDBObjectStore.prototype, 'get')
    const getAll = vi.spyOn(IDBObjectStore.prototype, 'getAll')

    const metadata = await idbGetMetadata()

    expect(metadata).toHaveLength(2)
    expect(
      get.mock.contexts.filter(
        (store) => (store as IDBObjectStore).name === PAYLOAD_STORE_NAME,
      ),
    ).toHaveLength(0)
    expect(
      getAll.mock.contexts.filter(
        (store) => (store as IDBObjectStore).name === PAYLOAD_STORE_NAME,
      ),
    ).toHaveLength(0)
  })

  it('discards an A quota admission that resumes after activating B', async () => {
    const bulk = Math.floor(MAX_PERSISTED_BYTES / 2) + 1
    const leaseA = installIdentity('server-A', 'physical-A')
    await idbPut(createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 2,
      size: bulk,
    }))
    await idbPut(createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B' },
      updatedAt: 1,
      size: bulk,
    }))

    let releaseBarrier!: () => void
    const barrier = new Promise<void>((resolve) => {
      releaseBarrier = resolve
    })
    const incoming = createPersistedRecord('physical-A', 'GET /api/links?owner=A-new', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-new' },
      updatedAt: 3,
      size: bulk,
    })
    const admission = cacheStorageQueue.enqueue(leaseA, 'test quota barrier', async (operation) => {
      await barrier
      return idbPutWithinQuota(incoming, MAX_PERSISTED_BYTES, operation)
    })
    await Promise.resolve()

    installIdentity('server-B', 'physical-B')
    releaseBarrier()
    expect(await admission).toBeNull()

    expect(
      (await idbGetAll())
        .map((record) => `${record.namespace}:${record.logicalKey}`)
        .sort(),
    ).toEqual([
      'physical-A:GET /api/links?owner=A',
      'physical-B:GET /api/links?owner=B',
    ])
  })

  it('accounts for every cache namespace and evicts globally without reading payload bodies', async () => {
    const bulk = Math.floor(MAX_PERSISTED_BYTES / 2) + 1
    const recordA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 1,
      size: bulk,
    })
    const oldestB = createPersistedRecord('physical-B', 'GET /api/links?owner=B-old', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-old' },
      updatedAt: 2,
      size: bulk,
    })
    const newestB = createPersistedRecord('physical-B', 'GET /api/links?owner=B-new', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-new' },
      updatedAt: 3,
      size: bulk,
    })
    await idbPut(recordA)
    await idbPut(oldestB)
    const leaseB = installIdentity('server-B', 'physical-B')

    const payloadGets = vi.spyOn(IDBObjectStore.prototype, 'get')
    const getAll = vi.spyOn(IDBObjectStore.prototype, 'getAll')
    const result = await cacheStorageQueue.enqueue(
      leaseB,
      'test global quota admission',
      (operation) => idbPutWithinQuota(newestB, MAX_PERSISTED_BYTES, operation),
    )

    expect(
      getAll.mock.contexts.filter(
        (store) => (store as IDBObjectStore).name === PAYLOAD_STORE_NAME,
      ),
    ).toHaveLength(0)
    expect(
      payloadGets.mock.contexts.filter(
        (store) => (store as IDBObjectStore).name === PAYLOAD_STORE_NAME,
      ),
    ).toHaveLength(0)
    getAll.mockRestore()
    payloadGets.mockRestore()
    expect(result).toMatchObject({
      stored: true,
      totalBytes: bulk,
    })
    expect((await idbGetAll()).map((record) => record.key).sort()).toEqual(
      [newestB.key],
    )
  })
})

describe('PF4 启动引导', () => {
  it('serializes an A hydrate commit and discards it after activating B', async () => {
    const leaseA = installIdentity('server-A', 'physical-A')
    await idbPut(createPersistedRecord('physical-A', LINKS_KEY, {
      schema: CACHE_SCHEMA_VERSION,
      data: { items: [{ id: 'A-on-disk' }] },
      updatedAt: 1,
      size: 20,
    }))
    resourceStore.clear()

    let releaseBarrier!: () => void
    const barrier = new Promise<void>((resolve) => {
      releaseBarrier = resolve
    })
    const blocker = cacheStorageQueue.enqueue(leaseA, 'test hydrate barrier', async () => barrier)
    await Promise.resolve()

    let hydrationSettled = false
    const hydration = hydrateFromDisk(undefined, leaseA).then((restored) => {
      hydrationSettled = true
      return restored
    })
    await settle(30)

    expect(hydrationSettled).toBe(false)
    expect(resourceStore.has(LINKS_KEY)).toBe(false)

    installIdentity('server-B', 'physical-B')
    releaseBarrier()
    await blocker

    expect(await hydration).toBe(0)
    expect(resourceStore.activePhysicalNamespace).toBe('physical-B')
    expect(resourceStore.has(LINKS_KEY)).toBe(false)
  })

  it('hydrate 超时即放弃，不把首屏卡住', async () => {
    // 让 open 永远不回调，模拟 IndexedDB 被阻塞。
    vi.spyOn(indexedDB, 'open').mockImplementation(() => {
      return { onsuccess: null, onerror: null, onupgradeneeded: null, onblocked: null } as unknown as IDBOpenDBRequest
    })
    resetDatabaseHandle()

    const started = Date.now()
    const result = await bootstrapCache()
    const elapsed = Date.now() - started

    expect(result.restored).toBe(0)
    // 必须断言**是 bootstrap 的 200ms 超时**在兜底，而不是 idb 自己那个
    // 1 秒 open 超时。此前这里写的是 `< HYDRATE_TIMEOUT_MS + 1500`（1700ms），
    // 于是把整个 Promise.race 删掉、退回裸 await 也照样绿——idb 的 1 秒会
    // 兜住，1000 < 1700。留出 150ms 调度余量，但绝不能跨过 idb 那道 1 秒线。
    expect(elapsed).toBeLessThan(HYDRATE_TIMEOUT_MS + 150)
    expect(result.timedOut).toBe(true)

    vi.mocked(indexedDB.open).mockRestore()
    resetDatabaseHandle()
  })

  it('超时之后迟到的磁盘快照不得盖掉已拿到的新数据', async () => {
    // 磁盘上有一份旧数据。
    await idbPut(createPersistedRecord(TEST_PHYSICAL_NAMESPACE, LINKS_KEY, {
      schema: CACHE_SCHEMA_VERSION,
      data: { items: [{ id: 'stale' }] },
      updatedAt: 1,
      size: 20,
    }))
    resetDatabaseHandle()

    // 模拟「超时之后应用已挂载并从网络拿到了新数据」。
    resourceStore.set(LINKS_KEY, { items: [{ id: 'fresh' }] })

    const abandoned = { value: true } // 调用方已放弃等待
    const restored = await hydrateFromDisk(abandoned)

    expect(restored).toBe(0)
    expect(
      resourceStore.peek<{ items: { id: string }[] }>(LINKS_KEY).data?.items[0].id,
    ).toBe('fresh')
  })

  it('IndexedDB 不可用时应用照常工作，只是退化为纯内存缓存', async () => {
    vi.spyOn(indexedDB, 'open').mockImplementation(() => {
      throw new Error('storage disabled')
    })
    resetDatabaseHandle()

    const result = await bootstrapCache()
    expect(result.restored).toBe(0)

    // 内存缓存仍然可用。
    await resourceStore.fetch(LINKS_KEY, async () => ok({ items: [{ id: 'L1' }] }))
    expect(resourceStore.has(LINKS_KEY)).toBe(true)

    vi.mocked(indexedDB.open).mockRestore()
    resetDatabaseHandle()
  })
})
