import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from 'react'
import type { ReaderCapabilityFeature, ReaderCapabilityLease } from '../../lib/capabilities'
import { isReaderFeedItemResponse } from '../../lib/api/guards'
import type { ReaderFeedItemResponse, ReaderTodoResponse } from '../../lib/api/types'
import type {
  ReaderIdentityPort,
  ReaderInboxTodosPort,
  ReaderLibrarySitesPort,
  ReaderSubscriptionsFeedPort,
} from '../../lib/reader-api-ports'
import type { ReaderRoute } from '../../lib/navigation/route'
import { feedScrollAnchorKey } from '../../lib/feed-scroll-anchor'
import { useFeedScrollAnchor } from '../../hooks/useFeedScrollAnchor'
import { useSurfaceRequestGate, type SurfaceRequestToken } from '../../hooks/useSurfaceRequestGate'
import { isRecord } from '../../lib/records'
import { READER_EVENTS, emitReaderEvent, subscribeReaderEvents } from '../../lib/reader-events'
import {
  SURFACE_IDENTITY_ERROR,
  formatRelativeDate,
  identityIsCurrent,
  isIdentityError,
  readerErrorMessage,
} from '../../lib/reader-surface'
import { Icon } from '../Icon'
import { ReaderListRow } from '../ui/ReaderListRow'
import { ListEmptyState, ListStateView } from './ListStateView'
import { SurfaceShell } from './SurfaceShell'

export interface FeedSurfaceProps {
  readonly client: FeedSurfaceClient
  readonly onNavigate: (route: ReaderRoute) => void
  readonly onOpenLink: (id: string) => void
  readonly capabilityLease: ReaderCapabilityLease
  /**
   * `surface` 是独立的 `?surface=feed` 路由；`embedded` 是「今天」里内嵌的那份。
   * 内嵌时不渲染自己的 SurfaceShell，滚动容器由宿主提供——RF57 的滚动位置恢复
   * 认的是那个容器，换掉就会静默失效。
   */
  readonly variant?: 'surface' | 'embedded'
  readonly hostScrollRef?: RefObject<HTMLDivElement>
}

type FeedMode = 'recommended' | 'chronological'
type FeedFeedbackAction = 'save' | 'unsave' | 'hide'
type FeedSource = 'inbox' | 'reading' | 'subscription'
type FeedSurfaceClient = ReaderIdentityPort &
  Pick<ReaderSubscriptionsFeedPort, 'getReaderFeed' | 'sendReaderFeedFeedback' | 'updateFeedItem'> &
  Pick<ReaderInboxTodosPort, 'listTodos' | 'confirmInbox' | 'discardInbox' | 'confirmAIProposals'> &
  Pick<ReaderLibrarySitesPort, 'patchEngagement'>
/** 两条互相独立的请求线：Feed 与右栏 TODO，各自按代次判断迟到回包。 */
type FeedRequestChannel = 'feed' | 'todos'
type FeedRequestToken = SurfaceRequestToken<FeedRequestChannel>

const FEED_SOURCES: readonly { readonly id: FeedSource; readonly label: string }[] = [
  { id: 'inbox', label: '收件箱' },
  { id: 'reading', label: '收藏' },
  { id: 'subscription', label: '订阅' },
]

const BATCH_CONFIRM_KEY = '__feed_batch_confirm__'

interface FeedResumeState {
  readonly nextCursor?: string
  readonly items: ReaderFeedItemResponse[]
}

interface FeedLoadOptions {
  readonly append: boolean
  readonly cursor?: string
}

const FEED_PAGE_SIZE = 30
const FEED_RESUME_STORAGE_PREFIX = 'webtag:reader:mixed-feed:v1'

// eslint-disable-next-line react-refresh/only-export-components
export function clearFeedSessionState(client: ReaderIdentityPort): void {
  const namespace = identityNamespace(client)
  if (!namespace || typeof window === 'undefined') return
  const prefix = `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:`
  try {
    for (let index = window.sessionStorage.length - 1; index >= 0; index -= 1) {
      const key = window.sessionStorage.key(index)
      if (key?.startsWith(prefix)) window.sessionStorage.removeItem(key)
    }
  } catch {
    // Feed position state is best-effort and contains no durable user data.
  }
}

function isFeedMode(value: unknown): value is FeedMode {
  return value === 'recommended' || value === 'chronological'
}

function isFeedSource(value: unknown): value is FeedSource {
  return value === 'inbox' || value === 'reading' || value === 'subscription'
}

function normalizeSourceFilter(values: Iterable<FeedSource>): FeedSource[] {
  const selected = new Set(values)
  return FEED_SOURCES
    .map((source) => source.id)
    .filter((source) => selected.has(source))
}

