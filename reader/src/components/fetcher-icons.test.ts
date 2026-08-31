import { describe, expect, it } from 'vitest'
import { fetcherIcon } from './fetcher-icons'

describe('fetcherIcon', () => {
  it('映射已知 fetcher 图标', () => {
    expect(fetcherIcon('github')).toBe('code')
  })

  it('复用 fetcher metadata 的后缀归一化', () => {
    expect(fetcherIcon('basic+thin')).toBe('globe')
  })

  it('未知 fetcher 回退 link 图标', () => {
    expect(fetcherIcon('custom')).toBe('link')
  })
})
