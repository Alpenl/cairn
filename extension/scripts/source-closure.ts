import { execFile } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { promisify } from 'node:util'
import { readZipFiles, sha256 } from './release-artifacts'

const execFileAsync = promisify(execFile)

export const SOURCE_CLOSURE_MANIFEST = 'SOURCE-CLOSURE.json'
const SOURCE_NODE_VERSION = '22.x'
const SOURCE_ALLOWLIST_EXACT = [
  'LICENSE',
  'package.json',
  'pnpm-lock.yaml',
  'pnpm-workspace.yaml',
  'internal/app/assets/openapi.json',
] as const
const SOURCE_ALLOWLIST_PREFIXES = [
  'extension/',
  'packages/webtag-api/',
] as const
const SOURCE_GIT_PATHS = [
  ...SOURCE_ALLOWLIST_EXACT,
  'extension',
  'packages/webtag-api',
] as const

export const SOURCE_REBUILD_COMMANDS = {
  install: ['pnpm', 'install', '--frozen-lockfile'],
  apiCheck: ['pnpm', '--filter', 'webtag-extension', 'api:check'],
  build: ['pnpm', '--filter', 'webtag-extension', 'build:firefox'],
  verify: ['pnpm', '--filter', 'webtag-extension', 'verify:build:firefox'],
  pack: ['pnpm', '--filter', 'webtag-extension', 'pack:firefox'],
} as const

export interface SourceClosureManifest {
  schemaVersion: 1
  artifact: 'webtag-firefox-source'
  toolchain: {
    node: typeof SOURCE_NODE_VERSION
    packageManager: string
  }
  workspace: {
    packages: ['extension', 'packages/webtag-api']
    openapiInput: 'internal/app/assets/openapi.json'
  }
  rebuild: {
    install: string[]
    apiCheck: string[]
    build: string[]
    verify: string[]
    pack: string[]
  }
  inputs: Array<{ path: string; sha256: string }>
}

export function isAllowedSourcePath(path: string): boolean {
  const basename = path.split('/').at(-1) ?? ''
  if (
    basename === '.env' ||
    basename.startsWith('.env.') ||
    basename.endsWith('.local') ||
    /\.(?:key|p12|pem|pfx)$/i.test(basename)
  ) {
    return false
  }
  return (
    SOURCE_ALLOWLIST_EXACT.some((candidate) => candidate === path) ||
    SOURCE_ALLOWLIST_PREFIXES.some((prefix) => path.startsWith(prefix))
  )
}

function assertSourceInputSet(paths: string[]): void {
  const unique = new Set(paths)
  if (unique.size !== paths.length) {
    throw new Error('Source closure contains duplicate input paths')
  }

  const disallowed = paths.filter((path) => !isAllowedSourcePath(path))
  if (disallowed.length) {
    throw new Error(
      `Source closure contains paths outside the tracked allowlist: ${disallowed.join(', ')}`,
    )
  }

  const missingExact = SOURCE_ALLOWLIST_EXACT.filter(
    (path) => !unique.has(path),
  )
  const missingPrefixes = SOURCE_ALLOWLIST_PREFIXES.filter(
    (prefix) => !paths.some((path) => path.startsWith(prefix)),
  )
  if (missingExact.length || missingPrefixes.length) {
    throw new Error(
      [
        missingExact.length
          ? `missing required files: ${missingExact.join(', ')}`
          : '',
        missingPrefixes.length
          ? `missing required trees: ${missingPrefixes.join(', ')}`
          : '',
      ]
        .filter(Boolean)
        .join('; '),
    )
  }
}

export async function collectTrackedSourceInputs(
  repositoryRoot: string,
): Promise<Map<string, Buffer>> {
  const { stdout } = await execFileAsync(
    'git',
    ['ls-files', '-z', '--', ...SOURCE_GIT_PATHS],
    {
      cwd: repositoryRoot,
      encoding: 'utf8',
      maxBuffer: 10 * 1024 * 1024,
    },
  )
  const paths = stdout
    .split('\0')
    .filter(Boolean)
    .sort((left, right) => left.localeCompare(right))
  assertSourceInputSet(paths)

  return new Map(
    await Promise.all(
      paths.map(
        async (path) =>
          [path, await readFile(resolve(repositoryRoot, path))] as [
            string,
            Buffer,
          ],
      ),
    ),
  )
}

function commandCopy(command: readonly string[]): string[] {
  return [...command]
}

