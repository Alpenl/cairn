/**
 * 部署 API 客户端（Issue #41 阶段 3）。
 *
 * 这四条路由**不属于 webtag 应用**。它们由 root-owned 的 `cairn-updater`
 * 直接提供，Caddy 把固定前缀 `/api/deploy/system/` 直连 helper 的 Unix
 * socket。因此：
 *
 * - 请求走**同源相对路径**：页面自己所在的域名上，Caddy 已经把这段前缀接到
 *   helper 上了。不去拼后端 baseURL——Core 停掉时那个地址上的其它路径全都是
 *   502，而部署状态恰恰要在那个时刻还能读。
 * - `credentials: 'omit'`：Reader 的 session cookie 永远不会被带到部署路由
 *   上。拥有一张阅读会话不等于拥有替换这台机器上程序的权限，这条在客户端也
 *   写死一遍，而不是只指望 helper 拒绝。
 * - 唯一凭证是 `Authorization: Bearer <DEPLOY_AUTH_TOKEN>`，由调用方**每次
 *   显式传入**。这个模块不持有、不缓存、不写任何存储：token 的唯一副本活在
 *   调用它的 React 组件的 state 里，刷新即消失。
 * - token 永远不进 URL、不进 query、不进日志。这个文件里没有一处
 *   `console.*`，失败信息里也不回显凭证。
 *
 * 响应形状是 `cmd/cairn-updater/api.go` 的镜像。`schema_version` 不等于本文件
 * 认识的版本时一律降级为 `unsupported`，不猜字段——一个读不懂的 job 文档，猜
 * 错的代价是对着停机中的机器显示错误的进度。
 */

/** 与 `APISchemaVersion` 对齐。helper 与 Reader 同版本发布，但 HOLD 期间可能差一版。 */
const DEPLOY_API_SCHEMA_VERSION = 1

/** Caddy 固定代理到 helper socket 的前缀。 */
const DEPLOY_API_PREFIX = '/api/deploy/system'

/** 正式 tag 的唯一形状，与 helper 的 `formalTagPattern` 逐字对齐。 */
const FORMAL_TAG = /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/

/** 提交与查询的超时。部署动作本身是异步的，请求只负责把 job 记下来。 */
const DEFAULT_TIMEOUT_MS = 15_000

/**
 * 是否是精确正式 tag。
 *
 * UI 只会提交已验证 manifest 里的 tag，但这条判断仍然写在提交路径上：它是
 * 「不提供一键升到 latest」这条约束在客户端的物理形式。
 */
export function isFormalReleaseTag(target: string): boolean {
  return FORMAL_TAG.test(target)
}

// ── 线上响应形状（cmd/cairn-updater/api.go 的镜像） ────────────────────────

export interface DeployHelperIdentity {
  readonly protocol: number
  readonly version: string
  readonly commit: string
  readonly build_time: string
}

export interface DeployCoreIdentity {
  readonly reachable: boolean
  readonly version?: string
  readonly commit?: string
  readonly build_time?: string
  readonly error?: string
}

export interface DeployVersionResponse {
  readonly schema_version: number
  readonly helper: DeployHelperIdentity
  readonly repo: string
  readonly install_mode: string
  readonly eligible: boolean
  readonly ineligible_reason?: string
  readonly current: DeployCoreIdentity
  readonly active_job_id?: string
}

export interface DeployCandidate {
  readonly tag: string
  readonly version: string
  readonly commit: string
  readonly build_time: string
  readonly manifest_sha256: string
  readonly signature_key_id: string
  readonly schema_target: string
  readonly river_ledger_target: number
  readonly minimum_helper_protocol: number
  readonly core_archive: string
  readonly core_sha256: string
  readonly core_size_bytes: number
  readonly reader_archive: string
  readonly reader_sha256: string
  readonly reader_size_bytes: number
  readonly online_update_compatible: boolean
  readonly online_update_reason: string
  readonly rollback_compatible: boolean
  readonly rollback_reason: string
}

export interface DeployCheckUpdatesResponse {
  readonly schema_version: number
  readonly checked_at: string
  readonly cached: boolean
  readonly current: DeployCoreIdentity
  readonly candidate?: DeployCandidate
  readonly update_available: boolean
  readonly can_update: boolean
  readonly disabled_reason?: string
  readonly discovery_error?: string
}

