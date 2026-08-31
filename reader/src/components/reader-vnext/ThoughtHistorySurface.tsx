import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderThoughtResponse, ReaderTodoResponse } from '../../lib/api/types'
import type { IdentityLease } from '../../lib/identity'
import {
  readerRouteIsAvailable,
  type ReaderCapabilityLease,
} from '../../lib/capabilities'
import {
  READER_NAVIGATION_COMMIT_EVENT,
  readerRouteURL,
  readerThoughtHostTarget,
  readerThoughtIDFromURL,
  readerThoughtViewFromURL,
  type ReaderNavigationRequest,
  type ReaderThoughtView,
} from '../../lib/navigation/route'
import {
  cacheServerThoughtPage,
  getThoughtSyncController,
  selectThoughtReadModel,
  sortThoughtReadModel,
  type ThoughtReadModelItem,
} from '../../lib/user-data/thought-sync'
import {
  commitThoughtHistoryAction,
  readThoughtActionSnapshot,
  type ThoughtHistorySnapshotInput,
} from '../../lib/user-data/thought-history-actions'
import {
  commitSupersessionRecovery,
  type SupersessionRecoveryResult,
} from '../../lib/user-data/annotation-store'
import {
  listThoughtSupersessionEvents,
  readThoughtSupersessionRecoveryAvailability,
  readThoughtSupersessionRecoveryStates,
  syncThoughtSupersessions,
  type ThoughtSupersessionRecoveryAvailability,
  type ThoughtSupersessionRecoveryState,
} from '../../lib/user-data/thought-supersession'
import type { ThoughtSupersessionEventRecord } from '../../lib/user-data/thought-types'
import { useExclusiveAction } from '../../hooks/useExclusiveAction'
import { useSurfaceRequestGate } from '../../hooks/useSurfaceRequestGate'
import { asRecord } from '../../lib/records'
import { restoreFocusWhenReady } from '../../lib/restore-focus'
import { READER_EVENTS, emitReaderEvent, subscribeReaderEvents } from '../../lib/reader-events'
import { formatRelativeDate, identityIsCurrent, readerErrorMessage } from '../../lib/reader-surface'
import { Icon } from '../Icon'
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'
import { NoteWorkspaceTabs } from './NoteWorkspaceTabs'

export type ThoughtFilter = 'all' | 'highlighted' | 'with-thought' | 'todo'

const THOUGHT_FILTERS: ReadonlyArray<{ readonly value: ThoughtFilter; readonly label: string }> = [
  { value: 'all', label: '全部' },
  { value: 'highlighted', label: '仅划线' },
  { value: 'with-thought', label: '有想法' },
  { value: 'todo', label: '未完成 TODO' },
]

const LIFECYCLE_REASON_LABELS: Readonly<Record<string, string>> = {
	link_converted_to_site: '链接已转为网站收藏',
  link_deleted: '链接已删除',
  note_deleted: '笔记已删除',
  note_revision_restored: '笔记版本已恢复',
  content_restored: '正文版本已恢复',
  inbox_discarded: 'Inbox 已丢弃',
}

function lifecycleReasonText(reason: string | null | undefined): string {
  const normalized = reason?.trim()
  if (!normalized) return '来源不可用'
  const label = LIFECYCLE_REASON_LABELS[normalized]
  return label ? `${label}（${normalized}）` : normalized
}

function historyActionFailureMessage(action: 'reattach' | 'delete', errorCode?: string): string {
  if (errorCode === 'superseded') {
    return '历史想法操作已被较新的服务端版本覆盖。冻结候选已保留，请刷新后重试。'
  }
  if (errorCode?.includes('revision_conflict')) {
    return '目标宿主已修改。冻结候选已保留，请刷新目标后重试。'
  }
  return action === 'reattach'
    ? '想法重挂已保存在本地，将在同步恢复后重试。'
    : '想法删除已保存在本地，将在同步恢复后重试。'
}

function hasHighlight(thought: ThoughtReadModelItem): boolean {
  const quote = asRecord(thought.quote)
  if (!quote) return false
  const exact = typeof quote.exact === 'string' && quote.exact.trim().length > 0
  const start = typeof quote.start === 'number' ? quote.start : -1
  const end = typeof quote.end === 'number' ? quote.end : -1
  return exact || (Number.isSafeInteger(start) && Number.isSafeInteger(end) && end > start)
}

function frozenValueText(value: unknown): string | null {
	if (typeof value === 'string') return value
	if (value === null || value === undefined) return null
	try {
		return JSON.stringify(value)
	} catch {
		return null
	}
}

function todoBelongsToThought(todo: ReaderTodoResponse, thoughtID: string): boolean {
  const originKind = todo.origin_host_kind ?? todo.origin_kind
  if (originKind === 'thought' && todo.origin_host_id === thoughtID) return true
  const origin = asRecord(todo.origin_ref)
  return origin?.thought_id === thoughtID || origin?.annotation_id === thoughtID
}

