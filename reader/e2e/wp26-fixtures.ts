import type { Page, Route } from '@playwright/test'

export const WP26_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

const NOW = '2026-08-10T08:00:00Z'

export interface Wp26Call {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

export interface Wp26BackendOptions {
  readonly ai?: boolean
}

interface Wp26Inbox {
  id: string
  url: string
  source_kind: string
  title: string
  body: string
  note: string
  summary: string
  suggested_tags: string[]
  proposal_signals: Record<string, unknown>
  proposal_status: 'pending' | 'running' | 'completed' | 'failed'
  tags: string[]
  category_ids: string[]
  status: 'pending' | 'confirmed' | 'discarded'
  metadata_revision: number
  job_id: string | null
  expires_at: string | null
  expired_at: string | null
  expired: boolean
  created_at: string
  updated_at: string
}

interface Wp26Link {
  id: string
  url: string
  title: string
  summary: string
  description: string | null
  tags: string[]
  content_type: string
  status: 'done'
  domain: string
  path_depth: number
  parent_id: string | null
  parent_path: string | null
  created_at: string
  updated_at: string
  fetcher_type: string
  is_low_confidence: boolean
  low_confidence_reason: string | null
  error_category: string | null
  error_msg: string | null
  metadata_revision: number
  has_content: boolean
  content: string
  content_revision: number
}

interface Wp26Thought {
  contract_version: 1
  id: string
  host_kind: string
  host_id: string
  link_id: string | null
  target: unknown
  quote: unknown
  body: string
  source: string
  deleted: boolean
  last_sequence: number
  winner_key: {
    logical_clock: number
    device_id: string
    op_id: string
  }
  created_at: string
  updated_at: string
}

interface Wp26Note {
  id: string
  title: string
  published_content: string
  published_revision: number
  draft_content: string | null
  draft_revision: number
  draft_updated_at: string | null
  deleted_at: string | null
  created_at: string
  updated_at: string
  dirty: boolean
}

interface Wp26Todo {
  id: string
  text: string
  due_at: string | null
  done: boolean
  origin_kind: string
  origin_host_kind: string | null
  origin_host_id: string | null
  origin_ref: unknown
  host_revision: number
  completed_at: string | null
  created_at: string
  updated_at: string
  expired: boolean
}

interface Wp26FeedItem {
  key: string
  source: string
  actions: string[]
  title: string
  summary: string
  url: string
  link_id: string | null
  inbox_id: string | null
  feed_item_id: string | null
  read: boolean
  read_later: boolean
  saved: boolean
  score: number
  score_contributions: {
    pending_confirmation: number
    saved_library: number
    subscription_recent: number
    unread: number
    read_later: number
    chronological_fallback: number
  }
  enabled_score_signals: string[]
  reason_code: string
  reason_params: Record<string, unknown>
  reason_contribution: number
  reason_text: string
  published_at: string | null
  event_at: string
  created_at: string
}

interface Wp26History {
  id: number
  revision: number
  content: string | null
  content_document: string | null
  content_format: string
  content_source: string
  created_at: string
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T
}

function responseHeaders(): Record<string, string> {
  return {
    'Content-Type': 'application/json',
    'Cache-Control': 'no-store',
    'X-WebTag-Data-Namespace': WP26_NAMESPACE,
  }
}

function requestBody(route: Route): unknown {
  const raw = route.request().postData()
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return null
  }
}

async function fulfill(route: Route, status: number, body: unknown): Promise<void> {
  await route.fulfill({
    status,
    headers: responseHeaders(),
    body: status === 204 ? '' : JSON.stringify(body),
  })
}

function makeInbox(overrides: Partial<Wp26Inbox> = {}): Wp26Inbox {
  return {
    id: 'inbox-capture-1',
    url: 'https://capture.example.test/article',
    source_kind: 'browser_capture',
    title: 'Captured article',
    body: 'Captured body from the browser extension.',
    note: '',
    summary: 'Captured summary',
    suggested_tags: ['capture'],
    proposal_signals: {},
    proposal_status: 'completed',
    tags: ['capture'],
    category_ids: [],
    status: 'pending',
    metadata_revision: 1,
    job_id: null,
    expires_at: '2026-09-09T08:00:00Z',
    expired_at: null,
    expired: false,
    created_at: NOW,
    updated_at: NOW,
    ...overrides,
  }
}

