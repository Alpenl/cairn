import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  buildFeedItemsQuery,
  buildLinksQuery,
  DATA_NAMESPACE_HEADER,
  ReaderClient,
  SESSION_HEADER,
  type ReaderClientConfig,
} from './client'
import {
  ARCHIVE_V2_MAX_BYTES,
  fullArchiveV2Selection,
  type ArchiveV2Selection,
} from './archive-v2'
import { makeLink } from '../../test/fixtures'
import { IdentityAuthority } from '../identity'
import type { ReaderHomeResponse, ReaderTodoResponse } from './types'

const BASE = 'http://localhost:8080'
const VALID_DATA_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

function mockFetch(impl: (input: string, init?: RequestInit) => Response | Promise<Response>) {
  const fn = vi.fn(impl)
  vi.stubGlobal('fetch', fn)
  return fn
}

function jsonResponse(
  body: unknown,
  init?: ResponseInit,
  marker: string | null = VALID_DATA_NAMESPACE,
): Response {
  const headers = new Headers({ 'Content-Type': 'application/json' })
  if (marker) headers.set(DATA_NAMESPACE_HEADER, marker)
  new Headers(init?.headers).forEach((value, key) => headers.set(key, value))
  return new Response(JSON.stringify(body), {
    status: 200,
    ...init,
    headers,
  })
}

function rawJSONResponse(
  text: string,
  init?: ResponseInit,
  marker: string | null = VALID_DATA_NAMESPACE,
): Response {
  const headers = new Headers({ 'Content-Type': 'application/json' })
  if (marker) headers.set(DATA_NAMESPACE_HEADER, marker)
  new Headers(init?.headers).forEach((value, key) => headers.set(key, value))
  return new Response(text, {
    status: 200,
    ...init,
    headers,
  })
}

function authenticatedClient(
  config: Omit<ReaderClientConfig, 'baseURL' | 'identity'> = {},
): ReaderClient {
  const authority = new IdentityAuthority()
  const identity = authority.install({
    serverClientDataNamespace: VALID_DATA_NAMESPACE,
    physicalNamespace: 'physical-test',
  })
  return new ReaderClient({ baseURL: BASE, ...config, identity })
}

const validPage = { items: [], total: 0, page: 1, limit: 1 }

const ARCHIVE_TOP_LEVEL_SECTIONS = [
  'links',
  'sites',
  'site_entries',
  'site_tags',
  'site_identities',
  'classification_rules',
]

const ARCHIVE_READER_BASE_SECTIONS = [
  'feed_folders',
  'feed_subscriptions',
  'feed_items',
  'feed_saves',
  'inbox',
  'categories',
  'categorizables',
  'todos',
  'engagement',
  'feed_feedback',
  'feed_snapshots',
  'tag_activity',
  'domain_activity',
  'content_history',
]

const ARCHIVE_READER_THOUGHT_SECTIONS = [
  'thoughts',
  'thought_ops',
  'thought_supersession_events',
  'thought_tombstones',
]

const ARCHIVE_READER_NOTE_SECTIONS = ['notes', 'note_history']

function archiveTokens(selection: ArchiveV2Selection): string[] {
  const tokens = ['base']
  if (selection.includeThoughts === true) tokens.push('thoughts')
  if (selection.includeNotes === true) tokens.push('notes')
  return tokens
}

function selectedArchiveReaderSections(selection: ArchiveV2Selection): string[] {
  const sections = [...ARCHIVE_READER_BASE_SECTIONS]
  if (selection.includeThoughts === true) sections.push(...ARCHIVE_READER_THOUGHT_SECTIONS)
  if (selection.includeNotes === true) sections.push(...ARCHIVE_READER_NOTE_SECTIONS)
  return sections
}

async function archiveChecksum(bytes: Uint8Array): Promise<string> {
  const input = new Uint8Array(bytes.byteLength)
  input.set(bytes)
  const digest = await globalThis.crypto.subtle.digest('SHA-256', input)
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

async function validArchiveV2Bytes(
  selection: ArchiveV2Selection = fullArchiveV2Selection,
): Promise<Uint8Array> {
  const reader: Record<string, unknown> = {
    schema_version: 2,
    thought_contract_version: 1,
  }
  for (const section of selectedArchiveReaderSections(selection)) reader[section] = []
  const archive: Record<string, unknown> = {
    schema_version: 2,
    exported_at: '2026-08-11T00:00:00Z',
    generator_version: 'webtag',
    links: [],
    sites: [],
    site_entries: [],
    site_tags: [],
    site_identities: [],
    classification_rules: [],
    reader,
  }
  const counts: Record<string, number> = {}
  for (const section of ARCHIVE_TOP_LEVEL_SECTIONS) counts[section] = 0
  for (const section of selectedArchiveReaderSections(selection)) counts[`reader.${section}`] = 0
  const prefix = JSON.stringify(archive).slice(0, -1)
  const manifest = {
    client_data_namespace: VALID_DATA_NAMESPACE,
    sections: archiveTokens(selection),
    counts,
    checksum_sha256: await archiveChecksum(new TextEncoder().encode(prefix)),
  }
  return new TextEncoder().encode(`${prefix},"manifest":${JSON.stringify(manifest)}}`)
}

function archiveResponse(bytes: Uint8Array, init: ResponseInit = {}): Response {
  const body = new Uint8Array(bytes.byteLength)
  body.set(bytes)
  const headers = new Headers({
    'Content-Type': 'application/json; charset=utf-8',
    [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE,
  })
  new Headers(init.headers).forEach((value, key) => headers.set(key, value))
  return new Response(body.buffer, { status: 200, ...init, headers })
}

function readBlobBytes(blob: Blob): Promise<Uint8Array> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener('error', () => reject(reader.error ?? new Error('Blob read failed')), { once: true })
    reader.addEventListener('load', () => {
      if (!(reader.result instanceof ArrayBuffer)) {
        reject(new Error('Blob did not produce ArrayBuffer bytes'))
        return
      }
      resolve(new Uint8Array(reader.result))
    }, { once: true })
    reader.readAsArrayBuffer(blob)
  })
}

function makeReaderTodo(overrides: Partial<ReaderTodoResponse> = {}): ReaderTodoResponse {
  return {
    id: 'todo-1',
    text: 'Review the aggregate',
    due_at: '2026-08-12T09:00:00Z',
    done: true,
    origin_kind: 'standalone',
    origin_host_kind: null,
    origin_host_id: null,
    origin_ref: { block_ref: 'task:aggregate', occurrence: 1 },
    host_revision: 1,
    completed_at: '2026-08-10T02:00:00Z',
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T02:00:00Z',
    expired: false,
    ...overrides,
  }
}

