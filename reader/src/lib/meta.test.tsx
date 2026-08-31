import { describe, expect, it } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { usePins } from './meta'
import { readerIdentity } from './identity'

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
