import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ApiError } from '@webtag/api'
import type { FeedItem, ListFeedItemsParams, PaginatedFeedItemsResponse } from '../lib/api/types'
import type { ReaderSubscriptionsFeedPort } from '../lib/reader-api-ports'
import { feedItemsCacheKey } from '../lib/cache/keys'
import { useFinalIdentityCachedResource } from './useIdentityCachedResource'
import { useIdentityBoundOperationGate } from './identityBoundOperation'
import { useIdentityPolling } from './useIdentityPolling'

const PAGE_SIZE = 30

export { FEED_ITEMS_CACHE_PREFIX } from '../lib/cache/keys'

const NO_ITEMS: FeedItem[] = []

type FeedItemsClient = Pick<
  ReaderSubscriptionsFeedPort,
  'identityLease' | 'isIdentityCurrent' | 'captureIdentity' | 'getFeedItems'
>

function mergeUnique(current: FeedItem[], incoming: FeedItem[]): FeedItem[] {
  const byID = new Map(current.map((item) => [item.id, item]))
  incoming.forEach((item) => byID.set(item.id, item))
  return [...byID.values()]
}

/**
 * Paginated RSS items for one stable filter.
 *
 * PF3 起第 1 页走共享缓存（切走再切回来不重拉、60 秒轮询在数据未变时零重渲染）；
 * 第 2 页起仍是本地累积——一次性追加动作，缓存它只会让键随页码膨胀。
 */
export function useFeedItems(client: FeedItemsClient, filters: ListFeedItemsParams) {
  const stableFilters = useMemo<ListFeedItemsParams>(
    () => ({
      view: filters.view,
      subscription_id: filters.subscription_id,
      folder_id: filters.folder_id,
      q: filters.q,
      limit: PAGE_SIZE,
    }),
    [filters.view, filters.subscription_id, filters.folder_id, filters.q],
  )
  const key = feedItemsCacheKey({ ...stableFilters, page: 1 })

  const {
    canFetch,
    resource: first,
  } = useFinalIdentityCachedResource<PaginatedFeedItemsResponse, FeedItemsClient>(
    client,
    key,
    ({ client, signal }) => client.getFeedItems({ ...stableFilters, page: 1 }, { signal }),
  )
  const operationGate = useIdentityBoundOperationGate<'reload' | 'load-more'>(client, [key])

  const [extra, setExtra] = useState<FeedItem[]>([])
  const [page, setPage] = useState(1)
  const [loadingMore, setLoadingMore] = useState(false)
  // 本地叠加层：状态改写（已读/星标）与删除都只动这里，不改共享缓存里的数组。
  const [patches, setPatches] = useState<Record<string, Partial<FeedItem>>>({})
  const [removed, setRemoved] = useState<Set<string>>(() => new Set())
  // 分页归属守卫：翻页请求发出时记下当时的筛选键，响应回来若筛选已变就丢弃。
  // 没有它的话，旧筛选迟到的第 2 页会被追加进新筛选的列表里。
  const keyRef = useRef(key)
  keyRef.current = key

  useEffect(() => {
    setExtra([])
    setPage(1)
    setLoadingMore(false)
    setPatches({})
    setRemoved(new Set())
  }, [key])

  const items = useMemo(() => {
    const base = first.data?.items ?? NO_ITEMS
    const merged = extra.length > 0 ? mergeUnique(base, extra) : base
    const visible = removed.size > 0 ? merged.filter((item) => !removed.has(item.id)) : merged
    if (Object.keys(patches).length === 0) return visible
    return visible.map((item) => (patches[item.id] ? { ...item, ...patches[item.id] } : item))
  }, [first.data, extra, patches, removed])

  const total = Math.max(0, (first.data?.total ?? 0) - removed.size)

  const reload = useCallback(
    async (quiet = false): Promise<boolean> => {
      const operation = operationGate.begin('reload', 'reload feed items')
      if (!operation) return false
      const result = await first.reload({ silent: quiet })
      if (result?.ok) {
        operationGate.commit(operation, () => {
          setExtra([])
          setPage(1)
        })
      }
      return Boolean(result?.ok && operationGate.isCurrent(operation))
    },
    [first, operationGate],
  )

  useIdentityPolling(client, {
    enabled: canFetch,
    intervalMs: 60_000,
    logicalKey: 'poll feed items',
    ownerKey: key,
    onTick: () => { void reload(true) },
  })

  const loadMore = useCallback(async (): Promise<void> => {
    if (!canFetch || first.loading || loadingMore || items.length >= total) return
    const operation = operationGate.begin('load-more', 'load more feed items')
    if (!operation) return
    const nextPage = page + 1
    setLoadingMore(true)
    const requestedKey = keyRef.current
    try {
      const result = await client.getFeedItems({ ...stableFilters, page: nextPage })
      operationGate.commit(operation, () => {
        if (keyRef.current !== requestedKey) return // 筛选已切换，这一页不再属于当前列表
        if (!result.ok) return
        setExtra((current) => mergeUnique(current, result.data.items))
        setPage(nextPage)
      })
    } finally {
      operationGate.finish(operation, () => setLoadingMore(false))
    }
  }, [canFetch, client, first.loading, items.length, loadingMore, operationGate, page, stableFilters, total])

  const patchItem = useCallback((id: string, patch: Partial<FeedItem>) => {
    setPatches((current) => ({ ...current, [id]: { ...current[id], ...patch } }))
  }, [])

  const removeItem = useCallback((id: string) => {
    setRemoved((current) => {
      const next = new Set(current)
      next.add(id)
      return next
    })
  }, [])

  return {
    items,
    total,
    loading: canFetch && first.loading,
    loadingMore,
    error: first.error as ApiError | null,
    hasMore: items.length < total,
    reload,
    loadMore,
    patchItem,
    removeItem,
  }
}
