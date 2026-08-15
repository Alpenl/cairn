import {
  ANNOTATED_LINKS_STORE,
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_STORE,
  LEGACY_ARCHIVE_STORE,
  LEGACY_DEDUP_STORE,
  LEGACY_PENDING_STORE,
  MIGRATION_DECISION_STORE,
  THOUGHT_REPAIR_MANIFEST_STORE,
  THOUGHT_REPAIR_READY_STORE,
  THOUGHT_REPAIR_SOURCE_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
} from '../src/lib/user-data/idb'
import type { IdentityBoundReaderClient } from '../src/lib/api/client'
import { IdentityLease } from '../src/lib/identity'
import { ownedDatabaseName } from '../src/lib/storage-ownership'
import { startThoughtSync, syncThoughts } from '../src/lib/user-data/thought-sync'

const DATABASE_NAME = ownedDatabaseName('userDataDatabase')

interface ThoughtRepairHarness {
  reset(): Promise<void>
  startQuarantineLifecycle(): Promise<void>
  lifecycleMetrics(): { readonly pulls: number; readonly pushedAnnotationIds: readonly string[] }
  migrateTail(kind: 'update' | 'delete'): Promise<{
    readonly legacyBefore: unknown
    readonly legacyAfter: unknown
    readonly ready: readonly unknown[]
    readonly manifest: readonly unknown[]
    readonly wireOps: readonly unknown[]
    readonly completeBaseReachedServer: boolean
    readonly serverBody: string | null
  }>
}

let stopLifecycle: (() => void) | null = null
let lifecyclePulls = 0
let lifecyclePushedAnnotationIds: string[] = []

declare global {
  interface Window {
    thoughtRepairHarness: ThoughtRepairHarness
  }
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error ?? new Error('IndexedDB request failed'))
  })
}

function transactionComplete(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error ?? new Error('IndexedDB transaction failed'))
    transaction.onabort = () => reject(transaction.error ?? new Error('IndexedDB transaction aborted'))
  })
}

async function readAll(database: IDBDatabase, store: string): Promise<unknown[]> {
  const transaction = database.transaction(store, 'readonly')
  const request = transaction.objectStore(store).getAll() as IDBRequest<unknown[]>
  await transactionComplete(transaction)
  return request.result
}

async function reset(): Promise<void> {
  stopLifecycle?.()
  stopLifecycle = null
  lifecyclePulls = 0
  lifecyclePushedAnnotationIds = []
  resetUserDataDatabaseHandle()
  await requestResult(indexedDB.deleteDatabase(DATABASE_NAME))
}

async function createV4Quarantine(): Promise<IDBDatabase> {
  const database = await createV4Tail('update')
  const transaction = database.transaction(ANNOTATION_OPS_STORE, 'readwrite')
  transaction.objectStore(ANNOTATION_OPS_STORE).put({
    sequence: 10, opId: 'browser-invalid-quote', namespace: 'browser-v4', linkId: 'browser-link',
    target: { kind: 'saved-content', contentRevision: 7 }, targetKey: 'saved-content:7',
    annotationId: 'browser-bad', kind: 'add',
    annotation: {
      id: 'browser-bad', blockKey: 'content-document', start: 0, end: 1, text: 'x', note: '',
      source: 'self', createdAt: 1, updatedAt: 1, sourceContentRevision: 7,
      quote: { exact: 7 },
    },
  })
  await transactionComplete(transaction)
  return database
}

async function createV4Tail(kind: 'update' | 'delete'): Promise<IDBDatabase> {
  const request = indexedDB.open(DATABASE_NAME, 4)
  request.onupgradeneeded = () => {
    const database = request.result
    database.createObjectStore(LEGACY_PENDING_STORE, { keyPath: 'id' })
    const archive = database.createObjectStore(LEGACY_ARCHIVE_STORE, { keyPath: 'archiveID', autoIncrement: true })
    archive.createIndex('by-imported-namespace', 'importedIntoNamespace')
    archive.createIndex('by-fingerprint-version', 'fingerprintVersion')
    database.createObjectStore(LEGACY_DEDUP_STORE, { keyPath: 'id' })
    database.createObjectStore(MIGRATION_DECISION_STORE, { keyPath: 'id' })
    const operations = database.createObjectStore(ANNOTATION_OPS_STORE, { keyPath: 'sequence', autoIncrement: true })
    operations.createIndex('by-op-id', 'opId', { unique: true })
    operations.createIndex('by-target-sequence', ['namespace', 'linkId', 'targetKey', 'sequence'])
    database.createObjectStore(ANNOTATION_MATERIALIZED_STORE, { keyPath: 'key' })
    database.createObjectStore(ANNOTATION_LINK_STATE_STORE, { keyPath: 'key' })
    const links = database.createObjectStore(ANNOTATED_LINKS_STORE, { keyPath: 'key' })
    links.createIndex('by-namespace', 'namespace')
    database.createObjectStore(ANNOTATION_IMPORTS_STORE, { keyPath: 'key' })
  }
  const database = await requestResult(request)
  const target = { kind: 'saved-content', contentRevision: 7 }
  const targetKey = 'saved-content:7'
  const annotation = {
    id: 'tail-a1', blockKey: 'content-document', start: 0, end: 4, text: 'tail', note: 'base',
    source: 'self', createdAt: 1, updatedAt: 1, sourceContentRevision: 7,
  }
  const transaction = database.transaction([
    ANNOTATION_OPS_STORE,
    ANNOTATION_MATERIALIZED_STORE,
    ANNOTATION_LINK_STATE_STORE,
  ], 'readwrite')
  transaction.objectStore(ANNOTATION_OPS_STORE).put({
    sequence: 9, opId: `tail-${kind}`, namespace: 'browser-v4', linkId: 'browser-link',
    target, targetKey, annotationId: 'tail-a1', kind,
    ...(kind === 'update' ? { patch: { note: 'updated', source: 'ai', updatedAt: 2 } } : {}),
  })
  transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).put({
    key: ['browser-v4', 'browser-link', targetKey, 'tail-a1'], namespace: 'browser-v4',
    linkId: 'browser-link', target, targetKey, annotationId: 'tail-a1', sequence: 8,
    annotation, fallbackAnnotation: annotation,
  })
  transaction.objectStore(ANNOTATION_LINK_STATE_STORE).put({
    key: ['browser-v4', 'browser-link', targetKey], namespace: 'browser-v4', linkId: 'browser-link',
    target, targetKey, version: 1, activeCount: 1, compactedThroughSequence: 8,
  })
  await transactionComplete(transaction)
  return database
}

