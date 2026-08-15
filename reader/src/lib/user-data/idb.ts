import type { IdentityLease, IdentityOperationContext } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  isThoughtRepairSourceSeal,
  isThoughtRepairAck,
  planThoughtRepair,
  repairInputsForKnownSources,
  thoughtRepairSourceSeal,
  type ThoughtRepairAckRecord,
  type ThoughtRepairPlan,
  type ThoughtRepairReadyRecord,
  type ThoughtRepairSourceRecord,
  canonicalRepairJSON,
  repairReadyChecksum,
} from './thought-repair'

export const USER_DATA_DATABASE_VERSION = 9
export const LEGACY_PENDING_STORE = 'legacy-unattributed'
export const LEGACY_ARCHIVE_STORE = 'legacy-import-archive'
export const LEGACY_ARCHIVE_NAMESPACE_INDEX = 'by-imported-namespace'
export const LEGACY_ARCHIVE_FINGERPRINT_INDEX = 'by-fingerprint-version'
export const LEGACY_DEDUP_STORE = 'legacy-import-dedup'
export const MIGRATION_DECISION_STORE = 'migration-decisions'

export const ANNOTATION_OPS_STORE = 'annotation_ops'
export const ANNOTATION_OPS_ID_INDEX = 'by-op-id'
export const ANNOTATION_OPS_TARGET_INDEX = 'by-target-sequence'
export const ANNOTATION_MATERIALIZED_STORE = 'annotation_materialized'
export const ANNOTATION_LINK_STATE_STORE = 'annotation_link_state'
export const ANNOTATED_LINKS_STORE = 'annotated_links'
export const ANNOTATED_LINKS_NAMESPACE_INDEX = 'by-namespace'
export const ANNOTATION_IMPORTS_STORE = 'annotation_imports'

// Reader vNext server sync has a separate durable queue and remote projection.
// `annotation_ops.sequence` remains the browser-local operation sequence; it
// is never reused as the server replay cursor.
export const THOUGHT_OUTBOX_STORE = 'thought_outbox'
export const THOUGHT_OUTBOX_NAMESPACE_INDEX = 'by-namespace'
export const THOUGHT_OUTBOX_OP_ID_INDEX = 'by-op-id'
export const THOUGHT_OUTBOX_SEQUENCE_INDEX = 'by-namespace-sequence'
export const THOUGHT_HISTORY_OUTBOX_STORE = 'thought_history_outbox'
export const THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX = 'by-namespace'
export const THOUGHT_HISTORY_OUTBOX_OP_ID_INDEX = 'by-op-id'
export const THOUGHT_SYNC_STATE_STORE = 'thought_sync_state'
export const THOUGHT_MATERIALIZED_STORE = 'thought_materialized'
export const THOUGHT_MATERIALIZED_NAMESPACE_INDEX = 'by-namespace'
export const THOUGHT_MATERIALIZED_HOST_INDEX = 'by-host'
// v6 is deliberately a derived, append-only migration surface.  The v4/v5
// stores above remain the forensic source of truth and are never rewritten by
// repair; a failed or corrupt derived copy can therefore be rebuilt safely.
export const THOUGHT_REPAIR_READY_STORE = 'thought_repair_ready'
export const THOUGHT_REPAIR_QUARANTINE_STORE = 'thought_repair_quarantine'
export const THOUGHT_REPAIR_MANIFEST_STORE = 'thought_repair_manifest'
/** Maps immutable v4/v5 source rows so sync can never dispatch them directly. */
export const THOUGHT_REPAIR_SOURCE_STORE = 'thought_repair_source'
/** Durable acknowledgement receipts prevent a later rebuild from reviving a row. */
export const THOUGHT_REPAIR_ACK_STORE = 'thought_repair_ack'

// Supersession events are a distinct immutable stream. Their cursor must not
// share the ordinary thought replay cursor or annotation operation sequence.
export const THOUGHT_SUPERSESSION_EVENTS_STORE = 'thought_supersession_events'
export const THOUGHT_SUPERSESSION_STATE_STORE = 'thought_supersession_state'

export const LEGACY_STORE = LEGACY_PENDING_STORE
export const DECISION_STORE = MIGRATION_DECISION_STORE

