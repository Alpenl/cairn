import { describe, expect, it } from 'vitest'
import {
  isDomainTreeSummaryEnvelope,
  isErrorResponse,
  isFeedItem,
  isFeedSubscription,
  isPaginatedFeedItems,
  isLinkResponse,
  isPaginatedLinks,
  isSubmitResponse,
  isTagCountArray,
  isGroupedSearchResponse,
  isCapabilitiesResponse,
  isReaderInboxConfirmAIProposalsResponse,
  isReaderInboxBulkResponse,
  isReaderInboxResponse,
  isReaderInboxResponsePage,
  isReaderFeedFeedbackResponse,
  isReaderFeedResponse,
  isReaderHomeResponse,
  isReaderLinkMetadataResponse,
  hasCanonicalSafeLinkMetadataRevisionTokens,
  isReaderContentHistoryResponse,
  isReaderContentHistoryRestoreResponse,
  isReaderActivityResponse,
  isReaderNoteHistoryResponse,
  isReaderThoughtAckResponse,
  isReaderThoughtResponse,
  isReaderThoughtSupersessionEventsResponse,
  isReaderThoughtsResponse,
  isReaderTodoResponse,
  isReaderTodosResponse,
  normalizeCapabilitiesResponse,
} from './guards'
import { makeLink } from '../../test/fixtures'
import type { ReaderFeedFeedbackResponse, ReaderFeedItemResponse, ReaderHomeResponse, ReaderTodoResponse } from './types'

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

function makeReaderFeedItem(overrides: Partial<ReaderFeedItemResponse> = {}): ReaderFeedItemResponse {
  return {
    key: 'subscription:feed-1',
    source: 'subscription',
    title: 'Feed item',
    summary: 'Summary',
    url: 'https://example.com/article',
    link_id: 'link-1',
    inbox_id: null,
    feed_item_id: 'feed-1',
    read: false,
    read_later: false,
    saved: false,
    score: 60,
    score_contributions: {
      pending_confirmation: 0,
      saved_library: 0,
      subscription_recent: 40,
      unread: 20,
      read_later: 0,
      chronological_fallback: 0,
    },
    enabled_score_signals: ['subscription_recent', 'unread', 'read_later', 'chronological_fallback'],
    reason_code: 'subscription_recent',
    reason_params: { source: 'subscription' },
    reason_contribution: 40,
    reason_text: '订阅更新',
    published_at: null,
    event_at: '2026-08-10T01:00:00Z',
    created_at: '2026-08-10T01:00:00Z',
    item_type: 'subscription',
    resource_key: 'link:link-1',
    action_key: 'subscription:feed-1',
    dedupe_key: 'url:https://example.com/article',
    section_id: 'subscription',
    actions: ['read', 'read_later', 'save', 'unsave', 'open'],
    ...overrides,
  } as ReaderFeedItemResponse
}

describe('isErrorResponse', () => {
  it('合法错误体', () => {
    expect(isErrorResponse({ error: { code: 404, message: 'nope' } })).toBe(true)
  })
  it('缺 error / 类型不符', () => {
    expect(isErrorResponse({})).toBe(false)
    expect(isErrorResponse({ error: { code: '404', message: 'x' } })).toBe(false)
  })
})

