import {
  capabilitySet,
  createReadingFocusStore,
  createReadingPreferenceStore,
  hasReadingCapability,
  htmlSource,
  markdownSource,
  normalizeReadingPreference,
  plainSource,
  readingTextVersion,
} from './index'

describe('reading surface contract', () => {
  it('normalizes source adapters while preserving host identity', () => {
    expect(markdownSource('# title', { hostId: 'link-1', version: 'content:3' })).toEqual({
      kind: 'markdown',
      text: '# title',
      blockKey: 'content',
      identity: { hostId: 'link-1', version: 'content:3' },
    })
    expect(plainSource('plain', { hostId: 'link-1', version: 'content:3' }, 'content-document').blockKey).toBe('content-document')
    expect(htmlSource('<p>x</p>', 'https://example.com/article', { hostId: 'feed-1', version: 'etag:v1' }).baseURL).toBe('https://example.com/article')
  })

  it('derives a stable fallback version from content when no server revision exists', () => {
    expect(readingTextVersion('same')).toBe(readingTextVersion('same'))
    expect(readingTextVersion('same')).not.toBe(readingTextVersion('changed'))
  })

  it('rejects source identities that could not own annotations or progress', () => {
    expect(() => markdownSource('x', { hostId: '', version: 'v1' })).toThrow('hostId')
    expect(() => markdownSource('x', { hostId: 'host', version: '' })).toThrow('version')
    expect(() => htmlSource('x', 'file:///tmp/article', { hostId: 'host', version: 'v1' })).toThrow('http')
  })

  it('keeps capability checks explicit and side-effect free', () => {
    const surface = {
      source: plainSource('x', { hostId: 'host', version: 'v1' }),
      capabilities: capabilitySet(['focus', 'progress']),
      slots: { rail: 'toc-only' as const },
    }
    expect(hasReadingCapability(surface, 'focus')).toBe(true)
    expect(hasReadingCapability(surface, 'annotations')).toBe(false)
  })
})

describe('shared reading focus store', () => {
  it('notifies consumers only when focus changes', () => {
    const store = createReadingFocusStore()
    const listener = vi.fn()
    const unsubscribe = store.subscribe(listener)
    store.set(false)
    store.set(true)
    store.set(true)
    store.toggle()
    expect(store.getSnapshot()).toBe(false)
    unsubscribe()
    store.toggle()
    expect(listener).toHaveBeenCalledTimes(2)
    expect(store.getSnapshot()).toBe(true)
  })
})

describe('shared reading preference contract', () => {
  it('clamps persisted values instead of allowing layout instability', () => {
    expect(normalizeReadingPreference({ size: -4, lineHeight: 99 })).toEqual({ size: 0, lineHeight: 2 })
    expect(normalizeReadingPreference({ size: 2.5, lineHeight: Number.NaN })).toEqual({ size: 1, lineHeight: 1 })
  })

  it('does not notify when a patch does not change the normalized value', () => {
    const store = createReadingPreferenceStore({ size: 1, lineHeight: 1 })
    const listener = vi.fn()
    store.subscribe(listener)
    store.set({ size: 99 })
    store.set({ size: 2 })
    expect(listener).toHaveBeenCalledTimes(2)
    expect(store.getSnapshot()).toEqual({ size: 2, lineHeight: 1 })
  })
})
