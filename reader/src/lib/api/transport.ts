import {
  type ApiError,
  type ApiResult,
  err,
  normalizeHttpError as normalizeSharedHttpError,
  normalizeThrownError,
  ok,
  parseRetryAfter as parseRetryAfterValue,
} from '@webtag/api'
import { isRecord } from '../records'
import {
  ARCHIVE_V2_MAX_BYTES,
  ARCHIVE_V2_MIME_TYPE,
  ARCHIVE_V2_TOO_LARGE_ERROR_CODE,
  ARCHIVE_V2_VALIDATION_ERROR_CODE,
  ArchiveV2ValidationError,
  archiveV2Sections,
  fullArchiveV2Selection,
  isArchiveV2ByteArray,
  validateArchiveV2Bytes,
  type ArchiveV2Selection,
} from './archive-v2'
import { hasCanonicalSafeLinkMetadataRevisionTokens, isHealthResponse } from './guards'
import type { HealthResponse, SessionIdentity } from './types'
import type { IdentityLease, IdentityOperationContext, IdentityOwnership } from '../identity'

const DEFAULT_TIMEOUT = 15000
export const SESSION_HEADER = 'X-WebTag-Session'
export const DATA_NAMESPACE_HEADER = 'X-WebTag-Data-Namespace'

export interface ReaderReadOptions {
  /** Optional cancellation owned by the caller. */
  signal?: AbortSignal
}

/** Per-call cancellation for request paths that are scheduled by a component. */
export interface ReaderRequestOptions extends ReaderReadOptions {
  readonly signal?: AbortSignal
  /** Stable identity reused only when replaying the same mutation intent. */
  readonly idempotencyKey?: string
}

export interface SessionLoginAttempt {
  readonly result: ApiResult<{ expiresAt: string; clientDataNamespace: string }>
  /** A successful HTTP response may already have replaced the browser cookie. */
  readonly sessionMayExist: boolean
}

export type ReaderHttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export interface ReaderHttpTransportConfig {
  /** Backend base URL, without a required trailing slash. */
  readonly baseURL: string
  /** Bearer fallback for legacy servers without session support. */
  readonly installationToken?: string
  /** Cookie-backed Reader session mode. */
  readonly session?: boolean
  /** Per-request timeout in milliseconds. */
  readonly timeoutMs?: number
  /** Installed authoritative identity for private Reader data. */
  readonly identity?: IdentityLease
  /** Called once when a response proves that the browser session changed identity. */
  readonly onIdentityMismatch?: () => void
}

export interface ReaderHttpRequest {
  readonly body?: unknown
  readonly headers?: Record<string, string>
  readonly readOptions?: ReaderReadOptions
  readonly signal?: AbortSignal
  readonly rawJSONContract?: 'link-metadata-revision'
}

const SESSION_IDENTITY_KEYS = new Set([
  'client_data_namespace',
  'representation_contract',
])
const SESSION_CREATED_KEYS = new Set([...SESSION_IDENTITY_KEYS, 'expires_at'])
const DATA_NAMESPACE_PATTERN = /^[A-Za-z0-9_-]{43}$/
const REPRESENTATION_CONTRACT = 'v3'
const RFC3339_DATE_TIME_PATTERN = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|[+-](\d{2}):(\d{2}))$/

function hasOnlyKeys(value: Record<string, unknown>, allowed: ReadonlySet<string>): boolean {
  const keys = Object.keys(value)
  return keys.length === allowed.size && keys.every((key) => allowed.has(key))
}

function hasSessionIdentityFields(
  value: Record<string, unknown>,
): value is Record<string, unknown> & SessionIdentity {
  return (
    typeof value.client_data_namespace === 'string' &&
    DATA_NAMESPACE_PATTERN.test(value.client_data_namespace) &&
    value.representation_contract === REPRESENTATION_CONTRACT
  )
}

