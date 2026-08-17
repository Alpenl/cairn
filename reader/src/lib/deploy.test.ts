import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import {
  confirmableTarget,
  createDeployClient,
  isFormalReleaseTag,
  phaseProgress,
  type DeployCandidate,
  type DeployJobResponse,
  type DeployResult,
} from './deploy'

/** 一个足够长的假部署令牌；长度与 helper 的 MinimumDeployTokenLength 对齐。 */
const DEPLOY_TOKEN = 'deploy-token-0123456789abcdef0123456789'

interface Call {
  readonly url: string
  readonly init: RequestInit
}

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 400,
    status,
    text: async () => (body === null ? '' : JSON.stringify(body)),
  } as unknown as Response
}

function versionBody(overrides: Record<string, unknown> = {}) {
  return {
    schema_version: 1,
    helper: { protocol: 1, version: '1.4.0', commit: 'a'.repeat(40), build_time: '2026-08-01T10:00:00Z' },
    repo: 'Alpenl/cairn',
    install_mode: 'systemd-release',
    eligible: true,
    current: { reachable: true, version: '1.4.0', commit: 'b'.repeat(40), build_time: '2026-08-01T10:00:00Z' },
    ...overrides,
  }
}

/**
 * 假 helper。
 *
 * 鉴权规则逐字照抄 `cmd/cairn-updater/auth.go`：只认**恰好一个**
 * `Authorization: Bearer <token>`，其它一切——cookie、自定义头、错 scheme、空值
 * ——都是 401。`appOpenMode` 存在只是为了证明它不改变任何结果：应用侧的开放模式
 * 从来不是部署权限。
 */
function fakeHelper(options: { appOpenMode?: boolean } = {}) {
  const calls: Call[] = []
  const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const request = init ?? {}
    calls.push({ url, init: request })
    const headers = (request.headers ?? {}) as Record<string, string>
    const authorization = headers.Authorization
    void options.appOpenMode // 开放模式对这里没有任何影响，这就是重点。
    if (authorization !== `Bearer ${DEPLOY_TOKEN}`) {
      return jsonResponse(401, { error: { code: 'unauthorized', message: 'this endpoint requires the deployment bearer token' } })
    }
    if (url.endsWith('/api/deploy/system/version')) return jsonResponse(200, versionBody())
    if (url.includes('/api/deploy/system/check-updates')) {
      return jsonResponse(200, { schema_version: 1, checked_at: '2026-08-17T00:00:00Z', cached: false, current: { reachable: true }, update_available: false, can_update: false })
    }
    if (url.endsWith('/api/deploy/system/jobs')) {
      return jsonResponse(202, { schema_version: 1, job_id: 'job-1', target: 'v1.5.0', state: 'running', phase: 'queued', deduplicated: false })
    }
    return jsonResponse(200, {
      schema_version: 1, job_id: 'job-1', state: 'running', phase: 'download', order: ['queued', 'download'],
      target: 'v1.5.0', created_at: '2026-08-17T00:00:00Z', updated_at: '2026-08-17T00:00:01Z', phases: [],
    })
  })
  return { fetchImpl, calls }
}

function client(fetchImpl: typeof fetch) {
  return createDeployClient({ baseURL: 'http://helper.test', fetchImpl })
}

const ROUTES: ReadonlyArray<{
  name: string
  call: (c: ReturnType<typeof client>, token: string) => Promise<DeployResult<unknown>>
}> = [
  { name: 'version', call: (c, token) => c.version(token) },
  { name: 'check-updates', call: (c, token) => c.checkUpdates(token, false) },
  { name: 'submit job', call: (c, token) => c.submitJob(token, 'v1.5.0') },
  { name: 'job status', call: (c, token) => c.job(token, 'job-1') },
]

