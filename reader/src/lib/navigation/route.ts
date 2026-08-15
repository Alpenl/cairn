export type ReaderLibraryId = 'pending' | 'reading' | 'sites' | 'subs' | 'notes'

export type ReaderStartupPreference = 'last' | 'home' | 'reading'
export type ReaderThoughtView = 'live' | 'history' | 'superseded'

export interface ReaderRouteIdentity {
  readonly physicalNamespace: string
}

export type ReaderRoute =
  | { readonly kind: 'surface'; readonly id: 'home' | 'feed' }
  | { readonly kind: 'library'; readonly id: 'pending'; readonly inboxId?: string }
  | { readonly kind: 'library'; readonly id: Exclude<ReaderLibraryId, 'pending'> }
  | { readonly kind: 'tool'; readonly id: 'todo' | 'settings' | 'history' }
  | { readonly kind: 'internal'; readonly id: 'review'; readonly reviewId?: string }

export const READER_LAST_LOCATION_STORAGE_KEY = 'webtag:reader:last-location:v1'
export const READER_LAST_LOCATION_STORAGE_PREFIX = 'webtag:reader:last-location:v2:'
export const READER_STARTUP_PREFERENCE_STORAGE_KEY = 'webtag:reader:startup-preference:v1'
export const READER_HISTORY_INDEX_KEY = '__webtag_reader_history_index'
export const READER_NAVIGATION_COMMIT_EVENT = 'webtag:reader-navigation-committed'
export const READER_NAVIGATION_RESTORED_EVENT = 'webtag:reader-navigation-restored'

const LIBRARY_ALIASES: Record<string, ReaderLibraryId> = {
  pending: 'pending',
  inbox: 'pending',
  reading: 'reading',
  read: 'reading',
  sites: 'sites',
  site: 'sites',
  subs: 'subs',
  subscription: 'subs',
  subscriptions: 'subs',
  notes: 'notes',
  note: 'notes',
}

const ROUTE_CHANNELS = ['surface', 'view', 'tool'] as const

function normalizeRouteValue(value: string | null): string | null {
  if (value === null) return null
  return value.trim().toLowerCase()
}

function normalizeRouteTarget(value: string | null): string | null {
  const normalized = value?.trim()
  return normalized && normalized.length <= 512 && !normalized.includes('\0')
    ? normalized
    : null
}

function routeFromValue(value: string | null, reviewId: string | null, inboxId: string | null = null): ReaderRoute | null {
  const normalizedValue = normalizeRouteValue(value)
  if (!normalizedValue) return null
  if (normalizedValue === 'home') return { kind: 'surface', id: 'home' }
  if (normalizedValue === 'feed') return { kind: 'surface', id: 'feed' }
  if (normalizedValue === 'todo' || normalizedValue === 'todos') return { kind: 'tool', id: 'todo' }
  if (normalizedValue === 'settings' || normalizedValue === 'setting') return { kind: 'tool', id: 'settings' }
  if (normalizedValue === 'history' || normalizedValue === 'thought-history') return { kind: 'tool', id: 'history' }
  if (normalizedValue === 'review') {
    const normalizedReviewId = normalizeRouteTarget(reviewId)
    return normalizedReviewId
      ? { kind: 'internal', id: 'review', reviewId: normalizedReviewId }
      : { kind: 'internal', id: 'review' }
  }
  const library = LIBRARY_ALIASES[normalizedValue]
  if (!library) return null
  const normalizedInboxId = normalizeRouteTarget(inboxId)
  return library === 'pending' && normalizedInboxId
    ? { kind: 'library', id: 'pending', inboxId: normalizedInboxId }
    : { kind: 'library', id: library }
}

function browserStorage(): Storage | null {
  if (typeof window === 'undefined') return null
  try {
    const globals = window as unknown as Record<string, unknown>
    const storage = globals['localStorage']
    return storage && typeof storage === 'object' ? storage as Storage : null
  } catch {
    return null
  }
}

