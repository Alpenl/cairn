import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  compactAnnotationOperations,
  commitAnnotationOperation,
  readAnnotationSnapshot,
} from './annotation-store'
import { listAnnotationOperationsForTest } from '../../test/annotation-operations'
import {
  annotationTargetKey,
  type AnnotationTarget,
} from '../annotation-domain'
import {
  type SavedContentAnnotationAddDraft,
} from './annotation-types'
import {
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
} from './idb'

const TARGET = {
  kind: 'saved-content',
  contentRevision: 7,
} as const satisfies AnnotationTarget

function identity(namespace = 'physical-A'): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

function annotationDraft(id: string, note = ''): SavedContentAnnotationAddDraft {
  return {
    id,
    blockKey: 'content-document',
    start: 0,
    end: 4,
    text: 'text',
    note,
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
  }
}

async function add(lease: IdentityLease, opId: string, id: string): Promise<void> {
  const result = await commitAnnotationOperation(lease, {
    kind: 'add',
    opId,
    linkId: 'L1',
    target: TARGET,
    draft: annotationDraft(id),
  })
  expect(result).toMatchObject({ ok: true, value: { status: 'committed' } })
}

async function writeRawImportRecord(record: Record<string, unknown>): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('user-data database unavailable')
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_IMPORTS_STORE, 'readwrite')
    transaction.objectStore(ANNOTATION_IMPORTS_STORE).add(record)
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function readRawImportRecord(key: IDBValidKey): Promise<unknown> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('user-data database unavailable')
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_IMPORTS_STORE, 'readonly')
    const request = transaction.objectStore(ANNOTATION_IMPORTS_STORE).get(key)
    transaction.oncomplete = () => resolve(request.result)
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

function targetKey(): string {
  const key = annotationTargetKey(TARGET)
  if (!key) throw new Error('test target must have a canonical key')
  return key
}

async function clearMaterializedProjection(lease: IdentityLease): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('user-data database unavailable')
  const namespace = lease.context.physicalNamespace
  const key = targetKey()
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_MATERIALIZED_STORE, 'readwrite')
    transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).delete(IDBKeyRange.bound(
      [namespace, 'L1', key],
      [namespace, 'L1', key, []],
    ))
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function clearCompactedReplayBase(lease: IdentityLease): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('user-data database unavailable')
  const stateKey = [lease.context.physicalNamespace, 'L1', targetKey()]
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(ANNOTATION_LINK_STATE_STORE, 'readwrite')
    const store = transaction.objectStore(ANNOTATION_LINK_STATE_STORE)
    const request = store.get(stateKey)
    request.onsuccess = () => {
      if (!request.result) {
        transaction.abort()
        return
      }
      store.put({ ...request.result, snapshot: [] })
    }
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  await deleteUserDataDatabase()
})

