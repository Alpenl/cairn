/**
 * 链接数据流 hook。按当前选择（Selection）映射后端查询参数，拉取并维护链接列表，
 * 支持分页加载更多。
 *
 * 视图→查询映射（对齐当前服务端查询与本地过滤分工）：
 * - smart/all       → 显式拉取已完成的 reading 链接
 * - smart/today     → 用浏览器本地相邻午夜的 UTC 瞬时范围分页拉取已完成的 reading 链接
 * - smart/annotated → 枚举 namespace-scoped durable index，再按 ID 点读
 * - tag             → tags=<id>（后端）
 * - domain          → domain=<id>（后端）
 *
 * 排序：created_at 降序（与设计原型一致）。
 *
 * PF3 起普通视图第 1 页走共享的 ResourceStore：切走再切回来直接用缓存渲染、不闪加载态，
 * 同键并发请求合并为一次往返。第 2 页起的「加载更多」仍是本地累积状态——它是
 * 一次性的追加动作，缓存它没有意义（键会随页码无限膨胀），而回到第 1 页时
 * 累积状态自然作废。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { buildLinksQuery, type ListLinksParams } from '../lib/api/client'
import type { ApiError, ApiResult } from '../lib/api/result'
import type { LinkResponse, PaginatedLinksResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'
import {
  ANNOTATED_LINKS_CACHE_KEY,
  linkDetailCacheKey,
  useAnnotatedLinks,
} from './useAnnotatedLinks'
import { reloadForActiveIdentity, type LibraryReloadOptions } from './libraryReload'
import { registerReaderClient } from './useReaderClient'

/** 智能视图 id 枚举。 */
export type SmartId = 'all' | 'today' | 'annotated'

export type Selection =
  | { type: 'smart'; id: SmartId; name: string }
  | { type: 'tag'; id: string; name: string }
  | { type: 'domain'; id: string; name: string }

/** 加载状态：首屏加载 / 加载更多 / 就绪 / 出错。 */
export interface LinksState {
  links: LinkResponse[]
  loading: boolean
  loadingMore: boolean
  error: ApiError | null
  hasMore: boolean
  /** Server total for the complete Reading corpus, when this response has one. */
  authoritativeTotal: number | null
  /** True only after pagination has ended and de-duplicated loaded rows equal that total. */
  corpusComplete: boolean
  /** 重新拉取首页（同步按钮 / 错误重试用）。 */
  reload: (
    options?: LibraryReloadOptions,
  ) => Promise<ApiResult<PaginatedLinksResponse> | null>
  /** 加载下一页（列表滚动到底用）。 */
  loadMore: () => void
  /** 乐观替换某条链接（保存正文 / 网站转换等写操作使用）。 */
  patchLink: (id: string, patch: Partial<LinkResponse>) => void
}

const PAGE_LIMIT = 30
const COMPLETED_READING_PARAMS: Pick<ListLinksParams, 'library_kind' | 'status'> = {
  library_kind: 'reading',
  status: 'done',
}

/** 链接列表在缓存里的键前缀。写操作按它整片失效。 */
export const LINKS_CACHE_PREFIX = 'GET /api/links'

/**
 * 首页静默刷新间隔。
 *
 * 为什么需要：用户在扩展里采集一条链接后，Reader 这边什么都不会发生——列表
 * 不轮询、后端也没有推送通道，必须手动点同步才看得见。收藏类产品最基本的
 * 那条闭环（采集 → 出现在库里）就断在这里。
 *
 * 30 秒是「解析通常几秒到几十秒完成」与「别把后端当秒表打」之间的折中。
 * 只刷第 1 页且只在页面可见时刷：翻到第 5 页的人不该被拽回顶部，
 * 后台标签页也不该继续发请求。
 */
const AUTO_REFRESH_MS = 30_000
const IDENTITY_MISMATCH_ERROR: ApiError = Object.freeze({
  kind: 'identity-mismatch',
  message: 'Reader client does not own the active cache identity',
})

function isAnnotatedSelection(sel: Selection): boolean {
  return sel.type === 'smart' && sel.id === 'annotated'
}

