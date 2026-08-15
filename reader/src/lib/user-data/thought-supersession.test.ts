import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it } from 'vitest'

import type { IdentityBoundReaderClient } from '../api/client'
import type { ReaderThoughtSupersessionEventResponse } from '../api/types'
import type { IdentityLease } from '../identity'
import { IdentityLease as Lease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SUPERSESSION_STATE_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_SYNC_STATE_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from './idb'
import {
  listThoughtSupersessionEvents,
  readThoughtSupersessionRecoveryAvailability,
  syncThoughtSupersessions,
} from './thought-supersession'

function identity(namespace: string, epoch = 1): IdentityLease {
  return new Lease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: epoch,
  })
}

function event(sequence = 7, quoteHasOffsets = true): Record<string, unknown> {
  const operation = (opID: string, logicalClock: number, body: string) => ({
    contract_version: 1,
    sequence: logicalClock + 100,
    op_id: opID,
    device_id: 'device-1',
    logical_clock: logicalClock,
    operation_kind: 'update',
    annotation_id: 'annotation-1',
    host_kind: 'link',
    host_id: 'L1',
    target: {
      kind: 'saved-content',
      host_id: 'L1',
      version: { content_revision: 3 },
    },
    payload: {
      body,
      source: 'self',
      quote: quoteHasOffsets
        ? { exact: 'quote', start: 0, end: 5, prefix: '', suffix: '' }
        : { exact: 'quote', prefix: '', suffix: '' },
    },
    created_at: '2026-08-11T00:00:00.000Z',
  })
  return {
    sequence,
    annotation_id: 'annotation-1',
    loser: operation('loser-op', 4, 'loser body'),
    winner_at_detection: operation('winner-op', 5, 'winner body'),
  }
}

const EVENT_CURSOR_7 = 'N3xldmVudA'

