import { expect, test, type Page } from '@playwright/test'
import {
  bootstrapReaderPage,
  configureReaderConnection,
  Wp26BackendFixture,
} from './wp26-fixtures'

// 原生 dialog 关闭之后不能用 toBeFocused()：CI runner 上没有
// /usr/bin/google-chrome（见 playwright.config.ts 的 executablePath 回退），
// Playwright 因此使用 chromium-headless-shell，而它在 dialog 之后不会把页面
// 重新标记为 active，断言会以 "Expected: focused / Received: inactive" 失败；
// 完整 Chrome 下则稳定通过。document.activeElement 在两种构建里都正确维护，
// 用它断言「焦点落在哪个可及名称上」语义相同且与浏览器构建无关。
async function expectFocusedAccessibleName(page: Page, name: string): Promise<void> {
  await expect
    .poll(() =>
      page.evaluate(() => {
        const active = document.activeElement as HTMLElement | null
        if (!active) return ''
        return (active.getAttribute('aria-label') ?? active.textContent ?? '').trim()
      }),
    )
    .toBe(name)
}

type JsonRecord = Record<string, unknown>

type CaptureResponse = JsonRecord & {
  inbox_id: string
  destination: string
  status: string
}

type PendingResponse = JsonRecord & { items: JsonRecord[] }

type ConfirmationResponse = JsonRecord & {
  target_kind: string
  link_id: string
  status: string
}

type LinkResponse = JsonRecord & {
  id: string
  has_content: boolean
  content_revision: number
}

type ContentResponse = JsonRecord & {
  link_id: string
  content: string
}

type PushedResponse = JsonRecord & { items: JsonRecord[] }
type SyncedResponse = JsonRecord & { items: JsonRecord[] }
type SearchResponse = JsonRecord & { thoughts: { items: JsonRecord[] } }

type NoteResponse = JsonRecord & {
  id: string
  draft_revision: number
  published_revision: number
}

type DraftResponse = JsonRecord & { draft_revision: number }

type PublishResponse = JsonRecord & {
  id: string
  published_revision: number
  draft_content: string | null
  dirty: boolean
}

type TodoItem = JsonRecord & {
  id: string
  origin_kind: string
  origin_host_id?: string | null
  done: boolean
  completed_at?: string | null
}

type TodosResponse = JsonRecord & { items: TodoItem[] }

type CompletedTodoResponse = JsonRecord & {
  done: boolean
  completed_at: string
}

type HistoryResponse = JsonRecord & { items: Array<JsonRecord & { id: string }> }

type RestoredResponse = JsonRecord & {
  link_id: string
  content_revision: number
}

type RestoredContentResponse = JsonRecord & { content: string }

type WorkflowResult = {
  capture: CaptureResponse
  pending: PendingResponse
  confirmation: ConfirmationResponse
  link: LinkResponse
  content: ContentResponse
  pushed: PushedResponse
  synced: SyncedResponse
  search: SearchResponse
  note: NoteResponse
  draft: DraftResponse
  publish: PublishResponse
  todos: TodosResponse
  completedTodo: CompletedTodoResponse
  history: HistoryResponse
  restored: RestoredResponse
  restoredContent: RestoredContentResponse
}

