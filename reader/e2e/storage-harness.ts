import { IdentityAuthority, readerIdentity } from '../src/lib/identity'
import type { Annotation } from '../src/lib/annotations'
import {
  CACHE_SCHEMA_VERSION,
  META_STORE_NAME,
  PAYLOAD_STORE_NAME,
  createPersistedRecord,
  idbClear,
  idbGetAll,
  idbGetMetadata,
  idbPut,
  idbPutWithinQuota,
  resetDatabaseHandle,
} from '../src/lib/cache/idb'
import { cacheStorageQueue, NamespaceStorageQueue } from '../src/lib/cache/io-queue'
import {
  deletePersistedPrefix,
  getCacheMaintenanceStats,
  hydrateFromDisk,
  MAX_PERSISTED_BYTES,
  startPersistence,
} from '../src/lib/cache/persist'
import { resourceStore } from '../src/lib/cache/store'
import {
  getLegacyImportPrompt,
  importLegacyData,
  quarantineLegacyData,
  resetUserDataDatabaseHandle,
} from '../src/lib/legacy-user-data'
import {
  commitAnnotationOperation,
  enumerateAnnotatedLinkIds,
  listAnnotatedLinks,
  readAnnotationSnapshot,
} from '../src/lib/user-data/annotation-store'
import type { AnnotationTarget } from '../src/lib/annotation-domain'
import type {
  SavedContentAnnotationAddDraft,
} from '../src/lib/user-data/annotation-types'
import {
  ANNOTATED_LINKS_STORE,
  ANNOTATION_OPS_STORE,
  LEGACY_ARCHIVE_STORE,
  LEGACY_PENDING_STORE,
  runUserDataTransaction,
} from '../src/lib/user-data/idb'
import {
  ownedDatabaseName,
  readOwnedStorage,
  readOwnedStorageForLease,
} from '../src/lib/storage-ownership'

interface CacheIsolationOperationProbe {
  readonly sequence: number
  readonly opId: string
  readonly logicalOpId: string
  readonly namespace: string
  readonly linkId: string
  readonly targetKey: string
  readonly annotationId: string
  readonly kind: string
}

interface CacheIsolationAnnotatedLinkProbe {
  readonly key: readonly [string, string, string]
  readonly namespace: string
  readonly linkId: string
  readonly targetKey: string
  readonly annotationCount: number
  readonly annotationStoreVersion: number
}

interface CacheIsolationLegacyPendingProbe {
  readonly id: string
  readonly legacyKey: string
  readonly value: string
  readonly quarantinedAt: number
}

interface CacheIsolationLegacyArchiveProbe {
  readonly archiveID: number
  readonly id: string
  readonly legacyKey: string
  readonly value: string
  readonly quarantinedAt: number
  readonly importedIntoNamespace: string
  readonly importedAt: number
  readonly fingerprintVersion: number
  readonly fingerprints: readonly string[]
}

export interface CacheIsolationUserDataSnapshot {
  readonly schema: {
    readonly version: number
    readonly stores: readonly string[]
  }
  readonly annotationSnapshot: Awaited<ReturnType<typeof readAnnotationSnapshot>>
  readonly annotatedLinks: Awaited<ReturnType<typeof listAnnotatedLinks>>
  readonly annotatedLinkIds: Awaited<ReturnType<typeof enumerateAnnotatedLinkIds>>
  readonly operations: readonly CacheIsolationOperationProbe[]
  readonly indexRecords: readonly CacheIsolationAnnotatedLinkProbe[]
  readonly legacyPending: CacheIsolationLegacyPendingProbe | null
  readonly legacyArchive: readonly CacheIsolationLegacyArchiveProbe[]
}

type CacheIsolationRawUserDataSnapshot = Pick<
  CacheIsolationUserDataSnapshot,
  'schema' | 'operations' | 'indexRecords' | 'legacyPending' | 'legacyArchive'
>

export interface CacheIsolationCacheRecordSnapshot {
  readonly key: string
  readonly namespace: string
  readonly logicalKey: string
  readonly schema: number
  readonly data: unknown
  readonly updatedAt: number
  readonly size: number
  readonly generation: number
}

export interface CacheIsolationCacheSnapshot {
  readonly version: number
  readonly stores: readonly string[]
  readonly records: readonly CacheIsolationCacheRecordSnapshot[]
}

export interface CacheIsolationUpgradeResult {
  readonly injectedFailures: number
  readonly attemptedRecords: readonly CacheIsolationCacheRecordSnapshot[]
  readonly prototypeRestored: boolean
  readonly rolledBackCache: CacheIsolationCacheSnapshot
}

