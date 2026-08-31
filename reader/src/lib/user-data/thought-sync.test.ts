import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ReaderThoughtResponse } from '../api/types'
import type { IdentityBoundReaderClient } from '../api/client'
import type { ApiError } from '@webtag/api'
import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  commitAnnotationOperation,
  readAnnotationSnapshot,
} from './annotation-store'
import { commitThoughtHistoryAction } from './thought-history-actions'
import {
  syncThoughts,
  startThoughtSync,
  getThoughtSyncController,
  THOUGHT_SYNC_POLL_MS,
  listThoughtConflicts,
  listRemoteThoughts,
  cacheServerThoughtPage,
  selectThoughtReadModel,
  sortThoughtReadModel,
} from './thought-sync'
import {
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_SYNC_STATE_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from './idb'
import type { ThoughtOutboxRecord } from './thought-types'

const TARGET = {
  kind: 'saved-content',
  contentRevision: 7,
} as const

const NOTE_TARGET = {
  kind: 'note',
  noteRevision: 4,
} as const

const WIRE_TARGET = {
  kind: 'saved-content',
  host_id: 'L1',
  version: { content_revision: 7 },
  block_key: 'content-document',
  range: { start: 0, end: 4 },
}

function identity(namespace: string, epoch = 1): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: epoch,
  })
}

function annotationDraft(id: string) {
  return {
    id,
    blockKey: 'content-document' as const,
    start: 0,
    end: 4,
    text: 'text',
    note: 'thought',
    source: 'self' as const,
    createdAt: 1,
    updatedAt: 1,
    quote: { exact: 'text', prefix: '', suffix: '' },
  }
}

