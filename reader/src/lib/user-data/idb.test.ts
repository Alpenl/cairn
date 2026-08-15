import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { IdentityLease } from '../identity'
import type { IdentityBoundReaderClient } from '../api/client'
import { ownedDatabaseName } from '../storage-ownership'
import {
  ANNOTATED_LINKS_STORE,
  ANNOTATED_LINKS_NAMESPACE_INDEX,
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_ID_INDEX,
  ANNOTATION_OPS_STORE,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_REPAIR_ACK_STORE,
  THOUGHT_REPAIR_MANIFEST_STORE,
  THOUGHT_REPAIR_QUARANTINE_STORE,
  THOUGHT_REPAIR_READY_STORE,
  THOUGHT_REPAIR_SOURCE_STORE,
  THOUGHT_SYNC_STATE_STORE,
  THOUGHT_MATERIALIZED_STORE,
  LEGACY_ARCHIVE_STORE,
  LEGACY_DEDUP_STORE,
  LEGACY_PENDING_STORE,
  MIGRATION_DECISION_STORE,
  USER_DATA_DATABASE_VERSION,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from './idb'
import { commitAnnotationOperation } from './annotation-store'
import { startThoughtSync, syncThoughts } from './thought-sync'

const DATABASE_NAME = ownedDatabaseName('userDataDatabase')

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
    const database = request.result
    database.createObjectStore(LEGACY_PENDING_STORE, { keyPath: 'id' })
    const archive = database.createObjectStore(LEGACY_ARCHIVE_STORE, {
      keyPath: 'archiveID',
      autoIncrement: true,
    })
    archive.createIndex('by-imported-namespace', 'importedIntoNamespace')
    archive.createIndex('by-fingerprint-version', 'fingerprintVersion')
    database.createObjectStore(LEGACY_DEDUP_STORE, { keyPath: 'id' })
    database.createObjectStore(MIGRATION_DECISION_STORE, { keyPath: 'id' })
  }
  const database = await openRequest(request)
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(LEGACY_PENDING_STORE, 'readwrite')
    transaction.objectStore(LEGACY_PENDING_STORE).put({
      id: 'pins',
      legacyKey: 'webtag:pins:v1',
      value: '{"tags":["kept"],"domains":[]}',
      quarantinedAt: 1,
    })
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  return database
}

