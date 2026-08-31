import { createElement, useEffect } from 'react'
import { createRoot, type Root } from 'react-dom/client'

import type { Annotation } from '../src/lib/annotations'
import {
  IdentityBoundReaderClient,
  type ReaderRequestOptions,
} from '../src/lib/api/client'
import type { ApiError, ApiResult } from '@webtag/api'
import type { LinkResponse } from '../src/lib/api/types'
import {
  AnnotationDocumentChannel,
  type AnnotationChangeHint,
} from '../src/lib/article/document-channel'
import {
  CACHE_SCHEMA_VERSION,
  createPersistedRecord,
  idbGetAll,
  idbPut,
} from '../src/lib/cache/idb'
import { clearPersistedCache } from '../src/lib/cache/persist'
import { resourceStore } from '../src/lib/cache/store'
import { useAnnotatedLinks } from '../src/hooks/useAnnotatedLinks'
import { IdentityAuthority, type IdentityLease } from '../src/lib/identity'
import {
  ownedDatabaseName,
  ownedStorageKeyForLease,
  readOwnedStorageForLease,
} from '../src/lib/storage-ownership'
import {
  commitAnnotationOperation,
  commitSupersessionRecovery,
  enumerateAnnotatedLinkIds,
  importAnnotationOperations,
  readAnnotationSnapshot,
  type AnnotationImportOperation,
  type SupersessionRecoveryResult,
} from '../src/lib/user-data/annotation-store'
import { listAnnotationOperationsForTest } from '../src/test/annotation-operations'
import type {
  AnnotationCommitResult,
  AnnotationOperationInput,
  AnnotationSnapshot,
  SavedContentAnnotationAddDraft,
  SavedContentAnnotationTarget,
} from '../src/lib/user-data/annotation-types'
import {
  THOUGHT_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
  runUserDataTransaction,
} from '../src/lib/user-data/idb'
import {
  isValidThoughtOutboxRecord,
  type ThoughtMaterializedRecord,
  type ThoughtSupersessionEventRecord,
} from '../src/lib/user-data/thought-types'

interface HarnessDocument {
  readonly namespace: string
  readonly linkId: string
  readonly contentRevision: number
}

interface HarnessOperationBase {
  readonly opId: string
  readonly annotationId: string
}

interface HarnessAddOperation extends HarnessOperationBase {
  readonly kind: 'add'
  readonly note: string
  readonly stamp: number
}

interface HarnessUpdateOperation extends HarnessOperationBase {
  readonly kind: 'update'
  readonly note: string
  readonly stamp: number
}

interface HarnessDeleteOperation extends HarnessOperationBase {
  readonly kind: 'delete'
}

type HarnessOperation =
  | HarnessAddOperation
  | HarnessUpdateOperation
  | HarnessDeleteOperation

type HarnessCommitTransactionResult =
  | { readonly ok: true; readonly value: AnnotationCommitResult }
  | { readonly ok: false }

interface HarnessProjection {
  readonly annotationStoreVersion: number
  readonly annotations: readonly Annotation[]
  readonly refreshCount: number
  readonly hintCount: number
  readonly lastHint: AnnotationChangeHint | null
  readonly error: string | null
}

interface HarnessWakeup {
  readonly key: string | null
  readonly value: string | null
}

interface HarnessThoughtKey {
  readonly opId: string
  readonly deviceId: string
  readonly logicalClock: number
}

interface HarnessLinkSeed {
  readonly id: string
  readonly createdAt: string
}

interface HarnessAnnotatedLinkSeedResult {
  readonly status: 'committed' | 'duplicate'
  readonly examined: number
  readonly applied: number
  readonly lastSequence: number
}

interface HarnessAnnotatedLinksProjection {
  readonly visibleLinkIds: readonly string[]
  readonly loading: boolean
  readonly error: ApiError | null
  readonly pointReadLinkIds: readonly string[]
  readonly activePointReads: number
  readonly maximumConcurrentPointReads: number
  readonly activePhysicalNamespace: string | null
  readonly clientOwnsActiveCacheIdentity: boolean
}

