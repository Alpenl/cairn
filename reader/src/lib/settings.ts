/**
 * Connection persistence and cross-tab ownership.
 *
 * Durable rows never contain an installation token. Every mutation is
 * serialized across tabs, revisioned, and read back before it becomes live.
 */
import { useEffect, useState } from 'react'
import { withBrowserLock } from './browser-lock'
import {
  ownedStorageKey,
  readOwnedStorageChecked,
  removeOwnedStorage,
  writeOwnedStorage,
} from './storage-ownership'

export type AuthMode = 'session' | 'installation-token'

export interface Connection {
  baseURL: string
  mode: AuthMode
  /** Only installation-token mode uses this field; it is never durable. */
  installationToken: string
}

interface DurableConnection {
  readonly baseURL: string
  readonly mode: AuthMode
  readonly installationToken: string
  readonly revision?: string
}

interface VolatileCredential {
  readonly baseURL: string
  readonly token: string
  readonly durableRevision: string
  upgradeRequired: boolean
}

export type ConnectionStorageState =
  | { readonly phase: 'loading' }
  | { readonly phase: 'ready' }
  | { readonly phase: 'error'; readonly message: string }

interface ConnectionSnapshot {
  readonly connection: Connection
  readonly storage: ConnectionStorageState
}

class ConnectionStorageError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ConnectionStorageError'
  }
}

const EMPTY: Connection = {
  baseURL: '',
  mode: 'installation-token',
  installationToken: '',
}
const CONNECTION_LOCK = 'webtag-reader:connection:v2'
const listeners = new Set<(snapshot: ConnectionSnapshot) => void>()
let volatileCredential: VolatileCredential | null = null

function normalizeBaseURL(value: string): string {
  return value.trim().replace(/\/+$/, '')
}

function isDurableConnection(value: unknown): value is DurableConnection {
  if (typeof value !== 'object' || value === null) return false
  const row = value as Record<string, unknown>
  return typeof row.baseURL === 'string' &&
    (row.mode === 'session' || row.mode === 'installation-token') &&
    typeof row.installationToken === 'string' &&
    (row.revision === undefined || (typeof row.revision === 'string' && row.revision !== ''))
}

function parseDurableConnection(raw: string | null): DurableConnection | null {
  if (raw === null) return null
  try {
    const value: unknown = JSON.parse(raw)
    return isDurableConnection(value) ? value : null
  } catch {
    return null
  }
}

function readDurableConnection(): DurableConnection | null {
  const result = readOwnedStorageChecked('connection')
  if (!result.ok) throw new ConnectionStorageError('无法读取连接存储')
  return parseDurableConnection(result.value)
}

function nextRevision(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('')
}

function durableFor(connection: Connection): DurableConnection {
  return {
    baseURL: normalizeBaseURL(connection.baseURL),
    mode: connection.mode,
    installationToken: '',
    revision: nextRevision(),
  }
}

function writeAndVerify(record: DurableConnection): void {
  const serialized = JSON.stringify(record)
  if (!writeOwnedStorage('connection', serialized)) {
    throw new ConnectionStorageError('无法写入连接存储')
  }
  const verified = readOwnedStorageChecked('connection')
  if (!verified.ok || verified.value !== serialized) {
    throw new ConnectionStorageError('连接存储写入后校验失败')
  }
}

function removeAndVerify(): void {
  if (!removeOwnedStorage('connection')) {
    throw new ConnectionStorageError('无法清除连接存储')
  }
  const verified = readOwnedStorageChecked('connection')
  if (!verified.ok || verified.value !== null) {
    throw new ConnectionStorageError('连接存储清除后校验失败')
  }
}

function candidateMatches(record: DurableConnection, candidate: VolatileCredential): boolean {
  return record.mode === 'installation-token' &&
    record.installationToken === '' &&
    record.revision === candidate.durableRevision &&
    normalizeBaseURL(record.baseURL) === candidate.baseURL
}

function snapshotFromRecord(record: DurableConnection | null): ConnectionSnapshot {
  if (record === null) {
    volatileCredential = null
    return { connection: EMPTY, storage: { phase: 'ready' } }
  }
  if (record.installationToken !== '') {
    volatileCredential = null
    return { connection: EMPTY, storage: { phase: 'loading' } }
  }

  const baseURL = normalizeBaseURL(record.baseURL)
  const candidate = volatileCredential
  if (candidate && !candidateMatches(record, candidate)) volatileCredential = null
  return {
    connection: {
      baseURL,
      mode: record.mode,
      installationToken:
        record.mode === 'installation-token' && volatileCredential?.baseURL === baseURL
          ? volatileCredential.token
          : '',
    },
    storage: { phase: 'ready' },
  }
}

function inspectConnection(): ConnectionSnapshot {
  try {
    return snapshotFromRecord(readDurableConnection())
  } catch (error) {
    volatileCredential = null
    return storageFailure(error)
  }
}

function storageFailure(error: unknown): ConnectionSnapshot {
  return {
    connection: EMPTY,
    storage: {
      phase: 'error',
      message: error instanceof Error ? error.message : '连接存储不可用',
    },
  }
}

function publish(snapshot: ConnectionSnapshot): void {
  for (const listener of listeners) listener(snapshot)
}

function publishFailure(error: unknown): ConnectionStorageError {
  volatileCredential = null
  const failure = error instanceof ConnectionStorageError
    ? error
    : new ConnectionStorageError(error instanceof Error ? error.message : '连接存储不可用')
  publish(storageFailure(failure))
  return failure
}

