import type { Annotation } from '../annotations'
import { isRecord } from '../records'
import type {
  AnnotationTarget,
  AnnotationUpdatePatch,
} from './annotation-types'
import { isSafeNonNegativeInteger, cloneTargetAnnotation } from './annotation-codec'
import { annotationTargetKey, canonicalAnnotationTarget } from './annotation-types'

export const THOUGHT_CONTRACT_VERSION = 1 as const
export const MAX_THOUGHT_LOGICAL_CLOCK = Number.MAX_SAFE_INTEGER

export interface ThoughtVersionKey {
  readonly logicalClock: number
  readonly deviceId: string
  readonly opId: string
}

/** Inbox carries a metadata revision, not an annotation-rendering source. */
export interface InboxThoughtTarget {
  readonly kind: 'inbox'
  readonly metadataRevision: number
}

// Thought sync also transports historical Inbox thoughts. Keep that additive
// target separate from the interactive AnnotationTarget union: Inbox has no
// document-annotation editor to bind into the local annotation projection.
export type ThoughtTarget = AnnotationTarget | InboxThoughtTarget

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function isChecksum(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{64}$/.test(value)
}

export function canonicalThoughtTarget(value: unknown): ThoughtTarget | null {
  if (isRecord(value) && value.kind === 'inbox') {
    return isSafeNonNegativeInteger(value.metadataRevision) && value.metadataRevision > 0
      ? { kind: 'inbox', metadataRevision: value.metadataRevision }
      : null
  }
  if (!isRecord(value)) return null
  return canonicalAnnotationTarget(value as unknown as AnnotationTarget)
}

export function isAnnotationThoughtTarget(target: ThoughtTarget): target is AnnotationTarget {
  return target.kind !== 'inbox'
}

export function thoughtTargetKey(target: ThoughtTarget): string | null {
  const canonical = canonicalThoughtTarget(target)
  if (!canonical) return null
  return canonical.kind === 'inbox'
    ? `inbox:${canonical.metadataRevision}`
    : annotationTargetKey(canonical)
}

export function isValidThoughtIdentifier(value: unknown, maxBytes = 128): value is string {
  if (!isNonEmptyString(value) || value !== value.trim() || value.includes('\0')) return false
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return new TextEncoder().encode(value).length <= maxBytes
}

export function isValidThoughtVersionKey(
  value: unknown,
): value is ThoughtVersionKey {
  if (!isRecord(value)) return false
  const key = value as Partial<ThoughtVersionKey>
  return isSafeNonNegativeInteger(key.logicalClock) &&
    key.logicalClock <= MAX_THOUGHT_LOGICAL_CLOCK &&
    key.logicalClock > 0 &&
    isValidThoughtIdentifier(key.deviceId) &&
    isValidThoughtIdentifier(key.opId)
}

function compareUTF8(left: string, right: string): number {
  const encoder = new TextEncoder()
  const leftBytes = encoder.encode(left)
  const rightBytes = encoder.encode(right)
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index += 1) {
    if (leftBytes[index] !== rightBytes[index]) return leftBytes[index] < rightBytes[index] ? -1 : 1
  }
  if (leftBytes.length === rightBytes.length) return 0
  return leftBytes.length < rightBytes.length ? -1 : 1
}

/** Ascending Lamport order; the maximum key is the materialized winner. */
export function compareThoughtVersionKeys(left: ThoughtVersionKey, right: ThoughtVersionKey): number {
  if (left.logicalClock !== right.logicalClock) {
    return left.logicalClock < right.logicalClock ? -1 : 1
  }
  const device = compareUTF8(left.deviceId, right.deviceId)
  return device === 0 ? compareUTF8(left.opId, right.opId) : device
}

function isValidPatch(value: unknown): value is AnnotationUpdatePatch {
  if (!isRecord(value) || !isSafeNonNegativeInteger(value.updatedAt)) return false
  return (value.note === undefined || typeof value.note === 'string') &&
    (value.source === undefined || value.source === 'self' || value.source === 'ai' || value.source === 'user')
}

