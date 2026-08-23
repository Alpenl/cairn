import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type Annotation,
  type AnnotationTarget,
} from '../annotation-domain'
import { isValidLinkId, isValidSourceHash } from '../article/source-block'
import {
  attachIDBAbortSignal,
  abortIDBTransaction,
  handleIDBRequest,
} from '../idb-core'
import type { IdentityLease } from '../identity'
import { asRecord } from '../records'
import {
  annotationFromAddDraft,
  annotationSignatureFields,
  annotationsEqual,
  cloneTargetAnnotation,
  isSafeNonNegativeInteger,
} from './annotation-codec'
import {
  ANNOTATED_LINKS_NAMESPACE_INDEX,
  ANNOTATED_LINKS_STORE,
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_ID_INDEX,
  ANNOTATION_OPS_STORE,
  ANNOTATION_OPS_TARGET_INDEX,
  THOUGHT_MATERIALIZED_HOST_INDEX,
  THOUGHT_MATERIALIZED_NAMESPACE_INDEX,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SYNC_STATE_STORE,
  runUserDataTransaction,
  type UserDataTransactionResult,
} from './idb'
import {
  DEFAULT_ANNOTATION_COMPACTION_THRESHOLD,
  annotationMaterializedKey,
  annotationTargetStateKey,
  type AnnotatedLinkRecord,
  type AnnotationCommitResult,
  type AnnotationCommitOptions,
  type AnnotationCompactionOptions,
  type AnnotationCompactionResult,
  type AnnotationLinkStateRecord,
  type AnnotationMaterializedRecord,
  type AnnotationOperationInput,
  type AnnotationOperationReceipt,
  type AnnotationOperationRecord,
  type AnnotationReplaySnapshotItem,
  type AnnotationSnapshot,
  type AnnotationUpdatePatch,
} from './annotation-types'
import {
  isAnnotationThoughtTarget,
  isValidThoughtHistoryOutboxRecord,
  isValidThoughtMaterializedRecord,
  isValidThoughtOutboxRecord,
  isValidThoughtSupersessionEventRecord,
  thoughtSupersessionRecoveryOperationID,
  type ThoughtHistoryOutboxRecord,
  type ThoughtMaterializedRecord,
  type ThoughtOutboxRecord,
  type ThoughtSupersessionEventRecord,
  type ThoughtSyncStateRecord,
  type ThoughtVersionKey,
} from './thought-types'
import {
  planThoughtOutboxEnqueue,
  type ThoughtOutboxRecoveryMetadata,
} from './thought-sync-transitions'
import {
  ThoughtClockError,
  maximumThoughtClock,
  nextThoughtLogicalClock,
  randomThoughtToken,
  stableThoughtDeviceID,
} from './thought-clock'

interface CanonicalOperationBase {
  readonly opId: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
}

type CanonicalOperation = CanonicalOperationBase & (
  | { readonly kind: 'add'; readonly annotation: Annotation }
  | {
      readonly kind: 'update'
      readonly patch: AnnotationUpdatePatch
      /** Recovery can seed a local projection from the current remote winner. */
      readonly anchorAnnotation?: Annotation
    }
  | { readonly kind: 'delete' }
)

type WithoutSequence<T> = T extends unknown ? Omit<T, 'sequence'> : never
type StoredAnnotationOperationRecord<T = AnnotationOperationRecord> =
  T extends AnnotationOperationRecord
    ? Omit<T, 'opId'> & {
        /** Namespace-qualified value used by the v4 unique `by-op-id` index. */
        readonly opId: string
        readonly logicalOpId: string
      }
    : never
type AnnotationOperationDraft = WithoutSequence<StoredAnnotationOperationRecord>
type CanonicalAddOperation = Extract<CanonicalOperation, { readonly kind: 'add' }>

export type AnnotationImportOperation = Extract<
  AnnotationOperationInput,
  { readonly kind: 'add' }
>

export interface AnnotationImportInput {
  readonly importId: string
  readonly sourceFingerprint: string
  readonly operations: readonly AnnotationImportOperation[]
}

export interface AnnotationImportResult {
  readonly status: 'committed' | 'duplicate'
  readonly examined: number
  readonly applied: number
  readonly lastSequence: number
}

interface AnnotationImportMarker {
  readonly key: [
    kind: 'legacy-annotation-import',
    namespace: string,
    importId: string,
    sourceFingerprint: string,
  ]
  readonly kind: 'legacy-annotation-import'
  readonly namespace: string
  readonly importId: string
  readonly sourceFingerprint: string
  readonly signature: string
  readonly examined: number
  readonly applied: number
  readonly lastSequence: number
}

function operationIDKey(namespace: string, opId: string): string {
  return `${namespace.length}:${namespace}:${opId}`
}

