import { rm } from 'node:fs/promises'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { r } from './utils'

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''

if (entryPath === fileURLToPath(import.meta.url)) {
  const targets = process.argv.slice(2)
  if (targets.length === 0) {
    throw new Error('clean requires at least one target')
  }
  await Promise.all(
    targets.map((target) => rm(r(target), { recursive: true, force: true })),
  )
}
