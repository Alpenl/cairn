import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReaderClient } from '../lib/api/client'
import { err, type ApiError } from '@webtag/api'
import type { ListSitesParams, PaginatedSitesResponse, SiteListItemResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'
import type { ReaderCapabilityLease } from '../lib/capabilities'

export const SITES_CACHE_PREFIX = 'GET /api/sites'
export const SITES_PAGE_SIZE = 30

const NO_SITES: SiteListItemResponse[] = []
const CAPABILITY_REVOKED_ERROR: ApiError = Object.freeze({
  kind: 'identity-mismatch',
  message: 'Reader site capability is not current',
})
const clientIDs = new WeakMap<object, number>()
let nextClientID = 1

interface SitesPaginationState {
  streamKey: string
  extraPages: SiteListItemResponse[]
  page: number
  loadingMore: boolean
  pageError: ApiError | null
}

interface PageRequest {
  streamKey: string
  epoch: number
}

/** 站点列表的缓存键。参数直接拼进键——不同筛选是不同的资源。 */
function sitesKey(params: ListSitesParams): string {
  const query = new URLSearchParams()
  if (params.view) query.set('view', params.view)
  if (params.tags?.trim()) query.set('tags', params.tags.trim())
  if (params.recentCutoff?.trim()) query.set('recent_cutoff', params.recentCutoff.trim())
  if (params.page && params.page > 1) query.set('page', String(params.page))
  if (params.limit && params.limit > 0) query.set('limit', String(params.limit))
  return SITES_CACHE_PREFIX + (query.size ? `?${query}` : '')
}

function clientKey(client: ReaderClient): number {
  const object = client as object
  const known = clientIDs.get(object)
  if (known !== undefined) return known
  const assigned = nextClientID
  nextClientID += 1
  clientIDs.set(object, assigned)
  return assigned
}

function firstPageQuery(params: ListSitesParams): ListSitesParams {
  const tags = params.tags?.trim()
  return {
    ...(params.view ? { view: params.view } : {}),
    ...(tags ? { tags } : {}),
    page: 1,
    limit: SITES_PAGE_SIZE,
  }
}

/** Keep the first server occurrence for each ID, preserving page order. */
function mergeUnique(
  first: readonly SiteListItemResponse[],
  second: readonly SiteListItemResponse[],
): SiteListItemResponse[] {
  const seen = new Set<string>()
  const merged: SiteListItemResponse[] = []
  for (const item of [...first, ...second]) {
    if (seen.has(item.id)) continue
    seen.add(item.id)
    merged.push(item)
  }
  return merged
}

function emptyPagination(streamKey: string): SitesPaginationState {
  return {
    streamKey,
    extraPages: [],
    page: 1,
    loadingMore: false,
    pageError: null,
  }
}

/**
 * Paginated sites for one stable identity and filter stream.
 *
 * Page one uses the shared ResourceStore. Later pages intentionally remain
 * local: they are an append-only UI action and must disappear together when
 * the filter or identity changes.
 */
export function useSites(
  client: ReaderClient,
  capabilityLease: ReaderCapabilityLease,
  params: ListSitesParams = {},
) {
  const { view, tags } = params
  const firstQuery = useMemo(
    () => firstPageQuery({ view, tags }),
    [view, tags],
  )
  const owner = useMemo(() => clientKey(client), [client])
  const readable = capabilityLease.isCurrent('siteRead')
  const baseKey = useMemo(() => sitesKey(firstQuery), [firstQuery])
  const key = readable
    ? `${baseKey}#capability=${capabilityLease.generation}`
    : null
  const streamKey = useMemo(
    () => `${baseKey}#capability=${capabilityLease.generation}#client=${owner}`,
    [baseKey, capabilityLease.generation, owner],
  )
  const first = useCachedResource<PaginatedSitesResponse>(
    key,
    async () => {
      const operationLease = capabilityLease
      if (!operationLease.isCurrent('siteRead')) return err(CAPABILITY_REVOKED_ERROR)
      const result = await client.getSites(firstQuery)
      return client.isIdentityCurrent() && operationLease.isCurrent('siteRead')
        ? result
        : err(CAPABILITY_REVOKED_ERROR)
    },
    { enabled: readable },
  )
  const firstOwnerRef = useRef(owner)

  useEffect(() => {
    if (firstOwnerRef.current === owner) return
    firstOwnerRef.current = owner
    // The logical cache key stays stable across identities because ResourceStore
    // owns the physical namespace. A retained hook still needs one forced fetch
    // so the new client, rather than an old in-flight fetcher, owns page one.
    void first.reload()
  }, [first, owner])

  const [pagination, setPagination] = useState<SitesPaginationState>(
    () => emptyPagination(streamKey),
  )
  const streamKeyRef = useRef(streamKey)
  const paginationEpochRef = useRef(0)
  const pageRequestRef = useRef<PageRequest | null>(null)
  streamKeyRef.current = streamKey

  useEffect(() => {
    paginationEpochRef.current += 1
    pageRequestRef.current = null
    setPagination((current) => (
      current.streamKey === streamKey ? current : emptyPagination(streamKey)
    ))
  }, [streamKey])

  // State cleanup happens after render, so ownership must also be enforced
  // while deriving the render value. This prevents one transition frame from
  // exposing a previous filter or identity's appended pages.
  const ownsPagination = pagination.streamKey === streamKey
  const extraPages = ownsPagination ? pagination.extraPages : NO_SITES
  const page = ownsPagination ? pagination.page : 1
  const loadingMore = ownsPagination && pagination.loadingMore
  const pageError = ownsPagination ? pagination.pageError : null

  const items = useMemo(
    () => readable ? mergeUnique(first.data?.items ?? NO_SITES, extraPages) : NO_SITES,
    [first.data, extraPages, readable],
  )
  const total = readable ? first.data?.total ?? 0 : 0
  const recentCutoff = readable ? first.data?.recent_cutoff : undefined
  const hasStableSnapshot = firstQuery.view !== 'recent' || Boolean(recentCutoff)
  const hasMore = readable && !first.loading && hasStableSnapshot && items.length < total

  const reload = useCallback(async (): Promise<void> => {
    if (!capabilityLease.isCurrent('siteRead')) return
    paginationEpochRef.current += 1
    pageRequestRef.current = null
    setPagination(emptyPagination(streamKey))
    await first.reload()
  }, [capabilityLease, first, streamKey])

  const loadMore = useCallback(async (): Promise<void> => {
    if (
      first.loading ||
      !capabilityLease.isCurrent('siteRead') ||
      pageRequestRef.current?.streamKey === streamKey ||
      loadingMore ||
      !hasMore
    ) return

    const requestedStream = streamKey
    const requestedEpoch = paginationEpochRef.current
    const nextPage = page + 1
    const request = { streamKey: requestedStream, epoch: requestedEpoch }
    pageRequestRef.current = request
    setPagination((current) => ({
      ...(current.streamKey === streamKey ? current : emptyPagination(streamKey)),
      loadingMore: true,
    }))

    const result = await client.getSites({
      ...firstQuery,
      page: nextPage,
      ...(firstQuery.view === 'recent' && recentCutoff ? { recentCutoff } : {}),
    })
    if (
      streamKeyRef.current !== requestedStream ||
      paginationEpochRef.current !== requestedEpoch ||
      !capabilityLease.isCurrent('siteRead')
    ) return

    if (pageRequestRef.current === request) pageRequestRef.current = null
    if (!client.isIdentityCurrent()) {
      setPagination((current) => current.streamKey === streamKey
        ? { ...current, loadingMore: false }
        : current)
      return
    }
    if (!result.ok) {
      setPagination((current) => ({
        ...(current.streamKey === streamKey ? current : emptyPagination(streamKey)),
        loadingMore: false,
        pageError: result.error,
      }))
      return
    }

    setPagination((current) => {
      const owned = current.streamKey === streamKey
        ? current
        : emptyPagination(streamKey)
      return {
        ...owned,
        extraPages: mergeUnique(owned.extraPages, result.data.items),
        page: nextPage,
        loadingMore: false,
        pageError: null,
      }
    })
  }, [capabilityLease, client, first.loading, firstQuery, hasMore, loadingMore, page, recentCutoff, streamKey])

  return useMemo(() => ({
    items,
    total,
    recentCutoff,
    loading: readable && first.loading,
    loadingMore,
    error: readable ? first.error : null,
    pageError,
    hasMore,
    reload,
    loadMore,
  }), [first.error, first.loading, hasMore, items, loadMore, loadingMore, pageError, readable, recentCutoff, reload, total])
}

/** 让全部站点列表缓存失效（站点写操作之后调用）。 */
export function invalidateSites(): void {
  resourceStore.invalidate(SITES_CACHE_PREFIX)
}