describe('isLinkResponse', () => {
  it('必需字段齐全', () => {
    expect(isLinkResponse(makeLink())).toBe(true)
  })

  it('任一 OpenAPI 必填字段缺失都 fail closed', () => {
    const complete = makeLink()
    for (const field of Object.keys(complete)) {
      const candidate = { ...complete } as Record<string, unknown>
      delete candidate[field]
      expect(isLinkResponse(candidate), field).toBe(false)
    }
  })

  it('兼容省略可选 library_kind，但拒绝 wire enum 之外的值', () => {
    expect(isLinkResponse(makeLink())).toBe(true)
    expect(isLinkResponse({ ...makeLink(), library_kind: 'archive' })).toBe(false)
  })

  it('requires a positive JavaScript-safe metadata revision that can form a CAS token', () => {
    expect(isLinkResponse(makeLink({ metadata_revision: 1 }))).toBe(true)
    expect(isLinkResponse(makeLink({ metadata_revision: Number.MAX_SAFE_INTEGER }))).toBe(true)
    expect(isLinkResponse(makeLink({ metadata_revision: 0 }))).toBe(false)
    expect(isLinkResponse(makeLink({ metadata_revision: -1 }))).toBe(false)
    expect(isLinkResponse(makeLink({ metadata_revision: 1.5 }))).toBe(false)
    const unsafe = JSON.parse(
      '{"link_id":"link-1","metadata_revision":9007199254740993}',
    )
    expect(isLinkResponse({ ...makeLink(), ...unsafe })).toBe(false)
  })
})

describe('isSubmitResponse', () => {
  it('拒绝 wire enum 之外的状态', () => {
    expect(isSubmitResponse({ link_id: '1', status: 'queued' })).toBe(false)
  })
})

describe('Reader activity response guard', () => {
  it('accepts a typed page and optional continuation cursor', () => {
    expect(isReaderActivityResponse({
      kind: 'tag',
      tags: [{ tag: 'alpha', last_at: '2026-08-11T00:00:00Z' }],
      domains: [],
      next_cursor: 'opaque-cursor',
    })).toBe(true)
  })

  it('rejects legacy, cross-kind, and malformed cursor shapes', () => {
    expect(isReaderActivityResponse({ tags: [], domains: [] })).toBe(false)
    expect(isReaderActivityResponse({ kind: 'site', tags: [], domains: [] })).toBe(false)
    expect(isReaderActivityResponse({ kind: 'all', tags: [], domains: [], next_cursor: 7 })).toBe(false)
  })
})

describe('Reader link metadata response guard', () => {
  it('requires a positive JavaScript-safe metadata revision that can become the next If-Match token', () => {
    expect(isReaderLinkMetadataResponse({ link_id: 'link-1', metadata_revision: 1 })).toBe(true)
    expect(isReaderLinkMetadataResponse({ link_id: 'link-1', metadata_revision: Number.MAX_SAFE_INTEGER })).toBe(true)
    expect(isReaderLinkMetadataResponse({ link_id: 'link-1', metadata_revision: 0 })).toBe(false)
    expect(isReaderLinkMetadataResponse({ link_id: 'link-1', metadata_revision: -1 })).toBe(false)
    expect(isReaderLinkMetadataResponse({ link_id: 'link-1', metadata_revision: 1.5 })).toBe(false)
    const unsafe = JSON.parse(
      '{"link_id":"link-1","metadata_revision":9007199254740993}',
    )
    expect(isReaderLinkMetadataResponse(unsafe)).toBe(false)
  })
})

describe('raw Link metadata revision tokens', () => {
  it('rejects fractional values before JSON.parse can round them', () => {
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(
      '{"metadata_revision":9007199254740991.1}',
    )).toBe(false)
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(
      '{"metadata_revision":9007199254740990.9}',
    )).toBe(false)
  })

  it('accepts the canonical JavaScript-safe maximum and escaped property names', () => {
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(
      '{"metadata_revision":9007199254740991}',
    )).toBe(true)
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(
      '{"metadata\\u005frevision":9007199254740991}',
    )).toBe(true)
  })

  it.each([
    ['empty array', '[]'],
    ['empty object', '{}'],
    ['nested containers', '{"items":[{"metadata_revision":7}]}'],
  ])('accepts bounded scanner case %s', (_name, raw) => {
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(raw)).toBe(true)
  })

  it.each([
    ['missing comma', '{"a":1 "metadata_revision":2}'],
    ['missing colon', '{"metadata_revision" 2}'],
    ['unclosed array', '[{"metadata_revision":2}'],
    ['unclosed object', '{"metadata_revision":2'],
  ])('rejects malformed scanner case %s', (_name, raw) => {
    expect(hasCanonicalSafeLinkMetadataRevisionTokens(raw)).toBe(false)
  })
})

