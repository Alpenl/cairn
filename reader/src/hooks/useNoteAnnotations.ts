import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type Annotation,
  type AnnotationInput,
  type AnnotationPatch,
  type NoteAnnotationTarget,
} from '../lib/annotation-domain'
import {
  buildAnnotationAddOperation,
  buildAnnotationDeleteOperation,
  buildAnnotationUpdateOperation,
  commitAnnotationCommand,
  loadAnnotationTargets,
  randomAnnotationCommandToken,
  type AnnotationCommandResult,
} from '../lib/annotation-commands'
import type { IdentityLease } from '../lib/identity'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../lib/reader-events'
import {
  compactAnnotationOperations,
} from '../lib/user-data/annotation-store'
import type { AnnotationOperationInput } from '../lib/user-data/annotation-types'
import { useAbortGeneration } from './useAbortGeneration'
import { useVisibilityRefresh } from './useVisibilityRefresh'

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

function noteCommandResult(result: AnnotationCommandResult): NoteAnnotationCommandResult {
  if (result.status !== 'committed' && result.status !== 'duplicate') return result
  return {
    status: result.status,
    annotationId: result.annotationId,
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
  const generation = useAbortGeneration(identityKey)
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

  const refresh = useCallback(async (): Promise<boolean> => {
    if (!lease || !noteID || !target || generation.controller.signal.aborted) {
      updateState({ identityKey, status: 'ready', annotations: EMPTY_ANNOTATIONS })
      return false
    }
    updateState(stateRef.current.identityKey === identityKey
      ? { ...stateRef.current, status: 'loading' }
      : { identityKey, status: 'loading', annotations: EMPTY_ANNOTATIONS })
    const result = await loadAnnotationTargets(lease, noteID, [target])
    if (generation.controller.signal.aborted) return false
    if (!result.ok) {
      updateState({ identityKey, status: 'error', annotations: EMPTY_ANNOTATIONS })
      return false
    }
    updateState({ identityKey, status: 'ready', annotations: result.annotations })
    return true
  }, [generation, identityKey, lease, noteID, target, updateState])

  useEffect(() => {
    updateState({ identityKey, status: 'loading', annotations: EMPTY_ANNOTATIONS })
    void refresh()
  }, [identityKey, refresh, updateState])

  useVisibilityRefresh(() => {
    void refresh()
  })

  useEffect(() => {
    const onChange = () => void refresh()
    const unsubscribe = subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged, READER_EVENTS.thoughtsSynced],
      onChange,
    )
    return () => {
      unsubscribe()
    }
  }, [refresh])

  const commit = useCallback(async (
    operation: AnnotationOperationInput,
    annotationId: string,
  ): Promise<NoteAnnotationCommandResult> => {
    if (!lease || !noteID || !target || generation.controller.signal.aborted) return { status: 'stale' }
    const outcome = await commitAnnotationCommand({
      lease,
      linkId: noteID,
      target,
      operation,
      annotationId,
      commandSignal: generation.controller.signal,
      afterCommit: async () => {
        await refresh()
        emitReaderEvent(READER_EVENTS.annotationsChanged)
        void compactAnnotationOperations(lease, noteID, target)
      },
    })
    return noteCommandResult(outcome)
  }, [generation, lease, noteID, refresh, target])

  const add = useCallback(async (input: AnnotationInput): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID) return { status: 'stale' }
    const annotationToken = randomAnnotationCommandToken()
    const operationToken = randomAnnotationCommandToken()
    if (!annotationToken || !operationToken) return { status: 'failed' }
    const annotationId = `an:${annotationToken}`
    const operation = buildAnnotationAddOperation({
      linkId: noteID,
      target,
      annotationId,
      opId: `op:${operationToken}`,
      annotation: input,
    })
    if (!operation) return { status: 'unsupported' }
    return commit(operation, annotationId)
  }, [commit, noteID, target])

  const update = useCallback(async (
    annotation: Annotation,
    patch: AnnotationPatch,
  ): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID) return { status: 'stale' }
    const operationToken = randomAnnotationCommandToken()
    if (!operationToken) return { status: 'failed' }
    const operation = buildAnnotationUpdateOperation({
      linkId: noteID,
      target,
      annotation,
      patch,
      opId: `op:${operationToken}`,
    })
    if (!operation) return { status: 'stale' }
    return commit(operation, annotation.id)
  }, [commit, noteID, target])

  const remove = useCallback(async (annotation: Annotation): Promise<NoteAnnotationCommandResult> => {
    if (!target || !noteID) return { status: 'stale' }
    const operationToken = randomAnnotationCommandToken()
    if (!operationToken) return { status: 'failed' }
    const operation = buildAnnotationDeleteOperation({
      linkId: noteID,
      target,
      opId: `op:${operationToken}`,
      annotation,
    })
    if (!operation) return { status: 'stale' }
    return commit(operation, annotation.id)
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