export interface DeploySubmitJobResponse {
  readonly schema_version: number
  readonly job_id: string
  readonly target: string
  readonly state: DeployJobState
  readonly phase: string
  readonly deduplicated: boolean
}

export type DeployJobState = 'running' | 'succeeded' | 'hold'

export type DeployHoldClass = 'trust' | 'policy' | 'environment' | 'integrity'

export interface DeployBlocker {
  readonly step_id: string
  readonly class: string
  readonly manual: boolean
  readonly reason: string
}

export interface DeployHold {
  readonly phase: string
  readonly class: DeployHoldClass
  readonly reason: string
  readonly detail?: string
  readonly blockers?: readonly DeployBlocker[]
  readonly service_stopped: boolean
  readonly database_migrated: boolean
  readonly switched: boolean
  readonly backup_path?: string
  readonly remediation: string
}

export interface DeployPhaseRecord {
  readonly phase: string
  readonly started_at: string
  readonly finished_at?: string
  readonly ok: boolean
  readonly note?: string
}

export interface DeployJobResponse {
  readonly schema_version: number
  readonly job_id: string
  readonly state: DeployJobState
  readonly phase: string
  readonly order: readonly string[]
  readonly target: string
  readonly target_commit?: string
  readonly schema_target?: string
  readonly river_ledger_target?: number
  readonly manifest_sha256?: string
  readonly signature_key_id?: string
  readonly from_version?: string
  readonly from_commit?: string
  readonly created_at: string
  readonly updated_at: string
  readonly finished_at?: string
  readonly phases: readonly DeployPhaseRecord[]
  readonly hold?: DeployHold
  readonly backup_path?: string
}

// ── 结果与失败 ──────────────────────────────────────────────────────────────

/**
 * 部署调用的失败形状。
 *
 * 分得比普通 API 细，因为这几种失败在页面上的**后果**完全不同：凭证错要退回
 * 锁定态，helper 缺席要退到手工升级说明，冲突要去认领已经存在的 job，而网络
 * 抖动在轮询里必须被当成「什么都没发生」——绝不能触发第二次部署。
 */
export type DeployFailure =
  /** 401：凭证不对。helper 对缺失、空、错、错 scheme 一律给同一个答案。 */
  | { readonly kind: 'unauthorized' }
  /** 404：这台机器上没有 helper，或者这个 job id 不存在。 */
  | { readonly kind: 'missing' }
  /** 409：操作锁拒绝了第二个目标。 */
  | { readonly kind: 'conflict'; readonly message: string }
  /** 其它 4xx：请求本身被判定为非法。 */
  | { readonly kind: 'refused'; readonly code: string; readonly message: string }
  /** 网络失败、超时、5xx、非 JSON。helper 可能根本没被问到。 */
  | { readonly kind: 'unavailable'; readonly message: string }
  /** schema 版本或字段形状读不懂。 */
  | { readonly kind: 'unsupported'; readonly message: string }

export type DeployResult<T> =
  | { readonly ok: true; readonly data: T }
  | { readonly ok: false; readonly failure: DeployFailure }

function ok<T>(data: T): DeployResult<T> {
  return { ok: true, data }
}

function fail<T>(failure: DeployFailure): DeployResult<T> {
  return { ok: false, failure }
}

// ── 形状校验 ────────────────────────────────────────────────────────────────

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isSupportedSchema(body: Record<string, unknown>): boolean {
  return body.schema_version === DEPLOY_API_SCHEMA_VERSION
}

function isVersionResponse(body: unknown): body is DeployVersionResponse {
  if (!isRecord(body) || !isSupportedSchema(body)) return false
  return isRecord(body.helper) && isRecord(body.current) &&
    typeof body.eligible === 'boolean' && typeof body.install_mode === 'string'
}

function isCheckUpdatesResponse(body: unknown): body is DeployCheckUpdatesResponse {
  if (!isRecord(body) || !isSupportedSchema(body)) return false
  if (body.candidate !== undefined && !isRecord(body.candidate)) return false
  return typeof body.can_update === 'boolean' && typeof body.update_available === 'boolean'
}

