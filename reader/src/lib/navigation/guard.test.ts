import { vi } from 'vitest'
import { ReaderNavigationGuardRegistry } from './guard'

describe('Reader navigation guard registry', () => {
  it.each([
    ['clean', false, true, true, 0],
    ['dirty-confirm', true, true, true, 1],
    ['dirty-cancel', true, false, false, 1],
  ] as const)('%s editor state gates one navigation request', (_name, dirty, answer, allowed, prompts) => {
    const registry = new ReaderNavigationGuardRegistry()
    const confirm = vi.fn(() => answer)
    const unregister = registry.register('notes', {
      blocksNavigation: () => dirty,
      requestNavigation: confirm,
    })

    expect(registry.hasBlocker()).toBe(dirty)
    expect(registry.requestNavigation()).toBe(allowed)
    expect(confirm).toHaveBeenCalledTimes(prompts)

    unregister()
    expect(registry.hasBlocker()).toBe(false)
  })

  it('fails closed when a registered editor cannot report or confirm its state', () => {
    const registry = new ReaderNavigationGuardRegistry()
    registry.register('state-error', {
      blocksNavigation: () => { throw new Error('state unavailable') },
      requestNavigation: () => true,
    })
    expect(registry.hasBlocker()).toBe(true)
    expect(registry.requestNavigation()).toBe(false)

    registry.register('state-error', {
      blocksNavigation: () => true,
      requestNavigation: () => { throw new Error('prompt unavailable') },
    })
    expect(registry.requestNavigation()).toBe(false)
  })
})
