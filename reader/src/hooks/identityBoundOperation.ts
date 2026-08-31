import { useCallback, useMemo } from 'react'
import type { ApiError, ApiResult } from '@webtag/api'
import type { ReaderClient } from '../lib/api/client'
import { resourceStore } from '../lib/cache/store'
import type { IdentityLease, IdentityOwnership } from '../lib/identity'
import {
  useSurfaceRequestGate,
  type SurfaceRequestToken,
} from './useSurfaceRequestGate'

export const READER_IDENTITY_MISMATCH_ERROR: ApiError = Object.freeze({
  kind: 'identity-mismatch',
  message: 'Reader client does not own the active cache identity',
})

export interface IdentityBoundOperationToken<Channel extends PropertyKey> {
  readonly request: SurfaceRequestToken<Channel>
  readonly ownership: IdentityOwnership
}

export interface IdentityBoundOperationGate<Channel extends PropertyKey> {
  readonly begin: (
    channel: Channel,
    logicalKey: string,
  ) => IdentityBoundOperationToken<Channel> | null
  readonly capture: (
    channel: Channel,
    logicalKey: string,
  ) => IdentityBoundOperationToken<Channel> | null
  readonly invalidate: (channel: Channel) => void
  readonly invalidateAll: () => void
  readonly isCurrent: (token: IdentityBoundOperationToken<Channel>) => boolean
  readonly isSameOwner: (token: IdentityBoundOperationToken<Channel>) => boolean
  readonly commit: (token: IdentityBoundOperationToken<Channel>, apply: () => void) => boolean
  readonly finish: (token: IdentityBoundOperationToken<Channel>, release: () => void) => boolean
}

type IdentityCapableReaderClient = ReaderClient & {
  readonly identityLease?: IdentityLease | null
  readonly isIdentityCurrent?: () => boolean
  readonly captureIdentity?: (logicalKey: string) => IdentityOwnership | null
}

export function readerIdentityMismatch<T>(): ApiResult<T> {
  return {
    ok: false,
    error: READER_IDENTITY_MISMATCH_ERROR,
  }
}

function readIdentityLease(client: ReaderClient | null): IdentityLease | null {
  if (!client) return null
  try {
    return (client as IdentityCapableReaderClient).identityLease ?? null
  } catch {
    return null
  }
}

function clientStillCurrent(client: ReaderClient): boolean {
  try {
    const current = (client as IdentityCapableReaderClient).isIdentityCurrent
    return typeof current === 'function' ? current.call(client) : false
  } catch {
    return false
  }
}

export function isActiveReaderOwnership(
  ownership: IdentityOwnership | null | undefined,
): ownership is IdentityOwnership {
  try {
    return Boolean(
      ownership &&
      ownership.lease.isOwnershipCurrent(ownership) &&
      resourceStore.isIdentityActive(ownership.lease),
    )
  } catch {
    return false
  }
}

export function captureActiveReaderOwnership(
  client: ReaderClient | null,
  logicalKey: string,
): IdentityOwnership | null {
  if (!client) return null
  const identityClient = client as IdentityCapableReaderClient
  try {
    if (typeof identityClient.captureIdentity === 'function') {
      const captured = identityClient.captureIdentity.call(client, logicalKey)
      if (captured) return isActiveReaderOwnership(captured) ? captured : null
    }

    const lease = readIdentityLease(client)
    if (!lease || !clientStillCurrent(client)) return null
    const ownership = lease.captureOwnership(logicalKey)
    return isActiveReaderOwnership(ownership) ? ownership : null
  } catch {
    return null
  }
}

function identityOwnerParts(client: ReaderClient | null): readonly unknown[] {
  const lease = readIdentityLease(client)
  const context = lease?.context
  return [
    client,
    lease,
    context?.serverClientDataNamespace ?? null,
    context?.physicalNamespace ?? null,
    context?.localEpoch ?? null,
  ]
}

export function useIdentityBoundOperationGate<Channel extends PropertyKey>(
  client: ReaderClient | null,
  owner: readonly unknown[] = [],
): IdentityBoundOperationGate<Channel> {
  const gate = useSurfaceRequestGate<Channel>({
    owner: [...identityOwnerParts(client), ...owner],
    authority: () => captureActiveReaderOwnership(client, 'identity-bound operation authority') !== null,
  })

  const tokenFor = useCallback((
    request: SurfaceRequestToken<Channel>,
    logicalKey: string,
  ): IdentityBoundOperationToken<Channel> | null => {
    const ownership = captureActiveReaderOwnership(client, logicalKey)
    if (!ownership || !gate.isCurrent(request)) return null
    return { request, ownership }
  }, [client, gate])

  const begin = useCallback((
    channel: Channel,
    logicalKey: string,
  ): IdentityBoundOperationToken<Channel> | null =>
    tokenFor(gate.begin(channel), logicalKey), [gate, tokenFor])

  const capture = useCallback((
    channel: Channel,
    logicalKey: string,
  ): IdentityBoundOperationToken<Channel> | null =>
    tokenFor(gate.capture(channel), logicalKey), [gate, tokenFor])

  const isCurrent = useCallback((token: IdentityBoundOperationToken<Channel>): boolean =>
    gate.isCurrent(token.request) && isActiveReaderOwnership(token.ownership), [gate])

  const isSameOwner = useCallback((token: IdentityBoundOperationToken<Channel>): boolean =>
    gate.isSameOwner(token.request), [gate])

  const commit = useCallback((
    token: IdentityBoundOperationToken<Channel>,
    apply: () => void,
  ): boolean => {
    if (!isCurrent(token)) return false
    apply()
    return true
  }, [isCurrent])

  const finish = useCallback((
    token: IdentityBoundOperationToken<Channel>,
    release: () => void,
  ): boolean => {
    if (!isSameOwner(token)) return false
    release()
    return true
  }, [isSameOwner])

  return useMemo(
    () => ({
      begin,
      capture,
      invalidate: gate.invalidate,
      invalidateAll: gate.invalidateAll,
      isCurrent,
      isSameOwner,
      commit,
      finish,
    }),
    [
      begin,
      capture,
      gate.invalidate,
      gate.invalidateAll,
      isCurrent,
      isSameOwner,
      commit,
      finish,
    ],
  )
}
