import { renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { makeLink } from '../test/fixtures'
import { useSidebarData } from './useSidebarData'

describe('useSidebarData Reading corpus authority', () => {
  const reading = makeLink({
    id: 'reading',
    library_kind: 'reading',
    status: 'done',
    tags: ['shared'],
    domain: 'shared.example',
    created_at: '2026-01-01T00:00:00Z',
  })
  const site = makeLink({
    id: 'site',
    library_kind: 'site',
    status: 'done',
    tags: ['shared'],
    domain: 'shared.example',
    created_at: '2026-03-01T00:00:00Z',
  })
  const pending = makeLink({
    id: 'pending',
    library_kind: 'reading',
    status: 'pending',
    tags: ['shared'],
    domain: 'shared.example',
    created_at: '2026-04-01T00:00:00Z',
  })

  it('derives scoped aggregate recency only from done Reading links', () => {
    const { result } = renderHook(() => useSidebarData(
      [reading, site, pending],
      [{ tag: 'shared', count: 1, reading_count: 1, site_count: 1 }],
      [{ domain: 'shared.example', count: 1 }],
      1,
      { links: [reading], total: 1, complete: true },
    ))

    expect(result.current.tags).toEqual([{
      tag: 'shared',
      count: 1,
      lastAt: reading.created_at,
    }])
    expect(result.current.domains).toEqual([{
      domain: 'shared.example',
      count: 1,
      lastAt: reading.created_at,
    }])
  })

  it('filters a proven local fallback through the same done Reading predicate', () => {
    const { result } = renderHook(() => useSidebarData(
      [],
      null,
      null,
      null,
      { links: [reading, reading, site, pending], total: 1, complete: true },
    ))

    expect(result.current.tags).toEqual([{
      tag: 'shared',
      count: 1,
      lastAt: reading.created_at,
    }])
    expect(result.current.domains).toEqual([{
      domain: 'shared.example',
      count: 1,
      lastAt: reading.created_at,
    }])
  })

  it('keeps aggregate fallback unavailable when filtered unique rows do not match total', () => {
    const { result } = renderHook(() => useSidebarData(
      [],
      null,
      null,
      null,
      { links: [pending], total: 1, complete: true },
    ))

    expect(result.current).toMatchObject({
      counts: { all: 1 },
      tags: [],
      domains: [],
      tagsAvailable: false,
      domainsAvailable: false,
    })
  })
})
