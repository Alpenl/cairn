/**
 * store 与 React 之间的桥（PF3）。
 *
 * 用 useSyncExternalStore 而不是 useState + 订阅回调：后者在并发渲染下会撕裂
 * （同一次渲染中不同组件读到 store 的不同版本），而这个 store 恰恰是多个组件
 * 共享同一个键的。
 */
import { useCallback, useEffect, useMemo, useRef, useSyncExternalStore } from 'react'

import type { ApiError, ApiResult } from '../api/result'
import { resourceStore, type CacheEntry, type ConditionalContext, type FetchOptions } from './store'

export interface CachedResource<T> {
  /** 当前可渲染的数据。从未成功过时为 undefined。 */
  data: T | undefined
  error: ApiError | null
  /**
   * 首屏加载中：**只有在没有任何可渲染数据时**才为 true。
   *
   * 有缓存时它恒为 false —— 这正是 stale-while-revalidate 的观感来源：
   * 切回来 / 刷新看到的是内容，不是骨架屏。
   */
  loading: boolean
  /** 后台是否有请求在途（可用于细微的"同步中"指示，不该用来遮挡内容）。 */
  revalidating: boolean
  /** 主动重取。silent=true 时失败不写错误态。 */
  reload: (options?: FetchOptions) => Promise<ApiResult<T> | null>
}

/**
 * 订阅某个缓存键，并在键变化时自动发起校验。
 *
 * key 为 null 表示"当前无可读资源"（例如未选中文章），此时不订阅、不请求。
 */
export function useCachedResource<T>(
  key: string | null,
  fetcher: (conditional: ConditionalContext) => Promise<ApiResult<T>>,
  options: { equal?: FetchOptions['equal']; enabled?: boolean } = {},
): CachedResource<T> {
  const enabled = options.enabled ?? true
  // fetcher 通常是调用方现写的箭头函数，逐次新建。用 ref 持有最新的一份，
  // 避免它进任何依赖数组——否则每次渲染都会重新订阅、重新请求。
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher
  const equalRef = useRef(options.equal)
  equalRef.current = options.equal

  const subscribe = useCallback(
    (listener: () => void) => {
      if (!key) return () => {}
      return resourceStore.subscribe(key, listener)
    },
    [key],
  )

  const getSnapshot = useCallback(
    (): CacheEntry<T> => (key ? resourceStore.peek<T>(key) : EMPTY),
    [key],
  )

  const entry = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)

  const reload = useCallback(
    (fetchOptions: FetchOptions = {}): Promise<ApiResult<T> | null> => {
      if (!key) return Promise.resolve(null)
      return resourceStore.fetch<T>(key, (conditional) => fetcherRef.current(conditional), {
        equal: equalRef.current,
        ...fetchOptions,
        // 用户主动重取默认强制回源；静默（后台）重取默认合并在途请求。
        // 放在展开之后，显式传 force 时仍以调用方为准。
        force: fetchOptions.force ?? !fetchOptions.silent,
      })
    },
    [key],
  )

  // Every desired generation is attempted at most once automatically. Multiple
  // consumers race through this effect, but ResourceStore coalesces the same
  // {identity, key, generation} into one request.
  useEffect(() => {
    if (!key || !enabled) return
    if (entry.attemptedGeneration >= entry.desiredGeneration) return
    void resourceStore.fetch<T>(key, (conditional) => fetcherRef.current(conditional), {
      equal: equalRef.current,
      generation: entry.desiredGeneration,
    })
  }, [key, enabled, entry.desiredGeneration, entry.attemptedGeneration])

  // A remount with renderable data keeps SWR's background validation. This is
  // intentionally separate from generation recovery: it may revalidate the
  // already-settled generation, while an empty snapshot is handled above.
  useEffect(() => {
    if (!key || !enabled || !resourceStore.has(key)) return
    void resourceStore.fetch<T>(key, (conditional) => fetcherRef.current(conditional), {
      equal: equalRef.current,
      silent: true,
      generation: resourceStore.peek(key).desiredGeneration,
    })
  }, [key, enabled])

  // 返回值必须 memo 化。调用方普遍把它整个塞进 useCallback 依赖数组
  // （`[client, linkId, resource]`），每渲染新建一个对象会让那些回调的引用
  // 逐次变化，进而击穿下游所有 memo —— PF2 刚加上的那几层当场失效。
  return useMemo(
    () => ({
      data: entry.data,
      error: entry.error,
      loading: entry.data === undefined && (
        entry.revalidating ||
        (entry.error === null && entry.settledGeneration < entry.desiredGeneration)
      ),
      revalidating: entry.revalidating,
      reload,
    }),
    [
      entry.data,
      entry.desiredGeneration,
      entry.error,
      entry.revalidating,
      entry.settledGeneration,
      reload,
    ],
  )
}

const EMPTY: CacheEntry<never> = {
  error: null,
  updatedAt: 0,
  revalidating: false,
  desiredGeneration: 0,
  attemptedGeneration: -1,
  settledGeneration: -1,
}
