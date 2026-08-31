import { useEffect, useState, type ReactNode } from 'react'
import type { ReaderCapabilityLease } from '../../lib/capabilities'
import { READER_EVENTS, subscribeReaderEvents } from '../../lib/reader-events'
import type { ReaderInboxTodosPort } from '../../lib/reader-api-ports'
import { PendingInboxCountContext } from './pending-inbox-count-context'

async function readPendingCount(
  client: Pick<ReaderInboxTodosPort, 'listInbox' | 'isIdentityCurrent'>,
  capabilityLease: ReaderCapabilityLease,
): Promise<number | null> {
  if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('inbox')) return null
  const page = await client.listInbox({ partition: 'active', limit: 1 })
  if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('inbox') || !page.ok) return null
  return page.data.active_count
}

type PendingInboxCountClient = Pick<ReaderInboxTodosPort, 'listInbox' | 'isIdentityCurrent'>

export function PendingInboxCountProvider({ client, capabilityLease, children }: { readonly client: PendingInboxCountClient; readonly capabilityLease: ReaderCapabilityLease; readonly children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<{
    readonly client: PendingInboxCountClient
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
