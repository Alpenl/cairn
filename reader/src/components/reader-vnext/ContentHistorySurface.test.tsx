import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { ContentHistorySurface } from './ContentHistorySurface'
import { err, ok, type ApiResult } from '../../lib/api/result'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderContentHistoryResponse } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { readerIdentity } from '../../lib/identity'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'

function historyItem(overrides: Partial<ReaderContentHistoryResponse> = {}): ReaderContentHistoryResponse {
  return {
    id: 7,
    revision: 1,
    content: '旧版正文',
    content_document: null,
    content_format: 'plain',
    content_source: 'fetched',
    created_at: '2026-08-09T01:00:00Z',
    ...overrides,
  }
}

function makeClient(items: ReaderContentHistoryResponse[]) {
  const listContentHistory = vi.fn(async () => ok(items))
  const restoreContentHistory = vi.fn(async () => ok({ link_id: 'L1', content_revision: 3 }))
  const getLink = vi.fn(async () => ok({ content_revision: 2 }))
  const client = {
    getLink,
    listContentHistory,
    restoreContentHistory,
    isIdentityCurrent: vi.fn(() => true),
    identityLease: readerIdentity.activeLease,
  } as unknown as IdentityBoundReaderClient
  return { client, getLink, listContentHistory, restoreContentHistory }
}

function renderSurface(
  client: IdentityBoundReaderClient,
  onRestored = vi.fn(),
  expectedContentRevision = 2,
) {
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  const rendered = render(
    <ContentHistorySurface
      client={client}
      capabilityPolicy={enabledReaderCapabilityPolicy()}
      linkID="L1"
      expectedContentRevision={expectedContentRevision}
      onNavigate={onNavigate}
      onRestored={onRestored}
    />,
  )
  return { ...rendered, onNavigate, onRestored }
}

