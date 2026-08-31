/**
 * Reader domain fixtures, shaped like API DTOs. Unit tests should override
 * only the fields that matter for the behavior under test.
 */
import { ok, type ApiResult } from '@webtag/api'
import type {
  LinkResponse,
  PaginatedLinksResponse,
  PaginatedSitesResponse,
  ReaderFeedItemResponse,
  ReaderHomeResponse,
  ReaderInboxListItemResponse,
  ReaderInboxResponse,
  ReaderNoteHistoryResponse,
  ReaderNoteResponse,
  ReaderThoughtResponse,
  ReaderTodoResponse,
  SiteListItemResponse,
} from '../lib/api/types'

/** 构造一条 LinkResponse，按需覆盖字段。 */
export function makeLink(over: Partial<LinkResponse> = {}): LinkResponse {
  return {
    id: 'id-' + (over.id ?? Math.random().toString(36).slice(2)),
    url: 'https://example.com/a',
    title: '示例标题',
    summary: '这是一段 AI 摘要正文。',
    description: null,
    tags: ['LLM'],
    content_type: 'article',
    status: 'done',
    domain: 'example.com',
    path_depth: 1,
    parent_id: null,
    created_at: '2026-06-10T10:00:00Z',
    updated_at: '2026-06-10T10:05:00Z',
    fetcher_type: 'http',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    parent_path: null,
    metadata_revision: 1,
    ...over,
    // has_content 在后端是 `content IS NOT NULL` 的生成列（migrate/steps.go），
    // 「有 content 但 has_content:false」这个组合线上不可能出现。fixture 必须照这个
    // 来——否则用例会跑在一个现实中不存在的响应形态上，而渲染路径正是靠 has_content
    // 判断「服务端还有没有这份正文」的。显式传 has_content 时仍以调用方为准（要造
    // 「正文被服务端清空、本地还留着一份」那种时序，就得显式写）。
    has_content: over.has_content ?? Boolean(over.content),
  }
}

export function makeReadingLink(overrides: Partial<LinkResponse> = {}): LinkResponse {
  return makeLink({ library_kind: 'reading', ...overrides })
}

/**
 * Cursor-mode link page. A full page carries `next_cursor`; a short page ends
 * pagination. This mirrors the backend contract used by `useLinks`.
 */
export function makeLinksPage(
  items: LinkResponse[] = [makeLink()],
  limit = 30,
): ApiResult<PaginatedLinksResponse> {
  const data: PaginatedLinksResponse = { items, total: 0, page: 0, limit }
  if (items.length >= limit) data.next_cursor = 'cursor-' + items.length
  return ok(data)
}

export function makeLegacyLinksPage(
  items: LinkResponse[],
  total: number,
  limit = 30,
): ApiResult<PaginatedLinksResponse> {
  return ok({ items, total, page: 1, limit })
}

export function makeSite(
  index = 1,
  prefix = 'site',
  overrides: Partial<SiteListItemResponse> = {},
): SiteListItemResponse {
  const suffix = String(index).padStart(12, '0')
  return {
    id: `00000000-0000-4000-8000-${suffix}`,
    name: `${prefix} ${index}`,
    intro: '',
    display_host: `${prefix}-${index}.example.test`,
    homepage_url: `https://${prefix}-${index}.example.test`,
    icon_url: null,
    tags: [],
    entry_count: 1,
    pinned: false,
    primary_entry: null,
    revision: index,
    first_collected_at: '2026-08-01T00:00:00Z',
    last_collected_at: '2026-08-01T00:00:00Z',
    ...overrides,
  }
}

export function makeSitesPage(
  items: SiteListItemResponse[],
  total = items.length,
  pageNumber = 1,
  recentCutoff?: string,
): ApiResult<PaginatedSitesResponse> {
  return ok({
    items,
    total,
    page: pageNumber,
    limit: 30,
    ...(recentCutoff ? { recent_cutoff: recentCutoff } : {}),
  })
}

