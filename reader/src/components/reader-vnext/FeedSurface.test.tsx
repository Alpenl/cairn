import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { FeedSurface } from './FeedSurface'
import { feedScrollAnchorKey, readFeedScrollAnchor, writeFeedScrollAnchor } from '../../lib/feed-scroll-anchor'
import { err, ok } from '../../lib/api/result'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderFeedAction, ReaderFeedFeedbackResponse, ReaderFeedItemResponse, ReaderFeedResponse, ReaderInboxConfirmAIProposalsResponse, ReaderTodoResponse } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { readerIdentity } from '../../lib/identity'
import { enabledReaderCapabilityLease } from '../../test/capabilities'

function feedItem(overrides: Partial<ReaderFeedItemResponse> = {}): ReaderFeedItemResponse {
  const source = overrides.source ?? 'reading'
  const key = overrides.key ?? 'link:L1'
  const createdAt = overrides.created_at ?? '2026-08-10T01:00:00Z'
  const publishedAt = overrides.published_at ?? null
  const actions: ReaderFeedAction[] = source === 'inbox'
    ? ['confirm', 'discard', 'hide', 'not_interested', 'open']
    : source === 'subscription'
      ? ['read', 'read_later', 'save', 'hide', 'not_interested', 'open']
      : ['read', 'read_later', 'hide', 'not_interested', 'open', 'open_workspace']
  return {
    key,
    resource_key: overrides.resource_key ?? key,
    source: 'reading',
    title: '保存的文章',
    summary: '文章摘要',
    url: 'https://example.com/article',
    link_id: 'L1',
    inbox_id: null,
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    score: 90,
    score_contributions: {
      pending_confirmation: 0,
      saved_library: 70,
      subscription_recent: 0,
      unread: 20,
      read_later: 0,
      chronological_fallback: 0,
    },
    enabled_score_signals: ['saved_library', 'unread', 'read_later', 'chronological_fallback'],
    reason_code: 'saved_library',
    reason_params: { source: 'reading' },
    reason_contribution: 70,
    reason_text: '服务端兼容文本，Reader 不应显示它',
    published_at: publishedAt,
    event_at: overrides.event_at ?? publishedAt ?? createdAt,
    created_at: createdAt,
    actions,
    ...overrides,
  } as ReaderFeedItemResponse
}

// 行结构归公共组件后，稳定的定位点是 Feed 自己挂上去的资源标识，不是布局 class。
function renderedResourceKeys(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll<HTMLElement>('li[data-resource-key]'))
    .map((item) => item.dataset.resourceKey ?? '')
}

function feedResponse(
  items: ReaderFeedItemResponse[],
  overrides: Partial<ReaderFeedResponse> = {},
): ReaderFeedResponse {
  return {
    items,
    next_cursor: undefined,
    snapshot_id: 'snapshot-1',
    mode: 'recommended',
    capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions', 'inbox_batch'],
    ...overrides,
  }
}

function todo(overrides: Partial<ReaderTodoResponse> = {}): ReaderTodoResponse {
  return {
    id: 'todo-1',
    text: '完成阅读整理',
    due_at: null,
    done: false,
    origin_kind: 'standalone',
    origin_host_kind: null,
    origin_host_id: null,
    origin_ref: null,
    host_revision: 1,
    completed_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    expired: false,
    ...overrides,
  }
}

function makeClient(
  responses: ReaderFeedResponse[] = [feedResponse([feedItem()])],
  todos: ReaderTodoResponse[] = [],
  todoResponses: ReaderTodoResponse[][] = [todos],
) {
  let responseIndex = 0
  let todoResponseIndex = 0
  const linkStates = new Map<string, { read: boolean; read_later: boolean }>()
  const feedItemStates = new Map<string, { read: boolean; read_later: boolean }>()
  for (const response of responses) {
    for (const item of response.items) {
      const state = { read: item.read, read_later: item.read_later }
      if (item.link_id) linkStates.set(item.link_id, state)
      if (item.feed_item_id) feedItemStates.set(item.feed_item_id, state)
    }
  }
  const getReaderFeed = vi.fn(async (_params: Parameters<IdentityBoundReaderClient['getReaderFeed']>[0]) => ok(
    responses[Math.min(responseIndex++, responses.length - 1)],
  ))
  const patchEngagement = vi.fn(async (linkID: string, patch: { read?: boolean; read_later?: boolean }) => {
    const previous = linkStates.get(linkID) ?? { read: false, read_later: false }
    const state = {
      read: patch.read ?? previous.read,
      read_later: patch.read_later ?? previous.read_later,
    }
    linkStates.set(linkID, state)
    return ok({
      link_id: linkID,
      ...state,
      progress: 0,
      updated_at: '2026-08-10T01:02:00Z',
    })
  })
  const updateFeedItem = vi.fn(async (id: string, patch: { read?: boolean; read_later?: boolean }) => {
    const previous = feedItemStates.get(id) ?? { read: false, read_later: false }
    const state = {
      read: patch.read ?? previous.read,
      read_later: patch.read_later ?? previous.read_later,
    }
    feedItemStates.set(id, state)
    return ok({
      ...state,
      read_at: state.read ? '2026-08-10T01:02:00Z' : null,
      read_later_at: state.read_later ? '2026-08-10T01:02:00Z' : null,
    })
  })
  const sendReaderFeedFeedback = vi.fn(async (itemKey: string, request: { action: ReaderFeedAction }) => ok({
    item_key: itemKey,
    action: request.action,
    saved: request.action === 'save',
  }))
  const confirmInbox = vi.fn(async () => ok({ target_kind: 'link' as const, link_id: 'confirmed-link', status: 'confirmed' as const }))
  const confirmAIProposals = vi.fn<IdentityBoundReaderClient['confirmAIProposals']>(async () => ok<ReaderInboxConfirmAIProposalsResponse>({
    atomic: true,
    items: [],
    remaining_count: 0,
  }))
  const discardInbox = vi.fn(async () => ok(true))
  const listTodos = vi.fn(async () => ok({
    items: todoResponses[Math.min(todoResponseIndex++, todoResponses.length - 1)] ?? [],
  }))
  const client = {
    getReaderFeed,
    patchEngagement,
    updateFeedItem,
    sendReaderFeedFeedback,
    confirmInbox,
    confirmAIProposals,
    discardInbox,
    listTodos,
    isIdentityCurrent: vi.fn(() => true),
    identityLease: readerIdentity.activeLease,
  } as unknown as IdentityBoundReaderClient
  return { client, getReaderFeed, patchEngagement, updateFeedItem, sendReaderFeedFeedback, confirmInbox, confirmAIProposals, discardInbox, listTodos }
}

function renderFeed(
  client: IdentityBoundReaderClient,
  capabilityOverrides: Parameters<typeof enabledReaderCapabilityLease>[0] = {},
) {
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  const onOpenLink = vi.fn<(id: string) => void>()
  const rendered = render(
    <FeedSurface client={client} capabilityLease={enabledReaderCapabilityLease(capabilityOverrides)} onNavigate={onNavigate} onOpenLink={onOpenLink} />,
  )
  return { ...rendered, onNavigate, onOpenLink }
}

afterEach(() => {
  window.sessionStorage.clear()
  vi.restoreAllMocks()
})

