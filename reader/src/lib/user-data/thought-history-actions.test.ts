import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it } from 'vitest'

import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
} from './idb'
import {
  commitThoughtHistoryAction,
  readThoughtActionSnapshot,
  type ThoughtHistorySnapshotInput,
} from './thought-history-actions'
import type { ThoughtHistoryOutboxRecord, ThoughtMaterializedRecord } from './thought-types'

function identity(namespace: string): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

function snapshot(overrides: Partial<ThoughtHistorySnapshotInput> = {}): ThoughtHistorySnapshotInput {
  const hostId = overrides.hostId ?? 'L1'
  return {
    id: 'thought-1',
    hostKind: 'link',
    hostId,
    target: {
      kind: 'saved-content',
      host_id: hostId,
      version: { content_revision: 7 },
    },
    quote: {
      exact: 'selected text',
      start: 0,
      end: 13,
      prefix: '',
      suffix: '',
      block_key: 'content-document',
    },
    body: 'frozen thought body',
    source: 'reader-history',
    lastSequence: 11,
    winnerKey: {
      logicalClock: 11,
      deviceId: 'server-device',
      opId: 'server-winner-11',
    },
    ...overrides,
  }
}

async function historyRecords(): Promise<ThoughtHistoryOutboxRecord[]> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('IndexedDB must be available in this test')
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_HISTORY_OUTBOX_STORE, 'readonly')
    const request = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE).getAll()
    transaction.oncomplete = () => resolve(request.result as ThoughtHistoryOutboxRecord[])
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

async function putMaterialized(record: ThoughtMaterializedRecord): Promise<void> {
  const database = await openUserDataDatabase()
  if (!database) throw new Error('IndexedDB must be available in this test')
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_MATERIALIZED_STORE, 'readwrite')
    transaction.objectStore(THOUGHT_MATERIALIZED_STORE).put(record)
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
  await deleteUserDataDatabase()
})

