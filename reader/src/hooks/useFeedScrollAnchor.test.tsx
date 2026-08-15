import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render } from '@testing-library/react'
import { useRef } from 'react'
import { useFeedScrollAnchor, type FeedScrollAnchorHandle } from './useFeedScrollAnchor'
import { readFeedScrollAnchor, writeFeedScrollAnchor } from '../lib/feed-scroll-anchor'

// jsdom has no layout, so the scroller and its cards get a modelled one:
// a viewport starting at 100px, cards 200px tall stacked in DOM order, and a
// scrollTop that actually moves them. Without it every rect is zero and any
// assertion about a restored position would hold for free.
const VIEWPORT_TOP = 100
const VIEWPORT_HEIGHT = 600
const CARD_HEIGHT = 200

const CARDS = ['k0', 'k1', 'k2', 'k3', 'k4', 'k5']

function installLayout(scroller: HTMLElement): void {
  let scrollTop = 0
  Object.defineProperty(scroller, 'scrollTop', {
    configurable: true,
    get: () => scrollTop,
    set: (value: number) => {
      scrollTop = Math.max(0, value)
    },
  })
  scroller.getBoundingClientRect = () => ({
    top: VIEWPORT_TOP,
    bottom: VIEWPORT_TOP + VIEWPORT_HEIGHT,
  }) as DOMRect
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    const cards = [...scroller.querySelectorAll<HTMLElement>('[data-feed-item-key]')]
    const index = cards.indexOf(this)
    if (index < 0) return { top: 0, bottom: 0 } as DOMRect
    const top = VIEWPORT_TOP + index * CARD_HEIGHT - scrollTop
    return { top, bottom: top + CARD_HEIGHT } as DOMRect
  })
}

interface HarnessProps {
  readonly stateKey: string | null
  readonly renderedKey: string | null
  readonly identityIsCurrent?: () => boolean
  readonly cards?: readonly string[]
  readonly onHandle: (handle: FeedScrollAnchorHandle) => void
}

function Harness({ stateKey, renderedKey, identityIsCurrent, cards = CARDS, onHandle }: HarnessProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  onHandle(useFeedScrollAnchor({
    containerRef,
    stateKey,
    renderedKey,
    identityIsCurrent: identityIsCurrent ?? (() => true),
  }))
  return (
    <div ref={containerRef} data-testid="scroller">
      {cards.map((key) => <div key={key} data-feed-item-key={key} />)}
    </div>
  )
}

function mountHarness(props: Omit<HarnessProps, 'onHandle'>) {
  let handle: FeedScrollAnchorHandle = { forget: () => {} }
  // The modelled layout can only be installed once the scroller exists, so the
  // first render never claims to have content yet.
  const rendered = render(
    <Harness {...props} renderedKey={null} onHandle={(value) => { handle = value }} />,
  )
  const scroller = rendered.getByTestId('scroller')
  installLayout(scroller)
  if (props.renderedKey !== null) {
    rendered.rerender(<Harness {...props} onHandle={(value) => { handle = value }} />)
  }
  return {
    scroller,
    forget: () => act(() => { handle.forget() }),
    update: (next: Omit<HarnessProps, 'onHandle'>) => rendered.rerender(
      <Harness {...next} onHandle={(value) => { handle = value }} />,
    ),
    unmount: () => rendered.unmount(),
  }
}

// k3 sits 600px below the first card; the reader is 50px into it.
const K3_ANCHOR = { itemKey: 'k3', offset: -50, scrollTop: 999 }
const K3_TARGET = 650

// Vitest unwinds afterEach hooks in reverse registration order, so the shared
// setup's unmount cleanup runs after a file-local afterEach and would leak the
// anchor it records into the next test.
beforeEach(() => {
  window.sessionStorage.clear()
})