async function createVersionFourDatabase(options: { includeUpdate?: boolean } = {}): Promise<IDBDatabase> {
  const request = indexedDB.open(DATABASE_NAME, 4)
  request.onupgradeneeded = () => {
    const database = request.result
    database.createObjectStore(LEGACY_PENDING_STORE, { keyPath: 'id' })
    const archive = database.createObjectStore(LEGACY_ARCHIVE_STORE, {
      keyPath: 'archiveID',
      autoIncrement: true,
    })
    archive.createIndex('by-imported-namespace', 'importedIntoNamespace')
    archive.createIndex('by-fingerprint-version', 'fingerprintVersion')
    database.createObjectStore(LEGACY_DEDUP_STORE, { keyPath: 'id' })
    database.createObjectStore(MIGRATION_DECISION_STORE, { keyPath: 'id' })
    const operations = database.createObjectStore(ANNOTATION_OPS_STORE, {
      keyPath: 'sequence',
      autoIncrement: true,
    })
    operations.createIndex(ANNOTATION_OPS_ID_INDEX, 'opId', { unique: true })
    operations.createIndex(
      'by-target-sequence',
      ['namespace', 'linkId', 'targetKey', 'sequence'],
    )
    database.createObjectStore(ANNOTATION_MATERIALIZED_STORE, { keyPath: 'key' })
    database.createObjectStore(ANNOTATION_LINK_STATE_STORE, { keyPath: 'key' })
    const annotatedLinks = database.createObjectStore(ANNOTATED_LINKS_STORE, { keyPath: 'key' })
    annotatedLinks.createIndex(ANNOTATED_LINKS_NAMESPACE_INDEX, 'namespace')
    database.createObjectStore(ANNOTATION_IMPORTS_STORE, { keyPath: 'key' })
  }
  const database = await openRequest(request)
  const target = { kind: 'saved-content', contentRevision: 7 }
  const targetKey = 'saved-content:7'
  const annotation = {
    id: 'a1',
    blockKey: 'content-document',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceContentRevision: 7,
  }
  const recoveryTarget = { kind: 'summary', sourceHash: 'a'.repeat(64) }
  const recoveryTargetKey = `summary:${'a'.repeat(64)}`
  const recoveryAnnotation = {
    ...annotation,
    id: 'a2',
    blockKey: 'summary',
    sourceContentRevision: undefined,
    sourceSummaryHash: 'a'.repeat(64),
  }
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction([
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
    ], 'readwrite')
    transaction.objectStore(ANNOTATION_OPS_STORE).put({
      sequence: 1,
      opId: 'legacy-op-1',
      namespace: 'physical-A',
      linkId: 'L1',
      target,
      targetKey,
      annotationId: 'a1',
      kind: 'add',
      annotation,
    })
    transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).put({
      key: ['physical-A', 'L1', targetKey, 'a1'],
      namespace: 'physical-A',
      linkId: 'L1',
      target,
      targetKey,
      annotationId: 'a1',
      sequence: 1,
      annotation,
      fallbackAnnotation: annotation,
    })
    transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
      key: ['physical-A', 'L1', targetKey],
      namespace: 'physical-A',
      linkId: 'L1',
      target,
      targetKey,
      version: 1,
      activeCount: 1,
      compactedThroughSequence: 0,
      snapshot: [],
    })
    transaction.objectStore(ANNOTATED_LINKS_STORE).put({
      key: ['physical-A', 'L1', targetKey],
      namespace: 'physical-A',
      linkId: 'L1',
      target,
      targetKey,
      annotationCount: 1,
      annotationStoreVersion: 1,
    })
    transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).put({
      key: ['physical-A', 'L1', recoveryTargetKey, 'a2'],
      namespace: 'physical-A',
      linkId: 'L1',
      target: recoveryTarget,
      targetKey: recoveryTargetKey,
      annotationId: 'a2',
      sequence: 2,
      annotation: recoveryAnnotation,
      fallbackAnnotation: recoveryAnnotation,
    })
    transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
      key: ['physical-A', 'L1', recoveryTargetKey],
      namespace: 'physical-A',
      linkId: 'L1',
      target: recoveryTarget,
      targetKey: recoveryTargetKey,
      version: 2,
      activeCount: 1,
      compactedThroughSequence: 2,
      snapshot: [{
        annotationId: 'a2',
        sequence: 2,
        annotation: recoveryAnnotation,
        fallbackAnnotation: recoveryAnnotation,
      }],
    })
    if (options.includeUpdate) {
      transaction.objectStore(ANNOTATION_OPS_STORE).put({
        sequence: 3,
        opId: 'legacy-op-update',
        namespace: 'physical-A',
        linkId: 'L1',
        target,
        targetKey,
        annotationId: 'a1',
        kind: 'update',
        patch: { note: 'updated legacy thought', source: 'ai', updatedAt: 2 },
      })
    }
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  return database
}

async function createVersionFiveDatabase(): Promise<IDBDatabase> {
  const v4 = await createVersionFourDatabase({ includeUpdate: true })
  v4.close()
  const request = indexedDB.open(DATABASE_NAME, 5)
  request.onupgradeneeded = () => {
    const database = request.result
    const outbox = database.createObjectStore(THOUGHT_OUTBOX_STORE, { keyPath: 'key' })
    outbox.createIndex('by-namespace', 'namespace')
    outbox.createIndex('by-op-id', ['namespace', 'opId'], { unique: true })
    outbox.createIndex('by-namespace-sequence', ['namespace', 'sequence'])
    database.createObjectStore(THOUGHT_SYNC_STATE_STORE, { keyPath: 'namespace' })
    const materialized = database.createObjectStore(THOUGHT_MATERIALIZED_STORE, { keyPath: 'key' })
    materialized.createIndex('by-namespace', 'namespace')
    materialized.createIndex('by-host', ['namespace', 'linkId', 'targetKey'])
  }
  const database = await openRequest(request)
  const target = { kind: 'saved-content', contentRevision: 7 }
  const targetKey = 'saved-content:7'
  const annotation = {
    id: 'a1', blockKey: 'content-document', start: 0, end: 4, text: 'text', note: '',
    source: 'self', createdAt: 1, updatedAt: 1, sourceContentRevision: 7,
  }
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_OUTBOX_STORE, 'readwrite')
    const outbox = transaction.objectStore(THOUGHT_OUTBOX_STORE)
    outbox.put({
      key: ['physical-A', 1], namespace: 'physical-A', sequence: 1, opId: 'legacy-op-1',
      deviceId: '', contractVersion: 0, logicalClock: 0, operationKind: 'add', annotationId: 'a1',
      hostKind: 'link', hostId: 'L1', linkId: 'L1', target, targetKey, annotation,
      createdAt: 0, attemptCount: 0,
    })
    outbox.put({
      key: ['physical-A', 3], namespace: 'physical-A', sequence: 3, opId: 'legacy-op-update',
      deviceId: '', contractVersion: 0, logicalClock: 0, operationKind: 'update', annotationId: 'a1',
      hostKind: 'link', hostId: 'L1', linkId: 'L1', target, targetKey, annotation: null,
      patch: { note: 'updated legacy thought', source: 'ai', updatedAt: 2 }, createdAt: 0, attemptCount: 0,
    })
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  return database
}

