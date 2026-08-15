import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import JSZip from 'jszip'
import {
  collectTrackedSourceInputs,
  createSourceClosureManifest,
  SOURCE_CLOSURE_MANIFEST,
} from './source-closure'

const extensionRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const repositoryRoot = resolve(extensionRoot, '..')
const output = resolve(extensionRoot, 'dist/webtag-source.zip')
const archiveDate = new Date(1980, 0, 1, 0, 0, 0)

export async function packageSourceArchive(
  repository = repositoryRoot,
  outputPath = output,
) {
  const inputs = await collectTrackedSourceInputs(repository)
  const manifest = createSourceClosureManifest(inputs)
  const zip = new JSZip()

  for (const [path, contents] of inputs) {
    zip.file(path, contents, {
      createFolders: false,
      date: archiveDate,
      unixPermissions: 0o100644,
    })
  }
  zip.file(SOURCE_CLOSURE_MANIFEST, `${JSON.stringify(manifest, null, 2)}\n`, {
    createFolders: false,
    date: archiveDate,
    unixPermissions: 0o100644,
  })

  const contents = await zip.generateAsync({
    type: 'nodebuffer',
    compression: 'DEFLATE',
    compressionOptions: { level: 9 },
    platform: 'UNIX',
  })
  await mkdir(dirname(outputPath), { recursive: true })
  await writeFile(outputPath, contents)
  return { output: outputPath, bytes: contents.byteLength, inputs: inputs.size }
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  console.log(JSON.stringify(await packageSourceArchive(), null, 2))
}
