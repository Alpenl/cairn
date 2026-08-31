/**
 * 连接引导卡片（必要偏差：设计稿无此页，复用 styles.css 的 titlebar / 卡片 / 按钮
 * 语言自建，视觉与设计统一——terracotta 主按钮、暖灰底、6/9px 圆角）。
 *
 * 流程：填后端地址 + 安装令牌 →「测试连接」（执行严格 identity handshake，展示
 * 连通 / 401 / 不可达）→ 保存进入主界面。支持编辑既有配置（带入当前值）。
 */
import { useState, type CSSProperties } from 'react'
import { Icon } from './Icon'
import { resetApplicationCache } from '../lib/sw'
import { ReaderClient } from '../lib/api/client'
import type { ApiError } from '@webtag/api'
import { negotiateSession } from '../lib/session-negotiation'
import { getConnection, type Connection } from '../lib/settings'

export interface ConnectionSetupProps {
  /** 保存成功回调，外层据此切换到主界面。 */
  onSaved: (conn: Connection) => void | Promise<void>
  /** 关闭回调（编辑场景下可取消；首跑无配置时外层可不传）。 */
  onCancel?: () => void
  /**
   * 探测到的同源后端地址（App 的 probeSameOriginBackend 结果）。
   * 已有存量配置时不覆盖它——用户显式填过的地址优先于探测值。
   */
  detectedBaseURL?: string
}

type TestState =
  | { phase: 'idle' }
  | { phase: 'testing' }
  | { phase: 'ok' }
  | { phase: 'error'; error: ApiError }

function errorText(error: ApiError): string {
  switch (error.kind) {
    case 'unauthorized':
      return '安装令牌无效（401）'
    case 'network-unreachable':
      return '无法连接到该地址，请检查地址与后端是否在线'
    case 'timeout':
      return '连接超时，请检查网络或后端状态'
    case 'rate-limited':
      return '请求被限流，请稍后重试'
    default:
      return error.message || '连接失败'
  }
}

function persistenceError(error: unknown): ApiError {
  return {
    kind: 'other',
    message: error instanceof Error ? error.message : '无法保存连接配置',
  }
}

export function ConnectionSetup({ onSaved, onCancel, detectedBaseURL }: ConnectionSetupProps) {
  const [resetting, setResetting] = useState(false)
  const existing = getConnection()
  const [baseURL, setBaseURL] = useState(existing.baseURL || detectedBaseURL?.trim() || '')
  const [installationToken, setInstallationToken] = useState(
    existing.installationToken,
  )
  const [test, setTest] = useState<TestState>({ phase: 'idle' })
  const trimmedURL = baseURL.trim()
  const canSubmit = trimmedURL !== ''

  /**
   * 连接：优先换 httpOnly 会话。只有明确的旧后端 404，或创建成功但 cookie
   * identity 返回 401，才进入 Bearer 兼容模式；瞬时失败必须停住等待重试。
   *
   * 旧后端没有会话端点，或跨源部署无法保留会话 cookie 时，回退到直接发送安装令牌。
   */
  async function connect(persist: boolean): Promise<Connection | null> {
    const trimmedToken = installationToken.trim()
    const sessionConnection: Connection = {
      baseURL: trimmedURL,
      mode: 'session',
      installationToken: '',
    }
    const session = await negotiateSession({
      baseURL: trimmedURL,
      installationToken: trimmedToken,
      commit: persist
        ? async () => {
            await onSaved(sessionConnection)
            return true
          }
        : undefined,
    })
    if (session.kind === 'session') return sessionConnection
    if (session.kind === 'error') {
      setTest({ phase: 'error', error: session.error })
      return null
    }

    const tokenClient = new ReaderClient({
      baseURL: trimmedURL,
      installationToken: trimmedToken,
    })
    const res = await tokenClient.testConnection()
    if (!res.ok) {
      setTest({ phase: 'error', error: res.error })
      return null
    }
    const tokenConnection: Connection = {
      baseURL: trimmedURL,
      mode: 'installation-token',
      installationToken: trimmedToken,
    }
    if (persist) {
      try {
        await onSaved(tokenConnection)
      } catch (error) {
        setTest({ phase: 'error', error: persistenceError(error) })
        return null
      }
    }
    return tokenConnection
  }

  async function runTest(): Promise<void> {
    if (!canSubmit) return
    setTest({ phase: 'testing' })
    const conn = await connect(false)
    if (conn) {
      setTest({ phase: 'ok' })
    }
  }

  async function handleSave(): Promise<void> {
    if (!canSubmit) return
    setTest({ phase: 'testing' })
    const conn = await connect(true)
    if (conn) setTest({ phase: 'ok' })
  }

  return (
    <div style={styles.overlay}>
      <div style={styles.card}>
        <div style={styles.head}>
          <span style={styles.brandIcon}>
            <Icon name="stack" size={20} />
          </span>
          <div>
            <div style={styles.title}>连接到 Cairn 后端</div>
            <div style={styles.sub}>填入后端地址与安装令牌，开始阅读你的链接库</div>
          </div>
        </div>

        <label style={styles.field}>
          <span style={styles.label}>后端地址</span>
          <input
            style={styles.input}
            type="url"
            placeholder="http://localhost:8080"
            value={baseURL}
            onChange={(e) => {
              setBaseURL(e.target.value)
              setTest({ phase: 'idle' })
            }}
            autoFocus
          />
        </label>

        <label style={styles.field}>
          <span style={styles.label}>安装令牌</span>
          <input
            style={styles.input}
            type="password"
            placeholder="留空则不带鉴权头"
            value={installationToken}
            onChange={(e) => {
              setInstallationToken(e.target.value)
              setTest({ phase: 'idle' })
            }}
          />
        </label>

        {test.phase === 'ok' && (
          <div style={{ ...styles.result, ...styles.resultOk }}>
            <Icon name="check" size={15} sw={2} />
            连接成功
          </div>
        )}
        {test.phase === 'error' && (
          <div style={{ ...styles.result, ...styles.resultErr }}>
            <Icon name="alert" size={15} sw={2} />
            {errorText(test.error)}
          </div>
        )}

        <div style={styles.actions}>
          {/*
            重置应用缓存：Service Worker 出问题时用户唯一的自救手段。
            它必须待在这里而不是某个隐藏入口——一个坏版本的 SW 会把用户钉死在
            旧代码上，而修复本身也要靠那个 SW 放行才能送达。
          */}
          <button
            type="button"
            className="ne-btn ghost"
            style={styles.btn}
            disabled={resetting}
            onClick={() => {
              if (!window.confirm('重置应用缓存会清空本地已缓存的列表与原文，并注销离线支持。已保存的划线笔记不受影响。继续吗？')) {
                return
              }
              setResetting(true)
              void resetApplicationCache().finally(() => {
                window.location.reload()
              })
            }}
          >
            {resetting ? '重置中…' : '重置应用缓存'}
          </button>
          {onCancel && (
            <button
              type="button"
              className="ne-btn ghost"
              style={styles.btn}
              onClick={onCancel}
            >
              取消
            </button>
          )}
          <button
            type="button"
            className="ne-btn ghost"
            style={styles.btn}
            disabled={!canSubmit || test.phase === 'testing'}
            onClick={runTest}
          >
            {test.phase === 'testing' ? (
              <Icon name="loader" size={14} sw={2} style={{ animation: 'spin 0.9s linear infinite' }} />
            ) : (
              <Icon name="refresh" size={14} />
            )}
            测试连接
          </button>
          <button
            type="button"
            className="ne-btn primary"
            style={styles.btn}
            disabled={!canSubmit || test.phase === 'testing'}
            onClick={() => void handleSave()}
          >
            保存并进入
          </button>
        </div>
      </div>
    </div>
  )
}

