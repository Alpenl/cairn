import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReaderClient } from '../lib/api/client'
import { buildFeedItemsQuery } from '../lib/api/client'
import type { ApiError } from '../lib/api/result'
import type { FeedItem, ListFeedItemsParams, PaginatedFeedItemsResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'

const PAGE_SIZE = 30

export const FEED_ITEMS_CACHE_PREFIX = 'GET /api/feed-items'

const NO_ITEMS: FeedItem[] = []

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
export function useFeedItems(client: ReaderClient, filters: ListFeedItemsParams) {
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
  const key = FEED_ITEMS_CACHE_PREFIX + buildFeedItemsQuery({ ...stableFilters, page: 1 })

  const first = useCachedResource<PaginatedFeedItemsResponse>(key, (conditional) =>
    client.getFeedItems({ ...stableFilters, page: 1 }, conditional),
  )

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
      const result = await first.reload({ silent: quiet })
      if (result?.ok) {
        setExtra([])
        setPage(1)
      }
      return Boolean(result?.ok)
    },
    [first],
  )

  useEffect(() => {
    const timer = window.setInterval(() => void reload(true), 60_000)
    return () => window.clearInterval(timer)
  }, [reload])

  const loadMore = useCallback(async (): Promise<void> => {
    if (first.loading || loadingMore || items.length >= total) return
    const nextPage = page + 1
    setLoadingMore(true)
    const requestedKey = keyRef.current
    const result = await client.getFeedItems({ ...stableFilters, page: nextPage })
    if (!client.isIdentityCurrent()) return
    if (keyRef.current !== requestedKey) return // 筛选已切换，这一页不再属于当前列表
    setLoadingMore(false)
    if (!result.ok) return
    setExtra((current) => mergeUnique(current, result.data.items))
    setPage(nextPage)
  }, [client, first.loading, items.length, loadingMore, page, stableFilters, total])

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
    loading: first.loading,
    loadingMore,
    error: first.error as ApiError | null,
    hasMore: items.length < total,
    reload,
    loadMore,
    patchItem,
    removeItem,
  }
}

/** 让全部 RSS 条目列表缓存失效（条目状态批量变更之后调用）。 */
export function invalidateFeedItems(): void {
  resourceStore.invalidate(FEED_ITEMS_CACHE_PREFIX)
}
