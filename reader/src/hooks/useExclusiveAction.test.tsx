import { describe, expect, it } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useExclusiveAction, type ExclusiveActionToken } from './useExclusiveAction'

function renderAction() {
  return renderHook(() => useExclusiveAction<string>())
}

/** begin/finish 会写 state，包在 act 里；同时把返回值带出来供断言。 */
function inAct<T>(run: () => T): T {
  let value!: T
  act(() => {
    value = run()
  })
  return value
}

describe('useExclusiveAction', () => {
  it('初始状态空闲', () => {
    const { result } = renderAction()

    expect(result.current.busy).toBe(false)
    expect(result.current.busyKey).toBe(null)
  })

  it('begin 占坑并把 busyKey 暴露给调用方派生 saving', () => {
    const { result } = renderAction()

    const token = inAct(() => result.current.begin('todo-1'))

    expect(token).not.toBe(null)
    expect(result.current.busy).toBe(true)
    expect(result.current.busyKey).toBe('todo-1')
    expect(result.current.owns(token as ExclusiveActionToken<string>)).toBe(true)
  })

  it('忙碌时并发 begin 返回 null 且不改变持有者', () => {
    const { result } = renderAction()

    // 同一个事件回合内连点两次：第二次必须被 ref 同步拦住，state 还来不及更新。
    const tokens = inAct(() => [result.current.begin('todo-1'), result.current.begin('todo-2')])

    expect(tokens[0]).not.toBe(null)
    expect(tokens[1]).toBe(null)
    expect(result.current.busyKey).toBe('todo-1')
    expect(result.current.owns(tokens[0] as ExclusiveActionToken<string>)).toBe(true)
  })

  it('同一个 key 也不能重入', () => {
    const { result } = renderAction()

    const second = inAct(() => {
      result.current.begin('todo-1')
      return result.current.begin('todo-1')
    })

    expect(second).toBe(null)
  })

  it('finish 释放后可以再次 begin', () => {
    const { result } = renderAction()

    const token = inAct(() => result.current.begin('todo-1')) as ExclusiveActionToken<string>
    inAct(() => result.current.finish(token))

    expect(result.current.busy).toBe(false)
    expect(result.current.busyKey).toBe(null)
    expect(result.current.owns(token)).toBe(false)

    const next = inAct(() => result.current.begin('todo-2'))
    expect(next).not.toBe(null)
    expect(result.current.busyKey).toBe('todo-2')
  })

  it('finish 只由持有者生效：旧 token 不能释放新操作', () => {
    const { result } = renderAction()

    const stale = inAct(() => result.current.begin('todo-1')) as ExclusiveActionToken<string>
    // 身份撤销一类的外部重置抢先释放了坑位。
    inAct(() => result.current.clear())
    const fresh = inAct(() => result.current.begin('todo-2')) as ExclusiveActionToken<string>

    // 旧操作的 finally 迟到了；它不能把新操作的 busy 抹掉。
    inAct(() => result.current.finish(stale))

    expect(result.current.busy).toBe(true)
    expect(result.current.busyKey).toBe('todo-2')
    expect(result.current.owns(stale)).toBe(false)
    expect(result.current.owns(fresh)).toBe(true)
  })

  it('同 key 重新 begin 后旧 token 不再持有', () => {
    const { result } = renderAction()

    const stale = inAct(() => {
      const first = result.current.begin('todo-1')
      result.current.clear()
      result.current.begin('todo-1')
      return first
    }) as ExclusiveActionToken<string>

    expect(result.current.owns(stale)).toBe(false)
    expect(result.current.busyKey).toBe('todo-1')
  })

  it('clear 无条件释放', () => {
    const { result } = renderAction()

    const token = inAct(() => result.current.begin('todo-1')) as ExclusiveActionToken<string>
    inAct(() => result.current.clear())

    expect(result.current.busy).toBe(false)
    expect(result.current.busyKey).toBe(null)
    expect(result.current.owns(token)).toBe(false)
  })

  it('空闲时 finish 是空操作', () => {
    const { result } = renderAction()

    inAct(() => result.current.finish({ key: 'todo-1', generation: 99 }))

    expect(result.current.busy).toBe(false)
  })

  it('busy 与 busyKey 互不混淆：key 本身可以是 null', () => {
    const { result } = renderHook(() => useExclusiveAction<number | null>())

    inAct(() => result.current.begin(null))

    expect(result.current.busy).toBe(true)
    expect(result.current.busyKey).toBe(null)
  })
})