export interface CacheIsolationQuotaResult {
  readonly oldStored: boolean
  readonly admission: Awaited<ReturnType<typeof idbPutWithinQuota>>
  readonly cache: CacheIsolationCacheSnapshot
}

export interface StorageHarnessResult {
  prefixDelete: {
    settledBeforeSwitch: boolean
    records: string[]
  }
  hydrate: {
    settledBeforeSwitch: boolean
    restored: number
    activeNamespace: string | null
    hasProbe: boolean
  }
  transactionAbort: {
    switchedAfterStart: boolean
    records: string[]
  }
  legacyImportRace: {
    firstImportStatus: string
    lateQuarantined: number
    secondPrompt: boolean
    secondImportStatus: string
    aPinsBeforeSwitch: string | null
    bPins: string | null
  }
}

export interface LegacyPageImportResult {
  harnessInstanceID: string
  physicalNamespace: string
  status: Awaited<ReturnType<typeof importLegacyData>>['status']
  imported: number
  pins: string | null
}

declare global {
  interface Window {
    rf2bStorageHarness: {
      run(): Promise<StorageHarnessResult>
      prepareLegacyImport(): Promise<void>
      importLegacyInto(
        serverNamespace: string,
        physicalNamespace: string,
      ): Promise<LegacyPageImportResult>
      closeLegacyDatabase(): void
      cleanupLegacyImport(): Promise<void>
      resetRevalidationStorage(): Promise<void>
      installRevalidationIdentity(namespace: string): void
      setRevalidationValue(key: string, value: string): void
      setRevalidationPayloadBytes(key: string, bytes: number): void
      startRevalidationPersistence(): void
      stopRevalidationPersistence(): void
      invalidateAndWait(prefix: string): Promise<void>
      waitForStorageQueue(): Promise<void>
      revalidationSnapshot(key: string): {
        hasData: boolean
        desiredGeneration: number
      }
      cacheQuotaSnapshot(): Promise<{
        knownBytes: number
        durableBytes: number
        records: number
        maxBytes: number
      }>
      resetCacheIsolationDatabases(): Promise<void>
      seedCacheIsolationUserData(): Promise<CacheIsolationUserDataSnapshot>
      readCacheIsolationUserData(): Promise<CacheIsolationUserDataSnapshot>
      readCacheIsolationCache(): Promise<CacheIsolationCacheSnapshot>
      runCacheIsolationUpgradeFailure(): Promise<CacheIsolationUpgradeResult>
      runCacheIsolationQuotaEviction(): Promise<CacheIsolationQuotaResult>
      blockActiveCacheQueue(): Promise<void>
      waitForBlockedLeaseRevocation(): Promise<void>
      releaseActiveCacheQueueAndDrain(): Promise<void>
    }
  }
}

const delay = (milliseconds: number) =>
  new Promise<void>((resolve) => window.setTimeout(resolve, milliseconds))
const harnessInstanceID = crypto.randomUUID()
const concurrentLegacyValue = '{"tags":["two-page-claim"],"domains":[]}'
const CACHE_DATABASE_NAME = ownedDatabaseName('cacheDatabase')
const USER_DATA_DATABASE_NAME = ownedDatabaseName('userDataDatabase')
const LEGACY_CACHE_STORE = 'resources'
const CACHE_CONTROL_STORE = 'cache_control'
const CACHE_ISOLATION_NAMESPACE = 'rf5b-cache-isolation'
const CACHE_ISOLATION_LINK_ID = 'durable-user-data-link'
const CACHE_ISOLATION_TARGET = {
  kind: 'saved-content',
  contentRevision: 7,
} as const satisfies AnnotationTarget
const CACHE_ISOLATION_ANNOTATION = {
  id: 'durable-annotation',
  blockKey: 'content-document',
  start: 0,
  end: 7,
  text: 'durable',
  note: 'must survive cache maintenance',
  source: 'self',
  createdAt: 100,
  updatedAt: 100,
  sourceContentRevision: CACHE_ISOLATION_TARGET.contentRevision,
} as const satisfies Annotation
const CACHE_ISOLATION_DRAFT = {
  id: CACHE_ISOLATION_ANNOTATION.id,
  blockKey: CACHE_ISOLATION_ANNOTATION.blockKey,
  start: CACHE_ISOLATION_ANNOTATION.start,
  end: CACHE_ISOLATION_ANNOTATION.end,
  text: CACHE_ISOLATION_ANNOTATION.text,
  note: CACHE_ISOLATION_ANNOTATION.note,
  source: CACHE_ISOLATION_ANNOTATION.source,
  createdAt: CACHE_ISOLATION_ANNOTATION.createdAt,
  updatedAt: CACHE_ISOLATION_ANNOTATION.updatedAt,
} as const satisfies SavedContentAnnotationAddDraft
let stopRevalidationPersistence: (() => void) | null = null