function isTodaySelection(sel: Selection): boolean {
  return sel.type === 'smart' && sel.id === 'today'
}

export type LocalDayCreatedRange = Readonly<{
  created_from: string
  created_before: string
}>

/** Convert one browser-local natural day to the UTC instants understood by the server. */
export function localDayCreatedRange(now = new Date()): LocalDayCreatedRange {
  const from = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const before = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
  return {
    created_from: from.toISOString(),
    created_before: before.toISOString(),
  }
}

function sameCreatedRange(
  left: LocalDayCreatedRange,
  right: LocalDayCreatedRange,
): boolean {
  return left.created_from === right.created_from &&
    left.created_before === right.created_before
}

/** Keep one Today stream frozen until the browser's next local midnight. */
function useTodayCreatedRange(active: boolean): LocalDayCreatedRange | undefined {
  const [, refreshClock] = useState(0)
  const range = active ? localDayCreatedRange() : undefined
  const createdFrom = range?.created_from
  const createdBefore = range?.created_before

  useEffect(() => {
    if (!active || !createdFrom || !createdBefore) return
    const refreshIfChanged = () => {
      if (!sameCreatedRange(
        { created_from: createdFrom, created_before: createdBefore },
        localDayCreatedRange(),
      )) {
        refreshClock((current) => current + 1)
      }
    }
    const delay = Math.max(0, Date.parse(createdBefore) - Date.now())
    const timer = window.setTimeout(refreshIfChanged, delay + 1)
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') refreshIfChanged()
    }
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [active, createdFrom, createdBefore])

  return range
}

/**
 * 把选择映射为后端查询参数。
 *
 * cursor 传 undefined 表示"流的开头"（发 `?after=`，后端据 after 参数**存在
 * 与否**切换到游标模式）；传字符串表示续读。游标模式下后端跳过
 * `COUNT(*) OVER()`，也不再用 OFFSET 扫描并丢弃前面的行——翻到第 5 页的代价
 * 与第 1 页相同。
 */
function selToParams(
  sel: Selection,
  cursor?: string,
  todayRange?: LocalDayCreatedRange,
): ListLinksParams {
  const base: ListLinksParams = {
    ...COMPLETED_READING_PARAMS,
    limit: PAGE_LIMIT,
    after: cursor ?? '',
  }
  if (sel.type === 'tag') return { ...base, tags: sel.id }
  if (sel.type === 'domain') return { ...base, domain: sel.id }
  if (sel.id === 'today') {
    if (!todayRange) throw new Error('Today selection requires a frozen local-day range')
    return { ...base, ...todayRange }
  }
  return base // all
}

/** created_at 降序。 */
function sortByCreatedDesc(items: readonly LinkResponse[]): LinkResponse[] {
  return [...items].sort((a, b) =>
    a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0)
}

function uniqueLinkCount(links: readonly LinkResponse[]): number {
  return new Set(links.map((link) => link.id)).size
}

function appendUniqueLinks(
  current: readonly LinkResponse[],
  incoming: readonly LinkResponse[],
): LinkResponse[] {
  const seen = new Set(current.map((link) => link.id))
  const result = [...current]
  for (const link of incoming) {
    if (seen.has(link.id)) continue
    seen.add(link.id)
    result.push(link)
  }
  return result
}

function isAllReadingSelection(selection: Selection): boolean {
  return selection.type === 'smart' && selection.id === 'all'
}

/**
 * 某个选择的「流的开头」缓存键。
 *
 * 键里带 selection 的身份，而不只依赖查询串。annotated 不发列表请求，但仍需要
 * 一个稳定的 selection key 来清空分页与乐观补丁状态。
 *
 * 前缀部分保持 `GET /api/links` 不变，好让写路径的前缀失效仍然覆盖到它们。
 */
function firstPageKey(sel: Selection, params: ListLinksParams): string {
  if (isAnnotatedSelection(sel)) return ANNOTATED_LINKS_CACHE_KEY
  return `${LINKS_CACHE_PREFIX}${buildLinksQuery(params)}#${sel.type}:${sel.id}`
}

