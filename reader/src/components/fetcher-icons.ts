import type { IconName } from './Icon'
import { fetcherKey } from '../lib/metadata'

/**
 * fetcher_type → 图标名。键为后端实际产出的主类型（见 internal/fetcher 各
 * FetcherType / FetcherTag：basic/arxiv/github/jina/pdf/grok/wechat）。
 * 查表前务必先经 fetcherKey() 剥掉 +thin 等后缀。
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

/** fetcher_type → UI 图标名，未命中回退 link。 */
export function fetcherIcon(raw: string | null | undefined): IconName {
  return FETCHER_ICON[fetcherKey(raw)] || 'link'
}
