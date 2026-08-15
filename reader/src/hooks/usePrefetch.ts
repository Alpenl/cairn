/**
 * 空闲预取（PF9）。
 *
 * MainView 已经算好了上一篇 / 下一篇是谁（ArticlePager 用的就是它们）。既然
 * 知道用户下一步大概率去哪，就在浏览器空闲时把那两篇的详情先取回缓存——
 * 点「下一篇」时是 0 个网络请求。
 *
 * 三条自我约束，都是为了不让预取变成骚扰：
 *   · 弱网 / 省流量模式不预取。用户的流量不该被"我猜他要看"花掉。
 *   · 同一时刻最多 1 个预取在途，且用低优先级的 idle 回调调度——预取永远
 *     不该和用户主动发起的请求抢带宽。
 *   · 已在缓存里的不重复预取（store 的 in-flight 去重再兜一层）。
 */
import { useEffect, useRef } from 'react'

import { resourceStore } from '../lib/cache/store'

type IdleHandle = number

interface NetworkInformation {
  saveData?: boolean
  effectiveType?: string
}

/** 弱网或省流量模式下不预取。 */
function shouldSkipPrefetch(): boolean {
  const connection = (navigator as Navigator & { connection?: NetworkInformation }).connection
  if (!connection) return false
  if (connection.saveData) return true
  const slow = connection.effectiveType === '2g' || connection.effectiveType === 'slow-2g'
  return Boolean(slow)
}

/** requestIdleCallback 的降级封装（Safari 长期没有它）。 */
function scheduleIdle(callback: () => void): IdleHandle {
  const idle = (window as Window & {
    requestIdleCallback?: (cb: () => void, options?: { timeout: number }) => number
  }).requestIdleCallback
  if (idle) return idle(callback, { timeout: 2000 })
  return window.setTimeout(callback, 300)
}

function cancelIdle(handle: IdleHandle): void {
  const cancel = (window as Window & { cancelIdleCallback?: (h: number) => void }).cancelIdleCallback
  if (cancel) {
    cancel(handle)
    return
  }
  window.clearTimeout(handle)
}

export interface PrefetchTarget {
  /** 缓存键。已在缓存里就跳过。 */
  key: string
  /** 真正取数的回调。 */
  load: () => Promise<unknown>
}

/**
 * 在浏览器空闲时依次预取给定目标。targets 变化即取消上一批。
 */
export function usePrefetch(targets: PrefetchTarget[]): void {
  const targetsRef = useRef(targets)
  targetsRef.current = targets
  // 用键的拼接做依赖：targets 是调用方现建的数组，直接进依赖会每渲染重排一次。
  const signature = targets.map((target) => target.key).join('|')

  useEffect(() => {
    if (signature === '') return
    if (shouldSkipPrefetch()) return

    let cancelled = false
    let handle: IdleHandle | null = null

    const runNext = (index: number) => {
      if (cancelled || index >= targetsRef.current.length) return
      handle = scheduleIdle(() => {
        if (cancelled) return
        const target = targetsRef.current[index]
        if (!target || resourceStore.has(target.key)) {
          runNext(index + 1)
          return
        }
        // 让路：此刻有用户主动发起的请求在途，就把这一轮推迟。预取的全部
        // 价值在于「反正闲着」，一旦它开始和真实交互抢带宽就得不偿失。
        if (resourceStore.busy) {
          handle = window.setTimeout(() => runNext(index), 400)
          return
        }
        // 串行：同一时刻最多一个预取在途，不与用户请求抢带宽。
        void target.load().finally(() => {
          if (!cancelled) runNext(index + 1)
        })
      })
    }
    runNext(0)

    return () => {
      cancelled = true
      if (handle !== null) cancelIdle(handle)
    }
  }, [signature])
}
