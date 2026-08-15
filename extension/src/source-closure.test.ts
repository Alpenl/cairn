import { afterEach, describe, expect, it } from 'vitest'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { resolve } from 'node:path'
import JSZip from 'jszip'
import {
  assertSourceClosureMatchesInputs,
  createSourceClosureManifest,
  isAllowedSourcePath,
  SOURCE_CLOSURE_MANIFEST,
  verifySourceArchiveContents,
} from '../scripts/source-closure'

const roots: string[] = []

function minimalSourceInputs() {
  return new Map<string, Buffer>([
    ['LICENSE', Buffer.from('root license')],
    [
      'package.json',
      Buffer.from(JSON.stringify({ packageManager: 'pnpm@10.13.1' })),
    ],
    ['pnpm-lock.yaml', Buffer.from('lockfileVersion: 9')],
    [
      'pnpm-workspace.yaml',
      Buffer.from('packages:\n  - extension\n  - packages/*\n'),
    ],
    ['internal/app/assets/openapi.json', Buffer.from('{}')],
    ['extension/package.json', Buffer.from('{}')],
    ['packages/webtag-api/package.json', Buffer.from('{}')],
  ])
}

async function writeSourceArchive(
  inputs: Map<string, Buffer>,
  mutate?: (zip: JSZip) => void,
) {
  const root = await mkdtemp(resolve(tmpdir(), 'webtag-source-closure-'))
  roots.push(root)
  const path = resolve(root, 'source.zip')
  const zip = new JSZip()
  for (const [name, contents] of inputs) {
    zip.file(name, contents)
  }
  zip.file(
    SOURCE_CLOSURE_MANIFEST,
    JSON.stringify(createSourceClosureManifest(inputs)),
  )
  mutate?.(zip)
  await writeFile(path, await zip.generateAsync({ type: 'nodebuffer' }))
  return path
}

afterEach(async () => {
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  )
})

describe('source closure allowlist', () => {
  it('只接受声明的 workspace 元数据、Extension、共享 API 和 OpenAPI 输入', () => {
    expect(isAllowedSourcePath('package.json')).toBe(true)
    expect(isAllowedSourcePath('extension/src/manifest.ts')).toBe(true)
    expect(isAllowedSourcePath('packages/webtag-api/src/index.ts')).toBe(true)
    expect(isAllowedSourcePath('internal/app/assets/openapi.json')).toBe(true)
    expect(isAllowedSourcePath('extension/.env.local')).toBe(false)
    expect(isAllowedSourcePath('extension/signing.pem')).toBe(false)
    expect(isAllowedSourcePath('.env')).toBe(false)
    expect(isAllowedSourcePath('reader/package.json')).toBe(false)
  })

  it('拒绝与当前 tracked 输入集合或 digest 不一致的 closure', () => {
    const inputs = minimalSourceInputs()
    const manifest = createSourceClosureManifest(inputs)
    inputs.set('extension/package.json', Buffer.from('{"changed":true}'))

    expect(() => assertSourceClosureMatchesInputs(manifest, inputs)).toThrow(
      'does not match the current tracked inputs',
    )
  })
})

describe('verifySourceArchiveContents', () => {
  it('接受成员集合与逐文件 SHA-256 完整的 closure', async () => {
    const archive = await writeSourceArchive(minimalSourceInputs())

    await expect(verifySourceArchiveContents(archive)).resolves.toMatchObject({
      manifest: { artifact: 'webtag-firefox-source' },
    })
  })

  it('拒绝 closure 声明但归档缺失的输入', async () => {
    const inputs = minimalSourceInputs()
    const archive = await writeSourceArchive(inputs, (zip) => {
      zip.remove('extension/package.json')
    })

    await expect(verifySourceArchiveContents(archive)).rejects.toThrow(
      'missing: extension/package.json',
    )
  })

  it('拒绝不在 tracked closure 中的额外成员', async () => {
    const archive = await writeSourceArchive(minimalSourceInputs(), (zip) => {
      zip.file('.env', 'secret')
    })

    await expect(verifySourceArchiveContents(archive)).rejects.toThrow(
      'extra: .env',
    )
  })

  it('拒绝内容与 closure SHA-256 不一致的成员', async () => {
    const archive = await writeSourceArchive(minimalSourceInputs(), (zip) => {
      zip.file('extension/package.json', '{"tampered":true}')
    })

    await expect(verifySourceArchiveContents(archive)).rejects.toThrow(
      'do not match closure SHA-256: extension/package.json',
    )
  })
})
