import { describe, expect, it } from 'vitest'
import {
  BACKGROUND_ENTRY,
  CONTENT_SCRIPT_ENTRY,
  CONTENT_SCRIPT_PATH,
  EXTENSION_PERMISSIONS,
  PREPARED_VIEWS,
  shouldCopyExtensionAsset,
  VIEW_ENTRIES,
  WEB_VIEWS,
} from '../scripts/build-target'

// 构建目标是产物形态的唯一描述：入口、资产、content script、权限。
// 这些断言的意义在于「改这张表的人必须知道自己在改什么」。
describe('Extension build target', () => {
  it('只装配采集链路需要的两个页面入口和采集 background', () => {
    expect(VIEW_ENTRIES).toEqual({
      popup: 'popup/main.capture.ts',
      options: 'options/main.capture.ts',
    })
    expect(BACKGROUND_ENTRY).toBe('src/background/main.ts')
    expect(WEB_VIEWS).toEqual(['popup', 'options'])
    expect(PREPARED_VIEWS).toEqual(['popup', 'options', 'background'])
  })

  it('只注册 RSS 发现这一条 content script', () => {
    expect(CONTENT_SCRIPT_PATH).toBe('dist/contentScripts/rss.global.js')
    expect(CONTENT_SCRIPT_ENTRY).toBe('src/contentScripts/rss-entry.ts')
  })

  // 权限是用户在安装弹窗里唯一看得见的东西，扩大一项都要有明确理由。
  // 上游 NaiveTab 的 sessions / tabGroups / system.display / bookmarks /
  // notifications 随 full 一起删掉了，别再顺手加回来。
  it('权限收敛在采集必需项', () => {
    expect([...EXTENSION_PERMISSIONS]).toEqual([
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