function abortTransaction(transaction: IDBTransaction): void {
  abortIDBTransaction(transaction)
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function cloneUpdatePatch(candidate: AnnotationUpdatePatch): AnnotationUpdatePatch | null {
  if (
    !isSafeNonNegativeInteger(candidate.updatedAt) ||
    (candidate.note !== undefined && typeof candidate.note !== 'string') ||
    (
      candidate.source !== undefined &&
      candidate.source !== 'self' &&
      candidate.source !== 'ai' &&
      candidate.source !== 'user'
    )
  ) {
    return null
  }

  return {
    updatedAt: candidate.updatedAt,
    ...(candidate.note === undefined ? {} : { note: candidate.note }),
    ...(candidate.source === undefined ? {} : { source: candidate.source }),
  }
}

function canonicalizeOperation(input: AnnotationOperationInput): CanonicalOperation | null {
  if (!isNonEmptyString(input.opId) || !isValidLinkId(input.linkId)) return null
  const target = canonicalAnnotationTarget(input.target)
  if (!target) return null
  const targetKey = annotationTargetKey(target)
  if (!targetKey) return null

  switch (input.kind) {
    case 'add': {
      const annotation = annotationFromAddDraft(input.draft, target)
      if (!annotation) return null
      return {
        kind: input.kind,
        opId: input.opId,
        linkId: input.linkId,
        target,
        targetKey,
        annotationId: annotation.id,
        annotation,
      }
    }
    case 'update': {
      if (!isNonEmptyString(input.annotationId)) return null
      const patch = cloneUpdatePatch(input.patch)
      if (!patch) return null
      return {
        kind: input.kind,
        opId: input.opId,
        linkId: input.linkId,
        target,
        targetKey,
        annotationId: input.annotationId,
        patch,
      }
    }
    case 'delete':
      if (!isNonEmptyString(input.annotationId)) return null
      return {
        kind: input.kind,
        opId: input.opId,
        linkId: input.linkId,
        target,
        targetKey,
        annotationId: input.annotationId,
      }
  }
}

function samePatch(left: AnnotationUpdatePatch, right: AnnotationUpdatePatch): boolean {
  return (
    left.note === right.note &&
    left.source === right.source &&
    left.updatedAt === right.updatedAt
  )
}

function operationSignature(
  operation: CanonicalOperation | AnnotationOperationRecord,
): string {
  switch (operation.kind) {
    case 'add':
      return JSON.stringify([operation.kind, ...annotationSignatureFields(operation.annotation)])
    case 'update':
      return JSON.stringify([
        operation.kind,
        operation.annotationId,
        operation.patch.note ?? null,
        operation.patch.source ?? null,
        operation.patch.updatedAt,
      ])
    case 'delete':
      return JSON.stringify([operation.kind, operation.annotationId])
  }
}

async function operationChecksum(operation: CanonicalOperation): Promise<string | undefined> {
  try {
    const subtle = globalThis.crypto?.subtle
    if (!subtle || typeof TextEncoder === 'undefined') return undefined
    const bytes = new TextEncoder().encode(
      `webtag:thought-operation:v1\0${operationSignature(operation)}`,
    )
    const digest = await subtle.digest('SHA-256', bytes)
    return Array.from(new Uint8Array(digest), (byte) =>
      byte.toString(16).padStart(2, '0')).join('')
  } catch {
    return undefined
  }
}

function existingOperationMatches(
  candidate: unknown,
  namespace: string,
  operation: CanonicalOperation,
): candidate is StoredAnnotationOperationRecord {
  if (!candidate || typeof candidate !== 'object') return false
  const existing = candidate as Partial<StoredAnnotationOperationRecord>
  if (
    !isSafeNonNegativeInteger(existing.sequence) ||
    existing.sequence === 0 ||
    existing.opId !== operationIDKey(namespace, operation.opId) ||
    existing.logicalOpId !== operation.opId ||
    existing.namespace !== namespace ||
    existing.linkId !== operation.linkId ||
    existing.targetKey !== operation.targetKey ||
    !existing.target ||
    annotationTargetKey(existing.target) !== operation.targetKey ||
    existing.annotationId !== operation.annotationId ||
    existing.kind !== operation.kind
  ) {
    return false
  }
  if (operation.kind === 'add') {
    return existing.kind === 'add' &&
      !!existing.annotation && annotationsEqual(existing.annotation, operation.annotation)
  }
  if (operation.kind === 'update') {
    return existing.kind === 'update' &&
      !!existing.patch && samePatch(existing.patch, operation.patch)
  }
  return existing.kind === 'delete'
}

function operationDraft(
  namespace: string,
  operation: CanonicalOperation,
): AnnotationOperationDraft {
  const base = {
    opId: operationIDKey(namespace, operation.opId),
    logicalOpId: operation.opId,
    namespace,
    linkId: operation.linkId,
    target: operation.target,
    targetKey: operation.targetKey,
    annotationId: operation.annotationId,
  }
  switch (operation.kind) {
    case 'add':
      return { ...base, kind: operation.kind, annotation: operation.annotation }
    case 'update':
      return { ...base, kind: operation.kind, patch: operation.patch }
    case 'delete':
      return { ...base, kind: operation.kind }
  }
}

function validMaterializedRecord(
  candidate: unknown,
  namespace: string,
  linkId: string,
  target: AnnotationTarget,
  targetKey: string,
  annotationId?: string,
): candidate is AnnotationMaterializedRecord {
  if (!candidate || typeof candidate !== 'object') return false
  const record = candidate as Partial<AnnotationMaterializedRecord>
  if (
    record.namespace !== namespace ||
    record.linkId !== linkId ||
    record.targetKey !== targetKey ||
    !record.target ||
    annotationTargetKey(record.target) !== targetKey ||
    !isNonEmptyString(record.annotationId) ||
    (annotationId !== undefined && record.annotationId !== annotationId) ||
    !isSafeNonNegativeInteger(record.sequence) ||
    record.sequence === 0 ||
    !Array.isArray(record.key) ||
    record.key.length !== 4 ||
    record.key[0] !== namespace ||
    record.key[1] !== linkId ||
    record.key[2] !== targetKey ||
    record.key[3] !== record.annotationId ||
    (record.importedBy !== undefined && !isNonEmptyString(record.importedBy))
  ) {
    return false
  }
  const fallback = record.fallbackAnnotation
  if (
    fallback !== null &&
    (
      !fallback ||
      fallback.id !== record.annotationId ||
      cloneTargetAnnotation(fallback, target) === null
    )
  ) {
    return false
  }
  if (record.annotation === null) return true
  if (!record.annotation || fallback === null) return false
  return cloneTargetAnnotation(record.annotation, target) !== null &&
    record.annotation.id === record.annotationId &&
    annotationsEqual(record.annotation, fallback)
}

function validReplaySnapshot(
  candidate: unknown,
  target: AnnotationTarget,
  highWaterMark: number,
): candidate is readonly AnnotationReplaySnapshotItem[] {
  if (!Array.isArray(candidate)) return false
  if (highWaterMark === 0) return candidate.length === 0
  if (candidate.length === 0) return false

  const annotationIDs = new Set<string>()
  let latestSequence = 0
  for (const value of candidate) {
    if (!value || typeof value !== 'object') return false
    const item = value as Partial<AnnotationReplaySnapshotItem>
    if (
      !isNonEmptyString(item.annotationId) ||
      annotationIDs.has(item.annotationId) ||
      !isSafeNonNegativeInteger(item.sequence) ||
      item.sequence === 0 ||
      item.sequence > highWaterMark ||
      (
        item.annotation !== null &&
        (
          !item.annotation ||
          item.annotation.id !== item.annotationId ||
          cloneTargetAnnotation(item.annotation, target) === null
        )
      ) ||
      (
        item.fallbackAnnotation !== null &&
        (
          !item.fallbackAnnotation ||
          item.fallbackAnnotation.id !== item.annotationId ||
          cloneTargetAnnotation(item.fallbackAnnotation, target) === null
        )
      ) ||
      (
        item.annotation !== null &&
        (
          item.fallbackAnnotation === null ||
          !annotationsEqual(item.annotation, item.fallbackAnnotation)
        )
      )
    ) {
      return false
    }
    annotationIDs.add(item.annotationId)
    latestSequence = Math.max(latestSequence, item.sequence)
  }
  return latestSequence === highWaterMark
}

function validLinkState(
  candidate: unknown,
  namespace: string,
  linkId: string,
  target: AnnotationTarget,
  targetKey: string,
): candidate is AnnotationLinkStateRecord {
  if (!candidate || typeof candidate !== 'object') return false
  const state = candidate as Partial<AnnotationLinkStateRecord>
  if (
    state.namespace !== namespace ||
    state.linkId !== linkId ||
    state.targetKey !== targetKey ||
    !state.target ||
    annotationTargetKey(state.target) !== targetKey ||
    !Array.isArray(state.key) ||
    state.key.length !== 3 ||
    state.key[0] !== namespace ||
    state.key[1] !== linkId ||
    state.key[2] !== targetKey ||
    !isSafeNonNegativeInteger(state.version) ||
    !isSafeNonNegativeInteger(state.activeCount) ||
    !isSafeNonNegativeInteger(state.compactedThroughSequence) ||
    state.compactedThroughSequence > state.version
  ) {
    return false
  }
  return validReplaySnapshot(
    state.snapshot,
    target,
    state.compactedThroughSequence,
  )
}

function materializedRange(
  namespace: string,
  linkId: string,
  targetKey: string,
): IDBKeyRange {
  return IDBKeyRange.bound(
    [namespace, linkId, targetKey],
    [namespace, linkId, targetKey, []],
  )
}

function operationRange(
  namespace: string,
  linkId: string,
  targetKey: string,
  afterSequence = 0,
): IDBKeyRange {
  return IDBKeyRange.bound(
    [namespace, linkId, targetKey, afterSequence],
    [namespace, linkId, targetKey, Number.MAX_SAFE_INTEGER],
    afterSequence > 0,
  )
}

function operationReceiptRange(namespace: string, opId: string): IDBKeyRange {
  return IDBKeyRange.bound(
    ['operation-receipt', namespace, opId],
    ['operation-receipt', namespace, opId, []],
  )
}

function operationReceipt(
  operation: StoredAnnotationOperationRecord,
): AnnotationOperationReceipt {
  return {
    key: [
      'operation-receipt',
      operation.namespace,
      operation.logicalOpId,
      operation.linkId,
      operation.targetKey,
    ],
    kind: 'operation-receipt',
    opId: operation.logicalOpId,
    namespace: operation.namespace,
    linkId: operation.linkId,
    targetKey: operation.targetKey,
    annotationId: operation.annotationId,
    operationKind: operation.kind,
    sequence: operation.sequence,
    signature: operationSignature(operation),
  }
}

function validOperationReceipt(candidate: unknown): candidate is AnnotationOperationReceipt {
  if (!candidate || typeof candidate !== 'object') return false
  const receipt = candidate as Partial<AnnotationOperationReceipt>
  return (
    receipt.kind === 'operation-receipt' &&
    isNonEmptyString(receipt.opId) &&
    isNonEmptyString(receipt.namespace) &&
    isValidLinkId(receipt.linkId) &&
    isNonEmptyString(receipt.targetKey) &&
    isNonEmptyString(receipt.annotationId) &&
    (
      receipt.operationKind === 'add' ||
      receipt.operationKind === 'update' ||
      receipt.operationKind === 'delete'
    ) &&
    isSafeNonNegativeInteger(receipt.sequence) &&
    receipt.sequence > 0 &&
    typeof receipt.signature === 'string' &&
    Array.isArray(receipt.key) &&
    receipt.key.length === 5 &&
    receipt.key[0] === 'operation-receipt' &&
    receipt.key[1] === receipt.namespace &&
    receipt.key[2] === receipt.opId &&
    receipt.key[3] === receipt.linkId &&
    receipt.key[4] === receipt.targetKey
  )
}

function receiptMatches(
  receipt: AnnotationOperationReceipt,
  namespace: string,
  operation: CanonicalOperation,
): boolean {
  return (
    receipt.namespace === namespace &&
    receipt.linkId === operation.linkId &&
    receipt.targetKey === operation.targetKey &&
    receipt.annotationId === operation.annotationId &&
    receipt.operationKind === operation.kind &&
    receipt.signature === operationSignature(operation)
  )
}

function emptyLinkState(
  namespace: string,
  linkId: string,
  target: AnnotationTarget,
  targetKey: string,
): AnnotationLinkStateRecord {
  return {
    key: annotationTargetStateKey(namespace, linkId, targetKey),
    namespace,
    linkId,
    target,
    targetKey,
    version: 0,
    activeCount: 0,
    compactedThroughSequence: 0,
    snapshot: [],
  }
}

function currentAnnotation(
  operation: CanonicalOperation,
  current: Pick<
    AnnotationMaterializedRecord,
    'annotation' | 'fallbackAnnotation'
  > | undefined,
): Pick<AnnotationMaterializedRecord, 'annotation' | 'fallbackAnnotation'> {
  switch (operation.kind) {
    case 'add':
      return {
        annotation: operation.annotation,
        fallbackAnnotation: operation.annotation,
      }
    case 'update': {
      const base = current?.annotation ?? current?.fallbackAnnotation ?? operation.anchorAnnotation ?? null
      const annotation = base ? { ...base, ...operation.patch } : null
      return { annotation, fallbackAnnotation: annotation ?? base }
    }
    case 'delete':
      return {
        annotation: null,
        fallbackAnnotation:
          current?.annotation ?? current?.fallbackAnnotation ?? null,
      }
  }
}

interface AnnotationProjectionAddress {
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
  readonly materializedKey: AnnotationMaterializedRecord['key']
  readonly stateKey: AnnotationLinkStateRecord['key']
}

interface AnnotationProjectionStores {
  readonly materialized: IDBObjectStore
  readonly state: IDBObjectStore
  readonly annotatedLinks: IDBObjectStore
}

interface AnnotationProjectionRead {
  readonly address: AnnotationProjectionAddress
  readonly stores: AnnotationProjectionStores
  readonly current: AnnotationMaterializedRecord | undefined
  readonly storedState: AnnotationLinkStateRecord | undefined
}

interface AnnotationProjectionValue {
  readonly annotation: Annotation | null
  readonly fallbackAnnotation: Annotation | null
  readonly importedBy?: string
}

function annotationProjectionAddress(
  namespace: string,
  operation: CanonicalOperationBase,
): AnnotationProjectionAddress {
  return {
    namespace,
    linkId: operation.linkId,
    target: operation.target,
    targetKey: operation.targetKey,
    annotationId: operation.annotationId,
    materializedKey: annotationMaterializedKey(
      namespace,
      operation.linkId,
      operation.targetKey,
      operation.annotationId,
    ),
    stateKey: annotationTargetStateKey(
      namespace,
      operation.linkId,
      operation.targetKey,
    ),
  }
}

function annotationProjectionStores(
  transaction: IDBTransaction,
): AnnotationProjectionStores {
  return {
    materialized: transaction.objectStore(ANNOTATION_MATERIALIZED_STORE),
    state: transaction.objectStore(ANNOTATION_LINK_STATE_STORE),
    annotatedLinks: transaction.objectStore(ANNOTATED_LINKS_STORE),
  }
}

/**
 * Reads and validates the two records that define one annotation projection.
 * Both commit and import must pass through this fail-closed boundary before
 * deciding how an operation changes the projection.
 */
function readAnnotationProjection(
  transaction: IDBTransaction,
  stores: AnnotationProjectionStores,
  address: AnnotationProjectionAddress,
  isCurrent: () => boolean,
  requireStateForCurrent: boolean,
  onReady: (projection: AnnotationProjectionRead) => void,
): void {
  const currentRequest = stores.materialized.get(address.materializedKey) as IDBRequest<
    AnnotationMaterializedRecord | undefined
  >
  const stateRequest = stores.state.get(address.stateKey) as IDBRequest<
    AnnotationLinkStateRecord | undefined
  >
  let currentDone = false
  let stateDone = false
  const finish = () => {
    if (!currentDone || !stateDone) return
    try {
      if (!isCurrent()) {
        abortTransaction(transaction)
        return
      }
      const current = currentRequest.result
      if (
        current !== undefined &&
        !validMaterializedRecord(
          current,
          address.namespace,
          address.linkId,
          address.target,
          address.targetKey,
          address.annotationId,
        )
      ) {
        abortTransaction(transaction)
        return
      }
      const storedState = stateRequest.result
      if (
        (requireStateForCurrent && current !== undefined && storedState === undefined) ||
        (
          storedState !== undefined &&
          !validLinkState(
            storedState,
            address.namespace,
            address.linkId,
            address.target,
            address.targetKey,
          )
        )
      ) {
        abortTransaction(transaction)
        return
      }
      onReady({ address, stores, current, storedState })
    } catch {
      abortTransaction(transaction)
    }
  }
  handleIDBRequest(transaction, currentRequest, () => {
    currentDone = true
    finish()
  })
  handleIDBRequest(transaction, stateRequest, () => {
    stateDone = true
    finish()
  })
}

/**
 * Maintains the materialized row, target state, and annotated-link index as
 * one invariant. The callback runs only after all three writes succeed while
 * the caller still owns the captured identity operation.
 */
function writeAnnotationProjection(
  transaction: IDBTransaction,
  projection: AnnotationProjectionRead,
  sequence: number,
  value: AnnotationProjectionValue,
  isCurrent: () => boolean,
  minimumActiveCount: 0 | 1,
  onWritten: (
    annotation: Annotation | null,
    fallbackAnnotation: Annotation | null,
    activeCount: number,
  ) => void,
): void {
  try {
    if (!isCurrent() || !isSafeNonNegativeInteger(sequence) || sequence === 0) {
      abortTransaction(transaction)
      return
    }
    const { address, stores, current, storedState } = projection
    const state = storedState ?? emptyLinkState(
      address.namespace,
      address.linkId,
      address.target,
      address.targetKey,
    )
    const wasActive = current !== undefined && current.annotation !== null
    const isActive = value.annotation !== null
    const activeCount = state.activeCount + Number(isActive) - Number(wasActive)
    if (activeCount < minimumActiveCount) {
      abortTransaction(transaction)
      return
    }

    const materialized: AnnotationMaterializedRecord = {
      key: address.materializedKey,
      namespace: address.namespace,
      linkId: address.linkId,
      target: address.target,
      targetKey: address.targetKey,
      annotationId: address.annotationId,
      sequence,
      annotation: value.annotation,
      fallbackAnnotation: value.fallbackAnnotation,
      ...(value.importedBy === undefined ? {} : { importedBy: value.importedBy }),
    }
    const nextState: AnnotationLinkStateRecord = {
      ...state,
      target: address.target,
      version: sequence,
      activeCount,
    }
    const materializedWrite = stores.materialized.put(materialized)
    const stateWrite = stores.state.put(nextState)

    let completedWrites = 0
    const markWritten = () => {
      try {
        completedWrites += 1
        if (completedWrites !== 3) return
        if (!isCurrent()) {
          abortTransaction(transaction)
          return
        }
        onWritten(value.annotation, value.fallbackAnnotation, activeCount)
      } catch {
        abortTransaction(transaction)
      }
    }
    handleIDBRequest(transaction, materializedWrite, markWritten)
    handleIDBRequest(transaction, stateWrite, markWritten)
    if (activeCount === 0) {
      handleIDBRequest(transaction, stores.annotatedLinks.delete(address.stateKey), markWritten)
    } else {
      handleIDBRequest(transaction, stores.annotatedLinks.put({
        key: address.stateKey,
        namespace: address.namespace,
        linkId: address.linkId,
        target: address.target,
        targetKey: address.targetKey,
        annotationCount: activeCount,
        annotationStoreVersion: sequence,
      } satisfies AnnotatedLinkRecord), markWritten)
    }
  } catch {
    abortTransaction(transaction)
  }
}

type ThoughtClockAllocationFailure = 'invalid-thought-clock' | 'thought-clock-exhausted'

/**
 * Allocates one or more clocks while the caller's annotation/outbox/projection
 * transaction is still open. Every readwrite transaction touches the shared
 * sync-state store, so IndexedDB serializes allocators across browsing contexts.
 */
export function allocateThoughtClocks(
  transaction: IDBTransaction,
  lease: IdentityLease,
  count: number,
  onAllocated: (deviceId: string, clocks: readonly number[]) => void,
  onFailure: (failure: ThoughtClockAllocationFailure) => void,
  observedClocks: readonly number[] = [],
): void {
  const namespace = lease.context.physicalNamespace
  const stateStore = transaction.objectStore(THOUGHT_SYNC_STATE_STORE)
  const outboxStore = transaction.objectStore(THOUGHT_OUTBOX_STORE)
  const historyOutboxStore = transaction.objectStore(THOUGHT_HISTORY_OUTBOX_STORE)
  const materializedStore = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
  const stateRequest = stateStore.get(namespace) as IDBRequest<unknown>
  const outboxRequest = outboxStore.index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
    .getAll(namespace) as IDBRequest<unknown[]>
  const historyOutboxRequest = historyOutboxStore.index(THOUGHT_HISTORY_OUTBOX_NAMESPACE_INDEX)
    .getAll(namespace) as IDBRequest<unknown[]>
  const materializedRequest = materializedStore.index(THOUGHT_MATERIALIZED_NAMESPACE_INDEX)
    .getAll(namespace) as IDBRequest<unknown[]>
  let stateDone = false
  let outboxDone = false
  let historyOutboxDone = false
  let materializedDone = false
  const finish = () => {
    if (!stateDone || !outboxDone || !historyOutboxDone || !materializedDone) return
    if (!lease.isCurrent(lease.capture('allocate thought logical clock'))) {
      abortTransaction(transaction)
      return
    }
    const rawState = asRecord(stateRequest.result)
    if (rawState && rawState.namespace !== namespace) {
      abortTransaction(transaction)
      return
    }
    const outbox = outboxRequest.result.filter((value): value is ThoughtOutboxRecord =>
      isValidThoughtOutboxRecord(value, namespace))
    const historyOutbox = historyOutboxRequest.result.filter(
      (value): value is ThoughtHistoryOutboxRecord =>
        isValidThoughtHistoryOutboxRecord(value, namespace),
    )
    const materialized = materializedRequest.result.filter((value): value is ThoughtMaterializedRecord =>
      isValidThoughtMaterializedRecord(value, namespace))
    if (outbox.length !== outboxRequest.result.length ||
      historyOutbox.length !== historyOutboxRequest.result.length ||
      materialized.length !== materializedRequest.result.length) {
      abortTransaction(transaction)
      return
    }
    try {
      const deviceId = stableThoughtDeviceID(lease, rawState)
      const priorFloor = rawState?.deviceId === deviceId
        ? rawState.logicalClockFloor ?? 0
        : 0
      let floor = maximumThoughtClock([
        priorFloor as number,
        ...materialized.map((record) => record.winnerKey.logicalClock),
        ...outbox.map((record) => record.logicalClock),
        ...historyOutbox.map((record) => record.logicalClock),
        ...historyOutbox.map((record) => record.snapshot.winnerKey.logicalClock),
        ...observedClocks,
      ])
      const clocks: number[] = []
      for (let index = 0; index < count; index += 1) {
        floor = nextThoughtLogicalClock([floor])
        clocks.push(floor)
      }
      const state: ThoughtSyncStateRecord = {
        namespace,
        cursor: typeof rawState?.cursor === 'string' ? rawState.cursor : '',
        deviceId,
        tabToken: typeof rawState?.tabToken === 'string' && rawState.tabToken.length > 0
          ? rawState.tabToken
          : randomThoughtToken('tab'),
        logicalClockFloor: floor,
        updatedAt: Date.now(),
        ...(typeof rawState?.lastAckSequence === 'number'
          ? { lastAckSequence: rawState.lastAckSequence }
          : {}),
        ...(typeof rawState?.pullAttemptCount === 'number'
          ? { pullAttemptCount: rawState.pullAttemptCount }
          : {}),
        ...(typeof rawState?.pullRetryAt === 'number'
          ? { pullRetryAt: rawState.pullRetryAt }
          : {}),
        ...(typeof rawState?.pullLastError === 'string'
          ? { pullLastError: rawState.pullLastError }
          : {}),
        ...(typeof rawState?.retryAt === 'number' ? { retryAt: rawState.retryAt } : {}),
        ...(typeof rawState?.lastError === 'string' ? { lastError: rawState.lastError } : {}),
        ...(typeof rawState?.lastErrorCode === 'string'
          ? { lastErrorCode: rawState.lastErrorCode }
          : {}),
        ...(typeof rawState?.lastSuccessfulSyncAt === 'number'
          ? { lastSuccessfulSyncAt: rawState.lastSuccessfulSyncAt }
          : {}),
        ...(typeof rawState?.resyncRequired === 'boolean'
          ? { resyncRequired: rawState.resyncRequired }
          : {}),
        ...(typeof rawState?.lastServerSequence === 'number'
          ? { lastServerSequence: rawState.lastServerSequence }
          : {}),
        ...(typeof rawState?.retentionCutoff === 'number'
          ? { retentionCutoff: rawState.retentionCutoff }
          : {}),
      }
      stateStore.put(state)
      onAllocated(deviceId, clocks)
    } catch (error) {
      if (error instanceof ThoughtClockError) {
        onFailure(error.code)
        return
      }
      abortTransaction(transaction)
    }
  }
  handleIDBRequest(transaction, stateRequest, () => { stateDone = true; finish() })
  handleIDBRequest(transaction, outboxRequest, () => { outboxDone = true; finish() })
  handleIDBRequest(transaction, historyOutboxRequest, () => {
    historyOutboxDone = true
    finish()
  })
  handleIDBRequest(transaction, materializedRequest, () => {
    materializedDone = true
    finish()
  })
}

function enqueueThoughtOutbox(
  transaction: IDBTransaction,
  namespace: string,
  operation: CanonicalOperation,
  sequence: number,
  annotation: Annotation | null,
  fallbackAnnotation: Annotation | null,
  checksum: string | undefined,
  versionKey: ThoughtVersionKey,
  onQueued: () => void,
  recovery?: ThoughtOutboxRecoveryMetadata,
): void {
  const record = planThoughtOutboxEnqueue({
    namespace,
    sequence,
    operation,
    annotation,
    fallbackAnnotation,
    ...(checksum === undefined ? {} : { checksum }),
    versionKey,
    createdAt: Date.now(),
    ...(recovery === undefined ? {} : { recovery }),
  })
  if (!record) {
    abortTransaction(transaction)
    return
  }
  handleIDBRequest(
    transaction,
    transaction.objectStore(THOUGHT_OUTBOX_STORE).put(record),
    () => onQueued(),
  )
}

function finishDuplicate(
  transaction: IDBTransaction,
  setResult: (result: AnnotationCommitResult) => void,
  existing: { readonly sequence: number },
  namespace: string,
  operation: CanonicalOperation,
): void {
  const stateRequest = transaction.objectStore(ANNOTATION_LINK_STATE_STORE).get(
    annotationTargetStateKey(namespace, operation.linkId, operation.targetKey),
  ) as IDBRequest<AnnotationLinkStateRecord | undefined>
  const annotationRequest = transaction.objectStore(ANNOTATION_MATERIALIZED_STORE).get(
    annotationMaterializedKey(
      namespace,
      operation.linkId,
      operation.targetKey,
      operation.annotationId,
    ),
  ) as IDBRequest<AnnotationMaterializedRecord | undefined>
  let stateDone = false
  let annotationDone = false
  const finish = () => {
    if (!stateDone || !annotationDone) return
    const state = stateRequest.result
    const materialized = annotationRequest.result
    if (
      !state ||
      !validLinkState(
        state,
        namespace,
        operation.linkId,
        operation.target,
        operation.targetKey,
      ) ||
      (
        materialized === undefined ||
        !validMaterializedRecord(
          materialized,
          namespace,
          operation.linkId,
          operation.target,
          operation.targetKey,
          operation.annotationId,
        )
      )
    ) {
      abortTransaction(transaction)
      return
    }
    setResult({
      status: 'duplicate',
      sequence: existing.sequence,
      annotationStoreVersion: state.version,
      annotation: materialized.annotation,
    })
  }
  handleIDBRequest(transaction, stateRequest, () => {
    stateDone = true
    finish()
  })
  handleIDBRequest(transaction, annotationRequest, () => {
    annotationDone = true
    finish()
  })
}

/**
 * Appends and materializes exactly one operation. All affected stores are in
 * the same readwrite transaction, so IndexedDB's cross-context serialization
 * is the concurrency authority; no caller projection participates in the write.
 */
export async function commitAnnotationOperation(
  lease: IdentityLease,
  input: AnnotationOperationInput,
  options: AnnotationCommitOptions = {},
): Promise<UserDataTransactionResult<AnnotationCommitResult>> {
  const operation = canonicalizeOperation(input)
  if (!operation || options.signal?.aborted) return Promise.resolve({ ok: false })
  const namespace = lease.context.physicalNamespace
  const checksum = await operationChecksum(operation)

  return runUserDataTransaction(
    lease,
    `commit annotation ${operation.opId}`,
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
      ANNOTATION_IMPORTS_STORE,
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      attachIDBAbortSignal(transaction, options.signal)
      const operations = transaction.objectStore(ANNOTATION_OPS_STORE)
      const existingRequest = operations.index(ANNOTATION_OPS_ID_INDEX).get(
        operationIDKey(namespace, operation.opId),
      ) as IDBRequest<StoredAnnotationOperationRecord | undefined>
      const receiptsRequest = transaction.objectStore(ANNOTATION_IMPORTS_STORE).getAll(
        operationReceiptRange(namespace, operation.opId),
      ) as IDBRequest<AnnotationOperationReceipt[]>
      let existingDone = false
      let receiptsDone = false
      const appendOrReturnExisting = () => {
        if (!existingDone || !receiptsDone) return
        try {
          if (!lease.isCurrent(identity)) {
            abortTransaction(transaction)
            return
          }
          if (!receiptsRequest.result.every(validOperationReceipt) ||
            receiptsRequest.result.length > 1) {
            abortTransaction(transaction)
            return
          }
          if (existingRequest.result !== undefined) {
            if (receiptsRequest.result.length !== 0) {
              abortTransaction(transaction)
              return
            }
            if (!existingOperationMatches(existingRequest.result, namespace, operation)) {
              setResult({ status: 'op-id-conflict' })
              return
            }
            finishDuplicate(
              transaction,
              setResult,
              existingRequest.result,
              namespace,
              operation,
            )
            return
          }
          const receipt = receiptsRequest.result[0]
          if (receipt) {
            if (!receiptMatches(receipt, namespace, operation)) {
              setResult({ status: 'op-id-conflict' })
              return
            }
            finishDuplicate(
              transaction,
              setResult,
              receipt,
              namespace,
              operation,
            )
            return
          }

          allocateThoughtClocks(
            transaction,
            lease,
            1,
            (deviceId, clocks) => {
              const logicalClock = clocks[0]
              if (!logicalClock) {
                abortTransaction(transaction)
                return
              }
              const versionKey: ThoughtVersionKey = {
                logicalClock,
                deviceId,
                opId: operation.opId,
              }
              const draft = operationDraft(namespace, operation)
              const appendRequest = operations.add(draft)
              handleIDBRequest(transaction, appendRequest, () => {
                try {
                  const sequence = appendRequest.result
                  if (
                    !lease.isCurrent(identity) ||
                    !isSafeNonNegativeInteger(sequence) ||
                    sequence === 0
                  ) {
                    abortTransaction(transaction)
                    return
                  }

                  const stores = annotationProjectionStores(transaction)
                  const address = annotationProjectionAddress(namespace, operation)
                  const isCurrent = () => lease.isCurrent(identity)
                  readAnnotationProjection(
                    transaction,
                    stores,
                    address,
                    isCurrent,
                    false,
                    (projection) => {
                      const nextValue = currentAnnotation(operation, projection.current)
                      writeAnnotationProjection(
                        transaction,
                        projection,
                        sequence,
                        nextValue,
                        isCurrent,
                        0,
                        (annotation, fallbackAnnotation) => enqueueThoughtOutbox(
                          transaction,
                          namespace,
                          operation,
                          sequence,
                          annotation,
                          fallbackAnnotation,
                          checksum,
                          versionKey,
                          () => setResult({
                            status: 'committed',
                            sequence,
                            annotationStoreVersion: sequence,
                            annotation,
                          }),
                        ),
                      )
                    },
                  )
                } catch {
                  abortTransaction(transaction)
                }
              })
            },
            () => abortTransaction(transaction),
          )
        } catch {
          abortTransaction(transaction)
        }
      }
      handleIDBRequest(transaction, existingRequest, () => {
        existingDone = true
        appendOrReturnExisting()
      })
      handleIDBRequest(transaction, receiptsRequest, () => {
        receiptsDone = true
        appendOrReturnExisting()
      })
    },
  )
}

