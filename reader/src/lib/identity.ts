const PHYSICAL_NAMESPACE_DOMAIN = 'webtag-reader-namespace-v1'
const OPERATION_LEASE_TOKEN: unique symbol = Symbol('operation lease token')
const OWNERSHIP_LEASE_TOKEN: unique symbol = Symbol('ownership lease token')

export interface IdentityContext {
  readonly serverClientDataNamespace: string
  readonly physicalNamespace: string
  readonly localEpoch: number
}

export interface IdentityOperationContext extends IdentityContext {
  readonly logicalKey: string
  readonly signal: AbortSignal
  readonly [OPERATION_LEASE_TOKEN]: object
}

export interface IdentityOwnership {
  readonly lease: IdentityLease
  readonly operation: IdentityOperationContext
  readonly [OWNERSHIP_LEASE_TOKEN]: object
}

export class IdentityLease {
  readonly context: IdentityContext
  private readonly controller = new AbortController()
  private readonly leaseToken = Object.freeze({})
  private active = true

  constructor(context: IdentityContext) {
    this.context = Object.freeze({ ...context })
  }

  capture(logicalKey: string): IdentityOperationContext {
    return Object.freeze({
      ...this.context,
      logicalKey,
      signal: this.controller.signal,
      [OPERATION_LEASE_TOKEN]: this.leaseToken,
    })
  }

  captureOwnership(logicalKey: string): IdentityOwnership {
    return Object.freeze({
      lease: this,
      operation: this.capture(logicalKey),
      [OWNERSHIP_LEASE_TOKEN]: this.leaseToken,
    })
  }

  isCurrent(operation: IdentityOperationContext): boolean {
    return (
      this.active &&
      operation[OPERATION_LEASE_TOKEN] === this.leaseToken &&
      operation.localEpoch === this.context.localEpoch &&
      operation.physicalNamespace === this.context.physicalNamespace &&
      operation.serverClientDataNamespace === this.context.serverClientDataNamespace
    )
  }

  isOwnershipCurrent(ownership: IdentityOwnership): boolean {
    return (
      ownership.lease === this &&
      ownership[OWNERSHIP_LEASE_TOKEN] === this.leaseToken &&
      this.isCurrent(ownership.operation)
    )
  }

  revoke(): void {
    if (!this.active) return
    this.active = false
    this.controller.abort()
  }
}

export class IdentityAuthority {
  private epoch = 0
  private current: IdentityLease | null = null

  install(identity: Omit<IdentityContext, 'localEpoch'>): IdentityLease {
    this.current?.revoke()
    this.epoch += 1
    const lease = new IdentityLease({ ...identity, localEpoch: this.epoch })
    this.current = lease
    return lease
  }

  clear(): void {
    this.current?.revoke()
    this.current = null
    this.epoch += 1
  }

  get activeLease(): IdentityLease | null {
    return this.current
  }
}

export const readerIdentity = new IdentityAuthority()

export function canonicalizeBaseURL(input: string): string {
  let url: URL
  try {
    url = new URL(input.trim())
  } catch {
    throw new Error('Reader backend URL is invalid')
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new Error('Reader backend URL must use HTTP or HTTPS')
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error('Reader backend URL must not contain credentials, query, or fragment')
  }

  const path = url.pathname.replace(/\/+$/, '')
  return path ? `${url.origin}${path}` : url.origin
}

function encodeBase64URL(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export async function derivePhysicalNamespace(
  baseURL: string,
  serverClientDataNamespace: string,
): Promise<string> {
  if (!serverClientDataNamespace) {
    throw new Error('Server client data namespace is missing')
  }
  if (!globalThis.crypto?.subtle) {
    throw new Error('Web Crypto is unavailable')
  }

  const canonicalBaseURL = canonicalizeBaseURL(baseURL)
  const input = new TextEncoder().encode(
    `${PHYSICAL_NAMESPACE_DOMAIN}\0${canonicalBaseURL}\0${serverClientDataNamespace}`,
  )
  const digest = await globalThis.crypto.subtle.digest('SHA-256', input)
  return encodeBase64URL(new Uint8Array(digest))
}
