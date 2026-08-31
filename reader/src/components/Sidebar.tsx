/**
 * 智能视图 / 标签 / 域名 / 订阅源侧栏。
 * 折叠态持久化（webtag:sbfold:*），钉选优先 + 活跃 top-N。
 *
 * 标签/域名及全部链接数来自后端 truthful 聚合；无聚合接口的
 * 智能视图只保留导航，不显示伪全局数字。
 */
import { useMemo } from 'react'
import { Icon } from './Icon'
import { PrimaryNav, type LibraryView } from './PrimaryNav'
import { FoldGroup, SbRow } from './SidebarRows'
import type { Selection } from '../hooks/useLinks'
import type { DomainStat, TagStat } from '../lib/stats'
import type { Pins, PinKind } from '../lib/meta'
import {
  compareReaderActivityKeyAsc,
  compareReaderActivityLastAtDesc,
  useReaderActivity,
} from '../hooks/useReaderActivity'
import type { ReaderCapabilityPolicy } from '../lib/capabilities'
import type { ReaderRoute } from '../lib/navigation/route'
import type { ReaderAmbientClientPort } from '../lib/reader-api-ports'

export interface SidebarProps {
  sel: Selection
  onSelect: (s: Selection) => void
  view: LibraryView
  onView: (v: LibraryView) => void
  collapsed?: boolean
  pins: Pins
  onTogglePin: (kind: PinKind, name: string) => void
  onBrowse: (kind: 'tags' | 'domains') => void
  /** 派生统计（来自已加载链接 + 后端聚合）。 */
  tags: TagStat[]
  domains: DomainStat[]
  /** False hides an unavailable aggregate instead of showing an approximate zero. */
  tagsAvailable?: boolean
  domainsAvailable?: boolean
  /** 仅包含后端可证明为全库值的计数。 */
  counts: { all?: number; annotated?: number }
  /** Optional explicit client for isolated surfaces; production passes the active Reader client. */
  readerClient?: ReaderAmbientClientPort
  /** Canonical route owner shared with the vNext surface rail. */
  onNavigate?: (route: ReaderRoute) => void
  capabilityPolicy: ReaderCapabilityPolicy
}

