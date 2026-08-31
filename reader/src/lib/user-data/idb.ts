import type { IdentityLease, IdentityOperationContext } from '../identity'
import {
  abortIDBTransaction,
  attachIDBTransactionAbortSignal,
  runIDBTransaction,
  type IDBExecutionResult,
} from '../idb-core'
import { ownedDatabaseName } from '../storage-ownership'

export const USER_DATA_DATABASE_VERSION = 10
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

// Supersession events are a distinct immutable stream. Their cursor must not
// share the ordinary thought replay cursor or annotation operation sequence.
export const THOUGHT_SUPERSESSION_EVENTS_STORE = 'thought_supersession_events'
export const THOUGHT_SUPERSESSION_STATE_STORE = 'thought_supersession_state'

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
  | typeof THOUGHT_SUPERSESSION_EVENTS_STORE
  | typeof THOUGHT_SUPERSESSION_STATE_STORE

export type UserDataTransactionResult<T> = IDBExecutionResult<T>

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
  resolutionRequest.onerror = () => abortIDBTransaction(transaction)
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
    recordsRequest.onerror = () => abortIDBTransaction(transaction)
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

  for (const retiredStore of [
    'thought_repair_ready',
    'thought_repair_quarantine',
    'thought_repair_manifest',
    'thought_repair_source',
    'thought_repair_ack',
  ]) {
    if (database.objectStoreNames.contains(retiredStore)) database.deleteObjectStore(retiredStore)
  }

  if (oldVersion === 1) migrateResolvedV1Batch(transaction)
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
        abortIDBTransaction(transaction)
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
      finish(database)
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
  return attachIDBTransactionAbortSignal(transaction, operation.signal)
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

  return runIDBTransaction<T>(
    database,
    storeNames,
    mode,
    ({ transaction, setResult }) => execute(transaction, operation, setResult),
    {
      signal: operation.signal,
      isCurrent: () => lease.isCurrent(operation),
    },
  )
}