function primeLinkPointCache(
  client: IdentityBoundReaderClient,
  links: readonly LinkResponse[],
  logicalKey: string,
): void {
  const lease = client.identityLease
  if (!resourceStore.isIdentityActive(lease)) return
  const ownership = lease.captureOwnership(logicalKey)
  if (!lease.isOwnershipCurrent(ownership)) return
  for (const link of links) {
    resourceStore.setForIdentity(ownership, linkDetailCacheKey(link.id), link)
  }
}

export function useLinks(
  client: IdentityBoundReaderClient,
  sel: Selection,
): LinksState {
  useEffect(() => registerReaderClient(client), [client])

  const lease = client.identityLease
  const sessionActive = resourceStore.isIdentityActive(lease)
  const annotatedSelected = isAnnotatedSelection(sel)
  const todayRange = useTodayCreatedRange(isTodaySelection(sel))
  const todayCreatedFrom = todayRange?.created_from
  const todayCreatedBefore = todayRange?.created_before
  // The memo freezes the exact bounds shared by the cache key, first request,
  // and every cursor page. Display-name changes do not create a new stream.
  const firstPageParams = useMemo(
    () => selToParams(
      sel,
      undefined,
      todayCreatedFrom && todayCreatedBefore
        ? { created_from: todayCreatedFrom, created_before: todayCreatedBefore }
        : undefined,
    ),
    [sel, todayCreatedFrom, todayCreatedBefore],
  )
  const selKey = firstPageKey(sel, firstPageParams)
  const activeStreamKey = useRef(selKey)
  activeStreamKey.current = selKey
  const paginationStreamKey = useRef(selKey)
  const paginationEpoch = useRef(0)
  if (paginationStreamKey.current !== selKey) {
    paginationStreamKey.current = selKey
    paginationEpoch.current += 1
  }

  const first = useCachedResource<PaginatedLinksResponse>(
    annotatedSelected || !sessionActive ? null : selKey,
    (conditional) => client.getLinks(firstPageParams, conditional),
    { enabled: !annotatedSelected && sessionActive },
  )
  const annotated = useAnnotatedLinks(client, annotatedSelected)
  const reloadAnnotated = annotated.reload
  const reloadFirst = first.reload

  // 第 2 页起的累积。选择一变即清空——那是另一条流。
  const [extraPages, setExtraPages] = useState<LinkResponse[]>([])
  // 续读游标。undefined = 还没翻过页；null = 已到流的末尾。
  const [cursor, setCursor] = useState<string | null | undefined>(undefined)
  const [loadingMore, setLoadingMore] = useState(false)
  const [paginationReady, setPaginationReady] = useState(false)
  const paginationReadyRef = useRef(false)
  const nextPageCursorRef = useRef<string | undefined>(undefined)
  const loadingMoreOwner = useRef<{ readonly epoch: number; readonly cursor: string } | null>(null)
  const pendingReloadEpoch = useRef<number | null>(null)
  useEffect(() => {
    setExtraPages([])
    setCursor(undefined)
    setLoadingMore(false)
    loadingMoreOwner.current = null
    pendingReloadEpoch.current = null
    paginationReadyRef.current = false
    nextPageCursorRef.current = undefined
    setPaginationReady(false)
  }, [selKey])

  useEffect(() => {
    if (annotatedSelected || pendingReloadEpoch.current !== null || !first.data || first.error) return
    paginationReadyRef.current = true
    nextPageCursorRef.current = first.data.next_cursor
    setPaginationReady(true)
  }, [annotatedSelected, first.data, first.error, selKey])

  // 本地乐观补丁：叠加在缓存数据之上，避免直接改写共享缓存里的数组。
  //
  // **服务端数据一到就作废**。补丁的用途是「请求还在路上时先让界面动起来」，
  // 一旦服务端给出了新的权威数据，它就该让位。
  const [patches, setPatches] = useState<Record<string, Partial<LinkResponse>>>({})
  useEffect(() => setPatches({}), [selKey])
  const dataStamp = annotatedSelected ? annotated.links : first.data
  useEffect(() => {
    // 普通列表响应或 annotated 点读投影换了引用 = 服务端给了一份新的权威数据。
    // store 只在内容真的变化时换普通列表引用，因此无变化的后台校验不会误清补丁。
    setPatches((current) => (Object.keys(current).length === 0 ? current : {}))
  }, [dataStamp])

  useEffect(() => {
    if (!first.data) return
    primeLinkPointCache(client, first.data.items, 'prime link point cache from list')
  }, [client, first.data])

  const links = useMemo(() => {
    if (!sessionActive) return []
    // 非静默错误时清空列表，与引入缓存之前的契约一致：ListPane 按
    // `!loading && !error` 渲染列表，调用方（含既有测试）也按「出错即空」断言。
    // 静默失败不会写 error（store 保证），因此后台刷新抖动不会走到这里。
    if (annotatedSelected ? annotated.error : first.error) return []
    const combined = annotatedSelected
      ? sortByCreatedDesc(annotated.links)
      : sortByCreatedDesc(appendUniqueLinks(first.data?.items ?? [], extraPages))
    if (Object.keys(patches).length === 0) return combined
    return combined.map((link) => (patches[link.id] ? { ...link, ...patches[link.id] } : link))
  }, [annotated.error, annotated.links, annotatedSelected, first.data, first.error, extraPages, patches, sessionActive])

  const hasMore = useMemo(() => {
    if (annotatedSelected || !paginationReady) return false
    if (cursor === null) return false // 已读到流末尾
    // 后端契约：满页必发 next_cursor，短于 limit 则省略。因此"还有没有更多"
    // 就看手上最新那份响应带没带游标。
    if (cursor !== undefined) return true
    return Boolean(first.data?.next_cursor)
  }, [annotatedSelected, first.data, cursor, paginationReady])

  // Current backend cursor responses deliberately use total=0/page=0 to
  // avoid COUNT(*). Older backends may ignore `after` and return offset-style
  // pages with a real total; retain that authoritative number but never infer
  // it from the loaded links. Tag/domain fallback may use a corpus only after
  // the explicit end marker and a unique-count equality proof both hold.
  const authoritativeTotal = useMemo(() => {
    if (!isAllReadingSelection(sel) || !first.data || first.data.page === 0) return null
    return first.data.total
  }, [first.data, sel])
  const corpusComplete = useMemo(() => (
    authoritativeTotal !== null &&
    !hasMore &&
    uniqueLinkCount(links) === authoritativeTotal
  ), [authoritativeTotal, hasMore, links])

  const reload = useCallback((options: LibraryReloadOptions = {}) => {
    const requestedStreamKey = selKey
    const requestedEpoch = ++paginationEpoch.current
    const reloadOwnership = lease.captureOwnership('reload links pagination epoch')
    pendingReloadEpoch.current = requestedEpoch
    paginationReadyRef.current = false
    nextPageCursorRef.current = undefined
    loadingMoreOwner.current = null
    setPaginationReady(false)
    setExtraPages([])
    setCursor(undefined)
    setLoadingMore(false)
    let operation: Promise<ApiResult<PaginatedLinksResponse> | null>
    if (annotatedSelected) {
      operation = reloadForActiveIdentity(
        client,
        'reload annotated links',
        async (reloadOptions) => {
          const result = await reloadAnnotated(reloadOptions)
          if (!result.ok) return { ok: false, error: result.error }
          return {
            ok: true,
            data: {
              items: [...result.data],
              total: result.data.length,
              page: 0,
              limit: PAGE_LIMIT,
            },
          }
        },
        options,
      )
    } else {
      operation = reloadForActiveIdentity(
        client,
        'reload reading links',
        reloadFirst,
        options,
      )
    }
    return operation.then((result) => {
      if (
        paginationEpoch.current === requestedEpoch &&
        activeStreamKey.current === requestedStreamKey &&
        lease.isOwnershipCurrent(reloadOwnership) &&
        resourceStore.isIdentityActive(lease)
      ) {
        pendingReloadEpoch.current = null
        if (result?.ok) {
          paginationReadyRef.current = true
          nextPageCursorRef.current = result.data.next_cursor
          setPaginationReady(true)
        } else {
          paginationReadyRef.current = false
          nextPageCursorRef.current = undefined
          setPaginationReady(false)
        }
      }
      return result
    })
  }, [annotatedSelected, client, lease, reloadAnnotated, reloadFirst, selKey])

  // 静默刷新：只在「停留在第 1 页 + 页面可见」时进行。
  //
  // silent 语义由 store 承接：失败时不写错误态、不动已有数据。没有它时一次
  // 网络抖动会把用户正读着的列表换成红色错误块——用户什么都没做。
  // 成功但数据未变时 store 不通知订阅者，因此这条定时器在稳态下**不产生任何
  // 重渲染**。
  useEffect(() => {
    if (annotatedSelected) return
    // 只在停留在"流的开头"时静默刷新：已经翻过页的人不该被拽回顶部。
    if (cursor !== undefined) return
    const tick = () => {
      if (document.visibilityState !== 'visible') return
      void reload({ silent: true })
    }
    const timer = window.setInterval(tick, AUTO_REFRESH_MS)
    // 从后台切回前台时立即补一次，而不是再等一个完整周期。
    document.addEventListener('visibilitychange', tick)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', tick)
    }
  }, [annotatedSelected, cursor, reload])

  const loadMore = useCallback(() => {
    if (
      annotatedSelected ||
      first.loading ||
      loadingMore ||
      loadingMoreOwner.current !== null ||
      !paginationReadyRef.current ||
      !hasMore ||
      !resourceStore.isIdentityActive(lease)
    ) return
    const ownership = lease.captureOwnership('load next links page')
    if (!lease.isOwnershipCurrent(ownership)) return
    // 续读游标：优先用上一次翻页拿到的，否则用首屏响应里的那个。
    const from = cursor ?? first.data?.next_cursor
    if (!from || nextPageCursorRef.current !== from) return
    const requestedStreamKey = selKey
    const requestedEpoch = paginationEpoch.current
    const owner = { epoch: requestedEpoch, cursor: from }
    loadingMoreOwner.current = owner
    setLoadingMore(true)
    void client.getLinks({ ...firstPageParams, after: from }).then((res) => {
      if (
        activeStreamKey.current !== requestedStreamKey ||
        paginationEpoch.current !== requestedEpoch ||
        loadingMoreOwner.current !== owner ||
        nextPageCursorRef.current !== from ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) return
      loadingMoreOwner.current = null
      setLoadingMore(false)
      if (!res.ok) return
      primeLinkPointCache(client, res.data.items, 'prime link point cache from next page')
      setExtraPages((prev) => appendUniqueLinks(prev, res.data.items))
      // 没有 next_cursor 就是读完了。后端满页必发它，因此这是可靠的终止信号。
      nextPageCursorRef.current = res.data.next_cursor
      setCursor(res.data.next_cursor ?? null)
    })
  }, [annotatedSelected, client, first.loading, first.data, firstPageParams, loadingMore, hasMore, cursor, lease, selKey])

  const patchLink = useCallback((id: string, patch: Partial<LinkResponse>) => {
    setPatches((prev) => ({ ...prev, [id]: { ...prev[id], ...patch } }))
  }, [])

  return useMemo(
    () => ({
      links,
      loading: sessionActive && (annotatedSelected ? annotated.loading : first.loading),
      loadingMore,
      error: sessionActive
        ? (annotatedSelected ? annotated.error : first.error)
        : IDENTITY_MISMATCH_ERROR,
      hasMore,
      authoritativeTotal,
      corpusComplete,
      reload,
      loadMore,
      patchLink,
    }),
    [
      annotated.error,
      annotated.loading,
      annotatedSelected,
      sessionActive,
      links,
      first.loading,
      first.error,
      loadingMore,
      hasMore,
      authoritativeTotal,
      corpusComplete,
      reload,
      loadMore,
      patchLink,
    ],
  )
}

/** 让全部链接列表缓存失效（写操作之后调用）。 */
export function invalidateLinks(): void {
  resourceStore.invalidate(LINKS_CACHE_PREFIX)
}
