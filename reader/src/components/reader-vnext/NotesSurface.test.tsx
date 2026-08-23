import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type {
  ReaderAIRequest,
  ReaderNoteHistoryResponse,
  ReaderNoteResponse,
} from '../../lib/api/types'
import type { Annotation } from '../../lib/annotation-domain'
import { IdentityLease } from '../../lib/identity'
import { err, ok } from '@webtag/api'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityLease } from '../../test/capabilities'
import { NotesSurface, type NotesLeaveResult } from './NotesSurface'

vi.mock('../../lib/user-data/thought-sync', () => ({
  listRemoteThoughtsForHost: vi.fn(async () => ({ ok: true, value: [] })),
}))

const noteAnnotationMock = vi.hoisted(() => ({
  anns: [] as Annotation[],
  add: vi.fn(async () => ({ status: 'committed' as const, annotationId: 'an-new', sequence: 1 })),
  update: vi.fn(async () => ({ status: 'committed' as const, annotationId: 'an-existing', sequence: 2 })),
  remove: vi.fn(async () => ({ status: 'committed' as const, annotationId: 'an-existing', sequence: 3 })),
}))

vi.mock('../../hooks/useNoteAnnotations', () => ({
  useNoteAnnotations: () => ({
    anns: noteAnnotationMock.anns,
    loading: false,
    error: false,
    refresh: vi.fn(async () => true),
    add: noteAnnotationMock.add,
    update: noteAnnotationMock.update,
    remove: noteAnnotationMock.remove,
  }),
}))

function note(overrides: Partial<ReaderNoteResponse> = {}): ReaderNoteResponse {
  return {
    id: 'N1',
    title: '测试笔记',
    published_content: 'published body',
    published_revision: 7,
    draft_content: null,
    draft_revision: 3,
    draft_updated_at: null,
    deleted_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    dirty: false,
    ...overrides,
  }
}

function noteWithID(id: string, overrides: Partial<ReaderNoteResponse> = {}): ReaderNoteResponse {
  return note({ id, title: `笔记 ${id}`, ...overrides })
}