function makeReaderHome(overrides: Partial<ReaderHomeResponse> = {}): ReaderHomeResponse {
  return {
    today: '2026-08-10',
    summary: '今日概览',
    counts: { pending: 1, todos: 1 },
    continue_reading: [],
    recent_thoughts: [],
    todos: [makeReaderTodo()],
    stale: false,
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('ReaderClient identity handshake', () => {
  it('loads the authoritative v2 identity without using the HTTP cache', async () => {
    const identity = {
      client_data_namespace: VALID_DATA_NAMESPACE,
      representation_contract: 'v3' as const,
    }
    const fn = mockFetch(() =>
      jsonResponse(identity, {
        headers: { 'X-WebTag-Data-Namespace': VALID_DATA_NAMESPACE },
      }),
    )

    await expect(
      new ReaderClient({ baseURL: BASE, session: true }).getIdentity(),
    ).resolves.toEqual({ ok: true, data: identity })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/session`,
      expect.objectContaining({
        method: 'GET',
        credentials: 'include',
        cache: 'no-store',
        headers: expect.objectContaining({ [SESSION_HEADER]: '1' }),
      }),
    )
  })

  it('fails closed when the authenticated identity marker is missing', async () => {
    mockFetch(() =>
      jsonResponse(
        {
          client_data_namespace: VALID_DATA_NAMESPACE,
          representation_contract: 'v3',
        },
        undefined,
        null,
      ),
    )

    await expect(new ReaderClient({ baseURL: BASE }).getIdentity()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it.each([
    {
      name: 'a short namespace',
      body: {
        client_data_namespace: 'too-short',
        representation_contract: 'v3',
      },
    },
    {
      name: 'an unknown field',
      body: {
        client_data_namespace: VALID_DATA_NAMESPACE,
        representation_contract: 'v3',
        account_hint: 'must-not-be-trusted',
      },
    },
  ])('fails closed when the identity snapshot contains $name', async ({ body }) => {
    mockFetch(() =>
      jsonResponse(body, {
        headers: { [DATA_NAMESPACE_HEADER]: body.client_data_namespace },
      }),
    )

    await expect(new ReaderClient({ baseURL: BASE }).getIdentity()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })
})

describe('ReaderClient capability negotiation', () => {
  it('reads the Reader capability snapshot through the identity-bound path', async () => {
    mockFetch(() => jsonResponse({
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
        ai: false,
        semantic: true,
        activity: true,
        history: true,
        trash: true,
      },
    }))

    const result = await authenticatedClient().getCapabilities()
    expect(result.ok).toBe(true)
    if (result.ok) {
      expect(result.data.reader_vnext).toBe(true)
      expect(result.data.reader.ai).toBe(false)
    }
  })
})

describe('ReaderClient activity pagination', () => {
  it('encodes kind, cursor, limit, and cancellation on the activity request', async () => {
    const fetchMock = mockFetch(() => jsonResponse({
      kind: 'tag',
      tags: [],
      domains: [],
      next_cursor: 'next-page',
    }))
    const controller = new AbortController()

    await expect(authenticatedClient().getReaderActivity(37, {
      kind: 'tag',
      after: 'opaque cursor',
      signal: controller.signal,
    })).resolves.toEqual({
      ok: true,
      data: { kind: 'tag', tags: [], domains: [], next_cursor: 'next-page' },
    })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/reader/activity?kind=tag&after=opaque+cursor&limit=37`,
      expect.objectContaining({ method: 'GET', signal: controller.signal }),
    )
  })
})

describe('ReaderClient Inbox mutations', () => {
  it('passes the requested Inbox partition through the list query and validates split counts', async () => {
    const fetchMock = mockFetch(() => jsonResponse({
      items: [],
      active_count: 4,
      expired_count: 2,
    }))

    await expect(authenticatedClient().listInbox({ partition: 'expired', after: 'cursor-1', limit: 10 })).resolves.toEqual({
      ok: true,
      data: { items: [], active_count: 4, expired_count: 2 },
    })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/inbox?partition=expired&after=cursor-1&limit=10`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('restores an Inbox item through the 200 endpoint', async () => {
    const fetchMock = mockFetch(() => new Response(null, {
      status: 200,
      headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
    }))

    await expect(authenticatedClient().restoreInbox('inbox-1')).resolves.toEqual({ ok: true, data: true })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/inbox/inbox-1/restore`,
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('forwards one stable command identity for Inbox restore replay', async () => {
    const fetchMock = mockFetch(() => new Response(null, {
      status: 200,
      headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
    }))

    await authenticatedClient().restoreInbox('inbox-1', { idempotencyKey: 'restore-inbox-1' })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/inbox/inbox-1/restore`,
      expect.objectContaining({
        headers: expect.objectContaining({ 'Idempotency-Key': 'restore-inbox-1' }),
      }),
    )
  })

  it('forwards the stable note-create intent as an Idempotency-Key', async () => {
    const fetchMock = mockFetch(() => jsonResponse({
      id: 'note-created',
      title: '未命名笔记',
      published_content: '',
      published_revision: 0,
      draft_content: null,
      draft_revision: 0,
      draft_updated_at: null,
      deleted_at: null,
      created_at: '2026-08-11T00:00:00Z',
      updated_at: '2026-08-11T00:00:00Z',
      dirty: false,
    }, { status: 201 }))

    await authenticatedClient().createNote({ content: '' }, { idempotencyKey: 'create-note-1' })

    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/notes`,
      expect.objectContaining({
        method: 'POST',
        headers: expect.objectContaining({ 'Idempotency-Key': 'create-note-1' }),
      }),
    )
  })

  it('rejects a malformed note-create response before navigation can consume it', async () => {
    mockFetch(() => jsonResponse({ id: 'note-created-without-required-fields' }, { status: 201 }))

    await expect(
      authenticatedClient().createNote({ content: '' }, { idempotencyKey: 'create-note-malformed' }),
    ).resolves.toEqual({
      ok: false,
      error: { kind: 'other', message: '响应体格式不符：ReaderNoteResponse' },
    })
  })

  it('uses the generated bulk request envelope and validates both bulk responses', async () => {
    const response = {
      atomic: true as const,
      items: [
        { inbox_id: 'inbox-1', status: 'confirmed' as const, link_id: 'link-1' },
        { inbox_id: 'inbox-2', status: 'discarded' as const },
      ],
    }
    const fetchMock = mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    await expect(client.confirmInboxBulk({ inbox_ids: ['inbox-1', 'inbox-2'] })).resolves.toEqual({ ok: true, data: response })
    await expect(client.discardInboxBulk({ inbox_ids: ['inbox-1', 'inbox-2'] })).resolves.toEqual({ ok: true, data: response })

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      `${BASE}/api/inbox/bulk/confirm`,
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ inbox_ids: ['inbox-1', 'inbox-2'] }) }),
    )
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      `${BASE}/api/inbox/bulk/discard`,
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ inbox_ids: ['inbox-1', 'inbox-2'] }) }),
    )
  })

  it('uses the server-selected AI proposal action and preserves remaining_count', async () => {
    const response = {
      atomic: true as const,
      items: [{ inbox_id: 'inbox-1', status: 'confirmed' as const, link_id: 'link-1' }],
      remaining_count: 37,
    }
    const fetchMock = mockFetch(() => jsonResponse(response))

    await expect(authenticatedClient().confirmAIProposals({ partition: 'active' })).resolves.toEqual({ ok: true, data: response })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/inbox/confirm-ai-proposals`,
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ partition: 'active' }) }),
    )
  })
})

describe('ReaderClient host lifecycle', () => {
  it('lists trash, restores links, and purges each host through the frozen routes', async () => {
    const trash = {
      count: 1,
      items: [{
        host_kind: 'link' as const,
        host_id: '00000000-0000-0000-0000-000000000053',
        title: 'Saved article',
        url: 'https://example.com/article',
        trashed_at: '2026-08-11T01:02:03Z',
      }],
    }
    const restored = {
      host_kind: 'link' as const,
      host_id: trash.items[0].host_id,
      state: 'live' as const,
      changed: true,
    }
    const fetchMock = mockFetch((input) => {
      if (input.includes('/api/trash')) return jsonResponse(trash)
      if (input.includes('/restore')) return jsonResponse(restored)
      return new Response(null, {
        status: 204,
        headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
      })
    })
    const client = authenticatedClient()

    await expect(client.listTrash({ hostKind: 'link', limit: 20 })).resolves.toEqual({ ok: true, data: trash })
    await expect(client.restoreLink(trash.items[0].host_id)).resolves.toEqual({ ok: true, data: restored })
    await expect(client.purgeHost('note', '00000000-0000-0000-0000-000000000054', '00000000-0000-0000-0000-000000000055')).resolves.toEqual({ ok: true, data: true })

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      `${BASE}/api/trash?host_kind=link&limit=20`,
      expect.objectContaining({ method: 'GET' }),
    )
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      `${BASE}/api/links/${trash.items[0].host_id}/restore`,
      expect.objectContaining({ method: 'POST' }),
    )
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      `${BASE}/api/notes/00000000-0000-0000-0000-000000000054/purge`,
      expect.objectContaining({
        method: 'DELETE',
        body: JSON.stringify({ operation_id: '00000000-0000-0000-0000-000000000055' }),
      }),
    )
  })
})

describe('ReaderClient Home/TODO APIs', () => {
  it.each([false, true])('getHome accepts fresh/stale aggregate wire shape: stale=%s', async (stale) => {
    const response = {
      ...makeReaderHome({ stale }),
      freshness: stale ? 'stale' : 'fresh',
      partial: false,
    }
    const fetchMock = mockFetch(() => jsonResponse(response))

    await expect(authenticatedClient().getHome()).resolves.toEqual({ ok: true, data: response })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/home`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('listTodos accepts completed and pending TODO wire variants', async () => {
    const completed = makeReaderTodo({ id: 'todo-done', done: true })
    const pending: Record<string, unknown> = {
      ...makeReaderTodo({
        id: 'todo-pending',
        done: false,
        expired: true,
        origin_ref: null,
      }),
    }
    delete pending.due_at
    delete pending.completed_at
    const response = { items: [completed, pending] }
    const fetchMock = mockFetch(() => jsonResponse(response))

    await expect(authenticatedClient().listTodos()).resolves.toEqual({ ok: true, data: response })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/todos?limit=200`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('listTodos reads every keyset page before exposing an atomic result', async () => {
    const first = makeReaderTodo({ id: 'todo-first', source_href: '/?view=notes&note_id=one' })
    const second = makeReaderTodo({ id: 'todo-second' })
    const fetchMock = mockFetch((input) => {
      const url = String(input)
      if (url.includes('after=cursor-two')) return jsonResponse({ items: [second] })
      return jsonResponse({ items: [first], next_after: 'cursor-two' })
    })

    await expect(authenticatedClient().listTodos()).resolves.toEqual({
      ok: true,
      data: { items: [first, second] },
    })
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      `${BASE}/api/todos?limit=200`,
      expect.objectContaining({ method: 'GET' }),
    )
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      `${BASE}/api/todos?limit=200&after=cursor-two`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('listTodos rejects a repeated cursor without exposing partial data', async () => {
    const fetchMock = mockFetch(() => jsonResponse({ items: [makeReaderTodo()], next_after: 'same-cursor' }))

    const result = await authenticatedClient().listTodos()

    expect(result).toMatchObject({ ok: false, error: { kind: 'other' } })
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('createTodo sends its due date and guards the created response', async () => {
    const request = { text: 'Schedule review', due_at: '2026-08-13T09:00:00Z' }
    const response = makeReaderTodo({
      id: 'todo-created',
      text: request.text,
      due_at: request.due_at,
      done: false,
      completed_at: null,
      expired: false,
    })
    const fetchMock = mockFetch(() => jsonResponse(response, { status: 201 }))

    await expect(authenticatedClient().createTodo(request)).resolves.toEqual({ ok: true, data: response })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/todos`,
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify(request),
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      }),
    )
  })

  it('patchTodo sends desired state and host revision while accepting completed_at', async () => {
    const id = 'todo/一'
    const request = { due_at: null, done: true, expected_host_revision: 1 }
    const response = makeReaderTodo({
      id,
      due_at: null,
      done: true,
      completed_at: '2026-08-10T03:00:00Z',
    })
    const fetchMock = mockFetch(() => jsonResponse(response))

    await expect(authenticatedClient().patchTodo(id, request)).resolves.toEqual({ ok: true, data: response })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/todos/todo%2F%E4%B8%80`,
      expect.objectContaining({
        method: 'PATCH',
        body: JSON.stringify(request),
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      }),
    )
  })

  it('deleteTodo treats the 204 response as a successful independent operation', async () => {
    const fetchMock = mockFetch(() => new Response(null, {
      status: 204,
      headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
    }))

    await expect(authenticatedClient().deleteTodo('todo/一')).resolves.toEqual({ ok: true, data: true })
    expect(fetchMock).toHaveBeenCalledWith(
      `${BASE}/api/todos/todo%2F%E4%B8%80`,
      expect.objectContaining({ method: 'DELETE' }),
    )
  })

  it('returns a shape mismatch for malformed Home/TODO success bodies', async () => {
    const partialHome: Record<string, unknown> = { ...makeReaderHome() }
    delete partialHome.todos
    const cases: Array<{
      name: string
      body: unknown
      invoke: (client: ReaderClient) => Promise<unknown>
    }> = [
      {
        name: 'getHome partial aggregate',
        body: partialHome,
        invoke: (client) => client.getHome(),
      },
      {
        name: 'listTodos invalid completed_at',
        body: { items: [{ ...makeReaderTodo(), completed_at: 7 }] },
        invoke: (client) => client.listTodos(),
      },
      {
        name: 'createTodo malformed response',
        body: { wrong: true },
        invoke: (client) => client.createTodo({ text: 'Create' }),
      },
      {
        name: 'patchTodo malformed response',
        body: { wrong: true },
        invoke: (client) => client.patchTodo('todo-1', { done: false }),
      },
    ]

    for (const testCase of cases) {
      mockFetch(() => jsonResponse(testCase.body))
      const result = await testCase.invoke(authenticatedClient())
      expect(result, testCase.name).toMatchObject({ ok: false, error: { kind: 'other' } })
    }
  })

  it('returns the HTTP failure for deleteTodo instead of a success sentinel', async () => {
    mockFetch(() => jsonResponse(
      { error: { code: 404, message: 'TODO not found' } },
      { status: 404 },
    ))

    const result = await authenticatedClient().deleteTodo('todo-missing')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.status).toBe(404)
  })
})

describe('ReaderClient session exchange identity', () => {
  it('accepts a v2 login snapshot whose body and marker agree', async () => {
    const fetchSpy = mockFetch(() =>
      jsonResponse(
        {
          expires_at: '2030-01-01T00:00:00Z',
          client_data_namespace: VALID_DATA_NAMESPACE,
          representation_contract: 'v3',
        },
        { status: 201, headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE } },
      ),
    )

    await expect(new ReaderClient({ baseURL: BASE }).login('secret')).resolves.toEqual({
      ok: true,
      data: {
        expiresAt: '2030-01-01T00:00:00Z',
        clientDataNamespace: VALID_DATA_NAMESPACE,
      },
    })
    expect(JSON.parse(String(fetchSpy.mock.calls[0]?.[1]?.body))).toEqual({
      token: 'secret',
    })
  })

  it('fails closed when a successful login omits its identity marker', async () => {
    mockFetch(() =>
      jsonResponse(
        {
          expires_at: '2030-01-01T00:00:00Z',
          client_data_namespace: VALID_DATA_NAMESPACE,
          representation_contract: 'v3',
        },
        { status: 201 },
        null,
      ),
    )

    await expect(new ReaderClient({ baseURL: BASE }).login('secret')).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it.each([
    {
      name: 'a date without an RFC3339 time',
      body: {
        expires_at: '2030-01-01',
        client_data_namespace: VALID_DATA_NAMESPACE,
        representation_contract: 'v3',
      },
    },
    {
      name: 'an impossible calendar date',
      body: {
        expires_at: '2030-02-30T00:00:00Z',
        client_data_namespace: VALID_DATA_NAMESPACE,
        representation_contract: 'v3',
      },
    },
    {
      name: 'an out-of-range timezone offset',
      body: {
        expires_at: '2030-01-01T00:00:00+24:00',
        client_data_namespace: VALID_DATA_NAMESPACE,
        representation_contract: 'v3',
      },
    },
    {
      name: 'an unknown field',
      body: {
        expires_at: '2030-01-01T00:00:00Z',
        client_data_namespace: VALID_DATA_NAMESPACE,
        representation_contract: 'v3',
        session_token: 'must-not-be-exposed',
      },
    },
  ])('fails closed when the login snapshot contains $name', async ({ body }) => {
    mockFetch(() =>
      jsonResponse(body, {
        status: 201,
        headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
      }),
    )

    await expect(new ReaderClient({ baseURL: BASE }).login('secret')).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })

  it('accepts the markerless 204 logout contract', async () => {
    const fn = mockFetch(() => new Response(null, { status: 204 }))

    await expect(new ReaderClient({ baseURL: BASE }).logout()).resolves.toBeUndefined()
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/session`,
      expect.objectContaining({
        method: 'DELETE',
        credentials: 'include',
        headers: { [SESSION_HEADER]: '1' },
      }),
    )
  })
})

