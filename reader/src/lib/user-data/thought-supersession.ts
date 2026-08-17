import type { IdentityBoundReaderClient } from '../api/client'
import { isReaderThoughtSupersessionEventsResponse } from '../api/guards'
import type {
  ReaderThoughtSupersessionEventResponse,
  ReaderThoughtSupersessionOperationResponse,
} from '../api/types'
import type { IdentityLease } from '../identity'
import { isRecord } from '../records'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type AnnotationTarget,
} from './annotation-types'
import { isSafeNonNegativeInteger } from './annotation-codec'
import {
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SUPERSESSION_STATE_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_OP_ID_INDEX,
  THOUGHT_OUTBOX_STORE,
  runUserDataTransaction,
  type UserDataTransactionResult,
} from './idb'
import {
  isValidThoughtIdentifier,
  isValidThoughtMaterializedRecord,
  isValidThoughtOutboxRecord,
  isValidThoughtSupersessionEventRecord,
  isValidThoughtSupersessionSyncStateRecord,
  isValidThoughtVersionKey,
  compareThoughtVersionKeys,
  thoughtSupersessionRecoveryOperationID,
  MAX_THOUGHT_LOGICAL_CLOCK,
  type ThoughtSupersessionEventRecord,
  type ThoughtSupersessionOperation,
  type ThoughtSupersessionSyncStateRecord,
  type ThoughtMaterializedRecord,
  type ThoughtVersionKey,
} from './thought-types'

const MAX_PULL_PAGES = 20
const MAX_BATCH = 100

const inFlight = new WeakMap<IdentityLease, Promise<ThoughtSupersessionSyncResult>>()

export interface ThoughtSupersessionSyncResult {
  readonly status: 'synced' | 'failed' | 'stale'
  readonly pulled: number
  readonly cursor: string
  readonly error?: string
}

export type ThoughtSupersessionRecoveryAvailability =
  | 'ready'
  | 'host-tombstoned'
  | 'missing-current-winner'
  | 'current-winner-deleted'
  | 'target-or-quote-incomplete'

/** Durable local state of the one idempotent recovery candidate for an event. */
export type ThoughtSupersessionRecoveryState =
  | 'not-started'
  | 'pending'
  | 'blocked'
  | 'recovery-conflict'

function eventCursorSequence(value: string): number | null {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) return null
  try {
    const padding = '='.repeat((4 - value.length % 4) % 4)
    const decoded = atob(value.replace(/-/g, '+').replace(/_/g, '/') + padding)
    const match = /^([1-9][0-9]*)\|event$/.exec(decoded)
    if (!match) return null
    const sequence = Number(match[1])
    return Number.isSafeInteger(sequence) && sequence > 0 ? sequence : null
  } catch {
    return null
  }
}

function validStoredCursor(value: string): boolean {
  return value === '' || (value.length <= 512 && eventCursorSequence(value) !== null)
}

function wireVersionKey(value: unknown): ThoughtVersionKey | null {
  if (!isRecord(value)) return null
  const key = {
    logicalClock: value.logical_clock,
    deviceId: value.device_id,
    opId: value.op_id,
  }
  return isValidThoughtVersionKey(key, true) ? key : null
}

function wireTarget(value: unknown, hostID: string): AnnotationTarget | null {
  if (!isRecord(value) || value.host_id !== hostID || !isRecord(value.version)) return null
  const version = value.version
  switch (value.kind) {
    case 'saved-content':
      return typeof version.content_revision === 'number'
        ? canonicalAnnotationTarget({ kind: 'saved-content', contentRevision: version.content_revision })
        : null
    case 'summary':
      return typeof version.source_hash === 'string'
        ? canonicalAnnotationTarget({ kind: 'summary', sourceHash: version.source_hash })
        : null
    case 'note':
      return typeof version.note_revision === 'number'
        ? canonicalAnnotationTarget({ kind: 'note', noteRevision: version.note_revision })
        : null
    case 'legacy-stale':
      return typeof version.source_key === 'string'
        ? canonicalAnnotationTarget({ kind: 'legacy-stale', sourceKey: version.source_key })
        : null
    default:
      return null
  }
}

function isWireQuote(value: unknown): value is Record<string, unknown> {
  if (!isRecord(value) || typeof value.exact !== 'string' ||
    (value.start !== undefined && !isSafeNonNegativeInteger(value.start)) ||
    (value.end !== undefined && !isSafeNonNegativeInteger(value.end)) ||
    (value.start !== undefined && value.end !== undefined && value.end < value.start)) return false
  return (value.prefix === undefined || typeof value.prefix === 'string') &&
    (value.suffix === undefined || typeof value.suffix === 'string') &&
    (value.block_key === undefined || typeof value.block_key === 'string')
}

