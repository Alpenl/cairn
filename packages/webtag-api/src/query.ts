import type { paths } from './generated'

export type QueryValue = string | number | boolean | null | undefined
export type QueryParameters = Readonly<Record<string, QueryValue>>

function encodeFormComponent(value: string): string {
  return encodeURIComponent(value)
    .replace(/[!'()~]/g, (character) =>
      `%${character.charCodeAt(0).toString(16).toUpperCase()}`,
    )
    .replace(/%20/g, '+')
}

/** Build a WHATWG-compatible query string without requiring URLSearchParams. */
export function buildQueryString(
  parameters: QueryParameters,
  prefix = false,
): string {
  const query = Object.entries(parameters)
    .filter((entry): entry is [string, Exclude<QueryValue, null | undefined>] =>
      entry[1] !== undefined && entry[1] !== null,
    )
    .map(
      ([key, value]) =>
        `${encodeFormComponent(key)}=${encodeFormComponent(String(value))}`,
    )
    .join('&')
  if (!query) return ''
  return prefix ? `?${query}` : query
}

type ListLinksParams = NonNullable<
  paths['/api/links']['get']['parameters']['query']
>

function trimmed(value: string | number | undefined): string | undefined {
  if (value === undefined) return undefined
  const normalized = String(value).trim()
  return normalized === '' ? undefined : normalized
}

/** Canonical GET /api/links query semantics shared by both clients. */
export function buildLinksQuery(params: ListLinksParams): string {
  return buildQueryString(
    {
      q: trimmed(params.q),
      tags: trimmed(params.tags),
      domain: trimmed(params.domain),
      content_type: trimmed(params.content_type),
      status: trimmed(params.status),
      library_kind: trimmed(params.library_kind),
      low_confidence: params.low_confidence,
      created_from: trimmed(params.created_from),
      created_before: trimmed(params.created_before),
      url: trimmed(params.url),
      after: params.after,
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