export type SupersessionRecoveryResult =
  | {
      readonly status: 'committed' | 'duplicate'
      readonly sequence: number
      readonly annotation: Annotation | null
    }
  | { readonly status: 'op-id-conflict' }
  | {
      readonly status: 'unrecoverable'
      readonly reason:
        | 'missing-current-winner'
        | 'host-tombstoned'
        | 'current-winner-deleted'
        | 'target-or-quote-incomplete'
    }

function isRecoveryOperationRecord(
  value: unknown,
  namespace: string,
  operationID: string,
): value is StoredAnnotationOperationRecord {
  if (!value || typeof value !== 'object') return false
  const operation = value as Partial<StoredAnnotationOperationRecord>
  return operation.namespace === namespace && operation.logicalOpId === operationID &&
    operation.opId === operationIDKey(namespace, operationID) &&
    isSafeNonNegativeInteger(operation.sequence) && operation.sequence > 0 &&
    isNonEmptyString(operation.annotationId) && isValidLinkId(operation.linkId) &&
    (operation.kind === 'update' || operation.kind === 'delete')
}

/**
 * Recover one immutable loser through the ordinary local annotation outbox.
 * The current remote winner is read and checked in the same transaction that
 * creates the candidate, so stale anchors and tombstoned hosts produce no
 * candidate operation at all.
 */
