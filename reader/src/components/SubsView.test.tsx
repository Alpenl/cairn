import 'fake-indexeddb/auto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { SubsView, type SubsViewProps } from './SubsView'
import { err, ok, type ApiResult } from '@webtag/api'
import type { ReaderClient } from '../lib/api/client'
import type { FeedItem, FeedSubscription, SubscriptionsResponse } from '../lib/api/types'
import { SUBSCRIPTIONS_CACHE_KEY } from '../lib/cache/keys'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { idbClear, idbGetAll, resetDatabaseHandle } from '../lib/cache/idb'
import { startPersistence } from '../lib/cache/persist'
import { enabledReaderCapabilityPolicy } from '../test/capabilities'

const subscription: FeedSubscription = {
  id: 'feed-1',
  feed_url: 'https://example.com/atom.xml',
  title: '工程周刊',
  folder_id: 'folder-1',
  unread_count: 1,
  item_count: 1,
  last_success_at: '2026-07-15T02:00:00Z',
}

const item: FeedItem = {
  id: 'item-1',
  subscription_id: 'feed-1',
  title: '可靠的订阅实现',
  url: 'https://example.com/posts/rss',
  author: 'Alice',
  summary: '一段来自 feed 的摘要',
  content: '正文只在普通 RSS 阅读器中展示。',
  published_at: '2026-07-15T03:00:00Z',
  read_at: null,
  starred_at: null,
  read_later_at: null,
}

function makeClient(
  subscriptions: FeedSubscription[] = [subscription],
  items: FeedItem[] = [item],
) {
  const getFeedItem = vi.fn(async (id: string) =>
    ok({ ...(items.find((candidate) => candidate.id === id) ?? item), read_at: new Date().toISOString() }),
  )
  const updateFeedItem = vi.fn(async (_id: string, patch: Record<string, boolean>) =>
    ok({
      ...item,
      starred_at: patch.starred ? new Date().toISOString() : null,
      read_later_at: patch.read_later ? new Date().toISOString() : null,
    }),
  )
  const analyzeFeedItem = vi.fn(async () =>
    ok({ link_id: 'link-1', status: 'pending' as const }),
  )
  const deleteSubscription = vi.fn(async () => ok(true))
  const importSubscriptionsOPML = vi.fn(async () =>
    ok({ imported: 1, folders: 0, skipped: 0, errors: [] }),
  )
  const isIdentityCurrent = vi.fn(() => true)
  const identityLease = readerIdentity.activeLease
  const captureIdentity = vi.fn((logicalKey: string) => {
    if (!identityLease) return null
    const ownership = identityLease.captureOwnership(logicalKey)
    return identityLease.isOwnershipCurrent(ownership) ? ownership : null
  })
  const client = {
    getSubscriptions: vi.fn(async () =>
      ok({
        folders: [{ id: 'folder-1', name: '技术' }],
        subscriptions,
        counts: { all: 1, unread: 1, starred: 0, later: 0 },
      }),
    ),
    getFeedItems: vi.fn(async () => ok({ items, total: items.length, page: 1, limit: 30 })),
    getFeedItem,
    updateFeedItem,
    analyzeFeedItem,
    markFeedItemsRead: vi.fn(async () => ok(true)),
    refreshSubscription: vi.fn(async () => ok(true)),
    refreshSubscriptions: vi.fn(async () => ok(true)),
    updateSubscription: vi.fn(async () => ok(subscription)),
    deleteSubscription,
    discoverFeeds: vi.fn(async () => ok({ feeds: [] })),
    createSubscription: vi.fn(async () => ok(subscription)),
    createFeedFolder: vi.fn(async () => ok({ id: 'new-folder', name: '新文件夹' })),
    updateFeedFolder: vi.fn(async () => ok(null)),
    deleteFeedFolder: vi.fn(async () => ok(true)),
    importSubscriptionsOPML,
    exportSubscriptionsOPML: vi.fn(async () => ok('<opml version="2.0"/>')),
    isIdentityCurrent,
    captureIdentity,
  } as unknown as ReaderClient
  return {
    client,
    getFeedItem,
    updateFeedItem,
    analyzeFeedItem,
    deleteSubscription,
    importSubscriptionsOPML,
    isIdentityCurrent,
  }
}

