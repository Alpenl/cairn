import {
  buildQueryString,
  type ApiResult,
  ok,
} from '@webtag/api'
import type { ReaderHttpTransport } from './transport'
import { shapeMismatch, type ReaderRequestOptions } from './transport'
import type { ListFeedItemsParams } from './types'

export type ReaderEndpointTransport = Pick<ReaderHttpTransport, 'send'>

export function readerIdempotencyHeaders(options: ReaderRequestOptions): Record<string, string> | undefined {
  const key = options.idempotencyKey?.trim()
  return key ? { 'Idempotency-Key': key } : undefined
}

/** Build the RSS item filter query without emitting empty/default values. */
export function buildFeedItemsQuery(params: ListFeedItemsParams): string {
  const normalize = (value: string | number | undefined): string | undefined => {
    if (value === undefined) return undefined
    const normalized = String(value).trim()
    return normalized || undefined
  }
  return buildQueryString(
    {
      view: normalize(params.view),
      subscription_id: normalize(params.subscription_id),
      folder_id: normalize(params.folder_id),
      q: normalize(params.q),
      page:
        params.page !== undefined && params.page > 1 ? params.page : undefined,
      limit:
        params.limit !== undefined && params.limit > 0
          ? params.limit
          : undefined,
    },
    true,
  )
}

export function buildReaderQuery(values: Record<string, string | number | readonly string[] | undefined>): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(values)) {
    if (value === undefined) continue
    if (Array.isArray(value)) {
      for (const candidate of value) {
        const normalized = String(candidate).trim()
        if (normalized) query.append(key, normalized)
      }
      continue
    }
    const normalized = String(value).trim()
    if (normalized) query.set(key, normalized)
  }
  return query.size > 0 ? `?${query.toString()}` : ''
}

const READER_FEED_SOURCE_ORDER = ['inbox', 'reading', 'subscription'] as const
export type ReaderFeedSource = typeof READER_FEED_SOURCE_ORDER[number]

export function normalizeReaderFeedSources(values: readonly string[] | undefined): ReaderFeedSource[] | undefined {
  if (values === undefined) return undefined
  const selected = new Set<ReaderFeedSource>()
  for (const value of values) {
    for (const part of value.split(',')) {
      const normalized = part.trim().toLowerCase()
      const source: ReaderFeedSource | null = normalized === 'pending'
        ? 'inbox'
        : normalized === 'saved'
          ? 'reading'
          : normalized === 'inbox' || normalized === 'reading' || normalized === 'subscription'
            ? normalized
            : null
      if (source) selected.add(source)
    }
  }
  return READER_FEED_SOURCE_ORDER.filter((source) => selected.has(source))
}

export function readerLimit(limit: number | undefined, fallback: number): number | undefined {
  if (limit === undefined) return fallback
  return limit > 0 ? Math.min(Math.floor(limit), 200) : fallback
}

export function expectPayload<T>(
  result: ApiResult<unknown>,
  guard: (value: unknown) => value is T,
  detail: string,
): ApiResult<T> {
  if (!result.ok) return result
  return guard(result.data) ? ok(result.data) : shapeMismatch(detail)
}