function isSubmitJobResponse(body: unknown): body is DeploySubmitJobResponse {
  if (!isRecord(body) || !isSupportedSchema(body)) return false
  return typeof body.job_id === 'string' && body.job_id !== '' &&
    typeof body.target === 'string' && typeof body.deduplicated === 'boolean'
}

function isJobResponse(body: unknown): body is DeployJobResponse {
  if (!isRecord(body) || !isSupportedSchema(body)) return false
  return typeof body.job_id === 'string' && typeof body.state === 'string' &&
    typeof body.phase === 'string' && Array.isArray(body.order) && Array.isArray(body.phases)
}

function apiError(body: unknown): { code: string; message: string } | null {
  if (!isRecord(body) || !isRecord(body.error)) return null
  const { code, message } = body.error
  if (typeof code !== 'string' || typeof message !== 'string') return null
  return { code, message }
}

// ── 客户端 ──────────────────────────────────────────────────────────────────

export interface DeployClientOptions {
  /** 默认同源。只有测试会覆盖它。 */
  readonly baseURL?: string
  readonly fetchImpl?: typeof fetch
  readonly timeoutMs?: number
}

export interface DeployClient {
  version(token: string, signal?: AbortSignal): Promise<DeployResult<DeployVersionResponse>>
  checkUpdates(token: string, force: boolean, signal?: AbortSignal): Promise<DeployResult<DeployCheckUpdatesResponse>>
  submitJob(token: string, target: string, signal?: AbortSignal): Promise<DeployResult<DeploySubmitJobResponse>>
  job(token: string, jobID: string, signal?: AbortSignal): Promise<DeployResult<DeployJobResponse>>
}

export function createDeployClient(options: DeployClientOptions = {}): DeployClient {
  const base = (options.baseURL ?? '').replace(/\/+$/, '')
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS
  const fetchImpl = options.fetchImpl ?? (typeof fetch === 'undefined' ? null : fetch)

  async function request<T>(
    method: 'GET' | 'POST',
    path: string,
    token: string,
    body: unknown,
    guard: (value: unknown) => value is T,
    signal?: AbortSignal,
  ): Promise<DeployResult<T>> {
    // 空 token 在客户端就停下，一个字节都不发。这是 fail-closed 的第一道：
    // 「没填凭证」不应该变成一次可以被日志、代理或计数器观察到的部署尝试。
    // dev 构建同样走这一条，没有豁免分支可言。
    if (token.trim() === '') return fail({ kind: 'unauthorized' })
    if (!fetchImpl) return fail({ kind: 'unavailable', message: '当前环境不支持网络请求' })

    const controller = new AbortController()
    const abortForCaller = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abortForCaller, { once: true })
    const timer = setTimeout(() => controller.abort(), timeoutMs)

    try {
      const headers: Record<string, string> = {
        Accept: 'application/json',
        Authorization: `Bearer ${token}`,
      }
      if (body !== undefined) headers['Content-Type'] = 'application/json'

      let response: Response
      try {
        response = await fetchImpl(`${base}${path}`, {
          method,
          // 部署答案永不缓存：维护窗口里读到一份几分钟前的 job 状态，比读不到
          // 更危险。helper 也会回 `Cache-Control: no-store`，两边都写。
          cache: 'no-store',
          // Reader 的 session cookie 不会被带上——见文件头。
          credentials: 'omit',
          referrerPolicy: 'no-referrer',
          headers,
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: controller.signal,
        })
      } catch {
        return fail({ kind: 'unavailable', message: '连不上部署助手' })
      }

      let parsed: unknown = null
      try {
        const text = await response.text()
        parsed = text ? JSON.parse(text) : null
      } catch {
        parsed = null
      }

      if (response.status === 401) return fail({ kind: 'unauthorized' })
      if (response.status === 404) return fail({ kind: 'missing' })
      if (response.status === 409) {
        return fail({ kind: 'conflict', message: apiError(parsed)?.message ?? '另一个更新正在进行' })
      }
      if (response.status >= 500) {
        return fail({ kind: 'unavailable', message: apiError(parsed)?.message ?? '部署助手内部失败' })
      }
      if (!response.ok) {
        const failure = apiError(parsed)
        return fail({
          kind: 'refused',
          code: failure?.code ?? 'invalid_request',
          message: failure?.message ?? '部署助手拒绝了这次请求',
        })
      }
      if (!guard(parsed)) {
        return fail({
          kind: 'unsupported',
          message: '部署助手返回了这个页面读不懂的响应，请用命令行 installer 处理',
        })
      }
      return ok(parsed)
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abortForCaller)
    }
  }

  return {
    version: (token, signal) =>
      request('GET', `${DEPLOY_API_PREFIX}/version`, token, undefined, isVersionResponse, signal),
    checkUpdates: (token, force, signal) =>
      request(
        'GET',
        `${DEPLOY_API_PREFIX}/check-updates${force ? '?force=true' : ''}`,
        token,
        undefined,
        isCheckUpdatesResponse,
        signal,
      ),
    submitJob: async (token, target, signal) => {
      // 目标形状在提交前再判一次。UI 只会传已验证 manifest 里的 tag，这条存在
      // 的意义是：任何试图把 `latest`、分支名或 URL 塞进提交路径的改动，会在
      // 这里而不是在生产机器上失败。
      if (!isFormalReleaseTag(target)) {
        return fail({ kind: 'refused', code: 'invalid_target', message: '只接受精确正式版本，例如 v1.2.3' })
      }
      return request('POST', `${DEPLOY_API_PREFIX}/jobs`, token, { target }, isSubmitJobResponse, signal)
    },
    job: (token, jobID, signal) =>
      request(
        'GET',
        `${DEPLOY_API_PREFIX}/jobs/${encodeURIComponent(jobID)}`,
        token,
        undefined,
        isJobResponse,
        signal,
      ),
  }
}

