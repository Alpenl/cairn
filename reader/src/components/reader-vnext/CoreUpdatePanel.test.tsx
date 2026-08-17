import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok, type ApiResult } from '../../lib/api/result'
import type { HealthResponse } from '../../lib/api/types'
import type {
  DeployCandidate,
  DeployCheckUpdatesResponse,
  DeployClient,
  DeployFailure,
  DeployJobResponse,
  DeployResult,
  DeploySubmitJobResponse,
  DeployVersionResponse,
} from '../../lib/deploy'
import { CoreUpdatePanel } from './CoreUpdatePanel'

const TOKEN = 'deploy-token-0123456789abcdef0123456789'
const CURRENT_COMMIT = 'b'.repeat(40)
const TARGET_COMMIT = 'c'.repeat(40)
const SCHEMA_TARGET = '0042_reader_notes'

function good<T>(data: T): DeployResult<T> {
  return { ok: true, data }
}

function bad<T>(failure: DeployFailure): DeployResult<T> {
  return { ok: false, failure }
}

function versionResponse(overrides: Partial<DeployVersionResponse> = {}): DeployVersionResponse {
  return {
    schema_version: 1,
    helper: { protocol: 1, version: '1.4.0', commit: 'a'.repeat(40), build_time: '2026-08-01T10:00:00Z' },
    repo: 'Alpenl/cairn',
    install_mode: 'systemd-release',
    eligible: true,
    current: { reachable: true, version: '1.4.0', commit: CURRENT_COMMIT, build_time: '2026-08-01T10:00:00Z' },
    ...overrides,
  }
}

function candidate(overrides: Partial<DeployCandidate> = {}): DeployCandidate {
  return {
    tag: 'v1.5.0',
    version: '1.5.0',
    commit: TARGET_COMMIT,
    build_time: '2026-08-16T09:00:00Z',
    manifest_sha256: 'd'.repeat(64),
    signature_key_id: 'cairn-release-2026',
    schema_target: SCHEMA_TARGET,
    river_ledger_target: 7,
    minimum_helper_protocol: 1,
    core_archive: 'cairn-core-v1.5.0-linux-amd64.tar.gz',
    core_sha256: 'e'.repeat(64),
    core_size_bytes: 24_000_000,
    reader_archive: 'cairn-reader-v1.5.0.tar.gz',
    reader_sha256: 'f'.repeat(64),
    reader_size_bytes: 3_000_000,
    online_update_compatible: true,
    online_update_reason: '',
    rollback_compatible: false,
    rollback_reason: '0042 新增了不可逆的列',
    ...overrides,
  }
}

function checkResponse(overrides: Partial<DeployCheckUpdatesResponse> = {}): DeployCheckUpdatesResponse {
  return {
    schema_version: 1,
    checked_at: '2026-08-17T00:00:00Z',
    cached: false,
    current: { reachable: true, version: '1.4.0', commit: CURRENT_COMMIT },
    candidate: candidate(),
    update_available: true,
    can_update: true,
    ...overrides,
  }
}

function submitResponse(overrides: Partial<DeploySubmitJobResponse> = {}): DeploySubmitJobResponse {
  return {
    schema_version: 1,
    job_id: 'job-2026-08-17-01',
    target: 'v1.5.0',
    state: 'running',
    phase: 'queued',
    deduplicated: false,
    ...overrides,
  }
}

const PHASE_ORDER = [
  'queued', 'verify_manifest', 'download', 'verify_artifacts', 'preflight',
  'quiesce', 'backup', 'migrate', 'switch', 'start', 'audit', 'done',
]

