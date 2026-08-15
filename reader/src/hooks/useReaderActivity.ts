import { useCallback, useEffect, useMemo } from 'react'
import type {
  IdentityBoundReaderClient,
  ReaderActivityKind,
  ReaderActivityRequestOptions,
} from '../lib/api/client'
import { err, ok, type ApiError, type ApiResult } from '../lib/api/result'
import type { LinkResponse, ReaderActivityResponse } from '../lib/api/types'
import { useCachedResource } from '../lib/cache/useCachedResource'
import { resourceStore } from '../lib/cache/store'
import { useReaderClient } from './useReaderClient'

const ACTIVITY_CACHE_PREFIX = 'GET /api/reader/activity'
const ACTIVITY_PAGE_LIMIT = 100

export interface ReaderActivityState {
  readonly tagLastAt: ReadonlyMap<string, string>
  readonly domainLastAt: ReadonlyMap<string, string>
  readonly source: 'server' | 'local'
  readonly degraded: boolean
  readonly loading: boolean
  readonly error: ApiError | null
  readonly reload: () => void
}

export interface ReaderActivityOptions {
  readonly kind?: ReaderActivityKind
  readonly allPages?: boolean
  readonly enabled?: boolean
}

interface NormalizedReaderActivityPayload {
  readonly tags: Array<{ tag: string; last_at: string }>
  readonly domains: Array<{ domain: string; last_at: string }>
}

interface CachedReaderActivityPage {
  readonly after: string | null
  readonly response: ReaderActivityResponse
}

interface CachedReaderActivity {
  readonly kind: ReaderActivityKind
  readonly pages: readonly CachedReaderActivityPage[]
  readonly complete: boolean
  readonly continuationError: ApiError | null
}

function identityNamespace(client: IdentityBoundReaderClient | null): string {
  try {
    const context = client?.identityLease?.context
    if (!context) return 'unscoped'
    return [
      context.serverClientDataNamespace,
      context.physicalNamespace,
      String(context.localEpoch),
    ].map((part) => encodeURIComponent(part)).join(':')
  } catch {
    return 'unscoped'
  }
}

function supportsActivity(client: IdentityBoundReaderClient | null): boolean {
  return typeof (client as unknown as { getReaderActivity?: unknown } | null)?.getReaderActivity === 'function'
}

function isActiveClient(client: IdentityBoundReaderClient | null): boolean {
  if (!client) return false
  try {
    const lease = client.identityLease
    return Boolean(
      lease &&
      client.isIdentityCurrent() &&
      resourceStore.isIdentityActive(lease),
    )
  } catch {
    return false
  }
}

function isValidActivityTimestamp(value: string): boolean {
  return value.trim() !== '' && Number.isFinite(Date.parse(value))
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function firstValidTimestamp(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === 'string' && isValidActivityTimestamp(value)) return value
  }
  return ''
}

function activityTimestamp(item: Record<string, unknown>): string {
  return firstValidTimestamp(
    item.last_at,
    item.lastAt,
    item.created_at,
    item.createdAt,
  )
}

/** Normalize old timestamp field names without treating the old wire shape as authoritative. */
export function normalizeReaderActivityPayload(value: unknown): NormalizedReaderActivityPayload | null {
  if (!isRecord(value) || !Array.isArray(value.tags) || !Array.isArray(value.domains)) return null
  const tags = value.tags.flatMap((item) => {
    if (!isRecord(item) || typeof item.tag !== 'string') return []
    const tag = item.tag.trim()
    return tag ? [{ tag, last_at: activityTimestamp(item) }] : []
  })
  const domains = value.domains.flatMap((item) => {
    if (!isRecord(item) || typeof item.domain !== 'string') return []
    const domain = item.domain.trim()
    return domain ? [{ domain, last_at: activityTimestamp(item) }] : []
  })
  return { tags, domains }
}

function putNewest(map: Map<string, string>, key: string, timestamp: string): void {
  if (!key.trim() || !isValidActivityTimestamp(timestamp)) return
  const previous = map.get(key)
  if (!previous || compareReaderActivityLastAtDesc(timestamp, previous) < 0) {
    map.set(key, timestamp)
  }
}

