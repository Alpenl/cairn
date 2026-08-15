import type { TranslationSourceIdentity } from './generated'

/** Stable error categories shared by browser-facing WebTag clients. */
export type ApiErrorKind =
  | 'not-modified'
  | 'identity-mismatch'
  | 'unauthorized'
  | 'network-unreachable'
  | 'timeout'
  | 'rate-limited'
  | 'other'

/** Environment-neutral error returned at the API adapter boundary. */
export interface ApiError {
  kind: ApiErrorKind
  message: string
  status?: number
  errorCode?: string
  currentIdentity?: TranslationSourceIdentity
  retryAfterSeconds?: number
}

/** API adapters return a discriminated result and do not leak fetch errors. */
export type ApiResult<T> =
  | { ok: true; data: T }
  | { ok: false; error: ApiError }

export function ok<T>(data: T): ApiResult<T> {
  return { ok: true, data }
}

export function err<T>(error: ApiError): ApiResult<T> {
  return { ok: false, error }
}
