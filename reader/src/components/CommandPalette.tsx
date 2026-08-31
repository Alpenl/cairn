/**
 * 命令面板（⌘K）。
 *
 * 结构：搜索框（关键词胶囊 + esc）→ 分组结果列表（标签 / 域名 / 链接 / 命令）。
 * 交互：上下键移动、回车执行、esc 关闭、鼠标 hover 选中。
 *
 * R3 接线：链接搜索接后端 `q=` 关键词搜索（useCommandSearch，300ms 防抖 + 本地即时
 * 预览 + feature-detect 降级）；`#` 前缀搜标签、否则标签 + 域名匹配（来自派生 stats）；
 * 命令组覆盖视图跳转 / AI / 订阅源 / 同步 / 切主题。
 */
import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { Icon, type IconName } from './Icon'
import { fetcherIcon } from './fetcher-icons'
import { useCommandSearch } from '../hooks/useCommandSearch'
import type { ReaderClient } from '../lib/api/client'
import type {
  LinkResponse,
  ReaderNoteSearchResponse,
  ReaderThoughtSearchResponse,
  SiteSearchResultResponse,
} from '../lib/api/types'
import { readerThoughtHostTarget } from '../lib/navigation/route'
import type { TagStat, DomainStat } from '../lib/stats'
import {
  deriveReaderCapabilityPolicy,
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from '../lib/capabilities'

/** 命令项的归一形态（视图 / AI / 链接 / 标签 / 域名统一结构）。 */
export interface CommandItem {
  id: string
  group: string
  label: string
  icon: IconName
  hint: string
  /** open: 命令携带命中的 Link，供详情 fallback 接管 ownership。 */
  link?: LinkResponse
  site?: SiteSearchResultResponse
  thought?: ReaderThoughtSearchResponse
  note?: ReaderNoteSearchResponse
  loadMoreThoughts?: true
}

const COMMANDS: CommandItem[] = [
  { id: 'nav:all', group: '视图', label: '全部链接', icon: 'stack', hint: '' },
  { id: 'pending', group: '导航', label: '收件箱', icon: 'inbox', hint: '' },
  { id: 'chat', group: 'AI', label: '打开 AI 助手', icon: 'chat', hint: '⌘J' },
  { id: 'subs', group: '导航', label: '订阅源（规划中）', icon: 'rss', hint: '' },
  { id: 'refresh', group: '导航', label: '同步资料库与想法', icon: 'refresh', hint: '' },
  { id: 'theme', group: '外观', label: '切换浅色 / 深色', icon: 'moon', hint: '' },
]

const CREATE_NOTE_COMMAND: CommandItem = {
  id: 'create-note',
  group: '笔记',
  label: '新建笔记',
  icon: 'plus',
  hint: '',
}

export interface CommandPaletteProps {
  open: boolean
  onClose: () => void
  onCommand: (c: CommandItem) => void
  client: ReaderClient
  /** 本地语料（已加载链接快照），用于即时预览 + 降级 + open: 标签生成。 */
  corpus: LinkResponse[]
  /** 派生标签统计（taxResults 候选）。 */
  tagStats: TagStat[]
  /** 派生域名统计（taxResults 候选）。 */
  domainStats: DomainStat[]
  /** Strict capability gate: unknown capability must not expose a write intent. */
  canCreateNote?: boolean
  capabilityPolicy?: ReaderCapabilityPolicy
}

const DEFAULT_CAPABILITY_POLICY = deriveReaderCapabilityPolicy(undefined)

export function CommandPalette({
  open,
  onClose,
  onCommand,
  client,
  corpus,
  tagStats,
  domainStats,
  canCreateNote = false,
  capabilityPolicy = DEFAULT_CAPABILITY_POLICY,
}: CommandPaletteProps) {
  const [q, setQ] = useState('')
  const [idx, setIdx] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)

  const {
    results: searchHits,
    sites: siteHits,
    thoughts: thoughtHits,
    notes: noteHits,
    thoughtTotalHint,
    hasMoreThoughts,
    loadingMoreThoughts,
    loadMoreThoughts,
    degraded,
  } = useCommandSearch(client, q, corpus, {
    remoteEnabled: true,
  })

  const linkResults = useMemo<CommandItem[]>(() => {
    if (!q.trim() || q.trim().startsWith('#')) return []
    return searchHits.slice(0, 5).map((l) => ({
      id: 'open:' + l.id,
      group: '链接',
      icon: fetcherIcon(l.fetcher_type),
      label: l.title || l.url.replace(/^https?:\/\//, ''),
      hint: l.domain || '',
      link: l,
    }))
  }, [q, searchHits])

  const siteResults = useMemo<CommandItem[]>(() => {
    if (!q.trim() || q.trim().startsWith('#')) return []
    if (!capabilityPolicy.siteRead) return []
    return siteHits.slice(0, 5).map((site) => ({
      id: 'site:' + site.id,
      group: '网站',
      icon: 'globe',
      label: site.name,
      hint: site.matched_entries.length ? site.matched_entries.map((entry) => entry.name || entry.url).join(' · ') : '网站资料匹配',
      site,
    }))
  }, [capabilityPolicy.siteRead, q, siteHits])

  const thoughtResults = useMemo<CommandItem[]>(() => {
    if (!q.trim() || q.trim().startsWith('#')) return []
    if (!capabilityPolicy.annotations) return []
    return thoughtHits.filter((thought) => {
      if (thought.lifecycle_status === 'tombstone') return true
      const target = readerThoughtHostTarget(thought)
      return target !== null && readerRouteIsAvailable(target.route, capabilityPolicy)
    }).map((thought) => ({
      id: 'thought:' + thought.id,
      group: '想法',
      icon: 'edit',
      label: thought.snippet || '未命名想法',
      hint: thought.link_id ? '阅读出处' : thought.host_kind,
      thought,
    }))
  }, [capabilityPolicy, q, thoughtHits])

  const moreThoughtResults = useMemo<CommandItem[]>(() => {
    if (!q.trim() || q.trim().startsWith('#') || !hasMoreThoughts) return []
    const remaining = Math.max(thoughtTotalHint - thoughtHits.length, 0)
    return [{
      id: 'thoughts:more',
      group: '想法',
      icon: 'more',
      label: loadingMoreThoughts ? '加载更多想法' : '更多想法',
      hint: loadingMoreThoughts ? '加载中' : remaining > 0 ? `还有 ${remaining} 条` : '下一页',
      loadMoreThoughts: true,
    }]
  }, [hasMoreThoughts, loadingMoreThoughts, q, thoughtHits.length, thoughtTotalHint])

  const noteResults = useMemo<CommandItem[]>(() => {
    if (!q.trim() || q.trim().startsWith('#')) return []
    if (!capabilityPolicy.notes) return []
    return noteHits.slice(0, 5).map((note) => ({
      id: 'note:' + note.id,
      group: '笔记',
      icon: 'doc',
      label: note.title || '未命名笔记',
      hint: note.snippet || `已发布 v${note.published_revision}`,
      note,
    }))
  }, [capabilityPolicy.notes, q, noteHits])

  // # 前缀 → 仅标签；否则标签 + 域名匹配。对齐 cmdk.jsx taxResults。
  const taxResults = useMemo<CommandItem[]>(() => {
    const raw = q.trim().toLowerCase()
    if (!raw) return []
    const k = raw.replace(/^#/, '')
    if (!k) return []
    const tags: CommandItem[] = tagStats
      .filter((t) => t.tag.toLowerCase().includes(k))
      .slice(0, 4)
      .map((t) => ({ id: 'tag:' + t.tag, group: '标签', icon: 'hash', label: t.tag, hint: t.count + ' 条' }))
    if (raw.startsWith('#')) return tags
    const doms: CommandItem[] = domainStats
      .filter((d) => d.domain.includes(k))
      .slice(0, 3)
      .map((d) => ({ id: 'domain:' + d.domain, group: '域名', icon: 'globe', label: d.domain, hint: d.count + ' 条' }))
    return [...tags, ...doms]
  }, [q, tagStats, domainStats])

  const filtered = useMemo<CommandItem[]>(() => {
    const k = q.toLowerCase()
    const trimmed = q.trim()
    if (trimmed.startsWith('#')) return taxResults
    const availableCommands = COMMANDS.filter((command) => {
      if (command.id === 'pending') return capabilityPolicy.inbox
      if (command.id === 'chat') return capabilityPolicy.ai
      return true
    })
    const cmds = trimmed
      ? availableCommands.filter((c) => c.label.toLowerCase().includes(k) || c.group.includes(trimmed))
      : availableCommands
    const createCommand = canCreateNote && (!trimmed || CREATE_NOTE_COMMAND.label.includes(trimmed))
      ? [CREATE_NOTE_COMMAND]
      : []
    return [...taxResults, ...linkResults, ...siteResults, ...thoughtResults, ...moreThoughtResults, ...noteResults, ...createCommand, ...cmds]
  }, [canCreateNote, capabilityPolicy.ai, capabilityPolicy.inbox, q, linkResults, moreThoughtResults, noteResults, siteResults, taxResults, thoughtResults])

  useEffect(() => {
    setIdx(0)
  }, [q])
  useEffect(() => {
    if (open) {
      setQ('')
      const t = window.setTimeout(() => inputRef.current?.focus(), 30)
      return () => window.clearTimeout(t)
    }
  }, [open])

  if (!open) return null

  const run = (c: CommandItem) => {
    if (c.loadMoreThoughts) {
      void loadMoreThoughts()
      return
    }
    onCommand(c)
    onClose()
  }

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setIdx((i) => Math.min(i + 1, filtered.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setIdx((i) => Math.max(i - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (filtered[idx]) run(filtered[idx])
    } else if (e.key === 'Escape') {
      onClose()
    }
  }

  let lastGroup: string | null = null
  return (
    <div className="cmdk-overlay" onMouseDown={onClose}>
      <div className="cmdk" onMouseDown={(e) => e.stopPropagation()}>
        <div className="cmdk-input">
          <Icon name="search" size={18} style={{ color: 'var(--text-tertiary)' }} />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={onKey}
            placeholder="搜索标题、摘要、域名… 输入 # 搜标签"
          />
          {q.trim() && (
            <span className="kbd" style={{ marginRight: 6 }}>
              {degraded ? '本地关键词' : '关键词'}
            </span>
          )}
          <span className="kbd">esc</span>
        </div>
        <div className="cmdk-list">
          {filtered.length === 0 && <div className="cmdk-empty">没有匹配的结果</div>}
          {filtered.map((c, i) => {
            const head = c.group !== lastGroup ? c.group : null
            lastGroup = c.group
            return (
              <Fragment key={c.id}>
                {head && <div className="cmdk-group">{head}</div>}
                <div
                  className={'cmdk-item' + (i === idx ? ' sel' : '')}
                  onMouseEnter={() => setIdx(i)}
                  onClick={() => run(c)}
                >
                  <span className="cmdk-ic">
                    <Icon name={c.icon} size={16} />
                  </span>
                  <span className="cmdk-label">{c.label}</span>
                  {c.hint && <span className="cmdk-hint">{c.hint}</span>}
                  {i === idx && (
                    <span className="kbd" style={{ marginLeft: 8 }}>
                      ↵
                    </span>
                  )}
                </div>
              </Fragment>
            )
          })}
        </div>
      </div>
    </div>
  )
}
