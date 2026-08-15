/**
 * 缓存的启动引导（PF4）：hydrate + 开启落盘。
 *
 * 抽成独立模块是为了它可测——main.tsx 里的顶层副作用没法在单测里驱动。
 */
import { hydrateFromDisk, startPersistence } from './persist'
import { type IdentityLease, readerIdentity } from '../identity'

/**
 * hydrate 的超时预算。
 *
 * 超过就放弃、照常空白启动。这条兜底不是可选的：IndexedDB 在存储被阻塞、
 * 配额告急或用户禁用时可能既不成功也不失败，没有超时的话「缓存加速首屏」
 * 会当场变成「缓存把首屏卡死」——比不做这个优化严格更糟。
 */
export const HYDRATE_TIMEOUT_MS = 200

export interface BootstrapResult {
  /** 从磁盘恢复的条目数。超时或无缓存时为 0。 */
  restored: number
  /** 是否因超时放弃了 hydrate。 */
  timedOut: boolean
}

let stopPersistence: (() => void) | null = null

export async function bootstrapCache(
  expectedLease: IdentityLease | null = readerIdentity.activeLease,
): Promise<BootstrapResult> {
  if (!expectedLease) return { restored: 0, timedOut: false }
  const ownership = expectedLease.capture('cache bootstrap')
  let timedOut = false
  // abandoned 会被超时分支置位，hydrateFromDisk 内部据此**放弃写入**。
  //
  // 只用 Promise.race 是不够的：race 只是不再等它，那个 hydrate 仍在跑，
  // 完成时会把磁盘上的旧快照逐条 set 进 store。时序是
  // 「200ms 超时 → 应用挂载并发请求 → 300ms 拿到新数据 → 1200ms 磁盘终于返回
  // → 旧快照覆盖新数据且不再触发校验」，用户看到列表倒退回上一次的内容。
  // idb 自己的 open 超时是 1 秒，5 倍于这里的预算，这条路径在冷启动稍慢的
  // 机器上完全可达。
  const abandoned = { value: false }
  const restored = await Promise.race([
    hydrateFromDisk(abandoned, expectedLease).catch(() => 0),
    new Promise<number>((resolve) => {
      setTimeout(() => {
        timedOut = true
        abandoned.value = true
        resolve(0)
      }, HYDRATE_TIMEOUT_MS)
    }),
  ])

  // 落盘照常开启：即便这次 hydrate 超时，本次会话产生的数据仍值得存下来，
  // 下次启动就能用上。
  if (expectedLease.isCurrent(ownership)) {
    stopPersistence?.()
    stopPersistence = startPersistence({ lease: expectedLease })
  }

  return { restored, timedOut }
}

/** 仅测试 / 重置缓存时用：停止落盘。 */
export function stopCachePersistence(): void {
  stopPersistence?.()
  stopPersistence = null
}
