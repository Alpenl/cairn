import { type IdentityLease, readerIdentity } from './identity'

export type StorageOwnership =
  | 'device'
  | 'installation-cache'
  | 'installation-user-data'
  | 'connection'

interface LocalStorageDefinition {
  readonly storage: 'localStorage'
  readonly key: string
  readonly keyKind: 'exact' | 'prefix'
  readonly ownership: StorageOwnership
}

interface IndexedDBDefinition {
  readonly storage: 'indexedDB'
  readonly key: string
  readonly keyKind: 'database'
  readonly ownership: Extract<
    StorageOwnership,
    'installation-cache' | 'installation-user-data'
  >
}

type StorageDefinition = LocalStorageDefinition | IndexedDBDefinition

export const storageOwnershipRegistry = {
  annotationsV1: {
    storage: 'localStorage',
    key: 'webtag:annotations:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  annotationsV2: {
    storage: 'localStorage',
    key: 'webtag:annotations:v2',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  annotationWakeup: {
    storage: 'localStorage',
    key: 'webtag:reader:annotations:wakeup:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  thoughtDevice: {
    storage: 'localStorage',
    key: 'webtag:reader:thought-device:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  pins: {
    storage: 'localStorage',
    key: 'webtag:pins:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  revisionFloor: {
    storage: 'localStorage',
    key: 'webtag:content-revision-floor',
    keyKind: 'exact',
    ownership: 'installation-cache',
  },
  feedSelection: {
    storage: 'localStorage',
    key: 'webtag:reader:feed-selection:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  collapsedFeedFolders: {
    storage: 'localStorage',
    key: 'webtag:reader:collapsed-feed-folders:v1',
    keyKind: 'exact',
    ownership: 'installation-user-data',
  },
  sidebarFold: {
    storage: 'localStorage',
    key: 'webtag:sbfold:',
    keyKind: 'prefix',
    ownership: 'installation-user-data',
  },
  theme: {
    storage: 'localStorage',
    key: 'webtag:theme',
    keyKind: 'exact',
    ownership: 'device',
  },
  readingPreference: {
    storage: 'localStorage',
    key: 'webtag:reading-preference',
    keyKind: 'exact',
    ownership: 'device',
  },
  sidebarCollapsed: {
    storage: 'localStorage',
    key: 'webtag:sidebar-collapsed',
    keyKind: 'exact',
    ownership: 'device',
  },
  connection: {
    storage: 'localStorage',
    key: 'webtag:reader:conn:v2',
    keyKind: 'exact',
    ownership: 'connection',
  },
  cacheDatabase: {
    storage: 'indexedDB',
    key: 'webtag-reader-cache',
    keyKind: 'database',
    ownership: 'installation-cache',
  },
  userDataDatabase: {
    storage: 'indexedDB',
    key: 'webtag-reader-user-data',
    keyKind: 'database',
    ownership: 'installation-user-data',
  },
} as const satisfies Record<string, StorageDefinition>

type Registry = typeof storageOwnershipRegistry
export type OwnedStorageID = {
  [ID in keyof Registry]: Registry[ID]['storage'] extends 'localStorage' ? ID : never
}[keyof Registry]
export type OwnedDatabaseID = {
  [ID in keyof Registry]: Registry[ID]['storage'] extends 'indexedDB' ? ID : never
}[keyof Registry]

export interface LegacyStorageEntry {
  readonly key: string
  readonly value: string
}

export function ownedStorageKeyForLease(
  id: OwnedStorageID,
  lease: IdentityLease | null,
  suffix?: string,
): string | null {
  const definition = storageOwnershipRegistry[id] as LocalStorageDefinition
  if (definition.keyKind === 'prefix' && suffix === undefined) return null
  const logicalKey = definition.keyKind === 'prefix' ? `${definition.key}${suffix}` : definition.key
  if (definition.ownership === 'device' || definition.ownership === 'connection') {
    return logicalKey
  }

  if (!lease) return null
  const ownership = lease.capture(`localStorage ${logicalKey}`)
  if (!lease.isCurrent(ownership)) return null
  return `${logicalKey}:namespace:${ownership.physicalNamespace}`
}

export function ownedStorageKey(id: OwnedStorageID, suffix?: string): string | null {
  return ownedStorageKeyForLease(id, readerIdentity.activeLease, suffix)
}

export function ownedDatabaseName(id: OwnedDatabaseID): string {
  return storageOwnershipRegistry[id].key
}

/**
 * Return only pre-RF2B unscoped values. Namespaced values that share a legacy
 * prefix are deliberately excluded so migration can never claim or discard
 * data already owned by an identity.
 */
export function listLegacyStorageEntries(id: OwnedStorageID): LegacyStorageEntry[] {
  const definition = storageOwnershipRegistry[id] as LocalStorageDefinition
  try {
    if (definition.keyKind === 'exact') {
      const value = localStorage.getItem(definition.key)
      return value === null ? [] : [{ key: definition.key, value }]
    }

    const entries: LegacyStorageEntry[] = []
    for (let index = 0; index < localStorage.length; index += 1) {
      const key = localStorage.key(index)
      if (!key || !key.startsWith(definition.key) || key.includes(':namespace:')) continue
      const value = localStorage.getItem(key)
      if (value !== null) entries.push({ key, value })
    }
    return entries.sort((a, b) => a.key.localeCompare(b.key))
  } catch {
    return []
  }
}

export function removeLegacyStorageEntries(entries: readonly LegacyStorageEntry[]): number {
  let removed = 0
  for (const entry of entries) {
    try {
      if (localStorage.getItem(entry.key) !== entry.value) continue
      localStorage.removeItem(entry.key)
      removed += 1
    } catch {
      // A failed comparison or removal leaves the unattributed value available for a retry.
    }
  }
  return removed
}

export function readOwnedStorage(id: OwnedStorageID, suffix?: string): string | null {
  return readOwnedStorageForLease(id, readerIdentity.activeLease, suffix)
}

export type CheckedStorageRead =
  | { readonly ok: true; readonly value: string | null }
  | { readonly ok: false }

export function readOwnedStorageChecked(
  id: OwnedStorageID,
  suffix?: string,
): CheckedStorageRead {
  const key = ownedStorageKey(id, suffix)
  if (!key) return { ok: false }
  try {
    return { ok: true, value: localStorage.getItem(key) }
  } catch {
    return { ok: false }
  }
}

export function readOwnedStorageForLease(
  id: OwnedStorageID,
  lease: IdentityLease | null,
  suffix?: string,
): string | null {
  const key = ownedStorageKeyForLease(id, lease, suffix)
  if (!key) return null
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

export function writeOwnedStorage(id: OwnedStorageID, value: string, suffix?: string): boolean {
  return writeOwnedStorageForLease(id, value, readerIdentity.activeLease, suffix)
}

export function writeOwnedStorageForLease(
  id: OwnedStorageID,
  value: string,
  lease: IdentityLease | null,
  suffix?: string,
): boolean {
  const key = ownedStorageKeyForLease(id, lease, suffix)
  if (!key) return false
  try {
    localStorage.setItem(key, value)
    return true
  } catch {
    // Installation state remains usable in memory when storage is unavailable.
    return false
  }
}

export function removeOwnedStorage(id: OwnedStorageID, suffix?: string): boolean {
  const key = ownedStorageKey(id, suffix)
  if (!key) return false
  try {
    localStorage.removeItem(key)
    return true
  } catch {
    // Removal is best-effort for unavailable browser storage.
    return false
  }
}
