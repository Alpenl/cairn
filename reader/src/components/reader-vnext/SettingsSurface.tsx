import { useEffect, useState } from 'react'
import type { ReaderNavigationRequest } from '../../lib/navigation/route'
import {
  readReaderStartupPreference,
  writeReaderStartupPreference,
  type ReaderStartupPreference,
} from '../../lib/navigation/route'
import { Icon } from '../Icon'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import {
  coreBuildIdentity,
  formatBuildTime,
  isDevelopmentBuild,
  lookupCoreRelease,
  shortCommit,
  type CoreBuildIdentity,
  type CoreReleaseLookup,
} from '../../lib/core-version'
import { SurfaceShell } from './SurfaceShell'

export interface SettingsSurfaceProps {
  readonly onNavigate: ReaderNavigationRequest
  readonly onOpenConnectionSettings: () => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly client: IdentityBoundReaderClient
}

const STARTUP_PREFERENCES: ReadonlyArray<{
  readonly id: ReaderStartupPreference
  readonly label: string
  readonly description: string
}> = [
  { id: 'last', label: '记住上次位置', description: '首次默认今天，之后恢复最后打开的 Reader 路由。' },
  { id: 'home', label: '总是今天', description: '每次进入 Reader 都先落到今天。' },
  { id: 'reading', label: '直接阅读', description: '跳过今天，直接进入阅读。' },
]

type CoreIdentityState =
  | { readonly kind: 'loading' }
  | { readonly kind: 'ready'; readonly identity: CoreBuildIdentity }
  /** 后端不可达：读不到版本，但设置页其余部分必须照常可用。 */
  | { readonly kind: 'unreachable' }

type ReleaseState =
  | { readonly kind: 'idle' }
  | { readonly kind: 'loading' }
  | { readonly kind: 'done'; readonly lookup: CoreReleaseLookup }

/**
 * 「关于」区域：显示后端 `/health` 报告的运行中 Core 标识。
 *
 * 这是一个纯只读面板。版本、commit、构建时间全部来自后端已有的探针端点；
 * Release 查询是使用者主动点一下才发生的旁支，且只读 GitHub 上与当前版本
 * 精确对应的那条 Release。这里既没有升级动作，也没有任何凭证输入——
 * 无论 GitHub 是否可达，当前版本的展示都不受影响。
 */
