import { useCallback, useMemo } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ApiError, ApiResult } from '../lib/api/result'
import type { DomainTreeSummaryEnvelope, DomainTreeSummaryResponse } from '../lib/api/types'
import { useCachedResource } from '../lib/cache/useCachedResource'
import { reloadForActiveIdentity, type LibraryReloadOptions } from './libraryReload'

export interface DomainSummariesState {
  summaries: DomainTreeSummaryResponse[] | null
  /** 包含无域名 done link 的完整总数；null 表示摘要尚不可用。 */
  total: number | null
  loading: boolean
  error: ApiError | null
  reload: (options?: LibraryReloadOptions) => Promise<ApiResult<DomainTreeSummaryEnvelope> | null>
}

export const DOMAIN_SUMMARIES_CACHE_PREFIX = 'GET /api/tree?view=domains'
export const DOMAIN_SUMMARIES_CACHE_KEY = `${DOMAIN_SUMMARIES_CACHE_PREFIX}&library_kind=reading`

/** 读取 Reading partition 的 truthful 域名聚合；null 表示尚未成功取得摘要。 */
export function useDomainSummaries(client: IdentityBoundReaderClient): DomainSummariesState {
  const resource = useCachedResource<DomainTreeSummaryEnvelope>(
    DOMAIN_SUMMARIES_CACHE_KEY,
    (conditional) => client.getDomainSummaries('reading', conditional),
  )
  const reload = useCallback(
    (options: LibraryReloadOptions = {}) => reloadForActiveIdentity(
      client,
      'reload reading domain summaries',
      resource.reload,
      options,
    ),
    [client, resource.reload],
  )

  return useMemo(() => {
    // 失败时 summaries / total 归 null（调用方据此退回 corpus 派生），与改造前一致。
    const envelope = resource.error ? undefined : resource.data
    return {
      summaries: envelope?.domains ?? null,
      total: envelope?.total ?? null,
      loading: resource.loading,
      error: resource.error,
      reload,
    }
  }, [resource.data, resource.error, resource.loading, reload])
}
