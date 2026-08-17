import { describe, expect, it } from 'vitest'
import { asRecord, isRecord } from './records'

describe('isRecord', () => {
  it('accepts plain objects', () => {
    expect(isRecord({})).toBe(true)
    expect(isRecord({ id: 'T1' })).toBe(true)
    expect(isRecord(Object.create(null) as unknown)).toBe(true)
  })

  it('rejects null and primitives', () => {
    expect(isRecord(null)).toBe(false)
    expect(isRecord(undefined)).toBe(false)
    expect(isRecord('todo')).toBe(false)
    expect(isRecord(0)).toBe(false)
    expect(isRecord(false)).toBe(false)
  })

  // 收紧点：Feed / Todo / feed-scroll-anchor 的本地实现会放行数组。
  it('rejects malformed arrays that the loose local guards let through', () => {
    expect(isRecord([])).toBe(false)
    expect(isRecord([{ id: 'T1' }])).toBe(false)
    expect(isRecord(['id', 'T1'])).toBe(false)
    // 数组带上业务字段依旧不算 record：JSON 解析出的是列表，不是对象。
    const decorated = Object.assign([], { id: 'T1' })
    expect(isRecord(decorated)).toBe(false)
  })

  it('treats other exotic objects as records', () => {
    expect(isRecord(new Error('boom'))).toBe(true)
    expect(isRecord(new Date())).toBe(true)
  })

  it('narrows the value for property reads', () => {
    const value: unknown = { message: '请求失败' }
    if (!isRecord(value)) throw new Error('expected a record')
    expect(value.message).toBe('请求失败')
  })
})

describe('asRecord', () => {
  it('returns the same object reference for records', () => {
    const value = { id: 'T1' }
    expect(asRecord(value)).toBe(value)
  })

  it('returns null for non-records including arrays', () => {
    expect(asRecord(null)).toBeNull()
    expect(asRecord(undefined)).toBeNull()
    expect(asRecord('T1')).toBeNull()
    expect(asRecord(7)).toBeNull()
    expect(asRecord([])).toBeNull()
    expect(asRecord([{ id: 'T1' }])).toBeNull()
  })

  it('agrees with isRecord on every sample', () => {
    const samples: readonly unknown[] = [null, undefined, 0, '', 'x', true, [], [1], {}, { a: 1 }, new Date()]
    for (const sample of samples) {
      expect(asRecord(sample) !== null).toBe(isRecord(sample))
    }
  })
})