async function addLocalThought(lease: IdentityLease, opId = 'op-1', id = 'a1'): Promise<void> {
  await expect(commitAnnotationOperation(lease, {
    kind: 'add',
    opId,
    linkId: 'L1',
    target: TARGET,
    draft: annotationDraft(id),
  })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
}

async function addLocalNoteThought(lease: IdentityLease, opId = 'note-op', id = 'note-a1') {
  await expect(commitAnnotationOperation(lease, {
    kind: 'add',
    opId,
    linkId: 'N1',
    target: NOTE_TARGET,
    draft: {
      id,
      start: 0,
      end: 4,
      text: 'note',
      note: 'note thought',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      quote: { exact: 'note', prefix: '', suffix: '' },
    },
  })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
}

async function addHistoryReattach(
  lease: IdentityLease,
  opId = 'history-reattach',
  winnerKey = {
    logicalClock: 11,
    deviceId: 'history-server-device',
    opId: 'history-server-winner',
  },
): Promise<void> {
  await expect(commitThoughtHistoryAction(lease, {
    action: 'reattach',
    opId,
    thought: {
      id: 'history-thought',
      hostKind: 'link',
      hostId: 'L1',
      target: WIRE_TARGET,
      quote: {
        exact: 'text',
        start: 0,
        end: 4,
        prefix: '',
        suffix: '',
        block_key: 'content-document',
      },
      body: 'frozen history body',
      source: 'reader-history',
      lastSequence: 11,
      winnerKey,
      originalHostSnapshot: { title: 'Original title', content: 'Original content' },
    },
    targetHostKind: 'note',
    targetHostId: 'N1',
    expectedHostRevision: 4,
  })).resolves.toMatchObject({ ok: true, value: { status: 'committed', opId } })
}

function client(overrides: {
  pushThoughtOps?: (
    ...args: Parameters<IdentityBoundReaderClient['pushThoughtOps']>
  ) => Promise<unknown>
  syncThoughts?: (
    ...args: Parameters<IdentityBoundReaderClient['syncThoughts']>
  ) => Promise<unknown>
}): IdentityBoundReaderClient {
  return {
    ...overrides,
    ...(overrides.pushThoughtOps
      ? {
          pushThoughtOps: async (
            request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0],
            options?: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[1],
          ) => {
            const result = await overrides.pushThoughtOps?.(request, options)
            if (!result || typeof result !== 'object') return result
            const response = result as Record<string, unknown>
            if (response.ok !== true || !Array.isArray(response.data)) return result
            return {
              ...response,
              data: response.data.map((ack: Record<string, unknown>) => {
                if (ack.contract_version === 1) return ack
                const operation = request.ops.find((candidate) => candidate.op_id === ack.op_id)
                if (!operation) return ack
                const key = {
                  logical_clock: operation.logical_clock,
                  device_id: operation.device_id,
                  op_id: operation.op_id,
                }
                return {
                  ...ack,
                  contract_version: 1,
                  disposition: 'applied',
                  submitted_key: key,
                  current_winner_key: key,
                }
              }),
            }
          },
        }
      : {}),
    ...(overrides.syncThoughts
      ? {
          syncThoughts: async (...args: Parameters<IdentityBoundReaderClient['syncThoughts']>) => {
            const result = await overrides.syncThoughts?.(...args)
            if (!result || typeof result !== 'object') return result
            const response = result as Record<string, unknown>
            const responseData = response.data
            if (
              response.ok !== true ||
              !responseData ||
              typeof responseData !== 'object' ||
              !Array.isArray((responseData as Record<string, unknown>).items)
            ) return result
            const data = responseData as Record<string, unknown> & { items: unknown[] }
            return {
              ...response,
              data: {
                ...data,
                contract_version: 1,
                items: data.items.map((item) => ({
                  ...(item as Record<string, unknown>),
                  contract_version: 1,
                  winner_key: (item as Record<string, unknown>).winner_key ?? {
                    logical_clock: (item as Record<string, unknown>).last_sequence,
                    device_id: 'remote-device',
                    op_id: `remote-${String((item as Record<string, unknown>).id)}-${String((item as Record<string, unknown>).last_sequence)}`,
                  },
                })),
              },
            }
          },
        }
      : {}),
  } as IdentityBoundReaderClient
}

async function readAll(storeName: string): Promise<unknown[]> {
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


async function deleteDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

async function seedPendingOperations(lease: IdentityLease, count: number): Promise<void> {
  if (count < 1) return
  await addLocalThought(lease, 'bulk-op-1', 'bulk-annotation-1')
  if (count === 1) return
  const database = await openUserDataDatabase()
  if (!database) throw new Error('IndexedDB must be available in this test')
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_OUTBOX_STORE, 'readwrite')
    const store = transaction.objectStore(THOUGHT_OUTBOX_STORE)
    const request = store.index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
      .getAll(lease.context.physicalNamespace) as IDBRequest<ThoughtOutboxRecord[]>
    request.onsuccess = () => {
      const seed = request.result[0]
      if (!seed) {
        transaction.abort()
        return
      }
      for (let sequence = 2; sequence <= count; sequence += 1) {
        store.put({
          ...seed,
          key: [lease.context.physicalNamespace, sequence],
          sequence,
          opId: `bulk-op-${sequence}`,
          logicalClock: sequence,
          annotationId: `bulk-annotation-${sequence}`,
        } satisfies ThoughtOutboxRecord)
      }
    }
    request.onerror = () => transaction.abort()
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

function remoteThought(id: string, sequence: number): ReaderThoughtResponse {
  return {
    contract_version: 1,
    id,
    host_kind: 'link',
    host_id: 'L1',
    link_id: 'L1',
    target: WIRE_TARGET,
    quote: {
      exact: 'text',
      prefix: '',
      suffix: '',
      block_key: 'content-document',
      start: 0,
      end: 4,
    },
    body: 'remote thought',
    source: 'self',
    deleted: false,
    last_sequence: sequence,
    winner_key: {
      logical_clock: sequence,
      device_id: 'remote-device',
      op_id: `remote-${id}-${sequence}`,
    },
    created_at: '2026-08-10T00:00:00.000Z',
    updated_at: '2026-08-10T00:00:00.000Z',
  }
}

function remoteThoughtAtClock(
  id: string,
  sequence: number,
  logicalClock: number,
): ReaderThoughtResponse {
  return {
    ...remoteThought(id, sequence),
    winner_key: {
      logical_clock: logicalClock,
      device_id: 'remote-device',
      op_id: `remote-${id}-${logicalClock}`,
    },
  }
}


afterEach(async () => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  localStorage.clear()
  await deleteDatabase()
})

describe('durable thought sync loop', () => {
  it('rolls back an allocated clock when a later projection write fails', async () => {
    const lease = identity('physical-A')
    const database = await openUserDataDatabase()
    if (!database) throw new Error('IndexedDB must be available in this test')
    const corruptKey = ['physical-A', 'L1', 'saved-content:7', 'rollback-thought']
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction(ANNOTATION_MATERIALIZED_STORE, 'readwrite')
      transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).put({
        key: corruptKey,
        namespace: 'wrong-namespace',
      })
      transaction.oncomplete = () => resolve()
      transaction.onerror = () => reject(transaction.error)
      transaction.onabort = () => reject(transaction.error)
    })

    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'rolled-back-op',
      linkId: 'L1',
      target: TARGET,
      draft: annotationDraft('rollback-thought'),
    })).resolves.toEqual({ ok: false })
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([])
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])

    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction(ANNOTATION_MATERIALIZED_STORE, 'readwrite')
      transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).delete(corruptKey)
      transaction.oncomplete = () => resolve()
      transaction.onerror = () => reject(transaction.error)
      transaction.onabort = () => reject(transaction.error)
    })
    await addLocalThought(lease, 'after-rollback', 'after-rollback')
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ opId: 'after-rollback', logicalClock: 1 }),
    ])
  })

  it('serializes 100 cross-context allocations and isolates identity floors', async () => {
    const firstTab = identity('physical-A')
    const secondTab = identity('physical-A')
    const commits = await Promise.all(Array.from({ length: 100 }, (_, index) =>
      commitAnnotationOperation(index % 2 === 0 ? firstTab : secondTab, {
        kind: 'add',
        opId: `parallel-op-${index}`,
        linkId: 'L1',
        target: TARGET,
        draft: annotationDraft(`parallel-thought-${index}`),
      })))
    expect(commits.every((result) => result.ok && result.value.status === 'committed')).toBe(true)

    const firstIdentityRows = (await readAll(THOUGHT_OUTBOX_STORE))
      .filter((row): row is { namespace: string; deviceId: string; logicalClock: number } =>
        typeof row === 'object' && row !== null &&
        (row as { namespace?: unknown }).namespace === 'physical-A')
    expect(firstIdentityRows).toHaveLength(100)
    expect(new Set(firstIdentityRows.map((row) => row.deviceId)).size).toBe(1)
    expect(firstIdentityRows.map((row) => row.logicalClock).sort((left, right) => left - right))
      .toEqual(Array.from({ length: 100 }, (_, index) => index + 1))

    resetUserDataDatabaseHandle()
    await expect(commitAnnotationOperation(identity('physical-A'), {
      kind: 'add',
      opId: 'after-reload',
      linkId: 'L1',
      target: TARGET,
      draft: annotationDraft('after-reload'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    await expect(commitAnnotationOperation(identity('physical-B'), {
      kind: 'add',
      opId: 'other-identity',
      linkId: 'L1',
      target: TARGET,
      draft: annotationDraft('other-identity'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ namespace: 'physical-A', opId: 'after-reload', logicalClock: 101 }),
      expect.objectContaining({ namespace: 'physical-B', opId: 'other-identity', logicalClock: 1 }),
    ]))
  })

  it('replays the exact operation key after a failed push and database reload', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const lease = identity('physical-A')
    await addLocalThought(lease, 'durable-retry')
    type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
    const submitted: WireOperation[] = []
    let attempt = 0
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => {
      const operation = request.ops[0]
      submitted.push(structuredClone(operation))
      attempt += 1
      if (attempt === 1) {
        return {
          ok: false,
          error: { kind: 'network-unreachable', message: 'retry later' },
        }
      }
      const key = {
        logical_clock: operation.logical_clock,
        device_id: operation.device_id,
        op_id: operation.op_id,
      }
      return {
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 17,
          disposition: 'applied',
          submitted_key: key,
          current_winner_key: key,
        }],
      }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'failed', pending: 1, retryAt: 2000 })
    resetUserDataDatabaseHandle()
    now.mockReturnValue(2000)
    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'synced', pushed: 1, pending: 0 })

    expect(submitted).toHaveLength(2)
    expect(submitted[1]).toEqual(submitted[0])
    expect(submitted[0]).toMatchObject({
      contract_version: 1,
      op_id: 'durable-retry',
      logical_clock: 1,
      device_id: expect.stringMatching(/^device-/),
    })
  })

  it('sends a history reattach as a narrow command and removes it only after its ACK', async () => {
    const lease = identity('physical-A')
    await addHistoryReattach(lease)
    type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
    const submitted: WireOperation[] = []
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => {
      const operation = structuredClone(request.ops[0])
      submitted.push(operation)
      return {
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 17,
          disposition: 'applied',
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
        }],
      }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'synced', pushed: 1, pending: 0 })
    expect(submitted).toHaveLength(1)
    expect(submitted[0]).toMatchObject({
      contract_version: 1,
      op_id: 'history-reattach',
      operation_kind: 'update',
      annotation_id: 'history-thought',
      host_kind: 'note',
      host_id: 'N1',
      target: {
        kind: 'note',
        host_id: 'N1',
        version: { note_revision: 4 },
      },
      payload: {
        reattach: {
          expected_last_sequence: 11,
          expected_host_revision: 4,
        },
      },
    })
    const payload = submitted[0]?.payload as Record<string, unknown>
    expect(payload).not.toHaveProperty('body')
    expect(payload).not.toHaveProperty('quote')
    expect(payload).not.toHaveProperty('source')
    expect(payload).not.toHaveProperty('annotation')
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual([])
  })

  it('blocks a history reattach revision conflict without losing its frozen candidate', async () => {
    const lease = identity('physical-A')
    await addHistoryReattach(lease)
    const pushThoughtOps = vi.fn(async () => ({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'revision_conflict',
        message: 'target host changed',
      },
    }))
    const syncRemote = vi.fn()

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'failed', pushed: 0, pending: 1 })
    expect(syncRemote).not.toHaveBeenCalled()
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        action: 'reattach',
        opId: 'history-reattach',
        status: 'blocked',
        blockedReason: 'other:revision_conflict:409',
        snapshot: expect.objectContaining({
          body: 'frozen history body',
          source: 'reader-history',
          quote: expect.objectContaining({ exact: 'text' }),
          originalHostSnapshot: { title: 'Original title', content: 'Original content' },
        }),
      }),
    ])
  })

  it('replays a history action after a lost acknowledgement and accepts its duplicate ACK', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const lease = identity('physical-A')
    await addHistoryReattach(lease, 'history-retry')
    type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
    const submitted: WireOperation[] = []
    let attempt = 0
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => {
      const operation = structuredClone(request.ops[0])
      submitted.push(operation)
      attempt += 1
      if (attempt === 1) {
        throw new Error('acknowledgement lost after the server accepted the operation')
      }
      return {
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 18,
          disposition: 'duplicate',
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
        }],
      }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'failed', pending: 1, retryAt: 2000 })
    now.mockReturnValue(2000)
    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'synced', pushed: 1, pending: 0 })

    expect(submitted).toHaveLength(2)
    expect(submitted[1]).toEqual(submitted[0])
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual([])
  })

  it('accepts a superseded ack while advancing the floor from its current winner', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease, 'superseded-op', 'superseded-thought')
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => {
      const operation = request.ops[0]
      return {
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 40,
          disposition: 'superseded',
          submitted_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
          current_winner_key: {
            logical_clock: 99,
            device_id: 'remote-winner',
            op_id: 'remote-winner-op',
          },
        }],
      }
    })

    await expect(syncThoughts(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))).resolves.toMatchObject({ status: 'synced', pushed: 1, pending: 0 })
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ logicalClockFloor: 99, lastAckSequence: 40 }),
    ])

    await addLocalThought(lease, 'after-superseded', 'after-superseded')
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ opId: 'after-superseded', logicalClock: 100 }),
    ])
  })

  it('blocks a superseded history candidate, retains its frozen evidence, and never retries it', async () => {
    const lease = identity('physical-A')
    await addHistoryReattach(lease, 'history-superseded')
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => {
      const operation = request.ops[0]
      return {
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 41,
          disposition: 'superseded',
          submitted_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
          current_winner_key: {
            logical_clock: 99,
            device_id: 'remote-history-winner',
            op_id: 'remote-history-winner-op',
          },
        }],
      }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))
    const readerClient = client({ pushThoughtOps, syncThoughts: syncRemote })

    await expect(syncThoughts(lease, readerClient))
      .resolves.toMatchObject({
        status: 'failed',
        pushed: 0,
        pending: 1,
        errorCode: 'superseded',
      })
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        action: 'reattach',
        opId: 'history-superseded',
        status: 'blocked',
        blockedReason: 'superseded',
        lastError: '历史想法操作已被较新的服务端版本覆盖。冻结候选已保留，请刷新后重试。',
        snapshot: expect.objectContaining({
          body: 'frozen history body',
          winnerKey: {
            logicalClock: 11,
            deviceId: 'history-server-device',
            opId: 'history-server-winner',
          },
        }),
      }),
    ])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ logicalClockFloor: 99, lastAckSequence: 41 }),
    ])

    await expect(syncThoughts(lease, readerClient))
      .resolves.toMatchObject({ status: 'failed', pushed: 0, pending: 1, errorCode: 'superseded' })
    expect(pushThoughtOps).toHaveBeenCalledTimes(1)
    expect(syncRemote).not.toHaveBeenCalled()

    await addHistoryReattach(lease, 'after-history-superseded')
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ opId: 'after-history-superseded', logicalClock: 100 }),
    ]))
  })

  it('raises a fresh device floor above divergent materialized and frozen server winners', async () => {
    const lease = identity('physical-A')
    await expect(syncThoughts(lease, client({
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: {
          contract_version: 1 as const,
          items: [remoteThoughtAtClock('history-thought', 4, 42)],
        },
      })),
    }))).resolves.toMatchObject({ status: 'synced', pulled: 1 })

    await addHistoryReattach(lease, 'materialized-winner-floor')
    await addHistoryReattach(lease, 'frozen-winner-floor', {
      logicalClock: 99,
      deviceId: 'history-snapshot-device',
      opId: 'history-snapshot-winner',
    })

    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({
        opId: 'materialized-winner-floor',
        logicalClock: 43,
        snapshot: expect.objectContaining({
          winnerKey: {
            logicalClock: 11,
            deviceId: 'history-server-device',
            opId: 'history-server-winner',
          },
        }),
      }),
      expect.objectContaining({
        opId: 'frozen-winner-floor',
        logicalClock: 100,
        snapshot: expect.objectContaining({
          winnerKey: {
            logicalClock: 99,
            deviceId: 'history-snapshot-device',
            opId: 'history-snapshot-winner',
          },
        }),
      }),
    ]))
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual([
      expect.objectContaining({
        annotationId: 'history-thought',
        winnerKey: {
          logicalClock: 42,
          deviceId: 'remote-device',
          opId: 'remote-history-thought-42',
        },
      }),
    ])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({
        deviceId: expect.stringMatching(/^device-/),
        logicalClockFloor: 100,
      }),
    ])
  })

  it('advances one atomic paginated replay floor from clocks 3, 20, and 7', async () => {
    const lease = identity('physical-A')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('clock-3', 1, 3)],
          next_cursor: 'cursor-1',
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('clock-20', 2, 20)],
          next_cursor: 'cursor-2',
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('clock-7', 3, 7)],
        },
      })

    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({ status: 'synced', pulled: 3, cursor: 'cursor-2' })
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ logicalClockFloor: 20, cursor: 'cursor-2' }),
    ])

    await addLocalThought(lease, 'after-paginated-pull', 'after-paginated-pull')
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ opId: 'after-paginated-pull', logicalClock: 21 }),
    ])
  })

  it('finishes a 1901-item replay at the per-round page boundary without leaving it pending', async () => {
    const lease = identity('pull-boundary-1901')
    const syncRemote = vi.fn(async (params: { after?: string; limit?: number } = {}) => {
      const page = params.after ? Number(params.after.slice('cursor-'.length)) : 0
      const start = page * 100
      const end = Math.min(start + 100, 1901)
      return {
        ok: true as const,
        data: {
          contract_version: 1 as const,
          items: Array.from({ length: end - start }, (_, index) =>
            remoteThought(`boundary-${start + index + 1}`, start + index + 1)),
          ...(end < 1901 ? { next_cursor: `cursor-${page + 1}` } : {}),
        },
      }
    })

    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({ status: 'synced', pulled: 1901, cursor: 'cursor-19' })
    expect(syncRemote).toHaveBeenCalledTimes(20)
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toHaveLength(1901)
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: 'cursor-19', lastSuccessfulSyncAt: expect.any(Number) }),
    ])
    expect((await readAll(THOUGHT_SYNC_STATE_STORE))[0]).not.toHaveProperty('pullInProgress')
  })

  it('persists a 20-page replay budget, presents pending, then follows up from the durable cursor', async () => {
    vi.useFakeTimers()
    const lease = identity('pull-follow-up-21-pages')
    type PullResult = Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>
    let releaseFinalPage!: (result: PullResult) => void
    const syncRemote = vi.fn<IdentityBoundReaderClient['syncThoughts']>((params = {}) => {
      const page = params.after ? Number(params.after.slice('cursor-'.length)) : 0
      if (page === 20) {
        return new Promise<PullResult>((resolve) => { releaseFinalPage = resolve })
      }
      return Promise.resolve({
        ok: true as const,
        data: {
          contract_version: 1 as const,
          items: [remoteThought(`follow-up-${page + 1}`, page + 1)],
          next_cursor: `cursor-${page + 1}`,
        },
      })
    })
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))
    const observedSnapshots: Array<ReturnType<typeof controller.getSnapshot>> = []
    const unsubscribe = controller.subscribe(() => {
      const snapshot = controller.getSnapshot()
      observedSnapshots.push(snapshot)
    })
    const stop = controller.start()

    // waitFor advances fake timers while fake IndexedDB persists each page.
    // Retain the published durable state because the immediate replay can make
    // the controller syncing again in the same timer tick.
    await vi.waitFor(() => expect(observedSnapshots).toContainEqual(expect.objectContaining({
      phase: 'pending',
      pendingCount: 0,
    })), { timeout: 5000 })
    expect(syncRemote).toHaveBeenCalledTimes(20)
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(21), { timeout: 5000 })
    vi.useRealTimers()

    expect(observedSnapshots).toContainEqual(expect.objectContaining({
      phase: 'pending',
      pendingCount: 0,
    }))
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({ phase: 'syncing' }))
    expect(syncRemote).toHaveBeenLastCalledWith(
      { after: 'cursor-20', limit: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: 'cursor-20', pullInProgress: true }),
    ])
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toHaveLength(20)

    releaseFinalPage({
      ok: true,
      data: {
        contract_version: 1,
        items: [remoteThought('follow-up-21', 21)],
      },
    })
    await vi.waitFor(() => expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'synced',
      pendingCount: 0,
      lastSuccessfulSyncAt: expect.any(Number),
    })), { timeout: 5000 })
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toHaveLength(21)
    expect((await readAll(THOUGHT_SYNC_STATE_STORE))[0]).not.toHaveProperty('pullInProgress')
    stop()
    unsubscribe()
  })

  it('persists completed replay pages before a later invalid page and recovers from the cursor', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const lease = identity('physical-A')
    const invalid = {
      ...remoteThoughtAtClock('invalid-final', 3, 7),
      winner_key: {
        logical_clock: 7.5,
        device_id: 'remote-device',
        op_id: 'invalid-final-op',
      },
    }
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('buffered-3', 1, 3)],
          next_cursor: 'cursor-1',
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('buffered-20', 2, 20)],
          next_cursor: 'cursor-2',
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { contract_version: 1, items: [invalid] },
      })

    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({ status: 'failed', pulled: 2, cursor: 'cursor-2', retryAt: 2000 })
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ annotationId: 'buffered-3', serverSequence: 1 }),
      expect.objectContaining({ annotationId: 'buffered-20', serverSequence: 2 }),
    ]))
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({
        cursor: 'cursor-2',
        logicalClockFloor: 20,
        pullInProgress: true,
        pullRetryAt: 2000,
      }),
    ])

    now.mockReturnValue(2000)
    const recovery = vi.fn(async () => ({
      ok: true as const,
      data: {
        contract_version: 1,
        items: [remoteThoughtAtClock('recovered-7', 3, 7)],
      },
    }))
    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: recovery,
    }))).resolves.toMatchObject({ status: 'synced', pulled: 1, cursor: 'cursor-2' })
    expect(recovery).toHaveBeenCalledWith(
      { after: 'cursor-2', limit: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ annotationId: 'buffered-3', serverSequence: 1 }),
      expect.objectContaining({ annotationId: 'buffered-20', serverSequence: 2 }),
      expect.objectContaining({ annotationId: 'recovered-7', serverSequence: 3 }),
    ]))
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: 'cursor-2', logicalClockFloor: 20 }),
    ])
    expect((await readAll(THOUGHT_SYNC_STATE_STORE))[0]).not.toHaveProperty('pullInProgress')
  })

  it('keeps already-committed pages when identity revocation stops a later replay response', async () => {
    const lease = identity('physical-A')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('identity-buffer-3', 1, 3)],
          next_cursor: 'cursor-1',
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [remoteThoughtAtClock('identity-buffer-20', 2, 20)],
          next_cursor: 'cursor-2',
        },
      })
      .mockImplementationOnce(async () => {
        lease.revoke()
        return {
          ok: true as const,
          data: {
            contract_version: 1,
            items: [remoteThoughtAtClock('identity-buffer-7', 3, 7)],
          },
        }
      })

    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({ status: 'stale', pulled: 2 })
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ annotationId: 'identity-buffer-3', serverSequence: 1 }),
      expect.objectContaining({ annotationId: 'identity-buffer-20', serverSequence: 2 }),
    ]))
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: 'cursor-2', logicalClockFloor: 20, pullInProgress: true }),
    ])
  })

  it('uses the normalized device id, durably acks, pulls, and resumes after reload', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease)
    const pushed: Array<{ device_id: string; logical_clock: number; contract_version: number }> = []
    const pullArgs: Array<{ after?: string; limit?: number }> = []
    const pushThoughtOps = vi.fn(async (request: { ops: Array<{ device_id: string; logical_clock: number; contract_version: number }> }) => {
      pushed.push(...request.ops)
      return { ok: true as const, data: [{ op_id: 'op-1', sequence: 17 }] }
    })
    const syncRemote = vi.fn(async (params: { after?: string; limit?: number } = {}) => {
      pullArgs.push(params)
      return { ok: true as const, data: { items: [], next_cursor: 'cursor-1' } }
    })

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({
        status: 'synced',
        pushed: 1,
        pulled: 0,
        cursor: 'cursor-1',
        pending: 0,
      })
    expect(pushed).toHaveLength(1)
    expect(pushed[0].device_id).toMatch(/^device-/)
    expect(pushed[0].device_id).not.toBe('')
    expect(pushed[0]).toMatchObject({ contract_version: 1, logical_clock: 1 })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        cursor: 'cursor-1',
        lastAckSequence: 17,
      }),
    ])

    resetUserDataDatabaseHandle()
    const reloadPull = vi.fn(async (params: { after?: string; limit?: number } = {}) => {
      pullArgs.push(params)
      return { ok: true as const, data: { items: [], next_cursor: 'cursor-1' } }
    })
    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: reloadPull,
    }))).resolves.toMatchObject({ status: 'idle', cursor: 'cursor-1', pending: 0 })
    expect(reloadPull).toHaveBeenCalledWith(
      { after: 'cursor-1', limit: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
    expect(pullArgs).toContainEqual({ after: 'cursor-1', limit: 100 })
  })

  it('pulls remote materialized thoughts and keeps local optimistic rows ahead of them', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease, 'local-op', 'local-id')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [remoteThought('remote-id', 4)], next_cursor: 'cursor-4' },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [], next_cursor: 'cursor-4' },
      })
    const result = await syncThoughts(lease, client({
      pushThoughtOps: vi.fn(async () => ({
        ok: true as const,
        data: [{ op_id: 'local-op', sequence: 3 }],
      })),
      syncThoughts: syncRemote,
    }))

    expect(result).toMatchObject({ status: 'synced', pushed: 1, pulled: 1, cursor: 'cursor-4' })
    await expect(listRemoteThoughts(lease, 'L1', TARGET)).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ annotationId: 'remote-id', serverSequence: 4 })],
    })
    const snapshot = await readAnnotationSnapshot(lease, 'L1', TARGET)
    expect(snapshot).toMatchObject({ ok: true })
    if (snapshot.ok) {
      expect(snapshot.value.annotations.map((annotation) => annotation.id)).toEqual([
        'local-id',
        'remote-id',
      ])
    }
    expect(syncRemote).toHaveBeenNthCalledWith(
      2,
      { after: 'cursor-4', limit: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
  })

  it('converts note annotations to thought wire without link_id and applies update patches', async () => {
    const lease = identity('physical-A')
    await addLocalNoteThought(lease)
    type CapturedOperation = {
      host_kind: string
      host_id: string
      target: unknown
      payload: unknown
    }
    const pushed: CapturedOperation[] = []
    const pushThoughtOps = vi.fn(async (request: { ops: CapturedOperation[] }) => {
      pushed.push(...request.ops)
      return {
      ok: true as const,
      data: [{ op_id: 'note-op', sequence: 12 }],
      }
    })
    await expect(syncThoughts(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { items: [] },
      })),
    }))).resolves.toMatchObject({ pending: 0 })
    expect(pushed[0]).toMatchObject({
      host_kind: 'note',
      host_id: 'N1',
      target: {
        kind: 'note',
        host_id: 'N1',
        version: { note_revision: 4 },
        block_key: 'note',
      },
      payload: {
        body: 'note thought',
        quote: expect.objectContaining({ block_key: 'note' }),
      },
    })
    expect(pushed[0]?.payload).not.toHaveProperty('link_id')

    await expect(commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'note-update',
      linkId: 'N1',
      target: NOTE_TARGET,
      annotationId: 'note-a1',
      patch: { note: 'patched note', source: 'ai', updatedAt: 2 },
    })).resolves.toMatchObject({ ok: true })
    const updatePushed: CapturedOperation[] = []
    const updatePush = vi.fn(async (request: { ops: CapturedOperation[] }) => {
      updatePushed.push(...request.ops)
      return {
      ok: true as const,
      data: [{ op_id: 'note-update', sequence: 13 }],
      }
    })
    await expect(syncThoughts(lease, client({
      pushThoughtOps: updatePush,
      syncThoughts: vi.fn(async () => ({ ok: true as const, data: { items: [] } })),
    }))).resolves.toMatchObject({ pending: 0 })
    expect(updatePushed[0]?.payload).toMatchObject({
      body: 'patched note',
      source: 'ai',
    })
    expect(updatePushed[0]?.payload).not.toHaveProperty('link_id')
  })

  it('schedules retry at the durable nextAttemptAt instead of stalling after a failure', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease)
    let attempts = 0
    const pushThoughtOps = vi.fn(async () => {
      attempts += 1
      return attempts === 1
        ? {
            ok: false as const,
            error: {
              kind: 'network-unreachable' as const,
              message: 'offline',
              retryAfterSeconds: 5,
            },
          }
        : { ok: true as const, data: [{ op_id: 'op-1', sequence: 9 }] }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { items: [], next_cursor: 'cursor-9' },
    }))
    const first = await syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    expect(first).toMatchObject({ status: 'failed', retryAt: expect.any(Number) })
    expect((await readAll(THOUGHT_OUTBOX_STORE)).every((row) =>
      (row as { attemptCount: number }).attemptCount === 1)).toBe(true)

    const durableRetryAt = ((await readAll(THOUGHT_OUTBOX_STORE))[0] as {
      nextAttemptAt: number
    }).nextAttemptAt
    expect(first).toMatchObject({ retryAt: durableRetryAt })

    const nativeSetTimeout = window.setTimeout
    let retryCallback: (() => void) | undefined
    let scheduledDelay: number | undefined
    vi.spyOn(window, 'setTimeout').mockImplementation(((handler: () => void, timeout?: number) => {
      const delay = timeout ?? 0
      if (delay > 1000) {
        retryCallback = handler
        scheduledDelay = delay
        return 1
      }
      return nativeSetTimeout.call(window, handler, timeout)
    }) as typeof window.setTimeout)

    const stop = startThoughtSync(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    await vi.waitFor(() => expect(retryCallback).toEqual(expect.any(Function)))
    expect(scheduledDelay).toBeGreaterThan(0)
    expect(Math.abs((scheduledDelay ?? 0) + Date.now() - durableRetryAt)).toBeLessThan(100)
    // Retry-After gates the complete controller round, not only another push.
    // A lifecycle mount and a titlebar retry must not turn the pull endpoint
    // into a side channel around the server's backoff contract.
    expect(syncRemote).not.toHaveBeenCalled()
    expect(pushThoughtOps).toHaveBeenCalledTimes(1)
    vi.spyOn(Date, 'now').mockReturnValue(durableRetryAt)
    retryCallback?.()
    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(2))
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
    const secondSync = syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    await expect(secondSync)
      .resolves.toMatchObject({ pending: 0 })
    stop()
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
  })

  it('honors a 600-second Retry-After for direct, manual, and polling sync attempts', async () => {
    let now = 1000
    vi.spyOn(Date, 'now').mockImplementation(() => now)
    const lease = identity('full-retry-after')
    await addLocalThought(lease, 'full-cooldown-op')
    let attempts = 0
    const pushThoughtOps = vi.fn(async (
      request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0],
    ): Promise<unknown> => {
      attempts += 1
      if (attempts === 1) {
        return {
          ok: false as const,
          error: {
            kind: 'rate-limited' as const,
            status: 429,
            errorCode: 'thought_cooldown',
            retryAfterSeconds: 600,
            message: 'server cooldown',
          },
        }
      }
      return {
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          op_id: operation.op_id,
          sequence: index + 1,
        })),
      }
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))
    const thoughtClient = client({ pushThoughtOps, syncThoughts: syncRemote })
    const controller = getThoughtSyncController(lease, thoughtClient)

    await expect(controller.sync()).resolves.toMatchObject({
      status: 'failed',
      retryAt: 601000,
    })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ opId: 'full-cooldown-op', nextAttemptAt: 601000 }),
    ])
    expect(syncRemote).not.toHaveBeenCalled()

    // The public direct entrypoint is behind the same durable cooldown.
    await expect(syncThoughts(lease, thoughtClient)).resolves.toMatchObject({
      status: 'idle',
      retryAt: 601000,
    })

    const nativeSetTimeout = window.setTimeout
    let retryCallback: (() => void) | undefined
    let scheduledDelay: number | undefined
    vi.spyOn(window, 'setTimeout').mockImplementation(((handler: () => void, timeout?: number) => {
      const delay = timeout ?? 0
      if (delay > 1000) {
        retryCallback = handler
        scheduledDelay = delay
        return 1
      }
      return nativeSetTimeout.call(window, handler, timeout)
    }) as typeof window.setTimeout)

    const stop = controller.start()
    await expect(controller.sync()).resolves.toMatchObject({
      status: 'idle',
      retryAt: 601000,
    })
    await vi.waitFor(() => expect(retryCallback).toEqual(expect.any(Function)))
    expect(scheduledDelay).toBe(600000)
    expect(pushThoughtOps).toHaveBeenCalledTimes(1)
    expect(syncRemote).not.toHaveBeenCalled()

    now += THOUGHT_SYNC_POLL_MS
    await expect(controller.sync()).resolves.toMatchObject({
      status: 'idle',
      retryAt: 601000,
    })
    expect(scheduledDelay).toBe(600000 - THOUGHT_SYNC_POLL_MS)
    expect(pushThoughtOps).toHaveBeenCalledTimes(1)
    expect(syncRemote).not.toHaveBeenCalled()

    now = 601000
    retryCallback?.()
    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(2))
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
    stop()
  })

  it('persists pull backoff and resumes it after a reload', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const lease = identity('physical-A')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: false as const,
        error: {
          kind: 'network-unreachable' as const,
          message: 'pull offline',
          retryAfterSeconds: 5,
        },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [], next_cursor: 'cursor-after-retry' },
      })

    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({
      status: 'failed',
      retryAt: 6000,
    })
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        pullRetryAt: 6000,
        pullAttemptCount: 1,
        retryAt: 6000,
      }),
    ])

    resetUserDataDatabaseHandle()
    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({
      status: 'idle',
      cursor: '',
      retryAt: 6000,
    })
    expect(syncRemote).toHaveBeenCalledTimes(1)

    now.mockReturnValue(6000)
    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({
      status: 'idle',
      cursor: 'cursor-after-retry',
    })
    expect(syncRemote).toHaveBeenCalledTimes(2)
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        cursor: 'cursor-after-retry',
      }),
    ])
    expect((await readAll(THOUGHT_SYNC_STATE_STORE))[0]).not.toHaveProperty('pullRetryAt')
  })

  it('keeps an incomplete ack in the outbox with a durable retry', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1000)
    const lease = identity('physical-A')
    await addLocalThought(lease)
    const pushThoughtOps = vi.fn()
      .mockResolvedValueOnce({ ok: true as const, data: [] })
      .mockResolvedValueOnce({
        ok: true as const,
        data: [{ op_id: 'op-1', sequence: 9 }],
      })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { items: [], next_cursor: 'cursor-9' },
    }))

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({
        status: 'failed',
        pending: 1,
        retryAt: 2000,
      })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        opId: 'op-1',
        attemptCount: 1,
        nextAttemptAt: 2000,
      }),
    ])
    expect(syncRemote).not.toHaveBeenCalled()

    now.mockReturnValue(2000)
    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({ status: 'synced', pending: 0, pushed: 1 })
    expect(pushThoughtOps).toHaveBeenCalledTimes(2)
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
  })

  it('quarantines permanent push failures without scheduling another attempt', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease)
    const pushThoughtOps = vi.fn(async () => ({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 422,
        errorCode: 'invalid_thought_payload',
        message: 'invalid thought payload',
      },
    }))
    const result = await syncThoughts(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({ ok: true as const, data: { items: [] } })),
    }))
    expect(result).toMatchObject({ status: 'failed', pending: 1 })
    expect(result).not.toHaveProperty('retryAt')
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        status: 'blocked',
        blockedReason: 'other:invalid_thought_payload:422',
      }),
    ])
    expect(await readAll(THOUGHT_OUTBOX_STORE)).not.toEqual([
      expect.objectContaining({ nextAttemptAt: expect.anything() }),
    ])
  })

  it('isolates a poison operation from valid siblings in one request-level 4xx batch', async () => {
    const lease = identity('mixed-permanent-batch')
    await addLocalThought(lease, 'good-op', 'good-thought')
    await addLocalThought(lease, 'poison-op', 'poison-thought')
    const requests: string[][] = []
    const pushThoughtOps = vi.fn(async (
      request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0],
    ): Promise<unknown> => {
      const ids = request.ops.map((operation) => operation.op_id)
      requests.push(ids)
      if (ids.includes('poison-op')) {
        return {
          ok: false as const,
          error: {
            kind: 'other' as const,
            status: 422,
            errorCode: 'invalid_thought_payload',
            message: 'one operation is invalid',
          },
        }
      }
      return {
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          op_id: operation.op_id,
          sequence: index + 1,
        })),
      }
    })
    const syncRemote = vi.fn()

    await expect(syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote })))
      .resolves.toMatchObject({
        status: 'failed',
        pushed: 1,
        pending: 1,
        errorCode: 'other:invalid_thought_payload:422',
      })
    expect(requests).toEqual([
      ['good-op', 'poison-op'],
      ['good-op'],
      ['poison-op'],
    ])
    expect(syncRemote).not.toHaveBeenCalled()
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        opId: 'poison-op',
        status: 'blocked',
        blockedReason: 'other:invalid_thought_payload:422',
      }),
    ])
  })

  it('preserves a recovery candidate on CAS conflict without persisting server payload text', async () => {
    const lease = identity('physical-recovery-cas')
    await addLocalThought(lease)
    await runUserDataTransaction(
      lease,
      'mark local thought as supersession recovery',
      [THOUGHT_OUTBOX_STORE],
      'readwrite',
      (transaction, _identity, setResult) => {
        const store = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        const request = store.get([lease.context.physicalNamespace, 1]) as IDBRequest<Record<string, unknown>>
        request.onerror = () => transaction.abort()
        request.onsuccess = () => {
          if (!request.result) {
            transaction.abort()
            return
          }
          store.put({
            ...request.result,
            opId: 'supersession-recovery:7',
            recoveryOf: { logicalClock: 4, deviceId: 'loser-device', opId: 'loser-op' },
            expectedCurrentWinnerKey: { logicalClock: 9, deviceId: 'winner-device', opId: 'winner-op' },
          })
          setResult(undefined)
        }
      },
    )
    const pushThoughtOps = vi.fn(async () => ({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'thought_recovery_conflict',
        message: 'private-recovery-cas-sentinel',
      },
    }))

    const result = await syncThoughts(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({ ok: true as const, data: { items: [] } })),
    }))

    expect(result).toMatchObject({
      status: 'failed',
      pending: 1,
      errorCode: 'other:thought_recovery_conflict:409',
    })
    expect(result).not.toHaveProperty('retryAt')
    expect(JSON.stringify(result)).not.toContain('private-recovery-cas-sentinel')
    expect(pushThoughtOps).toHaveBeenCalledWith(expect.objectContaining({
      ops: [expect.objectContaining({
        recovery_of: { logical_clock: 4, device_id: 'loser-device', op_id: 'loser-op' },
        expected_current_winner_key: { logical_clock: 9, device_id: 'winner-device', op_id: 'winner-op' },
      })],
    }), expect.anything())
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        opId: 'supersession-recovery:7',
        status: 'blocked',
        blockedReason: 'thought_recovery_conflict',
        recoveryConflict: true,
        lastError: 'sync-failed',
      }),
    ])
    expect(JSON.stringify(await readAll(THOUGHT_OUTBOX_STORE))).not.toContain('private-recovery-cas-sentinel')
    expect(JSON.stringify(await readAll(THOUGHT_SYNC_STATE_STORE))).not.toContain('private-recovery-cas-sentinel')
  })

  it('resets only an expired cursor, preserves rows, and clears resync after replay', async () => {
    const lease = identity('physical-A')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [remoteThought('remote-retained', 4)], next_cursor: 'cursor-4' },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [], next_cursor: 'cursor-4' },
      })
      .mockResolvedValueOnce({
        ok: false as const,
        error: { kind: 'other' as const, status: 422, errorCode: 'cursor_expired', message: 'expired' },
      })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [], next_cursor: 'cursor-replayed' },
      })
    await syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))
    await expect(syncThoughts(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))).resolves.toMatchObject({ cursor: 'cursor-replayed' })
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual([
      expect.objectContaining({ annotationId: 'remote-retained', serverSequence: 4 }),
    ])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: 'cursor-replayed', resyncRequired: false }),
    ])
  })

  it('does not reset a cursor for an unrelated 422 contract failure', async () => {
    const lease = identity('physical-A')
    const firstPull = vi.fn(async () => ({
      ok: true as const,
      data: { items: [], next_cursor: 'cursor-kept' },
    }))
    await syncThoughts(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: firstPull }))
    const secondPull = vi.fn(async () => ({
      ok: false as const,
      error: { kind: 'other' as const, status: 422, errorCode: 'invalid_thought_payload', message: 'bad page' },
    }))
    await expect(syncThoughts(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: secondPull })))
      .resolves.toMatchObject({ status: 'failed', cursor: 'cursor-kept' })
    expect(secondPull).toHaveBeenCalledWith(
      { after: 'cursor-kept', limit: 100 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
  })

  it('keeps a deterministic loser copy for an older multi-device version', async () => {
    const lease = identity('physical-A')
    const syncRemote = vi.fn()
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [remoteThought('conflict-a1', 9)], next_cursor: 'cursor-9' },
      })
      .mockResolvedValueOnce({ ok: true as const, data: { items: [], next_cursor: 'cursor-9' } })
      .mockResolvedValueOnce({
        ok: true as const,
        data: { items: [{ ...remoteThought('conflict-a1', 8), body: 'older device version' }] },
      })
    await syncThoughts(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: syncRemote }))
    const reset = await runUserDataTransaction(
      lease,
      'test reset cursor for conflict replay',
      [THOUGHT_SYNC_STATE_STORE],
      'readwrite',
      (transaction, _identity, setResult) => {
        transaction.objectStore(THOUGHT_SYNC_STATE_STORE).put({
          namespace: 'physical-A',
          cursor: '',
          deviceId: 'device-test',
          tabToken: 'tab-test',
          updatedAt: Date.now(),
        })
        setResult(undefined)
      },
    )
    expect(reset).toEqual({ ok: true, value: undefined })
    await syncThoughts(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: syncRemote }))
    await expect(listThoughtConflicts(lease, 'conflict-a1')).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({
        reason: 'older-server-version',
        loser: expect.objectContaining({ body: 'older device version', serverSequence: 8 }),
        winner: expect.objectContaining({ serverSequence: 9 }),
      })],
    })
    expect(await readAll(THOUGHT_MATERIALIZED_STORE)).toEqual([
      expect.objectContaining({ body: 'remote thought', serverSequence: 9 }),
    ])
    expect(await readAll(ANNOTATION_IMPORTS_STORE)).toEqual([
      expect.objectContaining({ kind: 'thought-conflict' }),
    ])
  })

  it('leaves a frozen history candidate intact when the identity is revoked during push', async () => {
    const lease = identity('physical-A')
    await addHistoryReattach(lease, 'history-revoked-during-push')
    type PushResult = Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
    type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
    let release: ((value: PushResult) => void) | undefined
    let signal: AbortSignal | undefined
    let submitted: WireOperation | undefined
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>((request, options) => {
      submitted = request.ops[0]
      signal = options?.signal
      return new Promise<PushResult>((resolve) => { release = resolve })
    })
    const syncRemote = vi.fn()
    const pending = syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(1))
    lease.revoke()
    expect(signal?.aborted).toBe(true)
    if (!submitted) throw new Error('history operation was not captured')
    release?.({
      ok: true,
      data: [{
        contract_version: 1,
        op_id: submitted.op_id,
        sequence: 11,
        disposition: 'applied',
        submitted_key: {
          logical_clock: submitted.logical_clock,
          device_id: submitted.device_id,
          op_id: submitted.op_id,
        },
        current_winner_key: {
          logical_clock: submitted.logical_clock,
          device_id: submitted.device_id,
          op_id: submitted.op_id,
        },
      }],
    })

    await expect(pending).resolves.toMatchObject({ status: 'stale', pending: 1 })
    expect(syncRemote).not.toHaveBeenCalled()
    expect(await readAll(THOUGHT_HISTORY_OUTBOX_STORE)).toEqual([
      expect.objectContaining({
        namespace: 'physical-A',
        opId: 'history-revoked-during-push',
        attemptCount: 0,
        snapshot: expect.objectContaining({
          winnerKey: {
            logicalClock: 11,
            deviceId: 'history-server-device',
            opId: 'history-server-winner',
          },
        }),
      }),
    ])
  })

  it('leaves the recovery outbox intact when the identity is revoked during push', async () => {
    const lease = identity('physical-A')
    await addLocalThought(lease)
    type PushResult = Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
    let release: ((value: PushResult) => void) | undefined
    let signal: AbortSignal | undefined
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>((_request, options) => {
      signal = options?.signal
      return new Promise<PushResult>((resolve) => { release = resolve })
    })
    const syncRemote = vi.fn()
    const pending = syncThoughts(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(1))
    lease.revoke()
    expect(signal?.aborted).toBe(true)
    release?.({
      ok: true,
      data: [{
        contract_version: 1,
        op_id: 'op-1',
        sequence: 11,
        disposition: 'applied',
        submitted_key: { logical_clock: 1, device_id: 'device-test', op_id: 'op-1' },
        current_winner_key: { logical_clock: 1, device_id: 'device-test', op_id: 'op-1' },
      }],
    })

    await expect(pending).resolves.toMatchObject({ status: 'stale', pending: 1 })
    expect(syncRemote).not.toHaveBeenCalled()
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ namespace: 'physical-A', opId: 'op-1', attemptCount: 0 }),
    ])

    const otherLease = identity('physical-B')
    const otherPush = vi.fn()
    await expect(syncThoughts(otherLease, client({
      pushThoughtOps: otherPush,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { items: [], next_cursor: 'other-cursor' },
      })),
    }))).resolves.toMatchObject({ status: 'idle', pending: 0 })
    expect(otherPush).not.toHaveBeenCalled()
  })

  it.each([
    {
      name: 'remote-only rows remain visible',
      operations: [] as const,
      expected: [['remote', 'remote thought']],
    },
    {
      name: 'pending add overlays the server cache',
      operations: [{ kind: 'add', opId: 'local-add', id: 'local-add', note: 'local add' }] as const,
      expected: [['remote', 'remote thought'], ['local-add', 'local add']],
    },
    {
      name: 'pending update overrides the matching server row',
      operations: [{ kind: 'update', opId: 'local-update', id: 'remote', note: 'local update' }] as const,
      expected: [['remote', 'local update']],
    },
    {
      name: 'pending delete hides the matching server row',
      operations: [{ kind: 'delete', opId: 'local-delete', id: 'remote' }] as const,
      expected: [] as string[][],
    },
  ])('selects $name after merging durable local operations', async ({ operations, expected }) => {
    const lease = identity('physical-A')
    await expect(cacheServerThoughtPage(lease, [remoteThought('remote', 9)])).resolves.toEqual({ ok: true, value: 1 })
    for (const operation of operations) {
      if (operation.kind === 'add') {
        await expect(commitAnnotationOperation(lease, {
          kind: 'add', opId: operation.opId, linkId: 'L1', target: TARGET,
          draft: annotationDraft(operation.id),
        })).resolves.toMatchObject({ ok: true })
        // Keep the table focused on selector semantics rather than the add
        // draft fixture's default body.
        await expect(commitAnnotationOperation(lease, {
          kind: 'update', opId: `${operation.opId}-body`, linkId: 'L1', target: TARGET,
          annotationId: operation.id, patch: { note: operation.note, updatedAt: 2_000 },
        })).resolves.toMatchObject({ ok: true })
      } else if (operation.kind === 'update') {
        await expect(commitAnnotationOperation(lease, {
          kind: 'update', opId: operation.opId, linkId: 'L1', target: TARGET,
          annotationId: operation.id, patch: { note: operation.note, updatedAt: 2_000 },
        })).resolves.toMatchObject({ ok: true })
      } else {
        await expect(commitAnnotationOperation(lease, {
          kind: 'delete', opId: operation.opId, linkId: 'L1', target: TARGET, annotationId: operation.id,
        })).resolves.toMatchObject({ ok: true })
      }
    }
    const selected = await selectThoughtReadModel(lease)
    expect(selected).toMatchObject({ ok: true })
    if (selected.ok) expect(selected.value.map((item) => [item.id, item.body])).toEqual(expected)
  })

  it('does not expose server tombstones through the live aggregate selector', async () => {
    const lease = identity('physical-A')
    await cacheServerThoughtPage(lease, [
      remoteThought('active', 1),
      { ...remoteThought('tombstoned', 2), body: '   ', deleted: true },
    ])
    await expect(selectThoughtReadModel(lease)).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ id: 'active' })],
    })
  })

  it('keeps retryable and blocked local updates over an older server row across reload', async () => {
    const lease = identity('physical-A')
    await cacheServerThoughtPage(lease, [remoteThought('remote', 3)])
    await commitAnnotationOperation(lease, {
      kind: 'update', opId: 'retryable-update', linkId: 'L1', target: TARGET,
      annotationId: 'remote', patch: { note: 'retryable body', updatedAt: 3_000 },
    })
    const first = await selectThoughtReadModel(lease)
    expect(first).toMatchObject({ ok: true, value: [expect.objectContaining({ body: 'retryable body' })] })
    const marked = await runUserDataTransaction(lease, 'mark selector row blocked', [THOUGHT_OUTBOX_STORE], 'readwrite', (transaction, _identity, setResult) => {
      const request = transaction.objectStore(THOUGHT_OUTBOX_STORE).get(['physical-A', 1]) as IDBRequest<Record<string, unknown>>
      request.onsuccess = () => {
        transaction.objectStore(THOUGHT_OUTBOX_STORE).put({ ...request.result, status: 'blocked', blockedReason: 'unauthorized', nextAttemptAt: undefined })
        setResult(undefined)
      }
      request.onerror = () => transaction.abort()
    })
    expect(marked).toEqual({ ok: true, value: undefined })
    resetUserDataDatabaseHandle()
    await expect(selectThoughtReadModel(identity('physical-A'))).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ id: 'remote', body: 'retryable body' })],
    })
  })

  it('hands an acknowledged local update back to its matching server projection without duplication', async () => {
    const lease = identity('physical-A')
    await cacheServerThoughtPage(lease, [remoteThought('remote', 3)])
    await commitAnnotationOperation(lease, {
      kind: 'update', opId: 'ack-update', linkId: 'L1', target: TARGET,
      annotationId: 'remote', patch: { note: 'acknowledged body', updatedAt: 4_000 },
    })
    await expect(selectThoughtReadModel(lease)).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ id: 'remote', body: 'acknowledged body' })],
    })
    await syncThoughts(lease, client({
      pushThoughtOps: vi.fn(async (request) => {
        const operation = request.ops[0]
        return {
          ok: true as const,
          data: [{ op_id: operation.op_id, sequence: 4 }],
        }
      }),
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: {
          contract_version: 1,
          items: [{ ...remoteThought('remote', 4), body: 'acknowledged body' }],
        },
      })),
    }))
    await expect(selectThoughtReadModel(lease)).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ id: 'remote', body: 'acknowledged body', last_sequence: 4 })],
    })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
  })

  it('keeps aggregate cursors independent from local overlays and deduplicates delayed server pages', async () => {
    const lease = identity('physical-A')
    await cacheServerThoughtPage(lease, [
      { ...remoteThought('later', 12), updated_at: '2026-08-12T00:00:00.000Z' },
      { ...remoteThought('remote', 8), updated_at: '2026-08-11T00:00:00.000Z' },
    ])
    await addLocalThought(lease, 'local-add', 'local')
    await commitAnnotationOperation(lease, {
      kind: 'delete', opId: 'local-delete', linkId: 'L1', target: TARGET, annotationId: 'remote',
    })
    await cacheServerThoughtPage(lease, [
      { ...remoteThought('remote', 8), updated_at: '2026-08-11T00:00:00.000Z' },
      remoteThought('older-page', 2),
    ])
    const selected = await selectThoughtReadModel(lease)
    expect(selected).toMatchObject({ ok: true })
    if (selected.ok) {
      expect(selected.value.map((item) => item.id)).toEqual(['later', 'older-page', 'local'])
      expect(new Set(selected.value.map((item) => item.id)).size).toBe(selected.value.length)
    }
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ cursor: '' }),
    ])
  })

  it('keeps local add, update, and delete projections through an offline database reload', async () => {
    const lease = identity('physical-A')
    await cacheServerThoughtPage(lease, [
      remoteThought('remote-update', 4),
      remoteThought('remote-delete', 5),
    ])
    await commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'offline-add',
      linkId: 'L1',
      target: TARGET,
      draft: {
        ...annotationDraft('offline-add'),
        note: 'offline add',
        createdAt: Date.parse('2026-08-11T00:00:00.000Z'),
        updatedAt: Date.parse('2026-08-11T00:00:00.000Z'),
      },
    })
    await commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'offline-update',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'remote-update',
      patch: { note: 'offline update', updatedAt: Date.parse('2026-08-12T00:00:00.000Z') },
    })
    await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'offline-delete',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'remote-delete',
    })

    resetUserDataDatabaseHandle()
    const selected = await selectThoughtReadModel(identity('physical-A'))
    expect(selected).toMatchObject({ ok: true })
    if (selected.ok) {
      expect(selected.value.map((item) => [item.id, item.body])).toEqual([
        ['remote-update', 'offline update'],
        ['offline-add', 'offline add'],
      ])
      expect(selected.value.some((item) => item.id === 'remote-delete')).toBe(false)
    }
  })

  it('does not persist a late server page after its identity lease is revoked', async () => {
    const leaseA = identity('physical-A')
    const pending = cacheServerThoughtPage(leaseA, [remoteThought('late-A', 1)])
    leaseA.revoke()

    await expect(pending).resolves.toEqual({ ok: false })
    await cacheServerThoughtPage(identity('physical-B'), [remoteThought('B', 1)])
    await expect(selectThoughtReadModel(identity('physical-A'))).resolves.toEqual({ ok: true, value: [] })
    await expect(selectThoughtReadModel(identity('physical-B'))).resolves.toMatchObject({
      ok: true,
      value: [expect.objectContaining({ id: 'B' })],
    })
  })

  it('uses a locale-independent thought id tie break for the shared read model', () => {
    const updatedAt = '2026-08-10T00:00:00.000Z'
    const sorted = sortThoughtReadModel([
      { ...remoteThought('z', 1), updated_at: updatedAt },
      { ...remoteThought('a', 2), updated_at: updatedAt },
      { ...remoteThought('é', 3), updated_at: updatedAt },
    ])

    expect(sorted.map((item) => item.id)).toEqual(['é', 'z', 'a'])
  })

  it('isolates selector rows and rejects a stale identity lease', async () => {
    const leaseA = identity('physical-A')
    const leaseB = identity('physical-B')
    await cacheServerThoughtPage(leaseA, [remoteThought('A', 1)])
    await cacheServerThoughtPage(leaseB, [remoteThought('B', 1)])
    await expect(selectThoughtReadModel(leaseA)).resolves.toMatchObject({ ok: true, value: [expect.objectContaining({ id: 'A' })] })
    await expect(selectThoughtReadModel(leaseB)).resolves.toMatchObject({ ok: true, value: [expect.objectContaining({ id: 'B' })] })
    leaseA.revoke()
    await expect(selectThoughtReadModel(leaseA)).resolves.toEqual({ ok: false })
  })
})

