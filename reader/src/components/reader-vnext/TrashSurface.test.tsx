import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok } from '../../lib/api/result'
import type { ReaderTrashItemResponse } from '../../lib/api/types'
import { SURFACE_IDENTITY_ERROR } from '../../lib/reader-surface'
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
  const rendered = render(
    <TrashSurface
      client={client as IdentityBoundReaderClient}
      onNavigate={vi.fn()}
      capabilityPolicy={enabledReaderCapabilityPolicy()}
      onToast={onToast}
    />,
  )
  return { onToast, unmount: rendered.unmount }
}

/** 手动控制回包时机的请求：用来把「等待期间世界变了」摆到 await 中间。 */
function deferred<T>() {
  let settle!: (value: T) => void
  const promise = new Promise<T>((resolve) => { settle = resolve })
  return {
    promise,
    resolve: async (value: T) => { await act(async () => { settle(value) }) },
  }
}

type TrashListResult = Awaited<ReturnType<IdentityBoundReaderClient['listTrash']>>
type RestoreLinkResult = Awaited<ReturnType<IdentityBoundReaderClient['restoreLink']>>

function deferredListTrash() {
  const pending = deferred<TrashListResult>()
  return { listTrash: vi.fn(() => pending.promise), resolve: pending.resolve }
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

  // 抛出物过去没人接：listTrash 一 reject，loading 就永远停在 true，界面卡在
  // 「加载中」且没有任何说明。现在它必须变成一条可重试的错误。
  it('turns a thrown listTrash into a retryable message instead of a stuck spinner', async () => {
    mountTrash({ listTrash: vi.fn().mockRejectedValue({ message: '后端炸了' }) })

    await screen.findByText('无法读取回收站')
    expect(screen.getByText('后端炸了')).toBeInTheDocument()
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeEnabled()
  })

  // 卸载后到达的回包不该再往已经消失的界面上写东西。React 18 会静默丢弃对已卸载
  // 树的更新，所以这条只能证明「不炸、不复活」；闸门自身的卸载语义由
  // useSurfaceRequestGate.test.tsx 的 unmount 用例守着。
  it('drops a listTrash response that arrives after the surface unmounts', async () => {
    const { listTrash, resolve } = deferredListTrash()
    const { unmount } = mountTrash({ listTrash })

    await waitFor(() => expect(listTrash).toHaveBeenCalled())
    unmount()
    await resolve(ok({ count: 1, items: [item()] }))

    expect(screen.queryByText('一篇文章')).not.toBeInTheDocument()
  })

  // 身份在等待期间被撤销：旧命名空间的回收站内容绝不能画到新身份上，同时必须
  // 说明为什么空着，并且把加载态收掉。
  it('refuses to paint trash fetched under an identity that was revoked while waiting', async () => {
    let identityCurrent = true
    const { listTrash, resolve } = deferredListTrash()
    mountTrash({ listTrash, isIdentityCurrent: vi.fn(() => identityCurrent) })

    await waitFor(() => expect(listTrash).toHaveBeenCalled())
    identityCurrent = false
    await resolve(ok({ count: 1, items: [item()] }))

    expect(await screen.findByText(SURFACE_IDENTITY_ERROR)).toBeInTheDocument()
    expect(screen.queryByText('一篇文章')).not.toBeInTheDocument()
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
  })

  // client 只在请求前检查过一次，所以换了 client、而新 client 又立刻因为身份失效
  // 而不发请求时，旧 client 的回包会落到新 client 的界面上。owner 让它落不下来。
  it('drops a response from the client it was replaced with while in flight', async () => {
    const first = deferredListTrash()
    const replacement = {
      listTrash: vi.fn().mockResolvedValue(ok({ count: 0, items: [] })),
      isIdentityCurrent: vi.fn(() => false),
    }

    function SwappableTrash() {
      const [client, setClient] = useState<Partial<IdentityBoundReaderClient>>({ listTrash: first.listTrash })
      return (
        <>
          <button type="button" onClick={() => setClient(replacement)}>换 client</button>
          <TrashSurface
            client={client as IdentityBoundReaderClient}
            onNavigate={vi.fn()}
            capabilityPolicy={enabledReaderCapabilityPolicy()}
            onToast={vi.fn()}
          />
        </>
      )
    }
    render(<SwappableTrash />)

    await waitFor(() => expect(first.listTrash).toHaveBeenCalled())
    fireEvent.click(screen.getByRole('button', { name: '换 client' }))
    expect(await screen.findByText(SURFACE_IDENTITY_ERROR)).toBeInTheDocument()

    await first.resolve(ok({ count: 1, items: [item()] }))

    expect(screen.queryByText('一篇文章')).not.toBeInTheDocument()
    expect(screen.getByText(SURFACE_IDENTITY_ERROR)).toBeInTheDocument()
    expect(replacement.listTrash).not.toHaveBeenCalled()
  })

  // 同一轮事件里的两次点击：state 还来不及把按钮禁用，只有单飞 token 拦得住。
  it('restores once for a double click on the same row', async () => {
    const pendingRestore = deferred<RestoreLinkResult>()
    const restoreLink = vi.fn(() => pendingRestore.promise)
    mountTrash({
      listTrash: vi.fn().mockResolvedValue(ok({ count: 1, items: [item({ host_id: 'link-1' })] })),
      restoreLink,
    })

    await screen.findByText('一篇文章')
    const button = screen.getByRole('button', { name: '恢复' })
    act(() => {
      fireEvent.click(button)
      fireEvent.click(button)
    })

    expect(restoreLink).toHaveBeenCalledTimes(1)
    await pendingRestore.resolve(ok({ host_kind: 'link', host_id: 'link-1', state: 'live', changed: true }))
    expect(screen.queryByText('一篇文章')).not.toBeInTheDocument()
  })
})
