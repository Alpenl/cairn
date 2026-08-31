import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  ANNOTATION_OPS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SUPERSESSION_STATE_STORE,
  THOUGHT_SYNC_STATE_STORE,
  USER_DATA_DATABASE_VERSION,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from './idb'

const DATABASE_NAME = ownedDatabaseName('userDataDatabase')
const RETIRED_THOUGHT_REPAIR_STORES = [
  'thought_repair_ready',
  'thought_repair_quarantine',
  'thought_repair_manifest',
  'thought_repair_source',
  'thought_repair_ack',
] as const

function openRequest(request: IDBOpenDBRequest): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

async function deleteDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(DATABASE_NAME)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

async function createVersionThreeDatabase(): Promise<IDBDatabase> {
  const request = indexedDB.open(DATABASE_NAME, 3)
  request.onupgradeneeded = () => {
    request.result.createObjectStore('legacy-unattributed', { keyPath: 'id' })
  }
  return openRequest(request)
}

async function createVersionNineDatabase(): Promise<IDBDatabase> {
  const request = indexedDB.open(DATABASE_NAME, 9)
  request.onupgradeneeded = () => {
    request.result.createObjectStore(ANNOTATION_MATERIALIZED_STORE, { keyPath: 'key' })
    for (const storeName of RETIRED_THOUGHT_REPAIR_STORES) {
      request.result.createObjectStore(storeName, { keyPath: 'key' })
    }
  }
  const database = await openRequest(request)
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_MATERIALIZED_STORE, 'readwrite')
    transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).put({
      key: ['physical-A', 'L1', 'saved-content:1', 'a1'],
      namespace: 'physical-A',
      linkId: 'L1',
      targetKey: 'saved-content:1',
      annotationId: 'a1',
    })
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  return database
}

async function readStoreRecord(storeName: string, key: IDBValidKey): Promise<unknown> {
  const database = await openUserDataDatabase()
  if (!database) return undefined
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(storeName, 'readonly')
    const request = transaction.objectStore(storeName).get(key)
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  await deleteDatabase()
})

