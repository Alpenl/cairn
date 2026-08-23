import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  isContentAnchored,
  type Annotation,
  type AnnotationInput,
  type AnnotationPatch,
  type AnnotationTarget,
} from '../lib/annotation-domain'
import {
  buildAnnotationAddOperation,
  buildAnnotationAddOperationFromAnnotation,
  buildAnnotationDeleteOperation,
  buildAnnotationUpdateOperation,
  commitAnnotationCommand,
  loadAnnotationTargets,
  randomAnnotationCommandToken,
  type AnnotationCommandResult,
} from '../lib/annotation-commands'
import {
  AnnotationDocumentChannel,
  type AnnotationChangeHintInput,
} from '../lib/article/document-channel'
import {
  reanchorSavedContentAnnotations,
  type DocumentCommandContext,
  type HistoricalSavedContentAnnotation,
  type SavedContentAnnotationReanchorResult,
  type SavedContentSource,
  type SavedDocumentId,
} from '../lib/article/document'
import {
  isValidSourceIdentityComponent,
  sourceBlockKey,
  type SourceBlockId,
} from '../lib/article/source-block'
import type { IdentityLease } from '../lib/identity'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../lib/reader-events'
import {
  compactAnnotationOperations,
  listAnnotatedLinks,
  readAnnotationSnapshot,
} from '../lib/user-data/annotation-store'
import type { AnnotationOperationInput } from '../lib/user-data/annotation-types'

export interface ArticleAnnotationRevisionChange {
  readonly previousRevision: number
  readonly previousSource: SavedContentSource
  readonly currentSource: SavedContentSource
  readonly currentRevision?: number
}

export interface UseArticleAnnotationsOptions {
  /** Optional source snapshots supplied by the document owner after a revision change. */
  readonly revisionChange?: ArticleAnnotationRevisionChange
}

export type HistoricalArticleAnnotation = HistoricalSavedContentAnnotation

export interface ArticleAnnotationReanchorSummary {
  readonly status: 'committed' | 'degraded' | 'failed' | 'stale'
  readonly reanchoredCount: number
  readonly historicalCount: number
  readonly results: readonly SavedContentAnnotationReanchorResult[]
}

interface LoadState {
  readonly identityKey: string
  readonly status: 'idle' | 'loading' | 'ready' | 'error'
  readonly annotations: readonly Annotation[]
  readonly historicalAnnotations: readonly HistoricalArticleAnnotation[]
  readonly annotationStoreVersion: number
}

export type ArticleAnnotationCommandResult = AnnotationCommandResult

export type ArticleAnnotationReference = Readonly<Annotation>

export interface ArticleAnnotationCommandOptions {
  /** Required for saved-content commands that originate from a document selection. */
  readonly documentContext?: DocumentCommandContext
}

export interface UseArticleAnnotationsResult {
  readonly anns: readonly Annotation[]
  readonly historicalAnnotations: readonly HistoricalArticleAnnotation[]
  /** Short alias for future Reader surfaces that call these items historical anns. */
  readonly historicalAnns: readonly HistoricalArticleAnnotation[]
  readonly loading: boolean
  readonly error: boolean
  readonly reanchoring: boolean
  readonly degraded: boolean
  readonly refresh: () => Promise<boolean>
  readonly reanchor: (
    change: ArticleAnnotationRevisionChange,
  ) => Promise<ArticleAnnotationReanchorSummary>
  readonly add: (
    input: AnnotationInput,
    options?: ArticleAnnotationCommandOptions,
  ) => Promise<ArticleAnnotationCommandResult>
  readonly update: (
    annotation: ArticleAnnotationReference,
    patch: AnnotationPatch,
    options?: ArticleAnnotationCommandOptions,
  ) => Promise<ArticleAnnotationCommandResult>
  readonly remove: (
    annotation: ArticleAnnotationReference,
    options?: ArticleAnnotationCommandOptions,
  ) => Promise<ArticleAnnotationCommandResult>
}

const EMPTY_ANNOTATIONS: readonly Annotation[] = Object.freeze([])
const EMPTY_HISTORICAL_ANNOTATIONS: readonly HistoricalArticleAnnotation[] = Object.freeze([])
const pendingCompactions = new Map<string, Promise<unknown>>()

