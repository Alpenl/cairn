import {
  type ApiResult,
  ok,
} from '@webtag/api'
import {
  buildReaderQuery,
  readerLimit,
  type ReaderEndpointTransport,
} from './endpoint-helpers'
import {
  isReaderAIResponse,
  isReaderHomeResponse,
  isReaderHostLifecycleResponse,
  isReaderTrashResponse,
  normalizeCapabilitiesResponse,
} from './guards'
import { shapeMismatch, type ReaderRequestOptions } from './transport'
import type {
  CapabilitiesResponse,
  ReaderAIRequest,
  ReaderAIResponse,
  ReaderHomeResponse,
  ReaderHostKind,
  ReaderHostLifecycleResponse,
  ReaderHostPurgeRequest,
  ReaderTrashResponse,
} from './types'

export async function getCapabilities(
  transport: ReaderEndpointTransport,
  signal?: AbortSignal,
): Promise<ApiResult<CapabilitiesResponse>> {
  const r = await transport.send('GET', '/api/capabilities', { signal })
  if (!r.ok) return r
  return ok(normalizeCapabilitiesResponse(r.data))
}

export async function listTrash(
  transport: ReaderEndpointTransport,
  params: { hostKind?: ReaderHostKind; after?: string; limit?: number } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderTrashResponse>> {
  const query = buildReaderQuery({
    host_kind: params.hostKind,
    after: params.after,
    limit: readerLimit(params.limit, 50),
  })
  const r = await transport.send('GET', `/api/trash${query}`, { signal: options.signal })
  if (!r.ok) return r
  return isReaderTrashResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderTrashResponse')
}

export async function deleteLink(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const r = await transport.send('DELETE', `/api/links/${encodeURIComponent(id)}`, {
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}

export async function restoreLink(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderHostLifecycleResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/restore`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderHostLifecycleResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHostLifecycleResponse')
}

export async function purgeHost(
  transport: ReaderEndpointTransport,
  kind: ReaderHostKind,
  id: string,
  operationID: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<true>> {
  const collection = kind === 'link' ? 'links' : kind === 'note' ? 'notes' : 'inbox'
  const request: ReaderHostPurgeRequest = { operation_id: operationID }
  const r = await transport.send('DELETE', `/api/${collection}/${encodeURIComponent(id)}/purge`, {
    body: request,
    signal: options.signal,
  })
  return r.ok ? ok(true) : r
}

export async function getHome(
  transport: ReaderEndpointTransport,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderHomeResponse>> {
  const r = await transport.send('GET', '/api/home', { signal: options.signal })
  if (!r.ok) return r
  return isReaderHomeResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderHomeResponse')
}

export async function completeReaderAI(
  transport: ReaderEndpointTransport,
  request: ReaderAIRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderAIResponse>> {
  const r = await transport.send('POST', '/api/ai', {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderAIResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderAIResponse')
}
