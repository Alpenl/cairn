/**
 * 缓存持久化桥（PF4）：把内存 store 的快照落进 IndexedDB，并在启动时读回来。
 *
 * 目的是让 **F5 之后的首屏不等网络**。PF3 解决了「切走再回来」，但刷新会把
 * 内存缓存整个清空——而刷新恰恰是用户最常做的动作之一。
 *
 * 落盘范围是刻意收窄的：只存「重建首屏所必需」的那几类键。全量落盘会把
 * 每一次搜索、每一页翻页都写进磁盘，换来的却只是极少复用的条目。
 */
import {
  CACHE_SCHEMA_VERSION,
  createPersistedRecord,
  idbAdvanceInvalidation,
  idbClear,
  idbDelete,
  idbDeleteLegacyUnowned,
  idbGetAll,
  idbGetInvalidations,
  idbGetMetadata,
  idbPutWithinQuota,
  idbRepairOrphans,
  persistedRecordKey,
  type PersistedRecord,
} from './idb'
import { CONTENT_CACHE_PREFIX, TRANSLATIONS_CACHE_PREFIX } from './keys'
import { resourceStore } from './store'
import { type IdentityLease, readerIdentity } from '../identity'
import { cacheStorageQueue, type NamespaceStorageOperation } from './io-queue'

/** 参与持久化的键前缀。 */
const PERSISTED_PREFIXES = [
  // 链接列表与 annotated 视图复用的单条 point projection。两者都在写后通过
  // invalidateLibrary / invalidateLinkProjection 明确失效，刷新后可直接恢复。
  'GET /api/links',
  CONTENT_CACHE_PREFIX, // 已保存原文（独立命名空间，见 keys.ts）
  // 译文。它此前是靠 `GET /api/links` 顺带落盘的；搬进独立命名空间躲开库级失效
  // 之后，必须在这里显式列出——漏了这一条，译文会从「刷新后仍在」静默退化成
  // 纯内存缓存，而且退化得毫无症状（只是每次刷新都重取一遍）。
  TRANSLATIONS_CACHE_PREFIX,
  'GET /api/tags',
  'GET /api/tree?view=domains',
  'GET /api/subscriptions',
  'GET /api/sites',
] as const

/**
 * 磁盘总量上限（字节）。超出按最久未更新淘汰。
 *
 * 50 MB 是「装得下几百篇文章的正文」与「不至于让浏览器因为一个阅读器就开始
 * 回收站点数据」之间的折中。IndexedDB 的配额是按站点算的，吃满会连带影响
 * 划线归档这类真正不能丢的东西。
 */
export const MAX_PERSISTED_BYTES = 50 * 1024 * 1024
const INVALIDATION_CHANNEL_NAME = 'webtag-reader-cache-invalidation-v1'

interface InvalidationWakeup {
  kind: 'cache-invalidation'
  namespace: string
  logicalPrefix: string
  generation: number
}

interface MetadataWakeup {
  kind: 'cache-metadata-changed'
}

export interface CacheMaintenanceStats {
  payloadOrphansRemoved: number
  metadataOrphansRemoved: number
  metadataRescans: number
  knownBytes: number
}

let maintenanceStats: CacheMaintenanceStats = {
  payloadOrphansRemoved: 0,
  metadataOrphansRemoved: 0,
  metadataRescans: 0,
  knownBytes: 0,
}

export function getCacheMaintenanceStats(): CacheMaintenanceStats {
  return { ...maintenanceStats }
}

function isPersistable(key: string): boolean {
  return PERSISTED_PREFIXES.some((prefix) => key.startsWith(prefix))
}

function overlapsPersistedPrefix(prefix: string): boolean {
  return PERSISTED_PREFIXES.some(
    (persistedPrefix) =>
      prefix.startsWith(persistedPrefix) || persistedPrefix.startsWith(prefix),
  )
}

/**
 * 估算一条记录的字节数。
 *
 * 用 UTF-8 字节而不是 JS 字符串长度（码元数）：中文正文每字符约 3 字节，
 * 按码元数算会把「50 MB 上限」实际放行到 100–150 MB —— 浏览器的站点配额
 * 会先于我们的上限触发，而配额一炸，落盘会静默降级，同站点的划线归档也
 * 可能被浏览器一并回收。
 */
function estimateSize(data: unknown): number {
  try {
    const json = JSON.stringify(data)
    if (!json) return 0
    if (typeof TextEncoder !== 'undefined') return new TextEncoder().encode(json).length
    return json.length * 2
  } catch {
    return 0
  }
}

/**
 * 从 IndexedDB 读回快照并喂进内存 store。
 *
 * schema 版本不匹配的记录整批丢弃：让旧结构的数据进入新代码，渲染路径上会
 * 到处是 undefined，而且只在升级过的老用户身上复现。
 */
