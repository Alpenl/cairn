/**
 * IndexedDB 最小封装（PF4）。
 *
 * 缓存数据分为 payload、metadata 和 durable control，并在原生 multi-store
 * transaction 中维持原子性。这里仍使用小型封装而不引入 idb，以维持
 * Reader 的精简运行时依赖面。
 *
 * **全部操作 fail-soft**：无痕模式、配额耗尽、用户禁用存储都会让这里的调用
 * 失败，而缓存持久化是纯优化。任何失败都退化为「这次没存上 / 没读到」，
 * 绝不能让应用崩掉或卡住。
 */

import { ownedDatabaseName } from '../storage-ownership'
import {
  abortIDBTransaction,
  collectIDBRequestList,
  collectIDBRequestResults,
  requestToResult,
  runIDBTransaction,
} from '../idb-core'
import type { NamespaceStorageOperation } from './io-queue'

const LEGACY_STORE_NAME = 'resources'
export const PAYLOAD_STORE_NAME = 'cache_payload'
export const META_STORE_NAME = 'cache_meta'
const CONTROL_STORE_NAME = 'cache_control'
export const CACHE_DATABASE_VERSION = 3

/**
 * 结构版本。DTO 形状变化时手动 +1：版本不匹配即删除当前身份分区中的旧记录。
 * RF2B 之前没有 ownership metadata 的全局记录会在首次权威 identity hydrate 时
 * 一次性清掉；其他 identity 的分区必须保留，等它自己 hydrate 时再处理。
 *
 * 没有它的话，一次后端字段调整会让旧结构的快照被 hydrate 进新代码里，
 * 渲染路径上到处是 undefined —— 而且只在"升级过的老用户"身上复现。
 *
 * 2：已保存原文换到独立的键命名空间（CONTENT_CACHE_PREFIX）。旧键仍能通过
 *    持久化前缀被 hydrate 回内存，却再也不会有人读——白占 LRU 名额，而且它们
 *    是最大的那批条目。清掉旧分区记录，比留着它们干净。
 * 3：译文换到独立的键命名空间（TRANSLATIONS_CACHE_PREFIX），与 2 同一个动作、
 *    同一个理由。旧键 `GET /api/links/{id}/translations` 仍落在
 *    PERSISTED_PREFIXES 的 `'GET /api/links'` 之下，不 bump 的话它们每次启动都
 *    被 hydrate 回来当僵尸，而三条清理路径没有一条可靠：旧 quota 投影的
 *    knownBytes 每 session 从 0 起算、看不见存量；LRU 淘汰不落盘；靠库级失效
 *    顺手删要三件事同时凑巧。
 * 5：译文持久化升级为来源身份 envelope：列表携带 current_content_revision，
 *    每条已保存正文译文携带 source_content_revision。v4 快照缺少这两级代次，
 *    hydrate 后无法证明译文属于当前正文，必须整批删除而不能当作当前译文恢复。
 * 6：译文缓存键带上正文代次（`…/translations?rev=<revision|unverified>`）。键格式
 *    变了而 schema 不变的话，v5 写下的旧键记录 schema 相同、不触发清理，会被
 *    hydrate 进内存却再也没人按新键读它——和 2/3 是同一类僵尸。改键就要 bump，
 *    没有例外。
 */
export const CACHE_SCHEMA_VERSION = 6

/** IndexedDB 打开超时。超时即放弃持久化，纯内存运行。 */
const OPEN_TIMEOUT_MS = 1000

export interface PersistedRecord {
  /** Physical IndexedDB primary key. */
  key: string
  /** Authoritative Reader physical namespace. */
  namespace: string
  /** ResourceStore key within the namespace. */
  logicalKey: string
  schema: number
  data: unknown
  updatedAt: number
  /** 估算字节数，用于容量治理。 */
  size: number
  /** Durable revalidation generation represented by this payload. */
  generation: number
}

export interface CachePayload {
  key: string
  data: unknown
}

export interface CacheMetadata {
  key: string
  namespace: string
  logicalKey: string
  schema: number
  size: number
  lastAccess: number
  generation: number
}

export interface OrphanRepairResult {
  payloadsRemoved: number
  metadataRemoved: number
}

export interface QuotaPutResult {
  stored: boolean
  mutated: boolean
  totalBytes: number
}

