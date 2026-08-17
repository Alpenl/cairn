import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok } from '../../lib/api/result'
import type { ReaderTrashItemResponse } from '../../lib/api/types'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { TrashSurface } from './TrashSurface'

function item(overrides: Partial<ReaderTrashItemResponse> = {}): ReaderTrashItemResponse {
  return {
    host_kind: 'link',
    host_id: '00000000-0000-0000-0000-000000000001',
    title: '一篇文章',
    url: 'https://example.com/article',
    trashed_at: '2026-08-17T01:00:00Z',
    ...overrides,
  } as ReaderTrashItemResponse
}

function mountTrash(client: Partial<IdentityBoundReaderClient>, onToast = vi.fn()) {
  render(
    <TrashSurface
      client={client as IdentityBoundReaderClient}
      onNavigate={vi.fn()}
      capabilityPolicy={enabledReaderCapabilityPolicy()}
      onToast={onToast}
    />,
  )
  return onToast
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('TrashSurface', () => {
  // 混合列表的核心价值就是三类同框、按删除时间排序。分类标记与「恢复去哪」
  // 必须同时出现，否则用户点恢复前不知道东西会落到哪里。
  it('lists all three host kinds together and says where each one restores to', async () => {
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(ok({
        count: 3,
        items: [
          item(),
          item({ host_kind: 'inbox', host_id: 'inbox-1', title: '待读条目' }),
          item({ host_kind: 'note', host_id: 'note-1', title: '一条笔记', url: null }),
        ],
      })),
    })

    await screen.findByText('一篇文章')
    expect(screen.getByText('待读条目')).toBeInTheDocument()
    expect(screen.getByText('一条笔记')).toBeInTheDocument()
    expect(screen.getByText(/恢复到阅读库/)).toBeInTheDocument()
    expect(screen.getByText(/恢复到收件箱/)).toBeInTheDocument()
    expect(screen.getByText(/恢复到所属文章/)).toBeInTheDocument()
  })

  // 三类的恢复走三个不同的后端路由。派发错了会静默地什么都不恢复，
  // 或者更糟——恢复了另一种东西。
  it('restores each kind through its own endpoint', async () => {
    const restoreLink = vi.fn().mockResolvedValue(ok({ host_kind: 'link', host_id: 'l', state: 'live', changed: true }))
    const restoreNote = vi.fn().mockResolvedValue(ok({ host_kind: 'note', host_id: 'n', state: 'live', changed: true }))
    const restoreInbox = vi.fn().mockResolvedValue(ok(true))
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(ok({
        count: 3,
        items: [
          item({ host_id: 'link-1', title: '链接项' }),
          item({ host_kind: 'inbox', host_id: 'inbox-1', title: '收件箱项' }),
          item({ host_kind: 'note', host_id: 'note-1', title: '笔记项', url: null }),
        ],
      })),
      restoreLink,
      restoreNote,
      restoreInbox,
    })

    await screen.findByText('链接项')
    const buttons = screen.getAllByRole('button', { name: '恢复' })
    fireEvent.click(buttons[0])
    await waitFor(() => expect(restoreLink).toHaveBeenCalledWith('link-1'))
    fireEvent.click(screen.getAllByRole('button', { name: '恢复' })[0])
    await waitFor(() => expect(restoreInbox).toHaveBeenCalledWith('inbox-1'))
    fireEvent.click(screen.getAllByRole('button', { name: '恢复' })[0])
    await waitFor(() => expect(restoreNote).toHaveBeenCalledWith('note-1'))
  })

  // 永久清除是这条链路上唯一不可逆的动作。确认框被取消时绝不能发出请求。
  it('does not purge when the confirmation is declined', async () => {
    const purgeHost = vi.fn().mockResolvedValue(ok(true))
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(ok({ count: 1, items: [item()] })),
      purgeHost,
    })

    await screen.findByText('一篇文章')
    fireEvent.click(screen.getByRole('button', { name: /永久删除/ }))
    await waitFor(() => expect(window.confirm).toHaveBeenCalled())
    expect(purgeHost).not.toHaveBeenCalled()
  })

  it('purges through the host-specific route once confirmed', async () => {
    const purgeHost = vi.fn().mockResolvedValue(ok(true))
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(ok({ count: 1, items: [item({ host_id: 'link-9' })] })),
      purgeHost,
    })

    await screen.findByText('一篇文章')
    fireEvent.click(screen.getByRole('button', { name: /永久删除/ }))
    await waitFor(() => expect(purgeHost).toHaveBeenCalledWith('link', 'link-9', expect.any(String)))
    await waitFor(() => expect(screen.queryByText('一篇文章')).not.toBeInTheDocument())
  })

  it('filters the list by host kind', async () => {
    const listTrash = vi.fn().mockResolvedValue(ok({ count: 0, items: [] }))
    mountTrash({ listTrash })

    await waitFor(() => expect(listTrash).toHaveBeenCalledWith(expect.objectContaining({ hostKind: undefined })))
    // 限定到回收站自己的这组标签：外层导航里也有一个叫「笔记」的入口。
    const filters = screen.getByRole('tablist', { name: '回收站类型' })
    fireEvent.click(within(filters).getByRole('tab', { name: '笔记' }))
    await waitFor(() => expect(listTrash).toHaveBeenCalledWith(expect.objectContaining({ hostKind: 'note' })))
  })

  it('surfaces a load failure instead of showing an empty trash', async () => {
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(err({ kind: 'other', message: '后端不可达' })),
    })

    await screen.findByText('无法读取回收站')
    expect(screen.getByText('后端不可达')).toBeInTheDocument()
    expect(screen.queryByText('回收站为空')).not.toBeInTheDocument()
  })
})
