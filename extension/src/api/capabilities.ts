import type { CapabilitiesResponse, SessionIdentity } from './types'

export interface ExtensionCapabilityPolicy {
  readonly inbox: boolean
  readonly site: boolean
}

export type ExtensionCapabilityFeature = keyof ExtensionCapabilityPolicy

/** Durable, non-secret identity attached to capture state. */
export interface CaptureOwner {
  /** SHA-256(origin, namespace, representation contract); no raw identity data. */
  readonly fingerprint: string
  /** Monotonic revision of the explicitly activated connection. */
  readonly revision: number
}

export function captureOwnersEqual(
  left: CaptureOwner,
  right: CaptureOwner,
): boolean {
  return (
    left.fingerprint === right.fingerprint && left.revision === right.revision
  )
}

/** Build a one-way owner identifier without retaining a token or raw namespace. */
export async function createCaptureOwnerFingerprint(
  backendUrl: string,
  identity: SessionIdentity,
): Promise<string> {
  const origin = new URL(backendUrl).origin
  const material = JSON.stringify([
    origin,
    identity.client_data_namespace,
    identity.representation_contract,
  ])
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(material),
  )
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, '0'),
  ).join('')
}

/** Conservative floor below which token-derived recovery is disabled. */
export const MIN_CAPTURE_RECOVERY_SECRET_LENGTH = 16

export function isCaptureRecoverySecretEligible(accessToken: string): boolean {
  return accessToken.trim().length >= MIN_CAPTURE_RECOVERY_SECRET_LENGTH
}

/**
 * Derive a non-extractable session-persistence key from the confirmed secret
 * and capture owner. The token is never retained in capture state or key bytes.
 */
export async function createCaptureRecoveryKey(
  accessToken: string,
  owner: CaptureOwner,
): Promise<CryptoKey> {
  const secret = accessToken.trim()
  if (!isCaptureRecoverySecretEligible(secret)) {
    throw new Error(
      `capture recovery requires at least ${MIN_CAPTURE_RECOVERY_SECRET_LENGTH} secret characters`,
    )
  }
  const material = JSON.stringify([
    'webtag-capture-recovery-v1',
    secret,
    owner.fingerprint,
    owner.revision,
  ])
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(material),
  )
  return crypto.subtle.importKey('raw', digest, { name: 'AES-GCM' }, false, [
    'encrypt',
    'decrypt',
  ])
}

/** Revocable activation checked before each capture continuation. */
export class CaptureActivationLease {
  private active = true

  constructor(
    readonly owner: CaptureOwner,
    private readonly checkCurrent: () => boolean | Promise<boolean>,
  ) {}

  async isCurrent(): Promise<boolean> {
    if (!this.active) return false
    try {
      const current = await this.checkCurrent()
      return this.active && current
    } catch {
      return false
    }
  }

  revoke(): void {
    this.active = false
  }
}

export const DISABLED_EXTENSION_CAPABILITY_POLICY: ExtensionCapabilityPolicy =
  Object.freeze({ inbox: false, site: false })

export function deriveExtensionCapabilityPolicy(
  capabilities: CapabilitiesResponse | null | undefined,
): ExtensionCapabilityPolicy {
  return {
    inbox: capabilities?.reader?.inbox === true,
    site:
      capabilities?.library_kinds === true &&
      capabilities?.site_library === true &&
      capabilities?.site_management === true,
  }
}

let nextCapabilityGeneration = 1

export class ExtensionCapabilityLease {
  readonly generation = nextCapabilityGeneration++
  private active = true

  constructor(readonly policy: ExtensionCapabilityPolicy) {}

  isCurrent(feature?: ExtensionCapabilityFeature): boolean {
    return this.active && (feature === undefined || this.policy[feature])
  }

  revoke(): void {
    this.active = false
  }
}