// 内联样式复用设计 tokens（CSS 变量），不另开类名，保持视觉与设计统一。
const styles: Record<string, CSSProperties> = {
  overlay: {
    position: 'fixed',
    inset: 0,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    background:
      'radial-gradient(1200px 800px at 20% -10%, rgba(217,119,87,0.06), transparent 60%), var(--bg-base)',
  },
  card: {
    width: 420,
    maxWidth: '90%',
    background: 'var(--bg-elevated)',
    border: '0.5px solid var(--border-strong)',
    borderRadius: 'var(--r-lg)',
    boxShadow: 'var(--shadow-pop)',
    padding: '24px 26px 22px',
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  head: { display: 'flex', alignItems: 'flex-start', gap: 12, marginBottom: 4 },
  brandIcon: {
    width: 38,
    height: 38,
    borderRadius: 'var(--r-md)',
    background: 'var(--accent-soft)',
    color: 'var(--accent)',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    flexShrink: 0,
  },
  title: { fontSize: 17, fontWeight: 680, color: 'var(--text-primary)' },
  sub: { fontSize: 12.5, color: 'var(--text-tertiary)', marginTop: 3, lineHeight: 1.5 },
  field: { display: 'flex', flexDirection: 'column', gap: 6 },
  label: {
    fontFamily: 'var(--font-mono)',
    fontSize: 10.5,
    fontWeight: 600,
    letterSpacing: '0.04em',
    textTransform: 'uppercase',
    color: 'var(--text-tertiary)',
  },
  input: {
    height: 38,
    padding: '0 12px',
    borderRadius: 'var(--r-md)',
    border: '0.5px solid var(--border-strong)',
    background: 'var(--bg-reader)',
    color: 'var(--text-primary)',
    fontFamily: 'var(--font-ui)',
    fontSize: 14,
    outline: 'none',
  },
  result: {
    display: 'flex',
    alignItems: 'center',
    gap: 7,
    fontSize: 12.5,
    padding: '8px 11px',
    borderRadius: 'var(--r-sm)',
  },
  resultOk: { background: 'var(--ok-soft)', color: 'var(--ok)' },
  resultErr: { background: 'var(--err-soft)', color: 'var(--err)' },
  actions: { display: 'flex', alignItems: 'center', gap: 8, marginTop: 4, justifyContent: 'flex-end' },
  btn: { display: 'inline-flex', alignItems: 'center', gap: 6 },
}
