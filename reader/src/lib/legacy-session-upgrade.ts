import type { ApiError } from '@webtag/api'
import type { SessionIdentity } from './api/types'
import { negotiateSession } from './session-negotiation'
import type { Connection } from './settings'

export type LegacySessionUpgradeOutcome =
  | { readonly kind: 'session' }
  | { readonly kind: 'bearer-compatibility' }
  | { readonly kind: 'error'; readonly error: ApiError }

export interface LegacySessionUpgradeOptions {
  readonly signal?: AbortSignal
  readonly commitSession: () => Promise<boolean>
}

/**
 * Upgrade a previously persisted installation token only after Bearer, exchange,
 * and cookie authentication all prove they belong to the same installation.
 */
export async function negotiateLegacySessionUpgrade(
  connection: Connection,
  bearerIdentity: SessionIdentity,
  options: LegacySessionUpgradeOptions,
): Promise<LegacySessionUpgradeOutcome> {
  const outcome = await negotiateSession({
    baseURL: connection.baseURL,
    installationToken: connection.installationToken,
    expectedClientDataNamespace: bearerIdentity.client_data_namespace,
    signal: options.signal,
    commit: options.commitSession,
  })
  if (outcome.kind === 'unsupported' || outcome.kind === 'cookie-unavailable') {
    return { kind: 'bearer-compatibility' }
  }
  if (outcome.kind === 'error') return outcome
  return { kind: 'session' }
}
