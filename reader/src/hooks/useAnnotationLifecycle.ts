import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import {
  commitTargetAnnotationCommand,
  loadAnnotationTargets,
  type AnnotationLifecycleExtraResult,
  type AnnotationLifecycleReadContext,
  type AnnotationMutationCommittedResult,
  type AnnotationMutationResult,
} from '../lib/annotation-commands'
import type { Annotation } from '../lib/annotations'
import type { IdentityLease } from '../lib/identity'
import {
  READER_EVENTS,
  subscribeReaderEvents,
  type ReaderEventName,
} from '../lib/reader-events'
import {
  compactAnnotationOperations,
  type AnnotationOperationInput,
  type AnnotationTarget,
} from '../lib/user-data/annotation-store'
import { annotationTargetKey } from '../lib/user-data/annotation-types'

export type AnnotationLifecycleStatus = 'idle' | 'loading' | 'ready' | 'error'

export interface AnnotationLifecycleState<TExtra> {
  readonly identityKey: string
  readonly status: AnnotationLifecycleStatus
  readonly annotations: readonly Annotation[]
  readonly annotationStoreVersion: number
  readonly extra: TExtra
}

export interface UseAnnotationLifecycleOptions<TExtra> {
  readonly identityKey: string
  readonly lease: IdentityLease | null
  readonly linkId: string | null
  readonly targets: readonly AnnotationTarget[]
  readonly emptyExtra: TExtra
  readonly readExtra?: (
    context: AnnotationLifecycleReadContext,
  ) => Promise<AnnotationLifecycleExtraResult<TExtra>>
  readonly events?: readonly ReaderEventName[]
}

export interface UseAnnotationLifecycleResult<TExtra> {
  readonly state: AnnotationLifecycleState<TExtra>
  readonly stateRef: React.MutableRefObject<AnnotationLifecycleState<TExtra>>
  readonly signal: AbortSignal
  readonly refresh: () => Promise<boolean>
  readonly setState: (
    update: AnnotationLifecycleState<TExtra> | (
      (current: AnnotationLifecycleState<TExtra>) => AnnotationLifecycleState<TExtra>
    ),
  ) => void
}

export interface CommitAnnotationMutationInput {
  readonly lease: IdentityLease | null
  readonly linkId: string | null
  readonly target: AnnotationTarget
  readonly operation: AnnotationOperationInput
  readonly annotationId: string
  readonly signal: AbortSignal
  readonly refresh: () => Promise<boolean>
  readonly afterCommit?: (result: AnnotationMutationCommittedResult) => void
}

const EMPTY_ANNOTATIONS: readonly Annotation[] = Object.freeze([])
const DEFAULT_LIFECYCLE_EVENTS: readonly ReaderEventName[] = Object.freeze([
  READER_EVENTS.annotationsChanged,
  READER_EVENTS.thoughtsSynced,
])
const pendingCompactions = new Map<string, Promise<unknown>>()

export {
  combineAnnotationSignals,
  randomAnnotationToken,
  type AnnotationLifecycleExtraResult,
  type AnnotationLifecycleReadContext,
  type AnnotationMutationCommittedResult,
  type AnnotationMutationResult,
} from '../lib/annotation-commands'

function targetIdentity(target: AnnotationTarget): string {
  return annotationTargetKey(target) ?? 'invalid'
}

function compactionKey(
  lease: IdentityLease,
  linkId: string,
  target: AnnotationTarget,
): string {
  return `${lease.context.localEpoch}\0${lease.context.physicalNamespace}\0${linkId}\0${targetIdentity(target)}`
}

export function scheduleAnnotationCompaction(
  lease: IdentityLease,
  linkId: string,
  target: AnnotationTarget,
): void {
  const key = compactionKey(lease, linkId, target)
  if (pendingCompactions.has(key)) return
  const task = compactAnnotationOperations(lease, linkId, target).finally(() => {
    if (pendingCompactions.get(key) === task) pendingCompactions.delete(key)
  })
  pendingCompactions.set(key, task)
}