test('WP-26 browser workflow keeps capture, Reader surfaces, and restore identities connected', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await page.goto('/__test__/blank', { waitUntil: 'domcontentloaded' })

  const workflow = await page.evaluate(async (): Promise<WorkflowResult> => {
    function asRecord<T extends JsonRecord>(value: unknown, label: string): T {
      if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        throw new Error(`${label} response is not an object`)
      }
      return value as T
    }

    async function request(path: string, init?: RequestInit): Promise<unknown> {
      const response = await fetch(path, init)
      const body: unknown = response.status === 204 ? null : await response.json()
      if (!response.ok) throw new Error(`${init?.method ?? 'GET'} ${path}: ${response.status}`)
      return body
    }

    const capture = asRecord<CaptureResponse>(await request('/api/ingest', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        destination: 'inbox',
        sources: [{
          kind: 'browser_capture',
          url: 'https://capture.example.test/article',
          title: 'Captured article',
          text: 'Captured body from the browser extension.',
        }],
      }),
    }), 'capture')
    const pending = asRecord<PendingResponse>(await request('/api/inbox'), 'pending')
    const confirmation = asRecord<ConfirmationResponse>(await request(`/api/inbox/${capture.inbox_id}/confirm`, { method: 'POST' }), 'confirmation')
    const link = asRecord<LinkResponse>(await request(`/api/links/${confirmation.link_id}`), 'link')
    const content = asRecord<ContentResponse>(await request(`/api/links/${confirmation.link_id}/content`), 'content')

    const thought = {
      op_id: 'wp26-thought-op-1',
      device_id: 'wp26-device-1',
      operation_kind: 'add',
      annotation_id: 'wp26-thought-1',
      host_kind: 'link',
      host_id: confirmation.link_id,
      target: {
        kind: 'saved-content',
        host_id: confirmation.link_id,
        version: { content_revision: link.content_revision },
      },
      payload: {
        body: 'A synced idea becomes a searchable note.',
        quote: { exact: 'Captured body', start: 0, end: 13, prefix: '', suffix: '' },
      },
    }
    const pushed = asRecord<PushedResponse>(await request('/api/annotations/ops', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ops: [thought] }),
    }), 'pushed')
    const synced = asRecord<SyncedResponse>(await request('/api/annotations/sync?after=0&limit=100'), 'synced')
    const search = asRecord<SearchResponse>(await request('/api/search?q=searchable'), 'search')

    const note = asRecord<NoteResponse>(await request('/api/notes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: 'Search result note', content: '' }),
    }), 'note')
    const draft = asRecord<DraftResponse>(await request(`/api/notes/${note.id}/draft`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        content: 'Published note with a reanchored quote.\n\n- [ ] Follow up on the captured idea',
        expected_draft_revision: note.draft_revision,
      }),
    }), 'draft')
    const publish = asRecord<PublishResponse>(await request(`/api/notes/${note.id}/publish`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        expected_draft_revision: draft.draft_revision,
        expected_published_revision: note.published_revision,
        reanchor_ops: [{
          thought_id: 'note-thought-1',
          status: 'reanchored',
          reason: 'unique-quote',
          target: { kind: 'note', host_id: note.id, version: { note_revision: draft.draft_revision + 1 } },
          quote: { exact: 'reanchored quote', prefix: '', suffix: '' },
          range: { start: 0, end: 16 },
        }],
      }),
    }), 'publish')
    const todos = asRecord<TodosResponse>(await request('/api/todos'), 'todos')
    const noteTodo = todos.items.find((item) => item.origin_host_id === note.id)
    if (!noteTodo) throw new Error(`TODO projection for ${note.id} was not returned`)
    const completedTodo = asRecord<CompletedTodoResponse>(await request(`/api/todos/${noteTodo.id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ done: true }),
    }), 'completedTodo')

    const history = asRecord<HistoryResponse>(await request(`/api/links/${confirmation.link_id}/content-history`), 'history')
    const historyItem = history.items[0]
    if (!historyItem) throw new Error('content history did not return a restore candidate')
    const restored = asRecord<RestoredResponse>(await request(`/api/links/${confirmation.link_id}/content-history/${historyItem.id}/restore`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ expected_content_revision: link.content_revision }),
    }), 'restored')
    const restoredContent = asRecord<RestoredContentResponse>(await request(`/api/links/${confirmation.link_id}/content`), 'restoredContent')

    return {
      capture,
      pending,
      confirmation,
      link,
      content,
      pushed,
      synced,
      search,
      note,
      draft,
      publish,
      todos,
      completedTodo,
      history,
      restored,
      restoredContent,
    }
  })

  expect(workflow.capture).toMatchObject({ inbox_id: 'inbox-capture-1', destination: 'inbox', status: 'pending' })
  expect(workflow.pending.items).toEqual(expect.arrayContaining([expect.objectContaining({ id: 'inbox-capture-1', status: 'pending' })]))
  expect(workflow.confirmation).toEqual({ target_kind: 'link', link_id: 'link-capture-1', status: 'confirmed' })
  expect(workflow.link).toMatchObject({ id: 'link-capture-1', has_content: true, content_revision: 3 })
  expect(workflow.content).toMatchObject({ link_id: 'link-capture-1', content: expect.stringContaining('Captured body') })
  expect(workflow.pushed.items).toEqual([expect.objectContaining({
    contract_version: 1,
    op_id: 'wp26-thought-op-1',
    sequence: 2,
    disposition: 'applied',
    submitted_key: {
      logical_clock: 0,
      device_id: 'wp26-device-1',
      op_id: 'wp26-thought-op-1',
    },
    current_winner_key: {
      logical_clock: 0,
      device_id: 'wp26-device-1',
      op_id: 'wp26-thought-op-1',
    },
  })])
  expect(workflow.synced.items).toEqual(expect.arrayContaining([expect.objectContaining({ id: 'wp26-thought-1', body: 'A synced idea becomes a searchable note.' })]))
  expect(workflow.search.thoughts.items).toEqual(expect.arrayContaining([expect.objectContaining({ id: 'wp26-thought-1' })]))
  expect(workflow.publish).toMatchObject({ id: workflow.note.id, published_revision: 1, draft_content: null, dirty: false })
  expect(workflow.todos.items).toEqual(expect.arrayContaining([expect.objectContaining({ origin_kind: 'note', origin_host_id: workflow.note.id, done: false })]))
  expect(workflow.completedTodo).toMatchObject({ done: true, completed_at: expect.any(String) })
  expect(workflow.restored).toEqual({ link_id: 'link-capture-1', content_revision: 4 })
  expect(workflow.restoredContent.content).toBe('The original body before the edit.')

  const paths = backend.calls.map((call) => `${call.method} ${call.path}`)
  expect(paths).toEqual(expect.arrayContaining([
    'POST /api/ingest',
    'GET /api/inbox',
    'POST /api/inbox/inbox-capture-1/confirm',
    'GET /api/links/link-capture-1/content',
    'POST /api/annotations/ops',
    'GET /api/annotations/sync?after=0&limit=100',
    'GET /api/search?q=searchable',
    'PATCH /api/notes/note-capture-2/draft',
    'POST /api/notes/note-capture-2/publish',
    'GET /api/todos',
    'POST /api/links/link-capture-1/content-history/1/restore',
  ]))
  const publishCall = backend.calls.find((call) => call.path === '/api/notes/note-capture-2/publish')
  expect(publishCall?.body).toMatchObject({ reanchor_ops: [expect.objectContaining({ status: 'reanchored', reason: 'unique-quote' })] })
})

test('WP-26 Home and Feed preserve the same identity after a Feed action', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page)

  await expect(page.getByRole('heading', { level: 1, name: '今天' })).toBeVisible()
  await expect(page.getByRole('heading', { level: 2, name: '继续整理捕获内容' })).toBeVisible()
  await page.getByRole('button', { name: '完整 Feed' }).click()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toBeVisible()

  const card = page.locator('li.rvx-feed-card').filter({ hasText: 'Captured article' })
  await expect(card).toBeVisible()
  await card.getByRole('button', { name: '未读' }).click()
  await expect(card.getByRole('button', { name: '已读' })).toBeVisible()
  await card.getByRole('button', { name: '保存' }).click()
  await expect(card.getByRole('button', { name: '取消保存' })).toBeVisible()
  await card.getByRole('button', { name: '查看推荐原因：尚未阅读' }).click()
  await expect(page.getByRole('note')).toContainText('规则 unread')

  await page.getByRole('button', { name: '返回' }).click()
  await expect(page.getByRole('heading', { level: 1, name: '今天' })).toBeVisible()
  expect(backend.calls.map((call) => `${call.method} ${call.path}`)).toEqual(expect.arrayContaining([
    'PATCH /api/engagement/link-capture-1',
    'POST /api/reader-feed/feedback?item_key=link%3Alink-capture-1',
    'GET /api/home',
  ]))
  const engagementCall = backend.calls.find((call) => call.path === '/api/engagement/link-capture-1')
  expect(engagementCall?.body).toMatchObject({ read: true })
})

test('Issue 59 Home thought navigation keeps canonical URLs through history and reload', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page)

  await expect(page.getByRole('heading', { level: 1, name: '今天' })).toBeVisible()
  await page.getByRole('button', { name: '查看全部想法' }).click()
  await expect(page).toHaveURL(/\/reader\/\?tool=history&thought_view=live$/)
  await expect(page.getByRole('heading', { level: 1, name: '想法' })).toBeVisible()

  await page.goBack()
  await expect(page).toHaveURL(/\/reader\/\?surface=home$/)
  await expect(page.getByRole('heading', { level: 1, name: '今天' })).toBeVisible()

  await page.goForward()
  await expect(page).toHaveURL(/\/reader\/\?tool=history&thought_view=live$/)
  await page.reload()
  await expect(page).toHaveURL(/\/reader\/\?tool=history&thought_view=live$/)
  await expect(page.getByRole('heading', { level: 1, name: '想法' })).toBeVisible()

  await page.goBack()
  await expect(page.getByRole('heading', { level: 1, name: '今天' })).toBeVisible()
  const thought = page.locator('.rvx-thought-list li').filter({ hasText: 'Keep this captured idea.' })
  await thought.getByRole('button', { name: '回到来源' }).click()
  await expect(page).toHaveURL(/\/reader\/\?view=reading&link_id=link-capture-1$/)
})

test('Issue 141 live thought deletion confirms from the keyboard and sends one durable delete', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?tool=history&thought_view=live')

  const deleteButton = page.getByRole('button', { name: '删除想法 thought-capture-1' })
  await expect(deleteButton).toBeVisible()
  await deleteButton.focus()

  const cancelDialogPromise = page.waitForEvent('dialog')
  const cancelPressPromise = page.keyboard.press('Enter')
  const cancelDialog = await cancelDialogPromise
  expect(cancelDialog.message()).toContain('删除操作会先保存在本地')
  await cancelDialog.dismiss()
  await cancelPressPromise
  await expect(page.getByText('Keep this captured idea.')).toBeVisible()
  expect(backend.calls.filter((call) => call.path === '/api/annotations/ops')).toHaveLength(0)
  await expectFocusedAccessibleName(page, '删除想法 thought-capture-1')

  const acceptDialogPromise = page.waitForEvent('dialog')
  const acceptPressPromise = page.keyboard.press('Space')
  const acceptDialog = await acceptDialogPromise
  await acceptDialog.accept()
  await acceptPressPromise

  await expect(page.getByText('Keep this captured idea.')).not.toBeVisible()
  await expectFocusedAccessibleName(page, '当前')
  await expect.poll(() => backend.calls.filter((call) => call.path === '/api/annotations/ops').length).toBe(1)
  expect(backend.calls.find((call) => call.path === '/api/annotations/ops')?.body).toMatchObject({
    ops: [expect.objectContaining({
      annotation_id: 'thought-capture-1',
      operation_kind: 'delete',
    })],
  })
})
