import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import type { Annotation, AnnotationInput, AnnotationPatch } from '../lib/annotations'
import type { IdentityLease } from '../lib/identity'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../lib/reader-events'
import {
  compactAnnotationOperations,
  commitAnnotationOperation,
  readAnnotationSnapshot,
  type AnnotationCommitResult,
} from '../lib/user-data/annotation-store'
import { cloneTargetAnnotation } from '../lib/user-data/annotation-codec'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type NoteAnnotationTarget,
} from '../lib/user-data/annotation-types'

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

function randomToken(): string | null {
  try {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
    const bytes = new Uint8Array(16)
    crypto.getRandomValues(bytes)
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  } catch {
    return null
  }
}

function commandResult(result: AnnotationCommitResult, annotationId: string): NoteAnnotationCommandResult {
  if (result.status === 'op-id-conflict') return { status: result.status }
  return {
    status: result.status,
    annotationId,
    sequence: result.sequence,
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
  const generation = useMemo(
    () => ({ key: identityKey, controller: new AbortController() }),
    [identityKey],
  )
  const [state, setState] = useState<{
    readonly identityKey: string
    readonly status: 'idle' | 'loading' | 'ready' | 'error'
    readonly annotations: readonly Annotation[]
  }>({ identityKey, status: 'idle', annotations: EMPTY_ANNOTATIONS })
  const stateRef = useRef(state)

  const updateState = useCallback((next: typeof state): void => {
    stateRef.current = next
    setState(next)
  }, [])

  useEffect(() => () => generation.controller.abort(), [generation])

  const refresh = useCallback(async (): Promise<boolean> => {
    if (!lease || !noteID || !target || generation.controller.signal.aborted) {
      updateState({ identityKey, status: 'ready', annotations: EMPTY_ANNOTATIONS })
      return false
    }
    updateState(stateRef.current.identityKey === identityKey
      ? { ...stateRef.current, status: 'loading' }
      : { identityKey, status: 'loading', annotations: EMPTY_ANNOTATIONS })
    const result = await readAnnotationSnapshot(lease, noteID, target)
    if (generation.controller.signal.aborted) return false
    if (!result.ok) {
      updateState({ identityKey, status: 'error', annotations: EMPTY_ANNOTATIONS })
      return false
    }
    updateState({ identityKey, status: 'ready', annotations: result.value.annotations })
    return true
  }, [generation, identityKey, lease, noteID, target, updateState])

  useEffect(() => {
    updateState({ identityKey, status: 'loading', annotations: EMPTY_ANNOTATIONS })
    void refresh()
  }, [identityKey, refresh, updateState])

  useEffect(() => {
    const onChange = () => void refresh()
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') void refresh()
    }
    const unsubscribe = subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged, READER_EVENTS.thoughtsSynced],
      onChange,
    )
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      unsubscribe()
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [refresh])

  const commit = useCallback(async (
    operation: Parameters<typeof commitAnnotationOperation>[1],
    annotationId: string,
  ): Promise<NoteAnnotationCommandResult> => {
    if (!lease || !noteID || !target || generation.controller.signal.aborted) return { status: 'stale' }
    const result = await commitAnnotationOperation(lease, operation, {
      signal: generation.controller.signal,
    })
    if (!result.ok) return generation.controller.signal.aborted ? { status: 'stale' } : { status: 'failed' }
    const outcome = commandResult(result.value, annotationId)
    if (outcome.status === 'op-id-conflict') return outcome
    await refresh()
    emitReaderEvent(READER_EVENTS.annotationsChanged)
    void compactAnnotationOperations(lease, noteID, target)
    return outcome
  }, [generation, lease, noteID, refresh, target])

  const add = useCallback(async (input: AnnotationInput): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID) return { status: 'stale' }
    const annotationToken = randomToken()
    const operationToken = randomToken()
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
    const operationToken = randomToken()
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
    const operationToken = randomToken()
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
