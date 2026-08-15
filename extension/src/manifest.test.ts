import { afterEach, describe, expect, it } from 'vitest'
import { getManifest } from './manifest'

const originalBrowser = process.env.BROWSER

afterEach(() => {
  if (originalBrowser === undefined) delete process.env.BROWSER
  else process.env.BROWSER = originalBrowser
})

describe('getManifest', () => {
  it('只注册轻量 RSS content script 与精简权限', async () => {
    process.env.BROWSER = 'chrome'
    const manifest = await getManifest()

    // 不接管新标签页：那是上游 NaiveTab 的形态，删掉 full profile 后不该再回来。
    expect(manifest.chrome_url_overrides).toBeUndefined()
    expect(manifest.content_scripts).toEqual([
      expect.objectContaining({
        js: ['dist/contentScripts/rss.global.js'],
        run_at: 'document_start',
      }),
    ])
    // 这四项是上游为 Widget / 键盘书签申请的权限，采集扩展一项都不需要。
    // 权限是安装弹窗里用户唯一看得见的东西，悄悄加回来会直接影响装机转化。
    expect(manifest.permissions).not.toEqual(
      expect.arrayContaining([
        'sessions',
        'tabGroups',
        'system.display',
        'favicon',
      ]),
    )
    expect(manifest.optional_permissions).toBeUndefined()
  })

  it('Firefox 使用 scripts adapter 且不添加 Chrome favicon 权限', async () => {
    process.env.BROWSER = 'firefox'

    const manifest = await getManifest()

    expect(manifest.background).toEqual({
      scripts: ['/dist/background/index.mjs'],
      type: 'module',
    })
    expect(manifest.browser_specific_settings?.gecko).toEqual({
      id: 'webtag@webtag.local',
      strict_min_version: '130.0',
    })
    expect(manifest.permissions).not.toContain('favicon')
  })
})
