import 'fake-indexeddb/auto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ownedDatabaseName } from '../storage-ownership'
import {
  CACHE_DATABASE_VERSION,
  CACHE_SCHEMA_VERSION,
  META_STORE_NAME,
  PAYLOAD_STORE_NAME,
  createPersistedRecord,
  idbGetAll,
  idbGetMetadata,
  resetDatabaseHandle,
} from './idb'

const DATABASE_NAME = ownedDatabaseName('cacheDatabase')
const LEGACY_STORE_NAME = 'resources'
const CONTROL_STORE_NAME = 'cache_control'

function openRequest(request: IDBOpenDBRequest): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

async function deleteDatabase(): Promise<void> {
  resetDatabaseHandle()
  await Promise.resolve()
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(DATABASE_NAME)
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
}

async function createLegacyDatabase(
  version: 1 | 2,
  record = createPersistedRecord('physical-A', 'GET /api/links?v2', {
    schema: CACHE_SCHEMA_VERSION,
    data: { from: 'v2' },
    updatedAt: 7,
    size: 12,
  }),
): Promise<IDBDatabase> {
  const request = indexedDB.open(DATABASE_NAME, version)
  request.onupgradeneeded = () => {
    const database = request.result
    database.createObjectStore(LEGACY_STORE_NAME, { keyPath: 'key' })
    if (version >= 2) database.createObjectStore(CONTROL_STORE_NAME, { keyPath: 'key' })
  }
  const database = await openRequest(request)
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(LEGACY_STORE_NAME, 'readwrite')
    transaction.objectStore(LEGACY_STORE_NAME).put(record)
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  return database
}

beforeEach(async () => {
  await deleteDatabase()
})

afterEach(async () => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  await deleteDatabase()
})

describe('cache database upgrade paths', () => {
  it('upgrades a deployed v1 resources database directly to v3', async () => {
    const oldDatabase = await createLegacyDatabase(1)
    oldDatabase.close()
    resetDatabaseHandle()

    expect(await idbGetAll()).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        logicalKey: 'GET /api/links?v2',
        data: { from: 'v2' },
        generation: 0,
      }),
    ])

    const database = await openRequest(indexedDB.open(DATABASE_NAME))
    expect(database.version).toBe(CACHE_DATABASE_VERSION)
    expect([...database.objectStoreNames]).toEqual(
      expect.arrayContaining([CONTROL_STORE_NAME, PAYLOAD_STORE_NAME, META_STORE_NAME]),
    )
    database.close()
  })

  it('atomically upgrades v2 resources into payload and metadata stores', async () => {
    const oldDatabase = await createLegacyDatabase(2)
    oldDatabase.close()
    resetDatabaseHandle()

    expect(await idbGetAll()).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        logicalKey: 'GET /api/links?v2',
        data: { from: 'v2' },
        generation: 0,
      }),
    ])
    expect(await idbGetMetadata()).toEqual([
      expect.objectContaining({ size: 12, lastAccess: 7, generation: 0 }),
    ])

    const database = await openRequest(indexedDB.open(DATABASE_NAME))
    expect(database.version).toBe(CACHE_DATABASE_VERSION)
    expect([...database.objectStoreNames]).toEqual(
      expect.arrayContaining([
        LEGACY_STORE_NAME,
        CONTROL_STORE_NAME,
        PAYLOAD_STORE_NAME,
        META_STORE_NAME,
      ]),
    )
    const legacyCount = await new Promise<number>((resolve) => {
      const transaction = database.transaction(LEGACY_STORE_NAME, 'readonly')
      const request = transaction.objectStore(LEGACY_STORE_NAME).count()
      request.onsuccess = () => resolve(request.result)
    })
    expect(legacyCount).toBe(0)
    database.close()
  })

  it('fails soft while an older tab blocks the upgrade', async () => {
    const blockingDatabase = await createLegacyDatabase(2)
    resetDatabaseHandle()

    expect(await idbGetMetadata()).toEqual([])
    expect(blockingDatabase.version).toBe(2)
    expect([...blockingDatabase.objectStoreNames]).not.toContain(PAYLOAD_STORE_NAME)

    blockingDatabase.close()
    await Promise.resolve()
  })

  it('fails soft when open never produces a browser event', async () => {
    vi.useFakeTimers()
    const realOpen = indexedDB.open.bind(indexedDB)
    vi.spyOn(indexedDB, 'open').mockImplementation(
      () => ({}) as IDBOpenDBRequest,
    )
    resetDatabaseHandle()

    const metadata = idbGetMetadata()
    await vi.advanceTimersByTimeAsync(1100)
    expect(await metadata).toEqual([])

    vi.mocked(indexedDB.open).mockImplementation(realOpen)
  })

  it('rolls back the versionchange transaction when metadata migration fails', async () => {
    const oldDatabase = await createLegacyDatabase(2)
    oldDatabase.close()
    const originalPut = IDBObjectStore.prototype.put
    IDBObjectStore.prototype.put = new Proxy(originalPut, {
      apply(target, thisArgument, argumentsList) {
        if ((thisArgument as IDBObjectStore).name === META_STORE_NAME) {
          throw new Error('injected migration failure')
        }
        return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<IDBValidKey>
      },
    })
    resetDatabaseHandle()
    try {
      expect(await idbGetAll()).toEqual([])
    } finally {
      IDBObjectStore.prototype.put = originalPut
    }
    resetDatabaseHandle()

    const database = await openRequest(indexedDB.open(DATABASE_NAME, 2))
    expect(database.version).toBe(2)
    expect([...database.objectStoreNames]).not.toContain(PAYLOAD_STORE_NAME)
    const records = await new Promise<unknown[]>((resolve) => {
      const transaction = database.transaction(LEGACY_STORE_NAME, 'readonly')
      const request = transaction.objectStore(LEGACY_STORE_NAME).getAll()
      request.onsuccess = () => resolve(request.result)
    })
    expect(records).toEqual([expect.objectContaining({ data: { from: 'v2' } })])
    database.close()
  })
})
