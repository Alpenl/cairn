import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ReaderClient } from '../lib/api/client'
import type { IdentityLease } from '../lib/identity'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { useIdentityBoundOperationGate } from './identityBoundOperation'
import { useIdentityPolling } from './useIdentityPolling'

function clientFor(
  lease: IdentityLease,
  current: () => boolean = () => true,
): ReaderClient {
  return {
    identityLease: lease,
    isIdentityCurrent: vi.fn(() => current()),
    captureIdentity: vi.fn((logicalKey: string) => {
      if (!current()) return null
      const ownership = lease.captureOwnership(logicalKey)
      return lease.isOwnershipCurrent(ownership) ? ownership : null
    }),
  } as unknown as ReaderClient
}

describe('identity-bound operation gate', () => {
  it('拒绝旧 owner 写入，也不让旧 finally 释放 replacement owner', () => {
    const leaseA = readerIdentity.activeLease!
    const clientA = clientFor(leaseA)
    const hook = renderHook(
      ({ client, ownerKey }) => useIdentityBoundOperationGate<'load'>(client, [ownerKey]),
      { initialProps: { client: clientA, ownerKey: 'A' } },
    )
    const old = hook.result.current.begin('load', 'load A')
    expect(old).not.toBeNull()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'identity-gate-B',
      physicalNamespace: 'identity-gate-B',
    })
    resourceStore.activateIdentity(leaseB)
    const clientB = clientFor(leaseB)
    hook.rerender({ client: clientB, ownerKey: 'B' })

    const write = vi.fn()
    const release = vi.fn()
    hook.result.current.commit(old!, write)
    hook.result.current.finish(old!, release)

    expect(write).not.toHaveBeenCalled()
    expect(release).not.toHaveBeenCalled()
  })

  it('同一 owner 的 finally 可在身份失效后释放自己占用的 busy', () => {
    let current = true
    const lease = readerIdentity.activeLease!
    const client = clientFor(lease, () => current)
    const { result } = renderHook(() => useIdentityBoundOperationGate<'load'>(client, ['same']))
    const token = result.current.begin('load', 'load same owner')
    expect(token).not.toBeNull()

    current = false
    const write = vi.fn()
    const release = vi.fn()
    result.current.commit(token!, write)
    result.current.finish(token!, release)

    expect(write).not.toHaveBeenCalled()
    expect(release).toHaveBeenCalledTimes(1)
  })
})

describe('identity-bound polling', () => {
  it('stops old identity timers until a replacement owner installs its own timer', () => {
    vi.useFakeTimers()
    try {
      const ticks = vi.fn()
      const leaseA = readerIdentity.activeLease!
      const clientA = clientFor(leaseA)
      const hook = renderHook(
        ({ client, ownerKey }) => useIdentityPolling(client, {
          intervalMs: 100,
          logicalKey: 'poll test',
          ownerKey,
          onTick: ticks,
        }),
        { initialProps: { client: clientA, ownerKey: 'A' } },
      )

      act(() => { vi.advanceTimersByTime(100) })
      expect(ticks).toHaveBeenCalledTimes(1)

      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'identity-poll-B',
        physicalNamespace: 'identity-poll-B',
      })
      resourceStore.activateIdentity(leaseB)
      act(() => { vi.advanceTimersByTime(100) })
      expect(ticks).toHaveBeenCalledTimes(1)

      hook.rerender({ client: clientFor(leaseB), ownerKey: 'B' })
      act(() => { vi.advanceTimersByTime(100) })
      expect(ticks).toHaveBeenCalledTimes(2)
    } finally {
      vi.useRealTimers()
    }
  })
})
