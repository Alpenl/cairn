import type { IdentityLease } from '../identity'
import { readOwnedStorageForLease, writeOwnedStorageForLease } from '../storage-ownership'
import {
  MAX_THOUGHT_LOGICAL_CLOCK,
  isValidThoughtIdentifier,
} from './thought-types'

export type ThoughtClockErrorCode = 'invalid-thought-clock' | 'thought-clock-exhausted'

export class ThoughtClockError extends Error {
  constructor(readonly code: ThoughtClockErrorCode, message: string) {
    super(message)
    this.name = 'ThoughtClockError'
  }
}

export function randomThoughtToken(prefix: string): string {
  try {
    if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
      return `${prefix}-${crypto.randomUUID()}`
    }
  } catch {
    // Test and legacy browser fallback.
  }
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
}

export function stableThoughtDeviceID(lease: IdentityLease, state?: unknown): string {
  const stored = readOwnedStorageForLease('thoughtDevice', lease)?.trim()
  if (isValidThoughtIdentifier(stored)) return stored
  if (state && typeof state === 'object' && !Array.isArray(state)) {
    const candidate = (state as { readonly deviceId?: unknown }).deviceId
    if (isValidThoughtIdentifier(candidate)) {
      writeOwnedStorageForLease('thoughtDevice', candidate, lease)
      return candidate
    }
  }
  const deviceID = randomThoughtToken('device')
  writeOwnedStorageForLease('thoughtDevice', deviceID, lease)
  return deviceID
}

export function maximumThoughtClock(values: readonly number[]): number {
  let maximum = 0
  for (const value of values) {
    if (!Number.isSafeInteger(value) || value < 0 || value > MAX_THOUGHT_LOGICAL_CLOCK) {
      throw new ThoughtClockError('invalid-thought-clock', 'Observed logical clock is invalid')
    }
    maximum = Math.max(maximum, value)
  }
  return maximum
}

export function nextThoughtLogicalClock(values: readonly number[]): number {
  const floor = maximumThoughtClock(values)
  if (floor === MAX_THOUGHT_LOGICAL_CLOCK) {
    throw new ThoughtClockError('thought-clock-exhausted', 'Thought logical clock is exhausted')
  }
  return floor + 1
}