function wireOperation(
  value: ReaderThoughtSupersessionOperationResponse,
  annotationID: string,
): ThoughtSupersessionOperation | null {
  if (!isRecord(value)) return null
  const raw = value
  const target = wireTarget(raw.target, raw.host_id as string)
  const targetKey = target ? annotationTargetKey(target) : null
  const payload = isRecord(raw.payload) ? raw.payload : null
  const hasRecoveryOf = raw.recovery_of !== undefined
  const hasExpectedWinner = raw.expected_current_winner_key !== undefined
  const recoveryOf = hasRecoveryOf ? wireVersionKey(raw.recovery_of) : undefined
  const expectedCurrentWinnerKey = hasExpectedWinner
    ? wireVersionKey(raw.expected_current_winner_key)
    : undefined
  if (
    !isSafeNonNegativeInteger(raw.sequence) || raw.sequence <= 0 ||
    !isValidThoughtIdentifier(raw.op_id) || !isValidThoughtIdentifier(raw.device_id) ||
    !isSafeNonNegativeInteger(raw.logical_clock) || raw.logical_clock > MAX_THOUGHT_LOGICAL_CLOCK ||
    raw.annotation_id !== annotationID || !isValidThoughtIdentifier(annotationID) ||
    (raw.operation_kind !== 'add' && raw.operation_kind !== 'update' && raw.operation_kind !== 'delete') ||
    (raw.host_kind !== 'link' && raw.host_kind !== 'note' && raw.host_kind !== 'inbox') ||
    !isValidThoughtIdentifier(raw.host_id) || !target || !targetKey || !payload ||
    typeof payload.body !== 'string' ||
    (payload.source !== 'self' && payload.source !== 'ai' && payload.source !== 'user') ||
    (raw.operation_kind !== 'delete' && !isWireQuote(payload.quote)) ||
    (raw.operation_kind === 'delete' && payload.quote !== undefined && !isWireQuote(payload.quote)) ||
    typeof raw.created_at !== 'string' || raw.created_at.length === 0 ||
    hasRecoveryOf !== hasExpectedWinner ||
    (hasRecoveryOf && (!recoveryOf || !expectedCurrentWinnerKey))
  ) return null

  return {
    sequence: raw.sequence,
    opId: raw.op_id,
    deviceId: raw.device_id,
    logicalClock: raw.logical_clock,
    operationKind: raw.operation_kind,
    annotationId: annotationID,
    hostKind: raw.host_kind,
    hostId: raw.host_id,
    target,
    targetKey,
    body: payload.body,
    source: payload.source,
    quote: isWireQuote(payload.quote) ? { ...payload.quote } : null,
    createdAt: raw.created_at,
    ...(recoveryOf ? { recoveryOf } : {}),
    ...(expectedCurrentWinnerKey ? { expectedCurrentWinnerKey } : {}),
  }
}

function wireEvent(value: ReaderThoughtSupersessionEventResponse): ThoughtSupersessionEventRecord | null {
  if (!isRecord(value)) return null
  const raw = value
  if (!isSafeNonNegativeInteger(raw.sequence) || raw.sequence <= 0 ||
    !isValidThoughtIdentifier(raw.annotation_id)) return null
  const loser = wireOperation(raw.loser as ReaderThoughtSupersessionOperationResponse, raw.annotation_id)
  const winnerAtDetection = wireOperation(
    raw.winner_at_detection as ReaderThoughtSupersessionOperationResponse,
    raw.annotation_id,
  )
  if (!loser || !winnerAtDetection) return null
  if (compareThoughtVersionKeys(winnerAtDetection, loser) <= 0) return null
  return {
    key: ['', raw.sequence],
    namespace: '',
    eventSequence: raw.sequence,
    annotationId: raw.annotation_id,
    loser,
    winnerAtDetection,
  }
}

function sameEvent(
  left: ThoughtSupersessionEventRecord,
  right: ThoughtSupersessionEventRecord,
): boolean {
  return left.namespace === right.namespace && left.eventSequence === right.eventSequence &&
    JSON.stringify(left) === JSON.stringify(right)
}

function abort(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // A failed IDB request can already have aborted the transaction.
  }
}