export interface DurableInvalidation {
  key: string
  kind: 'invalidation'
  namespace: string
  logicalPrefix: string
  generation: number
}

interface DurableGenerationClock {
  key: string
  kind: 'clock'
  namespace: string
  generation: number
}

export function persistedRecordKey(namespace: string, logicalKey: string): string {
  return `${namespace.length}:${namespace}:${logicalKey}`
}

export function createPersistedRecord(
  namespace: string,
  logicalKey: string,
  fields: Omit<PersistedRecord, 'key' | 'namespace' | 'logicalKey' | 'generation'> & {
    generation?: number
  },
): PersistedRecord {
  return {
    key: persistedRecordKey(namespace, logicalKey),
    namespace,
    logicalKey,
    ...fields,
    generation: fields.generation ?? 0,
  }
}

function namespaceRecordPrefix(namespace: string): string {
  return persistedRecordKey(namespace, '')
}

function namespaceRecordRange(namespace: string): IDBKeyRange {
  const prefix = namespaceRecordPrefix(namespace)
  return IDBKeyRange.bound(prefix, `${prefix}\uffff`)
}

function controlNamespacePrefix(namespace: string): string {
  return `invalidation:${namespace.length}:${namespace}:`
}

function controlNamespaceRange(namespace: string): IDBKeyRange {
  const prefix = controlNamespacePrefix(namespace)
  return IDBKeyRange.bound(prefix, `${prefix}\uffff`)
}

function clockKey(namespace: string): string {
  return `clock:${namespace.length}:${namespace}`
}

function invalidationKey(namespace: string, logicalPrefix: string): string {
  return `${controlNamespacePrefix(namespace)}${logicalPrefix}`
}

function isNamespacedRecordKey(key: IDBValidKey): boolean {
  if (typeof key !== 'string') return false
  const firstSeparator = key.indexOf(':')
  if (firstSeparator <= 0) return false
  const lengthText = key.slice(0, firstSeparator)
  if (!/^\d+$/.test(lengthText)) return false
  const namespaceLength = Number(lengthText)
  if (!Number.isSafeInteger(namespaceLength) || namespaceLength <= 0) return false
  return key[firstSeparator + 1 + namespaceLength] === ':'
}

let dbPromise: Promise<IDBDatabase | null> | null = null

function openDatabase(): Promise<IDBDatabase | null> {
  if (typeof indexedDB === 'undefined') return Promise.resolve(null)

  return new Promise((resolve) => {
    let settled = false
    const finish = (value: IDBDatabase | null) => {
      if (settled) {
        value?.close()
        return
      }
      settled = true
      resolve(value)
    }
    // 超时兜底：某些浏览器在存储被阻塞时 open 既不 success 也不 error，
    // 不设超时会让首屏 hydrate 永远等下去。
    const timer = setTimeout(() => finish(null), OPEN_TIMEOUT_MS)

    let request: IDBOpenDBRequest
    try {
      request = indexedDB.open(ownedDatabaseName('cacheDatabase'), CACHE_DATABASE_VERSION)
    } catch {
      clearTimeout(timer)
      finish(null)
      return
    }
    request.onupgradeneeded = () => {
      const db = request.result
      const transaction = request.transaction
      if (!transaction) {
        request.result.close()
        return
      }
      try {
        if (!db.objectStoreNames.contains(LEGACY_STORE_NAME)) {
          db.createObjectStore(LEGACY_STORE_NAME, { keyPath: 'key' })
        }
        if (!db.objectStoreNames.contains(CONTROL_STORE_NAME)) {
          db.createObjectStore(CONTROL_STORE_NAME, { keyPath: 'key' })
        }
        const payloads = db.objectStoreNames.contains(PAYLOAD_STORE_NAME)
          ? transaction.objectStore(PAYLOAD_STORE_NAME)
          : db.createObjectStore(PAYLOAD_STORE_NAME, { keyPath: 'key' })
        const metadata = db.objectStoreNames.contains(META_STORE_NAME)
          ? transaction.objectStore(META_STORE_NAME)
          : db.createObjectStore(META_STORE_NAME, { keyPath: 'key' })

        const legacy = transaction.objectStore(LEGACY_STORE_NAME)
        const cursorRequest = legacy.openCursor()
        cursorRequest.onerror = () => abortIDBTransaction(transaction)
        cursorRequest.onsuccess = () => {
          try {
            const cursor = cursorRequest.result
            if (!cursor) return
            const record = cursor.value as Partial<PersistedRecord>
            if (
              typeof record.key === 'string' &&
              typeof record.namespace === 'string' &&
              typeof record.logicalKey === 'string' &&
              record.key === persistedRecordKey(record.namespace, record.logicalKey)
            ) {
              const lastAccess = typeof record.updatedAt === 'number' ? record.updatedAt : 0
              const payload: CachePayload = { key: record.key, data: record.data }
              const meta: CacheMetadata = {
                key: record.key,
                namespace: record.namespace,
                logicalKey: record.logicalKey,
                schema: typeof record.schema === 'number' ? record.schema : 0,
                size: typeof record.size === 'number' ? record.size : 0,
                lastAccess,
                generation: typeof record.generation === 'number' ? record.generation : 0,
              }
              payloads.put(payload)
              metadata.put(meta)
            }
            cursor.delete()
            cursor.continue()
          } catch {
            abortIDBTransaction(transaction)
          }
        }
      } catch {
        abortIDBTransaction(transaction)
      }
    }
    request.onsuccess = () => {
      clearTimeout(timer)
      request.result.onversionchange = () => request.result.close()
      finish(request.result)
    }
    request.onerror = () => {
      clearTimeout(timer)
      finish(null)
    }
    request.onblocked = () => {
      clearTimeout(timer)
      finish(null)
    }
  })
}