function isValidQuote(value: unknown): boolean {
  if (!isRecord(value) || typeof value.exact !== 'string' ||
    (value.start !== undefined && !isSafeNonNegativeInteger(value.start)) ||
    (value.end !== undefined && !isSafeNonNegativeInteger(value.end)) ||
    (value.start !== undefined && value.end !== undefined && value.end < value.start)) return false
  return (value.prefix === undefined || typeof value.prefix === 'string') &&
    (value.suffix === undefined || typeof value.suffix === 'string') &&
    (value.block_key === undefined || typeof value.block_key === 'string')
}

function isJSONValue(value: unknown, depth = 0): boolean {
  if (depth > 32) return false
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return true
  if (typeof value === 'number') return Number.isFinite(value)
  if (Array.isArray(value)) return value.every((item) => isJSONValue(item, depth + 1))
  if (!isRecord(value)) return false
  return Object.values(value).every((item) => isJSONValue(item, depth + 1))
}

export interface ThoughtFrozenSnapshot {
  readonly body: string
  readonly quote: Record<string, unknown> | null
  /** Server-provided provenance retained as evidence; its vocabulary is open. */
  readonly source: string
  readonly target: Record<string, unknown>
  /** Server winner observed when the user froze this historical candidate. */
  readonly winnerKey: ThoughtVersionKey
  readonly originalHostSnapshot?: unknown
}

function isValidFrozenSnapshot(value: unknown): value is ThoughtFrozenSnapshot {
  if (!isRecord(value) || typeof value.body !== 'string' ||
    !isNonEmptyString(value.source) || !value.source.trim() ||
    (value.quote !== null && !isValidQuote(value.quote)) || !isRecord(value.target) ||
    !isValidThoughtVersionKey(value.winnerKey)) return false
  return value.originalHostSnapshot === undefined || isJSONValue(value.originalHostSnapshot)
}

/** A browser-local operation waiting for the server thought log. */
export interface ThoughtOutboxRecord {
  readonly key: readonly [namespace: string, sequence: number]
  readonly namespace: string
  /** Browser-local annotation operation sequence; never a server cursor. */
  readonly sequence: number
  readonly opId: string
  readonly deviceId: string
  readonly contractVersion: typeof THOUGHT_CONTRACT_VERSION
  readonly logicalClock: number
  readonly operationKind: 'add' | 'update' | 'delete'
  readonly annotationId: string
  readonly hostKind: string
  readonly hostId: string
  readonly linkId: string
  readonly target: ThoughtTarget
  readonly targetKey: string
  readonly annotation: Annotation | null
  readonly patch?: AnnotationUpdatePatch
  readonly createdAt: number
  readonly attemptCount: number
  readonly nextAttemptAt?: number
  readonly lastError?: string
  /** SHA-256 of the canonical thought payload when available. */
  readonly checksum?: string
  readonly status?: 'pending' | 'blocked'
  readonly blockedReason?: string
  /** A recovery CAS saw a newer winner; the candidate remains durable. */
  readonly recoveryConflict?: boolean
  readonly recoveryOf?: ThoughtVersionKey
  readonly expectedCurrentWinnerKey?: ThoughtVersionKey
}

export function isThoughtOutboxDispatchable(
  record: ThoughtOutboxRecord,
): boolean {
  return record.status !== 'blocked'
}

/**
 * A history action has its own durable keyspace. Annotation operations use an
 * auto-incremented local sequence, so sharing their outbox key would permit a
 * later annotation write to overwrite a queued lifecycle action.
 */
export interface ThoughtHistoryOutboxRecord {
  readonly key: readonly [namespace: string, opId: string]
  readonly namespace: string
  readonly opId: string
  readonly deviceId: string
  readonly contractVersion: typeof THOUGHT_CONTRACT_VERSION
  readonly logicalClock: number
  readonly action: 'delete' | 'reattach'
  readonly annotationId: string
  readonly hostKind: string
  readonly hostId: string
  readonly target: ThoughtTarget
  readonly targetKey: string
  readonly expectedLastSequence: number
  readonly expectedHostRevision?: number
  /** Immutable candidate evidence retained until the server ACKs the action. */
  readonly snapshot: ThoughtFrozenSnapshot
  readonly createdAt: number
  readonly attemptCount: number
  readonly nextAttemptAt?: number
  readonly lastError?: string
  readonly status?: 'pending' | 'blocked'
  readonly blockedReason?: string
}

