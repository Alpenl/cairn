import { spawn } from 'node:child_process'
import {
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  realpath,
  rm,
  writeFile,
} from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, relative, resolve } from 'node:path'
import { performance } from 'node:perf_hooks'
import { fileURLToPath } from 'node:url'
import {
  assertLegalFilesInArchive,
  assertZipMembersEqual,
  EXTENSION_ROOT,
  sha256,
  zipMemberHashes,
} from './release-artifacts'
import {
  assertSourceClosureMatchesInputs,
  collectTrackedSourceInputs,
  SOURCE_REBUILD_COMMANDS,
  verifySourceArchiveContents,
} from './source-closure'

export interface CapturedCommandResult {
  exitCode: number | null
  signal: NodeJS.Signals | null
  stdout: string
  stderr: string
}

export interface SourceRebuildSandbox {
  root: string
  rebuildRoot: string
  storeRoot: string
  offlineProbeRoot: string
  offlineProbeStoreRoot: string
}

export function isolatedPnpmEnvironment(
  storeRoot: string,
  baseEnvironment: NodeJS.ProcessEnv = process.env,
): NodeJS.ProcessEnv {
  return {
    ...baseEnvironment,
    CI: 'true',
    npm_config_store_dir: storeRoot,
    NPM_CONFIG_STORE_DIR: storeRoot,
    PNPM_STORE_DIR: storeRoot,
    NODE_PATH: '',
  }
}

export function isolatedInstallCommand(
  storeRoot: string,
  offline = false,
): string[] {
  return [
    ...SOURCE_REBUILD_COMMANDS.install,
    '--store-dir',
    storeRoot,
    ...(offline ? ['--offline'] : []),
  ]
}

async function assertDirectoryEmpty(path: string): Promise<void> {
  const entries = await readdir(path)
  if (entries.length) {
    throw new Error(`Isolated pnpm store is not empty: ${path}`)
  }
}

export async function createSourceRebuildSandbox(
  temporaryRoot = tmpdir(),
): Promise<SourceRebuildSandbox> {
  const root = await mkdtemp(resolve(temporaryRoot, 'webtag-source-rebuild-'))
  const sandbox = {
    root,
    rebuildRoot: resolve(root, 'workspace'),
    storeRoot: resolve(root, 'pnpm-store'),
    offlineProbeRoot: resolve(root, 'offline-probe-workspace'),
    offlineProbeStoreRoot: resolve(root, 'offline-probe-store'),
  }
  try {
    await Promise.all(
      Object.values(sandbox)
        .filter((path) => path !== root)
        .map((path) => mkdir(path, { recursive: true })),
    )
    await Promise.all([
      assertDirectoryEmpty(sandbox.storeRoot),
      assertDirectoryEmpty(sandbox.offlineProbeStoreRoot),
    ])
    return sandbox
  } catch (error) {
    await rm(root, { recursive: true, force: true })
    throw error
  }
}

export async function removeSourceRebuildSandbox(
  sandbox: SourceRebuildSandbox,
): Promise<void> {
  await rm(sandbox.root, { recursive: true, force: true })
}

export function assertOfflineProbeFailed(result: CapturedCommandResult): void {
  if (result.exitCode === 0) {
    throw new Error(
      'Empty-store offline install unexpectedly succeeded; a prewarmed store was visible',
    )
  }

  const output = `${result.stdout}\n${result.stderr}`
  if (!/ERR_PNPM_NO_OFFLINE_(?:META|TARBALL)/.test(output)) {
    throw new Error(
      `Empty-store offline probe failed for an unexpected reason: ${output.trim()}`,
    )
  }
}

function runCommand(
  command: readonly string[],
  cwd: string,
  environment: NodeJS.ProcessEnv = { ...process.env, CI: 'true' },
): Promise<void> {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command[0]!, command.slice(1), {
      cwd,
      env: { ...environment, PWD: cwd, INIT_CWD: cwd },
      stdio: 'inherit',
    })
    child.on('error', reject)
    child.on('exit', (code, signal) => {
      if (code === 0) {
        resolvePromise()
      } else {
        reject(
          new Error(
            `${command.join(' ')} failed with ${signal ? `signal ${signal}` : `exit ${code}`}`,
          ),
        )
      }
    })
  })
}

export function captureCommandResult(
  command: readonly string[],
  cwd: string,
  environment: NodeJS.ProcessEnv = { ...process.env, CI: 'true' },
): Promise<CapturedCommandResult> {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command[0]!, command.slice(1), {
      cwd,
      env: { ...environment, PWD: cwd, INIT_CWD: cwd },
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    let stdout = ''
    let stderr = ''
    child.stdout.setEncoding('utf8')
    child.stderr.setEncoding('utf8')
    child.stdout.on('data', (chunk: string) => {
      stdout += chunk
    })
    child.stderr.on('data', (chunk: string) => {
      stderr += chunk
    })
    child.on('error', reject)
    child.on('close', (code, signal) => {
      resolvePromise({ exitCode: code, signal, stdout, stderr })
    })
  })
}

async function captureCommand(
  command: readonly string[],
  cwd: string,
): Promise<string> {
  const result = await captureCommandResult(command, cwd)
  if (result.exitCode !== 0) {
    throw new Error(
      `${command.join(' ')} failed with ${result.signal ? `signal ${result.signal}` : `exit ${result.exitCode}`}: ${result.stderr.trim()}`,
    )
  }
  return result.stdout.trim()
}