export type UserDataStoreName =
  | typeof LEGACY_PENDING_STORE
  | typeof LEGACY_ARCHIVE_STORE
  | typeof LEGACY_DEDUP_STORE
  | typeof MIGRATION_DECISION_STORE
  | typeof ANNOTATION_OPS_STORE
  | typeof ANNOTATION_MATERIALIZED_STORE
  | typeof ANNOTATION_LINK_STATE_STORE
  | typeof ANNOTATED_LINKS_STORE
  | typeof ANNOTATION_IMPORTS_STORE
  | typeof THOUGHT_OUTBOX_STORE
  | typeof THOUGHT_HISTORY_OUTBOX_STORE
  | typeof THOUGHT_SYNC_STATE_STORE
  | typeof THOUGHT_MATERIALIZED_STORE
  | typeof THOUGHT_REPAIR_READY_STORE
  | typeof THOUGHT_REPAIR_QUARANTINE_STORE
  | typeof THOUGHT_REPAIR_MANIFEST_STORE
  | typeof THOUGHT_REPAIR_SOURCE_STORE
  | typeof THOUGHT_REPAIR_ACK_STORE
  | typeof THOUGHT_SUPERSESSION_EVENTS_STORE
  | typeof THOUGHT_SUPERSESSION_STATE_STORE

export type UserDataTransactionResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false }

const OPEN_TIMEOUT_MS = 1000
const LEGACY_RESOLUTION_ID = 'resolution'

let databasePromise: Promise<IDBDatabase | null> | null = null
let databaseHandle: IDBDatabase | null = null
let databaseGeneration = 0

function migrateResolvedV1Batch(transaction: IDBTransaction): void {
  const pending = transaction.objectStore(LEGACY_PENDING_STORE)
  const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE)
  const resolutionRequest = transaction.objectStore(MIGRATION_DECISION_STORE).get(
    LEGACY_RESOLUTION_ID,
  )
  resolutionRequest.onerror = () => transaction.abort()
  resolutionRequest.onsuccess = () => {
    const resolution = resolutionRequest.result as {
      kind?: unknown
      namespace?: unknown
      decidedAt?: unknown
    } | undefined
    if (
      resolution?.kind !== 'imported' ||
      typeof resolution.namespace !== 'string' ||
      typeof resolution.decidedAt !== 'number'
    ) {
      return
    }

    const recordsRequest = pending.getAll()
    recordsRequest.onerror = () => transaction.abort()
    recordsRequest.onsuccess = () => {
      for (const record of recordsRequest.result as Array<Record<string, unknown>>) {
        archive.add({
          ...record,
          importedIntoNamespace: resolution.namespace,
          importedAt: resolution.decidedAt,
        })
      }
      pending.clear()
    }
  }
}

function getAll(store: IDBObjectStore): IDBRequest<unknown[]> {
  return store.getAll() as IDBRequest<unknown[]>
}

function writeRepairPlan(transaction: IDBTransaction, plan: ThoughtRepairPlan, clear = false): void {
  const sources = transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)
  const ready = transaction.objectStore(THOUGHT_REPAIR_READY_STORE)
  const quarantine = transaction.objectStore(THOUGHT_REPAIR_QUARANTINE_STORE)
  const manifests = transaction.objectStore(THOUGHT_REPAIR_MANIFEST_STORE)
  if (clear) {
    sources.clear()
    ready.clear()
    quarantine.clear()
    manifests.clear()
  }
  for (const source of plan.sources) sources.put(source)
  sources.put(thoughtRepairSourceSeal(plan.sources))
  for (const record of plan.ready) ready.put(record)
  for (const record of plan.quarantine) quarantine.put(record)
  // A manifest is the commit marker. It is deliberately the final durable
  // write for each namespace after all ready/quarantine/floor planning exists.
  for (const manifest of plan.manifests) manifests.put(manifest)
}

