/**
 * FeedItemCard 迁到 ReaderPreviewCard 之后的调用侧守卫。
 *
 * 迁移删掉了卡片上手写的 Enter/Space keydown 和两个操作按钮里的
 * stopPropagation()：打开由公共组件里的真实 <button> 承载，操作按钮是它的
 * 兄弟节点。这两条行为必须仍然成立，所以在这里直接以 props 渲染整个 pane。
 */
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { FeedItemsPane } from './FeedItemsPane'
import type { FeedItem, FeedSubscription } from '../../lib/api/types'

const subscription: FeedSubscription = {
  id: 'feed-1',
  url: 'https://example.com/feed.xml',
  title: '工程周刊',
  folder_id: null,
  created_at: '2026-06-01T00:00:00Z',
}

function makeItem(over: Partial<FeedItem> = {}): FeedItem {
  return {
    id: 'item-1',
    subscription_id: 'feed-1',
    title: '可靠的订阅实现',
    url: 'https://example.com/posts/rss',
    summary: '订阅文章摘要。',
    published_at: '2026-06-10T10:00:00Z',
    created_at: '2026-06-10T10:00:00Z',
    ...over,
  }
}

function renderPane(over: {
  items?: FeedItem[]
  activeID?: string | null
  onSelect?: (item: FeedItem) => void
  onToggleStar?: (item: FeedItem) => void
  onToggleLater?: (item: FeedItem) => void
} = {}) {
  const items = over.items ?? [makeItem()]
  render(
    <FeedItemsPane
      title="全部文章"
      items={items}
      total={items.length}
      subscriptions={[subscription]}
      activeID={over.activeID ?? null}
      query=""
      loading={false}
      loadingMore={false}
      error={null}
      hasMore={false}
      refreshing={false}
      onQueryChange={() => {}}
      onSelect={over.onSelect ?? (() => {})}
      onToggleStar={over.onToggleStar ?? (() => {})}
      onToggleLater={over.onToggleLater ?? (() => {})}
      onLoadMore={() => {}}
      onReload={() => {}}
      onRefresh={() => {}}
      onMarkAllRead={() => {}}
      onOpenSettings={() => {}}
    />,
  )
}

describe('FeedItemCard 打开行为', () => {
  it('点击、Enter 和 Space 都打开这条订阅文章', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    renderPane({ onSelect })

    const open = screen.getByRole('button', { name: '打开 可靠的订阅实现' })
    await user.click(open)
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: 'item-1' }))

    open.focus()
    await user.keyboard('{Enter}')
    await user.keyboard(' ')
    expect(onSelect).toHaveBeenCalledTimes(3)
  })

  it('无标题文章的打开标签退回到占位标题', () => {
    renderPane({ items: [makeItem({ title: '' })] })

    expect(screen.getByRole('button', { name: '打开 无标题文章' })).toBeInTheDocument()
  })

  it('稍后读与收藏是打开按钮的兄弟节点，点它们不会打开文章', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const onToggleStar = vi.fn()
    const onToggleLater = vi.fn()
    renderPane({ onSelect, onToggleStar, onToggleLater })

    const open = screen.getByRole('button', { name: '打开 可靠的订阅实现' })
    const later = screen.getByRole('button', { name: '加入稍后读' })
    const star = screen.getByRole('button', { name: '收藏' })
    expect(open.contains(later)).toBe(false)
    expect(open.contains(star)).toBe(false)

    await user.click(later)
    await user.click(star)

    expect(onToggleLater).toHaveBeenCalledTimes(1)
    expect(onToggleStar).toHaveBeenCalledTimes(1)
    expect(onSelect).not.toHaveBeenCalled()
  })
})

describe('FeedItemCard 状态映射', () => {
  it('选中态映射到 is-selected 与 aria-current，业务 class 仍在根节点', () => {
    renderPane({ activeID: 'item-1' })

    const card = document.querySelector('.reader-preview-card')
    expect(card).toHaveClass('card', 'rss-item-card', 'is-selected')
    expect(card).toHaveAttribute('aria-current', 'true')
    expect(card).not.toHaveClass('is-read')
  })

  it('已读映射到 is-read 且不再渲染未读点，未读时相反', () => {
    renderPane({ items: [makeItem({ read: true })] })
    expect(document.querySelector('.reader-preview-card')).toHaveClass('is-read')
    expect(document.querySelector('.unread-dot')).toBeNull()
    expect(document.querySelector('.reader-preview-card')).not.toHaveAttribute('aria-current')
  })

  it('未读条目渲染未读点', () => {
    renderPane()
    expect(document.querySelector('.reader-preview-card')).not.toHaveClass('is-read')
    expect(document.querySelector('.unread-dot')).not.toBeNull()
  })
})
