/** Stable ownership names shared by the build-time Workbox config and runtime reset path. */

export function normalizeReaderBasePath(base: string): string {
  let path: string
  try {
    path = new URL(base || '/', 'https://reader.invalid/').pathname
  } catch {
    path = '/'
  }
  if (!path.startsWith('/')) path = `/${path}`
  return path.endsWith('/') ? path : `${path}/`
}

export function readerCacheIDForBase(base: string): string {
  return `webtag-reader-${encodeURIComponent(normalizeReaderBasePath(base))}`
}

export function readerScopeURL(base: string, origin: string): string {
  return new URL(normalizeReaderBasePath(base), origin).href
}

/**
 * Match current cache names plus the Workbox defaults used before Reader had
 * an explicit cacheId. The legacy suffix is the exact registration scope, so
 * another Reader base on the same origin cannot be mistaken for this one.
 */
export function isReaderOwnedCacheName(name: string, base: string, origin: string): boolean {
  const cacheID = readerCacheIDForBase(base)
  if (name.startsWith(`${cacheID}-`)) return true

  const scope = readerScopeURL(base, origin)
  return name === `workbox-precache-v2-${scope}` || name === `workbox-runtime-${scope}`
}
