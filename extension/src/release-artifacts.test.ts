import { afterEach, describe, expect, it } from 'vitest'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { resolve } from 'node:path'
import JSZip from 'jszip'
import { packageInstallArchive } from '../scripts/package-install'
import {
  assertZipMembersEqual,
  authoritativeLegalFiles,
  EXTENSION_ROOT,
  readZipFiles,
} from '../scripts/release-artifacts'
import { verifyInstallArchive } from '../scripts/verify-install-archives'

const roots: string[] = []

async function writeArchive(entries: Map<string, Buffer | string>) {
  const root = await mkdtemp(resolve(tmpdir(), 'webtag-install-archive-'))
  roots.push(root)
  const path = resolve(root, 'artifact.zip')
  const zip = new JSZip()
  for (const [name, contents] of entries) {
    zip.file(name, contents)
  }
  await writeFile(path, await zip.generateAsync({ type: 'nodebuffer' }))
  return path
}

async function validEntries() {
  return new Map<string, Buffer | string>([
    ...(await authoritativeLegalFiles()),
    ['manifest.json', '{}'],
  ])
}

afterEach(async () => {
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  )
})

describe('verifyInstallArchive', () => {
  it('接受归档根逐字节一致的 LICENSE 和 NOTICE', async () => {
    const archive = await writeArchive(await validEntries())

    await expect(verifyInstallArchive(archive)).resolves.toMatchObject({
      fileCount: 3,
    })
  })

  it('拒绝只在嵌套目录提供 LICENSE 的归档', async () => {
    const entries = await validEntries()
    entries.set('legal/LICENSE', entries.get('LICENSE')!)
    entries.delete('LICENSE')
    const archive = await writeArchive(entries)

    await expect(verifyInstallArchive(archive)).rejects.toThrow(
      'Archive is missing root legal file: LICENSE',
    )
  })

  it('拒绝篡改 NOTICE 的归档', async () => {
    const entries = await validEntries()
    entries.set('NOTICE', 'tampered')
    const archive = await writeArchive(entries)

    await expect(verifyInstallArchive(archive)).rejects.toThrow(
      'Archive legal file does not match authoritative extension/NOTICE',
    )
  })

  it('拒绝用 workspace 根 MIT LICENSE 替代 Extension GPL LICENSE', async () => {
    const entries = await validEntries()
    entries.set(
      'LICENSE',
      await readFile(resolve(EXTENSION_ROOT, '../LICENSE')),
    )
    const archive = await writeArchive(entries)

    await expect(verifyInstallArchive(archive)).rejects.toThrow(
      'Archive legal file does not match authoritative extension/LICENSE',
    )
  })
})

describe('assertZipMembersEqual', () => {
  it('逐成员比较集合与 SHA-256', async () => {
    const official = await writeArchive(new Map([['manifest.json', 'one']]))
    const rebuilt = await writeArchive(new Map([['manifest.json', 'two']]))

    await expect(assertZipMembersEqual(official, rebuilt)).rejects.toThrow(
      'changed: manifest.json',
    )
  })
})

describe('packageInstallArchive', () => {
  it('从浏览器构建目录内容创建根级 ZIP，并忽略 .DS_Store', async () => {
    const root = await mkdtemp(resolve(tmpdir(), 'webtag-package-install-'))
    roots.push(root)
    const input = resolve(root, 'extension-firefox')
    const output = resolve(root, 'artifact.zip')
    await mkdir(resolve(input, 'assets'), { recursive: true })
    await writeFile(resolve(input, 'manifest.json'), '{}')
    await writeFile(resolve(input, 'LICENSE'), 'license')
    await writeFile(resolve(input, '.DS_Store'), 'noise')
    await writeFile(resolve(input, 'assets/icon.png'), 'icon')

    await packageInstallArchive(input, output)

    expect([...(await readZipFiles(output)).keys()]).toEqual([
      'assets/icon.png',
      'LICENSE',
      'manifest.json',
    ])
  })
})
