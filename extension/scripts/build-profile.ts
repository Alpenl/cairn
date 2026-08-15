/**
 * 扩展构建目标的唯一描述：页面入口、background 入口、要拷贝的资产、
 * manifest 权限与 content script 接线。构建配置（vite.config*.ts）、
 * manifest 生成（src/manifest.ts）、产物校验（scripts/verify-build-output.ts）
 * 都从这里取，禁止再用散落注释或环境变量切换产品形态。
 *
 * 历史：这里曾有两个 profile —— `capture`（采集扩展，发布形态）与 `full`
 * （恢复上游 NaiveTab 的新标签页、Widget、快捷键、壁纸等）。full 从未发布，
 * 却让 src/ 里约 49,000 行不进产物的代码继续参与 lint / typecheck / test /
 * 依赖审计，并让整个目录背着上游的 GPL-3.0。那批代码连同 full 一起删除后，
 * 只剩一个构建目标，profile 的分支也就没有存在意义了。
 */
export type WebView = 'popup' | 'options'
export type PreparedView = WebView | 'background'

export interface BuildProfile {
  name: string
  /** 页面视图 → 相对 src/ 的入口模块路径。 */
  entries: Readonly<Partial<Record<WebView, string>>>
  backgroundEntry: string
  /** 相对 assets/ 的路径是否进入产物。 */
  shouldCopyAsset: (relativePath: string) => boolean
  manifest: {
    permissions: readonly string[]
    optionalPermissions: readonly string[]
    contentScriptPath?: string
    contentScriptEntry?: string
    includeChromeFavicon: boolean
  }
}

export const buildProfile: BuildProfile = {
  name: 'capture',
  entries: {
    popup: 'popup/main.capture.ts',
    options: 'options/main.capture.ts',
  },
  backgroundEntry: 'src/background/main.ts',
  // 只拷图标：字体、键盘/时钟/赞助图片都是上游新标签页的资产，随 full 一起删了。
  shouldCopyAsset: (relativePath) =>
    relativePath === '' ||
    relativePath === 'img' ||
    relativePath === 'img/icon' ||
    relativePath.startsWith('img/icon/'),
  manifest: {
    permissions: ['storage', 'tabs', 'scripting', 'contextMenus', 'alarms'],
    optionalPermissions: [],
    contentScriptPath: 'dist/contentScripts/rss.global.js',
    contentScriptEntry: 'src/contentScripts/rss-entry.ts',
    includeChromeFavicon: false,
  },
}

export function getWebViews(profile: BuildProfile = buildProfile): WebView[] {
  return Object.keys(profile.entries) as WebView[]
}

export function getPreparedViews(
  profile: BuildProfile = buildProfile,
): PreparedView[] {
  return [...getWebViews(profile), 'background']
}
