/**
 * 异步请求的所有权闸门。
 *
 * 每个 Surface 都在重复同一件事：发一个异步请求，等它回来，然后判断「这个回包
 * 还算数吗」。算不算数由四件互相独立的事决定——组件还挂着吗、身份/目标还是发
 * 请求时那一个吗、这条通道有没有更新的请求把它顶掉了、以及此刻的身份租约和能
 * 力租约是否仍然有效。少判一条就是一类真实的错painting：卸载后写 state、旧命名
 * 空间的数据画在新身份上、翻页时旧页覆盖新页、身份撤销后仍然写回服务端数据。
 *
 * 这里只做「这个 token 还算数吗」这一个判断，不持有 data / loading / error。各
 * Surface 的领域状态形状差得很远（Feed 有游标和快照、Inbox 有分区和 CAS 重读、
 * Home 是聚合），把它们收成一个通用列表状态机只会让每个调用方都要绕过它。
 *
 * 两个判断刻意分开：
 *   - `isCurrent`：可以拿这个回包去写数据吗。检查挂载、owner、generation 和
 *     authority（身份/能力租约）。
 *   - `isSameOwner`：可以拿这个 token 去释放自己占住的 loading / busy 吗。不看
 *     authority——身份刚被撤销时，发起这次请求的那段代码仍然必须能在 finally 里
 *     把自己置的 loading 收掉，否则界面永远停在加载中。但它仍然检查 generation：
 *     一个被顶掉的旧请求的 finally 不能清掉新请求刚置上的状态。
 */
import { useCallback, useLayoutEffect, useMemo, useRef } from 'react'

export interface SurfaceRequestToken<Channel extends PropertyKey> {
  /** 请求所属通道；同一通道内后发的请求会顶掉先发的。 */
  readonly channel: Channel
  /** 通道内的请求代次。`begin` / `invalidate` / `invalidateAll` 都会推进它。 */
  readonly generation: number
  /** 闸门内全局单调递增的取号；供调用方维护「已应用代次」下限等总序判断。 */
  readonly sequence: number
  /** 取号时的 owner 快照（身份、租约、目标 ID 等）。 */
  readonly owner: readonly unknown[]
}

export interface SurfaceRequestGateOptions {
  /**
   * 决定「谁在发这个请求」的身份分量，按元素浅比较。任一元素变化都会让此前
   * 取出的所有 token 立刻失效——新身份不能被旧命名空间的回包画上。
   */
  readonly owner: readonly unknown[]
  /**
   * 写回权威判断，典型是 `() => identityIsCurrent(client) && lease.isCurrent('home')`。
   * 只影响 `isCurrent`；不传视为始终有权威。
   */
  readonly authority?: () => boolean
}

export interface SurfaceRequestGate<Channel extends PropertyKey> {
  /** 开始一次新请求：使同 channel 的旧 token 立即失效，并返回新 token。 */
  readonly begin: (channel: Channel) => SurfaceRequestToken<Channel>
  /** 取当前代次的 token，不顶掉任何在途请求。用于「搭车」于既有请求的旁路读取。 */
  readonly capture: (channel: Channel) => SurfaceRequestToken<Channel>
  /** 作废该 channel 的全部在途 token（例如本地 mutation 让列表读取过期）。 */
  readonly invalidate: (channel: Channel) => void
  /** 作废所有 channel 的在途 token。 */
  readonly invalidateAll: () => void
  /** 这个回包可以写数据吗：挂载中、owner 未变、未被顶掉、且当前仍有权威。 */
  readonly isCurrent: (token: SurfaceRequestToken<Channel>) => boolean
  /** 这个 token 可以释放自己占住的 loading / busy 吗：同 `isCurrent` 但不看权威。 */
  readonly isSameOwner: (token: SurfaceRequestToken<Channel>) => boolean
}

function sameOwner(left: readonly unknown[], right: readonly unknown[]): boolean {
  if (left === right) return true
  if (left.length !== right.length) return false
  for (let index = 0; index < left.length; index += 1) {
    if (!Object.is(left[index], right[index])) return false
  }
  return true
}

export function useSurfaceRequestGate<Channel extends PropertyKey>(
  options: SurfaceRequestGateOptions,
): SurfaceRequestGate<Channel> {
  const { owner, authority } = options
  const ownerRef = useRef<readonly unknown[]>(Object.freeze([...owner]))
  const authorityRef = useRef<(() => boolean) | undefined>(authority)
  // 渲染即视为已挂载：调用方可能在首个 effect 之前就从事件回调里取号，此时
  // token 不该因为「还没跑 effect」而作废。卸载由 cleanup 负责置回。
  const mountedRef = useRef(true)
  const generationsRef = useRef(new Map<Channel, number>())
  const sequenceRef = useRef(0)

  // owner 只在内容变化时换引用，于是绝大多数 token 的 owner 比较是一次引用相等。
  useLayoutEffect(() => {
    authorityRef.current = authority
    if (!sameOwner(ownerRef.current, owner)) ownerRef.current = Object.freeze([...owner])
  })

  useLayoutEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  const generationOf = useCallback((channel: Channel): number => {
    const existing = generationsRef.current.get(channel)
    if (existing !== undefined) return existing
    generationsRef.current.set(channel, 1)
    return 1
  }, [])

  const tokenFor = useCallback((channel: Channel, generation: number): SurfaceRequestToken<Channel> => {
    sequenceRef.current += 1
    return { channel, generation, sequence: sequenceRef.current, owner: ownerRef.current }
  }, [])

  const begin = useCallback((channel: Channel): SurfaceRequestToken<Channel> => {
    const next = generationOf(channel) + 1
    generationsRef.current.set(channel, next)
    return tokenFor(channel, next)
  }, [generationOf, tokenFor])

  const capture = useCallback(
    (channel: Channel): SurfaceRequestToken<Channel> => tokenFor(channel, generationOf(channel)),
    [generationOf, tokenFor],
  )

  const invalidate = useCallback((channel: Channel): void => {
    generationsRef.current.set(channel, generationOf(channel) + 1)
  }, [generationOf])

  const invalidateAll = useCallback((): void => {
    for (const [channel, generation] of generationsRef.current) {
      generationsRef.current.set(channel, generation + 1)
    }
  }, [])

  const isSameOwner = useCallback((token: SurfaceRequestToken<Channel>): boolean =>
    mountedRef.current &&
    sameOwner(token.owner, ownerRef.current) &&
    generationsRef.current.get(token.channel) === token.generation, [])

  const isCurrent = useCallback((token: SurfaceRequestToken<Channel>): boolean =>
    isSameOwner(token) && (authorityRef.current?.() ?? true), [isSameOwner])

  return useMemo(
    () => ({ begin, capture, invalidate, invalidateAll, isCurrent, isSameOwner }),
    [begin, capture, invalidate, invalidateAll, isCurrent, isSameOwner],
  )
}