export async function commitSupersessionRecovery(
  lease: IdentityLease,
  event: ThoughtSupersessionEventRecord,
  options: AnnotationCommitOptions = {},
): Promise<UserDataTransactionResult<SupersessionRecoveryResult>> {
  const namespace = lease.context.physicalNamespace
  if (!isValidThoughtSupersessionEventRecord(event, namespace) || options.signal?.aborted) {
    return { ok: false }
  }
  const operationID = thoughtSupersessionRecoveryOperationID(event.eventSequence)
  return runUserDataTransaction(
    lease,
    `recover thought supersession ${event.eventSequence}`,
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
      ANNOTATION_IMPORTS_STORE,
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
      THOUGHT_SUPERSESSION_EVENTS_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      attachIDBAbortSignal(transaction, options.signal)
      const operations = transaction.objectStore(ANNOTATION_OPS_STORE)
      const existingRequest = operations.index(ANNOTATION_OPS_ID_INDEX).get(
        operationIDKey(namespace, operationID),
      ) as IDBRequest<unknown>
      const receiptsRequest = transaction.objectStore(ANNOTATION_IMPORTS_STORE).getAll(
        operationReceiptRange(namespace, operationID),
      ) as IDBRequest<unknown[]>
      const eventRequest = transaction.objectStore(THOUGHT_SUPERSESSION_EVENTS_STORE).get(
        [namespace, event.eventSequence],
      ) as IDBRequest<unknown>
      const winnerRequest = transaction.objectStore(THOUGHT_MATERIALIZED_STORE).get(
        [namespace, event.annotationId],
      ) as IDBRequest<unknown>
      let existingDone = false
      let receiptsDone = false
      let eventDone = false
      let winnerDone = false
      const finish = () => {
        if (!existingDone || !receiptsDone || !eventDone || !winnerDone) return
        try {
          if (!lease.isCurrent(identity)) {
            abortTransaction(transaction)
            return
          }
          const storedEvent = eventRequest.result
          if (!isValidThoughtSupersessionEventRecord(storedEvent, namespace) ||
            storedEvent.eventSequence !== event.eventSequence ||
            storedEvent.annotationId !== event.annotationId ||
            !receiptsRequest.result.every(validOperationReceipt) || receiptsRequest.result.length > 1) {
            abortTransaction(transaction)
            return
          }
          if (existingRequest.result !== undefined) {
            if (!isRecoveryOperationRecord(existingRequest.result, namespace, operationID)) {
              setResult({ status: 'op-id-conflict' })
              return
            }
            setResult({
              status: 'duplicate',
              sequence: existingRequest.result.sequence,
              annotation: null,
            })
            return
          }
          const receipt = receiptsRequest.result[0]
          if (receipt !== undefined) {
            if (receipt.namespace !== namespace || receipt.opId !== operationID) {
              setResult({ status: 'op-id-conflict' })
              return
            }
            setResult({ status: 'duplicate', sequence: receipt.sequence, annotation: null })
            return
          }
          if (!isValidThoughtMaterializedRecord(winnerRequest.result, namespace) ||
            winnerRequest.result.annotationId !== event.annotationId) {
            setResult({ status: 'unrecoverable', reason: 'missing-current-winner' })
            return
          }
          const winner = winnerRequest.result
          if (!isAnnotationThoughtTarget(winner.target)) {
            setResult({ status: 'unrecoverable', reason: 'target-or-quote-incomplete' })
            return
          }
          if ((winner.lifecycleStatus ?? 'active') !== 'active') {
            setResult({ status: 'unrecoverable', reason: 'host-tombstoned' })
            return
          }
          const recoveryOf = storedEvent.loser
          const winnerAtDetection = storedEvent.winnerAtDetection
          const operation: CanonicalOperation | null = recoveryOf.operationKind === 'delete'
            ? {
                kind: 'delete',
                opId: operationID,
                linkId: winner.hostId,
                target: winner.target,
                targetKey: winner.targetKey,
                annotationId: winner.annotationId,
              }
            : (() => {
                if (winner.deleted) return null
                const anchorAnnotation = annotationFromRemoteThought(winner)
                if (!anchorAnnotation) return null
                return {
                  kind: 'update' as const,
                  opId: operationID,
                  linkId: winner.hostId,
                  target: winner.target,
                  targetKey: winner.targetKey,
                  annotationId: winner.annotationId,
                  patch: {
                    note: recoveryOf.body,
                    source: recoveryOf.source,
                    // Stable across repeated local clicks and reloads.
                    updatedAt: storedEvent.eventSequence,
                  },
                  anchorAnnotation,
                }
              })()
          if (!operation) {
            setResult({
              status: 'unrecoverable',
              reason: winner.deleted ? 'current-winner-deleted' : 'target-or-quote-incomplete',
            })
            return
          }
          allocateThoughtClocks(
            transaction,
            lease,
            1,
            (deviceId, clocks) => {
              const logicalClock = clocks[0]
              if (!logicalClock) {
                abortTransaction(transaction)
                return
              }
              const versionKey: ThoughtVersionKey = { logicalClock, deviceId, opId: operationID }
              const appendRequest = operations.add(operationDraft(namespace, operation))
              handleIDBRequest(transaction, appendRequest, () => {
                const sequence = appendRequest.result
                if (!lease.isCurrent(identity) || !isSafeNonNegativeInteger(sequence) || sequence === 0) {
                  abortTransaction(transaction)
                  return
                }
                const stores = annotationProjectionStores(transaction)
                const address = annotationProjectionAddress(namespace, operation)
                const isCurrent = () => lease.isCurrent(identity)
                readAnnotationProjection(
                  transaction,
                  stores,
                  address,
                  isCurrent,
                  false,
                  (projection) => {
                    const nextValue = currentAnnotation(operation, projection.current)
                    writeAnnotationProjection(
                      transaction,
                      projection,
                      sequence,
                      nextValue,
                      isCurrent,
                      0,
                      (annotation, fallbackAnnotation) => enqueueThoughtOutbox(
                        transaction,
                        namespace,
                        operation,
                        sequence,
                        annotation,
                        fallbackAnnotation,
                        undefined,
                        versionKey,
                        () => setResult({ status: 'committed', sequence, annotation }),
                        {
                          recoveryOf: {
                            logicalClock: recoveryOf.logicalClock,
                            deviceId: recoveryOf.deviceId,
                            opId: recoveryOf.opId,
                          },
                          expectedCurrentWinnerKey: winner.winnerKey,
                          hostKind: winner.hostKind === 'note' || winner.hostKind === 'inbox'
                            ? winner.hostKind
                            : 'link',
                        },
                      ),
                    )
                  },
                )
              })
            },
            () => abortTransaction(transaction),
            [
              recoveryOf.logicalClock,
              winnerAtDetection.logicalClock,
              winner.winnerKey.logicalClock,
            ],
          )
        } catch {
          abortTransaction(transaction)
        }
      }
      handleIDBRequest(transaction, existingRequest, () => { existingDone = true; finish() })
      handleIDBRequest(transaction, receiptsRequest, () => { receiptsDone = true; finish() })
      handleIDBRequest(transaction, eventRequest, () => { eventDone = true; finish() })
      handleIDBRequest(transaction, winnerRequest, () => { winnerDone = true; finish() })
    },
  )
}

