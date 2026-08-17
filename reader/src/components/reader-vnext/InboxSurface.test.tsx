import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok } from '../../lib/api/result'
import type { ReaderInboxResponse } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { InboxSurface } from './InboxSurface'

function inbox(overrides: Partial<ReaderInboxResponse> = {}): ReaderInboxResponse {
  return {
    id: 'inbox-1',
    url: 'https://example.com/article',
    source_kind: 'manual',
    title: '第一篇',
	body: '正文',
	note: '',
	summary: '摘要',
    suggested_tags: ['阅读'],
    proposal_signals: {},
    proposal_status: 'completed',
    tags: ['阅读'],
    category_ids: [],
    status: 'pending',
    metadata_revision: 1,
    job_id: null,
    expires_at: '2026-09-09T01:00:00Z',
    expired_at: null,
    expired: false,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

function makeClient(
  items: ReaderInboxResponse[],
  overrides: Record<string, unknown> = {},
) {
  let serverItems = [...items]
  const listInbox = vi.fn(async (params: { partition?: 'active' | 'expired' } = {}) => {
    const pendingItems = serverItems.filter((item) => item.status === 'pending')
    return ok({
      items: serverItems.filter((item) => {
        if (item.status === 'confirmed') return false
        return params.partition === 'expired' ? item.expired : !item.expired
      }),
      next_cursor: undefined,
      active_count: pendingItems.filter((item) => !item.expired).length,
      expired_count: pendingItems.filter((item) => item.expired).length,
    })
  })
  const listCategories = vi.fn(async () => ok({ items: [] }))
  const getInbox = vi.fn(async (id: string) => ok(serverItems.find((item) => item.id === id) ?? inbox({ id })))
  const patchInbox = vi.fn(async (id: string, _revision: number, request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] }) => {
    const current = serverItems.find((item) => item.id === id) ?? inbox({ id })
    const patched = {
      ...current,
      title: request.title,
      body: request.body,
      note: request.note,
      summary: request.summary,
      tags: request.tags,
      metadata_revision: current.metadata_revision + 1,
    }
    serverItems = serverItems.map((item) => item.id === id ? patched : item)
    return ok(patched)
  })
  const createInbox = vi.fn(async () => {
    const created = inbox({ id: 'created', title: '新条目' })
    serverItems = [created, ...serverItems]
    return ok(created)
  })
  const confirmInbox = vi.fn(async (id: string) => {
    serverItems = serverItems.map((item) => item.id === id ? { ...item, status: 'confirmed' } : item)
    return ok({ target_kind: 'link' as const, link_id: 'link-1', status: 'confirmed' as const })
  })
  const restoreInbox = vi.fn(async (id: string) => {
    serverItems = serverItems.map((item) => item.id === id
      ? { ...item, status: 'pending', expired: false, expired_at: null }
      : item)
    return ok(true)
  })
  const discardInbox = vi.fn(async (id: string) => {
    serverItems = serverItems.map((item) => item.id === id ? { ...item, status: 'discarded' } : item)
    return ok(true)
  })
  const confirmInboxBulk = vi.fn(async (request: { inbox_ids: string[]; expected_revisions?: Record<string, number> }) => {
    const ids = new Set(request.inbox_ids)
    serverItems = serverItems.map((item) => ids.has(item.id) ? { ...item, status: 'confirmed' } : item)
    return ok({
      atomic: true as const,
      items: request.inbox_ids.map((id) => ({ inbox_id: id, status: 'confirmed' as const, link_id: `link-${id}` })),
    })
  })
  const confirmAIProposals = vi.fn(async () => ok({ atomic: true as const, items: [], remaining_count: 0 }))
  const discardInboxBulk = vi.fn(async (request: { inbox_ids: string[] }) => {
    const ids = new Set(request.inbox_ids)
    serverItems = serverItems.map((item) => ids.has(item.id) ? { ...item, status: 'discarded' } : item)
    return ok({
      atomic: true as const,
      items: request.inbox_ids.map((id) => ({ inbox_id: id, status: 'discarded' as const })),
    })
  })
  const resummarizeInbox = vi.fn(async () => ok({ inbox_id: 'inbox-1', status: 'queued', job_id: 'job-1' }))
  const getInboxJob = vi.fn(async () => ok({ inbox_id: 'inbox-1', status: 'queued', job_id: 'job-1' }))
  const createCategory = vi.fn(async () => ok({ id: 'category-1', name: '分类', count: 0, created_at: '2026-08-10T01:00:00Z' }))
  const setCategoryMembership = vi.fn(async () => ok(true))
  const client = {
    listInbox,
    listCategories,
    getInbox,
    patchInbox,
    createInbox,
    confirmInbox,
    restoreInbox,
    discardInbox,
    confirmInboxBulk,
    confirmAIProposals,
    discardInboxBulk,
    resummarizeInbox,
    getInboxJob,
    createCategory,
    setCategoryMembership,
    isIdentityCurrent: vi.fn(() => true),
    ...overrides,
  } as unknown as IdentityBoundReaderClient
  return {
    client,
    listInbox,
    getInbox,
    patchInbox,
    createInbox,
    confirmInbox,
    restoreInbox,
    discardInbox,
    confirmInboxBulk,
    confirmAIProposals,
    discardInboxBulk,
    resummarizeInbox,
    setCategoryMembership,
  }
}

function renderInbox(client: IdentityBoundReaderClient, initialInboxID?: string) {
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  const onOpenLink = vi.fn<(id: string) => void>()
  const capabilityPolicy = enabledReaderCapabilityPolicy()
  const rendered = render(
    <InboxSurface client={client} capabilityPolicy={capabilityPolicy} onNavigate={onNavigate} onOpenLink={onOpenLink} initialInboxID={initialInboxID} />,
  )
  return { ...rendered, onNavigate, onOpenLink, capabilityPolicy }
}

afterEach(() => {
  window.history.replaceState({}, '', '/')
  vi.restoreAllMocks()
})

