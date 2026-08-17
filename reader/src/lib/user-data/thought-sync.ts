import type {
  ReaderThoughtAckResponse,
  ReaderThoughtOpRequest,
} from '../api/types'
import type { IdentityBoundReaderClient } from '../api/client'
import type { ApiError } from '../api/result'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../reader-events'
import { isRecord } from '../records'
import {
  cloneTargetAnnotation,
  decodeAnnotationWire,
  isSafeNonNegativeInteger,
} from './annotation-codec'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type AnnotationTarget,
} from './annotation-types'
import type { IdentityLease } from '../identity'
import type { Annotation } from '../annotations'
import {
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_IMPORTS_STORE,
  THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_HOST_INDEX,
  THOUGHT_MATERIALIZED_NAMESPACE_INDEX,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_REPAIR_ACK_STORE,
  THOUGHT_REPAIR_QUARANTINE_STORE,
  THOUGHT_REPAIR_READY_STORE,
  THOUGHT_REPAIR_SOURCE_STORE,
  THOUGHT_SYNC_STATE_STORE,
  runUserDataTransaction,
  type UserDataTransactionResult,
} from './idb'
import {
  maximumThoughtClock,
  randomThoughtToken,
  stableThoughtDeviceID,
} from './thought-clock'
import type {
  CompleteThoughtOutboxRecord,
  ThoughtConflictRecord,
  ThoughtHistoryOutboxRecord,
  ThoughtMaterializedRecord,
  ThoughtOutboxRecord,
  ThoughtSyncStateRecord,
  ThoughtTarget,
  ThoughtVersionKey,
} from './thought-types'
import {
  isThoughtRepairAck,
  repairReadyChecksum,
  type ThoughtRepairReadyRecord,
} from './thought-repair'
import {
  THOUGHT_CONTRACT_VERSION,
  THOUGHT_CONFLICT_KIND,
  canonicalThoughtTarget,
  hasCompleteThoughtWireBaseline,
  hydrateThoughtHistoryOutboxWinnerKey,
  isQuarantinedThoughtHistoryOutboxRecord,
  isThoughtOutboxDispatchable,
  isValidThoughtConflictRecord,
  isValidThoughtHistoryOutboxRecord,
  isValidThoughtMaterializedRecord,
  isValidThoughtOutboxRecord,
  isValidThoughtVersionKey,
  quarantineLegacyThoughtOutboxRecord,
  quarantineThoughtHistoryOutboxMissingWinnerKey,
  thoughtTargetKey,
} from './thought-types'
import { syncThoughtSupersessions } from './thought-supersession'

const MAX_BATCH = 100
/** Bound one foreground round without discarding the durable replay cursor. */
const MAX_PULL_PAGES_PER_ROUND = 20
const MAX_PUSH_BATCHES = 5
const BASE_RETRY_MS = 1000
const MAX_RETRY_MS = 5 * 60 * 1000
export const THOUGHT_SYNC_POLL_MS = 30_000
const THOUGHT_SYNC_CHANNEL_NAME = 'webtag-reader-thought-sync-v1'
const REPAIR_BROADCAST_SUPPRESSION_MS = 1_000
const HISTORY_SUPERSEDED_ERROR = '历史想法操作已被较新的服务端版本覆盖。冻结候选已保留，请刷新后重试。'

// Deduplicate calls for the same lease, while ensuring a newly installed
// identity can never inherit a promise owned by an older lease with the same
// namespace string (a useful property in tests and during rapid re-auth).
const inFlight = new WeakMap<IdentityLease, Promise<SyncExecution>>()
const TAB_TOKEN = randomThoughtToken('tab')

export interface ThoughtSyncResult {
  readonly status: 'synced' | 'idle' | 'failed' | 'stale'
  readonly pushed: number
  readonly pulled: number
  readonly cursor: string
  readonly pending: number
  /** Earliest durable retry time for a remaining outbox record. */
  readonly retryAt?: number
  /** Payload-free failure classification. Never expose a transport message here. */
  readonly errorCode?: string
}

/** Internal execution facts used by the namespace controller for one-way invalidation. */
interface SyncExecution {
  readonly result: ThoughtSyncResult
  /** False only when navigator.locks returned null for ifAvailable. */
  readonly ran: boolean
  /** Durable sync-owned state or materialized data changed during this execution. */
  readonly durableChanged: boolean
}

/** Ordered user-facing state for the current identity's durable Thought queue. */
export type ThoughtSyncPhase = 'offline' | 'syncing' | 'failed' | 'pending' | 'synced'

/**
 * Observable, payload-free view of the namespace-scoped Thought sync state.
 * Counts are operations, never a surrogate for visible Thought records.
 */
export interface ThoughtSyncSnapshot {
  readonly phase: ThoughtSyncPhase
  readonly pendingCount: number
  readonly blockedCount: number
  readonly retryAt?: number
  readonly lastSuccessfulSyncAt?: number
  readonly errorCode?: string
}

export const EMPTY_THOUGHT_SYNC_SNAPSHOT: ThoughtSyncSnapshot = Object.freeze({
  phase: 'syncing',
  pendingCount: 0,
  blockedCount: 0,
})

/** A revoked identity must never leave a consumer presenting stale work as active. */
const DISPOSED_THOUGHT_SYNC_SNAPSHOT: ThoughtSyncSnapshot = Object.freeze({
  phase: 'offline',
  pendingCount: 0,
  blockedCount: 0,
})

export interface ThoughtSyncController {
  getSnapshot(): ThoughtSyncSnapshot
  subscribe(listener: () => void): () => void
  /** Starts automatic lifecycle handling and returns a balanced disposer. */
  start(): () => void
  /** Joins the controller's in-flight sync; service Retry-After remains authoritative. */
  sync(): Promise<ThoughtSyncResult>
}

/**
 * Identity-scoped data used by Reader aggregate surfaces. This deliberately
 * excludes the server operation winner key: a local recovery projection is
 * not a generated server response and must never become a replay input.
 */
export interface ThoughtReadModelItem {
  readonly id: string
  readonly host_kind: string
  readonly host_id: string
  readonly link_id?: string | null
  readonly target: unknown
  readonly quote?: unknown
  readonly body: string
  readonly source: string
  readonly deleted: boolean
  readonly last_sequence: number
  readonly created_at: string
  readonly updated_at: string
  readonly lifecycle_status?: 'active' | 'tombstone'
  readonly lifecycle_reason?: string | null
  readonly tombstoned_at?: string | null
}

interface PreparedState {
  readonly state: ThoughtSyncStateRecord
  readonly outbox: readonly ThoughtOutboxRecord[]
  readonly historyOutbox: readonly ThoughtHistoryOutboxRecord[]
}

/**
 * Reads only sync metadata used to decide whether the lock holder changed
 * durable state. Thought payload, quote, target, and annotation fields are
 * intentionally absent from this local-only fingerprint.
 */
async function readSyncDurableFingerprint(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<string>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'read thought sync fingerprint',
    [THOUGHT_OUTBOX_STORE, THOUGHT_HISTORY_OUTBOX_STORE, THOUGHT_SYNC_STATE_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const stateRequest = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
        .get(namespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const historyRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyDone) return
        const state = stateRequest.result
        const outbox = outboxRequest.result.filter((record): record is ThoughtOutboxRecord =>
          isOutboxRecord(record) && record.namespace === namespace)
        const historyOutbox = activeHistoryOutboxRows(historyRequest.result, namespace)
        if (
          (state !== undefined && !isSyncState(state, namespace)) ||
          outbox.length !== outboxRequest.result.length || historyOutbox === null
        ) {
          transaction.abort()
          return
        }
        // Use ordered scalar tuples so this comparison never serializes wire
        // payloads or materialized Thought content.
        const stateFields = state === undefined ? null : [
          state.cursor,
          state.deviceId,
          state.tabToken,
          state.clockContractVersion ?? null,
          state.logicalClockFloor ?? null,
          state.updatedAt,
          state.lastAckSequence ?? null,
          state.pullAttemptCount ?? null,
          state.pullRetryAt ?? null,
          state.pullLastError ?? null,
          state.retryAt ?? null,
          state.lastError ?? null,
          state.lastErrorCode ?? null,
          state.lastSuccessfulSyncAt ?? null,
          state.pullInProgress ?? false,
          state.resyncRequired ?? false,
          state.lastServerSequence ?? null,
          state.retentionCutoff ?? null,
        ]
        const outboxFields = [...outbox]
          .sort((left, right) => left.sequence - right.sequence)
          .map((record) => [
            record.sequence,
            record.opId,
            record.deviceId,
            record.contractVersion,
            record.logicalClock,
            record.operationKind,
            record.createdAt,
            record.attemptCount,
            record.nextAttemptAt ?? null,
            record.status ?? 'pending',
            record.blockedReason ?? null,
            record.lastError ?? null,
          ])
        const historyFields = [...historyOutbox]
          .sort((left, right) => left.opId.localeCompare(right.opId))
          .map((record) => [
            record.opId,
            record.deviceId,
            record.logicalClock,
            record.action,
            record.createdAt,
            record.attemptCount,
            record.nextAttemptAt ?? null,
            record.status ?? 'pending',
            record.blockedReason ?? null,
            record.lastError ?? null,
          ])
        setResult(JSON.stringify([stateFields, outboxFields, historyFields]))
      }
      stateRequest.onerror = () => transaction.abort()
      outboxRequest.onerror = () => transaction.abort()
      historyRequest.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      historyRequest.onsuccess = () => { historyDone = true; finish() }
    },
  )
}

/**
 * A lock-losing tab must reread durable state without running the normalizing
 * readwrite preparation path. The lock holder owns all mutations for its round.
 */
async function readPreparedState(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<PreparedState | undefined>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'read thought sync state',
    [THOUGHT_OUTBOX_STORE, THOUGHT_HISTORY_OUTBOX_STORE, THOUGHT_SYNC_STATE_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const stateRequest = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
        .get(namespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const historyRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyDone) return
        const state = stateRequest.result
        const outbox = outboxRequest.result.filter((record): record is ThoughtOutboxRecord =>
          isOutboxRecord(record) && record.namespace === namespace)
        const historyOutbox = activeHistoryOutboxRows(historyRequest.result, namespace)
        if (
          (state !== undefined && !isSyncState(state, namespace)) ||
          outbox.length !== outboxRequest.result.length || historyOutbox === null
        ) {
          transaction.abort()
          return
        }
        setResult(state === undefined ? undefined : {
          state,
          outbox: outbox.sort((left, right) => left.sequence - right.sequence),
          historyOutbox: historyOutbox.sort((left, right) => left.createdAt - right.createdAt ||
            left.opId.localeCompare(right.opId)),
        })
      }
      stateRequest.onerror = () => transaction.abort()
      outboxRequest.onerror = () => transaction.abort()
      historyRequest.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      historyRequest.onsuccess = () => { historyDone = true; finish() }
    },
  )
}

