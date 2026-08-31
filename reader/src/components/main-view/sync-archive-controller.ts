import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'

import type { ApiResult } from '@webtag/api'
import type { ArchiveV2Selection } from '../../lib/api/archive-v2'
import type { ReaderCapabilityLease } from '../../lib/capabilities'
import type { IdentityLease } from '../../lib/identity'
import type {
  ReaderSessionArchivePort,
  ReaderThoughtsNotesPort,
} from '../../lib/reader-api-ports'
import { resourceStore } from '../../lib/cache/store'
import {
  EMPTY_THOUGHT_SYNC_SNAPSHOT,
  getThoughtSyncController,
  type ThoughtSyncSnapshot,
} from '../../lib/user-data/thought-sync'
import type { LibraryReloadOptions } from '../../hooks/libraryReload'
import type { IconName } from '../Icon'
import type { ToastAction } from '../Toast'

const LIBRARY_SYNC_RESOURCES = ['links', 'tags', 'domains'] as const

export type LibrarySyncResource = typeof LIBRARY_SYNC_RESOURCES[number]

type Flash = (msg: string, icon?: IconName, action?: ToastAction) => void
type LibraryReload = (
  options?: LibraryReloadOptions,
) => Promise<ApiResult<unknown> | null>

type SyncArchiveClient = ReaderSessionArchivePort & ReaderThoughtsNotesPort

interface UseSyncArchiveControllerOptions {
  readonly client: SyncArchiveClient
  readonly lease: IdentityLease
  readonly capabilityLease: ReaderCapabilityLease
  readonly reloadLinks: LibraryReload
  readonly reloadTags: LibraryReload
  readonly reloadDomains: LibraryReload
  readonly onRefreshCapabilities?: () => void
  readonly flash: Flash
}

export interface UseSyncArchiveControllerResult {
  readonly librarySyncing: boolean
  readonly librarySyncFailures: readonly LibrarySyncResource[]
  readonly thoughtSync: ThoughtSyncSnapshot | null
  readonly syncLibraryAndThoughts: () => void
  readonly subscriptionSyncRequest: number
  readonly requestSubscriptionSync: () => void
  readonly archiveDownloading: boolean
  readonly downloadArchive: (selection: ArchiveV2Selection) => Promise<boolean>
}

function libraryReloadFailed(
  outcome: PromiseSettledResult<ApiResult<unknown> | null>,
): boolean {
  if (outcome.status === 'rejected' || outcome.value === null) return true
  return !outcome.value.ok
}

function startLibraryReload<T>(reload: () => Promise<T>): Promise<T> {
  try {
    return reload()
  } catch (thrown) {
    return Promise.reject(thrown)
  }
}

function thoughtSyncOutcome(snapshot: ThoughtSyncSnapshot): string {
  switch (snapshot.phase) {
    case 'offline':
      return snapshot.pendingCount > 0 ? `想法离线，${snapshot.pendingCount} 项待同步` : '想法离线'
    case 'syncing':
      return snapshot.pendingCount > 0 ? `想法同步中，${snapshot.pendingCount} 项待同步` : '想法同步中'
    case 'failed': {
      const blocked = snapshot.blockedCount > 0 ? `，${snapshot.blockedCount} 项被阻塞` : ''
      const code = snapshot.errorCode ? `（${snapshot.errorCode}）` : ''
      return `想法同步失败，${snapshot.pendingCount} 项待同步${blocked}${code}`
    }
    case 'pending':
      return `想法待同步，${snapshot.pendingCount} 项操作`
    case 'synced':
      return '想法已同步'
  }
}

