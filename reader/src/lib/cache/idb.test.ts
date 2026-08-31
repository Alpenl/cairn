import 'fake-indexeddb/auto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { IdentityAuthority } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  CACHE_SCHEMA_VERSION,
  META_STORE_NAME,
  PAYLOAD_STORE_NAME,
  createPersistedRecord,
  idbAdvanceInvalidation,
  idbClear,
  idbDelete,
  idbDeletePrefix,
  idbGetAll,
  idbGetInvalidations,
  idbGetMetadata,
  idbPut,
  idbPutWithinQuota,
  idbRepairOrphans,
  resetDatabaseHandle,
} from './idb'
import { NamespaceStorageQueue } from './io-queue'

async function rawCacheTransaction(
  storeNames: string[],
  work: (transaction: IDBTransaction) => void,
): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.open(ownedDatabaseName('cacheDatabase'))
    request.onerror = () => reject(request.error)
    request.onsuccess = () => {
      const database = request.result
      const transaction = database.transaction(storeNames, 'readwrite')
      work(transaction)
      transaction.oncomplete = () => {
        database.close()
        resolve()
      }
      transaction.onerror = () => {
        database.close()
        reject(transaction.error)
      }
      transaction.onabort = () => {
        database.close()
        reject(transaction.error)
      }
    }
  })
}

beforeEach(async () => {
  resetDatabaseHandle()
  await idbClear()
})

afterEach(() => {
  vi.restoreAllMocks()
  resetDatabaseHandle()
})

describe('namespace-aware IndexedDB transactions', () => {
  it('reads only the active physical namespace for an owned operation', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const recordA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 1,
      size: 10,
    })
    const recordB = createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B' },
      updatedAt: 2,
      size: 10,
    })
    await idbPut(recordA)
    await idbPut(recordB)
    let records = [] as typeof recordB[]
    const getAll = vi.spyOn(IDBObjectStore.prototype, 'getAll')

    await queue.enqueue(leaseB, 'B reads its physical range', async (operation) => {
      records = await idbGetAll(operation)
    })

    expect(records).toEqual([recordB])
    expect(getAll.mock.calls[0]?.[0]).toBeInstanceOf(IDBKeyRange)
  })

  it('does not put a record for another physical namespace', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const recordA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 1,
      size: 10,
    })

    await queue.enqueue(leaseB, 'B attempts to write A', async (operation) => {
      await idbPut(recordA, operation)
    })

    expect(await idbGetAll()).toEqual([])
  })

  it('does not delete a record owned by another physical namespace', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const recordA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 1,
      size: 10,
    })
    await idbPut(recordA)
    const get = vi.spyOn(IDBObjectStore.prototype, 'get')

    await queue.enqueue(leaseB, 'B attempts to delete A', async (operation) => {
      await idbDelete(recordA.key, operation)
    })

    expect(get).not.toHaveBeenCalled()
    get.mockRestore()
    expect(await idbGetAll()).toEqual([recordA])
  })

  it('does not prefix-delete another physical namespace', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const recordA = createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A' },
      updatedAt: 1,
      size: 10,
    })
    const recordB = createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B' },
      updatedAt: 2,
      size: 10,
    })
    await idbPut(recordA)
    await idbPut(recordB)

    await queue.enqueue(leaseB, 'B prefix-deletes its own namespace', async (operation) => {
      await idbDeletePrefix('GET /api/links', operation)
    })

    expect(await idbGetAll()).toEqual([recordA])
  })

  it('aborts a started write when its identity lease is revoked', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const originalTransaction = IDBDatabase.prototype.transaction
    let switched = false
    vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementation(function (
      this: IDBDatabase,
      storeNames: string | Iterable<string>,
      mode?: IDBTransactionMode,
      options?: IDBTransactionOptions,
    ) {
      const transaction = originalTransaction.call(this, storeNames, mode, options)
      if (mode === 'readwrite' && !switched) {
        switched = true
        queueMicrotask(() => {
          authority.install({
            serverClientDataNamespace: 'server-B',
            physicalNamespace: 'physical-B',
          })
        })
      }
      return transaction
    })

    await queue.enqueue(leaseA, 'A started write', async (operation) => {
      const write = operation.commit(() =>
        idbPut(
          createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
            schema: CACHE_SCHEMA_VERSION,
            data: { owner: 'A' },
            updatedAt: 1,
            size: 10,
          }),
          operation,
        ),
      )
      if (write) await write
    })

    expect(switched).toBe(true)
    expect(await idbGetAll()).toEqual([])
  })

  it('atomically advances a durable generation and deletes the matching payload range', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const matching = createPersistedRecord('physical-A', 'GET /api/links?page=1', {
      schema: CACHE_SCHEMA_VERSION,
      data: { old: true },
      updatedAt: 1,
      size: 10,
    })
    const other = createPersistedRecord('physical-A', 'GET /api/tags', {
      schema: CACHE_SCHEMA_VERSION,
      data: { tag: true },
      updatedAt: 1,
      size: 10,
    })
    await idbPut(matching)
    await idbPut(other)

    let generation: number | null = null
    let invalidations: Awaited<ReturnType<typeof idbGetInvalidations>> = []
    await queue.enqueue(lease, 'advance invalidation', async (operation) => {
      generation = await idbAdvanceInvalidation('GET /api/links', operation)
      invalidations = await idbGetInvalidations(operation)
    })

    expect(generation).toBe(1)
    expect(invalidations).toEqual([
      expect.objectContaining({ logicalPrefix: 'GET /api/links', generation: 1 }),
    ])
    expect(await idbGetAll()).toEqual([other])
  })

  it('rejects an old persist after a tombstone but accepts the revalidated generation', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const logicalKey = 'GET /api/links?page=1'

    await queue.enqueue(lease, 'create tombstone', async (operation) => {
      await idbAdvanceInvalidation('GET /api/links', operation)
      await idbPut(createPersistedRecord('physical-A', logicalKey, {
        schema: CACHE_SCHEMA_VERSION,
        data: { generation: 0 },
        updatedAt: 1,
        size: 10,
        generation: 0,
      }), operation)
      await idbPut(createPersistedRecord('physical-A', logicalKey, {
        schema: CACHE_SCHEMA_VERSION,
        data: { generation: 1 },
        updatedAt: 2,
        size: 10,
        generation: 1,
      }), operation)
    })

    expect(await idbGetAll()).toEqual([
      expect.objectContaining({ data: { generation: 1 }, generation: 1 }),
    ])
  })
})