interface ActiveCacheQueueBarrier {
  lease: NonNullable<typeof readerIdentity.activeLease>
  release: () => void
  blocker: Promise<unknown>
  revoked: Promise<void>
}

let activeCacheQueueBarrier: ActiveCacheQueueBarrier | null = null

async function blockActiveCacheQueue(): Promise<void> {
  if (activeCacheQueueBarrier) {
    throw new Error('active cache queue is already blocked')
  }
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('cannot block cache queue without an active identity')

  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let markEntered!: () => void
  const entered = new Promise<void>((resolve) => {
    markEntered = resolve
  })
  let markRevoked!: () => void
  const revoked = new Promise<void>((resolve) => {
    markRevoked = resolve
  })
  const blocker = cacheStorageQueue.enqueue(
    lease,
    'browser active cache queue barrier',
    async (operation) => {
      const onAbort = () => markRevoked()
      operation.signal.addEventListener('abort', onAbort, { once: true })
      if (operation.signal.aborted) onAbort()
      markEntered()
      try {
        await gate
      } finally {
        operation.signal.removeEventListener('abort', onAbort)
      }
    },
  )
  activeCacheQueueBarrier = { lease, release, blocker, revoked }

  await Promise.race([
    entered,
    blocker.then(() => {
      throw new Error('cache queue barrier was skipped before it became active')
    }),
  ])
}

async function waitForBlockedLeaseRevocation(): Promise<void> {
  const barrier = activeCacheQueueBarrier
  if (!barrier) throw new Error('active cache queue is not blocked')
  await barrier.revoked
}

async function releaseActiveCacheQueueAndDrain(): Promise<void> {
  const barrier = activeCacheQueueBarrier
  if (!barrier) return
  activeCacheQueueBarrier = null

  const drain = cacheStorageQueue.enqueue(
    barrier.lease,
    'browser active cache queue drain',
    () => undefined,
  )
  barrier.release()
  await Promise.all([barrier.blocker, drain])
}

async function resetRevalidationStorage(): Promise<void> {
  stopRevalidationPersistence?.()
  stopRevalidationPersistence = null
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  await idbClear()
}

function installRevalidationIdentity(namespace: string): void {
  stopRevalidationPersistence?.()
  stopRevalidationPersistence = null
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  const lease = readerIdentity.install({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
  })
  resourceStore.activateIdentity(lease)
}

function setRevalidationValue(key: string, value: string): void {
  resourceStore.set(key, { value })
}

function setRevalidationPayloadBytes(key: string, bytes: number): void {
  resourceStore.set(key, { payload: 'x'.repeat(bytes) })
}

function startRevalidationPersistence(): void {
  stopRevalidationPersistence?.()
  stopRevalidationPersistence = startPersistence({ debounceMs: 10 })
}

function stopRevalidationListener(): void {
  stopRevalidationPersistence?.()
  stopRevalidationPersistence = null
}

async function waitForStorageQueue(): Promise<void> {
  const lease = readerIdentity.activeLease
  if (!lease) return
  await cacheStorageQueue.enqueue(lease, 'browser await cache storage', () => undefined)
}

async function invalidateAndWait(prefix: string): Promise<void> {
  resourceStore.invalidate(prefix)
  await waitForStorageQueue()
}

function revalidationSnapshot(key: string): {
  hasData: boolean
  desiredGeneration: number
} {
  return {
    hasData: resourceStore.has(key),
    desiredGeneration: resourceStore.peek(key).desiredGeneration,
  }
}

async function cacheQuotaSnapshot(): Promise<{
  knownBytes: number
  durableBytes: number
  records: number
  maxBytes: number
}> {
  const metadata = await idbGetMetadata()
  return {
    knownBytes: getCacheMaintenanceStats().knownBytes,
    durableBytes: metadata.reduce((sum, record) => sum + record.size, 0),
    records: metadata.length,
    maxBytes: MAX_PERSISTED_BYTES,
  }
}