describe('部署 API 客户端的权限矩阵', () => {
  // 这些凭证在这个系统里都真实存在，也都绝不足以部署。它们在这里被当作
  // 「操作者填进解锁框的东西」送进去——UI 没有别的通道能把凭证交给 helper。
  const rejected: ReadonlyArray<{ name: string; token: string }> = [
    { name: '空 deploy token', token: '' },
    { name: '只有空白的 deploy token', token: '   ' },
    { name: '错的 deploy token', token: 'not-the-deploy-token-but-long-enough-to-look' },
    { name: 'Reader 会话 id', token: 'webtag_session=0123456789abcdef0123456789abcdef' },
    { name: 'admin token', token: 'admin-token-0123456789abcdef0123456789' },
    { name: 'extension token', token: 'extension-token-0123456789abcdef012345' },
    { name: '真令牌少一个字符', token: DEPLOY_TOKEN.slice(0, -1) },
    { name: '真令牌多一个字符', token: `${DEPLOY_TOKEN}x` },
  ]

  for (const route of ROUTES) {
    for (const attempt of rejected) {
      it(`${route.name} 拒绝${attempt.name}`, async () => {
        const helper = fakeHelper()
        const result = await route.call(client(helper.fetchImpl), attempt.token)
        expect(result.ok).toBe(false)
        if (!result.ok) expect(result.failure.kind).toBe('unauthorized')
      })
    }
  }

  it('空令牌一个字节都不发出去', async () => {
    const helper = fakeHelper()
    for (const route of ROUTES) {
      await route.call(client(helper.fetchImpl), '')
      await route.call(client(helper.fetchImpl), '  ')
    }
    expect(helper.fetchImpl).not.toHaveBeenCalled()
  })

  it('应用侧的开放模式不改变任何一条路由的答案', async () => {
    const helper = fakeHelper({ appOpenMode: true })
    for (const route of ROUTES) {
      const result = await route.call(client(helper.fetchImpl), '')
      expect(result.ok).toBe(false)
      const wrong = await route.call(client(helper.fetchImpl), 'anything-at-all-that-is-not-the-token')
      expect(wrong.ok).toBe(false)
    }
    const allowed = await ROUTES[0].call(client(helper.fetchImpl), DEPLOY_TOKEN)
    expect(allowed.ok).toBe(true)
  })

  it('正确的令牌在四条路由上都被接受', async () => {
    const helper = fakeHelper()
    for (const route of ROUTES) {
      const result = await route.call(client(helper.fetchImpl), DEPLOY_TOKEN)
      expect(result.ok).toBe(true)
    }
  })

  it('源码里没有任何 dev / 开放模式豁免分支', () => {
    // fail-closed 不是运行时才生效的：这个模块里根本不存在一个能读出「现在是
    // 开发构建」的表达式，所以也就不可能有一条只在 dev 放行的路径。
    const source = readFileSync(resolve(process.cwd(), 'src/lib/deploy.ts'), 'utf8')
    expect(source).not.toMatch(/import\.meta\.env/)
    expect(source).not.toMatch(/process\.env/)
    expect(source).not.toMatch(/\bNODE_ENV\b/)
  })
})

describe('部署请求的形状', () => {
  it('永远不带 cookie、不缓存、不把令牌放进 URL', async () => {
    const helper = fakeHelper()
    const deploy = client(helper.fetchImpl)
    await deploy.version(DEPLOY_TOKEN)
    await deploy.checkUpdates(DEPLOY_TOKEN, true)
    await deploy.submitJob(DEPLOY_TOKEN, 'v1.5.0')
    await deploy.job(DEPLOY_TOKEN, 'job-1')

    expect(helper.calls).toHaveLength(4)
    for (const call of helper.calls) {
      // Reader 的会话 cookie 不会被浏览器附上——拥有一张阅读会话不等于拥有
      // 替换这台机器上程序的权限，这一条在客户端也写死。
      expect(call.init.credentials).toBe('omit')
      expect(call.init.cache).toBe('no-store')
      expect(call.init.referrerPolicy).toBe('no-referrer')
      expect(call.url).not.toContain(DEPLOY_TOKEN)
      const headers = call.init.headers as Record<string, string>
      expect(headers.Authorization).toBe(`Bearer ${DEPLOY_TOKEN}`)
      expect(headers.Cookie).toBeUndefined()
    }
    expect(helper.calls[1].url).toContain('check-updates?force=true')
    expect(helper.calls[2].init.method).toBe('POST')
    expect(helper.calls[2].init.body).toBe(JSON.stringify({ target: 'v1.5.0' }))
  })

  it('只提交精确正式版本，channel / 分支 / URL 连请求都发不出去', async () => {
    const helper = fakeHelper()
    const deploy = client(helper.fetchImpl)
    for (const bad of ['latest', 'main', 'v1.2', 'v1.2.3-rc1', 'v01.2.3', 'https://example.com/x.tar.gz', '']) {
      const result = await deploy.submitJob(DEPLOY_TOKEN, bad)
      expect(result.ok).toBe(false)
    }
    expect(helper.fetchImpl).not.toHaveBeenCalled()
    expect(isFormalReleaseTag('v1.2.3')).toBe(true)
    expect(isFormalReleaseTag('v1.2.3+build')).toBe(false)
  })
})

