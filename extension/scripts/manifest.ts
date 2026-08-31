import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { getManifest } from '../src/manifest'
import { BROWSER_DIR, log, r } from './utils'

export async function writeManifest() {
  const output = r(`${BROWSER_DIR}/manifest.json`)
  await mkdir(dirname(output), { recursive: true })
  await writeFile(output, `${JSON.stringify(await getManifest(), null, 2)}\n`)
  log('PRE', `write ${BROWSER_DIR}/manifest.json`)
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  await writeManifest()
}
