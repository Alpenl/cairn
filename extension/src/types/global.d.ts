/// <reference types="vite/client" />

// 由 vite.config.ts 的 define 注入。
declare const __DEV__: boolean
declare const __NAME__: string // 扩展名，取自 package.json 的 name

interface Window {
  /** 页面入口写入的扩展版本号，便于在控制台里确认装的是哪一版。 */
  appVersion: string
  /** Content Script 的 RSS 发现初始化守卫，防止重复注入时重复注册监听。 */
  __webtagRssDiscoveryInit: boolean
}
