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
  it('用构建目录相对路径打安装包，并忽略 .DS_Store', async () => {
    const root = await mkdtemp(resolve(tmpdir(), 'webtag-install-package-'))
    roots.push(root)
    const inputRoot = resolve(root, 'extension-chrome')
    const legalFiles = await authoritativeLegalFiles()
    await mkdir(resolve(inputRoot, 'dist/popup'), { recursive: true })
    await writeFile(resolve(inputRoot, 'manifest.json'), '{}')
    await writeFile(resolve(inputRoot, 'LICENSE'), legalFiles.get('LICENSE')!)
    await writeFile(resolve(inputRoot, 'NOTICE'), legalFiles.get('NOTICE')!)
    await writeFile(resolve(inputRoot, '.DS_Store'), 'ignored')
    await writeFile(resolve(inputRoot, 'dist/.DS_Store'), 'ignored')
    await writeFile(
      resolve(inputRoot, 'dist/popup/index.html'),
      '<!doctype html>',
    )

    const report = await packageInstallArchive('chrome', root)
    const members = await readZipFiles(report.output)

    expect(members).toEqual(
      new Map([
        ['dist/popup/index.html', Buffer.from('<!doctype html>')],
        ['LICENSE', legalFiles.get('LICENSE')!],
        ['manifest.json', Buffer.from('{}')],
        ['NOTICE', legalFiles.get('NOTICE')!],
      ]),
    )
  })

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