function waitForOpenRequest(request: IDBOpenDBRequest): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error ?? new Error('failed to open IndexedDB'))
    request.onblocked = () => reject(new Error('opening IndexedDB was blocked'))
  })
}

function deleteIndexedDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error(`failed to delete ${name}`))
    request.onblocked = () => reject(new Error(`deleting ${name} was blocked`))
  })
}

async function resetCacheIsolationCacheDatabase(): Promise<void> {
  resetDatabaseHandle()
  await delay(0)
  await deleteIndexedDatabase(CACHE_DATABASE_NAME)
}

async function resetCacheIsolationDatabases(): Promise<void> {
  stopRevalidationPersistence?.()
  stopRevalidationPersistence = null
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  resetDatabaseHandle()
  resetUserDataDatabaseHandle()
  await delay(0)
  await deleteIndexedDatabase(CACHE_DATABASE_NAME)
  await deleteIndexedDatabase(USER_DATA_DATABASE_NAME)
}

function cacheIsolationLease(): NonNullable<typeof readerIdentity.activeLease> {
  const active = readerIdentity.activeLease
  if (
    active?.context.serverClientDataNamespace === 'server-rf5b-cache-isolation' &&
    active.context.physicalNamespace === CACHE_ISOLATION_NAMESPACE
  ) {
    return active
  }
  readerIdentity.clear()
  return readerIdentity.install({
    serverClientDataNamespace: 'server-rf5b-cache-isolation',
    physicalNamespace: CACHE_ISOLATION_NAMESPACE,
  })
}

async function readCacheIsolationUserData(): Promise<CacheIsolationUserDataSnapshot> {
  const lease = cacheIsolationLease()
  const annotationSnapshot = await readAnnotationSnapshot(
    lease,
    CACHE_ISOLATION_LINK_ID,
    CACHE_ISOLATION_TARGET,
  )
  const annotatedLinks = await listAnnotatedLinks(lease)
  const annotatedLinkIds = await enumerateAnnotatedLinkIds(lease)
  const rawSnapshot = await runUserDataTransaction<CacheIsolationRawUserDataSnapshot>(
    lease,
    'inspect browser cache isolation fixtures',
    [
      ANNOTATION_OPS_STORE,
      ANNOTATED_LINKS_STORE,
      LEGACY_PENDING_STORE,
      LEGACY_ARCHIVE_STORE,
    ],
    'readonly',
    (transaction, _operation, setResult) => {
      const operations = transaction.objectStore(ANNOTATION_OPS_STORE).getAll() as IDBRequest<
        CacheIsolationOperationProbe[]
      >
      const indexRecords = transaction.objectStore(ANNOTATED_LINKS_STORE).getAll() as IDBRequest<
        CacheIsolationAnnotatedLinkProbe[]
      >
      const legacyPending = transaction.objectStore(LEGACY_PENDING_STORE).get(
        'pins',
      ) as IDBRequest<CacheIsolationLegacyPendingProbe | undefined>
      const legacyArchive = transaction.objectStore(LEGACY_ARCHIVE_STORE).getAll() as IDBRequest<
        CacheIsolationLegacyArchiveProbe[]
      >
      let completed = 0
      const finish = () => {
        completed += 1
        if (completed !== 4) return
        setResult({
          schema: {
            version: transaction.db.version,
            stores: [...transaction.db.objectStoreNames].sort(),
          },
          operations: operations.result.map((record) => ({
            sequence: record.sequence,
            opId: record.opId,
            logicalOpId: record.logicalOpId,
            namespace: record.namespace,
            linkId: record.linkId,
            targetKey: record.targetKey,
            annotationId: record.annotationId,
            kind: record.kind,
          })).sort((left, right) => left.sequence - right.sequence),
          indexRecords: indexRecords.result.map((record) => ({
            key: [record.key[0], record.key[1], record.key[2]] as const,
            namespace: record.namespace,
            linkId: record.linkId,
            targetKey: record.targetKey,
            annotationCount: record.annotationCount,
            annotationStoreVersion: record.annotationStoreVersion,
          })).sort((left, right) =>
            left.linkId.localeCompare(right.linkId) ||
            left.targetKey.localeCompare(right.targetKey)),
          legacyPending: legacyPending.result
            ? {
                id: legacyPending.result.id,
                legacyKey: legacyPending.result.legacyKey,
                value: legacyPending.result.value,
                quarantinedAt: legacyPending.result.quarantinedAt,
              }
            : null,
          legacyArchive: legacyArchive.result.map((record) => ({
            archiveID: record.archiveID,
            id: record.id,
            legacyKey: record.legacyKey,
            value: record.value,
            quarantinedAt: record.quarantinedAt,
            importedIntoNamespace: record.importedIntoNamespace,
            importedAt: record.importedAt,
            fingerprintVersion: record.fingerprintVersion,
            fingerprints: record.fingerprints,
          })).sort((left, right) => left.archiveID - right.archiveID),
        })
      }
      for (const request of [operations, indexRecords, legacyPending, legacyArchive]) {
        request.onerror = () => transaction.abort()
        request.onsuccess = finish
      }
    },
  )
  if (!rawSnapshot.ok) {
    throw new Error('failed to inspect cache-isolation user data')
  }
  return {
    ...rawSnapshot.value,
    annotationSnapshot,
    annotatedLinks,
    annotatedLinkIds,
  }
}

