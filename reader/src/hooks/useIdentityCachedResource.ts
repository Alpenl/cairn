import type { ApiResult } from '@webtag/api'
import type { ReaderIdentityPort } from '../lib/reader-api-ports'
import {
  type FetchContext,
  type FetchOptions,
} from '../lib/cache/store'
import { identityScopedCacheKey } from '../lib/cache/keys'
import type { IdentityContext } from '../lib/identity'
import {
  useCachedResource,
  type CachedResource,
} from '../lib/cache/useCachedResource'
import {
  captureActiveReaderOwnership,
  isActiveReaderOwnership,
} from './identityBoundOperation'

export interface IdentityCachedResourceContext<
  TClient extends ReaderIdentityPort = ReaderIdentityPort,
> extends FetchContext {
  readonly client: TClient
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

export function isActiveReaderClient(
  client: ReaderIdentityPort | null,
): client is ReaderIdentityPort {
  return captureActiveReaderOwnership(client, 'read identity-bound resource') !== null
}

function scopedCacheKey(
  baseKey: string | null,
  client: ReaderIdentityPort | null,
): string | null {
  const ownership = captureActiveReaderOwnership(client, 'build identity-scoped cache key')
  if (!baseKey || !ownership) return null
  return identityScopedCacheKey(baseKey, ownership.operation)
}

export function cacheKeyForActiveReaderIdentity(
  client: ReaderIdentityPort | null,
  logicalKey: string,
  build: (context: IdentityContext | null | undefined) => string,
): string | null {
  const ownership = captureActiveReaderOwnership(client, logicalKey)
  if (!ownership) return null
  try {
    return build(ownership.operation)
  } catch {
    return build(null)
  }
}

export function useIdentityCachedResource<T, TClient extends ReaderIdentityPort = ReaderIdentityPort>(
  client: TClient | null,
  baseKey: string | null,
  fetcher: (context: Omit<IdentityCachedResourceContext, 'client'> & { readonly client: TClient }) => Promise<ApiResult<T>>,
  options: IdentityCachedResourceOptions<T> = {},
): IdentityCachedResource<T> {
  const cacheKey = scopedCacheKey(baseKey, client)
  return useFinalIdentityCachedResource(client, cacheKey, fetcher, options)
}

export function useFinalIdentityCachedResource<T, TClient extends ReaderIdentityPort = ReaderIdentityPort>(
  client: TClient | null,
  finalKey: string | null,
  fetcher: (context: IdentityCachedResourceContext<TClient>) => Promise<ApiResult<T>>,
  options: IdentityCachedResourceOptions<T> = {},
): IdentityCachedResource<T> {
  const ownership = captureActiveReaderOwnership(client, 'use final identity cache key')
  const canFetch = options.enabled !== false && Boolean(finalKey) && isActiveReaderOwnership(ownership)
  const cacheKey = canFetch ? finalKey : null
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
