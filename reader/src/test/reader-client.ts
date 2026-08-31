import { vi } from 'vitest'

import type { IdentityBoundReaderClient, ReaderClient } from '../lib/api/client'
import { IdentityLease, readerIdentity, type IdentityContext } from '../lib/identity'

export interface TestReaderClientOptions {
  readonly lease?: IdentityLease
  readonly isIdentityCurrent?: () => boolean
}

export function makeTestIdentityLease(
  prefix = 'reader-test',
  localEpoch = 1,
  overrides: Partial<IdentityContext> = {},
): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `${prefix}-server`,
    physicalNamespace: `${prefix}-physical`,
    localEpoch,
    ...overrides,
  })
}

export function bindReaderClient<T extends object>(
  client: T,
  options: TestReaderClientOptions = {},
): T & IdentityBoundReaderClient {
  const lease = options.lease ?? readerIdentity.activeLease ?? makeTestIdentityLease()
  const existingCurrent = (client as { isIdentityCurrent?: () => boolean }).isIdentityCurrent
  const isIdentityCurrent = existingCurrent ?? vi.fn(options.isIdentityCurrent ?? (() => true))

  Object.defineProperty(client, 'identityLease', {
    configurable: true,
    writable: true,
    value: lease,
  })
  Object.defineProperty(client, 'isIdentityCurrent', {
    configurable: true,
    writable: true,
    value: isIdentityCurrent,
  })
  Object.defineProperty(client, 'captureIdentity', {
    configurable: true,
    writable: true,
    value: vi.fn((logicalKey: string) => {
      if (!isIdentityCurrent.call(client)) return null
      const ownership = lease.captureOwnership(logicalKey)
      return lease.isOwnershipCurrent(ownership) ? ownership : null
    }),
  })

  return client as T & IdentityBoundReaderClient
}

export function makeReaderClient<T extends object>(
  methods: T,
  options: TestReaderClientOptions = {},
): T & IdentityBoundReaderClient {
  return bindReaderClient({ ...methods }, options)
}

export function bindLegacyReaderClient<T extends object>(
  client: T,
  options: TestReaderClientOptions = {},
): T & ReaderClient {
  return bindReaderClient(client, options) as T & ReaderClient
}
