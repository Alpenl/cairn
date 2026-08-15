import type {
  HTMLReadingSource,
  MarkdownReadingSource,
  PlainReadingSource,
  ReadingSourceIdentity,
} from './types'

/**
 * A compact fallback version for sources that do not have a server revision.
 * Feed items normally use their updated timestamp; this keeps a content-only
 * fixture or legacy response from retaining a TOC for a different body.
 */
export function readingTextVersion(text: string): string {
  let hash = 2166136261
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index)
    hash = Math.imul(hash, 16777619)
  }
  return `${text.length}:${hash >>> 0}`
}

function assertIdentity(identity: ReadingSourceIdentity): ReadingSourceIdentity {
  if (identity.hostId.trim() === '') throw new Error('reading source hostId must not be empty')
  if (identity.version.trim() === '') throw new Error('reading source version must not be empty')
  return { hostId: identity.hostId, version: identity.version }
}

export function markdownSource(
  text: string,
  identity: ReadingSourceIdentity,
  blockKey = 'content',
): MarkdownReadingSource {
  if (blockKey.trim() === '') throw new Error('reading source blockKey must not be empty')
  return { kind: 'markdown', text, identity: assertIdentity(identity), blockKey }
}

export function plainSource(
  text: string,
  identity: ReadingSourceIdentity,
  blockKey = 'content',
): PlainReadingSource {
  if (blockKey.trim() === '') throw new Error('reading source blockKey must not be empty')
  return { kind: 'plain', text, identity: assertIdentity(identity), blockKey }
}

export function htmlSource(
  html: string,
  baseURL: string,
  identity: ReadingSourceIdentity,
  blockKey = 'content',
): HTMLReadingSource {
  if (blockKey.trim() === '') throw new Error('reading source blockKey must not be empty')
  let parsedURL: URL
  try {
    parsedURL = new URL(baseURL)
  } catch {
    throw new Error('reading source baseURL must be an absolute URL')
  }
  if (parsedURL.protocol !== 'http:' && parsedURL.protocol !== 'https:') {
    throw new Error('reading source baseURL must use http or https')
  }
  return {
    kind: 'html',
    html,
    baseURL: parsedURL.href,
    identity: assertIdentity(identity),
    blockKey,
  }
}