describe('部署 API 的失败归一', () => {
  function respond(status: number, body: unknown) {
    return vi.fn(async () => jsonResponse(status, body)) as unknown as typeof fetch
  }

  it('404 是 missing（这台机器上没有 helper，或者 job 不存在）', async () => {
    const result = await client(respond(404, { error: { code: 'not_found', message: 'no such job' } })).job(DEPLOY_TOKEN, 'x')
    expect(result.ok ? null : result.failure.kind).toBe('missing')
  })

  it('409 是 conflict，并把操作锁的原话带出来', async () => {
    const result = await client(respond(409, { error: { code: 'operation_in_progress', message: '另一个目标正在部署' } }))
      .submitJob(DEPLOY_TOKEN, 'v1.5.0')
    expect(result.ok ? null : result.failure).toEqual({ kind: 'conflict', message: '另一个目标正在部署' })
  })

  it('5xx 与网络失败都是 unavailable', async () => {
    const server = await client(respond(500, { error: { code: 'internal', message: 'boom' } })).version(DEPLOY_TOKEN)
    expect(server.ok ? null : server.failure.kind).toBe('unavailable')

    const offline = vi.fn(async () => { throw new TypeError('Failed to fetch') }) as unknown as typeof fetch
    const network = await client(offline).version(DEPLOY_TOKEN)
    expect(network.ok ? null : network.failure.kind).toBe('unavailable')
  })

  it('读不懂的 schema 版本降级为 unsupported，而不是猜字段', async () => {
    const result = await client(respond(200, { ...versionBody(), schema_version: 2 })).version(DEPLOY_TOKEN)
    expect(result.ok ? null : result.failure.kind).toBe('unsupported')
  })
})

describe('确认目标的必备字段', () => {
  function candidate(overrides: Partial<DeployCandidate> = {}): DeployCandidate {
    return {
      tag: 'v1.5.0',
      version: '1.5.0',
      commit: 'c'.repeat(40),
      build_time: '2026-08-16T09:00:00Z',
      manifest_sha256: 'd'.repeat(64),
      signature_key_id: 'cairn-release-2026',
      schema_target: '0042_reader_notes',
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

  it('齐备时返回精确 tag、完整 commit 与 schema target', () => {
    expect(confirmableTarget(candidate())).toEqual({
      tag: 'v1.5.0',
      commit: 'c'.repeat(40),
      schemaTarget: '0042_reader_notes',
    })
  })

  it('缺一不可：短 commit、空 schema target、非精确 tag 都不可确认', () => {
    expect(confirmableTarget(candidate({ commit: 'c'.repeat(7) }))).toBeNull()
    expect(confirmableTarget(candidate({ schema_target: '  ' }))).toBeNull()
    expect(confirmableTarget(candidate({ tag: 'latest' }))).toBeNull()
    expect(confirmableTarget(null)).toBeNull()
  })
})

describe('阶段进度', () => {
  it('用 helper 给的 order 算步数，不在前端写死顺序', () => {
    const job = {
      order: ['queued', 'download', 'migrate', 'done'],
      phase: 'migrate',
    } as unknown as DeployJobResponse
    expect(phaseProgress(job)).toEqual({ index: 3, total: 4 })
  })
})