function isTarget(value: unknown): value is ThoughtTarget {
  return canonicalThoughtTarget(value) !== null
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function isOutboxRecord(value: unknown): value is ThoughtOutboxRecord {
  return isValidThoughtOutboxRecord(value)
}

function isRepairRecord(value: ThoughtOutboxRecord): value is ThoughtRepairReadyRecord {
  return (value as ThoughtOutboxRecord & { repair?: unknown }).repair === true
}

function sourceLegacyOpIDs(rows: readonly unknown[], namespace: string): ReadonlySet<string> {
  const ids = new Set<string>()
  for (const row of rows) {
    if (!isRecord(row) || row.namespace !== namespace || typeof row.legacyOpId !== 'string') continue
    ids.add(row.legacyOpId)
  }
  return ids
}

function liveOutboxRows(
  rows: readonly unknown[],
  sources: readonly unknown[],
  namespace: string,
): readonly ThoughtOutboxRecord[] | null {
  const legacyIDs = sourceLegacyOpIDs(sources, namespace)
  const live = rows.filter((raw) => {
    const opId = isRecord(raw) ? raw.opId : undefined
    return typeof opId !== 'string' || !legacyIDs.has(opId)
  })
  const valid = live.filter((row): row is ThoughtOutboxRecord =>
    isOutboxRecord(row) && row.namespace === namespace)
  return valid.length === live.length ? valid : null
}

const repairBroadcastSuppression = new Map<string, { fingerprint: string; until: number }>()

function repairQuarantineDetail(namespace: string, rows: readonly unknown[]) {
  const reasons = new Map<string, number>()
  for (const row of rows) {
    if (!isRecord(row) || row.namespace !== namespace || typeof row.reason !== 'string') continue
    reasons.set(row.reason, (reasons.get(row.reason) ?? 0) + 1)
  }
  return {
    namespace,
    reasons: [...reasons.entries()].sort(([left], [right]) => left.localeCompare(right))
      .map(([reason, count]) => ({ reason, count })),
  }
}

function announceRepairQuarantine(namespace: string, rows: readonly unknown[]): void {
  const detail = repairQuarantineDetail(namespace, rows)
  if (detail.reasons.length === 0 || typeof window === 'undefined') return
  window.dispatchEvent(new CustomEvent('webtag:thought-repair-quarantine', { detail }))
  const fingerprint = JSON.stringify(detail)
  const suppressed = repairBroadcastSuppression.get(namespace)
  if (suppressed && suppressed.until >= Date.now() && suppressed.fingerprint === fingerprint) return
  repairBroadcastSuppression.delete(namespace)
  try {
    const Channel = globalThis.BroadcastChannel
    if (!Channel) return
    const channel = new Channel(THOUGHT_SYNC_CHANNEL_NAME)
    channel.postMessage({ kind: 'thought-repair-quarantine', ...detail } satisfies ThoughtRepairQuarantineHint)
    channel.close()
    repairBroadcastSuppression.set(namespace, {
      fingerprint,
      until: Date.now() + REPAIR_BROADCAST_SUPPRESSION_MS,
    })
  } catch {
    // Cross-tab observability is best-effort; quarantine remains durable.
  }
}

function isSyncState(value: unknown, namespace: string): value is ThoughtSyncStateRecord {
  return isRecord(value) && value.namespace === namespace &&
    typeof value.cursor === 'string' && typeof value.deviceId === 'string' &&
    value.deviceId.length > 0 && typeof value.tabToken === 'string' &&
    value.tabToken.length > 0 && isSafeNonNegativeInteger(value.updatedAt) &&
    (value.clockContractVersion === undefined ||
      value.clockContractVersion === THOUGHT_CONTRACT_VERSION) &&
    (value.logicalClockFloor === undefined ||
      (isSafeNonNegativeInteger(value.logicalClockFloor) &&
        value.logicalClockFloor <= Number.MAX_SAFE_INTEGER)) &&
    (value.lastAckSequence === undefined || isSafeNonNegativeInteger(value.lastAckSequence)) &&
    (value.pullAttemptCount === undefined || isSafeNonNegativeInteger(value.pullAttemptCount)) &&
    (value.pullRetryAt === undefined || isSafeNonNegativeInteger(value.pullRetryAt)) &&
    (value.pullLastError === undefined || typeof value.pullLastError === 'string') &&
    (value.retryAt === undefined || isSafeNonNegativeInteger(value.retryAt)) &&
    (value.lastError === undefined || typeof value.lastError === 'string') &&
    (value.lastErrorCode === undefined || typeof value.lastErrorCode === 'string') &&
    (value.lastSuccessfulSyncAt === undefined || isSafeNonNegativeInteger(value.lastSuccessfulSyncAt)) &&
    (value.pullInProgress === undefined || typeof value.pullInProgress === 'boolean') &&
    (value.resyncRequired === undefined || typeof value.resyncRequired === 'boolean') &&
    (value.lastServerSequence === undefined || isSafeNonNegativeInteger(value.lastServerSequence)) &&
    (value.retentionCutoff === undefined || isSafeNonNegativeInteger(value.retentionCutoff))
}

function activeHistoryOutboxRows(
  values: readonly unknown[],
  namespace: string,
): ThoughtHistoryOutboxRecord[] | null {
  const active = values.filter((record): record is ThoughtHistoryOutboxRecord =>
    isValidThoughtHistoryOutboxRecord(record, namespace))
  const quarantined = values.filter((record) =>
    isQuarantinedThoughtHistoryOutboxRecord(record, namespace))
  return active.length + quarantined.length === values.length ? active : null
}

function normalizeHistoryOutboxRows(
  transaction: IDBTransaction,
  values: readonly unknown[],
  namespace: string,
  materialized: readonly ThoughtMaterializedRecord[],
): ThoughtHistoryOutboxRecord[] | null {
  const materializedByID = new Map(materialized.map((record) => [record.annotationId, record]))
  const store = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
  const active: ThoughtHistoryOutboxRecord[] = []
  for (const value of values) {
    const raw = isRecord(value) ? value : null
    const winnerKey = typeof raw?.annotationId === 'string'
      ? materializedByID.get(raw.annotationId)?.winnerKey
      : undefined
    const hydrated = hydrateThoughtHistoryOutboxWinnerKey(value, namespace, winnerKey)
    if (hydrated) {
      if (hydrated !== value) store.put(hydrated)
      active.push(hydrated)
      continue
    }
    const quarantined = quarantineThoughtHistoryOutboxMissingWinnerKey(value, namespace)
    if (!quarantined) return null
    if (quarantined !== value) store.put(quarantined)
  }
  return active
}

async function prepareState(lease: IdentityLease): Promise<UserDataTransactionResult<PreparedState>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'prepare thought sync',
    [
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_SOURCE_STORE,
      THOUGHT_REPAIR_QUARANTINE_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      const stateRequest = transaction.objectStore(THOUGHT_SYNC_STATE_STORE).get(namespace) as IDBRequest<unknown>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const historyOutboxRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const repairedRequest = transaction.objectStore(THOUGHT_REPAIR_READY_STORE)
        .getAll() as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      const quarantineRequest = transaction.objectStore(THOUGHT_REPAIR_QUARANTINE_STORE)
        .getAll() as IDBRequest<unknown[]>
      const materializedRequest = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyOutboxDone = false
      let materializedDone = false
      let repairedDone = false
      let sourceDone = false
      let quarantineDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyOutboxDone || !repairedDone ||
          !sourceDone || !quarantineDone || !materializedDone) return
        if (!lease.isCurrent(identity)) {
          transaction.abort()
          return
        }
        try {
          const rawState = stateRequest.result
          const deviceID = stableThoughtDeviceID(lease, rawState)
          const prior = isSyncState(rawState, namespace) ? rawState : undefined
          const repaired = repairedRequest.result.filter(isOutboxRecord).filter((record) =>
            record.namespace === namespace)
          // Source mappings cover every v5 row, including acknowledged rows
          // whose v6 ready copy has already been removed. This prevents an
          // immutable legacy tail from returning after a reload/rebuild.
          const liveOutbox = liveOutboxRows(outboxRequest.result, sourceRequest.result, namespace)
          if (!liveOutbox) {
            transaction.abort()
            return
          }
          const allRaw = [...liveOutbox, ...repaired]
          const outbox = allRaw.filter(isOutboxRecord).sort((left, right) =>
            left.sequence - right.sequence)
          const materialized = materializedRequest.result.filter((record): record is ThoughtMaterializedRecord =>
            isValidThoughtMaterializedRecord(record, namespace))
          const historyOutbox = normalizeHistoryOutboxRows(
            transaction,
            historyOutboxRequest.result,
            namespace,
            materialized,
          )
          if (outbox.length !== allRaw.length ||
            materialized.length !== materializedRequest.result.length || historyOutbox === null) {
            transaction.abort()
            return
          }
          let floor = prior?.deviceId === deviceID &&
            prior.clockContractVersion === THOUGHT_CONTRACT_VERSION
            ? prior.logicalClockFloor ?? 0
            : 0
          floor = maximumThoughtClock([
            floor,
            ...materialized.map((record) => record.winnerKey.logicalClock),
            ...outbox
              .filter(hasCompleteThoughtWireBaseline)
              .map((record) => record.logicalClock),
            ...historyOutbox.map((record) => record.logicalClock),
            ...historyOutbox.map((record) => record.snapshot.winnerKey.logicalClock),
          ])
          const normalizedOutbox: ThoughtOutboxRecord[] = []
          const outboxStore = transaction.objectStore(THOUGHT_OUTBOX_STORE)
          for (const record of outbox) {
            const quarantined = quarantineLegacyThoughtOutboxRecord(record)
            const failureSafe = normalizedOutboxFailureMarkers(quarantined)
            if (failureSafe !== record) outboxStore.put(failureSafe)
            normalizedOutbox.push(failureSafe)
          }
          const state: ThoughtSyncStateRecord = {
            ...(prior ?? {}),
            namespace,
            cursor: prior?.cursor ?? '',
            deviceId: deviceID,
            tabToken: prior?.tabToken || TAB_TOKEN,
            clockContractVersion: THOUGHT_CONTRACT_VERSION,
            logicalClockFloor: floor,
            updatedAt: prior?.updatedAt ?? Date.now(),
            // Historical versions persisted transport messages. Retain only a
            // generic marker while a later successful round clears it.
            ...(prior?.lastError === undefined
              ? {}
              : { lastError: storedFailureCode(prior.lastError) ?? 'sync-failed' }),
            ...(prior?.pullLastError === undefined
              ? {}
              : { pullLastError: storedFailureCode(prior.pullLastError) ?? 'sync-failed' }),
            ...(prior?.lastErrorCode === undefined
              ? {}
              : { lastErrorCode: storedFailureCode(prior.lastErrorCode) ?? 'sync-failed' }),
          }
          transaction.objectStore(THOUGHT_SYNC_STATE_STORE).put(state)
          announceRepairQuarantine(namespace, quarantineRequest.result)
          setResult({ state, outbox: normalizedOutbox, historyOutbox })
        } catch {
          transaction.abort()
        }
      }
      stateRequest.onerror = () => transaction.abort()
      outboxRequest.onerror = () => transaction.abort()
      historyOutboxRequest.onerror = () => transaction.abort()
      repairedRequest.onerror = () => transaction.abort()
      sourceRequest.onerror = () => transaction.abort()
      quarantineRequest.onerror = () => transaction.abort()
      materializedRequest.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      historyOutboxRequest.onsuccess = () => { historyOutboxDone = true; finish() }
      repairedRequest.onsuccess = () => { repairedDone = true; finish() }
      sourceRequest.onsuccess = () => { sourceDone = true; finish() }
      quarantineRequest.onsuccess = () => { quarantineDone = true; finish() }
      materializedRequest.onsuccess = () => { materializedDone = true; finish() }
    },
  )
}

function wireTarget(record: ThoughtOutboxRecord): Record<string, unknown> {
  const annotation = record.annotation
  const target: Record<string, unknown> = {
    kind: record.target.kind,
    host_id: record.hostId,
    version: record.target.kind === 'saved-content'
      ? { content_revision: record.target.contentRevision }
      : record.target.kind === 'summary'
        ? { source_hash: record.target.sourceHash }
        : record.target.kind === 'note'
          ? { note_revision: record.target.noteRevision }
          : record.target.kind === 'inbox'
            ? { metadata_revision: record.target.metadataRevision }
            : { source_key: record.target.sourceKey },
  }
  if (annotation) {
    target.block_key = annotation.blockKey || (record.target.kind === 'note' ? 'note' : 'content')
    target.range = { start: annotation.start, end: annotation.end }
  }
  if (record.target.kind === 'note' && target.block_key === undefined) target.block_key = 'note'
  return target
}

function wirePayload(record: ThoughtOutboxRecord): Record<string, unknown> {
  const annotation = record.annotation
  const payload: Record<string, unknown> = {
    body: record.patch?.note ?? annotation?.note ?? '',
    source: record.patch?.source ?? annotation?.source ?? 'self',
  }
  if (record.hostKind === 'link') payload.link_id = record.linkId
  if (annotation) {
    payload.annotation = annotation
    payload.quote = {
      exact: annotation.text,
      block_key: annotation.blockKey || (record.target.kind === 'note' ? 'note' : 'content'),
      start: annotation.start,
      end: annotation.end,
      prefix: annotation.quote?.prefix ?? '',
      suffix: annotation.quote?.suffix ?? '',
    }
  }
  return payload
}

function toWireOperation(record: CompleteThoughtOutboxRecord): ReaderThoughtOpRequest {
  return {
    contract_version: THOUGHT_CONTRACT_VERSION,
    op_id: record.opId,
    device_id: record.deviceId,
    logical_clock: record.logicalClock,
    operation_kind: record.operationKind,
    annotation_id: record.annotationId,
    host_kind: record.hostKind,
    host_id: record.hostId,
    target: wireTarget(record) as ReaderThoughtOpRequest['target'],
    payload: wirePayload(record) as ReaderThoughtOpRequest['payload'],
    ...(record.recoveryOf && record.expectedCurrentWinnerKey ? {
      recovery_of: {
        logical_clock: record.recoveryOf.logicalClock,
        device_id: record.recoveryOf.deviceId,
        op_id: record.recoveryOf.opId,
      },
      expected_current_winner_key: {
        logical_clock: record.expectedCurrentWinnerKey.logicalClock,
        device_id: record.expectedCurrentWinnerKey.deviceId,
        op_id: record.expectedCurrentWinnerKey.opId,
      },
    } : {}),
  }
}

function wireHistoryTarget(record: ThoughtHistoryOutboxRecord): Record<string, unknown> {
  return {
    kind: record.target.kind,
    host_id: record.hostId,
    version: record.target.kind === 'saved-content'
      ? { content_revision: record.target.contentRevision }
      : record.target.kind === 'summary'
        ? { source_hash: record.target.sourceHash }
        : record.target.kind === 'note'
          ? { note_revision: record.target.noteRevision }
          : record.target.kind === 'inbox'
            ? { metadata_revision: record.target.metadataRevision }
            : { source_key: record.target.sourceKey },
  }
}

function toHistoryWireOperation(record: ThoughtHistoryOutboxRecord): ReaderThoughtOpRequest {
  return {
    contract_version: THOUGHT_CONTRACT_VERSION,
    op_id: record.opId,
    device_id: record.deviceId,
    logical_clock: record.logicalClock,
    operation_kind: record.action === 'delete' ? 'delete' : 'update',
    annotation_id: record.annotationId,
    host_kind: record.hostKind,
    host_id: record.hostId,
    target: wireHistoryTarget(record) as ReaderThoughtOpRequest['target'],
    payload: record.action === 'delete'
      ? {} as ReaderThoughtOpRequest['payload']
      : {
          reattach: {
            expected_last_sequence: record.expectedLastSequence,
            expected_host_revision: record.expectedHostRevision,
          },
        } as ReaderThoughtOpRequest['payload'],
  }
}

function retryDelay(error: ApiError, attemptCount: number): number {
  if (
    error.retryAfterSeconds !== undefined &&
    Number.isFinite(error.retryAfterSeconds) &&
    error.retryAfterSeconds >= 0
  ) {
    // Retry-After is a server contract. Local exponential backoff remains
    // capped below, but never shortens an explicit service cooldown.
    return Math.ceil(error.retryAfterSeconds * 1000)
  }
  return Math.min(
    MAX_RETRY_MS,
    BASE_RETRY_MS * (2 ** Math.min(Math.max(attemptCount - 1, 0), 8)),
  )
}

