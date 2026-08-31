import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ApiError, ApiResult } from '@webtag/api'
import type { LinkResponse } from '../lib/api/types'
import { isLinkResponse } from '../lib/api/guards'
import {
  AnnotationDocumentChannel,
  type AnnotationChangeHint,
} from '../lib/article/document-channel'
import {
  ANNOTATED_LINKS_CACHE_KEY,
  linkDetailCacheKey,
} from '../lib/cache/keys'
import { resourceStore } from '../lib/cache/store'
import type { IdentityLease } from '../lib/identity'
import { READER_EVENTS, subscribeReaderEvents } from '../lib/reader-events'
import { isCompletedReadingLink } from '../lib/stats'
import { listAnnotatedLinks } from '../lib/user-data/annotation-store'
import type { AnnotatedLinkRecord } from '../lib/user-data/annotation-types'
import type { LibraryReloadOptions } from './libraryReload'

export {
  ANNOTATED_LINKS_CACHE_KEY,
  linkDetailCacheKey,
} from '../lib/cache/keys'

export interface AnnotatedLinksState {
  readonly links: readonly LinkResponse[]
  readonly loading: boolean
  readonly error: ApiError | null
  readonly reload: (
    options?: LibraryReloadOptions,
  ) => Promise<ApiResult<readonly LinkResponse[]>>
}

interface LoadState {
  readonly identityKey: string | null
  readonly links: readonly LinkResponse[]
  readonly loading: boolean
  readonly error: ApiError | null
}

const EMPTY_LINKS: readonly LinkResponse[] = Object.freeze([])
const MAX_CONCURRENT_POINT_READS = 6

function cancelledReload<T>(): ApiResult<T> {
  return {
    ok: false,
    error: { kind: 'other', message: '资料库刷新已取消' },
  }
}

function staleIdentity<T>(): ApiResult<T> {
  return {
    ok: false,
    error: {
      kind: 'identity-mismatch',
      message: 'Reader client does not own the active cache identity',
    },
  }
}

interface ReloadWaiter {
  readonly signal?: AbortSignal
  readonly resolve: (result: ApiResult<readonly LinkResponse[]>) => void
  readonly onAbort: () => void
  settled: boolean
}

interface AnnotatedIndexSnapshot {
  readonly linkIds: readonly string[]
  readonly versionByLinkId: ReadonlyMap<string, number>
}

function sortByCreatedDesc(links: readonly LinkResponse[]): readonly LinkResponse[] {
  return [...links].sort((left, right) =>
    left.created_at < right.created_at ? 1 : left.created_at > right.created_at ? -1 : 0)
}

function reuseUnchangedProjection(
  current: LoadState,
  identityKey: string,
  links: readonly LinkResponse[],
): readonly LinkResponse[] {
  if (
    current.identityKey === identityKey &&
    current.links.length === links.length &&
    current.links.every((link, index) => link === links[index])
  ) {
    return current.links
  }
  return links
}

function readCachedLink(lease: IdentityLease, linkId: string): LinkResponse | null {
  if (!resourceStore.isIdentityActive(lease)) return null
  const cached: unknown = resourceStore.peek(linkDetailCacheKey(linkId)).data
  return isLinkResponse(cached) && cached.id === linkId ? cached : null
}

function aggregateAnnotatedIndex(rows: readonly AnnotatedLinkRecord[]): AnnotatedIndexSnapshot {
  const versionByLinkId = new Map<string, number>()
  for (const row of rows) {
    versionByLinkId.set(
      row.linkId,
      Math.max(versionByLinkId.get(row.linkId) ?? 0, row.annotationStoreVersion),
    )
  }
  return {
    linkIds: [...versionByLinkId.keys()].sort((left, right) => left.localeCompare(right)),
    versionByLinkId,
  }
}

function advanceVersionHighWater(
  highWater: Map<string, number>,
  versions: ReadonlyMap<string, number>,
): void {
  for (const [linkId, version] of versions) {
    highWater.set(linkId, Math.max(highWater.get(linkId) ?? 0, version))
  }
}

function acceptNewerHint(
  highWater: Map<string, number>,
  hint: AnnotationChangeHint,
): boolean {
  const previous = highWater.get(hint.linkId) ?? 0
  if (hint.annotationStoreVersion <= previous) return false
  highWater.set(hint.linkId, hint.annotationStoreVersion)
  return true
}

function useAnnotationVersionHighWater(identityKey: string | null): Map<string, number> {
  return useMemo(() => ({
    identityKey,
    versions: new Map<string, number>(),
  }), [identityKey]).versions
}

