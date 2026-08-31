/**
 * ReaderToc —— RSS 详情保留的兼容目录（设计稿 WebTag.html 的 nav[aria-label="目录"]）。
 *
 * 阅读资料库使用 ReaderRail；RSS 详情没有标签、阅读进度和持久化划线等同源数据，
 * 因此继续使用这个只提供目录的兼容组件。摆位与容器门槛仍由 app.css 控制。
 */
import type { TocHeading } from '../lib/toc'
import { hasRenderableOutline } from '../lib/outline-tree'
import { OutlineTree } from './detail/OutlineTree'

export interface ReaderTocProps {
  items: TocHeading[]
  activeId: string | null
  onJump: (id: string) => void
  focusMode: boolean
}

export function ReaderToc({ items, activeId, onJump, focusMode }: ReaderTocProps) {
  if (!hasRenderableOutline(items)) return null
  return (
    <nav className={'reader-toc' + (focusMode ? ' focused' : '')} aria-label="正文目录">
      <div className="reader-toc-inner">
        <div className="reader-toc-title">目录</div>
        <OutlineTree items={items} activeId={activeId} onJump={onJump} variant="reader" />
      </div>
    </nav>
  )
}
