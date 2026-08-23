import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises'
import { dirname, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import JSZip from 'jszip'
import { EXTENSION_ROOT } from './release-artifacts'

export type BrowserArchiveTarget = 'chrome' | 'firefox'

const INSTALL_ARCHIVES: Record<
  BrowserArchiveTarget,
  { inputDirectory: string; outputArchive: string }
> = {
  chrome: {
    inputDirectory: 'extension-chrome',
    outputArchive: 'dist/webtag-chrome.zip',
  },
  firefox: {
    inputDirectory: 'extension-firefox',
    outputArchive: 'dist/webtag-firefox.zip',
  },
}

const archiveDate = new Date(1980, 0, 1, 0, 0, 0)

async function listArchiveFiles(
  root: string,
  directory = root,
): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(
    entries.map(async (entry) => {
      if (entry.name === '.DS_Store') return []
      const path = resolve(directory, entry.name)
      return entry.isDirectory()
        ? listArchiveFiles(root, path)
        : [relative(root, path).replaceAll('\\', '/')]
    }),
  )
  return nested.flat().sort((left, right) => left.localeCompare(right))
}

export async function packageInstallArchive(
  target: BrowserArchiveTarget,
  extensionRoot = EXTENSION_ROOT,
) {
  const archive = INSTALL_ARCHIVES[target]
  const inputRoot = resolve(extensionRoot, archive.inputDirectory)
  const outputPath = resolve(extensionRoot, archive.outputArchive)
  const files = await listArchiveFiles(inputRoot)
  const zip = new JSZip()

  for (const path of files) {
    zip.file(path, await readFile(resolve(inputRoot, path)), {
      createFolders: false,
      date: archiveDate,
      unixPermissions: 0o100644,
    })
  }

  const contents = await zip.generateAsync({
    type: 'nodebuffer',
    compression: 'DEFLATE',
    compressionOptions: { level: 9 },
    platform: 'UNIX',
  })
  await mkdir(dirname(outputPath), { recursive: true })
  await writeFile(outputPath, contents)
  return {
    target,
    inputRoot,
    output: outputPath,
    bytes: contents.byteLength,
    fileCount: files.length,
  }
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  const target = process.argv[2]
  if (target !== 'chrome' && target !== 'firefox') {
    throw new Error('Usage: esno scripts/package-install.ts <chrome|firefox>')
  }
  console.log(JSON.stringify(await packageInstallArchive(target), null, 2))
}
