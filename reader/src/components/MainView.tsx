/**
 * 三栏主界面外壳，连接配置留在上层 App.tsx。
 *
 * 职责：状态机（theme/view/sel/activeId/chatOpen/cmdkOpen/browse/toast）、
 * titlebar（交通灯 + 同步=真实重拉 + ⌘K 胶囊 + 主题 + AI + 设置齿轮）、
 * 真实数据流（当前列表 + truthful 标签/域名摘要 + 有界已见语料）、
 * Mail 式「详情永不因筛选自动切换」原则、⌘K/⌘J 快捷键、Toast。
 *
 * R3 接线：CommandPalette（⌘K 接后端 q= 关键词搜索）、BrowsePanel（标签 / 域名全集浏览）、
 * 划线笔记全流程（durable annotation document 提升到此处，DetailPane 与 NotePanel 共享同一投影）。
 * R4 接线：ChatSidebar（AI 助手，离线回退）、SubsView（订阅源）、网站转换与原文保存。
 * 右栏互斥：NotePanel 与 ChatSidebar 共用同一条
 * 右栏。问 AI 创建 AI 划线 + 写草稿（chatDraft），ChatSidebar 消费并「采用为笔记」回写。
 */
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { type IconName } from './Icon'
import { Sidebar } from './Sidebar'
import { ListPane } from './ListPane'
import {
  DetailPane,
  type ContentEditState,
  type MetadataEditOutcome,
} from './DetailPane'
import { Toast, type ToastAction } from './Toast'
import { CommandPalette, type CommandItem } from './CommandPalette'
import { BrowsePanel } from './BrowsePanel'
import { NotePanel } from './NotePanel'
import { ChatSidebar, type ChatDraft } from './ChatSidebar'
import { SubsView } from './SubsView'
import { SitesView } from './SitesView'
import { LinkConversionDialog } from './LinkConversionDialog'
import { Titlebar } from './Titlebar'
import { AddLinkDialog } from './AddLinkDialog'
import { ArchiveDownloadDialog } from './ArchiveDownloadDialog'
import { HomeSurface } from './reader-vnext/HomeSurface'
import { clearFeedSessionState, FeedSurface } from './reader-vnext/FeedSurface'
import { InboxSurface } from './reader-vnext/InboxSurface'
import { NotesSurface } from './reader-vnext/NotesSurface'
import { TodoSurface } from './reader-vnext/TodoSurface'
import { SettingsSurface } from './reader-vnext/SettingsSurface'
import { ThoughtHistorySurface } from './reader-vnext/ThoughtHistorySurface'
import { TrashSurface } from './reader-vnext/TrashSurface'
import { PendingInboxCountProvider, refreshPendingInboxCount } from './reader-vnext/PendingInboxCount'
import type { LibraryView } from './LibraryModeNav'
import { useLinks, type Selection, type SmartId } from '../hooks/useLinks'
import { useAnnotatedLinkCount } from '../hooks/useAnnotatedLinks'
import { useTags } from '../hooks/useTags'
import { useDomainSummaries } from '../hooks/useDomainSummaries'
import { invalidateSites } from '../hooks/useSites'
import { useSidebarData } from '../hooks/useSidebarData'
import { invalidateReaderActivity } from '../hooks/useReaderActivity'
import { invalidateReaderRelatedTags } from '../hooks/useReaderRelatedTags'
import { useAppShortcuts } from '../hooks/useAppChrome'
import { translationsKey, useTranslations } from '../hooks/useTranslations'
import { usePrefetch, type PrefetchTarget } from '../hooks/usePrefetch'
import {
  annotationMatchesLocator,
  isContentAnchored,
  type Annotation,
  type AnnotationInput,
  type AnnotationLocator,
  type SelectionInfo,
} from '../lib/annotations'
import {
  useArticleAnnotations,
  type ArticleAnnotationCommandResult,
  type ArticleAnnotationRevisionChange,
  type HistoricalArticleAnnotation,
} from '../hooks/useArticleAnnotations'
import { readAnnotationSnapshot } from '../lib/user-data/annotation-store'
import {
  EMPTY_THOUGHT_SYNC_SNAPSHOT,
  getThoughtSyncController,
  type ThoughtSyncSnapshot,
} from '../lib/user-data/thought-sync'
import { useSavedArticleDocument } from '../hooks/useSavedArticleDocument'
import { usePins } from '../lib/meta'
import { readingFocusStore } from '../lib/reading-surface'
import { applyServiceWorkerUpdate } from '../lib/sw'
import {
  invalidateLibrary,
  invalidateLink,
  invalidateLinkContent,
  invalidateLinkProjection,
} from '../lib/cache/invalidate'
import { loadLinkContent } from '../lib/cache/link-content'
import {
  loadRevisionFloors,
  mergeRevisionFloors,
  noteRevisionFloor,
  revisionFloorStorageKey,
} from '../lib/cache/revision-floor'
import { resourceStore } from '../lib/cache/store'
import { readOwnedStorage, writeOwnedStorage } from '../lib/storage-ownership'
import {
  readerThoughtHostTarget,
  type ReaderRoute,
} from '../lib/navigation/route'
import type { ArchiveV2Selection } from '../lib/api/archive-v2'
import type {
  CapabilitiesResponse,
  ContentEditRequest,
  LinkContentResponse,
  LinkResponse,
  ReaderLinkMetadataRequest,
  TranslationResponse,
} from '../lib/api/types'
import { err, type ApiError, type ApiResult } from '@webtag/api'
import type {
  DocumentCommandContext,
  SavedArticleDocumentController,
  SavedContentSource,
} from '../lib/article/document'
import { isSavedContentTranslationSource } from '../lib/article/document'
import type { SourceBlockId } from '../lib/article/source-block'
import type {
  ReaderAIPort,
  ReaderHomePort,
  ReaderInboxTodosPort,
  ReaderLibrarySitesPort,
  ReaderSessionArchivePort,
  ReaderSubscriptionsFeedPort,
  ReaderThoughtsNotesPort,
} from '../lib/reader-api-ports'
import {
  deriveReaderCapabilityPolicy,
  ReaderCapabilityLease,
  readerCapabilityFingerprint,
} from '../lib/capabilities'
import {
  type OpenLinkOptions,
  useMainViewNavigation,
} from './main-view/navigation-controller'

type Theme = 'light' | 'dark'
const CORPUS_LIMIT = 100
const LIBRARY_SYNC_RESOURCES = ['links', 'tags', 'domains'] as const
const EMPTY_ANNOTATIONS: readonly Annotation[] = []
type LibrarySyncResource = typeof LIBRARY_SYNC_RESOURCES[number]

function libraryReloadFailed(
  outcome: PromiseSettledResult<ApiResult<unknown> | null>,
): boolean {
  if (outcome.status === 'rejected' || outcome.value === null) return true
  return !outcome.value.ok
}

function startLibraryReload<T>(reload: () => Promise<T>): Promise<T> {
  try {
    return reload()
  } catch (thrown) {
    return Promise.reject(thrown)
  }
}

// 两个码同属「翻译来源已变」这一类 CAS 冲突，服务端都会附带权威的
// current_identity，Reader 据此刷新来源后重试。
const TRANSLATION_SOURCE_CONFLICTS = new Set([
  'content_revision_conflict',
  'source_block_conflict',
])

function thoughtSyncOutcome(snapshot: ThoughtSyncSnapshot): string {
  switch (snapshot.phase) {
    case 'offline':
      return snapshot.pendingCount > 0 ? `想法离线，${snapshot.pendingCount} 项待同步` : '想法离线'
    case 'syncing':
      return snapshot.pendingCount > 0 ? `想法同步中，${snapshot.pendingCount} 项待同步` : '想法同步中'
    case 'failed': {
      const blocked = snapshot.blockedCount > 0 ? `，${snapshot.blockedCount} 项被阻塞` : ''
      const code = snapshot.errorCode ? `（${snapshot.errorCode}）` : ''
      return `想法同步失败，${snapshot.pendingCount} 项待同步${blocked}${code}`
    }
    case 'pending':
      return `想法待同步，${snapshot.pendingCount} 项操作`
    case 'synced':
      return '想法已同步'
  }
}

interface SummarySourceIdentity {
  linkId: string
  source: string | null
  hash: string | null
}

type DetailRequestState =
  | {
      readonly id: string
      readonly sequence: number
      readonly status: 'loading'
    }
  | {
      readonly id: string
      readonly sequence: number
      readonly status: 'error'
      readonly error: ApiError
    }

interface OwnedDetailRequest {
  readonly id: string
  readonly sequence: number
  readonly controller: SavedArticleDocumentController
  readonly context: DocumentCommandContext
}

/**
 * A metadata write only returns its new revision, not a complete LinkResponse.
 * Keep that tuple locally until every older list/detail response has had a
 * chance to arrive, otherwise a request started before the write can erase the
 * user's just-saved title, summary, or tags.
 */
interface MetadataProjection {
  readonly metadataRevision: number
  readonly title: string | null
  readonly summary: string | null
  readonly tags: readonly string[]
}

function metadataProjectionFrom(value: Partial<LinkResponse>): MetadataProjection | null {
  const revision = value.metadata_revision
  const title = value.title
  const summary = value.summary
  const tags = value.tags
  if (
    typeof revision !== 'number' ||
    !Number.isSafeInteger(revision) ||
    revision < 1 ||
    (title !== null && typeof title !== 'string') ||
    (summary !== null && typeof summary !== 'string') ||
    !Array.isArray(tags) ||
    !tags.every((tag) => typeof tag === 'string')
  ) {
    return null
  }
  return {
    metadataRevision: revision,
    title,
    summary,
    tags: [...tags],
  }
}

function sameMetadataProjection(
  left: MetadataProjection | undefined,
  right: MetadataProjection,
): boolean {
  return left?.metadataRevision === right.metadataRevision &&
    left.title === right.title &&
    left.summary === right.summary &&
    left.tags.length === right.tags.length &&
    left.tags.every((tag, index) => tag === right.tags[index])
}

function metadataPatchTouchesTuple(patch: Partial<LinkResponse>): boolean {
  return (
    Object.prototype.hasOwnProperty.call(patch, 'metadata_revision') ||
    Object.prototype.hasOwnProperty.call(patch, 'title') ||
    Object.prototype.hasOwnProperty.call(patch, 'summary') ||
    Object.prototype.hasOwnProperty.call(patch, 'tags')
  )
}

function annotationCommandCommitted(
  result: ArticleAnnotationCommandResult,
): result is Extract<ArticleAnnotationCommandResult, { readonly sequence: number }> {
  return result.status === 'committed' || result.status === 'duplicate'
}

function sameDocumentCommandContext(
  left: DocumentCommandContext,
  right: DocumentCommandContext,
): boolean {
  return !left.signal.aborted &&
    !right.signal.aborted &&
    left.generation === right.generation &&
    left.id.namespace === right.id.namespace &&
    left.id.linkId === right.id.linkId &&
    left.id.contentRevision === right.id.contentRevision
}

async function sha256Hex(text: string): Promise<string> {
  if (!globalThis.crypto?.subtle) throw new Error('Web Crypto is unavailable')
  const digest = await globalThis.crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(text),
  )
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

/** 当前/最近已见链接的有界去重缓存；新数据优先，避免长期会话无界增长。 */
function mergeCorpus(
  current: LinkResponse[],
  incoming: LinkResponse[],
): LinkResponse[] {
  if (incoming.length === 0) return current
  const byID = new Map<string, LinkResponse>()
  incoming.forEach((link) => byID.set(link.id, link))
  current.forEach((link) => {
    if (!byID.has(link.id)) byID.set(link.id, link)
  })
  const merged = [...byID.values()].slice(0, CORPUS_LIMIT)
  // 结果与原来逐项同一引用时返回**原数组**。
  //
  // corpus 是 DetailPane / CommandPalette / BrowsePanel 的 prop，而这个函数
  // 每次都新建数组——于是列表每一次校验（哪怕后端一个字节都没变）都会换掉
  // corpus 的引用，把刚加上的那几层 memo 全部击穿。PF3 之后列表校验在数据
  // 未变时已经不再通知，但「同一批链接重新合并一次」的路径仍然存在。
  if (merged.length === current.length && merged.every((link, index) => link === current[index])) {
    return current
  }
  return merged
}

function pagerTitle(link: LinkResponse): string {
  return link.title?.trim() || link.domain?.trim() || link.url
}

/**
 * 按 content_revision 下界抬升一条链接的代次；已经不低于下界时原样返回，
 * 不制造多余的新对象（`active` 会流向已 memo 的 DetailPane）。
 */
function liftContentRevision(
  link: LinkResponse | undefined,
  floor: number | undefined,
): LinkResponse | undefined {
  if (!link || floor === undefined || (link.content_revision ?? 0) >= floor) return link
  return {
    ...link,
    content_revision: floor,
    content: undefined,
    content_document: undefined,
    content_format: undefined,
  }
}

function contentRevisionOrUndefined(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : undefined
}

