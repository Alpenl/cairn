import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { useReaderClient } from './useReaderClient'

function clampProgress(value: number): number {
  return Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0))
}

export interface UseReadingProgressOptions {
  scrollRef: RefObject<HTMLElement>
  sourceKey: string
  layoutKey: string
  disabled?: boolean
  /** Saved-link identity whose progress/read state is shared with the server. */
  engagementLinkID?: string
  /** Explicit client for isolated surfaces and tests. */
  readerClient?: IdentityBoundReaderClient
}

export interface ReadingProgressState {
  progress: number
  sync: () => void
  reset: () => void
  backToTop: () => void
}

const PROGRESS_WRITE_DELAY_MS = 700

function clientIsCurrent(client: IdentityBoundReaderClient): boolean {
  try {
    return typeof client.isIdentityCurrent !== 'function' || client.isIdentityCurrent()
  } catch {
    return false
  }
}

function supportsEngagement(
  client: IdentityBoundReaderClient | null,
  method: 'getEngagement' | 'patchEngagement',
): boolean {
  return typeof (client as unknown as Record<string, unknown> | null)?.[method] === 'function'
}

export function useReadingProgress({
  scrollRef,
  sourceKey,
  layoutKey,
  disabled = false,
  engagementLinkID,
  readerClient,
}: UseReadingProgressOptions): ReadingProgressState {
  const client = useReaderClient(readerClient)
  const [progress, setProgress] = useState(0)
  const [remoteProgress, setRemoteProgress] = useState<number | null>(null)
  const progressRef = useRef(0)
  const hydratingRef = useRef(false)
  const pendingWriteRef = useRef<number | null>(null)
  const lastWrittenRef = useRef<number | null>(null)
  const writeTimerRef = useRef<number | null>(null)
  const writeChainRef = useRef<Promise<void>>(Promise.resolve())

  const setLocalProgress = useCallback((value: number) => {
    progressRef.current = value
    setProgress((current) => (current === value ? current : value))
  }, [])

  const flushProgress = useCallback((): Promise<void> => {
    if (writeTimerRef.current !== null && typeof window !== 'undefined') {
      window.clearTimeout(writeTimerRef.current)
      writeTimerRef.current = null
    }
    const value = pendingWriteRef.current
    pendingWriteRef.current = null
    if (
      value === null ||
      disabled ||
      !engagementLinkID ||
      !client ||
      !supportsEngagement(client, 'patchEngagement') ||
      !clientIsCurrent(client)
    ) {
      return Promise.resolve()
    }

    const task = writeChainRef.current.then(async () => {
      if (lastWrittenRef.current === value || !clientIsCurrent(client)) return
      try {
        const result = await client.patchEngagement(engagementLinkID, { progress: value })
        if (
          result.ok &&
          result.data.link_id === engagementLinkID &&
          clientIsCurrent(client)
        ) {
          lastWrittenRef.current = value
        }
      } catch {
        // A scroll write is best effort.  The next scroll or page lifecycle
        // event retries it without replacing the visible local projection.
      }
    })
    writeChainRef.current = task.then(() => undefined, () => undefined)
    return task
  }, [client, disabled, engagementLinkID])

  const scheduleProgress = useCallback((value: number) => {
    if (
      disabled ||
      !engagementLinkID ||
      !client ||
      !supportsEngagement(client, 'patchEngagement') ||
      hydratingRef.current
    ) return
    pendingWriteRef.current = value / 100
    if (writeTimerRef.current !== null || typeof window === 'undefined') return
    writeTimerRef.current = window.setTimeout(() => {
      writeTimerRef.current = null
      void flushProgress()
    }, PROGRESS_WRITE_DELAY_MS)
  }, [client, disabled, engagementLinkID, flushProgress])

  const sync = useCallback(() => {
    if (disabled) {
      setLocalProgress(0)
      return
    }
    const scroller = scrollRef.current
    if (!scroller) return
    const max = scroller.scrollHeight - scroller.clientHeight
    const next = max <= 0 ? 0 : clampProgress((scroller.scrollTop / max) * 100)
    setLocalProgress(next)
    scheduleProgress(next)
  }, [disabled, scheduleProgress, scrollRef, setLocalProgress])

  const reset = useCallback(() => {
    setRemoteProgress(null)
    setLocalProgress(0)
  }, [setLocalProgress])

  const backToTop = useCallback(() => {
    scrollRef.current?.scrollTo({ top: 0, behavior: 'smooth' })
  }, [scrollRef])

  useEffect(() => {
    hydratingRef.current = false
    setRemoteProgress(null)
    setLocalProgress(0)
    lastWrittenRef.current = null

    if (disabled || !engagementLinkID || !client) return

    let cancelled = false
    const isUsable = () => !cancelled && clientIsCurrent(client)
    const hasGet = supportsEngagement(client, 'getEngagement')
    const hasPatch = supportsEngagement(client, 'patchEngagement')
    hydratingRef.current = hasGet

    if (hasGet) {
      void client.getEngagement(engagementLinkID).then((result) => {
        if (!isUsable()) return
        if (result.ok && result.data.link_id === engagementLinkID) {
          setRemoteProgress(clampProgress(result.data.progress * 100))
        }
      }).catch(() => {
        // Old Reader backends may not expose engagement yet.
      }).finally(() => {
        if (isUsable()) hydratingRef.current = false
      })
    }
    if (!hasGet) hydratingRef.current = false

    // Opening a saved link is a shared engagement event.  The response is not
    // copied into local storage; the next read remains server-authoritative.
    if (hasPatch && isUsable()) {
      void client.patchEngagement(engagementLinkID, { read: true }).catch(() => {
        // Opening remains usable when an older backend or a transient network
        // failure cannot accept engagement writes.
      })
    }

    return () => {
      cancelled = true
      hydratingRef.current = false
      void flushProgress()
    }
  }, [client, disabled, engagementLinkID, flushProgress, setLocalProgress, sourceKey])

  useEffect(() => {
    if (remoteProgress === null || disabled) return
    let attempts = 0
    let retryTimer: number | null = null
    const apply = () => {
      const scroller = scrollRef.current
      if (!scroller) return false
      const max = scroller.scrollHeight - scroller.clientHeight
      if (max <= 0) {
        if (remoteProgress === 0) {
          setLocalProgress(0)
          setRemoteProgress(null)
          return true
        }
        return false
      }
      scroller.scrollTop = (remoteProgress / 100) * max
      setLocalProgress(remoteProgress)
      setRemoteProgress(null)
      return true
    }
    const retry = () => {
      if (apply() || typeof window === 'undefined') return
      attempts += 1
      if (attempts < 8) retryTimer = window.setTimeout(retry, 50)
    }
    retry()
    return () => {
      if (retryTimer !== null && typeof window !== 'undefined') window.clearTimeout(retryTimer)
    }
  }, [disabled, layoutKey, remoteProgress, scrollRef, setLocalProgress, sourceKey])

  useEffect(() => {
    sync()
  }, [layoutKey, sync])

  useEffect(() => {
    if (!engagementLinkID) return
    const onPageHide = () => { void flushProgress() }
    window.addEventListener('pagehide', onPageHide)
    return () => window.removeEventListener('pagehide', onPageHide)
  }, [engagementLinkID, flushProgress])

  return { progress, sync, reset, backToTop }
}
