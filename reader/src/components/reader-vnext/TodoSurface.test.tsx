import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { ok } from '@webtag/api'
import type { ReaderTodoPatchRequest, ReaderTodoResponse } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { makeReaderTodo as todo } from '../../test/fixtures'
import { makeReaderClient } from '../../test/reader-client'
import { sortTodos, TodoSurface } from './TodoSurface'

function makeClient(items: ReaderTodoResponse[]) {
  const listTodos = vi.fn(async () => ok({ items }))
  const patchTodo = vi.fn(async (id: string, patch: ReaderTodoPatchRequest) => {
    const current = items.find((item) => item.id === id) ?? items[0]
    return ok(todo({
      ...current,
      ...patch,
      text: patch.text === null || patch.text === undefined ? current.text : patch.text,
      due_at: patch.due_at === undefined ? current.due_at : patch.due_at,
      done: patch.done === null || patch.done === undefined ? current.done : patch.done,
    }))
  })
  const deleteTodo = vi.fn(async () => ok(true as const))
  const client = makeReaderClient({
    listTodos,
    patchTodo,
    deleteTodo,
  })
  return { client, listTodos, patchTodo, deleteTodo }
}

function makeStatefulClient(initialItems: ReaderTodoResponse[]) {
  const state = new Map(initialItems.map((item) => [item.id, item]))
  let nextID = 1
  const listTodos = vi.fn(async () => ok({ items: [...state.values()] }))
  const createTodo = vi.fn(async (request: { readonly text: string; readonly due_at?: string | null }) => {
    const created = todo({
      id: `created-${nextID++}`,
      text: request.text,
      due_at: request.due_at ?? null,
      created_at: '2026-08-10T06:00:00Z',
      updated_at: '2026-08-10T06:00:00Z',
    })
    state.set(created.id, created)
    return ok(created)
  })
  const patchTodo = vi.fn(async (id: string, patch: ReaderTodoPatchRequest) => {
    const current = state.get(id)
    if (!current) throw new Error(`missing todo ${id}`)
    const nextDone = patch.done === undefined || patch.done === null ? current.done : patch.done
    const next = todo({
      ...current,
      ...(patch.text === undefined || patch.text === null ? {} : { text: patch.text }),
      ...(patch.due_at === undefined ? {} : { due_at: patch.due_at }),
      done: nextDone,
      completed_at: nextDone ? '2026-08-10T07:00:00Z' : null,
      expired: nextDone ? false : Boolean(patch.due_at ? Date.parse(patch.due_at) < Date.now() : current.expired),
      updated_at: '2026-08-10T07:00:00Z',
    })
    state.set(id, next)
    return ok(next)
  })
  const deleteTodo = vi.fn(async (id: string) => {
    state.delete(id)
    return ok(true as const)
  })
  const client = makeReaderClient({
    listTodos,
    createTodo,
    patchTodo,
    deleteTodo,
  })
  return { client, listTodos, createTodo, patchTodo, deleteTodo }
}

function renderTodo(
  client: IdentityBoundReaderClient,
  capabilityOverrides: Parameters<typeof enabledReaderCapabilityPolicy>[0] = {},
) {
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  const onOpenLink = vi.fn<(id: string) => void>()
  function SessionTodoSurface() {
    const [completedExpanded, setCompletedExpanded] = useState(false)
    const [todoVisible, setTodoVisible] = useState(true)
    return (
      <>
        <button type="button" onClick={() => setTodoVisible((visible) => !visible)}>切换 TODO 页面</button>
        {todoVisible && (
          <TodoSurface
            client={client}
            capabilityPolicy={enabledReaderCapabilityPolicy(capabilityOverrides)}
            onNavigate={onNavigate}
            onOpenLink={onOpenLink}
            completedExpanded={completedExpanded}
            onCompletedExpandedChange={setCompletedExpanded}
          />
        )}
      </>
    )
  }
  const rendered = render(<SessionTodoSurface />)
  return { ...rendered, onNavigate, onOpenLink }
}