function historyEntry(overrides: Partial<ReaderNoteHistoryResponse> = {}): ReaderNoteHistoryResponse {
  return {
    id: 1,
    revision: 6,
    title: '历史标题',
    content: 'historical body',
		reanchor_ops: [],
    created_at: '2026-08-09T01:00:00Z',
    ...overrides,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function makeClient(initial: ReaderNoteResponse = note(), listItems: ReaderNoteResponse[] = [initial], count = listItems.length) {
  const listNotes = vi.fn(async () => ok({ items: listItems, count }))
  const getNote = vi.fn(async () => ok(initial))
  const saveNoteDraft = vi.fn(async (
    _noteID: string,
    _request: Parameters<IdentityBoundReaderClient['saveNoteDraft']>[1],
  ) => ok(initial))
  const publishNote = vi.fn(async () => ok(note({
    published_content: initial.published_content,
    published_revision: initial.published_revision + 1,
  })))
  const listNoteHistory = vi.fn(async () => ok([historyEntry()]))
  const restoreNoteRevision = vi.fn(async () => ok(note({
    published_content: 'historical body',
    published_revision: 8,
  })))
  const discardNoteDraft = vi.fn(async () => ok(true))
  const completeReaderAI = vi.fn(async (_request: ReaderAIRequest) => ({
    ok: true as const,
    data: { enabled: true, answer: '笔记选区解读' },
  }))
  const client = {
    listNotes,
    getNote,
    saveNoteDraft,
    publishNote,
    listNoteHistory,
    restoreNoteRevision,
    discardNoteDraft,
    createNote: vi.fn(async () => ok(initial)),
    deleteNote: vi.fn(async () => ok({ host_kind: 'note', host_id: 'N1', state: 'trashed', changed: true })),
    restoreNote: vi.fn(async () => ok({ host_kind: 'note', host_id: 'N1', state: 'live', changed: true })),
    listTrash: vi.fn(async () => ok({ items: [], count: 0 })),
    purgeHost: vi.fn(async () => ok(true)),
    completeReaderAI,
    isIdentityCurrent: vi.fn(() => true),
  } as unknown as IdentityBoundReaderClient
  return {
    client,
    listNotes,
    getNote,
    saveNoteDraft,
    publishNote,
    listNoteHistory,
    restoreNoteRevision,
    discardNoteDraft,
    completeReaderAI,
  }
}

function renderNotes(client: IdentityBoundReaderClient, onPrepareToLeaveChange?: (prepare: (() => Promise<NotesLeaveResult>) | null) => void) {
  const lease = new IdentityLease({
    serverClientDataNamespace: 'server-test',
    physicalNamespace: 'physical-test',
    localEpoch: 1,
  })
  const onNavigate = vi.fn<(route: ReaderRoute) => void>()
  return render(<NotesSurface client={client} capabilityLease={enabledReaderCapabilityLease()} lease={lease} onNavigate={onNavigate} onPrepareToLeaveChange={onPrepareToLeaveChange} annotationsEnabled aiEnabled trashEnabled />)
}

async function selectPreviewRange(start: number, end: number): Promise<HTMLElement> {
  const { block, textNode } = await waitFor(() => {
    const candidate = document.querySelector<HTMLElement>('[data-hl-block="note"]')
    if (!candidate) throw new Error('note preview block is missing')
    const paragraph = candidate.querySelector('p')
    const candidateTextNode = paragraph
      ? document.createTreeWalker(paragraph, NodeFilter.SHOW_TEXT).nextNode()
      : null
    if (!candidateTextNode) throw new Error('note preview text is missing')
    return { block: candidate, textNode: candidateTextNode }
  })
  const range = document.createRange()
  range.setStart(textNode, start)
  range.setEnd(textNode, end)
  Object.defineProperty(range, 'getBoundingClientRect', {
    value: () => new DOMRect(12, 24, Math.max(1, end - start) * 10, 16),
  })
  const browserSelection = window.getSelection()
  browserSelection?.removeAllRanges()
  browserSelection?.addRange(range)
  fireEvent.mouseUp(block)
  return block
}

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  noteAnnotationMock.anns = []
  noteAnnotationMock.add.mockClear()
  noteAnnotationMock.update.mockClear()
  noteAnnotationMock.remove.mockClear()
})