async function addInvalidQuoteOperation(database: IDBDatabase): Promise<void> {
  const target = { kind: 'saved-content', contentRevision: 7 }
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_OPS_STORE, 'readwrite')
    transaction.objectStore(ANNOTATION_OPS_STORE).put({
      sequence: 4,
      opId: 'invalid-quote-op',
      namespace: 'physical-A',
      linkId: 'L1',
      target,
      targetKey: 'saved-content:7',
      annotationId: 'bad-quote',
      kind: 'add',
      annotation: {
        id: 'bad-quote', blockKey: 'content-document', start: 0, end: 1, text: 'x', note: '',
        source: 'self', createdAt: 1, updatedAt: 1, sourceContentRevision: 7,
        quote: { exact: 7 },
      },
    })
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function snapshotDatabase(
  database: IDBDatabase,
  stores: readonly string[],
): Promise<Record<string, unknown[]>> {
  return new Promise((resolve, reject) => {
    const transaction = database.transaction([...stores], 'readonly')
    const requests = Object.fromEntries(stores.map((store) => [store, transaction.objectStore(store).getAll()])) as
      Record<string, IDBRequest<unknown[]>>
    transaction.oncomplete = () => resolve(Object.fromEntries(stores.map((store) => [store, requests[store].result])))
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function snapshotDatabaseKeysAndValues(
  database: IDBDatabase,
  stores: readonly string[],
): Promise<Record<string, { readonly keys: readonly IDBValidKey[]; readonly values: readonly unknown[] }>> {
  return new Promise((resolve, reject) => {
    const transaction = database.transaction([...stores], 'readonly')
    const requests = Object.fromEntries(stores.map((store) => [store, {
      keys: transaction.objectStore(store).getAllKeys(),
      values: transaction.objectStore(store).getAll(),
    }])) as Record<string, { keys: IDBRequest<IDBValidKey[]>; values: IDBRequest<unknown[]> }>
    transaction.oncomplete = () => resolve(Object.fromEntries(stores.map((store) => [store, {
      keys: requests[store].keys.result,
      values: requests[store].values.result,
    }])))
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function clearStore(storeName: string): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('database did not open')
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(storeName, 'readwrite')
    transaction.objectStore(storeName).clear()
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function mutateStoreRows(
  storeName: string,
  mutate: (store: IDBObjectStore, rows: readonly unknown[]) => void,
): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('database did not open')
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(storeName, 'readwrite')
    const store = transaction.objectStore(storeName)
    const request = store.getAll() as IDBRequest<unknown[]>
    request.onsuccess = () => mutate(store, request.result)
    request.onerror = () => transaction.abort()
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function readAllStore(storeName: string): Promise<unknown[]> {
  const database = await openUserDataDatabase()
  if (!database) return []
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(storeName, 'readonly')
    const request = transaction.objectStore(storeName).getAll()
    transaction.oncomplete = () => resolve(request.result)
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function readStoreRecord(
  storeName: typeof ANNOTATION_LINK_STATE_STORE,
  key: IDBValidKey,
): Promise<unknown> {
  const database = await openUserDataDatabase()
  if (!database) return undefined
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(storeName, 'readonly')
    const request = transaction.objectStore(storeName).get(key)
    transaction.oncomplete = () => resolve(request.result)
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  await deleteDatabase()
})

describe('user-data IndexedDB v4 owner', () => {
  it('upgrades v3 atomically while preserving every legacy store and record', async () => {
    const oldDatabase = await createVersionThreeDatabase()
    oldDatabase.close()
    resetUserDataDatabaseHandle()

    const database = await openUserDataDatabase()
    expect(database?.version).toBe(USER_DATA_DATABASE_VERSION)
    expect(Array.from(database?.objectStoreNames ?? [])).toEqual(expect.arrayContaining([
      LEGACY_PENDING_STORE,
      LEGACY_ARCHIVE_STORE,
      LEGACY_DEDUP_STORE,
      MIGRATION_DECISION_STORE,
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
      ANNOTATION_IMPORTS_STORE,
    ]))

    const result = await runUserDataTransaction(
      new IdentityLease({
        serverClientDataNamespace: 'server-A',
        physicalNamespace: 'physical-A',
        localEpoch: 1,
      }),
      'inspect v4 stores',
      [LEGACY_PENDING_STORE, ANNOTATION_OPS_STORE, ANNOTATED_LINKS_STORE],
      'readonly',
      (transaction, _operation, setResult) => {
        const pending = transaction.objectStore(LEGACY_PENDING_STORE).get('pins')
        pending.onsuccess = () => setResult(pending.result)
        expect(transaction.objectStore(ANNOTATION_OPS_STORE).indexNames.contains(
          ANNOTATION_OPS_ID_INDEX,
        )).toBe(true)
        expect(transaction.objectStore(ANNOTATED_LINKS_STORE).indexNames.contains(
          ANNOTATED_LINKS_NAMESPACE_INDEX,
        )).toBe(true)
      },
    )
    expect(result).toEqual({ ok: true, value: expect.objectContaining({ id: 'pins' }) })
  })

  it('migrates v4 operations and compacted projections into durable thought recovery rows', async () => {
    const oldDatabase = await createVersionFourDatabase()
    oldDatabase.close()
    resetUserDataDatabaseHandle()

    const database = await openUserDataDatabase()
    expect(database?.version).toBe(USER_DATA_DATABASE_VERSION)

    // The v4 log stays byte-identical. The v6 view is the only writable
    // repair surface, even during a direct v4 -> v6 upgrade.
    expect(await readAllStore(ANNOTATION_OPS_STORE)).toEqual([
      expect.objectContaining({ sequence: 1, opId: 'legacy-op-1' }),
    ])
    expect(await readAllStore(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_SYNC_STATE_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ annotationId: 'a1', operationKind: 'add', repair: true }),
    ]))
    expect(await readAllStore(THOUGHT_REPAIR_MANIFEST_STORE)).toEqual([expect.objectContaining({
      namespace: 'physical-A', schemaVersion: 6, complete: true,
    })])
    expect(database?.objectStoreNames.contains(THOUGHT_OUTBOX_STORE)).toBe(true)
  })

  it('derives v4 update operations into the v6-only dispatch queue', async () => {
    const oldDatabase = await createVersionFourDatabase({ includeUpdate: true })
    oldDatabase.close()
    resetUserDataDatabaseHandle()

    await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
    expect(await readAllStore(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({
        opId: 'legacy-op-update',
        operationKind: 'update',
        patch: { note: 'updated legacy thought', source: 'ai', updatedAt: 2 },
      }),
    ]))
    const update = (await readAllStore(THOUGHT_REPAIR_READY_STORE)).find((row) =>
      (row as { opId?: string }).opId === 'legacy-op-update') as { status?: string } | undefined
    expect(update?.status).not.toBe('blocked')
  })

  it('makes direct v4 and already-v5 repair output byte-equivalent', async () => {
    const v4 = await createVersionFourDatabase({ includeUpdate: true })
    v4.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
    const v4Result = await snapshotDatabase((await openUserDataDatabase())!, [
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_QUARANTINE_STORE,
      THOUGHT_REPAIR_MANIFEST_STORE,
    ])

    await deleteDatabase()
    const v5 = await createVersionFiveDatabase()
    v5.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
    const v5Result = await snapshotDatabase((await openUserDataDatabase())!, [
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_QUARANTINE_STORE,
      THOUGHT_REPAIR_MANIFEST_STORE,
    ])
    expect(v5Result).toEqual(v4Result)
  })

  it('preserves every v4 and v5 store key and structured-clone value', async () => {
    for (const createLegacy of [
      () => createVersionFourDatabase({ includeUpdate: true }),
      () => createVersionFiveDatabase(),
    ]) {
      const legacy = await createLegacy()
      const legacyStores = Array.from(legacy.objectStoreNames)
      const before = await snapshotDatabaseKeysAndValues(legacy, legacyStores)
      legacy.close()
      resetUserDataDatabaseHandle()
      const upgraded = await openUserDataDatabase()
      if (!upgraded) throw new Error('database did not upgrade')
      expect(legacyStores.every((store) => upgraded.objectStoreNames.contains(store))).toBe(true)
      expect(await snapshotDatabaseKeysAndValues(upgraded, legacyStores)).toEqual(before)
      await deleteDatabase()
    }
  })

  it('allocates a post-upgrade local operation after every reserved repair clock', async () => {
    const legacy = await createVersionFourDatabase({ includeUpdate: true })
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
    const repairRows = await readAllStore(THOUGHT_REPAIR_READY_STORE) as Array<{ logicalClock: number }>
    const repairFloor = Math.max(...repairRows.map((row) => row.logicalClock))
    expect(repairFloor).toBeGreaterThan(0)

    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'post-upgrade-add',
      linkId: 'link-2',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'post-upgrade-annotation', blockKey: 'content-document', start: 0, end: 4,
        text: 'fresh', note: 'newer local edit', source: 'self', createdAt: 20, updatedAt: 20,
      },
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    const regularRows = await readAllStore(THOUGHT_OUTBOX_STORE) as Array<{
      opId: string
      logicalClock: number
    }>
    expect(regularRows).toEqual([expect.objectContaining({
      opId: 'post-upgrade-add',
      logicalClock: repairFloor + 1,
    })])
  })

  it('never normalizes receipt-backed v5 rows during a later local edit', async () => {
    const legacy = await createVersionFiveDatabase()
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
    const legacyIDs = new Set(['legacy-op-1', 'legacy-op-update'])
    const before = (await readAllStore(THOUGHT_OUTBOX_STORE)).filter((row) =>
      row !== null && typeof row === 'object' && legacyIDs.has((row as { opId?: string }).opId ?? ''))

    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    await expect(commitAnnotationOperation(lease, {
      kind: 'add', opId: 'fresh-after-v5', linkId: 'L2',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'fresh-a2', blockKey: 'content-document', start: 0, end: 4, text: 'new', note: '',
        source: 'self', createdAt: 20, updatedAt: 20,
      },
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    const after = (await readAllStore(THOUGHT_OUTBOX_STORE)).filter((row) =>
      row !== null && typeof row === 'object' && legacyIDs.has((row as { opId?: string }).opId ?? ''))
    expect(after).toEqual(before)
  })

  it.each(['empty', 'partial', 'tampered'] as const)(
    'fails closed when the repair source receipt set is %s',
    async (mode) => {
      const legacy = await createVersionFiveDatabase()
      legacy.close()
      resetUserDataDatabaseHandle()
      await expect(openUserDataDatabase()).resolves.toMatchObject({ version: USER_DATA_DATABASE_VERSION })
      await mutateStoreRows(THOUGHT_REPAIR_SOURCE_STORE, (store, rows) => {
        const source = rows.find((row) => row !== null && typeof row === 'object' &&
          (row as { recordKind?: string }).recordKind !== 'source-seal') as
          | ({ key: IDBValidKey; sourceChecksum: string } & Record<string, unknown>)
          | undefined
        if (mode === 'empty') store.clear()
        else if (mode === 'partial' && source) store.delete(source.key)
        else if (mode === 'tampered' && source) {
          store.put({ ...source, sourceChecksum: '0'.repeat(64) })
        }
      })
      resetUserDataDatabaseHandle()
      await expect(openUserDataDatabase()).resolves.toBeNull()
    },
  )

  it('rebuilds ready and manifest rows from untouched v4 legacy inputs after tampering', async () => {
    const legacy = await createVersionFourDatabase({ includeUpdate: true })
    const legacyBefore = await snapshotDatabase(legacy, [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
    ])
    legacy.close()
    resetUserDataDatabaseHandle()
    const upgraded = await openUserDataDatabase()
    if (!upgraded) throw new Error('database did not upgrade')
    const expected = await snapshotDatabase(upgraded, [
      THOUGHT_REPAIR_SOURCE_STORE,
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_QUARANTINE_STORE,
      THOUGHT_REPAIR_MANIFEST_STORE,
    ])

    for (const store of [THOUGHT_REPAIR_READY_STORE, THOUGHT_REPAIR_MANIFEST_STORE]) {
      await clearStore(store)
      resetUserDataDatabaseHandle()
      const rebuilt = await openUserDataDatabase()
      if (!rebuilt) throw new Error('database did not rebuild')
      expect(await snapshotDatabase(rebuilt, [
        THOUGHT_REPAIR_SOURCE_STORE,
        THOUGHT_REPAIR_READY_STORE,
        THOUGHT_REPAIR_QUARANTINE_STORE,
        THOUGHT_REPAIR_MANIFEST_STORE,
      ])).toEqual(expected)
    }
    expect(await snapshotDatabase((await openUserDataDatabase())!, [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
    ])).toEqual(legacyBefore)
  })

  it('keeps durable quarantine out of sync and publishes only namespace-scoped counts', async () => {
    const legacy = await createVersionFourDatabase()
    await addInvalidQuoteOperation(legacy)
    const legacyBefore = await snapshotDatabase(legacy, [ANNOTATION_OPS_STORE])
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()
    const expectedQuarantine = await readAllStore(THOUGHT_REPAIR_QUARANTINE_STORE)
    expect(expectedQuarantine).toEqual([expect.objectContaining({
      namespace: 'physical-A', annotationId: 'bad-quote', reason: 'invalid_quote',
    })])

    await clearStore(THOUGHT_REPAIR_QUARANTINE_STORE)
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()
    expect(await readAllStore(THOUGHT_REPAIR_QUARANTINE_STORE)).toEqual(expectedQuarantine)

    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    const observed: unknown[] = []
    const listener = (event: Event) => observed.push((event as CustomEvent).detail)
    window.addEventListener('webtag:thought-repair-quarantine', listener)
    const pushThoughtOps = vi.fn(async (request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]) => ({
      ok: true as const,
      data: request.ops.map((operation, index) => ({
        contract_version: 1 as const, op_id: operation.op_id, sequence: index + 1,
        disposition: 'applied' as const,
        submitted_key: { logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id },
        current_winner_key: { logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id },
      })),
    }))
    try {
      await expect(syncThoughts(lease, {
        pushThoughtOps,
        syncThoughts: async () => ({ ok: true as const, data: { contract_version: 1 as const, items: [] } }),
      } as unknown as IdentityBoundReaderClient)).resolves.toMatchObject({ status: 'synced' })
    } finally {
      window.removeEventListener('webtag:thought-repair-quarantine', listener)
    }
    const dispatched = pushThoughtOps.mock.calls.flatMap(([request]) => request.ops)
    expect(dispatched.map((operation) => operation.annotation_id)).not.toContain('bad-quote')
    expect(observed.length).toBeGreaterThan(0)
    for (const detail of observed) {
      expect(detail).toEqual({
        namespace: 'physical-A', reasons: [{ reason: 'invalid_quote', count: 1 }],
      })
    }
    expect(await snapshotDatabase((await openUserDataDatabase())!, [ANNOTATION_OPS_STORE])).toEqual(legacyBefore)
  })

  it('suppresses a malformed v5 source without blocking unrelated ready rows', async () => {
    const legacy = await createVersionFiveDatabase()
    await new Promise<void>((resolve, reject) => {
      const transaction = legacy.transaction(THOUGHT_OUTBOX_STORE, 'readwrite')
      transaction.objectStore(THOUGHT_OUTBOX_STORE).put({
        key: ['physical-A', 4], namespace: 'physical-A', sequence: 4, opId: 'bad-v5',
        deviceId: '', contractVersion: 0, logicalClock: 0, operationKind: 'add',
        annotationId: 'bad-v5-a', hostKind: 'link', hostId: 'L1', linkId: 'L1',
        target: { kind: 'saved-content', contentRevision: 7 }, targetKey: 'saved-content:7',
        annotation: {
          id: 'bad-v5-a', blockKey: 'content-document', start: 0, end: 1, text: 'x', note: '',
          source: 'self', createdAt: 1, updatedAt: 1, sourceContentRevision: 7,
          quote: { exact: 7 },
        },
        createdAt: 0, attemptCount: 0,
      })
      transaction.oncomplete = () => resolve()
      transaction.onerror = () => reject(transaction.error)
      transaction.onabort = () => reject(transaction.error)
    })
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    const pushThoughtOps = vi.fn(async (request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]) => ({
      ok: true as const,
      data: request.ops.map((operation, index) => ({
        contract_version: 1 as const, op_id: operation.op_id, sequence: index + 1,
        disposition: 'applied' as const,
        submitted_key: {
          logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id,
        },
        current_winner_key: {
          logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id,
        },
      })),
    }))
    const malformedV5Result = await syncThoughts(lease, {
      pushThoughtOps,
      syncThoughts: async () => ({ ok: true as const, data: { contract_version: 1 as const, items: [] } }),
    } as unknown as IdentityBoundReaderClient)
    expect(malformedV5Result).toMatchObject({ status: 'synced' })
    const dispatched = pushThoughtOps.mock.calls.flatMap(([request]) => request.ops)
    expect(dispatched.length).toBeGreaterThan(0)
    expect(dispatched.map((operation) => operation.op_id)).not.toContain('bad-v5')
  })

  it('never sends quarantine during automatic, manual, online, or cross-tab sync', async () => {
    const legacy = await createVersionFourDatabase()
    await addInvalidQuoteOperation(legacy)
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()

    type MessageListener = (event: MessageEvent<unknown>) => void
    const listeners: MessageListener[] = []
    const posted: unknown[] = []
    class FakeBroadcastChannel {
      constructor(readonly name: string) {}
      addEventListener(type: string, listener: EventListener) {
        if (type === 'message') listeners.push(listener as MessageListener)
      }
      removeEventListener(type: string, listener: EventListener) {
        if (type !== 'message') return
        const index = listeners.indexOf(listener as MessageListener)
        if (index >= 0) listeners.splice(index, 1)
      }
      postMessage(value: unknown) { posted.push(value) }
      close() {}
    }
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    const pushed: Array<readonly { annotation_id: string }[]> = []
    const pushThoughtOps = vi.fn(async (request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]) => {
      pushed.push(request.ops)
      return {
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          contract_version: 1 as const, op_id: operation.op_id, sequence: index + 1,
          disposition: 'applied' as const,
          submitted_key: {
            logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id,
          },
          current_winner_key: {
            logical_clock: operation.logical_clock, device_id: operation.device_id, op_id: operation.op_id,
          },
        })),
      }
    })
    const pull = vi.fn(async () => ({
      ok: true as const, data: { contract_version: 1 as const, items: [] },
    }))
    const client = { pushThoughtOps, syncThoughts: pull } as unknown as IdentityBoundReaderClient
    const now = Date.now()
    vi.spyOn(Date, 'now').mockReturnValue(now + 2_000)
    const stop = startThoughtSync(lease, client)
    try {
      await vi.waitFor(() => expect(pull).toHaveBeenCalledTimes(1))
      await expect(syncThoughts(lease, client)).resolves.toMatchObject({ status: 'idle' })
      window.dispatchEvent(new Event('online'))
      await vi.waitFor(() => expect(pull.mock.calls.length).toBeGreaterThanOrEqual(3))
      expect(listeners).toHaveLength(1)
      listeners[0](new MessageEvent('message', { data: {
        kind: 'thought-sync-invalidation', namespace: 'physical-A', invalidation: 'outbox',
      } }))
      await vi.waitFor(() => expect(pull.mock.calls.length).toBeGreaterThanOrEqual(4))
    } finally {
      stop()
    }
    expect(pushed.flat().map((operation) => operation.annotation_id)).not.toContain('bad-quote')
    expect(posted).toEqual(expect.arrayContaining([{
      kind: 'thought-repair-quarantine',
      namespace: 'physical-A',
      reasons: [{ reason: 'invalid_quote', count: 1 }],
    }]))
    expect(JSON.stringify(posted)).not.toContain('bad-quote')
  })

  it('rolls every derived durable write back to the v4 snapshot when injected to fail', async () => {
    const captureBaseline = async () => {
      const source = await createVersionFourDatabase({ includeUpdate: true })
      source.close()
      resetUserDataDatabaseHandle()
      const original = IDBObjectStore.prototype.put
      let writes = 0
      IDBObjectStore.prototype.put = new Proxy(original, {
        apply(target, thisArgument, args) {
          writes += 1
          return Reflect.apply(target, thisArgument, args) as IDBRequest
        },
      })
      let upgraded: IDBDatabase | null = null
      try {
        upgraded = await openUserDataDatabase()
        expect(upgraded).not.toBeNull()
      } finally {
        IDBObjectStore.prototype.put = original
      }
      if (!upgraded) throw new Error('baseline database did not upgrade')
      const stores = Array.from(upgraded.objectStoreNames)
      const snapshot = await snapshotDatabaseKeysAndValues(upgraded, stores)
      await deleteDatabase()
      return { writes, stores, snapshot }
    }
    const baseline = await captureBaseline()
    expect(baseline.writes).toBeGreaterThan(0)
    for (let ordinal = 1; ordinal <= baseline.writes; ordinal += 1) {
      const source = await createVersionFourDatabase({ includeUpdate: true })
      const legacyStores = Array.from(source.objectStoreNames)
      const before = await snapshotDatabaseKeysAndValues(source, legacyStores)
      source.close()
      resetUserDataDatabaseHandle()
      const original = IDBObjectStore.prototype.put
      let writes = 0
      IDBObjectStore.prototype.put = new Proxy(original, {
        apply(target, thisArgument, args) {
          writes += 1
          if (writes === ordinal) throw new Error(`injected durable write ${ordinal}`)
          return Reflect.apply(target, thisArgument, args) as IDBRequest
        },
      })
      try {
        await expect(openUserDataDatabase()).resolves.toBeNull()
      } finally {
        IDBObjectStore.prototype.put = original
      }
      const rolledBack = await openRequest(indexedDB.open(DATABASE_NAME, 4))
      expect(rolledBack.version).toBe(4)
      expect(Array.from(rolledBack.objectStoreNames)).toEqual(legacyStores)
      expect(await snapshotDatabaseKeysAndValues(rolledBack, legacyStores)).toEqual(before)
      rolledBack.close()
      resetUserDataDatabaseHandle()
      const recovered = await openUserDataDatabase()
      expect(recovered).toMatchObject({ version: USER_DATA_DATABASE_VERSION })
      if (!recovered) throw new Error('database did not recover')
      expect(Array.from(recovered.objectStoreNames)).toEqual(baseline.stores)
      expect(await snapshotDatabaseKeysAndValues(recovered, baseline.stores)).toEqual(baseline.snapshot)
      await deleteDatabase()
    }
  })

  it('persists repair retry/ack state without ever reviving its immutable v4 source', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const legacy = await createVersionFourDatabase({ includeUpdate: true })
    legacy.close()
    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A', physicalNamespace: 'physical-A', localEpoch: 1,
    })
    type PushRequest = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]
    let call = 0
    const pushThoughtOps = vi.fn(async (request: PushRequest) => {
      call += 1
      if (call === 1) return {
        ok: false as const,
        error: { kind: 'network-unreachable' as const, message: 'offline' },
      }
      return {
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          contract_version: 1 as const,
          op_id: operation.op_id,
          sequence: index + 1,
          disposition: 'applied' as const,
          submitted_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
          current_winner_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
        })),
      }
    })
    const client = {
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({ ok: true as const, data: { contract_version: 1 as const, items: [] } })),
    } as unknown as IdentityBoundReaderClient

    await expect(syncThoughts(lease, client)).resolves.toMatchObject({
      status: 'failed', pending: 2, retryAt: 2000,
    })
    expect(await readAllStore(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ attemptCount: 1, nextAttemptAt: 2000, repair: true }),
    ]))

    now.mockReturnValue(2000)
    await expect(syncThoughts(lease, client)).resolves.toMatchObject({
      status: 'synced', pushed: 2, pending: 0,
    })
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_REPAIR_ACK_STORE)).toHaveLength(2)

    resetUserDataDatabaseHandle()
    await expect(openUserDataDatabase()).resolves.not.toBeNull()
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual([])
    const neverPush = vi.fn()
    const afterReload = {
      pushThoughtOps: neverPush,
      syncThoughts: vi.fn(async () => ({ ok: true as const, data: { contract_version: 1 as const, items: [] } })),
    } as unknown as IdentityBoundReaderClient
    await expect(syncThoughts(lease, afterReload)).resolves.toMatchObject({ pending: 0 })
    expect(neverPush).not.toHaveBeenCalled()
    expect(await readAllStore(THOUGHT_REPAIR_READY_STORE)).toEqual([])
    expect(await readAllStore(THOUGHT_REPAIR_ACK_STORE)).toHaveLength(2)
  })

  it('rolls back a failed versionchange and succeeds on the next open', async () => {
    const oldDatabase = await createVersionThreeDatabase()
    oldDatabase.close()
    const originalCreate = IDBDatabase.prototype.createObjectStore
    IDBDatabase.prototype.createObjectStore = new Proxy(originalCreate, {
      apply(target, thisArgument, argumentsList) {
        if (argumentsList[0] === ANNOTATION_MATERIALIZED_STORE) {
          throw new Error('injected v4 upgrade failure')
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
    expect(versionThree.objectStoreNames.contains(ANNOTATION_OPS_STORE)).toBe(false)
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
