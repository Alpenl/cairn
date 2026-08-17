import { describe, expect, it, vi } from 'vitest'
import { renderHook } from '@testing-library/react'
import { useSurfaceRequestGate, type SurfaceRequestGate } from './useSurfaceRequestGate'

type Channel = 'list' | 'detail'

interface GateProps {
  readonly owner: readonly unknown[]
  readonly authority?: () => boolean
}

function renderGate(initialProps: GateProps) {
  return renderHook(
    (props: GateProps) => useSurfaceRequestGate<Channel>(props),
    { initialProps },
  )
}

function gateOf(result: { current: SurfaceRequestGate<Channel> }): SurfaceRequestGate<Channel> {
  return result.current
}

describe('useSurfaceRequestGate', () => {
  it('刚取出的 token 既是当前请求也是同一 owner', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const token = gateOf(result).begin('list')

    expect(gateOf(result).isCurrent(token)).toBe(true)
    expect(gateOf(result).isSameOwner(token)).toBe(true)
    expect(token.channel).toBe('list')
  })

  it('后发请求顶掉先发请求，迟到的回包被丢弃', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const stale = gateOf(result).begin('list')
    const fresh = gateOf(result).begin('list')

    expect(gateOf(result).isCurrent(stale)).toBe(false)
    // 旧 finally 不能清掉新请求刚置上的 loading。
    expect(gateOf(result).isSameOwner(stale)).toBe(false)
    expect(gateOf(result).isCurrent(fresh)).toBe(true)
  })

  it('begin 只影响同一 channel', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const detail = gateOf(result).begin('detail')
    gateOf(result).begin('list')
    gateOf(result).begin('list')

    expect(gateOf(result).isCurrent(detail)).toBe(true)
  })

  it('capture 不顶掉在途请求，两个 token 同时有效', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const running = gateOf(result).begin('list')
    const bystander = gateOf(result).capture('list')

    expect(gateOf(result).isCurrent(running)).toBe(true)
    expect(gateOf(result).isCurrent(bystander)).toBe(true)
    expect(bystander.generation).toBe(running.generation)
    // 取号在闸门内是全局单调的，可用于「已应用代次」下限比较。
    expect(bystander.sequence).toBeGreaterThan(running.sequence)
  })

  it('unmount 之后 isCurrent 与 isSameOwner 都为 false', () => {
    const { result, unmount } = renderGate({ owner: ['client-a'] })
    const token = gateOf(result).begin('list')

    unmount()

    expect(gateOf(result).isCurrent(token)).toBe(false)
    expect(gateOf(result).isSameOwner(token)).toBe(false)
  })

  it('owner 变化后旧 token 失效，新 owner 的 token 有效', () => {
    const { result, rerender } = renderGate({ owner: ['client-a', 'lease-1'] })
    const stale = gateOf(result).begin('list')

    rerender({ owner: ['client-b', 'lease-2'] })

    expect(gateOf(result).isCurrent(stale)).toBe(false)
    expect(gateOf(result).isSameOwner(stale)).toBe(false)

    const fresh = gateOf(result).begin('list')
    expect(gateOf(result).isCurrent(fresh)).toBe(true)
  })

  it('owner 内容相同但数组是新引用时不算换主', () => {
    const client = { id: 'client-a' }
    const { result, rerender } = renderGate({ owner: [client, 'target'] })
    const token = gateOf(result).begin('list')

    rerender({ owner: [client, 'target'] })

    expect(gateOf(result).isCurrent(token)).toBe(true)
  })

  it('owner 长度变化也算换主', () => {
    const { result, rerender } = renderGate({ owner: ['client-a'] })
    const token = gateOf(result).begin('list')

    rerender({ owner: ['client-a', 'extra'] })

    expect(gateOf(result).isSameOwner(token)).toBe(false)
  })

  it('authority 被撤销后 isCurrent 为 false，但 isSameOwner 仍可释放 loading', () => {
    let identityCurrent = true
    const authority = vi.fn(() => identityCurrent)
    const { result } = renderGate({ owner: ['client-a'], authority })
    const token = gateOf(result).begin('list')

    expect(gateOf(result).isCurrent(token)).toBe(true)

    identityCurrent = false

    expect(gateOf(result).isCurrent(token)).toBe(false)
    expect(gateOf(result).isSameOwner(token)).toBe(true)
    expect(authority).toHaveBeenCalled()
  })

  it('isSameOwner 从不调用 authority', () => {
    const authority = vi.fn(() => true)
    const { result } = renderGate({ owner: ['client-a'], authority })
    const token = gateOf(result).begin('list')

    authority.mockClear()
    gateOf(result).isSameOwner(token)

    expect(authority).not.toHaveBeenCalled()
  })

  it('authority 换成新闭包后按新闭包判定', () => {
    const { result, rerender } = renderGate({ owner: ['client-a'], authority: () => true })
    const token = gateOf(result).begin('list')

    rerender({ owner: ['client-a'], authority: () => false })

    expect(gateOf(result).isCurrent(token)).toBe(false)
    expect(gateOf(result).isSameOwner(token)).toBe(true)
  })

  it('不传 authority 时视为始终有权威', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const token = gateOf(result).begin('list')

    expect(gateOf(result).isCurrent(token)).toBe(true)
  })

  it('invalidate 只作废指定 channel', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const list = gateOf(result).begin('list')
    const detail = gateOf(result).begin('detail')

    gateOf(result).invalidate('list')

    expect(gateOf(result).isCurrent(list)).toBe(false)
    expect(gateOf(result).isSameOwner(list)).toBe(false)
    expect(gateOf(result).isCurrent(detail)).toBe(true)
  })

  it('invalidateAll 作废全部 channel，之后新请求照常有效', () => {
    const { result } = renderGate({ owner: ['client-a'] })
    const list = gateOf(result).begin('list')
    const detail = gateOf(result).capture('detail')

    gateOf(result).invalidateAll()

    expect(gateOf(result).isCurrent(list)).toBe(false)
    expect(gateOf(result).isCurrent(detail)).toBe(false)

    const fresh = gateOf(result).begin('list')
    expect(gateOf(result).isCurrent(fresh)).toBe(true)
  })

  it('闸门对象在重渲染之间保持稳定引用', () => {
    const { result, rerender } = renderGate({ owner: ['client-a'] })
    const first = gateOf(result)

    rerender({ owner: ['client-b'] })

    expect(gateOf(result)).toBe(first)
  })
})
