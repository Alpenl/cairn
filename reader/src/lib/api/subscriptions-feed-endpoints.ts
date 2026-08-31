import {
  type ApiResult,
  ok,
} from '@webtag/api'
import { isRecord } from '../records'
import {
  buildFeedItemsQuery,
  buildReaderQuery,
  normalizeReaderFeedSources,
  readerLimit,
  type ReaderEndpointTransport,
  type ReaderFeedSource,
} from './endpoint-helpers'
import {
  isDiscoverFeedsResponse,
  isFeedFolder,
  isFeedItem,
  isFeedItemAnalyzeResponse,
  isFeedSubscription,
  isOPMLImportResponse,
  isPaginatedFeedItems,
  isReaderFeedFeedbackResponse,
  isReaderFeedResponse,
  isSubscriptionsResponse,
} from './guards'
import { shapeMismatch, type ReaderReadOptions, type ReaderRequestOptions } from './transport'
import type {
  DiscoverFeedsResponse,
  FeedFolder,
  FeedItem,
  FeedItemAnalyzeResponse,
  FeedItemStatePatch,
  FeedSubscription,
  ListFeedItemsParams,
  OPMLImportResponse,
  PaginatedFeedItemsResponse,
  ReaderFeedFeedbackRequest,
  ReaderFeedFeedbackResponse,
  ReaderFeedResponse,
  SubscriptionsResponse,
} from './types'

export async function getSubscriptions(
  transport: ReaderEndpointTransport,
  url?: string,
  options?: ReaderReadOptions,
): Promise<ApiResult<SubscriptionsResponse>> {
  const query = url?.trim() ? `?url=${encodeURIComponent(url.trim())}` : ''
  const r = await transport.send('GET', `/api/subscriptions${query}`, { readOptions: options })
  if (!r.ok) return r
  if (!isSubscriptionsResponse(r.data)) return shapeMismatch('SubscriptionsResponse')
  return ok(r.data)
}

export async function discoverFeeds(
  transport: ReaderEndpointTransport,
  url: string,
): Promise<ApiResult<DiscoverFeedsResponse>> {
  const r = await transport.send('POST', '/api/subscriptions/discover', { body: { url } })
  if (!r.ok) return r
  if (!isDiscoverFeedsResponse(r.data)) return shapeMismatch('DiscoverFeedsResponse')
  return ok(r.data)
}

export async function createSubscription(
  transport: ReaderEndpointTransport,
  input: {
    url: string
    folder_id?: string | null
  },
): Promise<ApiResult<FeedSubscription>> {
  const r = await transport.send('POST', '/api/subscriptions', { body: input })
  if (!r.ok) return r
  const candidate = isRecord(r.data) && 'subscription' in r.data ? r.data.subscription : r.data
  if (!isFeedSubscription(candidate)) return shapeMismatch('FeedSubscription')
  return ok(candidate)
}

export async function updateSubscription(
  transport: ReaderEndpointTransport,
  id: string,
  patch: { folder_id?: string | null },
): Promise<ApiResult<FeedSubscription | null>> {
  const r = await transport.send('PUT', `/api/subscriptions/${encodeURIComponent(id)}`, { body: patch })
  if (!r.ok) return r
  if (r.data === null) return ok(null)
  const candidate = isRecord(r.data) && 'subscription' in r.data ? r.data.subscription : r.data
  if (!isFeedSubscription(candidate)) return shapeMismatch('FeedSubscription')
  return ok(candidate)
}

