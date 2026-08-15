import { afterEach, describe, expect, it } from 'vitest'
import {
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  stat,
  writeFile,
} from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { resolve } from 'node:path'
import {
  assertOfflineProbeFailed,
  captureCommandResult,
  createSourceRebuildSandbox,
  isolatedInstallCommand,
  isolatedPnpmEnvironment,
  removeSourceRebuildSandbox,
} from '../scripts/verify-source-archive'

const roots: string[] = []

async function temporaryRoot() {
  const root = await mkdtemp(resolve(tmpdir(), 'webtag-store-test-'))
  roots.push(root)
  return root
}

afterEach(async () => {
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  )
})

describe('source rebuild pnpm store isolation', () => {
  it('覆盖父进程的预热 store 配置且从空目录开始', async () => {
    const root = await temporaryRoot()
    const prewarmedStore = resolve(root, 'prewarmed-store')
    await mkdir(prewarmedStore)
    await writeFile(resolve(prewarmedStore, 'sentinel'), 'host cache')
    const sandbox = await createSourceRebuildSandbox(root)

    const environment = isolatedPnpmEnvironment(sandbox.storeRoot, {
      npm_config_store_dir: prewarmedStore,
      NPM_CONFIG_STORE_DIR: prewarmedStore,
      PNPM_STORE_DIR: prewarmedStore,
    })
    const command = isolatedInstallCommand(sandbox.storeRoot)

    expect(environment.npm_config_store_dir).toBe(sandbox.storeRoot)
    expect(environment.NPM_CONFIG_STORE_DIR).toBe(sandbox.storeRoot)
    expect(environment.PNPM_STORE_DIR).toBe(sandbox.storeRoot)
    expect(environment.NODE_PATH).toBe('')
    expect(command).toContain(sandbox.storeRoot)
    expect(command).not.toContain(prewarmedStore)
    expect(await readdir(sandbox.storeRoot)).toEqual([])
    await expect(
      readFile(resolve(sandbox.storeRoot, 'sentinel')),
    ).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('不允许 offline 安装借预热 store 伪装成功', () => {
    expect(() =>
      assertOfflineProbeFailed({
        exitCode: 0,
        signal: null,
        stdout: 'Lockfile is up to date',
        stderr: '',
      }),
    ).toThrow('offline install unexpectedly succeeded')

    expect(() =>
      assertOfflineProbeFailed({
        exitCode: 1,
        signal: null,
        stdout: '',
        stderr: 'ERR_PNPM_NO_OFFLINE_TARBALL package is missing',
      }),
    ).not.toThrow()

    expect(() =>
      assertOfflineProbeFailed({
        exitCode: 1,
        signal: null,
        stdout: '',
        stderr: 'unrelated failure',
      }),
    ).toThrow('failed for an unexpected reason')
  })

  it('等待 stdio close 后保留临退出的 cache-miss 标识', async () => {
    const marker = 'ERR_PNPM_NO_OFFLINE_TARBALL trailing marker'
    const script = [
      "process.stderr.write('x'.repeat(1024 * 1024))",
      `process.stderr.write('\\n${marker}')`,
      'process.exitCode = 1',
    ].join(';')

    const result = await captureCommandResult(
      [process.execPath, '-e', script],
      process.cwd(),
    )

    expect(result.stderr.endsWith(marker)).toBe(true)
    expect(() => assertOfflineProbeFailed(result)).not.toThrow()
  })

  it('统一清理 workspace、offline probe 和两个临时 store', async () => {
    const root = await temporaryRoot()
    const sandbox = await createSourceRebuildSandbox(root)

    await removeSourceRebuildSandbox(sandbox)

    await expect(stat(sandbox.root)).rejects.toMatchObject({ code: 'ENOENT' })
  })
})
