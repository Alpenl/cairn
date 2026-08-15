import { Icon } from '../Icon'
import { MIN_TOC_HEADINGS } from '../../hooks/useReaderToc'
import type { TocHeading } from '../../lib/toc'

export interface ArticleOutlineProps {
  items: TocHeading[]
  activeId: string | null
  onJump: (id: string) => void
}

export function ArticleOutline({ items, activeId, onJump }: ArticleOutlineProps) {
  if (items.length < MIN_TOC_HEADINGS) return null

  return (
    <section className="reader-rail-section reader-rail-toc" aria-labelledby="reader-rail-toc-title">
      <h2 id="reader-rail-toc-title" className="reader-rail-title">
        <Icon name="stack" size={13} /> 目录
      </h2>
      <nav aria-label="正文目录">
        <ul>
          {items.map((item) => (
            <li key={item.id}>
              <button
                type="button"
                className={'reader-rail-toc-item' + (activeId === item.id ? ' cur' : '')}
                style={{ paddingInlineStart: (item.level - 1) * 11 }}
                title={item.text}
                aria-current={activeId === item.id ? 'true' : undefined}
                onClick={() => onJump(item.id)}
              >
                {item.text}
              </button>
            </li>
          ))}
        </ul>
      </nav>
    </section>
  )
}
