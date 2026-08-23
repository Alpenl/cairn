import { describe, expect, it } from 'vitest'
import { contentTypeLabel, fetcherKey, fetcherLabel, relDate } from './metadata'

const NOW = new Date('2026-06-11T10:30:00Z')

describe('relDate', () => {
  it('今天', () => {
    expect(relDate('2026-06-11T08:00:00Z', NOW)).toBe('今天')
  })

  it('昨天', () => {
    expect(relDate('2026-06-10T08:00:00Z', NOW)).toBe('昨天')
  })

  it('N 天前', () => {
    expect(relDate('2026-06-08T08:00:00Z', NOW)).toBe('3 天前')
  })

  it('超过 7 天 → MM-DD', () => {
    expect(relDate('2026-05-20T08:00:00Z', NOW)).toBe('05-20')
  })

  it('空值 → 空串', () => {
    expect(relDate(null, NOW)).toBe('')
  })
})

describe('fetcher metadata', () => {
  it('剥离 fetcher 质量后缀', () => {
    expect(fetcherKey('basic+thin')).toBe('basic')
  })

  it('返回已知 fetcher 标签', () => {
    expect(fetcherLabel('github+thin')).toBe('GitHub')
  })

  it('未知 fetcher 不伪造标签', () => {
    expect(fetcherLabel('custom')).toBeUndefined()
  })
})

describe('contentTypeLabel', () => {
  it('返回已知内容类型标签', () => {
    expect(contentTypeLabel('article')).toBe('文章')
  })

  it('未知内容类型保留后端原值', () => {
    expect(contentTypeLabel('custom')).toBe('custom')
  })
})