export function compareReaderActivityKeyAsc(left: string, right: string): number {
  const normalizedLeft = left.trim().toLocaleLowerCase()
  const normalizedRight = right.trim().toLocaleLowerCase()
  if (normalizedLeft < normalizedRight) return -1
  if (normalizedLeft > normalizedRight) return 1
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

function sortActivityMap(map: Map<string, string>): ReadonlyMap<string, string> {
  return new Map([...map.entries()].sort((left, right) => {
    const byTime = compareReaderActivityLastAtDesc(left[1], right[1])
    return byTime || compareReaderActivityKeyAsc(left[0], right[0])
  }))
}

function indexActivity(data: CachedReaderActivity | undefined): {
  tags: ReadonlyMap<string, string>
  domains: ReadonlyMap<string, string>
  usable: boolean
} {
  if (!data || data.pages.length === 0) return { tags: new Map(), domains: new Map(), usable: false }
  const tags = new Map<string, string>()
  const domains = new Map<string, string>()
  let rowCount = 0
  for (const page of data.pages) {
    const normalized = normalizeReaderActivityPayload(page.response)
    if (!normalized) return { tags: new Map(), domains: new Map(), usable: false }
    rowCount += normalized.tags.length + normalized.domains.length
    for (const item of normalized.tags) putNewest(tags, item.tag, item.last_at)
    for (const item of normalized.domains) putNewest(domains, item.domain, item.last_at)
  }
  return {
    tags: sortActivityMap(tags),
    domains: sortActivityMap(domains),
    // Empty pages are authoritative. Non-empty pages with no usable timestamp
    // must fall back instead of presenting an empty projection as exact.
    usable: rowCount === 0 || tags.size > 0 || domains.size > 0,
  }
}

function indexLocalActivity(links: LinkResponse[]): {
  tags: ReadonlyMap<string, string>
  domains: ReadonlyMap<string, string>
} {
  const tags = new Map<string, string>()
  const domains = new Map<string, string>()
  for (const link of links) {
    for (const rawTag of link.tags) {
      const tag = rawTag.trim()
      if (tag) putNewest(tags, tag, link.created_at)
    }
    const domain = link.domain?.trim() ?? ''
    if (domain) putNewest(domains, domain, link.created_at)
  }
  return { tags: sortActivityMap(tags), domains: sortActivityMap(domains) }
}

/** Compare activity timestamps from newest to oldest with a deterministic fallback. */
export function compareReaderActivityLastAtDesc(left: string, right: string): number {
  const leftTime = Date.parse(left)
  const rightTime = Date.parse(right)
  const leftValid = left.trim() !== '' && Number.isFinite(leftTime)
  const rightValid = right.trim() !== '' && Number.isFinite(rightTime)
  if (leftValid && rightValid) return rightTime - leftTime
  if (leftValid !== rightValid) return leftValid ? -1 : 1
  return left.localeCompare(right, 'zh')
}

function activityRequestOptions(
  kind: ReaderActivityKind,
  after: string | undefined,
  signal: AbortSignal | undefined,
): ReaderActivityRequestOptions {
  return {
    kind,
    ...(after ? { after } : {}),
    ...(signal ? { signal } : {}),
  }
}

function activityProtocolError(message: string): ApiError {
  return { kind: 'other', status: 422, errorCode: 'invalid_activity_page', message }
}

function firstPageCache(
  kind: ReaderActivityKind,
  response: ReaderActivityResponse,
  previous: CachedReaderActivity | undefined,
): CachedReaderActivity {
  const first: CachedReaderActivityPage = { after: null, response }
  const next = response.next_cursor?.trim() ?? ''
  if (!next) return { kind, pages: [first], complete: true, continuationError: null }

  const tailIndex = previous?.kind === kind
    ? previous.pages.findIndex((page) => page.after === next)
    : -1
  if (previous && tailIndex >= 0) {
    return {
      kind,
      pages: [first, ...previous.pages.slice(tailIndex)],
      complete: previous.complete,
      continuationError: previous.continuationError,
    }
  }
  return { kind, pages: [first], complete: false, continuationError: null }
}

async function fetchActivity(
  client: IdentityBoundReaderClient,
  kind: ReaderActivityKind,
  allPages: boolean,
  signal: AbortSignal | undefined,
  previous: CachedReaderActivity | undefined,
): Promise<ApiResult<CachedReaderActivity>> {
  let after: string | undefined
  const pages: CachedReaderActivityPage[] = []
  const seenCursors = new Set<string>()

  for (;;) {
    const result = await client.getReaderActivity(
      ACTIVITY_PAGE_LIMIT,
      activityRequestOptions(kind, after, signal),
    )
    if (!result.ok) {
      if (pages.length === 0) return result
      return ok({ kind, pages, complete: false, continuationError: result.error })
    }
    if (result.data.kind !== kind) {
      const protocolError = activityProtocolError('Reader activity response kind does not match the request')
      if (pages.length === 0) return err(protocolError)
      return ok({ kind, pages, complete: false, continuationError: protocolError })
    }

    pages.push({ after: after ?? null, response: result.data })
    const next = result.data.next_cursor?.trim() ?? ''
    if (!allPages) return ok(firstPageCache(kind, result.data, previous))
    if (!next) return ok({ kind, pages, complete: true, continuationError: null })
    if (seenCursors.has(next)) {
      return ok({
        kind,
        pages,
        complete: false,
        continuationError: activityProtocolError('Reader activity cursor repeated before pagination completed'),
      })
    }
    seenCursors.add(next)
    after = next
  }
}

export function useReaderActivity(
  explicitClient?: IdentityBoundReaderClient,
  fallbackLinks: LinkResponse[] = [],
  options: ReaderActivityOptions = {},
): ReaderActivityState {
  const client = useReaderClient(explicitClient)
  const kind = options.kind ?? 'all'
  const allPages = options.allPages ?? false
  const canFetch = options.enabled !== false && isActiveClient(client) && supportsActivity(client)
  const cacheKey = canFetch
    ? `${ACTIVITY_CACHE_PREFIX}?kind=${kind}&limit=${ACTIVITY_PAGE_LIMIT}#${identityNamespace(client)}`
    : null
  const resource = useCachedResource<CachedReaderActivity>(
    cacheKey,
    ({ signal }) => fetchActivity(
      client!,
      kind,
      allPages,
      signal,
      cacheKey ? resourceStore.peek<CachedReaderActivity>(cacheKey).data : undefined,
    ),
    { enabled: canFetch },
  )
  const {
    data: activityData,
    error: resourceError,
    loading: resourceLoading,
    reload: reloadResource,
  } = resource

  // A first-page consumer may win the initial in-flight request for this
  // identity/kind. Once that page settles, a full consumer advances the same
  // cache entry rather than creating a second approximation namespace.
  useEffect(() => {
    if (!allPages || !cacheKey || !activityData || activityData.complete) return
    if (activityData.continuationError || resourceStore.peek(cacheKey).revalidating) return
    void reloadResource({ force: true })
  }, [activityData, allPages, cacheKey, reloadResource])

  const indexed = useMemo(() => indexActivity(activityData), [activityData])
  const local = useMemo(() => indexLocalActivity(fallbackLinks), [fallbackLinks])
  const useServer = Boolean(activityData && indexed.usable)
  const visible = useServer ? indexed : local
  const reload = useCallback(() => {
    void reloadResource()
  }, [reloadResource])
  const activityError = resourceError ?? activityData?.continuationError ?? null

  return useMemo(
    () => ({
      tagLastAt: visible.tags,
      domainLastAt: visible.domains,
      source: useServer ? 'server' : 'local',
      degraded: !useServer,
      loading: canFetch && resourceLoading,
      error: activityError,
      reload,
    }),
    [activityError, canFetch, reload, resourceLoading, useServer, visible],
  )
}

/** Invalidate activity after a collection event is known locally. */
export function invalidateReaderActivity(): void {
  resourceStore.invalidate(ACTIVITY_CACHE_PREFIX)
}
