import type { FeedAnalysisStatus, FeedItem, FeedSubscription } from './api/types'

function feedURL(subscription: FeedSubscription): string {
  return subscription.feed_url ?? subscription.url ?? ''
}

export function feedTitle(subscription: FeedSubscription): string {
  if (subscription.title?.trim()) return subscription.title.trim()
  if (subscription.name?.trim()) return subscription.name.trim()
  try {
    return new URL(feedURL(subscription)).hostname
  } catch {
    return feedURL(subscription) || '未命名订阅源'
  }
}

export function feedItemSourceTitle(
  item: FeedItem,
  subscription?: FeedSubscription,
): string {
  if (subscription) return feedTitle(subscription)
  return item.subscription_title?.trim() || 'RSS'
}

export function feedError(subscription: FeedSubscription): string | null {
  return subscription.last_error ?? subscription.fetch_error ?? null
}

export function safeHTTPURL(raw: string): string | null {
  try {
    const parsed = new URL(raw)
    return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : null
  } catch {
    return null
  }
}

export function itemIsRead(item: FeedItem): boolean {
  return item.read ?? Boolean(item.read_at)
}

export function itemIsStarred(item: FeedItem): boolean {
  return item.starred ?? Boolean(item.starred_at)
}

export function itemIsReadLater(item: FeedItem): boolean {
  return item.read_later ?? Boolean(item.read_later_at)
}

export function itemAnalysisStatus(item: FeedItem): FeedAnalysisStatus {
  if (item.analysis_status) return item.analysis_status
  if (item.link_status) return item.link_status
  return item.link_id ? 'done' : 'none'
}

export function patchItemState(
  item: FeedItem,
  patch: { read?: boolean; starred?: boolean; read_later?: boolean },
): FeedItem {
  const now = new Date().toISOString()
  return {
    ...item,
    ...(patch.read === undefined
      ? {}
      : { read: patch.read, read_at: patch.read ? now : null }),
    ...(patch.starred === undefined
      ? {}
      : { starred: patch.starred, starred_at: patch.starred ? now : null }),
    ...(patch.read_later === undefined
      ? {}
      : { read_later: patch.read_later, read_later_at: patch.read_later ? now : null }),
  }
}

export function formatFeedDate(value?: string | null): string {
  if (!value) return '时间未知'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  const diff = Date.now() - date.getTime()
  const minute = 60_000
  const day = 24 * 60 * minute
  if (diff >= 0 && diff < minute) return '刚刚'
  if (diff >= minute && diff < 60 * minute) return `${Math.floor(diff / minute)} 分钟前`
  if (diff >= 60 * minute && diff < day) return `${Math.floor(diff / (60 * minute))} 小时前`
  if (diff >= day && diff < 7 * day) return `${Math.floor(diff / day)} 天前`
  return new Intl.DateTimeFormat('zh-CN', {
    year: date.getFullYear() === new Date().getFullYear() ? undefined : 'numeric',
    month: 'short',
    day: 'numeric',
  }).format(date)
}

export function itemPreview(item: FeedItem): string {
  const raw = item.summary ?? item.content ?? ''
  return raw.replace(/\s+/g, ' ').trim()
}
