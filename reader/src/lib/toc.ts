/**
 * 正文目录 —— 从 markdown 正文解析出的标题大纲。
 *
 * 关键约束：目录条目与正文锚点必须来自**同一棵 hast 树**。若目录另起一套正则去扫
 * markdown 源码，遇到引用块内的标题、setext 标题、缩进代码块这类差异就会与
 * react-markdown 的真实渲染错位，点目录跳到错误的章节。所以这里做成 rehype 插件：
 * 渲染时顺手给标题写 id，并把同一次遍历收集到的条目回传给调用方。
 */
import type { HastNode } from './hast'

export interface TocHeading {
  id: string
  /** 标题级别 1-6，目录用它做缩进。 */
  level: number
  text: string
}

/** 少于三条标题的「大纲」没有足够的导航价值。渲染与高亮共用这个门槛。 */
export const MIN_TOC_HEADINGS = 3

/** 目录只收前三级；更深的标题照样拿到 id，只是不进目录（否则大纲会被淹没）。 */
const TOC_MAX_LEVEL = 3

const HEADING_TAG = /^h([1-6])$/

/** 锚点 id 的统一构造：两种正文来源必须给出同一形态，否则跳转逻辑要分叉。 */
function headingID(prefix: string, index: number): string {
  return `${prefix}-h${index}`
}

function textOf(node: HastNode): string {
  if (node.type === 'text') return node.value || ''
  return (node.children || []).map(textOf).join('')
}

export interface HeadingIdsOptions {
  /** id 前缀，最终形如 `<prefix>-h0`。同一页面里不同正文块要用不同前缀。 */
  prefix: string
  collect: (headings: TocHeading[]) => void
}

/**
 * rehypeHeadingIds —— 按文档顺序给 h1~h6 编号写 id，并把前 TOC_MAX_LEVEL 级标题
 * 收集出来。进目录的标题额外带 data-toc-heading（滚动高亮据此定位，不会把 h4+
 * 这类目录里没有的标题算成当前章节）和 tabIndex=-1（点目录跳转后要把焦点交给
 * 标题，否则键盘 / 读屏用户还留在目录里）。
 */
export function rehypeHeadingIds({ prefix, collect }: HeadingIdsOptions) {
  return (tree: HastNode) => {
    const headings: TocHeading[] = []
    let index = 0

    const walk = (node: HastNode) => {
      for (const child of node.children || []) {
        const matched = child.tagName ? HEADING_TAG.exec(child.tagName) : null
        if (matched) {
          const id = headingID(prefix, index++)
          const level = Number(matched[1])
          const text = textOf(child).trim()
          const inToc = level <= TOC_MAX_LEVEL && text !== ''
          child.properties = {
            ...child.properties,
            id,
            ...(inToc ? { dataTocHeading: '', tabIndex: -1 } : {}),
          }
          if (inToc) headings.push({ id, level, text })
        }
        walk(child)
      }
    }

    walk(tree)
    collect(headings)
  }
}

/**
 * collectHeadingsFromDOM —— 给已经渲染成 DOM 的正文（如 RSS 清洗后的 HTML）补
 * 目录锚点，并返回条目。写入的属性与 rehypeHeadingIds 完全一致，下游分不出、
 * 也不需要分辨正文原本是 markdown 还是 HTML。
 *
 * 直接改 DOM 是有意的：这类正文由 dangerouslySetInnerHTML 注入，React 不持有
 * 它的子树，没有中间表示可以挂锚点。每次正文变化都整体重扫（见 useDomHeadings）。
 */
export function collectHeadingsFromDOM(root: HTMLElement, prefix: string): TocHeading[] {
  const headings: TocHeading[] = []
  const nodes = root.querySelectorAll<HTMLElement>('h1, h2, h3, h4, h5, h6')
  nodes.forEach((node, index) => {
    const level = Number(node.tagName.slice(1))
    const text = (node.textContent || '').trim()
    const id = headingID(prefix, index)
    node.id = id
    if (level <= TOC_MAX_LEVEL && text !== '') {
      node.setAttribute('data-toc-heading', '')
      node.tabIndex = -1
      headings.push({ id, level, text })
    } else {
      // 四级以下 / 空标题：给 id 便于将来引用，但不进目录、不参与滚动高亮。
      node.removeAttribute('data-toc-heading')
    }
  })
  return headings
}
