import type {
  Annotation,
  AnnotationInput,
  AnnotationPatch,
  SavedContentAnnotationBlockKey,
} from './annotations-domain'
import { isSavedContentAnnotationBlockKey } from './annotations-domain'
import type { IdentityLease } from './identity'
import { emitReaderEvent, READER_EVENTS } from './reader-events'
import { cloneTargetAnnotation } from './user-data/annotation-codec'
import {
  commitAnnotationOperation,
  readAnnotationSnapshot,
  type AnnotationCommitResult,
  type AnnotationOperationInput,
  type AnnotationSnapshot,
  type AnnotationTarget,
} from './user-data/annotation-store'

const EMPTY_ANNOTATIONS: readonly Annotation[] = Object.freeze([])

export interface AnnotationLifecycleReadContext {
  readonly lease: IdentityLease
  readonly linkId: string
  readonly targets: readonly AnnotationTarget[]
  readonly annotations: readonly Annotation[]
  readonly annotationStoreVersion: number
}

export type AnnotationLifecycleExtraResult<TExtra> =
  | { readonly ok: true; readonly value: TExtra }
  | { readonly ok: false }

export type AnnotationLifecycleLoadResult<TExtra> =
  | {
      readonly status: 'ready'
      readonly annotations: readonly Annotation[]
      readonly annotationStoreVersion: number
      readonly extra: TExtra
    }
  | { readonly status: 'empty' }
  | { readonly status: 'stale' }
  | { readonly status: 'failed' }

export type AnnotationMutationCommittedResult = {
  readonly status: 'committed' | 'duplicate'
  readonly annotationId: string
  readonly sequence: number
  readonly annotationStoreVersion: number
}

export type AnnotationMutationResult =
  | AnnotationMutationCommittedResult
  | { readonly status: 'stale' | 'failed' | 'op-id-conflict' }

export type AnnotationCommandResult =
  | AnnotationMutationCommittedResult
  | { readonly status: 'stale' | 'failed' | 'unsupported' | 'op-id-conflict' }

export interface AnnotationCommandTargetSet {
  readonly saved?: AnnotationTarget | null
  readonly summary?: AnnotationTarget | null
  readonly note?: AnnotationTarget | null
}

export type AnnotationOperationBuildResult =
  | {
      readonly ok: true
      readonly annotationId: string
      readonly operation: AnnotationOperationInput
    }
  | { readonly ok: false; readonly status: 'stale' | 'failed' | 'unsupported' }

export interface CommitTargetAnnotationCommandInput {
  readonly lease: IdentityLease | null
  readonly linkId: string | null
  readonly target: AnnotationTarget
  readonly operation: AnnotationOperationInput
  readonly annotationId: string
  readonly signal: AbortSignal
  readonly refresh: () => Promise<boolean>
  readonly afterCommit?: (result: AnnotationMutationCommittedResult) => void
  readonly scheduleCompaction?: (
    lease: IdentityLease,
    linkId: string,
    target: AnnotationTarget,
  ) => void
}

export interface LoadAnnotationTargetsInput<TExtra> {
  readonly lease: IdentityLease | null
  readonly linkId: string | null
  readonly targets: readonly AnnotationTarget[]
  readonly signal: AbortSignal
  readonly emptyExtra: TExtra
  readonly readExtra?: (
    context: AnnotationLifecycleReadContext,
  ) => Promise<AnnotationLifecycleExtraResult<TExtra>>
}

export function randomAnnotationToken(): string | null {
  try {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
    const bytes = new Uint8Array(16)
    crypto.getRandomValues(bytes)
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  } catch {
    return null
  }
}

export function combineAnnotationSignals(signals: readonly AbortSignal[]): {
  readonly signal: AbortSignal
  readonly dispose: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort()
  for (const signal of signals) {
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) controller.abort()
  }
  return {
    signal: controller.signal,
    dispose: () => {
      for (const signal of signals) signal.removeEventListener('abort', abort)
    },
  }
}

export function annotationCommandTargetForBlock(
  blockKey: string,
  targets: AnnotationCommandTargetSet,
): AnnotationTarget | null {
  if (isSavedContentAnnotationBlockKey(blockKey)) return targets.saved ?? null
  if (blockKey === 'summary') return targets.summary ?? null
  return targets.note ?? null
}

export function annotationMutationResult(
  result: AnnotationCommitResult,
  annotationId: string,
): AnnotationMutationResult {
  if (result.status === 'op-id-conflict') return { status: result.status }
  return {
    status: result.status,
    annotationId,
    sequence: result.sequence,
    annotationStoreVersion: result.annotationStoreVersion,
  }
}

function mergeAnnotationSnapshots(
  snapshots: readonly AnnotationSnapshot[],
): { readonly annotations: readonly Annotation[]; readonly version: number } {
  const annotations = snapshots
    .flatMap((snapshot) => snapshot.annotations)
    .sort((left, right) => left.createdAt - right.createdAt || left.id.localeCompare(right.id))
  return {
    annotations: annotations.length === 0 ? EMPTY_ANNOTATIONS : annotations,
    version: snapshots.reduce(
      (current, snapshot) => Math.max(current, snapshot.annotationStoreVersion),
      0,
    ),
  }
}