function isPermanentPushFailure(error: ApiError): boolean {
  if (error.kind === 'unauthorized') return true
  if (error.kind !== 'other' || error.status === undefined) return false
  return error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

function isRecoveryCASConflict(record: ThoughtOutboxRecord, error: ApiError): boolean {
  return record.recoveryOf !== undefined && record.expectedCurrentWinnerKey !== undefined &&
    error.kind === 'other' && error.status === 409 && error.errorCode === 'thought_recovery_conflict'
}

function durablePushError(record: ThoughtOutboxRecord, error: ApiError): string {
  return record.recoveryOf === undefined
    ? error.message.slice(0, 512)
    : '恢复版本同步失败，请刷新后重试。'
}

/** Only request-validation 4xx responses can safely identify a single poison op by splitting. */
function isIsolatablePermanentPushFailure(error: ApiError): boolean {
  return error.kind === 'other' && error.status !== undefined &&
    error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

const STABLE_ERROR_COMPONENT = /^[a-z0-9][a-z0-9._-]{0,63}$/
const STABLE_FAILURE_CODE = /^[a-z0-9][a-z0-9._-]{0,63}(?::[a-z0-9][a-z0-9._-]{0,63}){0,2}$/

function stableErrorComponent(value: unknown): string | undefined {
  return typeof value === 'string' && STABLE_ERROR_COMPONENT.test(value) ? value : undefined
}

function storedFailureCode(value: unknown): string | undefined {
  return typeof value === 'string' && value.length <= 128 && STABLE_FAILURE_CODE.test(value)
    ? value
    : undefined
}

/**
 * The UI and cross-tab controller need a durable failure classification, but
 * must never surface transport messages as protocol state: messages can be
 * server supplied and may contain request context. Keep this deliberately
 * small and composed only from the typed API classification.
 */
function failureCode(error: ApiError): string {
  const serverCode = stableErrorComponent(error.errorCode)
  const status = Number.isSafeInteger(error.status) && (error.status as number) >= 100 &&
    (error.status as number) <= 599
    ? String(error.status)
    : undefined
  return [
    error.kind,
    serverCode,
    status,
  ]
    .filter((value): value is string => value !== undefined && value.length > 0)
    .join(':')
}

type SyncOutboxRecord = ThoughtOutboxRecord | ThoughtHistoryOutboxRecord
type DispatchableSyncOutboxRecord = CompleteThoughtOutboxRecord | ThoughtHistoryOutboxRecord
type SyncOutboxStore = typeof THOUGHT_OUTBOX_STORE | typeof THOUGHT_HISTORY_OUTBOX_STORE

function blockedFailureCode(records: readonly SyncOutboxRecord[]): string | undefined {
  const blocked = records.find((record) => record.status === 'blocked')
  if (!blocked) return undefined
  return storedFailureCode(blocked.blockedReason) ?? 'blocked-operation'
}

function normalizedOutboxFailureMarkers(record: ThoughtOutboxRecord): ThoughtOutboxRecord {
  const lastError = record.lastError === undefined
    ? undefined
    : storedFailureCode(record.lastError) ?? 'sync-failed'
  const blockedReason = record.status === 'blocked'
    ? storedFailureCode(record.blockedReason) ?? 'blocked-operation'
    : undefined
  if (lastError === record.lastError && blockedReason === record.blockedReason) return record
  const normalized = { ...record }
  if (lastError === undefined) delete normalized.lastError
  else normalized.lastError = lastError
  if (blockedReason === undefined) delete normalized.blockedReason
  else normalized.blockedReason = blockedReason
  return normalized
}

function earliestRetryAt(records: readonly SyncOutboxRecord[]): number | undefined {
  return records.reduce<number | undefined>((earliest, record) => {
    if (record.status === 'blocked') return earliest
    if (record.nextAttemptAt === undefined) return earliest
    return earliest === undefined ? record.nextAttemptAt : Math.min(earliest, record.nextAttemptAt)
  }, undefined)
}

function earliestRetryTime(
  outboxRetryAt: number | undefined,
  pullRetryAt: number | undefined,
): number | undefined {
  if (outboxRetryAt === undefined) return pullRetryAt
  if (pullRetryAt === undefined) return outboxRetryAt
  return Math.min(outboxRetryAt, pullRetryAt)
}

function stateRetryAt(
  records: readonly SyncOutboxRecord[],
  state: ThoughtSyncStateRecord,
): number | undefined {
  return earliestRetryTime(earliestRetryAt(records), state.pullRetryAt)
}

function isOffline(): boolean {
  return typeof navigator !== 'undefined' && navigator.onLine === false
}

async function markPushFailure(
  lease: IdentityLease,
  outboxStoreName: SyncOutboxStore,
  records: readonly DispatchableSyncOutboxRecord[],
  error: ApiError,
): Promise<UserDataTransactionResult<number | undefined>> {
  if (records.length === 0) return { ok: true, value: 0 }
  return runUserDataTransaction(
    lease,
    'record thought sync failure',
    [
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_SOURCE_STORE,
      THOUGHT_SYNC_STATE_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const now = Date.now()
      const outbox = transaction.objectStore(THOUGHT_OUTBOX_STORE)
      const historyOutbox = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
      const repaired = transaction.objectStore(THOUGHT_REPAIR_READY_STORE)
      const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const permanent = isPermanentPushFailure(error)
      const code = failureCode(error)
      for (const record of records) {
        const attemptCount = record.attemptCount + 1
        if (outboxStoreName === THOUGHT_OUTBOX_STORE) {
          const standardRecord = record as CompleteThoughtOutboxRecord
          const destination = isRepairRecord(standardRecord) ? repaired : outbox
          if (isRecoveryCASConflict(standardRecord, error)) {
            const withoutRetry = { ...standardRecord }
            delete withoutRetry.nextAttemptAt
            outbox.put({
              ...withoutRetry,
              attemptCount,
              status: 'blocked',
              blockedReason: 'thought_recovery_conflict',
              recoveryConflict: true,
              lastError: durablePushError(standardRecord, error),
            } satisfies ThoughtOutboxRecord)
            continue
          }
          if (permanent) {
            const withoutRetry = { ...standardRecord }
            delete withoutRetry.nextAttemptAt
            destination.put({
              ...withoutRetry,
              attemptCount,
              status: 'blocked',
              blockedReason: code,
              lastError: code,
            } satisfies ThoughtOutboxRecord)
            continue
          }
          const retryAt = now + retryDelay(error, attemptCount)
          const withoutBlock = { ...standardRecord }
          delete withoutBlock.blockedReason
          delete withoutBlock.recoveryConflict
          destination.put({
            ...withoutBlock,
            attemptCount,
            status: 'pending',
            nextAttemptAt: retryAt,
            lastError: code,
          } satisfies ThoughtOutboxRecord)
          continue
        }

        const historyRecord = record as ThoughtHistoryOutboxRecord
        if (permanent) {
          const withoutRetry = { ...historyRecord }
          delete withoutRetry.nextAttemptAt
          historyOutbox.put({
            ...withoutRetry,
            attemptCount,
            status: 'blocked',
            blockedReason: code,
            lastError: code,
          } satisfies ThoughtHistoryOutboxRecord)
          continue
        }
        const retryAt = now + retryDelay(error, attemptCount)
        const withoutBlock = { ...historyRecord }
        delete withoutBlock.blockedReason
        historyOutbox.put({
          ...withoutBlock,
          attemptCount,
          status: 'pending',
          nextAttemptAt: retryAt,
          lastError: code,
        } satisfies ThoughtHistoryOutboxRecord)
      }
      const stateRequest = stateStore.get(lease.context.physicalNamespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const standardRequest = outbox.index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const historyRequest = historyOutbox.index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const repairRequest = repaired.getAll() as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      let stateDone = false
      let standardDone = false
      let historyDone = false
      let repairDone = false
      let sourceDone = false
      const finish = () => {
        if (!stateDone || !standardDone || !historyDone || !repairDone || !sourceDone) return
        const state = stateRequest.result
        const standard = liveOutboxRows(
          standardRequest.result,
          sourceRequest.result,
          lease.context.physicalNamespace,
        )
        const rawRepair = repairRequest.result.filter((record) =>
          isRecord(record) && record.namespace === lease.context.physicalNamespace)
        const remainingRepair = rawRepair.filter(isOutboxRecord)
        const history = activeHistoryOutboxRows(
          historyRequest.result,
          lease.context.physicalNamespace,
        )
        if (!state || !standard || remainingRepair.length !== rawRepair.length || history === null) {
          transaction.abort()
          return
        }
        const retryAt = stateRetryAt([...standard, ...remainingRepair, ...history], state)
        stateStore.put({
          ...state,
          ...(retryAt === undefined ? { retryAt: undefined } : { retryAt }),
          lastError: code,
          lastErrorCode: code,
          updatedAt: now,
        })
        setResult(retryAt)
      }
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      stateRequest.onerror = () => transaction.abort()
      standardRequest.onsuccess = () => { standardDone = true; finish() }
      standardRequest.onerror = () => transaction.abort()
      historyRequest.onsuccess = () => { historyDone = true; finish() }
      historyRequest.onerror = () => transaction.abort()
      repairRequest.onsuccess = () => { repairDone = true; finish() }
      repairRequest.onerror = () => transaction.abort()
      sourceRequest.onsuccess = () => { sourceDone = true; finish() }
      sourceRequest.onerror = () => transaction.abort()
    },
  )
}

function versionKeyFromWire(value: unknown): ThoughtVersionKey | null {
  if (!isRecord(value)) return null
  const key = {
    logicalClock: value.logical_clock,
    deviceId: value.device_id,
    opId: value.op_id,
  }
  return isValidThoughtVersionKey(key, true) ? key : null
}

async function acknowledge(
  lease: IdentityLease,
  outboxStoreName: SyncOutboxStore,
  records: readonly DispatchableSyncOutboxRecord[],
  acks: readonly ReaderThoughtAckResponse[],
): Promise<UserDataTransactionResult<readonly string[]>> {
  if (acks.length !== records.length || !acks.every((ack) => {
    const submitted = versionKeyFromWire(ack.submitted_key)
    const winner = versionKeyFromWire(ack.current_winner_key)
    const record = records.find((candidate) => candidate.opId === ack.op_id)
    return ack.contract_version === THOUGHT_CONTRACT_VERSION &&
      (ack.disposition === 'applied' || ack.disposition === 'superseded' ||
        ack.disposition === 'duplicate') &&
      isNonEmptyString(ack.op_id) && isSafeNonNegativeInteger(ack.sequence) && ack.sequence > 0 &&
      submitted !== null && winner !== null && record !== undefined &&
      submitted.logicalClock === record.logicalClock &&
      submitted.deviceId === record.deviceId && submitted.opId === record.opId
  })) {
    return { ok: false }
  }
  const ackByID = new Map(acks.map((ack) => [ack.op_id, ack]))
  if (ackByID.size !== records.length || records.some((record) => !ackByID.has(record.opId))) {
    return { ok: false }
  }
  const acknowledged = records.filter((record) => ackByID.has(record.opId))
  if (acknowledged.length === 0) return { ok: true, value: [] }
  return runUserDataTransaction(
    lease,
    'ack thought sync',
    [
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_REPAIR_READY_STORE,
      THOUGHT_REPAIR_ACK_STORE,
      THOUGHT_REPAIR_SOURCE_STORE,
      THOUGHT_SYNC_STATE_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const outbox = transaction.objectStore(THOUGHT_OUTBOX_STORE)
      const historyOutbox = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
      const repaired = transaction.objectStore(THOUGHT_REPAIR_READY_STORE)
      const acknowledgements = transaction.objectStore(THOUGHT_REPAIR_ACK_STORE)
      let lastAck = 0
      const observedClocks: number[] = []
      const removedOpIds: string[] = []
      for (const record of acknowledged) {
        const ack = ackByID.get(record.opId)
        lastAck = Math.max(lastAck, ack?.sequence ?? 0)
        const submitted = versionKeyFromWire(ack?.submitted_key)
        const winner = versionKeyFromWire(ack?.current_winner_key)
        if (!submitted || !winner) {
          transaction.abort()
          return
        }
        observedClocks.push(submitted.logicalClock, winner.logicalClock)
        if (outboxStoreName === THOUGHT_HISTORY_OUTBOX_STORE) {
          const historyRecord = record as ThoughtHistoryOutboxRecord
          if (ack?.disposition !== 'superseded') {
            historyOutbox.delete([historyRecord.key[0], historyRecord.key[1]])
            removedOpIds.push(historyRecord.opId)
            continue
          }
          const blocked = { ...historyRecord }
          delete blocked.nextAttemptAt
          historyOutbox.put({
            ...blocked,
            status: 'blocked',
            blockedReason: 'superseded',
            lastError: HISTORY_SUPERSEDED_ERROR,
          } satisfies ThoughtHistoryOutboxRecord)
          continue
        }
        const standardRecord = record as CompleteThoughtOutboxRecord
        if (isRepairRecord(standardRecord)) {
          // Repair rows are derived copies of immutable v4/v5 sources. Remove
          // only the v6 dispatch copy after an authoritative acknowledgement.
          const receipt = {
            key: [standardRecord.namespace, standardRecord.opId] as [string, string],
            namespace: standardRecord.namespace,
            opId: standardRecord.opId,
            readyChecksum: repairReadyChecksum(standardRecord),
          }
          if (!isThoughtRepairAck(receipt, standardRecord)) {
            transaction.abort()
            return
          }
          acknowledgements.put(receipt)
          repaired.delete([standardRecord.key[0], standardRecord.key[1]])
          removedOpIds.push(standardRecord.opId)
        } else {
          outbox.delete([standardRecord.key[0], standardRecord.key[1]])
          removedOpIds.push(standardRecord.opId)
        }
      }
      const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const stateRequest = stateStore.get(lease.context.physicalNamespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const remainingRequest = outbox.index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const historyRequest = historyOutbox.index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const remainingRepairRequest = repaired.getAll() as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      let stateDone = false
      let remainingDone = false
      let historyDone = false
      let remainingRepairDone = false
      let sourceDone = false
      const finish = () => {
        if (!stateDone || !remainingDone || !historyDone || !remainingRepairDone || !sourceDone) return
        const state = stateRequest.result
        const remainingOutbox = liveOutboxRows(
          remainingRequest.result,
          sourceRequest.result,
          lease.context.physicalNamespace,
        )
        const rawRepair = remainingRepairRequest.result.filter((record) =>
          isRecord(record) && record.namespace === lease.context.physicalNamespace)
        const remainingRepair = rawRepair.filter(isOutboxRecord)
        const history = activeHistoryOutboxRows(
          historyRequest.result,
          lease.context.physicalNamespace,
        )
        if (!state || !remainingOutbox || remainingRepair.length !== rawRepair.length ||
          history === null) {
          transaction.abort()
          return
        }
        const remaining = [...remainingOutbox, ...remainingRepair, ...history]
        const retryAt = earliestRetryTime(earliestRetryAt(remaining), state.pullRetryAt)
        const blockedCode = blockedFailureCode(remaining)
        try {
          const logicalClockFloor = maximumThoughtClock([
            state.logicalClockFloor ?? 0,
            ...observedClocks,
          ])
          stateStore.put({
            ...state,
            clockContractVersion: THOUGHT_CONTRACT_VERSION,
            logicalClockFloor,
            lastAckSequence: Math.max(state.lastAckSequence ?? 0, lastAck),
            ...(retryAt === undefined
              ? {
                  retryAt: undefined,
                  lastError: undefined,
                  ...(blockedCode === undefined
                    ? { lastErrorCode: undefined }
                    : { lastErrorCode: blockedCode }),
                }
              : { retryAt }),
            updatedAt: Date.now(),
          })
          setResult(removedOpIds)
        } catch {
          transaction.abort()
        }
      }
      stateRequest.onsuccess = () => {
        stateDone = true
        finish()
      }
      stateRequest.onerror = () => transaction.abort()
      remainingRequest.onsuccess = () => {
        remainingDone = true
        finish()
      }
      remainingRequest.onerror = () => transaction.abort()
      historyRequest.onsuccess = () => {
        historyDone = true
        finish()
      }
      historyRequest.onerror = () => transaction.abort()
      remainingRepairRequest.onsuccess = () => {
        remainingRepairDone = true
        finish()
      }
      remainingRepairRequest.onerror = () => transaction.abort()
      sourceRequest.onsuccess = () => {
        sourceDone = true
        finish()
      }
      sourceRequest.onerror = () => transaction.abort()
    },
  )
}

function firstDefined(record: Record<string, unknown>, keys: readonly string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined) return record[key]
  }
  return undefined
}

function positiveInteger(value: unknown): number | null {
  if (isSafeNonNegativeInteger(value) && value > 0) return value
  if (typeof value !== 'string' || !/^[1-9][0-9]*$/.test(value)) return null
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null
}

function wireDate(value: unknown): string | null {
  if (typeof value === 'string' && value.trim() !== '') return value
  if (isSafeNonNegativeInteger(value)) {
    try {
      return new Date(value).toISOString()
    } catch {
      return null
    }
  }
  return null
}

function wireEpoch(value: unknown): number | undefined {
  if (isSafeNonNegativeInteger(value)) return value
  if (typeof value !== 'string' || value.trim() === '') return undefined
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined
}

function wireChecksum(raw: Record<string, unknown>): string | undefined {
  const candidate = firstDefined(raw, [
    'checksum',
    'checksum_sha256',
    'checksumSha256',
    'sourceFingerprint',
    'source_fingerprint',
  ])
  return typeof candidate === 'string' && /^[0-9a-f]{64}$/.test(candidate)
    ? candidate
    : undefined
}

function versionString(kind: string, value: unknown): Record<string, unknown> {
  if (typeof value !== 'string') return {}
  const prefix = kind === 'saved-content' ? 'content:' : `${kind}:`
  if (!value.startsWith(prefix)) return {}
  const tail = value.slice(prefix.length)
  if (kind === 'saved-content') return { contentRevision: positiveInteger(tail) }
  if (kind === 'summary') return { sourceHash: tail }
  if (kind === 'note') return { noteRevision: positiveInteger(tail) }
  if (kind === 'inbox') return { metadataRevision: positiveInteger(tail) }
  return { sourceKey: tail }
}

function decodeTarget(
  raw: unknown,
  hostID: string,
  annotation: ReturnType<typeof decodeAnnotationWire> = null,
): ThoughtTarget | null {
  const candidate = isRecord(raw) && isRecord(raw.target) ? raw.target : raw
  if (isRecord(candidate)) {
    const targetHostID = firstDefined(candidate, ['host_id', 'hostId'])
    if (targetHostID !== undefined && targetHostID !== hostID) return null
    if (isTarget(candidate)) return canonicalThoughtTarget(candidate)
    const kind = firstDefined(candidate, ['kind', 'target_kind'])
    if (typeof kind === 'string') {
      const versionValue = firstDefined(candidate, [
        'version',
        'sourceIdentity',
        'source_identity',
      ])
      const version = isRecord(versionValue)
        ? versionValue
        : kind === 'saved-content' && isSafeNonNegativeInteger(versionValue)
          ? { contentRevision: versionValue }
          : kind === 'note' && isSafeNonNegativeInteger(versionValue)
            ? { noteRevision: versionValue }
            : { ...versionString(kind, versionValue) }
      const value = (keys: readonly string[]): unknown =>
        firstDefined(candidate, keys) ?? firstDefined(version, keys)
      if (kind === 'saved-content') {
        const revision = positiveInteger(value([
          'contentRevision',
          'content_revision',
          'sourceContentRevision',
          'source_content_revision',
        ]))
        return revision === null
          ? null
          : canonicalAnnotationTarget({ kind, contentRevision: revision })
      }
      if (kind === 'summary') {
        const sourceHash = value([
          'sourceHash',
          'source_hash',
          'sourceSummaryHash',
          'source_summary_hash',
        ])
        return typeof sourceHash === 'string' && sourceHash.length > 0
          ? canonicalAnnotationTarget({ kind, sourceHash })
          : null
      }
      if (kind === 'note') {
        const revision = positiveInteger(value([
          'noteRevision',
          'note_revision',
          'sourceNoteRevision',
          'source_note_revision',
        ]))
        return revision === null
          ? null
          : canonicalAnnotationTarget({ kind, noteRevision: revision })
      }
      if (kind === 'inbox') {
        const revision = positiveInteger(value([
          'metadataRevision',
          'metadata_revision',
        ]))
        return revision === null
          ? null
          : canonicalThoughtTarget({ kind, metadataRevision: revision })
      }
      if (kind === 'legacy-stale') {
        const sourceKey = value(['sourceKey', 'source_key'])
        return typeof sourceKey === 'string' && sourceKey.length > 0
          ? canonicalAnnotationTarget({ kind, sourceKey })
          : null
      }
    }
  }

  if (annotation?.sourceContentRevision !== undefined) {
    return canonicalAnnotationTarget({
      kind: 'saved-content',
      contentRevision: annotation.sourceContentRevision,
    })
  }
  if (annotation?.sourceSummaryHash !== undefined) {
    return canonicalAnnotationTarget({
      kind: 'summary',
      sourceHash: annotation.sourceSummaryHash,
    })
  }
  if (annotation?.sourceNoteRevision !== undefined) {
    return canonicalAnnotationTarget({
      kind: 'note',
      noteRevision: annotation.sourceNoteRevision,
    })
  }
  if (isRecord(raw)) {
    const sourceKey = firstDefined(raw, ['sourceKey', 'source_key'])
    if (typeof sourceKey === 'string' && sourceKey.length > 0) {
      return canonicalAnnotationTarget({ kind: 'legacy-stale', sourceKey })
    }
  }
  return null
}

function decodeQuote(raw: unknown): Record<string, unknown> | null {
  if (!isRecord(raw)) return null
  const quoteValue = firstDefined(raw, ['quote', 'selector'])
  const quote = isRecord(quoteValue) ? quoteValue : raw
  const rangeValue = firstDefined(raw, ['range', 'selection']) ??
    firstDefined(quote, ['range', 'selection'])
  const range = isRecord(rangeValue) ? rangeValue : undefined
  const exact = firstDefined(quote, ['exact', 'text', 'selected_text']) ??
    firstDefined(raw, ['text', 'selected_text'])
  const start = firstDefined(quote, ['start', 'start_offset', 'startOffset']) ??
    firstDefined(raw, ['start', 'start_offset', 'startOffset']) ??
    (range ? firstDefined(range, ['start', 'start_offset', 'startOffset']) : undefined)
  const end = firstDefined(quote, ['end', 'end_offset', 'endOffset']) ??
    firstDefined(raw, ['end', 'end_offset', 'endOffset']) ??
    (range ? firstDefined(range, ['end', 'end_offset', 'endOffset']) : undefined)
  if (typeof exact !== 'string' || !isSafeNonNegativeInteger(start) ||
    !isSafeNonNegativeInteger(end) || end < start) return null
  const prefix = firstDefined(quote, ['prefix', 'before']) ??
    firstDefined(raw, ['prefix', 'before'])
  const suffix = firstDefined(quote, ['suffix', 'after']) ??
    firstDefined(raw, ['suffix', 'after'])
  const blockKey = firstDefined(quote, ['block_key', 'blockKey']) ??
    firstDefined(raw, ['block_key', 'blockKey'])
  if ((prefix !== undefined && typeof prefix !== 'string') ||
    (suffix !== undefined && typeof suffix !== 'string') ||
    (blockKey !== undefined && typeof blockKey !== 'string')) return null
  return {
    exact,
    start,
    end,
    prefix: typeof prefix === 'string' ? prefix : '',
    suffix: typeof suffix === 'string' ? suffix : '',
    ...(typeof blockKey === 'string' ? { block_key: blockKey } : {}),
  }
}

function materializedRecord(namespace: string, item: unknown): ThoughtMaterializedRecord | null {
  if (!isRecord(item)) return null
  if (item.contract_version !== THOUGHT_CONTRACT_VERSION) return null
  const winnerKey = versionKeyFromWire(item.winner_key)
  if (!winnerKey) return null
  const nestedAnnotation = isRecord(item.annotation) ? item.annotation : undefined
  const annotation = decodeAnnotationWire(nestedAnnotation
    ? { ...item, ...nestedAnnotation }
    : item)
  const targetRaw = firstDefined(item, ['target', 'selection_target'])
  const hostIDValue = firstDefined(item, ['host_id', 'hostId']) ??
    (isRecord(targetRaw) ? firstDefined(targetRaw, ['host_id', 'hostId']) : undefined)
  const hostID = typeof hostIDValue === 'string' ? hostIDValue : ''
  const target = decodeTarget(targetRaw, hostID, annotation)
  const targetKey = target ? thoughtTargetKey(target) : null
  const kindValue = firstDefined(item, ['host_kind', 'hostKind', 'kind'])
  const hostKind = typeof kindValue === 'string' && kindValue.length > 0
    ? kindValue
    : target?.kind === 'note' ? 'note' : 'link'
  const idValue = firstDefined(item, ['id', 'annotation_id', 'annotationId', 'thought_id']) ??
    annotation?.id
  const sequence = positiveInteger(firstDefined(item, [
    'last_sequence',
    'lastSequence',
    'server_sequence',
    'serverSequence',
    'sequence',
  ]))
  const bodyValue = firstDefined(item, ['body', 'note', 'thought']) ?? annotation?.note
  const sourceValue = firstDefined(item, ['source', 'origin']) ?? annotation?.source ?? 'user'
  const deletedValue = firstDefined(item, ['deleted', 'is_deleted'])
  const lifecycle = firstDefined(item, ['lifecycle_status', 'lifecycleStatus'])
  const deleted = deletedValue === true || lifecycle === 'tombstone' || lifecycle === 'deleted'
  const createdAt = wireDate(firstDefined(item, ['created_at', 'createdAt', 'created'])) ??
    (annotation ? wireDate(annotation.createdAt) : null)
  const updatedAt = wireDate(firstDefined(item, ['updated_at', 'updatedAt', 'updated'])) ??
    (annotation ? wireDate(annotation.updatedAt) : null)
  const linkIDValue = firstDefined(item, ['link_id', 'linkId'])
  const linkID = linkIDValue === null || linkIDValue === undefined
    ? (hostKind === 'link' || hostKind === 'note' ? hostID : null)
    : typeof linkIDValue === 'string' && linkIDValue.length > 0 ? linkIDValue : null
  const rawQuote = firstDefined(item, ['quote', 'selector'])
  const quote = rawQuote === null
    ? (annotation ? decodeQuote(annotation) : null)
    : decodeQuote(item) ?? (annotation ? decodeQuote(annotation) : null)
  const retainedUntil = wireEpoch(firstDefined(item, [
    'retained_until',
    'retainedUntil',
    'tombstone_retained_until',
    'tombstoneRetainedUntil',
  ]))
  if (
    !isNonEmptyString(idValue) ||
    !isNonEmptyString(hostID) ||
    !isSafeNonNegativeInteger(sequence) ||
    sequence <= 0 ||
    !target ||
    !targetKey ||
    typeof bodyValue !== 'string' ||
    (sourceValue !== 'self' && sourceValue !== 'ai' && sourceValue !== 'user') ||
    !createdAt ||
    !updatedAt
  ) return null
  const record: ThoughtMaterializedRecord = {
    key: [namespace, idValue],
    namespace,
    annotationId: idValue,
    contractVersion: THOUGHT_CONTRACT_VERSION,
    winnerKey,
    hostKind,
    hostId: hostID,
    linkId: linkID,
    target,
    targetKey,
    quote,
    body: bodyValue,
    source: sourceValue,
    deleted,
    lifecycleStatus: lifecycle === 'tombstone' ? 'tombstone' : 'active',
    serverSequence: sequence,
    createdAt,
    updatedAt,
    ...(wireChecksum(item) === undefined ? {} : { checksum: wireChecksum(item) }),
    ...(retainedUntil === undefined ? {} : { retainedUntil }),
  }
  return isValidThoughtMaterializedRecord(record, namespace) ? record : null
}

function stableValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stableValue)
  if (!isRecord(value)) return value
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stableValue(value[key])]))
}

