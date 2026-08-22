import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it } from 'vitest'

import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  commitSupersessionRecovery,
} from './annotation-store'
import {
  readThoughtSupersessionRecoveryStates,
} from './thought-supersession'
import {
  ANNOTATED_LINKS_STORE,
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SYNC_STATE_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from './idb'
import type {
  ThoughtMaterializedRecord,
  ThoughtSupersessionEventRecord,
} from './thought-types'

const TARGET = { kind: 'saved-content', contentRevision: 3 } as const

function identity(namespace: string): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

function event(namespace: string, eventSequence = 7): ThoughtSupersessionEventRecord {
  const operation = (opId: string, logicalClock: number, body: string) => ({
    sequence: logicalClock + 100,
    opId,
    deviceId: 'device-1',
    logicalClock,
    operationKind: 'update' as const,
    annotationId: 'annotation-1',
    hostKind: 'link' as const,
    hostId: 'L1',
    target: TARGET,
    targetKey: 'saved-content:3',
    body,
    source: 'self' as const,
    quote: { exact: 'quote', start: 0, end: 5, prefix: '', suffix: '' },
    createdAt: '2026-08-11T00:00:00.000Z',
  })
  return {
    key: [namespace, eventSequence],
    namespace,
    eventSequence,
    annotationId: 'annotation-1',
    loser: operation('loser-op', 4, 'loser body'),
    winnerAtDetection: operation('winner-op', 5, 'winner body'),
  }
}

function deleteEvent(namespace: string): ThoughtSupersessionEventRecord {
  const storedEvent = event(namespace)
  return {
    ...storedEvent,
    loser: {
      ...storedEvent.loser,
      operationKind: 'delete',
      body: '',
      quote: null,
    },
  }
}

function winner(namespace: string, lifecycleStatus: 'active' | 'tombstone' = 'active'): ThoughtMaterializedRecord {
  return {
    key: [namespace, 'annotation-1'],
    namespace,
    annotationId: 'annotation-1',
    contractVersion: 1,
    winnerKey: { logicalClock: 9, deviceId: 'winner-device', opId: 'winner-current' },
    hostKind: 'link',
    hostId: 'L1',
    linkId: 'L1',
    target: TARGET,
    targetKey: 'saved-content:3',
    quote: { exact: 'quote', start: 0, end: 5, prefix: '', suffix: '' },
    body: 'current winner body',
    source: 'self',
    deleted: false,
    lifecycleStatus,
    serverSequence: 17,
    createdAt: '2026-08-11T00:00:00.000Z',
    updatedAt: '2026-08-11T00:00:00.000Z',
  }
}

async function seed(
  lease: IdentityLease,
  lifecycleStatus: 'active' | 'tombstone' = 'active',
  storedEvent = event(lease.context.physicalNamespace),
) {
  const namespace = lease.context.physicalNamespace
  await runUserDataTransaction(
    lease,
    'seed supersession recovery',
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
      ANNOTATION_IMPORTS_STORE,
      THOUGHT_OUTBOX_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
      THOUGHT_SUPERSESSION_EVENTS_STORE,
    ],
    'readwrite',
    (transaction, _operation, setResult) => {
      transaction.objectStore(THOUGHT_SUPERSESSION_EVENTS_STORE).put(storedEvent)
      transaction.objectStore(THOUGHT_MATERIALIZED_STORE).put(winner(namespace, lifecycleStatus))
      transaction.objectStore(THOUGHT_SYNC_STATE_STORE).put({
        namespace,
        cursor: '',
        deviceId: 'recovery-device',
        tabToken: 'tab',
        logicalClockFloor: 20,
        updatedAt: 1,
      })
      setResult(undefined)
    },
  )
  return storedEvent
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

afterEach(async () => {
  await deleteDatabase()
})

describe('thought supersession recovery', () => {
  it('creates one local update with the current winner anchor and max-observed clock', async () => {
    const lease = identity('recovery-a')
    const storedEvent = await seed(lease)
    const result = await commitSupersessionRecovery(lease, storedEvent)
    expect(result).toMatchObject({ ok: true, value: { status: 'committed' } })

    const outbox = await readAll(THOUGHT_OUTBOX_STORE)
    expect(outbox).toMatchObject([{
      namespace: 'recovery-a',
      opId: 'supersession-recovery:7',
      logicalClock: 21,
      operationKind: 'update',
      annotationId: 'annotation-1',
      hostId: 'L1',
      target: TARGET,
      patch: { note: 'loser body', source: 'self', updatedAt: 7 },
      recoveryOf: { logicalClock: 4, deviceId: 'device-1', opId: 'loser-op' },
      expectedCurrentWinnerKey: {
        logicalClock: 9,
        deviceId: 'winner-device',
        opId: 'winner-current',
      },
    }])
    const retry = await commitSupersessionRecovery(lease, storedEvent)
    expect(retry).toMatchObject({ ok: true, value: { status: 'duplicate' } })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toHaveLength(1)
  })

  it('does not write a recovery candidate while the host is tombstoned', async () => {
    const lease = identity('recovery-b')
    const storedEvent = await seed(lease, 'tombstone')
    const result = await commitSupersessionRecovery(lease, storedEvent)
    expect(result).toEqual({ ok: true, value: { status: 'unrecoverable', reason: 'host-tombstoned' } })
    expect(await readAll(THOUGHT_OUTBOX_STORE)).toEqual([])
    expect(await readAll(ANNOTATION_OPS_STORE)).toEqual([])
  })

  it('restores a delete loser as one same-identity delete operation', async () => {
    const lease = identity('recovery-delete')
    const storedEvent = await seed(lease, 'active', deleteEvent(lease.context.physicalNamespace))
    const result = await commitSupersessionRecovery(lease, storedEvent)
    expect(result).toMatchObject({ ok: true, value: { status: 'committed' } })

    expect(await readAll(THOUGHT_OUTBOX_STORE)).toMatchObject([{
      namespace: 'recovery-delete',
      opId: 'supersession-recovery:7',
      operationKind: 'delete',
      annotationId: 'annotation-1',
      hostId: 'L1',
      target: TARGET,
      recoveryOf: { logicalClock: 4, deviceId: 'device-1', opId: 'loser-op' },
      expectedCurrentWinnerKey: {
        logicalClock: 9,
        deviceId: 'winner-device',
        opId: 'winner-current',
      },
    }])
    expect(await readAll(ANNOTATION_OPS_STORE)).toHaveLength(1)
  })

  it('reports a durable CAS conflict without reading recovery payloads', async () => {
    const lease = identity('recovery-cas-state')
    const storedEvent = await seed(lease)
    await commitSupersessionRecovery(lease, storedEvent)

    await runUserDataTransaction(
      lease,
      'mark recovery CAS conflict',
      [THOUGHT_OUTBOX_STORE],
      'readwrite',
      (transaction, _operation, setResult) => {
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
            status: 'blocked',
            blockedReason: 'thought_recovery_conflict',
            recoveryConflict: true,
          })
          setResult(undefined)
        }
      },
    )

    const states = await readThoughtSupersessionRecoveryStates(lease, [storedEvent])
    expect(states).toMatchObject({ ok: true })
    if (states.ok) {
      expect(states.value.get(storedEvent.eventSequence)).toBe('recovery-conflict')
    }
  })
})
