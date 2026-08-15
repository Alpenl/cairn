import { useCallback, type KeyboardEvent, type UIEvent } from 'react'
import { Icon } from '../Icon'
import {
  feedError,
  feedItemSourceTitle,
  formatFeedDate,
  itemAnalysisStatus,
  itemIsRead,
  itemIsReadLater,
  itemIsStarred,
  itemPreview,
} from '../../lib/feed'
import type { ApiError } from '../../lib/api/result'
import type { FeedItem, FeedSubscription } from '../../lib/api/types'

interface FeedItemsPaneProps {
  title: string
  items: FeedItem[]
  total: number
  subscriptions: FeedSubscription[]
  selectedSubscription?: FeedSubscription
  activeID: string | null
  query: string
  loading: boolean
  loadingMore: boolean
  error: ApiError | null
  hasMore: boolean
  refreshing: boolean
  onQueryChange: (query: string) => void
  onSelect: (item: FeedItem) => void
  onToggleStar: (item: FeedItem) => void
  onToggleLater: (item: FeedItem) => void
  onLoadMore: () => void
  onReload: () => void
  onRefresh: () => void
  onMarkAllRead: () => void
  onOpenSettings: () => void
}

function errorHint(error: ApiError): string {
  if (error.kind === 'unauthorized') return '连接凭据已失效，请检查设置'
  if (error.kind === 'network-unreachable') return '无法连接订阅服务'
  if (error.kind === 'timeout') return '加载超时，请重试'
  return error.message || '加载订阅文章失败'
}

function FeedItemCard({
  item,
  source,
  active,
  onSelect,
  onToggleStar,
  onToggleLater,
}: {
  item: FeedItem
  source?: FeedSubscription
  active: boolean
  onSelect: () => void
  onToggleStar: () => void
  onToggleLater: () => void
}) {
  const read = itemIsRead(item)
  const starred = itemIsStarred(item)
  const later = itemIsReadLater(item)
  const analysis = itemAnalysisStatus(item)
  const openFromKeyboard = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault()
      onSelect()
    }
  }

  return (
    <article
      className={'card rss-item-card' + (read ? ' read' : '') + (active ? ' active' : '')}
      tabIndex={0}
      aria-current={active ? 'true' : undefined}
      onClick={onSelect}
      onKeyDown={openFromKeyboard}
    >
      <div className="card-top">
        {!read && <span className="unread-dot" aria-label="未读" />}
        <span className="src-badge">
          <Icon name="rss" size={12} />
          {feedItemSourceTitle(item, source)}
        </span>
        <span className="card-spacer" />
        <time className="card-time" dateTime={item.published_at ?? item.created_at}>
          {formatFeedDate(item.published_at ?? item.created_at)}
        </time>
      </div>
      <h3>{item.title || '无标题文章'}</h3>
      {itemPreview(item) && <p>{itemPreview(item)}</p>}
      <div className="rss-card-foot">
        <div className="rss-card-signals">
          {analysis === 'done' && (
            <span title="已有 AI 分析">
              <Icon name="sparkles" size={13} />
            </span>
          )}
          {(analysis === 'pending' || analysis === 'processing') && (
            <span className="spinning" title="AI 分析中">
              <Icon name="loader" size={13} />
            </span>
          )}
          {analysis === 'failed' && (
            <span className="error" title="AI 分析失败">
              <Icon name="alert" size={13} />
            </span>
          )}
        </div>
        <button
          type="button"
          className={later ? 'on' : ''}
          onClick={(event) => {
            event.stopPropagation()
            onToggleLater()
          }}
          aria-label={later ? '移出稍后读' : '加入稍后读'}
          title={later ? '移出稍后读' : '加入稍后读'}
        >
          <Icon name="bookmark" size={14} />
        </button>
        <button
          type="button"
          className={starred ? 'on' : ''}
          onClick={(event) => {
            event.stopPropagation()
            onToggleStar()
          }}
          aria-label={starred ? '取消收藏' : '收藏'}
          title={starred ? '取消收藏' : '收藏'}
        >
          <Icon name={starred ? 'star_fill' : 'star'} size={14} />
        </button>
      </div>
    </article>
  )
}