function stableFingerprint(value: unknown): string {
  try {
    return JSON.stringify(stableValue(value)) ?? ''
  } catch {
    return ''
  }
}

function thoughtConflict(
  namespace: string,
  winner: ThoughtMaterializedRecord,
  loser: ThoughtMaterializedRecord,
  reason: ThoughtConflictRecord['reason'],
): ThoughtConflictRecord | null {
  const winnerFingerprint = stableFingerprint(winner)
  const loserFingerprint = stableFingerprint(loser)
  if (!winnerFingerprint || !loserFingerprint || winnerFingerprint === loserFingerprint) return null
  return {
    key: [
      THOUGHT_CONFLICT_KIND,
      namespace,
      winner.annotationId,
      winner.serverSequence,
      loserFingerprint,
    ],
    kind: THOUGHT_CONFLICT_KIND,
    namespace,
    annotationId: winner.annotationId,
    serverSequence: winner.serverSequence,
    winner,
    loser,
    winnerFingerprint,
    loserFingerprint,
    reason,
    detectedAt: Date.now(),
    ...(winner.retainedUntil === undefined ? {} : { retainedUntil: winner.retainedUntil }),
  }
}

async function writeRemotePage(
  lease: IdentityLease,
  items: readonly unknown[],
  expectedCursor: string,
  cursor: string,
  retentionCutoff?: number,
  pullInProgress = false,
): Promise<UserDataTransactionResult<{ readonly stored: number; readonly cursor: string; readonly advanced: boolean }>> {
  const namespace = lease.context.physicalNamespace
  const records = items.map((item) => materializedRecord(namespace, item))
  if (records.some((record) => record === null)) return { ok: false }
  const materializedRecords = records as ThoughtMaterializedRecord[]
  return runUserDataTransaction(
    lease,
    'store remote thoughts',
    [THOUGHT_MATERIALIZED_STORE, THOUGHT_SYNC_STATE_STORE, ANNOTATION_IMPORTS_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const materialized = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
      const imports = transaction.objectStore(ANNOTATION_IMPORTS_STORE)
      const existingRequest = materialized.index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const stateRequest = stateStore.get(namespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      let existingDone = false
      let stateDone = false
      const finish = () => {
        if (!existingDone || !stateDone) return
        if (!lease.isCurrent(identity)) { transaction.abort(); return }
        const state = stateRequest.result
        if (!state || !isSyncState(state, namespace)) { transaction.abort(); return }
        // A second browsing context may have completed a later page while
        // this request was in flight. Check before queuing any materialized
        // writes so a stale page cannot commit without its cursor progress.
        if (state.cursor !== expectedCursor) {
          setResult({ stored: 0, cursor: state.cursor, advanced: false })
          return
        }
        const existingByID = new Map<string, ThoughtMaterializedRecord>()
        for (const raw of existingRequest.result) {
          if (!isValidThoughtMaterializedRecord(raw, namespace)) {
            transaction.abort()
            return
          }
          existingByID.set(raw.annotationId, raw)
        }
        for (const record of materializedRecords) {
          const existing = existingByID.get(record.annotationId)
          if (!existing) {
            materialized.put(record)
            existingByID.set(record.annotationId, record)
            continue
          }
          // A replay page may arrive out of order or be repeated.  The
          // materialized server cache is ordered solely by the server replay
          // sequence; Lamport keys remain payload metadata, not a second
          // server ordering rule.
          if (existing.serverSequence > record.serverSequence) {
            const conflict = thoughtConflict(namespace, existing, record, 'older-server-version')
            if (conflict) imports.put(conflict)
            continue
          }
          if (existing.serverSequence < record.serverSequence) {
            const conflict = thoughtConflict(namespace, record, existing, 'older-server-version')
            if (conflict) imports.put(conflict)
            materialized.put(record)
            existingByID.set(record.annotationId, record)
            continue
          }
          const existingFingerprint = stableFingerprint(existing)
          const recordFingerprint = stableFingerprint(record)
          if (existingFingerprint === recordFingerprint) continue
          // One immutable operation key cannot describe two projections. A
          // fingerprint fallback would introduce an ordering rule outside the
          // frozen Lamport tuple, so fail the whole page closed.
          transaction.abort()
          return
        }
        const lastServerSequence = materializedRecords.reduce(
          (highest, record) => Math.max(highest, record.serverSequence),
          state.lastServerSequence ?? 0,
        )
        const nextRetentionCutoff = retentionCutoff === undefined
          ? state.retentionCutoff
          : Math.max(state.retentionCutoff ?? 0, retentionCutoff)
        try {
          const logicalClockFloor = maximumThoughtClock([
            state.logicalClockFloor ?? 0,
            ...materializedRecords.map((record) => record.winnerKey.logicalClock),
          ])
          const stateWithoutPullInProgress = { ...state }
          delete stateWithoutPullInProgress.pullInProgress
          stateStore.put({
            ...stateWithoutPullInProgress,
            cursor,
            clockContractVersion: THOUGHT_CONTRACT_VERSION,
            logicalClockFloor,
            lastServerSequence,
            ...(pullInProgress ? { pullInProgress: true } : {}),
            ...(nextRetentionCutoff === undefined
              ? {}
              : { retentionCutoff: nextRetentionCutoff }),
            updatedAt: Date.now(),
          })
          setResult({ stored: items.length, cursor, advanced: true })
        } catch {
          transaction.abort()
        }
      }
      existingRequest.onerror = () => transaction.abort()
      existingRequest.onsuccess = () => {
        existingDone = true
        finish()
      }
      stateRequest.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => {
        stateDone = true
        finish()
      }
    },
  )
}

async function markPullFailure(
  lease: IdentityLease,
  error: ApiError,
  expectedCursor: string,
): Promise<UserDataTransactionResult<number | undefined>> {
  return runUserDataTransaction(
    lease,
    'record thought pull failure',
    [THOUGHT_OUTBOX_STORE, THOUGHT_HISTORY_OUTBOX_STORE, THOUGHT_REPAIR_SOURCE_STORE, THOUGHT_SYNC_STATE_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const namespace = lease.context.physicalNamespace
      const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const stateRequest = stateStore.get(namespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const historyRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyDone = false
      let sourceDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyDone || !sourceDone) return
        const state = stateRequest.result
        const outbox = liveOutboxRows(outboxRequest.result, sourceRequest.result, namespace)
        const history = activeHistoryOutboxRows(historyRequest.result, namespace)
        if (!state || !isSyncState(state, namespace) || !outbox || history === null) {
          transaction.abort()
          return
        }
        if (state.cursor !== expectedCursor) {
          setResult(stateRetryAt([...outbox, ...history], state))
          return
        }
        const now = Date.now()
        const pullAttemptCount = (state.pullAttemptCount ?? 0) + 1
        const pullRetryAt = now + retryDelay(error, pullAttemptCount)
        const retryAt = earliestRetryTime(earliestRetryAt([...outbox, ...history]), pullRetryAt)
        const code = failureCode(error)
        stateStore.put({
          ...state,
          pullAttemptCount,
          pullRetryAt,
          pullLastError: code,
          retryAt,
          lastErrorCode: code,
          updatedAt: now,
        })
        setResult(retryAt)
      }
      stateRequest.onerror = () => transaction.abort()
      outboxRequest.onerror = () => transaction.abort()
      historyRequest.onerror = () => transaction.abort()
      sourceRequest.onerror = () => transaction.abort()
      stateRequest.onsuccess = () => { stateDone = true; finish() }
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      historyRequest.onsuccess = () => { historyDone = true; finish() }
      sourceRequest.onsuccess = () => { sourceDone = true; finish() }
    },
  )
}

