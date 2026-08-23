import { useEffect, useMemo } from 'react'

export interface AbortGeneration {
  readonly key: string
  readonly controller: AbortController
}

/**
 * Owns one AbortController per logical key and aborts it when the key is
 * replaced or the hook unmounts.
 */
export function useAbortGeneration(key: string): AbortGeneration {
  const generation = useMemo(
    () => ({ key, controller: new AbortController() }),
    [key],
  )

  useEffect(() => () => {
    generation.controller.abort()
  }, [generation])

  return generation
}