/** Per-identity sync cursor and retry state. */
export interface ThoughtSyncStateRecord {
  readonly namespace: string
  readonly cursor: string
  readonly deviceId: string
  readonly tabToken: string
  /** Highest durable local, pulled, or acknowledged Lamport clock. */
  readonly logicalClockFloor?: number
  readonly updatedAt: number
  readonly lastAckSequence?: number
  /** Retry state for the remote replay stream, separate from push retries. */
  readonly pullAttemptCount?: number
  readonly pullRetryAt?: number
  readonly pullLastError?: string
  /** Earliest retry time across the outbox and pull stream. */
  readonly retryAt?: number
  readonly lastError?: string
  /** Stable, payload-free classification of the most recent sync failure. */
  readonly lastErrorCode?: string
  /** Set only after a real push/pull round completes with no durable outbox rows. */
  readonly lastSuccessfulSyncAt?: number
  /** A bounded replay round persisted progress but still has later pages to fetch. */
  readonly pullInProgress?: boolean
  /** A cursor reset caused by server retention requires a full replay. */
  readonly resyncRequired?: boolean
  readonly lastServerSequence?: number
  /** Monotonic local record of the server retention cutoff, if supplied. */
  readonly retentionCutoff?: number
}

/** Server materialized thought, including tombstones and its replay sequence. */
export interface ThoughtMaterializedRecord {
  readonly key: readonly [namespace: string, annotationId: string]
  readonly namespace: string
  readonly annotationId: string
  readonly contractVersion: typeof THOUGHT_CONTRACT_VERSION
  readonly winnerKey: ThoughtVersionKey
  readonly hostKind: string
  readonly hostId: string
  readonly linkId: string | null
  readonly target: ThoughtTarget
  readonly targetKey: string
  readonly quote: Record<string, unknown> | null
  readonly body: string
  readonly source: 'self' | 'ai' | 'user'
  readonly deleted: boolean
  /** Host lifecycle is independent from a thought's own delete operation. */
  readonly lifecycleStatus?: 'active' | 'tombstone'
  readonly serverSequence: number
  readonly createdAt: string
  readonly updatedAt: string
  readonly checksum?: string
  /** Server-provided tombstone retention boundary, in epoch milliseconds. */
  readonly retainedUntil?: number
  /** The local operation that most recently won the local optimistic merge. */
  readonly localOpId?: string
}

/** One immutable operation snapshot inside the supersession event stream. */
export interface ThoughtSupersessionOperation {
  readonly sequence: number
  readonly opId: string
  readonly deviceId: string
  readonly logicalClock: number
  readonly operationKind: 'add' | 'update' | 'delete'
  readonly annotationId: string
  readonly hostKind: 'link' | 'note' | 'inbox'
  readonly hostId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly body: string
  readonly source: 'self' | 'ai' | 'user'
  readonly quote: Record<string, unknown> | null
  readonly createdAt: string
  readonly recoveryOf?: ThoughtVersionKey
  readonly expectedCurrentWinnerKey?: ThoughtVersionKey
}

/** A server supersession event, keyed by its own event sequence. */
export interface ThoughtSupersessionEventRecord {
  readonly key: readonly [namespace: string, eventSequence: number]
  readonly namespace: string
  readonly eventSequence: number
  readonly annotationId: string
  readonly loser: ThoughtSupersessionOperation
  readonly winnerAtDetection: ThoughtSupersessionOperation
}

/** Separate per-identity cursor for immutable supersession events. */
export interface ThoughtSupersessionSyncStateRecord {
  readonly namespace: string
  readonly cursor: string
  readonly updatedAt: number
}

/** Stable idempotency key for the one permitted recovery intent per event. */
export function thoughtSupersessionRecoveryOperationID(eventSequence: number): string {
  return `supersession-recovery:${eventSequence}`
}

