import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type {
	ReaderInboxBulkResponse,
	ReaderInboxConfirmAIProposalsResponse,
	ReaderInboxListItemResponse,
  ReaderInboxPartition,
  ReaderInboxResponse,
} from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import type { TocHeading } from '../../lib/toc'
import { useReaderToc } from '../../hooks/useReaderToc'
import { useExclusiveAction } from '../../hooks/useExclusiveAction'
import { useSurfaceRequestGate, type SurfaceRequestToken } from '../../hooks/useSurfaceRequestGate'
import { NO_ANNOTATIONS } from '../../lib/annotation-domain'
import { formatRelativeDate, readerErrorMessage } from '../../lib/reader-surface'
import { Icon, type IconName } from '../Icon'
import { PlainTextView } from '../PlainTextView'
import { ArticleOutline } from '../detail/ArticleOutline'
import { ReaderDialog } from '../ui/ReaderDialog'
import { ReaderPreviewCard } from '../ui/ReaderPreviewCard'
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'
import { refreshPendingInboxCount } from './PendingInboxCount'

export interface InboxSurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly onNavigate: (route: ReaderRoute) => void
  readonly onOpenLink: (id: string) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly initialInboxID?: string
  readonly onDraftStateChange?: (state: InboxDraftLeaveState) => void
}

export interface InboxDraftLeaveState {
  readonly dirty: boolean
  readonly saving: boolean
}

const INBOX_EDITOR_OUTLINE: TocHeading[] = [
  { id: 'inbox-overview', level: 1, text: '概览' },
  { id: 'inbox-note', level: 1, text: '笔记' },
  { id: 'inbox-content', level: 1, text: '正文' },
  { id: 'inbox-organization', level: 1, text: '整理' },
]

function ignoreInboxHighlight(): void {}

function parseTags(value: string): string[] {
  return [...new Set(value.split(/[\s,，]+/).map((tag) => tag.trim()).filter(Boolean))]
}

function statusLabel(status: ReaderInboxListItemResponse['status']): string {
	if (status === 'pending') return '待处理'
	return '已入库'
}

const SOURCE_ICONS: Partial<Record<string, IconName>> = {
  browser_capture: 'link',
  extension: 'link',
  rss: 'rss',
  subscription: 'rss',
  manual: 'pencil',
}

const SOURCE_LABELS: Partial<Record<string, string>> = {
  browser_capture: '网页捕获',
  extension: '扩展捕获',
  rss: '订阅',
  subscription: '订阅',
  manual: '手动添加',
}

function sourceIcon(kind: string): IconName {
  return SOURCE_ICONS[kind] ?? 'inbox'
}

function sourceLabel(kind: string): string {
  return SOURCE_LABELS[kind] ?? kind
}

/**
 * The queue rows read like the Reading list: a host, not a snake_cased source
 * enum. The source kind survives as the row icon and its tooltip.
 */
function hostLabel(url: string): string {
  const host = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]+)/i.exec(url)?.[1] ?? url
  return host.replace(/^www\./, '')
}

function otherInboxPartition(partition: ReaderInboxPartition): ReaderInboxPartition {
  return partition === 'active' ? 'expired' : 'active'
}

// The card preview mirrors what the server projects: summary, else note, else
// body, collapsed to one line and bounded. It exists for the cards the client
// builds itself from a write response (create, patch, deep-link detail) so
// those rows do not jump when the next list refresh replaces them.
const INBOX_PREVIEW_LIMIT = 280
const INBOX_PREVIEW_SOURCE_LIMIT = 2048

function inboxPreviewSource(item: ReaderInboxResponse): string {
  // Slice before trimming: body may be megabytes, and only the head can ever
  // reach a bounded preview.
  if (item.summary && /\S/.test(item.summary)) return item.summary.slice(0, INBOX_PREVIEW_SOURCE_LIMIT)
  if (/\S/.test(item.note)) return item.note.slice(0, INBOX_PREVIEW_SOURCE_LIMIT)
  return item.body.slice(0, INBOX_PREVIEW_SOURCE_LIMIT)
}

function inboxPreview(item: ReaderInboxResponse): string {
  const collapsed = inboxPreviewSource(item).replace(/\s+/g, ' ').trim()
  const runes = [...collapsed]
  return runes.length > INBOX_PREVIEW_LIMIT ? runes.slice(0, INBOX_PREVIEW_LIMIT).join('') + '…' : collapsed
}

/** Projects a detail record onto the queue card the list endpoint returns. */
function listItemFromInbox(item: ReaderInboxResponse): ReaderInboxListItemResponse {
  return {
    id: item.id,
    url: item.url,
    source_kind: item.source_kind,
    title: item.title,
    preview: inboxPreview(item),
    tags: item.tags,
    status: item.status,
    metadata_revision: item.metadata_revision,
    expired: item.expired,
    updated_at: item.updated_at,
  }
}

function preferNewerInbox(
  current: ReaderInboxListItemResponse | undefined,
  incoming: ReaderInboxListItemResponse,
): ReaderInboxListItemResponse {
  return current && incoming.metadata_revision < current.metadata_revision ? current : incoming
}

function upsertInboxItem(
  current: ReaderInboxListItemResponse[],
  incoming: ReaderInboxListItemResponse,
  placement: 'append' | 'prepend' | 'ignore' = 'ignore',
): ReaderInboxListItemResponse[] {
  const index = current.findIndex((item) => item.id === incoming.id)
  if (index < 0) {
    if (placement === 'prepend') return [incoming, ...current]
    return placement === 'append' ? [...current, incoming] : current
  }
  const next = preferNewerInbox(current[index], incoming)
  return next === current[index]
    ? current
    : [...current.slice(0, index), next, ...current.slice(index + 1)]
}

function mergeInboxItems(
  current: ReaderInboxListItemResponse[],
  incoming: ReaderInboxListItemResponse[],
  append: boolean,
  preserveIDs?: ReadonlySet<string>,
): ReaderInboxListItemResponse[] {
  if (append) return incoming.reduce((items, item) => upsertInboxItem(items, item, 'append'), current)

  const currentByID = new Map(current.map((item) => [item.id, item]))
  const incomingByID = new Map<string, ReaderInboxListItemResponse>()
  const incomingIDs: string[] = []
  for (const item of incoming) {
    const previous = incomingByID.get(item.id)
    if (!previous) incomingIDs.push(item.id)
    incomingByID.set(item.id, preferNewerInbox(previous, item))
  }
	return [
		...incomingIDs.map((id) => preferNewerInbox(currentByID.get(id), incomingByID.get(id)!)),
		...current.filter((item) =>
			preserveIDs?.has(item.id) && !incomingByID.has(item.id)),
	]
}

interface InboxBulkResult {
  readonly action: 'confirm' | 'discard' | 'confirm-ai'
  readonly response: ReaderInboxBulkResponse | ReaderInboxConfirmAIProposalsResponse
}

interface InboxPartitionPage {
  readonly items: ReaderInboxListItemResponse[]
  readonly nextCursor?: string
  readonly loaded: boolean
  readonly loading: boolean
  readonly loadingMore: boolean
  readonly error: string | null
}

interface InboxCounts {
  readonly activeCount: number
  readonly expiredCount: number
}

function emptyInboxPartitionPage(): InboxPartitionPage {
  return {
    items: [],
    nextCursor: undefined,
    loaded: false,
    loading: false,
    loadingMore: false,
    error: null,
  }
}

interface InboxDraftFields {
  readonly title: string
  readonly body: string
  readonly note: string
  readonly summary: string
  readonly tags: string
}

interface InboxDraftBaseline extends InboxDraftFields {
  readonly revision: number
}

interface InboxDraft extends InboxDraftFields {
  readonly revision: number
  readonly baseline: InboxDraftBaseline
  readonly conflict: boolean
}

/**
 * 请求通道。分页按分区各占一条：切到「已过期」不该顶掉「活跃」的在途请求。
 * `mutation` 是所有写操作共用的身份/目标快照通道——写操作之间由
 * `useExclusiveAction` 单飞互斥，通道本身只回答「这次写还属于当前 owner 吗」。
 * `detail:*` 每个条目各占一条：详情按需取，给另一个条目取详情不该顶掉这一条，
 * 同一条目改了修订再取才顶掉先发的。
 */
