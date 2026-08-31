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
import { type Selection, type SmartId } from '../hooks/useLinks'
import { invalidateSites } from '../hooks/useSites'
import { invalidateReaderActivity } from '../hooks/useReaderActivity'
import { invalidateReaderRelatedTags } from '../hooks/useReaderRelatedTags'
import { useAppShortcuts } from '../hooks/useAppChrome'
import { useTranslations } from '../hooks/useTranslations'
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
import { usePins } from '../lib/meta'
import { readingFocusStore } from '../lib/reading-surface'
import { applyServiceWorkerUpdate } from '../lib/sw'
import {
  invalidateLibrary,
  invalidateLink,
  invalidateLinkContent,
  invalidateLinkProjection,
} from '../lib/cache/invalidate'
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
  TranslationResponse,
} from '../lib/api/types'
import { err, type ApiError, type ApiResult } from '@webtag/api'
import type {
  DocumentCommandContext,
  SavedContentSource,
} from '../lib/article/document'
import { isSavedContentTranslationSource } from '../lib/article/document'
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
  useMainViewNavigation,
} from './main-view/navigation-controller'
import {
  contentRevisionOrUndefined,
  useActiveResourceController,
} from './main-view/active-resource-controller'

type Theme = 'light' | 'dark'
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
  const {
    selection: sel,
    setSelection: setSel,
    list,
    reloadLinks,
    reloadTags,
    reloadDomains,
    protectedListLinks,
    corpus,
    renderedActive,
    aiContentContext,
    savedArticle,
    savedDocument,
    captureSavedDocumentContext,
    loadSavedDocumentBody,
    detailLoading,
    summaryBlock,
    activeSummarySourceHash,
    summaryProjectionEpoch,
    onSummaryBlockText,
    resetSummarySourceHash,
    revisionFloor,
    noteContentRevision,
    patchKnownLink,
    onSaveLinkMetadata,
    getActiveLink,
    openLink,
    onDeleteLink,
    clearActiveResource,
    sidebarCounts,
    tagStatList,
    domainStatList,
    tagsAvailable,
    domainsAvailable,
    previousPager,
    nextPager,
  } = useActiveResourceController({
    client,
    lease,
    capabilityLease,
    activeId,
    setActiveId,
    view,
    pendingLinkTarget,
    commitRoute,
    confirmDiscardNavigation,
    confirmDiscardContentEdit,
    setMobilePane,
    setMobileNavOpen,
    flash,
    dismissToast,
  })
	const homeScrollRef = useRef<HTMLDivElement>(null)
	const [todoCompletedExpanded, setTodoCompletedExpanded] = useState(false)
  const [subsSyncRequest, setSubsSyncRequest] = useState(0)
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
  const [pins, togglePin] = usePins()
  const librarySyncInFlight = useRef<Promise<void> | null>(null)
  const librarySyncController = useRef<AbortController | null>(null)
  const draftNonce = useRef(0)
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
  useEffect(() => thoughtSyncController?.start(), [thoughtSyncController])

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

  // 切换文章时清掉笔记面板与问 AI 草稿，避免跨链接误写。
  useEffect(() => {
    setNoteEd(null)
    setHistoricalNote(null)
    setChatDraft(null)
  }, [activeId])

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
      const current = getActiveLink()
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
      getActiveLink,
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
        const current = getActiveLink()
        resetSummarySourceHash(
          requestedLinkId,
          current?.id === requestedLinkId ? (current.summary ?? null) : null,
        )
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
          if (getActiveLink()?.id === requestedLinkId) {
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
        if (getActiveLink()?.id === requestedLinkId) {
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

      if (getActiveLink()?.id === requestedLinkId) {
        flash(
          linkResult.ok
            ? '翻译来源已更新，请重新选择后再翻译'
            : '翻译来源刷新失败，请稍后重试',
          linkResult.ok ? 'refresh' : 'alert',
        )
      }
      return true
    },
    [client, flash, getActiveLink, noteContentRevision, patchKnownLink, resetSummarySourceHash],
  )

  const onTranslateSelection = useCallback(
    async (info: SelectionInfo, force: boolean): Promise<string | null> => {
      const sourceLink = getActiveLink()
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
      if (getActiveLink()?.id !== sourceLink.id) return null

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
      getActiveLink,
      refreshAfterTranslationConflict,
      runSavedTranslationCommand,
    ],
  )

  const onTranslateFull = useCallback(
    async (force: boolean) => {
      const sourceLink = getActiveLink()
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
      getActiveLink,
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
    [capabilityLease, createEmptyNote, navigateRoute, onSync, openLink, setMobileNavOpen, setSel],
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
  }, [setMobileNavOpen, setMobilePane, setSel])

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

  // 以下几个回调与两个 pager 对象过去都是 JSX 里的内联字面量，每渲染新建一次。
  // DetailPane 加上 memo 之后它们必须稳定，否则 memo 的浅比较每次都判定
  // props 变了——测试照样绿，收益却是零。
  const onPickTag = useCallback((tag: string) => {
    setSel({ type: 'tag', id: tag, name: '#' + tag })
    setMobilePane('list')
  }, [setMobilePane, setSel])

  const onToggleFocus = useCallback(() => readingFocusStore.toggle(), [])

  const onConvertToSite = useCallback(() => {
    if (!capabilityLease.isCurrent('siteWrite')) return
    if (!confirmDiscardContentEdit()) return
    setConvertingLink((current) => current ?? getActiveLink() ?? null)
  }, [capabilityLease, confirmDiscardContentEdit, getActiveLink])

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
			{capabilityPolicy.siteWrite && convertingLink && <LinkConversionDialog capabilityLease={capabilityLease} client={client} link={convertingLink} initialNote={anns.map((ann) => ann.note.trim()).filter(Boolean).join('\n\n')} onClose={() => setConvertingLink(null)} onToast={flash} onConverted={() => { if (!capabilityLease.isCurrent('siteWrite')) return; invalidateLibrary(); invalidateLink(convertingLink.id); setConvertingLink(null); clearActiveResource(); navigateRoute({ kind: 'library', id: 'sites' }); list.reload() }} />}
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
