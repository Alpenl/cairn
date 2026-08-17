import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { AddLinkDialog } from './AddLinkDialog'
import type { ReaderClient } from '../lib/api/client'
import { err, ok, type ApiResult } from '../lib/api/result'
import type { SubmitResponse } from '../lib/api/types'
import {
  enabledReaderCapabilityLease,
} from '../test/capabilities'

function renderDialog(
  response: ApiResult<SubmitResponse> | Promise<ApiResult<SubmitResponse>> = ok<SubmitResponse>({
      link_id: '11111111-1111-1111-1111-111111111111',
      job_id: '22222222-2222-2222-2222-222222222222',
      status: 'pending' as const,
    }),
  destination: 'inbox' | 'library' = 'library',
  isCurrent: () => boolean = () => true,
) {
  const submitLink = vi.fn(async () => response)
  const onAdded = vi.fn()
  const onToast = vi.fn()
  const onClose = vi.fn()
  render(
    <AddLinkDialog
      client={{ submitLink, isIdentityCurrent: vi.fn(isCurrent) } as unknown as ReaderClient}
      capabilityLease={enabledReaderCapabilityLease()}
      destination={destination}
      onClose={onClose}
      onAdded={onAdded}
      onToast={onToast}
    />,
  )
  return { submitLink, onAdded, onToast, onClose }
}

function backdrop(): HTMLElement {
  const element = document.querySelector<HTMLElement>('.reader-dialog-backdrop')
  if (!element) throw new Error('backdrop 未渲染')
  return element
}

describe('AddLinkDialog', () => {
  it('rejects non-HTTP URLs without calling the API', () => {
    const { submitLink } = renderDialog()
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'ftp://example.com/file' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    expect(screen.getByRole('alert')).toHaveTextContent('请输入有效的 http 或 https 链接')
    expect(submitLink).not.toHaveBeenCalled()
  })

  it('submits a trimmed URL through the production API contract', async () => {
    const { submitLink, onAdded, onToast } = renderDialog()
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: '  https://example.com/article  ' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    await waitFor(() =>
      expect(submitLink).toHaveBeenCalledWith({
        url: 'https://example.com/article',
        requested_library_kind: 'auto',
        destination: 'library',
      }),
    )
    expect(onToast).toHaveBeenCalledWith('链接已加入资料库', 'check')
    expect(onAdded).toHaveBeenCalledWith({ kind: 'library', id: '11111111-1111-1111-1111-111111111111' })
  })

  it('uses a real Inbox identity when the current server advertises Inbox', async () => {
    const { submitLink, onAdded, onToast } = renderDialog(ok<SubmitResponse>({
      inbox_id: 'inbox-42',
      destination: 'inbox',
      status: 'pending',
    }), 'inbox')
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/inbox' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    await waitFor(() => expect(submitLink).toHaveBeenCalledWith({
      url: 'https://example.com/inbox',
      requested_library_kind: 'auto',
      destination: 'inbox',
    }))
    expect(onToast).toHaveBeenCalledWith('链接已加入收件箱', 'inbox')
    expect(onAdded).toHaveBeenCalledWith({ kind: 'inbox', id: 'inbox-42' })
  })

  it('fails closed when an Inbox response lacks its Inbox identity', async () => {
    const { onAdded, onToast } = renderDialog(ok<SubmitResponse>({
      link_id: 'library-should-not-be-used',
      destination: 'inbox',
      status: 'pending',
    }), 'inbox')
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/inbox' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    expect(await screen.findByRole('alert')).toHaveTextContent('服务端没有返回收件箱条目标识')
    expect(onAdded).not.toHaveBeenCalled()
    expect(onToast).not.toHaveBeenCalled()
  })

  it('fails closed when a library-only response has no link identity', async () => {
    const { onAdded, onToast } = renderDialog(
      ok<SubmitResponse>({
        inbox_id: 'inbox-1',
        destination: 'inbox',
        status: 'pending',
      }),
    )
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/inbox' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        '服务端没有返回资料库链接身份',
      ),
    )
    expect(onAdded).not.toHaveBeenCalled()
    expect(onToast).not.toHaveBeenCalled()
  })

  it('keeps an API failure in the dialog without navigating', async () => {
    const { onAdded, onToast } = renderDialog(err({
      kind: 'network-unreachable', message: '服务不可达',
    }), 'inbox')
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/inbox' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)

    expect(await screen.findByRole('alert')).toHaveTextContent('服务不可达')
    expect(onAdded).not.toHaveBeenCalled()
    expect(onToast).not.toHaveBeenCalled()
  })

  it('drops a successful response when the bound identity changes in flight', async () => {
    let current = true
    let resolve!: (value: ApiResult<SubmitResponse>) => void
    const response = new Promise<ApiResult<SubmitResponse>>((done) => { resolve = done })
    const { submitLink, onAdded, onToast } = renderDialog(response, 'inbox', () => current)
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/inbox' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)
    await waitFor(() => expect(submitLink).toHaveBeenCalledTimes(1))
    await act(async () => {
      current = false
      resolve(ok({ inbox_id: 'stale-inbox', destination: 'inbox', status: 'pending' }))
      await response
    })

    expect(onAdded).not.toHaveBeenCalled()
    expect(onToast).not.toHaveBeenCalled()
  })
})

describe('AddLinkDialog 外壳', () => {
  it('渲染带名字的 Dialog，并把初始焦点放在网址输入框而不是关闭按钮', () => {
    renderDialog()

    expect(screen.getByRole('dialog', { name: '添加链接' })).toBeInTheDocument()
    expect(document.activeElement).toBe(screen.getByPlaceholderText('粘贴 https:// 链接，回车提交'))
  })

  it('空闲时 Escape、backdrop 和关闭按钮都关闭对话框', () => {
    const { onClose } = renderDialog()

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)

    fireEvent.mouseDown(backdrop())
    expect(onClose).toHaveBeenCalledTimes(2)

    fireEvent.mouseDown(screen.getByRole('dialog'))
    expect(onClose).toHaveBeenCalledTimes(2)

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(onClose).toHaveBeenCalledTimes(3)
  })

  it('提交中三条关闭路径全部失效', async () => {
    let resolve!: (value: ApiResult<SubmitResponse>) => void
    const response = new Promise<ApiResult<SubmitResponse>>((done) => { resolve = done })
    const { onClose } = renderDialog(response)
    const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')

    fireEvent.change(input, { target: { value: 'https://example.com/slow' } })
    fireEvent.submit(input.closest('form') as HTMLFormElement)
    const close = await screen.findByRole('button', { name: '关闭' })
    await waitFor(() => expect(close).toBeDisabled())

    fireEvent.click(close)
    fireEvent.keyDown(document, { key: 'Escape' })
    fireEvent.mouseDown(backdrop())
    expect(onClose).not.toHaveBeenCalled()

    await act(async () => {
      resolve(ok({ link_id: 'done', status: 'pending' }))
      await response
    })
  })
})