function isRFC3339DateTime(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const match = RFC3339_DATE_TIME_PATTERN.exec(value)
  if (!match) return false

  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const hour = Number(match[4])
  const minute = Number(match[5])
  const second = Number(match[6])
  const offsetHour = match[7] === undefined ? 0 : Number(match[7])
  const offsetMinute = match[8] === undefined ? 0 : Number(match[8])
  if (month < 1 || month > 12) return false

  const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
  const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  return (
    day >= 1 &&
    day <= daysInMonth[month - 1] &&
    hour <= 23 &&
    minute <= 59 &&
    second <= 60 &&
    offsetHour <= 23 &&
    offsetMinute <= 59
  )
}

/** Strict RF2A v2 identity wire guard shared by bootstrap and the authenticated client. */
export function isSessionIdentity(value: unknown): value is SessionIdentity {
  return isRecord(value) && hasOnlyKeys(value, SESSION_IDENTITY_KEYS) && hasSessionIdentityFields(value)
}

function isSessionCreated(
  value: unknown,
): value is SessionIdentity & { readonly expires_at: string } {
  if (!isRecord(value)) return false
  const expiresAt = value.expires_at
  return (
    hasOnlyKeys(value, SESSION_CREATED_KEYS) &&
    hasSessionIdentityFields(value) &&
    isRFC3339DateTime(expiresAt)
  )
}

/** Parse the integer-seconds Retry-After form; HTTP-date is intentionally ignored. */
function parseRetryAfter(headers: Headers): number | undefined {
  return parseRetryAfterValue(headers.get('Retry-After'), { allowZero: true })
}

/** Normalize fetch exceptions into timeout / network-unreachable / other. */
function normalizeThrown(e: unknown, timeoutMs: number): ApiError {
  return normalizeThrownError(e, timeoutMs)
}

/** Normalize non-2xx HTTP responses after their error body has been parsed. */
function normalizeHttpError(
  status: number,
  body: unknown,
  retryAfterSeconds: number | undefined,
): ApiError {
  return normalizeSharedHttpError(status, body, retryAfterSeconds)
}

/** Shared failure result for endpoint payload contract mismatches. */
export function shapeMismatch<T>(detail: string): ApiResult<T> {
  return err<T>({ kind: 'other', message: `响应体格式不符：${detail}` })
}

function archiveValidationFailure<T>(failure: unknown): ApiResult<T> {
  if (failure instanceof ArchiveV2ValidationError) {
    return err({
      kind: 'other',
      message: failure.message,
      errorCode: failure.errorCode,
    })
  }
  return err({
    kind: 'other',
    message: '归档完整性校验失败',
    errorCode: ARCHIVE_V2_VALIDATION_ERROR_CODE,
  })
}

function archiveTooLargeFailure<T>(): ApiResult<T> {
  return err({
    kind: 'other',
    message: '归档超过浏览器可验证的 64 MiB 上限',
    errorCode: ARCHIVE_V2_TOO_LARGE_ERROR_CODE,
  })
}

function archiveContentLengthExceedsBrowserLimit(headers: Headers): boolean {
  const raw = headers.get('Content-Length')?.trim()
  if (!raw || !/^\d+$/.test(raw)) return false
  const length = Number(raw)
  return !Number.isSafeInteger(length) || length > ARCHIVE_V2_MAX_BYTES
}

export class ReaderHttpTransport {
  private readonly baseURL: string
  private readonly installationToken: string
  private readonly session: boolean
  private readonly timeoutMs: number
  private readonly identity?: IdentityLease
  private readonly onIdentityMismatch?: () => void

  constructor(config: ReaderHttpTransportConfig) {
    this.baseURL = config.baseURL.replace(/\/+$/, '')
    this.installationToken = config.installationToken?.trim() ?? ''
    this.session = config.session ?? false
    this.timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT
    this.identity = config.identity
    this.onIdentityMismatch = config.onIdentityMismatch
  }

  /** The sole identity lease owned by this transport, or null for bootstrap-only clients. */
  get identityLease(): IdentityLease | null {
    return this.identity ?? null
  }

