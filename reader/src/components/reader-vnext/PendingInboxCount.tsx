import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderCapabilityLease } from '../../lib/capabilities'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../../lib/reader-events'

const PendingInboxCountContext = createContext<number | null>(null)

// eslint-disable-next-line react-refresh/only-export-components
export function refreshPendingInboxCount(): void {
  emitReaderEvent(READER_EVENTS.pendingInboxChanged)
}

async function readPendingCount(
  client: IdentityBoundReaderClient,
  capabilityLease: ReaderCapabilityLease,
): Promise<number | null> {
  if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('inbox')) return null
  const page = await client.listInbox({ partition: 'active', limit: 1 })
  if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('inbox') || !page.ok) return null
  return page.data.active_count
}

export function PendingInboxCountProvider({ client, capabilityLease, children }: { readonly client: IdentityBoundReaderClient; readonly capabilityLease: ReaderCapabilityLease; readonly children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<{
    readonly client: IdentityBoundReaderClient
    readonly count: number | null
  }>({ client, count: null })

  useEffect(() => {
    let active = true
    let generation = 0
    const refresh = () => {
      const requestGeneration = ++generation
      void readPendingCount(client, capabilityLease).then((next) => {
        if (active && requestGeneration === generation && client.isIdentityCurrent() && capabilityLease.isCurrent('inbox')) {
          setSnapshot({ client, count: next })
        }
      })
    }
    refresh()
    const unsubscribe = subscribeReaderEvents([READER_EVENTS.pendingInboxChanged], refresh)
    return () => {
      active = false
      unsubscribe()
    }
  }, [capabilityLease, client])

  const count = snapshot.client === client ? snapshot.count : null
  return <PendingInboxCountContext.Provider value={count}>{children}</PendingInboxCountContext.Provider>
}

// eslint-disable-next-line react-refresh/only-export-components
export function usePendingInboxCount(): number | null {
  return useContext(PendingInboxCountContext)
}
