import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ApiResult } from '@webtag/api'
import { resourceStore, type FetchOptions } from '../lib/cache/store'

export type LibraryReloadOptions = Pick<FetchOptions, 'signal' | 'silent'>

function identityMismatch<T>(): ApiResult<T> {
  return {
    ok: false,
    error: {
      kind: 'identity-mismatch',
      message: 'Reader client does not own the active cache identity',
    },
  }
}

/** Keep one manual reload tied to the exact identity that started it. */
export async function reloadForActiveIdentity<T>(
  client: IdentityBoundReaderClient,
  logicalKey: string,
  reload: (options?: LibraryReloadOptions) => Promise<ApiResult<T> | null>,
  options: LibraryReloadOptions = {},
): Promise<ApiResult<T> | null> {
  const lease = client.identityLease
  const ownership = lease.captureOwnership(logicalKey)
  if (
    !lease.isOwnershipCurrent(ownership) ||
    !resourceStore.isIdentityActive(lease)
  ) {
    return identityMismatch()
  }

  const result = await reload(options)
  if (
    !lease.isOwnershipCurrent(ownership) ||
    !resourceStore.isIdentityActive(lease)
  ) {
    return identityMismatch()
  }
  return result
}
