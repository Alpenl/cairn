import {
  type ApiResult,
  ok,
} from '@webtag/api'
import {
  buildReaderQuery,
  readerIdempotencyHeaders,
  readerLimit,
  type ReaderEndpointTransport,
} from './endpoint-helpers'
import {
  isReaderConfirmResponse,
  isReaderInboxBulkResponse,
  isReaderInboxConfirmAIProposalsResponse,
  isReaderInboxResponse,
  isReaderInboxResponsePage,
  isReaderTodoResponse,
  isReaderTodosResponse,
} from './guards'
import { shapeMismatch, type ReaderRequestOptions } from './transport'
import type {
  ReaderConfirmResponse,
  ReaderInboxBulkRequest,
  ReaderInboxBulkResponse,
  ReaderInboxConfirmAIProposalsRequest,
  ReaderInboxConfirmAIProposalsResponse,
  ReaderInboxCreateRequest,
  ReaderInboxPartition,
  ReaderInboxPatchRequest,
  ReaderInboxResponse,
  ReaderInboxResponsePage,
  ReaderTodoCreateRequest,
  ReaderTodoPatchRequest,
  ReaderTodoResponse,
  ReaderTodosResponse,
} from './types'

export async function createInbox(
  transport: ReaderEndpointTransport,
  request: ReaderInboxCreateRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxResponse>> {
  const r = await transport.send('POST', '/api/inbox', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
}

export async function listInbox(
  transport: ReaderEndpointTransport,
  params: { partition?: ReaderInboxPartition; after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxResponsePage>> {
  const query = buildReaderQuery({
    partition: params.partition,
    after: params.after,
    limit: readerLimit(params.limit, 50),
  })
  const r = await transport.send('GET', `/api/inbox${query}`, { signal: options.signal })
  if (!r.ok) return r
  return isReaderInboxResponsePage(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponsePage')
}

export async function getInbox(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxResponse>> {
  const r = await transport.send('GET', `/api/inbox/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
}

export async function patchInbox(
  transport: ReaderEndpointTransport,
  id: string,
  revision: number,
  request: ReaderInboxPatchRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxResponse>> {
  const r = await transport.send('PATCH', `/api/inbox/${encodeURIComponent(id)}`, {
    body: request,
    headers: { 'If-Match': `"${revision}"` },
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
}

export async function confirmInbox(
  transport: ReaderEndpointTransport,
  id: string,
  revision?: number,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderConfirmResponse>> {
  const r = await transport.send('POST', `/api/inbox/${encodeURIComponent(id)}/confirm`, {
    headers: {
      ...readerIdempotencyHeaders(options),
      ...(revision === undefined ? {} : { 'If-Match': `"${revision}"` }),
    },
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderConfirmResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderConfirmResponse')
}

export async function restoreInbox(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const r = await transport.send('POST', `/api/inbox/${encodeURIComponent(id)}/restore`, {
    headers: readerIdempotencyHeaders(options),
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}

export async function confirmInboxBulk(
  transport: ReaderEndpointTransport,
  request: ReaderInboxBulkRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxBulkResponse>> {
  const r = await transport.send('POST', '/api/inbox/bulk/confirm', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxBulkResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxBulkResponse')
}

export async function confirmAIProposals(
  transport: ReaderEndpointTransport,
  request: ReaderInboxConfirmAIProposalsRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxConfirmAIProposalsResponse>> {
  const r = await transport.send('POST', '/api/inbox/confirm-ai-proposals', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxConfirmAIProposalsResponse(r.data)
    ? ok(r.data)
    : shapeMismatch('ReaderInboxConfirmAIProposalsResponse')
}

export async function discardInboxBulk(
  transport: ReaderEndpointTransport,
  request: ReaderInboxBulkRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxBulkResponse>> {
  const r = await transport.send('POST', '/api/inbox/bulk/discard', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxBulkResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxBulkResponse')
}

export async function discardInbox(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const r = await transport.send('POST', `/api/inbox/${encodeURIComponent(id)}/discard`, {
    headers: readerIdempotencyHeaders(options),
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}

export async function resummarizeInbox(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderInboxResponse>> {
  const r = await transport.send('POST', `/api/inbox/${encodeURIComponent(id)}/resummarize`, {
    headers: readerIdempotencyHeaders(options),
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderInboxResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderInboxResponse')
}

export async function createTodo(
  transport: ReaderEndpointTransport,
  request: ReaderTodoCreateRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderTodoResponse>> {
  const r = await transport.send('POST', '/api/todos', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderTodoResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTodoResponse')
}

export async function listTodos(
  transport: ReaderEndpointTransport,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderTodosResponse>> {
  const items: ReaderTodoResponse[] = []
  const seen = new Set<string>()
  let after: string | undefined
  for (let pageIndex = 0; pageIndex < 100; pageIndex += 1) {
    const path = `/api/todos${buildReaderQuery({ limit: 200, after })}`
    const r = await transport.send('GET', path, { signal: options.signal })
    if (!r.ok) return r
    if (!isReaderTodosResponse(r.data)) return shapeMismatch('ReaderTodosResponse')
    items.push(...r.data.items)
    const next = r.data.next_after?.trim()
    if (!next) {
      const response = { ...r.data, items }
      delete response.next_after
      return ok(response)
    }
    if (seen.has(next)) return shapeMismatch('ReaderTodosResponse cursor')
    seen.add(next)
    after = next
  }
  return shapeMismatch('ReaderTodosResponse page limit')
}

export async function patchTodo(
  transport: ReaderEndpointTransport,
  id: string,
  request: ReaderTodoPatchRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderTodoResponse>> {
  const r = await transport.send('PATCH', `/api/todos/${encodeURIComponent(id)}`, {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderTodoResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTodoResponse')
}

export async function deleteTodo(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const r = await transport.send('DELETE', `/api/todos/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}
