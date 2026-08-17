import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { restoreFocusWhenReady } from './restore-focus'

function button(disabled = false): HTMLButtonElement {
  const element = document.createElement('button')
  element.disabled = disabled
  element.textContent = 'target'
  document.body.append(element)
  return element
}

describe('restoreFocusWhenReady', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    // rAF 在 jsdom 里也会排队；把它接到假时钟上，测试才能确定性地推进帧。
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => (
      window.setTimeout(() => callback(performance.now()), 16)
    ))
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
    document.body.replaceChildren()
  })

  it('focuses a target that is ready on the first attempt', () => {
    const target = button()
    restoreFocusWhenReady({ target: () => target })
    vi.advanceTimersByTime(1)
    expect(target).toHaveFocus()
  })

  // 这条是这个模块存在的理由。禁用的按钮无法接收焦点，`focus()` 静默失败；
  // 只判断「拿到了引用」的实现会在这里把键盘用户留在 <body> 上。
  it('keeps retrying while the target is disabled and focuses it once enabled', () => {
    const target = button(true)
    restoreFocusWhenReady({ target: () => target })

    vi.advanceTimersByTime(100)
    expect(target).not.toHaveFocus()
    expect(document.body).toHaveFocus()

    target.disabled = false
    vi.advanceTimersByTime(32)
    expect(target).toHaveFocus()
  })

  // 目标可能在重渲染里被换成新节点，所以每次尝试都要重新求值。
  it('re-reads the target so a replaced node still receives focus', () => {
    let target: HTMLButtonElement | null = null
    restoreFocusWhenReady({ target: () => target })

    vi.advanceTimersByTime(64)
    target = button()
    vi.advanceTimersByTime(32)
    expect(target).toHaveFocus()
  })

  it('gives up after the attempt cap instead of polling forever', () => {
    const target = button(true)
    restoreFocusWhenReady({ target: () => target, maxAttempts: 3 })

    vi.advanceTimersByTime(1000)
    target.disabled = false
    vi.advanceTimersByTime(1000)
    expect(target).not.toHaveFocus()
  })

  it('stops when cancelled so an unmounted surface leaves no timers behind', () => {
    const target = button(true)
    const cancel = restoreFocusWhenReady({ target: () => target })

    cancel()
    target.disabled = false
    vi.advanceTimersByTime(1000)
    expect(target).not.toHaveFocus()
  })
})
