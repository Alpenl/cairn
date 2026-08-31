import { vi } from 'vitest'
import { derivePhysicalNamespace } from '../identity'
import {
  READER_HISTORY_INDEX_KEY,
  READER_LAST_LOCATION_STORAGE_KEY,
  READER_STARTUP_PREFERENCE_STORAGE_KEY,
  parseReaderRoute,
  readerLastLocationStorageKey,
  readerRouteURL,
  readerThoughtHostTarget,
  readerThoughtViewFromURL,
  installReaderNavigationGuard,
  notifyReaderNavigationCommitted,
  sameReaderRoute,
  readReaderStartupPreference,
  rememberReaderRoute,
  resolveReaderStartupRoute,
  writeReaderStartupPreference,
  type ReaderRoute,
} from './route'

const IDENTITY_A = { physicalNamespace: 'physical-A' }
const IDENTITY_B = { physicalNamespace: 'physical-B' }

describe('RC-01A route model', () => {
  it.each([
    ['?surface=home', { kind: 'surface', id: 'home' }],
    ['?surface=feed', { kind: 'surface', id: 'feed' }],
    ['?view=pending', { kind: 'library', id: 'pending' }],
    ['?view=inbox', { kind: 'library', id: 'pending' }],
    ['?view=reading', { kind: 'library', id: 'reading' }],
    ['?view=read', { kind: 'library', id: 'reading' }],
    ['?view=sites', { kind: 'library', id: 'sites' }],
    ['?view=site', { kind: 'library', id: 'sites' }],
    ['?view=subs', { kind: 'library', id: 'subs' }],
    ['?view=subscription', { kind: 'library', id: 'subs' }],
    ['?view=subscriptions', { kind: 'library', id: 'subs' }],
    ['?view=notes', { kind: 'library', id: 'notes' }],
    ['?view=note', { kind: 'library', id: 'notes' }],
    ['?tool=todo', { kind: 'tool', id: 'todo' }],
    ['?tool=todos', { kind: 'tool', id: 'todo' }],
    ['?tool=settings', { kind: 'tool', id: 'settings' }],
    ['?tool=setting', { kind: 'tool', id: 'settings' }],
    ['?view=%20READING%20', { kind: 'library', id: 'reading' }],
  ])('normalizes %s', (query, expected) => {
    const parsed = parseReaderRoute(`https://reader.invalid/${query}`)
    expect(parsed).toEqual(expected)
  })

  it('uses the first populated route channel and keeps invalid values fail-closed', () => {
    expect(parseReaderRoute('https://reader.invalid/?surface=feed&view=notes&tool=settings')).toEqual({
      kind: 'surface',
      id: 'feed',
    })
    expect(parseReaderRoute('https://reader.invalid/?view=notes&tool=settings')).toEqual({
      kind: 'library',
      id: 'notes',
    })
    expect(parseReaderRoute('https://reader.invalid/?surface=unknown&view=notes')).toEqual({
      kind: 'surface',
      id: 'home',
    })
    expect(parseReaderRoute(
      'https://reader.invalid/?surface=unknown',
    )).toEqual({ kind: 'surface', id: 'home' })
    expect(parseReaderRoute('https://reader.invalid/?surface=&view=notes')).toEqual({
      kind: 'surface',
      id: 'home',
    })
  })

  it('normalizes relative deep-link input without allowing an unknown channel to fall through', () => {
    expect(parseReaderRoute('/reader?tool=%20TODOS%20')).toEqual({ kind: 'tool', id: 'todo' })
    expect(parseReaderRoute('/reader?surface=unknown&view=notes')).toEqual({ kind: 'surface', id: 'home' })
    expect(parseReaderRoute('/reader?tool=unknown&view=notes')).toEqual({ kind: 'library', id: 'notes' })
  })

  it('keeps an inbox target part of the pending deep-link identity', () => {
    const route = parseReaderRoute('https://reader.invalid/?view=inbox&inbox_id=%20I1%20')
    expect(route).toEqual({ kind: 'library', id: 'pending', inboxId: 'I1' })
    const url = readerRouteURL(route, '/reader?view=reading')
    expect(url.pathname).toBe('/reader')
    expect(url.search).toBe('?view=pending&inbox_id=I1')
    expect(parseReaderRoute(url)).toEqual(route)
    expect(sameReaderRoute(route, { kind: 'library', id: 'pending' })).toBe(false)
  })

  it('resolves missing or unknown startup routes by setting, then last location, then Home', () => {
    const storage = new StorageMock()

    expect(parseReaderRoute('https://reader.invalid/', { storage })).toEqual({ kind: 'surface', id: 'home' })
    rememberReaderRoute({ kind: 'surface', id: 'home' }, storage, undefined, IDENTITY_A)
    expect(resolveReaderStartupRoute(storage, IDENTITY_A)).toEqual({ kind: 'surface', id: 'home' })
    expect(parseReaderRoute('https://reader.invalid/?view=unknown', { storage, identity: IDENTITY_A })).toEqual({ kind: 'surface', id: 'home' })

    writeReaderStartupPreference('reading', storage)
    expect(readReaderStartupPreference(storage)).toBe('reading')
    expect(parseReaderRoute('https://reader.invalid/', { storage, identity: IDENTITY_A })).toEqual({ kind: 'library', id: 'reading' })
    writeReaderStartupPreference('home', storage)
    expect(parseReaderRoute('https://reader.invalid/', { storage, identity: IDENTITY_A })).toEqual({ kind: 'surface', id: 'home' })

    expect(parseReaderRoute('https://reader.invalid/?surface=feed', { storage, persist: true, identity: IDENTITY_A })).toEqual({ kind: 'surface', id: 'feed' })
    writeReaderStartupPreference('last', storage)
    expect(resolveReaderStartupRoute(storage, IDENTITY_A)).toEqual({ kind: 'surface', id: 'feed' })
  })

  it.each([
    {
      name: 'an explicit deep link before a forced Home preference',
      url: 'https://reader.invalid/?view=notes',
      preference: 'home',
      stored: '?surface=feed',
      expected: { kind: 'library', id: 'notes' },
    },
    {
      name: 'a forced Home preference before the last location',
      url: 'https://reader.invalid/',
      preference: 'home',
      stored: '?surface=feed',
      expected: { kind: 'surface', id: 'home' },
    },
    {
      name: 'a forced Reading preference before the last location',
      url: 'https://reader.invalid/',
      preference: 'reading',
      stored: '?surface=feed',
      expected: { kind: 'library', id: 'reading' },
    },
    {
      name: 'a valid last location when the preference is last',
      url: 'https://reader.invalid/',
      preference: 'last',
      stored: '?surface=feed',
      expected: { kind: 'surface', id: 'feed' },
    },
    {
      name: 'Home on first launch without a last location',
      url: 'https://reader.invalid/',
      preference: 'last',
      stored: null,
      expected: { kind: 'surface', id: 'home' },
    },
    {
      name: 'Home for a malformed last location',
      url: 'https://reader.invalid/',
      preference: 'last',
      stored: 'http://[invalid',
      expected: { kind: 'surface', id: 'home' },
    },
    {
      name: 'Home for an unknown stored route',
      url: 'https://reader.invalid/',
      preference: 'last',
      stored: '?view=unknown',
      expected: { kind: 'surface', id: 'home' },
    },
  ] satisfies ReadonlyArray<{
    readonly name: string
    readonly url: string
    readonly preference: 'last' | 'home' | 'reading'
    readonly stored: string | null
    readonly expected: ReaderRoute
  }>)('freezes startup precedence for $name', ({ url, preference, stored, expected }) => {
    const storage = new StorageMock()
    storage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, preference)
    if (stored !== null) storage.setItem(readerLastLocationStorageKey(IDENTITY_A), stored)

    expect(parseReaderRoute(url, { storage, identity: IDENTITY_A })).toEqual(expected)
  })

  it('restores a stored pending inbox target into the current startup URL', () => {
    const storage = new StorageMock()
    rememberReaderRoute({ kind: 'library', id: 'pending', inboxId: 'I7' }, storage, undefined, IDENTITY_A)
    window.history.replaceState({}, '', '/')

    expect(parseReaderRoute(window.location.href, { storage: undefined })).toEqual({
      kind: 'surface',
      id: 'home',
    })

    // The browser-backed storage path is the one used by MainView. Keep this
    // assertion isolated from the injectable StorageMock above.
    localStorage.setItem(readerLastLocationStorageKey(IDENTITY_A), '?view=pending&inbox_id=I7')
    expect(parseReaderRoute(window.location.href, { identity: IDENTITY_A })).toEqual({ kind: 'library', id: 'pending', inboxId: 'I7' })
    expect(window.location.search).toBe('?view=pending&inbox_id=I7')
  })

  it('keeps remembered routes and object targets owned by one physical installation', () => {
    const storage = new StorageMock()
    storage.setItem(READER_LAST_LOCATION_STORAGE_KEY, '?view=pending&inbox_id=legacy-A')
    expect(resolveReaderStartupRoute(storage, IDENTITY_A)).toEqual({ kind: 'surface', id: 'home' })

    expect(rememberReaderRoute(
      { kind: 'library', id: 'pending', inboxId: 'I-A' },
      storage,
      undefined,
      IDENTITY_A,
    )).toBe(true)
    expect(rememberReaderRoute(
      { kind: 'library', id: 'notes' },
      storage,
      undefined,
      IDENTITY_B,
    )).toBe(true)

    expect(parseReaderRoute('/', { storage, identity: IDENTITY_A })).toEqual({
      kind: 'library',
      id: 'pending',
      inboxId: 'I-A',
    })
    expect(parseReaderRoute('/', { storage, identity: IDENTITY_B })).toEqual({
      kind: 'library',
      id: 'notes',
    })
    expect(parseReaderRoute('/?view=notes', { storage, identity: IDENTITY_B })).toEqual({
      kind: 'library',
      id: 'notes',
    })
    expect(storage.getItem(readerLastLocationStorageKey(IDENTITY_A))).toBe('?view=pending&inbox_id=I-A')
    expect(storage.getItem(readerLastLocationStorageKey(IDENTITY_B))).toBe('?view=notes')
  })

  it('separates canonical origins while equivalent configs share one physical route owner', async () => {
    const namespace = 'server-client-data-namespace'
    const physicalA = await derivePhysicalNamespace('https://a.example/', namespace)
    const physicalAEquivalent = await derivePhysicalNamespace('https://a.example', namespace)
    const physicalB = await derivePhysicalNamespace('https://b.example', namespace)
    expect(physicalAEquivalent).toBe(physicalA)
    expect(physicalB).not.toBe(physicalA)

    const storage = new StorageMock()
    rememberReaderRoute(
      { kind: 'library', id: 'pending', inboxId: 'I-origin-A' },
      storage,
      undefined,
      { physicalNamespace: physicalA },
    )
    expect(resolveReaderStartupRoute(storage, { physicalNamespace: physicalAEquivalent })).toEqual({
      kind: 'library', id: 'pending', inboxId: 'I-origin-A',
    })
    expect(resolveReaderStartupRoute(storage, { physicalNamespace: physicalB })).toEqual({
      kind: 'surface', id: 'home',
    })
  })

  it('drops malformed stored object ids without claiming the legacy global key', () => {
    const storage = new StorageMock()
    storage.setItem(READER_LAST_LOCATION_STORAGE_KEY, '?view=notes&note_id=legacy')
    storage.setItem(readerLastLocationStorageKey(IDENTITY_A), '?view=pending&inbox_id=%00')
    expect(parseReaderRoute('/', { storage, identity: IDENTITY_A })).toEqual({
      kind: 'library', id: 'pending',
    })
    expect(rememberReaderRoute(
      { kind: 'surface', id: 'home' }, storage, undefined, { physicalNamespace: ' ' },
    )).toBe(false)
  })

  it.each([
    [{ kind: 'surface', id: 'home' }, '?surface=home'],
    [{ kind: 'surface', id: 'feed' }, '?surface=feed'],
    [{ kind: 'library', id: 'pending' }, '?view=pending'],
    [{ kind: 'library', id: 'pending', inboxId: 'I1' }, '?view=pending&inbox_id=I1'],
    [{ kind: 'library', id: 'notes' }, '?view=notes'],
    [{ kind: 'tool', id: 'todo' }, '?tool=todo'],
    [{ kind: 'tool', id: 'settings' }, '?tool=settings'],
  ] satisfies readonly [ReaderRoute, string][])('serializes %s canonically', (route, search) => {
    const url = readerRouteURL(route, 'https://reader.invalid/?view=stale&other=discarded')
    expect(url.search).toBe(search)
    expect(parseReaderRoute(url)).toEqual(route)
  })

  it('serializes only the target belonging to the canonical route', () => {
    expect(readerRouteURL(
      { kind: 'library', id: 'reading' },
      'https://reader.invalid/?view=read&note_id=stale&site_id=stale',
      { linkId: ' L1 ', noteId: 'N1', siteId: 'S1' },
    ).search).toBe('?view=reading&link_id=L1')
    expect(readerRouteURL(
      { kind: 'library', id: 'notes' },
      'https://reader.invalid/?view=reading&link_id=stale',
      { linkId: 'L1', noteId: ' N1 ' },
    ).search).toBe('?view=notes&note_id=N1')
    expect(readerRouteURL(
      { kind: 'library', id: 'sites' },
      'https://reader.invalid/?view=sites&note_id=stale',
      { noteId: 'N1', siteId: ' S1 ' },
    ).search).toBe('?view=sites&site_id=S1')
  })

  it.each([
    {
      name: 'a link thought with its explicit link identity',
      thought: { host_kind: 'link', host_id: 'L-host', link_id: ' L-explicit ' },
      expected: { route: { kind: 'library', id: 'reading' }, targets: { linkId: 'L-explicit' } },
    },
    {
      name: 'a link thought using its host identity',
      thought: { host_kind: ' LINK ', host_id: ' L-host ', link_id: null },
      expected: { route: { kind: 'library', id: 'reading' }, targets: { linkId: 'L-host' } },
    },
    {
      name: 'a note thought',
      thought: { host_kind: ' NOTE ', host_id: ' N1 ', link_id: null },
      expected: { route: { kind: 'library', id: 'notes' }, targets: { noteId: 'N1' } },
    },
    {
      name: 'an inbox thought',
      thought: { host_kind: ' INBOX ', host_id: ' I1 ', link_id: null },
      expected: { route: { kind: 'library', id: 'pending', inboxId: 'I1' } },
    },
  ] as const)('resolves $name to one canonical host target', ({ thought, expected }) => {
    expect(readerThoughtHostTarget(thought)).toEqual(expected)
  })

  it.each([
    { host_kind: 'unsupported', host_id: 'X1', link_id: null },
    { host_kind: 'unsupported', host_id: 'X1', link_id: 'L-invalid' },
    { host_kind: 'link', host_id: ' ', link_id: null },
    { host_kind: 'note', host_id: ' ', link_id: 'L-invalid' },
    { host_kind: 'inbox', host_id: ' ', link_id: null },
  ])('fails closed for a thought without a valid canonical host target', (thought) => {
    expect(readerThoughtHostTarget(thought)).toBeNull()
  })

  it('compares canonical route identity', () => {
    expect(sameReaderRoute(
      { kind: 'library', id: 'reading' },
      { kind: 'library', id: 'reading' },
    )).toBe(true)
    expect(sameReaderRoute(
      { kind: 'surface', id: 'home' },
      { kind: 'library', id: 'reading' },
    )).toBe(false)
    expect(sameReaderRoute(
      { kind: 'library', id: 'pending', inboxId: ' ' },
      { kind: 'library', id: 'pending' },
    )).toBe(true)
  })

  it('keeps all thought history branches addressable without adding a route kind', () => {
    expect(readerThoughtViewFromURL('https://reader.invalid/?tool=history')).toBe('history')
    expect(readerThoughtViewFromURL('https://reader.invalid/?tool=history&thought_view=live')).toBe('live')
    expect(readerThoughtViewFromURL('https://reader.invalid/?tool=history&thought_view=superseded')).toBe('superseded')
    expect(readerRouteURL({ kind: 'tool', id: 'history' }, 'https://reader.invalid/?thought_view=live').search)
      .toBe('?tool=history&thought_view=live')
    expect(readerRouteURL({ kind: 'tool', id: 'history' }, 'https://reader.invalid/?thought_view=history').search)
      .toBe('?tool=history&thought_view=history')
    expect(readerRouteURL({ kind: 'tool', id: 'history' }, 'https://reader.invalid/?thought_view=superseded').search)
      .toBe('?tool=history&thought_view=superseded')
  })

  it('serializes the Home recent-thought CTA as the refreshable live-thought route', () => {
    const url = readerRouteURL(
      { kind: 'tool', id: 'history' },
      'https://reader.invalid/?surface=home&stale=discarded',
      { thoughtView: 'live' },
    )

    expect(url.search).toBe('?tool=history&thought_view=live')
    expect(parseReaderRoute(url)).toEqual({ kind: 'tool', id: 'history' })
    expect(readerThoughtViewFromURL(url)).toBe('live')
  })

  it('blocks unmarked browser traversal before the host listener without rewriting its target', () => {
    window.history.replaceState({}, '', '/?view=reading')
    const hostListener = vi.fn()
    window.addEventListener('popstate', hostListener)
    const cleanup = installReaderNavigationGuard(() => false)

    window.history.pushState({}, '', '/?tool=history')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(hostListener).not.toHaveBeenCalled()
    expect(window.location.search).toBe('?tool=history')

    cleanup()
    window.history.pushState({}, '', '/?tool=history')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(hostListener).toHaveBeenCalledTimes(1)

    hostListener.mockClear()
    const throwingCleanup = installReaderNavigationGuard(() => { throw new Error('guard failure') })
    window.history.pushState({}, '', '/?view=notes')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(hostListener).not.toHaveBeenCalled()
    expect(window.location.search).toBe('?view=notes')
    throwingCleanup()
    window.removeEventListener('popstate', hostListener)
  })

  it('restores a marked blocked traversal with history.go instead of losing the entry', () => {
    const previousURL = window.location.href
    const previousState = window.history.state
    const hostListener = vi.fn()
    const go = vi.spyOn(window.history, 'go').mockImplementation(() => undefined)
    window.history.replaceState({ [READER_HISTORY_INDEX_KEY]: 10 }, '', '/?view=reading')
    window.addEventListener('popstate', hostListener)
    const cleanup = installReaderNavigationGuard(() => false)

    window.history.pushState({ [READER_HISTORY_INDEX_KEY]: 11 }, '', '/?tool=history')
    window.dispatchEvent(new PopStateEvent('popstate', {
      state: { [READER_HISTORY_INDEX_KEY]: 11 },
    }))

    expect(go).toHaveBeenCalledWith(-1)
    expect(hostListener).not.toHaveBeenCalled()

    cleanup()
    window.removeEventListener('popstate', hostListener)
    go.mockRestore()
    window.history.replaceState(previousState, '', previousURL)
  })

  it('restores the most recently committed entry after programmatic navigation', () => {
    const previousURL = window.location.href
    const previousState = window.history.state
    const go = vi.spyOn(window.history, 'go').mockImplementation(() => undefined)
    window.history.replaceState({ [READER_HISTORY_INDEX_KEY]: 4 }, '', '/?view=reading')
    const cleanup = installReaderNavigationGuard(() => false)

    window.history.pushState({ [READER_HISTORY_INDEX_KEY]: 5 }, '', '/?view=notes')
    notifyReaderNavigationCommitted()
    window.history.pushState({ [READER_HISTORY_INDEX_KEY]: 4 }, '', '/?view=reading')
    window.dispatchEvent(new PopStateEvent('popstate', {
      state: { [READER_HISTORY_INDEX_KEY]: 4 },
    }))

    expect(go).toHaveBeenCalledWith(1)

    cleanup()
    go.mockRestore()
    window.history.replaceState(previousState, '', previousURL)
  })
})

class StorageMock implements Storage {
  private readonly values = new Map<string, string>()

  get length(): number { return this.values.size }
  clear(): void { this.values.clear() }
  getItem(key: string): string | null { return this.values.get(key) ?? null }
  key(index: number): string | null { return [...this.values.keys()][index] ?? null }
  removeItem(key: string): void { this.values.delete(key) }
  setItem(key: string, value: string): void { this.values.set(key, value) }
}
