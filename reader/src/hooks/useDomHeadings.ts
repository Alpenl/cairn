/**
 * useDomHeadings —— 给「已经是 DOM 的正文」建目录锚点。
 *
 * 正文目录有两种来源，共用同一套渲染（ReaderToc）与同一套滚动高亮（useReaderToc）：
 *   - markdown 正文：走 MarkdownView 的 rehype 插件，在渲染那棵 hast 树上写锚点，
 *     条目与锚点天生同源（见 lib/toc.ts）。
 *   - 已清洗的 HTML 正文（RSS 订阅）：没有中间树可挂，只能在渲染完成后遍历真实
 *     DOM 补锚点。本 hook 就是这一种。
 *
 * 两条路径写出的锚点属性完全一致（id / data-toc-heading / tabIndex），所以下游的
 * 高亮、跳转、焦点交接不需要知道正文原本是什么格式。
 */
import { useEffect, type RefObject } from 'react'

import { collectHeadingsFromDOM, type TocHeading } from '../lib/toc'

export function useDomHeadings<T extends HTMLElement>(
  rootRef: RefObject<T>,
  /** 正文内容签名：变了就重新扫一遍锚点（换文章、换清洗结果）。 */
  contentKey: string,
  /** 锚点 id 前缀，同页多块正文要区分开。 */
  prefix: string,
  onHeadings: ((headings: TocHeading[]) => void) | undefined,
): void {
  useEffect(() => {
    if (!onHeadings) return
    const root = rootRef.current
    if (!root) {
      onHeadings([])
      return
    }
    onHeadings(collectHeadingsFromDOM(root, prefix))
    // contentKey 进依赖：正文换了必须重扫，否则目录还指着上一篇的锚点。
  }, [rootRef, contentKey, prefix, onHeadings])
}
