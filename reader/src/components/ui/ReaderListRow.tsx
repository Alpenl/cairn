/**
 * Reader 通用列表行。
 *
 * 取代 vNext Feed / Trash / Home 最近打开各写一遍的
 * 「元信息 + 标题 + 摘要 + footer + actions」行结构与对应的响应式 CSS。
 * 三个调用方的差异只有密度，所以用 variant 表达，不再各写一套布局规则。
 *
 * 固定 DOM：
 *   li.reader-list-row
 *   ├── .reader-list-row-meta（source + 调用方元信息）
 *   ├── button | strong.reader-list-row-title
 *   ├── .reader-list-row-summary
 *   ├── .reader-list-row-details
 *   └── .reader-list-row-foot（footer + actions）
 *
 * 打开行为同样由真实 button 承载；没有 onOpen 时降级成 strong，不留一个
 * 点不动的按钮。dataAttributes 用于保留调用方的 data-* 定位属性
 * （FeedSurface 的 data-feed-item-key / data-resource-key 靠它做滚动锚点）。
 */
import type { ReactNode } from 'react'

/** 行密度。feed：Feed 卡片式；dense：Trash 的圆角行卡；compact：Home 的紧凑行。 */
export type ReaderListRowVariant = 'feed' | 'dense' | 'compact'

export interface ReaderListRowProps {
  variant: ReaderListRowVariant
  source?: ReactNode
  meta?: ReactNode
  title: ReactNode
  summary?: ReactNode
  details?: ReactNode
  footer?: ReactNode
  actions?: ReactNode
  onOpen?: () => void
  openLabel?: string
  busy?: boolean
  className?: string
  dataAttributes?: Record<string, string>
}

function present(node: ReactNode): boolean {
  return node !== undefined && node !== null && node !== false
}

export function ReaderListRow({
  variant,
  source,
  meta,
  title,
  summary,
  details,
  footer,
  actions,
  onOpen,
  openLabel,
  busy = false,
  className,
  dataAttributes,
}: ReaderListRowProps) {
  const classes = ['reader-list-row', `reader-list-row-${variant}`]
  if (busy) classes.push('is-busy')
  if (className) classes.push(className)
  const hasMeta = present(source) || present(meta)
  const hasFoot = present(footer) || present(actions)
  return (
    <li className={classes.join(' ')} {...dataAttributes}>
      {hasMeta && (
        <div className="reader-list-row-meta">
          {present(source) && <span className="reader-list-row-source">{source}</span>}
          {meta}
        </div>
      )}
      {onOpen ? (
        <button
          type="button"
          className="reader-list-row-title"
          disabled={busy}
          aria-label={openLabel}
          onClick={onOpen}
        >
          {title}
        </button>
      ) : (
        <strong className="reader-list-row-title">{title}</strong>
      )}
      {present(summary) && <p className="reader-list-row-summary">{summary}</p>}
      {present(details) && <div className="reader-list-row-details">{details}</div>}
      {hasFoot && (
        <div className="reader-list-row-foot">
          {present(footer) && <div className="reader-list-row-footer">{footer}</div>}
          {present(actions) && <div className="reader-list-row-actions">{actions}</div>}
        </div>
      )}
    </li>
  )
}