function historyStateRecord(state: unknown): Record<string, unknown> {
  return state && typeof state === 'object' && !Array.isArray(state)
    ? { ...(state as Record<string, unknown>) }
    : {}
}

export function readerHistoryIndex(state: unknown): number | undefined {
  if (!state || typeof state !== 'object' || Array.isArray(state)) return undefined
  const value = (state as Record<string, unknown>)[READER_HISTORY_INDEX_KEY]
  return typeof value === 'number' && Number.isSafeInteger(value) ? value : undefined
}

export function readerHistoryState(state: unknown, index: number): Record<string, unknown> {
  return { ...historyStateRecord(state), [READER_HISTORY_INDEX_KEY]: index }
}

export function ensureReaderHistoryEntry(): number {
  if (typeof window === 'undefined') return 0
  const current = readerHistoryIndex(window.history.state)
  if (current !== undefined) return current
  window.history.replaceState(
    readerHistoryState(window.history.state, 0),
    '',
    window.location.href,
  )
  return 0
}

/**
 * Tell capture-phase traversal guards that the coordinator has committed a
 * new canonical route. Components never write history themselves, so this is
 * the one point where a later rejected Back/Forward traversal learns the
 * entry it must restore.
 */
export function notifyReaderNavigationCommitted(): void {
  if (typeof window === 'undefined') return
  window.dispatchEvent(new Event(READER_NAVIGATION_COMMIT_EVENT))
}

function routeFromURL(url: URL): ReaderRoute | null {
  const channel = ROUTE_CHANNELS.find((name) => url.searchParams.has(name))
  const value = channel ? url.searchParams.get(channel) : null
  return routeFromValue(value, url.searchParams.get('review_id'), url.searchParams.get('inbox_id'))
}

export function readerThoughtViewFromURL(input: URL | string): ReaderThoughtView {
  const url = typeof input === 'string' ? new URL(input, 'https://reader.invalid/') : input
  const view = url.searchParams.get('thought_view')?.trim().toLowerCase()
  return view === 'live' || view === 'superseded' ? view : 'history'
}

export function readerThoughtIDFromURL(input: URL | string): string | undefined {
  const url = typeof input === 'string' ? new URL(input, 'https://reader.invalid/') : input
  const id = url.searchParams.get('thought_id')?.trim()
  return id && id.length <= 256 && !id.includes('\0') ? id : undefined
}

interface StoredReaderLocation {
  readonly route: ReaderRoute
  readonly targets: ReaderRouteTargets
}

function rememberedRouteStorageKey(identity?: ReaderRouteIdentity): string | null {
  const namespace = identity?.physicalNamespace.trim()
  if (!namespace || namespace.includes('\0')) return null
  return `${READER_LAST_LOCATION_STORAGE_PREFIX}${encodeURIComponent(namespace)}`
}

export function readerLastLocationStorageKey(identity: ReaderRouteIdentity): string {
  const key = rememberedRouteStorageKey(identity)
  if (!key) throw new Error('Reader route identity is invalid')
  return key
}

function normalizedStoredTarget(url: URL, key: string): string | undefined {
  const value = url.searchParams.get(key)?.trim()
  return value && value.length <= 512 && !value.includes('\0') ? value : undefined
}

function storedRouteTargets(route: ReaderRoute, url: URL): ReaderRouteTargets {
  if (route.kind === 'library' && route.id === 'reading') {
    return { linkId: normalizedStoredTarget(url, 'link_id') }
  }
  if (route.kind === 'library' && route.id === 'sites') {
    return { siteId: normalizedStoredTarget(url, 'site_id') }
  }
  if (route.kind === 'library' && route.id === 'notes') {
    return { noteId: normalizedStoredTarget(url, 'note_id') }
  }
  if (route.kind === 'tool' && route.id === 'history') {
    return {
      contentHistoryLinkId: normalizedStoredTarget(url, 'content_history_link_id'),
      thoughtView: readerThoughtViewFromURL(url),
      thoughtId: readerThoughtIDFromURL(url),
    }
  }
  return {}
}