afterEach(() => {
  window.history.replaceState({}, '', '/')
  vi.restoreAllMocks()
})

describe('TodoSurface', () => {
  it('sorts undone first, due dates chronologically, then undated by creation time', () => {
    const ordered = sortTodos([
      todo({ id: 'done', done: true, created_at: '2026-08-10T05:00:00Z' }),
      todo({ id: 'undated-old', created_at: '2026-08-10T01:00:00Z' }),
      todo({ id: 'due-late', due_at: '2026-08-12T00:00:00Z', created_at: '2026-08-10T04:00:00Z' }),
      todo({ id: 'due-soon', due_at: '2026-08-11T00:00:00Z', created_at: '2026-08-10T02:00:00Z', expired: true }),
      todo({ id: 'undated-new', created_at: '2026-08-10T03:00:00Z' }),
    ])

    expect(ordered.map((item) => item.id)).toEqual(['due-soon', 'due-late', 'undated-new', 'undated-old', 'done'])
  })

  it('keeps completed TODOs in a default-collapsed group for one mounted session', async () => {
    const completedNew = todo({ id: 'done-new', text: '较新已完成', done: true, created_at: '2026-08-10T05:00:00Z' })
    const completedOld = todo({ id: 'done-old', text: '较旧已完成', done: true, created_at: '2026-08-10T02:00:00Z' })
    const { client, listTodos } = makeClient([
      todo({ id: 'open-1', text: '未完成一' }),
      completedOld,
      todo({ id: 'open-2', text: '未完成二', created_at: '2026-08-10T03:00:00Z' }),
      completedNew,
    ])
    const user = userEvent.setup()
    const firstSession = renderTodo(client)

    expect(await screen.findByText('未完成一')).toBeInTheDocument()
    expect(screen.getByText('未完成二')).toBeInTheDocument()
    const disclosure = screen.getByRole('button', { name: '已完成 2' })
    expect(disclosure).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('较新已完成')).not.toBeInTheDocument()
    expect(screen.queryByText('较旧已完成')).not.toBeInTheDocument()
    expect(screen.queryByRole('list', { name: '已完成 TODO' })).not.toBeInTheDocument()

    disclosure.focus()
    await user.keyboard('{Enter}')

    expect(disclosure).toHaveAttribute('aria-expanded', 'true')
    const completedList = screen.getByRole('list', { name: '已完成 TODO' })
    expect(within(completedList).getAllByRole('button', { name: /^重新打开 / }).map((button) => button.getAttribute('aria-label'))).toEqual([
      '重新打开 较新已完成',
      '重新打开 较旧已完成',
    ])

    act(() => window.dispatchEvent(new Event('webtag:todos-change')))
    await waitFor(() => expect(listTodos).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('button', { name: '已完成 2' })).toHaveAttribute('aria-expanded', 'true')

    await user.keyboard(' ')
    expect(screen.getByRole('button', { name: '已完成 2' })).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByRole('list', { name: '已完成 TODO' })).not.toBeInTheDocument()

    await user.keyboard('{Enter}')
    fireEvent.click(screen.getByRole('button', { name: '切换 TODO 页面' }))
    expect(screen.queryByRole('button', { name: '已完成 2' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '切换 TODO 页面' }))
    expect(await screen.findByRole('button', { name: '已完成 2' })).toHaveAttribute('aria-expanded', 'true')

    firstSession.unmount()
    renderTodo(client)
    expect(await screen.findByRole('button', { name: '已完成 2' })).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('较新已完成')).not.toBeInTheDocument()
  })

  it('does not render a completed-group control when every TODO is open', async () => {
    renderTodo(makeClient([todo({ text: '唯一未完成' })]).client)

    expect(await screen.findByText('唯一未完成')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^已完成 / })).not.toBeInTheDocument()
  })

  it('omits host revision for standalone toggles, uses the current revision for projections, and keeps projections source-owned', async () => {
    const standalone = todo({ id: 'standalone', text: '独立任务' })
    const projection = todo({ id: 'projection', text: '来源任务', origin_kind: 'thought', origin_host_kind: 'thought', origin_host_id: 'T1', host_revision: 11 })
    const { client, patchTodo, deleteTodo } = makeClient([standalone, projection])

    renderTodo(client)

    expect(await screen.findByText('来源任务')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: '删除' })).toHaveLength(1)
    expect(screen.getByText('来源任务').tagName).toBe('SPAN')

    fireEvent.click(screen.getByRole('button', { name: '完成 独立任务' }))
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('standalone', { done: true }))

    fireEvent.click(screen.getByRole('button', { name: '完成 来源任务' }))
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('projection', {
      done: true,
      expected_host_revision: 11,
    }))
    expect(deleteTodo).not.toHaveBeenCalled()
  })

  it('edits only standalone text and routes inbox projections back to their target', async () => {
    const standalone = todo({ id: 'standalone', text: '旧文本' })
    const inbox = todo({ id: 'inbox-todo', text: '收件箱任务', origin_kind: 'inbox', origin_host_kind: 'inbox', origin_host_id: 'I9', host_revision: 3 })
    const { client, patchTodo } = makeClient([standalone, inbox])
    const { onNavigate } = renderTodo(client)

    expect(await screen.findByText('旧文本')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '旧文本' }))
    const editor = screen.getByDisplayValue('旧文本')
    fireEvent.change(editor, { target: { value: '新文本' } })
    fireEvent.keyDown(editor, { key: 'Enter' })
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('standalone', { text: '新文本' }))

    const inboxRow = screen.getByText('收件箱任务').closest('li') as HTMLElement
    fireEvent.click(within(inboxRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I9' })
    expect(window.location.search).toBe('')
  })

  it('routes projected sources to the exact link, note, inbox, and thought targets', async () => {
    const link = todo({
      id: 'thought-link',
      text: '链接来源任务',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-link',
      origin_ref: { source_kind: 'link', source_id: 'L9', link_id: 'L9' },
    })
    const note = todo({
      id: 'thought-note',
      text: '笔记来源任务',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-note',
      origin_ref: { source_kind: 'note', source_id: 'N9' },
    })
    const inbox = todo({
      id: 'thought-inbox',
      text: '收件箱来源任务',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-inbox',
      origin_ref: { source_kind: 'inbox', source_id: 'I9' },
    })
    const thought = todo({
      id: 'thought-direct',
      text: '想法来源任务',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T9',
      origin_ref: { source_kind: 'thought', source_id: 'T9' },
    })
    const { onNavigate, onOpenLink } = renderTodo(makeClient([link, note, inbox, thought]).client)

    const linkRow = (await screen.findByText('链接来源任务')).closest('li') as HTMLElement
    fireEvent.click(within(linkRow).getByRole('button', { name: '打开来源' }))
    expect(onOpenLink).toHaveBeenCalledWith('L9')

    const noteRow = screen.getByText('笔记来源任务').closest('li') as HTMLElement
    fireEvent.click(within(noteRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'notes' }, { noteId: 'N9' })
    expect(window.location.search).toBe('')

    const inboxRow = screen.getByText('收件箱来源任务').closest('li') as HTMLElement
    fireEvent.click(within(inboxRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I9' })
    expect(window.location.search).toBe('')

    const thoughtRow = screen.getByText('想法来源任务').closest('li') as HTMLElement
    fireEvent.click(within(thoughtRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'tool', id: 'history' }, { thoughtView: 'live' })
    expect(window.location.search).toBe('')
  })

  it('opens note and inbox sources when origin_ref is the only target metadata', async () => {
    const note = todo({
      id: 'ref-only-note',
      text: '仅引用笔记任务',
      origin_kind: 'thought',
      origin_host_kind: null,
      origin_host_id: null,
      origin_ref: { source_kind: 'note', source_id: 'N-ref-only' },
    })
    const inbox = todo({
      id: 'ref-only-inbox',
      text: '仅引用收件箱任务',
      origin_kind: 'thought',
      origin_host_kind: null,
      origin_host_id: null,
      origin_ref: { source_kind: 'inbox', source_id: 'I-ref-only' },
    })
    const { onNavigate } = renderTodo(makeClient([note, inbox]).client)

    const noteRow = (await screen.findByText('仅引用笔记任务')).closest('li') as HTMLElement
    expect(within(noteRow).getByRole('button', { name: '打开来源' })).toBeEnabled()
    fireEvent.click(within(noteRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'notes' }, { noteId: 'N-ref-only' })
    expect(window.location.search).toBe('')

    const inboxRow = screen.getByText('仅引用收件箱任务').closest('li') as HTMLElement
    expect(within(inboxRow).getByRole('button', { name: '打开来源' })).toBeEnabled()
    fireEvent.click(within(inboxRow).getByRole('button', { name: '打开来源' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'pending', inboxId: 'I-ref-only' })
    expect(window.location.search).toBe('')
  })

  it('disables only source routes whose capability is unavailable', async () => {
    const link = todo({
      id: 'available-link',
      text: '可用链接来源',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-link',
      origin_ref: { source_kind: 'link', source_id: 'L-disabled-test', link_id: 'L-disabled-test' },
    })
    const note = todo({
      id: 'unavailable-note',
      text: '禁用笔记来源',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-note',
      origin_ref: { source_kind: 'note', source_id: 'N-disabled-test' },
    })
    const inbox = todo({
      id: 'unavailable-inbox',
      text: '禁用待确认来源',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'thought-inbox',
      origin_ref: { source_kind: 'inbox', source_id: 'I-disabled-test' },
    })
    const history = todo({
      id: 'unavailable-history',
      text: '禁用想法历史来源',
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T-disabled-test',
    })
    const { onNavigate, onOpenLink } = renderTodo(
      makeClient([link, note, inbox, history]).client,
      { notes: false, inbox: false, history: false },
    )

    const linkButton = within((await screen.findByText('可用链接来源')).closest('li') as HTMLElement)
      .getByRole('button', { name: '打开来源' })
    expect(linkButton).toBeEnabled()
    fireEvent.click(linkButton)
    expect(onOpenLink).toHaveBeenCalledWith('L-disabled-test')

    for (const label of ['禁用笔记来源', '禁用待确认来源', '禁用想法历史来源']) {
      const row = screen.getByText(label).closest('li') as HTMLElement
      expect(within(row).getByRole('button', { name: '打开来源' })).toBeDisabled()
    }
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('creates a due TODO and publishes one shared change event after the authoritative response', async () => {
    const { client, createTodo, listTodos } = makeStatefulClient([])
    const dispatchEvent = vi.spyOn(window, 'dispatchEvent')
    renderTodo(client)

    fireEvent.change(await screen.findByPlaceholderText('下一步要完成什么？'), { target: { value: '带截止时间的任务' } })
    fireEvent.change(screen.getByLabelText('截止时间'), { target: { value: '2026-08-12T03:45' } })
    fireEvent.click(screen.getByRole('button', { name: '添加' }))

    const dueAt = new Date('2026-08-12T03:45').toISOString()
    await waitFor(() => expect(createTodo).toHaveBeenCalledWith({ text: '带截止时间的任务', due_at: dueAt }))
    expect(await screen.findByText('带截止时间的任务')).toBeInTheDocument()
    expect(listTodos).toHaveBeenCalledTimes(2)
    expect(dispatchEvent.mock.calls.filter(([event]) => event.type === 'webtag:todos-change')).toHaveLength(1)
  })

  it('edits and clears due_at without changing the legacy text-only patch shape', async () => {
    const initial = todo({ id: 'due-edit', text: '可编辑截止时间', due_at: '2026-08-11T01:00:00Z' })
    const { client, patchTodo } = makeStatefulClient([initial])
    const dispatchEvent = vi.spyOn(window, 'dispatchEvent')
    renderTodo(client)

    const firstRow = (await screen.findByText('可编辑截止时间')).closest('li') as HTMLElement
    fireEvent.click(within(firstRow).getByRole('button', { name: '可编辑截止时间' }))
    const dueInput = within(firstRow).getByLabelText('截止时间')
    fireEvent.change(dueInput, { target: { value: '2026-08-12T03:45' } })
    fireEvent.keyDown(within(firstRow).getByDisplayValue('可编辑截止时间'), { key: 'Enter' })

    const changedDueAt = new Date('2026-08-12T03:45').toISOString()
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('due-edit', { text: '可编辑截止时间', due_at: changedDueAt }))

    const secondRow = (await screen.findByText('可编辑截止时间')).closest('li') as HTMLElement
    fireEvent.click(within(secondRow).getByRole('button', { name: '可编辑截止时间' }))
    fireEvent.change(within(secondRow).getByLabelText('截止时间'), { target: { value: '' } })
    fireEvent.keyDown(within(secondRow).getByDisplayValue('可编辑截止时间'), { key: 'Enter' })
    await waitFor(() => expect(patchTodo).toHaveBeenLastCalledWith('due-edit', { text: '可编辑截止时间', due_at: null }))
    expect(dispatchEvent.mock.calls.filter(([event]) => event.type === 'webtag:todos-change')).toHaveLength(2)
  })

  it('uses desired done state for completion and reopen, displays server completed_at, and shares both changes', async () => {
    const projection = todo({ id: 'projection-life', text: '完成后可重开', origin_kind: 'thought', origin_host_kind: 'thought', origin_host_id: 'T1', host_revision: 12 })
    const { client, patchTodo } = makeStatefulClient([projection])
    const dispatchEvent = vi.spyOn(window, 'dispatchEvent')
    renderTodo(client)

    fireEvent.click(await screen.findByRole('button', { name: '完成 完成后可重开' }))
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('projection-life', { done: true, expected_host_revision: 12 }))
    expect(await screen.findByRole('button', { name: '已完成 1' })).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('完成后可重开')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '已完成 1' }))
    expect(await screen.findByText(/完成于 ·/)).toBeInTheDocument()
    fireEvent.click(await screen.findByRole('button', { name: '重新打开 完成后可重开' }))
    await waitFor(() => expect(patchTodo).toHaveBeenLastCalledWith('projection-life', { done: false, expected_host_revision: 12 }))
    await waitFor(() => expect(screen.queryByText(/完成于 ·/)).not.toBeInTheDocument())
    expect(screen.getByText('完成后可重开')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^已完成 / })).not.toBeInTheDocument()
    expect(dispatchEvent.mock.calls.filter(([event]) => event.type === 'webtag:todos-change')).toHaveLength(2)
  })

  it('updates the completed count and removes the group as completed TODOs are deleted', async () => {
    const { client, deleteTodo } = makeStatefulClient([
      todo({ id: 'done-1', text: '删除已完成一', done: true, completed_at: '2026-08-10T07:00:00Z' }),
      todo({ id: 'done-2', text: '删除已完成二', done: true, completed_at: '2026-08-10T07:00:00Z' }),
    ])
    renderTodo(client)

    fireEvent.click(await screen.findByRole('button', { name: '已完成 2' }))
    const firstRow = screen.getByText('删除已完成一').closest('li') as HTMLElement
    fireEvent.click(within(firstRow).getByRole('button', { name: '删除' }))

    await waitFor(() => expect(deleteTodo).toHaveBeenCalledWith('done-1'))
    expect(await screen.findByRole('button', { name: '已完成 1' })).toHaveAttribute('aria-expanded', 'true')
    expect(screen.queryByText('删除已完成一')).not.toBeInTheDocument()

    const lastRow = screen.getByText('删除已完成二').closest('li') as HTMLElement
    fireEvent.click(within(lastRow).getByRole('button', { name: '删除' }))
    await waitFor(() => expect(deleteTodo).toHaveBeenCalledWith('done-2'))
    await waitFor(() => expect(screen.queryByRole('button', { name: /^已完成 / })).not.toBeInTheDocument())
  })

  it('reloads when another surface publishes a TODO change', async () => {
    const { client, listTodos } = makeClient([todo({ text: '共享刷新' })])
    renderTodo(client)
    expect(await screen.findByText('共享刷新')).toBeInTheDocument()

    act(() => window.dispatchEvent(new Event('webtag:todos-change')))
    await waitFor(() => expect(listTodos).toHaveBeenCalledTimes(2))
  })

  it('clears private TODO state and exits loading when identity is lost during a refresh', async () => {
    let identityCurrent = true
    const listTodos = vi.fn(async () => ok({ items: [todo({ text: '身份隔离任务' })] }))
    const client = makeReaderClient({
      listTodos,
    }, { isIdentityCurrent: () => identityCurrent })
    renderTodo(client)

    expect(await screen.findByText('身份隔离任务')).toBeInTheDocument()
    identityCurrent = false
    act(() => window.dispatchEvent(new Event('webtag:todos-change')))

    expect(await screen.findByRole('alert')).toHaveTextContent('身份已失效')
    expect(screen.queryByText('身份隔离任务')).not.toBeInTheDocument()
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
    expect(listTodos).toHaveBeenCalledTimes(1)
  })

  // 共享的 isRecord 拒绝数组。带上同名字段的数组过去能骗过「非 null object」这
  // 一层守卫，被当成一条真 TODO 画出来；现在它在第一层就被判为坏数据。
  it('refuses an array-shaped TODO even when it carries the expected fields', async () => {
    const arrayShaped = Object.assign(['unexpected'], {
      id: 'array-todo',
      text: '数组任务',
      done: false,
      origin_kind: 'standalone',
      created_at: '2026-08-10T01:00:00Z',
      updated_at: '2026-08-10T01:00:00Z',
    })
    const listTodos = vi.fn(async () => ok({ items: [arrayShaped] }))
    const client = makeReaderClient({ listTodos })

    renderTodo(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('数据不完整')
    expect(screen.queryByText('数组任务')).not.toBeInTheDocument()
  })

  // 错误文案收口到 readerErrorMessage：抛出物只带状态码、没有 message 字符串时，
  // 也要走宿主协议的 409 / 412 / 403 文案，而不是笼统的「请求失败」。
  it('translates a thrown host status into the shared conflict copy', async () => {
    const listTodos = vi.fn(async () => { throw { status: 409 } })
    const client = makeReaderClient({ listTodos })

    renderTodo(client)

    expect(await screen.findByRole('alert')).toHaveTextContent('内容已经被其他窗口更新，请刷新后重试。')
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
  })

  it('accepts an old TODO envelope for display but rejects partial mutation success', async () => {
    const legacy = {
      todoId: 'legacy-todo',
      text: '旧字段任务',
      dueAt: '2026-08-09T01:00:00Z',
      done: false,
      origin: { kind: 'standalone' },
      createdAt: '2026-08-08T01:00:00Z',
      updatedAt: '2026-08-08T01:00:00Z',
      isExpired: true,
    }
    const listTodos = vi.fn(async () => ok({ todos: [legacy] }))
    const patchTodo = vi.fn(async () => ok({ id: 'legacy-todo', done: true }))
    const client = makeReaderClient({ listTodos, patchTodo })

    renderTodo(client)
    expect(await screen.findByText('旧字段任务')).toBeInTheDocument()
    expect(screen.getByText(/已过期 ·/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '完成 旧字段任务' }))
    await waitFor(() => expect(patchTodo).toHaveBeenCalledWith('legacy-todo', { done: true }))
    expect(await screen.findByRole('alert')).toHaveTextContent('数据不完整')
    expect(screen.getByRole('button', { name: '完成 旧字段任务' })).toBeInTheDocument()
  })
})
