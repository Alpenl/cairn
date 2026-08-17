/**
 * 把焦点还给一个「可能还没准备好」的目标。
 *
 * 键盘用户按下删除之后，焦点所在的那个按钮会连同它所在的行一起消失。如果
 * 没人把焦点接住，浏览器会把它丢回 `<body>`，读屏用户就此失去位置。接住它
 * 有两个独立的难点，两个都出现过：
 *
 *  1. **目标还没挂上。** 删除后列表要重渲染，目标按钮的 ref 未必已经存在，
 *     此时 `focus()` 静默什么也不做。
 *  2. **目标存在但不可聚焦。** 调用方的视图按钮在删除进行中是 `disabled` 的，
 *     而**禁用元素无法接收焦点**——`focus()` 同样静默失败。
 *
 * 第二点是这个模块存在的理由：只判断「拿到了引用」会在快机器上碰巧通过，在
 * 慢机器上确定性地失败。所以收工的判据是**焦点确实落在目标上**，不是找到了
 * 目标。
 */

/** 一次尝试之间的间隔，约一帧。 */
const RETRY_INTERVAL_MS = 16

/**
 * 尝试上限。约 20 帧；再等下去说明这一屏根本没有可聚焦的落点，继续轮询只会
 * 在后台标签页里空转。
 */
const MAX_ATTEMPTS = 20

export interface RestoreFocusOptions {
  /** 每次尝试重新求值——目标可能在重渲染中被替换。 */
  readonly target: () => HTMLElement | null | undefined
  readonly retryIntervalMs?: number
  readonly maxAttempts?: number
}

/** 取消一次尚未完成的恢复，供调用方在卸载时释放定时器。 */
export type CancelRestoreFocus = () => void

function focusable(element: HTMLElement | null | undefined): element is HTMLElement {
  if (!element) return false
  // `disabled` 只存在于表单控件上；其它元素没有这个属性，也就不受它限制。
  return !(element as HTMLElement & { disabled?: boolean }).disabled
}

export function restoreFocusWhenReady(options: RestoreFocusOptions): CancelRestoreFocus {
  const retryInterval = options.retryIntervalMs ?? RETRY_INTERVAL_MS
  const maxAttempts = options.maxAttempts ?? MAX_ATTEMPTS

  let settled = false
  let attempts = 0
  const timers = new Set<number>()

  const cancel: CancelRestoreFocus = () => {
    settled = true
    for (const timer of timers) window.clearTimeout(timer)
    timers.clear()
  }

  const attempt = () => {
    if (settled) return
    const target = options.target()
    if (focusable(target)) {
      target.focus()
      // 焦点真的落上了才算完。否则说明目标此刻仍不接受焦点，下一帧带着
      // 刷新后的状态再试。
      if (document.activeElement === target) {
        cancel()
        return
      }
    }
    attempts += 1
    if (attempts >= maxAttempts) {
      cancel()
      return
    }
    timers.add(window.setTimeout(attempt, retryInterval))
  }

  // rAF 只在页面参与渲染时回调——标签页切到后台、窗口最小化或无显示环境里
  // 可能迟迟不触发甚至不触发。用一个立即的 setTimeout 并行兜底。
  window.requestAnimationFrame(attempt)
  timers.add(window.setTimeout(attempt, 0))

  return cancel
}