describe('Reader TODO response guards', () => {
  it('接受 due_at/completed_at 的日期字符串和显式 null', () => {
    expect(isReaderTodoResponse(makeReaderTodo())).toBe(true)
    expect(isReaderTodoResponse(makeReaderTodo({
      due_at: null,
      completed_at: null,
      done: false,
      expired: true,
    }))).toBe(true)
  })

  it('兼容省略 optional due_at/completed_at 的 wire 形状', () => {
    const value: Record<string, unknown> = { ...makeReaderTodo() }
    delete value.due_at
    delete value.completed_at

    expect(isReaderTodoResponse(value)).toBe(true)
    expect(isReaderTodosResponse({ items: [value] })).toBe(true)
  })

  it('接受同源相对 source_href 和可选 next_after', () => {
    expect(isReaderTodoResponse(makeReaderTodo({ source_href: '/?view=notes&note_id=one' }))).toBe(true)
    expect(isReaderTodosResponse({
      items: [makeReaderTodo({ source_href: '/?tool=history&thought_view=live' })],
      next_after: 'opaque-cursor',
    })).toBe(true)
  })

  it('兼容 legacy TODO 列表，并接受所有合法 freshness/partial 组合', () => {
    expect(isReaderTodosResponse({ items: [makeReaderTodo()] })).toBe(true)
    for (const metadata of [
      { freshness: 'unknown', partial: false },
      { freshness: 'fresh', partial: false },
      { freshness: 'partial', partial: true },
      { freshness: 'stale', partial: false },
    ]) {
      expect(isReaderTodosResponse({ items: [makeReaderTodo()], ...metadata })).toBe(true)
    }
  })

  it.each([
    ['freshness literal', { freshness: 'invalid' }],
    ['freshness number', { freshness: 1 }],
    ['partial string', { partial: 'false' }],
    ['partial null', { partial: null }],
  ])('TODO response rejects invalid %s metadata', (_name, metadata) => {
    expect(isReaderTodosResponse({ items: [makeReaderTodo()], ...metadata })).toBe(false)
  })

  it('接受 origin_ref 的 null 和 JSON object wire 值', () => {
    expect(isReaderTodoResponse(makeReaderTodo({ origin_ref: null }))).toBe(true)
    expect(isReaderTodoResponse(makeReaderTodo({ origin_ref: { block_ref: 'task:1' } }))).toBe(true)
  })

  it.each([
    ['due_at', { due_at: 7 }],
    ['completed_at', { completed_at: {} }],
    ['done', { done: 'true' }],
    ['host_revision', { host_revision: -1 }],
    ['source_href', { source_href: 'https://other.example/todo' }],
    ['source_href protocol-relative', { source_href: '//other.example/todo' }],
  ])('对错误的 %s 类型失败关闭', (field, overrides) => {
    expect(isReaderTodoResponse({ ...makeReaderTodo(), ...overrides }), field).toBe(false)
  })

  it('拒绝错误的 next_after 类型', () => {
    expect(isReaderTodosResponse({ items: [makeReaderTodo()], next_after: 7 })).toBe(false)
  })
})

