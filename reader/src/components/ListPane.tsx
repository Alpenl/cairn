/**
 * 列表面板。
 * 列表头（视图名 + N 条 + 排序占位）、卡片列表、独立滚动。
 *
 * 空态之外补充 loading / error 态（复用 .empty
 * 容器 + faint 文案 + 错误重试按钮），以及滚动到底加载更多。
 */
import { useCallback, type UIEvent } from 'react'
import { Icon } from './Icon'
import { LinkCard } from './LinkCard'
import type { ApiError } from '../lib/api/result'
import type { LinkResponse } from '../lib/api/types'

export interface ListPaneProps {
  title: string
  links: LinkResponse[]
  activeId: string | null
  onSelect: (id: string) => void
  loading: boolean
  loadingMore: boolean
  error: ApiError | null
  hasMore: boolean
  onLoadMore: () => void
  onReload: () => void
  /** 未授权错误时引导去设置。 */
  onOpenSettings: () => void
}

/** 错误态文案（按 error.kind 给出可操作提示）。 */
function errorHint(error: ApiError): string {
  switch (error.kind) {
    case 'unauthorized':
      return 'API token 无效或已过期，请检查连接配置'
    case 'network-unreachable':
      return '无法连接后端，请确认地址正确且服务在运行'
    case 'timeout':
      return '请求超时，请重试'
    case 'rate-limited':
      return '请求过于频繁，请稍后再试'
    default:
      return error.message || '加载失败'
  }
}

export function ListPane({
  title,
  links,
  activeId,
  onSelect,
  loading,
  loadingMore,
  error,
  hasMore,
  onLoadMore,
  onReload,
  onOpenSettings,
}: ListPaneProps) {
  const onScroll = useCallback(
    (e: UIEvent<HTMLDivElement>) => {
      if (!hasMore || loadingMore || loading) return
      const el = e.currentTarget
      if (el.scrollHeight - el.scrollTop - el.clientHeight < 120) onLoadMore()
    },
    [hasMore, loadingMore, loading, onLoadMore],
  )

  return (
    <section className="list-pane">
      <div className="list-head">
        <div>
          <div className="list-title">{title}</div>
          <div className="list-sub">
            {links.length} 条
          </div>
        </div>
        <div className="list-sort">
          <Icon name="sort" size={14} /> 最新
        </div>
      </div>
      <div className="list-scroll" onScroll={onScroll}>
        {loading && (
          <div style={{ padding: '60px 20px', textAlign: 'center', color: 'var(--text-faint)', fontSize: 13 }}>
            正在加载…
          </div>
        )}

        {!loading && error && (
          <div style={{ padding: '52px 24px', textAlign: 'center', color: 'var(--text-faint)', fontSize: 13 }}>
            <div style={{ marginBottom: 12 }}>{errorHint(error)}</div>
            <button
              type="button"
              className="link-btn"
              onClick={error.kind === 'unauthorized' ? onOpenSettings : onReload}
            >
              {error.kind === 'unauthorized' ? '前往设置' : '重试'}
            </button>
          </div>
        )}

        {!loading && !error && links.length === 0 && (
          <div style={{ padding: '60px 20px', textAlign: 'center', color: 'var(--text-faint)', fontSize: 13 }}>
            这个筛选下还没有链接
          </div>
        )}

        {!loading &&
          !error &&
          links.map((l) => (
            <LinkCard key={l.id} l={l} active={l.id === activeId} onSelect={onSelect} />
          ))}

        {loadingMore && (
          <div style={{ padding: '18px', textAlign: 'center', color: 'var(--text-faint)', fontSize: 12 }}>
            加载更多…
          </div>
        )}
      </div>
    </section>
  )
}