describe('NotesSurface draft barriers', () => {
  it('renders the capability-owned sidebar create control as an accessible icon button and disables it while creating', async () => {
    const onCreateNote = vi.fn()
    const fixture = makeClient()
    render(<NotesSurface
      client={fixture.client}
      capabilityLease={enabledReaderCapabilityLease()}
      lease={new IdentityLease({ serverClientDataNamespace: 'server-test', physicalNamespace: 'physical-test', localEpoch: 1 })}
      onNavigate={() => {}}
      onCreateNote={onCreateNote}
      creatingNote
      annotationsEnabled
      aiEnabled
      trashEnabled
    />)

    const button = await screen.findByRole('button', { name: '新建笔记' })
    expect(button).toHaveAttribute('title', '新建笔记')
    expect(button).toBeDisabled()
    expect(button.querySelector('svg')).not.toBeNull()
  })

  it('hides the create icon when MainView did not grant the notes capability', async () => {
    const fixture = makeClient()
    renderNotes(fixture.client)
    await screen.findByRole('textbox', { name: '笔记内容' })
    expect(screen.queryByRole('button', { name: '新建笔记' })).not.toBeInTheDocument()
  })

  it('hides Trash entry points and sends no Trash request when Trash is unavailable', async () => {
    const fixture = makeClient()
    render(
      <NotesSurface
        client={fixture.client}
        capabilityLease={enabledReaderCapabilityLease({ trash: false })}
        lease={new IdentityLease({ serverClientDataNamespace: 'server-test', physicalNamespace: 'physical-test', localEpoch: 1 })}
        onNavigate={() => {}}
        annotationsEnabled
        aiEnabled
        trashEnabled={false}
      />,
    )

    await screen.findByRole('textbox', { name: '笔记内容' })
    // 只断言「笔记视图」这一组里没有回收站标签。全局导航里另有一个通用回收站
    // 入口，它与笔记的 trashEnabled 能力无关，不该被这条断言连坐。
    expect(within(screen.getByRole('tablist', { name: '笔记视图' })).queryByRole('button', { name: /回收站/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '移入回收站' })).not.toBeInTheDocument()
    expect(fixture.client.listTrash).not.toHaveBeenCalled()
    expect(fixture.client.deleteNote).not.toHaveBeenCalled()
  })

  it('flushes a debounce-pending edit through CAS before allowing leave', async () => {
    let prepare: (() => Promise<NotesLeaveResult>) | null = null
    const fixture = makeClient()
    fixture.saveNoteDraft.mockResolvedValue(ok(note({ draft_content: 'last input', draft_revision: 4, dirty: true })))
    renderNotes(fixture.client, (next) => { prepare = next })

    fireEvent.change(await screen.findByRole('textbox', { name: '笔记内容' }), { target: { value: 'last input' } })
    await waitFor(() => expect(prepare).not.toBeNull())
    await expect(prepare!()).resolves.toEqual({ status: 'ready' })
    expect(fixture.saveNoteDraft).toHaveBeenCalledWith('N1', {
      content: 'last input', expected_draft_revision: 3,
    })
  })

  it('soft-deletes an unpublished canonical-empty note exactly once when leaving', async () => {
    let prepare: (() => Promise<NotesLeaveResult>) | null = null
    const fixture = makeClient(note({ published_content: '', published_revision: 0, draft_revision: 0 }))
    renderNotes(fixture.client, (next) => { prepare = next })

    await screen.findByRole('textbox', { name: '笔记内容' })
    await expect(prepare!()).resolves.toEqual({ status: 'ready' })
    await expect(prepare!()).resolves.toEqual({ status: 'ready' })
    expect((fixture.client.deleteNote as ReturnType<typeof vi.fn>)).toHaveBeenCalledTimes(1)
  })

  it('discards an empty draft while preserving an already published note', async () => {
    let prepare: (() => Promise<NotesLeaveResult>) | null = null
    const fixture = makeClient(note({ draft_content: '\n\t ', draft_revision: 9, dirty: true }))
    renderNotes(fixture.client, (next) => { prepare = next })

    await screen.findByRole('textbox', { name: '笔记内容' })
    await expect(prepare!()).resolves.toEqual({ status: 'ready' })
    expect(fixture.discardNoteDraft).toHaveBeenCalledWith('N1', 9)
  })

  it('keeps Trash pagination independent and renders a selected deleted note as read-only', async () => {
    const fixture = makeClient()
    const trashed = note({
      id: 'T1',
      title: '已删除笔记',
      published_content: '已删除正文',
      deleted_at: '2026-08-11T01:00:00Z',
    })
    const listTrash = fixture.client.listTrash as ReturnType<typeof vi.fn>
    const getNote = fixture.client.getNote as ReturnType<typeof vi.fn>
    listTrash
      .mockResolvedValueOnce(ok({
        items: [{ host_kind: 'note', host_id: 'T1', title: '已删除笔记', trashed_at: '2026-08-11T01:00:00Z' }],
        count: 2,
        next_cursor: 'trash-next',
      }))
      .mockResolvedValueOnce(ok({ items: [], count: 2 }))
    getNote.mockResolvedValue(ok(trashed))
    renderNotes(fixture.client)

    fireEvent.click(within(await screen.findByRole('tablist', { name: '笔记视图' })).getByRole('button', { name: /回收站/ }))
    const trashedButton = await screen.findByRole('button', { name: /已删除笔记/ })
    fireEvent.click(trashedButton)

    expect(await screen.findByText('已删除 · 只读')).toBeInTheDocument()
    expect(screen.getByText('已删除正文')).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '笔记内容' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '更多' }))
    await waitFor(() => expect(listTrash).toHaveBeenLastCalledWith({ hostKind: 'note', after: 'trash-next', limit: 50 }))
  })

  it('waits for a pending autosave before restoring history and blocks the old generation afterwards', async () => {
    vi.useFakeTimers()
    const save = deferred<ReturnType<typeof ok<ReaderNoteResponse>>>()
    const restore = deferred<ReturnType<typeof ok<ReaderNoteResponse>>>()
    const fixture = makeClient()
    fixture.saveNoteDraft.mockReturnValue(save.promise)
    fixture.restoreNoteRevision.mockReturnValue(restore.promise)
    renderNotes(fixture.client)

    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    const textarea = screen.getByRole('textbox', { name: '笔记内容' })
    fireEvent.change(textarea, { target: { value: 'edited before restore' } })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1200)
    })
    expect(fixture.saveNoteDraft).toHaveBeenCalledWith('N1', {
      content: 'edited before restore',
      expected_draft_revision: 3,
    })
    vi.useRealTimers()

    fireEvent.click(screen.getByRole('button', { name: '历史版本' }))
    expect(await screen.findByText('历史标题')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '恢复到此版本' }))
    expect(fixture.restoreNoteRevision).not.toHaveBeenCalled()

    await act(async () => {
      save.resolve(ok(note({
        draft_content: 'edited before restore',
        draft_revision: 4,
        dirty: true,
      })))
      await save.promise
    })
    await waitFor(() => expect(fixture.restoreNoteRevision).toHaveBeenCalledWith('N1', 6, {
      expected_draft_revision: 4,
      expected_published_revision: 7,
      reanchor_ops: [],
    }))

    await act(async () => {
      restore.resolve(ok(note({
        published_content: 'historical body',
        published_revision: 8,
        draft_content: null,
        draft_revision: 4,
        dirty: false,
      })))
      await restore.promise
    })
    await waitFor(() => expect(textarea).toHaveValue('historical body'))
    expect(screen.getByText('已发布')).toBeInTheDocument()

    expect(fixture.saveNoteDraft).toHaveBeenCalledTimes(1)
  })

  it('flushes edited content before publish and uses the returned draft revision CAS', async () => {
    const save = deferred<ReturnType<typeof ok<ReaderNoteResponse>>>()
    const fixture = makeClient()
    fixture.saveNoteDraft.mockReturnValue(save.promise)
    fixture.publishNote.mockResolvedValue(ok(note({
      published_content: 'content to publish',
      published_revision: 8,
      draft_content: null,
      draft_revision: 4,
      dirty: false,
    })))
    renderNotes(fixture.client)

    const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
    fireEvent.change(textarea, { target: { value: 'content to publish' } })
    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    await waitFor(() => expect(fixture.saveNoteDraft).toHaveBeenCalledWith('N1', {
      content: 'content to publish',
      expected_draft_revision: 3,
    }))
    expect(fixture.publishNote).not.toHaveBeenCalled()

    await act(async () => {
      save.resolve(ok(note({
        draft_content: 'content to publish',
        draft_revision: 4,
        dirty: true,
      })))
      await save.promise
    })
    await waitFor(() => expect(fixture.publishNote).toHaveBeenCalledWith('N1', {
      expected_draft_revision: 4,
      expected_published_revision: 7,
      reanchor_ops: [],
    }))

    await waitFor(() => expect(screen.getByText('已发布')).toBeInTheDocument())
    expect(textarea).toHaveValue('content to publish')
  })

  it('opens complete immutable Markdown before restoring a history revision', async () => {
    const fixture = makeClient()
    fixture.listNoteHistory.mockResolvedValue(ok([historyEntry({ content: '# Previous heading\n\nFull historical Markdown body.' })]))
    renderNotes(fixture.client)

    fireEvent.click(await screen.findByRole('button', { name: '历史版本' }))
	await screen.findByText('历史标题')
    fireEvent.click((await screen.findAllByRole('button', { name: '预览' })).at(-1)!)

    expect(await screen.findByRole('region', { name: '历史版本 v6 预览' })).toHaveTextContent('Full historical Markdown body.')
    expect(fixture.restoreNoteRevision).not.toHaveBeenCalled()
  })

  it('uses the server notes count and projects the published first paragraph and unfinished TODO count', async () => {
    const fixture = makeClient(note({
      published_content: '# 标题\n\n首段摘录\n\n- [ ] 未完成任务\n- [x] 已完成任务',
    }), [noteWithID('N1', {
      published_content: '# 标题\n\n首段摘录\n\n- [ ] 未完成任务\n- [x] 已完成任务',
    }), noteWithID('N2', { published_content: '另一篇首段' })], 7)
    renderNotes(fixture.client)

    expect(await screen.findByText('首段摘录')).toBeInTheDocument()
    expect(screen.getByText(/1 个待办/)).toBeInTheDocument()
    expect(screen.getByText(/7 篇/)).toBeInTheDocument()
  })

  it('guards an internal note switch when the current draft is dirty', async () => {
    const first = noteWithID('N1', { title: '第一篇' })
    const second = noteWithID('N2', { title: '第二篇', published_content: '第二篇内容' })
    const fixture = makeClient(first, [first, second])
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    renderNotes(fixture.client)

    const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
    fireEvent.change(textarea, { target: { value: '尚未保存的内容' } })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    expect(confirm).toHaveBeenCalledWith('当前笔记草稿有未保存修改，确定切换？')
    expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('尚未保存的内容')
    expect(fixture.saveNoteDraft).not.toHaveBeenCalled()
  })

  it('persists a confirmed dirty switch before changing the selected note', async () => {
    const first = noteWithID('N1', { title: '第一篇' })
    const second = noteWithID('N2', { title: '第二篇', published_content: '第二篇内容' })
    const fixture = makeClient(first, [first, second])
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fixture.saveNoteDraft.mockResolvedValue(ok(noteWithID('N1', {
      title: '第一篇',
      draft_content: '确认后的草稿',
      draft_revision: 4,
      dirty: true,
    })))
    renderNotes(fixture.client)

    const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
    fireEvent.change(textarea, { target: { value: '确认后的草稿' } })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    await waitFor(() => expect(fixture.saveNoteDraft).toHaveBeenCalledWith('N1', {
      content: '确认后的草稿',
      expected_draft_revision: 3,
    }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('第二篇内容'))
  })

  it('automatically discards an empty draft when leaving the note normally', async () => {
    const first = noteWithID('N1', {
      published_content: '已发布内容',
      draft_content: '',
      draft_revision: 8,
      dirty: true,
    })
    const second = noteWithID('N2', { title: '第二篇', published_content: '第二篇内容' })
    const fixture = makeClient(first, [first, second])
    renderNotes(fixture.client)

    await screen.findByRole('textbox', { name: '笔记内容' })
    fireEvent.click(screen.getByRole('button', { name: /第二篇/ }))

    await waitFor(() => expect(fixture.discardNoteDraft).toHaveBeenCalledWith('N1', 8))
    expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('第二篇内容')
  })

  it('captures selections only in the published preview and renders note marks', async () => {
    const fixture = makeClient()
    renderNotes(fixture.client)
    fireEvent.click(await screen.findByRole('button', { name: '预览' }))

    await selectPreviewRange(0, 4)

    fireEvent.click(await screen.findByRole('button', { name: '划线' }))
    expect(noteAnnotationMock.add).toHaveBeenCalledWith(expect.objectContaining({
      blockKey: 'note',
      start: 0,
      end: 4,
      text: 'publ',
      quote: expect.objectContaining({ exact: 'publ' }),
    }))

    noteAnnotationMock.anns = [{
      id: 'an-existing',
      blockKey: 'note',
      start: 0,
      end: 4,
      text: 'publ',
      note: '一个想法',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceNoteRevision: 7,
      quote: { exact: 'publ', prefix: '', suffix: 'ished body' },
    }]
    fireEvent.click(screen.getByRole('button', { name: '预览' }))
    fireEvent.click(screen.getByRole('button', { name: '编辑' }))
    fireEvent.click(screen.getByRole('button', { name: '预览' }))
    expect(document.querySelector('mark[data-ann="an-existing"]')).toBeInTheDocument()
  })

  it('restores unsaved text, selection, and scroll after an Edit/Preview round trip', async () => {
    const fixture = makeClient(note({ published_content: '0123456789\n'.repeat(80) }))
    renderNotes(fixture.client)

    let textarea = await screen.findByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: `draft-${textarea.value}` } })
    textarea.setSelectionRange(7, 19)
    textarea.scrollTop = 180
    fireEvent.scroll(textarea)

    fireEvent.click(screen.getByRole('button', { name: '预览' }))
    expect(screen.getByRole('article', { name: '笔记预览' })).toHaveTextContent('draft-0123456789')
    fireEvent.click(screen.getByRole('button', { name: '编辑' }))

    textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    expect(textarea.value).toMatch(/^draft-0123456789/)
    expect(textarea.selectionStart).toBe(7)
    expect(textarea.selectionEnd).toBe(19)
    expect(textarea.scrollTop).toBe(180)
    expect(fixture.saveNoteDraft).not.toHaveBeenCalled()
  })

  it('shows the fixed Notes actions for one character and hides them for an empty selection', async () => {
    const fixture = makeClient(note({ published_content: '选区操作顺序' }))
    renderNotes(fixture.client)
    fireEvent.click(await screen.findByRole('button', { name: '预览' }))

    const block = await selectPreviewRange(0, 1)
    const popover = document.querySelector<HTMLElement>('.sel-pop')
    if (!popover) throw new Error('selection popover is missing')
    expect(within(popover).getAllByRole('button').map((button) => button.textContent?.trim())).toEqual([
      '划线', '写想法', '问 AI', '复制',
    ])
    expect(within(popover).queryByRole('button', { name: '翻译' })).not.toBeInTheDocument()

    window.getSelection()?.removeAllRanges()
    fireEvent.mouseUp(block)
    expect(document.querySelector('.sel-pop')).not.toBeInTheDocument()
  })

  it('sends only selected Note text and minimal source metadata to AI', async () => {
    const fullNote = 'VISIBLE UNSELECTED_PRIVATE_SENTINEL'
    const fixture = makeClient(note({ published_content: fullNote }))
    renderNotes(fixture.client)
    fireEvent.click(await screen.findByRole('button', { name: '预览' }))
    await selectPreviewRange(0, 7)
    fireEvent.click(screen.getByRole('button', { name: '问 AI' }))

    await waitFor(() => expect(fixture.completeReaderAI).toHaveBeenCalledOnce())
    const request = fixture.completeReaderAI.mock.calls[0]?.[0]
    expect(request).toBeDefined()
    expect(request).toMatchObject({
      scope: 'selection',
      selected_text: 'VISIBLE',
    })
    expect(request).not.toHaveProperty('link_id')
    expect(request?.prompt).toContain(
      'Selection source metadata: {"source_type":"note","host_id":"N1","version":{"note_revision":7},"range":{"start":0,"end":7}}',
    )
    expect(JSON.stringify(request)).not.toContain('UNSELECTED_PRIVATE_SENTINEL')
    expect(JSON.stringify(request)).not.toContain(fullNote)
    expect(await screen.findByText('笔记选区解读')).toBeInTheDocument()
  })

  it('starts selection AI for an existing Note annotation', async () => {
    noteAnnotationMock.anns = [{
      id: 'an-existing',
      blockKey: 'note',
      start: 0,
      end: 7,
      text: 'VISIBLE',
      note: '已有想法',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceNoteRevision: 7,
      quote: { exact: 'VISIBLE', prefix: '', suffix: ' remainder' },
    }]
    const fixture = makeClient(note({ published_content: 'VISIBLE remainder' }))
    renderNotes(fixture.client)
    fireEvent.click(await screen.findByRole('button', { name: '预览' }))

    await selectPreviewRange(0, 7)
    fireEvent.click(screen.getByRole('button', { name: '问 AI' }))

    await waitFor(() => expect(fixture.completeReaderAI).toHaveBeenCalledOnce())
    expect(fixture.completeReaderAI.mock.calls[0]?.[0]).toMatchObject({
      scope: 'selection',
      selected_text: 'VISIBLE',
    })
    expect(noteAnnotationMock.add).not.toHaveBeenCalled()
    expect(await screen.findByText('笔记选区解读')).toBeInTheDocument()
  })

  it('closes the annotation panel and releases its grid column when returning to Edit', async () => {
    noteAnnotationMock.anns = [{
      id: 'an-existing',
      blockKey: 'note',
      start: 0,
      end: 4,
      text: 'publ',
      note: '已有想法',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceNoteRevision: 7,
      quote: { exact: 'publ', prefix: '', suffix: 'ished body' },
    }]
    const fixture = makeClient()
    const view = renderNotes(fixture.client)
    fireEvent.click(await screen.findByRole('button', { name: '预览' }))
    const mark = await waitFor(() => {
      const candidate = view.container.querySelector<HTMLElement>('mark[data-ann="an-existing"]')
      if (!candidate) throw new Error('existing annotation mark is missing')
      return candidate
    })
    fireEvent.click(mark)
    expect(view.container.querySelector('.note-panel')).toBeInTheDocument()
    expect(view.container.querySelector('.notes-split')).toHaveClass('tool-open')

    fireEvent.click(screen.getByRole('button', { name: '编辑' }))

    expect(view.container.querySelector('.note-panel')).not.toBeInTheDocument()
    expect(view.container.querySelector('.notes-split')).not.toHaveClass('tool-open')
  })

  it.each([
    {
      command: 'Enter list continuation',
      initial: '- item',
      expected: '- item\n- ',
      run: (textarea: HTMLTextAreaElement) => {
        textarea.setSelectionRange(textarea.value.length, textarea.value.length)
        fireEvent.keyDown(textarea, { key: 'Enter' })
      },
    },
    {
      command: 'Mod+B',
      initial: 'word',
      expected: '**word**',
      run: (textarea: HTMLTextAreaElement) => {
        textarea.setSelectionRange(0, textarea.value.length)
        fireEvent.keyDown(textarea, { key: 'b', ctrlKey: true })
      },
    },
    {
      command: 'list indentation',
      initial: '- one\n- two',
      expected: '  - one\n  - two',
      run: (textarea: HTMLTextAreaElement) => {
        textarea.setSelectionRange(0, textarea.value.length)
        fireEvent.keyDown(textarea, { key: 'Tab' })
      },
    },
    {
      command: 'slash command',
      initial: '',
      expected: '# ',
      run: (textarea: HTMLTextAreaElement) => {
        fireEvent.change(textarea, { target: { value: '/h1' } })
        fireEvent.click(screen.getByRole('option', { name: /一级标题/ }))
      },
    },
  ])('persists $command as one autosave operation', async ({ initial, expected, run }) => {
    vi.useFakeTimers()
    const fixture = makeClient(note({ published_content: initial }))
    fixture.saveNoteDraft.mockImplementation(async (_noteID, request) => ok(note({
      published_content: initial,
      draft_content: request.content,
      draft_revision: 4,
      dirty: true,
    })))
    renderNotes(fixture.client)
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement

    run(textarea)
    expect(textarea).toHaveValue(expected)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1200)
    })

    expect(fixture.saveNoteDraft).toHaveBeenCalledOnce()
    expect(fixture.saveNoteDraft).toHaveBeenCalledWith('N1', {
      content: expected,
      expected_draft_revision: 3,
    })
  })

  it('keeps command output in the editor when autosave fails', async () => {
    vi.useFakeTimers()
    const fixture = makeClient(note({ published_content: 'word' }))
    fixture.saveNoteDraft.mockResolvedValue(err({
      kind: 'other',
      status: 409,
      message: 'draft CAS rejected',
    }))
    renderNotes(fixture.client)
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, textarea.value.length)
    fireEvent.keyDown(textarea, { key: 'i', ctrlKey: true })

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1200)
    })

    expect(fixture.saveNoteDraft).toHaveBeenCalledOnce()
    expect(textarea).toHaveValue('*word*')
    expect(screen.getByText('内容已经被其他窗口更新，请刷新后重试。')).toBeInTheDocument()
    expect(screen.getByText('等待自动保存')).toBeInTheDocument()
  })
})
