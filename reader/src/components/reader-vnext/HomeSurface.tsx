import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import {
  readerRouteIsAvailable,
  type ReaderCapabilityLease,
} from '../../lib/capabilities'
import type { ReaderFeedAction, ReaderHomeResponse, ReaderTodoResponse } from '../../lib/api/types'
import type { IdentityLease } from '../../lib/identity'
import {
  cacheServerThoughtPage,
  selectThoughtReadModel,
  type ThoughtReadModelItem,
} from '../../lib/user-data/thought-sync'
import {
  readerThoughtHostTarget,
  type ReaderNavigationRequest,
  type ReaderRoute,
  type ReaderRouteTargets,
} from '../../lib/navigation/route'
import { Icon } from '../Icon'
import {
  SurfaceError,
  SurfaceLoading,
  SurfaceShell,
  SURFACE_IDENTITY_ERROR,
  errorMessage,
  formatRelativeDate,
  identityIsCurrent,
  isIdentityError,
  todoDesiredStatePatch,
} from './SurfaceShell'

export interface HomeSurfaceProps {
  readonly client: IdentityBoundReaderClient
  /** Optional only for the unauthenticated component harness; mounted Reader passes a lease. */
  readonly lease?: IdentityLease
  readonly onNavigate: ReaderNavigationRequest
  readonly onOpenLink: (id: string) => void
  readonly capabilityLease: ReaderCapabilityLease
  /** 内嵌的 Feed 流，由 MainView 注入；省略时「今天」只有概览与最近记录。 */
  readonly feedSlot?: ReactNode
  /** 与内嵌流共用的滚动容器——RF57 的位置恢复认的就是这个元素。 */
  readonly scrollRef?: RefObject<HTMLDivElement>
}

function countFor(counts: Record<string, number>, ...keys: string[]): number | null {
  for (const key of keys) {
    const value = counts[key]
    if (typeof value === 'number' && Number.isFinite(value)) return value
  }
  return null
}

const TODO_CHANGE_EVENT = 'webtag:todos-change'
const HOME_CHANGE_EVENT = 'webtag:home-change'

type HomeStatusPayload = ReaderHomeResponse & {
  readonly freshness?: unknown
  readonly partial?: unknown
}

type HomeStatus = {
  readonly freshness: string | null
  readonly partial: boolean
  readonly stale: boolean
  readonly label: string
  readonly attention: boolean
}

