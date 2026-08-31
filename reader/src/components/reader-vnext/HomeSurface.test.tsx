import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok, type ApiResult } from '@webtag/api'
import type {
  ReaderHomeResponse,
  ReaderThoughtResponse,
  ReaderTodoResponse,
} from '../../lib/api/types'
import type { ReaderRoute, ReaderRouteTargets } from '../../lib/navigation/route'
import { formatRelativeDate } from '../../lib/reader-surface'
import { enabledReaderCapabilityLease } from '../../test/capabilities'
import {
  deferred,
  makeReaderFeedItem as feedItem,
  makeReaderHome as home,
  makeReaderThought as thought,
  makeReaderTodo as todo,
} from '../../test/fixtures'
import { makeReaderClient } from '../../test/reader-client'
import { HomeSurface } from './HomeSurface'

function makeClient(responses: ReaderHomeResponse[]) {
  let index = 0
  const getHome = vi.fn(async () => ok(responses[Math.min(index++, responses.length - 1)]))
  const patchTodo = vi.fn(async (_id: string, _patch: unknown) => ok(todo({ done: true, completed_at: '2026-08-10T02:00:00Z' })))
  const client = makeReaderClient({
    getHome,
    patchTodo,
  })
  return { client, getHome, patchTodo }
}

function renderHome(
  client: IdentityBoundReaderClient,
  capabilityOverrides: Parameters<typeof enabledReaderCapabilityLease>[0] = {},
) {
  const onNavigate = vi.fn<(route: ReaderRoute, targets?: ReaderRouteTargets) => boolean | void>()
  const onOpenLink = vi.fn<(id: string) => void>()
  const rendered = render(<HomeSurface client={client} capabilityLease={enabledReaderCapabilityLease(capabilityOverrides)} onNavigate={onNavigate} onOpenLink={onOpenLink} />)
  return { ...rendered, onNavigate, onOpenLink }
}

afterEach(() => {
  window.history.replaceState({}, '', '/')
  vi.restoreAllMocks()
})

