import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderCapabilityFeature, ReaderCapabilityLease } from '../../lib/capabilities'
import { isReaderFeedItemResponse } from '../../lib/api/guards'
import type {
  ReaderFeedAction,
  ReaderFeedItemResponse,
  ReaderFeedSectionResponse,
  ReaderFeedSourceResponse,
  ReaderTodoResponse,
} from '../../lib/api/types'
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
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'

export interface FeedSurfaceProps {
  readonly client: IdentityBoundReaderClient
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
type FeedFeedbackAction = 'save' | 'unsave' | 'hide' | 'not_interested'
type FeedSource = 'inbox' | 'reading' | 'subscription'
/** 两条互相独立的请求线：Feed 快照与右栏 TODO，各自按代次判断迟到回包。 */
type FeedRequestChannel = 'feed' | 'todos'
type FeedRequestToken = SurfaceRequestToken<FeedRequestChannel>

const FEED_SOURCES: readonly { readonly id: FeedSource; readonly label: string }[] = [
  { id: 'inbox', label: '收件箱' },
  { id: 'reading', label: '收藏' },
  { id: 'subscription', label: '订阅' },
]

const BATCH_CONFIRM_KEY = '__feed_batch_confirm__'

interface FeedResumeState {
  readonly snapshotID: string
  readonly nextCursor?: string
  readonly items: ReaderFeedItemResponse[]
  readonly sourceFilter: FeedSource[]
}

interface FeedLoadOptions {
  readonly append: boolean
  readonly cursor?: string
  readonly snapshotID?: string
  readonly resume?: FeedResumeState
}

const FEED_PAGE_SIZE = 30
const FEED_RESUME_STORAGE_PREFIX = 'webtag:reader:mixed-feed:v1'

// eslint-disable-next-line react-refresh/only-export-components
export function clearFeedSessionState(client: IdentityBoundReaderClient): void {
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

function sourceFilterStorageKey(client: IdentityBoundReaderClient): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:sources`
    : null
}

function readStoredSources(client: IdentityBoundReaderClient): Set<FeedSource> {
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

function writeStoredSources(client: IdentityBoundReaderClient, sources: Iterable<FeedSource>): void {
  const key = sourceFilterStorageKey(client)
  if (!key || typeof window === 'undefined') return
  try {
    window.sessionStorage.setItem(key, JSON.stringify(normalizeSourceFilter(sources)))
  } catch {
    // Session storage is a best-effort filter preference.
  }
}

function identityNamespace(client: IdentityBoundReaderClient): string | null {
  try {
    const namespace = client.identityLease.context.physicalNamespace
    return typeof namespace === 'string' && namespace.length > 0 ? namespace : null
  } catch {
    return null
  }
}

function resumeStorageKey(client: IdentityBoundReaderClient, mode: FeedMode, sourceFilter: readonly FeedSource[]): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:${mode}:${sourceFilter.length > 0 ? sourceFilter.join(',') : 'none'}`
    : null
}

function legacyResumeStorageKey(client: IdentityBoundReaderClient, mode: FeedMode): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:${mode}`
    : null
}

function modeStorageKey(client: IdentityBoundReaderClient): string | null {
  const namespace = identityNamespace(client)
  return namespace
    ? `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:mode`
    : null
}

function readStoredMode(client: IdentityBoundReaderClient): FeedMode {
  const key = modeStorageKey(client)
  if (!key || typeof window === 'undefined') return 'recommended'
  try {
    const value = window.sessionStorage.getItem(key)
    return isFeedMode(value) ? value : 'recommended'
  } catch {
    return 'recommended'
  }
}

function writeStoredMode(client: IdentityBoundReaderClient, mode: FeedMode): void {
  const key = modeStorageKey(client)
  if (!key || typeof window === 'undefined') return
  try {
    window.sessionStorage.setItem(key, mode)
  } catch {
    // Session storage is a best-effort position cache.
  }
}

function readResume(client: IdentityBoundReaderClient, mode: FeedMode, sourceFilter: readonly FeedSource[]): FeedResumeState | null {
  const key = resumeStorageKey(client, mode, sourceFilter)
  const legacyKey = sourceFilter.length === FEED_SOURCES.length
    ? legacyResumeStorageKey(client, mode)
    : null
  const keys = [key, legacyKey].filter((candidate): candidate is string => Boolean(candidate))
  if (keys.length === 0 || typeof window === 'undefined') return null
  for (const candidate of keys) {
    try {
      const raw = window.sessionStorage.getItem(candidate)
      if (!raw) continue
      const value: unknown = JSON.parse(raw)
      if (!isRecord(value) || (value.version !== 1 && value.version !== 2) || typeof value.snapshot_id !== 'string' || !value.snapshot_id) continue
      if (!Array.isArray(value.items) || !value.items.every(isReaderFeedItemResponse)) continue
      if (value.next_cursor !== undefined && typeof value.next_cursor !== 'string') continue
      if (value.source_filter !== undefined && (
        !Array.isArray(value.source_filter) ||
        !value.source_filter.every(isFeedSource) ||
        normalizeSourceFilter(value.source_filter).join(',') !== sourceFilter.join(',')
      )) continue
      return {
        snapshotID: value.snapshot_id,
        nextCursor: value.next_cursor || undefined,
        items: dedupeItems(value.items),
        sourceFilter: [...sourceFilter],
      }
    } catch {
      // Ignore malformed or unavailable position storage and start a new snapshot.
    }
  }
  return null
}

function writeResume(
  client: IdentityBoundReaderClient,
  mode: FeedMode,
  sourceFilter: readonly FeedSource[],
  snapshotID: string | undefined,
  nextCursor: string | undefined,
  items: readonly ReaderFeedItemResponse[],
): void {
  const key = resumeStorageKey(client, mode, sourceFilter)
  if (!key || typeof window === 'undefined' || !snapshotID) return
  try {
    window.sessionStorage.setItem(key, JSON.stringify({
      version: 2,
      snapshot_id: snapshotID,
      next_cursor: nextCursor,
      source_filter: sourceFilter,
      items: dedupeItems(items),
    }))
  } catch {
    // Session storage is a best-effort position cache.
  }
}

function feedResourceIdentity(item: ReaderFeedItemResponse): string {
  return item.resource_key?.trim() || item.key.trim()
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

/** Append unseen resources; a resume may restore only mutable local state in place. */
function mergeItems(
  current: readonly ReaderFeedItemResponse[],
  incoming: readonly ReaderFeedItemResponse[],
  restoreIncomingEngagement: boolean,
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
    if (restoreIncomingEngagement) {
      result[index] = { ...result[index], read: item.read, read_later: item.read_later }
    }
  }
  return result
}

function supportsItemAction(item: ReaderFeedItemResponse, action: ReaderFeedAction): boolean {
  return Array.isArray(item.actions) && item.actions.includes(action)
}

function withSavedFeedAction(item: ReaderFeedItemResponse, saved: boolean): ReaderFeedItemResponse {
  return {
    ...item,
    saved,
    actions: (item.actions ?? []).map((action) => action === 'save' || action === 'unsave'
      ? (saved ? 'unsave' : 'save')
      : action),
  }
}

function supportsOpen(item: ReaderFeedItemResponse): boolean {
  return supportsItemAction(item, 'open') || (supportsItemAction(item, 'open_workspace') && Boolean(item.link_id))
}

function feedSource(item: ReaderFeedItemResponse): FeedSource | null {
  const source = item.item_type ?? item.source
  if (source === 'inbox' || source === 'pending') return 'inbox'
  if (source === 'subscription' || source === 'feed') return 'subscription'
  if (source === 'reading' || source === 'saved') return 'reading'
  if (item.inbox_id) return 'inbox'
  if (item.feed_item_id) return 'subscription'
  if (item.link_id) return 'reading'
  return null
}

function feedSectionID(item: ReaderFeedItemResponse, sections: readonly ReaderFeedSectionResponse[]): string | null {
  if (item.section_id && sections.some((section) => section.id === item.section_id)) return item.section_id
  const source = feedSource(item)
  return sections.find((section) => section.source === source)?.id ?? source
}

function feedSourceLabel(item: ReaderFeedItemResponse, sources: readonly ReaderFeedSourceResponse[]): string {
  const source = feedSource(item)
  if (!source) return item.source
  return sources.find((candidate) => candidate.id === source)?.label ?? FEED_SOURCES.find((candidate) => candidate.id === source)?.label ?? item.source
}

function feedSectionLabel(item: ReaderFeedItemResponse, sections: readonly ReaderFeedSectionResponse[]): string | null {
  const sectionID = feedSectionID(item, sections)
  if (!sectionID) return null
  return sections.find((section) => section.id === sectionID)?.label ?? null
}

function openActionLabel(item: ReaderFeedItemResponse): string {
  return item.feed_item_id && !item.link_id && supportsItemAction(item, 'open') ? '打开原文' : '打开'
}

// Feed reasons are frozen server-side snapshot evidence. Reader maps only the
// discriminated tuple and deliberately ignores compatibility reason_text.
function feedReasonText(item: ReaderFeedItemResponse): string {
  switch (item.reason_code) {
    case 'pending_confirmation':
      return item.reason_params.source === 'inbox' ? '收件箱采集' : ''
    case 'saved_library':
      return item.reason_params.source === 'reading' ? '已保存到资料库' : ''
    case 'subscription_recent':
      return item.reason_params.source === 'subscription' ? '订阅更新' : ''
    case 'unread':
      return item.reason_params.read === false ? '尚未阅读' : ''
    case 'read_later':
      return item.reason_params.read_later === true ? '已加入稍后读' : ''
    case 'chronological_fallback':
      return typeof item.reason_params.created_at === 'string' ? '按时间排序' : ''
  }
}

function hasFeedCapability(capabilities: readonly string[], capability: string): boolean {
  return capabilities.includes(capability)
}

function isRecoverableSnapshotError(status: number | undefined): boolean {
  return status === 400 || status === 404 || status === 410 || status === 422
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
  const [snapshotID, setSnapshotID] = useState<string | undefined>()
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyKeys, setBusyKeys] = useState<Set<string>>(() => new Set())
  const [todos, setTodos] = useState<ReaderTodoResponse[]>([])
  const [todosLoading, setTodosLoading] = useState(true)
  const [todoError, setTodoError] = useState<string | null>(null)
  const [feedCapabilities, setFeedCapabilities] = useState<string[]>([])
  const [feedSections, setFeedSections] = useState<ReaderFeedSectionResponse[]>([])
  const [feedSources, setFeedSources] = useState<ReaderFeedSourceResponse[]>([])
  const [showRecommendationHelp, setShowRecommendationHelp] = useState(false)
  const [expandedReasonKey, setExpandedReasonKey] = useState<string | null>(null)
  // Key whose snapshot is on screen. Distinct from the key being requested, so
  // a response for an abandoned mode/filter can never move the current view.
  const [renderedKey, setRenderedKey] = useState<string | null>(null)
  const ownScrollRef = useRef<HTMLDivElement>(null)
  const scrollRef = hostScrollRef ?? ownScrollRef
  const itemsRef = useRef<ReaderFeedItemResponse[]>([])
  const snapshotRef = useRef<string | undefined>()
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
    snapshotRef.current = undefined
    cursorRef.current = undefined
    setItems([])
    setSnapshotID(undefined)
    setNextCursor(undefined)
    setBusyKeys(new Set())
    setTodos([])
    setTodosLoading(false)
    setTodoError(null)
    setFeedCapabilities([])
    setFeedSections([])
    setFeedSources([])
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
    nextSnapshotID: string | undefined,
    nextCursor: string | undefined,
  ) => {
    if (!capabilityLease.isCurrent('feed')) return
    const normalizedItems = dedupeItems(nextItems)
    itemsRef.current = normalizedItems
    snapshotRef.current = nextSnapshotID
    cursorRef.current = nextCursor
    setItems(normalizedItems)
    setSnapshotID(nextSnapshotID)
    setNextCursor(nextCursor)
    writeResume(client, mode, sourceFilter, nextSnapshotID, nextCursor, normalizedItems)
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
    if (typeof client.listTodos !== 'function') {
      setTodos([])
      setTodosLoading(false)
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
  const load = useCallback(async ({ append, cursor, snapshotID: requestedSnapshotID, resume }: FeedLoadOptions): Promise<FeedRequestToken | null> => {
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
        ...(requestedSnapshotID ? { snapshotID: requestedSnapshotID } : {}),
        ...(cursor ? { after: cursor } : {}),
        limit: FEED_PAGE_SIZE,
      }
      let result = await client.getReaderFeed(requestParams)

      // A persisted cursor can outlive the server's snapshot retention window.
      // Start a fresh snapshot instead of turning an expired position into an
      // apparently empty Feed.
      if (
        !result.ok &&
        !append &&
        requestedSnapshotID &&
        isRecoverableSnapshotError(result.error.status)
      ) {
        result = await client.getReaderFeed({ mode, source: sourceFilter, limit: FEED_PAGE_SIZE })
      }

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

      setFeedCapabilities(result.data.capabilities ?? [])
      setFeedSections(result.data.sections ?? [])
      setFeedSources(result.data.sources ?? [])

      const responseSnapshotID = result.data.snapshot_id || undefined
      const responseCursor = result.data.next_cursor || undefined
      if (append) {
        const sameSnapshot = Boolean(snapshotRef.current && responseSnapshotID === snapshotRef.current)
        const nextItems = sameSnapshot
          ? mergeItems(itemsRef.current, result.data.items, false)
          : dedupeItems(result.data.items)
        commitState(nextItems, responseSnapshotID, responseCursor)
      } else if (resume && responseSnapshotID === resume.snapshotID) {
        // The server response validates the snapshot and refreshes its first
        // page. Retain every locally loaded page and local action in the resume
        // copy, including the cursor for the next page.
        const nextItems = mergeItems(result.data.items, resume.items, true)
        commitState(nextItems, responseSnapshotID, resume.nextCursor ?? responseCursor)
        setRenderedKey(scrollAnchorKey)
      } else {
        commitState(result.data.items, responseSnapshotID, responseCursor)
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
    snapshotRef.current = undefined
    cursorRef.current = undefined
    setItems([])
    setSnapshotID(undefined)
    setNextCursor(undefined)
    setRenderedKey(null)
    setLoading(sourceFilter.length > 0)
    setLoadingMore(false)
    setError(null)
    setFeedCapabilities([])
    setFeedSections([])
    setFeedSources([])
    if (sourceFilter.length === 0) {
      return () => {
        gate.invalidate('feed')
      }
    }
    void load({ append: false, snapshotID: resume?.snapshotID, resume: resume ?? undefined })
    return () => {
      gate.invalidate('feed')
    }
  }, [clearForIdentityLoss, client, gate, load, mode, sourceFilter])

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

  // Re-requests the current key's snapshot without touching its stored anchor.
  // This is the reload half of the gesture: retrying a failed load is the
  // reader asking for the content they never got, not for a new place in it.
  const retry = useCallback(() => {
    const currentSnapshotID = snapshotRef.current
    const resume = currentSnapshotID
      ? { snapshotID: currentSnapshotID, nextCursor: cursorRef.current, items: itemsRef.current, sourceFilter: [...sourceFilter] }
      : undefined
    void load({ append: false, snapshotID: currentSnapshotID, resume })
  }, [load, sourceFilter])

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
    void load({ append: true, cursor, snapshotID: snapshotRef.current })
  }, [load, loadingMore])

  const patchEngagement = useCallback(async (
    item: ReaderFeedItemResponse,
    patch: { read?: boolean; read_later?: boolean },
  ) => {
    if (!capabilityCurrent('engagement') || !item.link_id && !item.feed_item_id) return
    const action: ReaderFeedAction | null = patch.read !== undefined
      ? 'read'
      : patch.read_later !== undefined
        ? 'read_later'
        : null
    if (!action || !supportsItemAction(item, action)) return
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
        commitState(nextItems, snapshotRef.current, cursorRef.current)
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
        commitState(nextItems, snapshotRef.current, cursorRef.current)
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
      commitState(nextItems, snapshotRef.current, cursorRef.current)
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
    if (!supportsItemAction(item, action)) return
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
          snapshotRef.current,
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
        snapshotRef.current,
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
    if (!capabilityCurrent('inbox') || !hasFeedCapability(feedCapabilities, 'inbox_batch') || busyKeys.has(BATCH_CONFIRM_KEY)) return
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
  }, [busyKeys, capabilityCurrent, clearForIdentityLoss, client, feedCapabilities, gate, load, markBusy, requireIdentity, scrollAnchor])

  const sendFeedback = useCallback(async (
    item: ReaderFeedItemResponse,
    action: FeedFeedbackAction,
  ) => {
    if (!capabilityCurrent('feed') || !supportsItemAction(item, action)) return
    if (!requireIdentity()) return
    markBusy(item.key, true)
    const previousItems = itemsRef.current
    const optimisticItems = action === 'hide' || action === 'not_interested'
      ? previousItems.filter((candidate) => candidate.key !== item.key)
      : previousItems.map((candidate) => candidate.key === item.key
        ? withSavedFeedAction(candidate, action === 'save')
        : candidate)
    commitState(optimisticItems, snapshotRef.current, cursorRef.current)
    emitReaderEvent(READER_EVENTS.homeChanged)
    try {
      const itemKey = item.action_key?.trim() || item.key
      const result = await client.sendReaderFeedFeedback(itemKey, { action })
      if (!requireIdentity() || !capabilityCurrent('feed')) return
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else {
          commitState(previousItems, snapshotRef.current, cursorRef.current)
          emitReaderEvent(READER_EVENTS.homeChanged)
          setError(readerErrorMessage(result.error))
        }
        return
      }
      const nextItems = action === 'hide' || action === 'not_interested'
        ? optimisticItems
        : optimisticItems.map((candidate) => candidate.key === item.key
          ? { ...withSavedFeedAction(candidate, result.data.saved), link_id: result.data.association?.link_id ?? candidate.link_id }
          : candidate)
      commitState(nextItems, snapshotRef.current, cursorRef.current)
      emitReaderEvent(READER_EVENTS.homeChanged)
    } catch (cause) {
      if (requireIdentity()) {
        commitState(previousItems, snapshotRef.current, cursorRef.current)
        emitReaderEvent(READER_EVENTS.homeChanged)
        setError(readerErrorMessage(cause))
      }
    } finally {
      if (identityIsCurrent(client)) markBusy(item.key, false)
      else clearForIdentityLoss()
    }
  }, [capabilityCurrent, clearForIdentityLoss, client, commitState, markBusy, requireIdentity])

  const openItem = useCallback((item: ReaderFeedItemResponse) => {
    if (supportsItemAction(item, 'open_workspace') && item.link_id) {
      onOpenLink(item.link_id)
    } else if (policy.inbox && supportsItemAction(item, 'open') && item.inbox_id) {
      onNavigate({ kind: 'library', id: 'pending', inboxId: item.inbox_id })
    } else if (supportsItemAction(item, 'open') && typeof window !== 'undefined') {
      window.open(item.url, '_blank', 'noopener,noreferrer')
    }
  }, [onNavigate, onOpenLink, policy.inbox])

  const sourceOptions = useMemo(
    () => FEED_SOURCES.filter((source) => source.id !== 'inbox' || policy.inbox).map((source) => ({
      ...source,
      metadata: feedSources.find((candidate) => candidate.id === source.id),
    })),
    [feedSources, policy.inbox],
  )
  const sourceFilterAvailable = hasFeedCapability(feedCapabilities, 'source_filter')
  const batchConfirmAvailable = policy.inbox && hasFeedCapability(feedCapabilities, 'inbox_batch')

  const renderFeedItem = (item: ReaderFeedItemResponse) => {
    const busy = busyKeys.has(item.key) || busyKeys.has(BATCH_CONFIRM_KEY)
    const saveAction: FeedFeedbackAction = item.saved ? 'unsave' : 'save'
    const canRead = policy.engagement && supportsItemAction(item, 'read')
    const canReadLater = policy.engagement && supportsItemAction(item, 'read_later')
    const canConfirm = policy.inbox && supportsItemAction(item, 'confirm')
    const canDiscard = policy.inbox && supportsItemAction(item, 'discard')
    const canSave = supportsItemAction(item, saveAction)
    const canHide = supportsItemAction(item, 'hide')
    const canReject = supportsItemAction(item, 'not_interested')
    const canOpen = supportsOpen(item)
    const reasonExpanded = expandedReasonKey === item.key
    const sourceLabel = feedSourceLabel(item, feedSources)
    const sectionLabel = feedSectionLabel(item, feedSections)
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
        meta={<>
          {sectionLabel && sectionLabel !== sourceLabel && <span className="rvx-muted">分段：{sectionLabel}</span>}
          <button className="rvx-reason rvx-link-button" type="button" aria-label={`查看推荐原因：${reasonText}`} aria-expanded={reasonExpanded} onClick={() => setExpandedReasonKey((current) => current === item.key ? null : item.key)}><Icon name="explain" size={13} />{reasonText}</button>
        </>}
        title={item.title || '未命名内容'}
        onOpen={canOpen ? () => openItem(item) : undefined}
        summary={item.summary || '没有摘要'}
        details={reasonExpanded ? <p className="rvx-muted" role="note">推荐原因：{reasonText}（规则 {item.reason_code}）</p> : undefined}
        footer={<time dateTime={item.event_at}>{formatRelativeDate(item.event_at)}</time>}
        actions={<div className="rvx-action-row">
          {canRead && <button type="button" disabled={busy} title={item.read ? '标为未读' : '标为已读'} onClick={() => void patchEngagement(item, { read: !item.read })}><Icon name={item.read ? 'check' : 'dot'} size={15} />{item.read ? '已读' : '未读'}</button>}
          {canReadLater && <button type="button" disabled={busy} title={item.read_later ? '移出稍后读' : '加入稍后读'} onClick={() => void patchEngagement(item, { read_later: !item.read_later })}><Icon name="clock" size={15} />{item.read_later ? '稍后' : '稍后读'}</button>}
          {canConfirm && item.inbox_id && <button type="button" disabled={busy} title="确认并保存到阅读库" onClick={() => void resolveInbox(item, 'confirm')}><Icon name="check" size={15} />确认</button>}
          {canDiscard && item.inbox_id && <button type="button" disabled={busy} title="丢弃该条目" onClick={() => void resolveInbox(item, 'discard')}><Icon name="trash" size={15} />丢弃</button>}
          {canSave && <button type="button" disabled={busy} title={saveAction === 'save' ? '保存到阅读库' : '取消保存'} onClick={() => void sendFeedback(item, saveAction)}><Icon name="bookmark" size={15} />{saveAction === 'save' ? '保存' : '取消保存'}</button>}
          {canHide && <button type="button" disabled={busy} title="从当前 Feed 隐藏" onClick={() => void sendFeedback(item, 'hide')}><Icon name="x" size={15} />隐藏</button>}
          {canReject && <button type="button" disabled={busy} title="不再推荐此内容" onClick={() => void sendFeedback(item, 'not_interested')}><Icon name="close" size={15} />不感兴趣</button>}
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
          {loading && items.length === 0 ? <SurfaceLoading /> : error && items.length === 0 ? (
            <SurfaceError message={error} onRetry={retry} />
          ) : items.length === 0 ? (
            <div className="rvx-empty"><Icon name="layers" size={24} /><h2>Feed 暂时为空</h2><p>新的收件箱条目、阅读记录或订阅条目会出现在这里。</p></div>
          ) : visibleItems.length === 0 ? (
            <div className="rvx-empty"><Icon name="layers" size={24} /><h2>没有符合筛选的内容</h2><p>打开右侧的来源开关，继续浏览混合 Feed。</p></div>
          ) : (
            <>
              {error && <SurfaceError message={error} onRetry={retry} />}
              <div className="rvx-feed-meta" data-feed-snapshot={snapshotID ?? undefined}>
                <span>{mode === 'recommended' ? '按推荐顺序' : '按时间倒序'} · 已加载 {visibleItems.length} 条</span>
              </div>
              {feedSections.length > 0 && <div className="rvx-feed-meta" aria-label="Feed 分段">
                <span className="rvx-eyebrow">分段</span>
                {feedSections.map((section) => <span key={section.id} className="rvx-source-chip">{section.label} · {section.count} 条</span>)}
              </div>}
              {renderBatchConfirmation()}
              <ul className="rvx-feed-list">{visibleItems.map(renderFeedItem)}</ul>
              {nextCursor && <button className="rvx-load-more" type="button" disabled={loadingMore} onClick={loadMore}>{loadingMore ? '加载中' : '加载更多'}</button>}
            </>
          )}
        </div>
        {variant === 'surface' && <aside className="rvx-section" aria-label="Feed 辅助栏">
          {policy.todos && <section className="rvx-editor" aria-labelledby="feed-todos">
            <div className="rvx-section-head"><div><span className="rvx-eyebrow">右栏</span><h2 id="feed-todos">今天要做的</h2></div><button className="rvx-link-button" type="button" onClick={() => onNavigate({ kind: 'tool', id: 'todo' })}>查看全部</button></div>
            {todosLoading ? <p className="rvx-muted">TODO 加载中…</p> : todoError ? <p className="rvx-muted">TODO 暂时不可用。</p> : topTodos.length === 0 ? <p className="rvx-muted">没有未完成任务。</p> : <ul className="rvx-todo-list">{topTodos.map((todo) => <li key={todo.id} className="rvx-todo-row"><span className="rvx-todo-text">{todo.text}</span>{todo.due_at && <small>{todo.expired && !todo.done ? '已过期 · ' : '截止 · '}{formatRelativeDate(todo.due_at)}</small>}</li>)}</ul>}
          </section>}
          <section className="rvx-editor" aria-labelledby="feed-sources">
            <div className="rvx-section-head"><div><span className="rvx-eyebrow">筛选</span><h2 id="feed-sources">流里显示什么</h2></div></div>
            {sourceFilterAvailable ? <div role="group" aria-label="Feed 来源筛选">
              {sourceOptions.map((source) => {
                const enabled = enabledSources.has(source.id)
                return <div key={source.id}><button className={'rvx-button ' + (enabled ? 'secondary' : '')} type="button" role="switch" aria-label={source.label} aria-checked={enabled} onClick={() => toggleSource(source.id)}><Icon name={enabled ? 'check' : 'close'} size={14} />{source.label}</button>{source.metadata && <small className="rvx-muted">{source.metadata.count} 条 · {source.metadata.capabilities.length > 0 ? '有可用动作' : '暂无动作'}</small>}</div>
              })}
            </div> : <p className="rvx-muted">服务端未提供来源切换能力。</p>}
            <button className="rvx-link-button" type="button" aria-expanded={showRecommendationHelp} onClick={() => setShowRecommendationHelp((current) => !current)}><Icon name="explain" size={14} />为什么推荐</button>
            {showRecommendationHelp && <p className="rvx-muted" role="note">推荐只使用当前身份的阅读、收藏和订阅行为；每张卡片都保留服务端返回的推荐原因，关闭来源后只影响当前显示。</p>}
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
