import { useEffect, useRef } from 'react'
import type { ReaderClient } from '../lib/api/client'
import { useIdentityBoundOperationGate } from './identityBoundOperation'

interface IdentityPollingOptions {
  readonly enabled?: boolean
  readonly intervalMs: number
  readonly logicalKey: string
  readonly ownerKey?: unknown
  readonly visibleOnly?: boolean
  readonly tickOnVisible?: boolean
  readonly onTick: () => void
}

export function useIdentityPolling(
  client: ReaderClient | null,
  options: IdentityPollingOptions,
): void {
  const {
    enabled = true,
    intervalMs,
    logicalKey,
    ownerKey = null,
    visibleOnly = false,
    tickOnVisible = false,
    onTick,
  } = options
  const onTickRef = useRef(onTick)
  onTickRef.current = onTick
  const gate = useIdentityBoundOperationGate<'poll'>(client, [ownerKey])

  useEffect(() => {
    if (!enabled) return
    const token = gate.capture('poll', logicalKey)
    if (!token) return

    const tick = () => {
      if (visibleOnly && document.visibilityState !== 'visible') return
      gate.commit(token, () => onTickRef.current())
    }

    const timer = window.setInterval(tick, intervalMs)
    if (tickOnVisible) document.addEventListener('visibilitychange', tick)
    return () => {
      window.clearInterval(timer)
      if (tickOnVisible) document.removeEventListener('visibilitychange', tick)
      gate.invalidate('poll')
    }
  }, [enabled, gate, intervalMs, logicalKey, ownerKey, tickOnVisible, visibleOnly])
}