describe('Reader Feed response guard', () => {
	it('accepts only feedback with a complete stable save association', () => {
		const response: ReaderFeedFeedbackResponse = {
			item_key: 'subscription:feed-1',
			action: 'save',
			saved: true,
			association: { feed_item_id: 'feed-1', link_id: 'link-1', created_link: true },
		}
		expect(isReaderFeedFeedbackResponse(response)).toBe(true)
		expect(isReaderFeedFeedbackResponse({ ...response, association: { feed_item_id: 'feed-1' } })).toBe(false)
	})

  it('接受显式 union identity、actions、section/source metadata', () => {
    const item = makeReaderFeedItem()
    expect(isReaderFeedResponse({
      items: [item],
      snapshot_id: 'snapshot-1',
      mode: 'recommended',
      capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions'],
      sections: [{
        id: 'subscription',
        source: 'subscription',
        label: '订阅',
        count: 1,
        capabilities: item.actions,
      }],
      sources: [{
        id: 'subscription',
        label: '订阅',
        enabled: true,
        count: 1,
        capabilities: item.actions,
      }],
    })).toBe(true)
  })

  it('接受 capability-off 和 legacy item wire', () => {
    const legacy = makeReaderFeedItem()
    delete (legacy as Record<string, unknown>).item_type
    delete (legacy as Record<string, unknown>).resource_key
    delete (legacy as Record<string, unknown>).action_key
    delete (legacy as Record<string, unknown>).dedupe_key
    delete (legacy as Record<string, unknown>).section_id
    delete (legacy as Record<string, unknown>).actions
    expect(isReaderFeedResponse({
      items: [legacy],
      snapshot_id: 'snapshot-legacy',
      mode: 'chronological',
      capabilities: [],
      sections: [],
      sources: [],
    })).toBe(true)
  })

  it('拒绝未知的 item actions', () => {
    const invalidItem = { ...makeReaderFeedItem(), actions: ['unsupported'] }
    expect(isReaderFeedResponse({
      items: [invalidItem],
      snapshot_id: 'snapshot-invalid',
      mode: 'recommended',
    })).toBe(false)
  })

  it('缺少权威 event_at 时失败关闭', () => {
    const item = makeReaderFeedItem()
    const candidate = { ...item } as Record<string, unknown>
    delete candidate.event_at
    expect(isReaderFeedResponse({
      items: [candidate],
      snapshot_id: 'snapshot-missing-event-at',
      mode: 'chronological',
    })).toBe(false)
  })

  it('拒绝 item_type 与 source 不一致', () => {
    expect(isReaderFeedResponse({
      items: [makeReaderFeedItem({ item_type: 'inbox' })],
      snapshot_id: 'snapshot-invalid',
      mode: 'recommended',
    })).toBe(false)
  })

  it('拒绝顶层未知的 Feed capability', () => {
    expect(isReaderFeedResponse({
      items: [makeReaderFeedItem()],
      snapshot_id: 'snapshot-invalid',
      mode: 'recommended',
      capabilities: ['unsupported'],
    })).toBe(false)
  })

  it('拒绝重复的 action/capability 集合值', () => {
    const item = makeReaderFeedItem()
    expect(isReaderFeedResponse({
      items: [{ ...item, actions: ['read', 'read'] }],
      snapshot_id: 'snapshot-duplicate-actions',
      mode: 'recommended',
    })).toBe(false)
    expect(isReaderFeedResponse({
      items: [item],
      snapshot_id: 'snapshot-duplicate-capabilities',
      mode: 'recommended',
      capabilities: ['snapshot', 'snapshot'],
    })).toBe(false)
  })

  it('拒绝与冻结 score evidence 不一致的 reason tuple', () => {
    const item = makeReaderFeedItem()
    for (const invalid of [
      { ...item, score: 61 },
      { ...item, reason_contribution: 20 },
      { ...item, enabled_score_signals: ['unread'] },
      { ...item, reason_params: { source: 'reading', inferred_from: 'subscription' } },
    ]) {
      expect(isReaderFeedResponse({
        items: [invalid],
        snapshot_id: 'snapshot-invalid-evidence',
        mode: 'recommended',
      })).toBe(false)
    }
  })
})