async function readState(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<ThoughtSupersessionSyncStateRecord | undefined>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'read thought supersession cursor',
    [THOUGHT_SUPERSESSION_STATE_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      const request = transaction.objectStore(THOUGHT_SUPERSESSION_STATE_STORE)
        .get(namespace) as IDBRequest<unknown>
      request.onerror = () => abort(transaction)
      request.onsuccess = () => {
        if (!lease.isCurrent(identity)) {
          abort(transaction)
          return
        }
        const stored = request.result
        if (stored !== undefined) {
          if (!isValidThoughtSupersessionSyncStateRecord(stored, namespace) ||
            !validStoredCursor(stored.cursor)) {
            abort(transaction)
            return
          }
        }
        setResult(stored as ThoughtSupersessionSyncStateRecord | undefined)
      }
    },
  )
}

async function writePage(
  lease: IdentityLease,
  after: string,
  nextCursor: string,
  input: readonly ThoughtSupersessionEventRecord[],
): Promise<UserDataTransactionResult<{ readonly stored: number; readonly cursor: string }>> {
  const namespace = lease.context.physicalNamespace
  const events = input.map((event) => ({
    ...event,
    key: [namespace, event.eventSequence] as const,
    namespace,
  }))
  if (new Set(events.map((event) => event.eventSequence)).size !== events.length ||
    !events.every((event) => isValidThoughtSupersessionEventRecord(event, namespace)) ||
    !validStoredCursor(after) || eventCursorSequence(nextCursor) === null) {
    return { ok: false }
  }
  return runUserDataTransaction(
    lease,
    'store thought supersession page',
    [THOUGHT_SUPERSESSION_EVENTS_STORE, THOUGHT_SUPERSESSION_STATE_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      const eventsStore = transaction.objectStore(THOUGHT_SUPERSESSION_EVENTS_STORE)
      const stateStore = transaction.objectStore(THOUGHT_SUPERSESSION_STATE_STORE)
      const stateRequest = stateStore.get(namespace) as IDBRequest<unknown>
      const existingRequests = events.map((event) => eventsStore.get(
        [namespace, event.eventSequence],
      ) as IDBRequest<unknown>)
      let completedReads = 0
      const finishReads = () => {
        if (completedReads !== existingRequests.length + 1) return
        try {
          if (!lease.isCurrent(identity)) {
            abort(transaction)
            return
          }
          const current = stateRequest.result
          if (current !== undefined && (!isValidThoughtSupersessionSyncStateRecord(current, namespace) ||
            !validStoredCursor(current.cursor))) {
            abort(transaction)
            return
          }
          if ((current?.cursor ?? '') !== after) {
            abort(transaction)
            return
          }
          const newEvents: ThoughtSupersessionEventRecord[] = []
          for (let index = 0; index < events.length; index += 1) {
            const existing = existingRequests[index].result
            if (existing === undefined) {
              newEvents.push(events[index])
              continue
            }
            if (!isValidThoughtSupersessionEventRecord(existing, namespace) ||
              !sameEvent(existing, events[index])) {
              abort(transaction)
              return
            }
          }
          const state: ThoughtSupersessionSyncStateRecord = {
            namespace,
            cursor: nextCursor,
            updatedAt: Date.now(),
          }
          const writes: IDBRequest[] = [
            ...newEvents.map((event) => eventsStore.add(event)),
            stateStore.put(state),
          ]
          let completedWrites = 0
          const finishedWrites = () => {
            completedWrites += 1
            if (completedWrites === writes.length) {
              setResult({ stored: newEvents.length, cursor: nextCursor })
            }
          }
          for (const request of writes) {
            request.onerror = () => abort(transaction)
            request.onsuccess = finishedWrites
          }
        } catch {
          abort(transaction)
        }
      }
      stateRequest.onerror = () => abort(transaction)
      stateRequest.onsuccess = () => {
        completedReads += 1
        finishReads()
      }
      for (const request of existingRequests) {
        request.onerror = () => abort(transaction)
        request.onsuccess = () => {
          completedReads += 1
          finishReads()
        }
      }
    },
  )
}

