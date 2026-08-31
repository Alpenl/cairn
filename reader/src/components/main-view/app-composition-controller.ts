import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type Dispatch,
  type MutableRefObject,
  type SetStateAction,
} from 'react'

import type { CommandItem } from '../CommandPalette'
import type { ChatDraft } from '../ChatSidebar'
import type { IconName } from '../Icon'
import type { LibraryView } from '../PrimaryNav'
import type { ToastAction } from '../Toast'
import { clearFeedSessionState } from '../../lib/feed-session-state'
import { refreshPendingInboxCount } from '../../lib/pending-inbox-events'
import { type Selection, type SmartId } from '../../hooks/useLinks'
import {
  type HistoricalArticleAnnotation,
} from '../../hooks/useArticleAnnotations'
import {
  annotationMatchesLocator,
  type Annotation,
  type AnnotationLocator,
  type AnnotationPatch,
} from '../../lib/annotations'
import type { LinkResponse } from '../../lib/api/types'
import {
  invalidateLibrary,
  invalidateLink,
} from '../../lib/cache/invalidate'
import { invalidateReaderActivity } from '../../hooks/useReaderActivity'
import { invalidateReaderRelatedTags } from '../../hooks/useReaderRelatedTags'
import { invalidateSites } from '../../hooks/useSites'
import type {
  ReaderCapabilityLease,
  ReaderCapabilityPolicy,
} from '../../lib/capabilities'
import { usePins } from '../../lib/meta'
import type { PinKind, Pins } from '../../lib/meta'
import {
  readerThoughtHostTarget,
  type ReaderRoute,
  type ReaderRouteTargets,
} from '../../lib/navigation/route'
import { readingFocusStore } from '../../lib/reading-surface'
import type {
  ReaderAIPort,
  ReaderHomePort,
  ReaderInboxTodosPort,
  ReaderLibrarySitesPort,
  ReaderSessionArchivePort,
  ReaderSubscriptionsFeedPort,
  ReaderThoughtsNotesPort,
} from '../../lib/reader-api-ports'
import { applyServiceWorkerUpdate } from '../../lib/sw'
import { readOwnedStorage, writeOwnedStorage } from '../../lib/storage-ownership'
import { useAppShortcuts } from '../../hooks/useAppChrome'

type Theme = 'light' | 'dark'
type BrowsePanelKind = 'tags' | 'domains'
type BrowsePickKind = 'tag' | 'domain'
type Flash = (msg: string, icon?: IconName, action?: ToastAction) => void

type MainViewAppClient = ReaderLibrarySitesPort &
  ReaderSubscriptionsFeedPort &
  ReaderThoughtsNotesPort &
  ReaderInboxTodosPort &
  ReaderHomePort &
  ReaderSessionArchivePort &
  ReaderAIPort

type OpenLink = (
  id: string,
  candidate?: LinkResponse,
  revealMobileDetail?: boolean,
) => unknown

type CommitRoute = (
  route: ReaderRoute,
  targets?: ReaderRouteTargets,
  historyMode?: 'push' | 'replace' | 'none',
  addressLink?: boolean,
) => void

type NavigateRoute = (
  route: ReaderRoute,
  targets?: ReaderRouteTargets,
) => boolean | Promise<boolean>

type AnnotationUpdate = (
  annotation: Annotation,
  patch: AnnotationPatch,
) => Promise<boolean>

interface AddLinkTarget {
  readonly kind: 'inbox' | 'library'
  readonly id: string
}

export interface MainViewToastController {
  readonly toast: { readonly msg: string; readonly icon?: IconName; readonly action?: ToastAction } | null
  readonly flash: Flash
  readonly dismissToast: () => void
}