function AboutCore({ client }: { readonly client: IdentityBoundReaderClient }) {
  const [identityState, setIdentityState] = useState<CoreIdentityState>({ kind: 'loading' })
  const [release, setRelease] = useState<ReleaseState>({ kind: 'idle' })
  const [retryToken, setRetryToken] = useState(0)

  useEffect(() => {
    let cancelled = false
    const controller = new AbortController()
    setIdentityState({ kind: 'loading' })
    setRelease({ kind: 'idle' })
    void (async () => {
      const result = await client.getHealth(controller.signal)
      if (cancelled) return
      setIdentityState(result.ok
        ? { kind: 'ready', identity: coreBuildIdentity(result.data) }
        : { kind: 'unreachable' })
    })()
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [client, retryToken])

  const identity = identityState.kind === 'ready' ? identityState.identity : null
  const development = identity ? isDevelopmentBuild(identity) : false
  const buildTime = identity ? formatBuildTime(identity.buildTime) : null

  const checkRelease = async (version: string) => {
    setRelease({ kind: 'loading' })
    const lookup = await lookupCoreRelease(version)
    setRelease({ kind: 'done', lookup })
  }

  return (
    <article className="rvx-settings-row">
      <div>
        <span className="rvx-eyebrow">关于</span>
        <h2>Core 版本</h2>
        {identityState.kind === 'loading' && <p>正在读取后端版本…</p>}
        {identityState.kind === 'unreachable' && (
          <p>后端不可达，暂时读不到当前版本。恢复连接后可以再试一次。</p>
        )}
        {identity && (
          <>
            <p>
              {development ? '开发构建' : `版本 ${identity.version}`}
              {' · commit '}
              {shortCommit(identity.commit)}
            </p>
            <p className="rvx-muted">{buildTime ? `构建于 ${buildTime}。` : '构建时间未注入。'}</p>
            {development && (
              <p className="rvx-muted">这个构建没有注入版本信息，GitHub 上也没有对应的正式 Release。</p>
            )}
            {release.kind === 'loading' && <p className="rvx-muted" role="status">正在查询 GitHub…</p>}
            {release.kind === 'done' && release.lookup.kind === 'found' && (
              <p className="rvx-muted" role="status">
                {release.lookup.title ?? release.lookup.tag}
                {release.lookup.publishedAt ? ` · 发布于 ${release.lookup.publishedAt}` : ''}
                {' · '}
                <a href={release.lookup.url} target="_blank" rel="noreferrer noopener">查看更新日志</a>
              </p>
            )}
            {release.kind === 'done' && release.lookup.kind === 'missing' && (
              <p className="rvx-muted" role="status">GitHub 上没有 {release.lookup.tag} 对应的 Release。</p>
            )}
            {release.kind === 'done' && release.lookup.kind === 'unavailable' && (
              <p className="rvx-muted" role="status">暂时连不上 GitHub，当前版本信息不受影响。</p>
            )}
          </>
        )}
      </div>
      <div className="rvx-settings-control">
        {identityState.kind === 'unreachable' && (
          <button className="rvx-button secondary" type="button" onClick={() => setRetryToken((token) => token + 1)}>
            <Icon name="more" size={15} />重新读取
          </button>
        )}
        {identity && !development && (
          <button
            className="rvx-button secondary"
            type="button"
            disabled={release.kind === 'loading'}
            onClick={() => { void checkRelease(identity.version) }}
          >
            <Icon name="clock" size={15} />查看这个版本的 Release
          </button>
        )}
      </div>
    </article>
  )
}

export function SettingsSurface({ onNavigate, onOpenConnectionSettings, capabilityPolicy, client }: SettingsSurfaceProps) {
  const [startupPreference, setStartupPreference] = useState<ReaderStartupPreference>(() => readReaderStartupPreference())
  const [saved, setSaved] = useState<boolean | null>(null)

  const chooseStartupPreference = (preference: ReaderStartupPreference) => {
    const ok = writeReaderStartupPreference(preference)
    if (ok) setStartupPreference(preference)
    setSaved(ok)
  }

  const availableStartupPreferences = capabilityPolicy.home
    ? STARTUP_PREFERENCES
    : STARTUP_PREFERENCES.filter((preference) => preference.id !== 'home')
  const effectiveStartupPreference = startupPreference === 'home' && !capabilityPolicy.home
    ? 'reading'
    : startupPreference
  const currentStartup = availableStartupPreferences.find(
    (preference) => preference.id === effectiveStartupPreference,
  )
  const currentStartupDescription = currentStartup?.id === 'last' && !capabilityPolicy.home
    ? '首次默认阅读，之后恢复最后打开的可用 Reader 路由。'
    : currentStartup?.description ?? '直接进入阅读资料库。'

  return (
    <SurfaceShell title="设置" subtitle="应用级入口与连接管理" onNavigate={onNavigate} capabilityPolicy={capabilityPolicy}>
      <section className="rvx-settings-list">
        <article className="rvx-settings-row">
          <div>
            <span className="rvx-eyebrow">启动</span>
            <h2>进入 Reader 时打开哪里</h2>
            <p>{currentStartupDescription}</p>
          </div>
          <div className="rvx-settings-control">
            <div className="rvx-segmented" role="group" aria-label="Reader 启动偏好">
              {availableStartupPreferences.map((preference) => (
                <button
                  key={preference.id}
                  type="button"
                  className={effectiveStartupPreference === preference.id ? 'active' : ''}
                  aria-pressed={effectiveStartupPreference === preference.id}
                  onClick={() => chooseStartupPreference(preference.id)}
                >
                  {preference.label}
                </button>
              ))}
            </div>
            {saved === false && <small className="rvx-muted" role="status">当前浏览器无法保存此偏好。</small>}
            {saved === true && <small className="rvx-muted" role="status">已保存。</small>}
          </div>
        </article>
        <article className="rvx-settings-row">
          <div><span className="rvx-eyebrow">连接</span><h2>后端与身份</h2><p>更换后端或账号时，本机缓存的阅读数据会先清空，再按新账号重新同步。</p></div>
          <button className="rvx-button secondary" type="button" onClick={onOpenConnectionSettings}><Icon name="more" size={15} />打开连接设置</button>
        </article>
        {/* 从主导航降级下来的两个入口。它们不再占一个顶级位置，但必须留一条
            明确的路——降级不等于藏起来。 */}
        {capabilityPolicy.todos && <article className="rvx-settings-row">
          <div><span className="rvx-eyebrow">任务</span><h2>全部 TODO</h2><p>未完成的任务同时显示在「今天」右栏，这里是完整列表。</p></div>
          <button className="rvx-button secondary" type="button" onClick={() => void onNavigate({ kind: 'tool', id: 'todo' })}><Icon name="check" size={15} />打开 TODO</button>
        </article>}
        {capabilityPolicy.history && <article className="rvx-settings-row">
          <div><span className="rvx-eyebrow">数据</span><h2>已归档想法</h2><p>原文或笔记被删除后留下的想法，可以在这里查看并重新挂回。</p></div>
          <button className="rvx-button secondary" type="button" onClick={() => void onNavigate({ kind: 'tool', id: 'history' }, { thoughtView: 'history' })}><Icon name="clock" size={15} />查看已归档</button>
        </article>}

        {/* 回收站放在设置里而不是主导航：找回误删是低频维护动作，主导航只留
            每天都会去的地方。删除时的撤销提示覆盖了绝大多数场景，这里是撤销
            错过之后的兜底入口。 */}
        <article className="rvx-settings-row">
          <div><span className="rvx-eyebrow">数据</span><h2>回收站</h2><p>删除的链接、收件箱条目和笔记都会先留在这里，可以恢复，也可以永久清除。</p></div>
          <button className="rvx-button secondary" type="button" onClick={() => void onNavigate({ kind: 'tool', id: 'trash' })}><Icon name="trash" size={15} />打开回收站</button>
        </article>
        <AboutCore client={client} />
      </section>
    </SurfaceShell>
  )
}
