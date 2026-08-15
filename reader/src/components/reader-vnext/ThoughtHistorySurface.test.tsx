import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const supersessionMocks = vi.hoisted(() => ({
  sync: vi.fn(),
  list: vi.fn(),
  availability: vi.fn(),
  recoveryStates: vi.fn(),
  commit: vi.fn(),
}))

const historyActionMocks = vi.hoisted(() => ({
  commit: vi.fn(),
  readSnapshot: vi.fn(),
  controller: vi.fn(),
  sync: vi.fn(),
}))

vi.mock('../../lib/user-data/thought-supersession', () => ({
  syncThoughtSupersessions: supersessionMocks.sync,
  listThoughtSupersessionEvents: supersessionMocks.list,
  readThoughtSupersessionRecoveryAvailability: supersessionMocks.availability,
  readThoughtSupersessionRecoveryStates: supersessionMocks.recoveryStates,
}))

vi.mock('../../lib/user-data/annotation-store', () => ({
  commitSupersessionRecovery: supersessionMocks.commit,
}))

vi.mock('../../lib/user-data/thought-history-actions', () => ({
  commitThoughtHistoryAction: historyActionMocks.commit,
  readThoughtActionSnapshot: historyActionMocks.readSnapshot,
}))

vi.mock('../../lib/user-data/thought-sync', async (importOriginal) => ({
  ...await importOriginal<typeof import('../../lib/user-data/thought-sync')>(),
  getThoughtSyncController: historyActionMocks.controller,
}))

import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok, type ApiResult } from '../../lib/api/result'
import type {
  ReaderNoteCreateRequest,
  ReaderNoteResponse,
  ReaderThoughtResponse,
  ReaderThoughtsResponse,
  ReaderTodoResponse,
} from '../../lib/api/types'
import type { IdentityLease } from '../../lib/identity'
import type { ReaderRoute, ReaderRouteTargets } from '../../lib/navigation/route'
import { enabledReaderCapabilityLease } from '../../test/capabilities'
import type { ThoughtSupersessionEventRecord } from '../../lib/user-data/thought-types'
import { ThoughtHistorySurface } from './ThoughtHistorySurface'

function makeThought(overrides: Partial<ReaderThoughtResponse> = {}): ReaderThoughtResponse {
  const id = overrides.id ?? 'thought-1'
  return {
    id,
    host_kind: 'link',
    host_id: `link-${id}`,
    link_id: `link-${id}`,
    target: {},
    quote: {},
    body: `想法 ${id}`,
    source: 'reader',
    deleted: false,
    last_sequence: 1,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    lifecycle_status: 'tombstone',
    lifecycle_reason: 'link_deleted',
    tombstoned_at: '2026-08-10T02:00:00Z',
    ...overrides,
    contract_version: 1,
    winner_key: overrides.winner_key ?? {
      logical_clock: 1,
      device_id: 'device-test',
      op_id: `op-${id}`,
    },
  }
}

function actionSnapshot(thought: ReaderThoughtResponse) {
  return {
    id: thought.id,
    hostKind: thought.host_kind,
    hostId: thought.host_id,
    target: thought.target,
    quote: thought.quote,
    body: thought.body,
    source: thought.source,
    lastSequence: thought.last_sequence,
    winnerKey: {
      logicalClock: thought.winner_key.logical_clock,
      deviceId: thought.winner_key.device_id,
      opId: thought.winner_key.op_id,
    },
  }
}

function makeTodo(overrides: Partial<ReaderTodoResponse> = {}): ReaderTodoResponse {
  return {
    id: 'todo-1',
    text: '完成想法',
    due_at: null,
    done: false,
    origin_kind: 'thought',
    origin_host_kind: 'thought',
    origin_host_id: 'thought-live',
    origin_ref: null,
    host_revision: 1,
    completed_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    expired: false,
    ...overrides,
  }
}

function makeNote(id: string, content: string): ReaderNoteResponse {
  return {
    id,
    title: '生成的想法笔记',
    published_content: '',
    published_revision: 0,
    draft_content: content,
    draft_revision: 1,
    draft_updated_at: '2026-08-10T03:00:00Z',
    deleted_at: null,
    created_at: '2026-08-10T03:00:00Z',
    updated_at: '2026-08-10T03:00:00Z',
    dirty: true,
  }
}

function makeListResult(items: ReaderThoughtResponse[]): ApiResult<ReaderThoughtsResponse> {
  return ok({ contract_version: 1, items, next_cursor: undefined })
}

function makeSupersessionEvent(): ThoughtSupersessionEventRecord {
  const operation = (opId: string, logicalClock: number, body: string) => ({
    sequence: logicalClock + 100,
    opId,
    deviceId: 'device-1',
    logicalClock,
    operationKind: 'update' as const,
    annotationId: 'annotation-1',
    hostKind: 'link' as const,
    hostId: 'link-1',
    target: { kind: 'saved-content' as const, contentRevision: 3 },
    targetKey: 'saved-content:3',
    body,
    source: 'self' as const,
    quote: { exact: 'quote', start: 0, end: 5 },
    createdAt: '2026-08-11T00:00:00.000Z',
  })
  return {
    key: ['supersession-test', 7],
    namespace: 'supersession-test',
    eventSequence: 7,
    annotationId: 'annotation-1',
    loser: operation('loser-op', 4, 'loser body'),
    winnerAtDetection: operation('winner-op', 5, 'winner body'),
  }
}