function savedTarget(contentRevision: number | null): AnnotationTarget | null {
  if (contentRevision === null) return null
  return canonicalAnnotationTarget({ kind: 'saved-content', contentRevision })
}

function summaryTarget(blockKind: SourceBlockId['blockKind'] | null, sourceHash: string | null): AnnotationTarget | null {
  if (blockKind !== 'summary' || sourceHash === null) return null
  return canonicalAnnotationTarget({ kind: 'summary', sourceHash })
}

function safeSourceBlockKey(sourceBlock: SourceBlockId | null): string | null {
  if (!sourceBlock) return null
  try {
    return sourceBlockKey(sourceBlock)
  } catch {
    return null
  }
}

function targetIdentity(target: AnnotationTarget | null): string {
  return target ? annotationTargetKey(target) ?? 'invalid' : 'none'
}

function targetForBlock(
  blockKey: string,
  saved: AnnotationTarget | null,
  summary: AnnotationTarget | null,
): AnnotationTarget | null {
  if (isContentAnchored(blockKey)) return saved
  if (blockKey === 'summary') return summary
  return null
}

type HistoricalTargetInfo = {
  readonly target: AnnotationTarget
  readonly sourceRevision: number
}

function historicalTargetInfo(
  target: AnnotationTarget,
  currentRevision: number,
): HistoricalTargetInfo | null {
  if (target.kind !== 'saved-content' || target.contentRevision >= currentRevision) return null
  return {
    target,
    sourceRevision: target.contentRevision,
  }
}

interface HistoricalReadResult {
  readonly ok: true
  readonly value: readonly HistoricalArticleAnnotation[]
}

interface HistoricalReadFailure {
  readonly ok: false
}

async function readHistoricalAnnotations(
  lease: IdentityLease,
  linkId: string,
  currentRevision: number,
  currentAnnotationIds: ReadonlySet<string>,
): Promise<HistoricalReadResult | HistoricalReadFailure> {
  const index = await listAnnotatedLinks(lease)
  if (!index.ok) return { ok: false }
  const targets = new Map<string, HistoricalTargetInfo>()
  for (const row of index.value) {
    if (row.linkId !== linkId) continue
    const info = historicalTargetInfo(row.target, currentRevision)
    if (info) targets.set(row.targetKey, info)
  }
  if (targets.size === 0) return { ok: true, value: EMPTY_HISTORICAL_ANNOTATIONS }

  const snapshots = await Promise.all(
    [...targets.values()].map(async (info) => ({
      info,
      snapshot: await readAnnotationSnapshot(lease, linkId, info.target),
    })),
  )
  if (snapshots.some(({ snapshot }) => !snapshot.ok)) return { ok: false }

  const candidates: HistoricalArticleAnnotation[] = []
  for (const { info, snapshot } of snapshots) {
    if (!snapshot.ok) continue
    for (const annotation of snapshot.value.annotations) {
      candidates.push({
        status: 'historical',
        reason: 'revision-changed',
        sourceContentRevision: info.sourceRevision,
        annotation,
      })
    }
  }

  const byAnnotationID = new Map<string, HistoricalArticleAnnotation>()
  for (const item of candidates.sort((left, right) =>
    right.sourceContentRevision - left.sourceContentRevision ||
    right.annotation.updatedAt - left.annotation.updatedAt ||
    left.annotation.id.localeCompare(right.annotation.id))) {
    if (currentAnnotationIds.has(item.annotation.id)) continue
    if (!byAnnotationID.has(item.annotation.id)) byAnnotationID.set(item.annotation.id, item)
  }
  const value = [...byAnnotationID.values()].sort((left, right) =>
    left.annotation.createdAt - right.annotation.createdAt ||
    left.annotation.id.localeCompare(right.annotation.id))
  return { ok: true, value: value.length === 0 ? EMPTY_HISTORICAL_ANNOTATIONS : value }
}

function reanchorOperationId(
  annotationId: string,
  fromRevision: number,
  toRevision: number,
  status: SavedContentAnnotationReanchorResult['status'],
  reason: string,
): string {
  return `reanchor:${encodeURIComponent(annotationId)}:${fromRevision}:${toRevision}:${status}:${reason}`
}

