import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { FeedSurface } from './FeedSurface'
import { feedScrollAnchorKey, readFeedScrollAnchor, writeFeedScrollAnchor } from '../../lib/feed-scroll-anchor'
import { err, ok } from '../../lib/api/result'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type {
  ReaderFeedFeedbackRequest,
  ReaderFeedFeedbackResponse,
  ReaderFeedItemResponse,
  ReaderFeedResponse,
  ReaderInboxConfirmAIProposalsResponse,
  ReaderTodoResponse,
} from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { readerIdentity } from '../../lib/identity'
import { enabledReaderCapabilityLease } from '../../test/capabilities'

function feedItem(overrides: Partial<ReaderFeedItemResponse> = {}): ReaderFeedItemResponse {
  const source = overrides.source ?? 'reading'
  const key = overrides.key ?? 'link:L1'
  return {
    key,
    source,
    resource_key: overrides.resource_key ?? key,
    title: '保存的文章',
    summary: '文章摘要',
    url: 'https://example.com/article',
    link_id: source === 'reading' ? 'L1' : null,
    inbox_id: null,
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    event_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

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
    mode: 'recommended',
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
  todoResponses: ReaderTodoResponse[][] = [[]],
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
  const sendReaderFeedFeedback = vi.fn(async (itemKey: string, request: ReaderFeedFeedbackRequest) => ok<ReaderFeedFeedbackResponse>({
    item_key: itemKey,
    action: request.action,
    ...(request.action === 'save' ? { link_id: 'saved-link' } : {}),
  }))
  const confirmInbox = vi.fn(async () => ok({
    target_kind: 'link' as const,
    link_id: 'confirmed-link',
    status: 'confirmed' as const,
  }))
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
  return {
    client,
    getReaderFeed,
    patchEngagement,
    updateFeedItem,
    sendReaderFeedFeedback,
    confirmInbox,
    confirmAIProposals,
    discardInbox,
    listTodos,
  }
}

function renderFeed(
  client: IdentityBoundReaderClient,
  capabilityOverrides: Parameters<typeof enabledReaderCapabilityLease>[0] = {},
) {
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  const onOpenLink = vi.fn<(id: string) => void>()
  const rendered = render(
    <FeedSurface
      client={client}
      capabilityLease={enabledReaderCapabilityLease(capabilityOverrides)}
      onNavigate={onNavigate}
      onOpenLink={onOpenLink}
    />,
  )
  return { ...rendered, onNavigate, onOpenLink }
}

afterEach(() => {
  window.sessionStorage.clear()
  vi.restoreAllMocks()
})

describe('FeedSurface', () => {
  it('does not request Feed data when Feed is unavailable', () => {
    const fixture = makeClient()

    renderFeed(fixture.client, { feed: false, todos: false })

    expect(fixture.getReaderFeed).not.toHaveBeenCalled()
    expect(screen.queryByText('保存的文章')).not.toBeInTheDocument()
  })

  it('loads the minimal live feed contract and derives source explanations locally', async () => {
    const fixture = makeClient([feedResponse([
      feedItem({ title: '阅读条目' }),
      feedItem({
        key: 'inbox:I1',
        resource_key: 'inbox:I1',
        source: 'inbox',
        title: '收件箱条目',
        url: 'https://example.com/inbox',
        link_id: null,
        inbox_id: 'I1',
      }),
      feedItem({
        key: 'subscription:R1',
        resource_key: 'feed_item:R1',
        source: 'subscription',
        title: '订阅条目',
        url: 'https://example.com/rss',
        link_id: null,
        feed_item_id: 'R1',
      }),
    ])])

    renderFeed(fixture.client)

    expect(await screen.findByText('阅读条目')).toBeInTheDocument()
    expect(screen.getByLabelText('查看推荐原因：已保存到资料库')).toBeInTheDocument()
    expect(screen.getByLabelText('查看推荐原因：收件箱采集')).toBeInTheDocument()
    expect(screen.getByLabelText('查看推荐原因：订阅更新')).toBeInTheDocument()
    expect(fixture.getReaderFeed).toHaveBeenCalledWith({
      mode: 'recommended',
      source: ['inbox', 'reading', 'subscription'],
      limit: 30,
    })
  })

  it('preserves live page order, deduplicates resources, and binds later requests to cursor parameters', async () => {
    const first = feedResponse([
      feedItem({ key: 'link:A', resource_key: 'link:A', link_id: 'A', title: 'A' }),
      feedItem({ key: 'link:B', resource_key: 'link:B', link_id: 'B', title: 'B', url: 'https://example.com/b' }),
    ], { next_cursor: 'cursor-1' })
    const second = feedResponse([
      feedItem({
        key: 'subscription:alias-A',
        resource_key: 'link:A',
        source: 'subscription',
        link_id: 'A',
        feed_item_id: 'alias-A',
        title: 'A duplicate',
      }),
      feedItem({ key: 'link:C', resource_key: 'link:C', link_id: 'C', title: 'C', url: 'https://example.com/c' }),
    ])
    const filtered = feedResponse([
      feedItem({ key: 'link:B', resource_key: 'link:B', link_id: 'B', title: 'B filtered' }),
    ])
    const chronological = feedResponse([
      feedItem({ key: 'link:C', resource_key: 'link:C', link_id: 'C', title: 'C chronological' }),
      feedItem({ key: 'link:B', resource_key: 'link:B', link_id: 'B', title: 'B chronological' }),
    ], { mode: 'chronological' })
    const fixture = makeClient([first, second, filtered, chronological])
    const { container } = renderFeed(fixture.client)

    await screen.findByText('A')
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('C')).toBeInTheDocument()
    expect(renderedResourceKeys(container)).toEqual(['link:A', 'link:B', 'link:C'])
    expect(screen.queryByText('A duplicate')).not.toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[1][0]).toMatchObject({
      mode: 'recommended',
      source: ['inbox', 'reading', 'subscription'],
      after: 'cursor-1',
    })

    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    expect(await screen.findByText('B filtered')).toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[2][0]).toMatchObject({
      mode: 'recommended',
      source: ['inbox', 'reading'],
    })
    expect(fixture.getReaderFeed.mock.calls[2][0]).not.toHaveProperty('after')
    expect(fixture.getReaderFeed.mock.calls[2][0]).not.toHaveProperty('snapshotID')

    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('C chronological')).toBeInTheDocument()
    expect(renderedResourceKeys(container)).toEqual(['link:C', 'link:B'])
    expect(fixture.getReaderFeed.mock.calls[3][0]).toMatchObject({
      mode: 'chronological',
      source: ['inbox', 'reading'],
    })
  })

  it('renders the server event_at instant without reconstructing chronology', async () => {
    const eventAt = '2026-07-01T08:30:00.123Z'
    renderFeed(makeClient([feedResponse([feedItem({ title: '权威可见时间', event_at: eventAt })])]).client)

    const card = await screen.findByText('权威可见时间').then((title) => title.closest('li') as HTMLElement)
    expect(card.querySelector('time')).toHaveAttribute('datetime', eventAt)
  })

  it('resumes local items and the live cursor without another first-page request', async () => {
    const first = feedResponse([feedItem({ title: '第一页' })], { next_cursor: 'cursor-1' })
    const second = feedResponse([
      feedItem({ key: 'link:L2', resource_key: 'link:L2', link_id: 'L2', title: '第二页' }),
    ])
    const fixture = makeClient([first, second])
    const firstRender = renderFeed(fixture.client)

    const firstCard = await screen.findByText('第一页').then((title) => title.closest('li') as HTMLElement)
    fireEvent.click(within(firstCard).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(within(firstCard).getByRole('button', { name: '已读' })).toBeInTheDocument())
    firstRender.unmount()

    renderFeed(fixture.client)
    const resumedCard = await screen.findByText('第一页').then((title) => title.closest('li') as HTMLElement)
    expect(within(resumedCard).getByRole('button', { name: '已读' })).toBeInTheDocument()
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(1)

    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('第二页')).toBeInTheDocument()
    expect(fixture.getReaderFeed.mock.calls[1][0]).toMatchObject({ after: 'cursor-1' })
  })

  it('stores source and mode views independently', async () => {
    const allSources = feedResponse([feedItem({ title: '全部来源' })])
    const filtered = feedResponse([feedItem({ title: '筛选来源' })], { next_cursor: 'filtered-cursor' })
    const chronological = feedResponse([feedItem({ title: '时间排序' })], { mode: 'chronological' })
    const fixture = makeClient([allSources, filtered, chronological])
    const rendered = renderFeed(fixture.client)

    await screen.findByText('全部来源')
    fireEvent.click(screen.getByRole('switch', { name: '订阅' }))
    expect(await screen.findByText('筛选来源')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('时间排序')).toBeInTheDocument()
    rendered.unmount()

    renderFeed(fixture.client)
    expect(await screen.findByText('时间排序')).toBeInTheDocument()
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(3)
  })

  it('updates reading and subscription engagement through their owning endpoints', async () => {
    const fixture = makeClient([feedResponse([
      feedItem({ title: '阅读文章' }),
      feedItem({
        key: 'subscription:R1',
        resource_key: 'feed_item:R1',
        source: 'subscription',
        title: 'RSS 文章',
        link_id: null,
        feed_item_id: 'R1',
      }),
    ])])
    renderFeed(fixture.client)

    const reading = await screen.findByText('阅读文章').then((title) => title.closest('li') as HTMLElement)
    const subscription = screen.getByText('RSS 文章').closest('li') as HTMLElement
    fireEvent.click(within(reading).getByRole('button', { name: '未读' }))
    await waitFor(() => expect(fixture.patchEngagement).toHaveBeenCalledWith('L1', { read: true }))
    fireEvent.click(within(subscription).getByRole('button', { name: '稍后读' }))
    await waitFor(() => expect(fixture.updateFeedItem).toHaveBeenCalledWith('R1', { read_later: true }))
  })

  it('saves subscriptions through feedback using the card key and rolls failed state back', async () => {
    const subscription = feedItem({
      key: 'subscription:R1',
      resource_key: 'feed_item:R1',
      source: 'subscription',
      title: 'RSS 文章',
      link_id: null,
      feed_item_id: 'R1',
    })
    const fixture = makeClient([feedResponse([subscription])])
    fixture.sendReaderFeedFeedback
      .mockResolvedValueOnce(ok({
        item_key: 'subscription:R1',
        action: 'save',
        link_id: 'saved-link',
      }))
      .mockResolvedValueOnce(err({ kind: 'other', message: '取消保存失败', status: 503 }))
    renderFeed(fixture.client)
    const card = await screen.findByRole('listitem')

    fireEvent.click(within(card).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fixture.sendReaderFeedFeedback).toHaveBeenCalledWith('subscription:R1', { action: 'save' }))
    expect(within(card).getByRole('button', { name: '取消保存' })).toBeInTheDocument()
    fireEvent.click(within(card).getByRole('button', { name: '取消保存' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('取消保存失败'))
    expect(within(card).getByRole('button', { name: '取消保存' })).toBeInTheDocument()
  })

  it('confirms and discards inbox cards while preserving the remaining live page', async () => {
    const fixture = makeClient([feedResponse([
      feedItem({
        key: 'inbox:I1',
        resource_key: 'inbox:I1',
        source: 'inbox',
        title: '待确认',
        link_id: null,
        inbox_id: 'I1',
      }),
      feedItem({
        key: 'inbox:I2',
        resource_key: 'inbox:I2',
        source: 'inbox',
        title: '待丢弃',
        url: 'https://example.com/inbox-2',
        link_id: null,
        inbox_id: 'I2',
      }),
    ], { next_cursor: 'cursor-after-inbox' })])
    const rendered = renderFeed(fixture.client)

    const confirmCard = await screen.findByText('待确认').then((title) => title.closest('li') as HTMLElement)
    fireEvent.click(within(confirmCard).getByRole('button', { name: '确认' }))
    await waitFor(() => expect(rendered.onOpenLink).toHaveBeenCalledWith('confirmed-link'))
    expect(screen.queryByText('待确认')).not.toBeInTheDocument()

    const discardCard = screen.getByText('待丢弃').closest('li') as HTMLElement
    fireEvent.click(within(discardCard).getByRole('button', { name: '丢弃' }))
    await waitFor(() => expect(screen.queryByText('待丢弃')).not.toBeInTheDocument())
    expect(fixture.confirmInbox).toHaveBeenCalledWith('I1')
    expect(fixture.discardInbox).toHaveBeenCalledWith('I2')
  })

  it('drains server-owned AI confirmation batches and reloads the first live page', async () => {
    const fixture = makeClient([
      feedResponse([feedItem({ title: '确认前' })]),
      feedResponse([feedItem({ key: 'link:L2', resource_key: 'link:L2', link_id: 'L2', title: '确认后' })]),
    ])
    fixture.confirmAIProposals
      .mockResolvedValueOnce(ok({
        atomic: true,
        items: [{ inbox_id: 'inbox-1', status: 'confirmed', link_id: 'link-inbox-1' }],
        remaining_count: 1,
      }))
      .mockResolvedValueOnce(ok({
        atomic: true,
        items: [{ inbox_id: 'inbox-2', status: 'confirmed', link_id: 'link-inbox-2' }],
        remaining_count: 0,
      }))
    renderFeed(fixture.client)

    await screen.findByText('确认前')
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    expect(await screen.findByText('确认后')).toBeInTheDocument()
    expect(fixture.confirmAIProposals).toHaveBeenCalledTimes(2)
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2)
    expect(fixture.getReaderFeed.mock.calls[1][0]).not.toHaveProperty('after')
  })

  it('refreshes the TODO preview after a cross-surface change', async () => {
    const fixture = makeClient(
      [feedResponse([feedItem()])],
      [[todo({ text: '待刷新任务' })], [todo({ id: 'todo-new', text: '刷新后的任务' })]],
    )
    const rendered = renderFeed(fixture.client)

    expect(await screen.findByText('待刷新任务')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))
    expect(await screen.findByText('刷新后的任务')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '查看全部' }))
    expect(rendered.onNavigate).toHaveBeenCalledWith({ kind: 'tool', id: 'todo' })
  })

  it('derives opening and feedback commands from source identity', async () => {
    const fixture = makeClient([feedResponse([
      feedItem({ title: '链接条目' }),
      feedItem({
        key: 'inbox:I1',
        resource_key: 'inbox:I1',
        source: 'inbox',
        title: '收件箱条目',
        url: 'https://example.com/inbox',
        link_id: null,
        inbox_id: 'I1',
      }),
      feedItem({
        key: 'subscription:R1',
        resource_key: 'feed_item:R1',
        source: 'subscription',
        title: '外部订阅条目',
        url: 'https://example.com/rss',
        link_id: null,
        feed_item_id: 'R1',
      }),
    ])])
    const rendered = renderFeed(fixture.client)
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)

    const linkCard = await screen.findByText('链接条目').then((title) => title.closest('li') as HTMLElement)
    const inboxCard = screen.getByText('收件箱条目').closest('li') as HTMLElement
    const externalCard = screen.getByText('外部订阅条目').closest('li') as HTMLElement
    fireEvent.click(within(linkCard).getByRole('button', { name: '打开' }))
    expect(rendered.onOpenLink).toHaveBeenCalledWith('L1')
    fireEvent.click(within(inboxCard).getByRole('button', { name: '打开' }))
    expect(rendered.onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I1' })
    fireEvent.click(within(externalCard).getByRole('button', { name: '打开原文' }))
    expect(openSpy).toHaveBeenCalledWith('https://example.com/rss', '_blank', 'noopener,noreferrer')

    fireEvent.click(within(linkCard).getByRole('button', { name: '隐藏' }))
    await waitFor(() => expect(screen.queryByText('链接条目')).not.toBeInTheDocument())
    expect(fixture.sendReaderFeedFeedback).toHaveBeenCalledWith('link:L1', { action: 'hide' })
  })

  it('returns to Reading when Home is unavailable', async () => {
    const fixture = makeClient()
    const rendered = renderFeed(fixture.client, { home: false })

    await screen.findByText('保存的文章')
    fireEvent.click(screen.getByRole('button', { name: '返回' }))

    expect(rendered.onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'reading' })
  })

  it('shows request errors and retries without discarding the current anchor', async () => {
    const response = feedResponse([feedItem({ title: '重试成功' })])
    let attempts = 0
    const fixture = makeClient([response])
    fixture.getReaderFeed.mockImplementation(async () => {
      attempts += 1
      return attempts === 1
        ? err({ kind: 'other', message: 'Feed 请求失败', status: 503 })
        : ok(response)
    })
    renderFeed(fixture.client)

    expect(await screen.findByRole('alert')).toHaveTextContent('Feed 请求失败')
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('重试成功')).toBeInTheDocument()
  })

  it('shows the empty state after a successful empty response', async () => {
    renderFeed(makeClient([feedResponse([])]).client)
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
    expect(getReaderFeed).not.toHaveBeenCalled()
  })
})