window.thoughtRepairHarness = {
  reset,
  async startQuarantineLifecycle() {
    await reset()
    const legacy = await createV4Quarantine()
    legacy.close()
    resetUserDataDatabaseHandle()
    const upgraded = await openUserDataDatabase()
    if (!upgraded) throw new Error('quarantine database did not upgrade')
    const lease = new IdentityLease({
      serverClientDataNamespace: 'browser-server', physicalNamespace: 'browser-v4', localEpoch: 1,
    })
    const client = {
      pushThoughtOps: async (request: { ops: Array<Record<string, unknown>> }) => {
        lifecyclePushedAnnotationIds.push(...request.ops.map((operation) =>
          operation.annotation_id as string))
        return {
          ok: true as const,
          data: request.ops.map((operation, index) => ({
            contract_version: 1 as const, op_id: operation.op_id as string, sequence: index + 1,
            disposition: 'applied' as const,
            submitted_key: {
              logical_clock: operation.logical_clock as number,
              device_id: operation.device_id as string,
              op_id: operation.op_id as string,
            },
            current_winner_key: {
              logical_clock: operation.logical_clock as number,
              device_id: operation.device_id as string,
              op_id: operation.op_id as string,
            },
          })),
        }
      },
      syncThoughts: async () => {
        lifecyclePulls += 1
        return { ok: true as const, data: { contract_version: 1 as const, items: [] } }
      },
    } as unknown as IdentityBoundReaderClient
    const startupComplete = new Promise<void>((resolve) => {
      window.addEventListener('webtag:thoughts-sync', () => resolve(), { once: true })
    })
    stopLifecycle = startThoughtSync(lease, client)
    await startupComplete
  },
  lifecycleMetrics() {
    return { pulls: lifecyclePulls, pushedAnnotationIds: [...lifecyclePushedAnnotationIds] }
  },
  async migrateTail(kind) {
    await reset()
    const legacy = await createV4Tail(kind)
    const legacyBefore = await readAll(legacy, ANNOTATION_OPS_STORE)
    legacy.close()
    resetUserDataDatabaseHandle()
    const upgraded = await openUserDataDatabase()
    if (!upgraded) throw new Error('v6 upgrade failed')
    const legacyAfter = await readAll(upgraded, ANNOTATION_OPS_STORE)
    const ready = await readAll(upgraded, THOUGHT_REPAIR_READY_STORE)
    const manifest = await readAll(upgraded, THOUGHT_REPAIR_MANIFEST_STORE)
    const sources = await readAll(upgraded, THOUGHT_REPAIR_SOURCE_STORE)
    if (sources.length === 0) throw new Error('repair source receipt missing')
    const lease = new IdentityLease({
      serverClientDataNamespace: 'browser-server', physicalNamespace: 'browser-v4', localEpoch: 1,
    })
    const wireOps: unknown[] = []
    let completeBaseReachedServer = false
    let serverBody: string | null = null
    const client = {
      pushThoughtOps: async (request: { ops: Array<Record<string, unknown>> }) => {
        for (const operation of request.ops) {
          wireOps.push(operation)
          const payload = operation.payload as Record<string, unknown> | undefined
          if (operation.operation_kind === 'add') {
            completeBaseReachedServer = Boolean(payload && typeof payload.body === 'string' &&
              payload.annotation && payload.quote && operation.target)
            serverBody = typeof payload?.body === 'string' ? payload.body : null
          } else {
            if (!completeBaseReachedServer) throw new Error('tail reached empty server before complete add')
            if (operation.operation_kind === 'update' && typeof payload?.body === 'string') {
              serverBody = payload.body
            }
            if (operation.operation_kind === 'delete') serverBody = null
          }
        }
        return {
          ok: true as const,
          data: request.ops.map((operation, index) => ({
            contract_version: 1 as const, op_id: operation.op_id as string, sequence: index + 1,
            disposition: 'applied' as const,
            submitted_key: {
              logical_clock: operation.logical_clock as number,
              device_id: operation.device_id as string,
              op_id: operation.op_id as string,
            },
            current_winner_key: {
              logical_clock: operation.logical_clock as number,
              device_id: operation.device_id as string,
              op_id: operation.op_id as string,
            },
          })),
        }
      },
      syncThoughts: async () => ({
        ok: true as const, data: { contract_version: 1 as const, items: [] },
      }),
    } as unknown as IdentityBoundReaderClient
    const result = await syncThoughts(lease, client)
    if (result.status !== 'synced') throw new Error(`repair sync failed: ${result.status}`)
    return { legacyBefore, legacyAfter, ready, manifest, wireOps, completeBaseReachedServer, serverBody }
  },
}
