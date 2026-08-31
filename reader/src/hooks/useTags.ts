/**
 * 标签聚合 hook。阅读侧栏固定请求 reading partition，避免网站库标签混入。
 * 失败时保留 unavailable，而不是把部分本地 corpus 伪装成全库计数。
 *
 * PF3 起走共享缓存：切走再切回来不重拉，同键并发合并为一次往返。
 */
import { useCallback, useMemo } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ApiError, ApiResult } from '@webtag/api'
import type { TagCountResponse } from '../lib/api/types'
import { TAGS_CACHE_KEY } from '../lib/cache/keys'
import { useCachedResource } from '../lib/cache/useCachedResource'
import { reloadForActiveIdentity, type LibraryReloadOptions } from './libraryReload'

export interface TagsState {
  tags: TagCountResponse[] | null
  loading: boolean
  error: ApiError | null
  reload: (options?: LibraryReloadOptions) => Promise<ApiResult<TagCountResponse[]> | null>
}

export {
  TAGS_CACHE_KEY,
  TAGS_CACHE_PREFIX,
} from '../lib/cache/keys'

export function useTags(client: IdentityBoundReaderClient): TagsState {
  const resource = useCachedResource<TagCountResponse[]>(TAGS_CACHE_KEY, (conditional) =>
    client.getTags('reading', conditional),
  )
  const reload = useCallback(
    (options: LibraryReloadOptions = {}) => reloadForActiveIdentity(
      client,
      'reload reading tags',
      resource.reload,
      options,
    ),
    [client, resource.reload],
  )

  return useMemo(
    () => ({
      tags: resource.error ? null : (resource.data ?? null),
      loading: resource.loading,
      error: resource.error,
      reload,
    }),
    [resource.data, resource.error, resource.loading, reload],
  )
}