export function makeReaderNote(overrides: Partial<ReaderNoteResponse> = {}): ReaderNoteResponse {
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

export function makeReaderNoteWithID(
  id: string,
  overrides: Partial<ReaderNoteResponse> = {},
): ReaderNoteResponse {
  return makeReaderNote({ id, title: `笔记 ${id}`, ...overrides })
}

export function makeReaderNoteHistory(
  overrides: Partial<ReaderNoteHistoryResponse> = {},
): ReaderNoteHistoryResponse {
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

export function makeReaderInbox(overrides: Partial<ReaderInboxResponse> = {}): ReaderInboxResponse {
  return {
    id: 'inbox-1',
    url: 'https://example.com/article',
    source_kind: 'manual',
    title: '第一篇',
    body: '正文',
    note: '',
    summary: '摘要',
    suggested_tags: ['阅读'],
    proposal_status: 'completed',
    tags: ['阅读'],
    status: 'pending',
    metadata_revision: 1,
    expires_at: '2026-09-09T01:00:00Z',
    expired: false,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

// GET /api/inbox returns the narrow queue card; the editor reads detail on demand.
export function makeReaderInboxCard(item: ReaderInboxResponse): ReaderInboxListItemResponse {
  return {
    id: item.id,
    url: item.url,
    source_kind: item.source_kind,
    title: item.title,
    preview: ((item.summary ?? '').trim() || item.note.trim() || item.body).slice(0, 280),
    tags: item.tags,
    status: item.status,
    metadata_revision: item.metadata_revision,
    expired: item.expired,
    updated_at: item.updated_at,
  }
}

export function makeReaderThought(
  overrides: Partial<ReaderThoughtResponse> = {},
): ReaderThoughtResponse {
  const id = overrides.id ?? 'thought-1'
  return {
    contract_version: 1,
    id,
    host_kind: 'link',
    host_id: `link-${id}`,
    link_id: `link-${id}`,
    target: {},
    quote: null,
    body: `想法 ${id}`,
    source: 'self',
    deleted: false,
    last_sequence: 1,
    winner_key: overrides.winner_key ?? {
      logical_clock: 1,
      device_id: 'device-test',
      op_id: `op-${id}`,
    },
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    lifecycle_status: 'active',
    lifecycle_reason: null,
    tombstoned_at: null,
    ...overrides,
  }
}

export function makeReaderTodo(overrides: Partial<ReaderTodoResponse> = {}): ReaderTodoResponse {
  return {
    id: 'todo-1',
    text: '任务',
    due_at: null,
    done: false,
    origin_kind: 'standalone',
    origin_host_kind: null,
    origin_host_id: null,
    origin_ref: null,
    host_revision: 0,
    completed_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    expired: false,
    ...overrides,
  }
}

export function makeReaderFeedItem(
  overrides: Partial<ReaderFeedItemResponse> = {},
): ReaderFeedItemResponse {
  return {
    key: 'inbox:I1',
    source: 'inbox',
    resource_key: 'inbox:I1',
    title: '收件箱文章',
    summary: '需要整理',
    url: 'https://example.com/inbox',
    link_id: null,
    inbox_id: 'I1',
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    event_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

export type ReaderHomeFixtureOverrides =
  Partial<ReaderHomeResponse> & {
    readonly freshness?: unknown
    readonly partial?: unknown
  }

export function makeReaderHome(
  overrides: ReaderHomeFixtureOverrides = {},
): ReaderHomeResponse {
  return {
    today: '2026-08-10',
    summary: '今天继续整理',
    counts: {
      pending_count: 2,
      reading_count: 3,
      sites_count: 4,
      subscriptions_count: 5,
      notes_count: 6,
    },
    continue_reading: [makeReaderFeedItem()],
    recent_thoughts: [],
    todos: [makeReaderTodo()],
    stale: false,
    ...overrides,
  }
}

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}
