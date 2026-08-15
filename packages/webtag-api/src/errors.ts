import { isErrorResponse } from './guards'
import type { ApiError, ApiErrorKind } from './result'

export interface RetryAfterOptions {
  allowZero?: boolean
}

/** Parse the integer-seconds form emitted by the WebTag backend. */
export function parseRetryAfter(
  raw: string | null | undefined,
  options: RetryAfterOptions = {},
): number | undefined {
  if (!raw) return undefined
  const seconds = Number(raw.trim())
  if (!Number.isInteger(seconds)) return undefined
  return seconds > 0 || (options.allowZero === true && seconds === 0)
    ? seconds
    : undefined
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function looseErrorFields(body: unknown): {
  message?: string
  errorCode?: string
} {
  if (!isRecord(body) || !isRecord(body.error)) return {}
  const result: { message?: string; errorCode?: string } = {}
  if (
    typeof body.error.message === 'string' &&
    body.error.message.trim() !== ''
  ) {
    result.message = body.error.message.trim()
  }
  if (typeof body.error.error_code === 'string') {
    result.errorCode = body.error.error_code
  }
  return result
}

/** Normalize a non-success HTTP response without depending on fetch globals. */
export function normalizeHttpError(
  status: number,
  body: unknown,
  retryAfterSeconds?: number,
): ApiError {
  const fields = looseErrorFields(body)
  let kind: ApiErrorKind = 'other'
  if (status === 401) {
    kind = 'unauthorized'
  } else if (status === 403) {
    kind = 'unauthorized'
  } else if (status === 429) {
    kind = 'rate-limited'
  }

  const error: ApiError = {
    kind,
    message: fields.message ?? `HTTP ${status}`,
    status,
  }
  if (fields.errorCode !== undefined) error.errorCode = fields.errorCode
  if (isErrorResponse(body) && body.error.current_identity !== undefined) {
    error.currentIdentity = body.error.current_identity
  }
  if (kind === 'rate-limited' && retryAfterSeconds !== undefined) {
    error.retryAfterSeconds = retryAfterSeconds
  }
  return error
}

/** Normalize an adapter exception without referencing DOMException. */
export function normalizeThrownError(
  thrown: unknown,
  timeoutMs: number,
): ApiError {
  if (
    isRecord(thrown) &&
    typeof thrown.name === 'string' &&
    thrown.name === 'AbortError'
  ) {
    return { kind: 'timeout', message: `请求超时（${timeoutMs}ms）` }
  }
  if (thrown instanceof TypeError) {
    return { kind: 'network-unreachable', message: thrown.message }
  }
  return {
    kind: 'other',
    message: thrown instanceof Error ? thrown.message : String(thrown),
  }
}