function deriveRepairInVersionchange(transaction: IDBTransaction): void {
  const requests = {
    v4Operations: getAll(transaction.objectStore(ANNOTATION_OPS_STORE)),
    v5Outbox: getAll(transaction.objectStore(THOUGHT_OUTBOX_STORE)),
    v4Materialized: getAll(transaction.objectStore(ANNOTATION_MATERIALIZED_STORE)),
    v5Materialized: getAll(transaction.objectStore(THOUGHT_MATERIALIZED_STORE)),
    syncStates: getAll(transaction.objectStore(THOUGHT_SYNC_STATE_STORE)),
  }
  let completed = 0
  const finish = () => {
    if (completed !== Object.keys(requests).length) return
    try {
      writeRepairPlan(transaction, planThoughtRepair({
        v4Operations: requests.v4Operations.result,
        v5Outbox: requests.v5Outbox.result,
        v4Materialized: requests.v4Materialized.result,
        v5Materialized: requests.v5Materialized.result,
        syncStates: requests.syncStates.result,
      }))
    } catch {
      transaction.abort()
    }
  }
  for (const request of Object.values(requests)) {
    request.onerror = () => transaction.abort()
    request.onsuccess = () => { completed += 1; finish() }
  }
}

function ensureUserDataSchema(
  database: IDBDatabase,
  transaction: IDBTransaction,
  oldVersion: number,
): void {
  if (!database.objectStoreNames.contains(LEGACY_PENDING_STORE)) {
    database.createObjectStore(LEGACY_PENDING_STORE, { keyPath: 'id' })
  }

  const archive = database.objectStoreNames.contains(LEGACY_ARCHIVE_STORE)
    ? transaction.objectStore(LEGACY_ARCHIVE_STORE)
    : database.createObjectStore(LEGACY_ARCHIVE_STORE, {
        keyPath: 'archiveID',
        autoIncrement: true,
      })
  if (!archive.indexNames.contains(LEGACY_ARCHIVE_NAMESPACE_INDEX)) {
    archive.createIndex(LEGACY_ARCHIVE_NAMESPACE_INDEX, 'importedIntoNamespace')
  }
  if (!archive.indexNames.contains(LEGACY_ARCHIVE_FINGERPRINT_INDEX)) {
    archive.createIndex(LEGACY_ARCHIVE_FINGERPRINT_INDEX, 'fingerprintVersion')
  }

  if (!database.objectStoreNames.contains(LEGACY_DEDUP_STORE)) {
    database.createObjectStore(LEGACY_DEDUP_STORE, { keyPath: 'id' })
  }
  if (!database.objectStoreNames.contains(MIGRATION_DECISION_STORE)) {
    database.createObjectStore(MIGRATION_DECISION_STORE, { keyPath: 'id' })
  }

  const operations = database.objectStoreNames.contains(ANNOTATION_OPS_STORE)
    ? transaction.objectStore(ANNOTATION_OPS_STORE)
    : database.createObjectStore(ANNOTATION_OPS_STORE, {
        keyPath: 'sequence',
        autoIncrement: true,
      })
  if (!operations.indexNames.contains(ANNOTATION_OPS_ID_INDEX)) {
    operations.createIndex(ANNOTATION_OPS_ID_INDEX, 'opId', { unique: true })
  }
  if (!operations.indexNames.contains(ANNOTATION_OPS_TARGET_INDEX)) {
    operations.createIndex(
      ANNOTATION_OPS_TARGET_INDEX,
      ['namespace', 'linkId', 'targetKey', 'sequence'],
    )
  }

  if (!database.objectStoreNames.contains(ANNOTATION_MATERIALIZED_STORE)) {
    database.createObjectStore(ANNOTATION_MATERIALIZED_STORE, { keyPath: 'key' })
  }
  if (!database.objectStoreNames.contains(ANNOTATION_LINK_STATE_STORE)) {
    database.createObjectStore(ANNOTATION_LINK_STATE_STORE, { keyPath: 'key' })
  }

  const annotatedLinks = database.objectStoreNames.contains(ANNOTATED_LINKS_STORE)
    ? transaction.objectStore(ANNOTATED_LINKS_STORE)
    : database.createObjectStore(ANNOTATED_LINKS_STORE, { keyPath: 'key' })
  if (!annotatedLinks.indexNames.contains(ANNOTATED_LINKS_NAMESPACE_INDEX)) {
    annotatedLinks.createIndex(ANNOTATED_LINKS_NAMESPACE_INDEX, 'namespace')
  }

  if (!database.objectStoreNames.contains(ANNOTATION_IMPORTS_STORE)) {
    database.createObjectStore(ANNOTATION_IMPORTS_STORE, { keyPath: 'key' })
  }

  const outbox = database.objectStoreNames.contains(THOUGHT_OUTBOX_STORE)
    ? transaction.objectStore(THOUGHT_OUTBOX_STORE)
    : database.createObjectStore(THOUGHT_OUTBOX_STORE, { keyPath: 'key' })
  if (!outbox.indexNames.contains(THOUGHT_OUTBOX_NAMESPACE_INDEX)) {
    outbox.createIndex(THOUGHT_OUTBOX_NAMESPACE_INDEX, 'namespace')
  }
  if (!outbox.indexNames.contains(THOUGHT_OUTBOX_OP_ID_INDEX)) {
    outbox.createIndex(THOUGHT_OUTBOX_OP_ID_INDEX, ['namespace', 'opId'], { unique: true })
  }
  if (!outbox.indexNames.contains(THOUGHT_OUTBOX_SEQUENCE_INDEX)) {
    outbox.createIndex(THOUGHT_OUTBOX_SEQUENCE_INDEX, ['namespace', 'sequence'])
  }

  const historyOutbox = database.objectStoreNames.contains(THOUGHT_HISTORY_OUTBOX_STORE)
    ? transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
    : database.createObjectStore(THOUGHT_HISTORY_OUTBOX_STORE, { keyPath: 'key' })
  if (!historyOutbox.indexNames.contains(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)) {
    historyOutbox.createIndex(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX, 'namespace')
  }
  if (!historyOutbox.indexNames.contains(THOUGHT_HISTORY_OUTBOX_OP_ID_INDEX)) {
    historyOutbox.createIndex(
      THOUGHT_HISTORY_OUTBOX_OP_ID_INDEX,
      ['namespace', 'opId'],
      { unique: true },
    )
  }

  if (!database.objectStoreNames.contains(THOUGHT_SYNC_STATE_STORE)) {
    database.createObjectStore(THOUGHT_SYNC_STATE_STORE, { keyPath: 'namespace' })
  }

  if (!database.objectStoreNames.contains(THOUGHT_REPAIR_READY_STORE)) {
    database.createObjectStore(THOUGHT_REPAIR_READY_STORE, { keyPath: 'key' })
  }
  if (!database.objectStoreNames.contains(THOUGHT_REPAIR_QUARANTINE_STORE)) {
    database.createObjectStore(THOUGHT_REPAIR_QUARANTINE_STORE, { keyPath: 'key' })
  }
  if (!database.objectStoreNames.contains(THOUGHT_REPAIR_MANIFEST_STORE)) {
    database.createObjectStore(THOUGHT_REPAIR_MANIFEST_STORE, { keyPath: 'namespace' })
  }
  if (!database.objectStoreNames.contains(THOUGHT_REPAIR_SOURCE_STORE)) {
    database.createObjectStore(THOUGHT_REPAIR_SOURCE_STORE, { keyPath: 'key' })
  }
  if (!database.objectStoreNames.contains(THOUGHT_REPAIR_ACK_STORE)) {
    database.createObjectStore(THOUGHT_REPAIR_ACK_STORE, { keyPath: 'key' })
  }

  const materialized = database.objectStoreNames.contains(THOUGHT_MATERIALIZED_STORE)
    ? transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
    : database.createObjectStore(THOUGHT_MATERIALIZED_STORE, { keyPath: 'key' })
  if (!materialized.indexNames.contains(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)) {
    materialized.createIndex(THOUGHT_MATERIALIZED_NAMESPACE_INDEX, 'namespace')
  }
  if (!materialized.indexNames.contains(THOUGHT_MATERIALIZED_HOST_INDEX)) {
    materialized.createIndex(
      THOUGHT_MATERIALIZED_HOST_INDEX,
      ['namespace', 'linkId', 'targetKey'],
    )
  }

  if (!database.objectStoreNames.contains(THOUGHT_SUPERSESSION_EVENTS_STORE)) {
    database.createObjectStore(THOUGHT_SUPERSESSION_EVENTS_STORE, { keyPath: 'key' })
  }
  if (!database.objectStoreNames.contains(THOUGHT_SUPERSESSION_STATE_STORE)) {
    database.createObjectStore(THOUGHT_SUPERSESSION_STATE_STORE, { keyPath: 'namespace' })
  }

  if (oldVersion === 1) migrateResolvedV1Batch(transaction)
  // v4/v5 rows are forensic inputs. Never add bookkeeping fields, normalize
  // clocks, clear queues, or otherwise write their stores in this migration.
  // All normalised data lives in the independent v6 repair stores instead.
  if (oldVersion < 6) deriveRepairInVersionchange(transaction)
}