type HomeStatusOptions = {
  readonly loading?: boolean
  readonly refreshFailed?: boolean
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function statusToken(value: unknown): string | null {
  if (typeof value === 'string') {
    const token = value.trim().toLowerCase()
    return token || null
  }
  if (!isRecord(value)) return null
  for (const key of ['status', 'state', 'kind', 'value']) {
    const token = statusToken(value[key])
    if (token) return token
  }
  return null
}

function isPartialValue(value: unknown): boolean {
  if (typeof value === 'boolean') return value
  if (typeof value === 'string') return ['partial', 'degraded', 'incomplete', 'stale'].includes(value.trim().toLowerCase())
  if (Array.isArray(value)) return value.length > 0
  if (!isRecord(value)) return false
  if (typeof value.partial === 'boolean') return value.partial
  if (Array.isArray(value.sections)) return value.sections.length > 0
  return Object.values(value).some(isPartialValue)
}

function homeStatus(response: ReaderHomeResponse, options: HomeStatusOptions = {}): HomeStatus {
  const payload = response as HomeStatusPayload
  const freshness = statusToken(payload.freshness)
  const partial = isPartialValue(payload.partial) || freshness === 'partial' || freshness === 'degraded' || freshness === 'incomplete'
  const stale = response.stale || freshness === 'stale' || options.refreshFailed === true

  if (options.loading) return { freshness: freshness ?? 'refreshing', partial, stale, label: partial ? '部分数据更新中' : '数据更新中', attention: true }
  if (partial && stale) return { freshness, partial, stale, label: '部分数据较旧', attention: true }
  if (partial) return { freshness, partial, stale, label: '部分数据可用', attention: true }
  if (stale) return { freshness, partial, stale, label: '数据较旧', attention: true }
  if (freshness === 'refreshing' || freshness === 'pending') return { freshness, partial, stale, label: '数据更新中', attention: true }
  if (freshness === null || freshness === 'unknown' || freshness === 'unavailable') return { freshness, partial, stale, label: '数据新鲜度未知', attention: true }
  return { freshness, partial, stale, label: '数据已同步', attention: false }
}

function thrownErrorMessage(value: unknown): string {
  if (!isRecord(value)) return '请求失败，请稍后重试。'
  return errorMessage({
    message: typeof value.message === 'string' ? value.message : '',
    status: typeof value.status === 'number' ? value.status : undefined,
  })
}

function supportsItemAction(item: ReaderHomeResponse['continue_reading'][number], action: ReaderFeedAction): boolean {
  return Array.isArray(item.actions) && item.actions.includes(action)
}

function TodoRow({
  todo,
  onToggle,
  disabled,
}: {
  readonly todo: ReaderTodoResponse
  readonly onToggle: (todo: ReaderTodoResponse) => void
  readonly disabled?: boolean
}) {
  return (
    <li className={'rvx-todo-row' + (todo.done ? ' done' : '')}>
      <button
        className="rvx-check-button"
        type="button"
        aria-label={todo.done ? `重新打开 ${todo.text}` : `完成 ${todo.text}`}
        aria-pressed={todo.done}
        disabled={disabled}
        onClick={() => onToggle(todo)}
      >
        <Icon name="check" size={15} />
      </button>
      <div className="rvx-todo-content">
        <span className="rvx-todo-text">{todo.text}</span>
        {todo.due_at && <small className={todo.expired && !todo.done ? 'rvx-expired' : undefined}>{todo.expired && !todo.done ? '已过期 · ' : '截止 · '}{formatRelativeDate(todo.due_at)}</small>}
      </div>
    </li>
  )
}

export function HomeSurface({ client, lease, onNavigate, onOpenLink, capabilityLease, feedSlot, scrollRef }: HomeSurfaceProps) {
  const [home, setHome] = useState<ReaderHomeResponse | null>(null)
  const [recentThoughts, setRecentThoughts] = useState<readonly ThoughtReadModelItem[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const ownScrollRef = useRef<HTMLDivElement>(null)
  const shellScrollRef = scrollRef ?? ownScrollRef
  const requestID = useRef(0)
  const thoughtProjectionRequestID = useRef(0)
  const mountedRef = useRef(false)
  const todoBusyID = useRef<string | null>(null)
  const [busyTodoID, setBusyTodoID] = useState<string | null>(null)

  // A new identity must not paint the old namespace while its initial request
  // is being negotiated.  The following effect runs before the browser paint.
  useLayoutEffect(() => {
    requestID.current += 1
    thoughtProjectionRequestID.current += 1
    setHome(null)
    setRecentThoughts(null)
    setError(null)
    setLoading(true)
  }, [lease])

  const refreshLocalThoughts = useCallback(async () => {
    if (!lease?.context?.physicalNamespace || !identityIsCurrent(client)) return
    const request = requestID.current
    const projectionRequest = ++thoughtProjectionRequestID.current
    const selected = await selectThoughtReadModel(lease)
    if (
      !selected.ok ||
      request !== requestID.current ||
      projectionRequest !== thoughtProjectionRequestID.current ||
      !mountedRef.current ||
      !identityIsCurrent(client)
    ) return
    setRecentThoughts(selected.value)
  }, [client, lease])

  const clearForIdentityLoss = useCallback(() => {
    if (!mountedRef.current) return
    todoBusyID.current = null
    setBusyTodoID(null)
    setHome(null)
    setRecentThoughts(null)
    setError(SURFACE_IDENTITY_ERROR)
    setLoading(false)
  }, [])

  const load = useCallback(async () => {
    if (!mountedRef.current) return
    const request = ++requestID.current
    if (!capabilityLease.isCurrent('home')) {
      setHome(null)
      setLoading(false)
      setError(null)
      return
    }
    setLoading(true)
    setError(null)
    if (!identityIsCurrent(client)) {
      clearForIdentityLoss()
      return
    }
    try {
      const result = await client.getHome()
      if (request !== requestID.current || !mountedRef.current || !capabilityLease.isCurrent('home')) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (result.ok) {
        let localRecentThoughts: readonly ThoughtReadModelItem[] | null = null
        const projectionRequest = ++thoughtProjectionRequestID.current
        if (lease?.context?.physicalNamespace) {
          const cached = await cacheServerThoughtPage(lease, result.data.recent_thoughts)
          if (
            request !== requestID.current ||
            projectionRequest !== thoughtProjectionRequestID.current ||
            !mountedRef.current ||
            !identityIsCurrent(client)
          ) return
          if (!cached.ok) {
            // The aggregate API deliberately permits legacy targets that are
            // too weak for the durable sync model. Keep storage fail-closed,
            // but retain the already validated server aggregate for display.
            setHome(result.data)
            setRecentThoughts(null)
            setError('无法保存想法本地读模型。')
            return
          }
          const selected = await selectThoughtReadModel(lease)
          if (
            request !== requestID.current ||
            projectionRequest !== thoughtProjectionRequestID.current ||
            !mountedRef.current ||
            !identityIsCurrent(client)
          ) return
          if (selected.ok) localRecentThoughts = selected.value
        }
        setHome(result.data)
        setRecentThoughts(localRecentThoughts)
      }
      else if (isIdentityError(result.error)) clearForIdentityLoss()
      else setError(errorMessage(result.error))
    } catch (cause) {
      if (request !== requestID.current || !mountedRef.current || !capabilityLease.isCurrent('home')) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      if (request === requestID.current && mountedRef.current && identityIsCurrent(client) && capabilityLease.isCurrent('home')) setLoading(false)
    }
  }, [capabilityLease, clearForIdentityLoss, client, lease])

  useEffect(() => {
    mountedRef.current = true
    const onSharedStateChange = () => { void load() }
    const onThoughtChange = () => { void refreshLocalThoughts() }
    window.addEventListener(TODO_CHANGE_EVENT, onSharedStateChange)
    window.addEventListener(HOME_CHANGE_EVENT, onSharedStateChange)
    window.addEventListener('webtag:annotations-change', onThoughtChange)
    window.addEventListener('webtag:thoughts-sync', onThoughtChange)
    void load()
    // A durable offline thought can predate this surface and its event listener.
    void refreshLocalThoughts()
    return () => {
      mountedRef.current = false
      requestID.current += 1
      thoughtProjectionRequestID.current += 1
      todoBusyID.current = null
      window.removeEventListener(TODO_CHANGE_EVENT, onSharedStateChange)
      window.removeEventListener(HOME_CHANGE_EVENT, onSharedStateChange)
      window.removeEventListener('webtag:annotations-change', onThoughtChange)
      window.removeEventListener('webtag:thoughts-sync', onThoughtChange)
    }
  }, [load, refreshLocalThoughts])

  const toggleTodo = useCallback(async (todo: ReaderTodoResponse) => {
    if (!mountedRef.current || !capabilityLease.isCurrent('todos') || todoBusyID.current !== null) return
    todoBusyID.current = todo.id
    setBusyTodoID(todo.id)
    const patch = todoDesiredStatePatch(todo, !todo.done)
    if (!patch) {
      setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
      todoBusyID.current = null
      setBusyTodoID(null)
      return
    }
    try {
      if (!identityIsCurrent(client) || !capabilityLease.isCurrent('todos')) {
        clearForIdentityLoss()
        return
      }
      const result = await client.patchTodo(todo.id, patch)
      if (!mountedRef.current || !capabilityLease.isCurrent('todos')) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
        return
      }
      // Home is an aggregate, not a second TODO store. Re-read the aggregate so
      // its counts and preview rows advance together with the authoritative item.
      window.dispatchEvent(new Event(TODO_CHANGE_EVENT))
    } catch (cause) {
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      if (mountedRef.current) {
        todoBusyID.current = null
        setBusyTodoID(null)
      }
    }
  }, [capabilityLease, clearForIdentityLoss, client])

  const goLibrary = (id: 'pending' | 'reading' | 'sites' | 'subs' | 'notes') =>
    onNavigate({ kind: 'library', id })

  const navigate = useCallback(
    (route: ReaderRoute, targets?: ReaderRouteTargets) => onNavigate(route, targets),
    [onNavigate],
  )
  const openThought = useCallback((thought: ThoughtReadModelItem) => {
    const target = readerThoughtHostTarget(thought)
    if (
      target &&
      capabilityLease.isCurrent() &&
      readerRouteIsAvailable(target.route, capabilityLease.policy)
    ) {
      navigate(target.route, target.targets)
    }
  }, [capabilityLease, navigate])
  const status = home ? homeStatus(home, { loading, refreshFailed: error !== null }) : null
  const policy = capabilityLease.policy
  const openTodos = policy.todos ? home?.todos.filter((todo) => !todo.done) ?? [] : []
  const libraryCounts = ([
    ['pending', '收件箱', countFor(home?.counts ?? {}, 'pending', 'pending_count', 'inbox', 'inbox_count'), policy.inbox],
    ['reading', '阅读', countFor(home?.counts ?? {}, 'reading', 'reading_count', 'links', 'library', 'saved', 'saved_links', 'link_count'), true],
    ['subs', '订阅', countFor(home?.counts ?? {}, 'subs', 'subs_count', 'subscriptions', 'subscriptions_count', 'subscription', 'subscription_count'), true],
    ['notes', '笔记', countFor(home?.counts ?? {}, 'notes', 'note', 'notes_count'), policy.notes],
  ] as const).filter((mode) => mode[3])
  const visibleRecentThoughts = recentThoughts ?? home?.recent_thoughts ?? []
  const canRenderLocalAggregate = recentThoughts !== null

  return (
    <SurfaceShell
      title="今天"
      subtitle={home ? `${home.today} · ${home.summary}` : '你的阅读工作台'}
      activeRoute={{ kind: 'surface', id: 'home' }}
      onNavigate={onNavigate}
      capabilityPolicy={policy}
      scrollRef={shellScrollRef}
      actions={policy.feed ? (
        <button className="rvx-button secondary" type="button" onClick={() => navigate({ kind: 'surface', id: 'feed' })}>
          <Icon name="layers" size={15} /> 完整 Feed
        </button>
      ) : undefined}
    >
      {loading && !home && !canRenderLocalAggregate ? <SurfaceLoading /> : error && !home && !canRenderLocalAggregate ? <SurfaceError message={error} onRetry={() => load()} /> : (
        <>
          {error && <div className="rvx-inline-error" role="alert">{error}</div>}
          {home && (
            <>
              <section className="rvx-hero-row" aria-label="今日概览">
                <div>
                  <span className="rvx-eyebrow">今日概览</span>
                  <h2>{home.summary || '从最近的位置继续。'}</h2>
                  <p className="rvx-muted">只显示当前身份可证明的数据；服务端标记为过期时会保留提示。</p>
                </div>
                <div
                  className={'rvx-freshness' + (status?.attention ? ' stale' : '') + (status?.partial ? ' partial' : '')}
                  data-freshness={status?.freshness ?? 'unknown'}
                  aria-label={status?.label}
                >
                  <Icon name={status?.attention ? 'clock' : 'check'} size={16} />
                  {status?.label}
                </div>
              </section>

              <section className="rvx-count-strip" aria-label="内容库">
                {libraryCounts.map(([id, label, count]) => (
                  <button key={id} className="rvx-count-chip" type="button" onClick={() => goLibrary(id)}>
                    <span>{label}</span>
                    <strong>{count === null ? '—' : count}</strong>
                  </button>
                ))}
              </section>

              <div className="rvx-home-columns">
                <section className="rvx-section" aria-labelledby="home-continue">
                  <div className="rvx-section-head">
                    <div><span className="rvx-eyebrow">继续阅读</span><h2 id="home-continue">最近打开</h2></div>
                    <button className="rvx-link-button" type="button" onClick={() => goLibrary('reading')}>全部阅读</button>
                  </div>
                  {home.continue_reading.length === 0 ? <p className="rvx-muted">还没有可继续的内容。</p> : (
                    <ul className="rvx-item-list">
                      {home.continue_reading.slice(0, 3).map((item) => (
                        <li key={item.key} className="rvx-feed-row">
                          <div className="rvx-row-main">
                            <strong>{item.title || '未命名内容'}</strong>
                            <span>{item.summary || item.reason_text}</span>
                            <small>{item.source} · {formatRelativeDate(item.event_at)}</small>
                          </div>
                          {item.link_id && supportsItemAction(item, 'open_workspace') ? (
                            <button className="rvx-icon-button" type="button" aria-label="打开阅读" title="打开阅读" onClick={() => onOpenLink(item.link_id as string)}><Icon name="arrowright" size={15} /></button>
                          ) : policy.inbox && item.inbox_id && supportsItemAction(item, 'open') ? (
                            <button className="rvx-icon-button" type="button" aria-label="打开收件箱" title="打开收件箱" onClick={() => navigate({ kind: 'library', id: 'pending', inboxId: item.inbox_id as string })}><Icon name="arrowright" size={15} /></button>
                          ) : null}
                        </li>
                      ))}
                    </ul>
                  )}
                </section>

                {policy.todos && <section className="rvx-section" aria-labelledby="home-todos">
                  <div className="rvx-section-head">
                    <div><span className="rvx-eyebrow">全局任务</span><h2 id="home-todos">TODO</h2></div>
                    <button className="rvx-link-button" type="button" onClick={() => navigate({ kind: 'tool', id: 'todo' })}>查看全部</button>
                  </div>
                  {openTodos.length === 0 ? <p className="rvx-muted">没有未完成任务。</p> : (
                    <ul className="rvx-todo-list">
                      {openTodos.slice(0, 3).map((todo) => <TodoRow key={todo.id} todo={todo} onToggle={toggleTodo} disabled={busyTodoID !== null} />)}
                    </ul>
                  )}
                </section>}
              </div>

              {policy.feed && feedSlot}
            </>
          )}

          {policy.annotations && policy.history && (home || canRenderLocalAggregate) && <section className="rvx-section" aria-labelledby="home-thoughts">
            <div className="rvx-section-head">
              <div><span className="rvx-eyebrow">最近记录</span><h2 id="home-thoughts">最近想法</h2></div>
              <button className="rvx-link-button" type="button" onClick={() => navigate({ kind: 'tool', id: 'history' }, { thoughtView: 'live' })}>查看全部想法</button>
            </div>
            {visibleRecentThoughts.length === 0 ? <p className="rvx-muted">还没有同步的想法。</p> : (
              <ul className="rvx-thought-list">
                {visibleRecentThoughts.slice(0, 5).map((thought) => {
                  const target = readerThoughtHostTarget(thought)
                  const targetAvailable = target !== null && readerRouteIsAvailable(target.route, policy)
                  return <li key={thought.id}>
                    <span className="rvx-thought-mark"><Icon name="marker" size={14} /></span>
                    <div><p>{thought.body || '空想法'}</p><small>{formatRelativeDate(thought.updated_at)}</small></div>
                    {targetAvailable && <button className="rvx-link-button" type="button" onClick={() => openThought(thought)}>回到来源</button>}
                  </li>
                })}
              </ul>
            )}
          </section>}
        </>
      )}
    </SurfaceShell>
  )
}