describe('ReaderClient authenticated response ownership', () => {
  it('reports whether its authoritative identity lease is still current', () => {
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: VALID_DATA_NAMESPACE,
      physicalNamespace: 'physical-A',
    })
    const client = new ReaderClient({ baseURL: BASE, identity: leaseA })

    expect(client.isIdentityCurrent()).toBe(true)
    authority.install({
      serverClientDataNamespace: 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB',
      physicalNamespace: 'physical-B',
    })
    expect(client.isIdentityCurrent()).toBe(false)
  })

  it('does not send a private request before an identity lease is installed', async () => {
    const fn = mockFetch(() => jsonResponse(validPage))

    await expect(new ReaderClient({ baseURL: BASE }).getLinks()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(fn).not.toHaveBeenCalled()
  })

  const ownedResponseCases = [
    {
      name: 'normal 2xx',
      response: (marker?: string) =>
        jsonResponse(
          validPage,
          { headers: marker ? { [DATA_NAMESPACE_HEADER]: marker } : undefined },
          marker ?? null,
        ),
      expectedKind: null,
    },
    {
      name: '304',
      response: (marker?: string) =>
        new Response(null, {
          status: 304,
          headers: marker ? { [DATA_NAMESPACE_HEADER]: marker } : undefined,
        }),
      expectedKind: 'not-modified',
    },
    {
      name: 'authenticated error',
      response: (marker?: string) =>
        jsonResponse(
          { error: { code: 500, message: 'boom' } },
          {
            status: 500,
            headers: marker ? { [DATA_NAMESPACE_HEADER]: marker } : undefined,
          },
          marker ?? null,
        ),
      expectedKind: 'other',
    },
  ] as const

  it.each(ownedResponseCases)('accepts a matching marker on $name', async (testCase) => {
    const authority = new IdentityAuthority()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const onIdentityMismatch = vi.fn()
    mockFetch(() => testCase.response('server-A'))

    const result = await new ReaderClient({
      baseURL: BASE,
      identity: lease,
      onIdentityMismatch,
    }).getLinks()

    if (testCase.expectedKind === null) {
      expect(result.ok).toBe(true)
    } else {
      expect(result).toMatchObject({ ok: false, error: { kind: testCase.expectedKind } })
    }
    expect(onIdentityMismatch).not.toHaveBeenCalled()
    expect(lease.capture('after matching marker').signal.aborted).toBe(false)
  })

  it.each(ownedResponseCases)('fails closed when the marker is missing on $name', async (testCase) => {
    const authority = new IdentityAuthority()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const onIdentityMismatch = vi.fn()
    mockFetch(() => testCase.response())

    await expect(
      new ReaderClient({
        baseURL: BASE,
        identity: lease,
        onIdentityMismatch,
      }).getLinks(),
    ).resolves.toMatchObject({ ok: false, error: { kind: 'identity-mismatch' } })
    expect(onIdentityMismatch).toHaveBeenCalledOnce()
    expect(lease.capture('after missing marker').signal.aborted).toBe(true)
  })

  it('reports identity mismatch when the lease is revoked while reading a response body', async () => {
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    let bodyStarted!: () => void
    const started = new Promise<void>((resolve) => {
      bodyStarted = resolve
    })
    vi.stubGlobal(
      'fetch',
      vi.fn((_input: string, init?: RequestInit) => {
        const signal = init?.signal as AbortSignal
        return Promise.resolve({
          ok: true,
          status: 200,
          headers: new Headers({ [DATA_NAMESPACE_HEADER]: 'server-A' }),
          text: () => {
            bodyStarted()
            return new Promise<string>((_resolve, reject) => {
              signal.addEventListener('abort', () =>
                reject(new DOMException('aborted', 'AbortError')),
              )
            })
          },
        } as Response)
      }),
    )
    const request = new ReaderClient({ baseURL: BASE, identity: leaseA }).getLinks()
    await started

    authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    await expect(request).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it('discards a JSON body resolved immediately before the identity lease is replaced', async () => {
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    let bodyStarted!: () => void
    const started = new Promise<void>((resolve) => {
      bodyStarted = resolve
    })
    let resolveBody!: (body: string) => void
    const body = new Promise<string>((resolve) => {
      resolveBody = resolve
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve({
          ok: true,
          status: 200,
          headers: new Headers({ [DATA_NAMESPACE_HEADER]: 'server-A' }),
          text: () => {
            bodyStarted()
            return body
          },
        } as Response),
      ),
    )
    const request = new ReaderClient({ baseURL: BASE, identity: leaseA }).getLinks()
    await started

    resolveBody(JSON.stringify(validPage))
    authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    await expect(request).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it('discards a response from a replaced session and revokes the old identity lease', async () => {
    const authority = new IdentityAuthority()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const onIdentityMismatch = vi.fn()
    mockFetch(() =>
      jsonResponse(validPage, { headers: { [DATA_NAMESPACE_HEADER]: 'server-B' } }),
    )
    const client = new ReaderClient({
      baseURL: BASE,
      identity: lease,
      onIdentityMismatch,
    })

    await expect(client.getLinks()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(onIdentityMismatch).toHaveBeenCalledOnce()
    expect(lease.capture('after-mismatch').signal.aborted).toBe(true)
  })
})

describe('buildLinksQuery', () => {
  it('全参数拼装并跳过空 / page<=1 / limit<=0', () => {
    const qs = buildLinksQuery({
      q: 'react',
      tags: 'a,b',
      domain: 'example.com',
      content_type: 'article',
      status: 'done',
      created_from: '2026-08-10T16:00:00.000Z',
      created_before: '2026-08-11T16:00:00.000Z',
      url: 'https://x.com',
      after: 'cur1',
      page: 3,
      limit: 20,
    })
    const sp = new URLSearchParams(qs.slice(1))
    expect(sp.get('q')).toBe('react')
    expect(sp.get('tags')).toBe('a,b')
    expect(sp.get('domain')).toBe('example.com')
    expect(sp.get('content_type')).toBe('article')
    expect(sp.get('status')).toBe('done')
    expect(sp.get('created_from')).toBe('2026-08-10T16:00:00.000Z')
    expect(sp.get('created_before')).toBe('2026-08-11T16:00:00.000Z')
    expect(sp.get('url')).toBe('https://x.com')
    expect(sp.get('after')).toBe('cur1')
    expect(sp.get('page')).toBe('3')
    expect(sp.get('limit')).toBe('20')
  })

  it('page=1 与空串被省略', () => {
    expect(buildLinksQuery({ page: 1, q: '   ', tags: '' })).toBe('')
  })

  it('保留 low_confidence 的显式 false', () => {
    expect(buildLinksQuery({ low_confidence: true })).toBe('?low_confidence=true')
    expect(buildLinksQuery({ low_confidence: false })).toBe('?low_confidence=false')
  })
})

describe('buildFeedItemsQuery', () => {
  it('拼装可组合的 RSS 视图、来源、文件夹和搜索分页', () => {
    const query = buildFeedItemsQuery({
      view: 'unread',
      subscription_id: 'feed/一',
      folder_id: 'folder-1',
      q: 'compiler',
      page: 2,
      limit: 30,
    })
    const params = new URLSearchParams(query.slice(1))
    expect(params.get('view')).toBe('unread')
    expect(params.get('subscription_id')).toBe('feed/一')
    expect(params.get('folder_id')).toBe('folder-1')
    expect(params.get('q')).toBe('compiler')
    expect(params.get('page')).toBe('2')
    expect(params.get('limit')).toBe('30')
  })
})

describe('ReaderClient link submission', () => {
  it('posts a LinkCreateRequest to /api/links', async () => {
    const response = {
      link_id: '11111111-1111-1111-1111-111111111111',
      job_id: '22222222-2222-2222-2222-222222222222',
      status: 'pending',
    }
    const fn = mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    await expect(
      client.submitLink({
        url: 'https://example.com/article',
        requested_library_kind: 'auto',
      }),
    ).resolves.toEqual({ ok: true, data: response })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links`,
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          url: 'https://example.com/article',
          requested_library_kind: 'auto',
        }),
      }),
    )
  })
})

describe('ReaderClient classification rules API', () => {
  const rule = {
    id: '11111111-1111-1111-1111-111111111111', host: 'example.com', target_kind: 'site', enabled: true,
    revision: 3, created_at: '2026-07-21T00:00:00Z', updated_at: '2026-07-21T00:00:00Z',
  }

  it('lists rules and preserves nullable shared scope fields', async () => {
    const fn = mockFetch(() => jsonResponse([rule]))
    const result = await authenticatedClient().getClassificationRules()
    expect(result).toEqual({ ok: true, data: [rule] })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/library-classification-rules`, expect.objectContaining({ method: 'GET' }))
  })

  it('sends revision guarded updates including explicit null clearing', async () => {
    const fn = mockFetch(() => jsonResponse({ ...rule, host: 'example.net', revision: 4 }))
    await expect(authenticatedClient().updateClassificationRule(rule.id, 3, { host: 'example.net', identity_adapter: null, path_prefix: null })).resolves.toMatchObject({ ok: true })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/library-classification-rules/${rule.id}`, expect.objectContaining({ method: 'PATCH', headers: expect.objectContaining({ 'If-Match': '"3"' }), body: JSON.stringify({ host: 'example.net', identity_adapter: null, path_prefix: null }) }))
  })
})

describe('ReaderClient RSS API', () => {
  it('读取订阅导航并接受 canonical feed_url', async () => {
    mockFetch(() => jsonResponse({
      folders: [{ id: 'folder-1', name: '技术' }],
      subscriptions: [{ id: 'feed-1', feed_url: 'https://example.com/atom.xml', title: 'Example' }],
      counts: { all: 4, unread: 2, starred: 1, later: 1 },
    }))
    const client = authenticatedClient()

    const result = await client.getSubscriptions()

    expect(result.ok).toBe(true)
    if (result.ok) expect(result.data.subscriptions[0].feed_url).toContain('atom.xml')
  })

  it('条目状态写入 /state 并发送 JSON patch', async () => {
    const feedItem = {
      id: 'item/一',
      subscription_id: 'feed-1',
      title: 'Item',
      url: 'https://example.com/item',
      starred_at: '2026-07-15T00:00:00Z',
    }
    const fn = mockFetch(() => jsonResponse(feedItem))
    const client = authenticatedClient()

    await expect(client.updateFeedItem('item/一', { starred: true })).resolves.toEqual({
      ok: true,
      data: feedItem,
    })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/feed-items/item%2F%E4%B8%80/state`,
      expect.objectContaining({
        method: 'PUT',
        body: JSON.stringify({ starred: true }),
      }),
    )
  })

  it('OPML 导出兼容直接 XML 响应', async () => {
    mockFetch(() =>
      new Response('<opml version="2.0"></opml>', {
        status: 200,
        headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
      }),
    )
    const client = authenticatedClient()
    await expect(client.exportSubscriptionsOPML()).resolves.toEqual({
      ok: true,
      data: '<opml version="2.0"></opml>',
    })
  })

  it('OPML 导入保留自动去重统计', async () => {
    mockFetch(() =>
      jsonResponse({ imported: 3, folders: 1, skipped: 2, errors: [] }),
    )
    const client = authenticatedClient()

    await expect(client.importSubscriptionsOPML('<opml/>')).resolves.toEqual({
      ok: true,
      data: { imported: 3, folders: 1, skipped: 2, errors: [] },
    })
  })
})

describe('ReaderClient v2 archive download', () => {
  it('does not download a private archive before an identity lease is installed', async () => {
    const fn = mockFetch(() => new Response('{"schema_version":2}'))

    await expect(new ReaderClient({ baseURL: BASE }).downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(fn).not.toHaveBeenCalled()
  })

  it('discards a binary response from a replaced session', async () => {
    const authority = new IdentityAuthority()
    const lease = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const onIdentityMismatch = vi.fn()
    mockFetch(() =>
      new Response('{"schema_version":2}', {
        status: 200,
        headers: { [DATA_NAMESPACE_HEADER]: 'server-B' },
      }),
    )

    await expect(
      new ReaderClient({
        baseURL: BASE,
        identity: lease,
        onIdentityMismatch,
      }).downloadArchiveV2(),
    ).resolves.toMatchObject({ ok: false, error: { kind: 'identity-mismatch' } })
    expect(onIdentityMismatch).toHaveBeenCalledOnce()
    expect(lease.capture('after binary mismatch').signal.aborted).toBe(true)
  })

  it('discards archive bytes when the identity lease changes during the stream', async () => {
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    let bodyStarted!: () => void
    const started = new Promise<void>((resolve) => {
      bodyStarted = resolve
    })
    let resolveBody!: (body: Uint8Array) => void
    const body = new Promise<Uint8Array>((resolve) => {
      resolveBody = resolve
    })
    const cancel = vi.fn()
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        bodyStarted()
        return body.then((bytes) => {
          controller.enqueue(bytes)
        })
      },
      cancel,
    })
    mockFetch(() => new Response(stream, {
      status: 200,
      headers: { [DATA_NAMESPACE_HEADER]: 'server-A' },
    }))
    const request = new ReaderClient({ baseURL: BASE, identity: leaseA }).downloadArchiveV2()
    await started

    resolveBody(await validArchiveV2Bytes())
    authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    await expect(request).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(cancel).toHaveBeenCalledOnce()
  })

  it('uses an explicit canonical full selector and returns the verified raw bytes', async () => {
    const raw = await validArchiveV2Bytes()
    const fn = mockFetch(() => archiveResponse(raw))
    const result = await authenticatedClient({
      installationToken: 'archive-token',
    }).downloadArchiveV2()
    expect(result.ok).toBe(true)
    if (result.ok) {
      expect(Array.from(await readBlobBytes(result.data))).toEqual(Array.from(raw))
      expect(result.data.type).toBe('application/json')
    }
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/export/v2?sections=base,thoughts,notes`, expect.objectContaining({
      method: 'GET', headers: expect.objectContaining({ Authorization: 'Bearer archive-token' }),
    }))
  })

  it('uses the canonical selected selector rather than falling back to an omitted query', async () => {
    const selection = { includeNotes: true }
    const fn = mockFetch(async () => archiveResponse(await validArchiveV2Bytes(selection)))

    await expect(authenticatedClient().downloadArchiveV2(selection)).resolves.toMatchObject({ ok: true })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/export/v2?sections=base,notes`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('normalizes non-success responses rather than downloading an error body', async () => {
    mockFetch(() =>
      new Response(JSON.stringify({ error: { message: 'denied' } }), {
        status: 401,
        headers: {
          'Content-Type': 'application/json',
          [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE,
        },
      }),
    )
    await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({ ok: false, error: { kind: 'unauthorized', message: 'denied' } })
  })

  it('preserves the documented unavailable Reader archive error without constructing a Blob', async () => {
    const blobSpy = vi.fn()
    vi.stubGlobal('Blob', blobSpy)
    mockFetch(() =>
      new Response(JSON.stringify({ error: {
        message: 'selected Reader archive sections are unavailable',
        error_code: 'archive_reader_unavailable',
      } }), {
        status: 503,
        headers: {
          'Content-Type': 'application/json',
          [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE,
        },
      }),
    )

    await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: {
        kind: 'other',
        status: 503,
        errorCode: 'archive_reader_unavailable',
      },
    })
    expect(blobSpy).not.toHaveBeenCalled()
  })

  it('never constructs a Blob for invalid, oversized, or identity-mismatched archives', async () => {
    const blobSpy = vi.fn()
    vi.stubGlobal('Blob', blobSpy)
    const responses = [
      () => new Response('<!doctype html>', {
        status: 200,
        headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
      }),
      () => new Response(null, {
        status: 200,
        headers: {
          [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE,
          'Content-Length': String(ARCHIVE_V2_MAX_BYTES + 1),
        },
      }),
      () => new Response('not accepted', {
        status: 200,
        headers: { [DATA_NAMESPACE_HEADER]: 'another-identity' },
      }),
    ]

    for (const response of responses) {
      mockFetch(response)
      await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({ ok: false })
    }
    expect(blobSpy).not.toHaveBeenCalled()
  })

  it('discards an archive error stream when the identity lease changes', async () => {
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    let bodyStarted!: () => void
    const started = new Promise<void>((resolve) => {
      bodyStarted = resolve
    })
    let resolveBody!: (body: string) => void
    const body = new Promise<string>((resolve) => {
      resolveBody = resolve
    })
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        bodyStarted()
        return body.then((text) => {
          controller.enqueue(new TextEncoder().encode(text))
          controller.close()
        })
      },
    })
    mockFetch(() => new Response(stream, {
      status: 401,
      headers: { [DATA_NAMESPACE_HEADER]: 'server-A' },
    }))
    const request = new ReaderClient({ baseURL: BASE, identity: leaseA }).downloadArchiveV2()
    await started

    resolveBody(JSON.stringify({ error: { message: 'denied' } }))
    authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    await expect(request).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it('rejects an oversized Content-Length before buffering a body', async () => {
    const cancel = vi.fn()
    const stream = new ReadableStream<Uint8Array>({ cancel })
    const fn = mockFetch(() => new Response(stream, {
      status: 200,
      headers: {
        [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE,
        'Content-Length': String(ARCHIVE_V2_MAX_BYTES + 1),
      },
    }))

    await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: { errorCode: 'archive_too_large_for_browser' },
    })
    const init = fn.mock.calls[0]?.[1] as RequestInit
    expect((init.signal as AbortSignal).aborted).toBe(true)
    expect(cancel).toHaveBeenCalledOnce()
  })

  it('allows exactly 64 MiB to reach validation rather than rejecting it as oversized', async () => {
    const chunk = new Uint8Array(ARCHIVE_V2_MAX_BYTES)
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(chunk)
        controller.close()
      },
    })
    const fn = mockFetch(() => new Response(stream, {
      status: 200,
      headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
    }))

    await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: { errorCode: 'archive_validation_failed' },
    })
    const init = fn.mock.calls[0]?.[1] as RequestInit
    expect((init.signal as AbortSignal).aborted).toBe(false)
  })

  it('aborts a stream when cumulative bytes exceed 64 MiB', async () => {
    const chunk = new Uint8Array(ARCHIVE_V2_MAX_BYTES + 1)
    const cancel = vi.fn()
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(chunk)
      },
      cancel,
    })
    const fn = mockFetch(() => new Response(stream, {
      status: 200,
      headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
    }))

    await expect(authenticatedClient().downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: { errorCode: 'archive_too_large_for_browser' },
    })
    const init = fn.mock.calls[0]?.[1] as RequestInit
    expect((init.signal as AbortSignal).aborted).toBe(true)
    expect(cancel).toHaveBeenCalledOnce()
  })

  it('discards a valid archive when identity changes while checksum validation is pending', async () => {
    const raw = await validArchiveV2Bytes()
    const authority = new IdentityAuthority()
    const leaseA = authority.install({
      serverClientDataNamespace: VALID_DATA_NAMESPACE,
      physicalNamespace: 'physical-A',
    })
    const originalDigest = globalThis.crypto.subtle.digest.bind(globalThis.crypto.subtle)
    vi.spyOn(globalThis.crypto.subtle, 'digest').mockImplementation(async (algorithm, data) => {
      const digest = await originalDigest(algorithm, data)
      authority.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      return digest
    })
    mockFetch(() => archiveResponse(raw))

    await expect(new ReaderClient({ baseURL: BASE, identity: leaseA }).downloadArchiveV2()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })
})