describe('ContentHistorySurface', () => {
  it('lists snapshots and restores with the current content revision CAS', async () => {
    const { client, restoreContentHistory } = makeClient([
      historyItem({ revision: 1 }),
      historyItem({ id: 8, revision: 2, content: '当前正文' }),
    ])
    const { onRestored } = renderSurface(client)

    expect(await screen.findByText('版本 1')).toBeInTheDocument()
    expect(screen.getByText('版本 2')).toBeInTheDocument()
    const first = screen.getByText('版本 1').closest('li') as HTMLElement
    fireEvent.click(within(first).getByRole('button', { name: '恢复' }))

    await waitFor(() => expect(restoreContentHistory).toHaveBeenCalledWith(
      'L1',
      7,
      { expected_content_revision: 2 },
    ))
    expect(onRestored).toHaveBeenCalledWith(3)
  })

  it('does not offer a restore action for the current revision', async () => {
    const { client } = makeClient([historyItem({ revision: 2, content: '当前正文' })])
    renderSurface(client)

    const item = await screen.findByText('版本 2')
    expect(within(item.closest('li') as HTMLElement).getByRole('button', { name: '当前版本' })).toBeDisabled()
  })

  it('does not accept a restore response that moves the current revision backwards', async () => {
    const { client, restoreContentHistory } = makeClient([historyItem({ revision: 1 })])
    const onRestored = vi.fn()
    restoreContentHistory.mockResolvedValue(ok({ link_id: 'L1', content_revision: 1 }))
    renderSurface(client, onRestored)

    const item = await screen.findByText('版本 1')
    fireEvent.click(within(item.closest('li') as HTMLElement).getByRole('button', { name: '恢复' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('版本没有前进')
    expect(onRestored).not.toHaveBeenCalled()
    expect(screen.getByText('当前正文版本').parentElement).toHaveTextContent('2')
  })

  it('recovers from a restore conflict without leaving the row busy', async () => {
    const { client, restoreContentHistory } = makeClient([historyItem({ revision: 1 })])
    restoreContentHistory.mockResolvedValue(err({
      kind: 'other',
      status: 409,
      message: '正文已被其他请求更新',
      errorCode: 'revision_conflict',
    }))
    renderSurface(client)

    const item = await screen.findByText('版本 1')
    const row = item.closest('li') as HTMLElement
    fireEvent.click(within(row).getByRole('button', { name: '恢复' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('内容已经被其他窗口更新')
    expect(within(row).getByRole('button', { name: '恢复' })).not.toBeDisabled()
  })

  it('shows a stale revision as a retryable error', async () => {
    const { client, restoreContentHistory } = makeClient([historyItem({ revision: 1 })])
    restoreContentHistory.mockResolvedValue(err({
      kind: 'other',
      status: 412,
      message: '正文版本已变化',
      errorCode: 'content_revision_stale',
    }))
    renderSurface(client)

    const item = await screen.findByText('版本 1')
    const row = item.closest('li') as HTMLElement
    fireEvent.click(within(row).getByRole('button', { name: '恢复' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('版本已经变化')
    expect(within(row).getByRole('button', { name: '恢复' })).not.toBeDisabled()
  })

  it('uses each accepted server revision as the next restore CAS floor', async () => {
    const { client, restoreContentHistory } = makeClient([historyItem({ revision: 1 })])
    const onRestored = vi.fn()
    restoreContentHistory
      .mockResolvedValueOnce(ok({ link_id: 'L1', content_revision: 3 }))
      .mockResolvedValueOnce(ok({ link_id: 'L1', content_revision: 4 }))
    const rendered = renderSurface(client, onRestored)

    const item = await screen.findByText('版本 1')
    const row = item.closest('li') as HTMLElement
    fireEvent.click(within(row).getByRole('button', { name: '恢复' }))
    await waitFor(() => expect(onRestored).toHaveBeenNthCalledWith(1, 3))
    expect(screen.getByText('当前正文版本').parentElement).toHaveTextContent('3')

    // A late parent render must not replay the older expected revision after a restore.
    rendered.rerender(
      <ContentHistorySurface
        client={client}
        capabilityPolicy={enabledReaderCapabilityPolicy()}
        linkID="L1"
        expectedContentRevision={2}
        onNavigate={rendered.onNavigate}
        onRestored={onRestored}
      />,
    )
    expect(screen.getByText('当前正文版本').parentElement).toHaveTextContent('3')

    await waitFor(() => expect(within(row).getByRole('button', { name: '恢复' })).not.toBeDisabled())
    fireEvent.click(within(row).getByRole('button', { name: '恢复' }))
    await waitFor(() => expect(restoreContentHistory).toHaveBeenNthCalledWith(
      2,
      'L1',
      7,
      { expected_content_revision: 3 },
    ))
    expect(onRestored).toHaveBeenNthCalledWith(2, 4)
  })

  it('keeps restore disabled until the current revision is known', async () => {
    const { client, getLink } = makeClient([historyItem({ revision: 1 })])
    let resolveLink!: (result: ApiResult<{ content_revision: number }>) => void
    const pending = new Promise<ApiResult<{ content_revision: number }>>((resolve) => {
      resolveLink = resolve
    })
    getLink.mockReturnValue(pending)
    renderSurface(client, vi.fn(), 0)

    const item = await screen.findByText('版本 1')
    const row = item.closest('li') as HTMLElement
    expect(within(row).getByRole('button', { name: '恢复' })).toBeDisabled()

    await act(async () => {
      resolveLink(ok({ content_revision: 2 }))
      await pending
    })
    await waitFor(() => expect(within(row).getByRole('button', { name: '恢复' })).not.toBeDisabled())
  })

  it('replaces the list on refresh without retaining the previous snapshot', async () => {
    const first = [historyItem({ content: '刷新前正文' })]
    const second = [historyItem({ id: 8, content: '刷新后正文' })]
    const { client, listContentHistory } = makeClient(first)
    listContentHistory
      .mockResolvedValueOnce(ok(first))
      .mockResolvedValueOnce(ok(second))
    renderSurface(client)

    expect(await screen.findByText('刷新前正文', { selector: 'p' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '刷新' }))

    expect(await screen.findByText('刷新后正文', { selector: 'p' })).toBeInTheDocument()
    expect(screen.queryByText('刷新前正文')).not.toBeInTheDocument()
    expect(listContentHistory).toHaveBeenCalledTimes(2)
  })

  it('does not let an old identity response populate a newly bound surface', async () => {
    let resolveOld!: (result: ApiResult<ReaderContentHistoryResponse[]>) => void
    const oldResponse = new Promise<ApiResult<ReaderContentHistoryResponse[]>>((resolve) => {
      resolveOld = resolve
    })
    const old = makeClient([])
    old.listContentHistory.mockReturnValue(oldResponse)
    const rendered = renderSurface(old.client)

    let resolveNew!: (result: ApiResult<ReaderContentHistoryResponse[]>) => void
    const newResponse = new Promise<ApiResult<ReaderContentHistoryResponse[]>>((resolve) => {
      resolveNew = resolve
    })
    const next = makeClient([])
    next.listContentHistory.mockReturnValue(newResponse)
    rendered.rerender(
      <ContentHistorySurface
        client={next.client}
        capabilityPolicy={enabledReaderCapabilityPolicy()}
        linkID="L1"
        expectedContentRevision={2}
        onNavigate={rendered.onNavigate}
        onRestored={rendered.onRestored}
      />,
    )

    await act(async () => {
      resolveOld(ok([historyItem({ content: '旧身份正文' })]))
      await oldResponse
    })
    expect(screen.queryByText('旧身份正文')).not.toBeInTheDocument()

    await act(async () => {
      resolveNew(ok([historyItem({ content: '新身份正文' })]))
      await newResponse
    })
    expect(await screen.findByText('新身份正文', { selector: 'p' })).toBeInTheDocument()
  })
})
