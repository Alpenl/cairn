import { readFile, readdir, stat } from 'node:fs/promises'
import { resolve, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { BUILD_TARGET } from './build-target'
import { assertLegalFilesInDirectory } from './release-artifacts'

interface ManifestLike {
  default_locale?: string
  action?: { default_icon?: string; default_popup?: string }
  icons?: Record<string, string>
  options_ui?: { page?: string }
  background?: { service_worker?: string; scripts?: string[] }
  chrome_url_overrides?: { newtab?: string }
  content_scripts?: Array<{ js?: string[]; css?: string[] }>
}

export interface BuildOutputReport {
  target: string
  totalBytes: number
  fileCount: number
  largestOwnChunk: { path: string; bytes: number } | null
}

const CAPTURE_FORBIDDEN_MARKERS = [
  'generalSetting.syncRateWarning',
  'focusVisibleWidgetMap',
  'keyboardBookmark',
  'NaiveTab Options',
]

async function listFiles(root: string, directory = root): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(
    entries.map(async (entry) => {
      const path = resolve(directory, entry.name)
      return entry.isDirectory()
        ? listFiles(root, path)
        : [relative(root, path)]
    }),
  )
  return nested.flat().sort()
}

function localManifestPaths(manifest: ManifestLike): string[] {
  const paths = [
    manifest.action?.default_icon,
    manifest.action?.default_popup,
    ...Object.values(manifest.icons ?? {}),
    manifest.options_ui?.page,
    manifest.background?.service_worker,
    ...(manifest.background?.scripts ?? []),
    manifest.chrome_url_overrides?.newtab,
    ...(manifest.content_scripts ?? []).flatMap((entry) => [
      ...(entry.js ?? []),
      ...(entry.css ?? []),
    ]),
  ]

  if (manifest.default_locale) {
    paths.push(`_locales/${manifest.default_locale}/messages.json`)
  }

  return paths
    .filter((path): path is string => Boolean(path))
    .map((path) => path.replace(/^\/+/, ''))
}

async function assertManifestReferences(
  root: string,
  files: Set<string>,
  manifest: ManifestLike,
) {
  const missing = localManifestPaths(manifest).filter(
    (path) => !files.has(path),
  )
  if (missing.length > 0) {
    throw new Error(`Manifest references missing files: ${missing.join(', ')}`)
  }

  const manifestPath = resolve(root, 'manifest.json')
  if (!(await stat(manifestPath)).isFile()) {
    throw new Error('manifest.json is not a file')
  }
}

async function assertCaptureReachability(
  root: string,
  files: string[],
  manifest: ManifestLike,
) {
  const forbiddenFiles = [
    'dist/newtab/index.html',
    'dist/contentScripts/index.global.js',
  ].filter((path) => files.includes(path))
  if (forbiddenFiles.length > 0) {
    throw new Error(
      `Capture output includes legacy entries: ${forbiddenFiles.join(', ')}`,
    )
  }

  const contentScriptFiles = (manifest.content_scripts ?? []).flatMap(
    (entry) => entry.js ?? [],
  )
  const normalizedContentScriptFiles = contentScriptFiles.map((path) =>
    path.replace(/^\/+/, ''),
  )
  if (
    manifest.chrome_url_overrides?.newtab ||
    normalizedContentScriptFiles.length !== 1 ||
    normalizedContentScriptFiles[0] !== 'dist/contentScripts/rss.global.js'
  ) {
    throw new Error(
      'Capture manifest must register only the RSS discovery content script',
    )
  }

  for (const view of ['popup', 'options']) {
    const htmlPath = `dist/${view}/index.html`
    const html = await readFile(resolve(root, htmlPath), 'utf8')
    const entryPath = html
      .match(/<script[^>]+type="module"[^>]+src="\/?([^"]+)"/)?.[1]
      ?.replace(/^\/+/, '')
    if (!entryPath || !files.includes(entryPath)) {
      throw new Error(
        `Capture ${view} HTML does not reference an existing module entry`,
      )
    }
    if (!entryPath.includes(`${view}.capture-`)) {
      throw new Error(
        `Capture ${view} HTML points to a non-capture entry: ${entryPath}`,
      )
    }
  }

  const unexpectedAssets = files.filter(
    (path) =>
      path.startsWith('assets/') && !path.startsWith('assets/img/icon/'),
  )
  if (unexpectedAssets.length > 0) {
    throw new Error(
      `Capture output includes legacy assets: ${unexpectedAssets.join(', ')}`,
    )
  }

  const textFiles = files.filter((path) => /\.(?:css|html|js|mjs)$/.test(path))
  for (const path of textFiles) {
    const contents = await readFile(resolve(root, path), 'utf8')
    const marker = CAPTURE_FORBIDDEN_MARKERS.find((value) =>
      contents.includes(value),
    )
    if (marker) {
      throw new Error(
        `Capture output ${path} contains legacy marker: ${marker}`,
      )
    }
  }
}

export async function verifyBuildOutput(
  root: string,
): Promise<BuildOutputReport> {
  await assertLegalFilesInDirectory(root)
  const files = await listFiles(root)
  const fileSet = new Set(files)
  if (!fileSet.has('manifest.json')) {
    throw new Error('Build output is missing manifest.json')
  }

  const manifest = JSON.parse(
    await readFile(resolve(root, 'manifest.json'), 'utf8'),
  ) as ManifestLike
  await assertManifestReferences(root, fileSet, manifest)

  await assertCaptureReachability(root, files, manifest)

  const sizes = await Promise.all(
    files.map(async (path) => ({
      path,
      bytes: (await stat(resolve(root, path))).size,
    })),
  )
  const ownChunks = sizes
    .filter(
      ({ path }) =>
        /\.(?:js|mjs)$/.test(path) &&
        !path.split('/').at(-1)?.startsWith('vendor-'),
    )
    .sort((a, b) => b.bytes - a.bytes)

  return {
    target: BUILD_TARGET,
    totalBytes: sizes.reduce((total, file) => total + file.bytes, 0),
    fileCount: files.length,
    largestOwnChunk: ownChunks[0] ?? null,
  }
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  const root = resolve(process.argv[2] ?? 'extension-chrome')
  console.log(JSON.stringify(await verifyBuildOutput(root), null, 2))
}
