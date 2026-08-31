import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import type { Annotation } from '../lib/annotations'
import type { IdentityLease } from '../lib/identity'
import {
  emitReaderEvent,
  READER_EVENTS,
  subscribeReaderEvents,
  type ReaderEventName,
} from '../lib/reader-events'
import {
  compactAnnotationOperations,
  commitAnnotationOperation,
  readAnnotationSnapshot,
  type AnnotationCommitResult,
  type AnnotationOperationInput,
  type AnnotationSnapshot,
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

export type AnnotationMutationCommittedResult = {
  readonly status: 'committed' | 'duplicate'
  readonly annotationId: string
  readonly sequence: number
  readonly annotationStoreVersion: number
}

export type AnnotationMutationResult =
  | AnnotationMutationCommittedResult
  | { readonly status: 'stale' | 'failed' | 'op-id-conflict' }

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

function mergeSnapshots(
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
  if (!lease || !linkId || signal.aborted) return { status: 'stale' }
  const result = await commitAnnotationOperation(lease, operation, { signal })
  if (!result.ok) return signal.aborted ? { status: 'stale' } : { status: 'failed' }
  const outcome = annotationMutationResult(result.value, annotationId)
  if (outcome.status !== 'committed' && outcome.status !== 'duplicate') return outcome
  await refresh()
  afterCommit?.(outcome)
  emitReaderEvent(READER_EVENTS.annotationsChanged)
  scheduleAnnotationCompaction(lease, linkId, target)
  return outcome
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
    const results = await Promise.all(
      targets.map((target) => readAnnotationSnapshot(lease, linkId, target)),
    )
    if (signal.aborted) return false
    if (results.some((result) => !result.ok)) {
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
    const merged = mergeSnapshots(results.flatMap((result) => result.ok ? [result.value] : []))
    const extraResult = readExtra
      ? await readExtra({
          lease,
          linkId,
          targets,
          annotations: merged.annotations,
          annotationStoreVersion: merged.version,
        })
      : { ok: true as const, value: emptyExtra }
    if (signal.aborted) return false
    if (!extraResult.ok) {
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
      annotations: merged.annotations,
      annotationStoreVersion: merged.version,
      extra: extraResult.value,
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