export function Sidebar({
  sel,
  onSelect,
  view,
  onView,
  collapsed = false,
  pins,
  onTogglePin,
  onBrowse,
  tags,
  domains,
  tagsAvailable = true,
  domainsAvailable = true,
  counts,
  readerClient,
  onNavigate,
  capabilityPolicy,
}: SidebarProps) {
  const tagActivity = useReaderActivity(readerClient, [], { kind: 'tag', enabled: tagsAvailable && capabilityPolicy.activity })
  const domainActivity = useReaderActivity(readerClient, [], { kind: 'domain', enabled: domainsAvailable && capabilityPolicy.activity })
  const isSel = (type: string, id: string) =>
    view === 'reading' && sel.type === type && sel.id === id
  const go = (type: Selection['type'], id: string, name: string) => {
    onView('reading')
    onSelect({ type, id, name } as Selection)
  }

  // 标签：钉选优先，其余按活跃度（最近收录 > 数量）补足到 6（对齐 sidebar.jsx）。
  const activityTags = useMemo(() => {
    if (tagActivity.source !== 'server') return tags
    return tags.flatMap((tag) => {
      const lastAt = tagActivity.tagLastAt.get(tag.tag)
      if (lastAt) return [{ ...tag, lastAt }]
      return pins.tags.includes(tag.tag) ? [{ ...tag, lastAt: '' }] : []
    })
  }, [pins.tags, tagActivity.source, tagActivity.tagLastAt, tags])

  const activityDomains = useMemo(() => {
    if (domainActivity.source !== 'server') return domains
    return domains.flatMap((domain) => {
      const lastAt = domainActivity.domainLastAt.get(domain.domain)
      if (lastAt) return [{ ...domain, lastAt }]
      return pins.domains.includes(domain.domain) ? [{ ...domain, lastAt: '' }] : []
    })
  }, [domainActivity.domainLastAt, domainActivity.source, domains, pins.domains])

  const tagRows = useMemo<(TagStat & { pinned?: boolean })[]>(() => {
    const byName = new Map(activityTags.map((t) => [t.tag, t]))
    const pinned: (TagStat & { pinned: boolean })[] = pins.tags
      .filter((t) => byName.has(t))
      .map((t) => ({ ...(byName.get(t) as TagStat), pinned: true }))
    const rest = activityTags
      .filter((t) => !pins.tags.includes(t.tag))
      .sort((a, b) => {
        const recentOrder = compareReaderActivityLastAtDesc(a.lastAt, b.lastAt)
        if (recentOrder !== 0) return recentOrder
        return compareReaderActivityKeyAsc(a.tag, b.tag)
      })
    return [...pinned, ...rest.slice(0, Math.max(0, 6 - pinned.length))]
  }, [activityTags, pins.tags])

  const domainRows = useMemo<(DomainStat & { pinned?: boolean })[]>(() => {
    const byName = new Map(activityDomains.map((d) => [d.domain, d]))
    const pinned: (DomainStat & { pinned: boolean })[] = pins.domains
      .filter((d) => byName.has(d))
      .map((d) => ({ ...(byName.get(d) as DomainStat), pinned: true }))
    const rest = activityDomains
      .filter((d) => !pins.domains.includes(d.domain))
      .sort((a, b) => {
        const recentOrder = compareReaderActivityLastAtDesc(a.lastAt, b.lastAt)
        if (recentOrder !== 0) return recentOrder
        return compareReaderActivityKeyAsc(a.domain, b.domain)
      })
    return [...pinned, ...rest.slice(0, Math.max(0, 5 - pinned.length))]
  }, [activityDomains, pins.domains])

  const navigatePrimary = (route: ReaderRoute) => {
    if (onNavigate) {
      onNavigate(route)
      return
    }
    if (route.kind === 'library') onView(route.id)
  }

  return (
    <aside className={'sidebar' + (collapsed ? ' collapsed' : '')} id="primary-navigation">
      <PrimaryNav activeLibrary={view} onNavigate={navigatePrimary} policy={capabilityPolicy} />
      <div className="sb-group smart-group">
        <SbRow
          icon="stack"
          name="全部链接"
          count={counts.all}
          active={isSel('smart', 'all')}
          onClick={() => go('smart', 'all', '全部链接')}
        />
        {/* 主导航的「今天」是那张日报 surface，这里是「今天新增的链接」这个
            筛选。同一根侧栏里不能出现两个「今天」。 */}
        <SbRow
          icon="sun"
          name="今天新增"
          active={isSel('smart', 'today')}
          onClick={() => go('smart', 'today', '今天新增')}
        />
        <SbRow
          icon="marker"
          name="有划线"
          count={counts.annotated}
          active={isSel('smart', 'annotated')}
          onClick={() => go('smart', 'annotated', '有划线')}
        />
        {/* 网站库从主导航降级到这里：它是同一批收藏的另一种聚合方式，不是
            第五个资料库。下面的「域名」分区留着——那是在阅读列表里按域名筛选，
            和站点实体（有分类、标签、置顶）不是一回事。 */}
        <SbRow
          icon="stack"
          name="按站点"
          active={false}
          onClick={() => onNavigate?.({ kind: 'library', id: 'sites' })}
        />
      </div>

      {tagsAvailable && <FoldGroup
        label="标签"
        k="tags"
        defaultOpen={true}
        activeHint={sel.type === 'tag' ? '#' + sel.id : null}
        status={!tagActivity.loading && tagActivity.degraded ? '近似' : undefined}
      >
        {tagRows.map((t) => (
          <SbRow
            key={t.tag}
            glyph="#"
            name={t.tag}
            count={t.count}
            pinned={!!t.pinned}
            onPin={() => onTogglePin('tags', t.tag)}
            active={isSel('tag', t.tag)}
            onClick={() => go('tag', t.tag, '#' + t.tag)}
          />
        ))}
        <div className="sb-more" onClick={() => onBrowse('tags')}>
          浏览全部 {tags.length} 个标签
          <Icon name="arrowright" size={12} sw={2} />
        </div>
      </FoldGroup>}

      {domainsAvailable && <FoldGroup
        label="域名"
        k="domains"
        defaultOpen={false}
        activeHint={sel.type === 'domain' ? sel.id : null}
        status={!domainActivity.loading && domainActivity.degraded ? '近似' : undefined}
      >
        {domainRows.map((d) => (
          <SbRow
            key={d.domain}
            name={d.domain}
            count={d.count}
            mono
            indent
            pinned={!!d.pinned}
            onPin={() => onTogglePin('domains', d.domain)}
            active={isSel('domain', d.domain)}
            onClick={() => go('domain', d.domain, d.domain)}
          />
        ))}
        <div className="sb-more" onClick={() => onBrowse('domains')}>
          浏览全部 {domains.length} 个域名
          <Icon name="arrowright" size={12} sw={2} />
        </div>
      </FoldGroup>}

    </aside>
  )
}