interface RepairRows {
  readonly v4Operations: readonly unknown[]
  readonly v5Outbox: readonly unknown[]
  readonly v4Materialized: readonly unknown[]
  readonly v5Materialized: readonly unknown[]
  readonly syncStates: readonly unknown[]
  readonly sources: readonly unknown[]
  readonly ready: readonly unknown[]
  readonly quarantine: readonly unknown[]
  readonly manifests: readonly unknown[]
  readonly acknowledgements: readonly unknown[]
}

function sortedCanonical(rows: readonly unknown[]): string[] {
  return rows.map(canonicalRepairJSON).sort()
}

function exactRows(actual: readonly unknown[], expected: readonly unknown[]): boolean {
  const a = sortedCanonical(actual)
  const b = sortedCanonical(expected)
  return a.length === b.length && a.every((row, index) => row === b[index])
}

/** Only operation bytes, never retry/backoff bookkeeping, participate in repair integrity. */
function exactReadyRows(
  actual: readonly unknown[],
  expected: readonly ThoughtRepairReadyRecord[],
): boolean {
  if (actual.length !== expected.length) return false
  const expectedByKey = new Map(expected.map((record) => [
    `${record.namespace}\0${record.opId}`, record,
  ]))
  const seen = new Set<string>()
  for (const raw of actual) {
    if (!raw || typeof raw !== 'object') return false
    const candidate = raw as Partial<ThoughtRepairReadyRecord>
    if (typeof candidate.namespace !== 'string' || typeof candidate.opId !== 'string') return false
    const key = `${candidate.namespace}\0${candidate.opId}`
    const planned = expectedByKey.get(key)
    if (!planned || seen.has(key)) return false
    if (repairReadyChecksum(raw as ThoughtRepairReadyRecord) !== repairReadyChecksum(planned)) return false
    seen.add(key)
  }
  return true
}