describe('FeedSurface scroll position', () => {
  const VIEWPORT_TOP = 100
  const CARD_HEIGHT = 200
  const NAMESPACE = 'reader-test-physical-namespace'
  const ALL_SOURCES = ['inbox', 'reading', 'subscription']
  const recommendedKey = feedScrollAnchorKey(NAMESPACE, 'recommended', ALL_SOURCES) as string
  const chronologicalKey = feedScrollAnchorKey(NAMESPACE, 'chronological', ALL_SOURCES) as string
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

  function page(prefix: string): ReaderFeedResponse {
    return feedResponse([1, 2, 3, 4].map((index) => feedItem({
      key: `link:${prefix}${index}`,
      resource_key: `link:${prefix}${index}`,
      link_id: `${prefix}${index}`,
      url: `https://example.com/${prefix}${index}`,
      title: `${prefix} 第 ${index} 条`,
    })))
  }

  function makeGatedClient(response: ReaderFeedResponse) {
    let releaseRequest!: () => void
    let identityCurrent = true
    const getReaderFeed = vi.fn(async () => {
      await new Promise<void>((resolve) => { releaseRequest = resolve })
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
      revokeIdentity: () => { identityCurrent = false },
      release: async () => {
        releaseRequest()
        await act(async () => {})
      },
    }
  }

  beforeEach(() => {
    window.sessionStorage.clear()
  })

  it('restores the anchored card instead of the stored scrollTop', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const fixture = makeClient([page('L')])
    renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
  })

  it('keeps each mode at its own card and restores cached content on return', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    writeFeedScrollAnchor(chronologicalKey, { itemKey: 'link:C2', offset: 0, scrollTop: 888 })
    const fixture = makeClient([page('L'), { ...page('C'), mode: 'chronological' }])
    renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('C 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(200)
    fireEvent.click(screen.getByRole('button', { name: '推荐' }))
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
    expect(fixture.getReaderFeed).toHaveBeenCalledTimes(2)
  })

  it('starts a mode with no stored position at the top', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const fixture = makeClient([page('L'), { ...page('C'), mode: 'chronological' }])
    renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
    fireEvent.click(screen.getByRole('button', { name: '时间' }))
    expect(await screen.findByText('C 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(0)
  })

  it('drops only the refreshed mode position', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    writeFeedScrollAnchor(chronologicalKey, { itemKey: 'link:C2', offset: 0, scrollTop: 888 })
    const fixture = makeClient([page('L'), page('L')])
    renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(scroller.scrollTop).toBe(450)
    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(scroller.scrollTop).toBe(0))
    expect(readFeedScrollAnchor(recommendedKey)).toBeNull()
    expect(readFeedScrollAnchor(chronologicalKey)).toEqual({ itemKey: 'link:C2', offset: 0, scrollTop: 888 })
  })

  it('keeps the stored position when a failed load is retried', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const response = page('L')
    const fixture = makeClient([response])
    fixture.getReaderFeed
      .mockResolvedValueOnce(err({ kind: 'other', message: 'Feed 暂时不可用', status: 503 }))
      .mockResolvedValueOnce(ok(response))
    renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByRole('alert')).toHaveTextContent('Feed 暂时不可用')
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    expect(scroller.scrollTop).toBe(450)
  })

  it('drops restoration when identity is revoked between response and rendered commit', async () => {
    writeFeedScrollAnchor(recommendedKey, { itemKey: 'link:L3', offset: -50, scrollTop: 999 })
    const gate = makeGatedClient(page('L'))
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

    await gate.release()
    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    expect(revokedAtCommit).toBe(true)
    expect(scroller.scrollTop).toBe(0)
    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 999 })
  })

  it('records the card visible when the surface unmounts', async () => {
    const fixture = makeClient([page('L')])
    const rendered = renderFeed(fixture.client)
    const scroller = layoutFeed()

    expect(await screen.findByText('L 第 1 条')).toBeInTheDocument()
    scroller.scrollTop = 450
    rendered.unmount()

    expect(readFeedScrollAnchor(recommendedKey)).toEqual({ itemKey: 'link:L3', offset: -50, scrollTop: 450 })
  })
})
