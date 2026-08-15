/**
 * 链接卡片。
 * 解析字段仍随 API 数据进入卡片，但阅读工作区只呈现来源、时间、标题和摘要。
 */
import { memo, useMemo } from 'react'
import { Icon } from './Icon'
import { fetcherIcon, relDate } from '../lib/meta'
import type { LinkResponse } from '../lib/api/types'

export interface LinkCardProps {
  l: LinkResponse
  active: boolean
  /** 稳定的选中回调（接收 id），配合 React.memo 让无关 state 变化不重渲染整列。 */
  onSelect: (id: string) => void
}

function LinkCardInner({ l, active, onSelect }: LinkCardProps) {
  const fIcon = fetcherIcon(l.fetcher_type)
  // summaryLine 含 split/map/filter/join，按 summary 缓存，避免无关重渲染重算。
  const summaryLine = useMemo(() => {
    // 摘要为多分点（每点「- 」开头、\n 换行）。卡片是 2 行截断预览，
    // 把分点摊平成单行：剥掉「- 」标记，换行用「 · 」分隔。
    return (l.summary || '')
      .split('\n')
      .map((s) => s.replace(/^\s*[-•]\s*/, '').trim())
      .filter(Boolean)
      .join(' · ')
  }, [l.summary])
  return (
    <div className={'card' + (active ? ' active' : '')} onClick={() => onSelect(l.id)}>
      <div className="card-top">
        <span className="src-badge">
          <span className="src-ic">
            <Icon name={fIcon} size={13} sw={1.8} />
          </span>
          {l.domain || '—'}
        </span>
        <span className="card-spacer" />
        <span className="card-time">{relDate(l.created_at)}</span>
      </div>
      {l.title ? (
        <h3>{l.title}</h3>
      ) : (
        <h3 className="card-url">{l.url.replace(/^https?:\/\//, '')}</h3>
      )}
      <p>{summaryLine}</p>
    </div>
  )
}

// React.memo：列表里几十~数百张卡片，父级任一无关 state 变化（toast / chatOpen /
// pop 等）原本会重渲染整列。props 都是基本类型 + 稳定的 onSelect，浅比较几乎零成本，
// 只有该卡片的 l / active / onSelect 真正变化时才重渲染。
export const LinkCard = memo(LinkCardInner)