async function resetCursor(lease: IdentityLease): Promise<UserDataTransactionResult<void>> {
  return runUserDataTransaction(
    lease,
    'reset thought cursor',
    [THOUGHT_OUTBOX_STORE, THOUGHT_HISTORY_OUTBOX_STORE, THOUGHT_REPAIR_SOURCE_STORE, THOUGHT_SYNC_STATE_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const store = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const request = store.get(lease.context.physicalNamespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const historyRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyDone = false
      let sourceDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyDone || !sourceDone) return
        const state = request.result
        const outbox = liveOutboxRows(
          outboxRequest.result,
          sourceRequest.result,
          lease.context.physicalNamespace,
        )
        const history = activeHistoryOutboxRows(
          historyRequest.result,
          lease.context.physicalNamespace,
        )
        if (!state || !isSyncState(state, lease.context.physicalNamespace) ||
          !outbox || history === null) {
          transaction.abort()
          return
        }
        store.put({
          ...state,
          cursor: '',
          pullInProgress: true,
          resyncRequired: true,
          pullAttemptCount: undefined,
          pullRetryAt: undefined,
          pullLastError: undefined,
          retryAt: earliestRetryAt([...outbox, ...history]),
          updatedAt: Date.now(),
          lastError: undefined,
          lastErrorCode: undefined,
        })
        setResult(undefined)
      }
      request.onsuccess = () => {
        stateDone = true
        finish()
      }
      request.onerror = () => transaction.abort()
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      outboxRequest.onerror = () => transaction.abort()
      historyRequest.onsuccess = () => { historyDone = true; finish() }
      historyRequest.onerror = () => transaction.abort()
      sourceRequest.onsuccess = () => { sourceDone = true; finish() }
      sourceRequest.onerror = () => transaction.abort()
    },
  )
}

