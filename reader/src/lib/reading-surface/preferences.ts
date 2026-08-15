import { readOwnedStorage, writeOwnedStorage } from '../storage-ownership'

export interface ReadingPreference {
  readonly size: number
  readonly lineHeight: number
}

export const DEFAULT_READING_PREFERENCE: ReadingPreference = Object.freeze({
  size: 1,
  lineHeight: 1,
})

export const READING_SIZES = [14.5, 16, 17.5, 19] as const
export const READING_LINE_HEIGHTS = [1.72, 1.92, 2.12] as const
export const READING_LINE_HEIGHT_LABELS = ['紧凑', '舒适', '宽松'] as const

export const READING_SIZE_COUNT = 4
export const READING_LINE_HEIGHT_COUNT = 3

export function normalizeReadingPreference(
  value: Partial<ReadingPreference> | null | undefined,
): ReadingPreference {
  const rawSize = value?.size
  const rawLineHeight = value?.lineHeight
  const size = typeof rawSize === 'number' && Number.isInteger(rawSize)
    ? rawSize
    : DEFAULT_READING_PREFERENCE.size
  const lineHeight = typeof rawLineHeight === 'number' && Number.isInteger(rawLineHeight)
    ? rawLineHeight
    : DEFAULT_READING_PREFERENCE.lineHeight
  return {
    size: Math.max(0, Math.min(READING_SIZE_COUNT - 1, size)),
    lineHeight: Math.max(0, Math.min(READING_LINE_HEIGHT_COUNT - 1, lineHeight)),
  }
}

export interface ReadingPreferenceStore {
  getSnapshot(): ReadingPreference
  subscribe(listener: () => void): () => void
  set(patch: Partial<ReadingPreference>): void
  reset(): void
}

export function createReadingPreferenceStore(
  initial: Partial<ReadingPreference> = DEFAULT_READING_PREFERENCE,
): ReadingPreferenceStore {
  let value = normalizeReadingPreference(initial)
  const listeners = new Set<() => void>()
  const notify = () => listeners.forEach((listener) => listener())

  return {
    getSnapshot: () => value,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    set(patch) {
      const next = normalizeReadingPreference({ ...value, ...patch })
      if (next.size === value.size && next.lineHeight === value.lineHeight) return
      value = next
      notify()
    },
    reset() {
      if (value.size === DEFAULT_READING_PREFERENCE.size && value.lineHeight === DEFAULT_READING_PREFERENCE.lineHeight) return
      value = DEFAULT_READING_PREFERENCE
      notify()
    },
  }
}

/** Shared preference state; persistence is owned by the Reader shell. */
export const readingPreferenceStore = createReadingPreferenceStore()

let persistenceConsumers = 0
let stopPersisting: (() => void) | null = null

/**
 * Mount one process-wide persistence owner while at least one preference
 * consumer is active. Multiple reading surfaces share the store and must not
 * each write the same device key for one logical preference change.
 */
export function retainReadingPreferencePersistence(): () => void {
  persistenceConsumers += 1
  if (persistenceConsumers === 1) {
    try {
      const raw = readOwnedStorage('readingPreference')
      if (raw) readingPreferenceStore.set(JSON.parse(raw) as Partial<ReadingPreference>)
    } catch {
      // Malformed device state leaves the normalized in-memory value intact.
    }
    stopPersisting = readingPreferenceStore.subscribe(() => {
      writeOwnedStorage('readingPreference', JSON.stringify(readingPreferenceStore.getSnapshot()))
    })
  }

  let retained = true
  return () => {
    if (!retained) return
    retained = false
    persistenceConsumers = Math.max(0, persistenceConsumers - 1)
    if (persistenceConsumers !== 0) return
    stopPersisting?.()
    stopPersisting = null
  }
}
