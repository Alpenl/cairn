/**
 * MarkdownView 的懒加载边界（PF9）。
 *
 * markdown 渲染栈（react-markdown + remark/rehype/micromark/mdast/unist 一大坨）
 * 是首屏体积的主力：157 KB 原始 / 47 KB gzip，而它只在详情面板真正渲染正文时
 * 才用得上。vite.config.ts 早就把它切成了独立 chunk，但 DetailPane 的静态
 * import 仍然让它进首屏——那条注释里留的 TODO 就是这件事。
 *
 * 降级渲染不是空白：加载期间用 PlainTextView 渲染同一段文本。用户看到的是
 * 内容（只是暂时没有排版），而不是骨架屏或空洞——对阅读器来说这个差别很大。
 */
import { Suspense, lazy } from 'react'

import { PlainTextView } from './PlainTextView'
import type { MarkdownViewProps } from './MarkdownView'

const MarkdownViewLazy = lazy(async () => {
  const module = await import('./MarkdownView')
  return { default: module.MarkdownView }
})

/**
 * 这里**刻意不加 memo**。
 *
 * 初版加了，并在注释里声称「不 memo 的话 Suspense 边界会在每次父渲染时重建，
 * 把已解析的 markdown 丢掉」——那个论断是错的：MarkdownViewLazy 是模块级常量，
 * 元素类型稳定，重渲染不会卸载已解析的子树；而真正挡住重复 parse 的是
 * MarkdownView 自己那层 memo。审查用变异证实了这一点（去掉此处 memo，
 * 重复 parse 的计数一条都不变）。
 *
 * 留一个没有守卫、注释还在夸大其作用的 memo，比不留更糟：它会让后来的人以为
 * 这里有一道闸。
 */
export function LazyMarkdownView(props: MarkdownViewProps) {
  return (
    <Suspense
      fallback={
        <PlainTextView
          className={props.className}
          blockKey={props.blockKey}
          text={props.text}
          anns={props.anns}
          onClickHL={props.onClickHL}
        />
      }
    >
      <MarkdownViewLazy {...props} />
    </Suspense>
  )
}