describe('ReaderClient.getLinks 成功', () => {
  it('返回 ok 且 data 为分页结构', async () => {
    mockFetch(() => jsonResponse(validPage))
    const client = authenticatedClient({ installationToken: 'tk' })
    const r = await client.getLinks({ q: 'x' })
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.data.items).toEqual([])
  })

  it('注入 Authorization: Bearer 头', async () => {
    const fn = mockFetch(() => jsonResponse(validPage))
    const client = authenticatedClient({
      installationToken: 'secret-token',
    })
    await client.getLinks()
    const init = fn.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBe('Bearer secret-token')
  })

  it('空安装令牌不带鉴权头', async () => {
    const fn = mockFetch(() => jsonResponse(validPage))
    const client = authenticatedClient()
    await client.getLinks()
    const init = fn.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBeUndefined()
  })
})

describe('ReaderClient.getLink 成功', () => {
  // 详情刻意带 include_content=false：原文默认折叠，打开一篇文章不该顺带把
  // 整篇正文拖过网络；has_content 告诉前端「有没有原文」就够了。
  it('读取单条详情时显式声明不要正文', async () => {
    const detail = makeLink({ id: 'id/一', has_content: true })
    const fn = mockFetch(() => jsonResponse(detail))
    const client = authenticatedClient()

    await expect(client.getLink('id/一')).resolves.toEqual({ ok: true, data: detail })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/id%2F%E4%B8%80?include_content=false`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('rejects a fractional metadata revision before JSON.parse rounds it', async () => {
    const rawDetail = JSON.stringify(makeLink()).replace(
      '"metadata_revision":1',
      '"metadata_revision":9007199254740991.1',
    )
    mockFetch(() => rawJSONResponse(rawDetail))

    const result = await authenticatedClient().getLink('link-1')

    expect(result).toMatchObject({ ok: false, error: { kind: 'other' } })
  })

  it('caller cancellation aborts the point-read fetch signal', async () => {
    const detail = makeLink({ id: 'cancelled-detail' })
    let observedSignal: AbortSignal | null = null
    let markStarted!: () => void
    const started = new Promise<void>((resolve) => {
      markStarted = resolve
    })
    let releaseFetch!: (response: Response) => void
    const response = new Promise<Response>((resolve) => {
      releaseFetch = resolve
    })
    mockFetch((_input, init) => {
      observedSignal = init?.signal as AbortSignal
      markStarted()
      return response
    })
    const caller = new AbortController()
    const request = authenticatedClient().getLink(detail.id, { signal: caller.signal })
    await started

    caller.abort()

    expect(observedSignal).not.toBeNull()
    expect((observedSignal as AbortSignal | null)?.aborted).toBe(true)
    releaseFetch(jsonResponse(detail))
    await expect(request).resolves.toMatchObject({
      ok: false,
      error: { kind: 'timeout' },
    })
  })
})

describe('ReaderClient.getContent', () => {
  it('按需读已保存原文', async () => {
    const body = {
      link_id: 'L1',
      content: '已保存原文',
      content_document: '## 已保存原文',
      content_format: 'markdown' as const,
      fetcher_type: 'stored',
      content_source: 'fetched' as const,
      content_revision: 4,
    }
    const fn = mockFetch(() => jsonResponse(body))
    const client = authenticatedClient()

    await expect(client.getContent('L1')).resolves.toEqual({ ok: true, data: body })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/L1/content`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('404（还没保存原文）归一化为 other，不当成成功', async () => {
    mockFetch(() =>
      new Response('{}', {
        status: 404,
        headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE },
      }),
    )
    const client = authenticatedClient()

    const res = await client.getContent('L1')

    expect(res.ok).toBe(false)
  })
})

describe('ReaderClient.refreshLink 成功', () => {
  it('202 返回后端 SubmitResponse，而不是把提交结果伪装成 LinkResponse', async () => {
    mockFetch(() =>
      jsonResponse({ link_id: 'id1', job_id: 'job-1', status: 'pending' }, { status: 202 }),
    )
    const client = authenticatedClient()

    const r = await client.refreshLink('id1')

    expect(r).toEqual({
      ok: true,
      data: { link_id: 'id1', job_id: 'job-1', status: 'pending' },
    })
  })
})

describe('ReaderClient.saveContent', () => {
  it('完整 LinkContentResponse 原样返回', async () => {
    const response = {
      link_id: 'id1',
      content: '# saved',
      content_document: '# saved',
      content_format: 'markdown' as const,
      fetcher_type: 'basic',
      content_source: 'fetched' as const,
      content_revision: 1,
    }
    mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    await expect(client.saveContent('id1')).resolves.toEqual({
      ok: true,
      data: response,
    })
  })

  it.each([
    { content: '# saved', content_format: 'markdown', fetcher_type: 'basic' },
    { link_id: 'id1', content: '# saved', content_format: 'markdown' },
    {
      link_id: 'id1',
      content: '# saved',
      content_format: 'markdown',
      fetcher_type: 7,
    },
    {
      link_id: 'id1',
      content: '# saved',
      content_format: 'unsafe',
      fetcher_type: 'basic',
    },
    // content_revision 缺失也必须失败关闭：它是正文缓存键，也是划线 envelope
    // 的代次。放行一个没有代次的响应，客户端只能拿 undefined 去当代次用。
    {
      link_id: 'id1',
      content: '# saved',
      content_format: 'markdown',
      fetcher_type: 'basic',
    },
  ])('缺失或破坏 required 字段时失败关闭：%j', async (response) => {
    mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    const result = await client.saveContent('id1')

    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.kind).toBe('other')
  })
})

