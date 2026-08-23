/**
 * Reader 展示元信息的纯值映射。
 *
 * 不依赖 React 或组件层类型；UI 图标名映射留在 components 层。
 */

/**
 * fetcher_type 归一化：后端会给低质量结果追加后缀（如 basic+thin），
 * 展示映射只认主类型。取「+」前的主段即可。
 */
export function fetcherKey(raw: string | null | undefined): string {
  if (!raw) return ''
  return raw.split('+')[0]
}

/** fetcher_type（已归一化）→ 中文标签。 */
export const FETCHER_LABEL: Record<string, string> = {
  basic: '网页',
  github: 'GitHub',
  arxiv: 'arXiv',
  pdf: 'PDF',
  jina: '渲染抓取',
  grok: 'AI 直读',
  wechat: '公众号',
}

/** fetcher_type → 中文标签，空值 / 未知值返回 undefined。 */
export function fetcherLabel(raw: string | null | undefined): string | undefined {
  const key = fetcherKey(raw)
  return key ? FETCHER_LABEL[key] : undefined
}

/** content_type → 中文标签。 */
export const CONTENT_TYPE_LABEL: Record<string, string> = {
  article: '文章',
  listing: '列表页',
  homepage: '主页',
  feed: '订阅源',
  unknown: '未知',
}

/** content_type → 中文标签，未命中时保留后端原值。 */
export function contentTypeLabel(raw: string): string {
  return CONTENT_TYPE_LABEL[raw] || raw
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
