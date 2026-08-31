import { describe, expect, it } from 'vitest'
import type { components, paths } from './generated'

type IngestRequestBody = NonNullable<paths['/api/ingest']['post']['requestBody']>['content']['application/json']
type IngestResponseBody = paths['/api/ingest']['post']['responses'][202]['content']['application/json']
type InboxListResponseBody = paths['/api/inbox']['get']['responses'][200]['content']['application/json']
type InboxConfirmResponseBody = paths['/api/inbox/{id}/confirm']['post']['responses'][200]['content']['application/json']
type InboxCard = InboxListResponseBody['items'][number]
/**
 * A capture accepts a 4 MiB body and a 1 MiB note, and the Inbox list is read
 * on every open. The queue page type must not admit either or the detail-only
 * category memberships: this alias stops compiling the moment the projection
 * widens back.
 */
type DetailOnlyFieldsOnCard = Extract<
  keyof InboxCard,
  'body' | 'note' | 'summary' | 'suggested_tags'
>
export const inboxCardCarriesNoDetailPayload: [DetailOnlyFieldsOnCard] extends [never] ? true : false = true
type ThoughtOpsRequestBody = NonNullable<paths['/api/annotations/ops']['post']['requestBody']>['content']['application/json']
type ThoughtOpsResponseBody = paths['/api/annotations/ops']['post']['responses'][200]['content']['application/json']
type ThoughtSyncResponseBody = paths['/api/annotations/sync']['get']['responses'][200]['content']['application/json']
type SearchResponseBody = paths['/api/search']['get']['responses'][200]['content']['application/json']
type NoteCreateRequestBody = NonNullable<paths['/api/notes']['post']['requestBody']>['content']['application/json']
type NoteCreateResponseBody = paths['/api/notes']['post']['responses'][201]['content']['application/json']
type NoteDraftRequestBody = NonNullable<paths['/api/notes/{id}/draft']['patch']['requestBody']>['content']['application/json']
type NoteDraftResponseBody = paths['/api/notes/{id}/draft']['patch']['responses'][200]['content']['application/json']
type NotePublishRequestBody = NonNullable<paths['/api/notes/{id}/publish']['post']['requestBody']>['content']['application/json']
type NotePublishResponseBody = paths['/api/notes/{id}/publish']['post']['responses'][200]['content']['application/json']
type TodoListResponseBody = paths['/api/todos']['get']['responses'][200]['content']['application/json']
type TodoPatchRequestBody = NonNullable<paths['/api/todos/{id}']['patch']['requestBody']>['content']['application/json']
type TodoPatchResponseBody = paths['/api/todos/{id}']['patch']['responses'][200]['content']['application/json']
type HomeResponseBody = paths['/api/home']['get']['responses'][200]['content']['application/json']
type FeedResponseBody = paths['/api/reader-feed']['get']['responses'][200]['content']['application/json']
type FeedFeedbackRequestBody = NonNullable<paths['/api/reader-feed/feedback']['post']['requestBody']>['content']['application/json']
type FeedFeedbackResponseBody = paths['/api/reader-feed/feedback']['post']['responses'][200]['content']['application/json']
type EngagementRequestBody = NonNullable<paths['/api/engagement/{link_id}']['patch']['requestBody']>['content']['application/json']
type EngagementResponseBody = paths['/api/engagement/{link_id}']['patch']['responses'][200]['content']['application/json']

const linkID = '00000000-0000-0000-0000-000000000001'
const inboxID = '00000000-0000-0000-0000-000000000002'
const noteID = '00000000-0000-0000-0000-000000000003'
const todoID = '00000000-0000-0000-0000-000000000004'
const thoughtOpID = '00000000-0000-0000-0000-000000000006'
const now = '2026-08-10T08:00:00Z'

const thought = {
  contract_version: 1,
  id: '00000000-0000-0000-0000-000000000005',
  host_kind: 'link',
  host_id: linkID,
  link_id: linkID,
  target: { kind: 'saved-content', host_id: linkID, version: { content_revision: 3 } },
  quote: { exact: 'Captured body', start: 0, end: 13 },
  body: 'A synced idea becomes a searchable note.',
  source: 'user',
  deleted: false,
  last_sequence: 2,
  winner_key: {
    logical_clock: 2,
    device_id: 'device-reader-1',
    op_id: thoughtOpID,
  },
  created_at: now,
  updated_at: now,
} satisfies components['schemas']['ReaderThoughtResponse']