describe('payload/metadata atomicity and repair', () => {
  it('aborts the whole put when either payload or metadata write fails', async () => {
    const record = createPersistedRecord('physical-A', 'GET /api/links?atomic-put', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'private' },
      updatedAt: 1,
      size: 10,
    })
    const originalPut = IDBObjectStore.prototype.put

    for (const failingStore of [PAYLOAD_STORE_NAME, META_STORE_NAME]) {
      await idbClear()
      IDBObjectStore.prototype.put = new Proxy(originalPut, {
        apply(target, thisArgument, argumentsList) {
          if ((thisArgument as IDBObjectStore).name === failingStore) {
            throw new Error(`injected ${failingStore} put failure`)
          }
          return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<IDBValidKey>
        },
      })
      try {
        expect(await idbPut(record)).toBe(false)
      } finally {
        IDBObjectStore.prototype.put = originalPut
      }
      expect(await idbGetAll()).toEqual([])
      expect(await idbGetMetadata()).toEqual([])
    }
  })

  it('aborts both sides when metadata deletion fails after payload deletion was queued', async () => {
    const record = createPersistedRecord('physical-A', 'GET /api/links?atomic-delete', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'private' },
      updatedAt: 1,
      size: 10,
    })
    await idbPut(record)
    const originalDelete = IDBObjectStore.prototype.delete
    IDBObjectStore.prototype.delete = new Proxy(originalDelete, {
      apply(target, thisArgument, argumentsList) {
        if ((thisArgument as IDBObjectStore).name === META_STORE_NAME) {
          throw new Error('injected metadata delete failure')
        }
        return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<undefined>
      },
    })
    try {
      expect(await idbDelete(record.key)).toBe(false)
    } finally {
      IDBObjectStore.prototype.delete = originalDelete
    }

    expect(await idbGetAll()).toEqual([record])
    expect(await idbGetMetadata()).toHaveLength(1)
  })

  it('rolls back quota eviction when either side of the incoming put fails', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const oldRecord = createPersistedRecord('physical-A', 'GET /api/links?quota-old', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'old' },
      updatedAt: 1,
      size: 30,
    })
    const newRecord = createPersistedRecord('physical-A', 'GET /api/links?quota-new', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'new' },
      updatedAt: 2,
      size: 30,
    })
    const originalPut = IDBObjectStore.prototype.put

    for (const failingStore of [PAYLOAD_STORE_NAME, META_STORE_NAME]) {
      await idbClear()
      await idbPut(oldRecord)
      IDBObjectStore.prototype.put = new Proxy(originalPut, {
        apply(target, thisArgument, argumentsList) {
          const store = thisArgument as IDBObjectStore
          const value = argumentsList[0] as { key?: unknown }
          if (store.name === failingStore && value.key === newRecord.key) {
            throw new Error(`injected ${failingStore} quota put failure`)
          }
          return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<IDBValidKey>
        },
      })
      try {
        const result = await queue.enqueue(lease, 'atomic quota failure', (operation) =>
          idbPutWithinQuota(newRecord, 50, operation),
        )
        expect(result).toBeNull()
      } finally {
        IDBObjectStore.prototype.put = originalPut
      }
      expect(await idbGetAll()).toEqual([oldRecord])
    }
  })

  it('fails soft and rolls back quota eviction when the browser rejects the payload write', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const oldRecord = createPersistedRecord('physical-A', 'GET /api/links?quota-old', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'old' },
      updatedAt: 1,
      size: 30,
    })
    const newRecord = createPersistedRecord('physical-A', 'GET /api/links?quota-new', {
      schema: CACHE_SCHEMA_VERSION,
      data: { body: 'new' },
      updatedAt: 2,
      size: 30,
    })
    await idbPut(oldRecord)
    const originalPut = IDBObjectStore.prototype.put
    IDBObjectStore.prototype.put = new Proxy(originalPut, {
      apply(target, thisArgument, argumentsList) {
        const store = thisArgument as IDBObjectStore
        const value = argumentsList[0] as { key?: unknown }
        if (store.name === PAYLOAD_STORE_NAME && value.key === newRecord.key) {
          throw new DOMException('quota exceeded', 'QuotaExceededError')
        }
        return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<IDBValidKey>
      },
    })
    try {
      await expect(queue.enqueue(lease, 'quota payload failure', (operation) =>
        idbPutWithinQuota(newRecord, 50, operation),
      )).resolves.toBeNull()
    } finally {
      IDBObjectStore.prototype.put = originalPut
    }

    expect(await idbGetAll()).toEqual([oldRecord])
    expect(await idbGetMetadata()).toHaveLength(1)
  })

  it('repairs payload and metadata orphans idempotently without reading payload bodies', async () => {
    await rawCacheTransaction([PAYLOAD_STORE_NAME, META_STORE_NAME], (transaction) => {
      transaction.objectStore(PAYLOAD_STORE_NAME).put({
        key: 'payload-without-meta',
        data: { private: 'body' },
      })
      transaction.objectStore(META_STORE_NAME).put({
        key: 'meta-without-payload',
        namespace: 'physical-A',
        logicalKey: 'GET /api/links?orphan',
        schema: CACHE_SCHEMA_VERSION,
        size: 10,
        lastAccess: 1,
        generation: 0,
      })
    })
    const get = vi.spyOn(IDBObjectStore.prototype, 'get')

    expect(await idbRepairOrphans()).toEqual({ payloadsRemoved: 1, metadataRemoved: 1 })
    expect(await idbRepairOrphans()).toEqual({ payloadsRemoved: 0, metadataRemoved: 0 })
    expect(
      get.mock.contexts.filter(
        (store) => (store as IDBObjectStore).name === PAYLOAD_STORE_NAME,
      ),
    ).toHaveLength(0)
    expect(await idbGetMetadata()).toEqual([])
    expect(await idbGetAll()).toEqual([])
  })
})