function database(): Promise<IDBDatabase | null> {
  if (!dbPromise) dbPromise = openDatabase()
  return dbPromise
}

/** 仅测试用：丢弃已缓存的连接，让下一次调用重新 open。 */
export function resetDatabaseHandle(): void {
  void dbPromise?.then((db) => db?.close())
  dbPromise = null
}

function runTransaction<T>(
  mode: IDBTransactionMode,
  work: (store: IDBObjectStore) => IDBRequest<T>,
  operation?: NamespaceStorageOperation,
  afterSuccess?: (result: T, store: IDBObjectStore) => void,
  storeName = META_STORE_NAME,
): Promise<T | null> {
  return database()
    .then((db) => runIDBTransaction<T>(
      db,
      [storeName],
      mode,
      ({ transaction, setResult }) => {
        const store = transaction.objectStore(storeName)
        requestToResult(transaction, work(store), (result) => {
          afterSuccess?.(result, store)
          setResult(result)
        })
      },
      {
        signal: operation?.signal,
        isCurrent: operation ? () => operation.isCurrent() : undefined,
      },
    ))
    .then((result) => result.ok ? result.value : null)
}

function runMultiStoreTransaction<T>(
  storeNames: readonly string[],
  mode: IDBTransactionMode,
  operation: NamespaceStorageOperation | undefined,
  work: (transaction: IDBTransaction, setResult: (result: T) => void) => void,
): Promise<T | null> {
  return database()
    .then((db) => runIDBTransaction<T>(
      db,
      storeNames,
      mode,
      ({ transaction, setResult }) => work(transaction, setResult),
      {
        signal: operation?.signal,
        isCurrent: operation ? () => operation.isCurrent() : undefined,
      },
    ))
    .then((result) => result.ok ? result.value : null)
}

export function idbGetAll(operation?: NamespaceStorageOperation): Promise<PersistedRecord[]> {
  return runMultiStoreTransaction<PersistedRecord[]>(
    [PAYLOAD_STORE_NAME, META_STORE_NAME],
    'readonly',
    operation,
    (transaction, setResult) => {
      const metadata = transaction.objectStore(META_STORE_NAME)
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME)
      const metaRequest = (operation
        ? metadata.getAll(namespaceRecordRange(operation.context.physicalNamespace))
        : metadata.getAll()) as IDBRequest<CacheMetadata[]>
      requestToResult(transaction, metaRequest, (rows) => {
        if (rows.length === 0) {
          setResult([])
          return
        }
        const payloadRequests = rows.map((meta) =>
          payloads.get(meta.key) as IDBRequest<CachePayload | undefined>)
        collectIDBRequestList(transaction, payloadRequests, (payloadRows) => {
          const records = rows.flatMap((meta, index): PersistedRecord[] => {
            const payload = payloadRows[index]
            return payload
              ? [{
                key: meta.key,
                namespace: meta.namespace,
                logicalKey: meta.logicalKey,
                schema: meta.schema,
                data: payload.data,
                updatedAt: meta.lastAccess,
                size: meta.size,
                generation: meta.generation,
              }]
              : []
          })
          setResult(records.sort((a, b) => a.key.localeCompare(b.key)))
        })
      })
    },
  ).then((records) => records ?? [])
}

