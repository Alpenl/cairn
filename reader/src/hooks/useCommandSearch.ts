/**
 * ⌘K 命令面板的分组搜索（阅读链接 + 网站 + 可分页想法）。
 *
 * 策略（对齐当前 Reader 分组搜索行为）：
 *   · 输入瞬时 → 本地过滤即时预览（语料 = 已加载链接快照）。
 *   · 300ms 防抖到点 → 调 `client.searchLibrary(q)` 用后端分组结果替换。
 *   · 想法首屏取 20 条；后续页仅合并未出现的 thought ID。
 *   · q 清空 → 回常规（结果清空）。
 *   · feature-detect：后端不支持 `q=`（返回 shape 异常 / 4xx）时退回纯本地过滤，
 *     `degraded` 置 true，不报错。
 *
 * stale 防护：用递增请求序号丢弃过期响应。
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import type {
  GroupedSearchResponse,
  LinkResponse,
  ReaderNoteSearchResponse,
  ReaderThoughtSearchResponse,
  SiteSearchResultResponse,
} from '../lib/api/types'
import type { ReaderCommandSearchPort } from '../lib/reader-api-ports'

type SearchableReaderCommandSearchPort = ReaderCommandSearchPort & {
  readonly searchLibrary: NonNullable<ReaderCommandSearchPort['searchLibrary']>
}

/** 本地过滤：标题 / url / 域名 / 摘要 / 标签命中关键词。对齐 cmdk.jsx linkResults。 */
export function localFilterLinks(links: LinkResponse[], q: string): LinkResponse[] {
  const k = q.trim().toLowerCase()
  if (!k) return []
  return links.filter(
    (l) =>
      (l.title || '').toLowerCase().includes(k) ||
      l.url.toLowerCase().includes(k) ||
      (l.domain || '').includes(k) ||
      (l.summary || '').toLowerCase().includes(k) ||
      l.tags.some((t) => t.toLowerCase().includes(k)),
  )
}

export interface CommandSearchResult {
  /** 当前用于展示的链接结果（后端优先，未到点 / 降级时为本地预览）。 */
  results: LinkResponse[]
  /** 后端命中的网站；本地降级时为空。 */
  sites: SiteSearchResultResponse[]
  /** 后端命中的已同步想法；旧后端缺少该可选组时为空。 */
  thoughts: ReaderThoughtSearchResponse[]
  /** 后端命中的已发布笔记；绝不包含草稿字段。 */
  notes: ReaderNoteSearchResponse[]
  /** 后端在 cursor 之前计算的完整想法命中数。 */
  thoughtTotalHint: number
  /** 当前想法页还有可请求的 cursor。 */
  hasMoreThoughts: boolean
  /** 追加下一页想法时保留当前结果，并阻止重复请求。 */
  loadingMoreThoughts: boolean
  /** 请求并按 ID 合并下一页想法；链接、网站和笔记保留首屏快照。 */
  loadMoreThoughts: () => Promise<void>
  /** 后端不支持 q= → 已退回本地过滤。 */
  degraded: boolean
}

const DEBOUNCE_MS = 300
const THOUGHT_PAGE_SIZE = 20

/**
 * @param client API client
 * @param q 当前输入
 * @param corpus 本地语料（已加载链接快照），用于即时预览 + 降级
 */