function renderView(
  client: ReaderClient,
  onToast: SubsViewProps['onToast'] = () => {},
  onNavigate: SubsViewProps['onNavigate'] = undefined,
) {
  return render(
    <SubsView
      client={client}
      navigationOpen={false}
      onCloseNavigation={() => {}}
      onView={() => {}}
      onNavigate={onNavigate}
      collapsed={false}
      onOpenAnalysis={() => {}}
      onOpenSettings={() => {}}
      onToast={onToast}
      capabilityPolicy={enabledReaderCapabilityPolicy()}
    />,
  )
}

afterEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('SubsView', () => {
  it('订阅工作区通过兼容适配复用 canonical surface/tool 导航', async () => {
    const { client } = makeClient()
    const onNavigate = vi.fn()
    renderView(client, undefined, onNavigate)

    await screen.findByRole('button', { name: /^工程周刊/ })
    fireEvent.click(screen.getByRole('button', { name: '设置' }))
    fireEvent.click(screen.getByRole('tab', { name: '笔记' }))

    expect(onNavigate.mock.calls).toEqual([
      [{ kind: 'tool', id: 'settings' }],
      [{ kind: 'library', id: 'notes' }],
    ])
  })

  it('does not let an A-era batch worker start a fifth action after identity changes', async () => {
    await idbClear()
    const leaseA = readerIdentity.activeLease
    expect(leaseA).not.toBeNull()
    const subscriptions = Array.from({ length: 5 }, (_, index) => ({
      ...subscription,
      id: `feed-${index + 1}`,
      title: `Feed ${index + 1}`,
    }))
    const barriers = Array.from({ length: 4 }, () => {
      let release!: (result: ApiResult<true>) => void
      const promise = new Promise<ApiResult<true>>((resolve) => {
        release = resolve
      })
      return { promise, release }
    })
    const { client, isIdentityCurrent } = makeClient(subscriptions, [])
    const refreshSubscription = vi.mocked(client.refreshSubscription)
    refreshSubscription.mockImplementation((_id) => {
      const index = refreshSubscription.mock.calls.length - 1
      return barriers[index]?.promise ?? Promise.resolve(
        err({ kind: 'identity-mismatch', message: 'stale A batch worker' }),
      )
    })
    isIdentityCurrent.mockImplementation(() => {
      if (!leaseA) return false
      return leaseA.isCurrent(leaseA.capture('SubsView batch continuation'))
    })

    const mounted = renderView(client)
    await screen.findByText('Feed 5')
    fireEvent.click(screen.getByRole('button', { name: '批量管理' }))
    fireEvent.click(screen.getByRole('button', { name: '全选订阅源' }))
    fireEvent.click(screen.getByRole('button', { name: '批量刷新' }))
    await waitFor(() => expect(refreshSubscription).toHaveBeenCalledTimes(4))
    mounted.unmount()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    resourceStore.activateIdentity(leaseB)
    const stopPersistence = startPersistence({ debounceMs: 0, lease: leaseB })
    const bSnapshot = {
      folders: [],
      subscriptions: [{ ...subscriptions[4], refreshing: false }],
      counts: { all: 1, unread: 1, starred: 0, later: 0 },
    }
    resourceStore.set(SUBSCRIPTIONS_CACHE_KEY, bSnapshot)

    try {
      await waitFor(async () => {
        const persisted = (await idbGetAll()).find(
          (record) => record.logicalKey === SUBSCRIPTIONS_CACHE_KEY,
        )
        expect(persisted?.data).toEqual(bSnapshot)
      })

      await act(async () => {
        barriers[0].release(
          err({ kind: 'identity-mismatch', message: 'A identity was replaced' }),
        )
        await barriers[0].promise
        await new Promise((resolve) => setTimeout(resolve, 20))
      })

      const persisted = (await idbGetAll()).find(
        (record) => record.logicalKey === SUBSCRIPTIONS_CACHE_KEY,
      )
      expect({
        refreshCalls: refreshSubscription.mock.calls.map(([id]) => id),
        memory: resourceStore.peek(SUBSCRIPTIONS_CACHE_KEY).data,
        disk: persisted?.data,
      }).toEqual({
        refreshCalls: ['feed-1', 'feed-2', 'feed-3', 'feed-4'],
        memory: bSnapshot,
        disk: bSnapshot,
      })
    } finally {
      for (const barrier of barriers.slice(1)) {
        barrier.release(err({ kind: 'identity-mismatch', message: 'test cleanup' }))
      }
      stopPersistence()
      await idbClear()
      resetDatabaseHandle()
    }
  })

  it('uses the captured ownership capability for a batch optimistic patch', async () => {
    await idbClear()
    const leaseA = readerIdentity.activeLease
    expect(leaseA).not.toBeNull()
    if (!leaseA) return
    const ownershipA = leaseA.captureOwnership('SubsView capability wiring')
    const { client } = makeClient([subscription], [])
    vi.mocked(client.captureIdentity).mockReturnValue(ownershipA)
    const refreshSubscription = vi.mocked(client.refreshSubscription)
    const bSnapshot: SubscriptionsResponse = {
      folders: [],
      subscriptions: [{ ...subscription, refreshing: false }],
      counts: { all: 1, unread: 1, starred: 0, later: 0 },
    }
    const originalCheck = leaseA.isOwnershipCurrent.bind(leaseA)
    let armIdentitySwitch = false
    let stopPersistence = () => {}
    vi.spyOn(leaseA, 'isOwnershipCurrent').mockImplementation((ownership) => {
      if (!armIdentitySwitch) return originalCheck(ownership)
      armIdentitySwitch = false
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
      stopPersistence = startPersistence({ debounceMs: 0, lease: leaseB })
      resourceStore.set(SUBSCRIPTIONS_CACHE_KEY, bSnapshot)
      return true
    })

    const mounted = renderView(client)
    await screen.findAllByText('工程周刊')
    fireEvent.click(screen.getByRole('button', { name: '批量管理' }))
    fireEvent.click(screen.getByRole('button', { name: '全选订阅源' }))
    armIdentitySwitch = true
    fireEvent.click(screen.getByRole('button', { name: '批量刷新' }))
    mounted.unmount()

    try {
      await waitFor(() => expect(refreshSubscription).toHaveBeenCalledTimes(1))
      await waitFor(async () => {
        const persisted = (await idbGetAll()).find(
          (record) => record.logicalKey === SUBSCRIPTIONS_CACHE_KEY,
        )
        expect({
          memory: resourceStore.peek(SUBSCRIPTIONS_CACHE_KEY).data,
          disk: persisted?.data,
        }).toEqual({ memory: bSnapshot, disk: bSnapshot })
      })
    } finally {
      stopPersistence()
      await idbClear()
      resetDatabaseHandle()
    }
  })

  it('does not reload B after an A batch action changes identity', async () => {
    const { client } = makeClient()
    const getSubscriptions = vi.mocked(client.getSubscriptions)
    let mounted: ReturnType<typeof renderView> | null = null
    vi.mocked(client.refreshSubscription).mockImplementation(async () => {
      mounted?.unmount()
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
      return ok(true)
    })
    const onToast = vi.fn<SubsViewProps['onToast']>()

    mounted = renderView(client, onToast)
    await screen.findAllByText('工程周刊')
    fireEvent.click(screen.getByRole('button', { name: '批量管理' }))
    fireEvent.click(screen.getByRole('button', { name: '全选订阅源' }))
    fireEvent.click(screen.getByRole('button', { name: '批量刷新' }))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    mounted.unmount()

    expect(getSubscriptions).toHaveBeenCalledTimes(1)
    expect(onToast).not.toHaveBeenCalled()
  })

  it('does not report an A batch after identity changes while reloads settle', async () => {
    const overview: SubscriptionsResponse = {
      folders: [{ id: 'folder-1', name: '技术' }],
      subscriptions: [subscription],
      counts: { all: 1, unread: 1, starred: 0, later: 0 },
    }
    let releaseReload!: (result: ApiResult<SubscriptionsResponse>) => void
    const reloadBarrier = new Promise<ApiResult<SubscriptionsResponse>>((resolve) => {
      releaseReload = resolve
    })
    const { client } = makeClient()
    const getSubscriptions = vi.mocked(client.getSubscriptions)
    getSubscriptions
      .mockImplementationOnce(async () => ok(overview))
      .mockImplementationOnce(() => reloadBarrier)
    const onToast = vi.fn<SubsViewProps['onToast']>()

    const mounted = renderView(client, onToast)
    await screen.findAllByText('工程周刊')
    fireEvent.click(screen.getByRole('button', { name: '批量管理' }))
    fireEvent.click(screen.getByRole('button', { name: '全选订阅源' }))
    fireEvent.click(screen.getByRole('button', { name: '批量刷新' }))
    await waitFor(() => expect(getSubscriptions).toHaveBeenCalledTimes(2))
    mounted.unmount()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    resourceStore.activateIdentity(leaseB)
    await act(async () => {
      releaseReload(ok(overview))
      await reloadBarrier
      await new Promise((resolve) => setTimeout(resolve, 20))
    })

    expect(onToast).not.toHaveBeenCalled()
  })

  it('does not let an A-era OPML continuation invalidate B after identity replacement', async () => {
    const leaseA = readerIdentity.activeLease
    expect(leaseA).not.toBeNull()
    let releaseFile!: (value: string) => void
    const fileTextBarrier = new Promise<string>((resolve) => {
      releaseFile = resolve
    })
    const fileText = vi.fn(() => fileTextBarrier)
    const {
      client,
      importSubscriptionsOPML,
      isIdentityCurrent,
    } = makeClient()
    isIdentityCurrent.mockImplementation(() => {
      if (!leaseA) return false
      return leaseA.isCurrent(leaseA.capture('SubsView OPML continuation'))
    })
    const mounted = renderView(client)
    await screen.findAllByText('工程周刊')
    const input = mounted.container.querySelector<HTMLInputElement>('input[type="file"]')
    expect(input).not.toBeNull()
    const file = new File([''], 'subscriptions.opml', { type: 'text/xml' })
    Object.defineProperty(file, 'text', { value: fileText })

    fireEvent.change(input!, { target: { files: [file] } })
    await waitFor(() => expect(fileText).toHaveBeenCalledTimes(1))
    mounted.unmount()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    resourceStore.activateIdentity(leaseB)
    const bSnapshot = {
      folders: [],
      subscriptions: [],
      counts: { all: 7, unread: 6, starred: 5, later: 4 },
    }
    resourceStore.set(SUBSCRIPTIONS_CACHE_KEY, bSnapshot)

    await act(async () => {
      releaseFile('<opml version="2.0"/>')
      await fileTextBarrier
      await Promise.resolve()
    })

    expect(importSubscriptionsOPML).not.toHaveBeenCalled()
    expect(resourceStore.peek(SUBSCRIPTIONS_CACHE_KEY).data).toBe(bSnapshot)
  })

  it('渲染 RSS 智能视图、文件夹和普通订阅文章', async () => {
    const { client } = makeClient()
    renderView(client)

    await waitFor(() => expect(screen.getAllByText('工程周刊').length).toBeGreaterThan(0))
    expect(screen.getByText('未读')).toBeInTheDocument()
    expect(screen.getByText('收藏')).toBeInTheDocument()
    expect(screen.getByText('稍后读')).toBeInTheDocument()
    expect(screen.getAllByText('技术').length).toBeGreaterThan(0)
    expect(
      await screen.findByRole('button', { name: '打开 可靠的订阅实现' }),
    ).toBeInTheDocument()
  })

  it('桌面加载后自动选择首篇且只请求一次详情', async () => {
    vi.stubGlobal('innerWidth', 1440)
    const { client, getFeedItem } = makeClient()
    const { container } = renderView(client)

    expect(
      await screen.findByRole('heading', { level: 1, name: '可靠的订阅实现' }),
    ).toBeInTheDocument()
    await waitFor(() => expect(getFeedItem).toHaveBeenCalledTimes(1))
    expect(getFeedItem).toHaveBeenCalledWith('item-1')
    expect(container.querySelector('.rss-workspace')).not.toHaveClass('rss-mobile-detail')
  })

  it('移动端加载后保留列表且不把未展示的首篇标为已读', async () => {
    vi.stubGlobal('innerWidth', 390)
    const { client, getFeedItem } = makeClient()
    const { container } = renderView(client)

    expect(
      await screen.findByRole('button', { name: '打开 可靠的订阅实现' }),
    ).toBeInTheDocument()
    expect(getFeedItem).not.toHaveBeenCalled()
    expect(container.querySelector('.rss-workspace')).not.toHaveClass('rss-mobile-detail')
    expect(
      screen.queryByRole('heading', { level: 1, name: '可靠的订阅实现' }),
    ).not.toBeInTheDocument()
  })

  it('按当前订阅列表顺序导航到下一篇', async () => {
    vi.stubGlobal('innerWidth', 1440)
    const second = {
      ...item,
      id: 'item-2',
      title: '订阅列表第二篇',
      url: 'https://example.com/posts/rss-2',
      content: '第二篇订阅正文。',
    }
    const { client, getFeedItem } = makeClient([subscription], [item, second])
    renderView(client)

    await waitFor(() => expect(getFeedItem).toHaveBeenCalledWith('item-1'))
    fireEvent.click(screen.getByRole('button', { name: '下一条：订阅列表第二篇' }))

    expect(
      await screen.findByRole('heading', { level: 1, name: '订阅列表第二篇' }),
    ).toBeInTheDocument()
    await waitFor(() => expect(getFeedItem).toHaveBeenLastCalledWith('item-2'))
    expect(screen.getByRole('button', { name: '上一条：可靠的订阅实现' })).toBeInTheDocument()
  })

  it('刷新后恢复上次选中的订阅文件夹', async () => {
    const first = makeClient()
    const mounted = renderView(first.client)

    fireEvent.click(await screen.findByRole('button', { name: '技术 1' }))
    await waitFor(() =>
      // 第二个实参携带缓存层的请求取消信号。
      expect(first.client.getFeedItems).toHaveBeenLastCalledWith(
        expect.objectContaining({ folder_id: 'folder-1' }),
        expect.anything(),
      ),
    )
    mounted.unmount()

    const second = makeClient()
    renderView(second.client)
    await waitFor(() =>
      expect(second.client.getFeedItems).toHaveBeenCalledWith(
        expect.objectContaining({ folder_id: 'folder-1' }),
        expect.anything(),
      ),
    )
  })

  it('确认后一键软删除连续失败的订阅源', async () => {
    const broken = {
      ...subscription,
      last_error: 'HTTP 410 Gone',
      failure_count: 3,
    }
    const { client, deleteSubscription } = makeClient([broken])
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    renderView(client)

    fireEvent.click(await screen.findByRole('button', { name: '清理 1 个失效源' }))

    await waitFor(() => expect(deleteSubscription).toHaveBeenCalledWith('feed-1'))
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('连续失败至少 3 次'))
  })

  it('打开条目由详情 GET 标已读，不重复调用 state PUT', async () => {
    const { client, getFeedItem, updateFeedItem } = makeClient()
    renderView(client)

    fireEvent.click(await screen.findByText('可靠的订阅实现'))

    await waitFor(() => expect(getFeedItem).toHaveBeenCalledWith('item-1'))
    expect(updateFeedItem).not.toHaveBeenCalled()
    expect(await screen.findByText('正文只在普通 RSS 阅读器中展示。')).toBeInTheDocument()
  })

  it('支持收藏、稍后读与按需 AI 分析', async () => {
    const { client, updateFeedItem, analyzeFeedItem } = makeClient()
    renderView(client)
    fireEvent.click(await screen.findByText('可靠的订阅实现'))
    await screen.findByText('正文只在普通 RSS 阅读器中展示。')

    const detail = document.querySelector('.rss-detail-pane') as HTMLElement
    fireEvent.click(within(detail).getByRole('button', { name: '收藏' }))
    await waitFor(() =>
      expect(updateFeedItem).toHaveBeenCalledWith('item-1', { starred: true }),
    )
    fireEvent.click(within(detail).getByRole('button', { name: '加入稍后读' }))
    await waitFor(() =>
      expect(updateFeedItem).toHaveBeenCalledWith('item-1', { read_later: true }),
    )
    fireEvent.click(within(detail).getByRole('button', { name: 'AI 分析' }))
    await waitFor(() => expect(analyzeFeedItem).toHaveBeenCalledWith('item-1'))
  })
})