function makeLink(overrides: Partial<Wp26Link> = {}): Wp26Link {
  return {
    id: 'link-capture-1',
    url: 'https://capture.example.test/article',
    title: 'Captured article',
    summary: 'Captured summary',
    description: null,
    tags: ['capture'],
    content_type: 'article',
    status: 'done',
    domain: 'capture.example.test',
    path_depth: 1,
    parent_id: null,
    parent_path: null,
    created_at: NOW,
    updated_at: NOW,
    fetcher_type: 'stored',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    metadata_revision: 1,
    has_content: true,
    content: 'Captured body from the browser extension.',
    content_revision: 3,
    ...overrides,
  }
}

function makeThought(overrides: Partial<Wp26Thought> = {}): Wp26Thought {
  return {
    contract_version: 1,
    id: 'thought-capture-1',
    host_kind: 'link',
    host_id: 'link-capture-1',
    link_id: 'link-capture-1',
    target: {
      kind: 'saved-content',
      host_id: 'link-capture-1',
      version: { content_revision: 1 },
    },
    quote: {
      exact: 'Captured body',
      prefix: '',
      suffix: ' from the browser extension.',
      start: 0,
      end: 13,
    },
    body: 'Keep this captured idea.',
    source: 'user',
    deleted: false,
    last_sequence: 1,
    // This seeded record predates the v1 client clock. The production API
    // preserves its zero key explicitly rather than omitting it.
    winner_key: {
      logical_clock: 0,
      device_id: 'legacy-wp26-device',
      op_id: 'legacy-wp26-thought-capture-1',
    },
    created_at: NOW,
    updated_at: NOW,
    ...overrides,
  }
}

function makeNote(overrides: Partial<Wp26Note> = {}): Wp26Note {
  return {
    id: 'note-capture-1',
    title: 'Captured idea note',
    published_content: 'Published note content.',
    published_revision: 1,
    draft_content: null,
    draft_revision: 1,
    draft_updated_at: null,
    deleted_at: null,
    created_at: NOW,
    updated_at: NOW,
    dirty: false,
    ...overrides,
  }
}

function makeTodo(overrides: Partial<Wp26Todo> = {}): Wp26Todo {
  return {
    id: 'todo-note-1',
    text: 'Follow up on the captured idea',
    due_at: null,
    done: false,
    origin_kind: 'note',
    origin_host_kind: 'note',
    origin_host_id: 'note-capture-1',
    origin_ref: { block_ref: 'task:follow-up' },
    host_revision: 2,
    completed_at: null,
    created_at: NOW,
    updated_at: NOW,
    expired: false,
    ...overrides,
  }
}

function makeFeedItem(overrides: Partial<Wp26FeedItem> = {}): Wp26FeedItem {
  return {
    key: 'link:link-capture-1',
    source: 'reading',
    actions: ['read', 'read_later', 'save', 'unsave', 'hide', 'not_interested', 'open', 'open_workspace'],
    title: 'Captured article',
    summary: 'Captured summary',
    url: 'https://capture.example.test/article',
    link_id: 'link-capture-1',
    inbox_id: null,
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    score: 20,
    score_contributions: {
      pending_confirmation: 0,
      saved_library: 0,
      subscription_recent: 0,
      unread: 20,
      read_later: 0,
      chronological_fallback: 0,
    },
    enabled_score_signals: ['unread'],
    reason_code: 'unread',
    reason_params: { read: false },
    reason_contribution: 20,
    reason_text: '来自最近捕获的内容',
    published_at: null,
    event_at: NOW,
    created_at: NOW,
    ...overrides,
  }
}

function makeHistory(): Wp26History[] {
  return [
    {
      id: 1,
      revision: 1,
      content: 'The original body before the edit.',
      content_document: null,
      content_format: 'plain',
      content_source: 'fetched',
      created_at: NOW,
    },
    {
      id: 2,
      revision: 2,
      content: 'The edited body that is currently visible.',
      content_document: null,
      content_format: 'plain',
      content_source: 'user',
      created_at: NOW,
    },
  ]
}