async function extractSourceArchive(
  root: string,
  members: Map<string, Buffer>,
): Promise<void> {
  for (const [path, contents] of members) {
    const target = resolve(root, path)
    const targetRelative = relative(root, target)
    if (targetRelative.startsWith('..') || targetRelative === '') {
      throw new Error(`Refusing to extract unsafe source member: ${path}`)
    }
    await mkdir(dirname(target), { recursive: true })
    await writeFile(target, contents)
  }
}

async function assertArchivedWorkspaceLink(root: string): Promise<void> {
  const [actual, expected] = await Promise.all([
    realpath(resolve(root, 'extension/node_modules/@webtag/api')),
    realpath(resolve(root, 'packages/webtag-api')),
  ])
  if (actual !== expected) {
    throw new Error(
      `@webtag/api resolved outside the archived workspace: ${actual}`,
    )
  }
}

async function verifySourceArchive(
  sourceArchive = resolve(EXTENSION_ROOT, 'dist/webtag-source.zip'),
  officialFirefoxArchive = resolve(EXTENSION_ROOT, 'dist/webtag-firefox.zip'),
) {
  const verificationStarted = performance.now()
  const verified = await verifySourceArchiveContents(sourceArchive)
  const trackedInputs = await collectTrackedSourceInputs(
    resolve(EXTENSION_ROOT, '..'),
  )
  assertSourceClosureMatchesInputs(verified.manifest, trackedInputs)
  await assertLegalFilesInArchive(officialFirefoxArchive)

  const currentNodeMajor = process.versions.node.split('.')[0]
  if (`${currentNodeMajor}.x` !== verified.manifest.toolchain.node) {
    throw new Error(
      `Source rebuild requires Node ${verified.manifest.toolchain.node}, got ${process.versions.node}`,
    )
  }

  const packageManagerVersion = verified.manifest.toolchain.packageManager
    .split('@')
    .at(-1)!
  const actualPnpmVersion = await captureCommand(
    ['pnpm', '--version'],
    process.cwd(),
  )
  if (actualPnpmVersion !== packageManagerVersion) {
    throw new Error(
      `Source rebuild requires pnpm ${packageManagerVersion}, got ${actualPnpmVersion}`,
    )
  }

  const sandbox = await createSourceRebuildSandbox()
  const rebuildStarted = performance.now()
  let report:
    | {
        sourceInputs: number
        firefoxMembers: number
        storeMode: 'isolated-empty'
        offlineProbe: 'cache-miss'
        installSeconds: number
        rebuildSeconds: number
      }
    | undefined
  try {
    await Promise.all([
      extractSourceArchive(sandbox.rebuildRoot, verified.members),
      extractSourceArchive(sandbox.offlineProbeRoot, verified.members),
    ])

    const offlineProbeResult = await captureCommandResult(
      isolatedInstallCommand(sandbox.offlineProbeStoreRoot, true),
      sandbox.offlineProbeRoot,
      isolatedPnpmEnvironment(sandbox.offlineProbeStoreRoot),
    )
    assertOfflineProbeFailed(offlineProbeResult)

    await assertDirectoryEmpty(sandbox.storeRoot)
    const lockfilePath = resolve(sandbox.rebuildRoot, 'pnpm-lock.yaml')
    const lockfileDigest = sha256(await readFile(lockfilePath))

    const installStarted = performance.now()
    await runCommand(
      isolatedInstallCommand(sandbox.storeRoot),
      sandbox.rebuildRoot,
      isolatedPnpmEnvironment(sandbox.storeRoot),
    )
    const installSeconds = (performance.now() - installStarted) / 1000
    if (sha256(await readFile(lockfilePath)) !== lockfileDigest) {
      throw new Error('Frozen install changed pnpm-lock.yaml')
    }
    await assertArchivedWorkspaceLink(sandbox.rebuildRoot)

    for (const command of [
      SOURCE_REBUILD_COMMANDS.apiCheck,
      SOURCE_REBUILD_COMMANDS.build,
      SOURCE_REBUILD_COMMANDS.verify,
      SOURCE_REBUILD_COMMANDS.pack,
    ]) {
      await runCommand(
        command,
        sandbox.rebuildRoot,
        isolatedPnpmEnvironment(sandbox.storeRoot),
      )
    }

    const rebuiltArchive = resolve(
      sandbox.rebuildRoot,
      'extension/dist/webtag-firefox.zip',
    )
    await assertZipMembersEqual(officialFirefoxArchive, rebuiltArchive)
    report = {
      sourceInputs: verified.manifest.inputs.length,
      firefoxMembers: (await zipMemberHashes(rebuiltArchive)).size,
      storeMode: 'isolated-empty',
      offlineProbe: 'cache-miss',
      installSeconds: Number(installSeconds.toFixed(3)),
      rebuildSeconds: Number(
        ((performance.now() - rebuildStarted) / 1000).toFixed(3),
      ),
    }
  } finally {
    await removeSourceRebuildSandbox(sandbox)
  }

  return {
    ...report!,
    temporaryStoresRemoved: true,
    verificationSeconds: Number(
      ((performance.now() - verificationStarted) / 1000).toFixed(3),
    ),
  }
}

const entryPath = process.argv[1] ? resolve(process.argv[1]) : ''
if (entryPath === fileURLToPath(import.meta.url)) {
  const [sourceArchive, officialFirefoxArchive] = process.argv.slice(2)
  console.log(
    JSON.stringify(
      await verifySourceArchive(sourceArchive, officialFirefoxArchive),
      null,
      2,
    ),
  )
}