function jobResponse(overrides: Partial<DeployJobResponse> = {}): DeployJobResponse {
  return {
    schema_version: 1,
    job_id: 'job-2026-08-17-01',
    state: 'running',
    phase: 'migrate',
    order: PHASE_ORDER,
    target: 'v1.5.0',
    target_commit: TARGET_COMMIT,
    schema_target: SCHEMA_TARGET,
    river_ledger_target: 7,
    manifest_sha256: 'd'.repeat(64),
    signature_key_id: 'cairn-release-2026',
    from_version: '1.4.0',
    from_commit: CURRENT_COMMIT,
    created_at: '2026-08-17T00:00:00Z',
    updated_at: '2026-08-17T00:01:00Z',
    phases: [
      { phase: 'verify_manifest', started_at: '2026-08-17T00:00:01Z', finished_at: '2026-08-17T00:00:03Z', ok: true },
      { phase: 'quiesce', started_at: '2026-08-17T00:00:30Z', finished_at: '2026-08-17T00:00:34Z', ok: true, note: 'webtag 已停止' },
    ],
    ...overrides,
  }
}

/** 队列式返回：最后一项之后一直重复最后一项，方便描述轮询序列。 */
function sequence<T>(items: readonly T[]): () => T {
  let index = 0
  return () => {
    const value = items[Math.min(index, items.length - 1)]
    index += 1
    return value
  }
}

function deployStub(overrides: Partial<DeployClient> = {}): DeployClient {
  return {
    version: vi.fn(async () => good(versionResponse())),
    checkUpdates: vi.fn(async () => good(checkResponse())),
    submitJob: vi.fn(async () => good(submitResponse())),
    job: vi.fn(async () => good(jobResponse())),
    ...overrides,
  }
}

function readerClient(healths: readonly ApiResult<HealthResponse>[] = []): IdentityBoundReaderClient {
  const next = sequence(healths.length > 0 ? healths : [err({ kind: 'network-unreachable', message: 'offline' })])
  return { getHealth: vi.fn(async () => next()) } as unknown as IdentityBoundReaderClient
}

function health(commit: string): ApiResult<HealthResponse> {
  return ok({ status: 'ok', version: '1.5.0', commit, build_time: '2026-08-16T09:00:00Z' })
}

function renderPanel(props: Partial<Parameters<typeof CoreUpdatePanel>[0]> = {}) {
  const deployClient = props.deployClient ?? deployStub()
  const client = props.client ?? readerClient()
  const view = render(
    <CoreUpdatePanel
      client={client}
      deployClient={deployClient}
      jobPollIntervalMs={5}
      healthPollIntervalMs={5}
      onReload={props.onReload ?? vi.fn()}
    />,
  )
  return { view, deployClient, client }
}

function typeToken(token: string) {
  fireEvent.change(screen.getByLabelText('部署令牌'), { target: { value: token } })
  fireEvent.click(screen.getByRole('button', { name: /解锁部署/ }))
}

/**
 * 凭证除了 React 内存之外不许出现在任何地方。
 *
 * 这不是「我们记得清理」，而是「没有第二份可清」——所以断言查的是所有会让它
 * 活过一次刷新或被别人读到的位置。
 */
function expectTokenLivesOnlyInMemory(token: string) {
  expect(localStorage.length).toBe(0)
  expect(sessionStorage.length).toBe(0)
  expect(JSON.stringify(localStorage)).not.toContain(token)
  expect(JSON.stringify(sessionStorage)).not.toContain(token)
  expect(document.cookie).not.toContain(token)
  expect(window.location.href).not.toContain(token)
  expect(document.body.innerHTML).not.toContain(token)
}