describe('Reader Home response guard', () => {
  it.each([
    { name: 'legacy without metadata', value: makeReaderHome() },
    { name: 'unknown freshness', value: { ...makeReaderHome(), freshness: 'unknown', partial: false } },
    { name: 'fresh freshness', value: { ...makeReaderHome(), freshness: 'fresh', partial: false } },
    { name: 'partial freshness', value: { ...makeReaderHome(), freshness: 'partial', partial: true } },
    { name: 'stale freshness', value: { ...makeReaderHome({ stale: true }), freshness: 'stale', partial: false } },
  ])('accepts $name Home wire shape', ({ value }) => {
    expect(isReaderHomeResponse(value)).toBe(true)
  })

  it.each([
    ['freshness literal', { freshness: 'invalid' }],
    ['freshness number', { freshness: 1 }],
    ['partial string', { partial: 'false' }],
    ['partial null', { partial: null }],
  ])('Home response rejects invalid %s metadata', (_name, metadata) => {
    expect(isReaderHomeResponse({ ...makeReaderHome(), ...metadata })).toBe(false)
  })

  it('partial aggregate 缺少任一权威区块时失败关闭', () => {
    for (const field of ['counts', 'continue_reading', 'recent_thoughts', 'todos', 'stale']) {
      const value: Record<string, unknown> = { ...makeReaderHome() }
      delete value[field]
      expect(isReaderHomeResponse(value), field).toBe(false)
    }
  })

  it('要求 stale 是 boolean，即使 wire 带 freshness/partial 兼容字段', () => {
    expect(isReaderHomeResponse({
      ...makeReaderHome(),
      stale: 'false',
      freshness: 'fresh',
      partial: false,
    })).toBe(false)
  })
})