function makeClient(items: ReaderThoughtResponse[]) {
  const listThoughts = vi.fn(async (): Promise<ApiResult<ReaderThoughtsResponse>> => makeListResult(items))
  const listThoughtHistory = vi.fn(async (): Promise<ApiResult<ReaderThoughtsResponse>> => makeListResult(items))
  const listTodos = vi.fn(async (): Promise<ApiResult<{ items: ReaderTodoResponse[] }>> => ok({ items: [] }))
  let noteNumber = 0
  const createNote = vi.fn(async (request: ReaderNoteCreateRequest): Promise<ApiResult<ReaderNoteResponse>> => {
    noteNumber += 1
    return ok(makeNote(`note-${noteNumber}`, request.content ?? ''))
  })
  const getNote = vi.fn()
  const getThought = vi.fn(async (id: string): Promise<ApiResult<ReaderThoughtResponse>> =>
    ok(items.find((thought) => thought.id === id) ?? makeThought({ id })),
  )
  const listThoughtSupersessions = vi.fn()
  const pushThoughtOps = vi.fn(async () => ok([]))
  const isIdentityCurrent = vi.fn(() => true)
  const client = {
    listThoughts,
    listThoughtHistory,
    listTodos,
    createNote,
    getNote,
    getThought,
    listThoughtSupersessions,
    pushThoughtOps,
    isIdentityCurrent,
  } as unknown as IdentityBoundReaderClient
  return {
    client,
    listThoughts,
    listThoughtHistory,
    listTodos,
    createNote,
    getNote,
    getThought,
    pushThoughtOps,
    listThoughtSupersessions,
    isIdentityCurrent,
  }
}

function renderSurface(
  client: IdentityBoundReaderClient,
  onNavigate: (route: ReaderRoute, targets?: ReaderRouteTargets) => boolean | void = vi.fn(),
  view: 'live' | 'history' | 'superseded' = 'history',
  thoughtIDOrCapabilityOverrides?: string | Parameters<typeof enabledReaderCapabilityLease>[0],
  capabilityOverrides: Parameters<typeof enabledReaderCapabilityLease>[0] = {},
) {
  const thoughtID = typeof thoughtIDOrCapabilityOverrides === 'string'
    ? thoughtIDOrCapabilityOverrides
    : undefined
  const resolvedCapabilityOverrides = typeof thoughtIDOrCapabilityOverrides === 'string'
    ? capabilityOverrides
    : thoughtIDOrCapabilityOverrides ?? capabilityOverrides
  const params = new URLSearchParams({ tool: 'history', thought_view: view })
  if (thoughtID) params.set('thought_id', thoughtID)
  window.history.replaceState({}, '', `/?${params.toString()}`)
  render(
    <ThoughtHistorySurface
      client={client}
      capabilityLease={enabledReaderCapabilityLease(resolvedCapabilityOverrides)}
      lease={{} as IdentityLease}
      onNavigate={onNavigate}
    />,
  )
  return onNavigate
}

beforeEach(() => {
  vi.spyOn(window, 'confirm').mockReturnValue(true)
  historyActionMocks.commit.mockReset()
  historyActionMocks.readSnapshot.mockReset()
  historyActionMocks.controller.mockReset()
  historyActionMocks.sync.mockReset()
  historyActionMocks.controller.mockReturnValue({ sync: historyActionMocks.sync })
  historyActionMocks.commit.mockResolvedValue({
    ok: true,
    value: { status: 'committed', opId: 'history-op' },
  })
  historyActionMocks.readSnapshot.mockResolvedValue({ ok: true, value: null })
  historyActionMocks.sync.mockResolvedValue({
    status: 'synced',
    pushed: 1,
    pulled: 0,
    cursor: '',
    pending: 0,
  })
})

afterEach(() => {
  vi.restoreAllMocks()
  window.history.replaceState({}, '', '/')
  supersessionMocks.sync.mockReset()
  supersessionMocks.list.mockReset()
  supersessionMocks.availability.mockReset()
  supersessionMocks.recoveryStates.mockReset()
  supersessionMocks.commit.mockReset()
})

