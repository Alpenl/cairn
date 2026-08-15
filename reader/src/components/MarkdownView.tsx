/**
 * MarkdownView —— 用 react-markdown + remark-gfm 渲染完整 markdown（标题/列表/
 * 表格/链接/代码块/引用/删除线等），替代原手写的极简解析。
 *
 * 关键：保留划线功能。划线锚定模型是「[data-hl-block] 块内字符偏移」，而偏移基于
 * 文本节点拼接（textContent，不含块间合成换行）。这里把整段 markdown 渲染进单个
 * [data-hl-block] 容器，并用一个 rehype 插件按块内字符偏移把划线区间包成 <mark>，
 * 偏移坐标系与 getSelectionInfo（lib/annotations.ts，已改为纯 Range 测量）完全一致，
 * 故新建/绘制划线都正确。react-markdown 渲染成 React 元素、不走 innerHTML，天然防 XSS。
 */
import { memo, useCallback, useEffect, useMemo, useRef } from 'react'
import type { MouseEvent, ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

import {
  annotationLocator,
  annotationLocatorTargetKey,
  blockHighlights,
  type Annotation,
  type AnnotationLocator,
} from '../lib/annotations'
import { rehypeHeadingIds, type HeadingIdsOptions, type TocHeading } from '../lib/toc'
import type { HastNode } from '../lib/hast'
import { BlockedContentImage } from './BlockedContentImage'

/** 模块级常量：内联 [remarkGfm] 会每渲染新建数组，白白让 unified 重建 processor。 */
const REMARK_PLUGINS = [remarkGfm]

/**
 * rehypeAnnotations —— hast 转换插件：按「整棵树文本节点拼接」的字符偏移，把命中
 * 划线区间的文本切出来包进 <mark>。highlights 已按 start 排序且不重叠（blockHighlights
 * 保证）。在文档顺序里单调累加 offset，与 DOM textContent / Range 偏移一致。
 */
function rehypeAnnotations(highlights: Annotation[]) {
  return (tree: HastNode) => {
    if (highlights.length === 0) return
    let offset = 0

    const splitText = (value: string, base: number): HastNode[] => {
      const segEnd = base + value.length
      const overlaps = highlights.filter((h) => h.start < segEnd && h.end > base)
      if (overlaps.length === 0) return [{ type: 'text', value }]
      const pieces: HastNode[] = []
      let cursor = base
      for (const h of overlaps) {
        const hs = Math.max(h.start, base)
        const he = Math.min(h.end, segEnd)
        const locator = annotationLocator(h)
        const targetKey = locator ? annotationLocatorTargetKey(locator) : null
        if (hs > cursor) pieces.push({ type: 'text', value: value.slice(cursor - base, hs - base) })
        pieces.push({
          type: 'element',
          tagName: 'mark',
          properties: {
            className: ['hl', ...(h.note ? ['has-note'] : []), ...(h.source === 'ai' ? ['ai'] : [])],
            dataAnn: h.id,
            ...(targetKey === null ? {} : { dataAnnTarget: targetKey }),
          },
          children: [{ type: 'text', value: value.slice(hs - base, he - base) }],
        })
        cursor = he
      }
      if (cursor < segEnd) pieces.push({ type: 'text', value: value.slice(cursor - base) })
      return pieces
    }

    const walk = (node: HastNode) => {
      if (!node.children) return
      const next: HastNode[] = []
      for (const child of node.children) {
        // hast-util-to-jsx-runtime drops inter-element formatting whitespace
        // directly under table structure nodes. Do not count or replace those
        // nodes, or annotation offsets diverge from the mounted DOM and a mark
        // can become an invalid child of table/thead/tr.
        const isTableFormattingWhitespace =
          child.type === 'text' &&
          typeof child.value === 'string' &&
          /^(table|thead|tbody|tfoot|tr)$/.test(node.tagName || '') &&
          /^[\t\n\r ]+$/.test(child.value)
        if (isTableFormattingWhitespace) {
          next.push(child)
          continue
        }
        // react-markdown converts raw HAST nodes to escaped text after rehype
        // plugins run. Count and split them here as text too, otherwise a
        // selection at/after literal `<tag>` markup cannot be redrawn at the
        // offsets produced by getSelectionInfo.
        if ((child.type === 'text' || child.type === 'raw') && typeof child.value === 'string') {
          const base = offset
          offset += child.value.length
          next.push(...splitText(child.value, base))
        } else {
          walk(child)
          next.push(child)
        }
      }
      node.children = next
    }
    walk(tree)
  }
}

export interface MarkdownViewProps {
  text: string
  /** 划线锚定块 key（整段 markdown 作为一个 [data-hl-block]）。 */
  blockKey: string
  anns: Annotation[]
  onClickHL: (annotation: AnnotationLocator, rect: DOMRect) => void
  className?: string
  /** 传入则给标题写 `<prefix>-h<n>` 锚点 id；正文目录靠它跳转。 */
  headingIdPrefix?: string
  /**
   * 每次渲染后回传本块的标题大纲。这里**不**替调用方去重：本组件只知道「大纲长
   * 什么样」，不知道它属于哪篇文章——两篇文章大纲文字碰巧一样时，去重会让调用方
   * 收不到「换文章了」的通知，目录直接消失。判重交给持有归属信息的一方。
   */
  onHeadings?: (headings: TocHeading[]) => void
}

function MarkdownViewInner({
  text,
  blockKey,
  anns,
  onClickHL,
  className,
  headingIdPrefix,
  onHeadings,
}: MarkdownViewProps) {
  const contentRef = useRef<HTMLDivElement>(null)
  const highlights = useMemo(() => blockHighlights(anns, blockKey), [anns, blockKey])
  // 划线内容签名：react-markdown 在 rehypePlugins 引用变化时会重跑整条 remark→hast
  // 管线（含整篇 markdown 重新 parse）。blockHighlights 每次 anns 变化都返回新数组，
  // 哪怕本块划线没变（如在*另一个* block 划线）——会无谓触发本块重 parse。用内容签名
  // 把 rehypePlugins 的身份稳定下来：仅当本块划线集合真正变化时才重建，跨块的 anns
  // 变动不再波及本块。
  const hlSig = useMemo(
    () => highlights.map((h) => {
      const locator = annotationLocator(h)
      const targetKey = locator ? annotationLocatorTargetKey(locator) : null
      return `${h.id}:${targetKey ?? 'invalid'}:${h.start}:${h.end}:${h.note ? 1 : 0}:${h.source}`
    }).join('|'),
    [highlights],
  )
  // tuple 形式 [attacher, options]：rehypeAnnotations 是 attacher（接 highlights 返回
  // transformer），交给 unified 调用——不能在这里就调用它（那会变成把 transformer
  // 当 attacher 传，unified 二次调用导致树错乱）。
  type AnnotationPlugin = [typeof rehypeAnnotations, Annotation[]]
  type HeadingPlugin = [typeof rehypeHeadingIds, HeadingIdsOptions]
  // 标题大纲随渲染产出：插件把结果写进 ref，渲染完成后再由 effect 回传，
  // 避免在 render 期间 setState。
  const headingsRef = useRef<TocHeading[]>([])
  const collectHeadings = useRef((headings: TocHeading[]) => {
    headingsRef.current = headings
  }).current
  const pluginSig = `${headingIdPrefix || ''}\u0000${hlSig}`
  const pluginCache = useRef<{
    signature: string
    plugins: (AnnotationPlugin | HeadingPlugin)[]
  } | null>(null)
  if (pluginCache.current?.signature !== pluginSig) {
    const plugins: (AnnotationPlugin | HeadingPlugin)[] = [[rehypeAnnotations, highlights]]
    if (headingIdPrefix) {
      plugins.push([rehypeHeadingIds, { prefix: headingIdPrefix, collect: collectHeadings }])
    }
    pluginCache.current = { signature: pluginSig, plugins }
  }
  const rehypePlugins = pluginCache.current.plugins

  // 无依赖数组：每次渲染后都把当前大纲交出去。调用方按自己的口径判重（见上方 prop
  // 注释），所以这里的重复回调不会引起状态抖动或循环。
  useEffect(() => {
    onHeadings?.(headingsRef.current)
  })

  // 外链统一新标签打开 + 安全 rel（其余元素用 react-markdown 默认渲染；rehype 注入的
  // <mark> 也由默认渲染输出真实 DOM，className/data-ann 透传，点击走容器级事件委托）。
  const components = useMemo(
    () => ({
      a({ href, children }: { href?: string; children?: ReactNode }) {
        return (
          <a href={href} target="_blank" rel="noopener noreferrer">
            {children}
          </a>
        )
      },
      img({ alt }: { alt?: string }) {
        return <BlockedContentImage alt={alt} />
      },
    }),
    [],
  )

  // 事件委托：点击落在某条划线 <mark data-ann> 上 → 打开对应笔记。避免给每个 mark
  // 挂监听，也绕开自定义 mark 组件在 react-markdown 下的渲染问题。
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
      ref={contentRef}
      className={'md' + (className ? ' ' + className : '')}
      data-hl-block={blockKey}
      onClick={onClick}
    >
      <ReactMarkdown remarkPlugins={REMARK_PLUGINS} rehypePlugins={rehypePlugins} components={components}>
        {text}
      </ReactMarkdown>
    </div>
  )
}

/**
 * React.memo：react-markdown v10 的 Markdown() 没有任何记忆化——它在组件体里
 * 直接跑 createProcessor + parse + runSync（node_modules/react-markdown/lib/index.js:175-179），
 * 于是父组件每一次重渲染都会把整篇 markdown 重新 parse 一遍，再走一遍
 * rehypeAnnotations 的全树遍历。长文这是几十毫秒的主线程停顿，而触发它的
 * 往往是与文章毫无关系的状态变化（toast、hover、翻译轮询）。
 *
 * memo 生效的前提是**所有 props 引用稳定**，因此调用侧同步做了：
 * DetailPane 的 onClickHL 改 useCallback、两处 anns={[]} 改共享的
 * NO_ANNOTATIONS、useReaderToc 的 onHeadings 改 useCallback。少任何一处
 * 这层 memo 就是装饰品。
 */
export const MarkdownView = memo(MarkdownViewInner)