async function markPullComplete(
  lease: IdentityLease,
  expectedCursor: string,
): Promise<UserDataTransactionResult<void>> {
  return runUserDataTransaction(
    lease,
    'complete thought replay',
    [THOUGHT_OUTBOX_STORE, THOUGHT_HISTORY_OUTBOX_STORE, THOUGHT_REPAIR_SOURCE_STORE, THOUGHT_SYNC_STATE_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const store = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
      const request = store.get(lease.context.physicalNamespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const historyRequest = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
        .index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      const sourceRequest = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
        .getAll() as IDBRequest<unknown[]>
      let stateDone = false
      let outboxDone = false
      let historyDone = false
      let sourceDone = false
      const finish = () => {
        if (!stateDone || !outboxDone || !historyDone || !sourceDone) return
        const state = request.result
        const outbox = liveOutboxRows(
          outboxRequest.result,
          sourceRequest.result,
          lease.context.physicalNamespace,
        )
        const history = activeHistoryOutboxRows(
          historyRequest.result,
          lease.context.physicalNamespace,
        )
        if (!state || !isSyncState(state, lease.context.physicalNamespace) ||
          !outbox || history === null) {
          transaction.abort()
          return
        }
        if (state.cursor !== expectedCursor) {
          setResult(undefined)
          return
        }
        const stateWithoutPullInProgress = { ...state }
        delete stateWithoutPullInProgress.pullInProgress
        const nextState = { ...stateWithoutPullInProgress, resyncRequired: false, updatedAt: Date.now() }
        delete nextState.pullAttemptCount
        delete nextState.pullRetryAt
        delete nextState.pullLastError
        const remaining = [...outbox, ...history]
        const retryAt = earliestRetryAt(remaining)
        const blockedCode = blockedFailureCode(remaining)
        const fullySynced = remaining.length === 0
        store.put({
          ...nextState,
          ...(retryAt === undefined
            ? {
                retryAt: undefined,
                lastError: undefined,
                ...(blockedCode === undefined
                  ? { lastErrorCode: undefined }
                  : { lastErrorCode: blockedCode }),
                ...(fullySynced ? { lastSuccessfulSyncAt: Date.now() } : {}),
              }
            : { retryAt }),
        })
        setResult(undefined)
      }
      request.onsuccess = () => {
        stateDone = true
        finish()
      }
      request.onerror = () => transaction.abort()
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      outboxRequest.onerror = () => transaction.abort()
      historyRequest.onsuccess = () => { historyDone = true; finish() }
      historyRequest.onerror = () => transaction.abort()
      sourceRequest.onsuccess = () => { sourceDone = true; finish() }
      sourceRequest.onerror = () => transaction.abort()
    },
  )
}

function isCursorRetentionError(error: ApiError): boolean {
  return error.status === 410 ||
    error.errorCode === 'invalid_cursor' ||
    error.errorCode === 'cursor_expired' ||
    error.errorCode === 'retention_cutoff' ||
    error.errorCode === 'resync_required'
}

async function pullThoughts(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<{
  pulled: number
  cursor: string
  error?: string
  failure?: ApiError
  retryAt?: number
  /** The per-round page budget ended after persisting a resumable cursor. */
  incomplete?: boolean
  stale?: boolean
}> {
  const stateResult: UserDataTransactionResult<ThoughtSyncStateRecord | undefined> =
    await runUserDataTransaction<ThoughtSyncStateRecord | undefined>(
    lease,
    'read thought cursor',
    [THOUGHT_SYNC_STATE_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const request = transaction.objectStore(THOUGHT_SYNC_STATE_STORE).get(lease.context.physicalNamespace) as IDBRequest<ThoughtSyncStateRecord | undefined>
      request.onsuccess = () => setResult(request.result)
      request.onerror = () => transaction.abort()
    },
  )
  if (!stateResult.ok) {
    return lease.isCurrent(lease.capture('thought pull after cursor read'))
      ? {
          pulled: 0,
          cursor: '',
          error: '无法读取想法同步游标',
          failure: { kind: 'other', message: '无法读取想法同步游标' },
        }
      : { pulled: 0, cursor: '', stale: true }
  }
  let cursor = stateResult.value?.cursor ?? ''
  const pullRetryAt = stateResult.value?.pullRetryAt
  if (pullRetryAt !== undefined && pullRetryAt > Date.now()) {
    return { pulled: 0, cursor, retryAt: pullRetryAt }
  }
  let pulled = 0
  let resetCount = 0
  for (let page = 0; page < MAX_PULL_PAGES_PER_ROUND; page += 1) {
    const operation = lease.capture(`pull thought page ${page}`)
    if (!lease.isCurrent(operation)) return { pulled, cursor, stale: true }
    let result: Awaited<ReturnType<IdentityBoundReaderClient['syncThoughts']>>
    try {
      result = await client.syncThoughts(
        { after: cursor, limit: MAX_BATCH },
        { signal: operation.signal },
      )
    } catch (error) {
      if (!lease.isCurrent(operation)) return { pulled, cursor, stale: true }
      return {
        pulled,
        cursor,
        error: error instanceof Error ? error.message : '想法拉取失败',
        failure: {
          kind: 'network-unreachable',
          message: error instanceof Error ? error.message : '想法拉取失败',
        },
      }
    }
    if (!lease.isCurrent(operation)) return { pulled, cursor, stale: true }
    if (!result.ok) {
      if (isCursorRetentionError(result.error) && cursor && resetCount === 0) {
        const reset = await resetCursor(lease)
        if (!reset.ok) {
          return lease.isCurrent(lease.capture('thought pull after cursor reset'))
            ? {
                pulled,
                cursor,
                error: '无法重置想法同步游标',
                failure: { kind: 'other', message: '无法重置想法同步游标' },
              }
            : { pulled, cursor, stale: true }
        }
        resetCount += 1
        cursor = ''
        continue
      }
      return result.error.kind === 'identity-mismatch'
        ? { pulled, cursor, stale: true }
        : { pulled, cursor, error: result.error.message, failure: result.error }
    }
    const pageData = result.data as unknown as Record<string, unknown>
    if (pageData.contract_version !== THOUGHT_CONTRACT_VERSION) {
      return {
        pulled,
        cursor,
        error: '远端想法契约版本不符',
        failure: { kind: 'other', message: '远端想法契约版本不符' },
      }
    }
    const rawItems = firstDefined(pageData, ['items', 'annotations', 'thoughts'])
    if (!Array.isArray(rawItems)) {
      return {
        pulled,
        cursor,
        error: '远端想法分页格式不符',
        failure: { kind: 'other', message: '远端想法分页格式不符' },
      }
    }
    const rawNextCursor = firstDefined(pageData, ['next_cursor', 'nextCursor', 'cursor'])
    if (rawNextCursor !== undefined && typeof rawNextCursor !== 'string') {
      return {
        pulled,
        cursor,
        error: '远端想法游标格式不符',
        failure: { kind: 'other', message: '远端想法游标格式不符' },
      }
    }
    const nextCursor = rawNextCursor ?? cursor
    if (rawNextCursor !== undefined && nextCursor === cursor && rawItems.length > 0) {
      return {
        pulled,
        cursor,
        error: '远端想法游标未前进',
        failure: { kind: 'other', message: '远端想法游标未前进' },
      }
    }
    const retentionValue = firstDefined(pageData, [
      'retention_cutoff',
      'retentionCutoff',
      'cutoff_sequence',
      'cutoffSequence',
    ])
    const retentionCutoff = positiveInteger(retentionValue) ?? undefined
    const hasMore = rawNextCursor !== undefined && rawItems.length > 0
    const stored = await writeRemotePage(
      lease,
      rawItems,
      cursor,
      nextCursor,
      retentionCutoff,
      hasMore,
    )
    if (!stored.ok) {
      return lease.isCurrent(lease.capture('thought pull after page replay write'))
        ? {
            pulled,
            cursor,
            error: '无法保存远端想法',
            failure: { kind: 'other', message: '无法保存远端想法' },
          }
        : { pulled, cursor, stale: true }
    }
    cursor = stored.value.cursor
    if (stored.value.advanced) pulled += stored.value.stored
    // A different context won the cursor race. Its durable cursor is the only
    // safe resume point; do not claim this round completed a stream it did not
    // own.
    if (!stored.value.advanced) return { pulled, cursor, incomplete: true }
    if (hasMore) continue

    const replayed = await markPullComplete(lease, cursor)
    if (!replayed.ok) {
      return lease.isCurrent(lease.capture('thought pull after replay completion'))
        ? {
            pulled,
            cursor,
            error: '无法完成想法全量重放',
            failure: { kind: 'other', message: '无法完成想法全量重放' },
          }
        : { pulled, cursor, stale: true }
    }
    return { pulled, cursor }
  }
  // Every preceding page advanced the durable cursor atomically. A later
  // controller round can continue from it, rather than re-reading or losing a
  // large backlog that exceeded the foreground page budget.
  return { pulled, cursor, incomplete: true }
}

interface PushDueResult {
  readonly pushed: number
  /** Records no longer need another attempt in this round: acknowledged or blocked. */
  readonly completedIDs: readonly string[]
  readonly retryAt?: number
  readonly error?: ApiError
  readonly stale?: boolean
  /** A retryable request-level failure leaves later records untouched for a later round. */
  readonly halt: boolean
}

function mergePushDueResults(left: PushDueResult, right: PushDueResult): PushDueResult {
  const error = right.error ?? left.error
  return {
    pushed: left.pushed + right.pushed,
    completedIDs: [...left.completedIDs, ...right.completedIDs],
    retryAt: earliestRetryTime(left.retryAt, right.retryAt),
    ...(error === undefined ? {} : { error }),
    stale: left.stale || right.stale || undefined,
    halt: left.halt || right.halt,
  }
}

async function recordPushFailure(
  lease: IdentityLease,
  records: readonly CompleteThoughtOutboxRecord[],
  error: ApiError,
): Promise<PushDueResult> {
  const marked = await markPushFailure(lease, THOUGHT_OUTBOX_STORE, records, error)
  if (!marked.ok) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  const permanent = isPermanentPushFailure(error)
  return {
    pushed: 0,
    completedIDs: permanent ? records.map((record) => record.opId) : [],
    retryAt: marked.value,
    error,
    halt: !permanent,
  }
}

/**
 * A request-level permanent 4xx has no per-operation rejection payload. Split
 * it until the poison record is singular, then quarantine only that record.
 * Valid siblings still receive their normal acknowledgement in the same round.
 */
async function pushDueOperations(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
  records: readonly CompleteThoughtOutboxRecord[],
  label: string,
): Promise<PushDueResult> {
  if (records.length === 0) return { pushed: 0, completedIDs: [], halt: false }
  const operation = lease.capture(label)
  if (!lease.isCurrent(operation)) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  let result: Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
  try {
    result = await client.pushThoughtOps(
      { ops: records.map(toWireOperation) },
      { signal: operation.signal },
    )
  } catch (error) {
    if (!lease.isCurrent(operation)) {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    return recordPushFailure(lease, records, {
      kind: 'network-unreachable',
      message: error instanceof Error ? error.message : '想法推送失败',
    })
  }
  if (!lease.isCurrent(operation)) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  if (!result.ok) {
    if (result.error.kind === 'identity-mismatch') {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    if (isIsolatablePermanentPushFailure(result.error) && records.length > 1) {
      const midpoint = Math.ceil(records.length / 2)
      const left = await pushDueOperations(lease, client, records.slice(0, midpoint), `${label} left`)
      if (left.stale || left.halt) return left
      const right = await pushDueOperations(lease, client, records.slice(midpoint), `${label} right`)
      return mergePushDueResults(left, right)
    }
    return recordPushFailure(lease, records, result.error)
  }
  const acknowledged = await acknowledge(lease, THOUGHT_OUTBOX_STORE, records, result.data)
  if (!acknowledged.ok) {
    if (!lease.isCurrent(lease.capture(`${label} after acknowledgement`))) {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    return recordPushFailure(lease, records, {
      kind: 'other',
      message: '无法保存想法同步确认',
    })
  }
  return {
    pushed: acknowledged.value.length,
    completedIDs: result.data.map((ack) => ack.op_id),
    halt: false,
  }
}

async function recordHistoryPushFailure(
  lease: IdentityLease,
  records: readonly ThoughtHistoryOutboxRecord[],
  error: ApiError,
): Promise<PushDueResult> {
  const marked = await markPushFailure(lease, THOUGHT_HISTORY_OUTBOX_STORE, records, error)
  if (!marked.ok) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  const permanent = isPermanentPushFailure(error)
  return {
    pushed: 0,
    completedIDs: permanent ? records.map((record) => record.opId) : [],
    retryAt: marked.value,
    error,
    halt: !permanent,
  }
}

async function pushDueHistoryOperations(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
  records: readonly ThoughtHistoryOutboxRecord[],
  label: string,
): Promise<PushDueResult> {
  if (records.length === 0) return { pushed: 0, completedIDs: [], halt: false }
  const operation = lease.capture(label)
  if (!lease.isCurrent(operation)) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  let result: Awaited<ReturnType<IdentityBoundReaderClient['pushThoughtOps']>>
  try {
    result = await client.pushThoughtOps(
      { ops: records.map(toHistoryWireOperation) },
      { signal: operation.signal },
    )
  } catch (error) {
    if (!lease.isCurrent(operation)) {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    return recordHistoryPushFailure(lease, records, {
      kind: 'network-unreachable',
      message: error instanceof Error ? error.message : '历史想法推送失败',
    })
  }
  if (!lease.isCurrent(operation)) {
    return { pushed: 0, completedIDs: [], stale: true, halt: true }
  }
  if (!result.ok) {
    if (result.error.kind === 'identity-mismatch') {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    if (isIsolatablePermanentPushFailure(result.error) && records.length > 1) {
      const midpoint = Math.ceil(records.length / 2)
      const left = await pushDueHistoryOperations(
        lease,
        client,
        records.slice(0, midpoint),
        `${label} left`,
      )
      if (left.stale || left.halt) return left
      const right = await pushDueHistoryOperations(
        lease,
        client,
        records.slice(midpoint),
        `${label} right`,
      )
      return mergePushDueResults(left, right)
    }
    return recordHistoryPushFailure(lease, records, result.error)
  }
  const acknowledged = await acknowledge(
    lease,
    THOUGHT_HISTORY_OUTBOX_STORE,
    records,
    result.data,
  )
  if (!acknowledged.ok) {
    if (!lease.isCurrent(lease.capture(`${label} after acknowledgement`))) {
      return { pushed: 0, completedIDs: [], stale: true, halt: true }
    }
    return recordHistoryPushFailure(lease, records, {
      kind: 'other',
      message: '无法保存历史想法同步确认',
    })
  }
  return {
    pushed: acknowledged.value.length,
    // A superseded command is durably blocked rather than deleted, but it is
    // complete for this round and must never be selected again immediately.
    completedIDs: result.data.map((ack) => ack.op_id),
    halt: false,
  }
}

async function performSync(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<ThoughtSyncResult> {
  const prepared = await prepareState(lease)
  if (!prepared.ok) return { status: 'stale', pushed: 0, pulled: 0, cursor: '', pending: 0 }
  let pushed = 0
  let pending = prepared.value.outbox.length + prepared.value.historyOutbox.length
  let remaining = [...prepared.value.outbox]
  let remainingHistory = [...prepared.value.historyOutbox]
  if (isOffline()) {
    return {
      status: 'idle',
      pushed: 0,
      pulled: 0,
      cursor: prepared.value.state.cursor,
      pending,
      retryAt: stateRetryAt([...remaining, ...remainingHistory], prepared.value.state),
    }
  }
  const initialRetryAt = stateRetryAt([...remaining, ...remainingHistory], prepared.value.state)
  // `syncThoughts()` is also a public entrypoint. Keep it behind the same
  // durable cooldown as the controller so its pull request cannot become a
  // side channel around a server-provided Retry-After.
  if (initialRetryAt !== undefined && initialRetryAt > Date.now()) {
    return {
      status: 'idle',
      pushed: 0,
      pulled: 0,
      cursor: prepared.value.state.cursor,
      pending,
      retryAt: initialRetryAt,
    }
  }
  for (let batchIndex = 0; batchIndex < MAX_PUSH_BATCHES && remaining.length > 0; batchIndex += 1) {
    const now = Date.now()
    const due = remaining
      .filter(isThoughtOutboxDispatchable)
      .filter((record) => record.nextAttemptAt === undefined || record.nextAttemptAt <= now)
      .slice(0, MAX_BATCH)
    if (due.length === 0) break
    const outcome = await pushDueOperations(lease, client, due, `push thought batch ${batchIndex}`)
    if (outcome.stale) {
      return { status: 'stale', pushed, pulled: 0, cursor: prepared.value.state.cursor, pending }
    }
    pushed += outcome.pushed
    const completedIDs = new Set(outcome.completedIDs)
    remaining = remaining.filter((record) =>
      typeof record.opId !== 'string' || !completedIDs.has(record.opId))
    pending = remaining.length + remainingHistory.length
    if (outcome.halt) {
      return {
        status: 'failed',
        pushed,
        pulled: 0,
        cursor: prepared.value.state.cursor,
        pending,
        retryAt: outcome.retryAt,
        ...(outcome.error === undefined ? {} : { errorCode: failureCode(outcome.error) }),
      }
    }
  }

  // A request-level permanent error may have been split until only its poison
  // singleton remains. Its valid siblings have already been acknowledged in
  // this round; do not hide the durable blocked outcome behind an unrelated
  // pull request.
  const afterPush = await prepareState(lease)
  if (!afterPush.ok || !lease.isCurrent(lease.capture('summarize blocked thought operations'))) {
    return { status: 'stale', pushed, pulled: 0, cursor: prepared.value.state.cursor, pending }
  }
  const postPushBlockedCode = blockedFailureCode(afterPush.value.outbox)
  if (postPushBlockedCode !== undefined) {
    const afterPushRecords = [...afterPush.value.outbox, ...afterPush.value.historyOutbox]
    const retryAt = stateRetryAt(afterPushRecords, afterPush.value.state)
    return {
      status: 'failed',
      pushed,
      pulled: 0,
      cursor: afterPush.value.state.cursor,
      pending: afterPushRecords.length,
      ...(retryAt === undefined ? {} : { retryAt }),
      errorCode: storedFailureCode(afterPush.value.state.lastErrorCode) ?? postPushBlockedCode,
    }
  }

  remainingHistory = [...afterPush.value.historyOutbox]
  for (let batchIndex = 0;
    batchIndex < MAX_PUSH_BATCHES && remainingHistory.length > 0;
    batchIndex += 1) {
    const now = Date.now()
    const due = remainingHistory
      .filter((record) => record.status !== 'blocked')
      .filter((record) => record.nextAttemptAt === undefined || record.nextAttemptAt <= now)
      .slice(0, MAX_BATCH)
    if (due.length === 0) break
    const outcome = await pushDueHistoryOperations(
      lease,
      client,
      due,
      `push thought history batch ${batchIndex}`,
    )
    if (outcome.stale) {
      return { status: 'stale', pushed, pulled: 0, cursor: prepared.value.state.cursor, pending }
    }
    pushed += outcome.pushed
    const completedIDs = new Set(outcome.completedIDs)
    remainingHistory = remainingHistory.filter((record) => !completedIDs.has(record.opId))
    pending = remaining.length + remainingHistory.length
    if (outcome.halt) {
      return {
        status: 'failed',
        pushed,
        pulled: 0,
        cursor: prepared.value.state.cursor,
        pending,
        retryAt: outcome.retryAt,
        ...(outcome.error === undefined ? {} : { errorCode: failureCode(outcome.error) }),
      }
    }
  }

  const beforePull = await prepareState(lease)
  if (!beforePull.ok || !lease.isCurrent(lease.capture('summarize thought queues before pull'))) {
    return { status: 'stale', pushed, pulled: 0, cursor: prepared.value.state.cursor, pending }
  }
  const beforePullRecords = [...beforePull.value.outbox, ...beforePull.value.historyOutbox]
  pending = beforePullRecords.length
  const beforePullBlockedCode = blockedFailureCode(beforePullRecords)
  if (beforePullBlockedCode !== undefined) {
    const retryAt = stateRetryAt(beforePullRecords, beforePull.value.state)
    return {
      status: 'failed',
      pushed,
      pulled: 0,
      cursor: beforePull.value.state.cursor,
      pending,
      ...(retryAt === undefined ? {} : { retryAt }),
      errorCode: storedFailureCode(beforePull.value.state.lastErrorCode) ?? beforePullBlockedCode,
    }
  }

  const pulledResult = await pullThoughts(lease, client)
  if (pulledResult.stale) {
    return { status: 'stale', pushed, pulled: pulledResult.pulled, cursor: pulledResult.cursor, pending }
  }
  if (pulledResult.error) {
    const failure = pulledResult.failure ?? {
      kind: 'other' as const,
      message: pulledResult.error,
    }
    const marked = await markPullFailure(lease, failure, pulledResult.cursor)
    if (!marked.ok) {
      return lease.isCurrent(lease.capture('thought pull failure after state write'))
        ? {
            status: 'failed',
            pushed,
            pulled: pulledResult.pulled,
            cursor: pulledResult.cursor,
            pending,
            errorCode: failureCode(failure),
          }
        : { status: 'stale', pushed, pulled: pulledResult.pulled, cursor: pulledResult.cursor, pending }
    }
    return {
      status: 'failed',
      pushed,
      pulled: pulledResult.pulled,
      cursor: pulledResult.cursor,
      pending,
      retryAt: marked.value,
      errorCode: failureCode(failure),
    }
  }
  const finalPrepared = await prepareState(lease)
  if (!finalPrepared.ok || !lease.isCurrent(lease.capture('complete thought sync summary'))) {
    return { status: 'stale', pushed, pulled: pulledResult.pulled, cursor: pulledResult.cursor, pending }
  }
  const finalRecords = [...finalPrepared.value.outbox, ...finalPrepared.value.historyOutbox]
  const finalPending = finalRecords.length
  const finalRetryAt = stateRetryAt(finalRecords, finalPrepared.value.state)
  const blockedCode = blockedFailureCode(finalRecords)
  const errorCode = storedFailureCode(finalPrepared.value.state.lastErrorCode) ?? blockedCode
  if (lease.isCurrent(lease.capture('thought sync event'))) {
    emitReaderEvent(READER_EVENTS.thoughtsSynced)
  }
  if (blockedCode !== undefined || finalRetryAt !== undefined) {
    return {
      status: 'failed',
      pushed,
      pulled: pulledResult.pulled,
      cursor: finalPrepared.value.state.cursor,
      pending: finalPending,
      retryAt: finalRetryAt,
      ...(errorCode === undefined ? {} : { errorCode }),
    }
  }
  return {
    status: finalPending === 0 && !finalPrepared.value.state.pullInProgress &&
      (pushed > 0 || pulledResult.pulled > 0) ? 'synced' : 'idle',
    pushed,
    pulled: pulledResult.pulled,
    cursor: finalPrepared.value.state.cursor,
    pending: finalPending,
    retryAt: finalRetryAt,
  }
}

async function withCrossTabLock(
  lease: IdentityLease,
  callback: () => Promise<ThoughtSyncResult>,
): Promise<SyncExecution> {
  const execute = async (): Promise<SyncExecution> => {
    const before = await readSyncDurableFingerprint(lease)
    const result = await callback()
    const after = await readSyncDurableFingerprint(lease)
    // A materialized page can replace a prior winner without changing any
    // outbox tuple. `pulled` records that page-level durable work separately.
    const durableChanged = result.pulled > 0 ||
      (!before.ok || !after.ok
        ? result.status !== 'stale'
        : before.value !== after.value)
    return { result, ran: true, durableChanged }
  }
  const lockName = `webtag-reader-thought-sync:${lease.context.physicalNamespace}`
  const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined
  if (!locks) return execute()
  let execution: SyncExecution | undefined
  await locks.request(lockName, { ifAvailable: true }, async (lock) => {
    if (!lock) return
    execution = await execute()
  })
  if (execution !== undefined) return execution
  // This caller did not own the lock, so it must not mutate or broadcast.
  // The controller will also reread on the lock holder's one-way `sync` hint.
  const summary = await readPreparedState(lease)
  if (!summary.ok) {
    return { result: staleSyncResult(), ran: false, durableChanged: false }
  }
  if (summary.value === undefined) {
    return {
      result: { status: 'idle', pushed: 0, pulled: 0, cursor: '', pending: 0 },
      ran: false,
      durableChanged: false,
    }
  }
  return { result: idleSyncResult(summary.value), ran: false, durableChanged: false }
}

function runThoughtSyncExecution(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<SyncExecution> {
  const existing = inFlight.get(lease)
  if (existing) return existing
  const pending = withCrossTabLock(lease, async () => {
    const synced = await performSync(lease, client)
    if (synced.status === 'stale' || synced.status === 'failed') return synced
    // Older embedded Readers can still retain their local durable queue. They
    // do not receive an event page until their client adapter is upgraded.
    if (typeof (client as Partial<IdentityBoundReaderClient>).listThoughtSupersessions !== 'function') {
      return synced
    }
    const supersessions = await syncThoughtSupersessions(lease, client)
    if (supersessions.status === 'stale') {
      return { ...synced, status: 'stale' }
    }
    if (supersessions.status === 'failed') {
      return { ...synced, status: 'failed', error: supersessions.error ?? '无法同步被取代版本' }
    }
    return supersessions.pulled > 0 ? { ...synced, status: 'synced' } : synced
  })
  inFlight.set(lease, pending)
  void pending.then(() => {
    if (inFlight.get(lease) === pending) inFlight.delete(lease)
  }, () => {
    if (inFlight.get(lease) === pending) inFlight.delete(lease)
  })
  return pending
}

/** Pushes local annotation operations and replays server thoughts once. */
export function syncThoughts(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): Promise<ThoughtSyncResult> {
  return runThoughtSyncExecution(lease, client).then(({ result }) => result)
}

interface ThoughtSyncInvalidation {
  readonly kind: 'thought-sync-invalidation'
  readonly namespace: string
  readonly invalidation: 'outbox' | 'sync'
}

interface ThoughtRepairQuarantineHint {
  readonly kind: 'thought-repair-quarantine'
  readonly namespace: string
  readonly reasons: readonly { readonly reason: string; readonly count: number }[]
}

function parseThoughtRepairQuarantineHint(value: unknown): ThoughtRepairQuarantineHint | null {
  if (!isRecord(value) || value.kind !== 'thought-repair-quarantine' ||
    !isNonEmptyString(value.namespace) || !Array.isArray(value.reasons) || value.reasons.length === 0) return null
  const reasons: Array<{ reason: string; count: number }> = []
  for (const item of value.reasons) {
    if (!isRecord(item) || !isNonEmptyString(item.reason) ||
      !Number.isSafeInteger(item.count) || (item.count as number) <= 0) return null
    reasons.push({ reason: item.reason, count: item.count as number })
  }
  return { kind: 'thought-repair-quarantine', namespace: value.namespace, reasons }
}

function parseThoughtSyncInvalidation(value: unknown): ThoughtSyncInvalidation | null {
  if (!isRecord(value)) return null
  if (
    value.kind !== 'thought-sync-invalidation' ||
    !isNonEmptyString(value.namespace) ||
    (value.invalidation !== 'outbox' && value.invalidation !== 'sync')
  ) return null
  return {
    kind: 'thought-sync-invalidation',
    namespace: value.namespace,
    invalidation: value.invalidation,
  }
}

function staleSyncResult(): ThoughtSyncResult {
  return { status: 'stale', pushed: 0, pulled: 0, cursor: '', pending: 0 }
}

function idleSyncResult(prepared: PreparedState): ThoughtSyncResult {
  const records = [...prepared.outbox, ...prepared.historyOutbox]
  return {
    status: 'idle',
    pushed: 0,
    pulled: 0,
    cursor: prepared.state.cursor,
    pending: records.length,
    retryAt: stateRetryAt(records, prepared.state),
  }
}

function snapshotFromPrepared(
  prepared: PreparedState,
  syncing: boolean,
): ThoughtSyncSnapshot {
  const records = [...prepared.outbox, ...prepared.historyOutbox]
  const pendingCount = records.length
  const blockedCount = records.filter((record) => record.status === 'blocked').length
  const retryAt = stateRetryAt(records, prepared.state)
  // Older durable rows predate lastErrorCode. Treat legacy text only as a
  // boolean signal. A blocked row remains a durable failure even after a
  // later push/pull succeeds, so use its sanitized classification as fallback.
  const errorCode = storedFailureCode(prepared.state.lastErrorCode) ??
    blockedFailureCode(records) ??
    (prepared.state.lastError || prepared.state.pullLastError ? 'sync-failed' : undefined)
  let phase: ThoughtSyncPhase
  if (isOffline()) phase = 'offline'
  else if (syncing) phase = 'syncing'
  else if (blockedCount > 0 || errorCode !== undefined) phase = 'failed'
  else if (prepared.state.pullInProgress) phase = 'pending'
  else if (pendingCount > 0) phase = 'pending'
  else if (prepared.state.lastSuccessfulSyncAt !== undefined) phase = 'synced'
  // A newly mounted namespace has not performed a genuine push/pull round
  // yet. Do not claim it is synced before that first result arrives.
  else phase = 'syncing'
  return Object.freeze({
    phase,
    pendingCount,
    blockedCount,
    ...(retryAt === undefined ? {} : { retryAt }),
    ...(prepared.state.lastSuccessfulSyncAt === undefined
      ? {}
      : { lastSuccessfulSyncAt: prepared.state.lastSuccessfulSyncAt }),
    ...(errorCode === undefined ? {} : { errorCode }),
  })
}

class NamespaceThoughtSyncController implements ThoughtSyncController {
  private snapshot: ThoughtSyncSnapshot = EMPTY_THOUGHT_SYNC_SNAPSHOT
  private readonly listeners = new Set<() => void>()
  private running: Promise<ThoughtSyncResult> | null = null
  private wakeAfterCurrentRun = false
  private lifecycleOwners = 0
  private timer: number | null = null
  private channel: BroadcastChannel | null = null
  private unsubscribeReaderEvents: (() => void)[] = []
  private repairFingerprint = ''
  private repairFingerprintUntil = 0
  private disposed = false
  private readonly leaseSignal: AbortSignal
  private readonly onLeaseRevoked = (): void => { this.dispose() }

  constructor(
    private readonly lease: IdentityLease,
    private readonly client: IdentityBoundReaderClient,
  ) {
    this.leaseSignal = lease.capture('observe thought sync lease').signal
    this.leaseSignal.addEventListener('abort', this.onLeaseRevoked, { once: true })
    if (this.leaseSignal.aborted) this.dispose()
  }

  getSnapshot(): ThoughtSyncSnapshot {
    return this.snapshot
  }

  subscribe(listener: () => void): () => void {
    if (this.disposed) return () => undefined
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  start(): () => void {
    if (this.disposed) return () => undefined
    this.lifecycleOwners += 1
    if (this.lifecycleOwners === 1) this.installLifecycle()
    let released = false
    return () => {
      if (released) return
      released = true
      if (this.disposed) return
      this.lifecycleOwners -= 1
      if (this.lifecycleOwners === 0) this.removeLifecycle()
    }
  }

  sync(): Promise<ThoughtSyncResult> {
    if (this.disposed) return Promise.resolve(staleSyncResult())
    if (this.running) return this.running
    const run = this.run()
    const settled = run.then(
      async (result) => {
        await this.completeRun(settled)
        return result
      },
      async (error: unknown) => {
        await this.completeRun(settled)
        throw error
      },
    )
    this.running = settled
    this.publishSnapshot({ ...this.snapshot, phase: isOffline() ? 'offline' : 'syncing' })
    return settled
  }

  private current(logicalKey: string): boolean {
    return !this.disposed && this.lease.isCurrent(this.lease.capture(logicalKey))
  }

  private publishSnapshot(next: ThoughtSyncSnapshot): void {
    if (this.snapshot === next) return
    this.snapshot = Object.freeze(next)
    for (const listener of [...this.listeners]) listener()
  }

  private async refreshSnapshot(): Promise<PreparedState | undefined | null> {
    if (this.disposed) return null
    const prepared = await readPreparedState(this.lease)
    if (!prepared.ok || !this.current('refresh thought sync snapshot')) return null
    if (prepared.value !== undefined) {
      this.publishSnapshot(snapshotFromPrepared(prepared.value, this.running !== null))
    }
    return prepared.value
  }

  private async run(): Promise<ThoughtSyncResult> {
    const prepared = await this.refreshSnapshot()
    if (prepared === null || !this.current('start thought sync controller run')) return staleSyncResult()
    if (isOffline()) {
      return prepared === undefined
        ? { status: 'idle', pushed: 0, pulled: 0, cursor: '', pending: 0 }
        : idleSyncResult(prepared)
    }
    const retryAt = prepared === undefined ? undefined : stateRetryAt(prepared.outbox, prepared.state)
    // Retry-After is a service contract, not a best-effort hint. A manual
    // titlebar command joins this controller but cannot punch through it.
    if (prepared !== undefined && retryAt !== undefined && retryAt > Date.now()) {
      return idleSyncResult(prepared)
    }
    try {
      const execution = await runThoughtSyncExecution(this.lease, this.client)
      if (!this.current('complete thought sync controller run')) return staleSyncResult()
      if (!execution.ran) {
        // `ifAvailable` lock losers do a read-only immediate snapshot refresh.
        // The holder broadcasts a later `sync` invalidation once its durable
        // transaction work has settled, which refreshes materialized rows too.
        const finalPrepared = await this.refreshSnapshot()
        if (finalPrepared === null || !this.current('refresh lock-losing sync snapshot')) {
          return staleSyncResult()
        }
      } else if (execution.durableChanged) {
        this.publishInvalidation('sync')
      }
      return execution.result
    } catch {
      // syncThoughts normally resolves an ApiResult-derived state. Keep an
      // unexpected adapter rejection observable without leaking its message.
      // It still becomes durable retry state, otherwise a later snapshot read
      // could incorrectly revive a previous "synced" presentation.
      const marked = await markPullFailure(
        this.lease,
        { kind: 'other', errorCode: 'sync-exception', message: '想法同步失败' },
        prepared?.state.cursor ?? '',
      )
      return {
        status: 'failed',
        pushed: 0,
        pulled: 0,
        cursor: prepared?.state.cursor ?? '',
        pending: prepared?.outbox.length ?? 0,
        retryAt: marked.ok ? marked.value : Date.now() + BASE_RETRY_MS,
        errorCode: 'other:sync-exception',
      }
    }
  }

  private async completeRun(run: Promise<ThoughtSyncResult>): Promise<void> {
    if (this.disposed || this.running !== run) return
    this.running = null
    if (!this.current('settle thought sync controller')) return
    const prepared = await this.refreshSnapshot()
    if (prepared === null || !this.current('schedule thought sync controller')) return
    if (this.wakeAfterCurrentRun) {
      this.wakeAfterCurrentRun = false
      if (this.lifecycleOwners > 0) {
        this.wake()
        return
      }
    }
    this.schedule(
      prepared?.state.pullInProgress === true &&
      (this.snapshot.retryAt === undefined || this.snapshot.retryAt <= Date.now()),
    )
    emitReaderEvent(READER_EVENTS.thoughtsSynced)
  }

  private schedule(immediateReplay = false): void {
    if (this.timer !== null) window.clearTimeout(this.timer)
    this.timer = null
    if (
      this.disposed ||
      this.lifecycleOwners === 0 ||
      !this.current('schedule thought sync controller timer') ||
      isOffline() ||
      document.visibilityState !== 'visible' ||
      this.running !== null
    ) return
    const delay = immediateReplay
      ? 0
      : this.snapshot.retryAt !== undefined && this.snapshot.retryAt > Date.now()
      ? this.snapshot.retryAt - Date.now()
      : THOUGHT_SYNC_POLL_MS
    this.timer = window.setTimeout(() => {
      this.timer = null
      void this.sync()
    }, Math.max(0, delay))
  }

  private installLifecycle(): void {
    if (this.disposed) return
    if (!this.current('start thought sync lifecycle')) return
    try {
      if (typeof BroadcastChannel !== 'undefined') {
        this.channel = new BroadcastChannel(THOUGHT_SYNC_CHANNEL_NAME)
        this.channel.addEventListener('message', this.onBroadcast)
      }
    } catch {
      this.channel = null
    }
    document.addEventListener('visibilitychange', this.onVisibility)
    window.addEventListener('online', this.onOnline)
    window.addEventListener('offline', this.onOffline)
    this.unsubscribeReaderEvents = [
      subscribeReaderEvents([READER_EVENTS.annotationsChanged], this.onLocalChange),
      subscribeReaderEvents([READER_EVENTS.thoughtsSyncRequested], this.onRequest),
    ]
    this.wake()
  }

  private removeLifecycle(): void {
    if (this.timer !== null) window.clearTimeout(this.timer)
    this.timer = null
    document.removeEventListener('visibilitychange', this.onVisibility)
    window.removeEventListener('online', this.onOnline)
    window.removeEventListener('offline', this.onOffline)
    for (const unsubscribe of this.unsubscribeReaderEvents) unsubscribe()
    this.unsubscribeReaderEvents = []
    if (this.channel) {
      this.channel.removeEventListener('message', this.onBroadcast)
      this.channel.close()
      this.channel = null
    }
    this.repairFingerprint = ''
    this.repairFingerprintUntil = 0
    this.wakeAfterCurrentRun = false
  }

  /**
   * Lifecycle wakes differ from a manual sync join: a durable change that
   * lands after this round selected its batch needs one immediate follow-up,
   * but repeated titlebar clicks must continue to share only one promise.
   */
  private wake(): void {
    if (this.disposed) return
    if (!this.current('wake thought sync controller')) return
    if (this.running) {
      this.wakeAfterCurrentRun = true
      return
    }
    if (isOffline() || document.visibilityState !== 'visible') {
      void this.refreshSnapshot().then(() => this.schedule())
      return
    }
    void this.sync()
  }

  private publishInvalidation(invalidation: ThoughtSyncInvalidation['invalidation']): void {
    if (!this.channel || !this.current('broadcast thought sync invalidation')) return
    try {
      this.channel.postMessage({
        kind: 'thought-sync-invalidation',
        namespace: this.lease.context.physicalNamespace,
        invalidation,
      } satisfies ThoughtSyncInvalidation)
    } catch {
      this.channel.removeEventListener('message', this.onBroadcast)
      this.channel.close()
      this.channel = null
    }
  }

  private readonly onBroadcast = (event: MessageEvent<unknown>): void => {
    const repair = parseThoughtRepairQuarantineHint(event.data)
    if (repair) {
      if (repair.namespace !== this.lease.context.physicalNamespace ||
        !this.current('receive thought repair quarantine')) return
      const fingerprint = JSON.stringify(repair)
      if (this.repairFingerprint === fingerprint && this.repairFingerprintUntil >= Date.now()) return
      this.repairFingerprint = fingerprint
      this.repairFingerprintUntil = Date.now() + REPAIR_BROADCAST_SUPPRESSION_MS
      this.wake()
      return
    }
    const hint = parseThoughtSyncInvalidation(event.data)
    if (
      !hint ||
      hint.namespace !== this.lease.context.physicalNamespace ||
      !this.current('receive thought sync invalidation')
    ) return
    if (hint.invalidation === 'sync') {
      this.refreshAfterRemoteSync()
      return
    }
    this.wake()
  }

  private readonly onVisibility = (): void => {
    if (document.visibilityState === 'visible') {
      this.wake()
    } else {
      this.schedule()
    }
  }

  private readonly onOnline = (): void => { this.wake() }

  private readonly onOffline = (): void => {
    if (this.disposed) return
    this.publishSnapshot({ ...this.snapshot, phase: 'offline' })
    this.schedule()
  }

  private readonly onLocalChange = (): void => {
    this.publishInvalidation('outbox')
    this.wake()
  }

  private readonly onRequest = (): void => { this.wake() }

  private refreshAfterRemoteSync(): void {
    if (this.disposed) return
    void this.refreshSnapshot().then((prepared) => {
      if (!prepared || this.disposed) return
      // A remote completion is an invalidation, not a request to run a second
      // sync round. The normal schedule remains the only future wake source.
      this.schedule()
    })
  }

  private dispose(): void {
    if (this.disposed) return
    this.disposed = true
    this.leaseSignal.removeEventListener('abort', this.onLeaseRevoked)
    this.running = null
    this.lifecycleOwners = 0
    this.wakeAfterCurrentRun = false
    this.removeLifecycle()
    this.publishSnapshot(DISPOSED_THOUGHT_SYNC_SNAPSHOT)
    this.listeners.clear()
  }
}

const controllers = new WeakMap<IdentityLease, NamespaceThoughtSyncController>()

/** Returns the single observable controller owned by the supplied identity lease. */
export function getThoughtSyncController(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): ThoughtSyncController {
  const existing = controllers.get(lease)
  if (existing) return existing
  const controller = new NamespaceThoughtSyncController(lease, client)
  controllers.set(lease, controller)
  return controller
}

/** Backward-compatible lifecycle entrypoint used by existing Reader surfaces. */
export function startThoughtSync(
  lease: IdentityLease,
  client: IdentityBoundReaderClient,
): () => void {
  return getThoughtSyncController(lease, client).start()
}

export async function listRemoteThoughts(
  lease: IdentityLease,
  linkId: string,
  target: AnnotationTarget,
): Promise<UserDataTransactionResult<readonly ThoughtMaterializedRecord[]>> {
  const targetKey = annotationTargetKey(target)
  if (!targetKey) return { ok: false }
  return runUserDataTransaction(
    lease,
    'list remote thoughts',
    [THOUGHT_MATERIALIZED_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const request = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .index(THOUGHT_MATERIALIZED_HOST_INDEX)
        .getAll([lease.context.physicalNamespace, linkId, targetKey]) as IDBRequest<unknown[]>
      request.onsuccess = () => {
        const rows = request.result.filter((row): row is ThoughtMaterializedRecord =>
          isValidThoughtMaterializedRecord(row, lease.context.physicalNamespace) &&
          row.hostId === linkId && row.targetKey === targetKey)
        setResult(rows.sort((left, right) => left.serverSequence - right.serverSequence))
      }
      request.onerror = () => transaction.abort()
    },
  )
}

/** Lists all server-materialized thoughts for a host, independent of target revision. */
export async function listRemoteThoughtsForHost(
  lease: IdentityLease,
  hostKind: string,
  hostId: string,
): Promise<UserDataTransactionResult<readonly ThoughtMaterializedRecord[]>> {
  if (!hostKind || !hostId) return { ok: false }
  return runUserDataTransaction(
    lease,
    'list remote thoughts for host',
    [THOUGHT_MATERIALIZED_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const request = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
        .getAll(lease.context.physicalNamespace) as IDBRequest<unknown[]>
      request.onsuccess = () => {
        const rows = request.result.filter((row): row is ThoughtMaterializedRecord =>
          isValidThoughtMaterializedRecord(row, lease.context.physicalNamespace) &&
          row.hostKind === hostKind && row.hostId === hostId)
        setResult(rows.sort((left, right) => left.serverSequence - right.serverSequence))
      }
      request.onerror = () => transaction.abort()
    },
  )
}

/**
 * Persists a page returned by an aggregate endpoint without touching the
 * replay cursor.  Aggregate pagination and the sync stream have different
 * cursors; only the latter is allowed to advance ThoughtSyncState.cursor.
 */
export async function cacheServerThoughtPage(
  lease: IdentityLease,
  items: readonly unknown[],
): Promise<UserDataTransactionResult<number>> {
  const namespace = lease.context.physicalNamespace
  const records = items.map((item) => materializedRecord(namespace, item))
  if (records.some((record) => record === null)) return { ok: false }
  const page = records as ThoughtMaterializedRecord[]
  return runUserDataTransaction(
    lease,
    'cache aggregate thought page',
    [THOUGHT_MATERIALIZED_STORE, ANNOTATION_IMPORTS_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const materialized = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
      const conflicts = transaction.objectStore(ANNOTATION_IMPORTS_STORE)
      const existingRequest = materialized.index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      existingRequest.onerror = () => transaction.abort()
      existingRequest.onsuccess = () => {
        if (!lease.isCurrent(identity)) { transaction.abort(); return }
        const existingByID = new Map<string, ThoughtMaterializedRecord>()
        for (const raw of existingRequest.result) {
          if (!isValidThoughtMaterializedRecord(raw, namespace)) {
            transaction.abort()
            return
          }
          existingByID.set(raw.annotationId, raw)
        }
        for (const record of page) {
          const existing = existingByID.get(record.annotationId)
          if (!existing) {
            materialized.put(record)
            existingByID.set(record.annotationId, record)
            continue
          }
          // The server replay sequence is the server's materialization order.
          // A duplicated or delayed aggregate page must never replace a newer
          // server row merely because its Lamport metadata happens to sort later.
          if (existing.serverSequence > record.serverSequence) {
            const conflict = thoughtConflict(namespace, existing, record, 'older-server-version')
            if (conflict) conflicts.put(conflict)
            continue
          }
          if (existing.serverSequence < record.serverSequence) {
            const conflict = thoughtConflict(namespace, record, existing, 'older-server-version')
            if (conflict) conflicts.put(conflict)
            materialized.put(record)
            existingByID.set(record.annotationId, record)
            continue
          }
          if (stableFingerprint(existing) !== stableFingerprint(record)) {
            transaction.abort()
            return
          }
        }
        setResult(page.length)
      }
    },
  )
}

function isoTimestamp(value: number): string {
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toISOString() : new Date(0).toISOString()
}

function readerThoughtFromMaterialized(record: ThoughtMaterializedRecord): ThoughtReadModelItem {
  return {
    id: record.annotationId,
    host_kind: record.hostKind,
    host_id: record.hostId,
    link_id: record.linkId,
    target: record.target,
    quote: record.quote ?? {},
    body: record.body,
    source: record.source,
    deleted: record.deleted,
    last_sequence: record.serverSequence,
    created_at: record.createdAt,
    updated_at: record.updatedAt,
  }
}

function annotationProjectionAddress(
  linkId: string,
  targetKey: string,
  annotationId: string,
): string {
  return JSON.stringify([linkId, targetKey, annotationId])
}

interface LocalAnnotationProjection {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
  readonly annotation: Annotation
}

function localAnnotationProjection(
  raw: unknown,
  namespace: string,
): LocalAnnotationProjection | null {
  if (!isRecord(raw) || raw.namespace !== namespace ||
    !isNonEmptyString(raw.linkId) || !isNonEmptyString(raw.targetKey) ||
    !isNonEmptyString(raw.annotationId) || !isRecord(raw.target) ||
    !isSafeNonNegativeInteger(raw.sequence) || raw.sequence === 0 ||
    !Array.isArray(raw.key) || raw.key.length !== 4 ||
    raw.key[0] !== namespace || raw.key[1] !== raw.linkId ||
    raw.key[2] !== raw.targetKey || raw.key[3] !== raw.annotationId) {
    return null
  }
  const target = canonicalAnnotationTarget(raw.target as unknown as AnnotationTarget)
  if (!target || annotationTargetKey(target) !== raw.targetKey) return null
  const annotation = cloneTargetAnnotation(raw.annotation, target)
  if (!annotation || annotation.id !== raw.annotationId) return null
  return {
    linkId: raw.linkId,
    target,
    targetKey: raw.targetKey,
    annotationId: raw.annotationId,
    annotation,
  }
}

function displayBaseFromAnnotationProjection(
  record: ThoughtOutboxRecord,
  projection: LocalAnnotationProjection | undefined,
): Annotation | null {
  if (!projection ||
    projection.linkId !== record.linkId ||
    projection.targetKey !== record.targetKey ||
    projection.annotationId !== record.annotationId) {
    return null
  }
  if (record.target.kind === 'inbox') return null
  const target = canonicalAnnotationTarget(record.target)
  if (!target || annotationTargetKey(target) !== record.targetKey ||
    annotationTargetKey(projection.target) !== record.targetKey) {
    return null
  }
  return cloneTargetAnnotation(projection.annotation, target)
}

function localThoughtProjection(
  record: ThoughtOutboxRecord,
  current: ThoughtReadModelItem | undefined,
  durableAnnotation?: Annotation | null,
): ThoughtReadModelItem | null {
  if (record.operationKind === 'delete') return null
  const annotation = record.annotation ?? durableAnnotation ?? null
  // Annotation materialization is display-only recovery for a legacy local
  // tail. It is never a source of wire identity, replay input, or clock allocation.
  if (!current && !annotation) return null
  const updatedAt = record.patch?.updatedAt ?? annotation?.updatedAt ?? record.createdAt
  const createdAt = annotation?.createdAt ?? record.createdAt
  const body = record.patch?.note ?? annotation?.note ?? current?.body
  if (body === undefined) return null
  return {
    ...(current ?? {}),
    id: record.annotationId,
    host_kind: record.hostKind,
    host_id: record.hostId,
    link_id: record.linkId,
    target: record.target,
    quote: annotation
      ? {
          exact: annotation.text,
          start: annotation.start,
          end: annotation.end,
          prefix: annotation.quote?.prefix ?? '',
          suffix: annotation.quote?.suffix ?? '',
          block_key: annotation.blockKey,
        }
      : current?.quote ?? {},
    body,
    source: record.patch?.source ?? annotation?.source ?? current?.source ?? 'self',
    deleted: false,
    last_sequence: current?.last_sequence ?? 0,
    created_at: current?.created_at ?? isoTimestamp(createdAt),
    updated_at: isoTimestamp(updatedAt),
  }
}

/**
 * Canonical ordering for every live Thought projection.  Keep this beside the
 * selector so Home and the aggregate surface cannot drift on timestamp ties.
 */
export function sortThoughtReadModel(
  items: readonly ThoughtReadModelItem[],
): ThoughtReadModelItem[] {
  return [...items].sort((left, right) => {
    const leftTime = Date.parse(left.updated_at)
    const rightTime = Date.parse(right.updated_at)
    if (Number.isFinite(leftTime) && Number.isFinite(rightTime) && leftTime !== rightTime) {
      return rightTime - leftTime
    }
    if (left.updated_at !== right.updated_at) {
      return left.updated_at > right.updated_at ? -1 : 1
    }
    if (left.id === right.id) return 0
    // `localeCompare` makes the Reader contract depend on the browser locale.
    // Use one deterministic identifier order for the `thought_id DESC` tie
    // break across the selector and every aggregate surface.
    return left.id > right.id ? -1 : 1
  })
}

/**
 * Returns the identity-scoped local-first aggregate projection.  The selector
 * is deliberately cursor-free: local inserts, replacements, and tombstones
 * change only visible rows, never the server replay position.
 */
export async function selectThoughtReadModel(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<readonly ThoughtReadModelItem[]>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'select local-first thoughts',
    [THOUGHT_MATERIALIZED_STORE, THOUGHT_OUTBOX_STORE, ANNOTATION_MATERIALIZED_STORE],
    'readwrite',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const serverRequest = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<unknown[]>
      const annotationRequest = transaction.objectStore(ANNOTATION_MATERIALIZED_STORE)
        .getAll() as IDBRequest<unknown[]>
      let serverDone = false
      let outboxDone = false
      let annotationDone = false
      const finish = () => {
        if (!serverDone || !outboxDone || !annotationDone) return
        if (!lease.isCurrent(identity)) { transaction.abort(); return }
        const merged = new Map<string, ThoughtReadModelItem>()
        for (const raw of serverRequest.result) {
          if (!isValidThoughtMaterializedRecord(raw, namespace)) { transaction.abort(); return }
          const row = raw as ThoughtMaterializedRecord
          const current = merged.get(row.annotationId)
          if (!current || row.serverSequence > current.last_sequence) {
            if (row.deleted) merged.delete(row.annotationId)
            else merged.set(row.annotationId, readerThoughtFromMaterialized(row))
          }
        }
        const operations = outboxRequest.result
          .filter((raw): raw is ThoughtOutboxRecord => isValidThoughtOutboxRecord(raw, namespace))
          .sort((left, right) => left.sequence - right.sequence)
        const annotationProjections = new Map<string, LocalAnnotationProjection>()
        for (const raw of annotationRequest.result) {
          const projection = localAnnotationProjection(raw, namespace)
          if (!projection) continue
          annotationProjections.set(annotationProjectionAddress(
            projection.linkId,
            projection.targetKey,
            projection.annotationId,
          ), projection)
        }
        const outboxStore = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        for (const record of operations) {
          const operation = quarantineLegacyThoughtOutboxRecord(record)
          if (operation !== record) outboxStore.put(operation)
          // pending is retryable by construction; blocked is intentionally
          // visible as its last durable local projection.
          const current = merged.get(operation.annotationId)
          const durableAnnotation = operation.operationKind === 'update' &&
            !current && operation.annotation === null
            ? displayBaseFromAnnotationProjection(operation, annotationProjections.get(
              annotationProjectionAddress(operation.linkId, operation.targetKey, operation.annotationId),
            ))
            : null
          const projection = localThoughtProjection(operation, current, durableAnnotation)
          if (operation.operationKind === 'delete') merged.delete(operation.annotationId)
          else if (projection) merged.set(operation.annotationId, projection)
        }
        setResult(sortThoughtReadModel([...merged.values()]))
      }
      serverRequest.onerror = () => transaction.abort()
      outboxRequest.onerror = () => transaction.abort()
      annotationRequest.onerror = () => transaction.abort()
      serverRequest.onsuccess = () => { serverDone = true; finish() }
      outboxRequest.onsuccess = () => { outboxDone = true; finish() }
      annotationRequest.onsuccess = () => { annotationDone = true; finish() }
    },
  )
}

/** Reads durable loser versions without merging them into the visible projection. */
export async function listThoughtConflicts(
  lease: IdentityLease,
  annotationId?: string,
): Promise<UserDataTransactionResult<readonly ThoughtConflictRecord[]>> {
  return runUserDataTransaction(
    lease,
    'list thought conflicts',
    [ANNOTATION_IMPORTS_STORE],
    'readonly',
    (transaction, identity, setResult) => {
      if (!lease.isCurrent(identity)) { transaction.abort(); return }
      const request = transaction.objectStore(ANNOTATION_IMPORTS_STORE).getAll() as IDBRequest<unknown[]>
      request.onsuccess = () => {
        const rows = request.result.filter((row): row is ThoughtConflictRecord =>
          isValidThoughtConflictRecord(row, lease.context.physicalNamespace) &&
          (annotationId === undefined || row.annotationId === annotationId))
        setResult(rows.sort((left, right) =>
          left.serverSequence - right.serverSequence ||
          left.loserFingerprint.localeCompare(right.loserFingerprint)))
      }
      request.onerror = () => transaction.abort()
    },
  )
}