async function seedCacheIsolationUserData(): Promise<CacheIsolationUserDataSnapshot> {
  const lease = cacheIsolationLease()
  const committed = await commitAnnotationOperation(lease, {
    kind: 'add',
    opId: 'durable-operation',
    linkId: CACHE_ISOLATION_LINK_ID,
    target: CACHE_ISOLATION_TARGET,
    draft: CACHE_ISOLATION_DRAFT,
  })
  if (!committed.ok || committed.value.status !== 'committed') {
    throw new Error(`failed to seed cache-isolation annotation: ${JSON.stringify(committed)}`)
  }

  const fixtures = await runUserDataTransaction(
    lease,
    'seed browser cache isolation recovery fixtures',
    [LEGACY_PENDING_STORE, LEGACY_ARCHIVE_STORE],
    'readwrite',
    (transaction, _operation, setResult) => {
      transaction.objectStore(LEGACY_PENDING_STORE).put({
        id: 'pins',
        legacyKey: 'webtag:pins:v1',
        value: '{"tags":["quarantined"],"domains":[]}',
        quarantinedAt: 101,
      })
      transaction.objectStore(LEGACY_ARCHIVE_STORE).add({
        id: 'annotationsV1',
        legacyKey: 'webtag:annotations:v1',
        value: '{"archived-link":[{"id":"archived"}]}',
        quarantinedAt: 102,
        importedIntoNamespace: CACHE_ISOLATION_NAMESPACE,
        importedAt: 103,
        fingerprintVersion: 1,
        fingerprints: ['archive-fingerprint'],
      })
      setResult(true)
    },
  )
  if (!fixtures.ok || fixtures.value !== true) {
    throw new Error('failed to seed cache-isolation recovery fixtures')
  }
  return readCacheIsolationUserData()
}

async function openExistingCacheDatabase(): Promise<IDBDatabase> {
  const request = indexedDB.open(CACHE_DATABASE_NAME)
  request.onupgradeneeded = () => request.transaction?.abort()
  return waitForOpenRequest(request)
}

async function readCacheIsolationCache(): Promise<CacheIsolationCacheSnapshot> {
  const database = await openExistingCacheDatabase()
  try {
    const stores = [...database.objectStoreNames].sort()
    const hasSplitSchema = database.objectStoreNames.contains(PAYLOAD_STORE_NAME) &&
      database.objectStoreNames.contains(META_STORE_NAME)
    const records = await new Promise<CacheIsolationCacheRecordSnapshot[]>((resolve, reject) => {
      if (!hasSplitSchema) {
        if (!database.objectStoreNames.contains(LEGACY_CACHE_STORE)) {
          resolve([])
          return
        }
        const transaction = database.transaction(LEGACY_CACHE_STORE, 'readonly')
        const request = transaction.objectStore(LEGACY_CACHE_STORE).getAll() as IDBRequest<
          CacheIsolationCacheRecordSnapshot[]
        >
        transaction.oncomplete = () => resolve(
          [...request.result].sort((left, right) => left.key.localeCompare(right.key)),
        )
        transaction.onerror = () => reject(transaction.error)
        transaction.onabort = () => reject(transaction.error)
        return
      }

      const transaction = database.transaction(
        [PAYLOAD_STORE_NAME, META_STORE_NAME],
        'readonly',
      )
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME).getAll() as IDBRequest<
        Array<{ key: string; data: unknown }>
      >
      const metadata = transaction.objectStore(META_STORE_NAME).getAll() as IDBRequest<
        Array<{
          key: string
          namespace: string
          logicalKey: string
          schema: number
          size: number
          lastAccess: number
          generation: number
        }>
      >
      transaction.oncomplete = () => {
        const payloadByKey = new Map(payloads.result.map((item) => [item.key, item.data]))
        resolve(metadata.result.flatMap((item) =>
          payloadByKey.has(item.key)
            ? [{
                key: item.key,
                namespace: item.namespace,
                logicalKey: item.logicalKey,
                schema: item.schema,
                data: payloadByKey.get(item.key),
                updatedAt: item.lastAccess,
                size: item.size,
                generation: item.generation,
              }]
            : []).sort((left, right) => left.key.localeCompare(right.key)))
      }
      transaction.onerror = () => reject(transaction.error)
      transaction.onabort = () => reject(transaction.error)
    })
    return { version: database.version, stores, records }
  } finally {
    database.close()
  }
}

