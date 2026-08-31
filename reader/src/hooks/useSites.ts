import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { err, type ApiError } from '@webtag/api'
import type { ListSitesParams, PaginatedSitesResponse, SiteListItemResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import type { ReaderCapabilityLease } from '../lib/capabilities'
import { useIdentityCachedResource } from './useIdentityCachedResource'

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

function clientKey(client: IdentityBoundReaderClient): number {
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
  client: IdentityBoundReaderClient,
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
  const pageOneKey = readable
    ? `${baseKey}#capability=${capabilityLease.generation}`
    : null
  const {
    resource: first,
    cacheKey,
    canFetch,
  } = useIdentityCachedResource<PaginatedSitesResponse>(
    client,
    pageOneKey,
    async ({ client }) => {
      const operationLease = capabilityLease
      if (!operationLease.isCurrent('siteRead')) return err(CAPABILITY_REVOKED_ERROR)
      const result = await client.getSites(firstQuery)
      return client.isIdentityCurrent() && operationLease.isCurrent('siteRead')
        ? result
        : err(CAPABILITY_REVOKED_ERROR)
    },
    { enabled: readable },
  )
  const streamKey = useMemo(
    () => `${cacheKey ?? pageOneKey ?? baseKey}#client=${owner}`,
    [baseKey, cacheKey, owner, pageOneKey],
  )
  const firstOwnerRef = useRef(owner)

  useEffect(() => {
    if (firstOwnerRef.current === owner) return
    firstOwnerRef.current = owner
    if (!canFetch) return
    // A retained hook with the same identity lease still needs one forced fetch
    // when a different client object takes over, so the new fetcher owns page one.
    void first.reload()
  }, [canFetch, first, owner])

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
    () => canFetch ? mergeUnique(first.data?.items ?? NO_SITES, extraPages) : NO_SITES,
    [canFetch, first.data, extraPages],
  )
  const total = canFetch ? first.data?.total ?? 0 : 0
  const recentCutoff = canFetch ? first.data?.recent_cutoff : undefined
  const hasStableSnapshot = firstQuery.view !== 'recent' || Boolean(recentCutoff)
  const hasMore = canFetch && !first.loading && hasStableSnapshot && items.length < total

  const reload = useCallback(async (): Promise<void> => {
    if (!canFetch || !capabilityLease.isCurrent('siteRead')) return
    paginationEpochRef.current += 1
    pageRequestRef.current = null
    setPagination(emptyPagination(streamKey))
    await first.reload()
  }, [canFetch, capabilityLease, first, streamKey])

  const loadMore = useCallback(async (): Promise<void> => {
    if (
      !canFetch ||
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
  }, [canFetch, capabilityLease, client, first.loading, firstQuery, hasMore, loadingMore, page, recentCutoff, streamKey])

  return useMemo(() => ({
    items,
    total,
    recentCutoff,
    loading: canFetch && first.loading,
    loadingMore,
    error: canFetch ? first.error : null,
    pageError,
    hasMore,
    reload,
    loadMore,
  }), [canFetch, first.error, first.loading, hasMore, items, loadMore, loadingMore, pageError, recentCutoff, reload, total])
}

/** 让全部站点列表缓存失效（站点写操作之后调用）。 */
export function invalidateSites(): void {
  resourceStore.invalidate(SITES_CACHE_PREFIX)
}