type InboxRequestChannel =
  | 'page:active'
  | 'page:expired'
  | 'aggregate'
  | 'target'
  | 'mutation'
  | `detail:${string}`

type InboxRequestToken = SurfaceRequestToken<InboxRequestChannel>

/** 单飞写操作的键；同一时刻只有一个能持有。 */
type InboxActionKey =
  | 'create'
  | 'save'
  | 'confirm'
  | 'discard'
  | 'restore'
  | 'bulk'
  | 'confirm-ai'
  | 'resummarize'

function inboxPageChannel(partition: ReaderInboxPartition): InboxRequestChannel {
  return partition === 'expired' ? 'page:expired' : 'page:active'
}

function inboxDetailChannel(inboxID: string): InboxRequestChannel {
  return `detail:${inboxID}`
}

function draftFieldsFromInbox(item: ReaderInboxResponse): InboxDraftFields {
  return {
    title: item.title ?? '',
    body: item.body,
    note: item.note,
    summary: item.summary ?? '',
    tags: item.tags.join(', '),
  }
}

function draftFromInbox(item: ReaderInboxResponse): InboxDraft {
  const baseline: InboxDraftBaseline = {
    ...draftFieldsFromInbox(item),
    revision: item.metadata_revision,
  }
  return {
    ...baseline,
    revision: item.metadata_revision,
    baseline,
    conflict: false,
  }
}

function draftFieldsDiffer(left: InboxDraftFields, right: InboxDraftFields): boolean {
  return left.title !== right.title ||
    left.body !== right.body ||
    left.note !== right.note ||
    left.summary !== right.summary ||
    left.tags !== right.tags
}

function draftHasLocalChanges(draft: InboxDraft): boolean {
  return draftFieldsDiffer(draft, draft.baseline)
}

function mergeDraftAfterServerRefresh(
  refreshed: ReaderInboxResponse,
  current?: InboxDraft,
  conflict = false,
): InboxDraft {
  if (current && refreshed.metadata_revision < current.baseline.revision) return current
  const remote = draftFromInbox(refreshed)
  if (!current) return remote
  const baseline = current.baseline
  return {
    title: current.title !== baseline.title ? current.title : remote.title,
    body: current.body !== baseline.body ? current.body : remote.body,
    note: current.note !== baseline.note ? current.note : remote.note,
    summary: current.summary !== baseline.summary ? current.summary : remote.summary,
    tags: current.tags !== baseline.tags ? current.tags : remote.tags,
    revision: remote.revision,
    baseline: remote.baseline,
    conflict: conflict || current.conflict,
  }
}

function mergeDraftsAfterServerRefresh(
  current: Record<string, InboxDraft>,
  refreshedItems: ReaderInboxResponse[],
): Record<string, InboxDraft> {
  let next = current
  for (const item of refreshedItems) {
    const draft = current[item.id]
    if (!draft) continue
    if (next === current) next = { ...current }
    next[item.id] = mergeDraftAfterServerRefresh(item, draft)
  }
  return next
}

// Without a local draft there is nothing to preserve, so the refreshed record
// is its own baseline; the conflict flag is what the editor surfaces.
function mergeDraftAfterConflict(
  refreshed: ReaderInboxResponse,
  current?: InboxDraft,
): InboxDraft {
  return mergeDraftAfterServerRefresh(refreshed, current ?? draftFromInbox(refreshed), true)
}

