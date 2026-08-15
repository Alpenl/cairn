import type { IdentityLease } from '../identity'
import { isSafeNonNegativeInteger } from './annotation-codec'
import { allocateThoughtClocks } from './annotation-store'
import {
  THOUGHT_HISTORY_OUTBOX_OP_ID_INDEX,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_REPAIR_READY_STORE,
  THOUGHT_REPAIR_SOURCE_STORE,
  THOUGHT_SYNC_STATE_STORE,
  runUserDataTransaction,
  type UserDataTransactionResult,
} from './idb'
import {
  THOUGHT_CONTRACT_VERSION,
  canonicalThoughtTarget,
  isValidThoughtHistoryOutboxRecord,
  isValidThoughtIdentifier,
  isValidThoughtMaterializedRecord,
  isValidThoughtVersionKey,
  thoughtTargetKey,
  type ThoughtFrozenSnapshot,
  type ThoughtHistoryOutboxRecord,
  type ThoughtMaterializedRecord,
  type ThoughtTarget,
} from './thought-types'

export interface ThoughtHistorySnapshotInput {
  readonly id: string
  readonly hostKind: string
  readonly hostId: string
  readonly target: unknown
  readonly quote: unknown
  readonly body: string
  readonly source: string
  readonly lastSequence: number
  /** Server winner observed with the immutable lifecycle snapshot. */
  readonly winnerKey: unknown
  readonly originalHostSnapshot?: unknown
}

export type ThoughtHistoryActionInput =
  | {
      readonly action: 'delete'
      readonly opId: string
      readonly thought: ThoughtHistorySnapshotInput
    }
  | {
      readonly action: 'reattach'
      readonly opId: string
      readonly thought: ThoughtHistorySnapshotInput
      readonly targetHostKind: 'link' | 'note' | 'inbox'
      readonly targetHostId: string
      readonly expectedHostRevision: number
    }

export interface ThoughtHistoryActionCommitResult {
  readonly status: 'committed' | 'duplicate' | 'op-id-conflict'
  readonly opId: string
}

function frozenTargetFromMaterialized(record: ThoughtMaterializedRecord): Record<string, unknown> {
  const version = record.target.kind === 'saved-content'
    ? { content_revision: record.target.contentRevision }
    : record.target.kind === 'summary'
      ? { source_hash: record.target.sourceHash }
      : record.target.kind === 'note'
        ? { note_revision: record.target.noteRevision }
        : record.target.kind === 'inbox'
          ? { metadata_revision: record.target.metadataRevision }
          : { source_key: record.target.sourceKey }
  return { kind: record.target.kind, host_id: record.hostId, version }
}

/**
 * Reads action authority without widening the display projection. Live
 * aggregate rows intentionally omit winner keys; only this identity-scoped
 * action boundary may recover the immutable server snapshot used by a delete.
 */
export async function readThoughtActionSnapshot(
  lease: IdentityLease,
  thoughtID: string,
): Promise<UserDataTransactionResult<ThoughtHistorySnapshotInput | null>> {
  if (!isValidThoughtIdentifier(thoughtID, 256)) return { ok: false }
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    `read thought action snapshot ${thoughtID}`,
    [THOUGHT_MATERIALIZED_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      const request = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .get([namespace, thoughtID]) as IDBRequest<unknown>
      request.onerror = () => transaction.abort()
      request.onsuccess = () => {
        if (!lease.isCurrent(identity)) {
          transaction.abort()
          return
        }
        if (request.result === undefined) {
          setResult(null)
          return
        }
        if (!isValidThoughtMaterializedRecord(request.result, namespace)) {
          transaction.abort()
          return
        }
        const record = request.result
        if (record.deleted) {
          setResult(null)
          return
        }
        setResult({
          id: record.annotationId,
          hostKind: record.hostKind,
          hostId: record.hostId,
          target: frozenTargetFromMaterialized(record),
          quote: record.quote,
          body: record.body,
          source: record.source,
          lastSequence: record.serverSequence,
          winnerKey: record.winnerKey,
        })
      }
    },
  )
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function cloneJSON<T>(value: T): T | null {
  try {
    return JSON.parse(JSON.stringify(value)) as T
  } catch {
    return null
  }
}

function positiveInteger(value: unknown): number | null {
  return isSafeNonNegativeInteger(value) && value > 0 ? value : null
}

