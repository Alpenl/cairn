import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { bgCyan, black } from 'kolorist'

export const port = parseInt(process.env.PORT || '', 10) || 3303
export const isDev = process.env.NODE_ENV !== 'production'

// 根据 BROWSER 环境变量决定输出目录，避免 Chrome/Firefox 构建互相覆盖
const browserMap: Record<string, string> = {
  chrome: 'extension-chrome',
  firefox: 'extension-firefox',
}
export const BROWSER_DIR = browserMap[process.env.BROWSER || 'chrome']

// temp  __dirname is not defined in ES module scope
const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
export const r = (...args: string[]) => resolve(__dirname, '..', ...args)

export function log(name: string, message: string) {
  console.log(black(bgCyan(` ${name} `)), message)
}