function repairSourceRecords(rows: readonly unknown[]): readonly ThoughtRepairSourceRecord[] | null {
  const sealRows = rows.filter((row) => {
    if (!row || typeof row !== 'object') return false
    const key = (row as { key?: unknown }).key
    return Array.isArray(key) && key.length === 2 &&
      key[0] === '__webtag-repair-source-seal__' && key[1] === 'v6'
  })
  if (sealRows.length !== 1) return null
  const sources = rows.filter((row) => row !== sealRows[0]) as readonly ThoughtRepairSourceRecord[]
  return isThoughtRepairSourceSeal(sealRows[0], sources) ? sources : null
}

function repairRowsMatch(
  plan: ThoughtRepairPlan,
  rows: RepairRows,
  sources: readonly ThoughtRepairSourceRecord[],
): boolean {
  const expectedByKey = new Map(plan.ready.map((record) => [
    `${record.namespace}\0${record.opId}`, record,
  ]))
  const acknowledgements: ThoughtRepairAckRecord[] = []
  for (const raw of rows.acknowledgements) {
    if (!raw || typeof raw !== 'object') return false
    const candidate = raw as Partial<ThoughtRepairAckRecord>
    const expected = typeof candidate.namespace === 'string' && typeof candidate.opId === 'string'
      ? expectedByKey.get(`${candidate.namespace}\0${candidate.opId}`)
      : undefined
    if (!expected || !isThoughtRepairAck(raw, expected)) return false
    acknowledgements.push(raw as ThoughtRepairAckRecord)
  }
  const acknowledged = new Set(acknowledgements.map((record) =>
    `${record.namespace}\0${record.opId}`))
  const expectedReady = plan.ready.filter((record) => !acknowledged.has(`${record.namespace}\0${record.opId}`))
  return exactRows(sources, plan.sources) &&
    exactReadyRows(rows.ready, expectedReady) &&
    exactRows(rows.quarantine, plan.quarantine) &&
    exactRows(rows.manifests, plan.manifests)
}