describe('Reader history and lifecycle response guards', () => {
  const thought = {
    contract_version: 1,
    id: 'thought-1',
    host_kind: 'link',
    host_id: 'link-1',
    link_id: 'link-1',
    target: {},
    quote: {},
    body: 'durable thought',
    source: 'reader',
    deleted: false,
    last_sequence: 4,
    winner_key: { logical_clock: 4, device_id: 'device-a', op_id: 'op-a' },
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T02:00:00Z',
  }

  it('accepts active and tombstone lifecycle metadata while keeping reason nullable', () => {
    expect(isReaderThoughtResponse({
      ...thought,
      lifecycle_status: 'active',
      lifecycle_reason: null,
      tombstoned_at: null,
    })).toBe(true)
    expect(isReaderThoughtsResponse({
      contract_version: 1,
      items: [{
        ...thought,
        lifecycle_status: 'tombstone',
        lifecycle_reason: 'content_restored',
        tombstoned_at: '2026-08-10T03:00:00Z',
      }],
    })).toBe(true)
  })

  it('fails closed for unknown lifecycle status or malformed lifecycle fields', () => {
    expect(isReaderThoughtResponse({ ...thought, lifecycle_status: 'deleted' })).toBe(false)
    expect(isReaderThoughtResponse({ ...thought, lifecycle_reason: 7 })).toBe(false)
    expect(isReaderThoughtResponse({ ...thought, tombstoned_at: 7 })).toBe(false)
    expect(isReaderThoughtResponse({ ...thought, contract_version: 2 })).toBe(false)
    expect(isReaderThoughtResponse({
      ...thought,
      winner_key: { ...thought.winner_key, logical_clock: Number.MAX_SAFE_INTEGER + 1 },
    })).toBe(false)
    expect(isReaderThoughtsResponse({ items: [thought] })).toBe(false)
  })

  it('requires complete safe winner keys and canonical identifiers', () => {
    const validAck = {
      contract_version: 1,
      op_id: 'op-a',
      sequence: 4,
      disposition: 'applied',
      submitted_key: thought.winner_key,
      current_winner_key: thought.winner_key,
    }
    expect(isReaderThoughtAckResponse(validAck)).toBe(true)
    expect(isReaderThoughtAckResponse({
      ...validAck,
      submitted_key: { logical_clock: 0, device_id: 'legacy-device', op_id: 'legacy-op' },
      current_winner_key: { logical_clock: 0, device_id: 'legacy-device', op_id: 'legacy-op' },
    })).toBe(true)

    for (const logicalClock of [-1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
      expect(isReaderThoughtAckResponse({
        ...validAck,
        current_winner_key: { ...thought.winner_key, logical_clock: logicalClock },
      })).toBe(false)
      expect(isReaderThoughtResponse({
        ...thought,
        winner_key: { ...thought.winner_key, logical_clock: logicalClock },
      })).toBe(false)
    }
    for (const deviceId of ['a'.repeat(129), '\ud800', ' device-a']) {
      expect(isReaderThoughtAckResponse({
        ...validAck,
        current_winner_key: { ...thought.winner_key, device_id: deviceId },
      })).toBe(false)
    }
    const missingWinner = { ...validAck } as Record<string, unknown>
    delete missingWinner.current_winner_key
    expect(isReaderThoughtAckResponse(missingWinner)).toBe(false)
  })

  it('validates note/content history shapes and requires a positive restored content revision', () => {
    expect(isReaderNoteHistoryResponse({
      id: 1,
      revision: 2,
      title: 'Note',
      content: 'body',
		reanchor_ops: [],
      created_at: '2026-08-10T01:00:00Z',
    })).toBe(true)
    expect(isReaderContentHistoryResponse({
      id: 1,
      revision: 2,
      content: 'body',
      content_document: null,
      content_format: 'plain',
      content_source: 'user',
      created_at: '2026-08-10T01:00:00Z',
    })).toBe(true)
    expect(isReaderContentHistoryRestoreResponse({ link_id: 'link-1', content_revision: 3 })).toBe(true)
    expect(isReaderContentHistoryRestoreResponse({ link_id: 'link-1', content_revision: 0 })).toBe(false)
    expect(isReaderContentHistoryRestoreResponse({ link_id: 'link-1', content_revision: -1 })).toBe(false)
  })
})

describe('Reader thought supersession event guard', () => {
  const operation = (opID: string, logicalClock: number, body: string) => ({
    contract_version: 1 as const,
    sequence: logicalClock + 10,
    op_id: opID,
    device_id: 'device-a',
    logical_clock: logicalClock,
    operation_kind: 'update' as const,
    annotation_id: 'annotation-1',
    host_kind: 'link' as const,
    host_id: 'link-1',
    target: {
      kind: 'saved-content' as const,
      host_id: 'link-1',
      version: { content_revision: 3 },
    },
    payload: {
      body,
      source: 'self' as const,
      quote: { exact: 'quote', start: 0, end: 5 },
    },
    created_at: '2026-08-11T00:00:00.000Z',
  })
  const response = {
    contract_version: 1 as const,
    items: [{
      sequence: 7,
      annotation_id: 'annotation-1',
      loser: operation('loser-op', 4, 'loser'),
      winner_at_detection: operation('winner-op', 5, 'winner'),
    }],
    // Raw base64url for the server's `1|event` event-stream cursor.
    next_cursor: 'MXxldmVudA',
  }

  it('accepts a complete immutable event page', () => {
    expect(isReaderThoughtSupersessionEventsResponse(response)).toBe(true)
  })

  it.each([
    ['loser identity', {
      ...response,
      items: [{ ...response.items[0], loser: { ...response.items[0].loser, annotation_id: 'other-annotation' } }],
    }],
    ['winner kind', {
      ...response,
      items: [{ ...response.items[0], winner_at_detection: { ...response.items[0].winner_at_detection, operation_kind: 'replace' } }],
    }],
    ['operation contract version', {
      ...response,
      items: [{ ...response.items[0], loser: { ...response.items[0].loser, contract_version: 2 } }],
    }],
    ['winner payload', {
      ...response,
      items: [{
        ...response.items[0],
        winner_at_detection: {
          ...response.items[0].winner_at_detection,
          payload: { ...response.items[0].winner_at_detection.payload, quote: null },
        },
      }],
    }],
    ['winner version order', {
      ...response,
      items: [{
        ...response.items[0],
        winner_at_detection: {
          ...response.items[0].winner_at_detection,
          logical_clock: 4,
          device_id: 'device-a',
          op_id: 'a-winner',
        },
      }],
    }],
    ['unsafe event sequence', {
      ...response,
      items: [{ ...response.items[0], sequence: Number.MAX_SAFE_INTEGER + 1 }],
    }],
    ['malformed event cursor', { ...response, next_cursor: 'not a cursor' }],
  ])('fails closed for invalid %s', (_name, candidate) => {
    expect(isReaderThoughtSupersessionEventsResponse(candidate)).toBe(false)
  })
})

describe('isPaginatedLinks', () => {
  it('合法分页', () => {
    expect(isPaginatedLinks({ items: [], total: 0, page: 1, limit: 10 })).toBe(true)
  })
  it('items 非数组 → false', () => {
    expect(isPaginatedLinks({ items: {}, total: 0, page: 1, limit: 10 })).toBe(false)
  })
  it('limit 非 number → false', () => {
    expect(isPaginatedLinks({ items: [], total: 0, page: 1, limit: '10' })).toBe(false)
  })
})

describe('isTagCountArray', () => {
  it('合法标签聚合', () => {
    expect(isTagCountArray([{ tag: 'go', count: 3 }])).toBe(true)
  })
  it('count 非 number → false', () => {
    expect(isTagCountArray([{ tag: 'go', count: 'x' }])).toBe(false)
  })
})

describe('isDomainTreeSummaryEnvelope', () => {
  it('要求独立的全库 total', () => {
    expect(
      isDomainTreeSummaryEnvelope({
        domains: [{ domain: 'example.com', count: 2 }],
        total: 3,
      }),
    ).toBe(true)
    expect(
      isDomainTreeSummaryEnvelope({
        domains: [{ domain: 'example.com', count: 2 }],
      }),
    ).toBe(false)
  })
})

describe('isGroupedSearchResponse', () => {
  it('兼容旧分组，并严格校验可选的想法/笔记结果', () => {
    const base = { reading: { items: [], total_hint: 0 }, sites: { items: [], total_hint: 0 } }
    expect(isGroupedSearchResponse(base)).toBe(true)
    expect(isGroupedSearchResponse({
      ...base,
      thoughts: {
        items: [{ id: 'thought-1', host_kind: 'link', host_id: 'link-1', link_id: null, snippet: 'thought', updated_at: '2026-08-09T00:00:00Z' }],
        total_hint: 1,
        next_cursor: 'opaque-next',
      },
      notes: {
        items: [{ id: 'note-1', title: 'note', snippet: 'published', published_revision: 1, updated_at: '2026-08-09T00:00:00Z' }],
        total_hint: 1,
      },
    })).toBe(true)
    expect(isGroupedSearchResponse({ ...base, notes: { items: [{ id: 'note-1' }], total_hint: 1 } })).toBe(false)
    expect(isGroupedSearchResponse({ ...base, thoughts: { items: [], total_hint: 1, next_cursor: 1 } })).toBe(false)
  })
})

describe('RSS runtime guards', () => {
  it('subscription 核心要求 id 与 feed_url/url，其余字段可选', () => {
    expect(isFeedSubscription({ id: 'feed-1', feed_url: 'https://example.com/rss' })).toBe(true)
    expect(isFeedSubscription({ id: 'feed-1', url: 'https://example.com/rss' })).toBe(true)
    expect(isFeedSubscription({ id: 'feed-1' })).toBe(false)
  })

  it('feed item 校验核心字段和可选状态类型', () => {
    const item = { id: 'item-1', subscription_id: 'feed-1', title: 'Title', url: 'https://example.com/1' }
    expect(isFeedItem({ ...item, read_at: null, starred: true, analysis_status: 'pending' })).toBe(true)
    expect(isFeedItem({ ...item, analysis_status: 'queued' })).toBe(false)
    expect(isPaginatedFeedItems({ items: [item], total: 1, page: 1, limit: 30 })).toBe(true)
    expect(isPaginatedFeedItems({ items: [{ ...item, title: 7 }], total: 1, page: 1, limit: 30 })).toBe(false)
  })
})

describe('Reader capability negotiation', () => {
  it('accepts the complete Reader capability envelope', () => {
    const reader = {
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
    }
    expect(isCapabilitiesResponse({
      library_kinds: true,
      site_library: true,
      site_auto_classification: false,
      site_management: true,
      site_advanced_management: false,
      archive_versions: [1, 2],
      reader_vnext: true,
      reader,
    })).toBe(true)
    expect(isCapabilitiesResponse({
      library_kinds: true,
      site_library: true,
      site_auto_classification: false,
      site_management: true,
      site_advanced_management: false,
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
      },
    })).toBe(false)
  })

  it('turns a legacy envelope without Reader fields into capability-off', () => {
    const normalized = normalizeCapabilitiesResponse({
      library_kinds: true,
      site_library: true,
      archive_versions: [1],
    })
    expect(normalized.reader_vnext).toBe(false)
    expect(normalized.reader.home).toBe(false)
    expect(normalized.reader.trash).toBe(false)
    expect(normalized.archive_versions).toEqual([1])
  })
})

