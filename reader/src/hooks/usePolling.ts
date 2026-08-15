import { useEffect, useRef } from 'react'

/**
 * 按固定间隔调用 callback。delay 为 null 时停表。
 *
 * callback 走 ref：调用方通常现写箭头函数，逐次新建。直接进依赖数组会让定时器
 * 每次渲染都被清掉重建——间隔越短越明显，1.2 秒的译文轮询会退化成"几乎不触发"。
 */
export function usePolling(callback: () => void, delay: number | null): void {
  const saved = useRef(callback)
  saved.current = callback

  useEffect(() => {
    if (delay === null) return
    const timer = window.setInterval(() => saved.current(), delay)
    return () => window.clearInterval(timer)
  }, [delay])
}