describe('ReaderClient.replaceContent', () => {
  it('使用 PUT 并返回结构化正文响应', async () => {
    const response = {
      link_id: 'id1',
      content: 'Heading\n\nBody',
      content_document: '# Heading\n\nBody',
      content_format: 'markdown' as const,
      fetcher_type: 'basic',
      content_source: 'fetched' as const,
      content_revision: 2,
    }
    const fn = mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    await expect(client.replaceContent('id1')).resolves.toEqual({
      ok: true,
      data: response,
    })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/id1/content`,
      expect.objectContaining({ method: 'PUT' }),
    )
  })
})

describe('ReaderClient.editContent', () => {
  it('使用 PATCH、发送 revision-bound body 并返回 source-aware response', async () => {
    const response = {
      link_id: 'id/一',
      content: 'Edited body',
      content_document: '# Edited body',
      content_format: 'markdown' as const,
      fetcher_type: 'stored',
      content_source: 'user' as const,
      content_revision: 8,
    }
    const request = { content: '# Edited body', expected_content_revision: 7 }
    const fn = mockFetch(() => jsonResponse(response))
    const client = authenticatedClient()

    await expect(client.editContent('id/一', request)).resolves.toEqual({ ok: true, data: response })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/id%2F%E4%B8%80/content`,
      expect.objectContaining({
        method: 'PATCH',
        body: JSON.stringify(request),
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      }),
    )
  })

  it.each(['content_revision_conflict', 'content_empty', 'content_too_large'])('保留 %s errorCode', async (errorCode) => {
    const status = errorCode === 'content_too_large' ? 413 : errorCode === 'content_empty' ? 400 : 409
    mockFetch(() => jsonResponse(
      { error: { code: status, error_code: errorCode, message: errorCode } },
      { status },
    ))
    const result = await authenticatedClient().editContent('id1', {
      content: 'body',
      expected_content_revision: 7,
    })

    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.errorCode).toBe(errorCode)
  })
})

