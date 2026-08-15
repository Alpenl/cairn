export interface FocusStore {
  getSnapshot(): boolean
  subscribe(listener: () => void): () => void
  set(value: boolean): void
  toggle(): void
}

function createFocusStore(initial = false): FocusStore {
  let value = initial
  const listeners = new Set<() => void>()

  const notify = () => listeners.forEach((listener) => listener())

  return {
    getSnapshot: () => value,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    set(next) {
      if (value === next) return
      value = next
      notify()
    },
    toggle() {
      value = !value
      notify()
    },
  }
}

/** One focus state for DetailPane, subscription detail, and future surfaces. */
export const readingFocusStore = createFocusStore()

export function createReadingFocusStore(initial = false): FocusStore {
  return createFocusStore(initial)
}