  /** Authentication headers and fetch options shared by JSON requests and archive downloads. */
  private authInit(): { headers: Record<string, string>; init: RequestInit } {
    if (this.session) {
      return {
        headers: { [SESSION_HEADER]: '1' },
        init: { credentials: 'include' },
      }
    }
    return {
      headers: this.installationToken
        ? { Authorization: `Bearer ${this.installationToken}` }
        : {},
      init: {},
    }
  }

  private validateResponseOwnership(
    response: Response,
    operation: IdentityOperationContext | undefined,
  ): ApiResult<never> | null {
    if (!this.identity || !operation) return null
    if (!this.identity.isCurrent(operation)) {
      return err({ kind: 'identity-mismatch', message: 'Authenticated response belongs to a stale identity epoch' })
    }
    if (response.headers.get(DATA_NAMESPACE_HEADER) === operation.serverClientDataNamespace) {
      return null
    }

    this.identity.revoke()
    this.onIdentityMismatch?.()
    return err({
      kind: 'identity-mismatch',
      message: 'Authenticated response identity marker changed',
    })
  }

  /** Whether component work started with this transport may still commit local side effects. */
  isIdentityCurrent(): boolean {
    return this.captureIdentity('ReaderHttpTransport component continuation') !== null
  }

  /** Capture the immutable lease/epoch owned by this transport for a multi-step component action. */
  captureIdentity(logicalKey: string): IdentityOwnership | null {
    const identity = this.identity
    if (!identity) return null
    const ownership = identity.captureOwnership(logicalKey)
    return identity.isOwnershipCurrent(ownership) ? ownership : null
  }

