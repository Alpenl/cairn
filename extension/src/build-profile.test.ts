import { describe, expect, it } from 'vitest'
import {
  backgroundEntry,
  contentScript,
  getWebViews,
  manifestPermissions,
  preparedViews,
  shouldCopyExtensionAsset,
  webViewEntries,
} from '../scripts/build-profile'

// 构建目标是产物形态的唯一描述：入口、资产、content script、权限。
// 这些断言的意义在于「改这张表的人必须知道自己在改什么」。
describe('extension build descriptor', () => {
  it('只装配采集链路需要的两个页面入口和采集 background', () => {
    expect(webViewEntries).toEqual({
      popup: 'popup/main.capture.ts',
      options: 'options/main.capture.ts',
    })
    expect(backgroundEntry).toBe('src/background/main.ts')
    expect(getWebViews()).toEqual(['popup', 'options'])
    expect(preparedViews).toEqual(['popup', 'options', 'background'])
  })

  it('只注册 RSS 发现这一条 content script', () => {
    expect(contentScript.path).toBe('dist/contentScripts/rss.global.js')
    expect(contentScript.entry).toBe('src/contentScripts/rss-entry.ts')
  })

  // 权限是用户在安装弹窗里唯一看得见的东西，扩大一项都要有明确理由。
  // 上游 NaiveTab 的 sessions / tabGroups / system.display / bookmarks /
  // notifications 随旧 full build 一起删掉了，别再顺手加回来。
  it('权限收敛在采集必需项，且不请求任何可选权限', () => {
    expect([...manifestPermissions]).toEqual([
      'storage',
      'tabs',
      'scripting',
      'contextMenus',
      'alarms',
    ])
  })

  it('资产只拷图标：上游的字体 / 键盘 / 时钟图片不再进产物', () => {
    expect(shouldCopyExtensionAsset('img/icon/icon-48x48.png')).toBe(true)
    expect(shouldCopyExtensionAsset('fonts/OpenCherry-Regular.woff')).toBe(
      false,
    )
    expect(shouldCopyExtensionAsset('img/keyboard/anne-pro-2.png')).toBe(false)
  })
})
