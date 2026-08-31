import type { IconName } from './Icon'
import { fetcherKey } from '../lib/metadata'

/**
 * fetcher_type -> UI icon name. Keys are normalized backend fetcher types
 * (basic/arxiv/github/jina/pdf/grok/wechat).
 */
export const FETCHER_ICON: Record<string, IconName> = {
  basic: 'globe',
  github: 'code',
  arxiv: 'doc',
  pdf: 'doc',
  jina: 'type',
  grok: 'sparkles',
  wechat: 'rss',
}

/** fetcher_type -> UI icon name; unknown values fall back to link. */
export function fetcherIcon(raw: string | null | undefined): IconName {
  return FETCHER_ICON[fetcherKey(raw)] || 'link'
}