describe('ReaderClient.saveNoteDraft', () => {
  it.each([
    { status: 404, errorCode: 'reader_not_found' },
    { status: 409, errorCode: 'revision_conflict' },
  ])('preserves $errorCode for Note draft save failures', async ({ status, errorCode }) => {
    mockFetch(() => jsonResponse(
      { error: { code: status, error_code: errorCode, message: errorCode } },
      { status },
    ))

    const result = await authenticatedClient().saveNoteDraft('note-1', {
      content: 'draft',
      expected_draft_revision: 0,
    })

    expect(result.ok).toBe(false)
    if (!result.ok) {
      expect(result.error.status).toBe(status)
      expect(result.error.errorCode).toBe(errorCode)
    }
  })
})

describe('ReaderClient.previewLinkConversion', () => {
  const preview = {
    link_id: 'id1',
    current_kind: 'reading' as const,
    target_kind: 'site' as const,
    expected_content_revision: 7,
    destructive: true,
    saved_original: true,
    translation_count: 2,
    reparse_required: false,
    annotation_policy: 'extract_local_note_then_hide_stale',
  }

  it('发送 target_kind 并返回完整 revision-bound preview', async () => {
    const fn = mockFetch(() => jsonResponse(preview))
    const client = authenticatedClient()

    await expect(client.previewLinkConversion('id1', { target_kind: 'site' })).resolves.toEqual({ ok: true, data: preview })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/id1/conversion-preview`,
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ target_kind: 'site' }) }),
    )
  })

  it('对缺少破坏性资产字段的响应失败关闭', async () => {
    mockFetch(() => jsonResponse({ ...preview, translation_count: undefined }))
    const result = await authenticatedClient().previewLinkConversion('id1', { target_kind: 'site' })
    expect(result.ok).toBe(false)
  })
})

describe('ReaderClient.convertLink', () => {
  it('发送确认后的 CAS 请求并接受结构化 Reader target', async () => {
    const response = {
      link_id: 'id1', library_kind: 'site' as const, content_revision: 8, status: 'done',
      site_id: 'site1', site_revision: 4, entry_id: 'entry1', reparse_required: false,
      reader_target: { view: 'sites' as const, link_id: 'id1', site_id: 'site1', entry_id: 'entry1' },
    }
    const fn = mockFetch(() => jsonResponse(response))
    const request = { target_kind: 'site' as const, expected_content_revision: 7, expected_site_revision: 3, target_site_id: 'site1', confirm_destructive: true }
    await expect(authenticatedClient().convertLink('id1', request)).resolves.toEqual({ ok: true, data: response })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/links/id1/convert`, expect.objectContaining({ method: 'POST', body: JSON.stringify(request) }))
  })
})

