import { ReaderClient } from './api/client'
import type { ApiError } from '@webtag/api'
import type { SessionIdentity } from './api/types'
import { withBrowserLock } from './browser-lock'

export type SessionNegotiationOutcome =
  | {
      readonly kind: 'session'
      readonly identity: SessionIdentity
      readonly retained: boolean
    }
  | { readonly kind: 'unsupported' }
  | { readonly kind: 'cookie-unavailable' }
  | { readonly kind: 'error'; readonly error: ApiError }

export interface SessionNegotiationOptions {
  readonly baseURL: string
  readonly installationToken: string
  readonly expectedClientDataNamespace?: string
  readonly signal?: AbortSignal
  /** Return false when a revision check proves this attempt has been superseded. */
  readonly commit?: (identity: SessionIdentity) => Promise<boolean | void>
}

function normalizedBaseURL(value: string): string {
  return value.trim().replace(/\/+$/, '')
}

function sessionMutationLockName(baseURL: string): string {
  const normalized = normalizedBaseURL(baseURL)
  try {
    return `webtag-reader:session-mutation:${new URL(normalized).origin}`
  } catch {
    return `webtag-reader:session-mutation:${normalized}`
  }
}

function failure(kind: ApiError['kind'], message: string): SessionNegotiationOutcome {
  return { kind: 'error', error: { kind, message } }
}

function cancelled(): SessionNegotiationOutcome {
  return failure('other', 'Session negotiation was cancelled')
}

function identityMismatch(message: string): SessionNegotiationOutcome {
  return failure('identity-mismatch', message)
}

/**
 * Own the origin's session cookie from exchange through durable commit or cleanup.
 * No successor exchange can begin before a failed attempt's DELETE has completed.
 */
export async function negotiateSession(
  options: SessionNegotiationOptions,
): Promise<SessionNegotiationOutcome> {
  const baseURL = normalizedBaseURL(options.baseURL)
  return withBrowserLock(sessionMutationLockName(baseURL), async () => {
    if (options.signal?.aborted) return cancelled()

    const client = new ReaderClient({ baseURL, session: true })
    const attempt = await client.loginWithMutationStatus(
      options.installationToken.trim(),
      options.signal,
    )
    const cleanup = async () => {
      if (attempt.sessionMayExist) await client.logout()
    }

    if (!attempt.result.ok) {
      if (attempt.result.error.status === 404) return { kind: 'unsupported' }
      await cleanup()
      return { kind: 'error', error: attempt.result.error }
    }
    if (options.signal?.aborted) {
      await cleanup()
      return cancelled()
    }

    const expectedNamespace = options.expectedClientDataNamespace
    if (
      expectedNamespace !== undefined &&
      attempt.result.data.clientDataNamespace !== expectedNamespace
    ) {
      await cleanup()
      return identityMismatch('Session exchange identity does not match the verified installation')
    }

    const cookieIdentity = await client.getIdentity(options.signal)
    if (!cookieIdentity.ok) {
      await cleanup()
      if (cookieIdentity.error.kind === 'unauthorized' && cookieIdentity.error.status === 401) {
        return { kind: 'cookie-unavailable' }
      }
      return { kind: 'error', error: cookieIdentity.error }
    }
    if (
      cookieIdentity.data.client_data_namespace !== attempt.result.data.clientDataNamespace ||
      (expectedNamespace !== undefined &&
        cookieIdentity.data.client_data_namespace !== expectedNamespace)
    ) {
      await cleanup()
      return identityMismatch('Session cookie identity does not match the verified installation')
    }
    if (options.signal?.aborted) {
      await cleanup()
      return cancelled()
    }

    if (options.commit) {
      try {
        const committed = await options.commit(cookieIdentity.data)
        if (committed === false) {
          await cleanup()
          return identityMismatch('Session negotiation was superseded by a newer connection')
        }
      } catch (error) {
        await cleanup()
        return failure(
          'other',
          error instanceof Error ? error.message : 'Unable to persist the negotiated session',
        )
      }
    }

    const retained = options.commit !== undefined
    if (!retained) await cleanup()
    return { kind: 'session', identity: cookieIdentity.data, retained }
  })
}