async function createVersionTwoCacheDatabase(): Promise<void> {
  const request = indexedDB.open(CACHE_DATABASE_NAME, 2)
  request.onupgradeneeded = () => {
    request.result.createObjectStore(LEGACY_CACHE_STORE, { keyPath: 'key' })
    request.result.createObjectStore(CACHE_CONTROL_STORE, { keyPath: 'key' })
  }
  const database = await waitForOpenRequest(request)
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(LEGACY_CACHE_STORE, 'readwrite')
    transaction.objectStore(LEGACY_CACHE_STORE).put(createPersistedRecord(
      CACHE_ISOLATION_NAMESPACE,
      'GET /api/links?legacy-cache',
      {
        schema: CACHE_SCHEMA_VERSION,
        data: { cache: 'legacy' },
        updatedAt: 1,
        size: 10,
      },
    ))
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  database.close()
}

async function runCacheIsolationUpgradeFailure(): Promise<CacheIsolationUpgradeResult> {
  await resetCacheIsolationCacheDatabase()
  await createVersionTwoCacheDatabase()

  const originalPut = IDBObjectStore.prototype.put
  let injectedFailures = 0
  IDBObjectStore.prototype.put = new Proxy(originalPut, {
    apply(target, thisArgument, argumentsList) {
      if ((thisArgument as IDBObjectStore).name === META_STORE_NAME) {
        injectedFailures += 1
        throw new Error('injected browser cache migration failure')
      }
      return Reflect.apply(target, thisArgument, argumentsList) as IDBRequest<IDBValidKey>
    },
  })
  let attemptedRecords: CacheIsolationCacheRecordSnapshot[] = []
  try {
    resetDatabaseHandle()
    attemptedRecords = await idbGetAll()
  } finally {
    IDBObjectStore.prototype.put = originalPut
    resetDatabaseHandle()
    await delay(0)
  }

  return {
    injectedFailures,
    attemptedRecords,
    prototypeRestored: IDBObjectStore.prototype.put === originalPut,
    rolledBackCache: await readCacheIsolationCache(),
  }
}

async function runCacheIsolationQuotaEviction(): Promise<CacheIsolationQuotaResult> {
  await resetCacheIsolationCacheDatabase()
  const lease = cacheIsolationLease()
  const oldCache = createPersistedRecord(
    CACHE_ISOLATION_NAMESPACE,
    'GET /api/links?old-cache',
    {
      schema: CACHE_SCHEMA_VERSION,
      data: { cache: 'old' },
      updatedAt: 1,
      size: 80,
    },
  )
  const incomingCache = createPersistedRecord(
    CACHE_ISOLATION_NAMESPACE,
    'GET /api/links?incoming-cache',
    {
      schema: CACHE_SCHEMA_VERSION,
      data: { cache: 'incoming' },
      updatedAt: 2,
      size: 60,
    },
  )
  const oldStored = await idbPut(oldCache)
  const admission = await cacheStorageQueue.enqueue(
    lease,
    'browser cache-isolation quota admission',
    (operation) => idbPutWithinQuota(incomingCache, 100, operation),
  )
  if (admission === undefined) {
    throw new Error('cache-isolation quota admission was revoked')
  }
  return {
    oldStored,
    admission,
    cache: await readCacheIsolationCache(),
  }
}