const note = {
  id: noteID,
  title: 'Captured idea note',
  published_content: 'Published note content.',
  published_revision: 2,
  draft_content: null,
  draft_revision: 2,
  draft_updated_at: null,
  deleted_at: null,
  created_at: now,
  updated_at: now,
  dirty: false,
} satisfies components['schemas']['ReaderNoteResponse']

const todo = {
  id: todoID,
  text: 'Follow up on the captured idea',
  due_at: null,
  done: false,
  origin_kind: 'note',
  origin_host_kind: 'note',
  origin_host_id: noteID,
  origin_ref: { block_ref: 'task:follow-up' },
  host_revision: 2,
  completed_at: null,
  created_at: now,
  updated_at: now,
  expired: false,
  source_href: `/?view=notes&note_id=${noteID}`,
} satisfies components['schemas']['ReaderTodoResponse']

const feedItem = {
  key: `link:${linkID}`,
  source: 'reading',
  resource_key: `link:${linkID}`,
  title: 'Captured article',
  summary: 'Captured summary',
  url: 'https://capture.example.test/article',
  link_id: linkID,
  inbox_id: null,
  feed_item_id: null,
  read: false,
  read_later: false,
  saved: false,
  event_at: now,
} satisfies components['schemas']['ReaderFeedItemResponse']