describe('observable Thought sync controller', () => {
  it('uses the required phase priority and durable operation counters', async () => {
    const lease = identity('controller-priority')
    await addLocalThought(lease, 'blocked-op')
    const syncRemote = vi.fn()
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(async () => ({
        ok: false as const,
        error: {
          kind: 'other' as const,
          status: 422,
          errorCode: 'invalid_thought_payload',
          message: 'body and quote must not leave this error category',
        },
      })),
      syncThoughts: syncRemote,
    }))

    await expect(controller.sync()).resolves.toMatchObject({ status: 'failed', pending: 1 })
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'failed',
      pendingCount: 1,
      blockedCount: 1,
      errorCode: 'other:invalid_thought_payload:422',
    }))

    const pending = controller.sync()
    // A fresh request owns the visible state while the earlier permanent
    // failure remains durable; this is the fixed syncing > failed priority.
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'syncing',
      pendingCount: 1,
      blockedCount: 1,
    })
    await expect(pending).resolves.toMatchObject({ status: 'failed', pending: 1 })
    expect(syncRemote).not.toHaveBeenCalled()

    const originalOnline = Object.getOwnPropertyDescriptor(window.navigator, 'onLine')
    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: false })
    try {
      await controller.sync()
      expect(controller.getSnapshot()).toMatchObject({ phase: 'offline', pendingCount: 1, blockedCount: 1 })
    } finally {
      if (originalOnline) Object.defineProperty(window.navigator, 'onLine', originalOnline)
      else delete (window.navigator as { onLine?: boolean }).onLine
    }
  })

  it('enters pending for durable future retries and synced only after a real empty pull', async () => {
    const lease = identity('controller-retry')
    await addLocalThought(lease, 'pending-op')
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ namespace: 'controller-retry', opId: 'pending-op' }),
    ])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ namespace: 'controller-retry' }),
    ])
    const now = Date.now()
    const database = await openUserDataDatabase()
    if (!database) throw new Error('IndexedDB must be available in this test')
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction(
        [THOUGHT_OUTBOX_STORE, THOUGHT_SYNC_STATE_STORE],
        'readwrite',
      )
      const outbox = transaction.objectStore(THOUGHT_OUTBOX_STORE)
      const request = outbox.index(THOUGHT_OUTBOX_NAMESPACE_INDEX).getAll(lease.context.physicalNamespace)
      const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const stateRequest = stateStore.get(lease.context.physicalNamespace)
      let outboxDone = false
      let stateDone = false
      const finish = () => {
        if (!outboxDone || !stateDone) return
        const row = request.result[0]
        if (!row || !stateRequest.result) { transaction.abort(); return }
        const nextOutbox = { ...row } as Record<string, unknown>
        delete nextOutbox.lastError
        nextOutbox.nextAttemptAt = now + 60_000
        outbox.put(nextOutbox)
        const nextState = { ...stateRequest.result } as Record<string, unknown>
        delete nextState.lastError
        delete nextState.lastErrorCode
        delete nextState.pullLastError
        nextState.retryAt = now + 60_000
        stateStore.put(nextState)
      }
      request.onsuccess = () => { outboxDone = true; finish() }
      request.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      stateRequest.onerror = () => transaction.abort()
      transaction.oncomplete = () => resolve()
      transaction.onerror = () => reject(transaction.error)
      transaction.onabort = () => reject(transaction.error)
    })

    const retryBlockedPull = vi.fn()
    const pendingController = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: retryBlockedPull,
    }))
    await pendingController.sync()
    expect(pendingController.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'pending',
      pendingCount: 1,
      blockedCount: 0,
      retryAt: now + 60_000,
    }))
    expect(retryBlockedPull).not.toHaveBeenCalled()

    const syncedLease = identity('controller-synced')
    const syncedController = getThoughtSyncController(syncedLease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))
    await syncedController.sync()
    expect(syncedController.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'synced',
      pendingCount: 0,
      blockedCount: 0,
      lastSuccessfulSyncAt: expect.any(Number),
    }))
  })

  it.each([
    [
      'a retryable 500 response',
      {
        kind: 'other',
        status: 500,
        errorCode: 'server_error',
        message: 'retryable internal error',
      },
      'other:server_error:500',
    ],
    [
      'a Retry-After 429 response',
      {
        kind: 'rate-limited',
        status: 429,
        errorCode: 'thought_cooldown',
        retryAfterSeconds: 2,
        message: 'retryable throttling response',
      },
      'rate-limited:thought_cooldown:429',
    ],
  ] satisfies ReadonlyArray<readonly [string, ApiError, string]>)('%s stays pending and failed without becoming blocked', async (_name, failure, errorCode) => {
    const lease = identity(`controller-${errorCode}`)
    await addLocalThought(lease)
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(async () => ({ ok: false as const, error: failure })),
      syncThoughts: vi.fn(),
    }))

    await expect(controller.sync()).resolves.toMatchObject({
      status: 'failed',
      pending: 1,
      retryAt: expect.any(Number),
      errorCode,
    })
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'failed',
      pendingCount: 1,
      blockedCount: 0,
      errorCode,
      retryAt: expect.any(Number),
    }))
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ status: 'pending', nextAttemptAt: expect.any(Number) }),
    ])
  })

  it('shares in-flight work, polls only while visible and online, and resumes immediately', async () => {
    vi.useFakeTimers()
    const lease = identity('controller-lifecycle')
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))
    const controller = getThoughtSyncController(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: syncRemote }))
    const stop = controller.start()
    await vi.runOnlyPendingTimersAsync()
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))

    await vi.advanceTimersByTimeAsync(THOUGHT_SYNC_POLL_MS)
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(2))

    const originalVisibility = Object.getOwnPropertyDescriptor(document, 'visibilityState')
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(THOUGHT_SYNC_POLL_MS * 2)
    expect(syncRemote).toHaveBeenCalledTimes(2)

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.runOnlyPendingTimersAsync()
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(3))

    const originalOnline = Object.getOwnPropertyDescriptor(window.navigator, 'onLine')
    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: false })
    window.dispatchEvent(new Event('offline'))
    await vi.advanceTimersByTimeAsync(THOUGHT_SYNC_POLL_MS * 2)
    expect(syncRemote).toHaveBeenCalledTimes(3)

    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: true })
    window.dispatchEvent(new Event('online'))
    await vi.runOnlyPendingTimersAsync()
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(4))
    if (originalOnline) Object.defineProperty(window.navigator, 'onLine', originalOnline)
    else delete (window.navigator as { onLine?: boolean }).onLine
    if (originalVisibility) Object.defineProperty(document, 'visibilityState', originalVisibility)
    stop()
  })

  it('returns one promise for concurrent manual and automatic wakeups', async () => {
    const lease = identity('controller-dedupe')
    let releasePull!: (result: Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>) => void
    const syncRemote = vi.fn(() => new Promise<Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>>(
      (resolve) => { releasePull = resolve },
    ))
    const controller = getThoughtSyncController(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: syncRemote }))
    expect(getThoughtSyncController(lease, client({ pushThoughtOps: vi.fn(), syncThoughts: vi.fn() }))).toBe(controller)
    const first = controller.sync()
    const second = controller.sync()
    expect(second).toBe(first)
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
    releasePull({ ok: true, data: { contract_version: 1, items: [] } })
    await expect(first).resolves.toMatchObject({ status: 'idle', pending: 0 })
  })

  it('immediately follows a running round when a durable local operation arrives', async () => {
    const lease = identity('controller-inflight-local-commit')
    type PullResult = Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>
    let releaseFirstPull!: (result: PullResult) => void
    const syncRemote = vi.fn<IdentityBoundReaderClient['syncThoughts']>()
      .mockImplementationOnce(() => new Promise<PullResult>((resolve) => { releaseFirstPull = resolve }))
      .mockResolvedValue({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => ({
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
    }))
    const controller = getThoughtSyncController(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    const stop = controller.start()

    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
    await addLocalThought(lease, 'arrived-during-pull', 'arrived-during-pull')
    window.dispatchEvent(new Event('webtag:annotations-change'))
    releaseFirstPull({ ok: true, data: { contract_version: 1, items: [] } })

    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(2))
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'synced',
      pendingCount: 0,
    }))
    stop()
  })

  it('keeps a queued lifecycle wake local while its running round finishes hidden', async () => {
    const lease = identity('controller-inflight-hidden-wake')
    type PullResult = Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>
    let releaseFirstPull!: (result: PullResult) => void
    const syncRemote = vi.fn<IdentityBoundReaderClient['syncThoughts']>()
      .mockImplementationOnce(() => new Promise<PullResult>((resolve) => { releaseFirstPull = resolve }))
      .mockResolvedValue({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: syncRemote,
    }))
    const stop = controller.start()
    const originalVisibility = Object.getOwnPropertyDescriptor(document, 'visibilityState')

    try {
      await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
      window.dispatchEvent(new Event('webtag:annotations-change'))
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
      document.dispatchEvent(new Event('visibilitychange'))
      releaseFirstPull({ ok: true, data: { contract_version: 1, items: [] } })

      await vi.waitFor(() => expect(controller.getSnapshot()).toMatchObject({ phase: 'synced' }))
      expect(syncRemote).toHaveBeenCalledTimes(1)

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
      document.dispatchEvent(new Event('visibilitychange'))
      await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(2))
    } finally {
      if (originalVisibility) Object.defineProperty(document, 'visibilityState', originalVisibility)
      else delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
      stop()
    }
  })

  it('fences a revoked controller response from a replacement namespace snapshot and IndexedDB state', async () => {
    const oldLease = identity('controller-identity-A', 1)
    await addLocalThought(oldLease, 'old-identity-op', 'old-identity-thought')
    type PushResult = Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
    let releaseOldPush!: (result: PushResult) => void
    const oldPush = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(() =>
      new Promise<PushResult>((resolve) => { releaseOldPush = resolve }),
    )
    const oldController = getThoughtSyncController(oldLease, client({
      pushThoughtOps: oldPush,
      syncThoughts: vi.fn(),
    }))
    const oldSync = oldController.sync()
    await vi.waitFor(() => expect(oldPush).toHaveBeenCalledTimes(1))

    oldLease.revoke()
    const newLease = identity('controller-identity-B', 2)
    const newController = getThoughtSyncController(newLease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))
    await newController.sync()

    releaseOldPush({
      ok: true as const,
      data: [{
        contract_version: 1 as const,
        op_id: 'old-identity-op',
        sequence: 1,
        disposition: 'applied' as const,
        submitted_key: { logical_clock: 1, device_id: 'device-test', op_id: 'old-identity-op' },
        current_winner_key: { logical_clock: 1, device_id: 'device-test', op_id: 'old-identity-op' },
      }],
    })

    await expect(oldSync).resolves.toMatchObject({ status: 'stale' })
    expect(newController.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'synced',
      pendingCount: 0,
      blockedCount: 0,
    }))
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ namespace: 'controller-identity-A', opId: 'old-identity-op' }),
    ])
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual(expect.arrayContaining([
      expect.objectContaining({ namespace: 'controller-identity-A' }),
      expect.objectContaining({ namespace: 'controller-identity-B', lastSuccessfulSyncAt: expect.any(Number) }),
    ]))
  })

  it('disposes a revoked controller, closes its channel, and ignores late lifecycle work', async () => {
    class FakeBroadcastChannel {
      static readonly instances = new Set<FakeBroadcastChannel>()
      closed = false
      readonly listeners = new Set<(event: MessageEvent<unknown>) => void>()

      constructor(readonly name: string) {
        FakeBroadcastChannel.instances.add(this)
      }

      addEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.add(listener as (event: MessageEvent<unknown>) => void)
      }

      removeEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.delete(listener as (event: MessageEvent<unknown>) => void)
      }

      postMessage(value: unknown): void {
        for (const instance of FakeBroadcastChannel.instances) {
          if (instance === this || instance.name !== this.name) continue
          for (const listener of instance.listeners) {
            listener(new MessageEvent('message', { data: structuredClone(value) }))
          }
        }
      }

      close(): void {
        this.closed = true
        FakeBroadcastChannel.instances.delete(this)
      }
    }

    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    const lease = identity('controller-revocation-dispose')
    type PushResult = Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
    type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
    let releasePush!: (result: PushResult) => void
    let pushedOperation: WireOperation | undefined
    let receivedSignal: AbortSignal | undefined
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>((request, options) => {
      pushedOperation = request.ops[0]
      receivedSignal = options?.signal
      return new Promise<PushResult>((resolve) => { releasePush = resolve })
    })
    const syncRemote = vi.fn(async () => ({
      ok: true as const,
      data: { contract_version: 1 as const, items: [] },
    }))
    const controller = getThoughtSyncController(lease, client({ pushThoughtOps, syncThoughts: syncRemote }))
    const observed: unknown[] = []
    controller.subscribe(() => observed.push(controller.getSnapshot()))
    const stop = controller.start()

    await vi.waitFor(() => expect(syncRemote).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(controller.getSnapshot()).toMatchObject({ phase: 'synced' }))
    await addLocalThought(lease, 'revoked-inflight-op', 'revoked-inflight-thought')
    window.dispatchEvent(new Event('webtag:annotations-change'))
    await vi.waitFor(() => expect(pushThoughtOps).toHaveBeenCalledTimes(1))
    const active = controller.sync()
    const channel = [...FakeBroadcastChannel.instances][0]
    if (!channel || !pushedOperation) throw new Error('expected an active sync channel and push operation')

    lease.revoke()
    expect(receivedSignal?.aborted).toBe(true)
    expect(channel.closed).toBe(true)
    expect(FakeBroadcastChannel.instances.size).toBe(0)
    expect(controller.getSnapshot()).toEqual({
      phase: 'offline',
      pendingCount: 0,
      blockedCount: 0,
    })
    const observedAfterRevocation = observed.length
    await expect(controller.sync()).resolves.toMatchObject({ status: 'stale' })

    window.dispatchEvent(new Event('online'))
    window.dispatchEvent(new Event('offline'))
    window.dispatchEvent(new Event('webtag:annotations-change'))
    window.dispatchEvent(new Event('webtag:thoughts-sync-request'))
    document.dispatchEvent(new Event('visibilitychange'))
    expect(pushThoughtOps).toHaveBeenCalledTimes(1)
    expect(syncRemote).toHaveBeenCalledTimes(1)

    const key = {
      logical_clock: pushedOperation.logical_clock,
      device_id: pushedOperation.device_id,
      op_id: pushedOperation.op_id,
    }
    releasePush({
      ok: true,
      data: [{
        contract_version: 1,
        op_id: pushedOperation.op_id,
        sequence: 10,
        disposition: 'applied',
        submitted_key: key,
        current_winner_key: key,
      }],
    })
    await expect(active).resolves.toMatchObject({ status: 'stale' })
    expect(controller.getSnapshot()).toEqual({
      phase: 'offline',
      pendingCount: 0,
      blockedCount: 0,
    })
    expect(observed).toHaveLength(observedAfterRevocation)
    stop()
    vi.unstubAllGlobals()
  })

  it('continues sending later operations when an earlier operation is permanently blocked', async () => {
    const lease = identity('controller-blocked-does-not-stall')
    await addLocalThought(lease, 'blocked-first', 'blocked-first')
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>()
      .mockResolvedValueOnce({
        ok: false as const,
        error: {
          kind: 'other' as const,
          status: 422,
          errorCode: 'invalid_thought_payload',
          message: 'invalid first operation',
        },
      })
      .mockImplementationOnce(async (request) => ({
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          contract_version: 1 as const,
          op_id: operation.op_id,
          sequence: index + 10,
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
      }))
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))

    await controller.sync()
    await addLocalThought(lease, 'send-after-block', 'send-after-block')
    await controller.sync()

    expect(pushThoughtOps).toHaveBeenCalledTimes(2)
    expect(pushThoughtOps.mock.calls[1]?.[0].ops.map((operation) => operation.op_id))
      .toEqual(['send-after-block'])
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([
      expect.objectContaining({ opId: 'blocked-first', status: 'blocked' }),
    ])
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'failed',
      pendingCount: 1,
      blockedCount: 1,
      errorCode: 'other:invalid_thought_payload:422',
    }))
  })

  it('keeps a blocked operation classification after later work and pull succeed', async () => {
    const lease = identity('controller-blocked-classification')
    await addLocalThought(lease, 'blocked-first', 'blocked-first')
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>()
      .mockResolvedValueOnce({
        ok: false as const,
        error: {
          kind: 'other' as const,
          status: 422,
          errorCode: 'invalid_thought_payload',
          message: 'server text must not replace the stable category',
        },
      })
      .mockImplementationOnce(async (request) => ({
        ok: true as const,
        data: request.ops.map((operation, index) => ({
          contract_version: 1 as const,
          op_id: operation.op_id,
          sequence: index + 20,
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
      }))
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))

    await controller.sync()
    await addLocalThought(lease, 'after-blocked', 'after-blocked')
    await controller.sync()

    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'failed',
      pendingCount: 1,
      blockedCount: 1,
      errorCode: 'other:invalid_thought_payload:422',
    }))
    expect(await readAll(THOUGHT_SYNC_STATE_STORE)).toEqual([
      expect.objectContaining({ lastErrorCode: 'other:invalid_thought_payload:422' }),
    ])
  })

  it('does not claim a successful empty sync while operations remain beyond a push round', async () => {
    const lease = identity('controller-large-outbox')
    await seedPendingOperations(lease, 501)
    const pushThoughtOps = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>(async (request) => ({
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
    }))
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps,
      syncThoughts: vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [] },
      })),
    }))

    await expect(controller.sync()).resolves.toMatchObject({
      status: 'idle',
      pushed: 500,
      pending: 1,
    })
    expect(pushThoughtOps).toHaveBeenCalledTimes(5)
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'pending',
      pendingCount: 1,
      blockedCount: 0,
    }))
    expect(controller.getSnapshot()).not.toHaveProperty('lastSuccessfulSyncAt')
  })

  it('keeps server failure text out of the sync result and durable status state', async () => {
    const lease = identity('controller-private-error')
    await addLocalThought(lease, 'private-error-op')
    const privateTransportText = 'opaque request body=private-note quote=private-target'
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(async () => ({
        ok: false as const,
        error: {
          kind: 'other' as const,
          status: 422,
          errorCode: 'invalid_thought_payload',
          message: privateTransportText,
        },
      })),
      syncThoughts: vi.fn(),
    }))

    const result = await controller.sync()
    const snapshot = controller.getSnapshot()
    const state = await readAll(THOUGHT_SYNC_STATE_STORE)
    const outbox = await readAll(THOUGHT_OUTBOX_STORE)

    expect(result).toMatchObject({
      status: 'failed',
      errorCode: 'other:invalid_thought_payload:422',
    })
    expect(JSON.stringify({ result, snapshot, state, outbox })).not.toContain(privateTransportText)
    expect(snapshot).toEqual(expect.objectContaining({
      errorCode: 'other:invalid_thought_payload:422',
    }))
  })

  it('turns malformed client output into a durable, payload-free failure category', async () => {
    const lease = identity('controller-malformed')
    const controller = getThoughtSyncController(lease, client({
      pushThoughtOps: vi.fn(),
      syncThoughts: vi.fn(async () => undefined),
    }))

    await expect(controller.sync()).resolves.toMatchObject({ status: 'failed' })
    expect(controller.getSnapshot()).toEqual(expect.objectContaining({
      phase: 'failed',
      errorCode: 'other:sync-exception',
    }))
    expect(JSON.stringify(controller.getSnapshot())).not.toMatch(/body|quote|target|payload/i)
  })

  it('broadcasts only namespace and invalidation kind across tabs', async () => {
    class FakeBroadcastChannel {
      static readonly instances = new Set<FakeBroadcastChannel>()
      static readonly messages: unknown[] = []
      readonly listeners = new Set<(event: MessageEvent<unknown>) => void>()

      constructor(readonly name: string) {
        FakeBroadcastChannel.instances.add(this)
      }

      addEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.add(listener as (event: MessageEvent<unknown>) => void)
      }

      removeEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.delete(listener as (event: MessageEvent<unknown>) => void)
      }

      postMessage(value: unknown): void {
        FakeBroadcastChannel.messages.push(structuredClone(value))
        for (const instance of FakeBroadcastChannel.instances) {
          if (instance === this || instance.name !== this.name) continue
          for (const listener of instance.listeners) {
            listener(new MessageEvent('message', { data: value }))
          }
        }
      }

      close(): void {
        FakeBroadcastChannel.instances.delete(this)
      }
    }

    const originalOnline = Object.getOwnPropertyDescriptor(window.navigator, 'onLine')
    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: false })
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    try {
      const firstLease = identity('shared-namespace', 1)
      const secondLease = identity('shared-namespace', 2)
      const controllerClient = client({ pushThoughtOps: vi.fn(), syncThoughts: vi.fn() })
      const first = getThoughtSyncController(firstLease, controllerClient)
      const second = getThoughtSyncController(secondLease, controllerClient)
      const stopFirst = first.start()
      const stopSecond = second.start()

      window.dispatchEvent(new Event('webtag:annotations-change'))
      await vi.waitFor(() => expect(FakeBroadcastChannel.messages.length).toBeGreaterThan(0))
      for (const message of FakeBroadcastChannel.messages) {
        expect(message).toEqual({
          kind: 'thought-sync-invalidation',
          namespace: 'shared-namespace',
          invalidation: 'outbox',
        })
        expect(JSON.stringify(message)).not.toMatch(/body|quote|target|payload/i)
      }

      stopFirst()
      stopSecond()
    } finally {
      vi.unstubAllGlobals()
      if (originalOnline) Object.defineProperty(window.navigator, 'onLine', originalOnline)
      else delete (window.navigator as { onLine?: boolean }).onLine
    }
  })

  it('broadcasts one sync invalidation from a lock holder and refreshes a lock loser without a loop', async () => {
    class FakeBroadcastChannel {
      static readonly instances = new Set<FakeBroadcastChannel>()
      static readonly messages: unknown[] = []
      readonly listeners = new Set<(event: MessageEvent<unknown>) => void>()

      constructor(readonly name: string) {
        FakeBroadcastChannel.instances.add(this)
      }

      addEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.add(listener as (event: MessageEvent<unknown>) => void)
      }

      removeEventListener(_type: 'message', listener: EventListener): void {
        this.listeners.delete(listener as (event: MessageEvent<unknown>) => void)
      }

      postMessage(value: unknown): void {
        FakeBroadcastChannel.messages.push(structuredClone(value))
        for (const instance of FakeBroadcastChannel.instances) {
          if (instance === this || instance.name !== this.name) continue
          for (const listener of instance.listeners) {
            listener(new MessageEvent('message', { data: structuredClone(value) }))
          }
        }
      }

      close(): void {
        FakeBroadcastChannel.instances.delete(this)
      }
    }

    const originalOnline = Object.getOwnPropertyDescriptor(window.navigator, 'onLine')
    const originalLocks = Object.getOwnPropertyDescriptor(window.navigator, 'locks')
    let lockHeld = false
    const request = vi.fn(async (
      _name: string,
      _options: { ifAvailable: boolean },
      callback: (lock: object | null) => Promise<unknown>,
    ) => {
      if (lockHeld) return callback(null)
      lockHeld = true
      try {
        return await callback({})
      } finally {
        lockHeld = false
      }
    })
    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: false })
    Object.defineProperty(window.navigator, 'locks', { configurable: true, value: { request } })
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    try {
      const firstLease = identity('lock-race-namespace', 1)
      const secondLease = identity('lock-race-namespace', 2)
      await addLocalThought(firstLease, 'lock-race-op', 'lock-race-local')
      type PushResult = Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
      type WireOperation = Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0]['ops'][number]
      let releaseFirstPush!: (result: PushResult) => void
      let operation: WireOperation | undefined
      const firstPush = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>((request) => {
        operation = request.ops[0]
        return new Promise<PushResult>((resolve) => { releaseFirstPush = resolve })
      })
      const firstPull = vi.fn(async () => ({
        ok: true as const,
        data: { contract_version: 1 as const, items: [remoteThought('lock-race-remote', 7)] },
      }))
      const secondPush = vi.fn<IdentityBoundReaderClient['pushThoughtOps']>()
      const secondPull = vi.fn<IdentityBoundReaderClient['syncThoughts']>()
      const first = getThoughtSyncController(firstLease, client({
        pushThoughtOps: firstPush,
        syncThoughts: firstPull,
      }))
      const second = getThoughtSyncController(secondLease, client({
        pushThoughtOps: secondPush,
        syncThoughts: secondPull,
      }))
      const stopFirst = first.start()
      const stopSecond = second.start()
      Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: true })

      const firstRun = first.sync()
      await vi.waitFor(() => expect(firstPush).toHaveBeenCalledTimes(1))
      await expect(second.sync()).resolves.toMatchObject({ status: 'idle', pending: 1 })
      expect(second.getSnapshot()).toEqual(expect.objectContaining({
        phase: 'pending',
        pendingCount: 1,
      }))
      expect(secondPush).not.toHaveBeenCalled()
      expect(secondPull).not.toHaveBeenCalled()
      if (!operation) throw new Error('expected the lock holder operation')
      const key = {
        logical_clock: operation.logical_clock,
        device_id: operation.device_id,
        op_id: operation.op_id,
      }
      releaseFirstPush({
        ok: true,
        data: [{
          contract_version: 1,
          op_id: operation.op_id,
          sequence: 7,
          disposition: 'applied',
          submitted_key: key,
          current_winner_key: key,
        }],
      })
      await expect(firstRun).resolves.toMatchObject({ status: 'synced', pushed: 1, pulled: 1 })

      await vi.waitFor(async () => {
        await expect(listRemoteThoughts(secondLease, 'L1', TARGET)).resolves.toMatchObject({
          ok: true,
          value: [expect.objectContaining({ annotationId: 'lock-race-remote', serverSequence: 7 })],
        })
        expect(second.getSnapshot()).toEqual(expect.objectContaining({
          phase: 'synced',
          pendingCount: 0,
          blockedCount: 0,
        }))
      })
      expect(FakeBroadcastChannel.messages).toEqual([{
        kind: 'thought-sync-invalidation',
        namespace: 'lock-race-namespace',
        invalidation: 'sync',
      }])
      expect(JSON.stringify(FakeBroadcastChannel.messages)).not.toMatch(/body|quote|target|payload/i)
      await Promise.resolve()
      await Promise.resolve()
      expect(FakeBroadcastChannel.messages).toHaveLength(1)
      expect(secondPush).not.toHaveBeenCalled()
      expect(secondPull).not.toHaveBeenCalled()
      stopFirst()
      stopSecond()
    } finally {
      vi.unstubAllGlobals()
      if (originalOnline) Object.defineProperty(window.navigator, 'onLine', originalOnline)
      else delete (window.navigator as { onLine?: boolean }).onLine
      if (originalLocks) Object.defineProperty(window.navigator, 'locks', originalLocks)
      else delete (window.navigator as { locks?: unknown }).locks
    }
  })

})
