// generate stub index.html files for dev entry
import { copyFile, cp, mkdir, readFile, writeFile } from 'node:fs/promises'
import chokidar from 'chokidar'
import { relative } from 'node:path'
import { getManifest } from '../src/manifest'
import { isDev, log, port, r, BROWSER_DIR } from './utils'
import {
  BACKGROUND_ENTRY,
  PREPARED_VIEWS,
  shouldCopyExtensionAsset,
  VIEW_ENTRIES,
} from './build-target'
import { LEGAL_FILES } from './release-artifacts'

/**
 * Stub index.html to use Vite in development
 */
async function stubIndexHtml() {
  for (const view of PREPARED_VIEWS) {
    await mkdir(r(`${BROWSER_DIR}/dist/${view}`), { recursive: true })
    let data = await readFile(r(`src/${view}/index.html`), 'utf-8')

    const sourceEntry =
      view === 'background'
        ? BACKGROUND_ENTRY.replace(/^src\//, '')
        : VIEW_ENTRIES[view]
    if (!sourceEntry) {
      throw new Error(`Missing source entry for ${view}`)
    }
    data = data
      .replace('"./main.ts"', `"http://localhost:${port}/${sourceEntry}"`)
      .replace(
        '<div id="app"></div>',
        '<div id="app">Vite server did not start</div>',
      )

    data += `<style type="text/css">
        @media (prefers-color-scheme: dark) {
          body {
            background: #35363A;
            color: #fff;
          }
        }
        @media (prefers-color-scheme: light) {
          body {
            background: #fff;
            color: #35363A;
          }
        }
      </style>`
    await writeFile(r(`${BROWSER_DIR}/dist/${view}/index.html`), data, 'utf-8')
    log('PRE', `stub ${view}`)
  }
}

async function writeManifest() {
  await writeFile(
    r(`${BROWSER_DIR}/manifest.json`),
    `${JSON.stringify(await getManifest(), null, 2)}\n`,
  )
  log('PRE', `write ${BROWSER_DIR}/manifest.json`)
}

async function writeLocales() {
  await cp(r('src/_locales'), r(`${BROWSER_DIR}/_locales`), {
    recursive: true,
  })
  log('PRE', 'write _locales')
}

await mkdir(r(BROWSER_DIR), { recursive: true })
const assetsRoot = r('assets')
await cp(r('assets'), r(`${BROWSER_DIR}/assets`), {
  recursive: true,
  filter: (src) => {
    if (src.endsWith('.DS_Store')) return false
    const assetPath = relative(assetsRoot, src).replaceAll('\\', '/')
    return shouldCopyExtensionAsset(assetPath)
  },
})
for (const name of LEGAL_FILES) {
  await copyFile(r(name), r(`${BROWSER_DIR}/${name}`))
}

await writeManifest()
await writeLocales()

if (isDev) {
  stubIndexHtml()
  chokidar.watch(r('src/**/*.html')).on('change', () => {
    stubIndexHtml()
  })
  chokidar.watch([r('src/manifest.ts'), r('package.json')]).on('change', () => {
    void writeManifest()
  })
  chokidar.watch(r('src/_locales')).on('change', () => {
    void writeLocales()
  })
}
