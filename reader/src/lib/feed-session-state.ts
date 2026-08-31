import type { ReaderIdentityPort } from './reader-api-ports'

export const FEED_RESUME_STORAGE_PREFIX = 'webtag:reader:mixed-feed:v1'

export function readerFeedIdentityNamespace(client: ReaderIdentityPort): string | null {
  try {
    const namespace = client.identityLease.context.physicalNamespace
    return typeof namespace === 'string' && namespace.length > 0 ? namespace : null
  } catch {
    return null
  }
}

export function clearFeedSessionState(client: ReaderIdentityPort): void {
  const namespace = readerFeedIdentityNamespace(client)
  if (!namespace || typeof window === 'undefined') return
  const prefix = `${FEED_RESUME_STORAGE_PREFIX}:${encodeURIComponent(namespace)}:`
  try {
    for (let index = window.sessionStorage.length - 1; index >= 0; index -= 1) {
      const key = window.sessionStorage.key(index)
      if (key?.startsWith(prefix)) window.sessionStorage.removeItem(key)
    }
  } catch {
    // Feed position state is best-effort and contains no durable user data.
  }
}
