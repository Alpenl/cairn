// generate stub index.html files for dev entry
import { execSync } from 'node:child_process'
import fs from 'fs-extra'
import chokidar from 'chokidar'
import { relative } from 'node:path'
import { isDev, log, port, r, BROWSER_DIR } from './utils'
import { buildProfile, getPreparedViews } from './build-profile'
import { LEGAL_FILES } from './release-artifacts'

/**
 * Stub index.html to use Vite in development
 */
async function stubIndexHtml() {
  const views = getPreparedViews(buildProfile)

  for (const view of views) {
    await fs.ensureDir(r(`${BROWSER_DIR}/dist/${view}`))
    let data = await fs.readFile(r(`src/${view}/index.html`), 'utf-8')

    const sourceEntry =
      view === 'background'
        ? buildProfile.backgroundEntry.replace(/^src\//, '')
        : buildProfile.entries[view]
    if (!sourceEntry) {
      throw new Error(
        `Missing source entry for ${view} in ${buildProfile.name} profile`,
      )
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
    await fs.writeFile(
      r(`${BROWSER_DIR}/dist/${view}/index.html`),
      data,
      'utf-8',
    )
    log('PRE', `stub ${view}`)
  }
}

function writeManifest() {
  execSync('esno ./scripts/manifest.ts', { stdio: 'inherit' })
}

function writeLocales() {
  execSync('esno ./scripts/locale.ts', { stdio: 'inherit' })
}

fs.ensureDirSync(r(BROWSER_DIR))
const assetsRoot = r('assets')
fs.copySync(r('assets'), r(`${BROWSER_DIR}/assets`), {
  filter: (src) => {
    if (src.endsWith('.DS_Store')) return false
    const assetPath = relative(assetsRoot, src).replaceAll('\\', '/')
    return buildProfile.shouldCopyAsset(assetPath)
  },
})
for (const name of LEGAL_FILES) {
  fs.copyFileSync(r(name), r(`${BROWSER_DIR}/${name}`))
}

writeManifest()
writeLocales()

if (isDev) {
  stubIndexHtml()
  chokidar.watch(r('src/**/*.html')).on('change', () => {
    stubIndexHtml()
  })
  chokidar.watch([r('src/manifest.ts'), r('package.json')]).on('change', () => {
    writeManifest()
  })
  chokidar.watch(r('src/_locales')).on('change', () => {
    writeLocales()
  })
}