// ── 展示用的纯函数 ──────────────────────────────────────────────────────────

/**
 * 确认对话框必须齐备的三个字段。
 *
 * 缺任何一个就**不允许打开确认框**：操作者确认的是「这个 tag、这个 commit、
 * 这个 schema target」，一份显示不全的确认等于让人对着未知点头。
 */
export interface ConfirmableTarget {
  readonly tag: string
  readonly commit: string
  readonly schemaTarget: string
}

export function confirmableTarget(candidate: DeployCandidate | null | undefined): ConfirmableTarget | null {
  if (!candidate) return null
  const tag = candidate.tag.trim()
  const commit = candidate.commit.trim()
  const schemaTarget = candidate.schema_target.trim()
  if (!isFormalReleaseTag(tag)) return null
  // 完整 commit：40 位十六进制。短 commit 在确认屏上不算数——两个 release 的
  // 短 commit 可以撞，而这块屏幕是唯一一次人工核对的机会。
  if (!/^[0-9a-f]{40}$/i.test(commit)) return null
  if (schemaTarget === '') return null
  return { tag, commit, schemaTarget }
}

/** 阶段的中文说明，顺序由 helper 的 `order` 决定，这里只做翻译。 */
const PHASE_LABELS: Readonly<Record<string, string>> = {
  queued: '排队',
  verify_manifest: '复核签名 manifest',
  download: '下载制品',
  verify_artifacts: '校验制品与身份',
  preflight: '预检磁盘与数据库',
  quiesce: '停止 Core（停写开始）',
  backup: '数据库备份',
  migrate: '执行数据库迁移',
  switch: '原子切换 Core 与 Reader',
  start: '启动并验证 /health、/ready',
  audit: '写入审计状态',
  done: '完成',
}

export function phaseLabel(phase: string): string {
  return PHASE_LABELS[phase] ?? phase
}

const HOLD_CLASS_LABELS: Readonly<Record<string, string>> = {
  trust: '信任校验失败',
  policy: '合同拒绝',
  environment: '主机环境问题',
  integrity: '完整性存疑',
}

export function holdClassLabel(holdClass: string): string {
  return HOLD_CLASS_LABELS[holdClass] ?? holdClass
}

/** `order` 里已经走过的阶段数，用于显示「第 n / m 步」。 */
export function phaseProgress(job: DeployJobResponse): { readonly index: number; readonly total: number } {
  const total = job.order.length
  const index = job.order.indexOf(job.phase)
  return { index: index < 0 ? 0 : index + 1, total }
}
