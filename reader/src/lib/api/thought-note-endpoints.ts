import {
  type ApiResult,
  ok,
} from '@webtag/api'
import { isRecord } from '../records'
import {
  buildReaderQuery,
  readerIdempotencyHeaders,
  readerLimit,
  type ReaderEndpointTransport,
} from './endpoint-helpers'
import {
  isReaderHostLifecycleResponse,
  isReaderNoteHistoryResponse,
  isReaderNoteResponse,
  isReaderNotesResponse,
  isReaderThoughtAckResponse,
  isReaderThoughtResponse,
  isReaderThoughtsResponse,
  isReaderThoughtSupersessionEventsResponse,
} from './guards'
import { shapeMismatch, type ReaderRequestOptions } from './transport'
import type {
  ReaderHostLifecycleResponse,
  ReaderNoteCreateRequest,
  ReaderNoteDraftRequest,
  ReaderNoteHistoryResponse,
  ReaderNotePublishRequest,
  ReaderNoteResponse,
  ReaderNoteRestoreRequest,
  ReaderNotesResponse,
  ReaderThoughtAckResponse,
  ReaderThoughtOpsRequest,
  ReaderThoughtResponse,
  ReaderThoughtsResponse,
  ReaderThoughtSupersessionEventsResponse,
} from './types'

export async function pushThoughtOps(
  transport: ReaderEndpointTransport,
  request: ReaderThoughtOpsRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtAckResponse[]>> {
  const r = await transport.send('POST', '/api/annotations/ops', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  if (!isRecord(r.data) || !Array.isArray(r.data.items) || !r.data.items.every(isReaderThoughtAckResponse)) {
    return shapeMismatch('ReaderThoughtAckResponse[]')
  }
  return ok(r.data.items)
}

export async function listThoughts(
  transport: ReaderEndpointTransport,
  params: { q?: string; after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtsResponse>> {
  const query = buildReaderQuery({
    q: params.q,
    after: params.after,
    limit: readerLimit(params.limit, 50),
  })
  const r = await transport.send('GET', `/api/annotations${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
}

export async function syncThoughts(
  transport: ReaderEndpointTransport,
  params: { after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtsResponse>> {
  const query = buildReaderQuery({
    after: params.after,
    limit: readerLimit(params.limit, 100),
  })
  const r = await transport.send('GET', `/api/annotations/sync${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
}

export async function listThoughtSupersessions(
  transport: ReaderEndpointTransport,
  params: { after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtSupersessionEventsResponse>> {
  const query = buildReaderQuery({
    after: params.after,
    limit: readerLimit(params.limit, 100),
  })
  const r = await transport.send('GET', `/api/annotations/conflicts${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderThoughtSupersessionEventsResponse(r.data)
    ? ok(r.data)
    : shapeMismatch('ReaderThoughtSupersessionEventsResponse')
}

export async function listThoughtHistory(
  transport: ReaderEndpointTransport,
  params: { after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtsResponse>> {
  const query = buildReaderQuery({ after: params.after, limit: readerLimit(params.limit, 30) })
  const r = await transport.send('GET', `/api/annotations/history${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderThoughtsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtsResponse')
}

export async function getThought(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderThoughtResponse>> {
  const r = await transport.send('GET', `/api/annotations/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderThoughtResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderThoughtResponse')
}

export async function createNote(
  transport: ReaderEndpointTransport,
  request: ReaderNoteCreateRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteResponse>> {
  const r = await transport.send('POST', '/api/notes', {
    body: request,
    headers: readerIdempotencyHeaders(options),
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
}

export async function listNotes(
  transport: ReaderEndpointTransport,
  params: { after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNotesResponse>> {
  const query = buildReaderQuery({ after: params.after, limit: readerLimit(params.limit, 30) })
  const r = await transport.send('GET', `/api/notes${query}`, { signal: options.signal })
  if (!r.ok) return r
  return isReaderNotesResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNotesResponse')
}

export async function getNote(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteResponse>> {
  const r = await transport.send('GET', `/api/notes/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
}

export async function saveNoteDraft(
  transport: ReaderEndpointTransport,
  id: string,
  request: ReaderNoteDraftRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteResponse>> {
  const r = await transport.send('PATCH', `/api/notes/${encodeURIComponent(id)}/draft`, {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
}

export async function discardNoteDraft(
  transport: ReaderEndpointTransport,
  id: string,
  expectedDraftRevision: number,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const r = await transport.send('DELETE', `/api/notes/${encodeURIComponent(id)}/draft`, {
    headers: { 'If-Match': `"${expectedDraftRevision}"` },
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}

export async function publishNote(
  transport: ReaderEndpointTransport,
  id: string,
  request: ReaderNotePublishRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteResponse>> {
  const r = await transport.send('POST', `/api/notes/${encodeURIComponent(id)}/publish`, {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
}

export async function deleteNote(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderHostLifecycleResponse>> {
  const r = await transport.send('DELETE', `/api/notes/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
}

export async function restoreNote(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderHostLifecycleResponse>> {
  const r = await transport.send('POST', `/api/notes/${encodeURIComponent(id)}/restore`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
}

export async function listNoteHistory(
  transport: ReaderEndpointTransport,
  id: string,
  limit = 50,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteHistoryResponse[]>> {
  const query = buildReaderQuery({ limit: readerLimit(limit, 50) })
  const r = await transport.send('GET', `/api/notes/${encodeURIComponent(id)}/history${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  if (!isRecord(r.data) || !Array.isArray(r.data.items) || !r.data.items.every(isReaderNoteHistoryResponse)) {
    return shapeMismatch('ReaderNoteHistoryResponse[]')
  }
  return ok(r.data.items)
}

export async function restoreNoteRevision(
  transport: ReaderEndpointTransport,
  id: string,
  revision: number,
  request: ReaderNoteRestoreRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderNoteResponse>> {
  const r = await transport.send(
    'POST',
    `/api/notes/${encodeURIComponent(id)}/history/${encodeURIComponent(String(revision))}/restore`,
    {
      body: request,
      signal: options.signal,
    },
  )
  if (!r.ok) return r
  return isReaderNoteResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderNoteResponse')
}
