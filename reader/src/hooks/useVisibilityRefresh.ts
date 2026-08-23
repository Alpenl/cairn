import { useEffect, useRef } from 'react'

/**
 * Runs the latest callback when the tab becomes visible.
 *
 * This keeps "refresh on foreground" as one browser event subscription rule,
 * while callers keep ownership of what "refresh" means for their resource.
 */
export function useVisibilityRefresh(onVisible: () => void, enabled = true): void {
  const onVisibleRef = useRef(onVisible)

  useEffect(() => {
    onVisibleRef.current = onVisible
  }, [onVisible])

  useEffect(() => {
    if (!enabled) return
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') onVisibleRef.current()
    }
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [enabled])
}