function importMarkerKey(
  namespace: string,
  importId: string,
  sourceFingerprint: string,
): AnnotationImportMarker['key'] {
  return ['legacy-annotation-import', namespace, importId, sourceFingerprint]
}

function importSignature(operations: readonly CanonicalAddOperation[]): string {
  return JSON.stringify(operations.map((operation) => [
    operation.opId,
    operation.linkId,
    operation.targetKey,
    ...annotationSignatureFields(operation.annotation),
  ]))
}

function validImportMarker(
  candidate: unknown,
  namespace: string,
  importId: string,
  sourceFingerprint: string,
  signature: string,
  examined: number,
): candidate is AnnotationImportMarker {
  if (!candidate || typeof candidate !== 'object') return false
  const marker = candidate as Partial<AnnotationImportMarker>
  return (
    Array.isArray(marker.key) &&
    marker.key.length === 4 &&
    marker.key[0] === 'legacy-annotation-import' &&
    marker.key[1] === namespace &&
    marker.key[2] === importId &&
    marker.key[3] === sourceFingerprint &&
    marker.kind === 'legacy-annotation-import' &&
    marker.namespace === namespace &&
    marker.importId === importId &&
    marker.sourceFingerprint === sourceFingerprint &&
    marker.signature === signature &&
    marker.examined === examined &&
    isSafeNonNegativeInteger(marker.applied) &&
    marker.applied <= examined &&
    isSafeNonNegativeInteger(marker.lastSequence)
  )
}