function readRepairRows(
  transaction: IDBTransaction,
  done: (rows: RepairRows) => void,
): void {
  const requests = {
    v4Operations: getAll(transaction.objectStore(ANNOTATION_OPS_STORE)),
    v5Outbox: getAll(transaction.objectStore(THOUGHT_OUTBOX_STORE)),
    v4Materialized: getAll(transaction.objectStore(ANNOTATION_MATERIALIZED_STORE)),
    v5Materialized: getAll(transaction.objectStore(THOUGHT_MATERIALIZED_STORE)),
    syncStates: getAll(transaction.objectStore(THOUGHT_SYNC_STATE_STORE)),
    sources: getAll(transaction.objectStore(THOUGHT_REPAIR_SOURCE_STORE)),
    ready: getAll(transaction.objectStore(THOUGHT_REPAIR_READY_STORE)),
    quarantine: getAll(transaction.objectStore(THOUGHT_REPAIR_QUARANTINE_STORE)),
    manifests: getAll(transaction.objectStore(THOUGHT_REPAIR_MANIFEST_STORE)),
    acknowledgements: getAll(transaction.objectStore(THOUGHT_REPAIR_ACK_STORE)),
  }
  let completed = 0
  const finish = () => {
    if (completed !== Object.keys(requests).length) return
    done({
      v4Operations: requests.v4Operations.result,
      v5Outbox: requests.v5Outbox.result,
      v4Materialized: requests.v4Materialized.result,
      v5Materialized: requests.v5Materialized.result,
      syncStates: requests.syncStates.result,
      sources: requests.sources.result,
      ready: requests.ready.result,
      quarantine: requests.quarantine.result,
      manifests: requests.manifests.result,
      acknowledgements: requests.acknowledgements.result,
    })
  }
  for (const request of Object.values(requests)) {
    request.onerror = () => transaction.abort()
    request.onsuccess = () => { completed += 1; finish() }
  }
}

/**
 * Versionchange cannot run again for an already-v6 database. Before exposing
 * the handle, therefore, rederive and validate its v6 view. Missing or
 * tampered derived data is atomically rebuilt from read-only legacy sources.
 */
function ensureRepairIntegrity(database: IDBDatabase): Promise<boolean> {
  const names: UserDataStoreName[] = [
    ANNOTATION_OPS_STORE,
    THOUGHT_OUTBOX_STORE,
    THOUGHT_HISTORY_OUTBOX_STORE,
    ANNOTATION_MATERIALIZED_STORE,
    THOUGHT_MATERIALIZED_STORE,
    THOUGHT_SYNC_STATE_STORE,
    THOUGHT_REPAIR_SOURCE_STORE,
    THOUGHT_REPAIR_READY_STORE,
    THOUGHT_REPAIR_QUARANTINE_STORE,
    THOUGHT_REPAIR_MANIFEST_STORE,
    THOUGHT_REPAIR_ACK_STORE,
  ]
  if (!names.every((name) => database.objectStoreNames.contains(name))) return Promise.resolve(false)
  return new Promise((resolve) => {
    let transaction: IDBTransaction
    try {
      transaction = database.transaction(names, 'readwrite')
      readRepairRows(transaction, (rows) => {
        try {
          const sources = repairSourceRecords(rows.sources)
          if (!sources) {
            transaction.abort()
            return
          }
          const plan = planThoughtRepair(repairInputsForKnownSources(rows, sources))
          if (repairRowsMatch(plan, rows, sources)) return
          const validAcks = rows.acknowledgements.filter((raw) => {
            if (!raw || typeof raw !== 'object') return false
            const value = raw as Partial<ThoughtRepairAckRecord>
            const expected = typeof value.namespace === 'string' && typeof value.opId === 'string'
              ? plan.ready.find((record) => record.namespace === value.namespace && record.opId === value.opId)
              : undefined
            return expected !== undefined && isThoughtRepairAck(raw, expected)
          }) as ThoughtRepairAckRecord[]
          transaction.objectStore(THOUGHT_REPAIR_ACK_STORE).clear()
          for (const ack of validAcks) transaction.objectStore(THOUGHT_REPAIR_ACK_STORE).put(ack)
          writeRepairPlan(transaction, plan, true)
          const acknowledged = new Set(validAcks.map((ack) => `${ack.namespace}\0${ack.opId}`))
          const ready = transaction.objectStore(THOUGHT_REPAIR_READY_STORE)
          // writeRepairPlan stores the complete immutable planned set. Remove
          // only receipt-backed records before commit, never any legacy source.
          for (const record of plan.ready) {
            if (acknowledged.has(`${record.namespace}\0${record.opId}`)) ready.delete([...record.key])
          }
        } catch {
          transaction.abort()
        }
      })
      transaction.oncomplete = () => resolve(true)
      transaction.onerror = () => resolve(false)
      transaction.onabort = () => resolve(false)
    } catch {
      resolve(false)
    }
  })
}

