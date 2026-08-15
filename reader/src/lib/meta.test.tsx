import { describe, expect, it } from 'vitest'
import { act, render, renderHook } from '@testing-library/react'
import { relDate, usePins } from './meta'
import { Icon } from '../components/Icon'
import { readerIdentity } from './identity'

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

describe('usePins identity ownership', () => {
  it('does not expose A pins to B and restores them after returning to A', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const a = renderHook(() => usePins())
    act(() => a.result.current[1]('tags', 'Agent'))
    a.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const b = renderHook(() => usePins())
    expect(b.result.current[0].tags).toEqual([])
    b.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const returned = renderHook(() => usePins())
    expect(returned.result.current[0].tags).toEqual(['Agent'])
  })
})

describe('Icon', () => {
  it('渲染 search 图标为 svg', () => {
    const { container } = render(<Icon name="search" />)
    const svg = container.querySelector('svg')
    expect(svg).toBeInTheDocument()
    expect(svg?.innerHTML).toContain('circle')
  })
})
