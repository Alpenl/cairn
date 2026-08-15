import fs from 'fs-extra'
import { BROWSER_DIR, log, r } from './utils'

export async function writeLocales() {
  await fs.copy(r('src/_locales'), r(`${BROWSER_DIR}/_locales`))
  log('PRE', 'write _locales')
}

writeLocales()