export const readerCrossSurfaceContractExamples = {
  ingestRequest: {
    destination: 'inbox',
    sources: [{
      kind: 'browser_capture',
      url: 'https://capture.example.test/article',
      title: 'Captured article',
      text: 'Captured body from the browser extension.',
    }],
  } satisfies components['schemas']['IngestRequest'] & IngestRequestBody,
  ingestResponse: {
    inbox_id: inboxID,
    destination: 'inbox',
    status: 'pending',
  } satisfies components['schemas']['SubmitResponse'] & IngestResponseBody,
  // The queue page carries cards only. The capture body, the user note and the
  // AI proposal payload are reachable through GET /api/inbox/{id} alone, which
  // is what keeps a 4 MiB capture out of every Inbox open.
  inboxPage: {
    items: [{
      id: inboxID,
      url: 'https://capture.example.test/article',
      source_kind: 'browser_capture',
      title: 'Captured article',
      preview: 'Captured summary',
      tags: ['capture'],
      status: 'pending',
      metadata_revision: 1,
      expired: false,
      updated_at: now,
    }],
    active_count: 1,
    expired_count: 0,
  } satisfies components['schemas']['ReaderInboxResponsePage'] & InboxListResponseBody,
  inboxConfirmResponse: {
    target_kind: 'link',
    link_id: linkID,
    status: 'confirmed',
  } satisfies components['schemas']['ReaderConfirmResponse'] & InboxConfirmResponseBody,
  thoughtOpsRequest: {
    ops: [{
      contract_version: 1,
      op_id: thoughtOpID,
      device_id: 'device-reader-1',
      logical_clock: 2,
      operation_kind: 'add',
      annotation_id: thought.id,
      host_kind: 'link',
      host_id: linkID,
      target: thought.target,
      payload: { body: thought.body, quote: thought.quote, source: thought.source },
    }],
  } satisfies components['schemas']['ReaderThoughtOpsRequest'] & ThoughtOpsRequestBody,
  thoughtOpsResponse: {
    items: [{
      contract_version: 1,
      op_id: thoughtOpID,
      sequence: 2,
      disposition: 'applied',
      submitted_key: thought.winner_key,
      current_winner_key: thought.winner_key,
    }],
  } satisfies ThoughtOpsResponseBody,
  thoughtSyncResponse: {
    contract_version: 1,
    items: [thought],
    next_cursor: 'cursor-2',
  } satisfies ThoughtSyncResponseBody,
  groupedSearchResponse: {
    reading: { total_hint: 1, items: [] },
    sites: { total_hint: 0, items: [] },
    thoughts: {
      total_hint: 1,
      next_cursor: 'opaque-thought-search-cursor',
      items: [{
        id: thought.id,
        host_kind: thought.host_kind,
        host_id: thought.host_id,
        link_id: thought.link_id,
        snippet: thought.body,
        updated_at: now,
      }],
    },
    notes: {
      total_hint: 1,
      items: [{ id: note.id, title: note.title, snippet: note.published_content, published_revision: note.published_revision, updated_at: now }],
    },
  } satisfies components['schemas']['GroupedSearchResponse'] & SearchResponseBody,
  noteCreateRequest: { title: note.title, content: '' } satisfies components['schemas']['ReaderNoteCreateRequest'] & NoteCreateRequestBody,
  noteCreateResponse: { ...note, published_content: '', published_revision: 1, draft_revision: 1, dirty: false } satisfies NoteCreateResponseBody,
  noteDraftRequest: {
    content: 'Published note with a reanchored quote.\n\n- [ ] Follow up on the captured idea',
    expected_draft_revision: 1,
  } satisfies components['schemas']['ReaderNoteDraftRequest'] & NoteDraftRequestBody,
  noteDraftResponse: { ...note, draft_content: 'Draft content', draft_revision: 2, dirty: true } satisfies NoteDraftResponseBody,
  notePublishRequest: {
    expected_draft_revision: 2,
    expected_published_revision: 1,
    reanchor_ops: [{ thought_id: thought.id, status: 'reanchored', reason: 'unique-quote' }],
  } satisfies components['schemas']['ReaderNotePublishRequest'] & NotePublishRequestBody,
  notePublishResponse: note satisfies NotePublishResponseBody,
  todoListResponse: { items: [todo], next_after: 'opaque-page-cursor' } satisfies components['schemas']['ReaderTodosResponse'] & TodoListResponseBody,
  todoStandalonePatchRequest: { done: true } satisfies components['schemas']['ReaderTodoPatchRequest'] & TodoPatchRequestBody,
  todoPatchRequest: { done: true, expected_host_revision: 2 } satisfies components['schemas']['ReaderTodoPatchRequest'] & TodoPatchRequestBody,
  todoPatchResponse: { ...todo, done: true, completed_at: now } satisfies TodoPatchResponseBody,
  homeResponse: {
    today: '2026-08-10',
    summary: '继续整理捕获内容',
    counts: { pending: 0, reading: 1, sites: 0, subs: 0, notes: 1 },
    continue_reading: [feedItem],
    recent_thoughts: [thought],
    todos: [todo],
    freshness: 'fresh',
    partial: false,
    stale: false,
  } satisfies components['schemas']['ReaderHomeResponse'] & HomeResponseBody,
  feedResponse: {
    items: [feedItem],
    next_cursor: 'live-cursor-1',
    mode: 'recommended',
  } satisfies components['schemas']['ReaderFeedResponse'] & FeedResponseBody,
  feedFeedbackRequest: { action: 'save' } satisfies components['schemas']['ReaderFeedFeedbackRequest'] & FeedFeedbackRequestBody,
  feedFeedbackResponse: {
    item_key: `subscription:${inboxID}`,
    action: 'save',
    link_id: linkID,
  } satisfies components['schemas']['ReaderFeedFeedbackResponse'] & FeedFeedbackResponseBody,
  engagementRequest: { read: true, read_later: true, progress: 0.5 } satisfies components['schemas']['ReaderEngagementRequest'] & EngagementRequestBody,
  engagementResponse: { link_id: linkID, read: true, read_later: true, progress: 0.5, last_opened: now, updated_at: now } satisfies components['schemas']['ReaderEngagementResponse'] & EngagementResponseBody,
} as const

export const readerCrossSurfaceNoContentResponses = {
  inboxRestore: 200,
  feedFeedback: 200,
} satisfies {
  inboxRestore: keyof paths['/api/inbox/{id}/restore']['post']['responses']
  feedFeedback: keyof paths['/api/reader-feed/feedback']['post']['responses']
}

function assertRecord(value: unknown, label: string): asserts value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(`${label} must be an object`)
}

function assertString(value: unknown, label: string): asserts value is string {
  if (typeof value !== 'string' || value.length === 0) throw new Error(`${label} must be a non-empty string`)
}

