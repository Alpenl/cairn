import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  assertLegalFilesInArchive,
  EXTENSION_ROOT,
  zipMemberHashes,
} from './release-artifacts'

const DEFAULT_ARCHIVES = [
  resolve(EXTENSION_ROOT, 'dist/webtag-chrome.zip'),
  resolve(EXTENSION_ROOT, 'dist/webtag-firefox.zip'),
]

export async function verifyInstallArchive(
  archivePath: string,
  authorityRoot = EXTENSION_ROOT,
) {
  await assertLegalFilesInArchive(archivePath, authorityRoot)
  const members = await zipMemberHashes(archivePath)
  return { archive: archivePath, fileCount: members.size }
}

export async function verifyInstallArchives(archivePaths = DEFAULT_ARCHIVES) {
  return Promise.all(archivePaths.map((path) => verifyInstallArchive(path)))
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  const paths = process.argv.slice(2)
  console.log(
    JSON.stringify(
      await verifyInstallArchives(paths.length ? paths : undefined),
      null,
      2,
    ),
  )
}