describe('CoreUpdatePanel 未解锁只读', () => {
  afterEach(() => {
    localStorage.clear()
    sessionStorage.clear()
  })

  it('锁定态不发任何部署请求，也不显示执行入口', async () => {
    const { deployClient } = renderPanel()

    expect(screen.getByText(/页面更新只替换 Core/)).toBeInTheDocument()
    expect(screen.getByLabelText('部署令牌')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到/ })).not.toBeInTheDocument()
    expect(deployClient.version).not.toHaveBeenCalled()
    expect(deployClient.checkUpdates).not.toHaveBeenCalled()
    expect(deployClient.job).not.toHaveBeenCalled()
  })

  it('令牌输入框是 password 且不参与自动填充', () => {
    renderPanel()
    const input = screen.getByLabelText('部署令牌')
    expect(input).toHaveAttribute('type', 'password')
    expect(input).toHaveAttribute('autocomplete', 'off')
  })

  it('空令牌不发请求，直接拒绝', () => {
    const { deployClient } = renderPanel()
    fireEvent.click(screen.getByRole('button', { name: /解锁部署/ }))
    expect(screen.getByRole('alert')).toHaveTextContent('空令牌一律拒绝')
    expect(deployClient.version).not.toHaveBeenCalled()
  })

  it('源码里没有任何持久化凭证的写法', () => {
    const source = readFileSync(resolve(process.cwd(), 'src/components/reader-vnext/CoreUpdatePanel.tsx'), 'utf8')
    for (const forbidden of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'console.']) {
      expect(source.split('\n').filter((line) => !line.trim().startsWith('*') && !line.trim().startsWith('//')).join('\n'))
        .not.toContain(forbidden)
    }
  })
})

describe('CoreUpdatePanel 权限矩阵', () => {
  afterEach(() => {
    localStorage.clear()
    sessionStorage.clear()
  })

  // helper 对每一种不合格凭证都回同一个 401，页面因此对每一种都回到锁定态。
  // 这里逐条走一遍，是为了钉住「没有任何一种应用侧凭证能换到部署权限」。
  const wrongCredentials = [
    ['错的 deploy token', 'not-the-deploy-token'],
    ['Reader 会话 id', 'webtag_session=0123456789abcdef'],
    ['admin token', 'admin-token-0123456789abcdef'],
    ['extension token', 'extension-token-0123456789abcdef'],
  ] as const

  for (const [name, credential] of wrongCredentials) {
    it(`拒绝${name}并退回锁定态`, async () => {
      const deployClient = deployStub({ version: vi.fn(async () => bad<DeployVersionResponse>({ kind: 'unauthorized' })) })
      renderPanel({ deployClient })

      typeToken(credential)

      expect(await screen.findByRole('alert')).toHaveTextContent('部署令牌不正确')
      expect(screen.getByLabelText('部署令牌')).toHaveValue('')
      expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
      expectTokenLivesOnlyInMemory(credential)
    })
  }

  it('开放模式的后端也换不到部署入口', async () => {
    // helper 不认识「开放模式」这回事：没有 Bearer 就是 401，页面照样锁着。
    const deployClient = deployStub({ version: vi.fn(async () => bad<DeployVersionResponse>({ kind: 'unauthorized' })) })
    renderPanel({ deployClient })
    typeToken('')
    expect(screen.getByRole('alert')).toHaveTextContent('空令牌一律拒绝')
    expect(deployClient.version).not.toHaveBeenCalled()
  })
})