export function FeedItemsPane({
  title,
  items,
  total,
  subscriptions,
  selectedSubscription,
  activeID,
  query,
  loading,
  loadingMore,
  error,
  hasMore,
  refreshing,
  onQueryChange,
  onSelect,
  onToggleStar,
  onToggleLater,
  onLoadMore,
  onReload,
  onRefresh,
  onMarkAllRead,
  onOpenSettings,
}: FeedItemsPaneProps) {
  const bySubscription = new Map(subscriptions.map((subscription) => [subscription.id, subscription]))
  const sourceError = selectedSubscription ? feedError(selectedSubscription) : null
  const onScroll = useCallback(
    (event: UIEvent<HTMLDivElement>) => {
      if (!hasMore || loading || loadingMore) return
      const element = event.currentTarget
      if (element.scrollHeight - element.scrollTop - element.clientHeight < 140) onLoadMore()
    },
    [hasMore, loading, loadingMore, onLoadMore],
  )

  return (
    <section className="list-pane rss-list-pane" aria-label="订阅文章列表">
      <header className="rss-list-head">
        <div className="rss-list-title-row">
          <div>
            <h1>{title}</h1>
            <span>{total} 条文章</span>
          </div>
          <div className="rss-list-actions">
            <button
              type="button"
              className={refreshing ? 'spinning' : ''}
              onClick={onRefresh}
              disabled={refreshing}
              aria-label={selectedSubscription ? '刷新当前订阅源' : '刷新全部订阅源'}
              title={selectedSubscription ? '刷新当前订阅源' : '刷新全部订阅源'}
            >
              <Icon name={refreshing ? 'loader' : 'refresh'} size={15} />
            </button>
            <button type="button" onClick={onMarkAllRead} aria-label="全部标为已读" title="全部标为已读">
              <Icon name="check" size={15} />
            </button>
          </div>
        </div>
        <label className="rss-search-box">
          <Icon name="search" size={14} />
          <input
            type="search"
            value={query}
            onChange={(event) => onQueryChange(event.target.value)}
            placeholder="搜索订阅文章"
            aria-label="搜索订阅文章"
          />
          {query && (
            <button type="button" onClick={() => onQueryChange('')} aria-label="清除搜索">
              <Icon name="close" size={13} />
            </button>
          )}
        </label>
        {selectedSubscription && (
          <div className={'rss-source-status' + (sourceError ? ' error' : '')}>
            {sourceError ? <Icon name="alert" size={13} /> : <Icon name="clock" size={13} />}
            <span>
              {sourceError
                ? `刷新失败：${sourceError}`
                : selectedSubscription.last_success_at
                  ? `上次成功 ${formatFeedDate(selectedSubscription.last_success_at)}`
                  : '等待首次刷新'}
              {sourceError && selectedSubscription.last_success_at
                ? ` · 上次成功 ${formatFeedDate(selectedSubscription.last_success_at)}`
                : ''}
              {sourceError && selectedSubscription.next_fetch_at
                ? ` · 将于 ${formatFeedDate(selectedSubscription.next_fetch_at)}重试`
                : ''}
            </span>
          </div>
        )}
      </header>

      <div className="list-scroll rss-items-scroll" onScroll={onScroll}>
        {loading && (
          <div className="rss-list-message">
            <span className="spinning"><Icon name="loader" size={18} /></span>
            正在加载订阅文章
          </div>
        )}
        {!loading && error && (
          <div className="rss-list-message error">
            <Icon name="alert" size={20} />
            <span>{errorHint(error)}</span>
            <button
              type="button"
              className="link-btn"
              onClick={error.kind === 'unauthorized' ? onOpenSettings : onReload}
            >
              {error.kind === 'unauthorized' ? '前往设置' : '重试'}
            </button>
          </div>
        )}
        {!loading && !error && items.length === 0 && (
          <div className="rss-list-message">
            <Icon name={query ? 'search' : 'rss'} size={24} />
            <span>{query ? '没有匹配的订阅文章' : '这个视图还没有文章'}</span>
          </div>
        )}
        {!loading &&
          !error &&
          items.map((item) => (
            <FeedItemCard
              key={item.id}
              item={item}
              source={bySubscription.get(item.subscription_id)}
              active={item.id === activeID}
              onSelect={() => onSelect(item)}
              onToggleStar={() => onToggleStar(item)}
              onToggleLater={() => onToggleLater(item)}
            />
          ))}
        {loadingMore && <div className="rss-loading-more">正在加载更多</div>}
        {!loading && !loadingMore && hasMore && (
          <button type="button" className="rss-load-more" onClick={onLoadMore}>
            加载更多
          </button>
        )}
      </div>
    </section>
  )
}