/**
 * Imports one captured localStorage snapshot atomically. A committed marker is
 * the proof that every accepted operation and every derived projection/index
 * write completed. The legacy source remains a recovery copy; deterministic
 * marker and operation IDs make repeated imports idempotent.
 */
export function importAnnotationOperations(
  lease: IdentityLease,
  input: AnnotationImportInput,
): Promise<UserDataTransactionResult<AnnotationImportResult>> {
  if (
    !isNonEmptyString(input.importId) ||
    !isValidSourceHash(input.sourceFingerprint) ||
    !Array.isArray(input.operations)
  ) {
    return Promise.resolve({ ok: false })
  }
  const operations: CanonicalAddOperation[] = []
  const operationIDs = new Set<string>()
  const materializedKeys = new Set<string>()
  for (const inputOperation of input.operations) {
    const operation = canonicalizeOperation(inputOperation)
    if (!operation || operation.kind !== 'add' || operationIDs.has(operation.opId)) {
      return Promise.resolve({ ok: false })
    }
    const materializedIdentity = JSON.stringify([
      operation.linkId,
      operation.targetKey,
      operation.annotationId,
    ])
    if (materializedKeys.has(materializedIdentity)) return Promise.resolve({ ok: false })
    operationIDs.add(operation.opId)
    materializedKeys.add(materializedIdentity)
    operations.push(operation)
  }

  const namespace = lease.context.physicalNamespace
  const signature = importSignature(operations)
  const markerKey = importMarkerKey(
    namespace,
    input.importId,
    input.sourceFingerprint,
  )

  return runUserDataTransaction(
    lease,
    `import annotations ${input.importId}`,
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATED_LINKS_STORE,
      ANNOTATION_IMPORTS_STORE,
      THOUGHT_OUTBOX_STORE,
      THOUGHT_HISTORY_OUTBOX_STORE,
      THOUGHT_SYNC_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      const imports = transaction.objectStore(ANNOTATION_IMPORTS_STORE)
      const markerRequest = imports.get(markerKey) as IDBRequest<
        AnnotationImportMarker | undefined
      >
      handleIDBRequest(transaction, markerRequest, () => {
        try {
          if (!lease.isCurrent(identity)) {
            abortTransaction(transaction)
            return
          }
          const existingMarker = markerRequest.result
          if (existingMarker !== undefined) {
            if (!validImportMarker(
              existingMarker,
              namespace,
              input.importId,
              input.sourceFingerprint,
              signature,
              operations.length,
            )) {
              abortTransaction(transaction)
              return
            }
            setResult({
              status: 'duplicate',
              examined: operations.length,
              applied: 0,
              lastSequence: existingMarker.lastSequence,
            })
            return
          }

          const operationStore = transaction.objectStore(ANNOTATION_OPS_STORE)
          const stores = annotationProjectionStores(transaction)
          const isCurrent = () => lease.isCurrent(identity)
          let applied = 0
          let lastSequence = 0

          const finish = () => {
            if (!isCurrent()) {
              abortTransaction(transaction)
              return
            }
            const marker: AnnotationImportMarker = {
              key: markerKey,
              kind: 'legacy-annotation-import',
              namespace,
              importId: input.importId,
              sourceFingerprint: input.sourceFingerprint,
              signature,
              examined: operations.length,
              applied,
              lastSequence,
            }
            const markerWrite = imports.add(marker)
            handleIDBRequest(transaction, markerWrite, () => {
              if (!isCurrent()) {
                abortTransaction(transaction)
                return
              }
              setResult({
                status: 'committed',
                examined: operations.length,
                applied,
                lastSequence,
              })
            })
          }

          const applyAt = (index: number) => {
            if (index >= operations.length) {
              finish()
              return
            }
            const operation = operations[index]
            const address = annotationProjectionAddress(namespace, operation)
            readAnnotationProjection(
              transaction,
              stores,
              address,
              isCurrent,
              true,
              (projection) => {
                const current = projection.current
                const currentValue = current?.annotation ??
                  current?.fallbackAnnotation ?? null
                const mayReplace = current === undefined || (
                  current.importedBy === input.importId &&
                  currentValue !== null &&
                  operation.annotation.updatedAt > currentValue.updatedAt
                )
                if (!mayReplace) {
                  applyAt(index + 1)
                  return
                }

                allocateThoughtClocks(
                  transaction,
                  lease,
                  1,
                  (deviceId, clocks) => {
                    const logicalClock = clocks[0]
                    if (!logicalClock) {
                      abortTransaction(transaction)
                      return
                    }
                    const versionKey: ThoughtVersionKey = {
                      logicalClock,
                      deviceId,
                      opId: operation.opId,
                    }
                    const appendRequest = operationStore.add(
                      operationDraft(namespace, operation),
                    )
                    handleIDBRequest(transaction, appendRequest, () => {
                      try {
                        const sequence = appendRequest.result
                        if (!isCurrent() || !isSafeNonNegativeInteger(sequence) || sequence === 0) {
                          abortTransaction(transaction)
                          return
                        }
                        writeAnnotationProjection(
                          transaction,
                          projection,
                          sequence,
                          {
                            annotation: operation.annotation,
                            fallbackAnnotation: operation.annotation,
                            importedBy: input.importId,
                          },
                          isCurrent,
                          1,
                          (annotation, fallbackAnnotation) => enqueueThoughtOutbox(
                            transaction,
                            namespace,
                            operation,
                            sequence,
                            annotation,
                            fallbackAnnotation,
                            input.sourceFingerprint,
                            versionKey,
                            () => {
                              applied += 1
                              lastSequence = sequence
                              applyAt(index + 1)
                            },
                          ),
                        )
                      } catch {
                        abortTransaction(transaction)
                      }
                    })
                  },
                  () => abortTransaction(transaction),
                )
              },
            )
          }

          applyAt(0)
        } catch {
          abortTransaction(transaction)
        }
      })
    },
  )
}

