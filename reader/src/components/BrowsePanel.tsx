/**
 * 标签 / 域名全集浏览器。
 *
 * 解决「标签和域名越来越多，侧栏放不下」：侧栏只放钉选 + 活跃 top-N，全集在这里，
 * 交互模型 = 搜索。tab 切换（标签 / 域名）、搜索自动聚焦、排序（常用 / 最近 / A-Z）、
 * 钉选到侧栏、esc 关闭、底部计数。
 *
 * 数据：标签全集来自派生 tagStats（接 getTags 聚合 / 已加载链接派生），域名全集来自
 * domainStats（接后端域名摘要）。「最近」排序依赖 lastAt，后端当前无该
 * 字段 → 用 created_at 近似（二期，与 stats.ts 注释一致）。
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './Icon'
import { relDate, type Pins, type PinKind } from '../lib/meta'
import type { TagStat, DomainStat } from '../lib/stats'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import {
  compareReaderActivityKeyAsc,
  compareReaderActivityLastAtDesc,
  useReaderActivity,
} from '../hooks/useReaderActivity'

type BrowseKind = 'tags' | 'domains'
type SortKey = 'count' | 'recent' | 'alpha'

interface BrowseEntry {
  name: string
  count: number
  lastAt: string
}

export interface BrowsePanelProps {
  kind: BrowseKind
  onClose: () => void
  /** 选中条目跳转：('tag' | 'domain', name)。 */
  onPick: (type: 'tag' | 'domain', name: string) => void
  pins: Pins
  onTogglePin: (kind: PinKind, name: string) => void
  /** 标签全集（派生 / 后端聚合）。 */
  tags: TagStat[]
  /** 域名全集（派生 / 后端聚合）。 */
  domains: DomainStat[]
  /** Optional explicit client for isolated surfaces; production passes the active Reader client. */
  readerClient?: IdentityBoundReaderClient
  /** Disabled or unknown activity support stays entirely local. */
  activityEnabled?: boolean
}

const SORTS: [SortKey, string][] = [
  ['count', '常用'],
  ['recent', '最近'],
  ['alpha', 'A-Z'],
]

export function BrowsePanel({
  kind,
  onClose,
  onPick,
  pins,
  onTogglePin,
  tags,
  domains,
  readerClient,
  activityEnabled = false,
}: BrowsePanelProps) {
  const [tab, setTab] = useState<BrowseKind>(kind)
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortKey>('count')
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    setTab(kind)
    setQ('')
  }, [kind])
  useEffect(() => {
    const t = window.setTimeout(() => inputRef.current?.focus(), 30)
    return () => window.clearTimeout(t)
  }, [kind, tab])
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [onClose])

  const isTags = tab === 'tags'
  const activity = useReaderActivity(readerClient, [], {
    kind: isTags ? 'tag' : 'domain',
    allPages: true,
    enabled: activityEnabled && sort === 'recent',
  })
  const items = useMemo<BrowseEntry[]>(() => {
    const tagActivity = activity.tagLastAt
    const domainActivity = activity.domainLastAt
    const base: BrowseEntry[] = isTags
      ? tags.flatMap((t) => {
          const lastAt = tagActivity.get(t.tag)
          if (activity.source === 'server' && !lastAt) return []
          return [{ name: t.tag, count: t.count, lastAt: lastAt ?? t.lastAt }]
        })
      : domains.flatMap((d) => {
          const lastAt = domainActivity.get(d.domain)
          if (activity.source === 'server' && !lastAt) return []
          return [{ name: d.domain, count: d.count, lastAt: lastAt ?? d.lastAt }]
        })
    const k = q.trim().toLowerCase().replace(/^#/, '')
    const filtered = k ? base.filter((x) => x.name.toLowerCase().includes(k)) : base
    const pinSet = new Set(isTags ? pins.tags : pins.domains)
    return [...filtered].sort((a, b) => {
      const pa = pinSet.has(a.name) ? 1 : 0
      const pb = pinSet.has(b.name) ? 1 : 0
      if (pa !== pb) return pb - pa
      if (sort === 'count') {
        if (b.count !== a.count) return b.count - a.count
        return a.name.localeCompare(b.name, 'zh')
      }
      if (sort === 'recent') {
        const recentOrder = compareReaderActivityLastAtDesc(a.lastAt, b.lastAt)
        if (recentOrder !== 0) return recentOrder
        return compareReaderActivityKeyAsc(a.name, b.name)
      }
      return a.name.localeCompare(b.name, 'zh')
    })
  }, [activity.domainLastAt, activity.source, activity.tagLastAt, domains, isTags, pins, q, sort, tags])

  const pinSet = new Set(isTags ? pins.tags : pins.domains)

  return (
    <div className="cmdk-overlay" onMouseDown={onClose}>
      <div className="cmdk browse" onMouseDown={(e) => e.stopPropagation()}>
        <div className="browse-tabs">
          <button className={'bt' + (isTags ? ' on' : '')} onClick={() => setTab('tags')}>
            <Icon name="hash" size={13} sw={2} /> 标签
          </button>
          <button className={'bt' + (!isTags ? ' on' : '')} onClick={() => setTab('domains')}>
            <Icon name="globe" size={13} sw={1.8} /> 域名
          </button>
          <span className="rt-grow" />
          <div className="browse-sorts">
            {SORTS.map(([s, lbl]) => (
              <button key={s} className={'bs' + (sort === s ? ' on' : '')} onClick={() => setSort(s)}>
                {lbl}
              </button>
            ))}
          </div>
        </div>
        <div className="cmdk-input">
          <Icon name="search" size={18} style={{ color: 'var(--text-tertiary)' }} />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={isTags ? '搜索标签…' : '搜索域名…'}
          />
          <span className="kbd">esc</span>
        </div>
        <div className="cmdk-list browse-list">
          {items.length === 0 && (
            <div className="cmdk-empty">没有匹配的{isTags ? '标签' : '域名'}</div>
          )}
          {items.map((x) => (
            <div
              key={x.name}
              className="cmdk-item browse-item"
              onClick={() => {
                onPick(isTags ? 'tag' : 'domain', x.name)
                onClose()
              }}
            >
              <span className={'tag-hash' + (isTags ? '' : ' dom')}>{isTags ? '#' : '@'}</span>
              <span
                className="cmdk-label"
                style={!isTags ? { fontFamily: 'var(--font-mono)', fontSize: 12.5 } : undefined}
              >
                {x.name}
              </span>
              <span className="browse-meta">
                {x.count} 条 · {relDate(x.lastAt)}
              </span>
              <span
                className={'pin-btn browse-pin' + (pinSet.has(x.name) ? ' on' : '')}
                title={pinSet.has(x.name) ? '取消钉选' : '钉选到侧栏'}
                onClick={(e) => {
                  e.stopPropagation()
                  onTogglePin(isTags ? 'tags' : 'domains', x.name)
                }}
              >
                <Icon name="pin" size={13} sw={2} />
              </span>
            </div>
          ))}
        </div>
        <div className="browse-foot">
          {sort === 'recent' && !activity.loading && activity.degraded && (
            <span className="activity-approximate">最近时间：本地近似 · </span>
          )}
          钉选的{isTags ? '标签' : '域名'}会固定显示在侧栏顶部 · 共 {items.length} 个
        </div>
      </div>
    </div>
  )
}
