/**
 * 单飞（single-flight）互斥操作。
 *
 * 各 Surface 里同一段代码反复出现：一个 `busyID` state 用来禁用按钮，一个同名
 * ref 用来在异步回调里同步地判断「现在是不是已经有人在做了」。两份必须成对读
 * 写，因为 state 更新是异步的——连点两下时第二次点击读到的 state 仍是旧值，只
 * 有 ref 能立刻拦住它。
 *
 * 这里把这一对收成一个 token：`begin` 拿不到 token 就说明有人在做（直接返回，
 * 不排队也不打断），`finish` 只在自己仍然持有时才释放。generation 让「已经被
 * `clear` 过、又被别人重新 begin」的情形不会被旧的 finally 误释放——这正是身份
 * 撤销后 clearForIdentityLoss 与在途操作 finally 相遇时会踩到的那一格。
 *
 * 刻意不做的事：不排队、不取消、不合并。允许并行的多行操作（Feed 的 busyKeys）
 * 语义不同，不要用这个 Hook 去改写它们。
 */
import { useCallback, useMemo, useRef, useState } from 'react'

export interface ExclusiveActionToken<Key> {
  /** 调用方用来标识「在忙哪一项」的键，通常是行 ID 或一个固定的动作名。 */
  readonly key: Key
  /** 持有代次；`clear` 之后重新 begin 会得到新代次，旧 token 从此不再持有。 */
  readonly generation: number
}

export interface ExclusiveAction<Key> {
  /** 当前是否有操作在进行。 */
  readonly busy: boolean
  /** 在忙哪一项；空闲时为 null。可据此派生调用方的 saving / disabled 状态。 */
  readonly busyKey: Key | null
  /** 占坑。已有操作在进行时返回 null，调用方应直接放弃本次动作。 */
  readonly begin: (key: Key) => ExclusiveActionToken<Key> | null
  /** 这个 token 现在仍然持有吗。 */
  readonly owns: (token: ExclusiveActionToken<Key>) => boolean
  /** 释放。只有仍然持有的 token 才生效，旧 token 的 finally 不会误放行。 */
  readonly finish: (token: ExclusiveActionToken<Key>) => void
  /** 无条件释放，用于身份撤销、owner 更换等外部重置。 */
  readonly clear: () => void
}

export function useExclusiveAction<Key>(): ExclusiveAction<Key> {
  const [holder, setHolder] = useState<ExclusiveActionToken<Key> | null>(null)
  // state 用来渲染，ref 用来在同一个事件里同步拦住第二次点击。
  const holderRef = useRef<ExclusiveActionToken<Key> | null>(null)
  const generationRef = useRef(0)

  const begin = useCallback((key: Key): ExclusiveActionToken<Key> | null => {
    if (holderRef.current !== null) return null
    generationRef.current += 1
    const token: ExclusiveActionToken<Key> = { key, generation: generationRef.current }
    holderRef.current = token
    setHolder(token)
    return token
  }, [])

  const owns = useCallback((token: ExclusiveActionToken<Key>): boolean =>
    holderRef.current !== null && holderRef.current.generation === token.generation, [])

  const finish = useCallback((token: ExclusiveActionToken<Key>): void => {
    if (!owns(token)) return
    holderRef.current = null
    setHolder(null)
  }, [owns])

  const clear = useCallback((): void => {
    holderRef.current = null
    setHolder(null)
  }, [])

  return useMemo(() => ({
    busy: holder !== null,
    busyKey: holder === null ? null : holder.key,
    begin,
    owns,
    finish,
    clear,
  }), [begin, clear, finish, holder, owns])
}
