/**
 * Extension 构建目标的唯一描述：页面入口、background 入口、图标资产、
 * manifest 权限与 RSS content script 接线。当前只有采集扩展一个发布形态，
 * 因此这里不再暴露 profile 参数或未来分支。
 */
export type WebView = 'popup' | 'options'
export type PreparedView = WebView | 'background'

export const webViewEntries: Readonly<Record<WebView, string>> = {
  popup: 'popup/main.capture.ts',
  options: 'options/main.capture.ts',
}

export const preparedViews: readonly PreparedView[] = [
  'popup',
  'options',
  'background',
]

export const backgroundEntry = 'src/background/main.ts'

export const contentScript = {
  path: 'dist/contentScripts/rss.global.js',
  entry: 'src/contentScripts/rss-entry.ts',
} as const

export const manifestPermissions = [
  'storage',
  'tabs',
  'scripting',
  'contextMenus',
  'alarms',
] as const

export function shouldCopyExtensionAsset(relativePath: string): boolean {
  return (
    relativePath === '' ||
    relativePath === 'img' ||
    relativePath === 'img/icon' ||
    relativePath.startsWith('img/icon/')
  )
}

export function getWebViews(): WebView[] {
  return Object.keys(webViewEntries) as WebView[]
}