function sourceFilterStorageKey(client: ReaderIdentityPort): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:sources`
    : null
}

function readStoredSources(client: ReaderIdentityPort): Set<FeedSource> {
  const key = sourceFilterStorageKey(client)
  if (!key || typeof window === 'undefined') return new Set(FEED_SOURCES.map((source) => source.id))
  try {
    const raw = window.sessionStorage.getItem(key)
    if (!raw) return new Set(FEED_SOURCES.map((source) => source.id))
    const value: unknown = JSON.parse(raw)
    if (!Array.isArray(value) || !value.every(isFeedSource)) return new Set(FEED_SOURCES.map((source) => source.id))
    return new Set(normalizeSourceFilter(value))
  } catch {
    return new Set(FEED_SOURCES.map((source) => source.id))
  }
}

function writeStoredSources(client: ReaderIdentityPort, sources: Iterable<FeedSource>): void {
  const key = sourceFilterStorageKey(client)
  if (!key || typeof window === 'undefined') return
  try {
    window.sessionStorage.setItem(key, JSON.stringify(normalizeSourceFilter(sources)))
  } catch {
    // Session storage is a best-effort filter preference.
  }
}

function identityNamespace(client: ReaderIdentityPort): string | null {
  try {
    const namespace = client.identityLease.context.physicalNamespace
    return typeof namespace === 'string' && namespace.length > 0 ? namespace : null
  } catch {
    return null
  }
}

function resumeStorageKey(client: ReaderIdentityPort, mode: FeedMode, sourceFilter: readonly FeedSource[]): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:${mode}:${sourceFilter.length > 0 ? sourceFilter.join(',') : 'none'}`
    : null
}