function isValidThoughtSupersessionOperation(value: unknown, annotationId: string): value is ThoughtSupersessionOperation {
  if (!isRecord(value)) return false
  const operation = value as Partial<ThoughtSupersessionOperation>
  const target = isRecord(operation.target)
    ? canonicalAnnotationTarget(operation.target as AnnotationTarget)
    : null
  const targetKey = target ? annotationTargetKey(target) : null
  const hasRecoveryOf = operation.recoveryOf !== undefined
  const hasExpectedWinner = operation.expectedCurrentWinnerKey !== undefined
  const quote = operation.quote
  return isSafeNonNegativeInteger(operation.sequence) && operation.sequence > 0 &&
    isValidThoughtIdentifier(operation.opId) && isValidThoughtIdentifier(operation.deviceId) &&
    isSafeNonNegativeInteger(operation.logicalClock) &&
    operation.logicalClock <= MAX_THOUGHT_LOGICAL_CLOCK &&
    (operation.operationKind === 'add' || operation.operationKind === 'update' || operation.operationKind === 'delete') &&
    operation.annotationId === annotationId && isValidThoughtIdentifier(operation.annotationId) &&
    (operation.hostKind === 'link' || operation.hostKind === 'note' || operation.hostKind === 'inbox') &&
    isValidThoughtIdentifier(operation.hostId) && target !== null && targetKey !== null &&
    operation.targetKey === targetKey && typeof operation.body === 'string' &&
    (operation.source === 'self' || operation.source === 'ai' || operation.source === 'user') &&
    (operation.operationKind === 'delete'
      ? (quote === null || isRecord(quote))
      : isRecord(quote)) &&
    isNonEmptyString(operation.createdAt) &&
    hasRecoveryOf === hasExpectedWinner &&
    (!hasRecoveryOf || (isValidThoughtVersionKey(operation.recoveryOf) &&
      isValidThoughtVersionKey(operation.expectedCurrentWinnerKey)))
}

export function isValidThoughtSupersessionEventRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtSupersessionEventRecord {
  if (!isRecord(value)) return false
  const event = value as Partial<ThoughtSupersessionEventRecord>
  return Array.isArray(event.key) && event.key.length === 2 &&
    isNonEmptyString(event.namespace) &&
    (expectedNamespace === undefined || event.namespace === expectedNamespace) &&
    event.key[0] === event.namespace && isSafeNonNegativeInteger(event.eventSequence) &&
    event.eventSequence > 0 && event.key[1] === event.eventSequence &&
    isValidThoughtIdentifier(event.annotationId) &&
    isValidThoughtSupersessionOperation(event.loser, event.annotationId) &&
    isValidThoughtSupersessionOperation(event.winnerAtDetection, event.annotationId) &&
    compareThoughtVersionKeys(event.winnerAtDetection, event.loser) > 0
}

export function isValidThoughtSupersessionSyncStateRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtSupersessionSyncStateRecord {
  if (!isRecord(value)) return false
  const state = value as Partial<ThoughtSupersessionSyncStateRecord>
  return isNonEmptyString(state.namespace) &&
    (expectedNamespace === undefined || state.namespace === expectedNamespace) &&
    typeof state.cursor === 'string' && isSafeNonNegativeInteger(state.updatedAt)
}

export const THOUGHT_CONFLICT_KIND = 'thought-conflict' as const

/** A durable copy of a server version that lost a deterministic merge. */
export interface ThoughtConflictRecord {
  readonly key: readonly [
    kind: typeof THOUGHT_CONFLICT_KIND,
    namespace: string,
    annotationId: string,
    serverSequence: number,
    loserFingerprint: string,
  ]
  readonly kind: typeof THOUGHT_CONFLICT_KIND
  readonly namespace: string
  readonly annotationId: string
  readonly serverSequence: number
  readonly winner: ThoughtMaterializedRecord
  readonly loser: ThoughtMaterializedRecord
  readonly winnerFingerprint: string
  readonly loserFingerprint: string
  readonly reason: 'older-server-version' | 'server-sequence-tie' | 'local-optimistic'
  readonly detectedAt: number
  readonly retainedUntil?: number
}

