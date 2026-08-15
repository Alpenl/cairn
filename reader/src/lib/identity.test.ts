import { describe, expect, it } from 'vitest'

import {
  derivePhysicalNamespace,
  IdentityAuthority,
  IdentityLease,
  type IdentityOwnership,
} from './identity'

describe('Reader physical namespace protocol', () => {
  it('binds the canonical backend URL and authoritative server namespace to the fixed protocol vector', async () => {
    await expect(
      derivePhysicalNamespace(' HTTPS://Example.COM:443/// ', 'server-namespace-A'),
    ).resolves.toBe('_-vQ_BiQ_JBUk9tTvVD9W2PTQsr2pUWEYa_RCnqSMZE')
  })

  it('maps equivalent backend URL spellings to the same physical namespace', async () => {
    const [spelled, canonical] = await Promise.all([
      derivePhysicalNamespace(' HTTPS://Example.COM:443/// ', 'server-namespace-A'),
      derivePhysicalNamespace('https://example.com', 'server-namespace-A'),
    ])

    expect(spelled).toBe(canonical)
  })

  it.each([
    { name: 'scheme', url: 'http://example.com' },
    { name: 'host', url: 'https://other.example.com' },
    { name: 'non-default port', url: 'https://example.com:8443' },
  ])('separates the same server namespace when the canonical backend $name changes', async ({ url }) => {
    const [reference, changed] = await Promise.all([
      derivePhysicalNamespace('https://example.com', 'server-namespace-A'),
      derivePhysicalNamespace(url, 'server-namespace-A'),
    ])

    expect(changed).not.toBe(reference)
  })
})

describe('Reader identity ownership', () => {
  it('requires ownership capabilities to be minted by their lease', () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })
    // @ts-expect-error The module-private brand prevents structural ownership forgery.
    const forged: IdentityOwnership = {
      lease,
      operation: lease.capture('forged ownership'),
    }

    expect(lease.isOwnershipCurrent(forged)).toBe(false)
    expect(lease.isOwnershipCurrent(lease.captureOwnership('minted ownership'))).toBe(true)
  })

  it('keeps a minted ownership capability bound to both its lease and operation', () => {
    const context = {
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    }
    const owner = new IdentityLease(context)
    const other = new IdentityLease(context)
    const ownership = owner.captureOwnership('owned operation')
    const replacedLease: IdentityOwnership = {
      ...ownership,
      lease: other,
    }
    const replacedOperation: IdentityOwnership = {
      ...ownership,
      operation: other.capture('foreign operation'),
    }

    expect(owner.isOwnershipCurrent(replacedLease)).toBe(false)
    expect(owner.isOwnershipCurrent(replacedOperation)).toBe(false)
  })

  it('rejects an operation captured by another lease with identical public context', () => {
    const context = {
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    }
    const first = new IdentityLease(context)
    const second = new IdentityLease(context)

    expect(first.isCurrent(second.capture('foreign operation'))).toBe(false)
  })

  it('invalidates and aborts work captured by the previous identity epoch', () => {
    const authority = new IdentityAuthority()
    const first = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const captured = first.capture('GET /api/links')

    const second = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })

    expect(first.isCurrent(captured)).toBe(false)
    expect(captured.signal.aborted).toBe(true)
    expect(second.context.localEpoch).toBeGreaterThan(captured.localEpoch)
  })
})
