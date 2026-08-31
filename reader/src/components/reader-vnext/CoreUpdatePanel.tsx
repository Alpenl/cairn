/**
 * 设置页「关于」区域的部署交互（Issue #41 阶段 3）。
 *
 * 这个面板是 `cairn-updater` 的唯一页面客户端。它遵守四条硬约束：
 *
 * 1. **未解锁只读**。锁定态不发任何部署请求，也不显示任何执行入口；当前版本
 *    由旁边的 AboutCore 从 `/health` 读，与这里是否解锁无关。
 * 2. **凭证只在内存**。`DEPLOY_AUTH_TOKEN` 只活在下面这个组件的 `useState`
 *    里，不写 localStorage / sessionStorage / IndexedDB / cookie / URL，不进
 *    日志，也不进任何 API 缓存（部署请求全部 `cache: 'no-store'`）。刷新页面
 *    组件重新挂载，token 就没了——这不是「记得清掉」，而是根本没有第二份。
 * 3. **每次都确认精确目标**。确认框必须同时显示精确 tag、完整 40 位 commit 和
 *    schema target；缺一个就不允许打开确认框。这里没有「升到最新版」这种按钮，
 *    提交的永远是一个已验证 manifest 里的确切 tag。
 * 4. **进度来自 job status，reload 只认 commit**。更新期间 Core 是停的，所以
 *    进度只能问 helper；成功后浏览器轮询 `/health`，**只有 commit 等于目标**
 *    才 reload——「过了几秒应该好了」不是判据。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Icon } from '../Icon'
import { shortCommit } from '../../lib/core-version'
import type { ReaderHealthPort } from '../../lib/reader-api-ports'
import {
  confirmableTarget,
  createDeployClient,
  holdClassLabel,
  phaseLabel,
  phaseProgress,
  type ConfirmableTarget,
  type DeployCandidate,
  type DeployCheckUpdatesResponse,
  type DeployClient,
  type DeployFailure,
  type DeployJobResponse,
  type DeployVersionResponse,
} from '../../lib/deploy'
import { ReaderDialog } from '../ui/ReaderDialog'

export interface CoreUpdatePanelProps {
  /** 用于成功后按目标 commit 探活 `/health`。 */
  readonly client: ReaderHealthPort
  /** 部署客户端。默认同源，测试用来注入假 helper。 */
  readonly deployClient?: DeployClient
  readonly jobPollIntervalMs?: number
  readonly healthPollIntervalMs?: number
  /** 目标 commit 上线后的动作。默认整页刷新。 */
  readonly onReload?: () => void
}

/** 未解锁 / 已解锁 / 只读手工升级。 */
type Access =
  | { readonly kind: 'locked'; readonly error: string | null }
  | { readonly kind: 'unlocking' }
  | { readonly kind: 'unlocked'; readonly version: DeployVersionResponse }
  | { readonly kind: 'manual'; readonly reason: string }

type Check =
  | { readonly kind: 'idle' }
  | { readonly kind: 'checking' }
  | { readonly kind: 'done'; readonly response: DeployCheckUpdatesResponse }
  | { readonly kind: 'failed'; readonly message: string }

interface Tracking {
  readonly jobID: string
  /** 页面自己提交时记下的确认目标；认领已存在 job 时为 null。 */
  readonly target: ConfirmableTarget | null
  readonly deduplicated: boolean
}

const CONFIRM_TITLE_ID = 'rvx-deploy-confirm-title'

function defaultReload() {
  window.location.reload()
}

/** 把失败翻译成一句能照做的话。任何分支都不回显凭证本身。 */
function describeFailure(failure: DeployFailure): string {
  switch (failure.kind) {
    case 'unauthorized':
      return '部署令牌不正确。这里只接受主机上的 DEPLOY_AUTH_TOKEN，阅读会话、管理员令牌和扩展令牌都不行。'
    case 'missing':
      return '这台机器上没有部署助手，或者它没有被 Caddy 接到这个路径上。'
    case 'conflict':
      return failure.message
    case 'refused':
      return failure.message
    case 'unavailable':
      return failure.message
    case 'unsupported':
      return failure.message
  }
}

