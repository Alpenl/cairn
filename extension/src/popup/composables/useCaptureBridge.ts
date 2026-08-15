/**
 * @module popup/composables/useCaptureBridge
 * popup 侧的采集消息桥 —— 封装与 background captureHandler 的 webext-bridge 通信。
 *
 * 为什么单独封一层：
 *   - webtag:* 消息 ID 已在 ProtocolMap（src/types/shims.d.ts）登记，payload /
 *     return 类型来自 capture-protocol.ts，sendMessage / onMessage 直接强类型，
 *     无需断言。本文件仍保留是为了给组件一个语义清晰的窄接口
 *     （start / fetchLatest / onUpdate），并集中管理 onScopeDispose 解绑。
 *   - 组件只面对强类型 API：start() / fetchLatest() / onUpdate()。
 *
 * 消费者：src/popup/components/CaptureForm.vue。
 */

import { onScopeDispose } from 'vue'
import { onMessage, sendMessage } from 'webext-bridge/popup'
import {
  MSG_CAPTURE_STATUS_UPDATE,
  MSG_GET_CAPTURE_STATUS,
  MSG_START_CAPTURE,
  type CaptureSnapshot,
  type RequestedLibraryKind,
  type StartCapturePayload,
} from '@/background/capture-protocol'

/** useCaptureBridge 暴露给组件的 API。 */
export interface CaptureBridge {
  /**
   * 触发一次采集，返回 background 给出的初始快照（capturing/submitted/done/failed）。
   *
   * @param note  用户备注
   * @param tabId popup 已解析的活动标签 id；传它保证采集到「用户看到的那个标签」，
   *              而非 background 自行 re-query 出的可能已切换的活动标签
   */
  start: (
    note: string,
    tabId?: number,
    requestedKind?: RequestedLibraryKind,
  ) => Promise<CaptureSnapshot>
  /** 拉取 background 持有的最近一次采集快照（popup 打开时调用）。 */
  fetchLatest: () => Promise<CaptureSnapshot>
  /**
   * 订阅 background 推送的采集状态更新。
   * 返回取消订阅函数；组件作用域销毁时也会自动取消。
   */
  onUpdate: (handler: (snapshot: CaptureSnapshot) => void) => () => void
}

/**
 * 创建 popup 侧采集消息桥。
 *
 * 通信目标固定为 `background`（captureHandler 在 SW 中注册了对应监听）。
 */
export function useCaptureBridge(): CaptureBridge {
  const start = async (
    note: string,
    tabId?: number,
    requestedKind?: RequestedLibraryKind,
  ): Promise<CaptureSnapshot> => {
    // tabId 为 undefined 时不写入载荷（保持 payload 干净）。
    const payload: StartCapturePayload = {
      note,
      ...(requestedKind === undefined ? {} : { requestedKind }),
      ...(tabId === undefined ? {} : { tabId }),
    }
    // ProtocolMap 已登记 webtag:start-capture，data / 返回值均为强类型。
    return sendMessage(MSG_START_CAPTURE, payload, 'background')
  }

  const fetchLatest = async (): Promise<CaptureSnapshot> => {
    // 无载荷消息：传空对象（协议登记为 Record<string, never>）。
    return sendMessage(MSG_GET_CAPTURE_STATUS, {}, 'background')
  }

  const onUpdate = (
    handler: (snapshot: CaptureSnapshot) => void,
  ): (() => void) => {
    const unsubscribe = onMessage(MSG_CAPTURE_STATUS_UPDATE, ({ data }) => {
      handler(data)
    })
    // 组件作用域销毁时自动解绑，避免 popup 多次开关后监听器泄漏。
    onScopeDispose(unsubscribe)
    return unsubscribe
  }

  return { start, fetchLatest, onUpdate }
}
