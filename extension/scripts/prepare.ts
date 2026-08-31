import chokidar from 'chokidar'
import { copyFile, cp, mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, relative } from 'node:path'
import { getManifest } from '../src/manifest'
import { isDev, log, port, r, BROWSER_DIR } from './utils'
import {
  backgroundEntry,
  preparedViews,
  shouldCopyExtensionAsset,
  webViewEntries,
} from './build-profile'
import { LEGAL_FILES } from './release-artifacts'

async function writeManifest() {
  const output = r(`${BROWSER_DIR}/manifest.json`)
  await mkdir(dirname(output), { recursive: true })
  await writeFile(output, `${JSON.stringify(await getManifest(), null, 2)}\n`)
  log('PRE', `write ${BROWSER_DIR}/manifest.json`)
}

async function writeLocales() {
  await cp(r('src/_locales'), r(`${BROWSER_DIR}/_locales`), {
    recursive: true,
  })
  log('PRE', 'write _locales')
}

/**
 * Stub index.html to use Vite in development
 */
async function stubIndexHtml() {
  for (const view of preparedViews) {
    await mkdir(r(`${BROWSER_DIR}/dist/${view}`), { recursive: true })
    let data = await readFile(r(`src/${view}/index.html`), 'utf-8')

    const sourceEntry =
      view === 'background'
        ? backgroundEntry.replace(/^src\//, '')
        : webViewEntries[view]
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

async function copyStaticFiles() {
  await mkdir(r(BROWSER_DIR), { recursive: true })
  const assetsRoot = r('assets')
  await cp(assetsRoot, r(`${BROWSER_DIR}/assets`), {
    recursive: true,
    filter: (src) => {
      if (src.endsWith('.DS_Store')) return false
      const assetPath = relative(assetsRoot, src).replaceAll('\\', '/')
      return shouldCopyExtensionAsset(assetPath)
    },
  })
  await Promise.all(
    LEGAL_FILES.map((name) => copyFile(r(name), r(`${BROWSER_DIR}/${name}`))),
  )
}

await copyStaticFiles()
await writeManifest()
await writeLocales()

if (isDev) {
  await stubIndexHtml()
  chokidar.watch(r('src/**/*.html')).on('change', () => {
    void stubIndexHtml()
  })
  chokidar
    .watch([
      r('src/manifest.ts'),
      r('scripts/build-profile.ts'),
      r('package.json'),
    ])
    .on('change', () => {
      void writeManifest()
    })
  chokidar.watch(r('src/_locales')).on('change', () => {
    void writeLocales()
  })
}
