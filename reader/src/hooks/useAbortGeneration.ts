import { useEffect, useMemo, useRef } from 'react'

export interface AbortGeneration {
  readonly key: string
  readonly controller: AbortController
}

/**
 * Owns one AbortController per logical key and aborts it when the key is
 * replaced or the hook unmounts.
 */
export function useAbortGeneration(key: string): AbortGeneration {
  const activeRef = useRef<AbortGeneration | null>(null)
  const pendingAbortRef = useRef(new WeakMap<AbortController, () => void>())
  const generation = useMemo(
    () => ({ key, controller: new AbortController() }),
    [key],
  )

  useEffect(() => {
    const pendingAborts = pendingAbortRef.current
    const cancelPendingAbort = pendingAborts.get(generation.controller)
    if (cancelPendingAbort) {
      cancelPendingAbort()
      pendingAborts.delete(generation.controller)
    }

    const active = activeRef.current
    if (active && active.controller !== generation.controller) {
      pendingAborts.get(active.controller)?.()
      pendingAborts.delete(active.controller)
      active.controller.abort()
    }
    activeRef.current = generation

    return () => {
      let cancelled = false
      pendingAborts.set(generation.controller, () => {
        cancelled = true
      })
      queueMicrotask(() => {
        pendingAborts.delete(generation.controller)
        if (!cancelled) generation.controller.abort()
      })
    }
  }, [generation])

  return generation
}
