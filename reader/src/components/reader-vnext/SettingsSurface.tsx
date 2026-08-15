import { useState } from 'react'
import type { ReaderNavigationRequest } from '../../lib/navigation/route'
import {
  readReaderStartupPreference,
  writeReaderStartupPreference,
  type ReaderStartupPreference,
} from '../../lib/navigation/route'
import { Icon } from '../Icon'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { SurfaceShell } from './SurfaceShell'

export interface SettingsSurfaceProps {
  readonly onNavigate: ReaderNavigationRequest
  readonly onOpenConnectionSettings: () => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
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

export function SettingsSurface({ onNavigate, onOpenConnectionSettings, capabilityPolicy }: SettingsSurfaceProps) {
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
      </section>
    </SurfaceShell>
  )
}
