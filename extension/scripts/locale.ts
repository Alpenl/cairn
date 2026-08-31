import { cp } from 'node:fs/promises'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { BROWSER_DIR, log, r } from './utils'

export async function writeLocales() {
  await cp(r('src/_locales'), r(`${BROWSER_DIR}/_locales`), {
    recursive: true,
  })
  log('PRE', 'write _locales')
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  await writeLocales()
}