/** Scrub a historical durable token before exposing it to application code. */
export async function initializeConnection(): Promise<ConnectionSnapshot> {
  return withBrowserLock(CONNECTION_LOCK, async () => {
    try {
      const record = readDurableConnection()
      if (record === null || record.installationToken === '') {
        const snapshot = snapshotFromRecord(record)
        publish(snapshot)
        return snapshot
      }

      const baseURL = normalizeBaseURL(record.baseURL)
      const token = record.installationToken.trim()
      const scrubbed = durableFor({ baseURL, mode: record.mode, installationToken: '' })
      writeAndVerify(scrubbed)
      volatileCredential = record.mode === 'installation-token' && token !== ''
        ? {
            baseURL,
            token,
            durableRevision: scrubbed.revision!,
            upgradeRequired: true,
          }
        : null
      const snapshot = snapshotFromRecord(scrubbed)
      publish(snapshot)
      return snapshot
    } catch (error) {
      const snapshot = storageFailure(publishFailure(error))
      return snapshot
    }
  })
}

/** Re-read a storage-event update, scrubbing old-format credentials under lock. */
async function synchronizeConnectionFromStorage(): Promise<ConnectionSnapshot> {
  const snapshot = inspectConnection()
  publish(snapshot)
  return snapshot.storage.phase === 'loading' ? initializeConnection() : snapshot
}

export function getConnection(): Connection {
  return inspectConnection().connection
}

export function hasConnection(): boolean {
  return getConnection().baseURL !== ''
}

export async function saveConnection(conn: Connection): Promise<Connection> {
  return withBrowserLock(CONNECTION_LOCK, async () => {
    const normalized: Connection = {
      baseURL: normalizeBaseURL(conn.baseURL),
      mode: conn.mode,
      installationToken: conn.mode === 'session' ? '' : conn.installationToken.trim(),
    }
    try {
      readDurableConnection()
      const durable = durableFor(normalized)
      writeAndVerify(durable)
      volatileCredential = normalized.mode === 'installation-token' && normalized.installationToken !== ''
        ? {
            baseURL: normalized.baseURL,
            token: normalized.installationToken,
            durableRevision: durable.revision!,
            upgradeRequired: false,
          }
        : null
      publish({ connection: normalized, storage: { phase: 'ready' } })
      return normalized
    } catch (error) {
      throw publishFailure(error)
    }
  })
}

export async function clearConnection(): Promise<void> {
  return withBrowserLock(CONNECTION_LOCK, async () => {
    try {
      readDurableConnection()
      removeAndVerify()
      volatileCredential = null
      publish({ connection: EMPTY, storage: { phase: 'ready' } })
    } catch (error) {
      throw publishFailure(error)
    }
  })
}

export function needsLegacySessionUpgrade(conn: Connection): boolean {
  const candidate = volatileCredential
  return candidate !== null &&
    candidate.upgradeRequired &&
    conn.mode === 'installation-token' &&
    conn.baseURL === candidate.baseURL &&
    conn.installationToken === candidate.token
}

/** Revision-checked commit used while the caller still owns the session mutation lock. */
export async function completeLegacySessionUpgrade(conn: Connection): Promise<boolean> {
  return withBrowserLock(CONNECTION_LOCK, async () => {
    const candidate = volatileCredential
    if (!candidate || !needsLegacySessionUpgrade(conn)) return false
    try {
      const current = readDurableConnection()
      if (!current || !candidateMatches(current, candidate)) {
        const snapshot = snapshotFromRecord(current)
        publish(snapshot)
        return false
      }
      const session = durableFor({
        baseURL: candidate.baseURL,
        mode: 'session',
        installationToken: '',
      })
      writeAndVerify(session)
      volatileCredential = null
      publish({
        connection: {
          baseURL: candidate.baseURL,
          mode: 'session',
          installationToken: '',
        },
        storage: { phase: 'ready' },
      })
      return true
    } catch (error) {
      throw publishFailure(error)
    }
  })
}

/** Confirm compatibility only if another tab has not replaced the durable row. */
export async function confirmLegacyBearerCompatibility(conn: Connection): Promise<boolean> {
  return withBrowserLock(CONNECTION_LOCK, async () => {
    const candidate = volatileCredential
    if (!candidate || !needsLegacySessionUpgrade(conn)) return false
    try {
      const current = readDurableConnection()
      if (!current || !candidateMatches(current, candidate)) {
        const snapshot = snapshotFromRecord(current)
        publish(snapshot)
        return false
      }
      candidate.upgradeRequired = false
      return true
    } catch (error) {
      throw publishFailure(error)
    }
  })
}

export function useConnection(): [
  Connection,
  (connection: Connection) => Promise<Connection>,
  () => Promise<void>,
  ConnectionStorageState,
] {
  const [snapshot, setSnapshot] = useState<ConnectionSnapshot>(inspectConnection)
  useEffect(() => {
    listeners.add(setSnapshot)
    const connectionKey = ownedStorageKey('connection')
    const onStorage = (event: StorageEvent) => {
      if (event.key === null || event.key === connectionKey) {
        void synchronizeConnectionFromStorage()
      }
    }
    window.addEventListener('storage', onStorage)
    if (snapshot.storage.phase === 'loading') void initializeConnection()
    return () => {
      listeners.delete(setSnapshot)
      window.removeEventListener('storage', onStorage)
    }
    // Initialization is driven by the first durable snapshot; later legacy
    // writes arrive through the storage listener and call synchronize directly.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return [snapshot.connection, saveConnection, clearConnection, snapshot.storage]
}