function modeStorageKey(client: ReaderIdentityPort): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:mode`
    : null
}

function readStoredMode(client: ReaderIdentityPort): FeedMode {
  const key = modeStorageKey(client)
  if (!key || typeof window === 'undefined') return 'recommended'
  try {
    const value = window.sessionStorage.getItem(key)
    return isFeedMode(value) ? value : 'recommended'
  } catch {
    return 'recommended'
  }
}

function writeStoredMode(client: ReaderIdentityPort, mode: FeedMode): void {
  const key = modeStorageKey(client)
  if (!key || typeof window === 'undefined') return
  try {
    window.sessionStorage.setItem(key, mode)
  } catch {
    // Session storage is a best-effort position cache.
  }
}

function readResume(client: ReaderIdentityPort, mode: FeedMode, sourceFilter: readonly FeedSource[]): FeedResumeState | null {
  const key = resumeStorageKey(client, mode, sourceFilter)
  if (!key || typeof window === 'undefined') return null
  try {
    const raw = window.sessionStorage.getItem(key)
    if (!raw) return null
    const value: unknown = JSON.parse(raw)
    if (!isRecord(value) || value.version !== 3) return null
    if (!Array.isArray(value.items) || !value.items.every(isReaderFeedItemResponse)) return null
    if (value.next_cursor !== undefined && typeof value.next_cursor !== 'string') return null
    if (!Array.isArray(value.source_filter) || !value.source_filter.every(isFeedSource)) return null
    if (normalizeSourceFilter(value.source_filter).join(',') !== sourceFilter.join(',')) return null
    return {
      nextCursor: value.next_cursor || undefined,
      items: dedupeItems(value.items),
    }
  } catch {
    return null
  }
}

function writeResume(
  client: ReaderIdentityPort,
  mode: FeedMode,
  sourceFilter: readonly FeedSource[],
  nextCursor: string | undefined,
  items: readonly ReaderFeedItemResponse[],
): void {
  const key = resumeStorageKey(client, mode, sourceFilter)
  if (!key || typeof window === 'undefined') return
  try {
    window.sessionStorage.setItem(key, JSON.stringify({
      version: 3,
      next_cursor: nextCursor,
      source_filter: sourceFilter,
      items: dedupeItems(items),
    }))
  } catch {
    // Session storage is a best-effort position cache.
  }
}

function feedResourceIdentity(item: ReaderFeedItemResponse): string {
  return item.resource_key
}

/** Keep the first server occurrence of each resource without changing its position. */
function dedupeItems(items: readonly ReaderFeedItemResponse[]): ReaderFeedItemResponse[] {
  const result: ReaderFeedItemResponse[] = []
  const seen = new Set<string>()
  for (const item of items) {
    const resourceKey = feedResourceIdentity(item)
    if (seen.has(resourceKey)) continue
    seen.add(resourceKey)
    result.push(item)
  }
  return result
}

/** Append unseen resources without changing the order already on screen. */
function mergeItems(
  current: readonly ReaderFeedItemResponse[],
  incoming: readonly ReaderFeedItemResponse[],
): ReaderFeedItemResponse[] {
  const result = dedupeItems(current)
  const indexes = new Map(result.map((item, index) => [feedResourceIdentity(item), index]))
  for (const item of incoming) {
    const resourceKey = feedResourceIdentity(item)
    const index = indexes.get(resourceKey)
    if (index === undefined) {
      indexes.set(resourceKey, result.length)
      result.push(item)
      continue
    }
  }
  return result
}

function withSavedFeedAction(item: ReaderFeedItemResponse, saved: boolean): ReaderFeedItemResponse {
  return { ...item, saved }
}

function supportsOpen(item: ReaderFeedItemResponse): boolean {
  return Boolean(item.link_id || item.inbox_id || item.url)
}

function feedSource(item: ReaderFeedItemResponse): FeedSource | null {
  if (item.source === 'inbox' || item.source === 'reading' || item.source === 'subscription') return item.source
  return null
}

function feedSourceLabel(item: ReaderFeedItemResponse): string {
  const source = feedSource(item)
  return FEED_SOURCES.find((candidate) => candidate.id === source)?.label ?? item.source
}

function openActionLabel(item: ReaderFeedItemResponse): string {
  return item.feed_item_id && !item.link_id ? '打开原文' : '打开'
}

function feedReasonText(item: ReaderFeedItemResponse): string {
  switch (feedSource(item)) {
    case 'inbox': return '收件箱采集'
    case 'reading': return '已保存到资料库'
    case 'subscription': return '订阅更新'
    default: return '混合 Feed'
  }
}

export function FeedSurface({
  client,
  onNavigate,
  onOpenLink,
  capabilityLease,
  variant = 'surface',
  hostScrollRef,
}: FeedSurfaceProps) {
  const [mode, setMode] = useState<FeedMode>(() => readStoredMode(client))
  const [enabledSources, setEnabledSources] = useState<Set<FeedSource>>(() => readStoredSources(client))
  const [items, setItems] = useState<ReaderFeedItemResponse[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyKeys, setBusyKeys] = useState<Set<string>>(() => new Set())
  const [todos, setTodos] = useState<ReaderTodoResponse[]>([])
  const [todosLoading, setTodosLoading] = useState(true)
  const [todoError, setTodoError] = useState<string | null>(null)
  const [showRecommendationHelp, setShowRecommendationHelp] = useState(false)
  const [expandedReasonKey, setExpandedReasonKey] = useState<string | null>(null)
  // Key whose mode/filter is on screen. A response for an abandoned view must
  // never move the current scroll anchor.
  const [renderedKey, setRenderedKey] = useState<string | null>(null)
  const ownScrollRef = useRef<HTMLDivElement>(null)
  const scrollRef = hostScrollRef ?? ownScrollRef
  const itemsRef = useRef<ReaderFeedItemResponse[]>([])
  const cursorRef = useRef<string | undefined>()
  const sourceFilter = useMemo(
    () => normalizeSourceFilter(enabledSources).filter((source) => source !== 'inbox' || capabilityLease.policy.inbox),
    [capabilityLease, enabledSources],
  )
  const scrollAnchorKey = useMemo(
    () => feedScrollAnchorKey(identityNamespace(client), mode, sourceFilter),
    [client, mode, sourceFilter],
  )
  const identityCurrent = useCallback(() => identityIsCurrent(client), [client])
  const capabilityCurrent = useCallback(
    (feature: ReaderCapabilityFeature) => capabilityLease.isCurrent(feature),
    [capabilityLease],
  )
  // 闸门只持有身份权威：Feed、TODO 和收件箱动作各要一张不同的能力票，把它们塞进
  // 同一个 authority 会让任意一张票失效时静默拖垮另一条通道，所以能力检查留在调用点。
  const gate = useSurfaceRequestGate<FeedRequestChannel>({ owner: [client], authority: identityCurrent })
  const policy = capabilityLease.policy
  const scrollAnchor = useFeedScrollAnchor({
    containerRef: scrollRef,
    stateKey: scrollAnchorKey,
    renderedKey,
    identityIsCurrent: identityCurrent,
  })

  const clearForIdentityLoss = useCallback(() => {
    itemsRef.current = []
    cursorRef.current = undefined
    setItems([])
    setNextCursor(undefined)
    setBusyKeys(new Set())
    setTodos([])
    setTodosLoading(false)
    setTodoError(null)
    setError(SURFACE_IDENTITY_ERROR)
    setLoading(false)
    setLoadingMore(false)
  }, [])

  const requireIdentity = useCallback((): boolean => {
    if (identityIsCurrent(client)) return true
    clearForIdentityLoss()
    return false
  }, [clearForIdentityLoss, client])

  const markBusy = useCallback((key: string, busy: boolean) => {
    setBusyKeys((current) => {
      const next = new Set(current)
      if (busy) next.add(key)
      else next.delete(key)
      return next
    })
  }, [])

  const commitState = useCallback((
    nextItems: readonly ReaderFeedItemResponse[],
    nextCursor: string | undefined,
  ) => {
    if (!capabilityLease.isCurrent('feed')) return
    const normalizedItems = dedupeItems(nextItems)
    itemsRef.current = normalizedItems
    cursorRef.current = nextCursor
    setItems(normalizedItems)
    setNextCursor(nextCursor)
    writeResume(client, mode, sourceFilter, nextCursor, normalizedItems)
  }, [capabilityLease, client, mode, sourceFilter])

  const loadTodos = useCallback(async () => {
    const token = gate.begin('todos')
    if (!capabilityCurrent('todos')) {
      setTodos([])
      setTodosLoading(false)
      setTodoError(null)
      return
    }
    setTodosLoading(true)
    setTodoError(null)
    if (!identityIsCurrent(client)) {
      clearForIdentityLoss()
      return
    }
    try {
      const result = await client.listTodos()
      if (!gate.isSameOwner(token) || !capabilityCurrent('todos')) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setTodoError(readerErrorMessage(result.error))
        setTodos([])
      } else {
        setTodos(result.data.items)
      }
    } catch (cause) {
      if (gate.isSameOwner(token)) {
        if (!identityIsCurrent(client)) clearForIdentityLoss()
        else setTodoError(readerErrorMessage(cause))
        setTodos([])
      }
    } finally {
      if (gate.isSameOwner(token)) setTodosLoading(false)
    }
  }, [capabilityCurrent, clearForIdentityLoss, client, gate])

  useEffect(() => {
    void loadTodos()
    return () => {
      gate.invalidate('todos')
    }
  }, [gate, loadTodos])

  // 返回本次请求的 token，让调用方可以在自己的 `then` 里问「我发出的那一次还算数吗」。
  // 闸门没有「下一个代次」这种东西，预测 id 只会在别的请求插队时判断错人。
  const load = useCallback(async ({ append, cursor }: FeedLoadOptions): Promise<FeedRequestToken | null> => {
    if (!capabilityCurrent('feed')) return null
    const token = gate.begin('feed')
    if (append) setLoadingMore(true)
    else setLoading(true)
    setError(null)

    if (!identityIsCurrent(client)) {
      clearForIdentityLoss()
      return token
    }
    try {
      const requestParams = {
        mode,
        source: sourceFilter,
        ...(cursor ? { after: cursor } : {}),
        limit: FEED_PAGE_SIZE,
      }
      const result = await client.getReaderFeed(requestParams)

      if (!gate.isSameOwner(token) || !capabilityCurrent('feed')) return token
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return token
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(readerErrorMessage(result.error))
        return token
      }

      const responseCursor = result.data.next_cursor || undefined
      if (append) {
        commitState(mergeItems(itemsRef.current, result.data.items), responseCursor)
      } else {
        commitState(result.data.items, responseCursor)
        setRenderedKey(scrollAnchorKey)
      }
    } catch (cause) {
      if (gate.isSameOwner(token)) {
        if (!identityIsCurrent(client)) clearForIdentityLoss()
        else setError(readerErrorMessage(cause))
      }
    } finally {
      if (gate.isSameOwner(token)) {
        setLoading(false)
        setLoadingMore(false)
      }
    }
    return token
  }, [capabilityCurrent, clearForIdentityLoss, client, commitState, gate, mode, scrollAnchorKey, sourceFilter])

  useEffect(() => {
    if (!identityIsCurrent(client)) {
      clearForIdentityLoss()
      return () => {
        gate.invalidate('feed')
      }
    }
    const resume = readResume(client, mode, sourceFilter)
    itemsRef.current = []
    cursorRef.current = undefined
    setItems([])
    setNextCursor(undefined)
    setRenderedKey(null)
    setLoading(sourceFilter.length > 0)
    setLoadingMore(false)
    setError(null)
    if (sourceFilter.length === 0) {
      return () => {
        gate.invalidate('feed')
      }
    }
    if (resume) {
      commitState(resume.items, resume.nextCursor)
      setRenderedKey(scrollAnchorKey)
      setLoading(false)
    } else {
      void load({ append: false })
    }
    return () => {
      gate.invalidate('feed')
    }
  }, [clearForIdentityLoss, client, commitState, gate, load, mode, scrollAnchorKey, sourceFilter])

  const selectMode = useCallback((nextMode: FeedMode) => {
    if (nextMode === mode) return
    writeStoredMode(client, nextMode)
    setMode(nextMode)
  }, [client, mode])

  const toggleSource = useCallback((source: FeedSource) => {
    setEnabledSources((current) => {
      const next = new Set(current)
      if (next.has(source)) next.delete(source)
      else next.add(source)
      writeStoredSources(client, next)
      return next
    })
  }, [client])

  const visibleItems = useMemo(
    () => items.filter((item) => {
      const source = feedSource(item)
      if (source === 'inbox' && !policy.inbox) return false
      return source === null || enabledSources.has(source)
    }),
    [enabledSources, items, policy.inbox],
  )

  const topTodos = useMemo(
    () => todos.filter((todo) => !todo.done).slice(0, 3),
    [todos],
  )

  const retry = useCallback(() => {
    void load({ append: false })
  }, [load])

  const refresh = useCallback(() => {
    // An explicit refresh is the one gesture that discards the reader's place;
    // every other path through this surface keeps its key's anchor.
    scrollAnchor.forget()
    retry()
  }, [retry, scrollAnchor])

  useEffect(
    () => subscribeReaderEvents([READER_EVENTS.todosChanged], () => { void loadTodos() }),
    [loadTodos],
  )

  const loadMore = useCallback(() => {
    const cursor = cursorRef.current
    if (!cursor || loadingMore) return
    void load({ append: true, cursor })
  }, [load, loadingMore])

  const patchEngagement = useCallback(async (
    item: ReaderFeedItemResponse,
    patch: { read?: boolean; read_later?: boolean },
  ) => {
    if (!capabilityCurrent('engagement') || !item.link_id && !item.feed_item_id) return
    if (feedSource(item) === 'inbox' || patch.read === undefined && patch.read_later === undefined) return
    if (!requireIdentity()) return
    markBusy(item.key, true)
    try {
      if (item.feed_item_id && feedSource(item) === 'subscription') {
        const result = await client.updateFeedItem(item.feed_item_id, patch)
        if (!requireIdentity() || !capabilityCurrent('engagement')) return
        if (!result.ok) {
          if (isIdentityError(result.error)) clearForIdentityLoss()
          else setError(readerErrorMessage(result.error))
          return
        }
        const read = result.data
          ? typeof result.data.read === 'boolean' ? result.data.read : Boolean(result.data.read_at)
          : patch.read ?? item.read
        const readLater = result.data
          ? typeof result.data.read_later === 'boolean' ? result.data.read_later : Boolean(result.data.read_later_at)
          : patch.read_later ?? item.read_later
        const nextItems = itemsRef.current.map((candidate) => candidate.key === item.key
          ? { ...candidate, read, read_later: readLater }
          : candidate)
        commitState(nextItems, cursorRef.current)
        emitReaderEvent(READER_EVENTS.homeChanged)
        return
      }

      if (item.link_id) {
        const result = await client.patchEngagement(item.link_id, patch)
        if (!requireIdentity() || !capabilityCurrent('engagement')) return
        if (!result.ok) {
          if (isIdentityError(result.error)) clearForIdentityLoss()
          else setError(readerErrorMessage(result.error))
          return
        }
        const nextItems = itemsRef.current.map((candidate) => candidate.key === item.key
          ? { ...candidate, read: result.data.read, read_later: result.data.read_later }
          : candidate)
        commitState(nextItems, cursorRef.current)
        emitReaderEvent(READER_EVENTS.homeChanged)
        return
      }

      const result = await client.updateFeedItem(item.feed_item_id as string, patch)
      if (!requireIdentity() || !capabilityCurrent('engagement')) return
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(readerErrorMessage(result.error))
        return
      }
      const read = result.data
        ? typeof result.data.read === 'boolean' ? result.data.read : Boolean(result.data.read_at)
        : patch.read ?? item.read
      const readLater = result.data
        ? typeof result.data.read_later === 'boolean' ? result.data.read_later : Boolean(result.data.read_later_at)
        : patch.read_later ?? item.read_later
      const nextItems = itemsRef.current.map((candidate) => candidate.key === item.key
        ? { ...candidate, read, read_later: readLater }
        : candidate)
      commitState(nextItems, cursorRef.current)
      emitReaderEvent(READER_EVENTS.homeChanged)
    } catch (cause) {
      if (requireIdentity()) setError(readerErrorMessage(cause))
    } finally {
      if (identityIsCurrent(client)) markBusy(item.key, false)
      else clearForIdentityLoss()
    }
  }, [capabilityCurrent, clearForIdentityLoss, client, commitState, markBusy, requireIdentity])

  const resolveInbox = useCallback(async (
    item: ReaderFeedItemResponse,
    action: 'confirm' | 'discard',
  ) => {
    if (!capabilityCurrent('inbox') || !item.inbox_id) return
    if (!requireIdentity()) return
    markBusy(item.key, true)
    try {
      if (action === 'confirm') {
        const result = await client.confirmInbox(item.inbox_id)
        if (!requireIdentity() || !capabilityCurrent('inbox')) return
        if (!result.ok) {
          if (isIdentityError(result.error)) clearForIdentityLoss()
          else setError(readerErrorMessage(result.error))
          return
        }
        commitState(
          itemsRef.current.filter((candidate) => candidate.key !== item.key),
          cursorRef.current,
        )
        emitReaderEvent(READER_EVENTS.homeChanged)
        onOpenLink(result.data.link_id)
        return
      }

      const result = await client.discardInbox(item.inbox_id)
      if (!requireIdentity() || !capabilityCurrent('inbox')) return
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(readerErrorMessage(result.error))
        return
      }
      commitState(
        itemsRef.current.filter((candidate) => candidate.key !== item.key),
        cursorRef.current,
      )
      emitReaderEvent(READER_EVENTS.homeChanged)
    } catch (cause) {
      if (requireIdentity()) setError(readerErrorMessage(cause))
    } finally {
      if (identityIsCurrent(client)) markBusy(item.key, false)
      else clearForIdentityLoss()
    }
  }, [capabilityCurrent, clearForIdentityLoss, client, commitState, markBusy, onOpenLink, requireIdentity])

  const confirmPendingBatch = useCallback(async () => {
    if (!capabilityCurrent('inbox') || !sourceFilter.includes('inbox') || busyKeys.has(BATCH_CONFIRM_KEY)) return
    if (!requireIdentity()) return
    // 搭在当前 Feed 代次上：批处理不发 Feed 请求，只需要知道结束时读者是否还停在
    // 同一份视图上——中途换了模式或来源筛选，这次刷新就不该再落地。
    const batchToken = gate.capture('feed')
    let actionError: string | null = null
    markBusy(BATCH_CONFIRM_KEY, true)
    try {
      for (;;) {
        const result = await client.confirmAIProposals({ partition: 'active' })
        if (!requireIdentity() || !capabilityCurrent('inbox')) return
        if (!result.ok) {
          if (isIdentityError(result.error)) clearForIdentityLoss()
          else actionError = readerErrorMessage(result.error)
          return
        }
        if (result.data.remaining_count === 0) return
        if (result.data.items.length === 0) {
          actionError = 'AI 确认队列未前进，请刷新后重试。'
          return
        }
      }
    } catch (cause) {
      if (requireIdentity()) actionError = readerErrorMessage(cause)
    } finally {
      if (identityIsCurrent(client)) {
        markBusy(BATCH_CONFIRM_KEY, false)
        if (gate.isCurrent(batchToken)) {
          scrollAnchor.forget()
          // 错误文案要盖在这次刷新的结果上，而不是任何后来的请求上，所以问的是
          // `load` 实际发出的那个 token 还算不算数。
          void load({ append: false }).then((refreshToken) => {
            if (actionError && refreshToken && gate.isCurrent(refreshToken)) {
              setError(actionError)
            }
          })
        }
      } else {
        clearForIdentityLoss()
      }
    }
  }, [busyKeys, capabilityCurrent, clearForIdentityLoss, client, gate, load, markBusy, requireIdentity, scrollAnchor, sourceFilter])

  const sendFeedback = useCallback(async (
    item: ReaderFeedItemResponse,
    action: FeedFeedbackAction,
  ) => {
    if (!capabilityCurrent('feed')) return
    if ((action === 'save' || action === 'unsave') && feedSource(item) !== 'subscription') return
    if (!requireIdentity()) return
    markBusy(item.key, true)
    const previousItems = itemsRef.current
    const optimisticItems = action === 'hide'
      ? previousItems.filter((candidate) => candidate.key !== item.key)
      : previousItems.map((candidate) => candidate.key === item.key
        ? withSavedFeedAction(candidate, action === 'save')
        : candidate)
    commitState(optimisticItems, cursorRef.current)
    emitReaderEvent(READER_EVENTS.homeChanged)
    try {
      const result = await client.sendReaderFeedFeedback(item.key, { action })
      if (!requireIdentity() || !capabilityCurrent('feed')) return
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else {
          commitState(previousItems, cursorRef.current)
          emitReaderEvent(READER_EVENTS.homeChanged)
          setError(readerErrorMessage(result.error))
        }
        return
      }
      const nextItems = action === 'hide'
        ? optimisticItems
        : optimisticItems.map((candidate) => candidate.key === item.key
          ? {
              ...withSavedFeedAction(candidate, result.data.action === 'save'),
              link_id: result.data.action === 'save'
                ? result.data.link_id ?? candidate.link_id
                : result.data.link_id ?? null,
            }
          : candidate)
      commitState(nextItems, cursorRef.current)
      emitReaderEvent(READER_EVENTS.homeChanged)
    } catch (cause) {
      if (requireIdentity()) {
        commitState(previousItems, cursorRef.current)
        emitReaderEvent(READER_EVENTS.homeChanged)
        setError(readerErrorMessage(cause))
      }
    } finally {
      if (identityIsCurrent(client)) markBusy(item.key, false)
      else clearForIdentityLoss()
    }
  }, [capabilityCurrent, clearForIdentityLoss, client, commitState, markBusy, requireIdentity])

  const openItem = useCallback((item: ReaderFeedItemResponse) => {
    if (item.link_id) {
      onOpenLink(item.link_id)
    } else if (policy.inbox && item.inbox_id) {
      onNavigate({ kind: 'library', id: 'pending', inboxId: item.inbox_id })
    } else if (typeof window !== 'undefined') {
      window.open(item.url, '_blank', 'noopener,noreferrer')
    }
  }, [onNavigate, onOpenLink, policy.inbox])

  const sourceOptions = useMemo(
    () => FEED_SOURCES.filter((source) => source.id !== 'inbox' || policy.inbox),
    [policy.inbox],
  )
  const batchConfirmAvailable = policy.inbox && sourceFilter.includes('inbox')

  const renderFeedItem = (item: ReaderFeedItemResponse) => {
    const busy = busyKeys.has(item.key) || busyKeys.has(BATCH_CONFIRM_KEY)
    const source = feedSource(item)
    const saveAction: FeedFeedbackAction = item.saved ? 'unsave' : 'save'
    const canRead = policy.engagement && (source === 'reading' || source === 'subscription')
    const canReadLater = policy.engagement && (source === 'reading' || source === 'subscription')
    const canConfirm = policy.inbox && source === 'inbox'
    const canDiscard = policy.inbox && source === 'inbox'
    const canSave = source === 'subscription'
    const canHide = source !== null
    const canOpen = supportsOpen(item)
    const reasonExpanded = expandedReasonKey === item.key
    const sourceLabel = feedSourceLabel(item)
    const reasonText = feedReasonText(item)
    return (
      <ReaderListRow
        key={feedResourceIdentity(item)}
        variant="feed"
        // 行的公共布局已由 reader-list-row-feed 提供，`.rvx-feed-card*` 的样式规则
        // 都已删除；这个 class 只剩三个 e2e spec 的定位钩子，等它们改掉再摘。
        className="rvx-feed-card"
        dataAttributes={{ 'data-feed-item-key': item.key, 'data-resource-key': feedResourceIdentity(item) }}
        source={<span className="rvx-source-chip">{sourceLabel}</span>}
        meta={<button className="rvx-reason rvx-link-button" type="button" aria-label={`查看推荐原因：${reasonText}`} aria-expanded={reasonExpanded} onClick={() => setExpandedReasonKey((current) => current === item.key ? null : item.key)}><Icon name="explain" size={13} />{reasonText}</button>}
        title={item.title || '未命名内容'}
        onOpen={canOpen ? () => openItem(item) : undefined}
        summary={item.summary || '没有摘要'}
        details={reasonExpanded ? <p className="rvx-muted" role="note">推荐原因：{reasonText}</p> : undefined}
        footer={<time dateTime={item.event_at}>{formatRelativeDate(item.event_at)}</time>}
        actions={<div className="rvx-action-row">
          {canRead && <button type="button" disabled={busy} title={item.read ? '标为未读' : '标为已读'} onClick={() => void patchEngagement(item, { read: !item.read })}><Icon name={item.read ? 'check' : 'dot'} size={15} />{item.read ? '已读' : '未读'}</button>}
          {canReadLater && <button type="button" disabled={busy} title={item.read_later ? '移出稍后读' : '加入稍后读'} onClick={() => void patchEngagement(item, { read_later: !item.read_later })}><Icon name="clock" size={15} />{item.read_later ? '稍后' : '稍后读'}</button>}
          {canConfirm && item.inbox_id && <button type="button" disabled={busy} title="确认并保存到阅读库" onClick={() => void resolveInbox(item, 'confirm')}><Icon name="check" size={15} />确认</button>}
          {canDiscard && item.inbox_id && <button type="button" disabled={busy} title="丢弃该条目" onClick={() => void resolveInbox(item, 'discard')}><Icon name="trash" size={15} />丢弃</button>}
          {canSave && <button type="button" disabled={busy} title={saveAction === 'save' ? '保存到阅读库' : '取消保存'} onClick={() => void sendFeedback(item, saveAction)}><Icon name="bookmark" size={15} />{saveAction === 'save' ? '保存' : '取消保存'}</button>}
          {canHide && <button type="button" disabled={busy} title="从当前 Feed 隐藏" onClick={() => void sendFeedback(item, 'hide')}><Icon name="x" size={15} />隐藏</button>}
          {canOpen && <button type="button" className="rvx-open-action" disabled={busy} onClick={() => openItem(item)}><Icon name="arrowright" size={15} />{openActionLabel(item)}</button>}
        </div>}
      />
    )
  }

  const renderBatchConfirmation = () => {
    if (!batchConfirmAvailable) return null
    return <div className="rvx-editor" role="region" aria-label="AI 待确认批量操作">
      <div className="rvx-section-head">
        <div><span className="rvx-eyebrow">待确认段</span><h3>摘要都没问题？</h3></div>
        <button className="rvx-button primary" type="button" disabled={busyKeys.has(BATCH_CONFIRM_KEY)} aria-label="确认全部 AI 建议" title="确认活跃分区中已完成的 AI 建议" onClick={() => void confirmPendingBatch()}><Icon name="check" size={15} />{busyKeys.has(BATCH_CONFIRM_KEY) ? '确认中…' : '确认全部 AI 建议'}</button>
      </div>
      <p className="rvx-muted">服务端按稳定批次确认活跃分区中所有已完成的 AI 建议；完成前会持续处理剩余队列。</p>
    </div>
  }

  const feedControls = (
    <>
      <button className="rvx-button secondary" type="button" disabled={loading || loadingMore} onClick={refresh}><Icon name="refresh" size={15} />刷新</button>
      <div className="rvx-segmented" role="group" aria-label="Feed 排序">
        <button type="button" className={mode === 'recommended' ? 'active' : ''} aria-pressed={mode === 'recommended'} onClick={() => selectMode('recommended')}>推荐</button>
        <button type="button" className={mode === 'chronological' ? 'active' : ''} aria-pressed={mode === 'chronological'} onClick={() => selectMode('chronological')}>时间</button>
      </div>
    </>
  )

  const feedBody = (
      <div className="rvx-home-columns">
        <div>
          <ListStateView
            loading={loading && items.length === 0}
            error={items.length === 0 || visibleItems.length > 0 ? error : null}
            empty={items.length === 0 || visibleItems.length === 0}
            emptyState={items.length === 0
              ? (
                  <ListEmptyState
                    icon="layers"
                    title="Feed 暂时为空"
                    description="新的收件箱条目、阅读记录或订阅条目会出现在这里。"
                  />
                )
              : (
                  <ListEmptyState
                    icon="layers"
                    title="没有符合筛选的内容"
                    description="打开右侧的来源开关，继续浏览混合 Feed。"
                  />
                )}
            onRetry={retry}
          >
            <>
              <div className="rvx-feed-meta">
                <span>{mode === 'recommended' ? '按推荐顺序' : '按时间倒序'} · 已加载 {visibleItems.length} 条</span>
              </div>
              {renderBatchConfirmation()}
              <ul className="rvx-feed-list">{visibleItems.map(renderFeedItem)}</ul>
              {nextCursor && <button className="rvx-load-more" type="button" disabled={loadingMore} onClick={loadMore}>{loadingMore ? '加载中' : '加载更多'}</button>}
            </>
          </ListStateView>
        </div>
        {variant === 'surface' && <aside className="rvx-section" aria-label="Feed 辅助栏">
          {policy.todos && <section className="rvx-editor" aria-labelledby="feed-todos">
            <div className="rvx-section-head"><div><span className="rvx-eyebrow">右栏</span><h2 id="feed-todos">今天要做的</h2></div><button className="rvx-link-button" type="button" onClick={() => onNavigate({ kind: 'tool', id: 'todo' })}>查看全部</button></div>
            {todosLoading ? <p className="rvx-muted">TODO 加载中…</p> : todoError ? <p className="rvx-muted">TODO 暂时不可用。</p> : topTodos.length === 0 ? <p className="rvx-muted">没有未完成任务。</p> : <ul className="rvx-todo-list">{topTodos.map((todo) => <li key={todo.id} className="rvx-todo-row"><span className="rvx-todo-text">{todo.text}</span>{todo.due_at && <small>{todo.expired && !todo.done ? '已过期 · ' : '截止 · '}{formatRelativeDate(todo.due_at)}</small>}</li>)}</ul>}
          </section>}
          <section className="rvx-editor" aria-labelledby="feed-sources">
            <div className="rvx-section-head"><div><span className="rvx-eyebrow">筛选</span><h2 id="feed-sources">流里显示什么</h2></div></div>
            <div role="group" aria-label="Feed 来源筛选">
              {sourceOptions.map((source) => {
                const enabled = enabledSources.has(source.id)
                return <button key={source.id} className={'rvx-button ' + (enabled ? 'secondary' : '')} type="button" role="switch" aria-label={source.label} aria-checked={enabled} onClick={() => toggleSource(source.id)}><Icon name={enabled ? 'check' : 'close'} size={14} />{source.label}</button>
              })}
            </div>
            <button className="rvx-link-button" type="button" aria-expanded={showRecommendationHelp} onClick={() => setShowRecommendationHelp((current) => !current)}><Icon name="explain" size={14} />为什么推荐</button>
            {showRecommendationHelp && <p className="rvx-muted" role="note">推荐顺序由来源、未读状态和稍后读状态决定。</p>}
          </section>
        </aside>}
      </div>
  )

  if (variant === 'embedded') {
    return (
      <section className="rvx-feed-embedded" aria-label="今天的流">
        <div className="rvx-section-head">
          <div>
            <span className="rvx-eyebrow">继续读</span>
            <h2>今天的流</h2>
          </div>
          <div className="rvx-header-actions">{feedControls}</div>
        </div>
        {feedBody}
      </section>
    )
  }

  return (
    <SurfaceShell
      title="混合 Feed"
      subtitle="收件箱、已保存内容和订阅条目的统一工作流"
      onNavigate={onNavigate}
      capabilityPolicy={policy}
      scrollRef={scrollRef}
      onBack={() => onNavigate(policy.home
        ? { kind: 'surface', id: 'home' }
        : { kind: 'library', id: 'reading' })}
      actions={feedControls}
    >
      {feedBody}
    </SurfaceShell>
  )
}