function client(pages: readonly unknown[]): IdentityBoundReaderClient {
  let index = 0
  return {
    listThoughtSupersessions: async () => {
      const data = pages[Math.min(index, pages.length - 1)]
      index += 1
      return { ok: true, data }
    },
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

afterEach(async () => {
  await deleteDatabase()
})

describe('thought supersession event sync', () => {
  it('persists immutable events and their separate cursor atomically', async () => {
    const lease = identity('event-a')
    await runUserDataTransaction(
      lease,
      'seed ordinary thought cursor',
      [THOUGHT_SYNC_STATE_STORE],
      'readwrite',
      (transaction, _operation, setResult) => {
        transaction.objectStore(THOUGHT_SYNC_STATE_STORE).put({
          namespace: lease.context.physicalNamespace,
          cursor: 'ordinary-cursor',
          deviceId: 'device',
          tabToken: 'tab',
          updatedAt: 1,
        })
        setResult(undefined)
      },
    )
    const synced = await syncThoughtSupersessions(lease, client([
      { contract_version: 1, items: [event()], next_cursor: EVENT_CURSOR_7 },
      { contract_version: 1, items: [] },
    ]))
    expect(synced).toMatchObject({ status: 'synced', pulled: 1, cursor: EVENT_CURSOR_7 })

    const events = await listThoughtSupersessionEvents(lease)
    expect(events).toMatchObject({ ok: true })
    if (events.ok) {
      expect(events.value).toHaveLength(1)
      expect(events.value[0]).toMatchObject({
        eventSequence: 7,
        annotationId: 'annotation-1',
        loser: { body: 'loser body', logicalClock: 4 },
        winnerAtDetection: { body: 'winner body', logicalClock: 5 },
      })
    }

    const eventState = await readAll(THOUGHT_SUPERSESSION_STATE_STORE)
    expect(eventState).toMatchObject([{ namespace: 'event-a', cursor: EVENT_CURSOR_7 }])
    const ordinaryState = await readAll(THOUGHT_SYNC_STATE_STORE)
    expect(ordinaryState).toMatchObject([{ namespace: 'event-a', cursor: 'ordinary-cursor' }])
  })

  it('persists legacy exact-only events, advances the cursor, and keeps recovery unavailable', async () => {
    const lease = identity('event-exact-only')
    const synced = await syncThoughtSupersessions(lease, client([
      { contract_version: 1, items: [event(7, false)], next_cursor: EVENT_CURSOR_7 },
      { contract_version: 1, items: [] },
    ]))
    expect(synced).toMatchObject({ status: 'synced', pulled: 1, cursor: EVENT_CURSOR_7 })

    const events = await listThoughtSupersessionEvents(lease)
    expect(events).toMatchObject({ ok: true })
    if (!events.ok) return
    expect(events.value).toHaveLength(1)
    expect(events.value[0].loser.quote).toEqual({ exact: 'quote', prefix: '', suffix: '' })
    expect(events.value[0].winnerAtDetection.quote).toEqual({ exact: 'quote', prefix: '', suffix: '' })
    expect(await readAll(THOUGHT_SUPERSESSION_STATE_STORE)).toMatchObject([
      { namespace: 'event-exact-only', cursor: EVENT_CURSOR_7 },
    ])

    await runUserDataTransaction(
      lease,
      'seed exact-only current winner',
      [THOUGHT_MATERIALIZED_STORE],
      'readwrite',
      (transaction, _operation, setResult) => {
        transaction.objectStore(THOUGHT_MATERIALIZED_STORE).put({
          key: [lease.context.physicalNamespace, 'annotation-1'],
          namespace: lease.context.physicalNamespace,
          annotationId: 'annotation-1',
          contractVersion: 1,
          winnerKey: { logicalClock: 5, deviceId: 'device-1', opId: 'winner-op' },
          hostKind: 'link',
          hostId: 'L1',
          linkId: 'L1',
          target: { kind: 'saved-content', contentRevision: 3 },
          targetKey: 'saved-content:3',
          quote: { exact: 'quote' },
          body: 'winner body',
          source: 'self',
          deleted: false,
          serverSequence: 105,
          createdAt: '2026-08-11T00:00:00.000Z',
          updatedAt: '2026-08-11T00:00:00.000Z',
        })
        setResult(undefined)
      },
    )
    const availability = await readThoughtSupersessionRecoveryAvailability(lease, events.value)
    expect(availability).toEqual({
      ok: true,
      value: new Map([[7, 'target-or-quote-incomplete']]),
    })
  })

  it('fails closed before event or cursor writes for a malformed page', async () => {
    const lease = identity('event-b')
    const malformed = event()
    delete malformed.winner_at_detection
    const synced = await syncThoughtSupersessions(lease, client([
      { contract_version: 1, items: [malformed], next_cursor: 'event-cursor-7' },
    ]))
    expect(synced.status).toBe('failed')
    expect(await readAll(THOUGHT_SUPERSESSION_EVENTS_STORE)).toEqual([])
    expect(await readAll(THOUGHT_SUPERSESSION_STATE_STORE)).toEqual([])
  })

  it('keeps event rows identity-scoped', async () => {
    const owner = identity('event-owner')
    const other = identity('event-other')
    await syncThoughtSupersessions(owner, client([
      { contract_version: 1, items: [event()], next_cursor: EVENT_CURSOR_7 },
      { contract_version: 1, items: [] },
    ]))
    const events = await listThoughtSupersessionEvents(other)
    expect(events).toEqual({ ok: true, value: [] })
  })

  it('keeps one immutable event after a reload resumes its separate cursor', async () => {
    const firstLease = identity('event-reload', 1)
    await syncThoughtSupersessions(firstLease, client([
      { contract_version: 1, items: [event()], next_cursor: EVENT_CURSOR_7 },
      { contract_version: 1, items: [] },
    ]))

    const reloadedLease = identity('event-reload', 2)
    const synced = await syncThoughtSupersessions(reloadedLease, client([
      { contract_version: 1, items: [] },
    ]))
    expect(synced).toMatchObject({ status: 'synced', pulled: 0, cursor: EVENT_CURSOR_7 })
    const events = await listThoughtSupersessionEvents(reloadedLease)
    expect(events).toMatchObject({ ok: true, value: [expect.objectContaining({ eventSequence: 7 })] })
  })

  it('drops an old-identity response before it reaches any event store', async () => {
    const oldLease = identity('event-old')
    type SupersessionPageResult = Awaited<ReturnType<IdentityBoundReaderClient['listThoughtSupersessions']>>
    let releaseResponse: ((value: SupersessionPageResult) => void) | undefined
    let markRequested: (() => void) | undefined
    const requested = new Promise<void>((resolve) => { markRequested = resolve })
    const delayedClient = {
      listThoughtSupersessions: async () => {
        markRequested?.()
        return new Promise<SupersessionPageResult>((resolve) => { releaseResponse = resolve })
      },
    } as IdentityBoundReaderClient

    const pending = syncThoughtSupersessions(oldLease, delayedClient)
    await requested
    oldLease.revoke()
    releaseResponse?.({
      ok: true,
      data: {
        contract_version: 1,
        items: [event() as unknown as ReaderThoughtSupersessionEventResponse],
        next_cursor: EVENT_CURSOR_7,
      },
    })

    expect(await pending).toMatchObject({ status: 'stale', pulled: 0 })
    const nextLease = identity('event-new')
    expect(await listThoughtSupersessionEvents(nextLease)).toEqual({ ok: true, value: [] })
    expect(await readAll(THOUGHT_SUPERSESSION_EVENTS_STORE)).toEqual([])
    expect(await readAll(THOUGHT_SUPERSESSION_STATE_STORE)).toEqual([])
  })

  it('rejects a malformed event cursor before event or cursor writes', async () => {
    const lease = identity('event-invalid-cursor')
    const synced = await syncThoughtSupersessions(lease, client([
      { contract_version: 1, items: [event()], next_cursor: 'not-a-valid-event-cursor' },
    ]))
    expect(synced.status).toBe('failed')
    expect(await readAll(THOUGHT_SUPERSESSION_EVENTS_STORE)).toEqual([])
    expect(await readAll(THOUGHT_SUPERSESSION_STATE_STORE)).toEqual([])
  })

  it('rejects a skip-ahead cursor before event or cursor writes', async () => {
    const lease = identity('event-skip-ahead')
    const synced = await syncThoughtSupersessions(lease, client([
      { contract_version: 1, items: [event()], next_cursor: 'OHxldmVudA' },
    ]))
    expect(synced.status).toBe('failed')
    expect(await readAll(THOUGHT_SUPERSESSION_EVENTS_STORE)).toEqual([])
    expect(await readAll(THOUGHT_SUPERSESSION_STATE_STORE)).toEqual([])
  })
})
