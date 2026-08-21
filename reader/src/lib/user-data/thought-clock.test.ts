import { describe, expect, it } from 'vitest'

import {
  ThoughtClockError,
  maximumThoughtClock,
  nextThoughtLogicalClock,
} from './thought-clock'
import {
  MAX_THOUGHT_LOGICAL_CLOCK,
  compareThoughtVersionKeys,
  isValidThoughtIdentifier,
  isValidThoughtVersionKey,
  type ThoughtVersionKey,
} from './thought-types'

describe('thought Lamport clock contract', () => {
  it('starts at one and advances only from the largest observed clock', () => {
    expect(nextThoughtLogicalClock([])).toBe(1)
    expect(nextThoughtLogicalClock([0])).toBe(1)
    expect(nextThoughtLogicalClock([41])).toBe(42)
    expect(nextThoughtLogicalClock([42, 9])).toBe(43)
    expect(maximumThoughtClock([3, 20, 7])).toBe(20)
  })

  it('fails closed for invalid and exhausted clocks', () => {
    for (const clock of [-1, 1.5, Number.POSITIVE_INFINITY]) {
      expect(() => maximumThoughtClock([clock])).toThrow(ThoughtClockError)
    }
    expect(() => nextThoughtLogicalClock([MAX_THOUGHT_LOGICAL_CLOCK])).toThrowError(
      expect.objectContaining({ code: 'thought-clock-exhausted' }),
    )
  })

  it('validates canonical identifiers and positive version keys', () => {
    expect(isValidThoughtIdentifier('device-a')).toBe(true)
    expect(isValidThoughtIdentifier(' device-a')).toBe(false)
    expect(isValidThoughtIdentifier('device\0a')).toBe(false)
    expect(isValidThoughtIdentifier('\ud800')).toBe(false)
    expect(isValidThoughtIdentifier('a'.repeat(129))).toBe(false)
    expect(isValidThoughtVersionKey({
      logicalClock: 1,
      deviceId: 'device',
      opId: 'op',
    })).toBe(true)
    expect(isValidThoughtVersionKey({
      logicalClock: 0,
      deviceId: 'device',
      opId: 'op',
    })).toBe(false)
  })

  it('orders equal clocks by canonical UTF-8 bytes and then operation id', () => {
    const keys: ThoughtVersionKey[] = [
      { logicalClock: 7, deviceId: 'device-\u{10000}', opId: 'op-a' },
      { logicalClock: 7, deviceId: 'device-\ue000', opId: 'op-z' },
      { logicalClock: 7, deviceId: 'device-\u{10000}', opId: 'op-b' },
      { logicalClock: 6, deviceId: 'device-z', opId: 'op-z' },
    ]

    expect([...keys].sort(compareThoughtVersionKeys)).toEqual([
      keys[3],
      keys[1],
      keys[0],
      keys[2],
    ])
    expect(keys[0].deviceId < keys[1].deviceId).toBe(true)
    expect(compareThoughtVersionKeys(keys[0], keys[1])).toBeGreaterThan(0)
  })
})
