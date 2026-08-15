import 'fake-indexeddb/auto'

import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import type { Annotation } from '../annotations'
import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  commitAnnotationOperation,
  enumerateAnnotatedLinkIds,
  readAnnotationSnapshot,
} from '../user-data/annotation-store'
import { listAnnotationOperationsForTest } from '../../test/annotation-operations'
import type { AnnotationTarget } from '../user-data/annotation-types'
import type { SavedContentAnnotationAddDraft } from '../user-data/annotation-types'
import {
  LEGACY_ARCHIVE_STORE,
  LEGACY_PENDING_STORE,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from '../user-data/idb'
import { cacheStorageQueue } from './io-queue'
import {
  CACHE_SCHEMA_VERSION,
  META_STORE_NAME,
  createPersistedRecord,
  idbClear,
  idbGetAll,
  idbPut,
  idbPutWithinQuota,
  resetDatabaseHandle,
} from './idb'

const CACHE_DATABASE_NAME = ownedDatabaseName('cacheDatabase')
const USER_DATA_DATABASE_NAME = ownedDatabaseName('userDataDatabase')
const LEGACY_CACHE_STORE = 'resources'
const CACHE_CONTROL_STORE = 'cache_control'
const NAMESPACE = 'physical-isolation'
const LINK_ID = 'user-data-link'
const TARGET = {
  kind: 'saved-content',
  contentRevision: 7,
} as const satisfies AnnotationTarget

const ANNOTATION = {
  id: 'durable-annotation',
  blockKey: 'content-document',
  start: 0,
  end: 7,
  text: 'durable',
  note: 'must survive cache maintenance',
  source: 'self',
  createdAt: 100,
  updatedAt: 100,
  sourceContentRevision: 7,
} as const satisfies Annotation

const ANNOTATION_DRAFT = {
  id: ANNOTATION.id,
  blockKey: ANNOTATION.blockKey,
  start: ANNOTATION.start,
  end: ANNOTATION.end,
  text: ANNOTATION.text,
  note: ANNOTATION.note,
  source: ANNOTATION.source,
  createdAt: ANNOTATION.createdAt,
  updatedAt: ANNOTATION.updatedAt,
} as const satisfies SavedContentAnnotationAddDraft

function identity(): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: 'server-isolation',
    physicalNamespace: NAMESPACE,
    localEpoch: 1,
  })
}

function openRequest(request: IDBOpenDBRequest): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

async function deleteDatabase(name: string): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error(`failed to delete ${name}`))
    request.onblocked = () => reject(new Error(`deleting ${name} was blocked`))
  })
}

async function resetDatabases(): Promise<void> {
  resetDatabaseHandle()
  resetUserDataDatabaseHandle()
  await Promise.resolve()
  await deleteDatabase(CACHE_DATABASE_NAME)
  await deleteDatabase(USER_DATA_DATABASE_NAME)
}

async function seedUserData(lease: IdentityLease): Promise<void> {
  await expect(commitAnnotationOperation(lease, {
    kind: 'add',
    opId: 'durable-operation',
    linkId: LINK_ID,
    target: TARGET,
    draft: ANNOTATION_DRAFT,
  })).resolves.toMatchObject({
    ok: true,
    value: { status: 'committed', annotationStoreVersion: 1 },
  })

  await expect(runUserDataTransaction(
    lease,
    'seed cache isolation fixtures',
    [LEGACY_PENDING_STORE, LEGACY_ARCHIVE_STORE],
    'readwrite',
    (transaction, _operation, setResult) => {
      transaction.objectStore(LEGACY_PENDING_STORE).put({
        id: 'pins',
        legacyKey: 'webtag:pins:v1',
        value: '{"tags":["quarantined"],"domains":[]}',
        quarantinedAt: 101,
      })
      transaction.objectStore(LEGACY_ARCHIVE_STORE).add({
        id: 'annotationsV1',
        legacyKey: 'webtag:annotations:v1',
        value: '{"archived-link":[{"id":"archived"}]}',
        quarantinedAt: 102,
        importedIntoNamespace: NAMESPACE,
        importedAt: 103,
        fingerprintVersion: 1,
        fingerprints: ['archive-fingerprint'],
      })
      setResult(true)
    },
  )).resolves.toEqual({ ok: true, value: true })
}

