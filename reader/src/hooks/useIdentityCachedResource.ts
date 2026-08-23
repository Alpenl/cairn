import type { ApiResult } from '@webtag/api'

import type { IdentityBoundReaderClient } from '../lib/api/client'
import {
  resourceStore,
  type FetchContext,
  type FetchOptions,
} from '../lib/cache/store'
import {
  useCachedResource,
  type CachedResource,
} from '../lib/cache/useCachedResource'

export interface IdentityCachedResourceContext extends FetchContext {
  readonly client: IdentityBoundReaderClient
  readonly cacheKey: string
}

export interface IdentityCachedResource<T> {
  readonly cacheKey: string | null
  readonly canFetch: boolean
  readonly resource: CachedResource<T>
}

interface IdentityCachedResourceOptions<T> {
  readonly enabled?: boolean
  readonly equal?: FetchOptions<T>['equal']
}

function readerIdentitySuffix(client: IdentityBoundReaderClient | null): string {
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

export function isActiveReaderClient(
  client: IdentityBoundReaderClient | null,
): client is IdentityBoundReaderClient {
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

function scopedCacheKey(
  baseKey: string | null,
  client: IdentityBoundReaderClient | null,
): string | null {
  if (!baseKey || !isActiveReaderClient(client)) return null
  return `${baseKey}#${readerIdentitySuffix(client)}`
}

export function useIdentityCachedResource<T>(
  client: IdentityBoundReaderClient | null,
  baseKey: string | null,
  fetcher: (context: IdentityCachedResourceContext) => Promise<ApiResult<T>>,
  options: IdentityCachedResourceOptions<T> = {},
): IdentityCachedResource<T> {
  const canFetch = options.enabled !== false && isActiveReaderClient(client) && Boolean(baseKey)
  const cacheKey = canFetch
    ? scopedCacheKey(baseKey, client)
    : null
  const resource = useCachedResource<T>(
    cacheKey,
    (context) => fetcher({
      ...context,
      client: client!,
      cacheKey: cacheKey!,
    }),
    {
      enabled: canFetch,
      equal: options.equal,
    },
  )
  return { cacheKey, canFetch, resource }
}