function readStoredLocation(
  storage: Storage | null,
  identity?: ReaderRouteIdentity,
): StoredReaderLocation | null {
  const key = rememberedRouteStorageKey(identity)
  if (!storage || !key) return null
  try {
    const raw = storage.getItem(key)?.trim()
    if (!raw) return null
    const url = new URL(raw, 'https://reader.invalid/')
    const route = routeFromURL(url)
    return route ? { route, targets: storedRouteTargets(route, url) } : null
  } catch {
    return null
  }
}

function normalizeStartupPreference(value: string | null): ReaderStartupPreference | null {
  const normalized = value?.trim().toLowerCase()
  if (normalized === 'home' || normalized === 'open_home' || normalized === 'force_home') return 'home'
  if (normalized === 'reading' || normalized === 'force_reading') return 'reading'
  if (normalized === 'last' || normalized === 'last_location' || normalized === 'remember') return 'last'
  return null
}

export function readReaderStartupPreference(storage: Storage | null = browserStorage()): ReaderStartupPreference {
  if (!storage) return 'last'
  try {
    return normalizeStartupPreference(storage.getItem(READER_STARTUP_PREFERENCE_STORAGE_KEY)) ?? 'last'
  } catch {
    return 'last'
  }
}

export function writeReaderStartupPreference(
  preference: ReaderStartupPreference,
  storage: Storage | null = browserStorage(),
): boolean {
  if (!storage) return false
  try {
    storage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, preference)
    return true
  } catch {
    return false
  }
}

export function rememberReaderRoute(
  route: ReaderRoute,
  storage: Storage | null = browserStorage(),
  targets?: ReaderRouteTargets,
  identity?: ReaderRouteIdentity,
): boolean {
  const key = rememberedRouteStorageKey(identity)
  if (!storage || !key) return false
  try {
    const base = route.kind === 'tool' && route.id === 'history' && typeof window !== 'undefined'
      ? window.location.href
      : undefined
    storage.setItem(key, readerRouteURL(route, base, targets).search)
    return true
  } catch {
    return false
  }
}

export function resolveReaderStartupRoute(
  storage: Storage | null = browserStorage(),
  identity?: ReaderRouteIdentity,
): ReaderRoute {
  if (!rememberedRouteStorageKey(identity)) return { kind: 'surface', id: 'home' }
  const preference = readReaderStartupPreference(storage)
  if (preference === 'home') return { kind: 'surface', id: 'home' }
  if (preference === 'reading') return { kind: 'library', id: 'reading' }
  return readStoredLocation(storage, identity)?.route ?? { kind: 'surface', id: 'home' }
}

export interface ParseReaderRouteOptions {
  readonly persist?: boolean
  readonly storage?: Storage | null
  readonly identity?: ReaderRouteIdentity
}

export interface ReaderRouteTargets {
  readonly linkId?: string
  readonly siteId?: string
  readonly noteId?: string
  readonly contentHistoryLinkId?: string
  readonly thoughtView?: ReaderThoughtView
  readonly thoughtId?: string
}

export interface ReaderThoughtHostReference {
  readonly host_kind: string
  readonly host_id: string
  readonly link_id?: string | null
}

export interface ReaderNavigationTarget {
  readonly route: ReaderRoute
  readonly targets?: ReaderRouteTargets
}

/** Resolve every thought entry point through the same canonical host route. */
export function readerThoughtHostTarget(
  thought: ReaderThoughtHostReference,
): ReaderNavigationTarget | null {
  const hostKind = thought.host_kind.trim().toLowerCase()
  const hostID = thought.host_id.trim()
  if (hostKind === 'note') {
    return hostID
      ? { route: { kind: 'library', id: 'notes' }, targets: { noteId: hostID } }
      : null
  }
  if (hostKind === 'inbox') {
    return hostID
      ? { route: { kind: 'library', id: 'pending', inboxId: hostID } }
      : null
  }
  if (hostKind === 'link') {
    const linkID = thought.link_id?.trim() || hostID
    return linkID
      ? { route: { kind: 'library', id: 'reading' }, targets: { linkId: linkID } }
      : null
  }
  return null
}

