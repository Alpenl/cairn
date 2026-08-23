import { readFile } from 'node:fs/promises'
import type { Manifest } from 'webextension-polyfill'
import type PkgType from '../package.json'
import { isDev, port, r } from '../scripts/utils'
import {
  CONTENT_SCRIPT_PATH,
  EXTENSION_PERMISSIONS,
} from '../scripts/build-target'

export async function getManifest() {
  const pkg = JSON.parse(
    await readFile(r('package.json'), 'utf8'),
  ) as typeof PkgType

  const manifest: Manifest.WebExtensionManifest = {
    manifest_version: 3,
    name: '__MSG_appName__',
    version: pkg.version,
    description: '__MSG_appDesc__',
    default_locale: 'zh_CN',
    action: {
      default_icon: '/assets/img/icon/icon.png',
      default_popup: '/dist/popup/index.html',
    },
    icons: {
      16: '/assets/img/icon/icon-16x16.png',
      48: '/assets/img/icon/icon-48x48.png',
      128: '/assets/img/icon/icon-128x128.png',
    },
    permissions: [...EXTENSION_PERMISSIONS] as Manifest.Permission[],
    // 采集脚本需注入任意当前页；后端地址由用户自填，因此仍保留全站 host 权限。
    host_permissions: ['*://*/*'],
    // 不接管新标签页：那是上游 NaiveTab 的产品形态，Cairn 是采集扩展。
    options_ui: {
      page: '/dist/options/index.html',
      open_in_tab: true,
    },
    background: {
      service_worker: '/dist/background/index.mjs',
      type: 'module',
    },
    // 一个扩展可以有很多命令，但只能指定 4 个建议的键。
    // commands: {},
    // web_accessible_resources: [
    //   {
    //     resources: ['dist/contentScripts/style.css'],
    //     matches: ['<all_urls>'],
    //   },
    // ],
    content_scripts: [
      {
        matches: ['<all_urls>'],
        match_about_blank: true,
        js: [CONTENT_SCRIPT_PATH],
        run_at: 'document_start' as const,
      },
    ],
    content_security_policy: {
      // this is required on dev for Vite script to load
      extension_pages: isDev
        ? `script-src 'self' http://localhost:${port}; object-src 'self'`
        : `script-src 'self'; object-src 'self'`,
    },
  }

  if (process.env.BROWSER === 'firefox') {
    manifest.background = {
      scripts: ['/dist/background/index.mjs'],
      type: 'module',
    }
    manifest.browser_specific_settings = {
      gecko: {
        id: 'webtag@webtag.local',
        strict_min_version: '130.0',
      },
    }
  }
  return manifest
}