async function runPrefixDeleteBarrier(): Promise<StorageHarnessResult['prefixDelete']> {
  await idbClear()
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  const leaseA = readerIdentity.install({
    serverClientDataNamespace: 'server-A',
    physicalNamespace: 'physical-A',
  })
  resourceStore.activateIdentity(leaseA)
  await idbPut(createPersistedRecord('physical-A', 'GET /api/links?owner=A', {
    schema: CACHE_SCHEMA_VERSION,
    data: { owner: 'A' },
    updatedAt: 1,
    size: 10,
  }))
  await idbPut(createPersistedRecord('physical-B', 'GET /api/links?owner=B', {
    schema: CACHE_SCHEMA_VERSION,
    data: { owner: 'B' },
    updatedAt: 2,
    size: 10,
  }))

  let releaseBarrier!: () => void
  const barrier = new Promise<void>((resolve) => {
    releaseBarrier = resolve
  })
  const blocker = cacheStorageQueue.enqueue(leaseA, 'browser prefix barrier', async () => barrier)
  await Promise.resolve()
  let settled = false
  const deletion = deletePersistedPrefix('GET /api/links', leaseA).then(() => {
    settled = true
  })
  await delay(30)
  const settledBeforeSwitch = settled

  const leaseB = readerIdentity.install({
    serverClientDataNamespace: 'server-B',
    physicalNamespace: 'physical-B',
  })
  resourceStore.activateIdentity(leaseB)
  releaseBarrier()
  await blocker
  await deletion
  const records = (await idbGetAll())
    .map((record) => `${record.namespace}:${record.logicalKey}`)
    .sort()
  return { settledBeforeSwitch, records }
}

async function runHydrateBarrier(): Promise<StorageHarnessResult['hydrate']> {
  await idbClear()
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  const leaseA = readerIdentity.install({
    serverClientDataNamespace: 'server-A',
    physicalNamespace: 'physical-A',
  })
  resourceStore.activateIdentity(leaseA)
  const probeKey = 'GET /api/links?browser-hydrate'
  await idbPut(createPersistedRecord('physical-A', probeKey, {
    schema: CACHE_SCHEMA_VERSION,
    data: { owner: 'A' },
    updatedAt: 1,
    size: 10,
  }))
  resourceStore.clear()

  let releaseBarrier!: () => void
  const barrier = new Promise<void>((resolve) => {
    releaseBarrier = resolve
  })
  const blocker = cacheStorageQueue.enqueue(leaseA, 'browser hydrate barrier', async () => barrier)
  await Promise.resolve()
  let settled = false
  const hydration = hydrateFromDisk(undefined, leaseA).then((restored) => {
    settled = true
    return restored
  })
  await delay(30)
  const settledBeforeSwitch = settled

  const leaseB = readerIdentity.install({
    serverClientDataNamespace: 'server-B',
    physicalNamespace: 'physical-B',
  })
  resourceStore.activateIdentity(leaseB)
  releaseBarrier()
  await blocker
  const restored = await hydration
  return {
    settledBeforeSwitch,
    restored,
    activeNamespace: resourceStore.activePhysicalNamespace,
    hasProbe: resourceStore.has(probeKey),
  }
}

async function runStartedTransactionAbort(): Promise<StorageHarnessResult['transactionAbort']> {
  await idbClear()
  const authority = new IdentityAuthority()
  const queue = new NamespaceStorageQueue()
  const leaseA = authority.install({
    serverClientDataNamespace: 'server-A',
    physicalNamespace: 'physical-A',
  })
  const originalTransaction = IDBDatabase.prototype.transaction
  let switchedAfterStart = false
  const interceptedTransaction = new Proxy(originalTransaction, {
    apply(target, thisArgument, argumentsList) {
      const transaction = Reflect.apply(target, thisArgument, argumentsList) as IDBTransaction
      if (argumentsList[1] === 'readwrite' && !switchedAfterStart) {
        switchedAfterStart = true
        queueMicrotask(() => {
          authority.install({
            serverClientDataNamespace: 'server-B',
            physicalNamespace: 'physical-B',
          })
        })
      }
      return transaction
    },
  })
  IDBDatabase.prototype.transaction = interceptedTransaction
  try {
    await queue.enqueue(leaseA, 'browser started transaction', async (operation) => {
      const write = operation.commit(() =>
        idbPutWithinQuota(
          createPersistedRecord('physical-A', 'GET /api/links?started=A', {
            schema: CACHE_SCHEMA_VERSION,
            data: { owner: 'A' },
            updatedAt: 1,
            size: 10,
          }),
          MAX_PERSISTED_BYTES,
          operation,
        ),
      )
      if (write) await write
    })
  } finally {
    IDBDatabase.prototype.transaction = originalTransaction
  }
  const records = (await idbGetAll()).map((record) => record.key).sort()
  return { switchedAfterStart, records }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
}

