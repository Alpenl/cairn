import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import JSZip from 'jszip'

export const EXTENSION_ROOT = resolve(
  dirname(fileURLToPath(import.meta.url)),
  '..',
)
export const LEGAL_FILES = ['LICENSE', 'NOTICE'] as const

export function sha256(contents: Uint8Array): string {
  return createHash('sha256').update(contents).digest('hex')
}

export async function authoritativeLegalFiles(
  authorityRoot = EXTENSION_ROOT,
): Promise<Map<string, Buffer>> {
  return new Map(
    await Promise.all(
      LEGAL_FILES.map(
        async (name) =>
          [name, await readFile(resolve(authorityRoot, name))] as [
            string,
            Buffer,
          ],
      ),
    ),
  )
}

export async function assertLegalFilesInDirectory(
  outputRoot: string,
  authorityRoot = EXTENSION_ROOT,
): Promise<void> {
  const expected = await authoritativeLegalFiles(authorityRoot)

  for (const name of LEGAL_FILES) {
    let actual: Buffer
    try {
      actual = await readFile(resolve(outputRoot, name))
    } catch {
      throw new Error(`Build output is missing root legal file: ${name}`)
    }

    if (!actual.equals(expected.get(name)!)) {
      throw new Error(
        `Build output legal file does not match authoritative extension/${name}`,
      )
    }
  }
}

function assertSafeArchivePath(path: string): void {
  const parts = path.split('/')
  if (
    path.startsWith('/') ||
    path.includes('\\') ||
    parts.includes('') ||
    parts.includes('.') ||
    parts.includes('..')
  ) {
    throw new Error(`Archive contains unsafe member path: ${path}`)
  }
}

export async function readZipFiles(
  archivePath: string,
): Promise<Map<string, Buffer>> {
  const zip = await JSZip.loadAsync(await readFile(archivePath), {
    checkCRC32: true,
  })
  const members = new Map<string, Buffer>()

  for (const [path, member] of Object.entries(zip.files)) {
    if (member.dir) continue
    assertSafeArchivePath(path)
    members.set(path, await member.async('nodebuffer'))
  }

  return new Map(
    [...members].sort(([left], [right]) => left.localeCompare(right)),
  )
}

export async function assertLegalFilesInArchive(
  archivePath: string,
  authorityRoot = EXTENSION_ROOT,
): Promise<void> {
  const [members, expected] = await Promise.all([
    readZipFiles(archivePath),
    authoritativeLegalFiles(authorityRoot),
  ])

  for (const name of LEGAL_FILES) {
    const actual = members.get(name)
    if (!actual) {
      throw new Error(`Archive is missing root legal file: ${name}`)
    }
    if (!actual.equals(expected.get(name)!)) {
      throw new Error(
        `Archive legal file does not match authoritative extension/${name}`,
      )
    }
  }
}

export async function zipMemberHashes(
  archivePath: string,
): Promise<Map<string, string>> {
  return new Map(
    [...(await readZipFiles(archivePath))].map(([path, contents]) => [
      path,
      sha256(contents),
    ]),
  )
}

export async function assertZipMembersEqual(
  expectedArchive: string,
  actualArchive: string,
): Promise<void> {
  const [expected, actual] = await Promise.all([
    zipMemberHashes(expectedArchive),
    zipMemberHashes(actualArchive),
  ])
  const missing = [...expected.keys()].filter((path) => !actual.has(path))
  const extra = [...actual.keys()].filter((path) => !expected.has(path))
  const changed = [...expected].flatMap(([path, digest]) =>
    actual.get(path) !== undefined && actual.get(path) !== digest ? [path] : [],
  )

  if (missing.length || extra.length || changed.length) {
    throw new Error(
      [
        'Rebuilt Firefox ZIP does not match the official artifact',
        missing.length ? `missing: ${missing.join(', ')}` : '',
        extra.length ? `extra: ${extra.join(', ')}` : '',
        changed.length ? `changed: ${changed.join(', ')}` : '',
      ]
        .filter(Boolean)
        .join('; '),
    )
  }
}
