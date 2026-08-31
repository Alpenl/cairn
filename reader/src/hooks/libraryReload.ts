import type { ReaderClient } from '../lib/api/client'
import type { ApiResult } from '@webtag/api'
import type { FetchOptions } from '../lib/cache/store'
import {
  captureActiveReaderOwnership,
  isActiveReaderOwnership,
  readerIdentityMismatch,
} from './identityBoundOperation'

export type LibraryReloadOptions = Pick<FetchOptions, 'signal' | 'silent'>

/** Keep one manual reload tied to the exact identity that started it. */
export async function reloadForActiveIdentity<T>(
  client: ReaderClient,
  logicalKey: string,
  reload: (options?: LibraryReloadOptions) => Promise<ApiResult<T> | null>,
  options: LibraryReloadOptions = {},
): Promise<ApiResult<T> | null> {
  const ownership = captureActiveReaderOwnership(client, logicalKey)
  if (!ownership) return readerIdentityMismatch()

  const result = await reload(options)
  if (!isActiveReaderOwnership(ownership)) return readerIdentityMismatch()
  return result
}
