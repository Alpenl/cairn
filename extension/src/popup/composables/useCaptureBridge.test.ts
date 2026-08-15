/**
 * useCaptureBridge.test.ts — popup 侧采集消息桥单元测试。
 *
 * 测试隔离策略：
 *   - vi.mock 掉 webext-bridge/popup，用可控的 sendMessage / onMessage stub，
 *     验证 useCaptureBridge 是否发送了正确的消息 ID / 目标 / 载荷，
 *     以及订阅 / 取消订阅是否正确管理，不触碰真实扩展消息总线。
 *   - onUpdate 的 onScopeDispose 自动解绑：在 effectScope 内创建桥，
 *     scope.stop() 模拟组件作用域销毁，断言订阅被取消。
 *
 * 覆盖：
 *   - start：发 webtag:start-capture → background，载荷为 { note }
 *   - fetchLatest：发 webtag:get-capture-status → background，载荷为空对象
 *   - onUpdate：经 onMessage 订阅 webtag:capture-status-update，
 *     回调收到推送快照；返回的 cleanup 与 onScopeDispose 均能解绑
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope } from 'vue'
import {
  MSG_CAPTURE_STATUS_UPDATE,
  MSG_GET_CAPTURE_STATUS,
  MSG_START_CAPTURE,
  type CaptureSnapshot,
} from '@/background/capture-protocol'

// ── webext-bridge/popup mock ────────────────────────────────

/**
 * onMessage 注册的活跃监听器表：messageId → 处理函数。
 * onMessage 返回的「取消订阅」函数会从表中删除对应项，
 * emitMessage 工具据此模拟 background 推送。
 */
const messageListeners = new Map<string, (msg: { data: unknown }) => void>()

/** sendMessage stub：每个用例可改写返回值；调用参数由 vi.fn 自动记录。 */
const sendMessage = vi.fn<
  (messageId: string, data: unknown, target: string) => Promise<unknown>
>(() => Promise.resolve(undefined))

/**
 * onMessage stub：登记监听器并返回取消订阅函数。
 * 取消订阅时只删除「当前这一份」监听器（按引用比对），与真实行为一致。
 */
const onMessage = vi.fn(
  (messageId: string, handler: (msg: { data: unknown }) => void) => {
    messageListeners.set(messageId, handler)
    return () => {
      if (messageListeners.get(messageId) === handler) {
        messageListeners.delete(messageId)
      }
    }
  },
)

vi.mock('webext-bridge/popup', () => ({
  sendMessage: (messageId: string, data: unknown, target: string) =>
    sendMessage(messageId, data, target),
  onMessage: (messageId: string, handler: (msg: { data: unknown }) => void) =>
    onMessage(messageId, handler),
}))

// mock 之后再 import 被测模块，确保拿到 mock 版本的 webext-bridge。
const { useCaptureBridge } = await import('./useCaptureBridge')

// ── 测试夹具 ────────────────────────────────────────────────

/** 构造一个最小可用的采集快照。 */
function makeSnapshot(
  overrides: Partial<CaptureSnapshot> = {},
): CaptureSnapshot {
  return {
    stage: 'done',
    url: 'https://example.com',
    title: '示例',
    updatedAt: 1_700_000_000_000,
    ...overrides,
  }
}

/** 模拟 background 向 popup 推送指定消息。 */
function emitMessage(messageId: string, data: unknown): void {
  messageListeners.get(messageId)?.({ data })
}

