/**
 * 侧栏摘要映射：后端标签/域名聚合 + 已见链接的 recent 元数据。
 *
 * done corpus total 来自后端摘要 envelope 的独立 total，因此无域名链接也会
 * 被计入。recentLinks 仅用于 lastAt 近似，绝不承担全局状态计数。
 */
import { useMemo } from 'react'
import {
  domainStats,
  isCompletedReadingLink,
  tagStats,
  type DomainStat,
  type TagStat,
} from '../lib/stats'
import type {
  DomainTreeSummaryResponse,
  LinkResponse,
  TagCountResponse,
} from '../lib/api/types'

export interface SidebarCounts {
  /** 后端域名摘要覆盖的全库 done 链接数；摘要不可用时不显示。 */
  all?: number
}

export interface SidebarData {
  counts: SidebarCounts
  tags: TagStat[]
  domains: DomainStat[]
  tagsAvailable: boolean
  domainsAvailable: boolean
}

function uniqueCompletedReadingLinks(links: LinkResponse[]): LinkResponse[] {
  const seen = new Set<string>()
  return links.filter((link) => {
    if (!isCompletedReadingLink(link) || seen.has(link.id)) return false
    seen.add(link.id)
    return true
  })
}

export function useSidebarData(
  recentLinks: LinkResponse[],
  backendTags: TagCountResponse[] | null,
  backendDomains: DomainTreeSummaryResponse[] | null,
  backendTotal: number | null,
  fallback: {
    links: LinkResponse[]
    total: number | null
    complete: boolean
  },
): SidebarData {
  const scopedRecentLinks = useMemo(
    () => recentLinks.filter(isCompletedReadingLink),
    [recentLinks],
  )
  const scopedFallbackLinks = useMemo(
    () => uniqueCompletedReadingLinks(fallback.links),
    [fallback.links],
  )
  const fallbackAuthoritative = fallback.complete &&
    fallback.total !== null &&
    scopedFallbackLinks.length === fallback.total
  const counts = useMemo<SidebarCounts>(
    () => {
      if (backendTotal !== null) return { all: backendTotal }
      if (fallback.total !== null) return { all: fallback.total }
      return {}
    },
    [backendTotal, fallback.total],
  )

  const tags = useMemo<TagStat[]>(() => {
    if (backendTags === null && !fallbackAuthoritative) return []
    if (backendTags === null) return tagStats(scopedFallbackLinks)
    const lastByTag = new Map(tagStats(scopedRecentLinks).map((t) => [t.tag, t.lastAt]))
    return backendTags.map((t) => ({
      tag: t.tag,
      count: t.count,
      lastAt: lastByTag.get(t.tag) || '',
    }))
  }, [backendTags, fallbackAuthoritative, scopedFallbackLinks, scopedRecentLinks])

  const domains = useMemo<DomainStat[]>(() => {
    if (backendDomains === null && !fallbackAuthoritative) return []
    if (backendDomains === null) return domainStats(scopedFallbackLinks)
    const lastByDomain = new Map(
      domainStats(scopedRecentLinks).map((d) => [d.domain, d.lastAt]),
    )
    return backendDomains.map((d) => ({
      domain: d.domain,
      count: d.count,
      lastAt: lastByDomain.get(d.domain) || '',
    }))
  }, [backendDomains, fallbackAuthoritative, scopedFallbackLinks, scopedRecentLinks])

  return {
    counts,
    tags,
    domains,
    tagsAvailable: backendTags !== null || fallbackAuthoritative,
    domainsAvailable: backendDomains !== null || fallbackAuthoritative,
  }
}
