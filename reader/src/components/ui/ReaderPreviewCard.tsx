/**
 * Reader 通用内容预览卡片。
 *
 * 取代 LinkCard / RSS FeedItemCard / Inbox 队列各写一遍的
 * 「来源 + 时间 + 标题 + 摘要 + 选中态」DOM。打开行为由**真实 button** 承载：
 * 点击、Enter、Space 全部来自浏览器原生按钮语义，调用方不再手写 keydown，
 * 也不需要给行内操作按钮加 stopPropagation()——它们是 main button 的兄弟节点，
 * 本来就不在按钮内部。
 *
 * 固定 DOM：
 *   article | li.reader-preview-card
 *   ├── .reader-preview-card-leading（可选）
 *   ├── button.reader-preview-card-main
 *   │   ├── .reader-preview-card-meta（source + time）
 *   │   ├── .reader-preview-card-title
 *   │   ├── .reader-preview-card-summary
 *   │   └── .reader-preview-card-details
 *   └── .reader-preview-card-actions（可选）
 */
import type { ReactNode } from 'react'

export interface ReaderPreviewCardProps {
  as?: 'article' | 'li'
  selected?: boolean
  read?: boolean
  picked?: boolean
  leading?: ReactNode
  source: ReactNode
  time?: ReactNode
  title: ReactNode
  summary?: ReactNode
  details?: ReactNode
  actions?: ReactNode
  openLabel: string
  onOpen: () => void
  className?: string
}

export function ReaderPreviewCard({
  as = 'article',
  selected = false,
  read = false,
  picked = false,
  leading,
  source,
  time,
  title,
  summary,
  details,
  actions,
  openLabel,
  onOpen,
  className,
}: ReaderPreviewCardProps) {
  const Root = as
  const classes = ['reader-preview-card']
  if (selected) classes.push('is-selected')
  if (read) classes.push('is-read')
  if (picked) classes.push('is-picked')
  if (className) classes.push(className)
  return (
    <Root className={classes.join(' ')} aria-current={selected ? 'true' : undefined}>
      {leading !== undefined && leading !== null && (
        <span className="reader-preview-card-leading">{leading}</span>
      )}
      <button type="button" className="reader-preview-card-main" aria-label={openLabel} onClick={onOpen}>
        <span className="reader-preview-card-meta">
          <span className="reader-preview-card-source">{source}</span>
          {time !== undefined && time !== null && <span className="reader-preview-card-time">{time}</span>}
        </span>
        <span className="reader-preview-card-title">{title}</span>
        {summary !== undefined && summary !== null && (
          <span className="reader-preview-card-summary">{summary}</span>
        )}
        {details !== undefined && details !== null && (
          <span className="reader-preview-card-details">{details}</span>
        )}
      </button>
      {actions !== undefined && actions !== null && (
        <span className="reader-preview-card-actions">{actions}</span>
      )}
    </Root>
  )
}
