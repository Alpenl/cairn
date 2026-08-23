import { writeFile } from 'node:fs/promises'
import { getManifest } from '../src/manifest'
import { BROWSER_DIR, log, r } from './utils'

export async function writeManifest() {
  await writeFile(
    r(`${BROWSER_DIR}/manifest.json`),
    `${JSON.stringify(await getManifest(), null, 2)}\n`,
  )
  log('PRE', `write ${BROWSER_DIR}/manifest.json`)
}

writeManifest()