function assertNumber(value: unknown, label: string): asserts value is number {
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${label} must be a number`)
}

function assertBoolean(value: unknown, label: string): asserts value is boolean {
  if (typeof value !== 'boolean') throw new Error(`${label} must be a boolean`)
}

function assertArray(value: unknown, label: string): asserts value is unknown[] {
  if (!Array.isArray(value)) throw new Error(`${label} must be an array`)
}

/** Runtime checks keep the executable contract useful when this module is loaded by a test runner. */
export function assertReaderCrossSurfaceContract(): void {
  const examples = readerCrossSurfaceContractExamples
  assertRecord(examples.ingestResponse, 'ingestResponse')
  assertString(examples.ingestResponse.inbox_id, 'ingestResponse.inbox_id')
  if (examples.ingestResponse.destination !== 'inbox' || examples.ingestResponse.status !== 'pending') throw new Error('ingestResponse must target a pending inbox')

  assertArray(examples.inboxPage.items, 'inboxPage.items')
  assertRecord(examples.inboxPage.items[0], 'inboxPage.items[0]')
  assertString(examples.inboxPage.items[0].id, 'inboxPage.items[0].id')
  assertString(examples.inboxPage.items[0].preview, 'inboxPage.items[0].preview')
  if (examples.inboxPage.items[0].status !== 'pending') throw new Error('inbox item must start pending')
  for (const detailOnly of ['body', 'note', 'suggested_tags']) {
    if (detailOnly in examples.inboxPage.items[0]) {
      throw new Error(`inbox list card must not carry ${detailOnly}; it belongs to GET /api/inbox/{id}`)
    }
  }
  if (!inboxCardCarriesNoDetailPayload) throw new Error('inbox list card type widened back to the detail payload')
  if (examples.inboxConfirmResponse.target_kind !== 'link' || examples.inboxConfirmResponse.status !== 'confirmed') throw new Error('confirm must produce a link')

  assertArray(examples.thoughtOpsRequest.ops, 'thoughtOpsRequest.ops')
  assertArray(examples.thoughtOpsResponse.items, 'thoughtOpsResponse.items')
  assertNumber(examples.thoughtOpsResponse.items[0]?.sequence, 'thoughtOpsResponse.items[0].sequence')
  assertArray(examples.thoughtSyncResponse.items, 'thoughtSyncResponse.items')
  assertString(examples.groupedSearchResponse.thoughts?.items[0]?.snippet, 'groupedSearchResponse.thoughts.items[0].snippet')
  assertString(examples.groupedSearchResponse.thoughts?.next_cursor, 'groupedSearchResponse.thoughts.next_cursor')
  assertString(examples.groupedSearchResponse.notes?.items[0]?.title, 'groupedSearchResponse.notes.items[0].title')

  assertString(examples.noteDraftResponse.draft_content, 'noteDraftResponse.draft_content')
  assertBoolean(examples.noteDraftResponse.dirty, 'noteDraftResponse.dirty')
  assertBoolean(examples.notePublishResponse.dirty, 'notePublishResponse.dirty')
  if (examples.notePublishResponse.draft_content !== null) throw new Error('published note must clear its draft')

  assertArray(examples.todoListResponse.items, 'todoListResponse.items')
  if ('expected_host_revision' in examples.todoStandalonePatchRequest) throw new Error('standalone TODO patches must omit expected_host_revision')
  assertNumber(examples.todoPatchRequest.expected_host_revision, 'todoPatchRequest.expected_host_revision')
  assertBoolean(examples.todoPatchResponse.done, 'todoPatchResponse.done')
  assertString(examples.homeResponse.continue_reading[0]?.key, 'homeResponse.continue_reading[0].key')
  assertString(examples.feedResponse.next_cursor, 'feedResponse.next_cursor')
  if (examples.feedFeedbackRequest.action !== 'save') throw new Error('feed feedback must preserve its action')
  assertString(examples.feedFeedbackResponse.item_key, 'feedFeedbackResponse.item_key')
  assertString(examples.feedFeedbackResponse.link_id, 'feedFeedbackResponse.link_id')
  assertBoolean(examples.engagementResponse.read, 'engagementResponse.read')

}

describe('reader cross-surface API contract', () => {
  it('keeps the shared reader wire examples executable', () => {
    expect(() => assertReaderCrossSurfaceContract()).not.toThrow()
  })
})
