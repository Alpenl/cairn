import { useCallback, useMemo } from 'react'
import type { ApiError, ApiResult } from '@webtag/api'
import type { DomainTreeSummaryEnvelope, DomainTreeSummaryResponse } from '../lib/api/types'
import type { ReaderIdentityPort, ReaderLibrarySitesPort } from '../lib/reader-api-ports'
import { DOMAIN_SUMMARIES_CACHE_KEY } from '../lib/cache/keys'
import { reloadForActiveIdentity, type LibraryReloadOptions } from './libraryReload'
import { useFinalIdentityCachedResource } from './useIdentityCachedResource'

export interface DomainSummariesState {
  summaries: DomainTreeSummaryResponse[] | null
  /** 包含无域名 done link 的完整总数；null 表示摘要尚不可用。 */
  total: number | null
  loading: boolean
  error: ApiError | null
  reload: (options?: LibraryReloadOptions) => Promise<ApiResult<DomainTreeSummaryEnvelope> | null>
}

export {
  DOMAIN_SUMMARIES_CACHE_KEY,
  DOMAIN_SUMMARIES_CACHE_PREFIX,
} from '../lib/cache/keys'

type DomainSummariesClient = ReaderIdentityPort &
  Pick<ReaderLibrarySitesPort, 'getDomainSummaries'>

/** 读取 Reading partition 的 truthful 域名聚合；null 表示尚未成功取得摘要。 */
export function useDomainSummaries(client: DomainSummariesClient): DomainSummariesState {
  const {
    resource,
  } = useFinalIdentityCachedResource<DomainTreeSummaryEnvelope, DomainSummariesClient>(
    client,
    DOMAIN_SUMMARIES_CACHE_KEY,
    ({ client, signal }) => client.getDomainSummaries('reading', { signal }),
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
