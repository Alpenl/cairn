import {
  type Annotation,
  type AnnotationInput,
  type AnnotationPatch,
  type AnnotationTarget,
  type SavedContentAnnotationBlockKey,
} from './annotation-domain'
import type { IdentityLease } from './identity'
import {
  commitAnnotationOperation,
  readAnnotationSnapshot,
} from './user-data/annotation-store'
import {
  cloneTargetAnnotation,
  isSavedContentAnnotationBlockKey,
} from './user-data/annotation-codec'
import type {
  AnnotationCommitResult,
  AnnotationOperationInput,
  AnnotationSnapshot,
} from './user-data/annotation-types'

const EMPTY_LOADED_ANNOTATIONS: readonly Annotation[] = Object.freeze([])

export interface AnnotationTargetsLoadSuccess {
  readonly ok: true
  readonly snapshots: readonly AnnotationSnapshot[]
  readonly annotations: readonly Annotation[]
  readonly annotationStoreVersion: number
}

export type AnnotationTargetsLoadResult =
  | AnnotationTargetsLoadSuccess
  | { readonly ok: false }

export type AnnotationCommandCommittedResult = {
  readonly status: 'committed' | 'duplicate'
  readonly annotationId: string
  readonly sequence: number
  readonly annotationStoreVersion: number
}

export type AnnotationCommandResult =
  | AnnotationCommandCommittedResult
  | { readonly status: 'stale' | 'failed' | 'unsupported' | 'op-id-conflict' }

export interface CommitAnnotationCommandInput {
  readonly lease: IdentityLease | null
  readonly linkId: string | null
  readonly target: AnnotationTarget
  readonly operation: AnnotationOperationInput
  readonly annotationId: string
  readonly commandSignal: AbortSignal
  readonly externalSignals?: readonly AbortSignal[]
  readonly isStale?: () => boolean
  readonly afterCommit?: (outcome: AnnotationCommandCommittedResult) => Promise<void> | void
}

function mergeSnapshots(
  snapshots: readonly AnnotationSnapshot[],
): Omit<AnnotationTargetsLoadSuccess, 'ok' | 'snapshots'> {
  const annotations = snapshots
    .flatMap((snapshot) => snapshot.annotations)
    .sort((left, right) => left.createdAt - right.createdAt || left.id.localeCompare(right.id))
  return {
    annotations: annotations.length === 0 ? EMPTY_LOADED_ANNOTATIONS : annotations,
    annotationStoreVersion: snapshots.reduce(
      (current, snapshot) => Math.max(current, snapshot.annotationStoreVersion),
      0,
    ),
  }
}

export async function loadAnnotationTargets(
  lease: IdentityLease,
  linkId: string,
  targets: readonly AnnotationTarget[],
): Promise<AnnotationTargetsLoadResult> {
  const results = await Promise.all(
    targets.map((target) => readAnnotationSnapshot(lease, linkId, target)),
  )
  const snapshots: AnnotationSnapshot[] = []
  for (const result of results) {
    if (!result.ok) return { ok: false }
    snapshots.push(result.value)
  }
  return { ok: true, snapshots, ...mergeSnapshots(snapshots) }
}

export function randomAnnotationCommandToken(): string | null {
  try {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
    const bytes = new Uint8Array(16)
    crypto.getRandomValues(bytes)
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  } catch {
    return null
  }
}