export class Wp26BackendFixture {
  readonly calls: Wp26Call[] = []
  readonly inbox = makeInbox()
  readonly links = new Map<string, Wp26Link>([['link-capture-1', makeLink()]])
  readonly thoughts = new Map<string, Wp26Thought>()
  readonly notes = new Map<string, Wp26Note>()
  readonly todos = new Map<string, Wp26Todo>()
  readonly feed = new Map<string, Wp26FeedItem>([['link:link-capture-1', makeFeedItem()]])
  readonly histories = new Map<string, Wp26History[]>([['link-capture-1', makeHistory()]])
  readonly engagement = new Map<string, { read: boolean; read_later: boolean; progress: number }>([
    ['link-capture-1', { read: false, read_later: false, progress: 0 }],
  ])
  private thoughtSequence = 0
  private nextNoteID = 2
  private nextTodoID = 2
  private readonly aiEnabled: boolean

  constructor(options: Wp26BackendOptions = {}) {
    this.aiEnabled = options.ai === true
    this.thoughts.set('thought-capture-1', makeThought())
    this.thoughtSequence = 1
    this.notes.set('note-capture-1', makeNote())
    this.todos.set('todo-note-1', makeTodo())
  }

  async install(page: Page): Promise<void> {
    await page.route('**/api/**', (route) => this.handle(route))
  }

