/**
 * Reader 应用入口。
 *
 * 路由：无连接配置 / 编辑中 → ConnectionSetup；有配置 → MainView（三栏主界面）。
 * 客户端按当前连接配置构造，配置变化时重建。
 *
 * 首跑的同源自举：Reader 由 Go 后端 serve 在 /reader/ 时，后端地址就是当前
 * origin，没有理由让用户手抄一遍。挂载后探测一次（lib/bootstrap.ts）：
 *   · 探到同源后端且无需鉴权 → 直接落配置进主界面，引导页整页跳过
 *   · 探到同源后端但需要令牌 → 引导页照出，地址已填好
 *   · 没探到（独立部署 / 纯静态托管）→ 与改造前完全一致的空白引导页
 * 探测期间渲染 null 而不是引导页：先闪一下"请填地址"再自动跳走，比多等
 * 几十毫秒更糟。
 */
import { useEffect, useRef, useState } from 'react'
import { ConnectionSetup } from './components/ConnectionSetup'
import { MainView } from './components/MainView'
import { IdentityBoundReaderClient, ReaderClient } from './lib/api/client'
import { probeSameOriginBackend } from './lib/bootstrap'
import { bootstrapCache, stopCachePersistence } from './lib/cache/bootstrap'
import { resourceStore } from './lib/cache/store'
import {
  derivePhysicalNamespace,
  type IdentityLease,
  readerIdentity,
} from './lib/identity'
import {
  getLegacyImportPrompt,
  importLegacyData,
  keepLegacyDataIsolated,
  quarantineLegacyData,
  type LegacyImportPrompt,
} from './lib/legacy-user-data'
import {
  completeLegacySessionUpgrade,
  confirmLegacyBearerCompatibility,
  initializeConnection,
  needsLegacySessionUpgrade,
  useConnection,
} from './lib/settings'
import { negotiateLegacySessionUpgrade } from './lib/legacy-session-upgrade'
import type { CapabilitiesResponse } from './lib/api/types'
import type { ApiError } from './lib/api/result'

type Probe =
  | { phase: 'pending' }
  | { phase: 'done'; detectedBaseURL?: string }

type IdentityGate =
  | { phase: 'idle' }
  | { phase: 'pending'; connectionKey: string }
  | { phase: 'error'; connectionKey: string; error: ApiError }
  | {
      phase: 'legacy'
      connectionKey: string
      client: IdentityBoundReaderClient
      prompt: LegacyImportPrompt
      busy: boolean
      message?: string
      capabilities: CapabilitiesResponse
  }
  | {
      phase: 'ready'
      connectionKey: string
      client: IdentityBoundReaderClient
      capabilities: CapabilitiesResponse
    }

const READER_CAPABILITIES_OFF: CapabilitiesResponse = {
  library_kinds: false,
  site_library: false,
  site_management: false,
  site_advanced_management: false,
  archive_versions: [],
  reader_vnext: false,
  reader: {
    annotations: false,
    notes: false,
    inbox: false,
    todos: false,
    engagement: false,
    home: false,
    feed: false,
    ai: false,
    related_tags: false,
    activity: false,
    history: false,
    trash: false,
  },
}

