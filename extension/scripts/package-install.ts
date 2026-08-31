import { mkdir, readFile, readdir, stat, writeFile } from 'node:fs/promises'
import { dirname, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import JSZip from 'jszip'
import { EXTENSION_ROOT } from './release-artifacts'

const archiveDate = new Date(1980, 0, 1, 0, 0, 0)

async function listFiles(root: string, directory = root): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(
    entries.map(async (entry) => {
      if (entry.name === '.DS_Store') return []
      const path = resolve(directory, entry.name)
      return entry.isDirectory()
        ? listFiles(root, path)
        : [relative(root, path).replaceAll('\\', '/')]
    }),
  )
  return nested.flat().sort()
}

export async function packageInstallArchive(
  inputDirectory: string,
  outputPath: string,
) {
  const root = resolve(EXTENSION_ROOT, inputDirectory)
  const zip = new JSZip()

  for (const path of await listFiles(root)) {
    const absolutePath = resolve(root, path)
    if (!(await stat(absolutePath)).isFile()) continue
    zip.file(path, await readFile(absolutePath), {
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
  await mkdir(dirname(resolve(EXTENSION_ROOT, outputPath)), { recursive: true })
  await writeFile(resolve(EXTENSION_ROOT, outputPath), contents)
  return { output: outputPath, bytes: contents.byteLength }
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  const [inputDirectory, outputPath] = process.argv.slice(2)
  if (!inputDirectory || !outputPath) {
    throw new Error('package-install requires input directory and output path')
  }
  console.log(
    JSON.stringify(
      await packageInstallArchive(inputDirectory, outputPath),
      null,
      2,
    ),
  )
}
