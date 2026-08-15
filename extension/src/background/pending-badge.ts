/**
 * Reader Inbox pending-count badge.
 *
 * The count is a global action badge. RSS discovery deliberately uses a
 * tab-scoped badge, so updating this badge does not replace an RSS badge on a
 * particular tab.
 */

import { buildWebTagClientFromSettings } from '@/api/webtag-client'
import { getWebTagSettings } from '@/composables/useSettings'

export const PENDING_BADGE_STORAGE_KEY = 'webtag_pending_inbox_count'
export const PENDING_BADGE_STORAGE_PREFIX = `${PENDING_BADGE_STORAGE_KEY}:v2:`
export const PENDING_BADGE_COLOR = '#b45309'

export interface PendingBadgeStorage {
  get: (key: string) => Promise<Record<string, unknown>>
  set: (items: Record<string, unknown>) => Promise<void>
}

export interface PendingBadgeDeps {
  buildClient: () => Promise<{
    getReaderPendingCount?: () => Promise<
      { ok: true; data: number | null } | { ok: false; error: unknown }
    >
  } | null>
  storage: PendingBadgeStorage
  /** Resolve the current installation bucket; tests may inject a deterministic key. */
  getStorageKey?: () => Promise<string>
  setBadgeText: (details: { text: string }) => Promise<void> | void
  setBadgeBackgroundColor: (details: { color: string }) => Promise<void> | void
}

function normalizeCount(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) {
    return null
  }
  return value
}

function badgeText(count: number): string {
  if (count <= 0) return ''
  return count > 99 ? '99+' : String(count)
}

export class PendingBadgeController {
  private refreshPromise: Promise<void> | null = null
  private refreshQueued = false
  private activeStorageKey: string | null = null

  constructor(private readonly deps: PendingBadgeDeps) {}

  /** Restore immediately, then refresh from the backend on worker startup. */
  async start(): Promise<void> {
    await this.restore()
    await this.refresh()
  }

  async restore(): Promise<void> {
    const storageKey = await this.ensureStorageKey()
    let count: number | null = null
    try {
      const stored = await this.deps.storage.get(storageKey)
      count = normalizeCount(stored[storageKey])
    } catch {
      // A missing/invalid cache is equivalent to no pending items.
    }
    await this.applyCount(count ?? 0)
  }

  private async ensureStorageKey(): Promise<string> {
    let storageKey = PENDING_BADGE_STORAGE_KEY
    try {
      storageKey =
        (await this.deps.getStorageKey?.()) ?? PENDING_BADGE_STORAGE_KEY
    } catch {
      // Keep the last known bucket if identity storage is temporarily unreadable.
      storageKey = this.activeStorageKey ?? PENDING_BADGE_STORAGE_KEY
    }
    if (this.activeStorageKey !== storageKey) {
      const changed = this.activeStorageKey !== null
      this.activeStorageKey = storageKey
      if (changed) await this.applyCount(0)
    }
    return storageKey
  }

  /** Refresh once at a time so an older response cannot overwrite a newer one. */
  refresh(): Promise<void> {
    if (this.refreshPromise) {
      this.refreshQueued = true
      return this.refreshPromise
    }
    const task = this.refreshFromBackend()
    const wrapped = task.then(
      () => {
        if (this.refreshPromise === wrapped) this.refreshPromise = null
        this.startQueuedRefresh()
      },
      () => {
        if (this.refreshPromise === wrapped) this.refreshPromise = null
        this.startQueuedRefresh()
      },
    )
    this.refreshPromise = wrapped
    return wrapped
  }

  private startQueuedRefresh(): void {
    if (!this.refreshQueued) return
    this.refreshQueued = false
    void this.refresh()
  }

  private async refreshFromBackend(): Promise<void> {
    const storageKey = await this.ensureStorageKey()
    let client: Awaited<ReturnType<PendingBadgeDeps['buildClient']>>
    try {
      client = await this.deps.buildClient()
    } catch {
      // Settings/storage failures are transient; keep the restored badge.
      return
    }
    if (!client || typeof client.getReaderPendingCount !== 'function') {
      await this.persistCount(0, storageKey)
      return
    }

    let result:
      { ok: true; data: number | null } | { ok: false; error: unknown }
    try {
      result = await client.getReaderPendingCount()
    } catch {
      return
    }
    if (!result.ok) return

    // `null` is the client's explicit old-backend/404 compatibility signal.
    const count = normalizeCount(result.data) ?? 0
    await this.persistCount(count, storageKey)
  }

  private async persistCount(
    count: number,
    expectedStorageKey: string,
  ): Promise<void> {
    // Do not let a response for one installation overwrite another after a
    // settings change while the request was in flight.
    const currentStorageKey = await this.ensureStorageKey()
    if (currentStorageKey !== expectedStorageKey) return
    try {
      await this.deps.storage.set({ [expectedStorageKey]: count })
    } catch {
      // The visible badge remains useful even when the cache cannot be saved.
    }
    await this.applyCount(count)
  }

  private async applyCount(count: number): Promise<void> {
    try {
      if (count > 0) {
        await this.deps.setBadgeBackgroundColor({ color: PENDING_BADGE_COLOR })
      }
      await this.deps.setBadgeText({ text: badgeText(count) })
    } catch {
      // Tabs/workers may disappear while an action API call is in flight.
    }
  }
}

export function createProductionPendingBadgeController(): PendingBadgeController {
  return new PendingBadgeController({
    buildClient: async () =>
      buildWebTagClientFromSettings(await getWebTagSettings()),
    storage: {
      get: (key) => chrome.storage.local.get(key),
      set: (items) => chrome.storage.local.set(items),
    },
    getStorageKey: createProductionPendingBadgeStorageKey,
    setBadgeText: (details) => chrome.action.setBadgeText(details),
    setBadgeBackgroundColor: (details) =>
      chrome.action.setBadgeBackgroundColor(details),
  })
}

/**
 * Build an installation-specific storage bucket without placing the bearer
 * token itself in the key.
 */
async function createProductionPendingBadgeStorageKey(): Promise<string> {
  const settings = await getWebTagSettings()
  const backend = settings.backendUrl.trim().replace(/\/+$/, '')
  const identity = `${backend}|${settings.accessToken.trim()}`
  return `${PENDING_BADGE_STORAGE_PREFIX}${await digestIdentity(identity)}`
}

async function digestIdentity(value: string): Promise<string> {
  try {
    const subtle = globalThis.crypto?.subtle
    if (subtle && typeof TextEncoder !== 'undefined') {
      const digest = await subtle.digest(
        'SHA-256',
        new TextEncoder().encode(value),
      )
      return Array.from(new Uint8Array(digest), (byte) =>
        byte.toString(16).padStart(2, '0'),
      ).join('')
    }
  } catch {
    // Fall through to a deterministic non-secret hash for older runtimes.
  }

  let hash = 2166136261
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index)
    hash = Math.imul(hash, 16777619)
  }
  return `fnv-${(hash >>> 0).toString(16).padStart(8, '0')}`
}
