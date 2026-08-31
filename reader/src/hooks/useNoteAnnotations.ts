import { useCallback, useMemo } from 'react'

import type { Annotation, AnnotationInput, AnnotationPatch } from '../lib/annotations'
import type { IdentityLease } from '../lib/identity'
import { cloneTargetAnnotation } from '../lib/user-data/annotation-codec'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type NoteAnnotationTarget,
} from '../lib/user-data/annotation-types'
import {
  commitAnnotationMutation,
  randomAnnotationToken,
  useAnnotationLifecycle,
  type AnnotationMutationResult,
} from './useAnnotationLifecycle'

export type NoteAnnotationCommandResult =
  | {
      readonly status: 'committed' | 'duplicate'
      readonly annotationId: string
      readonly sequence: number
    }
  | { readonly status: 'stale' | 'failed' | 'unsupported' | 'op-id-conflict' }

export interface UseNoteAnnotationsResult {
  readonly anns: readonly Annotation[]
  readonly loading: boolean
  readonly error: boolean
  readonly refresh: () => Promise<boolean>
  readonly add: (input: AnnotationInput) => Promise<NoteAnnotationCommandResult>
  readonly update: (annotation: Annotation, patch: AnnotationPatch) => Promise<NoteAnnotationCommandResult>
  readonly remove: (annotation: Annotation) => Promise<NoteAnnotationCommandResult>
}

const EMPTY_ANNOTATIONS: readonly Annotation[] = Object.freeze([])

function noteCommandResult(result: AnnotationMutationResult): NoteAnnotationCommandResult {
  if (result.status === 'committed' || result.status === 'duplicate') {
    return {
      status: result.status,
      annotationId: result.annotationId,
      sequence: result.sequence,
    }
  }
  return {
    status: result.status,
  }
}

function noteTarget(noteRevision: number): NoteAnnotationTarget | null {
  const target = canonicalAnnotationTarget({ kind: 'note', noteRevision })
  return target?.kind === 'note' ? target : null
}

/**
 * Note annotations are keyed by the note host, not by a link. The local store
 * still calls this field `linkId` for its shared index shape; thought-sync
 * uses `hostKind` and deliberately omits it from the wire payload for notes.
 */
export function useNoteAnnotations(
  lease: IdentityLease | null,
  noteID: string | null,
  noteRevision: number | null,
): UseNoteAnnotationsResult {
  const target = useMemo(
    () => noteRevision === null ? null : noteTarget(noteRevision),
    [noteRevision],
  )
  const targetKey = target ? annotationTargetKey(target) : null
  const identityKey = `${lease?.context.physicalNamespace ?? 'none'}\0${noteID ?? 'none'}\0${targetKey ?? 'none'}`
  const targets = useMemo(
    () => target ? [target] : [],
    [target],
  )
  const {
    state,
    signal,
    refresh,
  } = useAnnotationLifecycle({
    identityKey,
    lease,
    linkId: noteID,
    targets,
    emptyExtra: null,
  })

  const commit = useCallback(async (
    operation: Parameters<typeof commitAnnotationMutation>[0]['operation'],
    annotationId: string,
  ): Promise<NoteAnnotationCommandResult> => {
    if (!target) return { status: 'stale' }
    return noteCommandResult(await commitAnnotationMutation({
      lease,
      linkId: noteID,
      target,
      operation,
      annotationId,
      signal,
      refresh,
    }))
  }, [lease, noteID, refresh, signal, target])

  const add = useCallback(async (input: AnnotationInput): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID) return { status: 'stale' }
    const annotationToken = randomAnnotationToken()
    const operationToken = randomAnnotationToken()
    if (!annotationToken || !operationToken) return { status: 'failed' }
    const annotationId = `an:${annotationToken}`
    const now = Date.now()
    return commit({
      kind: 'add',
      opId: `op:${operationToken}`,
      linkId: noteID,
      target,
      draft: {
        id: annotationId,
        blockKey: input.blockKey || 'note',
        start: input.start,
        end: input.end,
        text: input.text,
        note: input.note ?? '',
        source: input.source ?? 'self',
        createdAt: now,
        updatedAt: now,
        quote: input.quote,
      },
    }, annotationId)
  }, [commit, noteID, target])

  const update = useCallback(async (
    annotation: Annotation,
    patch: AnnotationPatch,
  ): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID || annotation.sourceNoteRevision !== target.noteRevision ||
      !cloneTargetAnnotation(annotation, target)) return { status: 'stale' }
    const operationToken = randomAnnotationToken()
    if (!operationToken) return { status: 'failed' }
    return commit({
      kind: 'update',
      opId: `op:${operationToken}`,
      linkId: noteID,
      target,
      annotationId: annotation.id,
      patch: { ...patch, updatedAt: Date.now() },
    }, annotation.id)
  }, [commit, noteID, target])

  const remove = useCallback(async (annotation: Annotation): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID || annotation.sourceNoteRevision !== target.noteRevision ||
      !cloneTargetAnnotation(annotation, target)) return { status: 'stale' }
    const operationToken = randomAnnotationToken()
    if (!operationToken) return { status: 'failed' }
    return commit({
      kind: 'delete',
      opId: `op:${operationToken}`,
      linkId: noteID,
      target,
      annotationId: annotation.id,
    }, annotation.id)
  }, [commit, noteID, target])

  const visibleState = state.identityKey === identityKey ? state : null
  return {
    anns: visibleState?.annotations ?? EMPTY_ANNOTATIONS,
    loading: visibleState === null || visibleState.status === 'idle' || visibleState.status === 'loading',
    error: visibleState?.status === 'error',
    refresh,
    add,
    update,
    remove,
  }
}