describe('ReaderClient.getTags', () => {
  it('按 collection scope 传递 library_kind 查询参数', async () => {
    const fn = mockFetch(() => jsonResponse([{ tag: 'whiteboard', count: 5, reading_count: 2, site_count: 3 }]))
    await expect(authenticatedClient().getTags('all')).resolves.toEqual({ ok: true, data: [{ tag: 'whiteboard', count: 5, reading_count: 2, site_count: 3 }] })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/tags?library_kind=all`, expect.objectContaining({ method: 'GET' }))
  })

  it('accepts a reading aggregate only when count matches reading_count', async () => {
    const data = [{ tag: 'reading-only', count: 2, reading_count: 2, site_count: 3 }]
    mockFetch(() => jsonResponse(data))

    await expect(authenticatedClient().getTags('reading')).resolves.toEqual({ ok: true, data })
  })

  it('reading scope 拒绝旧后端忽略 query 后返回的 bare counts', async () => {
    const fn = mockFetch(() => jsonResponse([{ tag: 'site-only', count: 4 }]))
    const result = await authenticatedClient().getTags('reading')

    expect(fn).toHaveBeenCalledWith(`${BASE}/api/tags?library_kind=reading`, expect.objectContaining({ method: 'GET' }))
    expect(result.ok).toBe(false)
  })

  it.each([
    ['reading', { count: 4, reading_count: 1, site_count: 3 }],
    ['site', { count: 4, reading_count: 1, site_count: 3 }],
    ['all', { count: 5, reading_count: 1, site_count: 3 }],
  ] as const)('%s scope rejects a count from another aggregate partition', async (scope, counts) => {
    mockFetch(() => jsonResponse([{ tag: 'mixed', ...counts }]))

    const result = await authenticatedClient().getTags(scope)

    expect(result.ok).toBe(false)
  })
})

describe('ReaderClient.searchLibrary', () => {
  it('请求分组搜索并验证阅读、网站和首个想法页', async () => {
    const data = { reading: { items: [makeLink({ id: 'reading-1' })], total_hint: 1 }, sites: { items: [{ id: 'site-1', name: 'Example', matched_entries: [{ id: 'entry-1', name: 'Docs', url: 'https://example.com/docs' }] }], total_hint: 1 } }
    const fn = mockFetch(() => jsonResponse(data))
    await expect(authenticatedClient().searchLibrary(' docs ', 20, 5)).resolves.toEqual({ ok: true, data })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/search?q=docs&reading_limit=20&site_limit=5&thought_limit=20`, expect.objectContaining({ method: 'GET' }))
  })

  it('发送不透明的想法分页 cursor', async () => {
    const data = {
      reading: { items: [], total_hint: 0 },
      sites: { items: [], total_hint: 0 },
      thoughts: { items: [], total_hint: 21 },
    }
    const fn = mockFetch(() => jsonResponse(data))
    await expect(authenticatedClient().searchLibrary(' thoughts ', 50, 10, 20, 'opaque-page')).resolves.toEqual({ ok: true, data })
    expect(fn).toHaveBeenCalledWith(`${BASE}/api/search?q=thoughts&reading_limit=50&site_limit=10&thought_limit=20&thought_after=opaque-page`, expect.objectContaining({ method: 'GET' }))
  })

  it('rejects a fractional nested Link metadata revision before JSON.parse rounds it', async () => {
    const rawSearch = JSON.stringify({
      reading: { items: [makeLink()], total_hint: 1 },
      sites: { items: [], total_hint: 0 },
    }).replace(
      '"metadata_revision":1',
      '"metadata_revision":9007199254740991.1',
    )
    mockFetch(() => rawJSONResponse(rawSearch))

    const result = await authenticatedClient().searchLibrary('docs')

    expect(result).toMatchObject({ ok: false, error: { kind: 'other' } })
  })
})