export function InboxSurface({ client, onNavigate, onOpenLink, capabilityPolicy, initialInboxID, onDraftStateChange }: InboxSurfaceProps) {
  const [partition, setPartition] = useState<ReaderInboxPartition>('active')
  const [inboxPages, setInboxPages] = useState<Record<ReaderInboxPartition, InboxPartitionPage>>(() => ({
    active: emptyInboxPartitionPage(),
    expired: emptyInboxPartitionPage(),
  }))
  const [inboxCounts, setInboxCounts] = useState<InboxCounts>({ activeCount: 0, expiredCount: 0 })
  // The list is a card projection, so the editor's record is fetched per item
  // and cached by ID. Keying by ID is also what makes a late detail response
  // unable to land in another item's editor.
  const [details, setDetails] = useState<Record<string, ReaderInboxResponse>>({})
  const [detailReloadToken, setDetailReloadToken] = useState(0)
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [selectedIDs, setSelectedIDs] = useState<Set<string>>(new Set())
  const [drafts, setDrafts] = useState<Record<string, InboxDraft>>({})
  const [createTitle, setCreateTitle] = useState('')
  const [createNote, setCreateNote] = useState('')
  const [createTags, setCreateTags] = useState('')
  const [url, setUrl] = useState('')
  const [createBody, setCreateBody] = useState('')
  const [previewBody, setPreviewBody] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [loadingTarget, setLoadingTarget] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [bulkResult, setBulkResult] = useState<InboxBulkResult | null>(null)
  // 谁在发请求：这块 Surface 的身份 = client + 深链目标。两者任一变化，此前取出
  // 的所有 token 立即失效，旧命名空间的回包画不到新身份上。
  const gate = useSurfaceRequestGate<InboxRequestChannel>({
    owner: [client, initialInboxID],
    authority: () => client.isIdentityCurrent(),
  })
  const inboxAction = useExclusiveAction<InboxActionKey>()
  const {
    busy: saving,
    begin: beginInboxAction,
    finish: finishInboxAction,
    clear: clearInboxAction,
  } = inboxAction
  // 「已应用聚合代次」的下界：迟到的旧聚合不能覆盖已经画上的新计数。
  const aggregateAppliedGenerationRef = useRef(0)
  // 每个列表行「已应用代次」的下界：CAS 重读之后拒绝更旧的列表行。
  const listItemAppliedGenerationRef = useRef(new Map<string, number>())
  const inboxLifecycleGenerationRef = useRef(0)
  // CAS rereads may run in bulk, so each Inbox item needs an independent token.
  const inboxRefreshGenerationsRef = useRef(new Map<string, number>())
  // 每个 Inbox ID 一个代次：一次详情回包只有在仍是该 ID 最新一次请求时才写回。
  // 闸门只知道通道被谁顶掉，记不住这条回包问的是哪个条目——领域语义留在这里。
  const inboxDetailGenerationsRef = useRef(new Map<string, number>())
  // Inbox ID -> the card revision an in-flight detail request was issued for.
  // A card whose revision has not moved does not need a second request.
  const inboxDetailRequestsRef = useRef(new Map<string, number>())
  const detailsRef = useRef(details)
  const inboxOperationGenerationRef = useRef(0)
  const inboxDataClientRef = useRef(client)
  const detailScrollRef = useRef<HTMLDivElement>(null)
  const titleRef = useRef<HTMLTextAreaElement>(null)
  const createUrlRef = useRef<HTMLInputElement>(null)

  const partitionRef = useRef(partition)
  const page = inboxPages[partition]
  const items = page.items
  const nextCursor = page.nextCursor
  const loading = page.loading || !page.loaded
  const loadingMore = page.loadingMore
  const inboxError = page.error

  const updateInboxPage = useCallback((
    target: ReaderInboxPartition,
    update: (current: InboxPartitionPage) => InboxPartitionPage,
  ) => {
    setInboxPages((current) => ({ ...current, [target]: update(current[target]) }))
  }, [])

  const updatePartitionAndEvictOther = useCallback((
    target: ReaderInboxPartition,
    ids: ReadonlySet<string>,
    update: (current: InboxPartitionPage) => InboxPartitionPage,
  ) => {
    const other = otherInboxPartition(target)
    setInboxPages((current) => ({
      ...current,
      [target]: update(current[target]),
      [other]: ids.size === 0
        ? current[other]
        : { ...current[other], items: current[other].items.filter((item) => !ids.has(item.id)) },
    }))
  }, [])

  const adjustInboxCounts = useCallback((activeDelta: number, expiredDelta: number) => {
    setInboxCounts((current) => ({
      activeCount: Math.max(0, current.activeCount + activeDelta),
      expiredCount: Math.max(0, current.expiredCount + expiredDelta),
    }))
  }, [])

  // A detail record only ever replaces an older revision of the same item, so
  // a response that arrives after a newer write is dropped instead of undoing
  // it.
  const cacheInboxDetail = useCallback((incoming: ReaderInboxResponse) => {
    setDetails((current) => {
      const previous = current[incoming.id]
      if (previous && incoming.metadata_revision < previous.metadata_revision) return current
      return { ...current, [incoming.id]: incoming }
    })
  }, [])

  const upsertAuthoritativeInbox = useCallback((
    incoming: ReaderInboxResponse,
    placement: 'append' | 'prepend' | 'ignore' = 'ignore',
  ) => {
    cacheInboxDetail(incoming)
    const target: ReaderInboxPartition = incoming.expired ? 'expired' : 'active'
    const ids = new Set([incoming.id])
    updatePartitionAndEvictOther(target, ids, (current) => ({
      ...current,
      items: upsertInboxItem(current.items, listItemFromInbox(incoming), placement),
    }))
    return target
  }, [cacheInboxDetail, updatePartitionAndEvictOther])

  const selected = useMemo(() => items.find((item) => item.id === selectedID) ?? items[0] ?? null, [items, selectedID])
  // The detail fetch keys on these two primitives rather than the card object:
  // a list refresh replaces the object on every merge, and re-running the
  // effect for an unchanged card would issue a redundant request.
  const selectedCardID = selected?.id ?? null
  const selectedCardRevision = selected?.metadata_revision ?? 0
  const selectedDetail = selected ? details[selected.id] ?? null : null
  const selectedDraft = selectedDetail
    ? drafts[selectedDetail.id] ?? draftFromInbox(selectedDetail)
    : null
  const title = selectedDraft?.title ?? ''
  const note = selectedDraft?.note ?? ''
  const summary = selectedDraft?.summary ?? ''
  const tags = selectedDraft?.tags ?? ''
  const body = selectedDraft?.body ?? ''
  const pendingItems = useMemo(() => items.filter((item) => item.status === 'pending'), [items])
  const selectedPendingItems = useMemo(
    () => pendingItems.filter((item) => selectedIDs.has(item.id)),
    [pendingItems, selectedIDs],
  )
  const selectedPendingCount = selectedPendingItems.length
  const selectedPendingHasEmptyTitle = selectedPendingItems.some((item) =>
    !(drafts[item.id]?.title ?? item.title ?? '').trim())
  const allPendingSelected = pendingItems.length > 0 && selectedPendingCount === pendingItems.length
  const displayError = error ?? inboxError
  const {
    items: editorOutlineItems,
    activeId: activeEditorOutlineID,
    onHeadings: setEditorOutlineHeadings,
    onScroll: syncEditorOutline,
    jumpTo: jumpToEditorSection,
  } = useReaderToc({
    scrollRef: detailScrollRef,
    sourceKey: selectedDetail?.id ?? '',
    layoutKey: previewBody ? 'preview' : 'edit',
    enabled: selectedDetail !== null,
  })

  useLayoutEffect(() => {
    setEditorOutlineHeadings(selectedDetail ? INBOX_EDITOR_OUTLINE : [])
  }, [selectedDetail, setEditorOutlineHeadings])

  // The title is a textarea so long titles wrap the way the reader renders
  // them; it grows to its content instead of scrolling inside one line.
  useLayoutEffect(() => {
    const node = titleRef.current
    if (!node) return
    node.style.height = 'auto'
    node.style.height = `${node.scrollHeight}px`
  }, [title, selected?.id])

  useEffect(() => {
    inboxOperationGenerationRef.current += 1
  }, [selected?.id])

  // 换 client 或换深链目标就是换 owner：在途写操作的 busy 归零（旧 token 从此
  // 不再持有，它的 finally 不会误放行新 owner 的 busy），CAS 重读代次重置。
  useLayoutEffect(() => {
    inboxOperationGenerationRef.current += 1
    inboxRefreshGenerationsRef.current.clear()
    inboxDetailGenerationsRef.current.clear()
    inboxDetailRequestsRef.current.clear()
    clearInboxAction()
  }, [clearInboxAction, client, initialInboxID])

  const advanceInboxLifecycle = useCallback((inboxIDs: readonly string[] = []) => {
    inboxLifecycleGenerationRef.current += 1
    for (const inboxID of inboxIDs) {
      const current = inboxRefreshGenerationsRef.current.get(inboxID)
      if (current !== undefined) inboxRefreshGenerationsRef.current.set(inboxID, current + 1)
    }
  }, [])

  const beginInboxRefresh = useCallback((inboxID: string): number => {
    const next = (inboxRefreshGenerationsRef.current.get(inboxID) ?? 0) + 1
    inboxRefreshGenerationsRef.current.set(inboxID, next)
    return next
  }, [])

  const load = useCallback(async (
    target: ReaderInboxPartition = partitionRef.current,
    append = false,
    cursor?: string,
  ) => {
    const pageToken = gate.begin(inboxPageChannel(target))
    // 聚合与列表行用 token 的全局取号做「已应用代次」的总序判断：分区不同的两次
    // 加载也必须能互相比较先后。
    const aggregateRequestGeneration = gate.begin('aggregate').sequence
    const itemRequestGeneration = pageToken.sequence
    const lifecycleGeneration = inboxLifecycleGenerationRef.current
    const refreshGenerations = new Map(inboxRefreshGenerationsRef.current)
    updateInboxPage(target, (current) => ({
      ...current,
      loading: !append,
      loadingMore: append,
      error: null,
    }))
    setError(null)
    try {
      const inboxResult = await client.listInbox({ partition: target, after: cursor, limit: 50 })
      if (
        !gate.isCurrent(pageToken) ||
        inboxLifecycleGenerationRef.current !== lifecycleGeneration
      ) return
      if (!inboxResult.ok) {
        updateInboxPage(target, (current) => ({
          ...current,
          loaded: true,
          loading: false,
          loadingMore: false,
          error: readerErrorMessage(inboxResult.error),
        }))
      }
      else {
        if (aggregateRequestGeneration > aggregateAppliedGenerationRef.current) {
          aggregateAppliedGenerationRef.current = aggregateRequestGeneration
          setInboxCounts((current) => ({
            activeCount: Number.isFinite(inboxResult.data.active_count)
              ? inboxResult.data.active_count
              : current.activeCount,
            expiredCount: Number.isFinite(inboxResult.data.expired_count)
              ? inboxResult.data.expired_count
              : current.expiredCount,
          }))
        }
        const skippedIDs = new Set(inboxResult.data.items
          .filter((item) =>
            inboxRefreshGenerationsRef.current.get(item.id) !== refreshGenerations.get(item.id) ||
            (listItemAppliedGenerationRef.current.get(item.id) ?? 0) > itemRequestGeneration)
          .map((item) => item.id))
        const mergedItems = inboxResult.data.items.filter((item) => !skippedIDs.has(item.id))
        for (const item of mergedItems) {
          listItemAppliedGenerationRef.current.set(item.id, itemRequestGeneration)
        }
        const mergedIDs = new Set(mergedItems.map((item) => item.id))
        updatePartitionAndEvictOther(target, mergedIDs, (current) => ({
          ...current,
          items: mergeInboxItems(current.items, mergedItems, append, skippedIDs),
          nextCursor: inboxResult.data.next_cursor,
          loaded: true,
          loading: false,
          loadingMore: false,
          error: null,
        }))
        // Cards carry no editable fields, so a list refresh cannot reconcile a
        // draft. It only publishes the newer revision; the detail effect below
        // refetches the open item and merges the local draft against it.
        const requested = initialInboxID ? mergedItems.find((item) => item.id === initialInboxID) : undefined
        if (partitionRef.current === target) {
          setSelectedID((current) => requested?.id ?? current ?? mergedItems[0]?.id ?? null)
        }
      }
    } finally {
      // 释放自己置上的 loading 不看身份：身份刚被撤销时这段代码仍然必须能把
      // 加载态收掉，否则界面永远停在加载中。它仍然检查 owner 与代次。
      if (gate.isSameOwner(pageToken)) {
        updateInboxPage(target, (current) => ({ ...current, loading: false, loadingMore: false }))
      }
    }
  }, [client, gate, initialInboxID, updateInboxPage, updatePartitionAndEvictOther])

  useLayoutEffect(() => {
    const clientChanged = inboxDataClientRef.current !== client
    inboxDataClientRef.current = client
    aggregateAppliedGenerationRef.current = 0
    listItemAppliedGenerationRef.current.clear()
    if (clientChanged) {
      partitionRef.current = 'active'
		setPartition('active')
		setInboxPages({ active: emptyInboxPartitionPage(), expired: emptyInboxPartitionPage() })
		setInboxCounts({ activeCount: 0, expiredCount: 0 })
		// Detail records belong to the identity that fetched them; a replacement
      // client must not read another identity's cached capture. The mirror ref
      // follows the state instead of being cleared here, so the render that
      // still holds the previous list does not read this as a cache miss.
		setDetails({})
		setSelectedID(null)
		setSelectedIDs(new Set())
		setError(null)
      setBulkResult(null)
    } else {
      const target = partitionRef.current
      setInboxPages((current) => ({
        ...current,
        [target]: {
          ...current[target],
          loaded: false,
          loading: false,
          loadingMore: false,
          error: null,
        },
      }))
    }
    // owner 变化本身已经让旧 token 失效；这里再作废一次，让「同一个 owner 下
    // effect 被重跑」（StrictMode、卸载）也不会有在途请求活到下一段生命周期。
    return () => {
      gate.invalidateAll()
    }
  }, [client, gate, initialInboxID])

  useEffect(() => {
    partitionRef.current = partition
  }, [partition])

  useEffect(() => {
    if (!page.loaded && !page.loading) void load(partition)
  }, [load, page.loaded, page.loading, partition])

  useEffect(() => {
    if (!initialInboxID || loading || items.some((item) => item.id === initialInboxID)) {
      setLoadingTarget(false)
      return
    }
    const targetToken = gate.begin('target')
    setLoadingTarget(true)
    void client.getInbox(initialInboxID).then((result) => {
      if (!gate.isCurrent(targetToken)) return
      if (!result.ok) {
        setError(readerErrorMessage(result.error))
        return
      }
      const target: ReaderInboxPartition = result.data.expired ? 'expired' : 'active'
      const ids = new Set([result.data.id])
      advanceInboxLifecycle([result.data.id])
      cacheInboxDetail(result.data)
      updatePartitionAndEvictOther(target, ids, (current) => ({
        ...current,
        items: upsertInboxItem(current.items, listItemFromInbox(result.data), 'prepend'),
      }))
      setDrafts((current) => mergeDraftsAfterServerRefresh(current, [result.data]))
      setPartition(target)
      setSelectedID(result.data.id)
    }).finally(() => {
      if (gate.isSameOwner(targetToken)) setLoadingTarget(false)
    })
    // effect 重跑（含走到上面提前返回那一支）先跑这段 cleanup，在途的目标读取
    // 就此作废——否则条目已经出现在列表里之后，迟到的回包还会把选中项拽回去。
    return () => { gate.invalidate('target') }
  }, [advanceInboxLifecycle, cacheInboxDetail, client, gate, initialInboxID, items, loading, updatePartitionAndEvictOther])

  useEffect(() => {
    const pendingIDs = new Set(pendingItems.map((item) => item.id))
    setSelectedIDs((current) => {
      const next = new Set([...current].filter((id) => pendingIDs.has(id)))
      return next.size === current.size ? current : next
    })
  }, [pendingItems])

  useLayoutEffect(() => {
    detailsRef.current = details
  }, [details])

  useEffect(() => {
    setPreviewBody(false)
  }, [selected?.id])

  // The first card, a clicked card and a deep link all reach the editor the
  // same way: the detail record is fetched on demand for the selected ID.
  //
  // The effect keys on (id, metadata_revision), so a card that the list
  // refreshed to a newer revision refetches, and the same pair never fetches
  // twice. `detailsRef` is read instead of `details` on purpose: depending on
  // the cache would re-run the effect on every write it performs.
  useEffect(() => {
    if (!selectedCardID) return
    const inboxID = selectedCardID
    const cached = detailsRef.current[inboxID]
    if (cached && cached.metadata_revision >= selectedCardRevision) return
    const inFlight = inboxDetailRequestsRef.current.get(inboxID)
    if (inFlight !== undefined && inFlight >= selectedCardRevision) return
    // 每个条目各占一条通道：同一条目的更新修订顶掉它自己的旧请求，而给另一个
    // 条目取详情不该作废这一条——它的回包按 ID 落进缓存，本来就到不了别人的编辑器。
    const detailToken = gate.begin(inboxDetailChannel(inboxID))
    if (!gate.isCurrent(detailToken)) return
    const requestGeneration = (inboxDetailGenerationsRef.current.get(inboxID) ?? 0) + 1
    inboxDetailGenerationsRef.current.set(inboxID, requestGeneration)
    inboxDetailRequestsRef.current.set(inboxID, selectedCardRevision)
    void client.getInbox(inboxID).then((result) => {
      // 三道互相独立的拦截：闸门（挂载、owner、通道代次、身份权威）、这个 ID
      // 自己的代次（闸门不记得回包问的是哪个条目），以及 cacheInboxDetail 里的
      // 修订下界（更旧的修订不能盖掉已经写进去的更新版本）。
      if (
        !gate.isCurrent(detailToken) ||
        inboxDetailGenerationsRef.current.get(inboxID) !== requestGeneration
      ) return
      if (!result.ok) {
        setError(readerErrorMessage(result.error))
        return
      }
      cacheInboxDetail(result.data)
    }).finally(() => {
      // 放掉自己置上的在途标记不看身份，只看 owner 与通道代次；值比较保证换了
      // owner 之后不会误删新 owner 刚置上的同名标记。
      if (
        gate.isSameOwner(detailToken) &&
        inboxDetailRequestsRef.current.get(inboxID) === selectedCardRevision
      ) {
        inboxDetailRequestsRef.current.delete(inboxID)
      }
    })
  }, [cacheInboxDetail, client, detailReloadToken, gate, selectedCardID, selectedCardRevision])

  // Seeding the draft from the detail record is what binds the editor fields.
  // It runs for a cached record too, so switching back to an item restores its
  // unsaved draft without another request.
  useEffect(() => {
    if (!selectedDetail) return
    setDrafts((current) => ({
      ...current,
      [selectedDetail.id]: mergeDraftAfterServerRefresh(selectedDetail, current[selectedDetail.id]),
    }))
  }, [selectedDetail])

  const reloadInbox = useCallback((target: ReaderInboxPartition = partitionRef.current) => {
    setDetailReloadToken((current) => current + 1)
    void load(target)
  }, [load])

  const dirty = Boolean(selectedDraft && draftHasLocalChanges(selectedDraft))

  const updateSelectedDraft = useCallback((update: Partial<Pick<InboxDraft, 'title' | 'body' | 'note' | 'summary' | 'tags'>>) => {
    if (!selectedDetail) return
    setDrafts((current) => ({
      ...current,
      [selectedDetail.id]: { ...(current[selectedDetail.id] ?? draftFromInbox(selectedDetail)), ...update, conflict: false },
    }))
  }, [selectedDetail])

  useLayoutEffect(() => {
    onDraftStateChange?.({ dirty, saving })
  }, [dirty, onDraftStateChange, saving])

  useLayoutEffect(() => () => {
    onDraftStateChange?.({ dirty: false, saving: false })
  }, [onDraftStateChange])

	useEffect(() => {
		const inboxID = selectedDetail?.id
		const proposalStatus = selectedDetail?.proposal_status
		if (!inboxID || (proposalStatus !== 'pending' && proposalStatus !== 'running')) return
		let active = true
		let timer: number | null = null
		const poll = async () => {
			const lifecycleGeneration = inboxLifecycleGenerationRef.current
			const result = await client.getInbox(inboxID)
			if (
				!active ||
				!client.isIdentityCurrent() ||
				inboxLifecycleGenerationRef.current !== lifecycleGeneration
			) return
			if (!result.ok) {
				setError(readerErrorMessage(result.error))
				return
			}
			advanceInboxLifecycle([result.data.id])
			upsertAuthoritativeInbox(result.data)
			setDrafts((current) => mergeDraftsAfterServerRefresh(current, [result.data]))
			if (result.data.proposal_status === 'failed') {
				setError('摘要任务失败，请稍后重试。')
				return
			}
			if (result.data.proposal_status === 'pending' || result.data.proposal_status === 'running') {
				timer = window.setTimeout(() => { void poll() }, 1500)
			}
		}
    void poll()
    return () => {
      active = false
      if (timer !== null) window.clearTimeout(timer)
    }
	}, [advanceInboxLifecycle, client, selectedDetail?.id, selectedDetail?.proposal_status, upsertAuthoritativeInbox])

  // Takes the ID rather than a record: the reread's whole job is to replace
  // whatever the caller was holding, and the caller may only hold a card.
  const refreshConflictRevision = useCallback(async (
    inboxID: string,
    requestToken: InboxRequestToken,
  ): Promise<ReaderInboxResponse | null> => {
    if (!gate.isCurrent(requestToken)) return null
    const requestGeneration = beginInboxRefresh(inboxID)
    const refreshed = await client.getInbox(inboxID)
    if (
      !gate.isCurrent(requestToken) ||
      inboxRefreshGenerationsRef.current.get(inboxID) !== requestGeneration ||
      !refreshed.ok
    ) return null
    advanceInboxLifecycle([inboxID])
    upsertAuthoritativeInbox(refreshed.data)
    setDrafts((current) => ({
      ...current,
      [inboxID]: mergeDraftAfterConflict(refreshed.data, current[inboxID]),
    }))
    return refreshed.data
  }, [advanceInboxLifecycle, beginInboxRefresh, client, gate, upsertAuthoritativeInbox])

  const saveMetadata = useCallback(async (
    requestToken: InboxRequestToken = gate.capture('mutation'),
  ): Promise<ReaderInboxResponse | null> => {
    if (!gate.isCurrent(requestToken)) return null
    const operationGeneration = inboxOperationGenerationRef.current
    const isCurrentSave = () =>
      gate.isCurrent(requestToken) &&
      inboxOperationGenerationRef.current === operationGeneration
    if (!selected || !selectedDetail || selected.status === 'confirmed') return selectedDetail
    const submittedDraft = selectedDraft ?? draftFromInbox(selectedDetail)
    const actionToken = beginInboxAction('save')
    if (!actionToken) return null
    setError(null)
    try {
      const result = await client.patchInbox(selected.id, submittedDraft.revision, {
        title: title.trim(),
        body,
        note,
        summary: summary.trim(),
        tags: parseTags(tags),
      })
      if (!isCurrentSave()) return null
      if (!result.ok) {
        if (result.error.status === 409) {
          setDrafts((current) => current[selected.id]
            ? { ...current, [selected.id]: { ...current[selected.id], conflict: true } }
            : current)
          const refreshed = await refreshConflictRevision(selected.id, requestToken)
          if (!isCurrentSave()) return null
          setError(refreshed
            ? '此条目已在其他位置更新。已保留本地草稿并同步最新版本，请再次保存。'
            : '此条目已在其他位置更新。已保留本地草稿，但暂时无法读取最新版本。')
        } else {
          setError(readerErrorMessage(result.error))
        }
        return null
      }
      advanceInboxLifecycle([selected.id])
      upsertAuthoritativeInbox(result.data)
      setDrafts((current) => {
        const latest = current[selected.id]
        if (latest && result.data.metadata_revision < latest.baseline.revision) return current
        const savedDraft = draftFromInbox(result.data)
        if (!latest || !draftFieldsDiffer(latest, submittedDraft)) {
          return { ...current, [selected.id]: savedDraft }
        }
        return {
          ...current,
          [selected.id]: {
            ...savedDraft,
            title: latest.title !== submittedDraft.title ? latest.title : savedDraft.title,
            body: latest.body !== submittedDraft.body ? latest.body : savedDraft.body,
            note: latest.note !== submittedDraft.note ? latest.note : savedDraft.note,
            summary: latest.summary !== submittedDraft.summary ? latest.summary : savedDraft.summary,
            tags: latest.tags !== submittedDraft.tags ? latest.tags : savedDraft.tags,
          },
        }
      })
      return result.data
    } finally {
      finishInboxAction(actionToken)
    }
  }, [advanceInboxLifecycle, beginInboxAction, body, client, finishInboxAction, gate, note, refreshConflictRevision, selected, selectedDetail, selectedDraft, summary, tags, title, upsertAuthoritativeInbox])

  const selectInbox = useCallback(async (id: string) => {
    if (id === selected?.id) return
    const operationGeneration = ++inboxOperationGenerationRef.current
    if (dirty && !await saveMetadata()) return
    if (inboxOperationGenerationRef.current !== operationGeneration) return
    setSelectedID(id)
  }, [dirty, saveMetadata, selected?.id])

  const createInbox = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    if (!url.trim()) return
    const actionToken = beginInboxAction('create')
    if (!actionToken) return
    setError(null)
    try {
      const result = await client.createInbox({
        url: url.trim(),
        source_kind: 'manual',
        title: createTitle.trim() || null,
        body: createBody,
        note: createNote,
        tags: parseTags(createTags),
      })
      if (!gate.isCurrent(requestToken)) return
      if (!result.ok) setError(readerErrorMessage(result.error))
      else {
        advanceInboxLifecycle([result.data.id])
        if (result.data.status === 'pending') {
          adjustInboxCounts(result.data.expired ? 0 : 1, result.data.expired ? 1 : 0)
        }
        const target = upsertAuthoritativeInbox(result.data, 'prepend')
        setPartition(target)
        setSelectedID(result.data.id)
        setCreateOpen(false)
        setUrl('')
        setCreateTitle('')
        setCreateBody('')
        setCreateNote('')
        setCreateTags('')
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, createBody, createNote, createTags, createTitle, finishInboxAction, gate, load, upsertAuthoritativeInbox, url])

  const confirm = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    if (!selected || selected.status !== 'pending') return
    if (!title.trim()) {
      setError('缺少标题，无法确认入库。')
      return
    }
    const inboxID = selected.id
    const operationGeneration = ++inboxOperationGenerationRef.current
    const isCurrentConfirmation = () =>
      gate.isCurrent(requestToken) &&
      inboxOperationGenerationRef.current === operationGeneration
    const saved = await saveMetadata(requestToken)
    if (!saved || !isCurrentConfirmation()) return
    const actionToken = beginInboxAction('confirm')
    if (!actionToken) return
    setError(null)
    try {
      const result = await client.confirmInbox(inboxID, saved.metadata_revision)
      if (!isCurrentConfirmation()) return
      if (!result.ok) {
        if (result.error.status === 409) {
          setDrafts((current) => current[inboxID]
            ? { ...current, [inboxID]: { ...current[inboxID], conflict: true } }
            : current)
          const refreshed = await refreshConflictRevision(saved.id, requestToken)
          if (!isCurrentConfirmation()) return
          setError(refreshed
            ? '此条目已在其他位置更新。已保留本地草稿并同步最新版本，请再次确认。'
            : '此条目已在其他位置更新。已保留本地草稿，但暂时无法读取最新版本。')
        } else {
          setError(readerErrorMessage(result.error))
        }
      } else {
        const target: ReaderInboxPartition = saved.expired ? 'expired' : 'active'
        const ids = new Set([inboxID])
        advanceInboxLifecycle([inboxID])
        adjustInboxCounts(saved.expired ? 0 : -1, saved.expired ? -1 : 0)
        updatePartitionAndEvictOther(target, ids, (current) => ({
          ...current,
          items: current.items.filter((item) => !ids.has(item.id)),
        }))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(inboxID)
          return next
        })
        setSelectedID((current) => current === inboxID ? null : current)
        setBulkResult(null)
        onOpenLink(result.data.link_id)
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, finishInboxAction, gate, load, onOpenLink, refreshConflictRevision, saveMetadata, selected, title, updatePartitionAndEvictOther])

  const discard = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    if (!selected || selected.status !== 'pending') return
    const actionToken = beginInboxAction('discard')
    if (!actionToken) return
    setError(null)
    try {
      const result = await client.discardInbox(selected.id)
      if (!gate.isCurrent(requestToken)) return
      if (!result.ok) setError(readerErrorMessage(result.error))
      else {
        const ids = new Set([selected.id])
        const target: ReaderInboxPartition = selected.expired ? 'expired' : 'active'
        advanceInboxLifecycle([selected.id])
        adjustInboxCounts(selected.expired ? 0 : -1, selected.expired ? -1 : 0)
		updatePartitionAndEvictOther(target, ids, (current) => ({
			...current,
			items: current.items.filter((item) => !ids.has(item.id)),
		}))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(selected.id)
          return next
        })
		setBulkResult(null)
		setSelectedID((current) => current === selected.id ? null : current)
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, finishInboxAction, gate, load, selected, updatePartitionAndEvictOther])

  const restore = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
	if (!selected || !selected.expired) return
    const actionToken = beginInboxAction('restore')
    if (!actionToken) return
    setError(null)
    try {
      const result = await client.restoreInbox(selected.id)
      if (!gate.isCurrent(requestToken)) return
      if (!result.ok) setError(readerErrorMessage(result.error))
      else {
        const ids = new Set([selected.id])
        advanceInboxLifecycle([selected.id])
        adjustInboxCounts(1, selected.status === 'pending' && selected.expired ? -1 : 0)
        // 恢复把这一项从「已过期」搬走，页面被清空重来：在途的过期分页作废，
        // 否则它回来时会把刚被搬走的行再画回去。
        gate.invalidate('page:expired')
        setInboxPages((current) => ({
          active: {
            ...current.active,
            items: current.active.items.filter((item) => !ids.has(item.id)),
            loaded: false,
            loading: false,
            loadingMore: false,
            error: null,
          },
          expired: {
            ...current.expired,
            items: current.expired.items.filter((item) => !ids.has(item.id)),
            nextCursor: undefined,
            loaded: false,
            loading: false,
            loadingMore: false,
            error: null,
          },
        }))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(selected.id)
          return next
        })
        setSelectedID(null)
        setPartition('active')
        setBulkResult(null)
        refreshPendingInboxCount()
        void load('active')
      }
    } finally {
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, finishInboxAction, gate, load, selected])

  const runBulk = useCallback(async (action: 'confirm' | 'discard') => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    if (action === 'confirm' && selectedPendingHasEmptyTitle) {
      setError('缺少标题，无法批量确认。')
      return
    }
    const ids = selectedPendingItems.map((item) => item.id)
    if (ids.length === 0) return
    let savedSelected: ReaderInboxResponse | null = null
    if (action === 'confirm' && selected && selectedIDs.has(selected.id)) {
      savedSelected = await saveMetadata(requestToken)
      if (!savedSelected) return
    }
    if (!gate.isCurrent(requestToken)) return
    const actionToken = beginInboxAction('bulk')
    if (!actionToken) return
    setError(null)
    setBulkResult(null)
    try {
      const request = action === 'confirm'
        ? {
            inbox_ids: ids,
            expected_revisions: Object.fromEntries(selectedPendingItems.map((item) => [
              item.id,
              item.id === savedSelected?.id
                ? savedSelected.metadata_revision
                : drafts[item.id]?.revision ?? item.metadata_revision,
            ])),
          }
        : { inbox_ids: ids }
      const result = action === 'confirm'
        ? await client.confirmInboxBulk(request)
        : await client.discardInboxBulk(request)
      if (!gate.isCurrent(requestToken)) return
      if (!result.ok) {
        if (action === 'confirm' && result.error.status === 409) {
          await Promise.all(selectedPendingItems.map((item) => refreshConflictRevision(item.id, requestToken)))
          if (!gate.isCurrent(requestToken)) return
        }
        setError(readerErrorMessage(result.error))
      } else {
        const idSet = new Set(ids)
        advanceInboxLifecycle(ids)
        const activeCount = selectedPendingItems.filter((item) => !item.expired).length
        const expiredCount = selectedPendingItems.length - activeCount
        adjustInboxCounts(-activeCount, -expiredCount)
        setBulkResult({ action, response: result.data })
        setSelectedIDs((current) => {
          const next = new Set(current)
          for (const id of ids) next.delete(id)
          return next
        })
        updatePartitionAndEvictOther(partition, idSet, (current) => ({
          ...current,
			items: current.items.filter((item) => !idSet.has(item.id)),
		}))
		setSelectedID((current) => current && idSet.has(current) ? null : current)
        refreshPendingInboxCount()
        void load(partition)
      }
    } finally {
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, drafts, finishInboxAction, gate, load, partition, refreshConflictRevision, saveMetadata, selected, selectedIDs, selectedPendingHasEmptyTitle, selectedPendingItems, updatePartitionAndEvictOther])

  const confirmAIProposals = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    const target = partition
    const confirmedItems: ReaderInboxConfirmAIProposalsResponse['items'] = []
    const actionToken = beginInboxAction('confirm-ai')
    if (!actionToken) return
    let actionError: string | null = null
    setError(null)
    setBulkResult(null)
    try {
      for (;;) {
        const result = await client.confirmAIProposals({ partition: target })
        if (!gate.isCurrent(requestToken)) return
        if (!result.ok) {
          actionError = readerErrorMessage(result.error)
          break
        }
        const confirmedIDs = new Set(result.data.items.map((item) => item.inbox_id))
        if (confirmedIDs.size > 0) {
          advanceInboxLifecycle([...confirmedIDs])
          adjustInboxCounts(
            target === 'active' ? -confirmedIDs.size : 0,
            target === 'expired' ? -confirmedIDs.size : 0,
          )
          updatePartitionAndEvictOther(target, confirmedIDs, (current) => ({
            ...current,
            items: current.items.filter((item) => !confirmedIDs.has(item.id)),
          }))
          setSelectedIDs((current) => {
            const next = new Set(current)
            for (const id of confirmedIDs) next.delete(id)
            return next
          })
          setSelectedID((current) => current && confirmedIDs.has(current) ? null : current)
        }
        confirmedItems.push(...result.data.items)
        if (result.data.remaining_count === 0) {
          setBulkResult({
            action: 'confirm-ai',
            response: { ...result.data, items: confirmedItems, remaining_count: 0 },
          })
          break
        }
        if (result.data.items.length === 0) {
          actionError = 'AI 确认队列未前进，请刷新后重试。'
          break
        }
      }
    } catch (cause) {
      if (gate.isCurrent(requestToken)) {
        actionError = cause instanceof Error ? cause.message : '批量确认失败，请重试。'
      }
    } finally {
      if (gate.isCurrent(requestToken)) {
        await load(target)
        if (gate.isCurrent(requestToken)) {
          refreshPendingInboxCount()
          if (actionError) setError(actionError)
        }
      }
      finishInboxAction(actionToken)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginInboxAction, client, finishInboxAction, gate, load, partition, updatePartitionAndEvictOther])

  const toggleSelection = useCallback((id: string) => {
    setSelectedIDs((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const toggleAllSelection = useCallback(() => {
    setSelectedIDs(() => {
      if (allPendingSelected) return new Set()
      return new Set(pendingItems.map((item) => item.id))
    })
  }, [allPendingSelected, pendingItems])

  const selectPartition = useCallback(async (next: ReaderInboxPartition) => {
    if (next === partition || saving) return
    const operationGeneration = ++inboxOperationGenerationRef.current
    if (dirty && !await saveMetadata()) return
    if (inboxOperationGenerationRef.current !== operationGeneration) return
    setPartition(next)
    setSelectedID(null)
    setSelectedIDs(new Set())
    setError(null)
    setBulkResult(null)
  }, [dirty, partition, saveMetadata, saving])

  const resummarize = useCallback(async () => {
    const requestToken = gate.capture('mutation')
    if (!gate.isCurrent(requestToken)) return
    if (!selected || selected.status !== 'pending') return
    const actionToken = beginInboxAction('resummarize')
    if (!actionToken) return
    setError(null)
    try {
      const result = await client.resummarizeInbox(selected.id)
      if (!gate.isCurrent(requestToken)) return
      if (!result.ok) setError(readerErrorMessage(result.error))
		else {
			advanceInboxLifecycle([selected.id])
			upsertAuthoritativeInbox(result.data)
			setDrafts((current) => mergeDraftsAfterServerRefresh(current, [result.data]))
		}
    } finally {
      finishInboxAction(actionToken)
    }
	}, [advanceInboxLifecycle, beginInboxAction, client, finishInboxAction, gate, selected, upsertAuthoritativeInbox])

  const adoptSuggestedTag = useCallback((tag: string) => {
    updateSelectedDraft({ tags: parseTags([...parseTags(tags), tag].join(', ')).join(', ') })
  }, [tags, updateSelectedDraft])

  return (
    <SurfaceShell
      title="收件箱"
		subtitle={`活跃 ${inboxCounts.activeCount} 项 · 已过期 ${inboxCounts.expiredCount} 项`}
      active="pending"
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      workspaceClassName="rvx-inbox-workspace"
      actions={(
        <button className="rvx-button primary" type="button" onClick={() => setCreateOpen(true)}>
          <Icon name="plus" size={15} />添加条目
        </button>
      )}
    >
      {displayError && (
        <div className="inbox-global-message">
          <SurfaceError message={displayError} onRetry={() => reloadInbox()} />
        </div>
      )}
      {bulkResult && (
        <section className="inbox-result" aria-label="批量操作结果" role="status" aria-live="polite">
          <Icon name="check" size={16} />
          <div>
            <strong>{bulkResult.action === 'confirm-ai'
              ? 'AI 建议确认已完成'
              : `批量${bulkResult.action === 'confirm' ? '确认' : '丢弃'}完成`}</strong>
            <span>本次操作共完成 {bulkResult.response.items.length} 项</span>
          </div>
          <details>
            <summary>查看结果</summary>
            <ul>
            {bulkResult.response.items.map((item, index) => (
              <li key={`${item.inbox_id}-${index}`}>
                <span>{item.inbox_id} · {item.status === 'confirmed' ? '已确认入库' : '已丢弃'}</span>
                {item.link_id && <button className="rvx-link-button" type="button" onClick={() => onOpenLink(item.link_id as string)}>打开已确认链接 {item.link_id}</button>}
              </li>
            ))}
            </ul>
          </details>
        </section>
      )}
      {createOpen && (
        <ReaderDialog
          title="添加条目"
          titleId="inbox-create-title"
          busy={saving}
          initialFocusRef={createUrlRef}
          onClose={() => setCreateOpen(false)}
        >
          <span className="rvx-eyebrow">新收件</span>
          <div className="rvx-form-grid">
            <label>网址<input ref={createUrlRef} value={url} onChange={(event) => setUrl(event.target.value)} placeholder="https://..." type="url" /></label>
            <label>标题<input value={createTitle} onChange={(event) => setCreateTitle(event.target.value)} /></label>
            <label>笔记<textarea value={createNote} onChange={(event) => setCreateNote(event.target.value)} rows={2} /></label>
            <label>标签<input value={createTags} onChange={(event) => setCreateTags(event.target.value)} placeholder="用逗号或空格分隔" /></label>
            <label>原始内容<textarea value={createBody} onChange={(event) => setCreateBody(event.target.value)} rows={4} /></label>
          </div>
          <footer>
            <button className="rvx-button secondary" type="button" disabled={saving} onClick={() => setCreateOpen(false)}>取消</button>
            <button className="rvx-button primary" type="button" disabled={saving || !url.trim()} onClick={() => void createInbox()}><Icon name="plus" size={15} />创建</button>
          </footer>
        </ReaderDialog>
      )}
      <div className="inbox-workbench">
        <aside className="inbox-queue" aria-label="收件箱列表">
          <div className="inbox-queue-head">
            <div className="rvx-segmented" role="tablist" aria-label="收件箱分区">
              <button
                className={partition === 'active' ? 'active' : ''}
                type="button"
                role="tab"
                aria-selected={partition === 'active'}
                aria-label={`活跃 (${inboxCounts.activeCount})`}
                onClick={() => selectPartition('active')}
              >
                活跃 <span>{inboxCounts.activeCount}</span>
              </button>
              <button
                className={partition === 'expired' ? 'active' : ''}
                type="button"
                role="tab"
                aria-selected={partition === 'expired'}
                aria-label={`已过期 (${inboxCounts.expiredCount})`}
                onClick={() => selectPartition('expired')}
              >
                已过期 <span>{inboxCounts.expiredCount}</span>
              </button>
            </div>
            {pendingItems.length > 0 && (
              <label className="inbox-select-all">
                <input type="checkbox" aria-label="全选" checked={allPendingSelected} disabled={saving} onChange={toggleAllSelection} />
                <span>全选</span>
              </label>
            )}
          </div>

          {pendingItems.length > 0 && (
            <section className={'inbox-bulkbar' + (selectedPendingCount > 0 ? ' active' : '')} aria-label="批量操作">
              <span>{selectedPendingCount > 0 ? `已选择 ${selectedPendingCount} 项` : `${pendingItems.length} 项待处理`}</span>
              <div>
                <button type="button" title="确认全部 AI 建议" aria-label="确认全部 AI 建议" disabled={saving} onClick={() => void confirmAIProposals()}><Icon name="sparkles" size={15} /></button>
                <button type="button" title="确认所选" aria-label="确认所选" disabled={saving || selectedPendingCount === 0 || selectedPendingHasEmptyTitle} onClick={() => void runBulk('confirm')}><Icon name="check" size={15} /></button>
                <button className="danger" type="button" title="批量丢弃" aria-label="批量丢弃" disabled={saving || selectedPendingCount === 0} onClick={() => void runBulk('discard')}><Icon name="trash" size={15} /></button>
              </div>
            </section>
          )}

          <div className="inbox-queue-scroll">
            {(loading || loadingTarget) && items.length === 0 ? (
              <SurfaceLoading />
            ) : inboxError && items.length === 0 ? null : items.length === 0 ? (
              <div className="inbox-queue-empty">
                <Icon name="inbox" size={22} />
                <strong>{partition === 'expired' ? '没有已过期内容' : '收件箱是空的'}</strong>
                <span>{partition === 'expired' ? '过期内容会保留在这里。' : '新采集的内容会出现在这里。'}</span>
              </div>
            ) : (
              <ul className="inbox-list">
                {items.map((item) => {
                  const flagged = item.expired || item.status !== 'pending'
                  const rowTags = item.tags.slice(0, 3)
                  const rowTitle = item.title || item.url
                  const rowSelected = item.id === selected?.id
                  const rowPicked = selectedIDs.has(item.id)
                  return (
                    <ReaderPreviewCard
                      key={item.id}
                      as="li"
                      selected={rowSelected}
                      picked={rowPicked}
                      leading={(
                        <label className="inbox-row-check">
                          <input type="checkbox" aria-label={`选择 ${rowTitle}`} checked={rowPicked} disabled={saving || item.status !== 'pending'} onChange={() => toggleSelection(item.id)} />
                        </label>
                      )}
                      source={(
                        <span className="inbox-row-source" title={sourceLabel(item.source_kind)}>
                          <Icon name={sourceIcon(item.source_kind)} size={12} />
                          {hostLabel(item.url)}
                        </span>
                      )}
                      time={<time>{formatRelativeDate(item.updated_at)}</time>}
                      title={rowTitle}
                      summary={item.preview || '暂无内容'}
                      details={(flagged || rowTags.length > 0) ? (
                        <span className="inbox-row-foot">
                          {flagged && (
                            <span className="inbox-row-state">
                              <span className={'inbox-state-dot ' + item.status} />
                              {item.expired ? '已过期' : statusLabel(item.status)}
                            </span>
                          )}
                          {rowTags.map((tag) => <span className="mini-tag" key={tag}>#{tag}</span>)}
                        </span>
                      ) : undefined}
                      openLabel={`打开 ${rowTitle}`}
                      onOpen={() => void selectInbox(item.id)}
                    />
                  )
                })}
              </ul>
            )}
            {nextCursor && <button className="rvx-load-more" type="button" disabled={loadingMore || saving} onClick={() => void load(partition, true, nextCursor)}>{loadingMore ? '加载中…' : '更多'}</button>}
          </div>
        </aside>

        <section className="inbox-detail" aria-label="条目编辑器">
          {selected && selectedDetail ? (
            <>
              <header className="inbox-detail-toolbar">
                <div className="inbox-detail-state">
                  <span className="rvx-source-chip">
                    <Icon name={sourceIcon(selected.source_kind)} size={12} />
                    {sourceLabel(selected.source_kind)}
                  </span>
                  <span className="rvx-status-chip">{selected.expired ? '已过期' : statusLabel(selected.status)}</span>
                  {dirty && <span className="inbox-unsaved">未保存</span>}
                </div>
                <div className="inbox-detail-actions">
					{selected.status === 'pending' ? (
						<>
							{selected.expired && <button className="rvx-icon-button" type="button" title="恢复有效期" aria-label="恢复有效期" disabled={saving} onClick={() => void restore()}><Icon name="refresh" size={16} /></button>}
							<button className="rvx-icon-button" type="button" title="重新生成摘要" aria-label="重新生成摘要" disabled={saving || selectedDetail.proposal_status === 'pending' || selectedDetail.proposal_status === 'running'} onClick={() => void resummarize()}><Icon name="sparkles" size={16} /></button>
                      <button className="rvx-icon-button danger" type="button" title="丢弃" aria-label="丢弃" disabled={saving} onClick={() => void discard()}><Icon name="trash" size={16} /></button>
                      <button className="rvx-button primary" type="button" disabled={saving || !title.trim()} onClick={() => void confirm()}><Icon name="check" size={15} />确认入库</button>
                    </>
                  ) : <span className="rvx-muted">该内容已确认入库</span>}
                  {selected.status !== 'confirmed' && <button className="rvx-button secondary" type="button" aria-label="保存元数据" disabled={saving} onClick={() => void saveMetadata()}><Icon name="bookmark" size={14} />保存</button>}
                </div>
              </header>

              <div className="inbox-detail-scroll" ref={detailScrollRef} onScroll={syncEditorOutline}>
                <div className="inbox-reader-layout">
                  <article className="inbox-reader-document">
                    <section id="inbox-overview" className="inbox-editor-section" data-toc-heading tabIndex={-1}>
                      <div className="inbox-source-line">
                        <span>{formatRelativeDate(selectedDetail.created_at)} 收件</span>
                        <span>修订 {selected.metadata_revision}</span>
                        <a href={selected.url} target="_blank" rel="noreferrer" title="打开原网页">
                          <span>{selected.url.replace(/^https?:\/\//, '')}</span>
                          <Icon name="external" size={13} />
                        </a>
                      </div>
                      {/* The visible title is the editor itself; the heading
                          stays for structure and assistive technology. */}
                      <h1 className="sr-only">{title || '未命名条目'}</h1>
                      <label className="inbox-title-field">
                        <span className="sr-only">标题</span>
                        <textarea
                          ref={titleRef}
                          rows={1}
                          value={title}
                          disabled={saving}
                          placeholder="未命名条目"
                          onChange={(event) => updateSelectedDraft({ title: event.target.value })}
                        />
                      </label>
                      {selectedDraft?.conflict && <p className="inbox-conflict" role="status"><Icon name="alert" size={15} />检测到其他位置的更新。本地草稿已保留，并会在下次保存时使用最新版本。</p>}
                      <label className="inbox-summary-card">
                        <span><Icon name="sparkles" size={13} />摘要</span>
                        <textarea value={summary} disabled={saving} onChange={(event) => updateSelectedDraft({ summary: event.target.value })} rows={3} placeholder="暂无摘要" />
                      </label>
					{(selectedDetail.proposal_status === 'pending' || selectedDetail.proposal_status === 'running' || selectedDetail.proposal_status === 'failed') && (
						<p className="inbox-job"><Icon name={selectedDetail.proposal_status === 'failed' ? 'alert' : 'loader'} size={13} />{selectedDetail.proposal_status === 'pending' ? '摘要等待中' : selectedDetail.proposal_status === 'running' ? '正在生成摘要' : '摘要生成失败'}</p>
					)}
                    </section>

                    <section id="inbox-note" className="inbox-editor-section" data-toc-heading tabIndex={-1}>
                      <header className="inbox-section-title"><span><Icon name="edit" size={15} /><h2>笔记</h2></span></header>
                      <label className="inbox-clean-field">
                        <span className="sr-only">笔记</span>
                        <textarea value={note} disabled={saving} onChange={(event) => updateSelectedDraft({ note: event.target.value })} rows={5} placeholder="记录为什么要留下它" />
                      </label>
                    </section>

                    <section id="inbox-content" className="inbox-editor-section" data-toc-heading tabIndex={-1}>
                      <header className="inbox-section-title">
                        <span><Icon name="doc" size={15} /><h2>正文</h2></span>
                        <div className="rvx-segmented">
                          <button type="button" className={!previewBody ? 'active' : ''} disabled={saving} onClick={() => setPreviewBody(false)} aria-pressed={!previewBody}>编辑</button>
                          <button type="button" className={previewBody ? 'active' : ''} disabled={saving} onClick={() => setPreviewBody(true)} aria-pressed={previewBody}>预览</button>
                        </div>
                      </header>
                      {previewBody ? (
                        <PlainTextView className="inbox-body-preview reader-flow" text={body || '暂无正文'} blockKey="inbox-body" anns={NO_ANNOTATIONS} onClickHL={ignoreInboxHighlight} />
                      ) : (
                        <textarea className="inbox-body-editor" aria-label="正文" value={body} disabled={saving} onChange={(event) => updateSelectedDraft({ body: event.target.value })} rows={10} />
                      )}
                      <details className="rvx-source-details"><summary>查看收件时的原始内容</summary><pre>{selectedDetail.body}</pre></details>
                    </section>

                    <section id="inbox-organization" className="inbox-editor-section" data-toc-heading tabIndex={-1}>
                      <header className="inbox-section-title"><span><Icon name="tag" size={15} /><h2>整理</h2></span></header>
                      <label className="inbox-clean-field">
                        <span>标签</span>
                        <input value={tags} disabled={saving} onChange={(event) => updateSelectedDraft({ tags: event.target.value })} placeholder="用逗号或空格分隔" />
                      </label>
                      {selectedDetail.suggested_tags.some((tag) => !parseTags(tags).includes(tag)) && (
                        <div className="inbox-suggestions">
                          <span><Icon name="sparkles" size={13} />AI 建议</span>
                          <div className="rvx-chip-row">{selectedDetail.suggested_tags.filter((tag) => !parseTags(tags).includes(tag)).map((tag) => <button key={tag} type="button" className="rvx-tag-chip" disabled={saving} onClick={() => adoptSuggestedTag(tag)}>采用 #{tag}</button>)}</div>
                        </div>
                      )}
                    </section>
                  </article>

                  <aside className="inbox-outline" aria-label="条目目录">
                    <ArticleOutline items={editorOutlineItems} activeId={activeEditorOutlineID} onJump={jumpToEditorSection} />
                  </aside>
                </div>
              </div>
            </>
          ) : selected ? (
            <SurfaceLoading />
          ) : (
            <div className="inbox-detail-empty">
              <Icon name="doc" size={25} />
              <strong>{items.length === 0 ? '没有待处理条目' : '选择一项开始整理'}</strong>
            </div>
          )}
        </section>
      </div>
    </SurfaceShell>
  )
}