  /** Fetch the authenticated identity snapshot without consulting any HTTP cache. */
  async getIdentity(signal?: AbortSignal): Promise<ApiResult<SessionIdentity>> {
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const auth = this.authInit()
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}/api/session`, {
          ...auth.init,
          method: 'GET',
          cache: 'no-store',
          headers: { Accept: 'application/json', ...auth.headers },
          signal: controller.signal,
        })
      } catch (e) {
        return err(normalizeThrown(e, this.timeoutMs))
      }

      let body: unknown = null
      try {
        const text = await response.text()
        body = text ? JSON.parse(text) : null
      } catch (e) {
        if (controller.signal.aborted) return err(normalizeThrown(e, this.timeoutMs))
      }
      if (!response.ok) {
        return err(normalizeHttpError(response.status, body, undefined))
      }
      if (!isSessionIdentity(body)) return shapeMismatch('SessionIdentity')
      const marker = response.headers.get(DATA_NAMESPACE_HEADER)
      if (!marker || marker !== body.client_data_namespace) {
        return err({
          kind: 'identity-mismatch',
          message: 'Authenticated response identity marker is missing or inconsistent',
        })
      }
      return ok(body)
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
  }

  /**
   * Read the build identity the backend already publishes on `/health`.
   *
   * Deliberately bypasses `send`: the probe is unauthenticated and carries no
   * per-identity data, so requiring a lease would only make "which Core am I
   * talking to" unanswerable exactly when something is wrong.
   */
  async getHealth(signal?: AbortSignal): Promise<ApiResult<HealthResponse>> {
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}/health`, {
          method: 'GET',
          cache: 'no-store',
          headers: { Accept: 'application/json' },
          signal: controller.signal,
        })
      } catch (e) {
        return err(normalizeThrown(e, this.timeoutMs))
      }

      let body: unknown = null
      try {
        const text = await response.text()
        body = text ? JSON.parse(text) : null
      } catch (e) {
        if (controller.signal.aborted) return err(normalizeThrown(e, this.timeoutMs))
      }
      if (!response.ok) return err(normalizeHttpError(response.status, body, undefined))
      if (!isHealthResponse(body)) return shapeMismatch('HealthResponse')
      return ok(body)
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
  }

  /** Session negotiation uses this to clean up malformed successful exchanges. */
  async loginWithMutationStatus(
    installationToken: string,
    signal?: AbortSignal,
  ): Promise<SessionLoginAttempt> {
    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    try {
      let res: Response
      try {
        res = await fetch(`${this.baseURL}/api/session`, {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ token: installationToken }),
          signal: controller.signal,
        })
      } catch (e) {
        return {
          result: err(normalizeThrown(e, this.timeoutMs)),
          sessionMayExist: false,
        }
      }
      const text = await res.text().catch(() => '')
      let body: unknown = null
      if (text) {
        try {
          body = JSON.parse(text)
        } catch {
          body = null
        }
      }
      if (!res.ok) {
        return {
          result: err(normalizeHttpError(res.status, body, undefined)),
          sessionMayExist: false,
        }
      }
      if (!isSessionCreated(body)) {
        return { result: shapeMismatch('SessionCreated'), sessionMayExist: true }
      }
      const marker = res.headers.get(DATA_NAMESPACE_HEADER)
      if (!marker || marker !== body.client_data_namespace) {
        return {
          result: err({
            kind: 'identity-mismatch',
            message: 'Session creation identity marker is missing or inconsistent',
          }),
          sessionMayExist: true,
        }
      }
      return {
        result: ok({
          expiresAt: body.expires_at,
          clientDataNamespace: body.client_data_namespace,
        }),
        sessionMayExist: true,
      }
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
  }

  /** End the cookie-backed session. Network failure must not block local logout. */
  async logout(): Promise<void> {
    try {
      await fetch(`${this.baseURL}/api/session`, {
        method: 'DELETE',
        credentials: 'include',
        headers: { [SESSION_HEADER]: '1' },
      })
    } catch {
      // The local config is cleared anyway; the cookie will expire server-side later.
    }
  }

  /**
   * Run one identity-bound JSON request and return the parsed response body.
   * Network exceptions, timeouts, non-2xx responses, and malformed JSON all
   * collapse to ApiResult instead of throwing.
   */
  async send(
    method: ReaderHttpMethod,
    path: string,
    request: ReaderHttpRequest = {},
  ): Promise<ApiResult<unknown>> {
    const requestSignal = request.signal ?? request.readOptions?.signal
    const identity = this.identity
    if (!identity) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private request requires an authoritative identity lease',
      })
    }
    const ownership = identity.capture(`${method} ${path}`)
    if (!identity.isCurrent(ownership)) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private request belongs to a stale identity epoch',
      })
    }
    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    const abortForCaller = () => controller.abort()
    if (ownership.signal.aborted) controller.abort()
    else ownership.signal.addEventListener('abort', abortForIdentity, { once: true })
    if (requestSignal?.aborted) controller.abort()
    else requestSignal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const cancellationFailure = (message: string): ApiResult<never> | null => {
      if (!identity.isCurrent(ownership)) {
        return err({ kind: 'identity-mismatch', message })
      }
      if (controller.signal.aborted) {
        return err(normalizeThrown(new DOMException('aborted', 'AbortError'), this.timeoutMs))
      }
      return null
    }
    const auth = this.authInit()
    const headers: Record<string, string> = { Accept: 'application/json', ...auth.headers }
    Object.assign(headers, request.headers)
    if (request.body !== undefined) headers['Content-Type'] = 'application/json'

    try {
      let res: Response
      try {
        res = await fetch(`${this.baseURL}${path}`, {
          ...auth.init,
          method,
          headers,
          ...(request.body === undefined ? {} : { body: JSON.stringify(request.body) }),
          signal: controller.signal,
        })
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated request was cancelled by an identity change',
          })
        }
        return err(normalizeThrown(e, this.timeoutMs))
      }

      const postFetchCancellation = cancellationFailure(
        'Authenticated request was cancelled by an identity change',
      )
      if (postFetchCancellation) return postFetchCancellation

      const ownershipFailure = this.validateResponseOwnership(res, ownership)
      if (ownershipFailure) return ownershipFailure

      let responseBody: unknown = null
      let text: string
      try {
        text = await res.text()
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated response body was cancelled by an identity change',
          })
        }
        if (controller.signal.aborted) {
          return err(normalizeThrown(e, this.timeoutMs))
        }
        text = ''
      }
      const postBodyCancellation = cancellationFailure(
        'Authenticated response body was cancelled by an identity change',
      )
      if (postBodyCancellation) return postBodyCancellation
      const postBodyOwnershipFailure = this.validateResponseOwnership(res, ownership)
      if (postBodyOwnershipFailure) return postBodyOwnershipFailure
      if (text) {
        if (
          res.ok &&
          request.rawJSONContract === 'link-metadata-revision' &&
          !hasCanonicalSafeLinkMetadataRevisionTokens(text)
        ) {
          return shapeMismatch('Link metadata revision JSON')
        }
        try {
          responseBody = JSON.parse(text)
        } catch {
          // OPML export may be returned directly as XML instead of `{ opml }`.
          responseBody = text
        }
      }

      if (!res.ok) {
        const retryAfter = res.status === 429 ? parseRetryAfter(res.headers) : undefined
        return err(normalizeHttpError(res.status, responseBody, retryAfter))
      }
      return ok(responseBody)
    } finally {
      clearTimeout(timer)
      ownership.signal.removeEventListener('abort', abortForIdentity)
      requestSignal?.removeEventListener('abort', abortForCaller)
    }
  }

  /**
   * Read a response body without allowing a streamed archive to exceed the
   * browser verification bound. Identity is checked around every awaited
   * stream read, because an old response must not cross a lease transition.
   */
  private async readArchiveV2Bytes(
    response: Response,
    identity: IdentityLease,
    ownership: IdentityOperationContext,
    controller: AbortController,
  ): Promise<ApiResult<Uint8Array>> {
    if (archiveContentLengthExceedsBrowserLimit(response.headers)) {
      controller.abort()
      void response.body?.cancel().catch(() => undefined)
      return archiveTooLargeFailure()
    }

    const body = response.body
    if (!body) return ok(new Uint8Array())

    const reader = body.getReader()
    const chunks: Uint8Array[] = []
    let total = 0
    const cancelForIdentityChange = (): ApiResult<Uint8Array> => {
      controller.abort()
      void reader.cancel().catch(() => undefined)
      return err({
        kind: 'identity-mismatch',
        message: 'Authenticated archive stream was cancelled by an identity change',
      })
    }
    try {
      for (;;) {
        if (!identity.isCurrent(ownership)) {
          return cancelForIdentityChange()
        }

        let next: ReadableStreamReadResult<Uint8Array>
        try {
          next = await reader.read()
        } catch (e) {
          if (!identity.isCurrent(ownership)) {
            return cancelForIdentityChange()
          }
          return err(normalizeThrown(e, this.timeoutMs))
        }

        if (!identity.isCurrent(ownership)) {
          return cancelForIdentityChange()
        }
        if (next.done) break
        if (!isArchiveV2ByteArray(next.value)) {
          return archiveValidationFailure(
            new ArchiveV2ValidationError('归档响应流不是字节流'),
          )
        }
        if (next.value.byteLength > ARCHIVE_V2_MAX_BYTES - total) {
          controller.abort()
          void reader.cancel().catch(() => undefined)
          return archiveTooLargeFailure()
        }
        chunks.push(next.value)
        total += next.value.byteLength
      }

      const bytes = new Uint8Array(total)
      let offset = 0
      for (const chunk of chunks) {
        bytes.set(chunk, offset)
        offset += chunk.byteLength
      }
      return ok(bytes)
    } finally {
      reader.releaseLock()
    }
  }

  /**
   * Download and verify a selected v2 archive. No Blob is constructed until
   * headers, the full byte stream, JSON/schema/count/checksum validation, and
   * the active identity lease have all passed.
   */
  async downloadArchiveV2(
    selection: ArchiveV2Selection = fullArchiveV2Selection,
  ): Promise<ApiResult<Blob>> {
    const identity = this.identity
    if (!identity) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private archive download requires an authoritative identity lease',
      })
    }
    let sections: string
    try {
      sections = archiveV2Sections(selection)
    } catch (e) {
      return archiveValidationFailure(e)
    }
    const path = `/api/export/v2?sections=${sections}`
    const ownership = identity.capture(`GET ${path}`)
    if (!identity.isCurrent(ownership)) {
      return err({
        kind: 'identity-mismatch',
        message: 'Private archive download belongs to a stale identity epoch',
      })
    }
    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    if (ownership.signal.aborted) controller.abort()
    else ownership.signal.addEventListener('abort', abortForIdentity, { once: true })
    const timer = setTimeout(() => controller.abort(), this.timeoutMs)
    const cancellationFailure = (message: string): ApiResult<never> | null => {
      if (!identity.isCurrent(ownership)) {
        return err({ kind: 'identity-mismatch', message })
      }
      if (controller.signal.aborted) {
        return err(normalizeThrown(new DOMException('aborted', 'AbortError'), this.timeoutMs))
      }
      return null
    }
    const auth = this.authInit()
    const headers: Record<string, string> = { Accept: 'application/json', ...auth.headers }
    try {
      let response: Response
      try {
        response = await fetch(`${this.baseURL}${path}`, {
          ...auth.init,
          method: 'GET',
          headers,
          signal: controller.signal,
        })
      } catch (e) {
        if (!identity.isCurrent(ownership)) {
          return err({
            kind: 'identity-mismatch',
            message: 'Authenticated archive request was cancelled by an identity change',
          })
        }
        return err(normalizeThrown(e, this.timeoutMs))
      }

      const postFetchCancellation = cancellationFailure(
        'Authenticated archive request was cancelled by an identity change',
      )
      if (postFetchCancellation) return postFetchCancellation

      const headerOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (headerOwnershipFailure) return headerOwnershipFailure

      const bodyResult = await this.readArchiveV2Bytes(
        response,
        identity,
        ownership,
        controller,
      )
      if (!bodyResult.ok) return bodyResult

      const postStreamCancellation = cancellationFailure(
        'Authenticated archive stream was cancelled by an identity change',
      )
      if (postStreamCancellation) return postStreamCancellation
      const postStreamOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (postStreamOwnershipFailure) return postStreamOwnershipFailure

      if (!response.ok) {
        let body: unknown = null
        const text = new TextDecoder().decode(bodyResult.data)
        if (text) {
          try {
            body = JSON.parse(text)
          } catch {
            // A non-JSON error body still maps deterministically by status.
          }
        }
        return err(normalizeHttpError(
          response.status,
          body,
          response.status === 429 ? parseRetryAfter(response.headers) : undefined,
        ))
      }

      try {
        await validateArchiveV2Bytes(bodyResult.data, {
          clientDataNamespace: ownership.serverClientDataNamespace,
          selection,
        })
      } catch (e) {
        return archiveValidationFailure(e)
      }

      const postValidationCancellation = cancellationFailure(
        'Authenticated archive validation was cancelled by an identity change',
      )
      if (postValidationCancellation) return postValidationCancellation
      const postValidationOwnershipFailure = this.validateResponseOwnership(response, ownership)
      if (postValidationOwnershipFailure) return postValidationOwnershipFailure

      const immediatelyBeforeSuccessCancellation = cancellationFailure(
        'Authenticated archive validation was cancelled by an identity change',
      )
      if (immediatelyBeforeSuccessCancellation) return immediatelyBeforeSuccessCancellation
      const immediatelyBeforeSuccess = this.validateResponseOwnership(response, ownership)
      if (immediatelyBeforeSuccess) return immediatelyBeforeSuccess
      try {
        const blobBytes = new Uint8Array(bodyResult.data.byteLength)
        blobBytes.set(bodyResult.data)
        return ok(new Blob([blobBytes.buffer], { type: ARCHIVE_V2_MIME_TYPE }))
      } catch (e) {
        return archiveValidationFailure(e)
      }
    } finally {
      clearTimeout(timer)
      ownership.signal.removeEventListener('abort', abortForIdentity)
    }
  }
}