describe('InboxSurface', () => {
  it('renders the Inbox as a queue and reader workbench', async () => {
    const { client } = makeClient([inbox({ body: '可阅读的正文' })])

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    expect(screen.getByRole('complementary', { name: '收件箱列表' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: '条目编辑器' })).toBeInTheDocument()
    expect(screen.getByRole('complementary', { name: '条目目录' })).toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: '正文目录' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '概览' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('可阅读的正文')

    fireEvent.click(screen.getByRole('button', { name: '预览' }))
    expect(screen.queryByRole('textbox', { name: '正文' })).not.toBeInTheDocument()
    expect(document.querySelector('.inbox-body-preview')).toHaveTextContent('可阅读的正文')

    fireEvent.click(screen.getByRole('button', { name: '添加条目' }))
    expect(screen.getByRole('dialog', { name: '添加条目' })).toBeInTheDocument()
  })

  it('renders the body preview as Markdown rather than its source', async () => {
    const { client } = makeClient([inbox({ body: '## 小节标题\n\n正文一段。' })])

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '预览' }))

    // The preview shares the reader's Markdown renderer, so a heading arrives
    // as a heading. Before that it was a plain-text dump and the reader saw
    // the '##' marker itself.
    const heading = await screen.findByRole('heading', { name: '小节标题' })
    expect(heading.tagName).toBe('H2')
    expect(document.querySelector('.inbox-body-preview')).not.toHaveTextContent('## 小节标题')
  })

  it('labels a queue row by host and only flags rows that left the pending flow', async () => {
    const { client } = makeClient([
      inbox({ url: 'https://www.example.com/2026/08/article', source_kind: 'browser_capture' }),
      inbox({ id: 'inbox-2', title: '第二篇', url: 'https://feeds.example.org/post', status: 'discarded' }),
    ])

    renderInbox(client)

    const pending = await screen.findByRole('button', { name: /第一篇/ })
    // The host reads like the Reading list; the source kind survives as the
    // row icon's tooltip rather than a snake_cased label.
    expect(pending).toHaveTextContent('example.com')
    expect(pending).not.toHaveTextContent('browser_capture')
    // Every active row is pending, so repeating the state on each of them is
    // noise. Only rows that left the flow carry a state.
    expect(pending).not.toHaveTextContent('待处理')
    expect(screen.getByRole('button', { name: /第二篇/ })).toHaveTextContent('已丢弃')
  })

  it('shows loading before an empty Inbox state', async () => {
    let resolveList!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn(() => new Promise((resolve) => { resolveList = resolve }))
    const { client } = makeClient([], { listInbox })

    renderInbox(client)

    expect(screen.getByText('加载中')).toBeInTheDocument()
    resolveList(ok({ items: [], next_cursor: undefined, active_count: 0, expired_count: 0 }))
    expect(await screen.findByText('收件箱是空的')).toBeInTheDocument()
  })

  it('keeps list errors separate from the empty state', async () => {
    const listInbox = vi.fn(async () => err({ kind: 'network-unreachable' as const, message: '服务不可达' }))
    const { client } = makeClient([], { listInbox })

    renderInbox(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('服务不可达')
    expect(screen.queryByText('没有收件箱内容')).not.toBeInTheDocument()
  })

  it('updates the aggregate counts when a new pending Inbox item is created', async () => {
    const created = inbox({ id: 'created', title: '新条目' })
    let createdOnServer = false
    const listInbox = vi.fn(async () => ok({
      items: createdOnServer ? [created] : [],
      next_cursor: undefined,
      active_count: createdOnServer ? 3 : 2,
      expired_count: 1,
    }))
    const createInbox = vi.fn(async () => {
      createdOnServer = true
      return ok(created)
    })
    const { client } = makeClient([], { listInbox, createInbox })

    renderInbox(client)

    await screen.findByText('收件箱是空的')
    expect(screen.getByRole('tab', { name: '活跃 (2)' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '已过期 (1)' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '添加条目' }))
    fireEvent.change(screen.getByRole('textbox', { name: '网址' }), {
      target: { value: 'https://example.com/new' },
    })
    fireEvent.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(createInbox).toHaveBeenCalledTimes(1))
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: '活跃 (3)' })).toBeInTheDocument()
      expect(screen.getByRole('tab', { name: '已过期 (1)' })).toBeInTheDocument()
    })
  })

  it('evicts an active cache row when an authoritative expired page rehomes its ID', async () => {
    const active = inbox({ id: 'shared', title: '活跃版本' })
    const expired = inbox({
      id: 'shared',
      title: '过期版本',
      expired: true,
      expired_at: '2026-08-11T01:00:00Z',
    })
    const listInbox = vi.fn(async (params: { partition?: 'active' | 'expired' }) => ok(
      params.partition === 'expired'
        ? { items: [expired], next_cursor: undefined, active_count: 0, expired_count: 1 }
        : { items: [active], next_cursor: undefined, active_count: 1, expired_count: 0 },
    ))
    const { client, confirmInbox } = makeClient([], { listInbox })

    renderInbox(client)

    await screen.findByRole('heading', { name: '活跃版本' })
    fireEvent.click(screen.getByRole('tab', { name: '已过期 (0)' }))
    await screen.findByRole('heading', { name: '过期版本' })
    fireEvent.click(screen.getByRole('tab', { name: '活跃 (0)' }))

    expect(await screen.findByText('收件箱是空的')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /活跃版本/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '确认入库' })).not.toBeInTheDocument()
    expect(confirmInbox).not.toHaveBeenCalled()
    expect(screen.getByRole('tab', { name: '活跃 (0)' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '已过期 (1)' })).toBeInTheDocument()
  })

  it('does not let an older active response overwrite newer expired aggregate counts', async () => {
    let resolveActive!: (value: ReturnType<typeof ok>) => void
    let resolveExpired!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn((params: { partition?: 'active' | 'expired' }) => new Promise<ReturnType<typeof ok>>((resolve) => {
      if (params.partition === 'expired') resolveExpired = resolve
      else resolveActive = resolve
    }))
    const { client } = makeClient([], { listInbox })

    renderInbox(client)

    await waitFor(() => expect(listInbox).toHaveBeenCalledWith({
      partition: 'active',
      after: undefined,
      limit: 50,
    }))
    fireEvent.click(screen.getByRole('tab', { name: '已过期 (0)' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledWith({
      partition: 'expired',
      after: undefined,
      limit: 50,
    }))
    await act(async () => {
      resolveExpired(ok({ items: [], next_cursor: undefined, active_count: 3, expired_count: 2 }))
    })
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: '活跃 (3)' })).toBeInTheDocument()
      expect(screen.getByRole('tab', { name: '已过期 (2)' })).toBeInTheDocument()
    })
    await act(async () => {
      resolveActive(ok({ items: [], next_cursor: undefined, active_count: 1, expired_count: 0 }))
    })

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: '活跃 (3)' })).toBeInTheDocument()
      expect(screen.getByRole('tab', { name: '已过期 (2)' })).toBeInTheDocument()
    })
  })

  it.each([
    ['确认', '确认入库'],
    ['丢弃', '丢弃'],
  ])('updates the active count after individual %s', async (_action, buttonName) => {
    const active = inbox()
    const expired = inbox({
      id: 'expired-1',
      title: '已过期文章',
      expired_at: '2026-08-11T01:00:00Z',
      expired: true,
    })
    let updatedOnServer = false
    const listInbox = vi.fn(async (params: { partition?: 'active' | 'expired' }) => ok(
      params.partition === 'expired'
        ? { items: [expired], next_cursor: undefined, active_count: 1, expired_count: 1 }
        : {
            items: updatedOnServer ? [] : [active],
            next_cursor: undefined,
            active_count: updatedOnServer ? 0 : 1,
            expired_count: 1,
          },
    ))
    const confirmInbox = vi.fn(async () => {
      updatedOnServer = true
      return ok({ target_kind: 'link' as const, link_id: 'link-1', status: 'confirmed' as const })
    })
    const discardInbox = vi.fn(async () => {
      updatedOnServer = true
      return ok(true)
    })
    const { client } = makeClient([active], { listInbox, confirmInbox, discardInbox })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: buttonName }))

    await waitFor(() => expect(screen.getByRole('tab', { name: '活跃 (0)' })).toBeInTheDocument())
    expect(screen.getByRole('tab', { name: '已过期 (1)' })).toBeInTheDocument()
  })

  it.each([
    ['确认', '确认所选'],
    ['丢弃', '批量丢弃'],
  ])('updates the expired count after bulk %s', async (_action, buttonName) => {
    const active = inbox()
    const firstExpired = inbox({
      id: 'expired-1',
      title: '已过期文章一',
      expired_at: '2026-08-11T01:00:00Z',
      expired: true,
    })
    const secondExpired = inbox({
      id: 'expired-2',
      title: '已过期文章二',
      url: 'https://example.com/expired-two',
      expired_at: '2026-08-11T01:00:00Z',
      expired: true,
    })
    let updatedOnServer = false
    const listInbox = vi.fn(async (params: { partition?: 'active' | 'expired' }) => ok(
      params.partition === 'expired'
        ? {
            items: updatedOnServer ? [] : [firstExpired, secondExpired],
            next_cursor: undefined,
            active_count: 1,
            expired_count: updatedOnServer ? 0 : 2,
          }
        : { items: [active], next_cursor: undefined, active_count: 1, expired_count: 2 },
    ))
    const confirmInboxBulk = vi.fn(async (request: { inbox_ids: string[] }) => {
      updatedOnServer = true
      return ok({
        atomic: true as const,
        items: request.inbox_ids.map((id) => ({ inbox_id: id, status: 'confirmed' as const, link_id: `link-${id}` })),
      })
    })
    const discardInboxBulk = vi.fn(async (request: { inbox_ids: string[] }) => {
      updatedOnServer = true
      return ok({
        atomic: true as const,
        items: request.inbox_ids.map((id) => ({ inbox_id: id, status: 'discarded' as const })),
      })
    })
    const { client } = makeClient([active], { listInbox, confirmInboxBulk, discardInboxBulk })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('tab', { name: '已过期 (2)' }))
    await screen.findByRole('heading', { name: '已过期文章一' })
    fireEvent.click(screen.getByRole('checkbox', { name: '全选' }))
    fireEvent.click(screen.getByRole('button', { name: buttonName }))

    await waitFor(() => expect(screen.getByRole('tab', { name: '已过期 (0)' })).toBeInTheDocument())
    expect(screen.getByRole('tab', { name: '活跃 (1)' })).toBeInTheDocument()
  })

  it('selects several pending items and displays the atomic confirm result', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    const { client, patchInbox, confirmInboxBulk } = makeClient([first, second])
    const { onOpenLink } = renderInbox(client)

    expect(await screen.findByRole('heading', { name: '第一篇' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('checkbox', { name: '全选' }))
    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))

    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.any(Object)))
    await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledWith({
      inbox_ids: ['inbox-1', 'inbox-2'],
      expected_revisions: { 'inbox-1': 2, 'inbox-2': 1 },
    }))
    expect(await screen.findByText('批量确认完成')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '打开已确认链接 link-inbox-1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '打开已确认链接 link-inbox-2' })).toBeInTheDocument()
    expect(onOpenLink).not.toHaveBeenCalled()
  })

  it('does not bulk-confirm an item whose effective title is trimmed empty', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    const { client, confirmInboxBulk } = makeClient([first, second])
    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('checkbox', { name: '全选' }))
    fireEvent.change(screen.getByRole('textbox', { name: '标题' }), { target: { value: '   ' } })

    expect(screen.getByRole('button', { name: '确认所选' })).toBeDisabled()
    expect(confirmInboxBulk).not.toHaveBeenCalled()
  })

  it('does not change any row when an atomic bulk operation fails', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    const confirmInboxBulk = vi.fn(async () => err({ kind: 'other' as const, message: '批量失败', status: 409 }))
    const { client } = makeClient([first, second], { confirmInboxBulk })

    renderInbox(client)

    expect(await screen.findByRole('heading', { name: '第一篇' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('checkbox', { name: '全选' }))
    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))

    await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledTimes(1))
    expect(await screen.findByRole('alert')).toHaveTextContent('内容已经被其他窗口更新，请刷新后重试。')
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '第一篇' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /第二篇/ })).toBeInTheDocument()
    expect(screen.queryByText('批量确认完成')).not.toBeInTheDocument()
  })

  it('confirms all AI-ready proposals through server-owned active batches', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    let confirmedOnServer = false
    const listInbox = vi.fn(async () => ok({
      items: confirmedOnServer ? [] : [first, second],
      next_cursor: undefined,
      active_count: confirmedOnServer ? 0 : 2,
      expired_count: 0,
    }))
    const confirmAIProposals = vi.fn()
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'inbox-1', status: 'confirmed' as const, link_id: 'link-inbox-1' }],
        remaining_count: 1,
      }))
      .mockImplementationOnce(async () => {
        confirmedOnServer = true
        return ok({
          atomic: true as const,
          items: [{ inbox_id: 'inbox-2', status: 'confirmed' as const, link_id: 'link-inbox-2' }],
          remaining_count: 0,
        })
      })
    const { client, confirmInboxBulk } = makeClient([first, second], { confirmAIProposals, listInbox })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(confirmAIProposals).toHaveBeenCalledTimes(2))
    expect(confirmAIProposals).toHaveBeenNthCalledWith(1, { partition: 'active' })
    expect(confirmAIProposals).toHaveBeenNthCalledWith(2, { partition: 'active' })
    expect(confirmInboxBulk).not.toHaveBeenCalled()
    expect(await screen.findByText('AI 建议确认已完成')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('tab', { name: '活跃 (0)' })).toBeInTheDocument())
  })

  it('refreshes the authoritative Inbox and preserves a later AI batch error', async () => {
    const first = inbox()
    const remaining = inbox({ id: 'inbox-2', title: '仍待确认文章', url: 'https://example.com/remaining' })
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [first], next_cursor: undefined, active_count: 2, expired_count: 0 }))
      .mockResolvedValueOnce(ok({ items: [remaining], next_cursor: undefined, active_count: 1, expired_count: 0 }))
    const confirmAIProposals = vi.fn()
      .mockResolvedValueOnce(ok({
        atomic: true as const,
        items: [{ inbox_id: 'inbox-1', status: 'confirmed' as const, link_id: 'link-inbox-1' }],
        remaining_count: 1,
      }))
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '第二批确认失败', status: 500 }))
    const { client } = makeClient([first], { listInbox, confirmAIProposals })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '确认全部 AI 建议' }))

    await waitFor(() => expect(confirmAIProposals).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    expect(listInbox).toHaveBeenLastCalledWith({ partition: 'active', after: undefined, limit: 50 })
    expect(await screen.findByRole('alert')).toHaveTextContent('第二批确认失败')
    expect(screen.getByRole('heading', { name: '仍待确认文章' })).toBeInTheDocument()
  })

  it('keeps active and expired pages separate and makes expired items recoverable', async () => {
    const active = inbox()
    const expired = inbox({
      id: 'expired-1',
      title: '已过期文章',
      expired_at: '2026-08-11T01:00:00Z',
      expired: true,
    })
    const restored = inbox({
      id: 'expired-1',
      title: '已恢复文章',
      expires_at: '2026-09-10T01:00:00Z',
    })
    let restoredExpiredItem = false
    let resolveExpiredAppend!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn((params: { partition?: 'active' | 'expired'; after?: string }) => {
      if (params.partition === 'expired') {
        if (params.after === 'expired-next') {
          return new Promise<ReturnType<typeof ok>>((resolve) => {
            resolveExpiredAppend = resolve
          })
        }
        return Promise.resolve(ok({
          items: restoredExpiredItem ? [] : [expired],
          next_cursor: restoredExpiredItem ? undefined : 'expired-next',
          active_count: restoredExpiredItem ? 2 : 1,
          expired_count: restoredExpiredItem ? 0 : 1,
        }))
      }
      return Promise.resolve(ok({
        items: restoredExpiredItem ? [restored, active] : [active],
        next_cursor: undefined,
        active_count: restoredExpiredItem ? 2 : 1,
        expired_count: restoredExpiredItem ? 0 : 1,
      }))
    })
    const restoreInbox = vi.fn(async () => {
      restoredExpiredItem = true
      return ok(true)
    })
    const { client } = makeClient([active], { listInbox, restoreInbox })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('tab', { name: '已过期 (1)' }))
    expect(await screen.findByRole('heading', { name: '已过期文章' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复有效期' })).toBeInTheDocument()
    expect(listInbox).toHaveBeenLastCalledWith({ partition: 'expired', after: undefined, limit: 50 })
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenLastCalledWith({
      partition: 'expired',
      after: 'expired-next',
      limit: 50,
    }))

    fireEvent.click(screen.getByRole('button', { name: '恢复有效期' }))
    await waitFor(() => expect(restoreInbox).toHaveBeenCalledWith('expired-1'))
    await waitFor(() => expect(screen.getByRole('tab', { name: '活跃 (2)' })).toBeInTheDocument())
    expect(screen.getByRole('tab', { name: '已过期 (0)' })).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /已恢复文章/ })).toBeInTheDocument()
    await act(async () => {
      resolveExpiredAppend(ok({
        items: [expired],
        next_cursor: undefined,
        active_count: 1,
        expired_count: 1,
      }))
    })

    fireEvent.click(screen.getByRole('tab', { name: '已过期 (0)' }))
    expect(screen.queryByRole('button', { name: /已过期文章/ })).not.toBeInTheDocument()
    expect(await screen.findByText('没有已过期内容')).toBeInTheDocument()
    expect(listInbox).toHaveBeenLastCalledWith({ partition: 'expired', after: undefined, limit: 50 })
  })

  it('keeps discarded items addressable and restores them to pending', async () => {
    const item = inbox()
    const { client, discardInbox, restoreInbox } = makeClient([item])

    renderInbox(client)

    expect(await screen.findByRole('heading', { name: '第一篇' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '丢弃' }))
    await waitFor(() => expect(discardInbox).toHaveBeenCalledWith('inbox-1'))
    expect(await screen.findByRole('button', { name: '恢复到收件箱' })).toBeInTheDocument()
    expect(screen.getAllByText('已丢弃').length).toBeGreaterThan(0)

    fireEvent.click(screen.getByRole('button', { name: '恢复到收件箱' }))
    await waitFor(() => expect(restoreInbox).toHaveBeenCalledWith('inbox-1'))
    expect(screen.getAllByText('收件箱').length).toBeGreaterThan(0)
  })

  it('does not revive a discarded row from a completed job refresh started before the mutation', async () => {
    const item = inbox({ job_id: 'job-1' })
    let resolveRefresh!: (value: ReturnType<typeof ok>) => void
    const getInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveRefresh = resolve }))
    const getInboxJob = vi.fn(async () => ok({ inbox_id: item.id, status: 'completed' as const, job_id: 'job-1' }))
    const { client, discardInbox } = makeClient([item], { getInbox, getInboxJob })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-1'))
    fireEvent.click(screen.getByRole('button', { name: '丢弃' }))
    await waitFor(() => expect(discardInbox).toHaveBeenCalledWith('inbox-1'))
    expect(await screen.findByRole('button', { name: '恢复到收件箱' })).toBeInTheDocument()

    await act(async () => {
      resolveRefresh(ok(item))
    })

    expect(screen.getByRole('button', { name: '恢复到收件箱' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '确认入库' })).not.toBeInTheDocument()
  })

  it('loads a discarded deep-link target so it has a recovery entry point', async () => {
    const discarded = inbox({ id: 'discarded-1', title: '已丢弃文章', status: 'discarded' })
    const getInbox = vi.fn(async () => ok(discarded))
    const { client } = makeClient([], { getInbox })

    renderInbox(client, 'discarded-1')

    expect(await screen.findByRole('heading', { name: '已丢弃文章' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复到收件箱' })).toBeInTheDocument()
    expect(getInbox).toHaveBeenCalledWith('discarded-1')
  })

  it('releases an empty deep-link target loader after identity revocation without applying its response', async () => {
    let identityCurrent = true
    let resolveTarget!: (value: ReturnType<typeof ok>) => void
    const getInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveTarget = resolve }))
    const { client } = makeClient([], {
      getInbox,
      isIdentityCurrent: () => identityCurrent,
    })

    renderInbox(client, 'missing-target')

    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('missing-target'))
    expect(screen.getByText('加载中')).toBeInTheDocument()

    identityCurrent = false
    await act(async () => {
      resolveTarget(ok(inbox({ id: 'missing-target', title: '迟到目标' })))
    })

    expect(await screen.findByText('收件箱是空的')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '迟到目标' })).not.toBeInTheDocument()
  })

  it('rejects an older load scope even when its first request number is reused', async () => {
    const staleItem = inbox({ id: 'inbox-a', title: '旧列表项' })
    const currentItem = inbox({ id: 'inbox-b', title: '新列表项', url: 'https://example.com/b' })
    let resolveStaleInbox!: (value: ReturnType<typeof ok>) => void
    let resolveCurrentInbox!: (value: ReturnType<typeof ok>) => void
    const listStaleInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveStaleInbox = resolve }))
    const listCurrentInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveCurrentInbox = resolve }))
    const staleClient = makeClient([], { listInbox: listStaleInbox })
    const currentClient = makeClient([], { listInbox: listCurrentInbox })
    const rendered = renderInbox(staleClient.client)

    await waitFor(() => expect(listStaleInbox).toHaveBeenCalledTimes(1))
    rendered.rerender(
      <InboxSurface
        client={currentClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )
    await waitFor(() => expect(listCurrentInbox).toHaveBeenCalledTimes(1))

    await act(async () => {
      resolveStaleInbox(ok({ items: [staleItem], next_cursor: undefined }))
    })

    expect(screen.queryByRole('button', { name: /旧列表项/ })).not.toBeInTheDocument()
    await act(async () => {
      resolveCurrentInbox(ok({ items: [currentItem], next_cursor: undefined }))
    })
    expect(await screen.findByRole('heading', { name: '新列表项' })).toBeInTheDocument()
  })

  it('ignores a stale same-identity deep-link target after the target changes', async () => {
    const first = inbox({ id: 'inbox-a', title: '旧目标' })
    const second = inbox({ id: 'inbox-b', title: '新目标', url: 'https://example.com/b' })
    const resolvers = new Map<string, (value: ReturnType<typeof ok>) => void>()
    const getInbox = vi.fn((id: string) => new Promise<ReturnType<typeof ok>>((resolve) => {
      resolvers.set(id, resolve)
    }))
    const { client } = makeClient([], { getInbox })
    const rendered = renderInbox(client, 'inbox-a')

    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-a'))
    rendered.rerender(
      <InboxSurface
        client={client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-b"
      />,
    )
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-b'))
    await act(async () => {
      resolvers.get('inbox-b')?.(ok(second))
    })

    expect(await screen.findByRole('heading', { name: '新目标' })).toBeInTheDocument()
    await act(async () => {
      resolvers.get('inbox-a')?.(ok(first))
    })

    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('新目标')
    expect(screen.queryByRole('button', { name: /旧目标/ })).not.toBeInTheDocument()
  })

  it('does not let a delayed metadata save invalidate a newer deep-link load', async () => {
    const first = inbox({ id: 'inbox-a', title: '旧目标' })
    const second = inbox({ id: 'inbox-b', title: '新目标', url: 'https://example.com/b' })
    const delayedSave = inbox({ id: 'inbox-a', title: '过期保存结果', metadata_revision: 2 })
    let resolvePatch!: (value: ReturnType<typeof ok>) => void
    let resolveReload!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [first], next_cursor: undefined }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveReload = resolve }))
    const patchInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolvePatch = resolve }))
    const getInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>(() => {}))
    const { client } = makeClient([first], { listInbox, patchInbox, getInbox })
    const rendered = renderInbox(client, 'inbox-a')

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '本地 A 草稿' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-a', 1, expect.any(Object)))

    rendered.rerender(
      <InboxSurface
        client={client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-b"
      />,
    )
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))

    await act(async () => {
      resolvePatch(ok(delayedSave))
    })
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('本地 A 草稿')

    await act(async () => {
      resolveReload(ok({ items: [second], next_cursor: undefined }))
    })

    expect(await screen.findByRole('heading', { name: '新目标' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('新目标')
  })

  it('rejects an earlier same-scope list request before its retry succeeds', async () => {
    const initial = inbox()
    const staleAppend = inbox({ id: 'stale-append', title: '过期追加项', url: 'https://example.com/stale' })
    const retryItem = inbox({ id: 'retry-current', title: '重试列表项', url: 'https://example.com/retry' })
    const category = { id: 'category-1', name: '研究', count: 0, created_at: '2026-08-10T01:00:00Z' }
    let resolveAppend!: (value: ReturnType<typeof ok>) => void
    let resolveRetry!: (value: ReturnType<typeof ok>) => void
    let resolveMembership!: (value: unknown) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [initial], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveAppend = resolve }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveRetry = resolve }))
    const setCategoryMembership = vi.fn(() => new Promise((resolve) => { resolveMembership = resolve }))
    const { client } = makeClient([initial], {
      listInbox,
      listCategories: vi.fn(async () => ok({ items: [category] })),
      setCategoryMembership,
    })

    renderInbox(client)

    await screen.findByRole('button', { name: '研究' })
    fireEvent.click(screen.getByRole('button', { name: '研究' }))
    await waitFor(() => expect(setCategoryMembership).toHaveBeenCalledTimes(1))
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))

    await act(async () => {
      resolveMembership(err({ kind: 'other' as const, message: '分类失败' }))
    })
    fireEvent.click(await screen.findByRole('button', { name: '重试' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(3))

    await act(async () => {
      resolveAppend(ok({ items: [staleAppend], next_cursor: undefined }))
    })

    expect(screen.queryByRole('button', { name: /过期追加项/ })).not.toBeInTheDocument()
    await act(async () => {
      resolveRetry(ok({ items: [retryItem], next_cursor: undefined }))
    })
    expect(await screen.findByRole('heading', { name: '重试列表项' })).toBeInTheDocument()
  })

  it('does not restore a bulk-confirmed row from a page started before the mutation', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    let resolveAppend!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [first, second], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveAppend = resolve }))
      .mockResolvedValue(ok({ items: [], next_cursor: undefined, active_count: 0, expired_count: 0 }))
    const { client, confirmInboxBulk } = makeClient([first, second], { listInbox })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 第二篇' }))
    await waitFor(() => expect(screen.getByText('已选择 1 项')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))
    await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledWith({
      inbox_ids: ['inbox-2'],
      expected_revisions: { 'inbox-2': 1 },
    }))
    await waitFor(() => expect(screen.queryByRole('button', { name: /第二篇/ })).not.toBeInTheDocument())

    await act(async () => {
      resolveAppend(ok({ items: [second], next_cursor: undefined }))
    })

    expect(screen.queryByRole('button', { name: /第二篇/ })).not.toBeInTheDocument()
  })

  it('releases its append loader when an identity-invalidated response returns early', async () => {
    const item = inbox()
    let identityCurrent = true
    let resolveAppend!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [item], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveAppend = resolve }))
    const { client } = makeClient([item], {
      listInbox,
      isIdentityCurrent: () => identityCurrent,
    })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('button', { name: '加载中…' })).toBeDisabled()
    identityCurrent = false

    await act(async () => {
      resolveAppend(ok({ items: [item], next_cursor: 'cursor-1' }))
    })

    expect(await screen.findByRole('button', { name: '更多' })).not.toBeDisabled()
  })

  it('keeps revision floors when a current append returns an older row', async () => {
    const initial = inbox({ metadata_revision: 1 })
    const stalePageItem = inbox({ title: '过期页面标题', metadata_revision: 1 })
    let resolveAppend!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [initial], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveAppend = resolve }))
    const patchInbox = vi.fn(async (
      id: string,
      revision: number,
      request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
    ) => ok(inbox({ id, ...request, metadata_revision: revision + 1 })))
    const { client } = makeClient([initial], { listInbox, patchInbox })

    renderInbox(client)

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '本地已保存标题' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, {
      title: '本地已保存标题',
      body: '正文',
      note: '',
      summary: '摘要',
      tags: ['阅读'],
    }))
    await waitFor(() => expect(screen.getByRole('button', { name: '保存元数据' })).not.toBeDisabled())

    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    await act(async () => {
      resolveAppend(ok({ items: [stalePageItem], next_cursor: undefined }))
    })

    expect(screen.getByRole('button', { name: /本地已保存标题/ })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('本地已保存标题')
    fireEvent.change(screen.getByRole('textbox', { name: '标题' }), { target: { value: '第二次保存标题' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 2, {
      title: '第二次保存标题',
      body: '正文',
      note: '',
      summary: '摘要',
      tags: ['阅读'],
    }))
  })

  it('sends selected IDs to bulk discard and renders discarded recovery rows', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    const { client, discardInboxBulk } = makeClient([first, second])

    renderInbox(client)

    expect(await screen.findByRole('heading', { name: '第一篇' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 第一篇' }))
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 第二篇' }))
    fireEvent.click(screen.getByRole('button', { name: '批量丢弃' }))

    await waitFor(() => expect(discardInboxBulk).toHaveBeenCalledWith({ inbox_ids: ['inbox-1', 'inbox-2'] }))
    expect(await screen.findByText('批量丢弃完成')).toBeInTheDocument()
    expect(screen.getAllByText(/已丢弃/).length).toBeGreaterThanOrEqual(2)
    expect(screen.getByRole('button', { name: '恢复到收件箱' })).toBeInTheDocument()
  })

  it('saves a dirty draft before changing selected Inbox items', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇' })
    const { client, patchInbox } = makeClient([first, second])

    renderInbox(client)
    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '本地标题' } })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, {
      title: '本地标题',
      body: '正文',
      note: '',
      summary: '摘要',
      tags: ['阅读'],
    }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇'))
  })

  it('keeps a conflicting draft, refreshes its revision, and succeeds on retry', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇' })
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockImplementationOnce(async (
        id: string,
        _revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => ok(inbox({ id, ...request, metadata_revision: 6 })))
    const getInbox = vi.fn(async () => ok(inbox({
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
      metadata_revision: 5,
    })))
    const { client } = makeClient([first, second], { patchInbox, getInbox })

    renderInbox(client)
    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '保留草稿' } })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    expect(await screen.findByRole('alert')).toHaveTextContent('已保留本地草稿并同步最新版本')
    expect(screen.getByRole('status')).toHaveTextContent('下次保存时使用最新版本')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('保留草稿')
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('远端正文')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('远端备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('远端摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('远端标签')
    expect(screen.getByRole('button', { name: /远端标题/ }).className).toContain('active')

    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 5, {
      title: '保留草稿',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
    }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇'))
  })

  it('keeps a newer retried list revision when a delayed CAS refresh returns older data', async () => {
    const initial = inbox({ metadata_revision: 1 })
    const listRevision = inbox({
      title: '列表版本三',
      body: '列表版本三正文',
      note: '列表版本三笔记',
      summary: '列表版本三摘要',
      tags: ['版本三'],
      metadata_revision: 3,
    })
    const staleConflictRefresh = inbox({
      title: '冲突版本二',
      body: '冲突版本二正文',
      note: '冲突版本二笔记',
      summary: '冲突版本二摘要',
      tags: ['版本二'],
      metadata_revision: 2,
    })
    let resolveAppend!: (value: unknown) => void
    let resolveConflictRefresh!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [initial], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise((resolve) => { resolveAppend = resolve }))
      .mockResolvedValueOnce(ok({ items: [listRevision], next_cursor: undefined }))
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockImplementationOnce(async (
        id: string,
        _revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => ok(inbox({ id, ...request, metadata_revision: 4 })))
    const getInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConflictRefresh = resolve }))
    const { client } = makeClient([initial], { listInbox, patchInbox, getInbox })

    renderInbox(client)

    await screen.findByRole('textbox', { name: '标题' })
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.any(Object)))
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-1'))

    await act(async () => {
      resolveAppend(err({ kind: 'other' as const, message: '追加加载失败' }))
    })
    fireEvent.click(await screen.findByRole('button', { name: '重试' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(3))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('列表版本三'))

    await act(async () => {
      resolveConflictRefresh(ok(staleConflictRefresh))
    })

    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('列表版本三')
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 3, expect.any(Object)))
  })

  it('keeps a CAS reread ahead of a stale list that started before it', async () => {
    const initial = inbox({
      title: '初始标题',
      body: '初始正文',
      note: '初始备注',
      summary: '初始摘要',
      tags: ['初始标签'],
      metadata_revision: 1,
    })
    const stalePageItem = inbox({
      title: '过期页面标题',
      body: '过期页面正文',
      note: '过期页面备注',
      summary: '过期页面摘要',
      tags: ['过期标签'],
      metadata_revision: 1,
    })
    const refreshed = inbox({
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
      metadata_revision: 3,
    })
    let resolveAppend!: (value: ReturnType<typeof ok>) => void
    let resolveConflictRefresh!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [initial], next_cursor: 'cursor-1' }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveAppend = resolve }))
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockResolvedValueOnce(ok(inbox({ metadata_revision: 4 })))
    const getInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConflictRefresh = resolve }))
    const { client } = makeClient([initial], { listInbox, patchInbox, getInbox })

    renderInbox(client)

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))
    fireEvent.change(title, { target: { value: '本地分歧标题' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.any(Object)))
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-1'))

    await act(async () => {
      resolveAppend(ok({ items: [stalePageItem], next_cursor: undefined }))
    })
    await act(async () => {
      resolveConflictRefresh(ok(refreshed))
    })

    expect(await screen.findByRole('alert')).toHaveTextContent('已保留本地草稿并同步最新版本')
    expect(screen.getByRole('status')).toHaveTextContent('下次保存时使用最新版本')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('本地分歧标题')
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('远端正文')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('远端备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('远端摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('远端标签')

    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 3, {
      title: '本地分歧标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
    }))
  })

  it('rebases refreshed server fields before a later CAS conflict and retry', async () => {
    const original = inbox()
    const refreshed = inbox({
      title: '刷新后的标题',
      body: '刷新后的正文',
      note: '刷新后的备注',
      summary: '刷新后的摘要',
      tags: ['刷新标签'],
      metadata_revision: 2,
    })
    const conflicted = inbox({
      title: '冲突后的标题',
      body: '冲突后的正文',
      note: '冲突后的备注',
      summary: '冲突后的摘要',
      tags: ['冲突标签'],
      metadata_revision: 3,
    })
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockImplementationOnce(async (
        id: string,
        _revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => ok({ ...conflicted, id, ...request, metadata_revision: 4 }))
    const getInbox = vi.fn(async () => ok(conflicted))
    const initialClient = makeClient([original])
    const refreshedClient = makeClient([refreshed], { patchInbox, getInbox })
    const rendered = renderInbox(initialClient.client)

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '本地标题' } })
    rendered.rerender(
      <InboxSurface
        client={refreshedClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )

    await waitFor(() => expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('刷新后的正文'))
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('本地标题')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('刷新后的备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('刷新后的摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('刷新标签')

    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 2, {
      title: '本地标题',
      body: '刷新后的正文',
      note: '刷新后的备注',
      summary: '刷新后的摘要',
      tags: ['刷新标签'],
    }))
    expect(await screen.findByRole('status')).toHaveTextContent('下次保存时使用最新版本')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('本地标题')
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('冲突后的正文')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('冲突后的备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('冲突后的摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('冲突标签')

    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 3, {
      title: '本地标题',
      body: '冲突后的正文',
      note: '冲突后的备注',
      summary: '冲突后的摘要',
      tags: ['冲突标签'],
    }))
  })

  it('adopts every server field and revision when the refreshed draft is clean', async () => {
    const original = inbox()
    const refreshed = inbox({
      title: '刷新标题',
      body: '刷新正文',
      note: '刷新备注',
      summary: '刷新摘要',
      tags: ['刷新标签'],
      metadata_revision: 2,
    })
    const patchInbox = vi.fn(async (
      id: string,
      _revision: number,
      request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
    ) => ok({ ...refreshed, id, ...request, metadata_revision: 3 }))
    const initialClient = makeClient([original])
    const refreshedClient = makeClient([refreshed], { patchInbox })
    const rendered = renderInbox(initialClient.client)

    await screen.findByRole('textbox', { name: '标题' })
    rendered.rerender(
      <InboxSurface
        client={refreshedClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )

    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('刷新标题'))
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('刷新正文')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('刷新备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('刷新摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('刷新标签')

    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 2, {
      title: '刷新标题',
      body: '刷新正文',
      note: '刷新备注',
      summary: '刷新摘要',
      tags: ['刷新标签'],
    }))
  })

  it('adopts remote fields after a clean confirm draft conflicts instead of overwriting them', async () => {
    const original = inbox()
    const remote = inbox({
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
      metadata_revision: 5,
    })
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockImplementationOnce(async (
        id: string,
        _revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => ok({
        ...remote,
        id,
        title: request.title,
        body: request.body,
        note: request.note,
        summary: request.summary,
        tags: request.tags,
        metadata_revision: 6,
      }))
    const getInbox = vi.fn(async () => ok(remote))
    const { client, confirmInbox } = makeClient([original], { patchInbox, getInbox })

    renderInbox(client)
    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('已保留本地草稿并同步最新版本')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('远端标题')
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('远端正文')
    expect(screen.getByRole('textbox', { name: '笔记' })).toHaveValue('远端备注')
    expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue('远端摘要')
    expect(screen.getByRole('textbox', { name: '标签' })).toHaveValue('远端标签')

    fireEvent.click(screen.getByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 5, {
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
    }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 6))
  })

  it('refreshes a stale confirmation POST and retries with its rebased revision', async () => {
    const initial = inbox()
    const saved = inbox({ title: '已保存标题', metadata_revision: 2 })
    const remote = inbox({
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
      metadata_revision: 3,
    })
    const patchInbox = vi.fn()
      .mockResolvedValueOnce(ok(saved))
      .mockImplementationOnce(async (
        id: string,
        revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => ok({ ...remote, id, ...request, metadata_revision: revision + 1 }))
    const confirmInbox = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '冲突', status: 409 }))
      .mockResolvedValueOnce(ok({ target_kind: 'link' as const, link_id: 'link-1', status: 'confirmed' as const }))
    const getInbox = vi.fn(async () => ok(remote))
    const { client } = makeClient([initial], { patchInbox, confirmInbox, getInbox })
    const { onOpenLink } = renderInbox(client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))

    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))
    expect(await screen.findByRole('alert')).toHaveTextContent('已保留本地草稿并同步最新版本，请再次确认')
    expect(getInbox).toHaveBeenCalledWith('inbox-1')
    expect(screen.getByRole('status')).toHaveTextContent('下次保存时使用最新版本')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('远端标题')
    expect(screen.getByRole('textbox', { name: '正文' })).toHaveValue('远端正文')
    expect(onOpenLink).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(patchInbox).toHaveBeenLastCalledWith('inbox-1', 3, {
      title: '远端标题',
      body: '远端正文',
      note: '远端备注',
      summary: '远端摘要',
      tags: ['远端标签'],
    }))
    await waitFor(() => expect(confirmInbox).toHaveBeenLastCalledWith('inbox-1', 4))
    expect(onOpenLink).toHaveBeenCalledWith('link-1')
  })

  it('refreshes every conflicting bulk row when CAS rereads resolve out of order', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    const third = inbox({ id: 'inbox-3', title: '第三篇', url: 'https://example.com/three' })
    const remoteSecond = inbox({
      id: 'inbox-2',
      title: '远端第二篇',
      url: 'https://example.com/two',
      metadata_revision: 4,
    })
    const remoteThird = inbox({
      id: 'inbox-3',
      title: '远端第三篇',
      url: 'https://example.com/three',
      metadata_revision: 5,
    })
    const refreshResolvers = new Map<string, (value: ReturnType<typeof ok>) => void>()
    const getInbox = vi.fn((id: string) => new Promise<ReturnType<typeof ok>>((resolve) => {
      refreshResolvers.set(id, resolve)
    }))
    const confirmInboxBulk = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'other' as const, message: '批量冲突', status: 409 }))
      .mockImplementationOnce(async (request: { inbox_ids: string[] }) => ok({
        atomic: true as const,
        items: request.inbox_ids.map((id) => ({ inbox_id: id, status: 'confirmed' as const, link_id: `link-${id}` })),
      }))
    const { client } = makeClient([first, second, third], { getInbox, confirmInboxBulk })

    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 第二篇' }))
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 第三篇' }))
    await waitFor(() => expect(screen.getByText('已选择 2 项')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))
    await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledWith({
      inbox_ids: ['inbox-2', 'inbox-3'],
      expected_revisions: { 'inbox-2': 1, 'inbox-3': 1 },
    }))
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-2'))
    await waitFor(() => expect(getInbox).toHaveBeenCalledWith('inbox-3'))

    await act(async () => {
      refreshResolvers.get('inbox-3')?.(ok(remoteThird))
    })
    expect(await screen.findByRole('button', { name: /远端第三篇/ })).toBeInTheDocument()
    await act(async () => {
      refreshResolvers.get('inbox-2')?.(ok(remoteSecond))
    })
    expect(await screen.findByRole('button', { name: /远端第二篇/ })).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('内容已经被其他窗口更新')

    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))
    await waitFor(() => expect(confirmInboxBulk).toHaveBeenLastCalledWith({
      inbox_ids: ['inbox-2', 'inbox-3'],
      expected_revisions: { 'inbox-2': 4, 'inbox-3': 5 },
    }))
  })

  it('keeps the local draft and selection when save-before-switch loses the network', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇' })
    const patchInbox = vi.fn(async () => err({
      kind: 'network-unreachable' as const, message: '网络不可达',
    }))
    const { client } = makeClient([first, second], { patchInbox })

    renderInbox(client)
    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '离线草稿' } })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    expect(await screen.findByRole('alert')).toHaveTextContent('网络不可达')
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('离线草稿')
    expect(screen.getByRole('button', { name: /第一篇/ }).className).toContain('active')
  })

  it('saves the current draft before confirming and blocks an empty title', async () => {
    const { client, patchInbox, confirmInbox } = makeClient([inbox()])

    renderInbox(client)
    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '已编辑标题' } })
    fireEvent.click(screen.getByRole('button', { name: '确认入库' }))

    await waitFor(() => expect(patchInbox).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))

    const { client: emptyClient, confirmInbox: emptyConfirm } = makeClient([inbox({ title: '' })])
    const { unmount } = renderInbox(emptyClient)
    expect((await screen.findAllByRole('button', { name: '确认入库' })).at(-1)).toBeDisabled()
    expect(emptyConfirm).not.toHaveBeenCalled()
    unmount()
  })

  it('does not continue confirmation after selection changes while its save is pending', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    let resolvePatch!: (value: ReturnType<typeof ok>) => void
    const patchInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolvePatch = resolve }))
    const { client, confirmInbox } = makeClient([first, second], { patchInbox })
    const { onOpenLink } = renderInbox(client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.any(Object)))
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇'))
    await act(async () => {
      resolvePatch(ok(inbox({ metadata_revision: 2 })))
    })

    expect(confirmInbox).not.toHaveBeenCalled()
    expect(onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
  })

  it('does not apply a delayed confirmation after selection changes', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    let resolveConfirm!: (value: ReturnType<typeof ok>) => void
    const confirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConfirm = resolve }))
    const { client } = makeClient([first, second], { confirmInbox })
    const { onOpenLink } = renderInbox(client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇'))
    await act(async () => {
      resolveConfirm(ok({ target_kind: 'link', link_id: 'link-1', status: 'confirmed' }))
    })

    expect(onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇')
  })

  it('does not apply a delayed confirmation after its deep-link scope changes', async () => {
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇', url: 'https://example.com/two' })
    let resolveReload!: (value: ReturnType<typeof ok>) => void
    let resolveConfirm!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockResolvedValueOnce(ok({ items: [first, second], next_cursor: undefined }))
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveReload = resolve }))
    const confirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConfirm = resolve }))
    const { client } = makeClient([first, second], { listInbox, confirmInbox })
    const rendered = renderInbox(client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))
    rendered.rerender(
      <InboxSurface
        client={client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-2"
      />,
    )
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(2))

    await act(async () => {
      resolveConfirm(ok({ target_kind: 'link', link_id: 'link-1', status: 'confirmed' }))
    })

    expect(rendered.onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
    await act(async () => {
      resolveReload(ok({ items: [first, second], next_cursor: undefined }))
    })
    expect(await screen.findByRole('textbox', { name: '标题' })).toHaveValue('第二篇')
  })

  it('does not apply a delayed confirmation after its identity is invalidated', async () => {
    let current = true
    let resolveConfirm!: (value: ReturnType<typeof ok>) => void
    const confirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConfirm = resolve }))
    const { client } = makeClient([inbox()], {
      confirmInbox,
      isIdentityCurrent: () => current,
    })
    const { onOpenLink } = renderInbox(client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))
    current = false
    await act(async () => {
      resolveConfirm(ok({ target_kind: 'link', link_id: 'link-1', status: 'confirmed' }))
    })

    expect(onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '确认入库' })).not.toBeDisabled()
  })

  describe('identity-revoked mutation busy release', () => {
    it('releases create busy without adding the stale Inbox row', async () => {
      let current = true
      let resolveCreate!: (value: ReturnType<typeof ok>) => void
      const createInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveCreate = resolve }))
      const { client } = makeClient([inbox()], { createInbox, isIdentityCurrent: () => current })

      renderInbox(client)
      await screen.findByRole('heading', { name: '第一篇' })
      fireEvent.click(screen.getByRole('button', { name: '添加条目' }))
      fireEvent.change(screen.getByPlaceholderText('https://...'), { target: { value: 'https://example.com/stale' } })
      fireEvent.click(screen.getByRole('button', { name: '创建' }))
      await waitFor(() => expect(createInbox).toHaveBeenCalledTimes(1))

      current = false
      await act(async () => {
        resolveCreate(ok(inbox({ id: 'stale-create', title: '旧创建结果' })))
      })

      expect(screen.getByRole('button', { name: '创建' })).not.toBeDisabled()
      expect(screen.queryByRole('button', { name: /旧创建结果/ })).not.toBeInTheDocument()
    })

    it('releases single discard busy without changing the row status', async () => {
      let current = true
      let resolveDiscard!: (value: ReturnType<typeof ok>) => void
      const discardInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveDiscard = resolve }))
      const { client } = makeClient([inbox()], { discardInbox, isIdentityCurrent: () => current })

      renderInbox(client)
      fireEvent.click(await screen.findByRole('button', { name: '丢弃' }))
      await waitFor(() => expect(discardInbox).toHaveBeenCalledWith('inbox-1'))

      current = false
      await act(async () => {
        resolveDiscard(ok(true))
      })

      expect(screen.getByRole('button', { name: '丢弃' })).not.toBeDisabled()
      expect(screen.queryByRole('button', { name: '恢复到收件箱' })).not.toBeInTheDocument()
    })

    it('releases single restore busy without changing the row status', async () => {
      let current = true
      let resolveRestore!: (value: ReturnType<typeof ok>) => void
      const restoreInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveRestore = resolve }))
      const discarded = inbox({ status: 'discarded', title: '已丢弃' })
      const { client } = makeClient([discarded], { restoreInbox, isIdentityCurrent: () => current })

      renderInbox(client)
      fireEvent.click(await screen.findByRole('button', { name: '恢复到收件箱' }))
      await waitFor(() => expect(restoreInbox).toHaveBeenCalledWith('inbox-1'))

      current = false
      await act(async () => {
        resolveRestore(ok(true))
      })

      expect(screen.getByRole('button', { name: '恢复到收件箱' })).not.toBeDisabled()
      expect(screen.queryByRole('button', { name: '确认入库' })).not.toBeInTheDocument()
    })

    it('releases bulk confirm busy without removing stale rows', async () => {
      let current = true
      let resolveBulk!: (value: ReturnType<typeof ok>) => void
      const confirmInboxBulk = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveBulk = resolve }))
      const { client } = makeClient([inbox()], { confirmInboxBulk, isIdentityCurrent: () => current })

      renderInbox(client)
      await screen.findByRole('heading', { name: '第一篇' })
      fireEvent.click(screen.getByRole('checkbox', { name: '选择 第一篇' }))
      fireEvent.click(screen.getByRole('button', { name: '确认所选' }))
      await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledTimes(1))

      current = false
      await act(async () => {
        resolveBulk(ok({ atomic: true, items: [{ inbox_id: 'inbox-1', status: 'confirmed', link_id: 'stale-link' }] }))
      })

      expect(screen.getByRole('button', { name: '确认所选' })).not.toBeDisabled()
      expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
      expect(screen.queryByText('批量确认完成')).not.toBeInTheDocument()
    })

    it('releases bulk discard busy without changing stale rows', async () => {
      let current = true
      let resolveBulk!: (value: ReturnType<typeof ok>) => void
      const discardInboxBulk = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveBulk = resolve }))
      const { client } = makeClient([inbox()], { discardInboxBulk, isIdentityCurrent: () => current })

      renderInbox(client)
      await screen.findByRole('heading', { name: '第一篇' })
      fireEvent.click(screen.getByRole('checkbox', { name: '选择 第一篇' }))
      fireEvent.click(screen.getByRole('button', { name: '批量丢弃' }))
      await waitFor(() => expect(discardInboxBulk).toHaveBeenCalledTimes(1))

      current = false
      await act(async () => {
        resolveBulk(ok({ atomic: true, items: [{ inbox_id: 'inbox-1', status: 'discarded' }] }))
      })

      expect(screen.getByRole('button', { name: '批量丢弃' })).not.toBeDisabled()
      expect(screen.queryByRole('button', { name: '恢复到收件箱' })).not.toBeInTheDocument()
      expect(screen.queryByText('批量丢弃完成')).not.toBeInTheDocument()
    })

    it('releases resummarize busy without applying the stale job', async () => {
      let current = true
      let resolveResummarize!: (value: ReturnType<typeof ok>) => void
      const resummarizeInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveResummarize = resolve }))
      const { client } = makeClient([inbox()], { resummarizeInbox, isIdentityCurrent: () => current })

      renderInbox(client)
      fireEvent.click(await screen.findByRole('button', { name: '重新生成摘要' }))
      await waitFor(() => expect(resummarizeInbox).toHaveBeenCalledWith('inbox-1'))

      current = false
      await act(async () => {
        resolveResummarize(ok({ inbox_id: 'inbox-1', status: 'queued', job_id: 'stale-job' }))
      })

      expect(screen.getByRole('button', { name: '重新生成摘要' })).not.toBeDisabled()
      expect(screen.queryByText(/stale-job/)).not.toBeInTheDocument()
    })
  })

  it('clears a pending confirmation when the client owner changes', async () => {
    let resolveConfirm!: (value: ReturnType<typeof ok>) => void
    const confirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveConfirm = resolve }))
    const firstClient = makeClient([inbox()], { confirmInbox })
    const secondClient = makeClient([inbox()])
    const rendered = renderInbox(firstClient.client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(confirmInbox).toHaveBeenCalledWith('inbox-1', 2))
    expect(screen.getByRole('button', { name: '确认入库' })).toBeDisabled()

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )

    await waitFor(() => expect(screen.getByRole('button', { name: '确认入库' })).not.toBeDisabled())
    await act(async () => {
      resolveConfirm(ok({ target_kind: 'link', link_id: 'link-1', status: 'confirmed' }))
    })

    expect(rendered.onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /第一篇/ })).toBeInTheDocument()
  })

  it('does not let an old confirmation clear a replacement owner busy state', async () => {
    let resolveOldConfirm!: (value: ReturnType<typeof ok>) => void
    let resolveNewConfirm!: (value: ReturnType<typeof ok>) => void
    const oldConfirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => {
      resolveOldConfirm = resolve
    }))
    const newConfirmInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => {
      resolveNewConfirm = resolve
    }))
    const firstClient = makeClient([inbox({ title: '旧 owner' })], { confirmInbox: oldConfirmInbox })
    const secondClient = makeClient([inbox({
      id: 'inbox-2',
      title: '新 owner',
      url: 'https://example.com/replacement',
    })], { confirmInbox: newConfirmInbox })
    const rendered = renderInbox(firstClient.client)

    fireEvent.click(await screen.findByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(oldConfirmInbox).toHaveBeenCalledWith('inbox-1', 2))

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )
    await screen.findByRole('heading', { name: '新 owner' })
    fireEvent.click(screen.getByRole('button', { name: '确认入库' }))
    await waitFor(() => expect(newConfirmInbox).toHaveBeenCalledWith('inbox-2', 2))
    expect(screen.getByRole('button', { name: '确认入库' })).toBeDisabled()

    await act(async () => {
      resolveOldConfirm(ok({ target_kind: 'link', link_id: 'old-link', status: 'confirmed' }))
    })

    expect(rendered.onOpenLink).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: '确认入库' })).toBeDisabled()

    await act(async () => {
      resolveNewConfirm(ok({ target_kind: 'link', link_id: 'new-link', status: 'confirmed' }))
    })
    expect(rendered.onOpenLink).toHaveBeenCalledWith('new-link')
  })

  it('does not apply a delayed create after both the client and deep-link owner change', async () => {
    const oldItem = inbox({ id: 'inbox-a', title: '旧 owner' })
    const currentItem = inbox({ id: 'inbox-b', title: '新 owner', url: 'https://example.com/current' })
    let resolveCreate!: (value: ReturnType<typeof ok>) => void
    const createInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveCreate = resolve }))
    const firstClient = makeClient([oldItem], { createInbox })
    const secondClient = makeClient([currentItem])
    const rendered = renderInbox(firstClient.client, 'inbox-a')

    await screen.findByRole('heading', { name: '旧 owner' })
    fireEvent.click(screen.getByRole('button', { name: '添加条目' }))
    fireEvent.change(screen.getByPlaceholderText('https://...'), { target: { value: 'https://example.com/created' } })
    fireEvent.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(createInbox).toHaveBeenCalledTimes(1))

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-b"
      />,
    )
    expect(await screen.findByRole('heading', { name: '新 owner' })).toBeInTheDocument()

    await act(async () => {
      resolveCreate(ok(inbox({ id: 'created', title: '旧创建响应', url: 'https://example.com/created' })))
    })

    expect(screen.getAllByRole('textbox', { name: '标题' }).at(-1)).toHaveValue('新 owner')
    expect(screen.queryByRole('button', { name: /旧创建响应/ })).not.toBeInTheDocument()
  })

  it('does not show a delayed bulk success after both the client and deep-link owner change', async () => {
    const oldItem = inbox({ id: 'inbox-a', title: '旧 owner' })
    const currentItem = inbox({ id: 'inbox-b', title: '新 owner', url: 'https://example.com/current' })
    let resolveBulk!: (value: ReturnType<typeof ok>) => void
    const discardInboxBulk = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveBulk = resolve }))
    const firstClient = makeClient([oldItem], { discardInboxBulk })
    const secondClient = makeClient([currentItem])
    const rendered = renderInbox(firstClient.client, 'inbox-a')

    await screen.findByRole('heading', { name: '旧 owner' })
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 旧 owner' }))
    fireEvent.click(screen.getByRole('button', { name: '批量丢弃' }))
    await waitFor(() => expect(discardInboxBulk).toHaveBeenCalledWith({ inbox_ids: ['inbox-a'] }))

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-b"
      />,
    )
    expect(await screen.findByRole('heading', { name: '新 owner' })).toBeInTheDocument()

    await act(async () => {
      resolveBulk(ok({ atomic: true, items: [{ inbox_id: 'inbox-a', status: 'discarded' }] }))
    })

    expect(screen.queryByText('批量丢弃完成')).not.toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('新 owner')
  })

  it('does not start bulk conflict rereads after both the client and deep-link owner change', async () => {
    const oldItem = inbox({ id: 'inbox-a', title: '旧 owner' })
    const currentItem = inbox({ id: 'inbox-b', title: '新 owner', url: 'https://example.com/current' })
    let resolveBulk!: (value: ReturnType<typeof err>) => void
    const confirmInboxBulk = vi.fn(() => new Promise<ReturnType<typeof err>>((resolve) => { resolveBulk = resolve }))
    const firstClient = makeClient([oldItem], { confirmInboxBulk })
    const secondClient = makeClient([currentItem])
    const rendered = renderInbox(firstClient.client, 'inbox-a')

    await screen.findByRole('heading', { name: '旧 owner' })
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 旧 owner' }))
    fireEvent.click(screen.getByRole('button', { name: '确认所选' }))
    await waitFor(() => expect(confirmInboxBulk).toHaveBeenCalledTimes(1))

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
        initialInboxID="inbox-b"
      />,
    )
    expect(await screen.findByRole('heading', { name: '新 owner' })).toBeInTheDocument()

    await act(async () => {
      resolveBulk(err({ kind: 'other', message: '旧 owner 冲突', status: 409 }))
    })

    expect(firstClient.getInbox).not.toHaveBeenCalled()
    expect(screen.queryByText('旧 owner 冲突')).not.toBeInTheDocument()
  })

  it('does not apply a delayed discard to a replacement client with the same Inbox ID', async () => {
    const oldItem = inbox({ title: '旧 owner' })
    const replacementItem = inbox({ title: '新 owner' })
    let resolveDiscard!: (value: ReturnType<typeof ok>) => void
    const discardInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveDiscard = resolve }))
    const firstClient = makeClient([oldItem], { discardInbox })
    const secondClient = makeClient([replacementItem])
    const rendered = renderInbox(firstClient.client)

    await screen.findByRole('heading', { name: '旧 owner' })
    fireEvent.click(screen.getByRole('button', { name: '丢弃' }))
    await waitFor(() => expect(discardInbox).toHaveBeenCalledWith('inbox-1'))

    rendered.rerender(
      <InboxSurface
        client={secondClient.client}
        capabilityPolicy={rendered.capabilityPolicy}
        onNavigate={rendered.onNavigate}
        onOpenLink={rendered.onOpenLink}
      />,
    )
    expect(await screen.findByRole('heading', { name: '新 owner' })).toBeInTheDocument()

    await act(async () => {
      resolveDiscard(ok(true))
    })

    expect(screen.getByRole('button', { name: '丢弃' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '恢复到收件箱' })).not.toBeInTheDocument()
  })

  it('keeps the create form isolated from the selected Inbox draft', async () => {
    const { client, createInbox } = makeClient([inbox()])

    renderInbox(client)
    const editorTitle = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(editorTitle, { target: { value: '编辑器标题' } })
    fireEvent.click(screen.getByRole('button', { name: '添加条目' }))
    const titles = screen.getAllByRole('textbox', { name: '标题' })
    fireEvent.change(titles[0] as HTMLInputElement, { target: { value: '创建标题' } })
    expect(titles[1]).toHaveValue('编辑器标题')
    fireEvent.change(screen.getByPlaceholderText('https://...'), { target: { value: 'https://example.com/new' } })
    fireEvent.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(createInbox).toHaveBeenCalledWith({
      url: 'https://example.com/new',
      source_kind: 'manual',
      title: '创建标题',
      body: '',
      note: '',
      tags: [],
    }))
  })

  it('persists title, body, summary, note, tags, adopted suggestions, and category membership', async () => {
    const category = { id: 'category-1', name: '研究', count: 0, created_at: '2026-08-10T01:00:00Z' }
    const { client, patchInbox } = makeClient([inbox()], {
      listCategories: vi.fn(async () => ok({ items: [category] })),
    })

    renderInbox(client)
    fireEvent.change(await screen.findByRole('textbox', { name: '标题' }), { target: { value: '新标题' } })
    fireEvent.change(screen.getByRole('textbox', { name: '笔记' }), { target: { value: '用户备注' } })
    fireEvent.change(screen.getByRole('textbox', { name: '摘要' }), { target: { value: '用户摘要' } })
    fireEvent.change(screen.getByRole('textbox', { name: '标签' }), { target: { value: '已有' } })
    fireEvent.change(screen.getByRole('textbox', { name: '正文' }), { target: { value: '完整正文' } })
    fireEvent.click(screen.getByRole('button', { name: '预览' }))
    expect(screen.getByText('完整正文')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '编辑' }))
    fireEvent.click(screen.getByRole('button', { name: '采用 #阅读' }))
    fireEvent.click(screen.getByRole('button', { name: '研究' }))
    await waitFor(() => expect((client.setCategoryMembership as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith('category-1', {
      host_kind: 'inbox', host_id: 'inbox-1', present: true,
    }))
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, {
      title: '新标题', body: '完整正文', note: '用户备注', summary: '用户摘要', tags: ['已有', '阅读'],
    }))
  })

  it('persists an explicitly cleared summary instead of restoring the old value', async () => {
    const { client, patchInbox } = makeClient([inbox({ summary: '旧摘要' })])
    renderInbox(client)

    const summary = await screen.findByRole('textbox', { name: '摘要' })
    fireEvent.change(summary, { target: { value: '   ' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))

    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.objectContaining({ summary: '' })))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '摘要' })).toHaveValue(''))
  })

  it('persists a missing title while keeping confirmation disabled', async () => {
    const { client, patchInbox, confirmInbox } = makeClient([inbox({ title: '旧标题' })])
    renderInbox(client)

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '   ' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))

    await waitFor(() => expect(patchInbox).toHaveBeenCalledWith('inbox-1', 1, expect.objectContaining({ title: '' })))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue(''))
    expect(screen.getByRole('button', { name: '确认入库' })).toBeDisabled()
    expect(confirmInbox).not.toHaveBeenCalled()
  })

  it('does not let an older save response overwrite edits made while it was pending', async () => {
    let resolvePatch!: (value: ReturnType<typeof ok>) => void
    const patchInbox = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolvePatch = resolve }))
    const { client } = makeClient([inbox()], { patchInbox })
    renderInbox(client)

    const title = await screen.findByRole('textbox', { name: '标题' })
    fireEvent.change(title, { target: { value: '提交中的标题' } })
    fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
    await waitFor(() => expect(patchInbox).toHaveBeenCalledTimes(1))
    expect(title).toBeDisabled()
    fireEvent.change(title, { target: { value: '请求后的新输入' } })
    await act(async () => {
      resolvePatch(ok(inbox({ title: '提交中的标题', metadata_revision: 2 })))
    })

    expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('请求后的新输入')
  })

  it('scopes a delayed category response to the Inbox item that initiated it', async () => {
    const category = { id: 'category-1', name: '研究', count: 0, created_at: '2026-08-10T01:00:00Z' }
    const first = inbox()
    const second = inbox({ id: 'inbox-2', title: '第二篇' })
    let resolveMembership!: (value: ReturnType<typeof ok>) => void
    const setCategoryMembership = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveMembership = resolve }))
    const { client } = makeClient([first, second], {
      listCategories: vi.fn(async () => ok({ items: [category] })),
      setCategoryMembership,
    })
    renderInbox(client)

    await screen.findByRole('heading', { name: '第一篇' })
    fireEvent.click(screen.getByRole('button', { name: '研究' }))
    await waitFor(() => expect(setCategoryMembership).toHaveBeenCalledWith('category-1', {
      host_kind: 'inbox', host_id: 'inbox-1', present: true,
    }))
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('第二篇'))
    await act(async () => {
      resolveMembership(ok(true))
    })

    expect(screen.getByRole('button', { name: '研究' }).className).not.toContain('active')
    fireEvent.click(screen.getByRole('button', { name: /第一篇/ }))
    await waitFor(() => expect(screen.getByRole('button', { name: '研究' }).className).toContain('active'))
  })

  it('ignores an older category result after a newer request for the same item succeeds', async () => {
    const category = { id: 'category-1', name: '研究', count: 0, created_at: '2026-08-10T01:00:00Z' }
    let resolveFirst!: (value: ReturnType<typeof err>) => void
    const setCategoryMembership = vi.fn()
      .mockImplementationOnce(() => new Promise<ReturnType<typeof err>>((resolve) => { resolveFirst = resolve }))
      .mockResolvedValueOnce(ok(true))
    const { client } = makeClient([inbox()], {
      listCategories: vi.fn(async () => ok({ items: [category] })),
      setCategoryMembership,
    })
    renderInbox(client)

    const categoryButton = await screen.findByRole('button', { name: '研究' })
    fireEvent.click(categoryButton)
    fireEvent.click(categoryButton)
    await waitFor(() => expect(setCategoryMembership).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(categoryButton.className).toContain('active'))

    await act(async () => {
      resolveFirst(err({ kind: 'network-unreachable', message: '旧分类请求失败' }))
    })

    expect(categoryButton.className).toContain('active')
    expect(screen.queryByText('旧分类请求失败')).not.toBeInTheDocument()
  })

  it('ignores a delayed category result after its identity is no longer current', async () => {
    const category = { id: 'category-1', name: '研究', count: 0, created_at: '2026-08-10T01:00:00Z' }
    let current = true
    let resolveMembership!: (value: ReturnType<typeof ok>) => void
    const setCategoryMembership = vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveMembership = resolve }))
    const { client } = makeClient([inbox()], {
      listCategories: vi.fn(async () => ok({ items: [category] })),
      setCategoryMembership,
      isIdentityCurrent: () => current,
    })
    renderInbox(client)

    const categoryButton = await screen.findByRole('button', { name: '研究' })
    fireEvent.click(categoryButton)
    await waitFor(() => expect(setCategoryMembership).toHaveBeenCalledTimes(1))
    current = false
    await act(async () => {
      resolveMembership(ok(true))
    })

    expect(categoryButton.className).not.toContain('active')
  })
})