export default function App() {
  const [conn, save, , connectionStorage] = useConnection()
  const [editing, setEditing] = useState(false)
  const configured = conn.baseURL.trim() !== ''
  const connectionKey = JSON.stringify([
    conn.baseURL,
    conn.mode,
    conn.installationToken,
  ])
  const [identityAttempt, setIdentityAttempt] = useState(0)
  const [identity, setIdentity] = useState<IdentityGate>(() =>
    configured ? { phase: 'pending', connectionKey } : { phase: 'idle' },
  )
  const capabilityRefreshRef = useRef<() => void>(() => undefined)
  // 已有配置就没什么可探的，直接标记完成，避免一次无谓请求。
  const [probe, setProbe] = useState<Probe>(() =>
    configured ? { phase: 'done' } : { phase: 'pending' },
  )

  useEffect(() => {
    if (connectionStorage.phase !== 'ready' || probe.phase === 'done') return
    if (configured) {
      setProbe({ phase: 'done' })
      return
    }
    let cancelled = false
    void probeSameOriginBackend().then(async (result) => {
      if (cancelled) return
      if (result.baseURL && result.openAccess) {
        // 免鉴权后端没有凭证可换，直接以空安装令牌进入。
        try {
          await save({
            baseURL: result.baseURL,
            mode: 'installation-token',
            installationToken: '',
          })
        } catch {
          return
        }
        if (cancelled) return
        setProbe({ phase: 'done' })
        return
      }
      setProbe({ phase: 'done', detectedBaseURL: result.baseURL ?? undefined })
    })
    return () => {
      cancelled = true
    }
  }, [configured, connectionStorage.phase, probe.phase, save])

  useEffect(() => {
    if (connectionStorage.phase !== 'ready' || !configured || editing) {
      readerIdentity.clear()
      stopCachePersistence()
      resourceStore.deactivateIdentity()
      setIdentity({ phase: 'idle' })
      return
    }

    let cancelled = false
    let installedLease: IdentityLease | null = null
    const handshakeController = new AbortController()
    setIdentity({ phase: 'pending', connectionKey })
    const temporaryClient = new ReaderClient({
      baseURL: conn.baseURL,
      installationToken: conn.installationToken,
      session: conn.mode === 'session',
    })

    void temporaryClient.getIdentity(handshakeController.signal).then(async (result) => {
      if (cancelled) return
      if (!result.ok) {
        setIdentity({ phase: 'error', connectionKey, error: result.error })
        return
      }

      if (needsLegacySessionUpgrade(conn)) {
        const upgrade = await negotiateLegacySessionUpgrade(
          conn,
          result.data,
          {
            signal: handshakeController.signal,
            commitSession: () => completeLegacySessionUpgrade(conn),
          },
        )
        if (cancelled) return
        if (upgrade.kind === 'error') {
          setIdentity({ phase: 'error', connectionKey, error: upgrade.error })
          return
        }
        if (upgrade.kind === 'session') {
          // The durable revision was committed while the session mutation lock
          // was still held. The published connection restarts this handshake.
          return
        }
        try {
          if (!await confirmLegacyBearerCompatibility(conn)) return
        } catch (error) {
          if (!cancelled) {
            setIdentity({
              phase: 'error',
              connectionKey,
              error: {
                kind: 'other',
                message: error instanceof Error ? error.message : String(error),
              },
            })
          }
          return
        }
      }

      let physicalNamespace: string
      try {
        physicalNamespace = await derivePhysicalNamespace(
          conn.baseURL,
          result.data.client_data_namespace,
        )
      } catch (error) {
        if (!cancelled) {
          setIdentity({
            phase: 'error',
            connectionKey,
            error: {
              kind: 'other',
              message: error instanceof Error ? error.message : String(error),
            },
          })
        }
        return
      }
      if (cancelled) return

      const lease = readerIdentity.install({
        serverClientDataNamespace: result.data.client_data_namespace,
        physicalNamespace,
      })
      installedLease = lease
      resourceStore.activateIdentity(lease)
      await bootstrapCache(lease)
      const gateOwnership = lease.capture('private Reader mount')
      if (cancelled || !lease.isCurrent(gateOwnership)) return
      const client = new IdentityBoundReaderClient({
        baseURL: conn.baseURL,
        installationToken: conn.installationToken,
        session: conn.mode === 'session',
        identity: lease,
        onIdentityMismatch: () => {
          lease.revoke()
          stopCachePersistence()
          resourceStore.deactivateIdentity(lease)
          setIdentity({
            phase: 'pending',
            connectionKey,
          })
          setIdentityAttempt((attempt) => attempt + 1)
        },
      })
      // The identity handshake is intentionally separate from feature
      // discovery. Older self-hosted servers can still serve the original
      // reading surface; missing or legacy capability responses must not make
      // the new routes look available.
      const capabilityResult = await client.getCapabilities()
      const capabilities = capabilityResult.ok ? capabilityResult.data : READER_CAPABILITIES_OFF
      await quarantineLegacyData()
      const prompt = await getLegacyImportPrompt(lease)
      if (cancelled || !lease.isCurrent(gateOwnership)) return
      if (prompt) {
        setIdentity({
          phase: 'legacy',
          connectionKey,
          client,
          prompt,
          busy: false,
          capabilities,
        })
      } else {
        setIdentity({ phase: 'ready', connectionKey, client, capabilities })
      }
    })

    return () => {
      cancelled = true
      handshakeController.abort()
      installedLease?.revoke()
      stopCachePersistence()
      resourceStore.deactivateIdentity(installedLease ?? undefined)
      if (readerIdentity.activeLease === installedLease) readerIdentity.clear()
    }
  }, [
    configured,
    connectionStorage.phase,
    conn,
    conn.baseURL,
    conn.mode,
    conn.installationToken,
    connectionKey,
    editing,
    identityAttempt,
  ])

  const capabilityClient = identity.phase === 'ready' ? identity.client : null
  useEffect(() => {
    if (!capabilityClient) {
      capabilityRefreshRef.current = () => undefined
      return
    }

    let disposed = false
    let generation = 0
    let activeController: AbortController | null = null
    const refresh = () => {
      if (disposed || activeController || !capabilityClient.isIdentityCurrent()) return
      if (typeof navigator !== 'undefined' && navigator.onLine === false) return
      const controller = new AbortController()
      const requestedGeneration = ++generation
      activeController = controller
      void capabilityClient.getCapabilities(controller.signal).then((result) => {
        if (
          disposed ||
          controller.signal.aborted ||
          requestedGeneration !== generation ||
          !capabilityClient.isIdentityCurrent()
        ) return
        const capabilities = result.ok ? result.data : READER_CAPABILITIES_OFF
        setIdentity((current) => current.phase === 'ready' && current.client === capabilityClient
          ? { ...current, capabilities }
          : current)
      }).finally(() => {
        if (activeController === controller) activeController = null
      })
    }
    capabilityRefreshRef.current = refresh
    const refreshVisible = () => {
      if (document.visibilityState === 'visible') refresh()
    }
    window.addEventListener('focus', refreshVisible)
    window.addEventListener('online', refresh)
    document.addEventListener('visibilitychange', refreshVisible)
    return () => {
      disposed = true
      generation += 1
      activeController?.abort()
      activeController = null
      if (capabilityRefreshRef.current === refresh) {
        capabilityRefreshRef.current = () => undefined
      }
      window.removeEventListener('focus', refreshVisible)
      window.removeEventListener('online', refresh)
      document.removeEventListener('visibilitychange', refreshVisible)
    }
  }, [capabilityClient])

  if (connectionStorage.phase === 'loading') return null
  if (connectionStorage.phase === 'error') {
    return (
      <div role="alert">
        <p>无法安全读取或保存连接：{connectionStorage.message}</p>
        <button type="button" onClick={() => void initializeConnection()}>
          重试
        </button>
      </div>
    )
  }

  if (!configured && probe.phase === 'pending') return null

  const reconnecting = identity.phase === 'error' && identity.error.kind === 'unauthorized'
  if (!configured || editing || reconnecting) {
    return (
      <ConnectionSetup
        detectedBaseURL={probe.phase === 'done' ? probe.detectedBaseURL : undefined}
        onSaved={async (c) => {
          await save(c)
          setEditing(false)
        }}
        onCancel={configured && !reconnecting ? () => setEditing(false) : undefined}
      />
    )
  }

  if (
    identity.phase === 'idle' ||
    identity.phase === 'pending' ||
    identity.connectionKey !== connectionKey
  ) {
    return null
  }
  if (identity.phase === 'error') {
    return (
      <div role="alert">
        <p>无法确认当前身份：{identity.error.message}</p>
        <button type="button" onClick={() => setIdentityAttempt((attempt) => attempt + 1)}>
          重试
        </button>
        <button type="button" onClick={() => setEditing(true)}>
          连接设置
        </button>
      </div>
    )
  }

  if (identity.phase === 'legacy') {
    const choose = (action: 'keep' | 'import') => {
      if (identity.busy) return
      const gate = identity
      setIdentity({ ...gate, busy: true, message: undefined })
      void (async () => {
        let accepted: boolean
        if (action === 'keep') {
          accepted = await keepLegacyDataIsolated(gate.client.identityLease)
        } else {
          const result = await importLegacyData(gate.client.identityLease)
          accepted = result.status === 'imported' ||
            result.status === 'already-resolved' ||
            result.status === 'no-data'
        }
        const ownership = gate.client.identityLease.capture('complete legacy identity gate')
        if (!gate.client.identityLease.isCurrent(ownership)) return
        setIdentity((current) => {
          if (current.phase !== 'legacy' || current.client !== gate.client) return current
          if (!accepted) {
            return {
              ...current,
              busy: false,
              message: '无法保存这次选择，本地用户数据仍保持隔离。',
            }
          }
          return {
            phase: 'ready',
            connectionKey: gate.connectionKey,
            client: gate.client,
            capabilities: gate.capabilities,
          }
        })
      })()
    }

    return (
      <div className="rss-dialog-overlay" role="presentation">
        <section
          className="rss-dialog compact"
          role="dialog"
          aria-modal="true"
          aria-labelledby="legacy-user-data-title"
          aria-label="未归属的本地数据"
        >
          <header>
            <div>
              <h2 id="legacy-user-data-title">未归属的本地数据</h2>
              <p>发现 {identity.prompt.assetCount} 组旧版划线或钉选数据</p>
            </div>
          </header>
          <div className="rss-dialog-body legacy-user-data-body">
            <div className="legacy-import-target">
              <span>目标连接</span>
              <strong>{conn.baseURL}</strong>
              <code>{identity.client.identityLease.context.serverClientDataNamespace}</code>
            </div>
            {identity.message && <div className="rss-dialog-error" role="alert">{identity.message}</div>}
            <footer className="legacy-migration-actions">
              <button
                type="button"
                className="ne-btn ghost"
                disabled={identity.busy}
                onClick={() => choose('keep')}
              >
                保持隔离
              </button>
              <button
                type="button"
                className="ne-btn primary"
                disabled={identity.busy}
                onClick={() => choose('import')}
              >
                {identity.busy ? '处理中' : '导入当前身份'}
              </button>
            </footer>
          </div>
        </section>
      </div>
    )
  }

  return (
    <MainView
      client={identity.client}
      capabilities={identity.capabilities}
      onRefreshCapabilities={() => capabilityRefreshRef.current()}
      onOpenSettings={() => setEditing(true)}
    />
  )
}
