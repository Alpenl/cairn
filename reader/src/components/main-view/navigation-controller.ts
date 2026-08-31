import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type MutableRefObject,
  type SetStateAction,
} from 'react'
import type { ContentEditState } from '../DetailPane'
import type { IconName } from '../Icon'
import type { InboxDraftLeaveState } from '../reader-vnext/InboxSurface'
import type { ToastAction } from '../Toast'
import {
  firstAvailableReaderRoute,
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from '../../lib/capabilities'
import type { IdentityLease } from '../../lib/identity'
import { ReaderNavigationGuardRegistry } from '../../lib/navigation/guard'
import {
  ensureReaderHistoryEntry,
  installReaderNavigationGuard,
  notifyReaderNavigationCommitted,
  parseReaderRoute,
  READER_NAVIGATION_RESTORED_EVENT,
  rememberReaderRoute,
  readerHistoryState,
  readerRouteURL,
  readerThoughtIDFromURL,
  readerThoughtViewFromURL,
  type ReaderRoute,
  type ReaderRouteTargets,
} from '../../lib/navigation/route'

export type MainViewRoute =
  | 'home'
  | 'feed'
  | 'pending'
  | 'reading'
  | 'sites'
  | 'subs'
  | 'notes'
  | 'todo'
  | 'settings'
  | 'history'
  | 'trash'

interface MainViewRouteTargets extends ReaderRouteTargets {
  readonly inboxId?: string
}

export interface OpenLinkOptions {
  readonly history?: 'push' | 'none'
  readonly address?: boolean
  readonly guard?: boolean
}

type NavigationDecision = boolean | Promise<boolean>
type PrepareNotesLeave = () => Promise<
  { readonly status: 'ready' } | { readonly status: 'blocked'; readonly code: string }
>

interface UseMainViewNavigationOptions {
  readonly lease: IdentityLease
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly flash: (msg: string, icon?: IconName, action?: ToastAction) => void
}

interface MainViewNavigationController {
  readonly view: MainViewRoute
  readonly displayedView: MainViewRoute
  readonly siteTargetID: string | undefined
  readonly noteTargetID: string | undefined
  readonly inboxTargetID: string | undefined
  readonly activeId: string | null
  readonly setActiveId: (value: string | null) => void
  readonly mobilePane: 'list' | 'detail'
  readonly setMobilePane: (value: 'list' | 'detail') => void
  readonly mobileNavOpen: boolean
  readonly setMobileNavOpen: Dispatch<SetStateAction<boolean>>
  readonly contentEditState: ContentEditState | null
  readonly navigationRestoreEpoch: number
  readonly pendingLinkTarget: MutableRefObject<string | null>
  readonly getContentEditState: () => ContentEditState | null
  readonly reportContentEditState: (next: ContentEditState | null) => void
  readonly confirmDiscardContentEdit: () => boolean
  readonly confirmDiscardNavigation: () => NavigationDecision
  readonly commitRoute: (
    route: ReaderRoute,
    targets?: ReaderRouteTargets,
    historyMode?: 'push' | 'replace' | 'none',
    addressLink?: boolean,
  ) => void
  readonly navigateRoute: (route: ReaderRoute, targets?: ReaderRouteTargets) => boolean | Promise<boolean>
  readonly reportNotesDraftDirty: (dirty: boolean) => void
  readonly reportNotesPendingPersistence: (pending: boolean) => void
  readonly reportNotesPrepareToLeave: (prepare: PrepareNotesLeave | null) => void
  readonly reportInboxDraftState: (state: InboxDraftLeaveState) => void
}

function sameContentEditState(
  left: ContentEditState | null,
  right: ContentEditState | null,
): boolean {
  return left?.linkId === right?.linkId &&
    left?.expectedRevision === right?.expectedRevision &&
    left?.editing === right?.editing &&
    left?.dirty === right?.dirty &&
    left?.saving === right?.saving
}

export function mainViewFromRoute(route: ReaderRoute): MainViewRoute {
  if (route.kind === 'surface') return route.id
  if (route.kind === 'tool') return route.id
  if (route.kind === 'library') {
    if (route.id === 'pending') return 'pending'
    if (route.id === 'sites') return 'sites'
    if (route.id === 'subs') return 'subs'
    if (route.id === 'reading') return 'reading'
    if (route.id === 'notes') return 'notes'
  }
  return 'reading'
}

function readerRouteForMainView(view: MainViewRoute, targets: MainViewRouteTargets = {}): ReaderRoute {
  if (view === 'home') return { kind: 'surface', id: 'home' }
  if (view === 'feed') return { kind: 'surface', id: 'feed' }
  if (view === 'pending') return { kind: 'library', id: 'pending', inboxId: targets.inboxId }
  if (view === 'notes') return { kind: 'library', id: 'notes' }
  if (view === 'todo') return { kind: 'tool', id: 'todo' }
  if (view === 'settings') return { kind: 'tool', id: 'settings' }
  if (view === 'history') return { kind: 'tool', id: 'history' }
  if (view === 'sites') return { kind: 'library', id: 'sites' }
  if (view === 'subs') return { kind: 'library', id: 'subs' }
  return { kind: 'library', id: 'reading' }
}

function siteIDFromLocation(): string | undefined {
  const route = parseReaderRoute(window.location.href)
  if (route.kind !== 'library' || route.id !== 'sites') return undefined
  const siteID = new URLSearchParams(window.location.search).get('site_id')?.trim()
  return siteID || undefined
}

function noteIDFromLocation(): string | undefined {
  const route = parseReaderRoute(window.location.href)
  if (route.kind !== 'library' || route.id !== 'notes') return undefined
  const noteID = new URLSearchParams(window.location.search).get('note_id')?.trim()
  return noteID || undefined
}

function inboxIDFromLocation(): string | undefined {
  const route = parseReaderRoute(window.location.href)
  if (route.kind !== 'library' || route.id !== 'pending') return undefined
  return route.inboxId?.trim() || undefined
}

function linkIDFromLocation(): string | undefined {
  const route = parseReaderRoute(window.location.href)
  if (route.kind !== 'library' || route.id !== 'reading') return undefined
  const linkID = new URLSearchParams(window.location.search).get('link_id')?.trim()
  return linkID || undefined
}

export function useMainViewNavigation({
  lease,
  capabilityPolicy,
  flash,
}: UseMainViewNavigationOptions): MainViewNavigationController {
  const initialRoute = useMemo(
    () => parseReaderRoute(window.location.href, undefined, { identity: lease.context }),
    [lease],
  )
  const initialLinkTargetID = linkIDFromLocation()
  const [view, setView] = useState<MainViewRoute>(() => mainViewFromRoute(initialRoute))
  const [siteTargetID, setSiteTargetID] = useState<string | undefined>(siteIDFromLocation)
  const [noteTargetID, setNoteTargetID] = useState<string | undefined>(noteIDFromLocation)
  const [inboxTargetID, setInboxTargetID] = useState<string | undefined>(() => (
    initialRoute.kind === 'library' && initialRoute.id === 'pending'
      ? initialRoute.inboxId
      : inboxIDFromLocation()
  ))
  const [activeId, setActiveId] = useState<string | null>(initialLinkTargetID ?? null)
  const [activeLinkAddressed, setActiveLinkAddressed] = useState(Boolean(initialLinkTargetID))
  const [mobilePane, setMobilePane] = useState<'list' | 'detail'>('list')
  const [mobileNavOpen, setMobileNavOpen] = useState(false)
  const [contentEditState, setContentEditState] = useState<ContentEditState | null>(null)
  const [navigationRestoreEpoch, setNavigationRestoreEpoch] = useState(0)
  const navigationGuards = useMemo(() => new ReaderNavigationGuardRegistry(), [])
  const contentEditStateRef = useRef<ContentEditState | null>(null)
  const notesDraftDirtyRef = useRef(false)
  const notesPendingPersistenceRef = useRef(false)
  const notesPrepareToLeaveRef = useRef<PrepareNotesLeave | null>(null)
  const inboxDraftStateRef = useRef<InboxDraftLeaveState>({ dirty: false, saving: false })
  const pendingLinkTarget = useRef<string | null>(initialLinkTargetID ?? null)

  const requestedRoute = useMemo(
    () => readerRouteForMainView(view, { inboxId: inboxTargetID }),
    [inboxTargetID, view],
  )
  const requestedRouteAvailable = readerRouteIsAvailable(requestedRoute, capabilityPolicy)
  const effectiveRoute = useMemo(
    () => requestedRouteAvailable ? requestedRoute : firstAvailableReaderRoute(capabilityPolicy),
    [capabilityPolicy, requestedRoute, requestedRouteAvailable],
  )
  const displayedView = mainViewFromRoute(effectiveRoute)

  useEffect(() => {
    const targets = requestedRouteAvailable ? {
      linkId: view === 'reading' && activeLinkAddressed ? activeId ?? undefined : undefined,
      siteId: view === 'sites' ? siteTargetID : undefined,
      noteId: view === 'notes' ? noteTargetID : undefined,
      thoughtView: view === 'history' ? readerThoughtViewFromURL(window.location.href) : undefined,
      thoughtId: view === 'history' ? readerThoughtIDFromURL(window.location.href) : undefined,
    } : {}
    const canonical = readerRouteURL(effectiveRoute, window.location.href, targets)
    if (canonical.href !== window.location.href) {
      window.history.replaceState(window.history.state, '', canonical)
    }
    rememberReaderRoute(effectiveRoute, undefined, targets, lease.context)
  }, [
    activeId,
    activeLinkAddressed,
    effectiveRoute,
    lease,
    noteTargetID,
    requestedRouteAvailable,
    siteTargetID,
    view,
  ])

  useEffect(() => {
    const restoreView = () => {
      const route = parseReaderRoute(window.location.href, undefined, { identity: lease.context })
      const nextLinkID = linkIDFromLocation()
      setView(mainViewFromRoute(route))
      setSiteTargetID(siteIDFromLocation())
      setNoteTargetID(noteIDFromLocation())
      setInboxTargetID(inboxIDFromLocation())
      setActiveId(nextLinkID ?? null)
      setActiveLinkAddressed(Boolean(nextLinkID))
      pendingLinkTarget.current = nextLinkID ?? null
    }
    window.addEventListener('popstate', restoreView)
    return () => window.removeEventListener('popstate', restoreView)
  }, [lease])

  const reportContentEditState = useCallback((next: ContentEditState | null) => {
    contentEditStateRef.current = next
    setContentEditState((current) => sameContentEditState(current, next) ? current : next)
  }, [])

  const getContentEditState = useCallback(() => contentEditStateRef.current, [])

  const confirmDiscardContentEdit = useCallback(() => {
    const current = contentEditStateRef.current
    if (!current?.editing) return true
    if (current.saving) {
      flash('正文正在保存，请稍候', 'clock')
      return false
    }
    if (!current.dirty) return true
    return window.confirm('当前正文有未保存修改，确定放弃？')
  }, [flash])

  const confirmDiscardInboxDraft = useCallback(() => {
    if (inboxDraftStateRef.current.saving) {
      flash('收件箱草稿正在保存，请稍候', 'clock')
      return false
    }
    if (!inboxDraftStateRef.current.dirty) return true
    return window.confirm('当前收件箱条目的草稿有未保存修改，确定离开？')
  }, [flash])

  const confirmDiscardNavigation = useCallback(() => {
    return navigationGuards.requestNavigation()
  }, [navigationGuards])

  useEffect(() => navigationGuards.register('saved-content', {
    blocksNavigation: () => Boolean(
      contentEditStateRef.current?.editing &&
      (contentEditStateRef.current.dirty || contentEditStateRef.current.saving),
    ),
    requestNavigation: confirmDiscardContentEdit,
  }), [confirmDiscardContentEdit, navigationGuards])

  useEffect(() => navigationGuards.register('notes', {
    blocksNavigation: () => notesDraftDirtyRef.current,
    requestNavigation: () => notesPrepareToLeaveRef.current?.().then((result) => result.status === 'ready') ?? true,
  }), [navigationGuards])

  useEffect(() => navigationGuards.register('inbox', {
    blocksNavigation: () => inboxDraftStateRef.current.dirty || inboxDraftStateRef.current.saving,
    requestNavigation: confirmDiscardInboxDraft,
  }), [confirmDiscardInboxDraft, navigationGuards])

  const reportNotesDraftDirty = useCallback((dirty: boolean) => {
    notesDraftDirtyRef.current = dirty
  }, [])

  const reportNotesPendingPersistence = useCallback((pending: boolean) => {
    notesPendingPersistenceRef.current = pending
  }, [])

  const reportNotesPrepareToLeave = useCallback((prepare: PrepareNotesLeave | null) => {
    notesPrepareToLeaveRef.current = prepare
  }, [])

  const reportInboxDraftState = useCallback((state: InboxDraftLeaveState) => {
    inboxDraftStateRef.current = state
  }, [])

  useEffect(() => {
    return installReaderNavigationGuard(confirmDiscardNavigation)
  }, [confirmDiscardNavigation])

  useEffect(() => {
    const restoreDraftUI = () => setNavigationRestoreEpoch((current) => current + 1)
    window.addEventListener(READER_NAVIGATION_RESTORED_EVENT, restoreDraftUI)
    return () => window.removeEventListener(READER_NAVIGATION_RESTORED_EVENT, restoreDraftUI)
  }, [])

  const applyRouteState = useCallback((route: ReaderRoute, targets: ReaderRouteTargets, addressLink: boolean) => {
    const nextView = mainViewFromRoute(route)
    const nextLinkID = route.kind === 'library' && route.id === 'reading'
      ? targets.linkId?.trim() || undefined
      : undefined
    setView(nextView)
    setSiteTargetID(route.kind === 'library' && route.id === 'sites' ? targets.siteId?.trim() || undefined : undefined)
    setNoteTargetID(route.kind === 'library' && route.id === 'notes' ? targets.noteId?.trim() || undefined : undefined)
    setInboxTargetID(route.kind === 'library' && route.id === 'pending' ? route.inboxId?.trim() || undefined : undefined)
    setActiveId(nextLinkID ?? null)
    setActiveLinkAddressed(Boolean(nextLinkID) && addressLink)
    pendingLinkTarget.current = nextLinkID && addressLink ? nextLinkID : null
    setMobilePane('list')
    setMobileNavOpen(false)
  }, [])

  const commitRoute = useCallback((
    route: ReaderRoute,
    targets: ReaderRouteTargets = {},
    historyMode: 'push' | 'replace' | 'none' = 'push',
    addressLink = Boolean(targets.linkId),
  ) => {
    const url = readerRouteURL(route, window.location.href, targets)
    if (historyMode === 'push') {
      const currentIndex = ensureReaderHistoryEntry()
      if (url.href !== window.location.href) {
        window.history.pushState(readerHistoryState(window.history.state, currentIndex + 1), '', url)
      }
    } else if (historyMode === 'replace') {
      ensureReaderHistoryEntry()
      window.history.replaceState(window.history.state, '', url)
    }
    if (historyMode !== 'none') rememberReaderRoute(route, undefined, targets, lease.context)
    applyRouteState(route, targets, addressLink)
    notifyReaderNavigationCommitted()
  }, [applyRouteState, lease])

  const navigateRoute = useCallback((route: ReaderRoute, targets?: ReaderRouteTargets): boolean | Promise<boolean> => {
    if (!readerRouteIsAvailable(route, capabilityPolicy)) return false
    const commit = () => {
      commitRoute(route, targets ?? {}, 'push', Boolean(targets?.linkId))
      return true
    }
    const result = confirmDiscardNavigation()
    if (typeof (result as Promise<boolean>)?.then === 'function') {
      return Promise.resolve(result).then((allowed) => allowed ? commit() : false)
    }
    return result ? commit() : false
  }, [capabilityPolicy, commitRoute, confirmDiscardNavigation])

  useEffect(() => {
    if (requestedRouteAvailable) return
    commitRoute(effectiveRoute, {}, 'replace', false)
  }, [commitRoute, effectiveRoute, requestedRouteAvailable])

  useEffect(() => {
    if (!capabilityPolicy.notes) {
      setNoteTargetID(undefined)
      notesDraftDirtyRef.current = false
      notesPendingPersistenceRef.current = false
      notesPrepareToLeaveRef.current = null
    }
    if (!capabilityPolicy.inbox) {
      setInboxTargetID(undefined)
      inboxDraftStateRef.current = { dirty: false, saving: false }
    }
    if (!capabilityPolicy.siteRead) setSiteTargetID(undefined)
  }, [capabilityPolicy])

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      const contentPending = Boolean(
        contentEditStateRef.current?.editing &&
        (contentEditStateRef.current.dirty || contentEditStateRef.current.saving),
      )
      const inboxPending = inboxDraftStateRef.current.dirty || inboxDraftStateRef.current.saving
      if (!contentPending && !notesPendingPersistenceRef.current && !inboxPending) return
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  return {
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
  }
}