export function useAnnotatedLinks(
  client: IdentityBoundReaderClient,
  enabled = true,
): AnnotatedLinksState {
  const lease = client.identityLease
  const sessionActive = resourceStore.isIdentityActive(lease)
  const identityKey = enabled && sessionActive
    ? `${lease.context.physicalNamespace}\0${lease.context.localEpoch}`
    : null
  const versionHighWater = useAnnotationVersionHighWater(identityKey)
  const [reloadSequence, setReloadSequence] = useState(0)
  const nextReloadSequence = useRef(0)
  const reloadWaiters = useRef(new Map<number, ReloadWaiter>())
  const reloadControllers = useRef(new Map<number, AbortController>())
  const cancelledReloads = useRef(new Set<number>())
  const [state, setState] = useState<LoadState>({
    identityKey: null,
    links: EMPTY_LINKS,
    loading: false,
    error: null,
  })

  const settleReload = useCallback((
    sequence: number,
    result: ApiResult<readonly LinkResponse[]>,
  ) => {
    const waiter = reloadWaiters.current.get(sequence)
    if (!waiter || waiter.settled) return
    waiter.settled = true
    waiter.signal?.removeEventListener('abort', waiter.onAbort)
    reloadWaiters.current.delete(sequence)
    waiter.resolve(result)
  }, [])

  const reload = useCallback((
    options: LibraryReloadOptions = {},
  ): Promise<ApiResult<readonly LinkResponse[]>> => {
    for (const pendingSequence of [...reloadWaiters.current.keys()]) {
      const pendingController = reloadControllers.current.get(pendingSequence)
      if (pendingController) {
        cancelledReloads.current.add(pendingSequence)
        pendingController.abort()
      }
      settleReload(pendingSequence, cancelledReload())
    }
    const sequence = nextReloadSequence.current + 1
    nextReloadSequence.current = sequence
    return new Promise((resolve) => {
      const onAbort = () => {
        cancelledReloads.current.add(sequence)
        reloadControllers.current.get(sequence)?.abort()
        settleReload(sequence, cancelledReload())
      }
      const waiter: ReloadWaiter = {
        signal: options.signal,
        resolve,
        onAbort,
        settled: false,
      }
      reloadWaiters.current.set(sequence, waiter)
      if (options.signal?.aborted) {
        onAbort()
      } else {
        options.signal?.addEventListener('abort', onAbort, { once: true })
      }
      setReloadSequence(sequence)
    })
  }, [settleReload])

  useEffect(() => {
    const controllers = reloadControllers.current
    const cancellations = cancelledReloads.current
    const controller = new AbortController()
    controllers.set(reloadSequence, controller)
    if (cancellations.delete(reloadSequence)) {
      controllers.delete(reloadSequence)
      controller.abort()
      return
    }
    if (!enabled || !identityKey || !sessionActive) {
      const error: ApiError = {
        kind: 'identity-mismatch',
        message: 'Reader client does not own the active cache identity',
      }
      const failure = staleIdentity<readonly LinkResponse[]>()
      setState({
        identityKey: null,
        links: EMPTY_LINKS,
        loading: false,
        error: enabled ? error : null,
      })
      settleReload(reloadSequence, failure)
      return () => {
        controllers.delete(reloadSequence)
        controller.abort()
      }
    }
    const ownership = lease.captureOwnership('load annotated links')
    const stopLoadingAfterCancellation = () => {
      if (
        !cancellations.has(reloadSequence) ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) return
      setState((current) => current.identityKey === identityKey && current.loading
        ? { ...current, loading: false }
        : current)
    }
    const abortForIdentity = () => {
      controller.abort()
      settleReload(reloadSequence, staleIdentity())
    }
    controller.signal.addEventListener('abort', stopLoadingAfterCancellation, { once: true })
    ownership.operation.signal.addEventListener('abort', abortForIdentity, { once: true })
    setState((current) => ({
      identityKey,
      links: current.identityKey === identityKey ? current.links : EMPTY_LINKS,
      loading: true,
      error: null,
    }))

    void (async (): Promise<ApiResult<readonly LinkResponse[]>> => {
      const indexedRows = await listAnnotatedLinks(lease)
      if (
        controller.signal.aborted ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) {
        return cancellations.has(reloadSequence)
          ? cancelledReload()
          : staleIdentity()
      }
      if (!indexedRows.ok) {
        const error: ApiError = { kind: 'other', message: '无法读取划线链接索引' }
        setState({
          identityKey,
          links: EMPTY_LINKS,
          loading: false,
          error,
        })
        return { ok: false, error }
      }

      const indexed = aggregateAnnotatedIndex(indexedRows.value)
      advanceVersionHighWater(versionHighWater, indexed.versionByLinkId)

      const loaded = indexed.linkIds.map((linkId) => readCachedLink(lease, linkId))
      const cachedLinks = loaded.filter(
        (link): link is LinkResponse => link !== null && isCompletedReadingLink(link),
      )
      if (cachedLinks.length > 0) {
        const sortedCachedLinks = sortByCreatedDesc(cachedLinks)
        setState((current) => ({
          identityKey,
          // Same-identity reloads keep the complete projection visible while misses resolve.
          // Initial loads can still reveal any point-cache hits immediately.
          links: current.identityKey === identityKey && current.links.length > 0
            ? current.links
            : reuseUnchangedProjection(current, identityKey, sortedCachedLinks),
          loading: true,
          error: null,
        }))
      }
      let nextIndex = 0
      let firstFatalFailure: ApiError | null = null
      const worker = async (): Promise<void> => {
        while (!controller.signal.aborted) {
          const index = nextIndex
          nextIndex += 1
          if (index >= indexed.linkIds.length) return
          if (!resourceStore.isIdentityActive(lease)) return
          const linkId = indexed.linkIds[index]
          if (loaded[index] !== null) continue
          const result = await client.getLink(linkId, { signal: controller.signal })
          if (
            controller.signal.aborted ||
            !lease.isOwnershipCurrent(ownership) ||
            !resourceStore.isIdentityActive(lease)
          ) return
          if (!result.ok) {
            // The durable index can outlive a backend link deletion. A 404 is therefore an
            // orphaned index member, not a reason to hide every other annotated link.
            if (result.error.status === 404) {
              loaded[index] = null
            } else {
              firstFatalFailure ??= result.error
            }
            continue
          }
          if (result.data.id !== linkId) {
            loaded[index] = null
            firstFatalFailure ??= { kind: 'other', message: '单条链接响应身份不匹配' }
            continue
          }
          loaded[index] = result.data
          resourceStore.setForIdentity(ownership, linkDetailCacheKey(linkId), result.data)
        }
      }
      await Promise.all(Array.from(
        { length: Math.min(MAX_CONCURRENT_POINT_READS, indexed.linkIds.length) },
        () => worker(),
      ))
      if (
        controller.signal.aborted ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) {
        return cancellations.has(reloadSequence)
          ? cancelledReload()
          : staleIdentity()
      }
      const links = loaded.filter(
        (link): link is LinkResponse => link !== null && isCompletedReadingLink(link),
      )
      if (firstFatalFailure) {
        setState({ identityKey, links: EMPTY_LINKS, loading: false, error: firstFatalFailure })
        return { ok: false, error: firstFatalFailure }
      }

      const sortedLinks = sortByCreatedDesc(links)
      setState((current) => ({
        identityKey,
        links: reuseUnchangedProjection(current, identityKey, sortedLinks),
        loading: false,
        error: null,
      }))
      return { ok: true, data: sortedLinks }
    })().then(
      (result) => {
        cancellations.delete(reloadSequence)
        settleReload(reloadSequence, result)
      },
      (thrown: unknown) => {
        const error: ApiError = {
          kind: 'other',
          message: thrown instanceof Error ? thrown.message : String(thrown),
        }
        if (!controller.signal.aborted) {
          setState({ identityKey, links: EMPTY_LINKS, loading: false, error })
        }
        cancellations.delete(reloadSequence)
        settleReload(reloadSequence, controller.signal.aborted ? cancelledReload() : { ok: false, error })
      },
    )

    return () => {
      controller.signal.removeEventListener('abort', stopLoadingAfterCancellation)
      ownership.operation.signal.removeEventListener('abort', abortForIdentity)
      controllers.delete(reloadSequence)
      cancellations.delete(reloadSequence)
      controller.abort()
      settleReload(reloadSequence, cancelledReload())
    }
  }, [client, enabled, identityKey, lease, reloadSequence, sessionActive, settleReload, versionHighWater])

  useEffect(() => () => {
    for (const sequence of [...reloadWaiters.current.keys()]) {
      reloadControllers.current.get(sequence)?.abort()
      settleReload(sequence, cancelledReload())
    }
  }, [settleReload])

  useEffect(() => {
    if (!enabled || !sessionActive) return
    const requestReload = () => {
      void reload()
    }
    const unsubscribeInvalidation = resourceStore.subscribe(
      ANNOTATED_LINKS_CACHE_KEY,
      requestReload,
    )
    const channel = new AnnotationDocumentChannel(lease, (hint) => {
      if (acceptNewerHint(versionHighWater, hint)) requestReload()
    })
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') requestReload()
    }
    const unsubscribeAnnotationsChanged = subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged],
      requestReload,
    )
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      unsubscribeInvalidation()
      channel.dispose()
      unsubscribeAnnotationsChanged()
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [enabled, lease, reload, sessionActive, versionHighWater])

  const visible = state.identityKey === identityKey
    ? state
    : {
        identityKey,
        links: EMPTY_LINKS,
        loading: identityKey !== null,
        error: null,
      }

  return useMemo(() => ({
    links: visible.links,
    loading: visible.loading,
    error: visible.error,
    reload,
  }), [reload, visible.error, visible.links, visible.loading])
}

export function useAnnotatedLinkCount(
  client: IdentityBoundReaderClient,
): number | undefined {
  const annotated = useAnnotatedLinks(client)
  if (annotated.loading || annotated.error) return undefined
  return annotated.links.length
}