describe('ThoughtHistorySurface', () => {
  it('keeps the loading state until history has returned', async () => {
    let resolveList!: (result: ApiResult<ReaderThoughtsResponse>) => void
    const pending = new Promise<ApiResult<ReaderThoughtsResponse>>((resolve) => {
      resolveList = resolve
    })
    const { client, listThoughtHistory } = makeClient([])
    listThoughtHistory.mockReturnValue(pending)

    renderSurface(client)
    expect(screen.getByText('加载中')).toBeInTheDocument()

    resolveList(makeListResult([makeThought({ id: 'thought-loading' })]))
    expect(await screen.findByText('想法 thought-loading')).toBeInTheDocument()
    expect(screen.queryByText('加载中')).not.toBeInTheDocument()
  })

  it('creates one independent Markdown draft for selected thoughts in selection order and deep-links to the note', async () => {
    const thoughts = [
      makeThought({ id: 'thought-a', body: '第一条想法', last_sequence: 11 }),
      makeThought({ id: 'thought-b', body: '第二条想法', last_sequence: 12 }),
      makeThought({ id: 'thought-c', body: '第三条想法', last_sequence: 13 }),
    ]
    const { client, createNote } = makeClient(thoughts)
    const onNavigate = vi.fn()
    renderSurface(client, onNavigate)
    await screen.findByText('第一条想法')

    fireEvent.click(screen.getByRole('checkbox', { name: '选择想法 thought-c' }))
    fireEvent.click(screen.getByRole('checkbox', { name: '选择想法 thought-a' }))
    fireEvent.click(screen.getByRole('button', { name: '生成笔记' }))

    await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
    const request = createNote.mock.calls[0][0]
    const content = request.content ?? ''
    expect(request.title).toBe('想法：第三条想法')
    expect(content).toContain('> 第三条想法')
    expect(content).toContain('来源：[link/link-thought-c](?view=reading&link_id=link-thought-c)')
    expect(content).toContain('thought:thought-c')
    expect(content).toContain('同步序列 `13`')
    expect(content).toContain('> 第一条想法')
    expect(content).toContain('thought:thought-a')
    expect(content).not.toContain('第二条想法')
    expect(content.indexOf('第三条想法')).toBeLessThan(content.indexOf('第一条想法'))
    expect(onNavigate).toHaveBeenCalledWith(
      { kind: 'library', id: 'notes' },
      { noteId: 'note-1' },
    )
    expect(window.location.search).toBe('?tool=history&thought_view=history')
  })

  it('writes canonical Reader source URLs for link, note, and inbox thoughts', async () => {
    const thoughts = [
      makeThought({ id: 'thought-link', host_kind: 'link', host_id: 'link-source', link_id: 'link-source', body: '链接想法' }),
      makeThought({ id: 'thought-note', host_kind: 'note', host_id: 'note-source', link_id: null, body: '笔记想法' }),
      makeThought({ id: 'thought-inbox', host_kind: 'inbox', host_id: 'inbox-source', link_id: null, body: '收件箱想法' }),
    ]
    const { client, createNote } = makeClient(thoughts)
    renderSurface(client)
    await screen.findByText('链接想法')

    for (const thought of thoughts) {
      fireEvent.click(screen.getByRole('checkbox', { name: `选择想法 ${thought.id}` }))
    }
    fireEvent.click(screen.getByRole('button', { name: '生成笔记' }))

    await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
    const content = createNote.mock.calls[0][0].content as string
    expect(content).toContain('[link/link-source](?view=reading&link_id=link-source)')
    expect(content).toContain('[note/note-source](?view=notes&note_id=note-source)')
    expect(content).toContain('[inbox/inbox-source](?view=pending&inbox_id=inbox-source)')
  })

  it('does not create anything with an empty selection', async () => {
    const { client, createNote } = makeClient([makeThought({ id: 'thought-empty' })])
    renderSurface(client)
    await screen.findByText('想法 thought-empty')

    const createButton = screen.getByRole('button', { name: '生成笔记' })
    expect(createButton).toBeDisabled()
    expect(createNote).not.toHaveBeenCalled()
  })

  it('shows a creation failure, keeps the selection, and does not navigate', async () => {
    const thought = makeThought({ id: 'thought-failed', body: '需要保留选择' })
    const { client, createNote } = makeClient([thought])
    createNote.mockResolvedValue(err({ kind: 'other', message: '服务端拒绝创建' }))
    const onNavigate = vi.fn()
    renderSurface(client, onNavigate)
    await screen.findByText('需要保留选择')

    const checkbox = screen.getByRole('checkbox', { name: '选择想法 thought-failed' })
    fireEvent.click(checkbox)
    fireEvent.click(screen.getByRole('button', { name: '生成笔记' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('服务端拒绝创建')
    expect(checkbox).toBeChecked()
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('keeps creation busy while the note request is pending', async () => {
    const { client, createNote } = makeClient([makeThought({ id: 'thought-busy' })])
    let resolveCreate!: (result: ApiResult<ReaderNoteResponse>) => void
    const pending = new Promise<ApiResult<ReaderNoteResponse>>((resolve) => {
      resolveCreate = resolve
    })
    createNote.mockReturnValue(pending)
    renderSurface(client)
    await screen.findByText('想法 thought-busy')

    fireEvent.click(screen.getByRole('checkbox', { name: '选择想法 thought-busy' }))
    const createButton = screen.getByRole('button', { name: '生成笔记' })
    fireEvent.click(createButton)
    expect(createButton).toBeDisabled()
    expect(screen.getByText('创建中…')).toBeInTheDocument()

    resolveCreate(ok(makeNote('note-busy', '> 想法')))
    await waitFor(() => expect(screen.queryByText('创建中…')).not.toBeInTheDocument())
  })

  it('retains manual reattach with its existing CAS request', async () => {
    const thought = makeThought({ id: 'thought-reattach', last_sequence: 42 })
    const { client, listThoughtHistory } = makeClient([thought])
    listThoughtHistory.mockResolvedValueOnce(makeListResult([thought]))
    listThoughtHistory.mockResolvedValueOnce(makeListResult([]))
    renderSurface(client)
    await screen.findByText('想法 thought-reattach')

    fireEvent.click(screen.getByRole('button', { name: '手动重挂' }))
    fireEvent.change(screen.getByRole('textbox', { name: '目标 ID' }), { target: { value: 'note-target' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: '目标版本号' }), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: '重挂' }))

    await waitFor(() => expect(historyActionMocks.commit).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        action: 'reattach',
        thought: expect.objectContaining({
          id: thought.id,
          lastSequence: 42,
          target: thought.target,
          quote: thought.quote,
          body: thought.body,
          source: thought.source,
          winnerKey: {
            logicalClock: thought.winner_key.logical_clock,
            deviceId: thought.winner_key.device_id,
            opId: thought.winner_key.op_id,
          },
        }),
        targetHostKind: 'link',
        targetHostId: 'note-target',
        expectedHostRevision: 7,
      }),
    ))
    expect(historyActionMocks.controller).toHaveBeenCalledWith(expect.anything(), client)
    expect(historyActionMocks.sync).toHaveBeenCalledWith()
    expect((client as unknown as { readonly reattachThought?: unknown }).reattachThought).toBeUndefined()
    await waitFor(() => expect(listThoughtHistory).toHaveBeenCalledTimes(2))
    expect(screen.queryByText('想法 thought-reattach')).not.toBeInTheDocument()
  })

  it('keeps the candidate, selection, and entered target after a blocked reattach conflict', async () => {
    const thought = makeThought({ id: 'thought-conflict', last_sequence: 42 })
    const { client } = makeClient([thought])
    historyActionMocks.sync.mockResolvedValue({
      status: 'failed',
      pushed: 0,
      pulled: 0,
      cursor: '',
      pending: 1,
      errorCode: 'other:revision_conflict:409',
    })
    renderSurface(client)
    await screen.findByText('想法 thought-conflict')

    const selection = screen.getByRole('checkbox', { name: '选择想法 thought-conflict' })
    fireEvent.click(selection)
    fireEvent.click(screen.getByRole('button', { name: '手动重挂' }))
    fireEvent.change(screen.getByRole('textbox', { name: '目标 ID' }), { target: { value: 'note-target' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: '目标版本号' }), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: '重挂' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('目标宿主已修改')
    expect(screen.getByText('想法 thought-conflict')).toBeInTheDocument()
    expect(selection).toBeChecked()
    expect(screen.getByRole('textbox', { name: '目标 ID' })).toHaveValue('note-target')
    expect(screen.getByRole('spinbutton', { name: '目标版本号' })).toHaveValue(7)
    expect(historyActionMocks.commit).toHaveBeenCalledTimes(1)
    expect((client as unknown as { readonly reattachThought?: unknown }).reattachThought).toBeUndefined()
  })

  it('keeps the candidate, selection, and entered target after a superseded ACK', async () => {
    const thought = makeThought({ id: 'thought-superseded', last_sequence: 42 })
    const { client } = makeClient([thought])
    historyActionMocks.sync.mockResolvedValue({
      status: 'failed',
      pushed: 0,
      pulled: 0,
      cursor: '',
      pending: 1,
      errorCode: 'superseded',
    })
    renderSurface(client)
    await screen.findByText('想法 thought-superseded')

    const selection = screen.getByRole('checkbox', { name: '选择想法 thought-superseded' })
    fireEvent.click(selection)
    fireEvent.click(screen.getByRole('button', { name: '手动重挂' }))
    fireEvent.change(screen.getByRole('textbox', { name: '目标 ID' }), { target: { value: 'note-target' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: '目标版本号' }), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: '重挂' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('冻结候选已保留，请刷新后重试。')
    expect(screen.getByText('想法 thought-superseded')).toBeInTheDocument()
    expect(selection).toBeChecked()
    expect(screen.getByRole('textbox', { name: '目标 ID' })).toHaveValue('note-target')
    expect(screen.getByRole('spinbutton', { name: '目标版本号' })).toHaveValue(7)
    expect(historyActionMocks.commit).toHaveBeenCalledTimes(1)
  })

  it('renders frozen snapshot authority and focuses an addressed record outside the first history page', async () => {
    const focused = makeThought({
      id: 'thought-frozen',
      body: '冻结正文',
      target: { kind: 'saved-content', content_revision: 9 },
      quote: { exact: '冻结引用' },
      source: 'reader-history',
      original_host_snapshot: { title: '转换前文章', content: '冻结原文内容' },
    })
    const { client, getThought, listThoughtHistory } = makeClient([makeThought({ id: 'thought-page', body: '首页历史想法' })])
    getThought.mockResolvedValue(ok(focused))
    renderSurface(client, vi.fn(), 'history', focused.id)

    expect(await screen.findByText('冻结正文')).toBeInTheDocument()
    expect(getThought).toHaveBeenCalledWith(focused.id)
    expect(listThoughtHistory).toHaveBeenCalledWith({ after: undefined, limit: 30 })
    const row = screen.getByText('冻结正文').closest('[data-thought-id]')
    expect(row).toHaveAttribute('data-thought-id', focused.id)
    expect(row).toHaveAttribute('aria-current', 'true')
    expect(screen.getByText('冻结目标：{"kind":"saved-content","content_revision":9}')).toBeInTheDocument()
    expect(screen.getByText('冻结引用：{"exact":"冻结引用"}')).toBeInTheDocument()
    expect(screen.getByText('冻结来源：reader-history')).toBeInTheDocument()
    expect(screen.getByText('冻结原文：{"title":"转换前文章","content":"冻结原文内容"}')).toBeInTheDocument()
  })

  it('ignores a deep-link response after the identity is no longer current', async () => {
    const focused = makeThought({ id: 'thought-stale-identity', body: '不应显示的冻结正文' })
    const fixture = makeClient([])
    let resolveThought!: (result: ApiResult<ReaderThoughtResponse>) => void
    fixture.getThought.mockReturnValue(new Promise<ApiResult<ReaderThoughtResponse>>((resolve) => {
      resolveThought = resolve
    }))
    renderSurface(fixture.client, vi.fn(), 'history', focused.id)
    await waitFor(() => expect(fixture.getThought).toHaveBeenCalledWith(focused.id))

    fixture.isIdentityCurrent.mockReturnValue(false)
    await act(async () => { resolveThought(ok(focused)) })
    expect(screen.queryByText('不应显示的冻结正文')).not.toBeInTheDocument()
  })

  it('removes a historical thought only after its durable delete action is acknowledged and refreshed', async () => {
    const thought = makeThought({ id: 'thought-delete' })
    const { client, listThoughtHistory, pushThoughtOps } = makeClient([thought])
    listThoughtHistory.mockResolvedValueOnce(makeListResult([thought]))
    listThoughtHistory.mockResolvedValueOnce(makeListResult([]))
    renderSurface(client)
    await screen.findByText('想法 thought-delete')

    fireEvent.click(screen.getByRole('button', { name: `删除想法 ${thought.id}` }))

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('删除操作会先保存在本地'))
    await waitFor(() => expect(historyActionMocks.commit).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        action: 'delete',
        thought: expect.objectContaining({
          id: thought.id,
          lastSequence: thought.last_sequence,
          target: thought.target,
          quote: thought.quote,
          winnerKey: {
            logicalClock: thought.winner_key.logical_clock,
            deviceId: thought.winner_key.device_id,
            opId: thought.winner_key.op_id,
          },
        }),
      }),
    ))
    expect(historyActionMocks.controller).toHaveBeenCalledWith(expect.anything(), client)
    expect(historyActionMocks.sync).toHaveBeenCalledWith()
    expect(pushThoughtOps).not.toHaveBeenCalled()
    expect(screen.queryByText('想法 thought-delete')).not.toBeInTheDocument()
  })

  it('offers the same confirmed delete command in live view and supports keyboard activation', async () => {
    const user = userEvent.setup()
    const thought = makeThought({ id: 'thought-live-delete', lifecycle_status: 'active' })
    const projected = { ...thought } as Partial<ReaderThoughtResponse>
    delete projected.contract_version
    delete projected.winner_key
    historyActionMocks.readSnapshot.mockResolvedValueOnce({ ok: true, value: actionSnapshot(thought) })
    const { client, pushThoughtOps } = makeClient([projected as ReaderThoughtResponse])
    const onNavigate = vi.fn()
    renderSurface(client, onNavigate, 'live')
    await screen.findByText('想法 thought-live-delete')

    const deleteButton = screen.getByRole('button', { name: `删除想法 ${thought.id}` })
    deleteButton.focus()
    await user.keyboard('{Enter}')

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('删除操作会先保存在本地'))
    expect(historyActionMocks.readSnapshot).toHaveBeenCalledWith(expect.anything(), thought.id)
    await waitFor(() => expect(historyActionMocks.commit).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        action: 'delete',
        thought: expect.objectContaining({ id: thought.id, lastSequence: thought.last_sequence }),
      }),
    ))
    expect(historyActionMocks.controller).toHaveBeenCalledWith(expect.anything(), client)
    expect(historyActionMocks.sync).toHaveBeenCalledTimes(1)
    expect(pushThoughtOps).not.toHaveBeenCalled()
    expect(onNavigate).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.queryByText('想法 thought-live-delete')).not.toBeInTheDocument())
    await waitFor(() => expect(screen.getByRole('button', { name: '当前' })).toHaveFocus())
  })

  it('fails closed for a live row without local snapshot authority and preserves its selection', async () => {
    const thought = makeThought({ id: 'thought-live-no-snapshot', lifecycle_status: 'active' })
    const projected = { ...thought } as Partial<ReaderThoughtResponse>
    delete projected.contract_version
    delete projected.winner_key
    const { client } = makeClient([projected as ReaderThoughtResponse])
    renderSurface(client, vi.fn(), 'live')
    await screen.findByText('想法 thought-live-no-snapshot')

    const selection = screen.getByRole('checkbox', { name: `选择想法 ${thought.id}` })
    fireEvent.click(selection)
    fireEvent.click(screen.getByRole('button', { name: `删除想法 ${thought.id}` }))

    expect(await screen.findByRole('alert')).toHaveTextContent('想法缺少可验证的服务端版本')
    expect(historyActionMocks.readSnapshot).toHaveBeenCalledWith(expect.anything(), thought.id)
    expect(window.confirm).not.toHaveBeenCalled()
    expect(historyActionMocks.commit).not.toHaveBeenCalled()
    expect(historyActionMocks.sync).not.toHaveBeenCalled()
    expect(screen.getByText('想法 thought-live-no-snapshot')).toBeInTheDocument()
    expect(selection).toBeChecked()
    await waitFor(() => expect(
      screen.getByRole('button', { name: `删除想法 ${thought.id}` }),
    ).toHaveFocus())
  })

  it('retains the live row and selection when its durable delete is waiting for sync recovery', async () => {
    const thought = makeThought({ id: 'thought-live-sync-failed', lifecycle_status: 'active' })
    historyActionMocks.sync.mockResolvedValueOnce({
      status: 'failed',
      pushed: 0,
      pulled: 0,
      cursor: '',
      pending: 1,
      errorCode: 'network-unreachable',
    })
    const { client } = makeClient([thought])
    renderSurface(client, vi.fn(), 'live')
    await screen.findByText('想法 thought-live-sync-failed')

    const selection = screen.getByRole('checkbox', { name: `选择想法 ${thought.id}` })
    fireEvent.click(selection)
    fireEvent.click(screen.getByRole('button', { name: `删除想法 ${thought.id}` }))

    expect(await screen.findByRole('alert')).toHaveTextContent('想法删除已保存在本地')
    expect(historyActionMocks.commit).toHaveBeenCalledTimes(1)
    expect(historyActionMocks.sync).toHaveBeenCalledTimes(1)
    expect(screen.getByText('想法 thought-live-sync-failed')).toBeInTheDocument()
    expect(selection).toBeChecked()
    await waitFor(() => expect(
      screen.getByRole('button', { name: `删除想法 ${thought.id}` }),
    ).toHaveFocus())
  })

  it('drops a live delete continuation when its capability generation is revoked during snapshot lookup', async () => {
    const thought = makeThought({ id: 'thought-live-revoked', lifecycle_status: 'active' })
    const projected = { ...thought } as Partial<ReaderThoughtResponse>
    delete projected.contract_version
    delete projected.winner_key
    let resolveSnapshot!: (result: { ok: true; value: ReturnType<typeof actionSnapshot> }) => void
    historyActionMocks.readSnapshot.mockReturnValueOnce(new Promise((resolve) => {
      resolveSnapshot = resolve
    }))
    const { client } = makeClient([projected as ReaderThoughtResponse])
    const capabilityLease = enabledReaderCapabilityLease()
    const onNavigate = vi.fn()
    window.history.replaceState({}, '', '/?tool=history&thought_view=live')
    render(
      <ThoughtHistorySurface
        client={client}
        capabilityLease={capabilityLease}
        lease={{} as IdentityLease}
        onNavigate={onNavigate}
      />,
    )
    await screen.findByText('想法 thought-live-revoked')

    fireEvent.click(screen.getByRole('button', { name: `删除想法 ${thought.id}` }))
    expect(screen.getByRole('button', { name: `正在删除想法 ${thought.id}` })).toBeDisabled()
    capabilityLease.revoke()
    await act(async () => { resolveSnapshot({ ok: true, value: actionSnapshot(thought) }) })

    expect(window.confirm).not.toHaveBeenCalled()
    expect(historyActionMocks.commit).not.toHaveBeenCalled()
    expect(historyActionMocks.sync).not.toHaveBeenCalled()
    expect(onNavigate).not.toHaveBeenCalled()
    expect(screen.getByText('想法 thought-live-revoked')).toBeInTheDocument()
    await waitFor(() => expect(
      screen.getByRole('button', { name: `删除想法 ${thought.id}` }),
    ).toBeEnabled())
  })

  it('cancels live deletion without losing selection, navigation, or focus', async () => {
    const user = userEvent.setup()
    const thought = makeThought({ id: 'thought-live-cancel', lifecycle_status: 'active' })
    const { client } = makeClient([thought])
    const onNavigate = vi.fn()
    vi.mocked(window.confirm).mockReturnValueOnce(false)
    renderSurface(client, onNavigate, 'live')
    await screen.findByText('想法 thought-live-cancel')

    const selection = screen.getByRole('checkbox', { name: `选择想法 ${thought.id}` })
    await user.click(selection)
    const deleteButton = screen.getByRole('button', { name: `删除想法 ${thought.id}` })
    deleteButton.focus()
    await user.keyboard(' ')

    expect(window.confirm).toHaveBeenCalledTimes(1)
    expect(historyActionMocks.commit).not.toHaveBeenCalled()
    expect(historyActionMocks.sync).not.toHaveBeenCalled()
    expect(screen.getByText('想法 thought-live-cancel')).toBeInTheDocument()
    expect(selection).toBeChecked()
    expect(onNavigate).not.toHaveBeenCalled()
    await waitFor(() => expect(deleteButton).toHaveFocus())
  })

  it('keeps one live delete in flight, blocks navigation, and restores the exact row after a local failure', async () => {
    const first = makeThought({ id: 'thought-live-pending', lifecycle_status: 'active' })
    const second = makeThought({ id: 'thought-live-other', lifecycle_status: 'active' })
    let resolveCommit!: (result: { ok: false }) => void
    historyActionMocks.commit.mockReturnValue(new Promise((resolve) => {
      resolveCommit = resolve
    }))
    const { client } = makeClient([first, second])
    const onNavigate = vi.fn()
    renderSurface(client, onNavigate, 'live')
    await screen.findByText('想法 thought-live-pending')

    const selection = screen.getByRole('checkbox', { name: `选择想法 ${first.id}` })
    fireEvent.click(selection)
    fireEvent.click(screen.getByRole('button', { name: `删除想法 ${first.id}` }))

    const pendingButton = screen.getByRole('button', { name: `正在删除想法 ${first.id}` })
    expect(pendingButton).toBeDisabled()
    expect(pendingButton).toHaveAttribute('aria-busy', 'true')
    expect(screen.getByRole('button', { name: `删除想法 ${second.id}` })).toBeDisabled()
    expect(screen.getByRole('button', { name: '当前' })).toBeDisabled()
    expect(screen.getByRole('combobox', { name: '想法筛选' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: '笔记' }))
    expect(onNavigate).not.toHaveBeenCalled()

    await act(async () => { resolveCommit({ ok: false }) })

    expect(await screen.findByRole('alert')).toHaveTextContent('无法保存删除操作')
    const retryButton = screen.getByRole('button', { name: `删除想法 ${first.id}` })
    expect(retryButton).toBeEnabled()
    expect(screen.getByText('想法 thought-live-pending')).toBeInTheDocument()
    expect(selection).toBeChecked()
    expect(historyActionMocks.sync).not.toHaveBeenCalled()
    await waitFor(() => expect(retryButton).toHaveFocus())
  })

  it('keeps a historical thought visible after a delete sync failure and blocks an exhausted logical clock', async () => {
    const failed = makeThought({ id: 'thought-delete-failed' })
    const exhausted = makeThought({
      id: 'thought-clock-exhausted',
      winner_key: { logical_clock: Number.MAX_SAFE_INTEGER, device_id: 'device-test', op_id: 'op-exhausted' },
    })
    const { client, pushThoughtOps } = makeClient([failed, exhausted])
    historyActionMocks.sync.mockResolvedValue({
      status: 'failed',
      pushed: 0,
      pulled: 0,
      cursor: '',
      pending: 1,
      errorCode: 'network-unreachable',
    })
    renderSurface(client)
    await screen.findByText('想法 thought-delete-failed')

    const rows = screen.getAllByRole('listitem')
    const failedRow = rows.find((row) => row.textContent?.includes('想法 thought-delete-failed'))!
    fireEvent.click(within(failedRow).getByRole('button', { name: `删除想法 ${failed.id}` }))
    expect(await screen.findByRole('alert')).toHaveTextContent('想法删除已保存在本地')
    expect(screen.getByText('想法 thought-delete-failed')).toBeInTheDocument()

    const exhaustedRow = rows.find((row) => row.textContent?.includes('想法 thought-clock-exhausted'))!
    fireEvent.click(within(exhaustedRow).getByRole('button', { name: `删除想法 ${exhausted.id}` }))
    expect(await screen.findByRole('alert')).toHaveTextContent('想法逻辑时钟无效，无法安全删除。')
    expect(historyActionMocks.commit).toHaveBeenCalledTimes(1)
    expect(pushThoughtOps).not.toHaveBeenCalled()
    expect(screen.getByText('想法 thought-clock-exhausted')).toBeInTheDocument()
  })

  it('keeps different tombstone reasons distinguishable in the history view', async () => {
    const thoughts = [
      makeThought({ id: 'thought-link-deleted', lifecycle_reason: 'link_deleted' }),
      makeThought({ id: 'thought-note-deleted', lifecycle_reason: 'note_deleted' }),
    ]
    renderSurface(makeClient(thoughts).client)

    await screen.findByText('想法 thought-link-deleted')
    expect(screen.getByText(/链接已删除（link_deleted）/)).toBeInTheDocument()
    expect(screen.getByText(/笔记已删除（note_deleted）/)).toBeInTheDocument()
  })

  it('loads live thoughts, orders ties by id, and applies practical filters', async () => {
    const thoughts = [
      makeThought({ id: 'thought-a', body: '有高亮', quote: { exact: '高亮' }, lifecycle_status: 'active', updated_at: '2026-08-10T03:00:00Z' }),
      makeThought({ id: 'thought-c', body: '有任务', lifecycle_status: 'active', updated_at: '2026-08-10T03:00:00Z' }),
      makeThought({ id: 'thought-b', body: '', quote: { exact: '仅划线' }, lifecycle_status: 'active', updated_at: '2026-08-10T03:00:00Z' }),
    ]
    const fixture = makeClient(thoughts)
    fixture.listTodos.mockResolvedValue(ok({ items: [makeTodo({ origin_host_id: 'thought-c' })] }))
    renderSurface(fixture.client, vi.fn(), 'live')

    expect(await screen.findByText('有高亮')).toBeInTheDocument()
    expect(fixture.listThoughts).toHaveBeenCalledWith({ after: undefined, limit: 30 })
    expect(fixture.listTodos).toHaveBeenCalledTimes(1)
    const rows = screen.getAllByRole('listitem').map((row) => row.textContent ?? '')
    expect(rows[0]).toContain('有任务')
    expect(rows[1]).toContain('无正文想法')
    expect(rows[2]).toContain('有高亮')

    fireEvent.change(screen.getByRole('combobox', { name: '想法筛选' }), { target: { value: 'highlighted' } })
    expect(screen.getByText('无正文想法')).toBeInTheDocument()
    expect(screen.queryByText('有任务')).not.toBeInTheDocument()
    fireEvent.change(screen.getByRole('combobox', { name: '想法筛选' }), { target: { value: 'todo' } })
    expect(screen.getByText('有任务')).toBeInTheDocument()
    expect(screen.queryByText('有高亮')).not.toBeInTheDocument()
  })

  it('does not request TODOs or expose the TODO filter when todos are unavailable', async () => {
    const fixture = makeClient([makeThought({ id: 'thought-no-todos', lifecycle_status: 'active' })])

    renderSurface(fixture.client, vi.fn(), 'live', { todos: false })

    await screen.findByText('想法 thought-no-todos')
    expect(fixture.listTodos).not.toHaveBeenCalled()
    expect(screen.queryByRole('option', { name: '未完成 TODO' })).not.toBeInTheDocument()
  })

  it('does not expose note generation or create a note when notes are unavailable', async () => {
    const fixture = makeClient([makeThought({ id: 'thought-no-notes' })])

    renderSurface(fixture.client, vi.fn(), 'history', { notes: false })

    await screen.findByText('想法 thought-no-notes')
    expect(screen.queryByRole('checkbox', { name: '选择想法 thought-no-notes' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '生成笔记' })).not.toBeInTheDocument()
    expect(fixture.createNote).not.toHaveBeenCalled()
  })

  it('deep-links live thought origins back to supported hosts', async () => {
    const thought = makeThought({ id: 'thought-origin', lifecycle_status: 'active', host_kind: 'note', host_id: 'note-origin' })
    const onNavigate = vi.fn()
    renderSurface(makeClient([thought]).client, onNavigate, 'live')

    const openSource = await screen.findByRole('button', { name: '打开出处' })
    act(() => {
      fireEvent.click(openSource)
    })

    expect(onNavigate).toHaveBeenCalledWith(
      { kind: 'library', id: 'notes' },
      { noteId: 'note-origin' },
    )
    expect(window.location.search).toBe('?tool=history&thought_view=live')
  })

  it('requests a committed history view change instead of writing history directly', async () => {
    const onNavigate = vi.fn(() => true)
    renderSurface(makeClient([]).client, onNavigate, 'history')
    await screen.findByText('没有已归档的想法')

    fireEvent.click(screen.getByRole('button', { name: '当前' }))

    expect(onNavigate).toHaveBeenCalledWith(
      { kind: 'tool', id: 'history' },
      { thoughtView: 'live' },
    )
    expect(window.location.search).toBe('?tool=history&thought_view=history')
  })

  it('shows the third superseded-version tab from the local immutable event store', async () => {
    const event = makeSupersessionEvent()
    supersessionMocks.sync.mockResolvedValue({ status: 'synced', pulled: 1, cursor: 'event-cursor' })
    supersessionMocks.list.mockResolvedValue({ ok: true, value: [event] })
    supersessionMocks.availability.mockResolvedValue({ ok: true, value: new Map([[7, 'ready']]) })
    supersessionMocks.recoveryStates.mockResolvedValue({ ok: true, value: new Map([[7, 'not-started']]) })
    supersessionMocks.commit.mockResolvedValue({ ok: true, value: { status: 'committed', sequence: 1, annotation: null } })
    const fixture = makeClient([])
    const onSyncRequest = vi.fn()
    window.addEventListener('webtag:thoughts-sync-request', onSyncRequest)

    renderSurface(fixture.client, vi.fn(), 'superseded')

    expect(await screen.findByText('loser body')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '被取代版本' })).toHaveAttribute('aria-pressed', 'true')
    expect(supersessionMocks.sync).toHaveBeenCalledTimes(1)
    expect(supersessionMocks.list).toHaveBeenCalledTimes(1)
    expect(fixture.listThoughtSupersessions).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: '恢复此版本' }))
    await waitFor(() => expect(supersessionMocks.commit).toHaveBeenCalledWith(expect.anything(), event))
    expect(onSyncRequest).toHaveBeenCalledTimes(1)
    window.removeEventListener('webtag:thoughts-sync-request', onSyncRequest)
  })

  it('does not enqueue recovery for a tombstoned host or a persisted CAS conflict', async () => {
    const event = makeSupersessionEvent()
    supersessionMocks.sync.mockResolvedValue({ status: 'synced', pulled: 1, cursor: 'event-cursor' })
    supersessionMocks.list.mockResolvedValue({ ok: true, value: [event] })
    supersessionMocks.availability.mockResolvedValue({ ok: true, value: new Map([[7, 'host-tombstoned']]) })
    supersessionMocks.recoveryStates.mockResolvedValue({ ok: true, value: new Map([[7, 'not-started']]) })
    const fixture = makeClient([])

    renderSurface(fixture.client, vi.fn(), 'superseded')

    expect(await screen.findByText(/宿主已 tombstone/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复此版本' })).toBeDisabled()
    expect(supersessionMocks.commit).not.toHaveBeenCalled()
  })

  it('surfaces a persisted recovery CAS conflict without retrying its candidate', async () => {
    const event = makeSupersessionEvent()
    supersessionMocks.sync.mockResolvedValue({ status: 'synced', pulled: 1, cursor: 'event-cursor' })
    supersessionMocks.list.mockResolvedValue({ ok: true, value: [event] })
    supersessionMocks.availability.mockResolvedValue({ ok: true, value: new Map([[7, 'ready']]) })
    supersessionMocks.recoveryStates.mockResolvedValue({ ok: true, value: new Map([[7, 'recovery-conflict']]) })
    const fixture = makeClient([])

    renderSurface(fixture.client, vi.fn(), 'superseded')

    expect(await screen.findByText(/恢复候选遇到新的当前版本/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '需刷新后重新恢复' })).toBeDisabled()
    expect(supersessionMocks.commit).not.toHaveBeenCalled()
  })
})