  async handle(route: Route): Promise<void> {
    const request = route.request()
    const url = new URL(request.url())
    if (!url.pathname.startsWith('/api/')) {
      await route.continue()
      return
    }
    const body = requestBody(route)
    this.calls.push({ method: request.method(), path: `${url.pathname}${url.search}`, body: clone(body) })

    if (url.pathname === '/api/session' && request.method() === 'GET') {
      await fulfill(route, 200, {
        client_data_namespace: WP26_NAMESPACE,
        representation_contract: 'v3',
      })
      return
    }
    if (url.pathname === '/api/capabilities' && request.method() === 'GET') {
      await fulfill(route, 200, {
        library_kinds: true,
        site_library: true,
        site_auto_classification: true,
        site_management: true,
        site_advanced_management: true,
        archive_versions: [1, 2],
        reader_vnext: true,
        reader: {
          annotations: true,
          notes: true,
          inbox: true,
          todos: true,
          engagement: true,
          home: true,
          feed: true,
          ai: this.aiEnabled,
          semantic: true,
          activity: true,
          history: true,
          trash: true,
        },
      })
      return
    }
    if (url.pathname === '/api/ingest' && request.method() === 'POST') {
      const source = this.sourceFromIngest(body)
      Object.assign(this.inbox, source)
      await fulfill(route, 202, {
        inbox_id: this.inbox.id,
        destination: 'inbox',
        status: 'pending',
      })
      return
    }
    if (url.pathname === '/api/inbox' && request.method() === 'GET') {
      const partition = url.searchParams.get('partition') === 'expired' ? 'expired' : 'active'
      const active = this.inbox.status === 'pending' && !this.inbox.expired
      const expired = this.inbox.status === 'pending' && this.inbox.expired
      await fulfill(route, 200, {
        items: partition === 'expired'
          ? (expired ? [clone(this.inbox)] : [])
          : (active ? [clone(this.inbox)] : []),
        active_count: active ? 1 : 0,
        expired_count: expired ? 1 : 0,
      })
      return
    }
    if (url.pathname === '/api/inbox' && request.method() === 'POST') {
      const input = isRecord(body) ? body : {}
      Object.assign(this.inbox, {
        url: stringValue(input.url) || this.inbox.url,
        source_kind: stringValue(input.source_kind) || 'manual',
        title: stringValue(input.title) || this.inbox.title,
        body: stringValue(input.body),
        summary: stringValue(input.summary),
        tags: stringArray(input.tags),
        suggested_tags: stringArray(input.tags),
        status: 'pending',
        updated_at: NOW,
      })
      await fulfill(route, 201, clone(this.inbox))
      return
    }

    const inboxID = matchID(url.pathname, '/api/inbox/')
    if (inboxID && inboxID !== 'bulk' && inboxID !== 'jobs') {
      if (request.method() === 'GET') {
        await fulfill(route, inboxID === this.inbox.id ? 200 : 404, clone(this.inbox))
        return
      }
      if (request.method() === 'POST' && url.pathname.endsWith('/confirm')) {
        this.inbox.status = 'confirmed'
        const link = this.links.get('link-capture-1') as Wp26Link
        await fulfill(route, 200, { target_kind: 'link', link_id: link.id, status: 'confirmed' })
        return
      }
      if (request.method() === 'POST' && url.pathname.endsWith('/discard')) {
        this.inbox.status = 'discarded'
        await fulfill(route, 204, null)
        return
      }
      if (request.method() === 'POST' && url.pathname.endsWith('/restore')) {
        this.inbox.status = 'pending'
        this.inbox.expired = false
        this.inbox.expired_at = null
        await fulfill(route, 204, null)
        return
      }
      if (request.method() === 'PATCH') {
        const input = isRecord(body) ? body : {}
        Object.assign(this.inbox, {
          title: stringValue(input.title) || this.inbox.title,
          summary: stringValue(input.summary),
          tags: stringArray(input.tags),
          metadata_revision: this.inbox.metadata_revision + 1,
        })
        await fulfill(route, 200, clone(this.inbox))
        return
      }
    }

    if (url.pathname === '/api/inbox/bulk/confirm' && request.method() === 'POST') {
      this.inbox.status = 'confirmed'
      await fulfill(route, 200, {
        atomic: true,
        items: [{ inbox_id: this.inbox.id, status: 'confirmed', link_id: 'link-capture-1' }],
      })
      return
    }

    // The pending surface loads its category chips beside the inbox list, so
    // the fixture answers with an empty catalogue instead of failing the load.
    if (url.pathname === '/api/categories' && request.method() === 'GET') {
      await fulfill(route, 200, { items: [] })
      return
    }

    const linkID = matchID(url.pathname, '/api/links/')
    if (linkID) {
      const link = this.links.get(linkID)
      if (url.pathname.endsWith('/content')) {
        await fulfill(route, link ? 200 : 404, link ? {
          link_id: link.id,
          content: link.content,
          content_format: 'plain',
          fetcher_type: link.fetcher_type,
          content_source: 'fetched',
          content_revision: link.content_revision,
        } : { error: { message: 'link not found' } })
        return
      }
      if (url.pathname.endsWith('/content-history') && request.method() === 'GET') {
        await fulfill(route, link ? 200 : 404, { items: clone(this.histories.get(linkID) ?? []) })
        return
      }
      if (url.pathname.includes('/content-history/') && url.pathname.endsWith('/restore') && request.method() === 'POST') {
        if (!link) {
          await fulfill(route, 404, { error: { message: 'link not found' } })
          return
        }
        const historyID = Number(url.pathname.split('/').at(-2))
        const history = (this.histories.get(linkID) ?? []).find((item) => item.id === historyID)
        if (!history) {
          await fulfill(route, 404, { error: { message: 'history not found' } })
          return
        }
        link.content = history.content_document ?? history.content ?? ''
        link.content_revision += 1
        link.updated_at = NOW
        await fulfill(route, 200, { link_id: link.id, content_revision: link.content_revision })
        return
      }
      if (request.method() === 'GET') {
        await fulfill(route, link ? 200 : 404, link ? clone(link) : { error: { message: 'link not found' } })
        return
      }
    }
    if (url.pathname === '/api/links' && request.method() === 'GET') {
      await fulfill(route, 200, { items: [...this.links.values()].map(clone), total: this.links.size, page: 1, limit: 30 })
      return
    }
    if (url.pathname === '/api/tags' && request.method() === 'GET') {
      await fulfill(route, 200, [])
      return
    }
    if (url.pathname === '/api/tree' && request.method() === 'GET') {
      await fulfill(route, 200, { nodes: [], total: 0 })
      return
    }

    if (url.pathname === '/api/annotations/ops' && request.method() === 'POST') {
      const ops = isRecord(body) && Array.isArray(body.ops) ? body.ops : []
      const acknowledgements: Array<Record<string, unknown>> = []
      for (const rawOp of ops) {
        const op = isRecord(rawOp) ? rawOp : {}
        const id = stringValue(op.annotation_id) || `thought-${this.thoughtSequence + 1}`
        const operationKind = stringValue(op.operation_kind)
        const payload = isRecord(op.payload) ? op.payload : {}
        const target = op.target ?? {}
        const submittedKey = {
          logical_clock: typeof op.logical_clock === 'number' ? op.logical_clock : 0,
          device_id: stringValue(op.device_id),
          op_id: stringValue(op.op_id),
        }
        this.thoughtSequence += 1
        const current = this.thoughts.get(id)
        this.thoughts.set(id, operationKind === 'delete' && current
          ? {
              ...current,
              body: '',
              deleted: true,
              last_sequence: this.thoughtSequence,
              winner_key: submittedKey,
              updated_at: NOW,
            }
          : makeThought({
              id,
              host_kind: stringValue(op.host_kind) || 'link',
              host_id: stringValue(op.host_id) || 'link-capture-1',
              link_id: stringValue(op.host_kind) === 'link' ? stringValue(op.host_id) || 'link-capture-1' : null,
              target,
              quote: payload.quote ?? null,
              body: operationKind === 'delete' ? '' : stringValue(payload.body),
              deleted: operationKind === 'delete',
              last_sequence: this.thoughtSequence,
              winner_key: submittedKey,
              updated_at: NOW,
            }))
        acknowledgements.push({
          contract_version: 1,
          op_id: submittedKey.op_id,
          sequence: this.thoughtSequence,
          disposition: 'applied',
          submitted_key: submittedKey,
          current_winner_key: submittedKey,
        })
      }
      await fulfill(route, 200, { items: acknowledgements })
      return
    }
    if ((url.pathname === '/api/annotations' || url.pathname === '/api/annotations/sync') && request.method() === 'GET') {
      await fulfill(route, 200, { contract_version: 1, items: [...this.thoughts.values()].map(clone) })
      return
    }
    const thoughtID = matchID(url.pathname, '/api/annotations/')
    if (thoughtID && url.pathname.endsWith('/reattach') && request.method() === 'POST') {
      const thought = this.thoughts.get(thoughtID)
      if (thought) {
        const input = isRecord(body) ? body : {}
        thought.host_kind = stringValue(input.target_host_kind) || thought.host_kind
        thought.host_id = stringValue(input.target_host_id) || thought.host_id
        thought.link_id = thought.host_kind === 'link' ? thought.host_id : null
        thought.last_sequence = ++this.thoughtSequence
      }
      await fulfill(route, thought ? 200 : 404, thought ? clone(thought) : { error: { message: 'thought not found' } })
      return
    }
    if (thoughtID && request.method() === 'GET') {
      const thought = this.thoughts.get(thoughtID)
      await fulfill(route, thought ? 200 : 404, thought ? clone(thought) : { error: { message: 'thought not found' } })
      return
    }

    if (url.pathname === '/api/search' && request.method() === 'GET') {
      const q = (url.searchParams.get('q') ?? '').trim().toLowerCase()
      const thoughts = [...this.thoughts.values()]
        .filter((thought) => !q || thought.body.toLowerCase().includes(q))
        .map((thought) => ({ id: thought.id, host_kind: thought.host_kind, host_id: thought.host_id, link_id: thought.link_id, snippet: thought.body, updated_at: thought.updated_at }))
      const notes = [...this.notes.values()]
        .filter((note) => !q || `${note.title} ${note.published_content}`.toLowerCase().includes(q))
        .map((note) => ({ id: note.id, title: note.title, snippet: note.published_content, published_revision: note.published_revision, updated_at: note.updated_at }))
      await fulfill(route, 200, {
        reading: { total_hint: this.links.size, items: [...this.links.values()].map(clone) },
        sites: { total_hint: 0, items: [] },
        thoughts: { total_hint: thoughts.length, items: thoughts },
        notes: { total_hint: notes.length, items: notes },
      })
      return
    }

    if (url.pathname === '/api/notes' && request.method() === 'GET') {
      await fulfill(route, 200, { items: [...this.notes.values()].map(clone), count: this.notes.size })
      return
    }
    if (url.pathname === '/api/notes' && request.method() === 'POST') {
      const input = isRecord(body) ? body : {}
      const id = `note-capture-${this.nextNoteID++}`
      const content = stringValue(input.content)
      const note = makeNote({
        id,
        title: stringValue(input.title) || '新建笔记',
        published_content: '',
        published_revision: 0,
        draft_content: content || null,
        draft_revision: content ? 1 : 0,
        dirty: Boolean(content),
      })
      this.notes.set(id, note)
      await fulfill(route, 201, clone(note))
      return
    }
    const noteID = matchID(url.pathname, '/api/notes/')
    if (noteID) {
      const note = this.notes.get(noteID)
      if (request.method() === 'DELETE' && !url.pathname.endsWith('/draft')) {
        if (note) this.notes.delete(noteID)
        await fulfill(route, 200, {
          host_kind: 'note',
          host_id: noteID,
          state: 'trashed',
          changed: Boolean(note),
        })
        return
      }
      if (url.pathname.endsWith('/draft') && request.method() === 'PATCH') {
        if (!note) {
          await fulfill(route, 404, { error: { message: 'note not found' } })
          return
        }
        const input = isRecord(body) ? body : {}
        note.draft_content = stringValue(input.content)
        note.draft_revision += 1
        note.draft_updated_at = NOW
        note.dirty = true
        note.updated_at = NOW
        await fulfill(route, 200, clone(note))
        return
      }
      if (url.pathname.endsWith('/publish') && request.method() === 'POST') {
        if (!note) {
          await fulfill(route, 404, { error: { message: 'note not found' } })
          return
        }
        const input = isRecord(body) ? body : {}
        const reanchorOps = Array.isArray(input.reanchor_ops) ? input.reanchor_ops : []
        note.published_content = note.draft_content ?? note.published_content
        note.published_revision += 1
        note.draft_content = null
        note.dirty = false
        note.updated_at = NOW
        if (note.published_content.includes('[ ]')) {
          const todoID = `todo-note-${this.nextTodoID++}`
          this.todos.set(todoID, makeTodo({
            id: todoID,
            text: 'Follow up on the captured idea',
            host_revision: note.published_revision,
            origin_host_id: note.id,
          }))
        }
        await fulfill(route, 200, { ...clone(note), reanchor_ops: clone(reanchorOps) })
        return
      }
      if (url.pathname.endsWith('/history') && request.method() === 'GET') {
        await fulfill(route, 200, { items: [{ id: 1, revision: 1, title: note?.title ?? '', content: 'Old note', created_at: NOW }] })
        return
      }
      if (url.pathname.includes('/history/') && url.pathname.endsWith('/restore') && request.method() === 'POST') {
        if (note) {
          note.published_content = 'Old note'
          note.published_revision += 1
          note.updated_at = NOW
        }
        await fulfill(route, note ? 200 : 404, note ? clone(note) : { error: { message: 'note not found' } })
        return
      }
      if (request.method() === 'GET') {
        await fulfill(route, note ? 200 : 404, note ? clone(note) : { error: { message: 'note not found' } })
        return
      }
    }

    if (url.pathname === '/api/todos' && request.method() === 'GET') {
      await fulfill(route, 200, { items: [...this.todos.values()].map(clone) })
      return
    }
    if (url.pathname === '/api/todos' && request.method() === 'POST') {
      const input = isRecord(body) ? body : {}
      const todo = makeTodo({
        id: `todo-standalone-${this.nextTodoID++}`,
        text: stringValue(input.text) || 'new task',
        origin_kind: 'standalone',
        origin_host_kind: null,
        origin_host_id: null,
        origin_ref: null,
      })
      this.todos.set(todo.id, todo)
      await fulfill(route, 201, clone(todo))
      return
    }
    const todoID = matchID(url.pathname, '/api/todos/')
    if (todoID && request.method() === 'PATCH') {
      const todo = this.todos.get(todoID)
      if (!todo) {
        await fulfill(route, 404, { error: { message: 'todo not found' } })
        return
      }
      const input = isRecord(body) ? body : {}
      if (typeof input.done === 'boolean') {
        todo.done = input.done
        todo.completed_at = input.done ? NOW : null
      }
      if (typeof input.text === 'string' && input.text.trim()) todo.text = input.text.trim()
      todo.updated_at = NOW
      await fulfill(route, 200, clone(todo))
      return
    }
    if (todoID && request.method() === 'DELETE') {
      this.todos.delete(todoID)
      await fulfill(route, 204, null)
      return
    }

    if (url.pathname === '/api/home' && request.method() === 'GET') {
      await fulfill(route, 200, this.home())
      return
    }
    if (url.pathname === '/api/reader-feed' && request.method() === 'GET') {
      const mode = url.searchParams.get('mode') === 'chronological' ? 'chronological' : 'recommended'
      const items = [...this.feed.values()].map((item) => {
        const state = item.link_id ? this.engagement.get(item.link_id) : undefined
        return { ...item, read: state?.read ?? item.read, read_later: state?.read_later ?? item.read_later }
      })
      await fulfill(route, 200, { items, snapshot_id: 'wp26-snapshot-1', mode })
      return
    }
    if (url.pathname === '/api/reader-feed/feedback' && request.method() === 'POST') {
      const item = this.feed.get(url.searchParams.get('item_key') ?? '')
      const input = isRecord(body) ? body : {}
      const action = input.action === 'save' || input.action === 'unsave' ? input.action : null
      if (!item || !action) {
        await fulfill(route, 404, { error: { message: 'feed item not found' } })
        return
      }
      item.saved = action === 'save'
      await fulfill(route, 200, {
        item_key: item.key,
        action,
        saved: item.saved,
      })
      return
    }
    const engagementID = matchID(url.pathname, '/api/engagement/')
    if (engagementID && (request.method() === 'GET' || request.method() === 'PATCH')) {
      const state = this.engagement.get(engagementID) ?? { read: false, read_later: false, progress: 0 }
      if (request.method() === 'PATCH') {
        const input = isRecord(body) ? body : {}
        if (typeof input.read === 'boolean') state.read = input.read
        if (typeof input.read_later === 'boolean') state.read_later = input.read_later
        if (typeof input.progress === 'number') state.progress = input.progress
        this.engagement.set(engagementID, state)
      }
      await fulfill(route, 200, { link_id: engagementID, ...state, last_opened: NOW, updated_at: NOW })
      return
    }

    await fulfill(route, 404, { error: { message: `unhandled fixture route: ${request.method()} ${url.pathname}` } })
  }