export function idbGetMetadata(
  operation?: NamespaceStorageOperation,
): Promise<CacheMetadata[]> {
  return runTransaction<CacheMetadata[]>(
    'readonly',
    (store) => operation
      ? store.getAll(namespaceRecordRange(operation.context.physicalNamespace))
      : store.getAll(),
    operation,
    undefined,
    META_STORE_NAME,
  ).then((records) => records ?? [])
}

export function idbGetInvalidations(
  operation: NamespaceStorageOperation,
): Promise<DurableInvalidation[]> {
  return runTransaction<DurableInvalidation[]>(
    'readonly',
    (store) => store.getAll(controlNamespaceRange(operation.context.physicalNamespace)),
    operation,
    undefined,
    CONTROL_STORE_NAME,
  ).then((records) => records ?? [])
}

export function idbAdvanceInvalidation(
  logicalPrefix: string,
  operation: NamespaceStorageOperation,
): Promise<number | null> {
  const namespace = operation.context.physicalNamespace
  return runMultiStoreTransaction<number>(
    [PAYLOAD_STORE_NAME, META_STORE_NAME, CONTROL_STORE_NAME],
    'readwrite',
    operation,
    (transaction, setResult) => {
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME)
      const metadata = transaction.objectStore(META_STORE_NAME)
      const controls = transaction.objectStore(CONTROL_STORE_NAME)
      const clockRequest = controls.get(clockKey(namespace)) as IDBRequest<
        DurableGenerationClock | undefined
      >
      requestToResult(transaction, clockRequest, (storedClock) => {
        if (!operation.isCurrent()) {
          abortIDBTransaction(transaction)
          return
        }
        const generation = (storedClock?.generation ?? 0) + 1
        const clock: DurableGenerationClock = {
          key: clockKey(namespace),
          kind: 'clock',
          namespace,
          generation,
        }
        const invalidation: DurableInvalidation = {
          key: invalidationKey(namespace, logicalPrefix),
          kind: 'invalidation',
          namespace,
          logicalPrefix,
          generation,
        }
        controls.put(clock)
        controls.put(invalidation)
        const physicalPrefix = persistedRecordKey(namespace, logicalPrefix)
        const range = IDBKeyRange.bound(physicalPrefix, `${physicalPrefix}\uffff`)
        payloads.delete(range)
        metadata.delete(range)
        setResult(generation)
      })
    },
  )
}

/** Delete pre-RF2B global cache rows by key only, without deserializing any namespaced body. */
export function idbDeleteLegacyUnowned(
  operation: NamespaceStorageOperation,
): Promise<void> {
  return runTransaction<IDBCursor | null>(
    'readwrite',
    (store) => store.openKeyCursor(),
    operation,
    (cursor, store) => {
      if (!cursor) return
      if (!isNamespacedRecordKey(cursor.primaryKey)) store.delete(cursor.primaryKey)
      cursor.continue()
    },
    LEGACY_STORE_NAME,
  ).then(() => undefined)
}

