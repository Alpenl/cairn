import { useCallback, useEffect, useMemo, useSyncExternalStore, type RefObject } from 'react'
import { useReaderToc, type ReaderTocState } from './useReaderToc'
import { useReadingProgress, type ReadingProgressState } from './useReadingProgress'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import {
  DEFAULT_READING_PREFERENCE,
  capabilitySet,
  readingFocusStore,
  readingPreferenceStore,
  retainReadingPreferencePersistence,
  type ReadingPreference,
} from '../lib/reading-surface'
import type {
  ReadingSource,
  ReadingSurfaceCapability,
  ReadingSurfaceContract,
  ReadingSurfaceSlots,
} from '../lib/reading-surface'

export interface UseReadingSurfaceOptions {
  readonly source: ReadingSource
  readonly capabilities: readonly ReadingSurfaceCapability[]
  readonly slots?: ReadingSurfaceSlots
  readonly scrollRef: RefObject<HTMLElement>
  readonly layoutKey: string
  /** Optional display identity for TOC ownership when source version is stable. */
  readonly tocSourceKey?: string
  /** Optional stable identity for progress, independent of TOC display state. */
  readonly progressSourceKey?: string
  /** Persist progress/read state for a saved Reader link when present. */
  readonly engagementLinkID?: string
  readonly readerClient?: IdentityBoundReaderClient
}

export interface ReadingSurfaceState {
  readonly contract: ReadingSurfaceContract
  readonly focusMode: boolean
  readonly setFocusMode: (value: boolean) => void
  readonly readingPreference: ReadingPreference
  readonly setReadingPreference: (patch: Partial<ReadingPreference>) => void
  readonly progress: ReadingProgressState
  readonly toc: ReaderTocState
}

const NOOP_SUBSCRIBE = () => () => {}
const NO_FOCUS_SNAPSHOT = () => false
const NO_PREFERENCE_SNAPSHOT = () => DEFAULT_READING_PREFERENCE
let tocCapabilityGenerationCounter = 0

export function useReadingSurface({
  source,
  capabilities,
  slots,
  scrollRef,
  layoutKey,
  tocSourceKey,
  progressSourceKey,
  engagementLinkID,
  readerClient,
}: UseReadingSurfaceOptions): ReadingSurfaceState {
  // Consumers often declare their capability array inline. Compare its value,
  // rather than the array reference, so unrelated consumer state does not
  // rebuild the contract or its downstream callbacks.
  const capabilityKey = capabilities.join('\u0000')
  const capabilitiesSnapshot = useMemo(
    () => capabilitySet(capabilities),
    // The value key is intentional: consumers commonly pass inline arrays.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [capabilityKey],
  )
  const focusEnabled = capabilitiesSnapshot.has('focus')
  const preferencesEnabled = capabilitiesSnapshot.has('preferences')
  const progressEnabled = capabilitiesSnapshot.has('progress')
  const tocEnabled = capabilitiesSnapshot.has('toc')
  const backToTopEnabled = capabilitiesSnapshot.has('back-to-top')
  const sourceKey = JSON.stringify([
    source.identity.hostId,
    source.identity.version,
    source.blockKey,
    source.kind,
  ])
  const resolvedTocSourceKey = tocSourceKey ?? sourceKey

  // useReaderToc intentionally keeps its last source snapshot while a
  // consumer is disabled so hook order stays stable. Give each capability
  // configuration a fresh identity so re-enabling TOC cannot briefly expose
  // headings collected by the previous configuration.
  const tocCapabilityGeneration = useMemo(() => {
    tocCapabilityGenerationCounter += 1
    return `${capabilityKey}:${tocCapabilityGenerationCounter}`
  }, [capabilityKey])

  const focusMode = useSyncExternalStore(
    focusEnabled ? readingFocusStore.subscribe : NOOP_SUBSCRIBE,
    focusEnabled ? readingFocusStore.getSnapshot : NO_FOCUS_SNAPSHOT,
    NO_FOCUS_SNAPSHOT,
  )
  const readingPreference = useSyncExternalStore(
    preferencesEnabled ? readingPreferenceStore.subscribe : NOOP_SUBSCRIBE,
    preferencesEnabled ? readingPreferenceStore.getSnapshot : NO_PREFERENCE_SNAPSHOT,
    NO_PREFERENCE_SNAPSHOT,
  )
  useEffect(() => {
    if (!preferencesEnabled) return
    return retainReadingPreferencePersistence()
  }, [preferencesEnabled])

  const setFocusMode = useCallback((value: boolean) => {
    if (focusEnabled) readingFocusStore.set(value)
  }, [focusEnabled])
  const setReadingPreference = useCallback((patch: Partial<ReadingPreference>) => {
    if (preferencesEnabled) readingPreferenceStore.set(patch)
  }, [preferencesEnabled])

  const resolvedLayoutKey = [
    layoutKey,
    focusEnabled ? String(focusMode) : 'focus-off',
    preferencesEnabled ? `${readingPreference.size}:${readingPreference.lineHeight}` : 'preferences-off',
  ].join('\u0000')

  const progress = useReadingProgress({
    scrollRef,
    sourceKey: progressEnabled ? (progressSourceKey ?? sourceKey) : '',
    layoutKey: progressEnabled ? resolvedLayoutKey : '',
    disabled: !progressEnabled,
    engagementLinkID: progressEnabled ? engagementLinkID : undefined,
    readerClient,
  })
  const toc = useReaderToc({
    scrollRef,
    sourceKey: tocEnabled
      ? `${resolvedTocSourceKey}\u0000${tocCapabilityGeneration}`
      : '',
    layoutKey: tocEnabled ? resolvedLayoutKey : '',
    enabled: tocEnabled,
  })

  const progressBackToTop = progress.backToTop
  const backToTop = useCallback(() => {
    if (backToTopEnabled) progressBackToTop()
  }, [backToTopEnabled, progressBackToTop])
  const progressState = useMemo<ReadingProgressState>(
    () => ({ ...progress, backToTop }),
    [backToTop, progress],
  )

  const contract = useMemo<ReadingSurfaceContract>(() => ({
    source,
    capabilities: capabilitiesSnapshot,
    slots: slots ?? { toolbar: 'default', rail: 'default', annotation: 'disabled' },
  }), [capabilitiesSnapshot, slots, source])

  return {
    contract,
    focusMode: focusEnabled ? focusMode : false,
    setFocusMode,
    readingPreference: preferencesEnabled ? readingPreference : DEFAULT_READING_PREFERENCE,
    setReadingPreference,
    progress: progressState,
    toc,
  }
}