function readField(record: Record<string, unknown>, camel: string, snake: string): unknown {
  return record[camel] ?? record[snake]
}

function thoughtTargetFromWire(raw: unknown, hostId: string): ThoughtTarget | null {
  if (!isRecord(raw) || raw.host_id !== hostId) return null
  const version = isRecord(raw.version) ? raw.version : null
  if (!version || typeof raw.kind !== 'string') return null
  switch (raw.kind) {
    case 'saved-content': {
      const contentRevision = positiveInteger(readField(version, 'contentRevision', 'content_revision'))
      return contentRevision === null
        ? null
        : canonicalThoughtTarget({ kind: raw.kind, contentRevision })
    }
    case 'summary': {
      const sourceHash = readField(version, 'sourceHash', 'source_hash')
      return typeof sourceHash === 'string' && sourceHash.length > 0
        ? canonicalThoughtTarget({ kind: raw.kind, sourceHash })
        : null
    }
    case 'note': {
      const noteRevision = positiveInteger(readField(version, 'noteRevision', 'note_revision'))
      return noteRevision === null
        ? null
        : canonicalThoughtTarget({ kind: raw.kind, noteRevision })
    }
    case 'inbox': {
      const metadataRevision = positiveInteger(readField(version, 'metadataRevision', 'metadata_revision'))
      return metadataRevision === null
        ? null
        : canonicalThoughtTarget({ kind: raw.kind, metadataRevision })
    }
    case 'legacy-stale': {
      const sourceKey = readField(version, 'sourceKey', 'source_key')
      return typeof sourceKey === 'string' && sourceKey.length > 0
        ? canonicalThoughtTarget({ kind: raw.kind, sourceKey })
        : null
    }
    default:
      return null
  }
}

function targetForHost(
  hostKind: 'link' | 'note' | 'inbox',
  revision: number,
): ThoughtTarget | null {
  if (!Number.isSafeInteger(revision) || revision <= 0) return null
  switch (hostKind) {
    case 'link':
      return canonicalThoughtTarget({ kind: 'saved-content', contentRevision: revision })
    case 'note':
      return canonicalThoughtTarget({ kind: 'note', noteRevision: revision })
    case 'inbox':
      return canonicalThoughtTarget({ kind: 'inbox', metadataRevision: revision })
  }
}

function frozenSnapshot(input: ThoughtHistorySnapshotInput): ThoughtFrozenSnapshot | null {
  if (!isValidThoughtIdentifier(input.id, 256) || !input.hostKind || !input.hostId ||
    !isSafeNonNegativeInteger(input.lastSequence) || typeof input.body !== 'string' ||
    !input.source.trim()) {
    return null
  }
  const target = cloneJSON(input.target)
  const quote = input.quote === null || input.quote === undefined
    ? null
    : cloneJSON(input.quote)
  const winnerKey = cloneJSON(input.winnerKey)
  if (!isRecord(target) || (quote !== null && !isRecord(quote)) ||
    !isValidThoughtVersionKey(winnerKey, true)) return null
  const originalHostSnapshot = input.originalHostSnapshot === undefined
    ? undefined
    : cloneJSON(input.originalHostSnapshot)
  if (input.originalHostSnapshot !== undefined && originalHostSnapshot === null) return null
  return {
    body: input.body,
    quote,
    source: input.source,
    target,
    winnerKey,
    ...(originalHostSnapshot === undefined ? {} : { originalHostSnapshot }),
  }
}

function stableJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(',')}]`
  if (!isRecord(value)) return JSON.stringify(value)
  return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(value[key])}`).join(',')}}`
}

function sameAction(left: ThoughtHistoryOutboxRecord, right: ThoughtHistoryOutboxRecord): boolean {
  return left.namespace === right.namespace && left.opId === right.opId &&
    left.deviceId === right.deviceId && left.logicalClock === right.logicalClock &&
    left.action === right.action && left.annotationId === right.annotationId &&
    left.hostKind === right.hostKind && left.hostId === right.hostId &&
    left.targetKey === right.targetKey &&
    left.expectedLastSequence === right.expectedLastSequence &&
    left.expectedHostRevision === right.expectedHostRevision &&
    stableJSON(left.target) === stableJSON(right.target) &&
    stableJSON(left.snapshot) === stableJSON(right.snapshot)
}

function canonicalAction(input: ThoughtHistoryActionInput): Omit<
  ThoughtHistoryOutboxRecord,
  'key' | 'namespace' | 'deviceId' | 'contractVersion' | 'logicalClock' | 'createdAt' | 'attemptCount'
> | null {
  if (!isValidThoughtIdentifier(input.opId)) return null
  const snapshot = frozenSnapshot(input.thought)
  if (!snapshot) return null
  const sourceTarget = thoughtTargetFromWire(snapshot.target, input.thought.hostId)
  if (!sourceTarget) return null
  const target = input.action === 'delete'
    ? sourceTarget
    : targetForHost(input.targetHostKind, input.expectedHostRevision)
  const targetKey = target ? thoughtTargetKey(target) : null
  if (!target || !targetKey) return null
  if (input.action === 'delete') {
    return {
      action: input.action,
      opId: input.opId,
      annotationId: input.thought.id,
      hostKind: input.thought.hostKind,
      hostId: input.thought.hostId,
      target,
      targetKey,
      expectedLastSequence: input.thought.lastSequence,
      snapshot,
    }
  }
  if (!input.targetHostId.trim() || !Number.isSafeInteger(input.expectedHostRevision) ||
    input.expectedHostRevision <= 0 || snapshot.quote === null) return null
  return {
    action: input.action,
    opId: input.opId,
    annotationId: input.thought.id,
    hostKind: input.targetHostKind,
    hostId: input.targetHostId.trim(),
    target,
    targetKey,
    expectedLastSequence: input.thought.lastSequence,
    expectedHostRevision: input.expectedHostRevision,
    snapshot,
  }
}

/**
 * Persists a history delete or reattach before any request is made. The
 * candidate is removed only by sync acknowledgement, so revision/CAS errors
 * and offline retries retain the exact frozen evidence that formed the action.
 */
export async function commitThoughtHistoryAction(
  lease: IdentityLease,
  input: ThoughtHistoryActionInput,
): Promise<UserDataTransactionResult<ThoughtHistoryActionCommitResult>> {
  const action = canonicalAction(input)
  if (!action) return { ok: false }
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    `commit thought history ${action.opId}`,
    [
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_OUTBOX_STORE,
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_SOURCE_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      const store = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
      const existingRequest = store.index(THOUGHT_HISTORY_OUTBOX_OP_ID_INDEX).get(
        [namespace, action.opId],
      ) as IDBRequest<unknown>
      existingRequest.onerror = () => transaction.abort()
      existingRequest.onsuccess = () => {
        if (!lease.isCurrent(identity)) {
          transaction.abort()
          return
        }
        const existing = existingRequest.result
        if (existing !== undefined) {
          if (!isValidThoughtHistoryOutboxRecord(existing, namespace)) {
            transaction.abort()
            return
          }
          const candidate: ThoughtHistoryOutboxRecord = {
            key: [namespace, action.opId],
            namespace,
            ...action,
            deviceId: existing.deviceId,
            contractVersion: THOUGHT_CONTRACT_VERSION,
            logicalClock: existing.logicalClock,
            createdAt: existing.createdAt,
            attemptCount: existing.attemptCount,
          }
          setResult({
            status: sameAction(existing, candidate) ? 'duplicate' : 'op-id-conflict',
            opId: action.opId,
          })
          return
        }
        allocateThoughtClocks(
          transaction,
          lease,
          1,
          (deviceId, clocks) => {
            const logicalClock = clocks[0]
            if (!logicalClock || !lease.isCurrent(identity)) {
              transaction.abort()
              return
            }
            const record: ThoughtHistoryOutboxRecord = {
              key: [namespace, action.opId],
              namespace,
              ...action,
              deviceId,
              contractVersion: THOUGHT_CONTRACT_VERSION,
              logicalClock,
              createdAt: Date.now(),
              attemptCount: 0,
            }
            if (!isValidThoughtHistoryOutboxRecord(record, namespace)) {
              transaction.abort()
              return
            }
            const write = store.add(record)
            write.onerror = () => transaction.abort()
            write.onsuccess = () => setResult({ status: 'committed', opId: action.opId })
          },
          () => transaction.abort(),
          [action.snapshot.winnerKey.logicalClock],
        )
      }
    },
  )
}