export async function loadAnnotationTargets<TExtra>({
  lease,
  linkId,
  targets,
  signal,
  emptyExtra,
  readExtra,
}: LoadAnnotationTargetsInput<TExtra>): Promise<AnnotationLifecycleLoadResult<TExtra>> {
  if (!lease || !linkId || signal.aborted) return { status: 'stale' }
  if (targets.length === 0) return { status: 'empty' }

  const results = await Promise.all(
    targets.map((target) => readAnnotationSnapshot(lease, linkId, target)),
  )
  if (signal.aborted) return { status: 'stale' }
  if (results.some((result) => !result.ok)) return { status: 'failed' }

  const merged = mergeAnnotationSnapshots(results.flatMap((result) =>
    result.ok ? [result.value] : []))
  const extraResult = readExtra
    ? await readExtra({
        lease,
        linkId,
        targets,
        annotations: merged.annotations,
        annotationStoreVersion: merged.version,
      })
    : { ok: true as const, value: emptyExtra }
  if (signal.aborted) return { status: 'stale' }
  if (!extraResult.ok) return { status: 'failed' }

  return {
    status: 'ready',
    annotations: merged.annotations,
    annotationStoreVersion: merged.version,
    extra: extraResult.value,
  }
}

export async function commitTargetAnnotationCommand({
  lease,
  linkId,
  target,
  operation,
  annotationId,
  signal,
  refresh,
  afterCommit,
  scheduleCompaction,
}: CommitTargetAnnotationCommandInput): Promise<AnnotationMutationResult> {
  if (!lease || !linkId || signal.aborted) return { status: 'stale' }
  const result = await commitAnnotationOperation(lease, operation, { signal })
  if (!result.ok) return signal.aborted ? { status: 'stale' } : { status: 'failed' }
  const outcome = annotationMutationResult(result.value, annotationId)
  if (outcome.status !== 'committed' && outcome.status !== 'duplicate') return outcome
  await refresh()
  afterCommit?.(outcome)
  emitReaderEvent(READER_EVENTS.annotationsChanged)
  scheduleCompaction?.(lease, linkId, target)
  return outcome
}

export function createAnnotationSelectionCommit(input: {
  readonly linkId: string | null
  readonly target: AnnotationTarget | null
  readonly selection: AnnotationInput
  readonly annotationToken?: string | null
  readonly operationToken?: string | null
  readonly now?: number
}): AnnotationOperationBuildResult {
  const { linkId, target, selection } = input
  if (!target || !linkId) return { ok: false, status: 'stale' }
  const annotationToken = input.annotationToken ?? randomAnnotationToken()
  const operationToken = input.operationToken ?? randomAnnotationToken()
  if (!annotationToken || !operationToken) return { ok: false, status: 'failed' }
  const now = input.now ?? Date.now()
  const annotationId = `an:${annotationToken}`
  const draftBase = {
    id: annotationId,
    note: selection.note ?? '',
    source: selection.source ?? 'self',
    createdAt: now,
    updatedAt: now,
    start: selection.start,
    end: selection.end,
    text: selection.text,
    ...(selection.quote === undefined ? {} : { quote: selection.quote }),
  }

  switch (target.kind) {
    case 'saved-content':
      if (!isSavedContentAnnotationBlockKey(selection.blockKey)) {
        return { ok: false, status: 'unsupported' }
      }
      return {
        ok: true,
        annotationId,
        operation: {
          kind: 'add',
          opId: `op:${operationToken}`,
          linkId,
          target,
          draft: {
            ...draftBase,
            blockKey: selection.blockKey as SavedContentAnnotationBlockKey,
          },
        },
      }
    case 'summary':
      if (selection.blockKey !== 'summary') return { ok: false, status: 'unsupported' }
      return {
        ok: true,
        annotationId,
        operation: {
          kind: 'add',
          opId: `op:${operationToken}`,
          linkId,
          target,
          draft: draftBase,
        },
      }
    case 'note':
      return {
        ok: true,
        annotationId,
        operation: {
          kind: 'add',
          opId: `op:${operationToken}`,
          linkId,
          target,
          draft: {
            ...draftBase,
            blockKey: selection.blockKey || 'note',
          },
        },
      }
  }
}

export function createAnnotationUpdateCommand(input: {
  readonly linkId: string | null
  readonly target: AnnotationTarget | null
  readonly annotation: Annotation
  readonly patch: AnnotationPatch
  readonly operationToken?: string | null
  readonly now?: number
}): AnnotationOperationBuildResult {
  const { linkId, target, annotation, patch } = input
  if (!target || !linkId) return { ok: false, status: 'stale' }
  if (!cloneTargetAnnotation(annotation, target)) return { ok: false, status: 'stale' }
  const operationToken = input.operationToken ?? randomAnnotationToken()
  if (!operationToken) return { ok: false, status: 'failed' }
  return {
    ok: true,
    annotationId: annotation.id,
    operation: {
      kind: 'update',
      opId: `op:${operationToken}`,
      linkId,
      target,
      annotationId: annotation.id,
      patch: { ...patch, updatedAt: input.now ?? Date.now() },
    },
  }
}

export function createAnnotationDeleteCommand(input: {
  readonly linkId: string | null
  readonly target: AnnotationTarget | null
  readonly annotation: Annotation
  readonly operationToken?: string | null
}): AnnotationOperationBuildResult {
  const { linkId, target, annotation } = input
  if (!target || !linkId) return { ok: false, status: 'stale' }
  if (!cloneTargetAnnotation(annotation, target)) return { ok: false, status: 'stale' }
  const operationToken = input.operationToken ?? randomAnnotationToken()
  if (!operationToken) return { ok: false, status: 'failed' }
  return {
    ok: true,
    annotationId: annotation.id,
    operation: {
      kind: 'delete',
      opId: `op:${operationToken}`,
      linkId,
      target,
      annotationId: annotation.id,
    },
  }
}