describe('FeedSurface', () => {
  it('does not request Feed data when Feed is unavailable', () => {
    const { client, getReaderFeed } = makeClient()

    renderFeed(client, { feed: false, todos: false })

    expect(getReaderFeed).not.toHaveBeenCalled()
    expect(screen.queryByText('保存的文章')).not.toBeInTheDocument()
  })

  it('renders reason text only from the frozen tuple, not item source or legacy text', async () => {
    const subscriptionWithSavedReason = feedItem({
      key: 'subscription:tuple-one',
      source: 'subscription',
      feed_item_id: 'tuple-one',
      link_id: null,
      title: '同一来源，保存原因',
      reason_code: 'saved_library',
      reason_params: { source: 'reading' },
      reason_contribution: 70,
      reason_text: '来自订阅的旧文案，不应展示',
    })
    const subscriptionWithUnreadReason = feedItem({
      key: 'subscription:tuple-two',
      source: 'subscription',
      feed_item_id: 'tuple-two',
      link_id: null,
      title: '同一来源，未读原因',
      url: 'https://example.com/tuple-two',
      reason_code: 'unread',
      reason_params: { read: false },
      reason_contribution: 20,
      reason_text: '另一条旧文案，不应展示',
    })
    const { client } = makeClient([feedResponse([subscriptionWithSavedReason, subscriptionWithUnreadReason])])
    renderFeed(client)

    const savedCard = await screen.findByText('同一来源，保存原因').then((title) => title.closest('li') as HTMLElement)
    const unreadCard = screen.getByText('同一来源，未读原因').closest('li') as HTMLElement
    expect(within(savedCard).getByText('已保存到资料库')).toBeInTheDocument()
    expect(within(unreadCard).getByText('尚未阅读')).toBeInTheDocument()
    expect(screen.queryByText(/旧文案/)).not.toBeInTheDocument()
  })

  it('sends the selected sources with every snapshot request and starts a new snapshot after filtering', async () => {
    const allSources = feedResponse([feedItem({ title: '全部来源' })], {
      snapshot_id: 'snapshot-all-sources',
      next_cursor: 'cursor-all-sources',
    })
    const filteredSources = feedResponse([feedItem({ title: '仅收件箱和收藏' })], {
      snapshot_id: 'snapshot-filtered-sources',
      next_cursor: 'cursor-filtered-sources',
    })
    const filteredNextPage = feedResponse([feedItem({ key: 'link:L2', title: '筛选后的下一页', url: 'https://example.com/two', link_id: 'L2' })], {
      snapshot_id: 'snapshot-filtered-sources',
    })
    const { client, getReaderFeed } = makeClient([allSources, filteredSources, filteredNextPage])
    renderFeed(client)

    expect(await screen.findByText('全部来源')).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[0][0]).toMatchObject({
      source: ['inbox', 'reading', 'subscription'],
    })

    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    expect(await screen.findByText('仅收件箱和收藏')).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[1][0]).toMatchObject({
      source: ['inbox', 'reading'],
    })
    expect(getReaderFeed.mock.calls[1][0]).not.toHaveProperty('snapshotID')
    expect(getReaderFeed.mock.calls[1][0]).not.toHaveProperty('after')

    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(getReaderFeed.mock.calls[2][0]).toMatchObject({
      source: ['inbox', 'reading'],
      snapshotID: 'snapshot-filtered-sources',
      after: 'cursor-filtered-sources',
    }))
  })

  it('keeps server item order flat when section metadata uses a different order', async () => {
    const pending = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '分段收件箱',
      url: 'https://example.com/pending',
      link_id: null,
      inbox_id: 'I1',
      actions: ['confirm', 'discard', 'open'],
    })
    const reading = feedItem({
      key: 'link:L1',
      source: 'reading',
      title: '分段收藏',
      url: 'https://example.com/reading',
      actions: ['read', 'open'],
    })
    const response = feedResponse([reading, pending], {
      capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions', 'inbox_batch'],
      sections: [
        { id: 'inbox', source: 'inbox', label: '收件箱', count: 1, capabilities: ['confirm', 'discard', 'open'] },
        { id: 'reading', source: 'reading', label: '收藏', count: 1, capabilities: ['read', 'open'] },
      ],
      sources: [
        { id: 'inbox', label: '收件箱', enabled: true, count: 1, capabilities: ['confirm', 'discard', 'open'] },
        { id: 'reading', label: '收藏', enabled: true, count: 1, capabilities: ['read', 'open'] },
      ],
    })
    const fixture = makeClient([response])
    const { container } = renderFeed(fixture.client)

    expect(await screen.findByText('分段收件箱')).toBeInTheDocument()
    expect(renderedResourceKeys(container)).toEqual(['link:L1', 'inbox:I1'])
    expect(screen.getByText('收件箱 · 1 条')).toBeInTheDocument()
    expect(screen.getByText('收藏 · 1 条')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '收件箱' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '收藏' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '确认全部 AI 建议' })).toBeInTheDocument()
    expect(screen.getByText(/服务端按稳定批次确认/)).toBeInTheDocument()
    expect(screen.getByText('分段收藏')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))
    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledWith({ partition: 'active' }))
    expect(fixture.confirmInbox).not.toHaveBeenCalled()
  })

  it('preserves server order across recommended pages, duplicate resources, filters, and chronological mode', async () => {
    const item = (resourceKey: string, source: 'reading' | 'inbox' | 'subscription', title: string) => feedItem({
      key: `${source}:${resourceKey}`,
      resource_key: resourceKey,
      source,
      title,
      url: `https://example.com/${resourceKey}`,
      link_id: source === 'reading' ? resourceKey : null,
      inbox_id: source === 'inbox' ? resourceKey : null,
      feed_item_id: source === 'subscription' ? resourceKey : null,
    })
    const a1 = item('resource:a1', 'reading', 'A1')
    const b1 = item('resource:b1', 'inbox', 'B1')
    const a2 = item('resource:a2', 'reading', 'A2')
    const c1 = item('resource:c1', 'subscription', 'C1')
    const d1 = item('resource:d1', 'inbox', 'D1')
    const first = feedResponse([a1, b1, a2, c1], {
      snapshot_id: 'snapshot-recommended',
      next_cursor: 'cursor-recommended',
      sections: [
        { id: 'subscription', source: 'subscription', label: '订阅', count: 1, capabilities: [] },
        { id: 'inbox', source: 'inbox', label: '收件箱', count: 1, capabilities: [] },
        { id: 'reading', source: 'reading', label: '收藏', count: 2, capabilities: [] },
      ],
    })
    const second = feedResponse([
      { ...b1, title: 'B1 duplicate from page two' },
      d1,
    ], { snapshot_id: 'snapshot-recommended' })
    const filtered = feedResponse([a1, b1, a2, d1], {
      snapshot_id: 'snapshot-filtered',
    })
    const chronological = feedResponse([a2, b1, a1, d1], {
      snapshot_id: 'snapshot-chronological-filtered',
      mode: 'chronological',
    })
    const fixture = makeClient([first, second, filtered, chronological])
    const { container } = renderFeed(fixture.client)

    await screen.findByText('C1')
    expect(renderedResourceKeys(container)).toEqual([
      'resource:a1', 'resource:b1', 'resource:a2', 'resource:c1',
    ])

    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await screen.findByText('D1')
    expect(renderedResourceKeys(container)).toEqual([
      'resource:a1', 'resource:b1', 'resource:a2', 'resource:c1', 'resource:d1',
    ])
    expect(screen.queryByText('B1 duplicate from page two')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(3))
    expect(renderedResourceKeys(container)).toEqual([
      'resource:a1', 'resource:b1', 'resource:a2', 'resource:d1',
    ])
    expect(fixture.getReaderFeed.mock.calls[2][0]).not.toHaveProperty('snapshotID')
    expect(fixture.getReaderFeed.mock.calls[2][0]).not.toHaveProperty('after')

    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(4))
    expect(renderedResourceKeys(container)).toEqual([
      'resource:a2', 'resource:b1', 'resource:a1', 'resource:d1',
    ])
    expect(fixture.getReaderFeed.mock.calls[3][0]).toMatchObject({
      mode: 'chronological',
      source: ['inbox', 'reading'],
    })
    expect(fixture.getReaderFeed.mock.calls[3][0]).not.toHaveProperty('snapshotID')
    expect(fixture.getReaderFeed.mock.calls[3][0]).not.toHaveProperty('after')
  })

  it('renders the authoritative event_at instant instead of recomputing card time', async () => {
    const eventAt = '2026-07-01T08:30:00.123Z'
    const response = feedResponse([feedItem({
      title: '权威可见时间',
      published_at: '2026-08-01T00:00:00Z',
      created_at: '2026-08-02T00:00:00Z',
      event_at: eventAt,
    })], { mode: 'chronological' })
    renderFeed(makeClient([response]).client)

    const card = await screen.findByText('权威可见时间').then((title) => title.closest('li') as HTMLElement)
    expect(card.querySelector('time')).toHaveAttribute('datetime', eventAt)
  })

  it('does not reorder a snapshot when read state, actions, or section metadata change', async () => {
    const first = feedResponse([
      feedItem({ key: 'link:one', resource_key: 'resource:one', title: 'One', link_id: 'one' }),
      feedItem({ key: 'link:two', resource_key: 'resource:two', title: 'Two', link_id: 'two' }),
      feedItem({ key: 'link:three', resource_key: 'resource:three', title: 'Three', link_id: 'three' }),
    ], {
      snapshot_id: 'snapshot-invariant',
      sections: [
        { id: 'reading', source: 'reading', label: '收藏', count: 3, capabilities: ['read'] },
      ],
    })
    const refreshed = feedResponse([
      { ...first.items[0], actions: [] },
      { ...first.items[1], actions: ['read', 'read_later'] },
      { ...first.items[2], actions: ['open_workspace'] },
    ], {
      snapshot_id: 'snapshot-invariant',
      sections: [
        { id: 'reading-new', source: 'reading', label: '重新命名', count: 3, capabilities: [] },
      ],
    })
    const fixture = makeClient([first, refreshed])
    const { container } = renderFeed(fixture.client)
    const wantOrder = ['resource:one', 'resource:two', 'resource:three']

    const secondCard = await screen.findByText('Two').then((title) => title.closest('li') as HTMLElement)
    expect(renderedResourceKeys(container)).toEqual(wantOrder)
    fireEvent.click(within(secondCard).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(within(secondCard).getByRole('button', { name: '已读' })).toBeInTheDocument())
    expect(renderedResourceKeys(container)).toEqual(wantOrder)
    fireEvent.click(within(secondCard).getByRole('button', { name: '稍后读' }))
    await waitFor(() => expect(within(secondCard).getByRole('button', { name: '稍后' })).toBeInTheDocument())
    expect(renderedResourceKeys(container)).toEqual(wantOrder)

    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(screen.getByText('重新命名 · 3 条')).toBeInTheDocument())
    expect(renderedResourceKeys(container)).toEqual(wantOrder)
    expect(within(screen.getByText('One').closest('li') as HTMLElement).queryByRole('button', { name: '未读' })).not.toBeInTheDocument()
  })

  it('renders only item-authorized actions and sends feedback to the canonical action key', async () => {
    const restricted = feedItem({
      key: 'subscription:R1',
      source: 'subscription',
      title: '能力受限订阅',
      url: 'https://example.com/restricted-rss',
      link_id: null,
      feed_item_id: 'R1',
      action_key: 'subscription:canonical-R1',
      actions: ['save', 'open'],
    })
    const disabled = feedItem({
      key: 'link:L2',
      title: '动作关闭文章',
      link_id: 'L2',
      actions: [],
    })
    const { client, sendReaderFeedFeedback, patchEngagement } = makeClient([feedResponse([restricted, disabled])])
    renderFeed(client)

    const restrictedCard = await screen.findByText('能力受限订阅').then((title) => title.closest('li') as HTMLElement)
    const disabledCard = screen.getByText('动作关闭文章').closest('li') as HTMLElement
    expect(within(restrictedCard).getByRole('button', { name: '保存' })).toBeInTheDocument()
    expect(within(restrictedCard).getByRole('button', { name: '打开原文' })).toBeInTheDocument()
    expect(within(restrictedCard).queryByRole('button', { name: '未读' })).not.toBeInTheDocument()
    expect(within(restrictedCard).queryByRole('button', { name: '稍后读' })).not.toBeInTheDocument()
    expect(within(restrictedCard).queryByRole('button', { name: '隐藏' })).not.toBeInTheDocument()
    expect(within(disabledCard).queryByRole('button', { name: '保存' })).not.toBeInTheDocument()
    expect(within(disabledCard).queryByRole('button', { name: '打开原文' })).not.toBeInTheDocument()

    fireEvent.click(within(restrictedCard).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(sendReaderFeedFeedback).toHaveBeenCalledWith('subscription:canonical-R1', { action: 'save' }))
    expect(patchEngagement).not.toHaveBeenCalled()
  })

  it('refreshes the TODO rail when Home publishes a TODO change', async () => {
    const first = todo({ id: 'todo-old', text: 'Home 完成前' })
    const second = todo({ id: 'todo-new', text: 'Feed 及时刷新' })
    const fixture = makeClient([feedResponse([feedItem()])], [first], [[first], [second]])
    renderFeed(fixture.client)

    expect(await screen.findByText('Home 完成前')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    expect(await screen.findByText('Feed 及时刷新')).toBeInTheDocument()
    expect(screen.queryByText('Home 完成前')).not.toBeInTheDocument()
    expect(fixture.listTodos).toHaveBeenCalledTimes(2)
  })

  it('deduplicates by resource_key and keeps the first server occurrence', async () => {
    const link = feedItem({ key: 'link:L1', title: '阅读库版本', link_id: 'L1' })
    const rss = feedItem({
      key: 'subscription:R1',
      resource_key: 'link:L1',
      source: 'subscription',
      title: 'RSS 重复版本',
      link_id: null,
      feed_item_id: 'R1',
      reason_code: 'subscription_recent',
      reason_text: '订阅更新',
    })
    const inbox = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '收件箱版本',
      url: 'https://example.com/inbox',
      link_id: null,
      inbox_id: 'I1',
      reason_code: 'pending_confirmation',
      reason_params: { source: 'inbox' },
      reason_text: '收件箱采集',
    })
    const { client } = makeClient([feedResponse([rss, inbox, link])])

    renderFeed(client)

    expect(await screen.findByText('RSS 重复版本')).toBeInTheDocument()
    expect(screen.queryByText('阅读库版本')).not.toBeInTheDocument()
    expect(screen.getByText('收件箱版本')).toBeInTheDocument()
  })

  it('switches between recommended and chronological and keeps snapshot cursor state on reload', async () => {
    const first = feedResponse([feedItem({ title: '第一页' })], {
      snapshot_id: 'snapshot-resume',
      next_cursor: 'cursor-1',
    })
    const second = feedResponse([feedItem({ key: 'link:L2', title: '第二页', url: 'https://example.com/two', link_id: 'L2' })], {
      snapshot_id: 'snapshot-resume',
      next_cursor: undefined,
    })
    const chronological = feedResponse([feedItem({ title: '时间排序' })], {
      snapshot_id: 'snapshot-chronological',
      mode: 'chronological',
    })
    const { client, getReaderFeed } = makeClient([first, second, first, second, chronological])
    const rendered = renderFeed(client)

    expect(await screen.findByText('第一页')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('第二页')).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[1][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'snapshot-resume',
      after: 'cursor-1',
    })
    const firstPageCard = screen.getByText('第一页').closest('li') as HTMLElement
    fireEvent.click(within(firstPageCard).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(within(firstPageCard).getByRole('button', { name: '已读' })).toBeInTheDocument())

    rendered.unmount()
    const resumed = renderFeed(client)
    expect(await screen.findByText('第一页')).toBeInTheDocument()
    expect(screen.getByText('第二页')).toBeInTheDocument()
    const resumedFirstPageCard = screen.getByText('第一页').closest('li') as HTMLElement
    expect(within(resumedFirstPageCard).getByRole('button', { name: '已读' })).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[2][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'snapshot-resume',
    })
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(getReaderFeed.mock.calls[3][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'snapshot-resume',
      after: 'cursor-1',
    }))
    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('时间排序')).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[4][0]).toMatchObject({ mode: 'chronological' })
    resumed.unmount()
  })

  it('sends the normalized source filter for fresh pages and resumes the same filtered snapshot', async () => {
    const first = feedResponse([feedItem({ title: '全部来源第一页' })], {
      snapshot_id: 'snapshot-all-sources',
      next_cursor: 'cursor-all-sources',
    })
    const filtered = feedResponse([feedItem({ title: '收件箱和收藏' })], {
      snapshot_id: 'snapshot-filtered-sources',
      next_cursor: 'cursor-filtered-sources',
    })
    const filteredPage = feedResponse([feedItem({ key: 'link:L2', title: '筛选后的下一页', url: 'https://example.com/next', link_id: 'L2' })], {
      snapshot_id: 'snapshot-filtered-sources',
    })
    const fixture = makeClient([first, filtered, filtered, filteredPage])
    const rendered = renderFeed(fixture.client)

    expect(await screen.findByText('全部来源第一页')).toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[0][0]).toMatchObject({
      source: ['inbox', 'reading', 'subscription'],
    })

    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    await waitFor(() => expect(fixture.getReaderFeed.mock.calls[1][0]).toMatchObject({
      source: ['inbox', 'reading'],
    }))
    expect(fixture.getReaderFeed.mock.calls[1][0]).not.toHaveProperty('snapshotID')
    expect(fixture.getReaderFeed.mock.calls[1][0]).not.toHaveProperty('after')

    rendered.unmount()
    const resumed = renderFeed(fixture.client)
    expect(await screen.findByText('收件箱和收藏')).toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[2][0]).toMatchObject({
      source: ['inbox', 'reading'],
      snapshotID: 'snapshot-filtered-sources',
    })
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(fixture.getReaderFeed.mock.calls[3][0]).toMatchObject({
      source: ['inbox', 'reading'],
      snapshotID: 'snapshot-filtered-sources',
      after: 'cursor-filtered-sources',
    }))
    resumed.unmount()
  })

  it('restores the current snapshot cursor and keeps an already-read card after refresh', async () => {
    const first = feedResponse([feedItem({ title: '刷新前', read: false })], {
      snapshot_id: 'snapshot-refresh',
      next_cursor: 'cursor-before-refresh',
    })
    const refreshed = feedResponse([feedItem({ title: '刷新后', read: false })], {
      snapshot_id: 'snapshot-refresh',
      next_cursor: 'cursor-after-refresh',
    })
    const { client, getReaderFeed } = makeClient([first, refreshed])
    renderFeed(client)

    const card = await screen.findByText('刷新前').then((title) => title.closest('li') as HTMLElement)
    fireEvent.click(within(card).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(within(card).getByRole('button', { name: '已读' })).toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(screen.getByText('刷新后')).toBeInTheDocument())
    const refreshedCard = screen.getByText('刷新后').closest('li') as HTMLElement
    expect(within(refreshedCard).getByRole('button', { name: '已读' })).toBeInTheDocument()
    expect(getReaderFeed.mock.calls[1][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'snapshot-refresh',
    })
    const refreshedCall = getReaderFeed.mock.calls[1]?.[0]
    expect(refreshedCall).toBeDefined()
    expect(refreshedCall?.after).toBeUndefined()

    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(getReaderFeed.mock.calls[2][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'snapshot-refresh',
      after: 'cursor-before-refresh',
    }))
  })

  it('updates reading-link read/read-later state in the current snapshot without reloading', async () => {
    const { client, getReaderFeed, patchEngagement, sendReaderFeedFeedback } = makeClient()
    renderFeed(client)
    const card = await screen.findByRole('listitem')

    fireEvent.click(within(card).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(patchEngagement).toHaveBeenCalledWith('L1', { read: true }))
    expect(within(card).getByRole('button', { name: '已读' })).toBeInTheDocument()

    fireEvent.click(within(card).getByRole('button', { name: '稍后读' }))
    await waitFor(() => expect(patchEngagement).toHaveBeenCalledWith('L1', { read_later: true }))
    expect(within(card).getByRole('button', { name: '稍后' })).toBeInTheDocument()

    fireEvent.click(within(card).getByRole('button', { name: '稍后' }))
    await waitFor(() => expect(patchEngagement).toHaveBeenCalledWith('L1', { read_later: false }))
    expect(within(card).getByRole('button', { name: '稍后读' })).toBeInTheDocument()

    expect(sendReaderFeedFeedback).not.toHaveBeenCalled()
    expect(getReaderFeed).toHaveBeenCalledTimes(1)
  })

  it('starts a fresh snapshot when a persisted snapshot has expired', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is required')
    const namespace = lease.context.physicalNamespace
    window.sessionStorage.setItem(
      `webtag:reader:mixed-feed:v1:${encodeURIComponent(namespace)}:recommended`,
      JSON.stringify({
        version: 1,
        snapshot_id: 'expired-snapshot',
        next_cursor: 'stale-cursor',
        items: [feedItem({ title: '旧位置' })],
      }),
    )
    const fresh = feedResponse([feedItem({ title: '新快照' })], {
      snapshot_id: 'fresh-snapshot',
      next_cursor: 'fresh-cursor',
    })
    let requestIndex = 0
    const getReaderFeed = vi.fn(async (_params: Parameters<IdentityBoundReaderClient['getReaderFeed']>[0]) => {
      requestIndex += 1
      return requestIndex === 1
        ? err({ kind: 'other', message: 'snapshot expired', status: 410 })
        : ok(fresh)
    })
    const client = {
      getReaderFeed,
      isIdentityCurrent: vi.fn(() => true),
      identityLease: lease,
    } as unknown as IdentityBoundReaderClient

    renderFeed(client)

    expect(await screen.findByText('新快照')).toBeInTheDocument()
    expect(screen.queryByText('旧位置')).not.toBeInTheDocument()
    expect(getReaderFeed.mock.calls[0][0]).toMatchObject({
      mode: 'recommended',
      snapshotID: 'expired-snapshot',
    })
    expect(getReaderFeed.mock.calls[1][0]).toMatchObject({ mode: 'recommended', limit: 30 })
    expect(getReaderFeed.mock.calls[1][0]).not.toHaveProperty('snapshotID')
    expect(getReaderFeed.mock.calls[1][0]).not.toHaveProperty('after')
  })

  it('rejects a malformed array position record instead of reading fields off it', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is required')
    const namespace = lease.context.physicalNamespace
    // 数组不是「键值对象」：过去第一层守卫放行它，再靠逐字段读取碰运气兜底。
    window.sessionStorage.setItem(
      `webtag:reader:mixed-feed:v1:${encodeURIComponent(namespace)}:recommended:inbox,reading,subscription`,
      JSON.stringify([{
        version: 2,
        snapshot_id: 'array-snapshot',
        next_cursor: 'array-cursor',
        source_filter: ['inbox', 'reading', 'subscription'],
        items: [feedItem({ title: '数组里的旧位置' })],
      }]),
    )
    const fixture = makeClient([feedResponse([feedItem({ title: '全新快照' })], { snapshot_id: 'fresh-snapshot' })])

    renderFeed(fixture.client)

    expect(await screen.findByText('全新快照')).toBeInTheDocument()
    expect(screen.queryByText('数组里的旧位置')).not.toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[0][0]).not.toHaveProperty('snapshotID')
    expect(fixture.getReaderFeed.mock.calls[0][0]).not.toHaveProperty('after')
  })

  it('routes subscription save/unsave through feedback and keeps RSS items separate from link engagement', async () => {
    const subscription = feedItem({
      key: 'subscription:R1',
      source: 'subscription',
      title: 'RSS 文章',
      link_id: null,
      feed_item_id: 'R1',
      reason_code: 'subscription_recent',
      reason_text: '订阅更新',
    })
    const { client, patchEngagement, updateFeedItem, sendReaderFeedFeedback } = makeClient([feedResponse([subscription])])
    renderFeed(client)
    const card = await screen.findByRole('listitem')

    expect(within(card).getByRole('button', { name: '未读' })).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: '稍后读' })).toBeInTheDocument()
    fireEvent.click(within(card).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(updateFeedItem).toHaveBeenCalledWith('R1', { read: true }))
    fireEvent.click(within(card).getByRole('button', { name: '稍后读' }))
    await waitFor(() => expect(updateFeedItem).toHaveBeenCalledWith('R1', { read_later: true }))
    fireEvent.click(within(card).getByRole('button', { name: '稍后' }))
    await waitFor(() => expect(updateFeedItem).toHaveBeenCalledWith('R1', { read_later: false }))
    fireEvent.click(within(card).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(sendReaderFeedFeedback).toHaveBeenCalledWith('subscription:R1', { action: 'save' }))
    expect(within(card).getByRole('button', { name: '取消保存' })).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: '稍后读' })).toBeInTheDocument()
    fireEvent.click(within(card).getByRole('button', { name: '取消保存' }))
    await waitFor(() => expect(sendReaderFeedFeedback).toHaveBeenCalledWith('subscription:R1', { action: 'unsave' }))
    expect(within(card).getByRole('button', { name: '保存' })).toBeInTheDocument()
    expect(patchEngagement).not.toHaveBeenCalled()
  })

  it('rolls an optimistic subscription save back when feedback fails', async () => {
    const subscription = feedItem({
      key: 'subscription:rollback',
      source: 'subscription',
      link_id: null,
      feed_item_id: 'rollback',
      action_key: 'subscription:rollback',
      actions: ['save', 'open'],
    })
    const fixture = makeClient([feedResponse([subscription])])
    let resolve!: (value: ReturnType<typeof err<ReaderFeedFeedbackResponse>>) => void
    const pending = new Promise<ReturnType<typeof err<ReaderFeedFeedbackResponse>>>((done) => { resolve = done })
    fixture.sendReaderFeedFeedback.mockReturnValueOnce(pending)
    renderFeed(fixture.client)
    const card = await screen.findByRole('listitem')

    fireEvent.click(within(card).getByRole('button', { name: '保存' }))
    expect(within(card).getByRole('button', { name: '取消保存' })).toBeInTheDocument()
    resolve(err({ kind: 'other', message: '保存失败', status: 503 }))

    await waitFor(() => expect(within(card).getByRole('button', { name: '保存' })).toBeInTheDocument())
    expect(screen.getByRole('alert')).toHaveTextContent('保存失败')
  })

  it('confirms or discards pending items and opens the confirmed link while retaining the snapshot cursor', async () => {
    const inbox = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '收件箱条目',
      url: 'https://example.com/inbox',
      link_id: null,
      inbox_id: 'I1',
      reason_code: 'pending_confirmation',
      reason_text: '收件箱采集',
    })
    const discard = feedItem({
      key: 'inbox:I2',
      source: 'inbox',
      title: '待丢弃条目',
      url: 'https://example.com/discard',
      link_id: null,
      inbox_id: 'I2',
      reason_code: 'pending_confirmation',
      reason_text: '收件箱采集',
    })
    const fixture = makeClient([
      feedResponse([inbox, discard], { snapshot_id: 'snapshot-inbox', next_cursor: 'cursor-inbox' }),
    ])
    const rendered = renderFeed(fixture.client)
    const { confirmInbox, discardInbox } = fixture
    const inboxCard = await screen.findByText('收件箱条目').then((title) => title.closest('li') as HTMLElement)
    const discardCard = screen.getByText('待丢弃条目').closest('li') as HTMLElement

    fireEvent.click(within(inboxCard).getByRole('button', { name: '确认' }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('I1'))
    expect(rendered.onOpenLink).toHaveBeenCalledWith('confirmed-link')
    expect(screen.queryByText('收件箱条目')).not.toBeInTheDocument()

    fireEvent.click(within(discardCard).getByRole('button', { name: '丢弃' }))
    await waitFor(() => expect(discardInbox).toHaveBeenCalledWith('I2'))
    expect(screen.queryByText('待丢弃条目')).not.toBeInTheDocument()
    expect(screen.getByText('Feed 暂时为空')).toBeInTheDocument()
  })

  it('confirms the whole active AI backlog through server-owned batches', async () => {
    const pendingOne = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '批量收件箱一',
      url: 'https://example.com/batch-one',
      link_id: null,
      inbox_id: 'I1',
      reason_code: 'pending_confirmation',
      reason_text: '收件箱采集',
    })
    const pendingTwo = feedItem({
      key: 'inbox:I2',
      source: 'inbox',
      title: '批量收件箱二',
      url: 'https://example.com/batch-two',
      link_id: null,
      inbox_id: 'I2',
      reason_code: 'pending_confirmation',
      reason_text: '收件箱采集',
    })
    const reading = feedItem({ title: '保留的阅读条目' })
    const fixture = makeClient([
      feedResponse([pendingOne, pendingTwo, reading]),
      feedResponse([reading]),
    ])
    fixture.confirmAIProposals
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'I1', status: 'confirmed' as const, link_id: 'confirmed-link-1' }],
        remaining_count: 1,
      }))
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'I2', status: 'confirmed' as const, link_id: 'confirmed-link-2' }],
        remaining_count: 0,
      }))
    const rendered = renderFeed(fixture.client)

    await screen.findByText('批量收件箱一')
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(2))
    expect(fixture.confirmAIProposals).toHaveBeenNthCalledWith(1, { partition: 'active' })
    expect(fixture.confirmAIProposals).toHaveBeenNthCalledWith(2, { partition: 'active' })
    expect(fixture.confirmInbox).not.toHaveBeenCalled()
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2))
    expect(screen.queryByText('批量收件箱一')).not.toBeInTheDocument()
    expect(screen.queryByText('批量收件箱二')).not.toBeInTheDocument()
    expect(screen.getByText('保留的阅读条目')).toBeInTheDocument()
    expect(rendered.onOpenLink).not.toHaveBeenCalled()
  })

  it('confirms a paginated AI backlog without loading or selecting Inbox cards', async () => {
    const reading = feedItem({ title: '第一页的阅读条目' })
    const fixture = makeClient([
      feedResponse([reading], {
        next_cursor: 'inbox-backlog-is-later',
        sections: [
          { id: 'inbox', source: 'inbox', label: '收件箱', count: 0, capabilities: ['confirm', 'discard', 'open'] },
          { id: 'reading', source: 'reading', label: '收藏', count: 1, capabilities: ['read', 'open_workspace'] },
        ],
      }),
      feedResponse([reading]),
    ])
    fixture.confirmAIProposals
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'hidden-inbox-one', status: 'confirmed' as const, link_id: 'confirmed-hidden-one' }],
        remaining_count: 1,
      }))
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'hidden-inbox-two', status: 'confirmed' as const, link_id: 'confirmed-hidden-two' }],
        remaining_count: 0,
      }))
    renderFeed(fixture.client)

    await screen.findByText('第一页的阅读条目')
    expect(screen.getByRole('region', { name: 'AI 待确认批量操作' })).toBeInTheDocument()
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2))
    expect(fixture.getReaderFeed.mock.calls.map(([request]) => request?.after)).not.toContain('inbox-backlog-is-later')
  })

  it('does not refresh a newer mode or source-filter Feed view after an AI batch finishes', async () => {
    const pending = feedItem({
      key: 'inbox:batch-view-change',
      source: 'inbox',
      title: '批处理开始前的待确认',
      link_id: null,
      inbox_id: 'batch-view-change',
      actions: ['confirm'],
    })
    const chronological = feedItem({ title: '新的时间 Feed' })
    const filtered = feedItem({ title: '新的筛选时间 Feed' })
    const stale = feedItem({ title: '不应覆盖的旧 Feed' })
    const fixture = makeClient([
      feedResponse([pending], { snapshot_id: 'batch-start' }),
      feedResponse([chronological], { snapshot_id: 'batch-chronological', mode: 'chronological' }),
      feedResponse([filtered], { snapshot_id: 'batch-filtered', mode: 'chronological' }),
      feedResponse([stale], { snapshot_id: 'stale-batch-refresh' }),
    ])
    let resolveBatch!: (value: ReturnType<typeof ok<ReaderInboxConfirmAIProposalsResponse>>) => void
    const pendingBatch = new Promise<ReturnType<typeof ok<ReaderInboxConfirmAIProposalsResponse>>>((resolve) => {
      resolveBatch = resolve
    })
    fixture.confirmAIProposals.mockImplementation(() => pendingBatch)
    renderFeed(fixture.client)

    await screen.findByText('批处理开始前的待确认')
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))
    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(1))

    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('新的时间 Feed')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    expect(await screen.findByText('新的筛选时间 Feed')).toBeInTheDocument()
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(3)

    await act(async () => {
      resolveBatch(ok({
        atomic: true as const,
        items: [{ inbox_id: 'batch-view-change', status: 'confirmed' as const, link_id: 'confirmed-batch-view-change' }],
        remaining_count: 0,
      }))
    })

    await waitFor(() => expect(screen.getByRole('button', { name: '确认全部 AI 建议' })).not.toBeDisabled())
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(3)
    expect(fixture.getReaderFeed.mock.calls[2][0]).toMatchObject({
      mode: 'chronological',
      source: ['inbox', 'reading'],
    })
    expect(screen.getByText('新的筛选时间 Feed')).toBeInTheDocument()
    expect(screen.queryByText('不应覆盖的旧 Feed')).not.toBeInTheDocument()
  })

  it('keeps the no-progress AI batch error after refreshing the authoritative Feed', async () => {
    const pending = feedItem({
      key: 'inbox:no-progress',
      source: 'inbox',
      title: '无法前进的待确认',
      link_id: null,
      inbox_id: 'no-progress',
      actions: ['confirm'],
    })
    const refreshed = feedItem({ title: '刷新后的待确认 Feed' })
    const fixture = makeClient([feedResponse([pending]), feedResponse([refreshed])])
    fixture.confirmAIProposals.mockResolvedValue(ok({
      atomic: true as const,
      items: [],
      remaining_count: 1,
    }))
    renderFeed(fixture.client)

    await screen.findByText('无法前进的待确认')
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2))
    expect(fixture.confirmAIProposals).toHaveBeenCalledWith({ partition: 'active' })
    expect(screen.getByText('刷新后的待确认 Feed')).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('AI 确认队列未前进，请刷新后重试。')
  })

  it('stops after a later AI batch failure and refreshes the authoritative Feed snapshot', async () => {
    const pendingOne = feedItem({
      key: 'inbox:partial-one',
      source: 'inbox',
      title: '原子确认一',
      link_id: null,
      inbox_id: 'partial-one',
      actions: ['confirm'],
    })
    const pendingTwo = feedItem({
      key: 'inbox:partial-two',
      source: 'inbox',
      title: '原子确认二',
      link_id: null,
      inbox_id: 'partial-two',
      actions: ['confirm'],
    })
    const fixture = makeClient([
      feedResponse([pendingOne, pendingTwo]),
      feedResponse([pendingTwo]),
    ])
    fixture.confirmAIProposals
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'partial-one', status: 'confirmed' as const, link_id: 'confirmed-link' }],
        remaining_count: 1,
      }))
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '第二批失败', status: 409 }))
    renderFeed(fixture.client)

    await screen.findByText('原子确认一')
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2))
    expect(screen.queryByText('原子确认一')).not.toBeInTheDocument()
    expect(screen.getByText('原子确认二')).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('内容已经被其他窗口更新，请刷新后重试。')
  })

  it('does not infer Feed actions from resource IDs when item capabilities are empty', async () => {
    const item = feedItem({
      title: '能力关闭的链接',
      actions: [],
    })
    const fixture = makeClient([feedResponse([item])])
    renderFeed(fixture.client)

    const card = await screen.findByText('能力关闭的链接').then((title) => title.closest('li') as HTMLElement)
    expect(within(card).queryByRole('button', { name: '未读' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: '稍后读' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: '保存' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: '隐藏' })).not.toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: '打开' })).not.toBeInTheDocument()
    expect(within(card).getByText('能力关闭的链接').tagName).toBe('STRONG')
    expect(fixture.patchEngagement).not.toHaveBeenCalled()
    expect(fixture.sendReaderFeedFeedback).not.toHaveBeenCalled()
  })

  it('does not retain stale envelope capabilities after a legacy response', async () => {
    const pending = feedItem({
      key: 'inbox:capability-reset',
      source: 'inbox',
      title: '能力重置收件箱',
      link_id: null,
      inbox_id: 'capability-reset',
      actions: ['confirm'],
    })
    const first = feedResponse([pending])
    const legacy = feedResponse([pending], {
      capabilities: undefined,
      sections: undefined,
      sources: undefined,
    })
    const fixture = makeClient([first, legacy])
    renderFeed(fixture.client)

    await screen.findByText('能力重置收件箱')
    expect(screen.getByRole('switch', { name: '订阅' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '确认全部 AI 建议' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '刷新' }))

    await waitFor(() => expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2))
    expect(screen.queryByRole('switch', { name: '订阅' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '确认全部 AI 建议' })).not.toBeInTheDocument()
  })

  it('keeps an item visible when the server sends an unknown section id', async () => {
    const item = feedItem({ title: '未知分段仍可见', section_id: 'server-new-section' })
    const response = feedResponse([item], {
      sections: [{ id: 'reading', source: 'reading', label: '收藏', count: 1, capabilities: ['open_workspace'] }],
    })

    renderFeed(makeClient([response]).client)

    expect(await screen.findByText('未知分段仍可见')).toBeInTheDocument()
  })

  it('does not render batch confirmation without the server inbox-batch capability', async () => {
    const item = feedItem({
      title: '伪装收件箱的阅读条目',
      source: 'reading',
      link_id: null,
      inbox_id: 'not-an-inbox-segment',
      actions: ['confirm'],
    })
    const response = feedResponse([item], {
      capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions'],
      sections: [{ id: 'reading', source: 'reading', label: '收藏', count: 1, capabilities: ['confirm'] }],
    })

    renderFeed(makeClient([response]).client)

    await screen.findByText('伪装收件箱的阅读条目')
    expect(screen.queryByRole('button', { name: '确认全部 AI 建议' })).not.toBeInTheDocument()
  })

  it('renders server section/source metadata and scopes batch confirmation to loaded filtered items', async () => {
    const item = feedItem({ section_id: 'reading' })
    const response = feedResponse([item], {
      sections: [{ id: 'reading', source: 'reading', label: '继续阅读', count: 7, capabilities: ['open_workspace'] }],
      sources: [{ id: 'reading', label: '阅读库', enabled: true, count: 7, capabilities: ['open_workspace'] }],
    })
    renderFeed(makeClient([response]).client)

    expect(await screen.findByText('继续阅读 · 7 条')).toBeInTheDocument()
    expect(screen.getByText('阅读库')).toBeInTheDocument()
    expect(screen.getByText('7 条 · 有可用动作')).toBeInTheDocument()
  })

  it('filters sources from the right rail and exposes each card recommendation reason', async () => {
    const pending = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '筛选收件箱',
      url: 'https://example.com/filter-pending',
      link_id: null,
      inbox_id: 'I1',
      reason_code: 'pending_confirmation',
      reason_params: { source: 'inbox' },
      reason_text: '收件箱采集',
    })
    const subscription = feedItem({
      key: 'subscription:R1',
      source: 'subscription',
      title: '筛选订阅',
      url: 'https://example.com/filter-subscription',
      link_id: null,
      feed_item_id: 'R1',
      reason_code: 'subscription_recent',
      reason_params: { source: 'subscription' },
      reason_text: '来自常读订阅源',
    })
    const { client } = makeClient([feedResponse([pending, subscription])])
    renderFeed(client)

    await screen.findByText('筛选收件箱')
    expect(screen.getByText('筛选订阅')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    await waitFor(() => expect(screen.getByText('筛选收件箱')).toBeInTheDocument())
    expect(screen.queryByText('筛选订阅')).not.toBeInTheDocument()
    const filteredPendingCard = screen.getByText('筛选收件箱').closest('li') as HTMLElement

    fireEvent.click(within(filteredPendingCard).getByRole('button', { name: /查看推荐原因/ }))
    expect(within(filteredPendingCard).getByText('推荐原因：收件箱采集（规则 pending_confirmation）')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '为什么推荐' }))
    expect(screen.getByText(/推荐只使用当前身份/)).toBeInTheDocument()
  })

  it('renders the unfinished TODO top three and routes to the canonical TODO surface', async () => {
    const fixture = makeClient([feedResponse([feedItem()])], [
      todo({ id: 'done', text: '已完成任务', done: true }),
      todo({ id: 'todo-1', text: '第一项' }),
      todo({ id: 'todo-2', text: '第二项' }),
      todo({ id: 'todo-3', text: '第三项' }),
      todo({ id: 'todo-4', text: '第四项' }),
    ])
    const { onNavigate } = renderFeed(fixture.client)

    expect(await screen.findByText('第一项')).toBeInTheDocument()
    expect(screen.getByText('第二项')).toBeInTheDocument()
    expect(screen.getByText('第三项')).toBeInTheDocument()
    expect(screen.queryByText('第四项')).not.toBeInTheDocument()
    expect(screen.queryByText('已完成任务')).not.toBeInTheDocument()
    expect(fixture.listTodos).toHaveBeenCalledTimes(1)

    fireEvent.click(screen.getByRole('button', { name: '查看全部' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'tool', id: 'todo' })
  })

  it('refreshes the right-rail TODO preview after a cross-surface TODO change event', async () => {
    const fixture = makeClient([feedResponse([feedItem()])], [todo({ text: '待刷新任务' })])
    fixture.listTodos
      .mockImplementationOnce(async () => ok({ items: [todo({ text: '待刷新任务' })] }))
      .mockImplementationOnce(async () => ok({ items: [] }))
    renderFeed(fixture.client)

    expect(await screen.findByText('待刷新任务')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    await waitFor(() => expect(fixture.listTodos).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('没有未完成任务。')).toBeInTheDocument()
    expect(screen.queryByText('待刷新任务')).not.toBeInTheDocument()
  })

  it('keeps feedback actions distinct, removes only the acted-on card, and opens each source correctly', async () => {
    const link = feedItem({ title: '链接条目' })
    const inbox = feedItem({
      key: 'inbox:I1',
      source: 'inbox',
      title: '收件箱条目',
      url: 'https://example.com/inbox',
      link_id: null,
      inbox_id: 'I1',
      reason_code: 'pending_confirmation',
      reason_text: '收件箱采集',
    })
    const external = feedItem({
      key: 'subscription:R1',
      source: 'subscription',
      title: '外部订阅条目',
      url: 'https://example.com/rss',
      link_id: null,
      feed_item_id: 'R1',
      reason_code: 'subscription_recent',
      reason_text: '订阅更新',
    })
    const { onNavigate, onOpenLink } = renderFeed(makeClient([feedResponse([link, inbox, external])]).client)
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)

    const linkCard = await screen.findByText('链接条目').then((title) => title.closest('li') as HTMLElement)
    const inboxCard = screen.getByText('收件箱条目').closest('li') as HTMLElement
    const externalCard = screen.getByText('外部订阅条目').closest('li') as HTMLElement

    fireEvent.click(within(linkCard).getByRole('button', { name: '打开' }))
    expect(onOpenLink).toHaveBeenCalledWith('L1')
    fireEvent.click(within(inboxCard).getByRole('button', { name: '打开' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I1' })
    fireEvent.click(within(externalCard).getByRole('button', { name: '打开原文' }))
    expect(openSpy).toHaveBeenCalledWith('https://example.com/rss', '_blank', 'noopener,noreferrer')

    fireEvent.click(within(linkCard).getByRole('button', { name: '隐藏' }))
    await waitFor(() => expect(screen.queryByText('链接条目')).not.toBeInTheDocument())
    expect(screen.getByText('收件箱条目')).toBeInTheDocument()

    fireEvent.click(within(inboxCard).getByRole('button', { name: '不感兴趣' }))
    await waitFor(() => expect(screen.queryByText('收件箱条目')).not.toBeInTheDocument())
    expect(screen.getByText('外部订阅条目')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '返回' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'surface', id: 'home' })
  })

  it('returns to Reading when Home is unavailable', async () => {
    const { client } = makeClient([feedResponse([feedItem()])])
    const { onNavigate } = renderFeed(client, { home: false })

    await screen.findByText('保存的文章')
    fireEvent.click(screen.getByRole('button', { name: '返回' }))

    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'reading' })
  })

  it('shows loading, empty, and request error states with retry', async () => {
    let release!: (value: ReturnType<typeof err>) => void
    const pending = new Promise<ReturnType<typeof err>>((resolve) => { release = resolve })
    const getReaderFeed = vi.fn(() => pending)
    const client = {
      getReaderFeed,
      isIdentityCurrent: vi.fn(() => true),
      identityLease: readerIdentity.activeLease,
    } as unknown as IdentityBoundReaderClient
    renderFeed(client)
    expect(screen.getByText('加载中')).toBeInTheDocument()

    await act(async () => {
      release(err({ kind: 'other', message: 'Feed 请求失败', status: 503 }))
      await pending
    })
    expect(screen.getByRole('alert')).toHaveTextContent('Feed 请求失败')
    expect(screen.queryByText('Feed 暂时为空')).not.toBeInTheDocument()
  })

  it('shows the empty state after a successful empty response', async () => {
    const { client } = makeClient([feedResponse([])])
    renderFeed(client)
    expect(await screen.findByText('Feed 暂时为空')).toBeInTheDocument()
  })

  it('clears private Feed state when identity is unavailable', async () => {
    const getReaderFeed = vi.fn()
    const client = {
      getReaderFeed,
      isIdentityCurrent: vi.fn(() => false),
    } as unknown as IdentityBoundReaderClient

    renderFeed(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('身份已失效')
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
    expect(getReaderFeed).not.toHaveBeenCalled()
  })
})

