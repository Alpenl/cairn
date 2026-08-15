import { describe, expect, it } from 'vitest'
import type { CapabilitiesResponse } from './types'
import {
  CaptureActivationLease,
  captureOwnersEqual,
  createCaptureOwnerFingerprint,
  createCaptureRecoveryKey,
  deriveExtensionCapabilityPolicy,
  ExtensionCapabilityLease,
} from './capabilities'

function capabilities(
  libraryKinds: unknown,
  siteLibrary: unknown,
  siteManagement: unknown,
  inbox: unknown = false,
): CapabilitiesResponse {
  return {
    library_kinds: libraryKinds,
    site_library: siteLibrary,
    site_management: siteManagement,
    reader: { inbox },
  } as unknown as CapabilitiesResponse
}

describe('Extension capability policy', () => {
  it.each([
    [false, false, false, false],
    [false, false, true, false],
    [false, true, false, false],
    [false, true, true, false],
    [true, false, false, false],
    [true, false, true, false],
    [true, true, false, false],
    [true, true, true, true],
  ] as const)(
    'maps library_kinds=%s site_library=%s site_management=%s to site=%s',
    (libraryKinds, siteLibrary, siteManagement, expected) => {
      expect(
        deriveExtensionCapabilityPolicy(
          capabilities(libraryKinds, siteLibrary, siteManagement),
        ).site,
      ).toBe(expected)
    },
  )

  it('requires a strict true Inbox field and fails closed without a response', () => {
    expect(
      deriveExtensionCapabilityPolicy(capabilities(true, true, true, true))
        .inbox,
    ).toBe(true)
    expect(
      deriveExtensionCapabilityPolicy(capabilities(true, true, true, 'true'))
        .inbox,
    ).toBe(false)
    expect(deriveExtensionCapabilityPolicy(undefined)).toEqual({
      inbox: false,
      site: false,
    })
  })

  it('revokes every feature from an old generation', () => {
    const lease = new ExtensionCapabilityLease({ inbox: true, site: true })
    expect(lease.isCurrent('inbox')).toBe(true)
    expect(lease.isCurrent('site')).toBe(true)

    lease.revoke()

    expect(lease.isCurrent()).toBe(false)
    expect(lease.isCurrent('inbox')).toBe(false)
    expect(lease.isCurrent('site')).toBe(false)
  })
})

describe('Capture owner identity', () => {
  it('creates a stable non-reversible fingerprint from origin and session identity', async () => {
    const identity = {
      client_data_namespace: 'raw-private-namespace',
      representation_contract: 'v3' as const,
    }
    const first = await createCaptureOwnerFingerprint(
      'https://api.example.test/private/path',
      identity,
    )
    const sameOrigin = await createCaptureOwnerFingerprint(
      'https://api.example.test/another/path',
      identity,
    )
    const otherNamespace = await createCaptureOwnerFingerprint(
      'https://api.example.test',
      { ...identity, client_data_namespace: 'another-namespace' },
    )

    expect(first).toMatch(/^[a-f0-9]{64}$/)
    expect(first).toBe(sameOrigin)
    expect(first).not.toBe(otherNamespace)
    expect(first).not.toContain(identity.client_data_namespace)
    expect(first).not.toContain('/private/path')
  })

  it('compares both fingerprint and activation revision', () => {
    expect(
      captureOwnersEqual(
        { fingerprint: 'a'.repeat(64), revision: 3 },
        { fingerprint: 'a'.repeat(64), revision: 3 },
      ),
    ).toBe(true)
    expect(
      captureOwnersEqual(
        { fingerprint: 'a'.repeat(64), revision: 3 },
        { fingerprint: 'a'.repeat(64), revision: 4 },
      ),
    ).toBe(false)
  })

  it('derives a non-extractable recovery key without exposing the token', async () => {
    const owner = { fingerprint: 'a'.repeat(64), revision: 3 }
    const key = await createCaptureRecoveryKey('private-recovery-token', owner)

    expect(key.algorithm.name).toBe('AES-GCM')
    expect(key.extractable).toBe(false)
    expect(key.usages).toEqual(['encrypt', 'decrypt'])
    await expect(crypto.subtle.exportKey('raw', key)).rejects.toBeDefined()
  })

  it('rejects blank and short recovery secrets instead of deriving weak keys', async () => {
    const owner = { fingerprint: 'a'.repeat(64), revision: 3 }

    await expect(createCaptureRecoveryKey('   ', owner)).rejects.toThrow(
      'at least 16',
    )
    await expect(
      createCaptureRecoveryKey('short-token', owner),
    ).rejects.toThrow('at least 16')
  })

  it('revokes an activation lease and fails closed when its check throws', async () => {
    const owner = { fingerprint: 'a'.repeat(64), revision: 1 }
    const lease = new CaptureActivationLease(owner, () => true)
    await expect(lease.isCurrent()).resolves.toBe(true)
    lease.revoke()
    await expect(lease.isCurrent()).resolves.toBe(false)

    const failing = new CaptureActivationLease(owner, () => {
      throw new Error('storage unavailable')
    })
    await expect(failing.isCurrent()).resolves.toBe(false)
  })

  it('stays revoked when an async current check resolves afterward', async () => {
    const owner = { fingerprint: 'a'.repeat(64), revision: 1 }
    let resolveCheck!: (current: boolean) => void
    const pendingCheck = new Promise<boolean>((resolve) => {
      resolveCheck = resolve
    })
    const lease = new CaptureActivationLease(owner, () => pendingCheck)

    const result = lease.isCurrent()
    lease.revoke()
    resolveCheck(true)

    await expect(result).resolves.toBe(false)
  })
})