function replayAnnotations(
  state: AnnotationLinkStateRecord,
  operations: readonly StoredAnnotationOperationRecord[],
): readonly Annotation[] | null {
  const replayed = new Map<string, AnnotationReplaySnapshotItem>(
    state.snapshot.map((item) => [item.annotationId, item]),
  )
  let latestSequence = state.compactedThroughSequence

  for (const operation of [...operations].sort(
    (left, right) => left.sequence - right.sequence,
  )) {
    if (operation.sequence <= latestSequence || operation.sequence > state.version) {
      return null
    }
    const nextValue = currentAnnotation(operation, replayed.get(operation.annotationId))
    replayed.set(operation.annotationId, {
      annotationId: operation.annotationId,
      sequence: operation.sequence,
      annotation: nextValue.annotation,
      fallbackAnnotation: nextValue.fallbackAnnotation,
    })
    latestSequence = operation.sequence
  }

  if (latestSequence !== state.version) return null
  const annotations = [...replayed.values()]
    .flatMap((item) => item.annotation ? [item.annotation] : [])
    .sort((left, right) =>
      left.createdAt - right.createdAt || left.id.localeCompare(right.id))
  return annotations.length === state.activeCount ? annotations : null
}

function annotationFromRemoteThought(
  record: ThoughtMaterializedRecord,
): Annotation | null {
  if (record.deleted || !record.quote || !isAnnotationThoughtTarget(record.target)) return null
  const start = record.quote.start
  const end = record.quote.end
  const exact = record.quote.exact
  const prefix = record.quote.prefix
  const suffix = record.quote.suffix
  if (!isSafeNonNegativeInteger(start) || !isSafeNonNegativeInteger(end) || end < start ||
    typeof exact !== 'string' ||
    (prefix !== undefined && typeof prefix !== 'string') ||
    (suffix !== undefined && typeof suffix !== 'string')) return null
  const createdAt = Date.parse(record.createdAt)
  const updatedAt = Date.parse(record.updatedAt)
  const draft = {
    id: record.annotationId,
    start,
    end,
    text: exact,
    note: record.body,
    source: record.source,
    createdAt: Number.isFinite(createdAt) ? createdAt : Date.now(),
    updatedAt: Number.isFinite(updatedAt) ? updatedAt : Date.now(),
    quote: {
      exact,
      prefix: typeof prefix === 'string' ? prefix : '',
      suffix: typeof suffix === 'string' ? suffix : '',
    },
    ...(record.target.kind === 'summary'
      ? {}
          : {
              blockKey: typeof record.quote.block_key === 'string'
                ? record.quote.block_key
                : record.target.kind === 'note' ? 'note' : 'content',
            }),
  }
  return annotationFromAddDraft(draft, record.target)
}

/** Rebuilds the authority from the compacted base plus its operation-log tail. */
export function readAnnotationSnapshot(
  lease: IdentityLease,
  linkId: string,
  requestedTarget: AnnotationTarget,
): Promise<UserDataTransactionResult<AnnotationSnapshot>> {
  const target = canonicalAnnotationTarget(requestedTarget)
  const targetKey = target ? annotationTargetKey(target) : null
  if (!isValidLinkId(linkId) || !target || !targetKey) {
    return Promise.resolve({ ok: false })
  }
  const namespace = lease.context.physicalNamespace

  return runUserDataTransaction(
    lease,
    `read annotations ${linkId}`,
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_LINK_STATE_STORE,
      THOUGHT_MATERIALIZED_STORE,
      THOUGHT_OUTBOX_STORE,
    ],
    'readonly',
    (transaction, _identity, setResult) => {
      const operationsRequest = transaction.objectStore(ANNOTATION_OPS_STORE)
        .index(ANNOTATION_OPS_TARGET_INDEX)
        .getAll(operationRange(namespace, linkId, targetKey)) as IDBRequest<
          StoredAnnotationOperationRecord[]
        >
      const stateRequest = transaction.objectStore(ANNOTATION_LINK_STATE_STORE).get(
        annotationTargetStateKey(namespace, linkId, targetKey),
      ) as IDBRequest<AnnotationLinkStateRecord | undefined>
      const remoteRequest = transaction.objectStore(THOUGHT_MATERIALIZED_STORE)
        .index(THOUGHT_MATERIALIZED_HOST_INDEX)
        .getAll([namespace, linkId, targetKey]) as IDBRequest<ThoughtMaterializedRecord[]>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<ThoughtOutboxRecord[]>
      let operationsDone = false
      let stateDone = false
      let remoteDone = false
      let outboxDone = false
      const finish = () => {
        if (!operationsDone || !stateDone || !remoteDone || !outboxDone) return
        try {
          if (!operationsRequest.result.every((operation) =>
            validStoredOperationRecord(operation, namespace, linkId, target, targetKey))) {
            abortTransaction(transaction)
            return
          }
          const storedState = stateRequest.result
          if (
            storedState !== undefined &&
            !validLinkState(storedState, namespace, linkId, target, targetKey)
          ) {
            abortTransaction(transaction)
            return
          }
          const state = storedState ?? emptyLinkState(namespace, linkId, target, targetKey)
          const annotations = replayAnnotations(state, operationsRequest.result)
          if (annotations === null) {
            abortTransaction(transaction)
            return
          }
          const pendingIDs = new Set(outboxRequest.result
            .filter((record) =>
              record.linkId === linkId && record.targetKey === targetKey)
            .map((record) => record.annotationId))
          const merged = new Map(annotations.map((annotation) => [annotation.id, annotation]))
          for (const remote of [...remoteRequest.result].sort((left, right) =>
            left.serverSequence - right.serverSequence)) {
            if (pendingIDs.has(remote.annotationId)) continue
            if (remote.deleted) {
              merged.delete(remote.annotationId)
              continue
            }
            const annotation = annotationFromRemoteThought(remote)
            if (annotation) merged.set(annotation.id, annotation)
          }
          const mergedAnnotations = [...merged.values()].sort((left, right) =>
            left.createdAt - right.createdAt || left.id.localeCompare(right.id))
          setResult({
            namespace,
            linkId,
            target,
            annotationStoreVersion: state.version,
            annotations: mergedAnnotations,
          })
        } catch {
          abortTransaction(transaction)
        }
      }
      handleIDBRequest(transaction, operationsRequest, () => {
        operationsDone = true
        finish()
      })
      handleIDBRequest(transaction, stateRequest, () => {
        stateDone = true
        finish()
      })
      handleIDBRequest(transaction, remoteRequest, () => {
        remoteDone = true
        finish()
      })
      handleIDBRequest(transaction, outboxRequest, () => {
        outboxDone = true
        finish()
      })
    },
  )
}

function validStoredOperationRecord(
  candidate: unknown,
  namespace: string,
  linkId: string,
  target: AnnotationTarget,
  targetKey: string,
): candidate is StoredAnnotationOperationRecord {
  if (!candidate || typeof candidate !== 'object') return false
  const operation = candidate as Partial<StoredAnnotationOperationRecord>
  if (
    operation.namespace !== namespace ||
    operation.linkId !== linkId ||
    operation.targetKey !== targetKey ||
    !operation.target ||
    annotationTargetKey(operation.target) !== targetKey ||
    !isNonEmptyString(operation.logicalOpId) ||
    operation.opId !== operationIDKey(namespace, operation.logicalOpId) ||
    !isNonEmptyString(operation.annotationId) ||
    !isSafeNonNegativeInteger(operation.sequence) ||
    operation.sequence === 0
  ) {
    return false
  }
  switch (operation.kind) {
    case 'add':
      return !!operation.annotation &&
        operation.annotation.id === operation.annotationId &&
        cloneTargetAnnotation(operation.annotation, target) !== null
    case 'update':
      return !!operation.patch && cloneUpdatePatch(operation.patch) !== null
    case 'delete':
      return true
    default:
      return false
  }
}