export async function hydrateFromDisk(
  abandoned?: { value: boolean },
  expectedLease: IdentityLease | null = readerIdentity.activeLease,
): Promise<number> {
  const lease = expectedLease
  if (!lease) return 0
  const restored = await cacheStorageQueue.enqueue(
    lease,
    'IndexedDB hydrate',
    async (operation) => {
      await idbDeleteLegacyUnowned(operation)
      if (abandoned?.value || !operation.isCurrent()) return 0
      const invalidations = await idbGetInvalidations(operation)
      for (const invalidation of invalidations) {
        resourceStore.applyInvalidationGeneration(
          invalidation.logicalPrefix,
          invalidation.generation,
        )
      }
      const records = await idbGetAll(operation)
      // 调用方可能已经因为超时放弃等待并挂载了应用；此时**绝不能**再写
      // store——那会用磁盘上的旧快照盖掉网络刚拿回来的新数据，而且不会再
      // 触发一次校验。hydrate commit 还必须排在同 namespace 的旧 IO 之后，
      // 这样切换 identity 时撤销 lease 就能统一拦住全部迟到提交。
      if (abandoned?.value || !operation.isCurrent()) return 0
      if (records.length === 0) return 0

      const isInvalidated = (record: PersistedRecord) =>
        typeof record.logicalKey === 'string' && invalidations.some(
          (invalidation) =>
            record.logicalKey.startsWith(invalidation.logicalPrefix) &&
            (record.generation ?? 0) < invalidation.generation,
        )
      const stale = records.filter(
        (record) =>
          record.namespace !== operation.context.physicalNamespace ||
          record.schema !== CACHE_SCHEMA_VERSION ||
          typeof record.logicalKey !== 'string' ||
          isInvalidated(record),
      )
      for (const record of stale) {
        const deletion = operation.commit(() => idbDelete(record.key, operation))
        if (deletion) await deletion
      }

      let count = 0
      for (const record of records) {
        if (abandoned?.value || !operation.isCurrent()) break
        if (record.schema !== CACHE_SCHEMA_VERSION) continue
        if (record.namespace !== operation.context.physicalNamespace) continue
        if (isInvalidated(record)) continue
        if (!isPersistable(record.logicalKey)) continue
        // 只填「内存里还没有」的键：应用可能已经挂载并拿到了更新的数据。
        if (resourceStore.has(record.logicalKey)) continue
        if (
          resourceStore.setForIdentity(
            operation.ownership,
            record.logicalKey,
            record.data,
          )
        ) {
          count += 1
        }
      }
      return count
    },
  )
  return restored ?? 0
}

/**
 * 开始把 store 的写入同步到磁盘。返回停止函数。
 *
 * 采用「变更后延迟批量落盘」而不是逐次写：列表校验、翻译轮询会高频触碰
 * store，每次都开一个 IndexedDB 事务会把主线程压满。
 */
