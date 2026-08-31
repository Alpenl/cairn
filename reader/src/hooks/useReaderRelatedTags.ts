import { useCallback, useMemo } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ApiError } from '@webtag/api'
import type { LinkResponse, ReaderRelatedTagsResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'
import { useReaderClient } from './useReaderClient'

const RELATED_TAGS_CACHE_PREFIX = 'GET /api/reader/related-tags'
const LOCAL_FALLBACK_LIMIT = 12

export interface ReaderRelatedTagsState {
  readonly tags: string[]
  readonly source: 'server' | 'local'
  readonly loading: boolean
  readonly error: ApiError | null
  readonly reload: () => void
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

function supportsRelatedTags(client: IdentityBoundReaderClient | null): boolean {
  return typeof (client as unknown as { getRelatedTags?: unknown } | null)?.getRelatedTags === 'function'
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

function stableLocalRelatedTags(
  link: LinkResponse | null | undefined,
  corpus: LinkResponse[],
): string[] {
  if (!link) return []
  const own = new Set(link.tags.map((tag) => tag.trim()).filter(Boolean))
  const scores = new Map<string, number>()
  for (const candidate of corpus) {
    if (candidate.id === link.id) continue
    const candidateTags = [...new Set(candidate.tags.map((tag) => tag.trim()).filter(Boolean))]
    const shared = candidateTags.filter((tag) => own.has(tag)).length +
      (candidate.domain && candidate.domain === link.domain ? 0.5 : 0)
    if (shared <= 0) continue
    for (const tag of candidateTags) {
      if (own.has(tag)) continue
      scores.set(tag, (scores.get(tag) ?? 0) + shared)
    }
  }
  return [...scores.entries()]
    .sort((left, right) => right[1] - left[1] || left[0].localeCompare(right[0], 'zh'))
    .map(([tag]) => tag)
}

function normalizeTags(items: readonly string[] | undefined): string[] {
  const seen = new Set<string>()
  const normalized: string[] = []
  for (const item of items ?? []) {
    if (typeof item !== 'string') continue
    const tag = item.trim()
    if (!tag || seen.has(tag)) continue
    seen.add(tag)
    normalized.push(tag)
  }
  return normalized
}

export function useReaderRelatedTags(
  link: LinkResponse | null | undefined,
  corpus: LinkResponse[],
  explicitClient?: IdentityBoundReaderClient,
  options: { readonly enabled?: boolean } = {},
): ReaderRelatedTagsState {
  const client = useReaderClient(explicitClient)
  const linkID = link?.id ?? null
  const canFetch = Boolean(options.enabled !== false && linkID && isActiveClient(client) && supportsRelatedTags(client))
  const cacheKey = canFetch
    ? `${RELATED_TAGS_CACHE_PREFIX}?link_id=${encodeURIComponent(linkID as string)}&limit=12#${identityNamespace(client)}`
    : null
  const resource = useCachedResource<ReaderRelatedTagsResponse>(
    cacheKey,
    () => client!.getRelatedTags(linkID!, LOCAL_FALLBACK_LIMIT),
    { enabled: canFetch },
  )
  const localTags = useMemo(
    () => stableLocalRelatedTags(link, corpus).slice(0, LOCAL_FALLBACK_LIMIT),
    [corpus, link],
  )
  const serverData = resource.error ? undefined : resource.data
  const serverTags = useMemo(
    () => normalizeTags(serverData?.items).slice(0, LOCAL_FALLBACK_LIMIT),
    [serverData?.items],
  )
  const reload = useCallback(() => {
    void resource.reload()
  }, [resource])

  return useMemo(
    () => ({
      tags: serverData ? serverTags : localTags,
      source: serverData ? 'server' : 'local',
      loading: resource.loading,
      error: resource.error,
      reload,
    }),
    [localTags, reload, resource.error, resource.loading, serverData, serverTags],
  )
}

/** Invalidate all related-tag representations after a metadata write. */
export function invalidateReaderRelatedTags(): void {
  resourceStore.invalidate(RELATED_TAGS_CACHE_PREFIX)
}