function beginOpen(generation: number): Promise<IDBDatabase | null> {
  if (typeof indexedDB === 'undefined') return Promise.resolve(null)

  return new Promise((resolve) => {
    let settled = false
    const finish = (database: IDBDatabase | null) => {
      if (settled) {
        database?.close()
        return
      }
      settled = true
      clearTimeout(timer)
      if (generation !== databaseGeneration) {
        database?.close()
        resolve(null)
        return
      }
      databaseHandle = database
      resolve(database)
    }
    const timer = setTimeout(() => finish(null), OPEN_TIMEOUT_MS)

    let request: IDBOpenDBRequest
    try {
      request = indexedDB.open(
        ownedDatabaseName('userDataDatabase'),
        USER_DATA_DATABASE_VERSION,
      )
    } catch {
      finish(null)
      return
    }

    request.onupgradeneeded = (event) => {
      const transaction = request.transaction
      if (!transaction) {
        request.result.close()
        return
      }
      try {
        ensureUserDataSchema(
          request.result,
          transaction,
          (event as IDBVersionChangeEvent).oldVersion,
        )
      } catch {
        transaction.abort()
      }
    }
    request.onsuccess = () => {
      const database = request.result
      database.onversionchange = () => {
        database.close()
        if (databaseHandle === database) {
          databaseHandle = null
          databasePromise = null
          databaseGeneration += 1
        }
      }
      // Sync never receives this handle until the manifest has been checked or
      // rebuilt. A malformed v6 view therefore has no window to send a tail.
      void ensureRepairIntegrity(database).then((ready) => finish(ready ? database : null), () => finish(null))
    }
    request.onerror = () => finish(null)
    request.onblocked = () => finish(null)
  })
}

export function openUserDataDatabase(): Promise<IDBDatabase | null> {
  if (!databasePromise) {
    const opening = beginOpen(databaseGeneration)
    databasePromise = opening
    void opening.then((database) => {
      if (!database && databasePromise === opening) databasePromise = null
    })
  }
  return databasePromise
}

export function resetUserDataDatabaseHandle(): void {
  databaseGeneration += 1
  databaseHandle?.close()
  databaseHandle = null
  databasePromise = null
}

export function attachLeaseAbort(
  transaction: IDBTransaction,
  operation: IdentityOperationContext,
): () => void {
  const abort = () => {
    try {
      transaction.abort()
    } catch {
      // The transaction may already have committed.
    }
  }
  operation.signal.addEventListener('abort', abort, { once: true })
  if (operation.signal.aborted) abort()
  return () => operation.signal.removeEventListener('abort', abort)
}

export async function runUserDataTransaction<T>(
  lease: IdentityLease,
  label: string,
  storeNames: readonly UserDataStoreName[],
  mode: IDBTransactionMode,
  execute: (
    transaction: IDBTransaction,
    operation: IdentityOperationContext,
    setResult: (value: T) => void,
  ) => void,
): Promise<UserDataTransactionResult<T>> {
  const operation = lease.capture(`user-data ${label}`)
  if (!lease.isCurrent(operation)) return { ok: false }
  const database = await openUserDataDatabase()
  if (!database || !lease.isCurrent(operation)) return { ok: false }

  return new Promise((resolve) => {
    let settled = false
    let value: T | undefined
    let hasValue = false
    const finish = (result: UserDataTransactionResult<T>) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(result)
    }
    let detachAbort: () => void = () => undefined
    let transaction: IDBTransaction | null = null
    try {
      transaction = database.transaction([...storeNames], mode)
      detachAbort = attachLeaseAbort(transaction, operation)
      execute(transaction, operation, (next) => {
        value = next
        hasValue = true
      })
      transaction.oncomplete = () => {
        if (!lease.isCurrent(operation) || !hasValue) {
          finish({ ok: false })
          return
        }
        finish({ ok: true, value: value as T })
      }
      transaction.onerror = () => finish({ ok: false })
      transaction.onabort = () => finish({ ok: false })
    } catch {
      try {
        transaction?.abort()
      } catch {
        // The transaction may already have aborted or committed.
      }
      finish({ ok: false })
    }
  })
}
