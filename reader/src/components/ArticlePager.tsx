import { Icon } from './Icon'

export interface ArticlePagerTarget {
  title: string
  onSelect: () => void
}

interface ArticlePagerProps {
  previous?: ArticlePagerTarget | null
  next?: ArticlePagerTarget | null
  className?: string
}

export function ArticlePager({ previous, next, className = '' }: ArticlePagerProps) {
  if (!previous && !next) return null

  return (
    <nav
      className={'article-pager' + (className ? ` ${className}` : '')}
      aria-label="文章导航"
    >
      {previous && (
        <button
          type="button"
          className="article-pager-button previous"
          onClick={previous.onSelect}
          aria-label={`上一条：${previous.title}`}
          title="上一条"
        >
          <span className="article-pager-direction">
            <Icon name="arrowright" size={12} />
            上一条
          </span>
          <span className="article-pager-title">{previous.title}</span>
        </button>
      )}
      {next && (
        <button
          type="button"
          className="article-pager-button next"
          onClick={next.onSelect}
          aria-label={`下一条：${next.title}`}
          title="下一条"
        >
          <span className="article-pager-direction">
            下一条
            <Icon name="arrowright" size={12} />
          </span>
          <span className="article-pager-title">{next.title}</span>
        </button>
      )}
    </nav>
  )
}
