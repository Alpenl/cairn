import { useCallback, useMemo } from 'react'
import type { ApiError } from '@webtag/api'
import type { LinkResponse, ReaderRelatedTagsResponse } from '../lib/api/types'
import type { ReaderAmbientClientPort } from '../lib/reader-api-ports'
import {
  READER_RELATED_TAGS_CACHE_PREFIX,
  readerRelatedTagsCacheKey,
} from '../lib/cache/keys'
import { resourceStore } from '../lib/cache/store'
import {
  cacheKeyForActiveReaderIdentity,
  useFinalIdentityCachedResource,
} from './useIdentityCachedResource'
import { useReaderClient } from './useReaderClient'

const LOCAL_FALLBACK_LIMIT = 12

export interface ReaderRelatedTagsState {
  readonly tags: string[]
  readonly source: 'server' | 'local'
  readonly loading: boolean
  readonly error: ApiError | null
  readonly reload: () => void
}

function supportsRelatedTags(client: ReaderAmbientClientPort | null): boolean {
  return typeof (client as unknown as { getRelatedTags?: unknown } | null)?.getRelatedTags === 'function'
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
  explicitClient?: ReaderAmbientClientPort,
  options: { readonly enabled?: boolean } = {},
): ReaderRelatedTagsState {
  const client = useReaderClient(explicitClient)
  const linkID = link?.id ?? null
  const cacheKey = options.enabled === false || !linkID
    ? null
    : cacheKeyForActiveReaderIdentity(
      client,
      'build related-tags cache key',
      (context) => readerRelatedTagsCacheKey(linkID, context, LOCAL_FALLBACK_LIMIT),
    )
  const {
    resource,
  } = useFinalIdentityCachedResource<ReaderRelatedTagsResponse, ReaderAmbientClientPort>(
    client,
    cacheKey,
    ({ client }) => client.getRelatedTags(linkID!, LOCAL_FALLBACK_LIMIT),
    { enabled: supportsRelatedTags(client) },
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
  resourceStore.invalidate(READER_RELATED_TAGS_CACHE_PREFIX)
}