interface RF5BAnnotationsHarness {
  reset(): Promise<void>
  install(document: HarnessDocument): void
  close(): void
  startProjection(): Promise<HarnessProjection>
  dropHintChannel(): void
  disableBroadcastChannel(): void
  restoreBroadcastChannel(): void
  triggerVisibilityRefresh(): Promise<HarnessProjection>
  projection(): HarnessProjection
  commit(operation: HarnessOperation): Promise<HarnessCommitTransactionResult>
  commitAfterBarrier(
    barrierName: string,
    participants: number,
    operation: HarnessOperation,
  ): Promise<HarnessCommitTransactionResult>
  read(): Promise<AnnotationSnapshot>
  operationKinds(): Promise<ReadonlyArray<{ kind: string; sequence: number }>>
  thoughtOutboxKeys(): Promise<readonly HarnessThoughtKey[]>
  seedSupersessionRecovery(): Promise<void>
  recoverSupersession(): Promise<
    { readonly ok: true; readonly value: SupersessionRecoveryResult } | { readonly ok: false }
  >
  annotatedLinkIds(): Promise<readonly string[]>
  seedAnnotatedLinkIds(linkIds: readonly string[]): Promise<HarnessAnnotatedLinkSeedResult>
  startAnnotatedLinksProjection(links: readonly HarnessLinkSeed[]): void
  annotatedLinksProjection(): HarnessAnnotatedLinksProjection
  wakeup(): HarnessWakeup
  seedCache(logicalKey: string): Promise<boolean>
  cacheRecordCount(): Promise<number>
  clearCache(): Promise<void>
}

declare global {
  interface Window {
    rf5bAnnotationsHarness: RF5BAnnotationsHarness
  }
}

const authority = new IdentityAuthority()
const nativeBroadcastChannel = globalThis.BroadcastChannel

let lease: IdentityLease | null = null
let activeDocument: HarnessDocument | null = null
let channel: AnnotationDocumentChannel | null = null
let projectionSnapshot: AnnotationSnapshot | null = null
let projectionRefreshCount = 0
let projectionHintCount = 0
let projectionLastHint: AnnotationChangeHint | null = null
let projectionError: string | null = null
let refreshTail: Promise<void> = Promise.resolve()
let visibilityListening = false
let annotationSeedBatch = 0

interface HarnessPointReadStats {
  readonly linkIds: string[]
  active: number
  maximumActive: number
}

interface HarnessAnnotatedLinksState {
  readonly visibleLinkIds: readonly string[]
  readonly loading: boolean
  readonly error: ApiError | null
}

let annotatedLinksRoot: Root | null = null
let annotatedLinksContainer: HTMLElement | null = null
let annotatedLinksClient: HarnessReaderClient | null = null
let annotatedLinksState: HarnessAnnotatedLinksState = {
  visibleLinkIds: [],
  loading: false,
  error: null,
}
let pointReadStats: HarnessPointReadStats = {
  linkIds: [],
  active: 0,
  maximumActive: 0,
}

function linkResponse(seed: HarnessLinkSeed): LinkResponse {
  return {
    id: seed.id,
    url: `https://example.test/${encodeURIComponent(seed.id)}`,
    title: `RF5B browser link ${seed.id}`,
    summary: `Browser IndexedDB projection fixture for ${seed.id}`,
    description: null,
    tags: ['rf5b-browser'],
    content_type: 'article',
    library_kind: 'reading',
    status: 'done',
    domain: 'example.test',
    path_depth: 1,
    parent_id: null,
    created_at: seed.createdAt,
    updated_at: seed.createdAt,
    fetcher_type: 'http',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    parent_path: null,
    metadata_revision: 1,
    has_content: false,
  }
}