describe('CoreUpdatePanel 只读降级', () => {
  it('helper 不存在时只给手工升级说明', async () => {
    const deployClient = deployStub({ version: vi.fn(async () => bad<DeployVersionResponse>({ kind: 'missing' })) })
    renderPanel({ deployClient })

    typeToken(TOKEN)

    expect(await screen.findByText(/没有部署助手/)).toBeInTheDocument()
    expect(screen.getByText(/只能手工升级/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到/ })).not.toBeInTheDocument()
  })

  it('source / Docker 安装模式不合格时不留凭证，也不给执行入口', async () => {
    const deployClient = deployStub({
      version: vi.fn(async () => good(versionResponse({
        install_mode: 'docker',
        eligible: false,
        ineligible_reason: '这是 Docker 镜像安装，页面不能替换容器内的程序。',
      }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)

    expect(await screen.findByText(/Docker 镜像安装/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
    expect(deployClient.checkUpdates).not.toHaveBeenCalled()
    expectTokenLivesOnlyInMemory(TOKEN)
  })

  it('helper protocol 不兼容时只显示手工升级，不给更新按钮', async () => {
    const deployClient = deployStub({
      checkUpdates: vi.fn(async () => good(checkResponse({
        candidate: candidate({ minimum_helper_protocol: 3 }),
        can_update: false,
        disabled_reason: 'helper protocol 太旧',
      }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))

    expect(await screen.findByText(/要求助手协议 v3/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到 v1\.5\.0/ })).not.toBeInTheDocument()
  })

  it('GitHub 查不到候选时仍然显示已解锁的当前版本', async () => {
    const deployClient = deployStub({
      checkUpdates: vi.fn(async () => good(checkResponse({
        candidate: undefined,
        update_available: false,
        can_update: false,
        discovery_error: 'dial tcp: lookup api.github.com: no such host',
      }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))

    expect(await screen.findByText(/查不到可用的新版本/)).toBeInTheDocument()
    expect(screen.getByText(/当前 Core：1\.4\.0/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到/ })).not.toBeInTheDocument()
  })
})

describe('CoreUpdatePanel 确认对话框', () => {
  async function openConfirm() {
    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    return screen.getByRole('dialog')
  }

  it('每次都显示精确 tag、完整 commit、schema target 与动作，缺一不可', async () => {
    const deployClient = deployStub()
    renderPanel({ deployClient })

    const dialog = await openConfirm()

    // 这三条是这块屏幕存在的理由：任何把它简化成「更新到最新版」的改动都会
    // 在这里失败。
    expect(dialog).toHaveTextContent('v1.5.0')
    expect(dialog).toHaveTextContent(TARGET_COMMIT)
    expect(dialog).toHaveTextContent(SCHEMA_TARGET)
    expect(dialog.textContent ?? '').not.toMatch(/最新版|latest/i)

    // 动作必须被说清楚：停写、备份、迁移、Core 范围、目标 identity。
    expect(dialog).toHaveTextContent('停写维护窗口')
    expect(dialog).toHaveTextContent('数据库备份')
    expect(dialog).toHaveTextContent(`迁移到 schema target ${SCHEMA_TARGET}`)
    expect(dialog).toHaveTextContent('浏览器扩展、Android、iOS 都不更新')
    expect(dialog).toHaveTextContent('/health 报出目标 commit')
    expect(dialog).toHaveTextContent('不能靠换回二进制回退')
    expect(dialog).toHaveTextContent('cairn-release-2026')
  })

  it('候选缺少 schema target 时根本不提供更新入口', async () => {
    const deployClient = deployStub({
      checkUpdates: vi.fn(async () => good(checkResponse({ candidate: candidate({ schema_target: '' }) }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))

    expect(await screen.findByText(/缺少精确 tag、完整 commit 或 schema target/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到 v1\.5\.0/ })).not.toBeInTheDocument()
    expect(deployClient.submitJob).not.toHaveBeenCalled()
  })

  it('取消不提交任何东西', async () => {
    const deployClient = deployStub()
    renderPanel({ deployClient })

    await openConfirm()
    fireEvent.click(screen.getByRole('button', { name: '取消' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(deployClient.submitJob).not.toHaveBeenCalled()
  })

  it('确认后提交的是精确 tag', async () => {
    const deployClient = deployStub({ job: vi.fn(async () => good(jobResponse())) })
    renderPanel({ deployClient })

    await openConfirm()
    fireEvent.click(screen.getByRole('button', { name: /停写并更新到 v1\.5\.0/ }))

    await waitFor(() => expect(deployClient.submitJob).toHaveBeenCalledTimes(1))
    expect(deployClient.submitJob).toHaveBeenCalledWith(TOKEN, 'v1.5.0', undefined)
  })
})

describe('CoreUpdatePanel 任务进度与刷新判据', () => {
  it('进度来自 job status，成功后只在 commit 等于目标时才刷新', async () => {
    const reload = vi.fn()
    const nextJob = sequence<DeployResult<DeployJobResponse>>([
      good(jobResponse({ state: 'running', phase: 'download' })),
      good(jobResponse({ state: 'running', phase: 'migrate' })),
      good(jobResponse({ state: 'succeeded', phase: 'done' })),
    ])
    const deployClient = deployStub({ job: vi.fn(async () => nextJob()) })
    // 重启期间：先连不上，再报旧 commit，最后才是目标 commit。
    const client = readerClient([
      err({ kind: 'network-unreachable', message: 'offline' }),
      health(CURRENT_COMMIT),
      health(CURRENT_COMMIT),
      health(TARGET_COMMIT),
    ])
    renderPanel({ deployClient, client, onReload: reload })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/job-2026-08-17-01/)).toBeInTheDocument()

    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1))
    // 前三次探针都不匹配目标 commit，所以刷新只可能发生在第四次之后。
    expect((client.getHealth as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(4)
  })

  it('目标 commit 一直没上线就一直不刷新', async () => {
    const reload = vi.fn()
    const deployClient = deployStub({ job: vi.fn(async () => good(jobResponse({ state: 'succeeded', phase: 'done' }))) })
    const client = readerClient([health(CURRENT_COMMIT)])
    renderPanel({ deployClient, client, onReload: reload })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/正在等待 commit/)).toBeInTheDocument()
    await waitFor(() => expect((client.getHealth as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(3))
    expect(reload).not.toHaveBeenCalled()
  })

  it('Core 停止并进入 HOLD 时仍然读得到任务状态', async () => {
    const deployClient = deployStub({
      job: vi.fn(async () => good(jobResponse({
        state: 'hold',
        phase: 'migrate',
        hold: {
          phase: 'migrate',
          class: 'integrity',
          reason: '应用 ledger 停在 0041，没有到达 0042。',
          detail: 'migrate: step 0042 failed: deadlock detected',
          service_stopped: true,
          database_migrated: true,
          switched: false,
          backup_path: '/opt/webtag/backups/2026-08-17T00-00-30Z.dump',
          remediation: '不要重启服务。按这份备份走人工恢复 runbook。',
          blockers: [{ step_id: '0042_reader_notes', class: 'manual', manual: true, reason: '需要人工执行' }],
        },
      }))),
    })
    // Core 已经停了：/health 一直失败，但 job 状态照样能读，因为 helper 是独立进程。
    const client = readerClient([err({ kind: 'network-unreachable', message: 'offline' })])
    renderPanel({ deployClient, client })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/已进入 HOLD（完整性存疑）/)).toBeInTheDocument()
    // HOLD 之后页面绝不提供「再试一次」：那是把一次可控停机变成叠加破坏的按钮。
    expect(screen.getByText(/页面不提供重试/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到 v1\.5\.0/ })).not.toBeInTheDocument()
    expect(screen.getByText(/不要重启服务/)).toBeInTheDocument()
    expect(screen.getByText(/webtag 服务处于停止状态/)).toBeInTheDocument()
    expect(screen.getByText(/数据库迁移已经执行过/)).toBeInTheDocument()
    expect(screen.getByText(/没有发生版本切换/)).toBeInTheDocument()
    expect(screen.getByText('/opt/webtag/backups/2026-08-17T00-00-30Z.dump')).toBeInTheDocument()
  })
})

describe('CoreUpdatePanel 并发、重放与断线', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
  })

  it('任务进行中不再提供第二个提交入口', async () => {
    const deployClient = deployStub()
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/任务进行中，不能再提交第二个目标/)).toBeInTheDocument()
    // 阶段与步数都来自 helper 给的 order，前端不写死顺序。
    expect(await screen.findByText(/第 8\/12 步 · 执行数据库迁移/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到 v1\.5\.0/ })).not.toBeInTheDocument()

    // 再检查一次也不会重新长出执行入口：同一目标只有一个 job。
    fireEvent.click(screen.getByRole('button', { name: /重新检查/ }))
    await waitFor(() => expect(deployClient.checkUpdates).toHaveBeenCalledTimes(2))
    expect(screen.queryByRole('button', { name: /更新到 v1\.5\.0/ })).not.toBeInTheDocument()
    expect(deployClient.submitJob).toHaveBeenCalledTimes(1)
  })

  it('重放的提交被 helper 去重后明确告知没有重复部署', async () => {
    const deployClient = deployStub({
      submitJob: vi.fn(async () => good(submitResponse({ deduplicated: true }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/并入了已经在进行的同一个任务，没有重复部署/)).toBeInTheDocument()
    expect(deployClient.submitJob).toHaveBeenCalledTimes(1)
  })

  it('操作锁拒绝第二个目标时不会静默重试', async () => {
    const deployClient = deployStub({
      submitJob: vi.fn(async () => bad<DeploySubmitJobResponse>({ kind: 'conflict', message: '另一个目标正在部署' })),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText('另一个目标正在部署')).toBeInTheDocument()
    expect(deployClient.submitJob).toHaveBeenCalledTimes(1)
  })

  it('轮询断线只重试读取，绝不重发提交', async () => {
    const nextJob = sequence<DeployResult<DeployJobResponse>>([
      bad({ kind: 'unavailable', message: '连不上部署助手' }),
      bad({ kind: 'unavailable', message: '连不上部署助手' }),
      good(jobResponse({ state: 'running', phase: 'backup' })),
    ])
    const deployClient = deployStub({ job: vi.fn(async () => nextJob()) })
    renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))

    expect(await screen.findByText(/断线不会重复部署/)).toBeInTheDocument()
    expect(await screen.findByText(/数据库备份/)).toBeInTheDocument()
    expect(deployClient.submitJob).toHaveBeenCalledTimes(1)
  })

  it('刷新之后凭证消失，页面回到锁定态且不再轮询', async () => {
    const deployClient = deployStub()
    const { view } = renderPanel({ deployClient })

    typeToken(TOKEN)
    fireEvent.click(await screen.findByRole('button', { name: /检查更新/ }))
    fireEvent.click(await screen.findByRole('button', { name: /更新到 v1\.5\.0/ }))
    fireEvent.click(await screen.findByRole('button', { name: /停写并更新到/ }))
    await waitFor(() => expect(deployClient.job).toHaveBeenCalled())

    expectTokenLivesOnlyInMemory(TOKEN)

    // 刷新 = 组件重新挂载。内存里的那一份是唯一一份，所以它就这么没了。
    view.unmount()
    const pollsBeforeRemount = (deployClient.job as ReturnType<typeof vi.fn>).mock.calls.length
    render(
      <CoreUpdatePanel
        client={readerClient()}
        deployClient={deployClient}
        jobPollIntervalMs={5}
        healthPollIntervalMs={5}
        onReload={vi.fn()}
      />,
    )

    expect(screen.getByLabelText('部署令牌')).toHaveValue('')
    expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect((deployClient.job as ReturnType<typeof vi.fn>).mock.calls.length).toBe(pollsBeforeRemount)
  })

  it('解锁时认领 helper 报告的在途任务，而不是新开一个', async () => {
    const deployClient = deployStub({
      version: vi.fn(async () => good(versionResponse({ active_job_id: 'job-already-running' }))),
    })
    renderPanel({ deployClient })

    typeToken(TOKEN)

    expect(await screen.findByText(/job-already-running/)).toBeInTheDocument()
    await waitFor(() => expect(deployClient.job).toHaveBeenCalledWith(TOKEN, 'job-already-running', expect.anything()))
    expect(deployClient.submitJob).not.toHaveBeenCalled()
  })
})