export function startPersistence(
  options: { debounceMs?: number; lease?: IdentityLease } = {},
): () => void {
  const lease = options.lease ?? readerIdentity.activeLease
  if (!lease) return () => undefined
  const ownership = lease.capture('IndexedDB persistence')
  const debounceMs = options.debounceMs ?? 500
  const dirty = new Set<string>()
  let timer: number | null = null
  let stopped = false
  let knownBytes = 0

  const installMetadataProjection = (
    metadata: Awaited<ReturnType<typeof idbGetMetadata>>,
  ) => {
    knownBytes = metadata.reduce((sum, record) => sum + record.size, 0)
    maintenanceStats = {
      ...maintenanceStats,
      metadataRescans: maintenanceStats.metadataRescans + 1,
      knownBytes,
    }
  }

  const rescanMetadata = async (operation: NamespaceStorageOperation) => {
    const metadata = await idbGetMetadata()
    if (!operation.isCurrent()) return
    installMetadataProjection(metadata)
  }

  const channel = typeof BroadcastChannel === 'undefined'
    ? null
    : new BroadcastChannel(INVALIDATION_CHANNEL_NAME)
  const reconcile = () => {
    if (stopped || !lease.isCurrent(ownership)) return
    void cacheStorageQueue.enqueue(
      lease,
      'IndexedDB cache reconciliation',
      async (operation) => {
        const invalidations = await idbGetInvalidations(operation)
        if (!operation.isCurrent()) return
        for (const invalidation of invalidations) {
          resourceStore.applyInvalidationGeneration(
            invalidation.logicalPrefix,
            invalidation.generation,
          )
        }
        await rescanMetadata(operation)
      },
    ).catch(() => undefined)
  }
  const publishMetadataWakeup = () => {
    if (stopped || !channel) return
    channel.postMessage({ kind: 'cache-metadata-changed' } satisfies MetadataWakeup)
  }
  if (channel) {
    channel.onmessage = (event: MessageEvent<unknown>) => {
      const wakeup = event.data as
        | Partial<InvalidationWakeup>
        | Partial<MetadataWakeup>
        | null
      if (
        wakeup?.kind === 'cache-metadata-changed' ||
        wakeup?.kind === 'cache-invalidation'
      ) {
        // Metadata quota is origin-global, so even another namespace's
        // invalidation must refresh this tab's observable byte projection.
        // Durable invalidations are still read through the active namespace.
        reconcile()
      }
    }
  }
  const onVisibilityChange = () => {
    if (document.visibilityState === 'visible') reconcile()
  }
  document.addEventListener('visibilitychange', onVisibilityChange)

  void cacheStorageQueue.enqueue(lease, 'IndexedDB cache maintenance bootstrap', async (operation) => {
    const repaired = await idbRepairOrphans()
    if (!operation.isCurrent()) return
    maintenanceStats = {
      ...maintenanceStats,
      payloadOrphansRemoved:
        maintenanceStats.payloadOrphansRemoved + repaired.payloadsRemoved,
      metadataOrphansRemoved:
        maintenanceStats.metadataOrphansRemoved + repaired.metadataRemoved,
    }
    await rescanMetadata(operation)
  }).catch(() => undefined)
  reconcile()

  const flush = () => {
    timer = null
    const keys = [...dirty]
    dirty.clear()
    if (!lease.isCurrent(ownership)) return
    void cacheStorageQueue.enqueue(lease, 'IndexedDB persistence batch', async (operation) => {
      for (const key of keys) {
        if (!operation.isCurrent()) return
        const entry = resourceStore.peek(key)
        const physicalKey = persistedRecordKey(ownership.physicalNamespace, key)
        if (entry.data === undefined) {
          const deletion = operation.commit(() =>
            idbDelete(physicalKey, operation),
          )
          if (deletion && await deletion) {
            await rescanMetadata(operation)
            publishMetadataWakeup()
          } else if (operation.isCurrent()) {
            await rescanMetadata(operation)
          }
          continue
        }
        const size = estimateSize(entry.data)
        const write = operation.commit(() =>
          idbPutWithinQuota(
            createPersistedRecord(ownership.physicalNamespace, key, {
              schema: CACHE_SCHEMA_VERSION,
              data: entry.data,
              updatedAt: entry.updatedAt,
              size,
              generation: resourceStore.durableGeneration(key),
            }),
            MAX_PERSISTED_BYTES,
            operation,
          ),
        )
        const result = write ? await write : null
        if (result) {
          knownBytes = result.totalBytes
          maintenanceStats = { ...maintenanceStats, knownBytes }
          if (!result.stored) await rescanMetadata(operation)
          if (result.mutated) publishMetadataWakeup()
        } else if (operation.isCurrent()) {
          await rescanMetadata(operation)
        }
      }
    })
  }

  const unsubscribeChanges = resourceStore.onChange((key) => {
    if (stopped || !lease.isCurrent(ownership) || !isPersistable(key)) return
    dirty.add(key)
    if (timer !== null) return
    timer = window.setTimeout(flush, debounceMs)
  })
  const unsubscribeInvalidations = resourceStore.onInvalidate((prefix) => {
    if (stopped || !lease.isCurrent(ownership) || !overlapsPersistedPrefix(prefix)) return
    void commitPersistedInvalidation(prefix, lease, channel, true)
      .then(reconcile)
      .catch(() => undefined)
  })

  return () => {
    stopped = true
    unsubscribeChanges()
    unsubscribeInvalidations()
    document.removeEventListener('visibilitychange', onVisibilityChange)
    channel?.close()
    if (timer !== null) window.clearTimeout(timer)
  }
}

async function commitPersistedInvalidation(
  logicalPrefix: string,
  lease: IdentityLease,
  channel: BroadcastChannel | null,
  alreadyInvalidated = false,
): Promise<void> {
  const generation = await cacheStorageQueue.enqueue(
    lease,
    `IndexedDB prefix invalidate ${logicalPrefix}`,
    async (operation) => idbAdvanceInvalidation(logicalPrefix, operation),
  )
  if (generation === undefined || generation === null) return
  if (alreadyInvalidated) {
    resourceStore.acknowledgeDurableInvalidation(logicalPrefix, generation)
  } else {
    resourceStore.applyInvalidationGeneration(logicalPrefix, generation)
  }
  const wakeup: InvalidationWakeup = {
    kind: 'cache-invalidation',
    namespace: lease.context.physicalNamespace,
    logicalPrefix,
    generation,
  }
  if (channel) {
    channel.postMessage(wakeup)
    return
  }
  if (typeof BroadcastChannel !== 'undefined') {
    const transient = new BroadcastChannel(INVALIDATION_CHANNEL_NAME)
    transient.postMessage(wakeup)
    transient.close()
  }
}

/** 删除当前身份磁盘分区内匹配 logical prefix 的缓存记录。 */
export async function deletePersistedPrefix(
  logicalPrefix: string,
  expectedLease: IdentityLease | null = readerIdentity.activeLease,
): Promise<void> {
  const lease = expectedLease
  if (!lease) return
  await commitPersistedInvalidation(logicalPrefix, lease, null)
}

/** 清空磁盘缓存（重置应用缓存 / 切换后端时用）。 */
export async function clearPersistedCache(): Promise<void> {
  await idbClear()
}

export type { PersistedRecord }