function documentContextMatches(
  context: DocumentCommandContext | undefined,
  documentId: SavedDocumentId | null,
): boolean {
  if (!context || !documentId) return false
  return !context.signal.aborted &&
    context.id.namespace === documentId.namespace &&
    context.id.linkId === documentId.linkId &&
    context.id.contentRevision === documentId.contentRevision
}

function scheduleCompaction(
  lease: IdentityLease,
  linkId: string,
  target: AnnotationTarget,
): void {
  const key = `${lease.context.localEpoch}\0${lease.context.physicalNamespace}\0${linkId}\0${targetIdentity(target)}`
  if (pendingCompactions.has(key)) return
  const task = compactAnnotationOperations(lease, linkId, target).finally(() => {
    if (pendingCompactions.get(key) === task) pendingCompactions.delete(key)
  })
  pendingCompactions.set(key, task)
}

export function useArticleAnnotations(
  lease: IdentityLease | null,
  documentId: SavedDocumentId | null,
  sourceBlock: SourceBlockId | null,
  options?: UseArticleAnnotationsOptions,
): UseArticleAnnotationsResult {
  const namespace = lease?.context.physicalNamespace ?? null
  const contentRevision = documentId?.contentRevision ?? null
  const sourceBlockKind = sourceBlock?.blockKind ?? null
  const sourceHash = sourceBlock?.sourceHash ?? null
  const independentSourceKey = useMemo(
    () => safeSourceBlockKey(sourceBlock),
    [sourceBlock],
  )
  const saved = useMemo(
    () => savedTarget(contentRevision),
    [contentRevision],
  )
  const summary = useMemo(
    () => summaryTarget(sourceBlockKind, sourceHash),
    [sourceBlockKind, sourceHash],
  )
  const identitiesCoherent = Boolean(
    lease &&
    isValidSourceIdentityComponent(namespace) &&
    (!documentId || isValidSourceIdentityComponent(documentId.linkId)) &&
    (!documentId || documentId.namespace === namespace) &&
    (!sourceBlock || (
      independentSourceKey !== null &&
      sourceBlock.namespace === namespace
    )) &&
    (!documentId || !sourceBlock || documentId.linkId === sourceBlock.linkId),
  )
  const linkId = identitiesCoherent
    ? documentId?.linkId ?? sourceBlock?.linkId ?? null
    : null
  const identityKey = `${namespace ?? 'none'}\0${linkId ?? 'none'}\0${targetIdentity(saved)}\0${independentSourceKey ?? 'none'}`
  const revisionChange = options?.revisionChange
  const revisionChangeKey = useMemo(() => {
    if (!revisionChange) return null
    return `${identityKey}\0${revisionChange.previousRevision}\0${revisionChange.currentRevision ?? contentRevision ?? 'current'}\0${JSON.stringify(revisionChange.previousSource)}\0${JSON.stringify(revisionChange.currentSource)}`
  }, [contentRevision, identityKey, revisionChange])
  const generation = useMemo(
    () => ({ key: identityKey, controller: new AbortController() }),
    [identityKey],
  )
  const savedCommandKey = `${namespace ?? 'none'}\0${linkId ?? 'none'}\0${targetIdentity(saved)}`
  const savedCommandGeneration = useMemo(
    () => ({ key: savedCommandKey, controller: new AbortController() }),
    [savedCommandKey],
  )
  const summaryCommandKey = `${namespace ?? 'none'}\0${linkId ?? 'none'}\0${targetIdentity(summary)}`
  const summaryCommandGeneration = useMemo(
    () => ({ key: summaryCommandKey, controller: new AbortController() }),
    [summaryCommandKey],
  )
  const [state, setState] = useState<LoadState>({
    identityKey,
    status: 'idle',
    annotations: EMPTY_ANNOTATIONS,
    historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
    annotationStoreVersion: 0,
  })
  const [reanchorState, setReanchorState] = useState<{
    readonly identityKey: string
    readonly status: 'idle' | 'running' | 'committed' | 'degraded' | 'failed' | 'stale'
  }>({ identityKey, status: revisionChange ? 'running' : 'idle' })
  const stateRef = useRef(state)
  const updateState = useCallback((
    update: LoadState | ((current: LoadState) => LoadState),
  ): void => {
    const next = typeof update === 'function' ? update(stateRef.current) : update
    stateRef.current = next
    setState(next)
  }, [])
  const channelRef = useRef<AnnotationDocumentChannel | null>(null)

  useEffect(() => () => generation.controller.abort(), [generation])
  useEffect(
    () => () => savedCommandGeneration.controller.abort(),
    [savedCommandGeneration],
  )
  useEffect(
    () => () => summaryCommandGeneration.controller.abort(),
    [summaryCommandGeneration],
  )

  const refresh = useCallback(async (): Promise<boolean> => {
    if (!lease || !linkId || generation.controller.signal.aborted) {
      updateState({
        identityKey,
        status: 'ready',
        annotations: EMPTY_ANNOTATIONS,
        historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
        annotationStoreVersion: 0,
      })
      return false
    }
    const targets = [saved, summary].filter((target): target is AnnotationTarget => target !== null)
    if (targets.length === 0) {
      updateState({
        identityKey,
        status: 'ready',
        annotations: EMPTY_ANNOTATIONS,
        historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
        annotationStoreVersion: 0,
      })
      return true
    }
    updateState((current) => current.identityKey === identityKey
      ? { ...current, status: 'loading' }
        : {
            identityKey,
            status: 'loading',
            annotations: EMPTY_ANNOTATIONS,
            historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
            annotationStoreVersion: 0,
          })
    const [loadResult, historicalResult] = await Promise.all([
      loadAnnotationTargets(lease, linkId, targets),
      saved && contentRevision !== null
        ? readHistoricalAnnotations(lease, linkId, contentRevision, new Set())
        : Promise.resolve<HistoricalReadResult>({
            ok: true,
            value: EMPTY_HISTORICAL_ANNOTATIONS,
      }),
    ])
    if (generation.controller.signal.aborted) return false
    if (!loadResult.ok || !historicalResult.ok) {
      updateState((current) => current.identityKey === identityKey
        ? { ...current, status: 'error' }
        : {
            identityKey,
            status: 'error',
            annotations: EMPTY_ANNOTATIONS,
            historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
            annotationStoreVersion: 0,
      })
      return false
    }
    const currentAnnotationIds = new Set(loadResult.annotations.map((item) => item.id))
    updateState({
      identityKey,
      status: 'ready',
      annotations: loadResult.annotations,
      historicalAnnotations: historicalResult.value.filter(
        (item) => !currentAnnotationIds.has(item.annotation.id),
      ),
      annotationStoreVersion: loadResult.annotationStoreVersion,
    })
    return true
  }, [contentRevision, generation, identityKey, lease, linkId, saved, summary, updateState])

  const reanchor = useCallback(async (
    change: ArticleAnnotationRevisionChange,
  ): Promise<ArticleAnnotationReanchorSummary> => {
    const currentRevision = documentId?.contentRevision ?? null
    const signal = savedCommandGeneration.controller.signal
    const stale = (): ArticleAnnotationReanchorSummary => ({
      status: 'stale',
      reanchoredCount: 0,
      historicalCount: 0,
      results: [],
    })
    if (
      !lease ||
      !linkId ||
      currentRevision === null ||
      !Number.isSafeInteger(change.previousRevision) ||
      change.previousRevision <= 0 ||
      !Number.isSafeInteger(currentRevision) ||
      currentRevision <= change.previousRevision ||
      (change.currentRevision !== undefined && change.currentRevision !== currentRevision) ||
      signal.aborted
    ) {
      setReanchorState({ identityKey, status: 'stale' })
      return stale()
    }

    setReanchorState({ identityKey, status: 'running' })
    const previous = await readAnnotationSnapshot(lease, linkId, {
      kind: 'saved-content',
      contentRevision: change.previousRevision,
    })
    if (!previous.ok) {
      setReanchorState({ identityKey, status: signal.aborted ? 'stale' : 'failed' })
      return signal.aborted ? stale() : {
        status: 'failed',
        reanchoredCount: 0,
        historicalCount: 0,
        results: [],
      }
    }

    const results = reanchorSavedContentAnnotations(
      previous.value.annotations,
      change.previousRevision,
      currentRevision,
      change.previousSource,
      change.currentSource,
      linkId,
    )
    let reanchoredCount = 0
    let historicalCount = 0
    for (const result of results) {
      if (signal.aborted) {
        setReanchorState({ identityKey, status: 'stale' })
        return { status: 'stale', reanchoredCount, historicalCount, results }
      }
      if (result.status !== 'reanchored') {
        historicalCount += 1
        continue
      }
      const target: AnnotationTarget = {
        kind: 'saved-content',
        contentRevision: currentRevision,
      }
      const operation = buildAnnotationAddOperationFromAnnotation({
        linkId,
        annotation: result.annotation,
        target,
        opId: reanchorOperationId(
          result.annotation.id,
          change.previousRevision,
          currentRevision,
          result.status,
          result.reason,
        ),
      })
      if (!operation) {
        setReanchorState({ identityKey, status: 'failed' })
        return { status: 'failed', reanchoredCount, historicalCount, results }
      }
      const committed = await commitAnnotationCommand({
        lease,
        linkId,
        target,
        operation,
        annotationId: result.annotation.id,
        commandSignal: signal,
        afterCommit: (outcome) => {
          const hint: AnnotationChangeHintInput = {
            linkId,
            documentRevision: currentRevision,
            annotationStoreVersion: outcome.annotationStoreVersion,
          }
          channelRef.current?.publish(hint)
          emitReaderEvent(READER_EVENTS.annotationsChanged)
          scheduleCompaction(lease, linkId, target)
        },
      })
      if (committed.status !== 'committed' && committed.status !== 'duplicate') {
        const status = committed.status === 'stale' ? 'stale' : 'failed'
        setReanchorState({ identityKey, status })
        return { status, reanchoredCount, historicalCount, results }
      }
      reanchoredCount += 1
    }

    if (signal.aborted) {
      setReanchorState({ identityKey, status: 'stale' })
      return { status: 'stale', reanchoredCount, historicalCount, results }
    }
    await refresh()
    const status = historicalCount > 0 ? 'degraded' : 'committed'
    setReanchorState({ identityKey, status })
    return { status, reanchoredCount, historicalCount, results }
  }, [documentId, identityKey, lease, linkId, refresh, savedCommandGeneration])

  useEffect(() => {
    updateState({
      identityKey,
      status: 'loading',
      annotations: EMPTY_ANNOTATIONS,
      historicalAnnotations: EMPTY_HISTORICAL_ANNOTATIONS,
      annotationStoreVersion: 0,
    })
    void refresh()
  }, [identityKey, refresh, updateState])

  const startedRevisionChange = useRef<string | null>(null)
  useEffect(() => {
    if (!revisionChange || !revisionChangeKey || startedRevisionChange.current === revisionChangeKey) return
    startedRevisionChange.current = revisionChangeKey
    void reanchor(revisionChange)
  }, [reanchor, revisionChange, revisionChangeKey])

  useEffect(() => {
    channelRef.current?.dispose()
    channelRef.current = null
    if (!lease || !linkId) return
    const channel = new AnnotationDocumentChannel(lease, (hint) => {
      const revisionMatches =
        (saved?.kind === 'saved-content' &&
          hint.documentRevision === saved.contentRevision) ||
        (summary?.kind === 'summary' && hint.documentRevision === 0)
      const loaded = stateRef.current
      if (
        hint.linkId !== linkId ||
        !revisionMatches ||
        loaded.identityKey !== identityKey ||
        hint.annotationStoreVersion <= loaded.annotationStoreVersion
      ) {
        return
      }
      void refresh()
    })
    channelRef.current = channel
    return () => {
      channel.dispose()
      if (channelRef.current === channel) channelRef.current = null
    }
  }, [identityKey, lease, linkId, refresh, saved, summary])

  useEffect(() => {
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') void refresh()
    }
    const onLocalAnnotationChange = () => void refresh()
    const onThoughtsSync = () => void refresh()
    const unsubscribeLocalChange = subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged],
      onLocalAnnotationChange,
    )
    const unsubscribeThoughtsSync = subscribeReaderEvents(
      [READER_EVENTS.thoughtsSynced],
      onThoughtsSync,
    )
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      unsubscribeLocalChange()
      unsubscribeThoughtsSync()
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [refresh])

  const commit = useCallback(async (
    operation: AnnotationOperationInput,
    target: AnnotationTarget,
    annotationId: string,
    options: ArticleAnnotationCommandOptions | undefined,
  ): Promise<ArticleAnnotationCommandResult> => {
    if (!lease || !linkId) return { status: 'stale' }
    const savedCommand = target.kind === 'saved-content'
    const commandSignal = savedCommand
      ? savedCommandGeneration.controller.signal
      : summaryCommandGeneration.controller.signal
    const publicationChannel = channelRef.current
    return commitAnnotationCommand({
      lease,
      linkId,
      target,
      operation,
      annotationId,
      commandSignal,
      externalSignals: savedCommand && options?.documentContext
        ? [options.documentContext.signal]
        : [],
      isStale: () => savedCommand &&
        !documentContextMatches(options?.documentContext, documentId),
      afterCommit: async (outcome) => {
        await refresh()
        const hint: AnnotationChangeHintInput = {
          linkId,
          documentRevision: target.kind === 'saved-content'
            ? target.contentRevision
            : 0,
          annotationStoreVersion: outcome.annotationStoreVersion,
        }
        publicationChannel?.publish(hint)
        emitReaderEvent(READER_EVENTS.annotationsChanged)
        scheduleCompaction(lease, linkId, target)
      },
    })
  }, [
    documentId,
    lease,
    linkId,
    refresh,
    savedCommandGeneration,
    summaryCommandGeneration,
  ])

  const add = useCallback(async (
    input: AnnotationInput,
    options?: ArticleAnnotationCommandOptions,
  ): Promise<ArticleAnnotationCommandResult> => {
    const target = targetForBlock(input.blockKey, saved, summary)
    const annotationToken = randomAnnotationCommandToken()
    const operationToken = randomAnnotationCommandToken()
    if (!target) return { status: 'unsupported' }
    if (!annotationToken || !operationToken) return { status: 'failed' }
    const annotationId = `an:${annotationToken}`
    const operation = buildAnnotationAddOperation({
      linkId: linkId ?? '',
      target,
      annotationId,
      opId: `op:${operationToken}`,
      annotation: input,
    })
    if (!operation) return { status: 'unsupported' }
    return commit(operation, target, annotationId, options)
  }, [commit, linkId, saved, summary])

  const update = useCallback(async (
    annotation: ArticleAnnotationReference,
    patch: AnnotationPatch,
    options?: ArticleAnnotationCommandOptions,
  ): Promise<ArticleAnnotationCommandResult> => {
    const target = targetForBlock(annotation.blockKey, saved, summary)
    const operationToken = randomAnnotationCommandToken()
    if (!target) return { status: 'unsupported' }
    if (!operationToken) return { status: 'failed' }
    const operation = buildAnnotationUpdateOperation({
      linkId: linkId ?? '',
      target,
      annotation,
      patch,
      opId: `op:${operationToken}`,
    })
    if (!operation) return { status: 'stale' }
    return commit(operation, target, annotation.id, options)
  }, [commit, linkId, saved, summary])

  const remove = useCallback(async (
    annotation: ArticleAnnotationReference,
    options?: ArticleAnnotationCommandOptions,
  ): Promise<ArticleAnnotationCommandResult> => {
    const target = targetForBlock(annotation.blockKey, saved, summary)
    const operationToken = randomAnnotationCommandToken()
    if (!target) return { status: 'unsupported' }
    if (!operationToken) return { status: 'failed' }
    const operation = buildAnnotationDeleteOperation({
      linkId: linkId ?? '',
      target,
      annotation,
      opId: `op:${operationToken}`,
    })
    if (!operation) return { status: 'stale' }
    return commit(operation, target, annotation.id, options)
  }, [commit, linkId, saved, summary])

  const visibleState = state.identityKey === identityKey ? state : null
  const visibleReanchorState = reanchorState.identityKey === identityKey
    ? reanchorState
    : { identityKey, status: 'idle' as const }
  const historicalAnnotations = visibleState?.historicalAnnotations ?? EMPTY_HISTORICAL_ANNOTATIONS

  return {
    anns: visibleState?.annotations ?? EMPTY_ANNOTATIONS,
    historicalAnnotations,
    historicalAnns: historicalAnnotations,
    loading: visibleState === null ||
      visibleState.status === 'loading' ||
      visibleState.status === 'idle',
    error: visibleState?.status === 'error',
    reanchoring: visibleReanchorState.status === 'running',
    degraded: visibleReanchorState.status === 'degraded',
    refresh,
    reanchor,
    add,
    update,
    remove,
  }
}