describe('durable thought history actions', () => {
  it('resolves live delete authority only from the current identity materialized snapshot', async () => {
    const lease = identity('physical-A')
    await putMaterialized({
      key: ['physical-A', 'thought-live'],
      namespace: 'physical-A',
      annotationId: 'thought-live',
      contractVersion: 1,
      winnerKey: { logicalClock: 17, deviceId: 'server-device', opId: 'server-op-17' },
      hostKind: 'link',
      hostId: 'L1',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      targetKey: 'saved-content:7',
      quote: { exact: 'selected text', start: 0, end: 13, prefix: '', suffix: '' },
      body: 'materialized body',
      source: 'user',
      deleted: false,
      lifecycleStatus: 'active',
      serverSequence: 21,
      createdAt: '2026-08-10T01:00:00.000Z',
      updatedAt: '2026-08-10T02:00:00.000Z',
    })

    await expect(readThoughtActionSnapshot(lease, 'thought-live')).resolves.toEqual({
      ok: true,
      value: {
        id: 'thought-live',
        hostKind: 'link',
        hostId: 'L1',
        target: {
          kind: 'saved-content',
          host_id: 'L1',
          version: { content_revision: 7 },
        },
        quote: { exact: 'selected text', start: 0, end: 13, prefix: '', suffix: '' },
        body: 'materialized body',
        source: 'user',
        lastSequence: 21,
        winnerKey: { logicalClock: 17, deviceId: 'server-device', opId: 'server-op-17' },
      },
    })
    await expect(readThoughtActionSnapshot(identity('physical-B'), 'thought-live'))
      .resolves.toEqual({ ok: true, value: null })
  })

  it('clones a reattach snapshot before creating an Inbox-targeted action', async () => {
    const lease = identity('physical-A')
    const thought = snapshot()

    await expect(commitThoughtHistoryAction(lease, {
      action: 'reattach',
      opId: 'reattach-inbox',
      thought,
      targetHostKind: 'inbox',
      targetHostId: 'inbox-1',
      expectedHostRevision: 3,
    })).resolves.toEqual({ ok: true, value: { status: 'committed', opId: 'reattach-inbox' } })

    const mutableThought = thought as {
      target: { version: { content_revision: number } }
      quote: { exact: string }
      winnerKey: { logicalClock: number }
      body: string
    }
    mutableThought.target.version.content_revision = 99
    mutableThought.quote.exact = 'mutated after commit'
    mutableThought.winnerKey.logicalClock = 99
    mutableThought.body = 'mutated body'

    await expect(historyRecords()).resolves.toEqual([expect.objectContaining({
      key: ['physical-A', 'reattach-inbox'],
      namespace: 'physical-A',
      action: 'reattach',
      hostKind: 'inbox',
      hostId: 'inbox-1',
      target: { kind: 'inbox', metadataRevision: 3 },
      targetKey: 'inbox:3',
      expectedLastSequence: 11,
      expectedHostRevision: 3,
      snapshot: expect.objectContaining({
        body: 'frozen thought body',
        source: 'reader-history',
        target: {
          kind: 'saved-content',
          host_id: 'L1',
          version: { content_revision: 7 },
        },
        quote: expect.objectContaining({ exact: 'selected text' }),
        winnerKey: {
          logicalClock: 11,
          deviceId: 'server-device',
          opId: 'server-winner-11',
        },
      }),
      logicalClock: 12,
      deviceId: expect.any(String),
    })])
  })

  it('requires a safe server winner key and fails closed at the Lamport maximum', async () => {
    const lease = identity('physical-A')

    await expect(commitThoughtHistoryAction(lease, {
      action: 'delete',
      opId: 'invalid-winner-key',
      thought: snapshot({
        winnerKey: {
          logicalClock: 11.5,
          deviceId: 'server-device',
          opId: 'invalid-winner',
        },
      }),
    })).resolves.toEqual({ ok: false })
    await expect(historyRecords()).resolves.toEqual([])

    await expect(commitThoughtHistoryAction(lease, {
      action: 'delete',
      opId: 'exhausted-winner-key',
      thought: snapshot({
        winnerKey: {
          logicalClock: Number.MAX_SAFE_INTEGER,
          deviceId: 'server-device',
          opId: 'exhausted-winner',
        },
      }),
    })).resolves.toEqual({ ok: false })
    await expect(historyRecords()).resolves.toEqual([])
  })

  it('allows a historical delete without a quote', async () => {
    const lease = identity('physical-A')

    await expect(commitThoughtHistoryAction(lease, {
      action: 'delete',
      opId: 'delete-without-quote',
      thought: snapshot({ quote: null }),
    })).resolves.toEqual({ ok: true, value: { status: 'committed', opId: 'delete-without-quote' } })

    const [record] = await historyRecords()
    expect(record).toMatchObject({
      action: 'delete',
      snapshot: { quote: null },
    })
    expect(record).not.toHaveProperty('expectedHostRevision')
  })

  it('rejects reattach when the frozen snapshot has no valid quote', async () => {
    const lease = identity('physical-A')

    await expect(commitThoughtHistoryAction(lease, {
      action: 'reattach',
      opId: 'reattach-without-quote',
      thought: snapshot({ quote: null }),
      targetHostKind: 'note',
      targetHostId: 'N1',
      expectedHostRevision: 4,
    })).resolves.toEqual({ ok: false })
    await expect(historyRecords()).resolves.toEqual([])
  })

  it('treats an exact duplicate op ID as idempotent and rejects altered evidence', async () => {
    const lease = identity('physical-A')
    const action = {
      action: 'delete' as const,
      opId: 'stable-delete',
      thought: snapshot(),
    }

    await expect(commitThoughtHistoryAction(lease, action))
      .resolves.toEqual({ ok: true, value: { status: 'committed', opId: 'stable-delete' } })
    await expect(commitThoughtHistoryAction(lease, action))
      .resolves.toEqual({ ok: true, value: { status: 'duplicate', opId: 'stable-delete' } })
    await expect(commitThoughtHistoryAction(lease, {
      ...action,
      thought: snapshot({ body: 'changed candidate evidence' }),
    })).resolves.toEqual({ ok: true, value: { status: 'op-id-conflict', opId: 'stable-delete' } })
    await expect(historyRecords()).resolves.toHaveLength(1)
  })

  it('isolates same operation IDs across identity namespaces', async () => {
    const action = {
      action: 'delete' as const,
      opId: 'same-id',
      thought: snapshot(),
    }

    await expect(commitThoughtHistoryAction(identity('physical-A'), action))
      .resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitThoughtHistoryAction(identity('physical-B'), action))
      .resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    const records = await historyRecords()
    expect(records).toHaveLength(2)
    expect(records.map((record) => record.namespace).sort()).toEqual(['physical-A', 'physical-B'])
  })
})