describe('FeedSurface scroll position', () => {
  // jsdom does not lay anything out, so the shell scroller and the cards inside
  // it get a modelled layout: a 600px viewport 100px down the page, cards 200px
  // tall in DOM order, and a scrollTop that actually moves them. Without it
  // every rect is zero and "restored to the right card" would assert nothing.
  const VIEWPORT_TOP = 100
  const CARD_HEIGHT = 200
  const NAMESPACE = 'reader-test-physical-namespace'
  const ALL_SOURCES = ['inbox', 'reading', 'subscription']
  const recommendedKey = feedScrollAnchorKey(NAMESPACE, 'recommended', ALL_SOURCES) as string
  const chronologicalKey = feedScrollAnchorKey(NAMESPACE, 'chronological', ALL_SOURCES) as string
  // The resume record the surface writes between accepting a response and
  // publishing the rendered key — see the identity revocation test below.
  const recommendedResumeKey = `webtag:reader:mixed-feed:v1:${encodeURIComponent(NAMESPACE)}:recommended:${ALL_SOURCES.join(',')}`

  function layoutFeed(): HTMLElement {
    const scroller = document.querySelector('.rvx-scroll') as HTMLElement
    let scrollTop = 0
    Object.defineProperty(scroller, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => { scrollTop = Math.max(0, value) },
    })
    scroller.getBoundingClientRect = () => ({ top: VIEWPORT_TOP, bottom: VIEWPORT_TOP + 600 }) as DOMRect
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      const cards = [...scroller.querySelectorAll<HTMLElement>('[data-feed-item-key]')]
      const index = cards.indexOf(this)
      if (index < 0) return { top: 0, bottom: 0 } as DOMRect
      const top = VIEWPORT_TOP + index * CARD_HEIGHT - scrollTop
      return { top, bottom: top + CARD_HEIGHT } as DOMRect
    })
    return scroller
  }

  function page(prefix: string, snapshotID: string): ReaderFeedResponse {
    return feedResponse(
      [1, 2, 3, 4].map((index) => feedItem({
        key: `link:${prefix}${index}`,
        link_id: `${prefix}${index}`,
        url: `https://example.com/${prefix}${index}`,
        title: `${prefix} 第 ${index} 条`,
      })),
      { snapshot_id: snapshotID },
    )
  }

  /** A client whose Feed responses only land when the test releases them. */
  function makeGatedClient(pages: readonly ReaderFeedResponse[]) {
    const gates: Array<() => void> = []
    let index = 0
    let identityCurrent = true
    const getReaderFeed = vi.fn(async () => {
      const response = pages[Math.min(index++, pages.length - 1)]
      await new Promise<void>((resolve) => { gates.push(resolve) })
      return ok(response)
    })
    const client = {
      getReaderFeed,
      listTodos: vi.fn(async () => ok({ items: [] as ReaderTodoResponse[] })),
      isIdentityCurrent: vi.fn(() => identityCurrent),
      identityLease: readerIdentity.activeLease,
    } as unknown as IdentityBoundReaderClient
    return {
      client,
      /** Models another tab revoking this tab's lease, at any instant. */
      revokeIdentity: () => { identityCurrent = false },
      release: async (request: number) => {
        gates[request]?.()
        await act(async () => {})
      },
    }
  }

  /** A client whose first Feed request fails and whose later ones succeed. */
  function makeFlakyClient(response: ReaderFeedResponse) {
    let attempts = 0
    const getReaderFeed = vi.fn(async () => {
      attempts += 1
      return attempts === 1
        ? err({ kind: 'other' as const, message: 'Feed 暂时不可用', status: 503 })
        : ok(response)
    })
    const client = {
      getReaderFeed,
      listTodos: vi.fn(async () => ok({ items: [] as ReaderTodoResponse[] })),
      isIdentityCurrent: vi.fn(() => true),
      identityLease: readerIdentity.activeLease,
    } as unknown as IdentityBoundReaderClient
    return { client, getReaderFeed }
  }

  // Vitest unwinds afterEach hooks in reverse registration order, so the shared
  // cleanup that unmounts the surface runs after this file's afterEach and
  // would leak the anchor it records into the next test.
  beforeEach(() => {
    window.sessionStorage.clear()
  })

  it('restores the anchored card of the rendered snapshot instead of the stored scrollTop', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const { client } = makeClient([page('L', 'snapshot-anchor')])
    renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    // L3 is the third card, so its top sits 400px into the list; the reader was
    // 50px inside it.
    expect(scroller.scrollTop).toBe(450)
  })

  it('keeps each mode at its own card and restores the position on the way back', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    writeFeedScrollAnchor(chronologicalKey, { itemKey: 'link:C2', offset: 0, scrollTop: 888 })
    const { client } = makeClient([
      page('L', 'snapshot-recommended'),
      page('C', 'snapshot-chronological'),
      page('L', 'snapshot-recommended'),
    ])
    renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)

    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('C 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(200)

    fireEvent.click(screen.getByRole('button', { name: '推荐' }))
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
  })

  it('starts a mode with no stored position at the top', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const { client } = makeClient([
      page('L', 'snapshot-recommended'),
      page('C', 'snapshot-chronological'),
    ])
    renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)

    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('C 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(0)
  })

  it('drops only the refreshed mode position and leaves the other mode alone', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    writeFeedScrollAnchor(chronologicalKey, { itemKey: 'link:C2', offset: 0, scrollTop: 888 })
    const { client } = makeClient([
      page('L', 'snapshot-recommended'),
      page('L', 'snapshot-recommended'),
    ])
    renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)

    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(scroller.scrollTop).toBe(0))
    expect(readFeedScrollAnchor(recommendedKey)).toBeNull()
    expect(readFeedScrollAnchor(chronologicalKey)).toEqual({ itemKey: 'link:C2', offset: 0, scrollTop: 888 })
  })

  it('waits for the returning key own snapshot instead of restoring against an emptied list', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const gate = makeGatedClient([
      page('L', 'snapshot-recommended'),
      page('C', 'snapshot-chronological'),
      page('L', 'snapshot-recommended'),
    ])
    renderFeed(gate.client)
    const scroller = layoutFeed()

    await gate.release(0)
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)

    // Leave and come back before either snapshot answers: the surface is on the
    // recommended key again, but nothing of its content is on screen yet.
    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    fireEvent.click(screen.getByRole('button', { name: '推荐' }))
    await act(async () => {})
    expect(scroller.scrollTop).toBe(0)

    await gate.release(2)
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
  })

  it('keeps the key stored position when a failed load is retried', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const { client, getReaderFeed } = makeFlakyClient(page('L', 'snapshot-recommended'))
    renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByRole('alert')).toHaveTextContent('Feed 暂时不可用')

    // Retry is a reload, not a refresh: the reader is asking for the content
    // they never received, not for a new place in it.
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(getReaderFeed).toHaveBeenCalledTimes(2)
    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    expect(scroller.scrollTop).toBe(450)
  })

  it('drops the restore when the identity is revoked between the response and the commit', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const gate = makeGatedClient([page('L', 'snapshot-recommended')])

    // The surface writes its resume record after `load` has made its own
    // identity check and before it publishes the rendered key — the real
    // window in which another tab's lease revocation can land. Only the
    // restore fence is left to catch it.
    const setItem = Storage.prototype.setItem
    let revokedAtCommit = false
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
      if (key === recommendedResumeKey && !revokedAtCommit) {
        revokedAtCommit = true
        gate.revokeIdentity()
      }
      setItem.call(this, key, value)
    })

    renderFeed(gate.client)
    const scroller = layoutFeed()

    await gate.release(0)
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    // Without this the assertions below would hold for a surface that never
    // lost its identity at all.
    expect(revokedAtCommit).toBe(true)
    expect(gate.client.isIdentityCurrent?.()).toBe(false)

    expect(scroller.scrollTop).toBe(0)
    // A revoked lease loses the replay, not the position itself: the reader
    // who owns this key still finds it where they left it.
    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 999 })
  })

  it('records the card the reader left on when the surface unmounts', async () => {
    const { client } = makeClient([page('L', 'snapshot-recommended')])
    const rendered = renderFeed(client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    scroller.scrollTop = 450

    rendered.unmount()
    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 450 })
  })
})
