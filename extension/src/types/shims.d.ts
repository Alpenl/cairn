/* eslint-disable @typescript-eslint/no-empty-object-type */
import type { ProtocolWithReturn } from 'webext-bridge'
import { DefineComponent } from 'vue'
import type {
  CaptureSnapshot,
  StartCapturePayload,
} from '@/background/capture-protocol'

declare module '*.vue' {
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module '*.md' {
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module 'webext-bridge' {
  export interface ProtocolMap {
    // define message protocol types
    // see https://github.com/antfu/webext-bridge#type-safe-protocols
    'tab-prev': { title: string | undefined }
    'get-current-tab': ProtocolWithReturn<{ tabId: number }, { title?: string }>
    // ── 当前页采集（webtag:*）协议 ──────────────────────────
    // payload / return 类型来自 src/background/capture-protocol.ts。
    // 登记后 captureHandler.ts / useCaptureBridge.ts 即可直接用强类型
    // sendMessage / onMessage，无需 `as unknown as` 收口断言。
    /** popup → background：触发一次当前页采集，返回初始快照。 */
    'webtag:start-capture': ProtocolWithReturn<
      StartCapturePayload,
      CaptureSnapshot
    >
    /** popup → background：拉取最近一次采集快照。 */
    'webtag:get-capture-status': ProtocolWithReturn<
      Record<string, never>,
      CaptureSnapshot
    >
    /** background → popup：采集状态推进时推送的最新快照。 */
    'webtag:capture-status-update': CaptureSnapshot
  }
}