function validAnnotatedLinkRecord(
  candidate: unknown,
  namespace: string,
): candidate is AnnotatedLinkRecord {
  if (!candidate || typeof candidate !== 'object') return false
  const record = candidate as Partial<AnnotatedLinkRecord>
  if (
    record.namespace !== namespace ||
    !isValidLinkId(record.linkId) ||
    !isNonEmptyString(record.targetKey) ||
    !record.target ||
    !Array.isArray(record.key) ||
    record.key.length !== 3 ||
    record.key[0] !== namespace ||
    record.key[1] !== record.linkId ||
    record.key[2] !== record.targetKey ||
    !isSafeNonNegativeInteger(record.annotationCount) ||
    record.annotationCount === 0 ||
    !isSafeNonNegativeInteger(record.annotationStoreVersion)
  ) {
    return false
  }
  return annotationTargetKey(record.target) === record.targetKey
}

/** Returns the complete namespace-scoped source-target index. */
export function listAnnotatedLinks(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<readonly AnnotatedLinkRecord[]>> {
  const namespace = lease.context.physicalNamespace
  return runUserDataTransaction(
    lease,
    'list annotated links',
    [ANNOTATED_LINKS_STORE],
    'readonly',
    (transaction, _identity, setResult) => {
      const request = transaction.objectStore(ANNOTATED_LINKS_STORE)
        .index(ANNOTATED_LINKS_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<AnnotatedLinkRecord[]>
      handleIDBRequest(transaction, request, () => {
        if (!request.result.every((record) => validAnnotatedLinkRecord(record, namespace))) {
          abortTransaction(transaction)
          return
        }
        setResult([...request.result].sort((left, right) =>
          left.linkId.localeCompare(right.linkId) ||
          left.targetKey.localeCompare(right.targetKey)))
      })
    },
  )
}

/** Aggregates the complete target index to unique link IDs for sidebar discovery. */
export async function enumerateAnnotatedLinkIds(
  lease: IdentityLease,
): Promise<UserDataTransactionResult<readonly string[]>> {
  const rows = await listAnnotatedLinks(lease)
  if (!rows.ok) return rows
  return {
    ok: true,
    value: [...new Set(rows.value.map((row) => row.linkId))].sort((left, right) =>
      left.localeCompare(right)),
  }
}

function compactionSnapshot(
  records: readonly AnnotationMaterializedRecord[],
): AnnotationReplaySnapshotItem[] {
  return [...records]
    .sort((left, right) => left.annotationId.localeCompare(right.annotationId))
    .map((record) => ({
      annotationId: record.annotationId,
      sequence: record.sequence,
      annotation: record.annotation,
      fallbackAnnotation: record.fallbackAnnotation,
    }))
}

/**
 * Replaces the covered log prefix with a materialized replay snapshot. The
 * read, snapshot put, and deletes share one readwrite transaction. A writer in
 * another browsing context therefore receives a later sequence and cannot be
 * included accidentally after the selected high-water mark.
 */
export function compactAnnotationOperations(
  lease: IdentityLease,
  linkId: string,
  requestedTarget: AnnotationTarget,
  options: AnnotationCompactionOptions = {},
): Promise<UserDataTransactionResult<AnnotationCompactionResult>> {
  const target = canonicalAnnotationTarget(requestedTarget)
  const targetKey = target ? annotationTargetKey(target) : null
  const threshold = options.threshold ?? DEFAULT_ANNOTATION_COMPACTION_THRESHOLD
  if (
    !isValidLinkId(linkId) ||
    !target ||
    !targetKey ||
    !isSafeNonNegativeInteger(threshold)
  ) {
    return Promise.resolve({ ok: false })
  }
  const namespace = lease.context.physicalNamespace

  return runUserDataTransaction(
    lease,
    `compact annotation operations ${linkId}`,
    [
      ANNOTATION_OPS_STORE,
      ANNOTATION_MATERIALIZED_STORE,
      ANNOTATION_LINK_STATE_STORE,
      ANNOTATION_IMPORTS_STORE,
      THOUGHT_OUTBOX_STORE,
    ],
    'readwrite',
    (transaction, identity, setResult) => {
      const operationsStore = transaction.objectStore(ANNOTATION_OPS_STORE)
      const operationsRequest = operationsStore
        .index(ANNOTATION_OPS_TARGET_INDEX)
        .getAll(operationRange(namespace, linkId, targetKey)) as IDBRequest<
          StoredAnnotationOperationRecord[]
        >
      const materializedRequest = transaction.objectStore(ANNOTATION_MATERIALIZED_STORE)
        .getAll(materializedRange(namespace, linkId, targetKey)) as IDBRequest<
          AnnotationMaterializedRecord[]
        >
      const stateStore = transaction.objectStore(ANNOTATION_LINK_STATE_STORE)
      const stateRequest = stateStore.get(
        annotationTargetStateKey(namespace, linkId, targetKey),
      ) as IDBRequest<AnnotationLinkStateRecord | undefined>
      const outboxRequest = transaction.objectStore(THOUGHT_OUTBOX_STORE)
        .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
        .getAll(namespace) as IDBRequest<ThoughtOutboxRecord[]>
      let operationsDone = false
      let materializedDone = false
      let stateDone = false
      let outboxDone = false
      const compact = () => {
        if (!operationsDone || !materializedDone || !stateDone || !outboxDone) return
        try {
          if (!lease.isCurrent(identity)) {
            abortTransaction(transaction)
            return
          }
          if (!operationsRequest.result.every((operation) =>
            validStoredOperationRecord(operation, namespace, linkId, target, targetKey)) ||
            !materializedRequest.result.every((record) =>
              validMaterializedRecord(record, namespace, linkId, target, targetKey))) {
            abortTransaction(transaction)
            return
          }
          const storedState = stateRequest.result
          if (
            storedState !== undefined &&
            !validLinkState(storedState, namespace, linkId, target, targetKey)
          ) {
            abortTransaction(transaction)
            return
          }
          const state = storedState ?? emptyLinkState(namespace, linkId, target, targetKey)
          const activeCount = materializedRequest.result.filter(
            (record) => record.annotation !== null,
          ).length
          if (activeCount !== state.activeCount) {
            abortTransaction(transaction)
            return
          }
          const operations = [...operationsRequest.result]
            .sort((left, right) => left.sequence - right.sequence)
          if (operations.length <= threshold) {
            setResult({
              status: 'skipped',
              highWaterMark: state.compactedThroughSequence,
              operationsDeleted: 0,
              annotationStoreVersion: state.version,
            })
            return
          }
          // The outbox is the durable recovery copy for cross-device sync.
          // It survives this local log compaction, so a failed push can still
          // be retried without retaining every already-materialized operation.
          if (!storedState) {
            abortTransaction(transaction)
            return
          }

          const highWaterMark = operations[operations.length - 1]?.sequence
          if (highWaterMark === undefined || highWaterMark > state.version) {
            abortTransaction(transaction)
            return
          }
          const snapshot = compactionSnapshot(materializedRequest.result)
          const receipts = transaction.objectStore(ANNOTATION_IMPORTS_STORE)
          stateStore.put({
            ...state,
            compactedThroughSequence: highWaterMark,
            snapshot,
          } satisfies AnnotationLinkStateRecord)
          for (const operation of operations) {
            receipts.add(operationReceipt(operation))
            operationsStore.delete(operation.sequence)
          }
          setResult({
            status: 'compacted',
            highWaterMark,
            operationsDeleted: operations.length,
            annotationStoreVersion: state.version,
          })
        } catch {
          abortTransaction(transaction)
        }
      }
      handleIDBRequest(transaction, operationsRequest, () => {
        operationsDone = true
        compact()
      })
      handleIDBRequest(transaction, materializedRequest, () => {
        materializedDone = true
        compact()
      })
      handleIDBRequest(transaction, stateRequest, () => {
        stateDone = true
        compact()
      })
      handleIDBRequest(transaction, outboxRequest, () => {
        outboxDone = true
        compact()
      })
    },
  )
}

export type {
  AnnotatedLinkRecord,
  AnnotationCommitOptions,
  AnnotationCommitResult,
  AnnotationCompactionOptions,
  AnnotationCompactionResult,
  AnnotationOperationInput,
  AnnotationOperationRecord,
  AnnotationSnapshot,
  AnnotationUpdatePatch,
} from './annotation-types'
export type { AnnotationTarget } from '../annotation-domain'