describe('HomeSurface', () => {
  it('does not request or retain a Home aggregate when Home is unavailable', () => {
    const { client, getHome } = makeClient([home()])

    renderHome(client, { home: false })

    expect(getHome).not.toHaveBeenCalled()
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
    expect(screen.queryByText('今天继续整理')).not.toBeInTheDocument()
  })

  it('stays in loading state until the first authoritative aggregate arrives', async () => {
    const pending = deferred<ApiResult<ReaderHomeResponse>>()
    const getHome = vi.fn(() => pending.promise)
    const client = makeReaderClient({
      getHome,
      patchTodo: vi.fn(),
    })

    renderHome(client)

    expect(screen.getByText('加载中')).toBeInTheDocument()
    expect(screen.queryByText('今天继续整理')).not.toBeInTheDocument()

    await act(async () => {
      pending.resolve(ok(home()))
      await pending.promise
    })

    expect(await screen.findByText('今天继续整理')).toBeInTheDocument()
  })

  it('shows the initial aggregate error and retries without inventing a snapshot', async () => {
    const getHome = vi.fn()
      .mockResolvedValueOnce(err<ReaderHomeResponse>({ kind: 'other', message: 'Home 暂时不可用' }))
      .mockResolvedValueOnce(ok(home()))
    const client = makeReaderClient({
      getHome,
      patchTodo: vi.fn(),
    })

    renderHome(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('Home 暂时不可用')
    expect(screen.queryByText('今天继续整理')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '重试' }))

    expect(await screen.findByText('今天继续整理')).toBeInTheDocument()
    expect(getHome).toHaveBeenCalledTimes(2)
  })

  it('marks the last complete snapshot stale when a shared-state refresh fails', async () => {
    const getHome = vi.fn()
      .mockResolvedValueOnce(ok(home({ freshness: 'fresh', partial: false, stale: false })))
      .mockResolvedValueOnce(err<ReaderHomeResponse>({ kind: 'other', message: '刷新失败' }))
    const client = makeReaderClient({
      getHome,
      patchTodo: vi.fn(),
    })

    renderHome(client)

    expect(await screen.findByText('数据已同步')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    expect(await screen.findByRole('alert')).toHaveTextContent('刷新失败')
    expect(screen.getByText('数据较旧')).toBeInTheDocument()
    expect(screen.getByText('今天继续整理')).toBeInTheDocument()
  })

  it.each([
    ['legacy missing freshness', home(), '数据新鲜度未知'],
    ['fresh', home({ freshness: 'fresh', partial: false, stale: false }), '数据已同步'],
    ['partial', home({ freshness: 'fresh', partial: true, stale: false, counts: { pending_count: 2 } }), '部分数据可用'],
    ['stale', home({ freshness: 'stale', partial: false, stale: false }), '数据较旧'],
    ['partial and stale', home({ freshness: 'stale', partial: true, stale: true }), '部分数据较旧'],
  ] as const)('renders the %s aggregate status and keeps unavailable counts explicit', async (_name, response, status) => {
    const { client } = makeClient([response])

    renderHome(client)

    expect(await screen.findByText(status)).toBeInTheDocument()
    if (response.partial && response.counts.reading === undefined && response.counts.reading_count === undefined) {
      // 计数条用「—」表示「服务端没给这个数」，和真实的 0 必须能区分开。
      expect(screen.getByRole('button', { name: /阅读.*—/ })).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /阅读.*0$/ })).not.toBeInTheDocument()
    }
  })

  it('renders the library counts from compatible field names and does not fetch TODOs separately', async () => {
    const { client } = makeClient([home()])

    renderHome(client)

    expect(await screen.findByText('今天继续整理')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /收件箱.*2/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /阅读.*3/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /订阅.*5/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /笔记.*6/ })).toBeInTheDocument()
    // 网站不再是一个顶级库，计数条里也就没有它了；路由仍然可达。
    expect(screen.queryByRole('button', { name: /网站/ })).not.toBeInTheDocument()
    expect(client.getHome).toHaveBeenCalledTimes(1)
  })

  it('does not present an unread count as the total subscription count', async () => {
    const { client } = makeClient([home({
      counts: { pending: 2, reading: 3, sites: 4, unread: 99, notes: 6 },
    })])

    renderHome(client)

    expect(await screen.findByText('今天继续整理')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /订阅.*—/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /订阅.*99/ })).not.toBeInTheDocument()
  })

  it('uses desired-state/CAS and re-reads the authoritative aggregate after a Home toggle', async () => {
    const projection = todo({ id: 'todo-projection', text: '勾选来源任务', origin_kind: 'note', origin_host_kind: 'note', origin_host_id: 'N1', host_revision: 7 })
    const first = home({ todos: [projection] })
    const second = home({ counts: { pending: 1, reading: 2, sites: 3, subs: 4, notes: 5 }, todos: [] })
    const { client, getHome, patchTodo } = makeClient([first, second])

    renderHome(client)

    const checkbox = await screen.findByRole('button', { name: '完成 勾选来源任务' })
    fireEvent.click(checkbox)

    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('todo-projection', {
      done: true,
      expected_host_revision: 7,
    }))
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
    expect(screen.getByText('没有未完成任务。')).toBeInTheDocument()
  })

  it('uses server due and completion fields and does not apply the patch response as Home state', async () => {
    const projection = todo({
      id: 'todo-due',
      text: '等待聚合回读',
      due_at: '2026-08-09T02:00:00Z',
      expired: true,
      origin_kind: 'note',
      origin_host_kind: 'note',
      origin_host_id: 'N1',
      host_revision: 7,
    })
    const first = home({ todos: [projection] })
    const refreshed = deferred<ApiResult<ReaderHomeResponse>>()
    const getHome = vi.fn()
      .mockResolvedValueOnce(ok(first))
      .mockReturnValueOnce(refreshed.promise)
    const patchTodo = vi.fn(async () => ok(todo({
      ...projection,
      done: true,
      completed_at: '2026-08-10T02:00:00Z',
    })))
    const client = makeReaderClient({
      getHome,
      patchTodo,
    })

    renderHome(client)

    const row = await screen.findByText('等待聚合回读')
    expect(within(row.closest('li') as HTMLElement).getByText(/已过期/)).toBeInTheDocument()

    fireEvent.click(within(row.closest('li') as HTMLElement).getByRole('button', { name: '完成 等待聚合回读' }))

    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('todo-due', {
      done: true,
      expected_host_revision: 7,
    }))
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
    expect(screen.getByText('等待聚合回读')).toBeInTheDocument()
    expect(screen.queryByText('没有未完成任务。')).not.toBeInTheDocument()

    await act(async () => {
      refreshed.resolve(ok(home({ todos: [todo({
        ...projection,
        done: true,
        completed_at: '2026-08-10T02:00:00Z',
        expired: false,
      })] })))
      await refreshed.promise
    })

    expect(await screen.findByText('没有未完成任务。')).toBeInTheDocument()
    expect(screen.queryByText('等待聚合回读')).not.toBeInTheDocument()
  })

  it('keeps the authoritative TODO visible and reports a toggle error when the server rejects the write', async () => {
    const item = todo({ due_at: '2026-08-11T02:00:00Z' })
    const getHome = vi.fn(async () => ok(home({ todos: [item] })))
    const patchTodo = vi.fn(async () => err<ReaderTodoResponse>({ kind: 'other', message: 'TODO 写入失败' }))
    const client = makeReaderClient({
      getHome,
      patchTodo,
    })

    renderHome(client)

    const row = await screen.findByText(item.text)
    fireEvent.click(within(row.closest('li') as HTMLElement).getByRole('button', { name: `完成 ${item.text}` }))

    expect(await screen.findByRole('alert')).toHaveTextContent('TODO 写入失败')
    expect(screen.getByText(item.text)).toBeInTheDocument()
    // 单飞的坑必须在失败后还给用户，否则这一行就永远点不动了。
    expect(screen.getByRole('button', { name: `完成 ${item.text}` })).toBeEnabled()
    expect(getHome).toHaveBeenCalledTimes(1)
  })

  it('does not write a projected TODO when the host revision is missing', async () => {
    const projection = {
      ...todo({ id: 'missing-revision', text: '缺少版本的来源任务', origin_kind: 'note', origin_host_kind: 'note', origin_host_id: 'N1' }),
      host_revision: undefined as unknown as number,
    }
    const patchTodo = vi.fn()
    const client = makeReaderClient({
      getHome: vi.fn(async () => ok(home({ todos: [projection] }))),
      patchTodo,
    })

    renderHome(client)

    const row = await screen.findByText('缺少版本的来源任务')
    fireEvent.click(within(row.closest('li') as HTMLElement).getByRole('button', { name: '完成 缺少版本的来源任务' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('缺少版本信息')
    expect(patchTodo).not.toHaveBeenCalled()
  })

  it('clears private aggregate data and exits loading when identity is unavailable', async () => {
    const getHome = vi.fn(async () => err<ReaderHomeResponse>({
      kind: 'identity-mismatch',
      message: '身份已失效',
    }))
    const client = makeReaderClient({
      getHome,
      patchTodo: vi.fn(),
    }, { isIdentityCurrent: () => false })

    renderHome(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('身份已失效')
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
    expect(getHome).not.toHaveBeenCalled()
  })

  it.each(['create', 'toggle', 'delete'] as const)('re-fetches the aggregate after a %s TODO change event', async (operation) => {
    const { client, getHome } = makeClient([
      home(),
      home({ summary: `${operation} 后的聚合`, todos: [] }),
    ])

    renderHome(client)

    expect(await screen.findByText('今天继续整理')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
    expect(await screen.findByText(`${operation} 后的聚合`)).toBeInTheDocument()
    expect(screen.getByText('没有未完成任务。')).toBeInTheDocument()
  })

  it('re-fetches the aggregate after a Feed-originated Home change event', async () => {
    const { client, getHome } = makeClient([
      home({ summary: 'Feed 操作前' }),
      home({ summary: 'Feed 操作后' }),
    ])

    renderHome(client)

    expect(await screen.findByText('Feed 操作前')).toBeInTheDocument()
    act(() => window.dispatchEvent(new Event('webtag:home-change')))

    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('Feed 操作后')).toBeInTheDocument()
  })

  it('renders continue reading through the shared list row, keeping source, time and the open action', async () => {
    const { client } = makeClient([home()])

    renderHome(client)

    const row = (await screen.findByText('收件箱文章')).closest('li') as HTMLElement
    expect(row).toHaveClass('reader-list-row', 'reader-list-row-compact')
    expect(row.querySelector('.reader-list-row-source')).toHaveTextContent('inbox')
    expect(row.querySelector('.reader-list-row-meta')).toHaveTextContent(formatRelativeDate('2026-08-10T01:00:00Z'))
    expect(row.querySelector('.reader-list-row-summary')).toHaveTextContent('需要整理')
    expect(within(row).getByRole('button', { name: '打开收件箱' }).closest('.reader-list-row-actions')).not.toBeNull()
  })

  it('does not expose a continue-reading open control without a resource target', async () => {
    const item = feedItem({ title: '不可打开的继续阅读', link_id: null, inbox_id: null })
    const { onOpenLink } = renderHome(makeClient([home({ continue_reading: [item] })]).client)

    const row = await screen.findByText('不可打开的继续阅读')
    expect(within(row.closest('li') as HTMLElement).queryByRole('button', { name: '打开阅读' })).not.toBeInTheDocument()
    expect(onOpenLink).not.toHaveBeenCalled()
  })

  it('ignores stale aggregate responses and removes the TODO listener on unmount', async () => {
    const initial = deferred<ApiResult<ReaderHomeResponse>>()
    const refresh = deferred<ApiResult<ReaderHomeResponse>>()
    const getHome = vi.fn()
      .mockReturnValueOnce(initial.promise)
      .mockReturnValueOnce(refresh.promise)
    const client = makeReaderClient({
      getHome,
      patchTodo: vi.fn(async (_id: string, _patch: unknown) => ok(todo({ done: true }))),
    })
    const { unmount } = renderHome(client)

    expect(getHome).toHaveBeenCalledTimes(1)
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))
    expect(getHome).toHaveBeenCalledTimes(2)

    refresh.resolve(ok(home({ summary: '新聚合', todos: [] })))
    expect(await screen.findByText('新聚合')).toBeInTheDocument()

    await act(async () => {
      initial.resolve(ok(home({ summary: '旧聚合' })))
      await initial.promise
    })
    expect(screen.getByText('新聚合')).toBeInTheDocument()
    expect(screen.queryByText('旧聚合')).not.toBeInTheDocument()

    unmount()
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    expect(getHome).toHaveBeenCalledTimes(2)
  })

  it('keeps an inbox item addressable by its inbox_id', async () => {
    const { client } = makeClient([home()])
    const { onNavigate } = renderHome(client)

    const row = await screen.findByText('收件箱文章')
    fireEvent.click(within(row.closest('li') as HTMLElement).getByRole('button', { name: '打开收件箱' }))

    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I1' }, undefined)
  })

  it('opens the live thought aggregate from the recent-thoughts CTA', async () => {
    window.history.replaceState({}, '', '/?surface=home')
    const { client } = makeClient([home()])
    const { onNavigate } = renderHome(client)

    await screen.findByText('最近想法')
    fireEvent.click(screen.getByRole('button', { name: '查看全部想法' }))

    expect(onNavigate).toHaveBeenCalledWith({ kind: 'tool', id: 'history' }, { thoughtView: 'live' })
  })

  it.each([
    ['annotations', { annotations: false }],
    ['history', { history: false }],
  ] as const)('hides recent thoughts when %s is unavailable', async (_capability, overrides) => {
    const { client } = makeClient([home({
      recent_thoughts: [thought({ body: '不应显示的最近想法' })],
    })])

    renderHome(client, overrides)

    await screen.findByText('今天继续整理')
    expect(screen.queryByText('最近想法')).not.toBeInTheDocument()
    expect(screen.queryByText('不应显示的最近想法')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '查看全部想法' })).not.toBeInTheDocument()
  })

  it.each([
    {
      label: '链接想法',
      item: thought({ id: 'thought-link', body: '链接想法', host_kind: 'link', host_id: 'L9', link_id: 'L9' }),
      route: { kind: 'library', id: 'reading' },
      targets: { linkId: 'L9' },
    },
    {
      label: '笔记想法',
      item: thought({ id: 'thought-note', body: '笔记想法', host_kind: 'note', host_id: 'N9', link_id: null }),
      route: { kind: 'library', id: 'notes' },
      targets: { noteId: 'N9' },
    },
    {
      label: '收件箱想法',
      item: thought({ id: 'thought-inbox', body: '收件箱想法', host_kind: 'inbox', host_id: 'I9', link_id: null }),
      route: { kind: 'library', id: 'pending', inboxId: 'I9' },
      targets: undefined,
    },
  ] satisfies ReadonlyArray<{
    readonly label: string
    readonly item: ReaderThoughtResponse
    readonly route: ReaderRoute
    readonly targets?: ReaderRouteTargets
  }>)('delegates $label to the canonical host route target', async ({ label, item, route, targets }) => {
    window.history.replaceState({}, '', '/?surface=home')
    const { client } = makeClient([home({ recent_thoughts: [item] })])
    const { onNavigate } = renderHome(client)

    const row = (await screen.findByText(label)).closest('li')
    if (!row) throw new Error(`missing recent thought row for ${label}`)
    fireEvent.click(within(row).getByRole('button', { name: '回到来源' }))

    expect(onNavigate).toHaveBeenCalledWith(route, targets)
  })

  it('only offers recent-thought source routes allowed by their own capability', async () => {
    const { client } = makeClient([home({
      recent_thoughts: [
        thought({ id: 'thought-link', body: '可用链接宿主', host_kind: 'link', host_id: 'L9', link_id: 'L9' }),
        thought({ id: 'thought-note', body: '禁用笔记宿主', host_kind: 'note', host_id: 'N9', link_id: null }),
        thought({ id: 'thought-inbox', body: '禁用待确认宿主', host_kind: 'inbox', host_id: 'I9', link_id: null }),
      ],
    })])
    const { onNavigate } = renderHome(client, { notes: false, inbox: false })

    const linkRow = (await screen.findByText('可用链接宿主')).closest('li') as HTMLElement
    const noteRow = screen.getByText('禁用笔记宿主').closest('li') as HTMLElement
    const inboxRow = screen.getByText('禁用待确认宿主').closest('li') as HTMLElement
    fireEvent.click(within(linkRow).getByRole('button', { name: '回到来源' }))

    expect(onNavigate).toHaveBeenCalledWith(
      { kind: 'library', id: 'reading' },
      { linkId: 'L9' },
    )
    expect(within(noteRow).queryByRole('button', { name: '回到来源' })).not.toBeInTheDocument()
    expect(within(inboxRow).queryByRole('button', { name: '回到来源' })).not.toBeInTheDocument()
  })

  it('does not offer source navigation for unknown or empty thought hosts', async () => {
    const { client } = makeClient([home({
      recent_thoughts: [
        thought({ id: 'thought-unknown', body: '未知宿主', host_kind: 'unsupported', host_id: 'X1', link_id: null }),
        thought({ id: 'thought-empty-note', body: '空笔记宿主', host_kind: 'note', host_id: ' ', link_id: null }),
      ],
    })])
    renderHome(client)

    await screen.findByText('未知宿主')
    expect(screen.getByText('空笔记宿主')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '回到来源' })).not.toBeInTheDocument()
  })
})
