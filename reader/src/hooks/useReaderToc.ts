/**
 * useReaderToc —— 正文目录的状态与副作用（大纲归属判定、滚动高亮、点跳转）。
 *
 * 从 DetailPane 抽出来的原因不是行数，是这三件事彼此耦合、和详情页其余逻辑不耦合：
 * 大纲什么时候作废决定高亮什么时候清零，高亮又要在跳转期间被锁住。放一处才看得懂。
 * 渲染交给 ReaderToc，摆位交给 CSS，这里只管「哪些条目、哪条是当前、点了怎么走」。
 */
import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'

import type { TocHeading } from '../lib/toc'

/** 少于三条标题的「大纲」没有足够的导航价值。渲染与高亮共用这个门槛。 */
export const MIN_TOC_HEADINGS = 3
/** 点目录跳转时给标题上方留的呼吸位。 */
const HEADING_JUMP_OFFSET = 18
/**
 * 标题越过阅读区上缘这么多像素后，算作「当前章节」。必须略大于 HEADING_JUMP_OFFSET：
 * 跳转后目标标题停在 18px 处，判定线更靠上就轮不到它高亮；判定线过深（设计稿用的
 * 130px）则短章节里紧随其后的下一个标题会抢走高亮，点 A 高亮 B。
 */
const ACTIVE_HEADING_OFFSET = HEADING_JUMP_OFFSET + 10
/** 平滑滚动期间锁住高亮的时长，够一次 scroll-behavior:smooth 走完。 */
const HEADING_LOCK_MS = 600
/** 稳定的空大纲：让「无目录」时的引用不变，避免 effect 每次渲染都重跑。 */
const NO_HEADINGS: TocHeading[] = []

function sameHeadings(a: TocHeading[], b: TocHeading[]): boolean {
  return (
    a.length === b.length &&
    a.every((item, i) => item.id === b[i].id && item.level === b[i].level && item.text === b[i].text)
  )
}

export interface UseReaderTocOptions {
  /** 阅读区滚动容器：高亮测量与跳转都以它为坐标系。 */
  scrollRef: RefObject<HTMLElement>
  /** 大纲归属：换文章 / 切原文译文 / 切排版文本，旧目录都要立刻作废。 */
  sourceKey: string
  /** 影响标题位置的排版设置（专注模式、字号行距）；变了要重算高亮。 */
  layoutKey: string
  /** Capability-off consumers keep hook order but install no DOM side effects. */
  enabled?: boolean
}

export interface ReaderTocState {
  items: TocHeading[]
  activeId: string | null
  /** 交给渲染正文的 MarkdownView：它每次渲染都回传大纲，判重在这里做。 */
  onHeadings: (items: TocHeading[]) => void
  /** 接到阅读区的 onScroll 上。 */
  onScroll: () => void
  jumpTo: (id: string) => void
}