describe('user-data IndexedDB owner', () => {
  it('upgrades v9 to the current schema while removing retired Thought repair stores', async () => {
    const versionNine = await createVersionNineDatabase()
    versionNine.close()

    const upgraded = await openUserDataDatabase()

    expect(upgraded?.version).toBe(USER_DATA_DATABASE_VERSION)
    for (const storeName of RETIRED_THOUGHT_REPAIR_STORES) {
      expect(upgraded?.objectStoreNames.contains(storeName)).toBe(false)
    }
    for (const storeName of [
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
      THOUGHT_SUPERSESSION_EVENTS_STORE,
      THOUGHT_SUPERSESSION_STATE_STORE,
    ]) {
      expect(upgraded?.objectStoreNames.contains(storeName)).toBe(true)
    }
    await expect(readStoreRecord(
      ANNOTATION_MATERIALIZED_STORE,
      ['physical-A', 'L1', 'saved-content:1', 'a1'],
    )).resolves.toMatchObject({ annotationId: 'a1' })
  })

  it('rolls back a failed versionchange and succeeds on the next open', async () => {
    const oldDatabase = await createVersionThreeDatabase()
    oldDatabase.close()
    const originalCreate = IDBDatabase.prototype.createObjectStore
    IDBDatabase.prototype.createObjectStore = new Proxy(originalCreate, {
      apply(target, thisArgument, argumentsList) {
        if (argumentsList[0] === ANNOTATION_MATERIALIZED_STORE) {
          throw new Error('injected schema upgrade failure')
        }
        return Reflect.apply(target, thisArgument, argumentsList) as IDBObjectStore
      },
    })
    resetUserDataDatabaseHandle()
    try {
      expect(await openUserDataDatabase()).toBeNull()
    } finally {
      IDBDatabase.prototype.createObjectStore = originalCreate
    }

    const versionThree = await openRequest(indexedDB.open(DATABASE_NAME, 3))
    expect(versionThree.version).toBe(3)
    expect(versionThree.objectStoreNames.contains(ANNOTATION_MATERIALIZED_STORE)).toBe(false)
    versionThree.close()

    const upgraded = await openUserDataDatabase()
    expect(upgraded?.version).toBe(USER_DATA_DATABASE_VERSION)
    expect(upgraded?.objectStoreNames.contains(ANNOTATION_MATERIALIZED_STORE)).toBe(true)
  })

  it('does not cache a blocked null opener', async () => {
    const blocker = await createVersionThreeDatabase()
    resetUserDataDatabaseHandle()
    expect(await openUserDataDatabase()).toBeNull()

    blocker.close()
    await Promise.resolve()
    const upgraded = await openUserDataDatabase()
    expect(upgraded?.version).toBe(USER_DATA_DATABASE_VERSION)
  })

  it('discards an opener that completes after the owner is reset', async () => {
    const abandoned = openUserDataDatabase()
    resetUserDataDatabaseHandle()

    await expect(abandoned).resolves.toBeNull()
    const current = await openUserDataDatabase()
    expect(current?.version).toBe(USER_DATA_DATABASE_VERSION)
  })

  it('reopens after a versionchange closes the cached connection', async () => {
    const first = await openUserDataDatabase()
    expect(first).not.toBeNull()

    first?.onversionchange?.call(
      first,
      new Event('versionchange') as IDBVersionChangeEvent,
    )

    const reopened = await openUserDataDatabase()
    expect(reopened).not.toBeNull()
    expect(reopened).not.toBe(first)
    expect(reopened?.version).toBe(USER_DATA_DATABASE_VERSION)
  })

  it('aborts an in-flight transaction when its identity lease is revoked', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })
    const resultPromise = runUserDataTransaction(
      lease,
      'revoked write',
      [ANNOTATION_LINK_STATE_STORE],
      'readwrite',
      (transaction) => {
        transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
          key: 'physical-A\0L1',
          namespace: 'physical-A',
          linkId: 'L1',
          version: 1,
        })
        lease.revoke()
      },
    )

    await expect(resultPromise).resolves.toEqual({ ok: false })
    await expect(readStoreRecord(
      ANNOTATION_LINK_STATE_STORE,
      'physical-A\0L1',
    )).resolves.toBeUndefined()
  })

  it('reports request errors as failed transactions and rolls back writes', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })
    const duplicateOp = {
      opId: '10:physical-A:duplicate-op',
      logicalOpId: 'duplicate-op',
      namespace: 'physical-A',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      targetKey: 'saved-content:7',
      annotationId: 'a1',
      kind: 'delete',
    }

    const result = await runUserDataTransaction(
      lease,
      'duplicate op-id request error',
      [ANNOTATION_OPS_STORE],
      'readwrite',
      (transaction, _operation, setResult) => {
        const operations = transaction.objectStore(ANNOTATION_OPS_STORE)
        operations.add(duplicateOp)
        operations.add({ ...duplicateOp, annotationId: 'a2' })
        setResult(true)
      },
    )

    expect(result).toEqual({ ok: false })
    await expect(runUserDataTransaction(
      lease,
      'count rolled back operations',
      [ANNOTATION_OPS_STORE],
      'readonly',
      (transaction, _operation, setResult) => {
        const request = transaction.objectStore(ANNOTATION_OPS_STORE).count()
        request.onsuccess = () => setResult(request.result)
      },
    )).resolves.toEqual({ ok: true, value: 0 })
  })

  it('reports an explicit transaction abort as failed and rolls back writes', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })

    const result = await runUserDataTransaction(
      lease,
      'explicit transaction abort',
      [ANNOTATION_LINK_STATE_STORE],
      'readwrite',
      (transaction, _operation, setResult) => {
        transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
          key: 'physical-A\0L1',
          namespace: 'physical-A',
          linkId: 'L1',
          version: 1,
        })
        setResult(true)
        transaction.abort()
      },
    )

    expect(result).toEqual({ ok: false })
    await expect(readStoreRecord(
      ANNOTATION_LINK_STATE_STORE,
      'physical-A\0L1',
    )).resolves.toBeUndefined()
  })

  it('aborts writes when the transaction callback throws', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })

    const result = await runUserDataTransaction(
      lease,
      'throwing write',
      [ANNOTATION_LINK_STATE_STORE],
      'readwrite',
      (transaction) => {
        transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
          key: 'physical-A\0L1',
          namespace: 'physical-A',
          linkId: 'L1',
          version: 1,
        })
        throw new Error('injected callback failure')
      },
    )

    expect(result).toEqual({ ok: false })
    await expect(readStoreRecord(
      ANNOTATION_LINK_STATE_STORE,
      'physical-A\0L1',
    )).resolves.toBeUndefined()
  })
})