type MainViewClient = ReaderLibrarySitesPort &
  ReaderSubscriptionsFeedPort &
  ReaderThoughtsNotesPort &
  ReaderInboxTodosPort &
  ReaderHomePort &
  ReaderSessionArchivePort &
  ReaderAIPort

export interface MainViewProps {
  client: MainViewClient
  /** Capabilities are fetched after identity; omitted keeps legacy tests local. */
  capabilities?: CapabilitiesResponse
  /** 打开连接配置编辑（titlebar 齿轮）。 */
  onOpenSettings: () => void
  /** Re-probe runtime capabilities as part of an explicit sync. */
  onRefreshCapabilities?: () => void
}

export function MainView({ client, capabilities, onOpenSettings, onRefreshCapabilities }: MainViewProps) {
  const lease = client.identityLease
	const capabilityPolicy = useMemo(
		() => deriveReaderCapabilityPolicy(capabilities),
		[capabilities],
	)
	const capabilityFingerprint = readerCapabilityFingerprint(capabilityPolicy)
	const capabilityLease = useMemo(
		() => new ReaderCapabilityLease(capabilityPolicy),
		// The fingerprint is the capability generation boundary. A new object
		// carrying the same strict values must not reset every temporary surface.
		// eslint-disable-next-line react-hooks/exhaustive-deps
		[capabilityFingerprint],
	)
	useLayoutEffect(() => {
		capabilityLease.activate()
		return () => capabilityLease.deactivate()
	}, [capabilityLease])
	const [theme, setTheme] = useState<Theme>(() => readOwnedStorage('theme') === 'dark' ? 'dark' : 'light')
  const [toast, setToast] = useState<{ msg: string; icon?: IconName; action?: ToastAction } | null>(null)
  const toastTimer = useRef<number | null>(null)
  useEffect(() => () => {
    if (toastTimer.current !== null) {
      window.clearTimeout(toastTimer.current)
      toastTimer.current = null
    }
  }, [])

  const dismissToast = useCallback(() => {
    if (toastTimer.current !== null) {
      window.clearTimeout(toastTimer.current)
      toastTimer.current = null
    }
    setToast(null)
  }, [])

  // 带动作的提示停留更久：2.6 秒够读完一句话，但不够看到、移动指针并点中一个
  // 按钮——撤销要是点不着，就等于没有撤销。
  const flash = useCallback((msg: string, icon?: IconName, action?: ToastAction) => {
    setToast({ msg, icon, action })
    if (toastTimer.current) window.clearTimeout(toastTimer.current)
    toastTimer.current = window.setTimeout(() => {
      toastTimer.current = null
      setToast(null)
    }, action ? 7000 : 2600)
  }, [])
  const {
    view,
    displayedView,
    siteTargetID,
    noteTargetID,
    inboxTargetID,
    activeId,
    setActiveId,
    mobilePane,
    setMobilePane,
    mobileNavOpen,
    setMobileNavOpen,
    contentEditState,
    navigationRestoreEpoch,
    pendingLinkTarget,
    getContentEditState,
    reportContentEditState,
    confirmDiscardContentEdit,
    confirmDiscardNavigation,
    commitRoute,
    navigateRoute,
    reportNotesDraftDirty,
    reportNotesPendingPersistence,
    reportNotesPrepareToLeave,
    reportInboxDraftState,
  } = useMainViewNavigation({ lease, capabilityPolicy, flash })
	const homeScrollRef = useRef<HTMLDivElement>(null)
	const [todoCompletedExpanded, setTodoCompletedExpanded] = useState(false)
  const [subsSyncRequest, setSubsSyncRequest] = useState(0)
  const [sel, setSel] = useState<Selection>({ type: 'smart', id: 'all', name: '全部链接' })
  const [activeFallback, setActiveFallback] = useState<LinkResponse | null>(null)
  const [activeDetail, setActiveDetail] = useState<LinkResponse | null>(null)
  const [summarySourceIdentity, setSummarySourceIdentity] =
    useState<SummarySourceIdentity | null>(null)
  const [summaryProjectionEpoch, setSummaryProjectionEpoch] = useState(0)
  const [metadataProjectionEpoch, setMetadataProjectionEpoch] = useState(0)
  const [corpus, setCorpus] = useState<LinkResponse[]>([])
  const [chatOpen, setChatOpen] = useState(false)
  const [sidebarCollapsed, setSidebarCollapsed] = useState(() => readOwnedStorage('sidebarCollapsed') === '1')
  const focusMode = useSyncExternalStore(
    readingFocusStore.subscribe,
    readingFocusStore.getSnapshot,
    () => false,
  )
  const [addLinkOpen, setAddLinkOpen] = useState(false)
  const [cmdkOpen, setCmdkOpen] = useState(false)
  const [browse, setBrowse] = useState<'tags' | 'domains' | null>(null)
  const [noteEd, setNoteEd] = useState<AnnotationLocator | null>(null)
  const [historicalNote, setHistoricalNote] = useState<HistoricalArticleAnnotation | null>(null)
  const [chatDraft, setChatDraft] = useState<ChatDraft | null>(null)
  const [librarySyncing, setLibrarySyncing] = useState(false)
  const [librarySyncFailures, setLibrarySyncFailures] = useState<LibrarySyncResource[]>([])
  // 新版本就绪：给一条可点的提示，不静默强制刷新（那会打断正在读文章的人）。
  const [updateReady, setUpdateReady] = useState(false)
  const [savingContent, setSavingContent] = useState<string | null>(null)
  const [archiveDialogOpen, setArchiveDialogOpen] = useState(false)
  const [archiveDownloading, setArchiveDownloading] = useState(false)
  const [convertingLink, setConvertingLink] = useState<LinkResponse | null>(null)
  const [detailRequest, setDetailRequest] = useState<DetailRequestState | null>(null)
  const [pins, togglePin] = usePins()
  const librarySyncInFlight = useRef<Promise<void> | null>(null)
  const librarySyncController = useRef<AbortController | null>(null)
  const draftNonce = useRef(0)
  const detailRequestSeq = useRef(0)
  const ownedDetailRequest = useRef<OwnedDetailRequest | null>(null)
  const flashedDetailError = useRef<number | null>(null)
  const automaticOpenRef = useRef<string | null>(null)
  const canCreateNote = capabilityPolicy.notes
  const createNoteIntent = useRef<Promise<void> | null>(null)
  const [creatingNote, setCreatingNote] = useState(false)
  const thoughtSyncController = useMemo(
    () => (
      capabilityLease.isCurrent('annotations') &&
      typeof client.syncThoughts === 'function' &&
      typeof client.pushThoughtOps === 'function'
        ? getThoughtSyncController(lease, client)
        : null
    ),
    [capabilityLease, client, lease],
  )
  const subscribeThoughtSync = useCallback(
    (listener: () => void) => thoughtSyncController?.subscribe(listener) ?? (() => undefined),
    [thoughtSyncController],
  )
  const getThoughtSyncSnapshot = useCallback(
    () => thoughtSyncController?.getSnapshot() ?? EMPTY_THOUGHT_SYNC_SNAPSHOT,
    [thoughtSyncController],
  )
  const thoughtSyncSnapshot = useSyncExternalStore(
    subscribeThoughtSync,
    getThoughtSyncSnapshot,
    getThoughtSyncSnapshot,
  )

  // 数据流
  const list = useLinks(client, sel)
  useEffect(() => thoughtSyncController?.start(), [thoughtSyncController])
  const { patchLink } = list
  const tagsData = useTags(client)
  const domainData = useDomainSummaries(client)
  const reloadLinks = list.reload
  const reloadTags = tagsData.reload
  const reloadDomains = domainData.reload
  const metadataProjectionRef = useRef(new Map<string, MetadataProjection>())

  const recordMetadataProjection = useCallback((id: string, value: Partial<LinkResponse>) => {
    const next = metadataProjectionFrom(value)
    if (!next) return
    const current = metadataProjectionRef.current.get(id)
    if (current && current.metadataRevision > next.metadataRevision) return
    if (sameMetadataProjection(current, next)) return
    metadataProjectionRef.current.set(id, next)
    setMetadataProjectionEpoch((epoch) => epoch + 1)
  }, [])

  const protectMetadataLink = useCallback((link: LinkResponse): LinkResponse => {
    const projection = metadataProjectionRef.current.get(link.id)
    if (!projection || link.metadata_revision >= projection.metadataRevision) return link
    return {
      ...link,
      title: projection.title,
      summary: projection.summary,
      tags: [...projection.tags],
      metadata_revision: projection.metadataRevision,
    }
  }, [])

  const protectMetadataPatch = useCallback((id: string, patch: Partial<LinkResponse>) => {
    recordMetadataProjection(id, patch)
    const projection = metadataProjectionRef.current.get(id)
    if (!projection || !metadataPatchTouchesTuple(patch)) return patch
    if (
      typeof patch.metadata_revision === 'number' &&
      patch.metadata_revision >= projection.metadataRevision
    ) {
      return patch
    }
    return {
      ...patch,
      title: projection.title,
      summary: projection.summary,
      tags: [...projection.tags],
      metadata_revision: projection.metadataRevision,
    }
  }, [recordMetadataProjection])

  const acceptMetadataLink = useCallback((link: LinkResponse): LinkResponse => {
    recordMetadataProjection(link.id, link)
    return protectMetadataLink(link)
  }, [protectMetadataLink, recordMetadataProjection])

  // The projection map is ref-backed so writes can fence an in-flight response
  // synchronously. Its epoch deliberately re-runs this overlay after a write.
  const protectedListLinks = useMemo(
    () => list.links.map(protectMetadataLink),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [list.links, metadataProjectionEpoch, protectMetadataLink],
  )

  useEffect(() => {
    for (const link of list.links) recordMetadataProjection(link.id, link)
  }, [list.links, recordMetadataProjection])
  const knownLinksRef = useRef<{ list: LinkResponse[]; corpus: LinkResponse[] }>({
    list: [],
    corpus: [],
  })
  knownLinksRef.current = { list: protectedListLinks, corpus }

  useEffect(() => {
    setCorpus((current) => mergeCorpus(current, protectedListLinks))
  }, [protectedListLinks])

  useEffect(() => {
    const onUpdateReady = () => setUpdateReady(true)
    window.addEventListener('webtag:sw-update-ready', onUpdateReady)
    return () => window.removeEventListener('webtag:sw-update-ready', onUpdateReady)
  }, [])

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    writeOwnedStorage('theme', theme)
  }, [theme])

  useEffect(() => {
    const policy = capabilityLease.policy
    setArchiveDownloading(false)
    if (!policy.annotations) {
      setNoteEd(null)
      setHistoricalNote(null)
    }
    if (!policy.ai) {
      setChatOpen(false)
      setChatDraft(null)
    }
    if (!policy.notes) {
      createNoteIntent.current = null
      setCreatingNote(false)
    }
    if (!policy.todos) setTodoCompletedExpanded(false)
    if (!policy.history) {
      setHistoricalNote(null)
    }
    if (!policy.siteRead) {
      setConvertingLink(null)
      invalidateSites()
    } else if (!policy.siteWrite) {
      setConvertingLink(null)
    }
    if (!policy.relatedTags) invalidateReaderRelatedTags()
    if (!policy.activity) invalidateReaderActivity()
    if (!policy.feed) clearFeedSessionState(client)
  }, [capabilityLease, client])

  const createEmptyNote = useCallback((): Promise<void> => {
    if (!canCreateNote || !capabilityLease.isCurrent('notes')) return Promise.resolve()
    if (createNoteIntent.current) return createNoteIntent.current

    const operationLease = capabilityLease
    const intent = (async () => {
      const allowed = await Promise.resolve(confirmDiscardNavigation())
      if (!allowed || !client.isIdentityCurrent() || !operationLease.isCurrent('notes')) return
      const operationID = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`
      const result = await client.createNote({ content: '' }, { idempotencyKey: operationID })
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('notes')) return
      if (!result.ok) {
        flash(result.error.message || '新建笔记失败，请重试。', 'alert')
        return
      }
      commitRoute({ kind: 'library', id: 'notes' }, { noteId: result.data.id })
    })()
    createNoteIntent.current = intent
    setCreatingNote(true)
    void intent.finally(() => {
      if (createNoteIntent.current === intent) {
        createNoteIntent.current = null
        setCreatingNote(false)
      }
    })
    return intent
  }, [canCreateNote, capabilityLease, client, commitRoute, confirmDiscardNavigation, flash])

  const openSettings = useCallback(() => {
    const result = confirmDiscardNavigation()
    if (typeof (result as Promise<boolean>)?.then === 'function') {
      void Promise.resolve(result).then((allowed) => { if (allowed) onOpenSettings() })
    } else if (result) onOpenSettings()
  }, [confirmDiscardNavigation, onOpenSettings])

  // 每条链接的 content_revision 下界。
  //
  // 代次由服务端单调自增（写正文 +1、`convertReadingToSiteSQL` / 转回阅读 +1、
  // 历史迁移置空 +1，全仓无递减路径，schema 里还有 CHECK > 0），所以「本机见过的
  // 最大值」永远是安全的下界。保存 / 重抓的响应是**目前唯一被接进下界的来源**
  // ——不是唯一带代次的响应：转换执行（ConversionExecuteResponse）与
  // `GET /api/links/{id}/content` 也带，只是两处的消费侧都把它扔了：
  // LinkConversionDialog 的 onConverted 签名不带参（转换后整条 active 都清掉了），
  // DetailPane 调 onLoadContent 之后也只读正文、不读代次。
  //
  // 表存在 state 里而不是 ref 里，且每次变化都换一个新 Map。
  //
  // 跨标签页那条路**必须**如此：storage 回调里 `loadRevisionFloors()` 产出新引用，
  // `active` 的 memo 靠它重算。同页写入那条今天其实不靠它——两个调用点后面都紧跟
  // `patchKnownLink`，那次 state 写本身就会触发重渲染，就地改 Map 也能work（实测：
  // 改成就地改，22 条用例全绿）。仍然换新引用，是不想把正确性挂在「调用点后面恰好
  // 跟着一次 state 写」这种调用顺序上——接第三个来源时那是会静默失效的。
  //
  // 为什么是一张按 id 的表，而不是在某个合并点取 max：切走再切回同一篇时，
  // activeDetail 早被另一篇覆盖了，「当前持有的更大值」根本不存在，取 max 无从
  // 取起。而 `useLinks` 在列表数据换引用时会清掉全部乐观补丁，陈旧列表随即把
  // 代次拖回写操作之前。
  //
  // 代次退一步的代价不是短暂显示错乱，而是文档身份错乱：
  // `useArticleAnnotations` 会按错误 revision 读取 durable snapshot，用户还可能把
  // 新正文的 offsets 提交到旧 revision。历史划线不会被删除，但当前文档不得读错代次。
  //
  // 表落盘（见 cache/revision-floor.ts）：列表缓存持久化在 IndexedDB，下界若只在
  // 内存里，窗口期内刷新一次页面就会 hydrate 出旧代次、下界同时蒸发，丢划线的
  // 那条链原样重演。
  //
  // 初值传的是**函数本身**而不是 `loadRevisionFloors()`：后者每次渲染都会求值，
  // 等于每帧读一遍 localStorage 再 JSON.parse 一遍，而只有第一次的结果会被采用。
  const [revisionFloor, setRevisionFloor] = useState<Map<string, number>>(loadRevisionFloors)
  // ⚠ updater 里调 noteRevisionFloor 是**有 IO 的**（读盘合并 + 写盘），严格说
  // 不纯，而 main.tsx 开着 StrictMode，dev 下 updater 会被双调用。之所以无害，
  // 靠的是 noteRevisionFloor 本身幂等：两次调用各自 max 合并，收敛到同一张表，
  // 只是多写一次盘。改动它之前先确认这条幂等性还在。
  const noteContentRevision = useCallback((id: string, revision: number) => {
    setRevisionFloor((current) => {
      const next = new Map(current)
      return noteRevisionFloor(next, id, revision) ? next : current
    })
  }, [])

  // 跟进**其他标签页**写的下界。annotation channel 只发低延迟提示，
  // `useArticleAnnotations` 始终从 durable store 重读权威 snapshot；这条 storage 订阅
  // 只负责让文档 revision 跟进。revision 变化后 hook 会以新 target 重读，
  // 不把 storage/BroadcastChannel payload 当成划线真值。
  //
  // RF5A 还要求正文一起服从这个下界：liftContentRevision 会在抬代次时撤下未绑定
  // revision 的 l.content，DetailPane 的 loadedContent 与落盘缓存都按
  // (linkId, revision) 读取。这样跨页先到达的 revision floor 不会把旧正文冒充成
  // 新一代正文，也不会允许用户在旧文本上写入新代次 offsets。
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      // key 为 null 表示 storage 被整体清空（clear()），也要跟进。
      if (event.key !== null && event.key !== revisionFloorStorageKey()) return
      // 按 max 并进来，不是整表替换：本页可能持有盘上没有的更高值（落盘失败，
      // 或本页那条被另一页挤出上限），替换会让下界下降。
      setRevisionFloor((current) => mergeRevisionFloors(current, loadRevisionFloors()))
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  // Mail 原则：详情 active 仅随 activeId 变；筛选 sel 变化不动 active。
  //
  // Keep the server projection's reported revision separate from the UI floor.
  // The lifted object is useful fallback metadata for rendering and commands,
  // but it is not an authoritative detail for that newer revision.
  const activeProjection = useMemo(
    () =>
      (activeDetail?.id === activeId ? activeDetail : undefined) ??
      protectedListLinks.find((link) => link.id === activeId) ??
      (activeFallback?.id === activeId ? activeFallback : undefined) ??
      corpus.find((link) => link.id === activeId),
    [protectedListLinks, activeFallback, activeDetail, corpus, activeId],
  )
  const activeRevisionFloor = activeId ? revisionFloor.get(activeId) : undefined
  const active = useMemo(
    () => liftContentRevision(activeProjection, activeRevisionFloor),
    [activeProjection, activeRevisionFloor],
  )
  // onConvertToSite 需要「点下去那一刻的当前文章」，但它必须是稳定引用
  // （DetailPane 已 memo）。把 active 走 ref 读，回调就不必把 active 列进依赖。
  const activeRef = useRef<LinkResponse | undefined>(undefined)
  activeRef.current = active
  const activeSummarySourceHash =
    active &&
    summarySourceIdentity?.linkId === active.id &&
    summarySourceIdentity.source === (active.summary ?? null)
      ? summarySourceIdentity.hash
      : null
  const onSummaryBlockText = useCallback(
    (linkId: string, source: string | null, renderedText: string | null) => {
      if (renderedText === null) {
        setSummarySourceIdentity({ linkId, source, hash: null })
        return
      }
      void sha256Hex(renderedText).then(
        (hash) => {
          const current = activeRef.current
          if (current?.id !== linkId || (current.summary ?? null) !== source) return
          setSummarySourceIdentity((previous) =>
            previous?.linkId === linkId &&
            previous.source === source &&
            previous.hash === hash
              ? previous
              : { linkId, source, hash },
          )
        },
        () => {
          const current = activeRef.current
          if (current?.id !== linkId || (current.summary ?? null) !== source) return
          setSummarySourceIdentity({ linkId, source, hash: null })
        },
      )
    },
    [],
  )
  const loadSavedBody = useCallback(
    async (context: DocumentCommandContext) => {
      const res = await loadLinkContent(
        client,
        context.id.linkId,
        context.id.contentRevision,
      )
      if (!res.ok && res.error.kind === 'unauthorized') {
        flash('鉴权失败，请检查连接配置', 'alert')
      }
      return res
    },
    [client, flash],
  )
  const savedArticle = useSavedArticleDocument({
    lease,
    detail: activeProjection,
    revisionFloor: activeRevisionFloor,
    loadBody: loadSavedBody,
  })
  const savedDocument = savedArticle.document
  const captureSavedDocumentContext = savedArticle.captureContext
  const loadSavedDocumentBody = savedArticle.loadBody

  // `openLink` starts before React can install the controller for the newly
  // selected article. Bind the request to that controller once it exists and
  // retain the exact generation context for the terminal error transition.
  // A revision advance or identity change aborts that context, so a late
  // failure cannot replace a newer detail resource with an error.
  useLayoutEffect(() => {
    if (!detailRequest) {
      ownedDetailRequest.current = null
      return
    }

    let owned = ownedDetailRequest.current
    if (
      owned &&
      (owned.id !== detailRequest.id || owned.sequence !== detailRequest.sequence)
    ) {
      ownedDetailRequest.current = null
      owned = null
    }

    const controller = savedArticle.controller
    if (!controller || controller.getSnapshot().id.linkId !== detailRequest.id) return
    // The only normal controller replacement for the same link/request is an
    // identity-lease replacement. Never rebind an old request to that owner.
    if (owned && owned.controller !== controller) return

    if (!owned) {
      const context = controller.captureContext()
      if (!controller.beginDetailLoad(context)) return
      owned = {
        id: detailRequest.id,
        sequence: detailRequest.sequence,
        controller,
        context,
      }
      ownedDetailRequest.current = owned
    }

    if (detailRequest.status === 'error') {
      controller.failDetail(detailRequest.error, owned.context)
    }
  }, [detailRequest, savedArticle.controller])

  const documentDetail =
    savedDocument?.detail.status === 'ready' && savedDocument.detail.data.id === active?.id
      ? savedDocument.detail.data
      : null
  const documentFallback =
    savedDocument && active && savedDocument.id.linkId === active.id
      ? liftContentRevision(active, savedDocument.id.contentRevision)
      : active
  const renderedActive = documentDetail ?? documentFallback
  const aiContentContext = (() => {
    if (!savedDocument || !renderedActive) {
      return renderedActive?.content_document ?? renderedActive?.content ?? renderedActive?.summary ?? null
    }
    if (
      savedDocument.id.linkId === renderedActive.id &&
      savedDocument.body.status === 'ready' &&
      savedDocument.body.revision === savedDocument.id.contentRevision &&
      (renderedActive.content_revision === undefined ||
        renderedActive.content_revision === savedDocument.id.contentRevision)
    ) {
      return savedDocument.body.data.content_document ?? savedDocument.body.data.content
    }
    return renderedActive.content_document ?? renderedActive.content ?? renderedActive.summary ?? null
  })()
  const documentOwnsDetailRequest = Boolean(
    detailRequest && savedDocument?.id.linkId === detailRequest.id,
  )
  const detailLoading = detailRequest?.status === 'loading' && detailRequest.id === activeId
    ? documentOwnsDetailRequest
      ? savedDocument?.detail.status === 'loading'
      : true
    : false

  useEffect(() => {
    if (
      detailRequest?.status !== 'error' ||
      flashedDetailError.current === detailRequest.sequence
    ) {
      return
    }

    const activeCanOwnDocument =
      active?.id === detailRequest.id &&
      contentRevisionOrUndefined(active.content_revision) !== undefined
    let error = detailRequest.error
    if (activeCanOwnDocument) {
      if (
        savedDocument?.id.linkId !== detailRequest.id ||
        savedDocument.detail.status !== 'error'
      ) {
        return
      }
      error = savedDocument.detail.error
    }

    flashedDetailError.current = detailRequest.sequence
    flash('加载链接详情失败：' + error.message, 'alert')
  }, [active, detailRequest, flash, savedDocument])

  activeRef.current = renderedActive
  const summaryBlock = useMemo<SourceBlockId | null>(() => {
    if (!renderedActive?.summary || !activeSummarySourceHash) return null
    return {
      namespace: lease.context.physicalNamespace,
      linkId: renderedActive.id,
      blockKind: 'summary',
      sourceHash: activeSummarySourceHash,
    }
  }, [activeSummarySourceHash, lease.context.physicalNamespace, renderedActive])
  const translationContentRevision = savedDocument?.id.contentRevision ??
    contentRevisionOrUndefined(renderedActive?.content_revision) ??
    null
  const {
	items: translationSnapshot,
	staleItems: staleTranslations,
    currentContentRevision: translationsContentRevision,
    loading: translationSnapshotLoading,
    error: translationsError,
    create: createTranslation,
    reloadAfterSourceChange: reloadTranslationsAfterSourceChange,
  } = useTranslations(client, renderedActive?.id ?? activeId, {
    contentRevision: translationContentRevision,
    summarySourceHash: renderedActive?.summary ? activeSummarySourceHash : undefined,
  })
  const currentSavedContent = useMemo<{
    readonly linkId: string
    readonly revision: number
    readonly source: SavedContentSource
  } | null>(() => {
    const document = savedDocument
    if (!document || document.body.status !== 'ready') return null
    if (
      document.body.revision !== document.id.contentRevision ||
      document.id.contentRevision < 0 ||
      document.body.data.content === undefined
    ) {
      return null
    }
    return {
      linkId: document.id.linkId,
      revision: document.id.contentRevision,
      source: {
        content: document.body.data.content,
        ...(document.body.data.content_document === undefined
          ? {}
          : { 'content-document': document.body.data.content_document }),
      },
    }
  }, [savedDocument])
  const previousSavedContentRef = useRef<{
    readonly linkId: string
    readonly revision: number
    readonly source: SavedContentSource
  } | null>(null)
  const previousSavedContent = previousSavedContentRef.current
  const revisionChange = useMemo<ArticleAnnotationRevisionChange | undefined>(() => {
    if (
      !currentSavedContent ||
      !previousSavedContent ||
      currentSavedContent.linkId !== previousSavedContent.linkId ||
      currentSavedContent.revision <= previousSavedContent.revision ||
      previousSavedContent.revision <= 0 ||
      currentSavedContent.revision <= 0
    ) {
      return undefined
    }
    return {
      previousRevision: previousSavedContent.revision,
      previousSource: previousSavedContent.source,
      currentSource: currentSavedContent.source,
      currentRevision: currentSavedContent.revision,
    }
  }, [currentSavedContent, previousSavedContent])
  const articleAnnotations = useArticleAnnotations(
    lease,
    capabilityPolicy.annotations ? savedDocument?.id ?? null : null,
    capabilityPolicy.annotations ? summaryBlock : null,
    capabilityPolicy.annotations && revisionChange ? { revisionChange } : undefined,
  )
  // This effect deliberately follows useArticleAnnotations: its revision-change
  // effect must observe the previous source before this ref advances.
  useEffect(() => {
    if (!currentSavedContent) return
    const previous = previousSavedContentRef.current
    if (
      !previous ||
      previous.linkId !== currentSavedContent.linkId ||
      currentSavedContent.revision >= previous.revision
    ) {
      previousSavedContentRef.current = currentSavedContent
    }
  }, [currentSavedContent])
  const { add, update, remove } = articleAnnotations
  const annotationSnapshot = capabilityPolicy.annotations
    ? articleAnnotations.anns
    : EMPTY_ANNOTATIONS

  const runSavedAnnotationCommand = useCallback(async (
    operation: (context: DocumentCommandContext) => Promise<ArticleAnnotationCommandResult>,
  ): Promise<ArticleAnnotationCommandResult> => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('annotations')) return { status: 'stale' }
    const controller = savedArticle.controller
    if (!controller) return { status: 'stale' }
    const durable = { value: null as ArticleAnnotationCommandResult | null }
    const outcome = await controller.annotate(async (context) => async () => {
      const result = await operation(context)
      if (!operationLease.isCurrent('annotations')) return { ok: false }
      durable.value = result
      return annotationCommandCommitted(result)
        ? { ok: true, sequence: result.sequence }
        : { ok: false }
    })
    if (
      outcome.status === 'committed' &&
      durable.value &&
      annotationCommandCommitted(durable.value)
    ) {
      return durable.value
    }
    if (
      outcome.status === 'failed' &&
      durable.value &&
      !annotationCommandCommitted(durable.value)
    ) {
      return durable.value
    }
    return outcome.status === 'failed' ? { status: 'failed' } : { status: 'stale' }
  }, [capabilityLease, savedArticle.controller])

  const runSavedTranslationCommand = useCallback(async (
    operation: (context: DocumentCommandContext) => Promise<ApiResult<TranslationResponse>>,
  ): Promise<ApiResult<TranslationResponse>> => {
    const controller = savedArticle.controller
    if (!controller) {
      return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
    }
    const durable = { value: null as ApiResult<TranslationResponse> | null }
    const outcome = await controller.requestTranslation(async (context) => async () => {
      const result = await operation(context)
      durable.value = result
      return { ok: result.ok }
    })
    if (outcome.status === 'committed' && durable.value?.ok) return durable.value
    if (outcome.status === 'failed' && durable.value && !durable.value.ok) {
      return durable.value
    }
    return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
  }, [savedArticle.controller])

  useEffect(() => {
    const controller = savedArticle.controller
    if (!controller) return
    const context = controller.captureContext()
    if (articleAnnotations.loading) {
      controller.beginAnnotationsLoad(context)
      return
    }
    if (articleAnnotations.error) {
      controller.failAnnotations(
        { kind: 'other', message: '无法读取本地划线' },
        context,
      )
      return
    }
    controller.acceptAnnotations(context.id.contentRevision, annotationSnapshot, context)
  }, [
    annotationSnapshot,
    articleAnnotations.error,
    articleAnnotations.loading,
    savedArticle.controller,
    savedDocument?.id.contentRevision,
  ])

  useEffect(() => {
    const controller = savedArticle.controller
    if (!controller) return
    const context = controller.captureContext()
    if (translationSnapshotLoading) {
      controller.beginTranslationsLoad(context)
      return
    }
    if (translationsError) {
      controller.failTranslations(translationsError, context)
      return
    }
    if (translationsContentRevision === null) return
    controller.acceptTranslations({
      current_content_revision: translationsContentRevision,
      current_summary_source_hash: null,
      items: translationSnapshot,
    }, context)
  }, [
    savedArticle.controller,
    savedDocument?.id.contentRevision,
    translationSnapshot,
    translationsContentRevision,
    translationsError,
    translationSnapshotLoading,
  ])

  const anns = useMemo(() => {
    const independent = annotationSnapshot.filter(
      (annotation) => !isContentAnchored(annotation.blockKey),
    )
    const saved = savedDocument?.annotations.status === 'ready'
      ? savedDocument.annotations.data
      : []
    if (independent.length === 0) return [...saved]
    if (saved.length === 0) return independent
    return [...independent, ...saved].sort(
      (left, right) => left.createdAt - right.createdAt || left.id.localeCompare(right.id),
    )
  }, [annotationSnapshot, savedDocument?.annotations])

  const translations = useMemo(() => {
    const independent = translationSnapshot.filter(
      (item) => !isSavedContentTranslationSource(item.scope, item.block_key),
    )
    const saved = savedDocument?.translations.status === 'ready'
      ? savedDocument.translations.data
      : []
    return saved.length === 0 ? independent : [...saved, ...independent]
  }, [savedDocument?.translations, translationSnapshot])
  // `idle` means the document has not accepted an authoritative list yet; it
  // is not itself proof that a request is running. In particular, summary hash
  // verification deliberately disables the hook resource without fabricating
  // an empty response for the saved-content aggregate.
  const translationsLoading = savedDocument
    ? savedDocument.translations.status === 'loading' ||
      (savedDocument.translations.status === 'idle' && translationSnapshotLoading)
    : translationSnapshotLoading

  useEffect(() => {
    const latest = protectedListLinks.find((link) => link.id === activeId)
    if (latest) {
      setActiveFallback(latest)
      setActiveDetail((current) => {
        if (current?.id !== latest.id) return current
        const merged = { ...current, ...latest }
        const advancesContentRevision =
          typeof latest.content_revision === 'number' &&
          latest.content_revision > (current.content_revision ?? 0)
        if (latest.content === undefined) {
          merged.content = advancesContentRevision ? undefined : current.content
        }
        if (latest.content_document === undefined) {
          merged.content_document = advancesContentRevision
            ? undefined
            : current.content_document
        }
        if (latest.content_format === undefined) {
          merged.content_format = advancesContentRevision
            ? undefined
            : current.content_format
        }
        // PF6 起列表如实汇报 has_content 与两项计数（后端落成了列，
        // has_content 还是生成列），因此这里不再需要「把详情端的真值保住、
        // 别被列表刷新冲掉」那段补丁——它当初存在的唯一理由是列表在撒谎。
        //
        // content_revision 同样不在这里护：陈旧列表确实会把它拖回去，但护在这里
        // 只能盖住「当前正开着这一篇」这一种路径，切走再切回就漏。统一由上面的
        // revisionFloor 在 active 汇合点抬升。
        //
        // has_content 也会被同一段窗口拖回旧值。那是可自愈的短暂闪烁，而且
        // **不能**照代次那样保住：revision 是单调身份，has_content 是当前正文
        // 是否存在的权威状态。RF5A 已让 requeue/site-complete 等 clear 路径也推进
        // revision，但若把 true 变成粘性值，服务端返回的正文删除仍会被本地旧值
        // 遮住，DetailPane 也就无法及时撤下旧正文。
        return merged
      })
    }
  }, [protectedListLinks, activeId])

  const patchKnownLink = useCallback(
    (id: string, patch: Partial<LinkResponse>) => {
      const protectedPatch = protectMetadataPatch(id, patch)
      patchLink(id, protectedPatch)
      setActiveFallback((current) =>
        current?.id === id
          ? protectMetadataLink({ ...current, ...protectedPatch })
          : current,
      )
      setActiveDetail((current) =>
        current?.id === id
          ? protectMetadataLink({ ...current, ...protectedPatch })
          : current,
      )
      setCorpus((current) =>
        current.map((link) => (
          link.id === id
            ? protectMetadataLink({ ...link, ...protectedPatch })
            : link
        )),
      )
    },
    [patchLink, protectMetadataLink, protectMetadataPatch],
  )

  const refreshMetadataViews = useCallback((id: string) => {
    invalidateLibrary()
    invalidateLinkProjection(id)
    invalidateReaderRelatedTags()
    invalidateReaderActivity()
    void Promise.allSettled([
      Promise.resolve(list.reload()),
      Promise.resolve(tagsData.reload()),
      Promise.resolve(domainData.reload()),
    ])
  }, [domainData, list, tagsData])

  const onSaveLinkMetadata = useCallback(
    async (
      id: string,
      revision: number,
      request: ReaderLinkMetadataRequest,
    ): Promise<MetadataEditOutcome> => {
      const result = await client.patchLinkMetadata(id, revision, request)
      if (!client.isIdentityCurrent()) {
        return {
          status: 'error',
          error: { kind: 'identity-mismatch', message: 'Reader identity changed' },
        }
      }

      if (result.ok) {
        if (result.data.link_id !== id) {
          return {
            status: 'error',
            error: { kind: 'other', message: '链接信息保存响应与当前链接不一致' },
          }
        }

        const metadataRevision = result.data.metadata_revision
        patchKnownLink(id, {
          title: request.title,
          summary: request.summary,
          tags: [...request.tags],
          metadata_revision: metadataRevision,
        })
        refreshMetadataViews(id)

        // PATCH returns only a revision. Re-read the normalized tuple in the
        // background, but never let a response from before this write lower the
        // local metadata projection.
        void client.getLink(id).then((refreshed) => {
          if (
            !client.isIdentityCurrent() ||
            !refreshed.ok ||
            refreshed.data.id !== id ||
            refreshed.data.metadata_revision < metadataRevision
          ) {
            return
          }
          patchKnownLink(id, {
            title: refreshed.data.title,
            summary: refreshed.data.summary,
            tags: [...refreshed.data.tags],
            metadata_revision: refreshed.data.metadata_revision,
          })
        })

        return { status: 'saved', metadataRevision }
      }

      if (result.error.errorCode !== 'metadata_revision_conflict') {
        return { status: 'error', error: result.error }
      }

      refreshMetadataViews(id)
      const refreshed = await client.getLink(id)
      if (!client.isIdentityCurrent()) {
        return {
          status: 'error',
          error: { kind: 'identity-mismatch', message: 'Reader identity changed' },
        }
      }
      if (!refreshed.ok) {
        return {
          status: 'error',
          error: {
            ...refreshed.error,
            message: `链接信息已变化，但无法读取最新版本：${refreshed.error.message}`,
          },
        }
      }
      if (
        refreshed.data.id !== id ||
        refreshed.data.metadata_revision <= revision
      ) {
        return {
          status: 'error',
          error: {
            kind: 'other',
            status: result.error.status,
            errorCode: result.error.errorCode,
            message: '链接信息已变化，但读取的最新版本无效，请刷新后重试',
          },
        }
      }

      patchKnownLink(id, {
        title: refreshed.data.title,
        summary: refreshed.data.summary,
        tags: [...refreshed.data.tags],
        metadata_revision: refreshed.data.metadata_revision,
      })
      return {
        status: 'conflict',
        metadataRevision: refreshed.data.metadata_revision,
        error: result.error,
      }
    },
    [client, patchKnownLink, refreshMetadataViews],
  )

  useEffect(() => {
    const linkId = savedDocument?.id.linkId
    const documentRevision = savedDocument?.id.contentRevision
    if (
      !linkId ||
      documentRevision === undefined ||
      activeProjection?.id !== linkId ||
      documentRevision <= (activeProjection.content_revision ?? 0)
    ) {
      return
    }

    noteContentRevision(linkId, documentRevision)
    let cancelled = false
    void client.getLink(linkId).then((result) => {
      if (cancelled || !client.isIdentityCurrent() || !result.ok) return
      const responseRevision = contentRevisionOrUndefined(result.data.content_revision)
      if (
        result.data.id !== linkId ||
        responseRevision === undefined ||
        responseRevision < documentRevision
      ) {
        return
      }
      patchKnownLink(linkId, result.data)
    })
    return () => {
      cancelled = true
    }
  }, [
    activeProjection?.content_revision,
    activeProjection?.id,
    client,
    noteContentRevision,
    patchKnownLink,
    savedDocument?.id.contentRevision,
    savedDocument?.id.linkId,
  ])

  // 切换文章时清掉笔记面板与问 AI 草稿，避免跨链接误写。
  useEffect(() => {
    setNoteEd(null)
    setHistoricalNote(null)
    setChatDraft(null)
  }, [activeId])

  // 侧栏摘要：标签/域名 count 来自后端；corpus 只补 recent 元数据。
  const {
    counts,
    tags: tagStatList,
    domains: domainStatList,
    tagsAvailable,
    domainsAvailable,
  } = useSidebarData(
    corpus,
    tagsData.tags,
    domainData.summaries,
    domainData.total,
    {
      links: list.links,
      total: list.authoritativeTotal,
      complete: list.corpusComplete,
    },
  )
  // Durable annotation membership is resolved through current link truth so
  // site, non-done, and deleted rows cannot inflate the Reading count.
  const annotationCount = useAnnotatedLinkCount(client)
  const sidebarCounts = useMemo(() => {
    return {
      ...counts,
      annotated: annotationCount,
    }
  }, [annotationCount, counts])

  const openLink = useCallback(
    (id: string, candidate?: LinkResponse, revealMobileDetail = true, options: OpenLinkOptions = {}) => {
      const normalizedID = id.trim()
      if (!normalizedID) return
      const commitOpen = () => {
        if (!client.isIdentityCurrent() || !capabilityLease.isCurrent()) return false
        const historyMode = options.history ?? 'push'
        const addressLink = options.address ?? historyMode === 'push'
        commitRoute(
          { kind: 'library', id: 'reading' },
          { linkId: normalizedID },
          historyMode,
          addressLink,
        )
        // The route commit owns both the address and the state update. The
        // detail loader below owns the same target, so it must not be replayed
        // by the location-target effect on the next render.
        pendingLinkTarget.current = null
        // Every navigation invalidates an earlier detail request, including the
        // fast path that can render a settled list projection synchronously.
        const requestSeq = ++detailRequestSeq.current
        const knownLinks = knownLinksRef.current
        const candidateLink =
          candidate ??
          knownLinks.list.find((item) => item.id === normalizedID) ??
          knownLinks.corpus.find((item) => item.id === normalizedID)
        const link = candidateLink ? acceptMetadataLink(candidateLink) : undefined
        if (link) {
          setActiveFallback(link)
          setCorpus((current) => mergeCorpus(current, [link]))
        }
        setActiveDetail(null)
        if (revealMobileDetail) setMobilePane('detail')
        setMobileNavOpen(false)

        // PF6：列表如今如实汇报 has_content 与两项计数，LinkResponse 的其余字段
        // 列表投影也都覆盖了。因此**手上已有这条链接的列表数据时，不必再发一次
        // 详情请求**——直接用它渲染，冷启动少一个 API、点击已加载的链接少一个。
        //
        // 仍然发请求的两种情况：这条链接不在当前列表里（从 ⌘K 或站点页跳进来），
        // 或者它还在解析中（pending/processing 的字段会变，值得一次权威读取）。
        const known = link ?? knownLinks.list.find((item) => item.id === normalizedID)
        const settled = known && known.status !== 'pending' && known.status !== 'processing'
        if (known && settled) {
          setActiveDetail(acceptMetadataLink(known))
          if (!known.has_content) {
            setDetailRequest(null)
            return true
          }

          // content_source is intentionally absent from list projections. Hydrate
          // only the detail metadata here; the body endpoint remains deferred until
          // the user expands the saved-original section.
          setDetailRequest({ id: normalizedID, sequence: requestSeq, status: 'loading' })
          void client.getLink(normalizedID).then((res) => {
            if (!client.isIdentityCurrent() || requestSeq !== detailRequestSeq.current) return
            if (res.ok) {
              const accepted = acceptMetadataLink(res.data)
              setDetailRequest(null)
              setActiveDetail(accepted)
              patchKnownLink(normalizedID, accepted)
              return
            }
            setDetailRequest({
              id: normalizedID,
              sequence: requestSeq,
              status: 'error',
              error: res.error,
            })
          })
          return true
        }

        setDetailRequest({ id: normalizedID, sequence: requestSeq, status: 'loading' })
        void client.getLink(normalizedID).then((res) => {
          if (!client.isIdentityCurrent()) return
          if (requestSeq !== detailRequestSeq.current) return
          if (res.ok) {
            const accepted = acceptMetadataLink(res.data)
            setDetailRequest(null)
            setActiveDetail(accepted)
            patchKnownLink(normalizedID, accepted)
            return
          }
          setDetailRequest({
            id: normalizedID,
            sequence: requestSeq,
            status: 'error',
            error: res.error,
          })
        })
        return true
      }

      if (normalizedID === activeId || options.guard === false) return commitOpen()
      const allowed = confirmDiscardNavigation()
      if (typeof (allowed as Promise<boolean>)?.then === 'function') {
        return Promise.resolve(allowed).then((ready) => ready ? commitOpen() : false)
      }
      return allowed ? commitOpen() : false
    },
    [
      acceptMetadataLink,
      activeId,
      capabilityLease,
      client,
      commitRoute,
      confirmDiscardNavigation,
      patchKnownLink,
      pendingLinkTarget,
      setMobileNavOpen,
      setMobilePane,
    ],
  )

  useEffect(() => {
    const target = pendingLinkTarget.current
    if (!target || list.loading) return
    const candidate = protectedListLinks.find((link) => link.id === target)
    openLink(target, candidate, false, { history: 'none', address: true, guard: false })
  }, [pendingLinkTarget, protectedListLinks, list.loading, openLink])

  useEffect(() => {
    if (activeId) {
      automaticOpenRef.current = null
      return
    }
    if (view !== 'reading' || list.loading || protectedListLinks.length === 0) return
    const first = protectedListLinks[0]
    if (automaticOpenRef.current === first.id) return
    automaticOpenRef.current = first.id
    openLink(first.id, first, false, { history: 'none', address: false, guard: false })
  }, [activeId, protectedListLinks, list.loading, openLink, view])

  // 右栏互斥：打开 AI 助手则关笔记面板，反之亦然。
  const toggleChat = useCallback(() => {
    if (!capabilityLease.isCurrent('ai')) return
    setMobileNavOpen(false)
    setChatOpen((o) => {
      if (!o) {
        setNoteEd(null)
        setHistoricalNote(null)
      }
      return !o
    })
  }, [capabilityLease, setMobileNavOpen])

  const openNote = useCallback((annotation: AnnotationLocator) => {
    if (!capabilityLease.isCurrent('annotations')) return
    setMobileNavOpen(false)
    setHistoricalNote(null)
    setNoteEd(annotation)
    setChatOpen(false)
  }, [capabilityLease, setMobileNavOpen])

  const openHistoricalAnnotation = useCallback((annotation: Annotation) => {
    if (!capabilityLease.isCurrent('annotations')) return
    const historical = articleAnnotations.historicalAnnotations.find(
      (item) => item.annotation.id === annotation.id,
    )
    if (!historical) {
      flash('已归档想法已更新，请重新打开正文', 'alert')
      return
    }
    setMobileNavOpen(false)
    setNoteEd(null)
    setHistoricalNote(historical)
    setChatOpen(false)
  }, [articleAnnotations.historicalAnnotations, capabilityLease, flash, setMobileNavOpen])

  const addAnnotation = useCallback(async (
    input: AnnotationInput,
    documentContext: DocumentCommandContext | null,
  ): Promise<AnnotationLocator | null> => {
    if (!capabilityLease.isCurrent('annotations')) return null
    const result = isContentAnchored(input.blockKey)
      ? documentContext
        ? await runSavedAnnotationCommand(async (controllerContext) => {
            if (!sameDocumentCommandContext(documentContext, controllerContext)) {
              return { status: 'stale' }
            }
            return add(input, { documentContext })
          })
        : { status: 'stale' as const }
      : await add(input)
    if (!('annotationId' in result)) return null
    if (isContentAnchored(input.blockKey)) {
      return documentContext
        ? {
            id: result.annotationId,
            blockKey: input.blockKey,
            target: {
              kind: 'saved-content',
              contentRevision: documentContext.id.contentRevision,
            },
          }
        : null
    }
    return input.blockKey === 'summary' && summaryBlock
      ? {
          id: result.annotationId,
          blockKey: input.blockKey,
          target: { kind: 'summary', sourceHash: summaryBlock.sourceHash },
        }
      : null
  }, [add, capabilityLease, runSavedAnnotationCommand, summaryBlock])

  const currentSavedRevision = savedDocument?.id.contentRevision ?? null
  const currentSummaryHash = summaryBlock?.sourceHash ?? null
  const annotationIsCurrent = useCallback((annotation: Annotation): boolean => {
    if (isContentAnchored(annotation.blockKey)) {
      return currentSavedRevision !== null &&
        annotation.sourceContentRevision === currentSavedRevision
    }
    return annotation.blockKey === 'summary' &&
      currentSummaryHash !== null &&
      annotation.sourceSummaryHash === currentSummaryHash
  }, [currentSavedRevision, currentSummaryHash])

  const updateAnnotation = useCallback(async (
    annotation: Annotation,
    patch: Parameters<typeof update>[1],
  ): Promise<boolean> => {
    if (!capabilityLease.isCurrent('annotations')) return false
    if (!annotationIsCurrent(annotation)) return false
    const savedContent = isContentAnchored(annotation.blockKey)
    const result = savedContent
      ? await runSavedAnnotationCommand((context) =>
          update(annotation, patch, { documentContext: context }))
      : await update(annotation, patch)
    return result.status === 'committed' || result.status === 'duplicate'
  }, [annotationIsCurrent, capabilityLease, runSavedAnnotationCommand, update])

  const removeAnnotation = useCallback(async (annotation: Annotation): Promise<boolean> => {
    if (!capabilityLease.isCurrent('annotations')) return false
    if (!annotationIsCurrent(annotation)) return false
    const savedContent = isContentAnchored(annotation.blockKey)
    const result = savedContent
      ? await runSavedAnnotationCommand((context) =>
          remove(annotation, { documentContext: context }))
      : await remove(annotation)
    if (result.status !== 'committed' && result.status !== 'duplicate') {
      flash('删除划线失败，请重试', 'alert')
      return false
    }
    return true
  }, [annotationIsCurrent, capabilityLease, flash, remove, runSavedAnnotationCommand])

  // 问 AI：DetailPane / NotePanel 先创建 AI 划线，这里写草稿并打开右栏（ChatSidebar 消费）。
  const onAskAI = useCallback((annotation: AnnotationLocator, text: string) => {
    if (!capabilityLease.isCurrent('ai') || !capabilityLease.isCurrent('annotations')) return
    draftNonce.current += 1
    setChatDraft({
      annotation,
      text,
      nonce: draftNonce.current,
    })
    setNoteEd(null)
    setMobileNavOpen(false)
    setChatOpen(true)
  }, [capabilityLease, setMobileNavOpen])

  // 同步：等待资料库与 Thought 两个独立子系统落定，部分成功的数据继续保留。
  const onSync = useCallback(() => {
    onRefreshCapabilities?.()
    if (librarySyncInFlight.current) return
    const ownership = lease.captureOwnership('synchronize Reader library')
    if (
      !lease.isOwnershipCurrent(ownership) ||
      !resourceStore.isIdentityActive(lease)
    ) return

    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    ownership.operation.signal.addEventListener('abort', abortForIdentity, { once: true })
    librarySyncController.current = controller
    setLibrarySyncing(true)

    const run = (async () => {
      const [outcomes, thoughtOutcome] = await Promise.all([
        Promise.allSettled([
          startLibraryReload(() => reloadLinks({ signal: controller.signal })),
          startLibraryReload(() => reloadTags({ signal: controller.signal })),
          startLibraryReload(() => reloadDomains({ signal: controller.signal })),
        ]),
        thoughtSyncController
          ? thoughtSyncController.sync().then(
              (result) => ({ ok: true as const, result }),
              () => ({ ok: false as const }),
            )
          : Promise.resolve(null),
      ])
      if (
        controller.signal.aborted ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) return

      const failures = LIBRARY_SYNC_RESOURCES.filter((_, index) =>
        libraryReloadFailed(outcomes[index] as PromiseSettledResult<ApiResult<unknown> | null>),
      )
      setLibrarySyncFailures(failures)

      let libraryMessage = '资料库已同步'
      for (const outcome of outcomes) {
        if (outcome.status === 'rejected' || outcome.value === null) {
          libraryMessage = '资料库同步失败'
          break
        }
        if (!outcome.value.ok) {
          libraryMessage = `资料库同步失败：${outcome.value.error.message}`
          break
        }
      }

      const thoughtSnapshot = thoughtSyncController?.getSnapshot()
      const thoughtStale = thoughtOutcome?.ok === true && thoughtOutcome.result.status === 'stale'
      if (!thoughtSnapshot || thoughtStale) {
        if (failures.length === 0) flash(libraryMessage, 'refresh')
        return
      }
      const thoughtFailed = thoughtOutcome?.ok === false ||
        thoughtOutcome?.result.status === 'failed' || thoughtSnapshot.phase === 'failed'
      flash(
        `${libraryMessage}；${thoughtSyncOutcome(thoughtSnapshot)}`,
        failures.length > 0 || thoughtFailed ? 'alert' : 'refresh',
      )
    })()
    librarySyncInFlight.current = run
    const finish = () => {
      ownership.operation.signal.removeEventListener('abort', abortForIdentity)
      if (librarySyncInFlight.current !== run) return
      librarySyncInFlight.current = null
      librarySyncController.current = null
      if (
        lease.isOwnershipCurrent(ownership) &&
        resourceStore.isIdentityActive(lease)
      ) {
        setLibrarySyncing(false)
      }
    }
    void run.then(finish, finish)
  }, [flash, lease, onRefreshCapabilities, reloadDomains, reloadLinks, reloadTags, thoughtSyncController])

  useEffect(() => () => {
    librarySyncController.current?.abort()
    librarySyncController.current = null
    librarySyncInFlight.current = null
  }, [lease])

  const onDownloadArchive = useCallback(async (selection: ArchiveV2Selection): Promise<boolean> => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('archiveDownload')) return false
    setArchiveDownloading(true)
    let objectURL: string | null = null
    let anchor: HTMLAnchorElement | null = null
    try {
      const result = await client.downloadArchiveV2(selection)
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('archiveDownload')) {
        return false
      }
      if (!result.ok) {
        flash(`归档下载失败：${result.error.message}`, 'alert')
        return false
      }

      // The API client has already verified the original response bytes. Do
      // not create an object URL until that succeeds, and release it as soon
      // as the browser receives the click.
      objectURL = URL.createObjectURL(result.data)
      anchor = document.createElement('a')
      anchor.href = objectURL
      anchor.download = `webtag-archive-v2-${new Date().toISOString().slice(0, 10)}.json`
      document.body.appendChild(anchor)
      anchor.click()
      flash('归档已下载', 'download')
      return true
    } catch (cause) {
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('archiveDownload')) {
        return false
      }
      const message = cause instanceof Error ? cause.message : '下载请求未完成'
      flash(`归档下载失败：${message}`, 'alert')
      return false
    } finally {
      anchor?.remove()
      if (objectURL) URL.revokeObjectURL(objectURL)
      if (client.isIdentityCurrent() && operationLease.isCurrent('archiveDownload')) {
        setArchiveDownloading(false)
      }
    }
  }, [capabilityLease, client, flash])

  useEffect(() => {
    if (capabilityPolicy.archiveDownload) return
    setArchiveDialogOpen(false)
    setArchiveDownloading(false)
  }, [capabilityPolicy.archiveDownload])

  // 保存原文：解析完成后按需抓取网页全文并保存，乐观把 content 合进当前 Link。
  const onSaveContent = useCallback(
    async (id: string) => {
      const documentController = savedArticle.controller
      const documentContext = captureSavedDocumentContext()
      setSavingContent(id)
      const res = await client.saveContent(id)
      if (!client.isIdentityCurrent()) return
      setSavingContent(null)
      if (res.ok) {
        if (documentContext?.id.linkId === id) {
          documentController?.acceptBody(res.data, documentContext)
        }
        patchKnownLink(id, {
          has_content: true,
          content_source: res.data.content_source,
        })
        // 补丁会被 useLinks 在下一次列表换引用时清掉，下界不会。
        noteContentRevision(id, res.data.content_revision)
        invalidateLinkContent(id)
        invalidateLinkProjection(id)
        flash('已保存原文', 'check')
      } else if (res.error.kind === 'rate-limited') {
        const wait = res.error.retryAfterSeconds
        flash(wait ? `操作冷却中，请 ${wait}s 后重试` : '操作冷却中，请稍后重试', 'clock')
      } else {
        flash('保存原文失败：' + res.error.message, 'alert')
      }
    },
    [
      captureSavedDocumentContext,
      client,
      flash,
      noteContentRevision,
      patchKnownLink,
      savedArticle.controller,
    ],
  )

  const onReplaceContent = useCallback(
    async (id: string) => {
      // 复用共享的 isContentAnchored 判据，不手抄 block key 列表。换正文会
      // 递增 content_revision；旧 revision 的正文/译文划线仍保留在 durable store，
      // 但不再应用到新正文。摘要有独立 source hash，不受 saved revision 前进影响。
      const current = activeRef.current
      const projectedContentAnnotations = anns.filter((ann) => isContentAnchored(ann.blockKey))
      let contentAnnotationCount = projectedContentAnnotations.length

      // A save/replace response advances the document generation before the
      // annotation hook has finished reading the matching IDB target. Do not
      // let that small projection gap bypass the destructive-action guard.
      if (contentAnnotationCount === 0 && lease && current?.id === id) {
        const revision = Math.max(
          contentRevisionOrUndefined(current.content_revision) ?? 0,
          savedDocument?.id.linkId === id ? savedDocument.id.contentRevision : 0,
          revisionFloor.get(id) ?? 0,
        )
        if (revision > 0) {
          const snapshot = await readAnnotationSnapshot(lease, id, {
            kind: 'saved-content',
            contentRevision: revision,
          })
          if (snapshot.ok) contentAnnotationCount = snapshot.value.annotations.length
        }
      }
      const hasContentAnnotations = contentAnnotationCount > 0
      const hasUserContent = current?.id === id && current.content_source === 'user'
      if (hasUserContent || hasContentAnnotations) {
        const message = hasUserContent
          ? hasContentAnnotations
            ? `当前原文是人工编辑过的，且有 ${contentAnnotationCount} 条划线。重新抓取会丢弃人工编辑并使当前 revision 的原文与译文划线失效；摘要划线不受影响。继续吗？`
            : '当前原文是人工编辑过的。重新抓取会丢弃你的编辑，用重新抓取的正文覆盖。继续吗？'
          : '重新抓取会替换当前原文。原文与译文上的划线会保留在历史版本中，但不会应用到新原文；摘要划线不受影响。继续吗？'
        if (!window.confirm(message)) return
      }
      const documentController = savedArticle.controller
      const documentContext = captureSavedDocumentContext()
      setSavingContent(id)
      const res = await client.replaceContent(id)
      if (!client.isIdentityCurrent()) return
      setSavingContent(null)
      if (res.ok) {
        if (documentContext?.id.linkId === id) {
          documentController?.acceptBody(res.data, documentContext)
        }
        patchKnownLink(id, {
          has_content: true,
          content_source: res.data.content_source,
        })
        noteContentRevision(id, res.data.content_revision)
        invalidateLinkContent(id)
        invalidateLinkProjection(id)
        // The active content revision changes in the same patch. useTranslations
        // observes that identity transition and owns the single authoritative
        // reload; this call only quarantines the old full translation now.
        await reloadTranslationsAfterSourceChange(id, { reload: false })
        flash('已重新抓取原文', 'check')
      } else if (res.error.kind === 'rate-limited') {
        const wait = res.error.retryAfterSeconds
        flash(wait ? `操作冷却中，请 ${wait}s 后重试` : '操作冷却中，请稍后重试', 'clock')
      } else {
        flash('重新抓取原文失败：' + res.error.message, 'alert')
      }
    },
    [
      anns,
      captureSavedDocumentContext,
      client,
      flash,
      lease,
      noteContentRevision,
      patchKnownLink,
      reloadTranslationsAfterSourceChange,
      revisionFloor,
      savedArticle.controller,
      savedDocument?.id,
    ],
  )

  const onSaveContentEdit = useCallback(
    async (
      id: string,
      request: ContentEditRequest,
    ): Promise<ApiResult<LinkContentResponse>> => {
      const current = getContentEditState()
      if (!current || current.linkId !== id || !current.editing) {
        return err({ kind: 'identity-mismatch', message: '当前编辑会话已经结束' })
      }
      if (current.saving) {
        return err({ kind: 'other', message: '正文正在保存' })
      }
      const controller = savedArticle.controller
      const context = captureSavedDocumentContext()
      const savingState: ContentEditState = { ...current, saving: true }
      reportContentEditState(savingState)

      const result = await client.editContent(id, request)
      if (!client.isIdentityCurrent()) {
        return err({ kind: 'identity-mismatch', message: 'Reader identity changed' })
      }

      if (!result.ok) {
        reportContentEditState({ ...savingState, saving: false })
        return result
      }

      // The request may have crossed another local resource transition. Use the
      // current controller context when possible, while still accepting the
      // canonical response only for this link and a non-older revision.
      const currentContext = captureSavedDocumentContext()
      if (controller && currentContext?.id.linkId === id) {
        controller.acceptBody(result.data, currentContext)
      } else if (controller && context?.id.linkId === id) {
        controller.acceptBody(result.data, context)
      }
      patchKnownLink(id, {
        has_content: true,
        content_source: result.data.content_source,
        content_revision: result.data.content_revision,
      })
      noteContentRevision(id, result.data.content_revision)
      invalidateLinkContent(id)
      invalidateLinkProjection(id)
      await reloadTranslationsAfterSourceChange(id, { reload: false })
      reportContentEditState(null)
      return result
    },
    [
      captureSavedDocumentContext,
      client,
      getContentEditState,
      noteContentRevision,
      patchKnownLink,
      reloadTranslationsAfterSourceChange,
      reportContentEditState,
      savedArticle.controller,
    ],
  )

  const flashTranslationError = useCallback(
    (message: string, kind: string, retryAfterSeconds?: number) => {
      if (kind === 'rate-limited') {
        flash(
          retryAfterSeconds
            ? `翻译请求过多，请 ${retryAfterSeconds}s 后重试`
            : '翻译请求过多，请稍后重试',
          'clock',
        )
        return
      }
      if (kind === 'unauthorized') {
        flash('鉴权失败，请检查连接配置', 'alert')
        return
      }
      flash('翻译失败：' + message, 'alert')
    },
    [flash],
  )

  const refreshAfterTranslationConflict = useCallback(
    async (requestedLinkId: string, error: ApiError): Promise<boolean> => {
      if (
        error.status !== 409 ||
        !error.errorCode ||
        !TRANSLATION_SOURCE_CONFLICTS.has(error.errorCode)
      ) {
        return false
      }

      const reportedRevision = contentRevisionOrUndefined(
        error.currentIdentity?.content_revision,
      )
      const blockKey = error.currentIdentity?.block_key
      const refreshSavedContent =
        error.errorCode === 'content_revision_conflict' ||
        blockKey === 'content' ||
        blockKey === 'content-document'
      if (!refreshSavedContent) {
        const current = activeRef.current
        setSummarySourceIdentity({
          linkId: requestedLinkId,
          source: current?.id === requestedLinkId ? (current.summary ?? null) : null,
          hash: null,
        })
        // The canonical text may be unchanged, or getLink may fail. Force the
        // already-mounted DOM projection to report again instead of relying on
        // a source/text mutation to invalidate DetailPane's deduplication key.
        setSummaryProjectionEpoch((epoch) => epoch + 1)
      }
      if (refreshSavedContent) {
        invalidateLinkContent(requestedLinkId)
        invalidateLinkProjection(requestedLinkId)
      } else {
        invalidateLink(requestedLinkId)
      }
      const [linkResult, contentResult] = await Promise.all([
        client.getLink(requestedLinkId),
        refreshSavedContent ? client.getContent(requestedLinkId) : Promise.resolve(null),
      ])
      if (!client.isIdentityCurrent()) return true

      if (refreshSavedContent) {
        const linkRevision = linkResult.ok
          ? contentRevisionOrUndefined(linkResult.data.content_revision)
          : undefined
        const contentRevision = contentResult?.ok
          ? contentRevisionOrUndefined(contentResult.data.content_revision)
          : undefined
        const coherentSource =
          linkResult.ok &&
          contentResult?.ok === true &&
          linkRevision !== undefined &&
          linkRevision === contentRevision &&
          (reportedRevision === undefined || reportedRevision === contentRevision)
        if (coherentSource && contentResult?.ok) {
          const patch: Partial<LinkResponse> = {
            ...linkResult.data,
            has_content: true,
            content: contentResult.data.content,
            content_document: contentResult.data.content_document,
            content_format: contentResult.data.content_format,
            content_revision: contentResult.data.content_revision,
          }
          noteContentRevision(requestedLinkId, contentResult.data.content_revision)
          patchKnownLink(requestedLinkId, patch)
          if (activeRef.current?.id === requestedLinkId) {
            flash('翻译来源已更新，请重新确认后再翻译', 'refresh')
          }
          return true
        }

        // The old body must not survive under a newly reported generation.
        // Keep has_content from the authoritative link projection so the open
        // pane can retry GET /content, but explicitly clear every body field.
        const blockedPatch: Partial<LinkResponse> = linkResult.ok
          ? {
              ...linkResult.data,
              content: undefined,
              content_document: undefined,
              content_format: undefined,
            }
          : {
              content: undefined,
              content_document: undefined,
              content_format: undefined,
            }
        const observedRevisions = [reportedRevision, linkRevision, contentRevision].filter(
          (candidate): candidate is number => candidate !== undefined,
        )
        const blockedRevision = observedRevisions.length > 0
          ? Math.max(...observedRevisions)
          : undefined
        if (blockedRevision !== undefined && blockedRevision > 0) {
          // A conflict or body response proves only the revision floor. Keep
          // the detail projection at the revision actually returned by
          // getLink; otherwise its older metadata would be relabelled as this
          // higher generation and accepted by SavedArticleDocument.
          noteContentRevision(requestedLinkId, blockedRevision)
        }
        patchKnownLink(requestedLinkId, blockedPatch)
        if (activeRef.current?.id === requestedLinkId) {
          flash('翻译来源已变化，原文刷新失败，请重试', 'alert')
        }
        return true
      }

      if (linkResult.ok) {
        const patch: Partial<LinkResponse> = { ...linkResult.data }
        if (patch.content_revision !== undefined && patch.content_revision > 0) {
          noteContentRevision(requestedLinkId, patch.content_revision)
        }
        patchKnownLink(requestedLinkId, patch)
      }

      if (activeRef.current?.id === requestedLinkId) {
        flash(
          linkResult.ok
            ? '翻译来源已更新，请重新选择后再翻译'
            : '翻译来源刷新失败，请稍后重试',
          linkResult.ok ? 'refresh' : 'alert',
        )
      }
      return true
    },
    [client, flash, noteContentRevision, patchKnownLink],
  )

  const onTranslateSelection = useCallback(
    async (info: SelectionInfo, force: boolean): Promise<string | null> => {
      const sourceLink = activeRef.current
      if (!sourceLink) return null
      const request = {
        scope: 'selection',
        block_key: info.blockKey,
        start_offset: info.start,
        end_offset: info.end,
        source_text: info.text,
        force,
      } as const
      const savedSelection = info.blockKey === 'content' || info.blockKey === 'content-document'
      if (savedSelection) {
        const revision = sourceLink.content_revision
        if (revision === undefined || !Number.isInteger(revision) || revision <= 0) {
          flash('后端未返回正文版本，无法安全创建翻译', 'alert')
          return null
        }
      } else if (info.blockKey === 'summary') {
        if (!activeSummarySourceHash) {
          flash('浏览器无法校验摘要版本，请刷新后重试', 'alert')
          return null
        }
      } else {
        flash('该内容块暂不支持翻译', 'alert')
        return null
      }
      if (activeRef.current?.id !== sourceLink.id) return null

      const res = savedSelection
        ? await runSavedTranslationCommand(async (context) => {
            if (
              context.id.linkId !== sourceLink.id ||
              context.id.contentRevision !== sourceLink.content_revision
            ) {
              return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
            }
            return createTranslation(request)
          })
        : await createTranslation(request)
      if (res.ok) return res.data.id
      if (await refreshAfterTranslationConflict(sourceLink.id, res.error)) return null
      flashTranslationError(
        res.error.message,
        res.error.kind,
        res.error.kind === 'rate-limited' ? res.error.retryAfterSeconds : undefined,
      )
      return null
    },
    [
      activeSummarySourceHash,
      createTranslation,
      flash,
      flashTranslationError,
      refreshAfterTranslationConflict,
      runSavedTranslationCommand,
    ],
  )

  const onTranslateFull = useCallback(
    async (force: boolean) => {
      const sourceLink = activeRef.current
      const revision = sourceLink?.content_revision
      if (!sourceLink || revision === undefined || !Number.isInteger(revision) || revision <= 0) {
        flash('后端未返回正文版本，无法安全创建翻译', 'alert')
        return
      }
      const res = await runSavedTranslationCommand(async (context) => {
        if (
          context.id.linkId !== sourceLink.id ||
          context.id.contentRevision !== revision
        ) {
          return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
        }
        return createTranslation({
          scope: 'full',
          force,
        })
      })
      if (res.ok) {
        flash(
          res.data.status === 'done' ? '全文译文已就绪' : '已开始全文翻译',
          res.data.status === 'done' ? 'check' : 'translate',
        )
        return
      }
      if (await refreshAfterTranslationConflict(sourceLink.id, res.error)) return
      flashTranslationError(
        res.error.message,
        res.error.kind,
        res.error.kind === 'rate-limited' ? res.error.retryAfterSeconds : undefined,
      )
    },
    [
      createTranslation,
      flashTranslationError,
      flash,
      refreshAfterTranslationConflict,
      runSavedTranslationCommand,
    ],
  )

  // 命令面板路由（对齐 app.jsx onCommand）。
  const onCommand = useCallback(
    (c: CommandItem) => {
      if (c.id === 'create-note') {
        void createEmptyNote()
        return
      }
      if (c.id.startsWith('open:')) return openLink(c.id.slice(5), c.link)
      if (c.id.startsWith('thought:')) {
        const thought = c.thought
        if (!thought) return
        if (thought.lifecycle_status === 'tombstone') {
          navigateRoute({ kind: 'tool', id: 'history' }, { thoughtView: 'history', thoughtId: thought.id })
          return
        }
        const target = readerThoughtHostTarget(thought)
        if (!target) return
        if (target.route.kind === 'library' && target.route.id === 'reading') {
          return openLink(target.targets?.linkId ?? '', undefined, true)
        }
        navigateRoute(target.route, target.targets)
        return
      }
      if (c.id.startsWith('note:')) {
        const noteID = c.note?.id?.trim() || c.id.slice(5).trim()
        if (noteID) navigateRoute({ kind: 'library', id: 'notes' }, { noteId: noteID })
        return
      }
      if (c.id.startsWith('site:')) {
        const siteID = c.id.slice(5).trim()
        if (siteID) navigateRoute({ kind: 'library', id: 'sites' }, { siteId: siteID })
        return
      }
      if (c.id.startsWith('tag:')) {
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSel({ type: 'tag', id: c.id.slice(4), name: '#' + c.id.slice(4) })
        return
      }
      if (c.id.startsWith('domain:')) {
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSel({ type: 'domain', id: c.id.slice(7), name: c.id.slice(7) })
        return
      }
      if (c.id.startsWith('nav:')) {
        const id = c.id.slice(4) as SmartId
        const names: Record<string, string> = {
          all: '全部链接',
          today: '今天',
          annotated: '有划线',
        }
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSel({ type: 'smart', id, name: names[id] || id })
        return
      }
      switch (c.id) {
        case 'pending':
          navigateRoute({ kind: 'library', id: 'pending' })
          break
        case 'theme':
          setTheme((t) => (t === 'light' ? 'dark' : 'light'))
          break
        case 'chat':
          if (!capabilityLease.isCurrent('ai')) break
          setNoteEd(null)
          setMobileNavOpen(false)
          setChatOpen(true)
          break
        case 'subs':
          navigateRoute({ kind: 'library', id: 'subs' })
          break
        case 'refresh':
          onSync()
          break
        default:
          break
      }
    },
    [capabilityLease, createEmptyNote, navigateRoute, onSync, openLink, setMobileNavOpen],
  )

  // 采纳某条 AI 回复为划线笔记（ChatSidebar 草稿模式调用）。
  const onAdoptNote = useCallback(
    async (locator: AnnotationLocator, text: string) => {
      if (!capabilityLease.isCurrent('ai') || !capabilityLease.isCurrent('annotations')) return
      const annotation = anns.find((item) => annotationMatchesLocator(item, locator))
      if (!annotation) return
      if (await updateAnnotation(annotation, { note: text, source: 'ai' })) {
        flash('已存为划线笔记 ✓', 'marker')
      } else {
        flash('保存划线笔记失败，请重试', 'alert')
      }
    },
    [anns, capabilityLease, flash, updateAnnotation],
  )

  // 退出草稿模式（ChatSidebar 调用）。
  const clearChatDraft = useCallback(() => setChatDraft(null), [])

  const onSidebarSelect = useCallback((next: Selection) => {
    setSel(next)
    setMobilePane('list')
    setMobileNavOpen(false)
  }, [setMobileNavOpen, setMobilePane])

  const onSidebarView = useCallback((next: LibraryView | ReaderRoute) => {
    if (typeof next === 'string') {
      // Changing a reading filter is a list concern. Keep the current detail
      // target mounted so an empty filtered list does not discard the article
      // the user was already reading.
      if (next === 'reading' && activeId) {
        navigateRoute({ kind: 'library', id: 'reading' }, { linkId: activeId })
        return
      }
      navigateRoute({ kind: 'library', id: next })
      return
    }
    navigateRoute(next)
  }, [activeId, navigateRoute])

  const onSidebarBrowse = useCallback((kind: 'tags' | 'domains') => {
    setBrowse(kind)
    setMobileNavOpen(false)
  }, [setMobileNavOpen])

  const toggleNavigation = useCallback(() => {
    if (!window.matchMedia('(max-width: 1439px)').matches) {
      setSidebarCollapsed((collapsed) => {
        const next = !collapsed
        writeOwnedStorage('sidebarCollapsed', next ? '1' : '0')
        return next
      })
      return
    }
    setMobileNavOpen((open) => {
      if (!open) {
        setChatOpen(false)
        setNoteEd(null)
      }
      return !open
    })
  }, [setMobileNavOpen])

  const backToMobileList = useCallback(() => {
    if (!confirmDiscardContentEdit()) return
    setChatOpen(false)
    setNoteEd(null)
    setHistoricalNote(null)
    setMobilePane('list')
  }, [confirmDiscardContentEdit, setMobilePane])

  // ⌘K / ⌘J（⌘J 右栏互斥：打开 AI 助手时关笔记面板）。
  const onToggleCmdk = useCallback(() => setCmdkOpen((o) => !o), [])
  useAppShortcuts({ onToggleCmdk, onToggleChat: toggleChat })

  // 当前编辑的划线（NotePanel 用）。
  const editingAnn = useMemo(
    () => noteEd
      ? anns.find((annotation) => annotationMatchesLocator(annotation, noteEd)) ?? null
      : null,
    [anns, noteEd],
  )
  const notePanelAnnotation = editingAnn ?? historicalNote?.annotation ?? null
  const activeListIndex = activeId
    ? protectedListLinks.findIndex((link) => link.id === activeId)
    : -1
  const previousLink = activeListIndex > 0 ? protectedListLinks[activeListIndex - 1] : null
  const nextLink =
    activeListIndex >= 0 && activeListIndex < protectedListLinks.length - 1
      ? protectedListLinks[activeListIndex + 1]
      : null

  // 以下几个回调与两个 pager 对象过去都是 JSX 里的内联字面量，每渲染新建一次。
  // DetailPane 加上 memo 之后它们必须稳定，否则 memo 的浅比较每次都判定
  // props 变了——测试照样绿，收益却是零。
  const onPickTag = useCallback((tag: string) => {
    setSel({ type: 'tag', id: tag, name: '#' + tag })
    setMobilePane('list')
  }, [setMobilePane])

  const onToggleFocus = useCallback(() => readingFocusStore.toggle(), [])

  const onConvertToSite = useCallback(() => {
    if (!capabilityLease.isCurrent('siteWrite')) return
    if (!confirmDiscardContentEdit()) return
    setConvertingLink((current) => current ?? activeRef.current ?? null)
  }, [capabilityLease, confirmDiscardContentEdit])

  // 删除当前文章。后端是软删（进回收站），所以这里不弹确认框，而是给一次
  // 撤销机会——真正不可逆的是 purge，不是这一步。失败时不动任何本地状态，
  // 否则列表会显示一条实际还在服务端的记录。
  const onDeleteLink = useCallback(() => {
    const target = activeRef.current
    if (!target) return
    if (!confirmDiscardContentEdit()) return
    const { id, title, url } = target
    void (async () => {
      const result = await client.deleteLink(id)
      if (!result.ok) {
        flash(`删除失败：${result.error.message}`, 'alert')
        return
      }
      invalidateLibrary()
      invalidateLink(id)
      setActiveId(null)
      setActiveDetail(null)
      setActiveFallback(null)
      void list.reload()
      flash(`已删除「${title || url}」`, 'trash', {
        label: '撤销',
        onAction: () => {
          dismissToast()
          void (async () => {
            const restored = await client.restoreLink(id)
            if (!restored.ok) {
              flash(`撤销失败：${restored.error.message}`, 'alert')
              return
            }
            invalidateLibrary()
            invalidateLink(id)
            void list.reload()
            flash('已恢复', 'check')
          })()
        },
      })
    })()
  }, [client, confirmDiscardContentEdit, dismissToast, flash, list, setActiveId])

  // 空闲预取上一篇 / 下一篇：用户点「下一篇」时是 0 个网络请求。
  // 预取的是译文列表——详情本身已经能从列表数据直接渲染（PF6 让列表说了真话）。
  const prefetchTargets = useMemo<PrefetchTarget[]>(() => {
    const targets: PrefetchTarget[] = []
    for (const candidate of [nextLink, previousLink]) {
      if (!candidate) continue
      const key = translationsKey(candidate.id, candidate.content_revision)
      targets.push({
        key,
        load: () =>
          resourceStore.fetch(key, () =>
            client.getTranslations(candidate.id),
          ),
      })
    }
    return targets
  }, [client, nextLink, previousLink])
  usePrefetch(prefetchTargets)

  const previousPager = useMemo(
    () =>
      previousLink
        ? { title: pagerTitle(previousLink), onSelect: () => openLink(previousLink.id, previousLink) }
        : null,
    [previousLink, openLink],
  )

  const nextPager = useMemo(
    () =>
      nextLink
        ? { title: pagerTitle(nextLink), onSelect: () => openLink(nextLink.id, nextLink) }
        : null,
    [nextLink, openLink],
  )

  return (
    <div className="stage">
      <div className="win" key={capabilityLease.generation}>
        <Titlebar
          theme={theme}
          chatOpen={chatOpen}
          navigationOpen={mobileNavOpen}
          sidebarCollapsed={sidebarCollapsed}
          syncing={displayedView !== 'subs' && librarySyncing}
          thoughtSync={thoughtSyncController ? thoughtSyncSnapshot : null}
          onSync={
            displayedView === 'subs'
              ? () => {
                  onRefreshCapabilities?.()
                  setSubsSyncRequest((request) => request + 1)
                }
              : onSync
          }
          onToggleNavigation={toggleNavigation}
          onAddLink={() => setAddLinkOpen(true)}
          onOpenCmdk={() => setCmdkOpen(true)}
          onToggleTheme={() => setTheme((t) => (t === 'light' ? 'dark' : 'light'))}
          onToggleChat={toggleChat}
          onOpenSettings={openSettings}
          archiveDownloading={archiveDownloading}
          onDownloadArchive={() => setArchiveDialogOpen(true)}
          canUseAI={capabilityPolicy.ai}
          canDownloadArchive={capabilityPolicy.archiveDownload}
        />

        {displayedView !== 'subs' && librarySyncFailures.length > 0 && (
          <div className="library-sync-error" role="alert">
            <span>资料库同步部分失败：{librarySyncFailures.join('、')}</span>
            <button type="button" onClick={onSync} disabled={librarySyncing}>
              重试资料库同步
            </button>
          </div>
        )}

        <PendingInboxCountProvider client={client} capabilityLease={capabilityLease}>
        <div
          aria-busy={displayedView === 'reading' && detailLoading ? true : undefined}
          className={
            'body' +
            (displayedView === 'reading' && mobilePane === 'detail' ? ' mobile-detail-active' : '') +
            (mobileNavOpen ? ' mobile-nav-open' : '') +
            (sidebarCollapsed ? ' sidebar-collapsed' : '') +
            (focusMode && displayedView === 'reading' ? ' focus-mode' : '') +
            (chatOpen || editingAnn || contentEditState?.editing ? ' mobile-tool-open' : '')
          }
        >
          {displayedView === 'home' ? (
            <HomeSurface
              client={client}
              lease={lease}
              capabilityLease={capabilityLease}
              onNavigate={navigateRoute}
              onOpenLink={openLink}
              scrollRef={homeScrollRef}
              feedSlot={(
                <FeedSurface
                  client={client}
                  capabilityLease={capabilityLease}
                  onNavigate={navigateRoute}
                  onOpenLink={openLink}
                  variant="embedded"
                  hostScrollRef={homeScrollRef}
                />
              )}
            />
          ) : displayedView === 'feed' ? (
            <FeedSurface client={client} capabilityLease={capabilityLease} onNavigate={navigateRoute} onOpenLink={openLink} />
          ) : displayedView === 'pending' ? (
            <InboxSurface client={client} capabilityPolicy={capabilityPolicy} onNavigate={navigateRoute} onOpenLink={openLink} initialInboxID={inboxTargetID} onDraftStateChange={reportInboxDraftState} />
          ) : displayedView === 'notes' ? (
            <NotesSurface
              client={client}
              lease={lease}
              capabilityLease={capabilityLease}
              onNavigate={navigateRoute}
              initialNoteID={noteTargetID}
              onDraftDirtyChange={reportNotesDraftDirty}
              onPendingPersistenceChange={reportNotesPendingPersistence}
              onPrepareToLeaveChange={reportNotesPrepareToLeave}
              onCreateNote={canCreateNote ? () => { void createEmptyNote() } : undefined}
              creatingNote={creatingNote}
              annotationsEnabled={capabilityPolicy.annotations}
              aiEnabled={capabilityPolicy.ai}
              trashEnabled={capabilityPolicy.trash}
            />
          ) : displayedView === 'todo' ? (
            <TodoSurface
              client={client}
              capabilityPolicy={capabilityPolicy}
              onNavigate={navigateRoute}
              onOpenLink={openLink}
              completedExpanded={todoCompletedExpanded}
              onCompletedExpandedChange={setTodoCompletedExpanded}
            />
          ) : displayedView === 'settings' ? (
            <SettingsSurface client={client} capabilityPolicy={capabilityPolicy} onNavigate={navigateRoute} onOpenConnectionSettings={openSettings} />
          ) : displayedView === 'history' ? (
			<ThoughtHistorySurface client={client} lease={lease} capabilityLease={capabilityLease} onNavigate={navigateRoute} />
          ) : displayedView === 'trash' ? (
            <TrashSurface client={client} onNavigate={navigateRoute} capabilityPolicy={capabilityPolicy} onToast={flash} />
          ) : displayedView === 'subs' ? (
              <SubsView
              client={client}
              navigationOpen={mobileNavOpen}
              onCloseNavigation={() => setMobileNavOpen(false)}
              onView={onSidebarView}
              onNavigate={navigateRoute}
              collapsed={sidebarCollapsed}
              onOpenAnalysis={openLink}
              onOpenSettings={openSettings}
              onToast={flash}
              syncRequest={subsSyncRequest}
              capabilityPolicy={capabilityPolicy}
            />
          ) : displayedView === 'sites' ? (
            <SitesView
              client={client}
              capabilityLease={capabilityLease}
              onToast={flash}
              initialSiteId={siteTargetID}
              collapsed={sidebarCollapsed}
              onView={onSidebarView}
              onNavigate={navigateRoute}
              navigationOpen={mobileNavOpen}
              onCloseNavigation={() => setMobileNavOpen(false)}
            />
          ) : (
            <>
              <Sidebar
                sel={sel}
                onSelect={onSidebarSelect}
                view={displayedView as LibraryView}
                onView={onSidebarView}
                onNavigate={navigateRoute}
                collapsed={sidebarCollapsed}
                pins={pins}
                onTogglePin={togglePin}
                onBrowse={onSidebarBrowse}
                tags={tagStatList}
                domains={domainStatList}
                tagsAvailable={tagsAvailable}
                domainsAvailable={domainsAvailable}
                counts={sidebarCounts}
                readerClient={client}
                capabilityPolicy={capabilityPolicy}
              />
              <button
                type="button"
                className="mobile-nav-backdrop"
                aria-label="关闭导航"
                onClick={() => setMobileNavOpen(false)}
              />
              <ListPane
                title={sel.name}
                links={protectedListLinks}
                activeId={activeId}
                onSelect={openLink}
                loading={list.loading}
                loadingMore={list.loadingMore}
                error={list.error}
                hasMore={list.hasMore}
                onLoadMore={list.loadMore}
                onReload={list.reload}
                onOpenSettings={openSettings}
              />
              <DetailPane
                l={renderedActive}
                document={savedDocument}
                captureDocumentContext={captureSavedDocumentContext}
                onBack={backToMobileList}
                chatOpen={chatOpen}
                onChat={toggleChat}
                onToast={flash}
                curTag={sel.type === 'tag' ? sel.id : null}
                onPickTag={onPickTag}
                corpus={corpus}
                anns={anns}
                historicalAnnotations={articleAnnotations.historicalAnnotations}
                historicalDegraded={articleAnnotations.degraded}
                onOpenHistoricalAnnotation={openHistoricalAnnotation}
                onAddAnn={addAnnotation}
                onRemoveAnn={removeAnnotation}
                onOpenNote={openNote}
                onAskAI={onAskAI}
                annotationsEnabled={capabilityPolicy.annotations}
                aiEnabled={capabilityPolicy.ai}
                relatedTagsEnabled={capabilityPolicy.relatedTags}
                engagementEnabled={capabilityPolicy.engagement}
                onSaveContent={onSaveContent}
                onReplaceContent={onReplaceContent}
                onEditContent={onSaveContentEdit}
                onEditMetadata={onSaveLinkMetadata}
                onContentEditStateChange={reportContentEditState}
                navigationRestoreEpoch={navigationRestoreEpoch}
                onLoadContent={loadSavedDocumentBody}
                savingContent={savingContent}
                loadingDetail={detailLoading}
                translations={translations}
                staleTranslations={staleTranslations}
                translationsLoading={translationsLoading}
                onSummaryBlockText={onSummaryBlockText}
                summarySourceHash={summaryBlock?.sourceHash ?? null}
                summaryProjectionEpoch={summaryProjectionEpoch}
                onTranslateSelection={onTranslateSelection}
                onTranslateFull={onTranslateFull}
				focusMode={focusMode}
				onToggleFocus={onToggleFocus}
				onConvertToSite={capabilityPolicy.siteWrite ? onConvertToSite : undefined}
				onDeleteLink={onDeleteLink}
				previous={previousPager}
				next={nextPager}
              />
            </>
          )}
          {capabilityPolicy.annotations && notePanelAnnotation && displayedView === 'reading' && (
            <NotePanel
              ann={notePanelAnnotation}
              readOnly={historicalNote !== null}
              onSave={async (v) => {
                if (!editingAnn || historicalNote) return
                if (await updateAnnotation(editingAnn, { note: v.trim() })) {
                  setNoteEd(null)
                  flash('笔记已保存', 'marker')
                } else {
                  flash('保存笔记失败，请重试', 'alert')
                }
              }}
              onDelete={async () => {
                if (!editingAnn || historicalNote) return
                if (await removeAnnotation(editingAnn)) setNoteEd(null)
              }}
              onAskAI={async (annotation, text, draftVal) => {
                if (!editingAnn || historicalNote) return
                // 切去问 AI 前把未保存草稿写回，不丢字。
                if (draftVal != null && draftVal.trim() !== (editingAnn.note || '')) {
                  if (!await updateAnnotation(editingAnn, { note: draftVal.trim() })) {
                    flash('保存草稿失败，请重试', 'alert')
                    return
                  }
                }
                onAskAI(annotation, text)
              }}
              onClose={() => {
                setNoteEd(null)
                setHistoricalNote(null)
              }}
            />
          )}
			{capabilityPolicy.siteWrite && convertingLink && <LinkConversionDialog capabilityLease={capabilityLease} client={client} link={convertingLink} initialNote={anns.map((ann) => ann.note.trim()).filter(Boolean).join('\n\n')} onClose={() => setConvertingLink(null)} onToast={flash} onConverted={() => { if (!capabilityLease.isCurrent('siteWrite')) return; invalidateLibrary(); invalidateLink(convertingLink.id); setConvertingLink(null); setActiveId(null); setActiveDetail(null); setActiveFallback(null); navigateRoute({ kind: 'library', id: 'sites' }); list.reload() }} />}
          {capabilityPolicy.ai && chatOpen && displayedView === 'reading' && !notePanelAnnotation && (
            <ChatSidebar
              client={client}
              link={renderedActive}
              contentContext={aiContentContext}
              draft={chatDraft}
              onAdopt={onAdoptNote}
              onClearDraft={clearChatDraft}
              onClose={() => setChatOpen(false)}
            />
          )}
        </div>
        </PendingInboxCountProvider>

        <CommandPalette
          open={cmdkOpen}
          onClose={() => setCmdkOpen(false)}
          onCommand={onCommand}
          client={client}
          corpus={corpus}
          tagStats={tagStatList}
          domainStats={domainStatList}
          canCreateNote={canCreateNote}
          capabilityPolicy={capabilityPolicy}
        />
        {addLinkOpen && (
          <AddLinkDialog
            client={client}
            capabilityLease={capabilityLease}
            destination={capabilityPolicy.inbox ? 'inbox' : 'library'}
            onClose={() => setAddLinkOpen(false)}
            onToast={flash}
            onAdded={(target) => {
              invalidateLibrary()
              setAddLinkOpen(false)
              if (target.kind === 'inbox') {
                refreshPendingInboxCount()
                navigateRoute({ kind: 'library', id: 'pending', inboxId: target.id })
                return
              }
              navigateRoute({ kind: 'library', id: 'reading' }, { linkId: target.id })
              setSel({ type: 'smart', id: 'all', name: '全部链接' })
              list.reload()
            }}
          />
        )}
        <ArchiveDownloadDialog
          open={archiveDialogOpen}
          downloading={archiveDownloading}
          onClose={() => setArchiveDialogOpen(false)}
          onDownload={onDownloadArchive}
        />
        {browse && (
          <BrowsePanel
            kind={browse}
            onClose={() => setBrowse(null)}
            pins={pins}
            onTogglePin={togglePin}
            tags={tagStatList}
            domains={domainStatList}
            readerClient={client}
            activityEnabled={capabilityPolicy.activity}
            onPick={(type, id) => {
              if (!navigateRoute({ kind: 'library', id: 'reading' })) return
              setSel({ type, id, name: type === 'tag' ? '#' + id : id })
            }}
          />
        )}

        <Toast msg={toast?.msg ?? null} icon={toast?.icon} action={toast?.action} />
        {updateReady && (
          <button
            type="button"
            className="ne-btn"
            style={{ position: 'fixed', right: 20, bottom: 20, zIndex: 90 }}
            onClick={applyServiceWorkerUpdate}
          >
            新版本已就绪，点击刷新
          </button>
        )}
      </div>
    </div>
  )
}
