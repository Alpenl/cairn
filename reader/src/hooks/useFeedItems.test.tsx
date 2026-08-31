import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useFeedItems } from './useFeedItems'
import { ok, type ApiResult } from '@webtag/api'
import type { FeedItem, ListFeedItemsParams, PaginatedFeedItemsResponse } from '../lib/api/types'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { deferred } from '../test/fixtures'
import { bindReaderClient } from '../test/reader-client'

type FeedItemsClient = Parameters<typeof useFeedItems>[0]

function makeItem(id: string, title: string): FeedItem {
  return { id, subscription_id: 'feed', title, url: `https://example.com/${id}` }
}

function bindClient(
  methods: Partial<FeedItemsClient>,
  current: () => boolean = () => true,
): FeedItemsClient {
  const lease = readerIdentity.activeLease!
  const client = bindReaderClient({
    ...methods,
    isIdentityCurrent: vi.fn(() => current()),
  }, { lease })
  return client as FeedItemsClient
}

function Harness({ client, q }: { client: FeedItemsClient; q: string }) {
  const result = useFeedItems(client, { view: 'all', q })
  return (
    <div>
      {result.items.map((candidate) => <span key={candidate.id}>{candidate.title}</span>)}
      <button type="button" onClick={() => void result.reload()}>reload</button>
      <button type="button" onClick={() => void result.loadMore()}>more</button>
    </div>
  )
}

describe('useFeedItems pagination ownership', () => {
  it('筛选切换后忽略旧筛选迟到的下一页', async () => {
    const oldPage = deferred<ApiResult<PaginatedFeedItemsResponse>>()
    const getFeedItems = vi.fn((params: ListFeedItemsParams) => {
      if (params.q === 'old' && params.page === 2) return oldPage.promise
      if (params.q === 'new') {
        return Promise.resolve(ok({ items: [makeItem('new-1', '新筛选文章')], total: 1, page: 1, limit: 30 }))
      }
      return Promise.resolve(ok({ items: [makeItem('old-1', '旧筛选首页')], total: 2, page: 1, limit: 30 }))
    })
    const client = bindClient({
      getFeedItems,
    })
    const rendered = render(<Harness client={client} q="old" />)

    await screen.findByText('旧筛选首页')
    fireEvent.click(screen.getByRole('button', { name: 'more' }))
    await waitFor(() => expect(getFeedItems).toHaveBeenCalledWith(expect.objectContaining({ q: 'old', page: 2 })))
    rendered.rerender(<Harness client={client} q="new" />)
    await screen.findByText('新筛选文章')

    await act(async () => {
      oldPage.resolve(ok({ items: [makeItem('old-2', '迟到的旧文章')], total: 2, page: 2, limit: 30 }))
    })
    expect(screen.queryByText('迟到的旧文章')).not.toBeInTheDocument()
  })

  it('does not append an A-era page after B replaces the client for the same filter', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('feed pagination test')
    const oldPage = deferred<ApiResult<PaginatedFeedItemsResponse>>()
    const clientA = bindClient({
      getFeedItems: vi.fn((params: ListFeedItemsParams) =>
        params.page === 2
          ? oldPage.promise
          : Promise.resolve(ok({
              items: [makeItem('A-1', 'A 首页')],
              total: 2,
              page: 1,
              limit: 30,
            })),
      ),
    }, () => leaseA.isCurrent(ownershipA))
    const rendered = render(<Harness client={clientA} q="same" />)
    await screen.findByText('A 首页')
    fireEvent.click(screen.getByRole('button', { name: 'more' }))
    await waitFor(() => expect(clientA.getFeedItems).toHaveBeenCalledWith(
      expect.objectContaining({ q: 'same', page: 2 }),
    ))

    let clientB!: FeedItemsClient
    act(() => {
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
      clientB = bindClient({
        getFeedItems: vi.fn(async () => ok({
          items: [makeItem('B-1', 'B 首页')],
          total: 1,
          page: 1,
          limit: 30,
        })),
      })
      rendered.rerender(<Harness client={clientB} q="same" />)
    })
    await screen.findByText('B 首页')

    await act(async () => {
      oldPage.resolve(ok({
        items: [makeItem('A-2', 'A 迟到的下一页')],
        total: 2,
        page: 2,
        limit: 30,
      }))
      await oldPage.promise
    })

    expect(screen.queryByText('A 迟到的下一页')).not.toBeInTheDocument()
    expect(screen.getByText('B 首页')).toBeInTheDocument()
  })

  it('does not let a stale reload reset appended items for the replacement filter', async () => {
    let oldFirstPageCalls = 0
    const oldReload = deferred<ApiResult<PaginatedFeedItemsResponse>>()
    const getFeedItems = vi.fn((params: ListFeedItemsParams) => {
      if (params.q === 'old' && params.page === 1) {
        oldFirstPageCalls += 1
        return oldFirstPageCalls === 1
          ? Promise.resolve(ok({ items: [makeItem('old-1', '旧筛选首页')], total: 2, page: 1, limit: 30 }))
          : oldReload.promise
      }
      if (params.q === 'new' && params.page === 2) {
        return Promise.resolve(ok({ items: [makeItem('new-2', '新筛选下一页')], total: 2, page: 2, limit: 30 }))
      }
      return Promise.resolve(ok({ items: [makeItem('new-1', '新筛选首页')], total: 2, page: 1, limit: 30 }))
    })
    const client = bindClient({ getFeedItems })
    const rendered = render(<Harness client={client} q="old" />)

    await screen.findByText('旧筛选首页')
    fireEvent.click(screen.getByRole('button', { name: 'reload' }))
    await waitFor(() => expect(oldFirstPageCalls).toBe(2))

    rendered.rerender(<Harness client={client} q="new" />)
    await screen.findByText('新筛选首页')
    fireEvent.click(screen.getByRole('button', { name: 'more' }))
    await screen.findByText('新筛选下一页')

    await act(async () => {
      oldReload.resolve(ok({ items: [makeItem('old-reload', '迟到 reload')], total: 1, page: 1, limit: 30 }))
      await oldReload.promise
    })

    expect(screen.getByText('新筛选首页')).toBeInTheDocument()
    expect(screen.getByText('新筛选下一页')).toBeInTheDocument()
    expect(screen.queryByText('迟到 reload')).not.toBeInTheDocument()
  })
})
