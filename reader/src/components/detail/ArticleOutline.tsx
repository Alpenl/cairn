import { Icon } from '../Icon'
import type { TocHeading } from '../../lib/toc'
import { hasRenderableOutline } from '../../lib/outline-tree'
import { OutlineTree } from './OutlineTree'

export interface ArticleOutlineProps {
  items: TocHeading[]
  activeId: string | null
  onJump: (id: string) => void
}

export function ArticleOutline({ items, activeId, onJump }: ArticleOutlineProps) {
  if (!hasRenderableOutline(items)) return null

  return (
    <section className="reader-rail-section reader-rail-toc" aria-labelledby="reader-rail-toc-title">
      <h2 id="reader-rail-toc-title" className="reader-rail-title">
        <Icon name="stack" size={13} /> 目录
      </h2>
      <nav aria-label="正文目录">
        <OutlineTree items={items} activeId={activeId} onJump={onJump} variant="rail" />
      </nav>
    </section>
  )
}
