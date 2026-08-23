/**
 * Extension 构建目标的唯一描述：页面入口、background 入口、要拷贝的资产、
 * manifest 权限与 content script 接线。构建配置（vite.config*.ts）、
 * manifest 生成（src/manifest.ts）、产物校验（scripts/verify-build-output.ts）
 * 都从这里取，禁止再用散落注释或环境变量切换产品形态。
 *
 * 历史：这里曾有两个产品形态 —— `capture`（采集扩展，发布形态）与 `full`
 * （恢复上游 NaiveTab 的新标签页、Widget、快捷键、壁纸等）。full 从未发布，
 * 却让 src/ 里约 49,000 行不进产物的代码继续参与 lint / typecheck / test /
 * 依赖审计，并让整个目录背着上游的 GPL-3.0。那批代码已经删除；现在只有
 * 一个 Extension 构建目标，因此不再保留 profile 分支或可注入配置对象。
 */
export type WebView = 'popup' | 'options'
export type PreparedView = WebView | 'background'

export const BUILD_TARGET = 'extension'
export const WEB_VIEWS: readonly WebView[] = ['popup', 'options']
export const PREPARED_VIEWS: readonly PreparedView[] = [
  ...WEB_VIEWS,
  'background',
]

/** 页面视图 → 相对 src/ 的入口模块路径。 */
export const VIEW_ENTRIES: Readonly<Record<WebView, string>> = {
  popup: 'popup/main.capture.ts',
  options: 'options/main.capture.ts',
}

export const BACKGROUND_ENTRY = 'src/background/main.ts'
export const CONTENT_SCRIPT_PATH = 'dist/contentScripts/rss.global.js'
export const CONTENT_SCRIPT_ENTRY = 'src/contentScripts/rss-entry.ts'
export const EXTENSION_PERMISSIONS = [
  'storage',
  'tabs',
  'scripting',
  'contextMenus',
  'alarms',
] as const

/** 相对 assets/ 的路径是否进入产物。 */
export function shouldCopyExtensionAsset(relativePath: string): boolean {
  // 只拷图标：字体、键盘/时钟/赞助图片都是上游新标签页的资产，随 full 一起删了。
  return (
    relativePath === '' ||
    relativePath === 'img' ||
    relativePath === 'img/icon' ||
    relativePath.startsWith('img/icon/')
  )
}