async function performSync(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<ThoughtSupersessionSyncResult> {
  const initial = await readState(lease)
  if (!initial.ok) return { status: 'stale', pulled: 0, cursor: '' }
  let cursor = initial.value?.cursor ?? ''
  let pulled = 0

  for (let page = 0; page < MAX_PULL_PAGES; page += 1) {
    const operation = lease.capture(`pull thought supersession page ${page}`)
    if (!lease.isCurrent(operation)) return { status: 'stale', pulled, cursor }
    let response: Awaited<ReturnType<IdentityBoundReaderClient['listThoughtSupersessions']>>
    try {
      response = await client.listThoughtSupersessions(
        { after: cursor, limit: MAX_BATCH },
        { signal: operation.signal },
      )
    } catch (error) {
      return lease.isCurrent(operation)
        ? { status: 'failed', pulled, cursor, error: error instanceof Error ? error.message : '无法读取被取代版本' }
        : { status: 'stale', pulled, cursor }
    }
    if (!lease.isCurrent(operation)) return { status: 'stale', pulled, cursor }
    if (!response.ok) {
      return response.error.kind === 'identity-mismatch'
        ? { status: 'stale', pulled, cursor }
        : { status: 'failed', pulled, cursor, error: response.error.message }
    }

    if (!isReaderThoughtSupersessionEventsResponse(response.data)) {
      return { status: 'failed', pulled, cursor, error: '被取代版本分页格式不符' }
    }
    const afterSequence = cursor === '' ? 0 : eventCursorSequence(cursor)
    if (afterSequence === null) {
      return { status: 'failed', pulled, cursor, error: '被取代版本游标格式不符' }
    }

    const events = response.data.items.map(wireEvent)
    if (events.some((event) => event === null)) {
      return { status: 'failed', pulled, cursor, error: '被取代版本分页格式不符' }
    }
    const pageEvents = events as ThoughtSupersessionEventRecord[]
    if (pageEvents.some((event, index) => index > 0 &&
      event.eventSequence <= pageEvents[index - 1].eventSequence)) {
      return { status: 'failed', pulled, cursor, error: '被取代版本事件顺序不符' }
    }
    if (pageEvents.some((event) => event.eventSequence <= afterSequence)) {
      return { status: 'failed', pulled, cursor, error: '被取代版本事件游标倒退' }
    }
    if (pageEvents.length === 0) {
      return response.data.next_cursor === undefined
        ? { status: 'synced', pulled, cursor }
        : { status: 'failed', pulled, cursor, error: '空的被取代版本分页不得推进游标' }
    }
    const nextCursor = response.data.next_cursor
    if (typeof nextCursor !== 'string' || nextCursor.length === 0 || nextCursor === cursor ||
      eventCursorSequence(nextCursor) !== pageEvents[pageEvents.length - 1].eventSequence) {
      return { status: 'failed', pulled, cursor, error: '被取代版本游标未前进' }
    }
    const stored = await writePage(lease, cursor, nextCursor, pageEvents)
    if (!stored.ok) {
      return lease.isCurrent(lease.capture('thought supersession page write'))
        ? { status: 'failed', pulled, cursor, error: '无法保存被取代版本' }
        : { status: 'stale', pulled, cursor }
    }
    pulled += stored.value.stored
    cursor = stored.value.cursor
  }
  return { status: 'failed', pulled, cursor, error: '被取代版本分页超过上限' }
}

/** Pull the immutable supersession stream using its own identity-scoped cursor. */
export function syncThoughtSupersessions(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<ThoughtSupersessionSyncResult> {
  const current = inFlight.get(lease)
  if (current) return current
  const result = performSync(lease, client).finally(() => {
    if (inFlight.get(lease) === result) inFlight.delete(lease)
  })
  inFlight.set(lease, result)
  return result
}

/** Read one identity's locally persisted immutable supersession events. */
export async function listThoughtSupersessionEvents(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<readonly ThoughtSupersessionEventRecord[]>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'list thought supersession events',
    [THOUGHT_SUPERSESSION_EVENTS_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      const range = IDBKeyRange.bound([namespace, 0], [namespace, Number.MAX_SAFE_INTEGER])
      const request = transaction.objectStore(THOUGHT_SUPERSESSION_EVENTS_STORE)
        .getAll(range) as IDBRequest<unknown[]>
      request.onerror = () => abort(transaction)
      request.onsuccess = () => {
        if (!lease.isCurrent(identity) ||
          !request.result.every((event) => isValidThoughtSupersessionEventRecord(event, namespace))) {
          abort(transaction)
          return
        }
        setResult([...request.result as ThoughtSupersessionEventRecord[]]
          .sort((left, right) => left.eventSequence - right.eventSequence))
      }
    },
  )
}

function hasCompleteQuote(record: ThoughtMaterializedRecord): boolean {
  const quote = record.quote
  return isRecord(quote) && typeof quote.exact === 'string' &&
    isSafeNonNegativeInteger(quote.start) && isSafeNonNegativeInteger(quote.end) &&
    quote.end >= quote.start
}

/** Resolve current local winner availability without exposing payloads to callers. */
export async function readThoughtSupersessionRecoveryAvailability(
  lease: IdentityLease,
  events: readonly ThoughtSupersessionEventRecord[],
): Promise<UserDataTransactionResult<ReadonlyMap<number, ThoughtSupersessionRecoveryAvailability>>> {
  const namespace = lease.context.physicalNamespace
  if (!events.every((event) => isValidThoughtSupersessionEventRecord(event, namespace))) {
    return { ok: false }
  }
  return runUserDataTransaction(
    lease,
    'read thought supersession recovery availability',
    [THOUGHT_MATERIALIZED_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      const store = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
      const requests = events.map((event) => store.get(
        [namespace, event.annotationId],
      ) as IDBRequest<unknown>)
      let completed = 0
      const finish = () => {
        if (completed !== requests.length) return
        if (!lease.isCurrent(identity)) {
          abort(transaction)
          return
        }
        const availability = new Map<number, ThoughtSupersessionRecoveryAvailability>()
        for (let index = 0; index < events.length; index += 1) {
          const winner = requests[index].result
          if (!isValidThoughtMaterializedRecord(winner, namespace) ||
            winner.annotationId !== events[index].annotationId) {
            availability.set(events[index].eventSequence, 'missing-current-winner')
            continue
          }
          if ((winner.lifecycleStatus ?? 'active') !== 'active') {
            availability.set(events[index].eventSequence, 'host-tombstoned')
            continue
          }
          if (events[index].loser.operationKind === 'delete') {
            availability.set(events[index].eventSequence, 'ready')
            continue
          }
          if (winner.deleted) {
            availability.set(events[index].eventSequence, 'current-winner-deleted')
            continue
          }
          availability.set(
            events[index].eventSequence,
            hasCompleteQuote(winner) ? 'ready' : 'target-or-quote-incomplete',
          )
        }
        setResult(availability)
      }
      for (const request of requests) {
        request.onerror = () => abort(transaction)
        request.onsuccess = () => {
          completed += 1
          finish()
        }
      }
      if (requests.length === 0) setResult(new Map())
    },
  )
}

/**
 * Read recovery state by the deterministic recovery operation id.  The result
 * deliberately contains no candidate body or error text, so the superseded
 * view can explain a CAS race without widening local payload exposure.
 */
export async function readThoughtSupersessionRecoveryStates(
  lease: IdentityLease,
  events: readonly ThoughtSupersessionEventRecord[],
): Promise<UserDataTransactionResult<ReadonlyMap<number, ThoughtSupersessionRecoveryState>>> {
  const namespace = lease.context.physicalNamespace
  if (!events.every((event) => isValidThoughtSupersessionEventRecord(event, namespace))) {
    return { ok: false }
  }
  return runUserDataTransaction(
    lease,
    'read thought supersession recovery states',
    [THOUGHT_OUTBOX_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      const byOperationID = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_OP_ID_INDEX)
      const requests = events.map((event) => byOperationID.get([
        namespace,
        thoughtSupersessionRecoveryOperationID(event.eventSequence),
      ]) as IDBRequest<unknown>)
      let completed = 0
      const finish = () => {
        if (completed !== requests.length) return
        if (!lease.isCurrent(identity)) {
          abort(transaction)
          return
        }
        const states = new Map<number, ThoughtSupersessionRecoveryState>()
        for (let index = 0; index < events.length; index += 1) {
          const record = requests[index].result
          if (record === undefined) {
            states.set(events[index].eventSequence, 'not-started')
            continue
          }
          if (!isValidThoughtOutboxRecord(record, namespace) ||
            record.opId !== thoughtSupersessionRecoveryOperationID(events[index].eventSequence) ||
            record.recoveryOf === undefined || record.expectedCurrentWinnerKey === undefined) {
            abort(transaction)
            return
          }
          if (record.recoveryConflict === true ||
            (record.status === 'blocked' && record.blockedReason === 'thought_recovery_conflict')) {
            states.set(events[index].eventSequence, 'recovery-conflict')
          } else if (record.status === 'blocked') {
            states.set(events[index].eventSequence, 'blocked')
          } else {
            states.set(events[index].eventSequence, 'pending')
          }
        }
        setResult(states)
      }
      for (const request of requests) {
        request.onerror = () => abort(transaction)
        request.onsuccess = () => {
          completed += 1
          finish()
        }
      }
      if (requests.length === 0) setResult(new Map())
    },
  )
}
