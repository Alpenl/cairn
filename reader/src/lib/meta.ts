/**
 * 展示元信息映射、日期工具与钉选 hook。
 *
 * 运行时使用 new Date()；relDate /
 * fmtDateTime 保留可注入基准参数（默认取当前时间），便于测试稳定断言。
 */
import { useCallback, useState } from 'react'
import { readOwnedStorage, writeOwnedStorage } from './storage-ownership'
import type { IconName } from '../components/Icon'

/** source_type → 图标名（对齐 components.jsx SRC_ICON）。 */
export const SRC_ICON: Record<string, IconName> = {
  rss: 'rss',
  x: 'x',
  weibo: 'weibo',
  youtube: 'youtube',
  url: 'link',
  pdf: 'doc',
  markdown: 'type',
}

/** source_type → 品牌色（对齐 components.jsx SRC_COLOR）。 */
export const SRC_COLOR: Record<string, string> = {
  rss: '#e8772e',
  x: '#1d9bf0',
  weibo: '#e6162d',
  youtube: '#ff0000',
  url: '#888',
  pdf: '#d23c3c',
  markdown: 'var(--accent)',
}

/**
 * fetcher_type 归一化：后端会给 fetcher_type 追加后缀（如 jina+light、basic+thin、
 * github+search），展示映射只认主类型。取「+」前的主段即可。
 */
export function fetcherKey(raw: string | null | undefined): string {
  if (!raw) return ''
  return raw.split('+')[0]
}

/**
 * fetcher_type → 图标名。键为后端实际产出的主类型（见 internal/fetcher 各
 * FetcherType / FetcherTag：basic/ytdlp/arxiv/github/jina/pdf/light/grok/search/wechat）。
 * 查表前务必先经 fetcherKey() 剥掉 +light / +thin / +search 等后缀。
 */
export const FETCHER_ICON: Record<string, IconName> = {
  basic: 'globe',
  light: 'globe',
  github: 'code',
  arxiv: 'doc',
  pdf: 'doc',
  ytdlp: 'youtube',
  jina: 'type',
  grok: 'sparkles',
  search: 'search',
  wechat: 'rss',
}

/** fetcher_type（已归一化）→ 图标名，未命中回退 link。 */
export function fetcherIcon(raw: string | null | undefined): IconName {
  return FETCHER_ICON[fetcherKey(raw)] || 'link'
}

/** content_type → 中文标签。 */
export const CONTENT_TYPE_LABEL: Record<string, string> = {
  article: '文章',
  listing: '列表页',
  homepage: '主页',
  feed: '订阅源',
  unknown: '未知',
}

/**
 * 相对日期：今天 / 昨天 / N 天前 / MM-DD。
 * @param iso ISO 时间字符串
 * @param now 比较基准，默认当前时间（测试可注入固定基准）
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

/** 钉选数据形状（标签 / 域名）。 */
export interface Pins {
  tags: string[]
  domains: string[]
}

/** 钉选 kind。 */
export type PinKind = keyof Pins

function loadPins(): Pins {
  try {
    const raw = JSON.parse(readOwnedStorage('pins') || '{}') as Partial<Pins>
    // 逐项校验：损坏 / 旧版本数据可能把 tags|domains 写成非数组，spread 覆盖默认值后
    // 会让 usePins 的 .includes 抛 TypeError；这里在持久化数据边界 fail closed。
    return {
      tags: Array.isArray(raw?.tags) ? raw.tags : [],
      domains: Array.isArray(raw?.domains) ? raw.domains : [],
    }
  } catch {
    return { tags: [], domains: [] }
  }
}

/** 钉选标签 / 域名，localStorage 持久化（键 webtag:pins:v1）。 */
export function usePins(): [Pins, (kind: PinKind, name: string) => void] {
  const [pins, setPins] = useState<Pins>(loadPins)
  const toggle = useCallback((kind: PinKind, name: string) => {
    setPins((p) => {
      const has = p[kind].includes(name)
      const next: Pins = {
        ...p,
        [kind]: has ? p[kind].filter((x) => x !== name) : [...p[kind], name],
      }
      writeOwnedStorage('pins', JSON.stringify(next))
      return next
    })
  }, [])
  return [pins, toggle]
}