function waitForPointRead(signal: AbortSignal | undefined): Promise<boolean> {
  if (signal?.aborted) return Promise.resolve(false)
  return new Promise((resolve) => {
    const finish = (completed: boolean) => {
      window.clearTimeout(timer)
      signal?.removeEventListener('abort', onAbort)
      resolve(completed)
    }
    const onAbort = () => finish(false)
    const timer = window.setTimeout(() => finish(true), 5)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

class HarnessReaderClient extends IdentityBoundReaderClient {
  private readonly linksById: ReadonlyMap<string, LinkResponse>

  constructor(
    currentLease: IdentityLease,
    links: readonly HarnessLinkSeed[],
    private readonly stats: HarnessPointReadStats,
  ) {
    super({ baseURL: window.location.origin, identity: currentLease })
    this.linksById = new Map(links.map((seed) => [seed.id, linkResponse(seed)]))
  }

  override async getLink(
    id: string,
    options: ReaderRequestOptions = {},
  ): Promise<ApiResult<LinkResponse>> {
    this.stats.linkIds.push(id)
    this.stats.active += 1
    this.stats.maximumActive = Math.max(this.stats.maximumActive, this.stats.active)
    const completed = await waitForPointRead(options.signal)
    this.stats.active -= 1
    if (!completed) {
      return { ok: false, error: { kind: 'timeout', message: 'point read aborted' } }
    }
    const link = this.linksById.get(id)
    return link
      ? { ok: true, data: link }
      : {
          ok: false,
          error: { kind: 'other', message: `missing harness link ${id}`, status: 404 },
        }
  }
}

function AnnotatedLinksObserver({ client }: { readonly client: HarnessReaderClient }) {
  const state = useAnnotatedLinks(client)
  useEffect(() => {
    annotatedLinksState = {
      visibleLinkIds: state.links.map((link) => link.id),
      loading: state.loading,
      error: state.error,
    }
  }, [state.error, state.links, state.loading])
  return null
}

function stopAnnotatedLinksProjection(): void {
  annotatedLinksRoot?.unmount()
  annotatedLinksRoot = null
  annotatedLinksContainer?.remove()
  annotatedLinksContainer = null
  annotatedLinksClient = null
  annotatedLinksState = { visibleLinkIds: [], loading: false, error: null }
  pointReadStats = { linkIds: [], active: 0, maximumActive: 0 }
}

function requireLease(): IdentityLease {
  if (!lease) throw new Error('RF5B annotation harness identity is not installed')
  return lease
}

function requireDocument(): HarnessDocument {
  if (!activeDocument) throw new Error('RF5B annotation harness document is not installed')
  return activeDocument
}

function targetFor(document: HarnessDocument): SavedContentAnnotationTarget {
  return { kind: 'saved-content', contentRevision: document.contentRevision }
}

function annotationFor(
  annotationId: string,
  note: string,
  stamp: number,
): SavedContentAnnotationAddDraft {
  const text = `selection:${annotationId}`
  return {
    id: annotationId,
    blockKey: 'content',
    start: 0,
    end: text.length,
    text,
    note,
    source: 'self',
    createdAt: stamp,
    updatedAt: stamp,
  }
}

function durableOperation(operation: HarnessOperation): AnnotationOperationInput {
  const document = requireDocument()
  const common = {
    opId: operation.opId,
    linkId: document.linkId,
    target: targetFor(document),
  }
  switch (operation.kind) {
    case 'add':
      return {
        ...common,
        kind: operation.kind,
        draft: annotationFor(
          operation.annotationId,
          operation.note,
          operation.stamp,
        ),
      }
    case 'update':
      return {
        ...common,
        kind: operation.kind,
        annotationId: operation.annotationId,
        patch: { note: operation.note, updatedAt: operation.stamp },
      }
    case 'delete':
      return {
        ...common,
        kind: operation.kind,
        annotationId: operation.annotationId,
      }
  }
}

function cloneProjection(): HarnessProjection {
  return {
    annotationStoreVersion: projectionSnapshot?.annotationStoreVersion ?? 0,
    annotations: projectionSnapshot?.annotations ?? [],
    refreshCount: projectionRefreshCount,
    hintCount: projectionHintCount,
    lastHint: projectionLastHint,
    error: projectionError,
  }
}

async function readSnapshot(): Promise<AnnotationSnapshot> {
  const document = requireDocument()
  const result = await readAnnotationSnapshot(
    requireLease(),
    document.linkId,
    targetFor(document),
  )
  if (!result.ok) throw new Error('RF5B annotation snapshot read failed')
  return result.value
}

function scheduleProjectionRefresh(): Promise<void> {
  const refresh = async () => {
    try {
      projectionSnapshot = await readSnapshot()
      projectionError = null
    } catch (error) {
      projectionError = error instanceof Error ? error.message : String(error)
    } finally {
      projectionRefreshCount += 1
    }
  }
  refreshTail = refreshTail.then(refresh, refresh)
  return refreshTail
}

function onVisibilityChange(): void {
  void scheduleProjectionRefresh()
}

function stopProjection(): void {
  channel?.dispose()
  channel = null
  if (visibilityListening) {
    document.removeEventListener('visibilitychange', onVisibilityChange)
    visibilityListening = false
  }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('user-data delete failed'))
    request.onblocked = () => reject(new Error('user-data delete was blocked'))
  })
}

