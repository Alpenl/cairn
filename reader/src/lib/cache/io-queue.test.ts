import { describe, expect, it } from 'vitest'

import { IdentityAuthority } from '../identity'
import { NamespaceStorageQueue } from './io-queue'

describe('namespace storage queue', () => {
  it('serializes one namespace, lets another progress, and fences a revoked epoch', async () => {
    const authority = new IdentityAuthority()
    const queue = new NamespaceStorageQueue()
    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    let releaseA!: () => void
    const barrierA = new Promise<void>((resolve) => {
      releaseA = resolve
    })
    const events: string[] = []

    const firstA = queue.enqueue(leaseA, 'A:first', async (operation) => {
      events.push('A:first:start')
      await barrierA
      return operation.commit(() => events.push('A:first:commit'))
    })
    const secondA = queue.enqueue(leaseA, 'A:second', async (operation) => {
      events.push('A:second:start')
      return operation.commit(() => events.push('A:second:commit'))
    })
    await Promise.resolve()
    expect(events).toEqual(['A:first:start'])

    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    await queue.enqueue(leaseB, 'B:first', async (operation) =>
      operation.commit(() => events.push('B:first:commit')),
    )
    expect(events).toEqual(['A:first:start', 'B:first:commit'])

    releaseA()
    await Promise.all([firstA, secondA])
    expect(events).toEqual(['A:first:start', 'B:first:commit'])
  })
})