describe('useFeedScrollAnchor', () => {
  it('restores the stored card anchor once the key own content is rendered', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: null })

    expect(harness.scroller.scrollTop).toBe(0)

    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)
  })

  it('starts a key with no stored position at the top and keeps other keys intact', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)

    harness.update({ stateKey: 'key-b', renderedKey: null })
    expect(harness.scroller.scrollTop).toBe(0)
    harness.update({ stateKey: 'key-b', renderedKey: 'key-b' })
    expect(harness.scroller.scrollTop).toBe(0)

    harness.scroller.scrollTop = 1000
    harness.update({ stateKey: 'key-a', renderedKey: null })
    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)
    // Leaving key-b recorded its own position rather than key-a's.
    expect(readFeedScrollAnchor('key-b')).toEqual({ itemKey: 'k5', offset: 0, scrollTop: 1000 })
  })

  it('drops a restore whose content belongs to a key the reader already left', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    writeFeedScrollAnchor('key-b', { itemKey: 'k1', offset: 0, scrollTop: 111 })
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)

    // The in-flight snapshot for key-a lands after the switch to key-b.
    harness.update({ stateKey: 'key-b', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(0)

    harness.update({ stateKey: 'key-b', renderedKey: 'key-b' })
    expect(harness.scroller.scrollTop).toBe(200)
  })

  it('does not restore for an identity that is no longer current', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: null, identityIsCurrent: () => false })

    harness.update({ stateKey: 'key-a', renderedKey: 'key-a', identityIsCurrent: () => false })
    expect(harness.scroller.scrollTop).toBe(0)
  })

  it('does not record a position for a key whose content never rendered', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: null })
    harness.scroller.scrollTop = 40

    harness.update({ stateKey: 'key-b', renderedKey: null })
    expect(readFeedScrollAnchor('key-a')).toEqual(K3_ANCHOR)
  })

  it('records the position when the surface unmounts', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    harness.scroller.scrollTop = 850

    harness.unmount()
    expect(readFeedScrollAnchor('key-a')).toEqual({ itemKey: 'k4', offset: -50, scrollTop: 850 })
  })

  it('records the position when the page is hidden without unmounting', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    harness.scroller.scrollTop = 850

    act(() => { window.dispatchEvent(new Event('pagehide')) })
    expect(readFeedScrollAnchor('key-a')).toEqual({ itemKey: 'k4', offset: -50, scrollTop: 850 })
  })

  it('forgets only the current key and returns it to the top', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    writeFeedScrollAnchor('key-b', { itemKey: 'k1', offset: 0, scrollTop: 111 })
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)

    harness.forget()
    expect(harness.scroller.scrollTop).toBe(0)
    expect(readFeedScrollAnchor('key-a')).toBeNull()
    expect(readFeedScrollAnchor('key-b')).toEqual({ itemKey: 'k1', offset: 0, scrollTop: 111 })

    harness.update({ stateKey: 'key-b', renderedKey: null })
    harness.update({ stateKey: 'key-b', renderedKey: 'key-b' })
    expect(harness.scroller.scrollTop).toBe(200)

    harness.update({ stateKey: 'key-a', renderedKey: null })
    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(0)
  })

  it('puts a key with no stored position at the top even if the viewport drifted while loading', () => {
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: null })
    // Content growing under a scroller can move it before the snapshot lands.
    harness.scroller.scrollTop = 300

    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(0)
  })

  it('restores a key once per entry so a re-rendered snapshot does not yank the reader back', () => {
    writeFeedScrollAnchor('key-a', K3_ANCHOR)
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(K3_TARGET)

    harness.scroller.scrollTop = 100
    // A retry after an error re-renders the same key's content.
    harness.update({ stateKey: 'key-a', renderedKey: null })
    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(100)
  })

  it('uses the stored scrollTop when the anchored card is gone from the snapshot', () => {
    writeFeedScrollAnchor('key-a', { itemKey: 'removed', offset: -50, scrollTop: 420 })
    const harness = mountHarness({ stateKey: 'key-a', renderedKey: null })

    harness.update({ stateKey: 'key-a', renderedKey: 'key-a' })
    expect(harness.scroller.scrollTop).toBe(420)
  })

  it('persists nothing when the surface has no identity namespace to key on', () => {
    const harness = mountHarness({ stateKey: null, renderedKey: null })
    harness.scroller.scrollTop = 850

    act(() => { window.dispatchEvent(new Event('pagehide')) })
    harness.unmount()
    expect(window.sessionStorage.length).toBe(0)
  })
})