function matchesFilter(
  thought: ThoughtReadModelItem,
  filter: ThoughtFilter,
  todos: readonly ReaderTodoResponse[],
): boolean {
  if (filter === 'all') return true
  // A highlight becomes a Thought as soon as it has any non-whitespace body.
  // Apply this after the local-first merge so a pending body update cannot
  // remain incorrectly visible in the pure-highlight view.
  if (filter === 'highlighted') return hasHighlight(thought) && thought.body.trim().length === 0
  if (filter === 'with-thought') return thought.body.trim().length > 0
  return todos.some((todo) => !todo.done && todoBelongsToThought(todo, thought.id))
}

function thoughtTitle(thought: ThoughtReadModelItem): string {
  const firstLine = thought.body.split(/\r?\n/).map((line) => line.trim()).find(Boolean) ?? '无正文想法'
  return `想法：${firstLine.slice(0, 200)}`
}

function thoughtSource(thought: ThoughtReadModelItem): string {
  return `${thought.host_kind}/${thought.host_id}`
}

function thoughtSourceURL(thought: ThoughtReadModelItem): string | null {
  const target = readerThoughtHostTarget(thought)
  return target
    ? readerRouteURL(target.route, 'https://reader.invalid/', target.targets).search
    : null
}

function thoughtMarkdownSource(thought: ThoughtReadModelItem): string {
  const sourceURL = thoughtSourceURL(thought)
  return sourceURL
    ? `[${thoughtSource(thought)}](${sourceURL})`
    : `\`${thoughtSource(thought)}\``
}

function thoughtMarkdown(thought: ThoughtReadModelItem): string {
  const body = thought.body.trim() || '无正文想法'
  const quote = body.split(/\r?\n/).map((line) => `> ${line}`).join('\n')
  return [
    quote,
    '',
    `来源：${thoughtMarkdownSource(thought)}`,
    `稳定引用：\`thought:${thought.id}\` · 同步序列 \`${thought.last_sequence}\``,
    `<!-- webtag-thought: ${thought.id} sequence=${thought.last_sequence} -->`,
  ].join('\n')
}

function thoughtDeleteConfirmation(thought: ThoughtReadModelItem): string {
  const firstLine = thought.body.split(/\r?\n/).map((line) => line.trim()).find(Boolean) ?? '无正文想法'
  const preview = firstLine.length > 60 ? `${firstLine.slice(0, 60)}...` : firstLine
  return `删除想法“${preview}”？删除操作会先保存在本地，并在同步后生效。`
}

type ThoughtHistoryItem = ThoughtReadModelItem & Partial<Pick<
  ReaderThoughtResponse,
  'contract_version' | 'winner_key' | 'original_host_snapshot'
>>

function frozenHistorySnapshot(thought: ThoughtHistoryItem): ThoughtHistorySnapshotInput | null {
  const winnerKey = asRecord(thought.winner_key)
  const logicalClock = winnerKey?.logical_clock
  const deviceID = winnerKey?.device_id
  const operationID = winnerKey?.op_id
  if (thought.contract_version !== 1 || !Number.isSafeInteger(logicalClock) ||
    (logicalClock as number) < 0 || typeof deviceID !== 'string' || !deviceID ||
    typeof operationID !== 'string' || !operationID) {
    return null
  }
  return {
    id: thought.id,
    hostKind: thought.host_kind,
    hostId: thought.host_id,
    target: thought.target,
    quote: thought.quote ?? null,
    body: thought.body,
    source: thought.source,
    lastSequence: thought.last_sequence,
    winnerKey: {
      logicalClock: logicalClock as number,
      deviceId: deviceID,
      opId: operationID,
    },
    ...(thought.original_host_snapshot === undefined
      ? {}
      : { originalHostSnapshot: thought.original_host_snapshot }),
  }
}
function navigateToNote(onNavigate: ReaderNavigationRequest, noteID: string): void {
  onNavigate({ kind: 'library', id: 'notes' }, { noteId: noteID })
}

function navigateToHost(onNavigate: ReaderNavigationRequest, thought: ThoughtReadModelItem): void {
  const target = readerThoughtHostTarget(thought)
  if (target) onNavigate(target.route, target.targets)
}

function mergeThoughts(
  current: readonly ThoughtReadModelItem[],
  incoming: readonly ThoughtReadModelItem[],
): readonly ThoughtReadModelItem[] {
  const merged = new Map(current.map((thought) => [thought.id, thought]))
  for (const thought of incoming) merged.set(thought.id, thought)
  return [...merged.values()]
}

/**
 * `list` 覆盖列表读取与本地读模型投影（投影搭车于列表代次，不自己顶掉在途
 * 列表请求）；`focused` 覆盖 deep-link 单条历史想法的补齐读取。
 */
type ThoughtRequestChannel = 'list' | 'focused'

export interface ThoughtHistorySurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly lease: IdentityLease
  readonly onNavigate: ReaderNavigationRequest
  readonly capabilityLease: ReaderCapabilityLease
}

