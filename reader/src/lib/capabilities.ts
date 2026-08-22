import type { CapabilitiesResponse } from './api/types'
import type { ReaderRoute } from './navigation/route'

export interface ReaderCapabilityPolicy {
  readonly annotations: boolean
  readonly notes: boolean
  readonly inbox: boolean
  readonly todos: boolean
  readonly engagement: boolean
  readonly home: boolean
  readonly feed: boolean
  readonly ai: boolean
  readonly relatedTags: boolean
  readonly activity: boolean
  readonly history: boolean
  readonly trash: boolean
  readonly libraryKinds: boolean
  readonly siteRead: boolean
  readonly siteWrite: boolean
  readonly siteAdvanced: boolean
  readonly archiveDownload: boolean
}

export type ReaderCapabilityFeature = keyof ReaderCapabilityPolicy

export function deriveReaderCapabilityPolicy(
  capabilities: CapabilitiesResponse | null | undefined,
): ReaderCapabilityPolicy {
  const readerVNext = capabilities?.reader_vnext === true
  const reader = capabilities?.reader
  const enabled = (feature: keyof NonNullable<CapabilitiesResponse['reader']>) =>
    readerVNext && reader?.[feature] === true
  const libraryKinds = capabilities?.library_kinds === true
  const siteRead = libraryKinds && capabilities?.site_library === true
  const siteWrite = siteRead && capabilities?.site_management === true

  return {
    annotations: enabled('annotations'),
    notes: enabled('notes'),
    inbox: enabled('inbox'),
    todos: enabled('todos'),
    engagement: enabled('engagement'),
    home: enabled('home'),
    feed: enabled('feed'),
    ai: enabled('ai'),
    relatedTags: enabled('related_tags'),
    activity: enabled('activity'),
    history: enabled('history'),
    trash: enabled('trash'),
    libraryKinds,
    siteRead,
    siteWrite,
    siteAdvanced:
      siteWrite && capabilities?.site_advanced_management === true,
    archiveDownload:
      Array.isArray(capabilities?.archive_versions) &&
      capabilities.archive_versions.includes(2),
  }
}

const POLICY_KEYS = [
  'annotations',
  'notes',
  'inbox',
  'todos',
  'engagement',
  'home',
  'feed',
  'ai',
  'relatedTags',
  'activity',
  'history',
  'trash',
  'libraryKinds',
  'siteRead',
  'siteWrite',
  'siteAdvanced',
  'archiveDownload',
] as const satisfies readonly ReaderCapabilityFeature[]

export function readerCapabilityFingerprint(
  policy: ReaderCapabilityPolicy,
): string {
  return POLICY_KEYS.map((key) => (policy[key] ? '1' : '0')).join('')
}

let nextCapabilityGeneration = 1

/**
 * Async work captures one lease. A capability snapshot replacement revokes
 * that lease, so a late response must pass both the client's identity check
 * and this generation check before it can publish state.
 */
export class ReaderCapabilityLease {
  readonly generation = nextCapabilityGeneration++
  private active = true
  private revoked = false

  constructor(readonly policy: ReaderCapabilityPolicy) {}

  isCurrent(feature?: ReaderCapabilityFeature): boolean {
    return this.active && (feature === undefined || this.policy[feature])
  }

  /** React may replay an effect setup after its simulated Strict Mode cleanup. */
  activate(): void {
    if (!this.revoked) this.active = true
  }

  /** Stop component continuations while this lease has no mounted owner. */
  deactivate(): void {
    this.active = false
  }

  /** Permanently retire a generation; unlike deactivation this cannot be undone. */
  revoke(): void {
    this.revoked = true
    this.active = false
  }
}

export function readerRouteIsAvailable(
  route: ReaderRoute,
  policy: ReaderCapabilityPolicy,
): boolean {
  if (route.kind === 'surface') {
    return route.id === 'home' ? policy.home : policy.feed
  }
  if (route.kind === 'tool') {
    if (route.id === 'todo') return policy.todos
    if (route.id === 'history') return policy.history
    return true
  }
  if (route.id === 'pending') return policy.inbox
  if (route.id === 'notes') return policy.notes
  if (route.id === 'sites') return policy.siteRead
  return true
}

const CANONICAL_ROUTE_ORDER: readonly ReaderRoute[] = [
  { kind: 'surface', id: 'home' },
  { kind: 'surface', id: 'feed' },
  { kind: 'library', id: 'pending' },
  { kind: 'library', id: 'reading' },
  { kind: 'library', id: 'sites' },
  { kind: 'library', id: 'subs' },
  { kind: 'library', id: 'notes' },
  { kind: 'tool', id: 'todo' },
  { kind: 'tool', id: 'settings' },
  { kind: 'tool', id: 'history' },
]

export function firstAvailableReaderRoute(
  policy: ReaderCapabilityPolicy,
): ReaderRoute {
  return (
    CANONICAL_ROUTE_ORDER.find((route) =>
      readerRouteIsAvailable(route, policy),
    ) ?? { kind: 'library', id: 'reading' }
  )
}
