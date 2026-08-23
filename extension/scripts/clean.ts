import { rm } from 'node:fs/promises'
import { log, r } from './utils'

const CLEAN_TARGETS = new Set([
  'extension-chrome',
  'extension-firefox',
  'dist/webtag-chrome.zip',
  'dist/webtag-firefox.zip',
  'dist/webtag-source.zip',
])

const targets = process.argv.slice(2)
if (targets.length === 0) {
  throw new Error('clean requires at least one explicit target')
}

for (const target of targets) {
  const normalizedTarget = target.replaceAll('\\', '/')
  if (!CLEAN_TARGETS.has(normalizedTarget)) {
    throw new Error(`Refusing to clean unknown target: ${target}`)
  }
  await rm(r(normalizedTarget), { recursive: true, force: true })
  log('CLEAN', `removed ${normalizedTarget}`)
}
