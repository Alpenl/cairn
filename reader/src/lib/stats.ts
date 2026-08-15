/**
 * 侧栏 / 详情面板的 tagStats、domainStats 与 relatedTags 派生统计。
 *
 * 接受已加载链接数组作为入参（来自 useLinks
 * 的当前结果集），保持算法 1:1。
 *
 * 二期标注：lastAt 在后端当前无对应字段（无 last_referenced_at），v1 用 created_at
 * 近似「最近收录」，语义上略有偏差但与设计原型一致（原型同样用 created_at）。
 */
import type { LinkResponse } from './api/types'

/** 标签统计：标签名 + 引用次数 + 最近收录时间（近似）。 */
export interface TagStat {
  tag: string
  count: number
  lastAt: string
}

/** 域名统计：域名 + 引用次数 + 最近收录时间（近似）。 */
export interface DomainStat {
  domain: string
  count: number
  lastAt: string
}

/** The exact link predicate shared by every Reading sidebar projection. */
export function isCompletedReadingLink(link: LinkResponse): boolean {
  return link.library_kind === 'reading' && link.status === 'done'
}

/** 从链接集合派生标签统计（对齐 sidebar.jsx tagStats）。 */
export function tagStats(links: LinkResponse[]): TagStat[] {
  const m = new Map<string, TagStat>()
  links.forEach((l) =>
    l.tags.forEach((t) => {
      const s = m.get(t) || { tag: t, count: 0, lastAt: '' }
      s.count += 1
      if (l.created_at > s.lastAt) s.lastAt = l.created_at
      m.set(t, s)
    }),
  )
  return [...m.values()]
}

/** 从链接集合派生域名统计（对齐 sidebar.jsx domainStats）。 */
export function domainStats(links: LinkResponse[]): DomainStat[] {
  const m = new Map<string, DomainStat>()
  links.forEach((l) => {
    if (!l.domain) return
    const s = m.get(l.domain) || { domain: l.domain, count: 0, lastAt: '' }
    s.count += 1
    if (l.created_at > s.lastAt) s.lastAt = l.created_at
    m.set(l.domain, s)
  })
  return [...m.values()]
}

/**
 * 相关阅读标签（客户端近似：标签共现 + 同域名加权）。对齐 sidebar.jsx relatedTags。
 * 这是已加载语料上的局部推荐，不等同于后端搜索使用的语义相似度。
 *
 * @param link 当前文章
 * @param corpus 派生语料（已加载链接集合）
 */
export function relatedTags(
  link: LinkResponse | null | undefined,
  corpus: LinkResponse[],
): string[] {
  if (!link) return []
  const own = new Set(link.tags)
  const score = new Map<string, number>()
  corpus.forEach((l) => {
    if (l.id === link.id) return
    const shared =
      l.tags.filter((t) => own.has(t)).length + (l.domain === link.domain ? 0.5 : 0)
    if (shared <= 0) return
    l.tags.forEach((t) => {
      if (!own.has(t)) score.set(t, (score.get(t) || 0) + shared)
    })
  })
  return [...score.entries()].sort((a, b) => b[1] - a[1]).map(([t]) => t)
}