export function ThoughtHistorySurface({ client, lease, onNavigate, capabilityLease }: ThoughtHistorySurfaceProps) {
  const capabilityPolicy = capabilityLease.policy
  const [view, setView] = useState<ReaderThoughtView>(() => typeof window === 'undefined' ? 'live' : readerThoughtViewFromURL(window.location.href))
  const [focusedThoughtID, setFocusedThoughtID] = useState<string | undefined>(() =>
    typeof window === 'undefined' ? undefined : readerThoughtIDFromURL(window.location.href))
  const [filter, setFilter] = useState<ThoughtFilter>('all')
  const [items, setItems] = useState<readonly ThoughtReadModelItem[]>([])
  const [supersessions, setSupersessions] = useState<readonly ThoughtSupersessionEventRecord[]>([])
  const [recoveryAvailability, setRecoveryAvailability] = useState<ReadonlyMap<
    number,
    ThoughtSupersessionRecoveryAvailability
  >>(new Map())
  const [recoveryStates, setRecoveryStates] = useState<ReadonlyMap<
    number,
    ThoughtSupersessionRecoveryState
  >>(new Map())
  const [todos, setTodos] = useState<ReaderTodoResponse[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [selectedThoughtIDs, setSelectedThoughtIDs] = useState<string[]>([])
  const [reattachID, setReattachID] = useState<string | null>(null)
  const [targetKind, setTargetKind] = useState<'link' | 'note' | 'inbox'>('link')
  const [targetID, setTargetID] = useState('')
  const [targetRevision, setTargetRevision] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
	const [saving, setSaving] = useState(false)
  const [creating, setCreating] = useState(false)
  const [recoveringSequence, setRecoveringSequence] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  // owner 是身份租约：换租约即让所有在途 token 失效，旧命名空间的回包画不到新
  // 身份上。authority 只管身份是否仍然当前；能力租约的 `isCurrent('history')`
  // 由各操作用自己捕获的 operationLease 单独判断，语义与迁移前逐字一致。
  const gate = useSurfaceRequestGate<ThoughtRequestChannel>({
    owner: [lease],
    authority: () => identityIsCurrent(client),
  })
  const deleting = useExclusiveAction<string>()
  const { begin: beginDelete, clear: clearDeleting, finish: finishDelete, owns: ownsDelete } = deleting
  const deletingBusy = deleting.busy
  const deletingID = deleting.busyKey
  const loadingMoreRef = useRef(false)
  const activeViewButtonRef = useRef<HTMLButtonElement | null>(null)
  const deleteButtonRefs = useRef(new Map<string, HTMLButtonElement>())
  // 闸门只记得「有没有更新的请求」，不记得请求的是什么。补齐读取还需要一个
  // 「同一 client + 同一 thoughtID 就不要再发一次」的去重键，由这个 ref 单独持有。
  const focusedTargetRef = useRef<{ readonly client: IdentityBoundReaderClient; readonly thoughtID: string } | null>(null)
  const focusedThoughtIDRef = useRef(focusedThoughtID)

  useEffect(() => {
    focusedThoughtIDRef.current = focusedThoughtID
  }, [focusedThoughtID])

  // Do not allow a previous identity's list or opaque next cursor to paint
  // while the replacement lease loads its own namespace.
  useLayoutEffect(() => {
    setItems([])
    setTodos([])
    setNextCursor(undefined)
    setSelectedThoughtIDs([])
    setError(null)
    setLoading(true)
    clearDeleting()
    focusedTargetRef.current = null
  }, [clearDeleting, lease])

  const requestNavigation = useCallback<ReaderNavigationRequest>((route, targets) => {
    if (deletingBusy) return false
    return onNavigate(route, targets)
  }, [deletingBusy, onNavigate])

  const restoreDeleteFocus = useCallback((thoughtID?: string) => {
    // 目标每次尝试重新求值：删除后列表要重渲染，按钮可能还没挂上，或者挂上了
    // 但仍处于 disabled（视图按钮在删除进行中是禁用的）。判据是焦点真的落上，
    // 细节见 lib/restore-focus。
    restoreFocusWhenReady({
      target: () => (thoughtID ? deleteButtonRefs.current.get(thoughtID) : undefined)
        ?? activeViewButtonRef.current,
    })
  }, [])

  const refreshLocalProjection = useCallback(async () => {
    if (view !== 'live' || !lease.context?.physicalNamespace || !client.isIdentityCurrent()) return
    // 搭车于当前列表代次：投影是旁路读取，不该顶掉在途的 `load()`。
    const token = gate.capture('list')
    const selected = await selectThoughtReadModel(lease)
    if (!selected.ok || !gate.isCurrent(token)) return
    setItems(selected.value)
  }, [client, gate, lease, view])

  const visibleItems = useMemo(() => {
    const ordered = sortThoughtReadModel(items)
    return view === 'live' ? ordered.filter((thought) => !thought.deleted && matchesFilter(thought, filter, todos)) : ordered
  }, [filter, items, todos, view])

  const selectedThoughts = useMemo(
    () => selectedThoughtIDs
      .map((id) => items.find((thought) => thought.id === id))
      .filter((thought): thought is ThoughtReadModelItem => thought !== undefined),
    [items, selectedThoughtIDs],
  )

  const load = useCallback(async (append = false, cursor?: string) => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('history')) {
      setItems([])
      setTodos([])
      setLoading(false)
      return
    }
    const token = gate.begin('list')
    if (append) {
      loadingMoreRef.current = true
      setLoadingMore(true)
    } else {
      setLoading(true)
    }
    setError(null)
    try {
      if (view === 'superseded') {
        const synced = await syncThoughtSupersessions(lease, client)
        if (!gate.isCurrent(token)) return
        if (synced.status === 'failed') {
          setError(synced.error ?? '被取代版本加载失败，请稍后重试。')
          return
        }
        if (synced.status === 'stale') return
        const events = await listThoughtSupersessionEvents(lease)
        if (!gate.isCurrent(token)) return
        if (!events.ok) {
          setError('无法读取被取代版本。')
          return
        }
        const availability = await readThoughtSupersessionRecoveryAvailability(lease, events.value)
        if (!gate.isCurrent(token)) return
        if (!availability.ok) {
          setError('无法确认被取代版本的当前锚点。')
          return
        }
        const recoveryStates = await readThoughtSupersessionRecoveryStates(lease, events.value)
        if (!gate.isCurrent(token)) return
        if (!recoveryStates.ok) {
          setError('无法确认恢复候选状态。')
          return
        }
        setSupersessions(events.value)
        setRecoveryAvailability(availability.value)
        setRecoveryStates(recoveryStates.value)
        setNextCursor(undefined)
        return
      }
      const thoughts = view === 'live'
        ? await client.listThoughts({ after: cursor, limit: 30 })
        : await client.listThoughtHistory({ after: cursor, limit: 30 })
      if (!gate.isCurrent(token) || !operationLease.isCurrent('history')) return
      if (!thoughts.ok) {
        setError(readerErrorMessage(thoughts.error))
        return
      }
      let visibleThoughts: readonly ThoughtReadModelItem[] = thoughts.data.items
      // The aggregate endpoint supplies a page and its opaque cursor, while
      // the selector owns visibility.  Cache the page first so local durable
      // operations survive a reload and then reapply the same overlay.
      if (view === 'live' && lease.context?.physicalNamespace) {
        const cached = await cacheServerThoughtPage(lease, thoughts.data.items)
        if (!gate.isCurrent(token)) return
        if (!cached.ok) {
          setError('无法保存想法本地读模型。')
          return
        }
        const selected = await selectThoughtReadModel(lease)
        if (!gate.isCurrent(token)) return
        if (!selected.ok) {
          setError('无法读取想法本地读模型。')
          return
        }
        visibleThoughts = selected.value
      }
      if (view === 'live' && operationLease.isCurrent('todos')) {
        const todoResult = await client.listTodos()
        if (!gate.isCurrent(token) || !operationLease.isCurrent('history')) return
        if (!todoResult.ok) setError(readerErrorMessage(todoResult.error))
        else setTodos(todoResult.data.items)
      } else if (!append) {
        setTodos([])
      }
      if (view === 'live') {
        setItems(visibleThoughts)
      } else {
        setItems((current) => {
          if (append) return mergeThoughts(current, thoughts.data.items)
          const focused = focusedThoughtIDRef.current
          const focusedThought = focused
            ? current.find((thought) => thought.id === focused)
            : undefined
          if (!focusedThought || thoughts.data.items.some((thought) => thought.id === focusedThought.id)) {
            return thoughts.data.items
          }
          return [...thoughts.data.items, focusedThought]
        })
      }
      setNextCursor(thoughts.data.next_cursor)
    } catch (requestError) {
      if (gate.isCurrent(token)) {
        setError(readerErrorMessage(requestError, view === 'live' ? '想法加载失败，请稍后重试。' : '历史想法加载失败，请稍后重试。'))
      }
    } finally {
      // 释放自己占住的 loading 用 isSameOwner：身份刚被撤销时也必须收掉转圈。
      if (gate.isSameOwner(token)) {
        setLoading(false)
        loadingMoreRef.current = false
        setLoadingMore(false)
      }
    }
  }, [capabilityLease, client, gate, lease, view])

  useEffect(() => {
    setItems([])
    setSupersessions([])
    setRecoveryAvailability(new Map())
    setRecoveryStates(new Map())
    setNextCursor(undefined)
    setSelectedThoughtIDs([])
    void load()
    // A durable offline thought can predate this surface and its event listener.
    void refreshLocalProjection()
    return () => { gate.invalidate('list') }
  }, [gate, load, refreshLocalProjection])

  useEffect(() => {
    if (!capabilityPolicy.todos && filter === 'todo') setFilter('all')
    if (!capabilityPolicy.notes) setSelectedThoughtIDs([])
    if (
      (targetKind === 'note' && !capabilityPolicy.notes) ||
      (targetKind === 'inbox' && !capabilityPolicy.inbox)
    ) {
      setTargetKind('link')
    }
  }, [capabilityPolicy.inbox, capabilityPolicy.notes, capabilityPolicy.todos, filter, targetKind])

  useEffect(() => subscribeReaderEvents(
    [READER_EVENTS.annotationsChanged, READER_EVENTS.thoughtsSynced],
    () => { void refreshLocalProjection() },
  ), [refreshLocalProjection])

  useEffect(() => {
    const onPopState = () => {
      const url = new URL(window.location.href)
      if (url.searchParams.get('tool') !== 'history') return
      setView(readerThoughtViewFromURL(url))
      setFocusedThoughtID(readerThoughtIDFromURL(url))
    }
    window.addEventListener('popstate', onPopState)
    window.addEventListener(READER_NAVIGATION_COMMIT_EVENT, onPopState)
    return () => {
      window.removeEventListener('popstate', onPopState)
      window.removeEventListener(READER_NAVIGATION_COMMIT_EVENT, onPopState)
    }
  }, [])

  useEffect(() => {
    const operationLease = capabilityLease
    if (view !== 'history' || !focusedThoughtID || items.some((item) => item.id === focusedThoughtID) ||
      !operationLease.isCurrent('history')) return
    if (focusedTargetRef.current?.client === client && focusedTargetRef.current.thoughtID === focusedThoughtID) return

    const target = { client, thoughtID: focusedThoughtID }
    focusedTargetRef.current = target
    const token = gate.begin('focused')
    void client.getThought(focusedThoughtID).then((result) => {
      if (!gate.isCurrent(token) || !operationLease.isCurrent('history')) return
      if (!result.ok) {
        setError(readerErrorMessage(result.error))
        return
      }
      if (result.data.id !== focusedThoughtID) return
      if (result.data.deleted || result.data.lifecycle_status !== 'tombstone') {
        setError('目标历史想法已不可用。')
        return
      }
      setItems((current) => mergeThoughts(current, [result.data]))
    }).catch((requestError: unknown) => {
      if (gate.isCurrent(token) && operationLease.isCurrent('history')) {
        setError(readerErrorMessage(requestError, '历史想法加载失败，请稍后重试。'))
      }
    })
    return () => {
      gate.invalidate('focused')
      if (focusedTargetRef.current === target) focusedTargetRef.current = null
    }
  }, [capabilityLease, client, focusedThoughtID, gate, items, view])

  const changeView = useCallback((nextView: ReaderThoughtView) => {
    if (nextView === view || deletingBusy) return
    if (requestNavigation({ kind: 'tool', id: 'history' }, { thoughtView: nextView }) === false) return
    setView(nextView)
  }, [deletingBusy, requestNavigation, view])

  const loadMore = useCallback(() => {
    if (!nextCursor || loadingMoreRef.current || loading || saving || creating) return
    void load(true, nextCursor)
  }, [creating, load, loading, nextCursor, saving])

  const toggleThought = useCallback((thoughtID: string) => {
    setSelectedThoughtIDs((current) => current.includes(thoughtID)
      ? current.filter((id) => id !== thoughtID)
      : [...current, thoughtID])
  }, [])

  const createNotes = useCallback(async () => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('history') || !operationLease.isCurrent('notes')) return
    if (selectedThoughts.length === 0) {
      setError('请至少选择一条想法。')
      return
    }
    setCreating(true)
    setError(null)
    try {
      const firstThought = selectedThoughts[0]
      const result = await client.createNote({
        title: thoughtTitle(firstThought),
        content: selectedThoughts.map(thoughtMarkdown).join('\n\n'),
      })
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('history') || !operationLease.isCurrent('notes')) return
      if (!result.ok) {
        setError(readerErrorMessage(result.error))
        return
      }
      if (result.data.id) {
        setSelectedThoughtIDs([])
        navigateToNote(requestNavigation, result.data.id)
      }
    } catch (requestError) {
      if (client.isIdentityCurrent()) setError(readerErrorMessage(requestError, '笔记创建失败，请稍后重试。'))
    } finally {
      setCreating(false)
    }
  }, [capabilityLease, client, requestNavigation, selectedThoughts])

  const reattach = useCallback(async (thought: ThoughtReadModelItem) => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('history')) return
    const targetRoute = targetKind === 'note'
      ? { kind: 'library', id: 'notes' } as const
      : targetKind === 'inbox'
        ? { kind: 'library', id: 'pending' } as const
        : { kind: 'library', id: 'reading' } as const
    if (!readerRouteIsAvailable(targetRoute, operationLease.policy)) return
    const expectedHostRevision = Number(targetRevision)
    if (!targetID.trim() || !Number.isSafeInteger(expectedHostRevision) || expectedHostRevision <= 0) {
      setError('请输入目标内容当前的正整数版本号。')
      return
    }
    setSaving(true)
    setError(null)
    try {
      const snapshot = frozenHistorySnapshot(thought as ThoughtHistoryItem)
      if (!snapshot) {
        setError('历史想法缺少可验证的服务端版本，无法安全重挂。')
        return
      }
      const committed = await commitThoughtHistoryAction(lease, {
        action: 'reattach',
        opId: `reattach-${crypto.randomUUID()}`,
        thought: snapshot,
        targetHostKind: targetKind,
        targetHostId: targetID.trim(),
        expectedHostRevision,
      })
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
      if (!committed.ok || committed.value.status === 'op-id-conflict') {
        setError('无法保存重挂操作，请稍后重试。')
        return
      }
      emitReaderEvent(READER_EVENTS.thoughtsSyncRequested)
      const synced = await getThoughtSyncController(lease, client).sync()
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
      if (synced.status === 'failed') {
        setError(historyActionFailureMessage('reattach', synced.errorCode))
        return
      }
      if (synced.pushed > 0) {
        await load()
        setItems((current) => current.filter((item) => item.id !== thought.id))
        setSelectedThoughtIDs((current) => current.filter((id) => id !== thought.id))
        setReattachID(null)
        setTargetID('')
        setTargetRevision('')
      }
    } catch (requestError) {
      if (client.isIdentityCurrent()) setError(readerErrorMessage(requestError, '想法重挂失败，请稍后重试。'))
    } finally {
      setSaving(false)
    }
  }, [capabilityLease, client, lease, load, targetID, targetKind, targetRevision])

  const deleteThought = useCallback(async (thought: ThoughtReadModelItem) => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('history') || saving || creating) return
    const currentIndex = visibleItems.findIndex((item) => item.id === thought.id)
    const nextFocusID = visibleItems[currentIndex + 1]?.id ?? visibleItems[currentIndex - 1]?.id
    // 单飞：拿不到 token 说明已有删除在进行，本次直接放弃。
    const token = beginDelete(thought.id)
    if (!token) return
    setError(null)
    let removed = false
    try {
      let snapshot = frozenHistorySnapshot(thought as ThoughtHistoryItem)
      if (!snapshot && view === 'live') {
        const resolved = await readThoughtActionSnapshot(lease, thought.id)
        if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
        if (resolved.ok) snapshot = resolved.value
      }
      if (!snapshot) {
        setError('想法缺少可验证的服务端版本，无法安全删除。')
        return
      }
      const winner = snapshot.winnerKey as { logicalClock?: unknown }
      if (!Number.isSafeInteger(winner.logicalClock) ||
        (winner.logicalClock as number) >= Number.MAX_SAFE_INTEGER) {
        setError('想法逻辑时钟无效，无法安全删除。')
        return
      }
      if (!window.confirm(thoughtDeleteConfirmation(thought))) return
      const committed = await commitThoughtHistoryAction(lease, {
        action: 'delete',
        opId: `delete-${crypto.randomUUID()}`,
        thought: snapshot,
      })
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
      if (!committed.ok || committed.value.status === 'op-id-conflict') {
        setError('无法保存删除操作，请稍后重试。')
        return
      }
      emitReaderEvent(READER_EVENTS.thoughtsSyncRequested)
      const synced = await getThoughtSyncController(lease, client).sync()
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
      if (synced.status === 'failed') {
        setError(historyActionFailureMessage('delete', synced.errorCode))
        return
      }
      if (synced.pushed > 0) {
        await load()
        if (!client.isIdentityCurrent() || !operationLease.isCurrent('history')) return
        setItems((current) => current.filter((item) => item.id !== thought.id))
        setSelectedThoughtIDs((current) => current.filter((id) => id !== thought.id))
        removed = true
      }
    } catch (requestError) {
      if (client.isIdentityCurrent()) {
        setError(readerErrorMessage(requestError, '想法删除失败，请稍后重试。'))
      }
    } finally {
      // 只有仍然持有这次删除的调用才收回 busy 并把焦点还给列表；被 clear()
      // 顶掉的旧 finally 不能抢走新一次删除的焦点落点。
      if (ownsDelete(token)) {
        finishDelete(token)
        restoreDeleteFocus(removed ? nextFocusID : thought.id)
      }
    }
  }, [beginDelete, capabilityLease, client, creating, finishDelete, lease, load, ownsDelete, restoreDeleteFocus, saving, view, visibleItems])

  const recover = useCallback(async (event: ThoughtSupersessionEventRecord) => {
    if (recoveringSequence !== null) return
    setRecoveringSequence(event.eventSequence)
    setError(null)
    try {
      const result = await commitSupersessionRecovery(lease, event)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        setError('无法保存恢复候选。')
        return
      }
      const recovery = result.value as SupersessionRecoveryResult
      if (recovery.status === 'unrecoverable') {
        const message: Record<typeof recovery.reason, string> = {
          'missing-current-winner': '当前版本尚未同步，无法恢复。',
          'host-tombstoned': '当前宿主已 tombstone，请先在历史想法中手动重挂。',
          'current-winner-deleted': '当前版本已删除，无法恢复该内容。',
          'target-or-quote-incomplete': '当前版本缺少完整定位信息，无法恢复。',
        }
        setError(message[recovery.reason])
        return
      }
      if (recovery.status === 'op-id-conflict') {
        setError('恢复候选与本地操作冲突，请刷新后重试。')
        return
      }
      emitReaderEvent(READER_EVENTS.thoughtsSyncRequested)
      if (recovery.status === 'committed') {
        await load()
      }
    } catch (recoveryError) {
      if (client.isIdentityCurrent()) setError(readerErrorMessage(recoveryError, '恢复版本失败，请稍后重试。'))
    } finally {
      setRecoveringSequence(null)
    }
  }, [client, lease, load, recoveringSequence])

  const title = view === 'live' ? '想法' : view === 'history' ? '已归档想法' : '被取代版本'
  const subtitle = view === 'live'
    ? `${visibleItems.length} 条当前想法 · 按更新时间倒序`
    : view === 'history'
      ? '原文或笔记被删除后留下的想法，可以重新挂回到其他内容上'
      : '由服务端不可变 supersession 事件保存的版本'

  return (
    <SurfaceShell
      title={title}
      subtitle={subtitle}
      activeRoute={{ kind: 'tool', id: 'history' }}
      onNavigate={requestNavigation}
      onBeforeNavigate={() => !deletingBusy}
      capabilityPolicy={capabilityPolicy}
      actions={(
        <>
          <NoteWorkspaceTabs active="thoughts" onNavigate={requestNavigation} capabilityPolicy={capabilityPolicy} />
          <button className="rvx-button secondary" type="button" disabled={loading || loadingMore || saving || creating || deletingID !== null || recoveringSequence !== null} onClick={() => void load()}><Icon name="refresh" size={15} />刷新</button>
          {capabilityPolicy.notes && view !== 'superseded' && <button className="rvx-button primary" type="button" disabled={loading || saving || creating || deletingID !== null || selectedThoughts.length === 0} onClick={() => void createNotes()}><Icon name="edit" size={15} />{creating ? '创建中…' : '生成笔记'}</button>}
        </>
      )}
    >
      <div className="rvx-section-head" role="group" aria-label="想法视图">
        <div className="rvx-segmented" role="group" aria-label="想法数据来源">
          <button ref={view === 'live' ? activeViewButtonRef : undefined} type="button" disabled={deletingID !== null} aria-pressed={view === 'live'} className={view === 'live' ? 'active' : ''} onClick={() => changeView('live')}>当前</button>
          <button ref={view === 'history' ? activeViewButtonRef : undefined} type="button" disabled={deletingID !== null} aria-pressed={view === 'history'} className={view === 'history' ? 'active' : ''} onClick={() => changeView('history')}>已归档</button>
          <button ref={view === 'superseded' ? activeViewButtonRef : undefined} type="button" disabled={deletingID !== null} aria-pressed={view === 'superseded'} className={view === 'superseded' ? 'active' : ''} onClick={() => changeView('superseded')}>被取代版本</button>
        </div>
        {view === 'live' && <label className="rvx-field"><span className="rvx-sr-only">筛选想法</span><select aria-label="想法筛选" value={filter} disabled={deletingID !== null} onChange={(event) => setFilter(event.target.value as ThoughtFilter)}>{THOUGHT_FILTERS.filter((option) => option.value !== 'todo' || capabilityPolicy.todos).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>}
      </div>
      {error && <SurfaceError message={error} onRetry={() => void load()} />}
      {loading && (view === 'superseded' ? supersessions.length === 0 : items.length === 0) ? <SurfaceLoading /> : view === 'superseded' ? supersessions.length === 0 ? <div className="rvx-empty"><Icon name="clock" size={24} /><h2>没有被取代版本</h2><p>发生 supersession 后，旧版本会保留在这里。</p></div> : (
        <ul className="rvx-compact-list rvx-history-list">
          {supersessions.map((event) => {
            const availability = recoveryAvailability.get(event.eventSequence) ?? 'missing-current-winner'
            const recoveryState = recoveryStates.get(event.eventSequence) ?? 'not-started'
            const disabled = availability !== 'ready' || recoveryState === 'recovery-conflict' || recoveringSequence !== null
            const reason = recoveryState === 'recovery-conflict'
              ? '恢复候选遇到新的当前版本。请刷新当前想法后重新选择恢复。'
              : recoveryState === 'blocked'
                ? '恢复候选已被阻止，请刷新后检查当前版本。'
              : availability === 'host-tombstoned'
              ? '宿主已 tombstone，请先手动重挂。'
              : availability === 'missing-current-winner'
                ? '等待当前版本同步。'
                : availability === 'current-winner-deleted'
                  ? '当前版本已删除。'
                  : availability === 'target-or-quote-incomplete'
                    ? '当前版本缺少完整定位。'
                    : null
            return <li key={event.eventSequence}>
              <article className="rvx-history-item">
                <div className="rvx-history-heading">
                  <strong>{event.loser.body || '无正文想法'}</strong>
                  <small>事件 {event.eventSequence}</small>
                </div>
                <p className="rvx-muted">来源：{event.loser.hostKind}/{event.loser.hostId}</p>
                {reason && <p className="rvx-muted">{reason}</p>}
                <button className="rvx-button primary" type="button" disabled={disabled} onClick={() => void recover(event)}>{recoveringSequence === event.eventSequence ? '恢复中…' : recoveryState === 'recovery-conflict' ? '需刷新后重新恢复' : '恢复此版本'}</button>
              </article>
            </li>
          })}
        </ul>
      ) : visibleItems.length === 0 ? <div className="rvx-empty"><Icon name={view === 'live' ? 'marker' : 'clock'} size={24} /><h2>{view === 'live' ? '没有符合条件的想法' : '没有已归档的想法'}</h2><p>{view === 'live' ? capabilityPolicy.todos ? '新的高亮、想法和来源 TODO 会出现在这里。' : '新的高亮和想法会出现在这里。' : '原文被删除后，写在上面的想法会留在这里。'}</p></div> : (
        <ul className="rvx-compact-list rvx-history-list">
          {visibleItems.map((thought) => {
            const focused = view === 'history' && focusedThoughtID === thought.id
            return (
            <li key={thought.id}>
              <article className={`rvx-history-item${focused ? ' is-focused' : ''}`} aria-current={focused ? 'true' : undefined} data-thought-id={thought.id}>
                <div className="rvx-history-heading">
                  <label>{capabilityPolicy.notes && <input type="checkbox" checked={selectedThoughtIDs.includes(thought.id)} disabled={saving || creating || deletingID !== null} aria-label={`选择想法 ${thought.id}`} onChange={() => toggleThought(thought.id)} />}<strong>{thought.body || '无正文想法'}</strong></label>
                  <small>{view === 'history' ? `${lifecycleReasonText(thought.lifecycle_reason)} · ` : ''}{formatRelativeDate(thought.updated_at)}</small>
                </div>
                <p className="rvx-muted">出处：{thoughtSource(thought)}{view === 'live' && <>{hasHighlight(thought) ? ' · 高亮' : ''}{thought.body.trim() ? ' · 有想法' : ''}</>}</p>
                {view === 'live' && (() => {
                  const target = readerThoughtHostTarget(thought)
                  return target && readerRouteIsAvailable(target.route, capabilityPolicy)
                    ? <button className="rvx-button secondary" type="button" disabled={deletingID !== null} onClick={() => navigateToHost(requestNavigation, thought)}><Icon name="arrowright" size={14} />打开出处</button>
                    : null
                })()}
                {view === 'history' && <>
                  <p className="rvx-muted">冻结目标：{frozenValueText(thought.target) ?? '无'}</p>
                  <p className="rvx-muted">冻结引用：{frozenValueText(thought.quote) ?? '无'}</p>
                  <p className="rvx-muted">冻结来源：{thought.source || '无'}</p>
                  <p className="rvx-muted">冻结原文：{frozenValueText((thought as ThoughtHistoryItem).original_host_snapshot) ?? '无'}</p>
                  <button className="rvx-link-button" type="button" disabled={creating || deletingID !== null} onClick={() => setReattachID((current) => current === thought.id ? null : thought.id)}>手动重挂</button>
                </>}
                {view === 'history' && reattachID === thought.id && <div className="rvx-history-reattach"><select value={targetKind} onChange={(event) => setTargetKind(event.target.value as typeof targetKind)} aria-label="目标类型"><option value="link">链接</option>{capabilityPolicy.notes && <option value="note">笔记</option>}{capabilityPolicy.inbox && <option value="inbox">收件箱</option>}</select><input value={targetID} onChange={(event) => setTargetID(event.target.value)} placeholder="目标 ID" aria-label="目标 ID" /><input type="number" min="1" step="1" value={targetRevision} onChange={(event) => setTargetRevision(event.target.value)} placeholder="目标版本号" aria-label="目标版本号" /><button className="rvx-button primary" type="button" disabled={saving || creating || deletingID !== null || !targetID.trim() || !targetRevision.trim()} onClick={() => void reattach(thought)}>重挂</button></div>}
                <button
                  ref={(button) => {
                    if (button) deleteButtonRefs.current.set(thought.id, button)
                    else deleteButtonRefs.current.delete(thought.id)
                  }}
                  className="rvx-link-button danger"
                  type="button"
                  disabled={creating || saving || deletingID !== null}
                  aria-busy={deletingID === thought.id ? 'true' : undefined}
                  aria-label={`${deletingID === thought.id ? '正在删除想法' : '删除想法'} ${thought.id}`}
                  title="删除想法"
                  onClick={() => void deleteThought(thought)}
                ><span aria-hidden="true"><Icon name={deletingID === thought.id ? 'loader' : 'trash'} size={14} /></span>{deletingID === thought.id ? '删除中…' : '删除'}</button>
              </article>
            </li>
            )
          })}
        </ul>
      )}
      {nextCursor && <button className="rvx-load-more" type="button" disabled={loadingMore || saving || creating || deletingID !== null} onClick={loadMore}>{loadingMore ? '加载中…' : '更多'}</button>}
    </SurfaceShell>
  )
}