function bytes(size: number): string {
  if (size <= 0) return '未知大小'
  const mib = size / (1024 * 1024)
  return mib >= 1 ? `${mib.toFixed(1)} MiB` : `${Math.max(1, Math.round(size / 1024))} KiB`
}

export function CoreUpdatePanel({
  client,
  deployClient,
  jobPollIntervalMs = 2000,
  healthPollIntervalMs = 3000,
  onReload,
}: CoreUpdatePanelProps) {
  const deploy = useMemo(() => deployClient ?? createDeployClient(), [deployClient])

  // 凭证的唯一副本。`draft` 是输入框内容，解锁成功后立刻清空；`token` 是生效
  // 的凭证，只在这个闭包里存在，随组件卸载消失。
  const [draft, setDraft] = useState('')
  const [token, setToken] = useState('')
  const [access, setAccess] = useState<Access>({ kind: 'locked', error: null })
  const [check, setCheck] = useState<Check>({ kind: 'idle' })
  const [confirming, setConfirming] = useState<{ candidate: DeployCandidate; target: ConfirmableTarget } | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [tracking, setTracking] = useState<Tracking | null>(null)
  const [job, setJob] = useState<DeployJobResponse | null>(null)
  const [pollError, setPollError] = useState<string | null>(null)
  const [awaitingCommit, setAwaitingCommit] = useState(false)

  const reload = useCallback(() => { (onReload ?? defaultReload)() }, [onReload])

  const lock = useCallback((error: string | null) => {
    setToken('')
    setDraft('')
    setAccess({ kind: 'locked', error })
    setCheck({ kind: 'idle' })
    setConfirming(null)
    setSubmitError(null)
    setTracking(null)
    setJob(null)
    setPollError(null)
    setAwaitingCommit(false)
  }, [])

  const unlock = useCallback(async (secret: string) => {
    setAccess({ kind: 'unlocking' })
    const result = await deploy.version(secret, undefined)
    if (!result.ok) {
      setDraft('')
      if (result.failure.kind === 'missing' || result.failure.kind === 'unsupported') {
        setAccess({ kind: 'manual', reason: describeFailure(result.failure) })
        return
      }
      setAccess({ kind: 'locked', error: describeFailure(result.failure) })
      return
    }
    setDraft('')
    if (!result.data.eligible) {
      // 不合格的安装（源码运行、Docker 镜像）连凭证都不留：这里往后只有说明文字。
      setAccess({
        kind: 'manual',
        reason: result.data.ineligible_reason ?? '这个安装模式不支持从页面更新。',
      })
      return
    }
    setToken(secret)
    setAccess({ kind: 'unlocked', version: result.data })
    if (result.data.active_job_id) {
      setTracking({ jobID: result.data.active_job_id, target: null, deduplicated: true })
    }
  }, [deploy])

  const runCheck = useCallback(async (force: boolean) => {
    if (!token) return
    setCheck({ kind: 'checking' })
    const result = await deploy.checkUpdates(token, force, undefined)
    if (!result.ok) {
      if (result.failure.kind === 'unauthorized') {
        lock('部署令牌已经失效，请重新解锁。')
        return
      }
      setCheck({ kind: 'failed', message: describeFailure(result.failure) })
      return
    }
    setCheck({ kind: 'done', response: result.data })
  }, [deploy, token, lock])

  const submit = useCallback(async (target: ConfirmableTarget) => {
    if (!token) return
    setSubmitting(true)
    setSubmitError(null)
    const result = await deploy.submitJob(token, target.tag, undefined)
    setSubmitting(false)
    if (!result.ok) {
      if (result.failure.kind === 'unauthorized') {
        lock('部署令牌已经失效，请重新解锁。')
        return
      }
      setSubmitError(describeFailure(result.failure))
      return
    }
    setConfirming(null)
    setTracking({ jobID: result.data.job_id, target, deduplicated: result.data.deduplicated })
  }, [deploy, token, lock])

  // 轮询 job 状态。
  //
  // 它独立于 Core：`quiesce` 之后 webtag 是停的，这条轮询仍然有人回答，因为它
  // 打的是 helper。任何一次读取失败都只记录，绝不重发提交——断线不是「再部署
  // 一次」的理由。
  const trackedJobID = tracking?.jobID ?? null
  useEffect(() => {
    if (!trackedJobID || !token) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const controller = new AbortController()
    const tick = async () => {
      const result = await deploy.job(token, trackedJobID, controller.signal)
      if (cancelled) return
      if (result.ok) {
        setPollError(null)
        setJob(result.data)
        if (result.data.state !== 'running') return
      } else if (result.failure.kind === 'unauthorized') {
        lock('部署令牌已经失效，请重新解锁后再查看任务状态。')
        return
      } else {
        setPollError(describeFailure(result.failure))
      }
      timer = setTimeout(() => { void tick() }, jobPollIntervalMs)
    }
    void tick()
    return () => {
      cancelled = true
      controller.abort()
      if (timer !== undefined) clearTimeout(timer)
    }
  }, [deploy, token, trackedJobID, jobPollIntervalMs, lock])

  // 成功之后按**目标 commit** 探活。
  //
  // 判据只有一个：`/health` 报出的 commit 等于 job 的 target_commit。服务重启
  // 期间探针会连续失败，那是预期；超时、次数、进度条都不是 reload 的理由。
  const targetCommit = (job?.target_commit ?? tracking?.target?.commit ?? '').trim()
  const succeeded = job?.state === 'succeeded'
  useEffect(() => {
    if (!succeeded || targetCommit === '') return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const controller = new AbortController()
    setAwaitingCommit(true)
    const probe = async () => {
      const result = await client.getHealth(controller.signal)
      if (cancelled) return
      if (result.ok && result.data.commit.trim().toLowerCase() === targetCommit.toLowerCase()) {
        setAwaitingCommit(false)
        reload()
        return
      }
      timer = setTimeout(() => { void probe() }, healthPollIntervalMs)
    }
    void probe()
    return () => {
      cancelled = true
      controller.abort()
      if (timer !== undefined) clearTimeout(timer)
    }
  }, [succeeded, targetCommit, client, healthPollIntervalMs, reload])

  const checked = check.kind === 'done' ? check.response : null
  const candidate = checked?.candidate ?? null
  const helperProtocol = access.kind === 'unlocked' ? access.version.helper.protocol : 0
  const protocolTooOld = candidate !== null && candidate.minimum_helper_protocol > helperProtocol
  const currentCommit = (checked?.current.commit ?? '').trim().toLowerCase()
  const alreadyCurrent = candidate !== null && currentCommit !== '' &&
    candidate.commit.trim().toLowerCase() === currentCommit
  const target = confirmableTarget(candidate)
  // 只要这个页面已经认领了一个 job，就不再提供任何提交入口——包括 HOLD 之后。
  // 「再试一次」是这套流程里最危险的按钮：HOLD 的定义就是「不知道机器现在处于
  // 什么状态」，而重试会把一次可控的停机变成一次叠加的破坏。
  const hasJob = tracking !== null
  const canOfferUpdate = candidate !== null && checked !== null &&
    checked.can_update && !protocolTooOld && !alreadyCurrent && !hasJob

  return (
    <article className="rvx-settings-row rvx-deploy">
      <div className="rvx-deploy-body">
        <span className="rvx-eyebrow">部署</span>
        <h2>更新 Core</h2>
        <p>
          页面更新只替换 Core：同一个版本的后端服务、迁移程序和 Reader 静态产物。
          浏览器扩展、Android 和 iOS 不在范围内，数据库容器也不动。
        </p>

        {access.kind !== 'unlocked' && access.kind !== 'manual' && (
          <>
            <p className="rvx-muted">
              未解锁时这里只读。解锁需要主机上的 DEPLOY_AUTH_TOKEN；它只存在这个页面的内存里，
              不写入任何浏览器存储，刷新后需要重新输入。
            </p>
            <form
              className="rvx-deploy-unlock"
              autoComplete="off"
              onSubmit={(event) => {
                event.preventDefault()
                const secret = draft.trim()
                if (secret === '') {
                  // 空令牌在这里就停下：不发请求，也不存在「dev 放行」这条分支。
                  setAccess({ kind: 'locked', error: '请输入部署令牌。空令牌一律拒绝。' })
                  return
                }
                void unlock(secret)
              }}
            >
              <label htmlFor="rvx-deploy-token">部署令牌</label>
              <input
                id="rvx-deploy-token"
                name="cairn-deploy-token"
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={draft}
                disabled={access.kind === 'unlocking'}
                onChange={(event) => setDraft(event.target.value)}
              />
              <button className="rvx-button primary" type="submit" disabled={access.kind === 'unlocking'}>
                <Icon name="bookmark" size={15} />{access.kind === 'unlocking' ? '正在校验…' : '解锁部署'}
              </button>
            </form>
            {access.kind === 'locked' && access.error && (
              <p className="rvx-deploy-error" role="alert">{access.error}</p>
            )}
          </>
        )}

        {access.kind === 'manual' && (
          <div className="rvx-deploy-manual">
            <p role="alert">{access.reason}</p>
            <p className="rvx-muted">
              只能手工升级：在主机上以 root 身份运行这一版发行物自带的 installer，按 runbook
              完成备份、迁移和切换。页面不提供任何替代入口，也不会代为执行。
            </p>
            <button className="rvx-button secondary" type="button" onClick={() => lock(null)}>
              <Icon name="close" size={15} />收起
            </button>
          </div>
        )}

        {access.kind === 'unlocked' && (
          <div className="rvx-deploy-unlocked">
            <p className="rvx-muted">
              已解锁 · 助手协议 v{access.version.helper.protocol} · 版本 {access.version.helper.version}
              {' · commit '}{shortCommit(access.version.helper.commit)}
              {' · 安装模式 '}{access.version.install_mode}
              {' · 仓库 '}{access.version.repo}
            </p>
            <p className="rvx-muted">
              {access.version.current.reachable
                ? `当前 Core：${access.version.current.version ?? '未知'} · commit ${shortCommit(access.version.current.commit ?? '')}`
                : '当前 Core 不可达——更新窗口内这是预期的，任务状态仍然可读。'}
            </p>

            {check.kind === 'failed' && <p className="rvx-deploy-error" role="alert">{check.message}</p>}

            {checked && checked.discovery_error && (
              <p className="rvx-deploy-error" role="alert">
                查不到可用的新版本：{checked.discovery_error}。当前版本的展示不受影响。
              </p>
            )}

            {checked && !checked.discovery_error && !candidate && (
              <p className="rvx-muted">没有查到经过签名验证的候选版本。</p>
            )}

            {candidate && (
              <dl className="rvx-deploy-facts">
                <div><dt>候选 tag</dt><dd>{candidate.tag}</dd></div>
                <div><dt>完整 commit</dt><dd className="rvx-deploy-hash">{candidate.commit}</dd></div>
                <div><dt>schema target</dt><dd>{candidate.schema_target}</dd></div>
                <div><dt>River ledger target</dt><dd>{candidate.river_ledger_target}</dd></div>
                <div><dt>manifest sha256</dt><dd className="rvx-deploy-hash">{candidate.manifest_sha256}</dd></div>
                <div><dt>签名 key</dt><dd>{candidate.signature_key_id}</dd></div>
                <div><dt>Core 制品</dt><dd>{candidate.core_archive}（{bytes(candidate.core_size_bytes)}）</dd></div>
                <div><dt>Reader 制品</dt><dd>{candidate.reader_archive}（{bytes(candidate.reader_size_bytes)}）</dd></div>
                <div>
                  <dt>在线更新</dt>
                  <dd>{candidate.online_update_compatible ? '发行物声明可在线更新' : `发行物拒绝在线更新：${candidate.online_update_reason}`}</dd>
                </div>
                <div>
                  <dt>回退</dt>
                  <dd>{candidate.rollback_compatible ? '可以换回上一版二进制' : `不能靠换回二进制回退：${candidate.rollback_reason}`}</dd>
                </div>
              </dl>
            )}

            {alreadyCurrent && <p className="rvx-muted">已经是最新版本，没有要执行的更新。</p>}

            {protocolTooOld && (
              <p className="rvx-deploy-error" role="alert">
                这个版本要求助手协议 v{candidate?.minimum_helper_protocol}，本机助手是 v{helperProtocol}。
                页面不提供执行入口，请在主机上以 root 手工运行 installer 升级。
              </p>
            )}

            {checked && !checked.can_update && checked.disabled_reason && !protocolTooOld && (
              <p className="rvx-deploy-error" role="alert">不能从页面更新：{checked.disabled_reason}</p>
            )}

            {candidate && !target && (
              <p className="rvx-deploy-error" role="alert">
                候选版本缺少精确 tag、完整 commit 或 schema target，无法生成确认内容，因此不提供更新入口。
              </p>
            )}
          </div>
        )}

        {tracking && (
          <div className="rvx-deploy-job">
            <p className="rvx-deploy-job-head" role="status">
              任务 {tracking.jobID}
              {job
                ? ` · 第 ${phaseProgress(job).index}/${phaseProgress(job).total} 步 · ${phaseLabel(job.phase)}`
                : ' · 正在读取任务状态…'}
            </p>
            {tracking.target && (
              <p className="rvx-muted">
                目标 {tracking.target.tag} · commit <span className="rvx-deploy-hash">{tracking.target.commit}</span>
                {' · schema target '}{tracking.target.schemaTarget}
              </p>
            )}
            {tracking.deduplicated && job?.state === 'running' && (
              <p className="rvx-muted">这次请求并入了已经在进行的同一个任务，没有重复部署。</p>
            )}
            {job && job.phases.length > 0 && (
              <ol className="rvx-deploy-phases">
                {job.phases.map((record) => (
                  <li key={`${record.phase}-${record.started_at}`} className={record.ok ? 'ok' : 'failed'}>
                    {phaseLabel(record.phase)}
                    {record.note ? ` — ${record.note}` : ''}
                  </li>
                ))}
              </ol>
            )}
            {pollError && (
              <p className="rvx-muted" role="alert">
                暂时读不到任务状态（{pollError}），仍在重试。断线不会重复部署。
              </p>
            )}
            {job?.state === 'succeeded' && (
              <p className="rvx-muted" role="status">
                {awaitingCommit
                  ? `更新已完成，正在等待 commit ${shortCommit(targetCommit)} 上线；只有 /health 报出这个 commit 才会刷新页面。`
                  : '更新已完成。'}
              </p>
            )}
            {job?.hold && (
              <div className="rvx-deploy-hold" role="alert">
                <p>
                  已进入 HOLD（{holdClassLabel(job.hold.class)}）· 停在 {phaseLabel(job.hold.phase)}：{job.hold.reason}
                </p>
                <p>{job.hold.remediation}</p>
                {job.hold.detail && <p className="rvx-deploy-hash">{job.hold.detail}</p>}
                <ul>
                  <li>{job.hold.service_stopped ? 'webtag 服务处于停止状态，站点当前不可用。' : 'webtag 服务没有被停止。'}</li>
                  <li>{job.hold.database_migrated ? '数据库迁移已经执行过。' : '数据库迁移没有执行。'}</li>
                  <li>{job.hold.switched ? 'Core 与 Reader 已经切到新版本目录。' : '没有发生版本切换。'}</li>
                  {job.hold.backup_path && <li>恢复用的备份：<span className="rvx-deploy-hash">{job.hold.backup_path}</span></li>}
                </ul>
                {job.hold.blockers && job.hold.blockers.length > 0 && (
                  <ul>
                    {job.hold.blockers.map((blocker) => (
                      <li key={blocker.step_id}>
                        {blocker.step_id}（{blocker.class}{blocker.manual ? '、需人工执行' : ''}）：{blocker.reason}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}
          </div>
        )}

        {submitError && <p className="rvx-deploy-error" role="alert">{submitError}</p>}
      </div>

      {access.kind === 'unlocked' && (
        <div className="rvx-settings-control">
          <button
            className="rvx-button secondary"
            type="button"
            disabled={check.kind === 'checking'}
            onClick={() => { void runCheck(check.kind === 'done') }}
          >
            <Icon name="refresh" size={15} />{check.kind === 'checking' ? '正在检查…' : check.kind === 'done' ? '重新检查' : '检查更新'}
          </button>
          {canOfferUpdate && target && candidate && (
            <button
              className="rvx-button primary"
              type="button"
              onClick={() => { setSubmitError(null); setConfirming({ candidate, target }) }}
            >
              <Icon name="download" size={15} />更新到 {target.tag}
            </button>
          )}
          {hasJob && (
            <small className="rvx-muted">
              {job?.state === 'hold'
                ? '任务已进入 HOLD，页面不提供重试；请按 runbook 人工处理。'
                : job?.state === 'succeeded'
                  ? '任务已完成，等待目标 commit 上线。'
                  : '任务进行中，不能再提交第二个目标。'}
            </small>
          )}
          <button className="rvx-button secondary" type="button" onClick={() => lock(null)}>
            <Icon name="close" size={15} />锁定
          </button>
        </div>
      )}

      {confirming && (
        <ReaderDialog
          title={`确认更新到 ${confirming.target.tag}`}
          titleId={CONFIRM_TITLE_ID}
          busy={submitting}
          onClose={() => { if (!submitting) setConfirming(null) }}
        >
          <div className="rvx-deploy-confirm">
            <dl className="rvx-deploy-facts">
              <div><dt>精确 tag</dt><dd>{confirming.target.tag}</dd></div>
              <div><dt>完整 commit</dt><dd className="rvx-deploy-hash">{confirming.target.commit}</dd></div>
              <div><dt>schema target</dt><dd>{confirming.target.schemaTarget}</dd></div>
              <div><dt>River ledger target</dt><dd>{confirming.candidate.river_ledger_target}</dd></div>
              <div><dt>manifest sha256</dt><dd className="rvx-deploy-hash">{confirming.candidate.manifest_sha256}</dd></div>
              <div><dt>签名 key</dt><dd>{confirming.candidate.signature_key_id}</dd></div>
            </dl>
            <p>确认后按顺序执行，中途不会再问你第二次：</p>
            <ol>
              <li>停止 webtag 服务，进入停写维护窗口；期间 API 与阅读界面不可用。</li>
              <li>做一次 custom-format 数据库备份，并验证它可以被列出，然后才继续。</li>
              <li>用目标版本自带的 migrate 执行数据库迁移到 schema target {confirming.target.schemaTarget}，
                并核对应用与 River 两套 ledger。</li>
              <li>原子切换 Core 与 Reader 到同一个 commit，启动服务并要求 /health 报出目标 commit、/ready 成功。</li>
            </ol>
            <p>
              范围只有 Core：后端服务、迁移程序和 Reader 静态产物。浏览器扩展、Android、iOS 都不更新，
              PostgreSQL 容器不会被重启或替换。
            </p>
            <p>
              {confirming.candidate.rollback_compatible
                ? '这个版本声明失败后可以换回上一版二进制。'
                : `这个版本声明不能靠换回二进制回退：${confirming.candidate.rollback_reason}。失败时按绑定备份的人工 runbook 恢复。`}
            </p>
            <footer>
              <button type="button" disabled={submitting} onClick={() => setConfirming(null)}>取消</button>
              <button
                type="submit"
                disabled={submitting}
                onClick={() => { void submit(confirming.target) }}
              >
                {submitting ? '正在提交…' : `停写并更新到 ${confirming.target.tag}`}
              </button>
            </footer>
          </div>
        </ReaderDialog>
      )}
    </article>
  )
}