describe('Reader Inbox bulk response guard', () => {
  it('accepts the generated atomic response and both item statuses', () => {
    expect(isReaderInboxBulkResponse({
      atomic: true,
      items: [
        { inbox_id: 'inbox-1', status: 'confirmed', link_id: 'link-1' },
        { inbox_id: 'inbox-2', status: 'discarded' },
      ],
    })).toBe(true)
  })

  it.each([
    { atomic: false, items: [] },
    { atomic: true, items: [{ inbox_id: 'inbox-1', status: 'partial' }] },
    { atomic: true, items: [{ inbox_id: 'inbox-1', status: 'confirmed', link_id: 7 }] },
    { atomic: true, items: [{ inbox_id: 7, status: 'discarded' }] },
  ])('fails closed for an invalid atomic response: %#', (value) => {
    expect(isReaderInboxBulkResponse(value)).toBe(false)
  })
})

describe('Reader Inbox expiration and AI confirmation guards', () => {
  const activeInbox = {
    id: 'inbox-1',
    url: 'https://example.com/article',
    source_kind: 'manual',
    title: 'Article',
    body: 'Body',
    note: '',
    summary: null,
    suggested_tags: [],
    proposal_signals: {},
    proposal_status: 'completed',
    tags: [],
    category_ids: [],
    status: 'pending',
    metadata_revision: 1,
    job_id: null,
    expires_at: '2026-09-10T01:00:00Z',
    expired_at: null,
    expired: false,
    created_at: '2026-08-11T01:00:00Z',
    updated_at: '2026-08-11T01:00:00Z',
  }

  it('requires the materialized expiration fields and their derived relation', () => {
    expect(isReaderInboxResponse(activeInbox)).toBe(true)
    expect(isReaderInboxResponse({ ...activeInbox, expired: true })).toBe(false)
    expect(isReaderInboxResponse({ ...activeInbox, expired_at: undefined })).toBe(false)
    expect(isReaderInboxResponsePage({
      items: [activeInbox],
      active_count: 1,
      expired_count: 0,
    })).toBe(true)
    expect(isReaderInboxResponsePage({
      items: [activeInbox],
      active_count: -1,
      expired_count: 0,
    })).toBe(false)
  })

  it('requires an atomic response with a nonnegative remaining count', () => {
    expect(isReaderInboxConfirmAIProposalsResponse({
      atomic: true,
      items: [{ inbox_id: 'inbox-1', status: 'confirmed', link_id: 'link-1' }],
      remaining_count: 0,
    })).toBe(true)
    expect(isReaderInboxConfirmAIProposalsResponse({
      atomic: true,
      items: [],
      remaining_count: -1,
    })).toBe(false)
    expect(isReaderInboxConfirmAIProposalsResponse({ atomic: true, items: [] })).toBe(false)
  })
})
