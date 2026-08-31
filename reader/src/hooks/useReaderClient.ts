import { useSyncExternalStore } from 'react'
import { resourceStore } from '../lib/cache/store'
import type { ReaderAmbientClientPort } from '../lib/reader-api-ports'

/**
 * MainView owns the authenticated client, while the reading surface
 * components are intentionally presentational siblings.  Keep one small
 * identity-bound hand-off here instead of making every surface carry a copy
 * of the client through unrelated props.
 *
 * The registry is only a runtime reference.  It never stores response data or
 * credentials, and a client is usable only while its lease owns the active
 * resource-store partition.
 */
let registeredClient: ReaderAmbientClientPort | null = null
const listeners = new Set<() => void>()

function notify(): void {
  for (const listener of [...listeners]) listener()
}

function isCurrentClient(client: ReaderAmbientClientPort | null): client is ReaderAmbientClientPort {
  if (!client) return false
  try {
    const lease = client.identityLease
    return Boolean(
      lease &&
      client.isIdentityCurrent() &&
      resourceStore.isIdentityActive(lease),
    )
  } catch {
    return false
  }
}

/** Register the client already owned by the mounted MainView. */
export function registerReaderClient(client: ReaderAmbientClientPort): () => void {
  registeredClient = client
  notify()
  return () => {
    if (registeredClient !== client) return
    registeredClient = null
    notify()
  }
}

function getRegisteredClient(): ReaderAmbientClientPort | null {
  return isCurrentClient(registeredClient) ? registeredClient : null
}

/**
 * Explicit clients are useful for isolated surfaces and tests.  Production
 * callers use the registered client and therefore get the active identity
 * partition check above.
 */
export function useReaderClient(
  explicitClient?: ReaderAmbientClientPort,
): ReaderAmbientClientPort | null {
  const registered = useSyncExternalStore(
    (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    getRegisteredClient,
    () => null,
  )
  return explicitClient ?? registered
}