export function useSyncArchiveController({
  client,
  lease,
  capabilityLease,
  reloadLinks,
  reloadTags,
  reloadDomains,
  onRefreshCapabilities,
  flash,
}: UseSyncArchiveControllerOptions): UseSyncArchiveControllerResult {
  const [subscriptionSyncRequest, setSubscriptionSyncRequest] = useState(0)
  const [librarySyncing, setLibrarySyncing] = useState(false)
  const [librarySyncFailures, setLibrarySyncFailures] = useState<LibrarySyncResource[]>([])
  const [archiveDownloading, setArchiveDownloading] = useState(false)
  const librarySyncInFlight = useRef<Promise<void> | null>(null)
  const librarySyncController = useRef<AbortController | null>(null)

  const thoughtSyncController = useMemo(
    () => (
      capabilityLease.isCurrent('annotations') &&
      typeof client.syncThoughts === 'function' &&
      typeof client.pushThoughtOps === 'function'
        ? getThoughtSyncController(lease, client)
        : null
    ),
    [capabilityLease, client, lease],
  )
  const subscribeThoughtSync = useCallback(
    (listener: () => void) => thoughtSyncController?.subscribe(listener) ?? (() => undefined),
    [thoughtSyncController],
  )
  const getThoughtSyncSnapshot = useCallback(
    () => thoughtSyncController?.getSnapshot() ?? EMPTY_THOUGHT_SYNC_SNAPSHOT,
    [thoughtSyncController],
  )
  const thoughtSyncSnapshot = useSyncExternalStore(
    subscribeThoughtSync,
    getThoughtSyncSnapshot,
    getThoughtSyncSnapshot,
  )
  useEffect(() => thoughtSyncController?.start(), [thoughtSyncController])

  const requestSubscriptionSync = useCallback(() => {
    onRefreshCapabilities?.()
    setSubscriptionSyncRequest((request) => request + 1)
  }, [onRefreshCapabilities])

  const syncLibraryAndThoughts = useCallback(() => {
    onRefreshCapabilities?.()
    if (librarySyncInFlight.current) return
    const ownership = lease.captureOwnership('synchronize Reader library')
    if (
      !lease.isOwnershipCurrent(ownership) ||
      !resourceStore.isIdentityActive(lease)
    ) return

    const controller = new AbortController()
    const abortForIdentity = () => controller.abort()
    ownership.operation.signal.addEventListener('abort', abortForIdentity, { once: true })
    librarySyncController.current = controller
    setLibrarySyncing(true)

    const run = (async () => {
      const [outcomes, thoughtOutcome] = await Promise.all([
        Promise.allSettled([
          startLibraryReload(() => reloadLinks({ signal: controller.signal })),
          startLibraryReload(() => reloadTags({ signal: controller.signal })),
          startLibraryReload(() => reloadDomains({ signal: controller.signal })),
        ]),
        thoughtSyncController
          ? thoughtSyncController.sync().then(
              (result) => ({ ok: true as const, result }),
              () => ({ ok: false as const }),
            )
          : Promise.resolve(null),
      ])
      if (
        controller.signal.aborted ||
        !lease.isOwnershipCurrent(ownership) ||
        !resourceStore.isIdentityActive(lease)
      ) return

      const failures = LIBRARY_SYNC_RESOURCES.filter((_, index) =>
        libraryReloadFailed(outcomes[index] as PromiseSettledResult<ApiResult<unknown> | null>),
      )
      setLibrarySyncFailures(failures)

      let libraryMessage = '资料库已同步'
      for (const outcome of outcomes) {
        if (outcome.status === 'rejected' || outcome.value === null) {
          libraryMessage = '资料库同步失败'
          break
        }
        if (!outcome.value.ok) {
          libraryMessage = `资料库同步失败：${outcome.value.error.message}`
          break
        }
      }

      const thoughtSnapshot = thoughtSyncController?.getSnapshot()
      const thoughtStale = thoughtOutcome?.ok === true && thoughtOutcome.result.status === 'stale'
      if (!thoughtSnapshot || thoughtStale) {
        if (failures.length === 0) flash(libraryMessage, 'refresh')
        return
      }
      const thoughtFailed = thoughtOutcome?.ok === false ||
        thoughtOutcome?.result.status === 'failed' || thoughtSnapshot.phase === 'failed'
      flash(
        `${libraryMessage}；${thoughtSyncOutcome(thoughtSnapshot)}`,
        failures.length > 0 || thoughtFailed ? 'alert' : 'refresh',
      )
    })()
    librarySyncInFlight.current = run
    const finish = () => {
      ownership.operation.signal.removeEventListener('abort', abortForIdentity)
      if (librarySyncInFlight.current !== run) return
      librarySyncInFlight.current = null
      librarySyncController.current = null
      if (
        lease.isOwnershipCurrent(ownership) &&
        resourceStore.isIdentityActive(lease)
      ) {
        setLibrarySyncing(false)
      }
    }
    void run.then(finish, finish)
  }, [
    flash,
    lease,
    onRefreshCapabilities,
    reloadDomains,
    reloadLinks,
    reloadTags,
    thoughtSyncController,
  ])

  useEffect(() => () => {
    librarySyncController.current?.abort()
    librarySyncController.current = null
    librarySyncInFlight.current = null
  }, [lease])

  const downloadArchive = useCallback(async (selection: ArchiveV2Selection): Promise<boolean> => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('archiveDownload')) return false
    setArchiveDownloading(true)
    let objectURL: string | null = null
    let anchor: HTMLAnchorElement | null = null
    try {
      const result = await client.downloadArchiveV2(selection)
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('archiveDownload')) {
        return false
      }
      if (!result.ok) {
        flash(`归档下载失败：${result.error.message}`, 'alert')
        return false
      }

      // The API client has already verified the original response bytes. Do
      // not create an object URL until that succeeds, and release it as soon
      // as the browser receives the click.
      objectURL = URL.createObjectURL(result.data)
      anchor = document.createElement('a')
      anchor.href = objectURL
      anchor.download = `webtag-archive-v2-${new Date().toISOString().slice(0, 10)}.json`
      document.body.appendChild(anchor)
      anchor.click()
      flash('归档已下载', 'download')
      return true
    } catch (cause) {
      if (!client.isIdentityCurrent() || !operationLease.isCurrent('archiveDownload')) {
        return false
      }
      const message = cause instanceof Error ? cause.message : '下载请求未完成'
      flash(`归档下载失败：${message}`, 'alert')
      return false
    } finally {
      anchor?.remove()
      if (objectURL) URL.revokeObjectURL(objectURL)
      if (client.isIdentityCurrent() && operationLease.isCurrent('archiveDownload')) {
        setArchiveDownloading(false)
      }
    }
  }, [capabilityLease, client, flash])

  useEffect(() => {
    if (capabilityLease.policy.archiveDownload) return
    setArchiveDownloading(false)
  }, [capabilityLease])

  return {
    librarySyncing,
    librarySyncFailures,
    thoughtSync: thoughtSyncController ? thoughtSyncSnapshot : null,
    syncLibraryAndThoughts,
    subscriptionSyncRequest,
    requestSubscriptionSync,
    archiveDownloading,
    downloadArchive,
  }
}