export async function deleteSubscription(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<true>> {
  const r = await transport.send('DELETE', `/api/subscriptions/${encodeURIComponent(id)}`)
  return r.ok ? ok(true) : r
}

export async function refreshSubscription(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<true>> {
  const r = await transport.send('POST', `/api/subscriptions/${encodeURIComponent(id)}/refresh`)
  return r.ok ? ok(true) : r
}

export async function refreshSubscriptions(
  transport: ReaderEndpointTransport,
): Promise<ApiResult<true>> {
  const r = await transport.send('POST', '/api/subscriptions/refresh')
  return r.ok ? ok(true) : r
}

export async function getFeedItems(
  transport: ReaderEndpointTransport,
  params: ListFeedItemsParams = {},
  options?: ReaderReadOptions,
): Promise<ApiResult<PaginatedFeedItemsResponse>> {
  const r = await transport.send('GET', `/api/feed-items${buildFeedItemsQuery(params)}`, {
    readOptions: options,
  })
  if (!r.ok) return r
  if (!isPaginatedFeedItems(r.data)) return shapeMismatch('PaginatedFeedItemsResponse')
  return ok(r.data)
}

export async function getFeedItem(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<FeedItem>> {
  const r = await transport.send('GET', `/api/feed-items/${encodeURIComponent(id)}`)
  if (!r.ok) return r
  const candidate = isRecord(r.data) && 'item' in r.data ? r.data.item : r.data
  if (!isFeedItem(candidate)) return shapeMismatch('FeedItem')
  return ok(candidate)
}

export async function updateFeedItem(
  transport: ReaderEndpointTransport,
  id: string,
  patch: FeedItemStatePatch,
): Promise<ApiResult<FeedItem | null>> {
  const r = await transport.send('PUT', `/api/feed-items/${encodeURIComponent(id)}/state`, {
    body: patch,
  })
  if (!r.ok) return r
  if (r.data === null) return ok(null)
  const candidate = isRecord(r.data) && 'item' in r.data ? r.data.item : r.data
  if (!isFeedItem(candidate)) return shapeMismatch('FeedItem')
  return ok(candidate)
}

export async function markFeedItemsRead(
  transport: ReaderEndpointTransport,
  filters: ListFeedItemsParams,
): Promise<ApiResult<true>> {
  const r = await transport.send('POST', '/api/feed-items/mark-read', { body: filters })
  return r.ok ? ok(true) : r
}

export async function analyzeFeedItem(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<FeedItemAnalyzeResponse>> {
  const r = await transport.send('POST', `/api/feed-items/${encodeURIComponent(id)}/analyze`)
  if (!r.ok) return r
  if (isFeedItem(r.data)) return ok({ item: r.data })
  if (!isFeedItemAnalyzeResponse(r.data)) return shapeMismatch('FeedItemAnalyzeResponse')
  return ok(r.data)
}

export async function createFeedFolder(
  transport: ReaderEndpointTransport,
  name: string,
): Promise<ApiResult<FeedFolder>> {
  const r = await transport.send('POST', '/api/feed-folders', { body: { name } })
  if (!r.ok) return r
  const candidate = isRecord(r.data) && 'folder' in r.data ? r.data.folder : r.data
  if (!isFeedFolder(candidate)) return shapeMismatch('FeedFolder')
  return ok(candidate)
}

export async function updateFeedFolder(
  transport: ReaderEndpointTransport,
  id: string,
  name: string,
): Promise<ApiResult<FeedFolder | null>> {
  const r = await transport.send('PUT', `/api/feed-folders/${encodeURIComponent(id)}`, {
    body: { name },
  })
  if (!r.ok) return r
  if (r.data === null) return ok(null)
  const candidate = isRecord(r.data) && 'folder' in r.data ? r.data.folder : r.data
  if (!isFeedFolder(candidate)) return shapeMismatch('FeedFolder')
  return ok(candidate)
}

export async function deleteFeedFolder(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<true>> {
  const r = await transport.send('DELETE', `/api/feed-folders/${encodeURIComponent(id)}`)
  return r.ok ? ok(true) : r
}

export async function exportSubscriptionsOPML(
  transport: ReaderEndpointTransport,
): Promise<ApiResult<string>> {
  const r = await transport.send('GET', '/api/subscriptions/opml')
  if (!r.ok) return r
  if (typeof r.data === 'string') return ok(r.data)
  if (isRecord(r.data) && typeof r.data.opml === 'string') return ok(r.data.opml)
  return shapeMismatch('OPML string or { opml }')
}

export async function importSubscriptionsOPML(
  transport: ReaderEndpointTransport,
  opml: string,
): Promise<ApiResult<OPMLImportResponse>> {
  const r = await transport.send('POST', '/api/subscriptions/opml', { body: { opml } })
  if (!r.ok) return r
  if (!isOPMLImportResponse(r.data)) return shapeMismatch('OPMLImportResponse')
  return ok(r.data)
}

export async function getReaderFeed(
  transport: ReaderEndpointTransport,
  params: {
    mode?: 'recommended' | 'chronological'
    source?: readonly ReaderFeedSource[]
    after?: string
    limit?: number
  } = {},
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderFeedResponse>> {
  const query = buildReaderQuery({
    mode: params.mode,
    source: normalizeReaderFeedSources(params.source),
    after: params.after,
    limit: readerLimit(params.limit, 50),
  })
  const r = await transport.send('GET', `/api/reader-feed${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderFeedResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderFeedResponse')
}

export async function sendReaderFeedFeedback(
  transport: ReaderEndpointTransport,
  itemKey: string,
  request: ReaderFeedFeedbackRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderFeedFeedbackResponse>> {
  const query = buildReaderQuery({ item_key: itemKey })
  const r = await transport.send('POST', `/api/reader-feed/feedback${query}`, {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderFeedFeedbackResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderFeedFeedbackResponse')
}
