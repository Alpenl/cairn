/// <reference types="vite/client" />

interface Window {
  /** 页面入口写入的扩展版本号，便于在控制台里确认装的是哪一版。 */
  appVersion: string
  /** Content Script 的 RSS 发现初始化守卫，防止重复注入时重复注册监听。 */
  __webtagRssDiscoveryInit: boolean
}