export function createSourceClosureManifest(
  inputs: Map<string, Buffer>,
): SourceClosureManifest {
  const paths = [...inputs.keys()].sort((left, right) =>
    left.localeCompare(right),
  )
  assertSourceInputSet(paths)

  const rootPackage = JSON.parse(
    inputs.get('package.json')!.toString('utf8'),
  ) as {
    packageManager?: unknown
  }
  if (
    typeof rootPackage.packageManager !== 'string' ||
    !/^pnpm@\d+\.\d+\.\d+$/.test(rootPackage.packageManager)
  ) {
    throw new Error('Root package.json must pin an exact pnpm packageManager')
  }

  return {
    schemaVersion: 1,
    artifact: 'webtag-firefox-source',
    toolchain: {
      node: SOURCE_NODE_VERSION,
      packageManager: rootPackage.packageManager,
    },
    workspace: {
      packages: ['extension', 'packages/webtag-api'],
      openapiInput: 'internal/app/assets/openapi.json',
    },
    rebuild: {
      install: commandCopy(SOURCE_REBUILD_COMMANDS.install),
      apiCheck: commandCopy(SOURCE_REBUILD_COMMANDS.apiCheck),
      build: commandCopy(SOURCE_REBUILD_COMMANDS.build),
      verify: commandCopy(SOURCE_REBUILD_COMMANDS.verify),
      pack: commandCopy(SOURCE_REBUILD_COMMANDS.pack),
    },
    inputs: paths.map((path) => ({
      path,
      sha256: sha256(inputs.get(path)!),
    })),
  }
}

export function assertSourceClosureMatchesInputs(
  manifest: SourceClosureManifest,
  inputs: Map<string, Buffer>,
): void {
  const expected = createSourceClosureManifest(inputs)
  if (JSON.stringify(manifest.inputs) !== JSON.stringify(expected.inputs)) {
    throw new Error(
      'Source archive closure does not match the current tracked inputs',
    )
  }
}

function commandsMatch(
  actual: Record<string, unknown>,
  expected: typeof SOURCE_REBUILD_COMMANDS,
): boolean {
  return Object.entries(expected).every(
    ([name, command]) =>
      Array.isArray(actual[name]) &&
      JSON.stringify(actual[name]) === JSON.stringify(command),
  )
}

function parseSourceClosureManifest(
  contents: Buffer,
): SourceClosureManifest {
  const value = JSON.parse(
    contents.toString('utf8'),
  ) as Partial<SourceClosureManifest>
  if (
    value.schemaVersion !== 1 ||
    value.artifact !== 'webtag-firefox-source' ||
    value.toolchain?.node !== SOURCE_NODE_VERSION ||
    typeof value.toolchain.packageManager !== 'string' ||
    !/^pnpm@\d+\.\d+\.\d+$/.test(value.toolchain.packageManager) ||
    value.workspace?.openapiInput !== 'internal/app/assets/openapi.json' ||
    JSON.stringify(value.workspace.packages) !==
      JSON.stringify(['extension', 'packages/webtag-api']) ||
    !value.rebuild ||
    !commandsMatch(
      value.rebuild as unknown as Record<string, unknown>,
      SOURCE_REBUILD_COMMANDS,
    ) ||
    !Array.isArray(value.inputs)
  ) {
    throw new Error('Source closure manifest metadata is invalid')
  }

  for (const input of value.inputs) {
    if (
      typeof input?.path !== 'string' ||
      typeof input.sha256 !== 'string' ||
      !/^[a-f0-9]{64}$/.test(input.sha256)
    ) {
      throw new Error('Source closure manifest contains an invalid input')
    }
  }

  const paths = value.inputs.map((input) => input.path)
  assertSourceInputSet(paths)
  const sorted = [...paths].sort((left, right) => left.localeCompare(right))
  if (JSON.stringify(paths) !== JSON.stringify(sorted)) {
    throw new Error('Source closure manifest inputs must be sorted')
  }

  return value as SourceClosureManifest
}

export interface VerifiedSourceClosure {
  manifest: SourceClosureManifest
  members: Map<string, Buffer>
}

export async function verifySourceArchiveContents(
  archivePath: string,
): Promise<VerifiedSourceClosure> {
  const members = await readZipFiles(archivePath)
  const manifestContents = members.get(SOURCE_CLOSURE_MANIFEST)
  if (!manifestContents) {
    throw new Error(`Source archive is missing ${SOURCE_CLOSURE_MANIFEST}`)
  }
  const manifest = parseSourceClosureManifest(manifestContents)
  const expectedPaths = manifest.inputs.map((input) => input.path)
  const actualPaths = [...members.keys()].filter(
    (path) => path !== SOURCE_CLOSURE_MANIFEST,
  )
  const missing = expectedPaths.filter((path) => !members.has(path))
  const extra = actualPaths.filter((path) => !expectedPaths.includes(path))
  if (missing.length || extra.length) {
    throw new Error(
      [
        'Source archive members do not match its closure manifest',
        missing.length ? `missing: ${missing.join(', ')}` : '',
        extra.length ? `extra: ${extra.join(', ')}` : '',
      ]
        .filter(Boolean)
        .join('; '),
    )
  }

  const changed = manifest.inputs
    .filter((input) => sha256(members.get(input.path)!) !== input.sha256)
    .map((input) => input.path)
  if (changed.length) {
    throw new Error(
      `Source archive members do not match closure SHA-256: ${changed.join(', ')}`,
    )
  }

  const rootPackage = JSON.parse(
    members.get('package.json')!.toString('utf8'),
  ) as {
    packageManager?: unknown
  }
  if (rootPackage.packageManager !== manifest.toolchain.packageManager) {
    throw new Error(
      'Source closure packageManager does not match root package.json',
    )
  }

  return { manifest, members }
}
