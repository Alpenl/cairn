import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, type RefObject } from 'react'
import {
  clearFeedScrollAnchor,
  feedScrollAnchorTarget,
  measureFeedItems,
  pickFeedScrollAnchor,
  readFeedScrollAnchor,
  writeFeedScrollAnchor,
} from '../lib/feed-scroll-anchor'

export interface FeedScrollAnchorOptions {
  readonly containerRef: RefObject<HTMLElement | null>
  /** Storage key for the identity/mode/filter tuple on screen; null disables persistence. */
  readonly stateKey: string | null
  /**
   * Key whose snapshot content is currently in the DOM. Restoring is gated on
   * this matching `stateKey`, so a response that arrives after the reader
   * switched mode or filter can only ever restore its own key — never the one
   * now on screen.
   */
  readonly renderedKey: string | null
  /** Identity fence: a revoked lease must not have its position replayed. */
  readonly identityIsCurrent: () => boolean
}

export interface FeedScrollAnchorHandle {
  /** Explicit refresh is the only gesture that drops a key's stored position. */
  readonly forget: () => void
}

/**
 * Keeps one stored scroll anchor per Feed state key.
 *
 * Capture happens when the key is left (mode/filter switch, unmount, page hide)
 * and only after that key's own restore has settled — otherwise the top-of-list
 * position the surface shows while a snapshot is still loading would overwrite
 * the real position the reader is coming back to.
 */
export function useFeedScrollAnchor({
  containerRef,
  stateKey,
  renderedKey,
  identityIsCurrent,
}: FeedScrollAnchorOptions): FeedScrollAnchorHandle {
  const restoredKeyRef = useRef<string | null>(null)

  const capture = useCallback((key: string) => {
    const container = containerRef.current
    if (!container) return
    const anchor = pickFeedScrollAnchor(
      measureFeedItems(container),
      container.getBoundingClientRect().top,
      container.scrollTop,
    )
    if (anchor) writeFeedScrollAnchor(key, anchor)
  }, [containerRef])

  useLayoutEffect(() => {
    const container = containerRef.current
    if (container) container.scrollTop = 0
    return () => {
      if (stateKey && restoredKeyRef.current === stateKey) capture(stateKey)
      restoredKeyRef.current = null
    }
  }, [capture, containerRef, stateKey])

  useLayoutEffect(() => {
    if (!stateKey || renderedKey !== stateKey) return
    if (restoredKeyRef.current === stateKey) return
    const container = containerRef.current
    if (!container) return
    if (!identityIsCurrent()) return
    const anchor = readFeedScrollAnchor(stateKey)
    if (anchor) {
      container.scrollTop = feedScrollAnchorTarget(
        anchor,
        measureFeedItems(container),
        container.getBoundingClientRect().top,
        container.scrollTop,
      )
    } else {
      container.scrollTop = 0
    }
    restoredKeyRef.current = stateKey
    // `identityIsCurrent` is read at commit time rather than through a ref so
    // the fence sees the identity the caller believes in for *this* commit; a
    // caller that hands over a new function each render only costs a re-run
    // that `restoredKeyRef` immediately turns away.
  }, [containerRef, identityIsCurrent, renderedKey, stateKey])

  useEffect(() => {
    if (!stateKey) return
    // A reload tears the surface down without unmounting React, so the effect
    // cleanup above never runs for it.
    const onPageHide = () => {
      if (restoredKeyRef.current === stateKey) capture(stateKey)
    }
    window.addEventListener('pagehide', onPageHide)
    return () => window.removeEventListener('pagehide', onPageHide)
  }, [capture, stateKey])

  const forget = useCallback(() => {
    if (!stateKey) return
    // Clearing the stored anchor is what keeps the refreshed snapshot from
    // replaying the position: a later restore for this key finds nothing.
    clearFeedScrollAnchor(stateKey)
    const container = containerRef.current
    if (container) container.scrollTop = 0
  }, [containerRef, stateKey])

  // A fresh handle every render would re-create every caller callback that
  // depends on it, so the object is tied to the identity of `forget` itself.
  return useMemo(() => ({ forget }), [forget])
}