async function reset(): Promise<void> {
  stopProjection()
  stopAnnotatedLinksProjection()
  if (lease) resourceStore.deactivateIdentity(lease)
  authority.clear()
  lease = null
  activeDocument = null
  projectionSnapshot = null
  projectionRefreshCount = 0
  projectionHintCount = 0
  projectionLastHint = null
  projectionError = null
  refreshTail = Promise.resolve()
  annotationSeedBatch = 0
  restoreBroadcastChannel()
  localStorage.clear()
  await deleteUserDataDatabase()
  await clearPersistedCache()
}

function install(document: HarnessDocument): void {
  stopProjection()
  stopAnnotatedLinksProjection()
  if (lease) resourceStore.deactivateIdentity(lease)
  authority.clear()
  activeDocument = { ...document }
  lease = authority.install({
    serverClientDataNamespace: `server:${document.namespace}`,
    physicalNamespace: document.namespace,
  })
  if (!resourceStore.activateIdentity(lease)) {
    throw new Error('RF5B annotation harness cache identity activation failed')
  }
  projectionSnapshot = null
  projectionRefreshCount = 0
  projectionHintCount = 0
  projectionLastHint = null
  projectionError = null
  refreshTail = Promise.resolve()
}

function close(): void {
  stopProjection()
  stopAnnotatedLinksProjection()
  if (lease) resourceStore.deactivateIdentity(lease)
  authority.clear()
  lease = null
  activeDocument = null
  resetUserDataDatabaseHandle()
  restoreBroadcastChannel()
}

async function startProjection(): Promise<HarnessProjection> {
  stopProjection()
  const currentLease = requireLease()
  channel = new AnnotationDocumentChannel(currentLease, (hint) => {
    projectionHintCount += 1
    projectionLastHint = hint
    void scheduleProjectionRefresh()
  })
  document.addEventListener('visibilitychange', onVisibilityChange)
  visibilityListening = true
  await scheduleProjectionRefresh()
  return cloneProjection()
}

function dropHintChannel(): void {
  channel?.dispose()
  channel = null
}

function disableBroadcastChannel(): void {
  if (channel) {
    throw new Error('disable BroadcastChannel before starting the projection')
  }
  Object.defineProperty(globalThis, 'BroadcastChannel', {
    configurable: true,
    writable: true,
    value: undefined,
  })
}

function restoreBroadcastChannel(): void {
  Object.defineProperty(globalThis, 'BroadcastChannel', {
    configurable: true,
    writable: true,
    value: nativeBroadcastChannel,
  })
}

async function triggerVisibilityRefresh(): Promise<HarnessProjection> {
  if (!visibilityListening) throw new Error('RF5B projection is not listening')
  const before = projectionRefreshCount
  document.dispatchEvent(new Event('visibilitychange'))
  await refreshTail
  if (projectionRefreshCount <= before) {
    throw new Error('visibilitychange did not schedule a durable refresh')
  }
  return cloneProjection()
}

async function commit(
  operation: HarnessOperation,
): Promise<HarnessCommitTransactionResult> {
  const document = requireDocument()
  const result = await commitAnnotationOperation(requireLease(), durableOperation(operation))
  if (
    result.ok &&
    result.value.status !== 'op-id-conflict' &&
    channel
  ) {
    channel.publish({
      linkId: document.linkId,
      documentRevision: document.contentRevision,
      annotationStoreVersion: result.value.annotationStoreVersion,
    })
  }
  return result
}

async function commitAfterBarrier(
  barrierName: string,
  participants: number,
  operation: HarnessOperation,
): Promise<HarnessCommitTransactionResult> {
  const query = new URLSearchParams({
    name: barrierName,
    participants: String(participants),
    party: operation.opId,
  })
  const response = await fetch(`/__test__/annotation-barrier?${query}`, {
    cache: 'no-store',
  })
  if (!response.ok) {
    throw new Error(`annotation barrier failed: ${response.status}`)
  }

  // The network barrier is fully released before the production store opens
  // its readwrite transaction. Never move this await into annotation-store.
  return commit(operation)
}

async function operationKinds(): Promise<ReadonlyArray<{ kind: string; sequence: number }>> {
  const document = requireDocument()
  const result = await listAnnotationOperationsForTest(
    requireLease(),
    document.linkId,
    targetFor(document),
  )
  if (!result.ok) throw new Error('RF5B annotation operation read failed')
  return result.value.map(({ kind, sequence }) => ({ kind, sequence }))
}