describe('ReaderClient translations', () => {
  const translation = {
    id: 'T1',
    link_id: 'L1',
    scope: 'selection' as const,
    block_key: 'summary',
    start_offset: 0,
    end_offset: 5,
    source_text: 'hello',
    translated_text: null,
    source_format: 'plain' as const,
    target_language: 'zh-CN' as const,
    status: 'pending' as const,
    model: null,
    error_msg: null,
    source_content_revision: null,
    stale: false,
    created_at: '2026-07-15T00:00:00Z',
    updated_at: '2026-07-15T00:00:00Z',
  }

  it('创建选段翻译时发送JSON请求体并编码链接ID', async () => {
    const fn = mockFetch(() => jsonResponse(translation, { status: 202 }))
    const client = authenticatedClient()

    await expect(
      client.createTranslation('id/一', {
        scope: 'selection',
        block_key: 'summary',
        start_offset: 0,
        end_offset: 5,
        source_text: 'hello',
        expected_source_hash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
        force: false,
      }),
    ).resolves.toEqual({ ok: true, data: translation })

    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/id%2F%E4%B8%80/translations`,
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          scope: 'selection',
          block_key: 'summary',
          start_offset: 0,
          end_offset: 5,
          source_text: 'hello',
          expected_source_hash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
          force: false,
        }),
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      }),
    )
  })

  it('读取数据库中的翻译列表', async () => {
    const fn = mockFetch(() =>
      jsonResponse({
        current_content_revision: 7,
        current_summary_source_hash: 'a'.repeat(64),
        items: [translation],
      }),
    )
    const client = authenticatedClient()

    await expect(client.getTranslations('L1')).resolves.toEqual({
      ok: true,
      data: {
        current_content_revision: 7,
        current_summary_source_hash: 'a'.repeat(64),
        items: [translation],
      },
    })
    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/links/L1/translations`,
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('接受表示尚无 saved-content generation 的零 revision envelope', async () => {
    mockFetch(() => jsonResponse({
      current_content_revision: 0,
      current_summary_source_hash: null,
      items: [],
    }))
    const client = authenticatedClient()

    await expect(client.getTranslations('L1')).resolves.toEqual({
      ok: true,
      data: {
        current_content_revision: 0,
        current_summary_source_hash: null,
        items: [],
      },
    })
  })

  it.each([
    {
      name: '负数 current_content_revision',
      payload: {
        current_content_revision: -1,
        current_summary_source_hash: null,
        items: [],
      },
    },
    {
      name: '零 source_content_revision',
      payload: {
        current_content_revision: 7,
        current_summary_source_hash: null,
        items: [{ ...translation, source_content_revision: 0 }],
      },
    },
  ])('翻译 revision 超出 OpenAPI 范围时失败关闭：$name', async ({ payload }) => {
    mockFetch(() => jsonResponse(payload))
    const client = authenticatedClient()

    const result = await client.getTranslations('L1')

    expect(result.ok).toBe(false)
  })

  it('翻译列表缺少状态字段时失败关闭', async () => {
    const broken: Record<string, unknown> = { ...translation }
    delete broken.status
    mockFetch(() => jsonResponse({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [broken],
    }))
    const client = authenticatedClient()

    const result = await client.getTranslations('L1')

    expect(result.ok).toBe(false)
  })

  it('翻译列表缺少权威 current_content_revision 时失败关闭', async () => {
    mockFetch(() => jsonResponse({ current_summary_source_hash: null, items: [translation] }))
    const client = authenticatedClient()

    const result = await client.getTranslations('L1')

    expect(result.ok).toBe(false)
  })

  it.each([undefined, '', 'A'.repeat(64), 'a'.repeat(63)])(
    '翻译列表缺少或破坏权威 current_summary_source_hash 时失败关闭：%#',
    async (currentSummarySourceHash) => {
      const payload: Record<string, unknown> = {
        current_content_revision: 7,
        current_summary_source_hash: currentSummarySourceHash,
        items: [translation],
      }
      if (currentSummarySourceHash === undefined) {
        delete payload.current_summary_source_hash
      }
      mockFetch(() => jsonResponse(payload))
      const client = authenticatedClient()

      const result = await client.getTranslations('L1')

      expect(result.ok).toBe(false)
    },
  )

  it('翻译条目缺少 nullable source_content_revision 时失败关闭', async () => {
    const broken: Record<string, unknown> = { ...translation }
    delete broken.source_content_revision
    mockFetch(() => jsonResponse({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [broken],
    }))
    const client = authenticatedClient()

    const result = await client.getTranslations('L1')

    expect(result.ok).toBe(false)
  })
})

describe('ReaderClient.getDomainSummaries', () => {
  it('读取后端 truthful 域名聚合并隐藏 transport envelope', async () => {
    const fn = mockFetch(() =>
      jsonResponse({
        domains: [
          { domain: 'example.com', count: 240 },
          { domain: 'docs.example.org', count: 10 },
        ],
        total: 251,
      }),
    )
    const client = authenticatedClient()

    const r = await client.getDomainSummaries()

    expect(fn).toHaveBeenCalledWith(`${BASE}/api/tree?view=domains`, expect.any(Object))
    expect(r).toEqual({
      ok: true,
      data: {
        domains: [
          { domain: 'example.com', count: 240 },
          { domain: 'docs.example.org', count: 10 },
        ],
        total: 251,
      },
    })
  })

  it('显式请求 reading scope 并要求服务端回显权威 scope', async () => {
    const fn = mockFetch(() =>
      jsonResponse({
        library_kind: 'reading',
        domains: [{ domain: 'reading.example', count: 1 }],
        total: 2,
      }),
    )
    const result = await authenticatedClient().getDomainSummaries('reading')

    expect(fn).toHaveBeenCalledWith(
      `${BASE}/api/tree?view=domains&library_kind=reading`,
      expect.any(Object),
    )
    expect(result).toEqual({
      ok: true,
      data: {
        library_kind: 'reading',
        domains: [{ domain: 'reading.example', count: 1 }],
        total: 2,
      },
    })
  })

  it('scoped 请求拒绝旧后端忽略 query 后返回的无 scope envelope', async () => {
    mockFetch(() => jsonResponse({ domains: [{ domain: 'site-only.example', count: 3 }], total: 3 }))

    const result = await authenticatedClient().getDomainSummaries('reading')

    expect(result.ok).toBe(false)
  })

  it('缺少独立 total 时失败关闭，不能用 domain bucket 求和代替', async () => {
    mockFetch(() => jsonResponse({ domains: [{ domain: 'example.com', count: 1 }] }))
    const client = authenticatedClient()

    const result = await client.getDomainSummaries()

    expect(result.ok).toBe(false)
  })
})

describe('ReaderClient 错误归一化', () => {
  it('409 source-CAS 冲突保留稳定 slug 与权威 current_identity', async () => {
    mockFetch(() =>
      jsonResponse(
        {
          error: {
            code: 409,
            error_code: 'content_revision_conflict',
            message: 'saved content changed',
            current_identity: {
              content_revision: 8,
              block_key: 'content',
            },
          },
        },
        { status: 409 },
      ),
    )
    const client = authenticatedClient()

    const result = await client.createTranslation('L1', {
      scope: 'full',
      expected_content_revision: 7,
      force: false,
    })

    expect(result.ok).toBe(false)
    if (!result.ok) {
      expect(result.error.status).toBe(409)
      expect(result.error.errorCode).toBe('content_revision_conflict')
      expect(result.error.currentIdentity).toEqual({
        content_revision: 8,
        block_key: 'content',
      })
    }
  })

  it('401 → unauthorized，并解析 error_code', async () => {
    mockFetch(() =>
      jsonResponse(
        {
          error: {
            code: 401,
            error_code: 'invalid_token',
            message: '无效令牌',
          },
        },
        { status: 401 },
      ),
    )
    const client = authenticatedClient()
    const r = await client.getLinks()
    expect(r.ok).toBe(false)
    if (!r.ok) {
      expect(r.error.kind).toBe('unauthorized')
      expect(r.error.errorCode).toBe('invalid_token')
      expect(r.error.message).toBe('无效令牌')
    }
  })

  it('网络错误（fetch 抛 TypeError）→ network-unreachable', async () => {
    mockFetch(() => {
      throw new TypeError('Failed to fetch')
    })
    const client = authenticatedClient()
    const r = await client.getLinks()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('network-unreachable')
  })

  it('AbortError → timeout', async () => {
    mockFetch(() => {
      throw new DOMException('aborted', 'AbortError')
    })
    const client = authenticatedClient()
    const r = await client.getLinks()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('timeout')
  })

  it('响应头已到但 body 挂起 → 超时计时器仍覆盖 res.text()（回归：54f943f 同款缺口）', async () => {
    // 伪造 headers 已返回、text() 永不 resolve（直到 abort 才 reject）的
    // 响应：若 clearTimeout 在 headers 到达即触发，本用例会无限挂起；
    // 计时器存活到 body 读完时，abort 让 text() reject → 归一化 timeout。
    const fn = vi.fn((_input: string, init?: RequestInit) => {
      const signal = init?.signal as AbortSignal
      const stalled = {
        ok: true,
        status: 200,
        headers: new Headers({ [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE }),
        text: () =>
          new Promise<string>((_resolve, reject) => {
            signal.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError')),
            )
          }),
      } as unknown as Response
      return Promise.resolve(stalled)
    })
    vi.stubGlobal('fetch', fn)
    const client = authenticatedClient({ timeoutMs: 50 })
    const r = await client.getLinks()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('timeout')
  })

  it('429 → rate-limited，并带 Retry-After 秒数', async () => {
    mockFetch(() =>
      jsonResponse(
        {
          error: {
            code: 429,
            error_code: 'cooldown_active',
            message: '冷却中',
          },
        },
        {
          status: 429,
          headers: { 'Retry-After': '30', 'Content-Type': 'application/json' },
        },
      ),
    )
    const client = authenticatedClient()
    const r = await client.refreshLink('id1')
    expect(r.ok).toBe(false)
    if (!r.ok) {
      expect(r.error.kind).toBe('rate-limited')
      expect(r.error.retryAfterSeconds).toBe(30)
      expect(r.error.errorCode).toBe('cooldown_active')
    }
  })

  it('500 → other', async () => {
    mockFetch(() => jsonResponse({ error: { code: 500, message: 'boom' } }, { status: 500 }))
    const client = authenticatedClient()
    const r = await client.getTags()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('other')
  })

  it('200 但响应体不符契约 → other（shape mismatch）', async () => {
    mockFetch(() => jsonResponse({ wrong: true }))
    const client = authenticatedClient()
    const r = await client.getLinks()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('other')
  })
})

describe('ReaderClient Link metadata CAS', () => {
  const request = { title: null, summary: null, tags: [] }

  it('sends the JavaScript-safe maximum revision as an exact quoted token', async () => {
    const fetchSpy = mockFetch(() =>
      jsonResponse({ link_id: 'link-1', metadata_revision: Number.MAX_SAFE_INTEGER }),
    )

    await expect(
      authenticatedClient().patchLinkMetadata('link-1', Number.MAX_SAFE_INTEGER, request),
    ).resolves.toEqual({
      ok: true,
      data: { link_id: 'link-1', metadata_revision: Number.MAX_SAFE_INTEGER },
    })
    expect(fetchSpy).toHaveBeenCalledWith(
      BASE + '/api/links/link-1/metadata',
      expect.objectContaining({
        method: 'PATCH',
        headers: expect.objectContaining({
          'If-Match': '"9007199254740991"',
        }),
        body: JSON.stringify(request),
      }),
    )
  })

  it('rejects an unsafe metadata revision before calling fetch', async () => {
    const fetchSpy = mockFetch(() =>
      jsonResponse({ link_id: 'link-1', metadata_revision: 1 }),
    )
    const unsafeRevision = JSON.parse('9007199254740993')

    const result = await authenticatedClient().patchLinkMetadata('link-1', unsafeRevision, request)

    expect(result).toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('rejects a fractional metadata PATCH response before JSON.parse rounds it', async () => {
    mockFetch(() => rawJSONResponse(
      '{"link_id":"link-1","metadata_revision":9007199254740990.9}',
    ))

    const result = await authenticatedClient().patchLinkMetadata('link-1', 1, request)

    expect(result).toMatchObject({ ok: false, error: { kind: 'other' } })
  })
})

describe('ReaderClient.testConnection', () => {
  it('严格 identity handshake 成功才算连通', async () => {
    const fetchSpy = mockFetch(() =>
      jsonResponse(
        {
          client_data_namespace: VALID_DATA_NAMESPACE,
          representation_contract: 'v3',
        },
        { headers: { [DATA_NAMESPACE_HEADER]: VALID_DATA_NAMESPACE } },
      ),
    )
    const client = new ReaderClient({ baseURL: BASE })
    const r = await client.testConnection()
    expect(r.ok).toBe(true)
    expect(fetchSpy).toHaveBeenCalledWith(
      `${BASE}/api/session`,
      expect.objectContaining({ method: 'GET', cache: 'no-store' }),
    )
  })

  it('markerless identity fails closed without probing a private collection', async () => {
    const fetchSpy = mockFetch(() =>
      jsonResponse(
        {
          client_data_namespace: VALID_DATA_NAMESPACE,
          representation_contract: 'v3',
        },
        undefined,
        null,
      ),
    )
    const result = await new ReaderClient({ baseURL: BASE }).testConnection()

    expect(result).toMatchObject({ ok: false, error: { kind: 'identity-mismatch' } })
    expect(fetchSpy).toHaveBeenCalledOnce()
    expect(fetchSpy.mock.calls[0]?.[0]).toBe(`${BASE}/api/session`)
  })

  it('401 → unauthorized', async () => {
    mockFetch(() =>
      jsonResponse({ error: { code: 401, message: 'x' } }, { status: 401 }, null),
    )
    const client = new ReaderClient({ baseURL: BASE })
    const r = await client.testConnection()
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error.kind).toBe('unauthorized')
  })
})

// 跨语言一致性：这个头名在后端是 internal/session.HeaderName，在这里是硬编码
// 字符串，中间没有编译器守。两侧各钉一条断言，改任何一边都会红。
//
// 改坏的后果是静默的：后端收不到这个头就**忽略 cookie**，请求落到 401，
// 而前端会把它显示成「凭证无效，前往设置」——用户去重填一遍同样的 key。
it('SESSION_HEADER 与后端 internal/session.HeaderName 保持一致', () => {
  expect(SESSION_HEADER).toBe('X-WebTag-Session')
})