export function idbPut(
  record: PersistedRecord,
  operation?: NamespaceStorageOperation,
): Promise<boolean> {
  const canonicalRecord =
    typeof record.namespace === 'string' &&
    typeof record.logicalKey === 'string' &&
    record.key === persistedRecordKey(record.namespace, record.logicalKey)
  if (!canonicalRecord) {
    if (operation) return Promise.resolve(false)
    return runTransaction(
      'readwrite',
      (store) => store.put(record) as IDBRequest<unknown>,
      undefined,
      undefined,
      LEGACY_STORE_NAME,
    ).then((stored) => stored !== null)
  }
  if (
    operation &&
    (
      record.namespace !== operation.context.physicalNamespace ||
      record.key !== persistedRecordKey(operation.context.physicalNamespace, record.logicalKey)
    )
  ) {
    return Promise.resolve(false)
  }

  const storeRecord = (transaction: IDBTransaction) => {
    const payload: CachePayload = { key: record.key, data: record.data }
    const metadata: CacheMetadata = {
      key: record.key,
      namespace: record.namespace,
      logicalKey: record.logicalKey,
      schema: record.schema,
      size: record.size,
      lastAccess: record.updatedAt,
      generation: record.generation ?? 0,
    }
    transaction.objectStore(PAYLOAD_STORE_NAME).put(payload)
    transaction.objectStore(META_STORE_NAME).put(metadata)
  }

  const storeNames = operation
    ? [PAYLOAD_STORE_NAME, META_STORE_NAME, CONTROL_STORE_NAME]
    : [PAYLOAD_STORE_NAME, META_STORE_NAME]
  return runMultiStoreTransaction<boolean>(
    storeNames,
    'readwrite',
    operation,
    (transaction, setResult) => {
      if (!operation) {
        storeRecord(transaction)
        setResult(true)
        return
      }
      const invalidations = transaction.objectStore(CONTROL_STORE_NAME).getAll(
        controlNamespaceRange(operation.context.physicalNamespace),
      ) as IDBRequest<DurableInvalidation[]>
      invalidations.onsuccess = () => {
        if (!operation.isCurrent()) {
          abortIDBTransaction(transaction)
          return
        }
        const blocked = invalidations.result.some(
          (invalidation) =>
            record.logicalKey.startsWith(invalidation.logicalPrefix) &&
            (record.generation ?? 0) < invalidation.generation,
        )
        if (!blocked) storeRecord(transaction)
        setResult(!blocked)
      }
    },
  ).then((stored) => stored === true)
}

/**
 * Atomically reserves global cache capacity and writes one payload/meta pair.
 *
 * IndexedDB serializes readwrite transactions that overlap these stores even
 * when they originate in different tabs. Keeping the metadata scan, eviction,
 * tombstone check, and put in this one transaction prevents two stale local
 * `knownBytes` projections from independently admitting writes over quota.
 */
export function idbPutWithinQuota(
  record: PersistedRecord,
  maxBytes: number,
  operation: NamespaceStorageOperation,
): Promise<QuotaPutResult | null> {
  if (
    record.namespace !== operation.context.physicalNamespace ||
    record.key !== persistedRecordKey(record.namespace, record.logicalKey) ||
    !Number.isSafeInteger(record.size) ||
    record.size < 0 ||
    !Number.isSafeInteger(maxBytes) ||
    maxBytes < 0
  ) {
    return Promise.resolve(null)
  }

  return runMultiStoreTransaction<QuotaPutResult>(
    [PAYLOAD_STORE_NAME, META_STORE_NAME, CONTROL_STORE_NAME],
    'readwrite',
    operation,
    (transaction, setResult) => {
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME)
      const metadata = transaction.objectStore(META_STORE_NAME)
      const controls = transaction.objectStore(CONTROL_STORE_NAME)
      const metadataRequest = metadata.getAll() as IDBRequest<CacheMetadata[]>
      const invalidationsRequest = controls.getAll(
        controlNamespaceRange(operation.context.physicalNamespace),
      ) as IDBRequest<DurableInvalidation[]>

      collectIDBRequestResults<{
        metadataRows: CacheMetadata[]
        invalidations: DurableInvalidation[]
      }>(
        transaction,
        { metadataRows: metadataRequest, invalidations: invalidationsRequest },
        ({ metadataRows, invalidations }) => {
        try {
          if (!operation.isCurrent()) {
            abortIDBTransaction(transaction)
            return
          }
          let totalBytes = metadataRows.reduce(
            (sum, item) => sum + (Number.isFinite(item.size) && item.size > 0 ? item.size : 0),
            0,
          )
          const existing = metadataRows.find((item) => item.key === record.key)
          const existingSize = existing?.size ?? 0
          const blocked = invalidations.some(
            (invalidation) =>
              record.logicalKey.startsWith(invalidation.logicalPrefix) &&
              record.generation < invalidation.generation,
          )
          if (blocked) {
            setResult({ stored: false, mutated: false, totalBytes })
            return
          }

          if (record.size > maxBytes) {
            if (existing) {
              payloads.delete(record.key)
              metadata.delete(record.key)
              totalBytes -= existingSize
            }
            setResult({
              stored: false,
              mutated: existing !== undefined,
              totalBytes,
            })
            return
          }

          let projectedBytes = totalBytes - existingSize + record.size
          const byAge = [...metadataRows].sort(
            (left, right) =>
              left.lastAccess - right.lastAccess || left.key.localeCompare(right.key),
          )
          for (const item of byAge) {
            if (projectedBytes <= maxBytes) break
            if (item.key === record.key) continue
            payloads.delete(item.key)
            metadata.delete(item.key)
            projectedBytes -= item.size
          }
          if (projectedBytes > maxBytes) {
            abortIDBTransaction(transaction)
            return
          }

          payloads.put({ key: record.key, data: record.data } satisfies CachePayload)
          metadata.put({
            key: record.key,
            namespace: record.namespace,
            logicalKey: record.logicalKey,
            schema: record.schema,
            size: record.size,
            lastAccess: record.updatedAt,
            generation: record.generation,
          } satisfies CacheMetadata)
          setResult({
            stored: true,
            mutated: true,
            totalBytes: projectedBytes,
          })
        } catch {
          abortIDBTransaction(transaction)
        }
      })
    },
  )
}

