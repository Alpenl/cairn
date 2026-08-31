/**
 * Reader display metadata value mappings.
 *
 * This module stays below the UI seam: no React imports and no component-layer
 * icon types. Components may turn these values into rendered icons elsewhere.
 */

/**
 * fetcher_type normalization: the backend may append quality suffixes such as
 * basic+thin, while display mappings only use the primary type.
 */
export function fetcherKey(raw: string | null | undefined): string {
  if (!raw) return ''
  return raw.split('+')[0]
}

/** fetcher_type (normalized) -> Chinese display label. */
export const FETCHER_LABEL: Record<string, string> = {
  basic: '网页',
  github: 'GitHub',
  arxiv: 'arXiv',
  pdf: 'PDF',
  jina: '渲染抓取',
  grok: 'AI 直读',
  wechat: '公众号',
}

/** fetcher_type -> Chinese display label; empty and unknown values stay absent. */
export function fetcherLabel(raw: string | null | undefined): string | undefined {
  const key = fetcherKey(raw)
  return key ? FETCHER_LABEL[key] : undefined
}

/** content_type -> Chinese display label. */
export const CONTENT_TYPE_LABEL: Record<string, string> = {
  article: '文章',
  listing: '列表页',
  homepage: '主页',
  feed: '订阅源',
  unknown: '未知',
}

/** content_type -> Chinese display label; unknown values preserve the backend value. */
export function contentTypeLabel(raw: string): string {
  return CONTENT_TYPE_LABEL[raw] || raw
}

/** Inbox source_kind -> Chinese display label. */
export const INBOX_SOURCE_LABEL: Record<string, string> = {
  browser_capture: '网页捕获',
  extension: '扩展捕获',
  rss: '订阅',
  subscription: '订阅',
  manual: '手动添加',
}

/** Inbox source_kind -> Chinese display label; unknown values preserve the backend value. */
export function inboxSourceLabel(kind: string): string {
  return INBOX_SOURCE_LABEL[kind] ?? kind
}

/**
 * Relative date: 今天 / 昨天 / N 天前 / MM-DD.
 * @param iso ISO timestamp
 * @param now comparison baseline, defaults to the current time
 */
export function relDate(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  const days = Math.floor((now.getTime() - d.getTime()) / 86400000)
  if (days <= 0) return '今天'
  if (days === 1) return '昨天'
  if (days < 7) return `${days} 天前`
  return (
    (d.getUTCMonth() + 1).toString().padStart(2, '0') +
    '-' +
    d.getUTCDate().toString().padStart(2, '0')
  )
}