beforeEach(() => {
  messageListeners.clear()
  sendMessage.mockClear()
  sendMessage.mockResolvedValue(undefined)
  onMessage.mockClear()
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('useCaptureBridge — start', () => {
  it('发送 webtag:start-capture 到 background，载荷为 { note }', async () => {
    const snapshot = makeSnapshot({ stage: 'capturing' })
    sendMessage.mockResolvedValue(snapshot)

    const bridge = useCaptureBridge()
    const result = await bridge.start('我的备注')

    expect(sendMessage).toHaveBeenCalledTimes(1)
    expect(sendMessage).toHaveBeenCalledWith(
      MSG_START_CAPTURE,
      { note: '我的备注' },
      'background',
    )
    expect(result).toEqual(snapshot)
  })

  it('空备注同样原样作为 note 载荷透传', async () => {
    sendMessage.mockResolvedValue(makeSnapshot())

    const bridge = useCaptureBridge()
    await bridge.start('')

    expect(sendMessage).toHaveBeenCalledWith(
      MSG_START_CAPTURE,
      { note: '' },
      'background',
    )
  })

  it('传入 tabId 时随载荷一并透传给 background', async () => {
    sendMessage.mockResolvedValue(makeSnapshot())

    const bridge = useCaptureBridge()
    await bridge.start('备注', 42)

    expect(sendMessage).toHaveBeenCalledWith(
      MSG_START_CAPTURE,
      { note: '备注', tabId: 42 },
      'background',
    )
  })

  it('未传 tabId 时载荷不含 tabId 字段', async () => {
    sendMessage.mockResolvedValue(makeSnapshot())

    const bridge = useCaptureBridge()
    await bridge.start('备注')

    expect(sendMessage).toHaveBeenCalledWith(
      MSG_START_CAPTURE,
      { note: '备注' },
      'background',
    )
  })

  it('显式网站意图随载荷透传给 background', async () => {
    sendMessage.mockResolvedValue(makeSnapshot())
    const bridge = useCaptureBridge()
    await bridge.start('备注', 42, 'site')
    expect(sendMessage).toHaveBeenCalledWith(
      MSG_START_CAPTURE,
      { note: '备注', tabId: 42, requestedKind: 'site' },
      'background',
    )
  })
})

describe('useCaptureBridge — fetchLatest', () => {
  it('发送 webtag:get-capture-status 到 background，载荷为空对象', async () => {
    const snapshot = makeSnapshot({ stage: 'parsing' })
    sendMessage.mockResolvedValue(snapshot)

    const bridge = useCaptureBridge()
    const result = await bridge.fetchLatest()

    expect(sendMessage).toHaveBeenCalledTimes(1)
    expect(sendMessage).toHaveBeenCalledWith(
      MSG_GET_CAPTURE_STATUS,
      {},
      'background',
    )
    expect(result).toEqual(snapshot)
  })
})

describe('useCaptureBridge — onUpdate', () => {
  it('经 onMessage 订阅 webtag:capture-status-update，回调收到推送快照', () => {
    const bridge = useCaptureBridge()
    const handler = vi.fn()
    bridge.onUpdate(handler)

    expect(onMessage).toHaveBeenCalledTimes(1)
    expect(onMessage).toHaveBeenCalledWith(
      MSG_CAPTURE_STATUS_UPDATE,
      expect.any(Function),
    )

    const pushed = makeSnapshot({ stage: 'submitted' })
    emitMessage(MSG_CAPTURE_STATUS_UPDATE, pushed)

    expect(handler).toHaveBeenCalledTimes(1)
    expect(handler).toHaveBeenCalledWith(pushed)
  })

  it('调用返回的 cleanup 后取消订阅，后续推送不再触达回调', () => {
    const bridge = useCaptureBridge()
    const handler = vi.fn()
    const cleanup = bridge.onUpdate(handler)

    cleanup()
    emitMessage(MSG_CAPTURE_STATUS_UPDATE, makeSnapshot())

    expect(handler).not.toHaveBeenCalled()
    // 订阅已从活跃监听器表移除，无监听器泄漏。
    expect(messageListeners.has(MSG_CAPTURE_STATUS_UPDATE)).toBe(false)
  })

  it('作用域销毁时 onScopeDispose 自动解绑，无监听器泄漏', () => {
    const scope = effectScope()
    const handler = vi.fn()
    scope.run(() => {
      const bridge = useCaptureBridge()
      bridge.onUpdate(handler)
    })

    // 作用域存活时订阅有效。
    expect(messageListeners.has(MSG_CAPTURE_STATUS_UPDATE)).toBe(true)

    // 模拟组件作用域销毁 → onScopeDispose 触发 unsubscribe。
    scope.stop()
    emitMessage(MSG_CAPTURE_STATUS_UPDATE, makeSnapshot())

    expect(handler).not.toHaveBeenCalled()
    expect(messageListeners.has(MSG_CAPTURE_STATUS_UPDATE)).toBe(false)
  })
})