async function prepareLegacyImport(): Promise<void> {
  await deleteUserDataDatabase()
  readerIdentity.clear()
  localStorage.clear()
  localStorage.setItem('webtag:pins:v1', concurrentLegacyValue)
  const result = await quarantineLegacyData()
  if (result.quarantined !== 1) {
    throw new Error(`expected one quarantined legacy asset, received ${result.quarantined}`)
  }
}

async function importLegacyInto(
  serverNamespace: string,
  physicalNamespace: string,
): Promise<LegacyPageImportResult> {
  const lease = readerIdentity.install({
    serverClientDataNamespace: serverNamespace,
    physicalNamespace,
  })
  const result = await importLegacyData(lease)
  return {
    harnessInstanceID,
    physicalNamespace,
    status: result.status,
    imported: result.imported,
    pins: readOwnedStorageForLease('pins', lease),
  }
}

function closeLegacyDatabase(): void {
  readerIdentity.clear()
  resetUserDataDatabaseHandle()
}

async function cleanupLegacyImport(): Promise<void> {
  closeLegacyDatabase()
  localStorage.clear()
  await deleteUserDataDatabase()
}

async function runLegacyImportRace(): Promise<StorageHarnessResult['legacyImportRace']> {
  await deleteUserDataDatabase()
  readerIdentity.clear()
  const legacyKey = 'webtag:pins:v1'
  const legacyValue = '{"tags":["single-claim"],"domains":[]}'
  localStorage.setItem(legacyKey, legacyValue)
  await quarantineLegacyData()

  localStorage.setItem(legacyKey, legacyValue)
  const leaseA = readerIdentity.install({
    serverClientDataNamespace: 'server-A',
    physicalNamespace: 'physical-A',
  })
  const startedImports: Array<ReturnType<typeof importLegacyData>> = []
  const originalGetItem = Storage.prototype.getItem
  Storage.prototype.getItem = new Proxy(originalGetItem, {
    apply(target, thisArgument, argumentsList) {
      const value = Reflect.apply(target, thisArgument, argumentsList) as string | null
      if (argumentsList[0] === legacyKey && startedImports.length === 0) {
        startedImports.push(importLegacyData(leaseA))
      }
      return value
    },
  })

  let lateQuarantined = 0
  try {
    lateQuarantined = (await quarantineLegacyData()).quarantined
  } finally {
    Storage.prototype.getItem = originalGetItem
  }
  const firstImport = await startedImports[0]
  if (!firstImport) throw new Error('legacy import did not start at the capture barrier')
  const aPinsBeforeSwitch = readOwnedStorageForLease('pins', leaseA)

  const leaseB = readerIdentity.install({
    serverClientDataNamespace: 'server-B',
    physicalNamespace: 'physical-B',
  })
  const secondPrompt = (await getLegacyImportPrompt(leaseB)) !== null
  const secondImport = await importLegacyData(leaseB)
  const bPins = readOwnedStorage('pins')
  const result = {
    firstImportStatus: firstImport.status,
    lateQuarantined,
    secondPrompt,
    secondImportStatus: secondImport.status,
    aPinsBeforeSwitch,
    bPins,
  }

  readerIdentity.clear()
  localStorage.clear()
  await deleteUserDataDatabase()
  return result
}

window.rf2bStorageHarness = {
  prepareLegacyImport,
  importLegacyInto,
  closeLegacyDatabase,
  cleanupLegacyImport,
  resetRevalidationStorage,
  installRevalidationIdentity,
  setRevalidationValue,
  setRevalidationPayloadBytes,
  startRevalidationPersistence,
  stopRevalidationPersistence: stopRevalidationListener,
  invalidateAndWait,
  waitForStorageQueue,
  revalidationSnapshot,
  cacheQuotaSnapshot,
  resetCacheIsolationDatabases,
  seedCacheIsolationUserData,
  readCacheIsolationUserData,
  readCacheIsolationCache,
  runCacheIsolationUpgradeFailure,
  runCacheIsolationQuotaEviction,
  blockActiveCacheQueue,
  waitForBlockedLeaseRevocation,
  releaseActiveCacheQueueAndDrain,
  async run(): Promise<StorageHarnessResult> {
    const prefixDelete = await runPrefixDeleteBarrier()
    const hydrate = await runHydrateBarrier()
    const transactionAbort = await runStartedTransactionAbort()
    const legacyImportRace = await runLegacyImportRace()
    readerIdentity.clear()
    resourceStore.deactivateIdentity()
    return { prefixDelete, hydrate, transactionAbort, legacyImportRace }
  },
}
