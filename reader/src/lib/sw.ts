/**
 * Service Worker 注册、更新提示与注销（PF10）。
 *
 * # 为什么必须有注销通道
 *
 * SW 是整个性能专项里唯一「出错后远程修不回来」的东西：一个坏版本的 SW 会把
 * 用户钉死在旧代码上，而修复本身也要靠那个坏 SW 放行才能送达。因此上线的前提
 * 是先有一个用户能自己触发的重置入口——`unregisterServiceWorker` 就是它，
 * 设置页把它接了出去。
 *
 * # 为什么是 prompt 而不是 autoUpdate
 *
 * 静默强制刷新会打断正在读文章的人。这里只在新版本就绪时给一条提示，
 * 刷不刷由用户决定。
 */

import { clearPersistedCache } from './cache/persist'
import { isReaderOwnedCacheName, readerScopeURL } from './sw-ownership'

type UpdateListener = () => void

let updateListener: UpdateListener | null = null
let waitingWorker: ServiceWorker | null = null

/** 注册 SW。仅生产构建调用；不支持时静默跳过。 */
export function registerServiceWorker(onUpdateReady: UpdateListener): void {
  if (!('serviceWorker' in navigator)) return
  updateListener = onUpdateReady

  const start = () => {
    // scope 跟随 base：Reader 有两种部署形态（根路径的独立站点、Go 后端挂在
    // /reader/ 下），写死 '/' 会让后者的 SW 注册失败或越权控制整个域名。
    const swURL = `${import.meta.env.BASE_URL}sw.js`
    void navigator.serviceWorker.register(swURL, { scope: import.meta.env.BASE_URL }).then(
      (registration) => {
        if (registration.waiting) {
          waitingWorker = registration.waiting
          updateListener?.()
        }
        registration.addEventListener('updatefound', () => {
          const installing = registration.installing
          if (!installing) return
          installing.addEventListener('statechange', () => {
            if (installing.state === 'installed' && navigator.serviceWorker.controller) {
              waitingWorker = installing
              updateListener?.()
            }
          })
        })
      },
      () => {
        // 注册失败不影响应用运行：它只是离线增强。
      },
    )
  }

  // 绝不能无条件等 'load'：只要调用方是在 load 之后才调到这里，监听器就永远
  // 等不到第二次，SW 一次都不会注册。当前的调用方 main.tsx 正是这种——它把本
  // 函数放在 `bootstrapCache()` 的 `.then()` 回调里，而 bootstrapCache 要读
  // IndexedDB——但守卫本身与调用方无关，换个调用时机同样成立。
  //
  // 这不是理论风险：修复前的 8bdad1e 线上实测，整份网络抓包里连一个
  // GET /sw.js 都没有，getRegistrations() 返回空数组。
  if (document.readyState === 'complete') start()
  else window.addEventListener('load', start, { once: true })
}

/** 让等待中的新版本立即接管，然后刷新。 */
export function applyServiceWorkerUpdate(): void {
  if (!waitingWorker) {
    window.location.reload()
    return
  }
  waitingWorker.postMessage({ type: 'SKIP_WAITING' })
  waitingWorker = null
  window.location.reload()
}

/**
 * 重置应用缓存：只注销当前 Reader scope 的 SW，并清空它拥有的 Cache
 * Storage 与可重建资源缓存。
 * installation user data 使用独立数据库，绝不能由缓存重置删除。
 *
 * 这是 SW 出问题时用户唯一的自救手段，因此它**不依赖 SW 本身**能正常工作。
 */
export async function resetApplicationCache(): Promise<void> {
  const base = import.meta.env.BASE_URL
  const origin = window.location.origin
  const ownedScope = readerScopeURL(base, origin)
  if ('serviceWorker' in navigator) {
    try {
      const registrations = await navigator.serviceWorker.getRegistrations()
      const owned = registrations.filter((registration) => registration.scope === ownedScope)
      await Promise.allSettled(owned.map((registration) => registration.unregister()))
    } catch {
      // 注销失败也要继续清后面两样。
    }
  }
  if ('caches' in window) {
    try {
      const keys = await caches.keys()
      const owned = keys.filter((key) => isReaderOwnedCacheName(key, base, origin))
      await Promise.allSettled(owned.map((key) => caches.delete(key)))
    } catch {
      // 同上。
    }
  }
  await clearPersistedCache().catch(() => undefined)
}
