import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useFeedItems } from './useFeedItems'
import { ok, type ApiResult } from '@webtag/api'
import type { ReaderClient } from '../lib/api/client'
import type { FeedItem, ListFeedItemsParams, PaginatedFeedItemsResponse } from '../lib/api/types'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'

function makeItem(id: string, title: string): FeedItem {
  return { id, subscription_id: 'feed', title, url: `https://example.com/${id}` }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}

function Harness({ client, q }: { client: ReaderClient; q: string }) {
  const result = useFeedItems(client, { view: 'all', q })
  return (
    <div>
      {result.items.map((candidate) => <span key={candidate.id}>{candidate.title}</span>)}
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
    const client = {
      getFeedItems,
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
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
    const clientA = {
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
      isIdentityCurrent: vi.fn(() => leaseA.isCurrent(ownershipA)),
    } as unknown as ReaderClient
    const rendered = render(<Harness client={clientA} q="same" />)
    await screen.findByText('A 首页')
    fireEvent.click(screen.getByRole('button', { name: 'more' }))
    await waitFor(() => expect(clientA.getFeedItems).toHaveBeenCalledWith(
      expect.objectContaining({ q: 'same', page: 2 }),
    ))

    const clientB = {
      getFeedItems: vi.fn(async () => ok({
        items: [makeItem('B-1', 'B 首页')],
        total: 1,
        page: 1,
        limit: 30,
      })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    act(() => {
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
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
})