export type ReaderNavigationRequest = (
  route: ReaderRoute,
  targets?: ReaderRouteTargets,
) => boolean | void | Promise<boolean>

/**
 * RC-01A draft parser. Unknown values fail closed through the startup policy
 * to Home; aliases never leak into the returned discriminated union.
 */
export function parseReaderRoute(
  input: URL | string,
  // Retained for source compatibility. Startup fallback is deliberately
  // governed only by preference, stored location, and the Home default.
  _fallback: ReaderRoute = { kind: 'surface', id: 'home' },
  options: ParseReaderRouteOptions = {},
): ReaderRoute {
  const url = typeof input === 'string' ? new URL(input, 'https://reader.invalid/') : input
  const parsed = routeFromURL(url)
  if (parsed) {
    if (options.persist) {
      rememberReaderRoute(parsed, options.storage ?? browserStorage(), storedRouteTargets(parsed, url), options.identity)
    }
    return parsed
  }
  const storage = options.storage ?? browserStorage()
  const preference = options.identity ? readReaderStartupPreference(storage) : null
  const stored = preference === 'last' ? readStoredLocation(storage, options.identity) : null
  const resolved = preference === 'home'
    ? { kind: 'surface', id: 'home' } as const
    : preference === 'reading'
      ? { kind: 'library', id: 'reading' } as const
      : stored?.route ?? { kind: 'surface', id: 'home' } as const

  // MainView reads target ids separately from the browser URL during its
  // initial state construction. Restore only the current installation's
  // canonical remembered search before that render; replaceState keeps
  // startup restoration out of Back history.
  if (
    options.storage === undefined &&
    typeof window !== 'undefined' &&
    url.href === window.location.href &&
    stored
  ) {
    window.history.replaceState(
      window.history.state,
      '',
      readerRouteURL(stored.route, window.location.href, stored.targets),
    )
  }
  return resolved
}

export function readerRouteURL(
  route: ReaderRoute,
  base: URL | string = 'https://reader.invalid/',
  targets: ReaderRouteTargets = {},
): URL {
  const url = typeof base === 'string'
    ? new URL(base, 'https://reader.invalid/')
    : new URL(base.href)
  const existingThoughtView = url.searchParams.get('thought_view')?.trim().toLowerCase()
  const thoughtView = targets.thoughtView ?? existingThoughtView
  url.search = ''
  if (route.kind === 'surface') {
    url.searchParams.set('surface', route.id)
  } else if (route.kind === 'library') {
    url.searchParams.set('view', route.id)
    if (route.id === 'pending') {
      const inboxId = route.inboxId?.trim()
      if (inboxId) url.searchParams.set('inbox_id', inboxId)
    }
  } else if (route.kind === 'tool') {
    url.searchParams.set('tool', route.id)
    if (route.id === 'history' &&
      (thoughtView === 'history' || thoughtView === 'live' || thoughtView === 'superseded')) {
      url.searchParams.set('thought_view', thoughtView)
    }
  } else {
    url.searchParams.set('view', 'review')
    const reviewId = route.reviewId?.trim()
    if (reviewId) url.searchParams.set('review_id', reviewId)
  }
  const setTarget = (key: string, value: string | undefined) => {
    const normalized = value?.trim()
    if (normalized) url.searchParams.set(key, normalized)
  }
  if (route.kind === 'library' && route.id === 'reading') {
    setTarget('link_id', targets.linkId)
  } else if (route.kind === 'library' && route.id === 'sites') {
    setTarget('site_id', targets.siteId)
  } else if (route.kind === 'library' && route.id === 'notes') {
    setTarget('note_id', targets.noteId)
  } else if (route.kind === 'tool' && route.id === 'history') {
    setTarget('content_history_link_id', targets.contentHistoryLinkId)
    setTarget('thought_id', targets.thoughtId)
  }
  return url
}