export function useCommandSearch(
  client: ReaderCommandSearchPort,
  q: string,
  corpus: LinkResponse[],
  options: { readonly remoteEnabled?: boolean } = {},
): CommandSearchResult {
  const [backend, setBackend] = useState<GroupedSearchResponse | null>(null)
  const [degraded, setDegraded] = useState(false)
  const [loadingMoreThoughts, setLoadingMoreThoughts] = useState(false)
  const seqRef = useRef(0)
  const moreRequestRef = useRef(false)

  // 标签搜索（# 前缀）不走链接后端搜索；空串清空。
  const trimmed = q.trim()
  const active = trimmed.length > 0 && !trimmed.startsWith('#')
  const remoteEnabled = options.remoteEnabled ?? true

  useEffect(() => {
	const seq = ++seqRef.current
	moreRequestRef.current = false
	setLoadingMoreThoughts(false)
    if (!active || !remoteEnabled) {
      setBackend(null)
      setDegraded(false)
      return
    }
    // 输入变化先清后端结果 → 立即用本地预览，等防抖到点再覆盖。
    setBackend(null)
    setDegraded(false)
    if (typeof client.searchLibrary !== 'function') {
      setDegraded(true)
      return
    }
    const searchClient = client as SearchableReaderCommandSearchPort
    const timer = window.setTimeout(() => {
      void Promise.resolve()
        .then(() => searchClient.searchLibrary(trimmed, 50, 10, THOUGHT_PAGE_SIZE))
        .then((res) => {
          if (!client.isIdentityCurrent()) return
          if (seq !== seqRef.current) return // stale
          if (res.ok) {
            setBackend(res.data)
            setDegraded(false)
          } else if (res.error.kind === 'other' && (res.error.status === 404 || res.error.status === undefined)) {
            // 旧后端没有分组搜索路由，或返回体不兼容 →
            // 降级本地过滤，不报错。
            setBackend(null)
            setDegraded(true)
          } else {
            // 网络 / 鉴权 / 超时 / 限流等瞬时错误：保持本地预览，不标记降级。
            setBackend(null)
          }
        })
        .catch(() => {
          if (!client.isIdentityCurrent() || seq !== seqRef.current) return
          // A legacy client or a transient transport failure has no grouped
          // payload to expose. Keep the truthful local link-only preview.
          setBackend(null)
          setDegraded(false)
        })
    }, DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [active, trimmed, client, remoteEnabled])

  const loadMoreThoughts = useCallback(async () => {
    const thoughtAfter = backend?.thoughts?.next_cursor?.trim()
    if (!active || !thoughtAfter || moreRequestRef.current || typeof client.searchLibrary !== 'function') return

    const seq = seqRef.current
    moreRequestRef.current = true
    setLoadingMoreThoughts(true)
    try {
      const res = await client.searchLibrary(trimmed, 50, 10, THOUGHT_PAGE_SIZE, thoughtAfter)
      if (!client.isIdentityCurrent() || seq !== seqRef.current || !res.ok) return
      setBackend((current) => {
        const currentThoughts = current?.thoughts
        if (current == null || currentThoughts == null) return current

        const incomingThoughts = res.data.thoughts
        if (incomingThoughts == null) {
          return { ...current, thoughts: { ...currentThoughts, next_cursor: undefined } }
        }
        const knownIDs = new Set(currentThoughts.items.map((thought) => thought.id))
        const items = [...currentThoughts.items]
        for (const thought of incomingThoughts.items) {
          if (!knownIDs.has(thought.id)) {
            knownIDs.add(thought.id)
            items.push(thought)
          }
        }
        return {
          ...current,
          thoughts: {
            ...currentThoughts,
            items,
            total_hint: Math.max(currentThoughts.total_hint, incomingThoughts.total_hint),
            next_cursor: incomingThoughts.next_cursor === thoughtAfter ? undefined : incomingThoughts.next_cursor,
          },
        }
      })
    } catch {
      // Keep the loaded page and its cursor so a transient failure can retry.
    } finally {
      if (seq === seqRef.current) {
        moreRequestRef.current = false
        setLoadingMoreThoughts(false)
      }
    }
  }, [active, backend, client, trimmed])

  if (!active) {
    return {
      results: [],
      sites: [],
      thoughts: [],
      notes: [],
      thoughtTotalHint: 0,
      hasMoreThoughts: false,
      loadingMoreThoughts: false,
      loadMoreThoughts,
      degraded,
    }
  }
  // 后端结果优先；未到点 / 降级时用本地预览。
  const results = backend?.reading.items ?? localFilterLinks(corpus, trimmed)
  const thoughtAfter = backend?.thoughts?.next_cursor?.trim() ?? ''
  return {
    results,
    sites: backend?.sites.items ?? [],
    thoughts: backend?.thoughts?.items ?? [],
    notes: backend?.notes?.items ?? [],
    thoughtTotalHint: backend?.thoughts?.total_hint ?? 0,
    hasMoreThoughts: thoughtAfter.length > 0,
    loadingMoreThoughts,
    loadMoreThoughts,
    degraded: remoteEnabled ? degraded : true,
  }
}