export function combineAnnotationCommandSignals(signals: readonly AbortSignal[]): {
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

export function annotationCommandResult(
  result: AnnotationCommitResult,
  annotationId: string,
): AnnotationCommandCommittedResult | { readonly status: 'op-id-conflict' } {
  if (result.status === 'op-id-conflict') return { status: result.status }
  return {
    status: result.status,
    annotationId,
    sequence: result.sequence,
    annotationStoreVersion: result.annotationStoreVersion,
  }
}

function addDraftBase(
  input: AnnotationInput,
  annotationId: string,
  now: number,
) {
  return {
    id: annotationId,
    note: input.note ?? '',
    source: input.source ?? 'self',
    createdAt: now,
    updatedAt: now,
    start: input.start,
    end: input.end,
    text: input.text,
    ...(input.quote === undefined ? {} : { quote: input.quote }),
  }
}

export function buildAnnotationAddOperation(input: {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotationId: string
  readonly opId: string
  readonly annotation: AnnotationInput
  readonly now?: number
}): AnnotationOperationInput | null {
  const now = input.now ?? Date.now()
  const draft = addDraftBase(input.annotation, input.annotationId, now)
  switch (input.target.kind) {
    case 'saved-content':
      if (!isSavedContentAnnotationBlockKey(input.annotation.blockKey)) return null
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft: {
          ...draft,
          blockKey: input.annotation.blockKey as SavedContentAnnotationBlockKey,
        },
      }
    case 'summary':
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft,
      }
    case 'note':
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft: {
          ...draft,
          blockKey: input.annotation.blockKey || 'note',
        },
      }
  }
}

export function buildAnnotationAddOperationFromAnnotation(input: {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotation: Annotation
  readonly opId: string
}): AnnotationOperationInput | null {
  const draftBase = {
    id: input.annotation.id,
    start: input.annotation.start,
    end: input.annotation.end,
    text: input.annotation.text,
    note: input.annotation.note,
    source: input.annotation.source,
    createdAt: input.annotation.createdAt,
    updatedAt: input.annotation.updatedAt,
    ...(input.annotation.quote === undefined ? {} : { quote: input.annotation.quote }),
  }
  switch (input.target.kind) {
    case 'saved-content':
      if (!isSavedContentAnnotationBlockKey(input.annotation.blockKey)) return null
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft: {
          ...draftBase,
          blockKey: input.annotation.blockKey as SavedContentAnnotationBlockKey,
        },
      }
    case 'summary':
      if (input.annotation.blockKey !== 'summary') return null
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft: draftBase,
      }
    case 'note':
      return {
        kind: 'add',
        opId: input.opId,
        linkId: input.linkId,
        target: input.target,
        draft: {
          ...draftBase,
          blockKey: input.annotation.blockKey || 'note',
        },
      }
  }
}

export function buildAnnotationUpdateOperation(input: {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotation: Annotation
  readonly patch: AnnotationPatch
  readonly opId: string
  readonly now?: number
}): AnnotationOperationInput | null {
  if (!cloneTargetAnnotation(input.annotation, input.target)) return null
  return {
    kind: 'update',
    opId: input.opId,
    linkId: input.linkId,
    target: input.target,
    annotationId: input.annotation.id,
    patch: { ...input.patch, updatedAt: input.now ?? Date.now() },
  }
}

export function buildAnnotationDeleteOperation(input: {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotation: Annotation
  readonly opId: string
}): AnnotationOperationInput | null {
  if (!cloneTargetAnnotation(input.annotation, input.target)) return null
  return {
    kind: 'delete',
    opId: input.opId,
    linkId: input.linkId,
    target: input.target,
    annotationId: input.annotation.id,
  }
}

export async function commitAnnotationCommand(
  input: CommitAnnotationCommandInput,
): Promise<AnnotationCommandResult> {
  if (
    !input.lease ||
    !input.linkId ||
    input.commandSignal.aborted ||
    input.isStale?.() === true
  ) {
    return { status: 'stale' }
  }

  const durableBefore = await readAnnotationSnapshot(input.lease, input.linkId, input.target)
  if (!durableBefore.ok) {
    return input.commandSignal.aborted ? { status: 'stale' } : { status: 'failed' }
  }
  if (input.commandSignal.aborted || input.isStale?.() === true) return { status: 'stale' }

  const cancellation = combineAnnotationCommandSignals([
    input.commandSignal,
    ...(input.externalSignals ?? []),
  ])
  const result = await commitAnnotationOperation(input.lease, input.operation, {
    signal: cancellation.signal,
  })
  const aborted = cancellation.signal.aborted || input.commandSignal.aborted
  cancellation.dispose()

  if (!result.ok) return aborted ? { status: 'stale' } : { status: 'failed' }
  const outcome = annotationCommandResult(result.value, input.annotationId)
  if (outcome.status === 'op-id-conflict') return outcome
  await input.afterCommit?.(outcome)
  return outcome
}
