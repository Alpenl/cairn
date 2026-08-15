/**
 * PlainTextView —— 把「保存原文」按纯文本渲染（white-space: pre-wrap），不走 markdown
 * 解析。原文多来自 readability 的 TextContent（纯文本），若用 MarkdownView 渲染，行首的
 * `#` / `1.` / `>` / `|` / 反引号等会被当成 markdown 语法，导致标题/列表/引用/表格/代码块
 * 错误成形、原文显示扭曲。纯文本渲染则原样呈现，对 jina 返回的 markdown 源码也只是字面
 * 显示（不破坏结构），是更安全的默认。
 *
 * 保留划线：与 MarkdownView 用同一套「[data-hl-block] 块内字符偏移」锚定模型。纯文本是
 * 单一连续字符串，offset 即字符串下标，比 markdown 的多文本节点拼接更直接。命中划线区间的
 * 文本切成 <mark>，偏移坐标系与 getSelectionInfo（纯 Range 测量）一致，故新建/绘制都正确。
 */
import { useCallback, useMemo } from 'react'
import type { MouseEvent, ReactNode } from 'react'

import {
  annotationLocator,
  annotationLocatorTargetKey,
  blockHighlights,
  type Annotation,
  type AnnotationLocator,
} from '../lib/annotations'

export interface PlainTextViewProps {
  text: string
  /** 划线锚定块 key。 */
  blockKey: string
  anns: Annotation[]
  onClickHL: (annotation: AnnotationLocator, rect: DOMRect) => void
  className?: string
}

export function PlainTextView({ text, blockKey, anns, onClickHL, className }: PlainTextViewProps) {
  const highlights = useMemo(() => blockHighlights(anns, blockKey), [anns, blockKey])

  // 纯文本是一个连续字符串：按已排序、不重叠的划线区间切成 文本 / <mark> 片段。
  const segments = useMemo<ReactNode[]>(() => {
    if (highlights.length === 0) return [text]
    const out: ReactNode[] = []
    let cursor = 0
    for (const h of highlights) {
      const hs = Math.max(0, Math.min(h.start, text.length))
      const he = Math.max(hs, Math.min(h.end, text.length))
      const locator = annotationLocator(h)
      const targetKey = locator ? annotationLocatorTargetKey(locator) : null
      if (hs > cursor) out.push(text.slice(cursor, hs))
      out.push(
        <mark
          key={`${h.id}\0${targetKey ?? 'invalid'}\0${h.start}`}
          className={'hl' + (h.note ? ' has-note' : '') + (h.source === 'ai' ? ' ai' : '')}
          data-ann={h.id}
          {...(targetKey === null ? {} : { 'data-ann-target': targetKey })}
        >
          {text.slice(hs, he)}
        </mark>,
      )
      cursor = he
    }
    if (cursor < text.length) out.push(text.slice(cursor))
    return out
  }, [text, highlights])

  // 事件委托：点击落在某条划线 <mark data-ann> 上 → 打开对应笔记。
  const onClick = useCallback(
    (e: MouseEvent<HTMLDivElement>) => {
      const mark = (e.target as HTMLElement).closest('mark[data-ann]') as HTMLElement | null
      if (!mark) return
      const annotation = highlights.find((item) => {
        const locator = annotationLocator(item)
        if (!locator) return false
        return locator.id === mark.dataset.ann &&
          annotationLocatorTargetKey(locator) === mark.dataset.annTarget
      })
      if (!annotation) return
      const locator = annotationLocator(annotation)
      if (!locator) return
      e.stopPropagation()
      onClickHL(
        locator,
        mark.getBoundingClientRect(),
      )
    },
    [highlights, onClickHL],
  )

  return (
    <div
      className={'plain' + (className ? ' ' + className : '')}
      data-hl-block={blockKey}
      style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}
      onClick={onClick}
    >
      {segments}
    </div>
  )
}