async function thoughtOutboxKeys(): Promise<readonly HarnessThoughtKey[]> {
  const currentLease = requireLease()
  const namespace = currentLease.context.physicalNamespace
  const database = await openUserDataDatabase()
  if (!database) throw new Error('RF5B user-data database is unavailable')
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_OUTBOX_STORE, 'readonly')
    const request = transaction.objectStore(THOUGHT_OUTBOX_STORE)
      .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
      .getAll(namespace) as IDBRequest<unknown[]>
    transaction.oncomplete = () => {
      if (!request.result.every((record) => isValidThoughtOutboxRecord(record, namespace))) {
        reject(new Error('RF5B Thought outbox contains an invalid row'))
        return
      }
      resolve(request.result
        .map((record) => {
          const row = record as {
            readonly sequence: number
            readonly opId: string
            readonly deviceId: string
            readonly logicalClock: number
          }
          return {
            sequence: row.sequence,
            opId: row.opId,
            deviceId: row.deviceId,
            logicalClock: row.logicalClock,
          }
        })
        .sort((left, right) => left.sequence - right.sequence)
        .map(({ opId, deviceId, logicalClock }) => ({ opId, deviceId, logicalClock })))
    }
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
}

function supersessionFixture(document: HarnessDocument): {
  readonly event: ThoughtSupersessionEventRecord
  readonly winner: ThoughtMaterializedRecord
} {
  const target = targetFor(document)
  const targetKey = `saved-content:${document.contentRevision}`
  const quote = { exact: 'supersession quote', start: 0, end: 18, prefix: '', suffix: '', block_key: 'content' }
  const operation = (opId: string, logicalClock: number, body: string) => ({
    sequence: logicalClock + 100,
    opId,
    deviceId: 'rf5b-supersession-device',
    logicalClock,
    operationKind: 'update' as const,
    annotationId: 'rf5b-supersession-thought',
    hostKind: 'link' as const,
    hostId: document.linkId,
    target,
    targetKey,
    body,
    source: 'self' as const,
    quote,
    createdAt: '2026-08-11T00:00:00.000Z',
  })
  const winnerKey = { logicalClock: 9, deviceId: 'rf5b-winner-device', opId: 'rf5b-winner-op' }
  return {
    event: {
      key: [document.namespace, 7],
      namespace: document.namespace,
      eventSequence: 7,
      annotationId: 'rf5b-supersession-thought',
      loser: operation('rf5b-loser-op', 4, 'recover this immutable loser'),
      winnerAtDetection: operation('rf5b-detection-winner-op', 5, 'winner at detection'),
    },
    winner: {
      key: [document.namespace, 'rf5b-supersession-thought'],
      namespace: document.namespace,
      annotationId: 'rf5b-supersession-thought',
      contractVersion: 1,
      winnerKey,
      hostKind: 'link',
      hostId: document.linkId,
      linkId: document.linkId,
      target,
      targetKey,
      quote,
      body: 'current winner anchor',
      source: 'self',
      deleted: false,
      lifecycleStatus: 'active',
      serverSequence: 17,
      createdAt: '2026-08-11T00:00:00.000Z',
      updatedAt: '2026-08-11T00:00:00.000Z',
    },
  }
}

async function seedSupersessionRecovery(): Promise<void> {
  const currentLease = requireLease()
  const document = requireDocument()
  const { event, winner } = supersessionFixture(document)
  const result = await runUserDataTransaction(
    currentLease,
    'seed rf5b supersession recovery',
    [THOUGHT_SUPERSESSION_EVENTS_STORE, THOUGHT_MATERIALIZED_STORE],
    'readwrite',
    (transaction, _identity, setResult) => {
      transaction.objectStore(THOUGHT_SUPERSESSION_EVENTS_STORE).put(event)
      transaction.objectStore(THOUGHT_MATERIALIZED_STORE).put(winner)
      setResult(undefined)
    },
  )
  if (!result.ok) throw new Error('RF5B supersession seed failed')
}

async function recoverSupersession(): Promise<
  { readonly ok: true; readonly value: SupersessionRecoveryResult } | { readonly ok: false }
> {
  const document = requireDocument()
  return commitSupersessionRecovery(requireLease(), supersessionFixture(document).event)
}