  home(): Record<string, unknown> {
    const firstLink = this.links.get('link-capture-1') as Wp26Link
    const feedItem = this.feed.get('link:link-capture-1') as Wp26FeedItem
    return {
      today: '2026-08-10',
      summary: '继续整理捕获内容',
      counts: {
        pending: this.inbox.status === 'pending' ? 1 : 0,
        inbox: this.inbox.status === 'pending' && !this.inbox.expired ? 1 : 0,
        inbox_expired: this.inbox.status === 'pending' && this.inbox.expired ? 1 : 0,
        reading: this.links.size,
        sites: 0,
        subs: 0,
        notes: this.notes.size,
      },
      continue_reading: [{ ...feedItem, title: firstLink.title, summary: firstLink.summary }],
      recent_thoughts: [...this.thoughts.values()],
      todos: [...this.todos.values()],
      stale: false,
    }
  }

  private sourceFromIngest(value: unknown): Partial<Wp26Inbox> {
    const root = isRecord(value) ? value : {}
    const sources = Array.isArray(root.sources) ? root.sources : []
    const source = isRecord(sources[0]) ? sources[0] : {}
    return {
      url: stringValue(source.url) || this.inbox.url,
      source_kind: stringValue(source.kind) || 'browser_capture',
      title: stringValue(source.title) || this.inbox.title,
      body: stringValue(source.text) || this.inbox.body,
      summary: 'Captured summary',
      updated_at: NOW,
    }
  }
}

function matchID(pathname: string, prefix: string): string | null {
  if (!pathname.startsWith(prefix)) return null
  const rest = pathname.slice(prefix.length)
  const id = rest.split('/')[0]
  return id ? decodeURIComponent(id) : null
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function stringValue(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function stringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : []
}

export async function bootstrapReaderPage(page: Page, query = '?surface=home'): Promise<void> {
  await page.goto(`/reader/${query}`, { waitUntil: 'domcontentloaded' })
}

export async function configureReaderConnection(page: Page): Promise<void> {
  await page.addInitScript(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({ baseURL: window.location.origin, mode: 'installation-token', installationToken: '' }),
    )
  })
}