export function sameReaderRoute(left: ReaderRoute, right: ReaderRoute): boolean {
  if (left.kind !== right.kind || left.id !== right.id) return false
  if (left.kind === 'internal' && right.kind === 'internal') {
    return (left.reviewId?.trim() || undefined) === (right.reviewId?.trim() || undefined)
  }
  if (left.kind === 'library' && right.kind === 'library' && left.id === 'pending' && right.id === 'pending') {
    return (left.inboxId?.trim() || undefined) === (right.inboxId?.trim() || undefined)
  }
  return true
}

/**
 * Install a capture-phase history guard for editors that own unsaved state.
 *
 * `MainView` listens for `popstate` to switch its view. An editor rendered
 * below it therefore needs to observe the event before that listener runs;
 * otherwise Back/Forward can unmount the editor before it has a chance to
 * ask whether the draft may be discarded. A blocked traversal is restored to
 * the URL that was active when the guard was installed. Programmatic route
 * actions should still use `onBeforeNavigate` at the component boundary.
 */
export function installReaderNavigationGuard(guard: () => boolean | Promise<boolean>): () => void {
  if (typeof window === 'undefined') return () => undefined

  let currentIndex = ensureReaderHistoryEntry()
  let restoring = false
  let replaying = false
  const rememberCommittedEntry = () => {
    currentIndex = ensureReaderHistoryEntry()
  }
  const onPopState = (event: PopStateEvent) => {
    if (replaying) return
    if (restoring) {
      restoring = false
      currentIndex = readerHistoryIndex(event.state) ?? currentIndex
      event.stopImmediatePropagation()
      window.dispatchEvent(new Event(READER_NAVIGATION_RESTORED_EVENT))
      return
    }

    const targetIndex = readerHistoryIndex(event.state) ?? readerHistoryIndex(window.history.state)
    event.stopImmediatePropagation()
    // Browser traversal has already changed location when popstate fires. Keep
    // the editor mounted while an async guard flushes its durable state, then
    // replay this event only after it permits the transition.
    const settle = (allowed: boolean) => {
        if (allowed) {
          if (targetIndex !== undefined) currentIndex = targetIndex
          replaying = true
          window.dispatchEvent(new PopStateEvent('popstate', { state: event.state }))
          replaying = false
          return
        }
        if (targetIndex !== undefined && targetIndex !== currentIndex) {
          restoring = true
          try {
            window.history.go(currentIndex - targetIndex)
            return
          } catch {
            restoring = false
          }
        }
      }
    try {
      const result = guard()
      if (typeof (result as Promise<boolean>)?.then === 'function') {
        void Promise.resolve(result).then(settle, () => settle(false))
      } else {
        settle(result as boolean)
      }
    } catch {
      settle(false)
    }
    // A Reader-owned entry is always indexed. Do not replace an unmarked
    // target here: doing so would destroy the destination the user attempted
    // to traverse to. The coordinator indexes every committed Reader route,
    // so this path only protects foreign history from accidental rewriting.
  }

  window.addEventListener('popstate', onPopState, true)
  window.addEventListener(READER_NAVIGATION_COMMIT_EVENT, rememberCommittedEntry)
  return () => {
    window.removeEventListener('popstate', onPopState, true)
    window.removeEventListener(READER_NAVIGATION_COMMIT_EVENT, rememberCommittedEntry)
  }
}

/**
 * Navigate through the app-owned callback while preserving a pending inbox
 * target. MainView owns the normal push/pop transition, but its legacy state
 * setter clears inbox_id during that transition; a same-entry replace plus
 * popstate lets its existing location reader restore the target without
 * creating a second history entry.
 */
export function navigateReaderRoute(
  route: ReaderRoute,
  onNavigate: (route: ReaderRoute) => void,
): void {
  onNavigate(route)
}