interface UseMainViewAppControllerOptions {
  readonly client: MainViewAppClient
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly capabilityLease: ReaderCapabilityLease
  readonly activeId: string | null
  readonly anns: readonly Annotation[]
  readonly historicalAnnotations: readonly HistoricalArticleAnnotation[]
  readonly setSelection: (selection: Selection) => void
  readonly setMobilePane: (pane: 'list' | 'detail') => void
  readonly setMobileNavOpen: Dispatch<SetStateAction<boolean>>
  readonly confirmDiscardContentEdit: () => boolean
  readonly confirmDiscardNavigation: () => boolean | Promise<boolean>
  readonly commitRoute: CommitRoute
  readonly navigateRoute: NavigateRoute
  readonly openLink: OpenLink
  readonly getActiveLink: () => LinkResponse | undefined
  readonly clearActiveResource: () => void
  readonly reloadList: () => unknown
  readonly updateAnnotation: AnnotationUpdate
  readonly removeAnnotation: (annotation: Annotation) => Promise<boolean>
  readonly onOpenSettings: () => void
  readonly syncLibraryAndThoughts: () => void
  readonly flash: Flash
}

export interface MainViewAppController {
  readonly theme: Theme
  readonly onToggleTheme: () => void
  readonly homeScrollRef: MutableRefObject<HTMLDivElement | null>
  readonly todoCompletedExpanded: boolean
  readonly setTodoCompletedExpanded: Dispatch<SetStateAction<boolean>>
  readonly chatOpen: boolean
  readonly closeChat: () => void
  readonly sidebarCollapsed: boolean
  readonly focusMode: boolean
  readonly pins: Pins
  readonly onTogglePin: (kind: PinKind, name: string) => void
  readonly addLinkOpen: boolean
  readonly openAddLinkDialog: () => void
  readonly closeAddLinkDialog: () => void
  readonly onAddLinkAdded: (target: AddLinkTarget) => void
  readonly cmdkOpen: boolean
  readonly openCommandPalette: () => void
  readonly closeCommandPalette: () => void
  readonly browse: BrowsePanelKind | null
  readonly closeBrowse: () => void
  readonly onBrowsePick: (type: BrowsePickKind, id: string) => void
  readonly notePanelAnnotation: Annotation | null
  readonly notePanelReadOnly: boolean
  readonly notePanelEditing: boolean
  readonly chatDraft: ChatDraft | null
  readonly clearChatDraft: () => void
  readonly updateReady: boolean
  readonly applyUpdate: () => void
  readonly archiveDialogOpen: boolean
  readonly openArchiveDialog: () => void
  readonly closeArchiveDialog: () => void
  readonly convertingLink: LinkResponse | null
  readonly conversionInitialNote: string
  readonly closeConversionDialog: () => void
  readonly onConversionConverted: () => void
  readonly canCreateNote: boolean
  readonly creatingNote: boolean
  readonly createEmptyNote: () => Promise<void>
  readonly openSettings: () => void
  readonly toggleChat: () => void
  readonly openNote: (annotation: AnnotationLocator) => void
  readonly openHistoricalAnnotation: (annotation: Annotation) => void
  readonly onAskAI: (annotation: AnnotationLocator, text: string) => void
  readonly onCommand: (command: CommandItem) => void
  readonly onAdoptNote: (locator: AnnotationLocator, text: string) => Promise<void>
  readonly onSidebarSelect: (selection: Selection) => void
  readonly onSidebarView: (view: LibraryView | ReaderRoute) => void
  readonly onSidebarBrowse: (kind: BrowsePanelKind) => void
  readonly toggleNavigation: () => void
  readonly backToMobileList: () => void
  readonly onPickTag: (tag: string) => void
  readonly onToggleFocus: () => void
  readonly onConvertToSite: () => void
  readonly onSaveNotePanel: (value: string) => Promise<void>
  readonly onDeleteNotePanel: () => Promise<void>
  readonly onAskAINotePanel: (
    annotation: AnnotationLocator,
    text: string,
    draftValue: string,
  ) => Promise<void>
  readonly closeNotePanel: () => void
}

export function useMainViewToast(): MainViewToastController {
  const [toast, setToast] = useState<MainViewToastController['toast']>(null)
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

  return { toast, flash, dismissToast }
}