export function idbDelete(
  key: string,
  operation?: NamespaceStorageOperation,
): Promise<boolean> {
  if (operation) {
    const prefix = namespaceRecordPrefix(operation.context.physicalNamespace)
    if (!key.startsWith(prefix)) return Promise.resolve(false)
  }
  return idbDeleteMany([key], operation)
}

export function idbDeleteMany(
  keys: readonly string[],
  operation?: NamespaceStorageOperation,
): Promise<boolean> {
  return runMultiStoreTransaction<boolean>(
    [PAYLOAD_STORE_NAME, META_STORE_NAME],
    'readwrite',
    operation,
    (transaction, setResult) => {
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME)
      const metadata = transaction.objectStore(META_STORE_NAME)
      for (const key of keys) {
        payloads.delete(key)
        metadata.delete(key)
      }
      setResult(true)
    },
  ).then((deleted) => deleted === true)
}

export function idbRepairOrphans(): Promise<OrphanRepairResult> {
  return runMultiStoreTransaction<OrphanRepairResult>(
    [PAYLOAD_STORE_NAME, META_STORE_NAME],
    'readwrite',
    undefined,
    (transaction, setResult) => {
      const payloads = transaction.objectStore(PAYLOAD_STORE_NAME)
      const metadata = transaction.objectStore(META_STORE_NAME)
      let payloadsRemoved = 0
      let metadataRemoved = 0

      const scanMetadata = () => {
        const cursorRequest = metadata.openKeyCursor()
        cursorRequest.onsuccess = () => {
          const cursor = cursorRequest.result
          if (!cursor) {
            setResult({ payloadsRemoved, metadataRemoved })
            return
          }
          const key = cursor.primaryKey
          const payloadRequest = payloads.getKey(key)
          payloadRequest.onsuccess = () => {
            if (payloadRequest.result === undefined) {
              metadata.delete(key)
              metadataRemoved += 1
            }
            cursor.continue()
          }
        }
      }

      const cursorRequest = payloads.openKeyCursor()
      cursorRequest.onsuccess = () => {
        const cursor = cursorRequest.result
        if (!cursor) {
          scanMetadata()
          return
        }
        const key = cursor.primaryKey
        const metadataRequest = metadata.getKey(key)
        metadataRequest.onsuccess = () => {
          if (metadataRequest.result === undefined) {
            payloads.delete(key)
            payloadsRemoved += 1
          }
          cursor.continue()
        }
      }
    },
  ).then((result) => result ?? { payloadsRemoved: 0, metadataRemoved: 0 })
}

export function idbDeletePrefix(
  logicalPrefix: string,
  operation: NamespaceStorageOperation,
): Promise<void> {
  return idbAdvanceInvalidation(logicalPrefix, operation).then(() => undefined)
}

export function idbClear(): Promise<void> {
  return runMultiStoreTransaction<boolean>(
    [LEGACY_STORE_NAME, PAYLOAD_STORE_NAME, META_STORE_NAME, CONTROL_STORE_NAME],
    'readwrite',
    undefined,
    (transaction, setResult) => {
      transaction.objectStore(LEGACY_STORE_NAME).clear()
      transaction.objectStore(PAYLOAD_STORE_NAME).clear()
      transaction.objectStore(META_STORE_NAME).clear()
      transaction.objectStore(CONTROL_STORE_NAME).clear()
      setResult(true)
    },
  ).then(() => undefined)
}