export function useReaderToc({
  scrollRef,
  sourceKey,
  layoutKey,
  enabled = true,
}: UseReaderTocOptions): ReaderTocState {
  const [source, setSource] = useState<{ key: string; items: TocHeading[] }>({
    key: '',
    items: NO_HEADINGS,
  })
  const [activeId, setActiveId] = useState<string | null>(null)
  const rafRef = useRef(0)
  // 点目录后的高亮锁：平滑滚动到文末章节时滚动条会先撞底，此时该标题离上缘仍
  // 超过 ACTIVE_HEADING_OFFSET，滚动回调会把高亮抢回上一节。锁住这段时间。
  const lockRef = useRef<{ id: string; timer: number } | null>(null)

  const items = enabled && source.key === sourceKey ? source.items : NO_HEADINGS
  const hasToc = items.length >= MIN_TOC_HEADINGS

  const clearLock = useCallback(() => {
    if (!lockRef.current) return
    clearTimeout(lockRef.current.timer)
    lockRef.current = null
  }, [])

  // 当前章节 = 最后一个越过阅读区上缘的标题（和设计稿一致）。
  const syncActive = useCallback(() => {
    if (lockRef.current) return
    const scroller = scrollRef.current
    if (!scroller) return
    const paneTop = scroller.getBoundingClientRect().top
    // 撞底后正文不会再往上走，文末几节的标题永远越不过上缘。此时改用「最后一个
    // 露在视口里的标题」，否则点最后一节，高亮会一直卡在它上一节。
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= 2
    const limit = atBottom ? scroller.clientHeight : ACTIVE_HEADING_OFFSET
    let current: string | null = null
    for (const heading of Array.from(scroller.querySelectorAll<HTMLElement>('[data-toc-heading]'))) {
      if (heading.getBoundingClientRect().top - paneTop < limit) current = heading.id
    }
    setActiveId((prev) => (prev === current ? prev : current))
  }, [scrollRef])

  // 滚动里每帧最多量一次：高亮要读所有标题的 rect，直接挂在 onScroll 上会抖。
  // 没有目录时连量都不用量。
  const onScroll = useCallback(() => {
    if (!enabled || !hasToc || rafRef.current) return
    rafRef.current = requestAnimationFrame(() => {
      rafRef.current = 0
      syncActive()
    })
  }, [enabled, hasToc, syncActive])

  useEffect(
    () => () => {
      if (rafRef.current) cancelAnimationFrame(rafRef.current)
      if (lockRef.current) clearTimeout(lockRef.current.timer)
    },
    [],
  )

  // 换正文来源时立刻松锁：锁是为上一篇的那次跳转设的，留着只会压住新正文的高亮。
  useEffect(() => clearLock, [clearLock, sourceKey])

  // 大纲换了（含换文章）、正文宽度 / 字号变了都立刻重算：标题位置整体位移过，
  // 不该等用户先滚一下才纠正高亮。
  useEffect(() => {
    if (!enabled) {
      setActiveId(null)
      return
    }
    if (!hasToc) {
      setActiveId(null)
      return
    }
    syncActive()
  }, [enabled, items, hasToc, layoutKey, syncActive])

  useEffect(() => {
    if (!enabled || !hasToc) return
    const onResize = () => onScroll()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [enabled, hasToc, onScroll])

  // 判重口径：来源 key + 大纲内容。只比内容会漏掉「换了文章但大纲文字相同」，
  // 只比 key 会在同一篇里被每次渲染的新数组刷成新状态。
  //
  // useCallback 不是可选的：这个回调作为 prop 一路传进已 memo 的 MarkdownView。
  // 每渲染新建一个函数会让 memo 的浅比较恒判 props 变化，于是整篇 markdown
  // 在每次无关状态变动时被重新 parse —— memo 加了但不生效。
  const onHeadings = useCallback(
    (next: TocHeading[]) => {
      if (!enabled) return
      setSource((prev) =>
        prev.key === sourceKey && sameHeadings(prev.items, next) ? prev : { key: sourceKey, items: next },
      )
    },
    [enabled, sourceKey],
  )

  const jumpTo = (id: string) => {
    if (!enabled) return
    const scroller = scrollRef.current
    const target = scroller?.querySelector<HTMLElement>(`[id="${id}"]`)
    if (!scroller || !target) return
    const delta = target.getBoundingClientRect().top - scroller.getBoundingClientRect().top
    scroller.scrollTo({ top: scroller.scrollTop + delta - HEADING_JUMP_OFFSET, behavior: 'smooth' })
    setActiveId(id)
    // 焦点跟着走：目录是导航控件，不移焦点的话键盘 / 读屏用户点完还留在目录里。
    target.focus({ preventScroll: true })
    clearLock()
    lockRef.current = {
      id,
      timer: window.setTimeout(() => {
        lockRef.current = null
        syncActive()
      }, HEADING_LOCK_MS),
    }
  }

  return { items, activeId, onHeadings, onScroll, jumpTo }
}