export function isValidThoughtOutboxRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtOutboxRecord {
  if (!isRecord(value)) return false
  const record = value as Partial<ThoughtOutboxRecord>
  const target = canonicalThoughtTarget(record.target)
  const targetKey = target ? thoughtTargetKey(target) : null
  const status = record.status ?? 'pending'
  const hasRecoveryOf = record.recoveryOf !== undefined
  const hasExpectedWinner = record.expectedCurrentWinnerKey !== undefined
  return Array.isArray(record.key) && record.key.length === 2 &&
    isNonEmptyString(record.namespace) &&
    (expectedNamespace === undefined || record.namespace === expectedNamespace) &&
    isSafeNonNegativeInteger(record.sequence) && record.sequence > 0 &&
    record.key[0] === record.namespace && record.key[1] === record.sequence &&
    record.contractVersion === THOUGHT_CONTRACT_VERSION &&
    isValidThoughtVersionKey({
      logicalClock: record.logicalClock,
      deviceId: record.deviceId,
      opId: record.opId,
    }) &&
    (record.operationKind === 'add' || record.operationKind === 'update' || record.operationKind === 'delete') &&
    isNonEmptyString(record.annotationId) && isNonEmptyString(record.hostKind) &&
    isNonEmptyString(record.hostId) && isNonEmptyString(record.linkId) &&
    target !== null && targetKey !== null && record.targetKey === targetKey &&
    (record.annotation === null ||
      (isAnnotationThoughtTarget(target) && cloneTargetAnnotation(record.annotation, target) !== null)) &&
    isSafeNonNegativeInteger(record.attemptCount) &&
    isSafeNonNegativeInteger(record.createdAt) &&
    (record.nextAttemptAt === undefined || isSafeNonNegativeInteger(record.nextAttemptAt)) &&
    (record.lastError === undefined || typeof record.lastError === 'string') &&
    (record.checksum === undefined || isChecksum(record.checksum)) &&
    (record.patch === undefined || isValidPatch(record.patch)) &&
    (record.operationKind === 'delete' || record.annotation !== null || record.patch !== undefined) &&
    (status === 'pending' || status === 'blocked') &&
    (record.blockedReason === undefined || typeof record.blockedReason === 'string') &&
    (record.recoveryConflict === undefined || typeof record.recoveryConflict === 'boolean') &&
    hasRecoveryOf === hasExpectedWinner &&
    (!hasRecoveryOf || (isValidThoughtVersionKey(record.recoveryOf) &&
      isValidThoughtVersionKey(record.expectedCurrentWinnerKey)))
}

function hasValidThoughtHistoryOutboxShape(
  value: unknown,
  expectedNamespace?: string,
): boolean {
  if (!isRecord(value)) return false
  const record = value as Partial<ThoughtHistoryOutboxRecord>
  const target = canonicalThoughtTarget(record.target)
  const targetKey = target ? thoughtTargetKey(target) : null
  const status = record.status ?? 'pending'
  const snapshot = record.snapshot
  const snapshotRecord = isRecord(snapshot) ? snapshot : null
  const validVersion = isValidThoughtVersionKey({
    logicalClock: record.logicalClock,
    deviceId: record.deviceId,
    opId: record.opId,
  })
  const reattach = record.action === 'reattach'
  return Array.isArray(record.key) && record.key.length === 2 &&
    isNonEmptyString(record.namespace) &&
    (expectedNamespace === undefined || record.namespace === expectedNamespace) &&
    record.key[0] === record.namespace && record.key[1] === record.opId &&
    isValidThoughtIdentifier(record.opId) && validVersion &&
    record.contractVersion === THOUGHT_CONTRACT_VERSION &&
    (record.action === 'delete' || reattach) &&
    isValidThoughtIdentifier(record.annotationId, 256) &&
    isNonEmptyString(record.hostKind) && isNonEmptyString(record.hostId) &&
    target !== null && targetKey !== null && record.targetKey === targetKey &&
    isSafeNonNegativeInteger(record.expectedLastSequence) &&
    (reattach
      ? isSafeNonNegativeInteger(record.expectedHostRevision) && record.expectedHostRevision > 0 &&
        snapshotRecord !== null && snapshotRecord.quote !== null
      : record.expectedHostRevision === undefined) &&
    isValidFrozenSnapshot(snapshot) &&
    isSafeNonNegativeInteger(record.createdAt) &&
    isSafeNonNegativeInteger(record.attemptCount) &&
    (record.nextAttemptAt === undefined || isSafeNonNegativeInteger(record.nextAttemptAt)) &&
    (record.lastError === undefined || typeof record.lastError === 'string') &&
    (status === 'pending' || status === 'blocked') &&
    (record.blockedReason === undefined || isNonEmptyString(record.blockedReason)) &&
    (status !== 'blocked' || isNonEmptyString(record.blockedReason))
}

export function isValidThoughtHistoryOutboxRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtHistoryOutboxRecord {
  return hasValidThoughtHistoryOutboxShape(value, expectedNamespace)
}

export function isValidThoughtMaterializedRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtMaterializedRecord {
  if (!isRecord(value)) return false
  const record = value as Partial<ThoughtMaterializedRecord>
  const target = canonicalThoughtTarget(record.target)
  const targetKey = target ? thoughtTargetKey(target) : null
  return Array.isArray(record.key) && record.key.length === 2 &&
    isNonEmptyString(record.namespace) &&
    (expectedNamespace === undefined || record.namespace === expectedNamespace) &&
    record.key[0] === record.namespace && record.key[1] === record.annotationId &&
    record.contractVersion === THOUGHT_CONTRACT_VERSION &&
    isValidThoughtVersionKey(record.winnerKey) &&
    isNonEmptyString(record.annotationId) && isNonEmptyString(record.hostKind) &&
    isNonEmptyString(record.hostId) && (record.linkId === null || isNonEmptyString(record.linkId)) &&
    target !== null && targetKey !== null && record.targetKey === targetKey &&
    (record.quote === null || isValidQuote(record.quote)) &&
    typeof record.body === 'string' &&
    (record.source === 'self' || record.source === 'ai' || record.source === 'user') &&
    typeof record.deleted === 'boolean' && isSafeNonNegativeInteger(record.serverSequence) &&
    record.serverSequence > 0 && isNonEmptyString(record.createdAt) &&
    isNonEmptyString(record.updatedAt) &&
    (record.checksum === undefined || isChecksum(record.checksum)) &&
    (record.lifecycleStatus === undefined || record.lifecycleStatus === 'active' || record.lifecycleStatus === 'tombstone') &&
    (record.retainedUntil === undefined || isSafeNonNegativeInteger(record.retainedUntil))
}

export function isValidThoughtConflictRecord(
  value: unknown,
  expectedNamespace?: string,
): value is ThoughtConflictRecord {
  if (!isRecord(value)) return false
  const record = value as Partial<ThoughtConflictRecord>
  return Array.isArray(record.key) && record.key.length === 5 &&
    record.key[0] === THOUGHT_CONFLICT_KIND &&
    record.kind === THOUGHT_CONFLICT_KIND &&
    isNonEmptyString(record.namespace) &&
    (expectedNamespace === undefined || record.namespace === expectedNamespace) &&
    record.key[1] === record.namespace &&
    isNonEmptyString(record.annotationId) && record.key[2] === record.annotationId &&
    isSafeNonNegativeInteger(record.serverSequence) && record.serverSequence > 0 &&
    record.key[3] === record.serverSequence &&
    isNonEmptyString(record.winnerFingerprint) &&
    isNonEmptyString(record.loserFingerprint) &&
    record.key[4] === record.loserFingerprint &&
    (record.reason === 'older-server-version' ||
      record.reason === 'server-sequence-tie' ||
      record.reason === 'local-optimistic') &&
    isSafeNonNegativeInteger(record.detectedAt) &&
    (record.retainedUntil === undefined || isSafeNonNegativeInteger(record.retainedUntil)) &&
    isValidThoughtMaterializedRecord(record.winner, record.namespace) &&
    isValidThoughtMaterializedRecord(record.loser, record.namespace) &&
    record.winner.annotationId === record.annotationId &&
    record.loser.annotationId === record.annotationId
}