export async function commitAnnotationMutation({
  lease,
  linkId,
  target,
  operation,
  annotationId,
  signal,
  refresh,
  afterCommit,
}: CommitAnnotationMutationInput): Promise<AnnotationMutationResult> {
  return commitTargetAnnotationCommand({
    lease,
    linkId,
    target,
    operation,
    annotationId,
    signal,
    refresh,
    afterCommit,
    scheduleCompaction: scheduleAnnotationCompaction,
  })
}

export function useAbortableAnnotationGeneration(key: string): AbortSignal {
  const generation = useMemo(
    () => ({ key, controller: new AbortController() }),
    [key],
  )
  useEffect(() => () => generation.controller.abort(), [generation])
  return generation.controller.signal
}

export function useAnnotationLifecycle<TExtra>({
  identityKey,
  lease,
  linkId,
  targets,
  emptyExtra,
  readExtra,
  events = DEFAULT_LIFECYCLE_EVENTS,
}: UseAnnotationLifecycleOptions<TExtra>): UseAnnotationLifecycleResult<TExtra> {
  const signal = useAbortableAnnotationGeneration(identityKey)
  const [state, setReactState] = useState<AnnotationLifecycleState<TExtra>>({
    identityKey,
    status: 'idle',
    annotations: EMPTY_ANNOTATIONS,
    annotationStoreVersion: 0,
    extra: emptyExtra,
  })
  const stateRef = useRef(state)

  const setState = useCallback((
    update: AnnotationLifecycleState<TExtra> | (
      (current: AnnotationLifecycleState<TExtra>) => AnnotationLifecycleState<TExtra>
    ),
  ): void => {
    const next = typeof update === 'function' ? update(stateRef.current) : update
    stateRef.current = next
    setReactState(next)
  }, [])

  const refresh = useCallback(async (): Promise<boolean> => {
    const readyEmpty = (): void => setState({
      identityKey,
      status: 'ready',
      annotations: EMPTY_ANNOTATIONS,
      annotationStoreVersion: 0,
      extra: emptyExtra,
    })
    if (!lease || !linkId || signal.aborted) {
      readyEmpty()
      return false
    }
    if (targets.length === 0) {
      readyEmpty()
      return true
    }
    setState((current) => current.identityKey === identityKey
      ? { ...current, status: 'loading' }
      : {
          identityKey,
          status: 'loading',
          annotations: EMPTY_ANNOTATIONS,
          annotationStoreVersion: 0,
          extra: emptyExtra,
        })
    const result = await loadAnnotationTargets({
      lease,
      linkId,
      targets,
      signal,
      emptyExtra,
      readExtra,
    })
    if (result.status === 'stale') return false
    if (result.status === 'empty') {
      readyEmpty()
      return true
    }
    if (result.status === 'failed') {
      setState((current) => current.identityKey === identityKey
        ? { ...current, status: 'error' }
        : {
            identityKey,
            status: 'error',
            annotations: EMPTY_ANNOTATIONS,
            annotationStoreVersion: 0,
            extra: emptyExtra,
          })
      return false
    }
    setState({
      identityKey,
      status: 'ready',
      annotations: result.annotations,
      annotationStoreVersion: result.annotationStoreVersion,
      extra: result.extra,
    })
    return true
  }, [emptyExtra, identityKey, lease, linkId, readExtra, setState, signal, targets])

  useEffect(() => {
    setState({
      identityKey,
      status: 'loading',
      annotations: EMPTY_ANNOTATIONS,
      annotationStoreVersion: 0,
      extra: emptyExtra,
    })
    void refresh()
  }, [emptyExtra, identityKey, refresh, setState])

  useEffect(() => {
    const onChange = () => void refresh()
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') void refresh()
    }
    const unsubscribe = subscribeReaderEvents(events, onChange)
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      unsubscribe()
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [events, refresh])

  return {
    state,
    stateRef,
    signal,
    refresh,
    setState,
  }
}