async function annotatedLinkIds(): Promise<readonly string[]> {
  const result = await enumerateAnnotatedLinkIds(requireLease())
  if (!result.ok) throw new Error('RF5B annotated-link index read failed')
  return result.value
}

async function seedAnnotatedLinkIds(
  linkIds: readonly string[],
): Promise<HarnessAnnotatedLinkSeedResult> {
  const currentLease = requireLease()
  const currentDocument = requireDocument()
  if (!resourceStore.isIdentityActive(currentLease)) {
    throw new Error('RF5B annotation seed does not own the active cache identity')
  }
  if (new Set(linkIds).size !== linkIds.length) {
    throw new Error('RF5B annotation seed link IDs must be unique')
  }
  annotationSeedBatch += 1
  const batch = annotationSeedBatch
  const operations: AnnotationImportOperation[] = linkIds.map((linkId, index) => ({
    kind: 'add',
    opId: `rf5b-browser-seed-${batch}-${index}`,
    linkId,
    target: targetFor(currentDocument),
    draft: annotationFor(
      `rf5b-browser-annotation-${batch}-${index}`,
      `Browser indexed annotation for ${linkId}`,
      batch * 1_000 + index,
    ),
  }))
  const result = await importAnnotationOperations(currentLease, {
    importId: `rf5b-browser-seed-${batch}`,
    sourceFingerprint: '0'.repeat(64),
    operations,
  })
  if (!result.ok) throw new Error('RF5B annotation index seed failed')
  return result.value
}

function startAnnotatedLinksProjection(links: readonly HarnessLinkSeed[]): void {
  stopAnnotatedLinksProjection()
  const currentLease = requireLease()
  if (!resourceStore.isIdentityActive(currentLease)) {
    throw new Error('RF5B annotated projection does not own the active cache identity')
  }
  pointReadStats = { linkIds: [], active: 0, maximumActive: 0 }
  annotatedLinksState = { visibleLinkIds: [], loading: true, error: null }
  annotatedLinksClient = new HarnessReaderClient(currentLease, links, pointReadStats)
  annotatedLinksContainer = document.createElement('div')
  annotatedLinksContainer.dataset.testid = 'rf5b-annotated-links-projection'
  document.body.append(annotatedLinksContainer)
  annotatedLinksRoot = createRoot(annotatedLinksContainer)
  annotatedLinksRoot.render(createElement(AnnotatedLinksObserver, {
    client: annotatedLinksClient,
  }))
}

function annotatedLinksProjection(): HarnessAnnotatedLinksProjection {
  return {
    visibleLinkIds: [...annotatedLinksState.visibleLinkIds],
    loading: annotatedLinksState.loading,
    error: annotatedLinksState.error,
    pointReadLinkIds: [...pointReadStats.linkIds],
    activePointReads: pointReadStats.active,
    maximumConcurrentPointReads: pointReadStats.maximumActive,
    activePhysicalNamespace: resourceStore.activePhysicalNamespace,
    clientOwnsActiveCacheIdentity: annotatedLinksClient !== null &&
      resourceStore.isIdentityActive(annotatedLinksClient.identityLease),
  }
}

function wakeup(): HarnessWakeup {
  const currentLease = requireLease()
  return {
    key: ownedStorageKeyForLease('annotationWakeup', currentLease),
    value: readOwnedStorageForLease('annotationWakeup', currentLease),
  }
}

async function seedCache(logicalKey: string): Promise<boolean> {
  const document = requireDocument()
  return idbPut(createPersistedRecord(document.namespace, logicalKey, {
    schema: CACHE_SCHEMA_VERSION,
    data: { rf5b: true },
    updatedAt: Date.now(),
    size: 16,
  }))
}

async function cacheRecordCount(): Promise<number> {
  return (await idbGetAll()).length
}

window.rf5bAnnotationsHarness = {
  reset,
  install,
  close,
  startProjection,
  dropHintChannel,
  disableBroadcastChannel,
  restoreBroadcastChannel,
  triggerVisibilityRefresh,
  projection: cloneProjection,
  commit,
  commitAfterBarrier,
  read: readSnapshot,
  operationKinds,
  thoughtOutboxKeys,
  seedSupersessionRecovery,
  recoverSupersession,
  annotatedLinkIds,
  seedAnnotatedLinkIds,
  startAnnotatedLinksProjection,
  annotatedLinksProjection,
  wakeup,
  seedCache,
  cacheRecordCount,
  clearCache: clearPersistedCache,
}