async function expectUserDataIntact(lease: IdentityLease): Promise<void> {
  await expect(readAnnotationSnapshot(lease, LINK_ID, TARGET)).resolves.toMatchObject({
    ok: true,
    value: {
      annotationStoreVersion: 1,
      annotations: [ANNOTATION],
    },
  })
  await expect(listAnnotationOperationsForTest(lease, LINK_ID, TARGET)).resolves.toMatchObject({
    ok: true,
    value: [{ opId: 'durable-operation', annotationId: ANNOTATION.id }],
  })
  await expect(enumerateAnnotatedLinkIds(lease)).resolves.toEqual({
    ok: true,
    value: [LINK_ID],
  })

  const legacy = await runUserDataTransaction(
    lease,
    'inspect cache isolation fixtures',
    [LEGACY_PENDING_STORE, LEGACY_ARCHIVE_STORE],
    'readonly',
    (transaction, _operation, setResult) => {
      const pending = transaction.objectStore(LEGACY_PENDING_STORE).get('pins')
      const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE).getAll()
      let completed = 0
      const finish = () => {
        completed += 1
        if (completed === 2) {
          setResult({ pending: pending.result, archive: archive.result })
        }
      }
      pending.onsuccess = finish
      archive.onsuccess = finish
      pending.onerror = () => transaction.abort()
      archive.onerror = () => transaction.abort()
    },
  )
  expect(legacy).toMatchObject({
    ok: true,
    value: {
      pending: {
        id: 'pins',
        value: '{"tags":["quarantined"],"domains":[]}',
      },
      archive: [{
        id: 'annotationsV1',
        value: '{"archived-link":[{"id":"archived"}]}',
        importedIntoNamespace: NAMESPACE,
        fingerprintVersion: 1,
      }],
    },
  })
}

async function createVersionTwoCacheDatabase(): Promise<void> {
  const request = indexedDB.open(CACHE_DATABASE_NAME, 2)
  request.onupgradeneeded = () => {
    request.result.createObjectStore(LEGACY_CACHE_STORE, { keyPath: 'key' })
    request.result.createObjectStore(CACHE_CONTROL_STORE, { keyPath: 'key' })
  }
  const database = await openRequest(request)
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(LEGACY_CACHE_STORE, 'readwrite')
    transaction.objectStore(LEGACY_CACHE_STORE).put(createPersistedRecord(
      NAMESPACE,
      'GET /api/links?legacy-cache',
      {
        schema: CACHE_SCHEMA_VERSION,
        data: { cache: 'legacy' },
        updatedAt: 1,
        size: 10,
      },
    ))
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  database.close()
}

beforeEach(async () => {
  await resetDatabases()
})

afterEach(async () => {
  await resetDatabases()
})

describe('cache and user-data database isolation', () => {
  it('preserves annotations, their index, quarantine, and archive after cache upgrade failure', async () => {
    const lease = identity()
    await seedUserData(lease)
    resetUserDataDatabaseHandle()
    await createVersionTwoCacheDatabase()

    const originalPut = IDBObjectStore.prototype.put
    IDBObjectStore.prototype.put = new Proxy(originalPut, {
      apply(target, thisArgument, argumentsList) {
        if ((thisArgument as IDBObjectStore).name === META_STORE_NAME) {
          throw new Error('injected cache migration failure')
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

    const rolledBackCache = await openRequest(indexedDB.open(CACHE_DATABASE_NAME, 2))
    expect(rolledBackCache.version).toBe(2)
    expect(rolledBackCache.objectStoreNames.contains(META_STORE_NAME)).toBe(false)
    rolledBackCache.close()

    await expectUserDataIntact(lease)
  })

  it('preserves user data while cache quota admission evicts old cache records', async () => {
    const lease = identity()
    await seedUserData(lease)
    resetUserDataDatabaseHandle()

    const oldCache = createPersistedRecord(NAMESPACE, 'GET /api/links?old-cache', {
      schema: CACHE_SCHEMA_VERSION,
      data: { cache: 'old' },
      updatedAt: 1,
      size: 80,
    })
    const incomingCache = createPersistedRecord(NAMESPACE, 'GET /api/links?incoming-cache', {
      schema: CACHE_SCHEMA_VERSION,
      data: { cache: 'incoming' },
      updatedAt: 2,
      size: 60,
    })
    expect(await idbPut(oldCache)).toBe(true)

    const admitted = await cacheStorageQueue.enqueue(
      lease,
      'cache isolation quota admission',
      (operation) => idbPutWithinQuota(incomingCache, 100, operation),
    )

    expect(admitted).toEqual({ stored: true, mutated: true, totalBytes: 60 })
    expect(await idbGetAll()).toEqual([incomingCache])
    await expectUserDataIntact(lease)
  })

  it('clears every cache store without clearing any user-data store', async () => {
    const lease = identity()
    await seedUserData(lease)
    resetUserDataDatabaseHandle()

    const cacheRecord = createPersistedRecord(NAMESPACE, 'GET /api/links?cached', {
      schema: CACHE_SCHEMA_VERSION,
      data: { cache: 'clear-me' },
      updatedAt: 1,
      size: 10,
    })
    expect(await idbPut(cacheRecord)).toBe(true)
    expect(await idbGetAll()).toEqual([cacheRecord])

    await idbClear()

    expect(await idbGetAll()).toEqual([])
    await expectUserDataIntact(lease)
  })
})