export function useMainViewAppController({
  client,
  capabilityPolicy,
  capabilityLease,
  activeId,
  anns,
  historicalAnnotations,
  setSelection,
  setMobilePane,
  setMobileNavOpen,
  confirmDiscardContentEdit,
  confirmDiscardNavigation,
  commitRoute,
  navigateRoute,
  openLink,
  getActiveLink,
  clearActiveResource,
  reloadList,
  updateAnnotation,
  removeAnnotation,
  onOpenSettings,
  syncLibraryAndThoughts,
  flash,
}: UseMainViewAppControllerOptions): MainViewAppController {
  const [theme, setTheme] = useState<Theme>(() => readOwnedStorage('theme') === 'dark' ? 'dark' : 'light')
  const homeScrollRef = useRef<HTMLDivElement>(null)
  const [todoCompletedExpanded, setTodoCompletedExpanded] = useState(false)
  const [chatOpen, setChatOpen] = useState(false)
  const [sidebarCollapsed, setSidebarCollapsed] = useState(() => readOwnedStorage('sidebarCollapsed') === '1')
  const focusMode = useSyncExternalStore(
    readingFocusStore.subscribe,
    readingFocusStore.getSnapshot,
    () => false,
  )
  const [addLinkOpen, setAddLinkOpen] = useState(false)
  const [cmdkOpen, setCmdkOpen] = useState(false)
  const [browse, setBrowse] = useState<BrowsePanelKind | null>(null)
  const [noteEd, setNoteEd] = useState<AnnotationLocator | null>(null)
  const [historicalNote, setHistoricalNote] = useState<HistoricalArticleAnnotation | null>(null)
  const [chatDraft, setChatDraft] = useState<ChatDraft | null>(null)
  // 新版本就绪：给一条可点的提示，不静默强制刷新（那会打断正在读文章的人）。
  const [updateReady, setUpdateReady] = useState(false)
  const [archiveDialogOpen, setArchiveDialogOpen] = useState(false)
  const [convertingLink, setConvertingLink] = useState<LinkResponse | null>(null)
  const [pins, togglePin] = usePins()
  const draftNonce = useRef(0)
  const createNoteIntent = useRef<Promise<void> | null>(null)
  const [creatingNote, setCreatingNote] = useState(false)
  const canCreateNote = capabilityPolicy.notes

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
    if (!policy.archiveDownload) setArchiveDialogOpen(false)
  }, [capabilityLease, client])

  // 切换文章时清掉笔记面板与问 AI 草稿，避免跨链接误写。
  useEffect(() => {
    setNoteEd(null)
    setHistoricalNote(null)
    setChatDraft(null)
  }, [activeId])

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

  const toggleTheme = useCallback(() => {
    setTheme((value) => (value === 'light' ? 'dark' : 'light'))
  }, [])

  // 右栏互斥：打开 AI 助手则关笔记面板，反之亦然。
  const toggleChat = useCallback(() => {
    if (!capabilityLease.isCurrent('ai')) return
    setMobileNavOpen(false)
    setChatOpen((open) => {
      if (!open) {
        setNoteEd(null)
        setHistoricalNote(null)
      }
      return !open
    })
  }, [capabilityLease, setMobileNavOpen])

  const closeChat = useCallback(() => setChatOpen(false), [])

  const openNote = useCallback((annotation: AnnotationLocator) => {
    if (!capabilityLease.isCurrent('annotations')) return
    setMobileNavOpen(false)
    setHistoricalNote(null)
    setNoteEd(annotation)
    setChatOpen(false)
  }, [capabilityLease, setMobileNavOpen])

  const openHistoricalAnnotation = useCallback((annotation: Annotation) => {
    if (!capabilityLease.isCurrent('annotations')) return
    const historical = historicalAnnotations.find(
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
  }, [capabilityLease, flash, historicalAnnotations, setMobileNavOpen])

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

  const onCommand = useCallback(
    (command: CommandItem) => {
      if (command.id === 'create-note') {
        void createEmptyNote()
        return
      }
      if (command.id.startsWith('open:')) return openLink(command.id.slice(5), command.link)
      if (command.id.startsWith('thought:')) {
        const thought = command.thought
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
      if (command.id.startsWith('note:')) {
        const noteID = command.note?.id?.trim() || command.id.slice(5).trim()
        if (noteID) navigateRoute({ kind: 'library', id: 'notes' }, { noteId: noteID })
        return
      }
      if (command.id.startsWith('site:')) {
        const siteID = command.id.slice(5).trim()
        if (siteID) navigateRoute({ kind: 'library', id: 'sites' }, { siteId: siteID })
        return
      }
      if (command.id.startsWith('tag:')) {
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSelection({ type: 'tag', id: command.id.slice(4), name: '#' + command.id.slice(4) })
        return
      }
      if (command.id.startsWith('domain:')) {
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSelection({ type: 'domain', id: command.id.slice(7), name: command.id.slice(7) })
        return
      }
      if (command.id.startsWith('nav:')) {
        const id = command.id.slice(4) as SmartId
        const names: Record<string, string> = {
          all: '全部链接',
          today: '今天',
          annotated: '有划线',
        }
        if (!navigateRoute({ kind: 'library', id: 'reading' })) return
        setSelection({ type: 'smart', id, name: names[id] || id })
        return
      }
      switch (command.id) {
        case 'pending':
          navigateRoute({ kind: 'library', id: 'pending' })
          break
        case 'theme':
          toggleTheme()
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
          syncLibraryAndThoughts()
          break
        default:
          break
      }
    },
    [
      capabilityLease,
      createEmptyNote,
      navigateRoute,
      openLink,
      setMobileNavOpen,
      setSelection,
      syncLibraryAndThoughts,
      toggleTheme,
    ],
  )

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

  const clearChatDraft = useCallback(() => setChatDraft(null), [])

  const onSidebarSelect = useCallback((next: Selection) => {
    setSelection(next)
    setMobilePane('list')
    setMobileNavOpen(false)
  }, [setMobileNavOpen, setMobilePane, setSelection])

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

  const onSidebarBrowse = useCallback((kind: BrowsePanelKind) => {
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

  const onToggleCmdk = useCallback(() => setCmdkOpen((open) => !open), [])
  useAppShortcuts({ onToggleCmdk, onToggleChat: toggleChat })

  const editingAnn = useMemo(
    () => noteEd
      ? anns.find((annotation) => annotationMatchesLocator(annotation, noteEd)) ?? null
      : null,
    [anns, noteEd],
  )
  const notePanelAnnotation = editingAnn ?? historicalNote?.annotation ?? null

  const onPickTag = useCallback((tag: string) => {
    setSelection({ type: 'tag', id: tag, name: '#' + tag })
    setMobilePane('list')
  }, [setMobilePane, setSelection])

  const onToggleFocus = useCallback(() => readingFocusStore.toggle(), [])

  const onConvertToSite = useCallback(() => {
    if (!capabilityLease.isCurrent('siteWrite')) return
    if (!confirmDiscardContentEdit()) return
    setConvertingLink((current) => current ?? getActiveLink() ?? null)
  }, [capabilityLease, confirmDiscardContentEdit, getActiveLink])

  const closeBrowse = useCallback(() => setBrowse(null), [])
  const closeCommandPalette = useCallback(() => setCmdkOpen(false), [])
  const openCommandPalette = useCallback(() => setCmdkOpen(true), [])
  const openAddLinkDialog = useCallback(() => setAddLinkOpen(true), [])
  const closeAddLinkDialog = useCallback(() => setAddLinkOpen(false), [])
  const openArchiveDialog = useCallback(() => setArchiveDialogOpen(true), [])
  const closeArchiveDialog = useCallback(() => setArchiveDialogOpen(false), [])
  const closeConversionDialog = useCallback(() => setConvertingLink(null), [])

  const onBrowsePick = useCallback((type: BrowsePickKind, id: string) => {
    if (!navigateRoute({ kind: 'library', id: 'reading' })) return
    setSelection({ type, id, name: type === 'tag' ? '#' + id : id })
  }, [navigateRoute, setSelection])

  const onAddLinkAdded = useCallback((target: AddLinkTarget) => {
    invalidateLibrary()
    setAddLinkOpen(false)
    if (target.kind === 'inbox') {
      refreshPendingInboxCount()
      navigateRoute({ kind: 'library', id: 'pending', inboxId: target.id })
      return
    }
    navigateRoute({ kind: 'library', id: 'reading' }, { linkId: target.id })
    setSelection({ type: 'smart', id: 'all', name: '全部链接' })
    void reloadList()
  }, [navigateRoute, reloadList, setSelection])

  const conversionInitialNote = useMemo(
    () => anns.map((annotation) => annotation.note.trim()).filter(Boolean).join('\n\n'),
    [anns],
  )

  const onConversionConverted = useCallback(() => {
    if (!convertingLink || !capabilityLease.isCurrent('siteWrite')) return
    invalidateLibrary()
    invalidateLink(convertingLink.id)
    setConvertingLink(null)
    clearActiveResource()
    navigateRoute({ kind: 'library', id: 'sites' })
    void reloadList()
  }, [capabilityLease, clearActiveResource, convertingLink, navigateRoute, reloadList])

  const onSaveNotePanel = useCallback(async (value: string) => {
    if (!editingAnn || historicalNote) return
    if (await updateAnnotation(editingAnn, { note: value.trim() })) {
      setNoteEd(null)
      flash('笔记已保存', 'marker')
    } else {
      flash('保存笔记失败，请重试', 'alert')
    }
  }, [editingAnn, flash, historicalNote, updateAnnotation])

  const onDeleteNotePanel = useCallback(async () => {
    if (!editingAnn || historicalNote) return
    if (await removeAnnotation(editingAnn)) setNoteEd(null)
  }, [editingAnn, historicalNote, removeAnnotation])

  const onAskAINotePanel = useCallback(async (
    annotation: AnnotationLocator,
    text: string,
    draftValue: string,
  ) => {
    if (!editingAnn || historicalNote) return
    // 切去问 AI 前把未保存草稿写回，不丢字。
    if (draftValue != null && draftValue.trim() !== (editingAnn.note || '')) {
      if (!await updateAnnotation(editingAnn, { note: draftValue.trim() })) {
        flash('保存草稿失败，请重试', 'alert')
        return
      }
    }
    onAskAI(annotation, text)
  }, [editingAnn, flash, historicalNote, onAskAI, updateAnnotation])

  const closeNotePanel = useCallback(() => {
    setNoteEd(null)
    setHistoricalNote(null)
  }, [])

  return {
    theme,
    onToggleTheme: toggleTheme,
    homeScrollRef,
    todoCompletedExpanded,
    setTodoCompletedExpanded,
    chatOpen,
    closeChat,
    sidebarCollapsed,
    focusMode,
    pins,
    onTogglePin: togglePin,
    addLinkOpen,
    openAddLinkDialog,
    closeAddLinkDialog,
    onAddLinkAdded,
    cmdkOpen,
    openCommandPalette,
    closeCommandPalette,
    browse,
    closeBrowse,
    onBrowsePick,
    notePanelAnnotation,
    notePanelReadOnly: historicalNote !== null,
    notePanelEditing: editingAnn !== null,
    chatDraft,
    clearChatDraft,
    updateReady,
    applyUpdate: applyServiceWorkerUpdate,
    archiveDialogOpen,
    openArchiveDialog,
    closeArchiveDialog,
    convertingLink,
    conversionInitialNote,
    closeConversionDialog,
    onConversionConverted,
    canCreateNote,
    creatingNote,
    createEmptyNote,
    openSettings,
    toggleChat,
    openNote,
    openHistoricalAnnotation,
    onAskAI,
    onCommand,
    onAdoptNote,
    onSidebarSelect,
    onSidebarView,
    onSidebarBrowse,
    toggleNavigation,
    backToMobileList,
    onPickTag,
    onToggleFocus,
    onConvertToSite,
    onSaveNotePanel,
    onDeleteNotePanel,
    onAskAINotePanel,
    closeNotePanel,
  }
}