describe('annotation operation-log compaction', () => {
  it('atomically snapshots through a high-water mark and removes only covered ops', async () => {
    const lease = identity()
    const otherNamespace = identity('physical-B')
    await add(lease, 'add-a', 'a')
    await add(lease, 'add-b', 'b')
    await commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'update-a',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'a',
      patch: { note: 'updated', updatedAt: 2 },
    })
    await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'delete-b',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'b',
    })
    await add(lease, 'add-c', 'c')
    await add(otherNamespace, 'other-namespace', 'private')

    const compacted = await compactAnnotationOperations(lease, 'L1', TARGET, {
      threshold: 2,
    })

    expect(compacted).toEqual({
      ok: true,
      value: {
        status: 'compacted',
        highWaterMark: 5,
        operationsDeleted: 5,
        annotationStoreVersion: 5,
      },
    })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET)).resolves.toEqual({
      ok: true,
      value: [],
    })
    await expect(listAnnotationOperationsForTest(otherNamespace, 'L1', TARGET))
      .resolves.toMatchObject({ ok: true, value: [{ opId: 'other-namespace', sequence: 6 }] })
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: {
        annotationStoreVersion: 5,
        annotations: [{ id: 'a', note: 'updated' }, { id: 'c' }],
      },
    })

    resetUserDataDatabaseHandle()
    await expect(commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'restore-b-after-reopen',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'b',
      patch: { note: 'restored after compacted tombstone', updatedAt: 7 },
    })).resolves.toMatchObject({
      ok: true,
      value: { status: 'committed', sequence: 7 },
    })
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: {
        annotationStoreVersion: 7,
        annotations: [
          { id: 'a' },
          { id: 'b', note: 'restored after compacted tombstone' },
          { id: 'c' },
        ],
      },
    })
  })

  it('queues a concurrent writer after the selected high-water mark', async () => {
    const lease = identity()
    await add(lease, 'seed-a', 'a')
    await add(lease, 'seed-b', 'b')
    await add(lease, 'seed-c', 'c')

    const compacting = compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 })
    await Promise.resolve()
    const writing = commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'post-high-water',
      linkId: 'L1',
      target: TARGET,
      draft: annotationDraft('later'),
    })
    const [compacted, written] = await Promise.all([compacting, writing])

    expect(compacted).toMatchObject({
      ok: true,
      value: { status: 'compacted', highWaterMark: 3, operationsDeleted: 3 },
    })
    expect(written).toMatchObject({
      ok: true,
      value: { status: 'committed', sequence: 4, annotationStoreVersion: 4 },
    })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: [{ opId: 'post-high-water', sequence: 4 }],
    })
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: {
        annotationStoreVersion: 4,
        annotations: [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'later' }],
      },
    })
  })

  it('replays the compacted base and tail when the materialized projection is gone', async () => {
    const lease = identity()
    await add(lease, 'add-a', 'a')
    await add(lease, 'add-b', 'b')
    await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'delete-b',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'b',
    })
    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 }))
      .resolves.toMatchObject({
        ok: true,
        value: { status: 'compacted', highWaterMark: 3 },
      })
    await expect(commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'restore-b-in-tail',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'b',
      patch: { note: 'restored from tombstone', updatedAt: 4 },
    })).resolves.toMatchObject({
      ok: true,
      value: { status: 'committed', sequence: 4 },
    })

    await clearMaterializedProjection(lease)

    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: {
        annotationStoreVersion: 4,
        annotations: [
          { id: 'a' },
          { id: 'b', note: 'restored from tombstone', updatedAt: 4 },
        ],
      },
    })
  })

  it('fails closed when a compacted replay base is emptied', async () => {
    const lease = identity()
    await add(lease, 'add-a', 'a')
    await add(lease, 'add-b', 'b')
    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 }))
      .resolves.toMatchObject({ ok: true, value: { status: 'compacted' } })
    await clearMaterializedProjection(lease)
    await clearCompactedReplayBase(lease)

    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toEqual({ ok: false })
  })

  it('keeps compacted operation IDs idempotent without restoring covered log rows', async () => {
    const lease = identity()
    const original = {
      kind: 'add',
      opId: 'retry-after-compaction',
      linkId: 'L1',
      target: TARGET,
      draft: annotationDraft('a'),
    } as const
    const committed = await commitAnnotationOperation(lease, original)
    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 }))
      .resolves.toMatchObject({ ok: true, value: { status: 'compacted' } })

    const duplicate = await commitAnnotationOperation(lease, original)
    const collision = await commitAnnotationOperation(lease, {
      ...original,
      draft: annotationDraft('changed-id'),
    })

    expect(committed).toMatchObject({ ok: true, value: { sequence: 1 } })
    expect(duplicate).toMatchObject({
      ok: true,
      value: {
        status: 'duplicate',
        sequence: 1,
        annotationStoreVersion: 1,
      },
    })
    expect(collision).toEqual({ ok: true, value: { status: 'op-id-conflict' } })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET)).resolves.toEqual({
      ok: true,
      value: [],
    })
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: { annotationStoreVersion: 1, annotations: [{ id: 'a' }] },
    })

    await expect(commitAnnotationOperation(lease, {
      ...original,
      linkId: 'other-link',
      target: { kind: 'saved-content', contentRevision: 9 },
      draft: annotationDraft('changed-id'),
    })).resolves.toEqual({ ok: true, value: { status: 'op-id-conflict' } })

    const otherNamespace = identity('physical-B')
    await expect(commitAnnotationOperation(otherNamespace, {
      ...original,
      linkId: 'other-link',
      target: { kind: 'saved-content', contentRevision: 9 },
      draft: annotationDraft('changed-id'),
    })).resolves.toMatchObject({
      ok: true,
      value: { status: 'committed', sequence: 2 },
    })
    await expect(readAnnotationSnapshot(
      otherNamespace,
      'other-link',
      { kind: 'saved-content', contentRevision: 9 },
    )).resolves.toMatchObject({
      ok: true,
      value: { annotations: [{ id: 'changed-id' }] },
    })
  })

  it.each([
    {
      label: 'exact',
      signature: JSON.stringify([
        'add',
        'a',
        'content-document',
        0,
        4,
        'text',
        '',
        'self',
        1,
        1,
        7,
        null,
      ]),
    },
    { label: 'conflicting', signature: 'different immutable payload' },
  ])('never overwrites an $label pre-existing operation receipt', async ({ signature }) => {
    const lease = identity()
    await add(lease, 'seed-a', 'a')
    const targetKey = annotationTargetKey(TARGET)
    if (!targetKey) throw new Error('test target must have a canonical key')
    const key: IDBValidKey = [
      'operation-receipt',
      'physical-A',
      'seed-a',
      'L1',
      targetKey,
    ]
    const receipt = {
      key,
      kind: 'operation-receipt',
      opId: 'seed-a',
      namespace: 'physical-A',
      linkId: 'L1',
      targetKey,
      annotationId: 'a',
      operationKind: 'add',
      sequence: 1,
      signature,
    }
    await writeRawImportRecord(receipt)

    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 }))
      .resolves.toEqual({ ok: false })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: [{ opId: 'seed-a', sequence: 1 }],
    })
    await expect(readRawImportRecord(key)).resolves.toEqual(receipt)
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: { annotations: [{ id: 'a' }] },
    })
  })

  it('leaves the old log replayable when any covered-op deletion aborts', async () => {
    const lease = identity()
    await add(lease, 'seed-a', 'a')
    await add(lease, 'seed-b', 'b')
    await add(lease, 'seed-c', 'c')
    const before = await listAnnotationOperationsForTest(lease, 'L1', TARGET)

    const originalDelete = IDBObjectStore.prototype.delete
    vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
      this: IDBObjectStore,
      query: IDBValidKey | IDBKeyRange,
    ) {
      if (this.name === ANNOTATION_OPS_STORE) {
        throw new Error('injected compaction delete failure')
      }
      return originalDelete.call(this, query)
    })

    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 0 }))
      .resolves.toEqual({ ok: false })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET)).resolves.toEqual(before)
    await expect(readAnnotationSnapshot(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: {
        annotations: [{ id: 'a' }, { id: 'b' }, { id: 'c' }],
      },
    })
  })

  it('skips below-threshold logs without changing replay state', async () => {
    const lease = identity()
    await add(lease, 'seed-a', 'a')

    await expect(compactAnnotationOperations(lease, 'L1', TARGET, { threshold: 1 }))
      .resolves.toEqual({
        ok: true,
        value: {
          status: 'skipped',
          highWaterMark: 0,
          operationsDeleted: 0,
          annotationStoreVersion: 1,
        },
      })
    await expect(listAnnotationOperationsForTest(lease, 'L1', TARGET))
      .resolves.toMatchObject({ ok: true, value: [{ opId: 'seed-a' }] })
  })
})
