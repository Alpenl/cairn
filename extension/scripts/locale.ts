import { cp } from 'node:fs/promises'
import { BROWSER_DIR, log, r } from './utils'

export async function writeLocales() {
  await cp(r('src/_locales'), r(`${BROWSER_DIR}/_locales`), {
    recursive: true,
  })
  log('PRE', 'write _locales')
}

writeLocales()
