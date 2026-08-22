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
import type { ReaderHomeResponse, ReaderTodoResponse } from '../../lib/api/types'
import type { IdentityLease } from '../../lib/identity'
import { useExclusiveAction } from '../../hooks/useExclusiveAction'
import { useSurfaceRequestGate } from '../../hooks/useSurfaceRequestGate'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../../lib/reader-events'
import { isRecord } from '../../lib/records'
import {
  SURFACE_IDENTITY_ERROR,
  formatRelativeDate,
  identityIsCurrent,
  isIdentityError,
  readerErrorMessage,
  todoDesiredStatePatch,
} from '../../lib/reader-surface'
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
import { ReaderListRow } from '../ui/ReaderListRow'
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'

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

/**
 * 三条通道。`home` 是聚合读取，`thought-projection` 是本地想法读模型的投影，
 * `todos` 是 TODO 写回——它刻意与 `home` 分开：原实现里 TODO 写回只判断挂载，
 * 从不看聚合读取的代次，并进的话一次并发的聚合刷新就会吞掉写回后的回读事件。
 */
type HomeRequestChannel = 'home' | 'thought-projection' | 'todos'

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

  // 身份是 owner：租约一换，此前取出的所有 token 立即作废，旧命名空间的回包
  // 画不到新身份上。authority 只放身份判断——Home 的两条能力检查用的不是同一
  // 个 capability（`home` 与 `todos`），塞进同一个 authority 会把它们混成一条。
  const gate = useSurfaceRequestGate<HomeRequestChannel>({
    owner: [lease],
    authority: () => identityIsCurrent(client),
  })
  const {
    busy: todoBusy,
    begin: beginTodoAction,
    finish: finishTodoAction,
    clear: clearTodoAction,
  } = useExclusiveAction<string>()

  // A new identity must not paint the old namespace while its initial request
  // is being negotiated.  The following effect runs before the browser paint.
  // 请求归属由闸门的 owner 负责作废，这里只负责把已经画出来的旧数据清掉。
  useLayoutEffect(() => {
    setHome(null)
    setRecentThoughts(null)
    setError(null)
    setLoading(true)
  }, [lease])

  const refreshLocalThoughts = useCallback(async () => {
    if (!lease?.context?.physicalNamespace || !identityIsCurrent(client)) return
    // 这是一次搭车读取：它不该顶掉在途的聚合请求，所以取号而不是重新开始。
    const request = gate.capture('home')
    const projection = gate.begin('thought-projection')
    const selected = await selectThoughtReadModel(lease)
    if (!selected.ok || !gate.isCurrent(request) || !gate.isCurrent(projection)) return
    setRecentThoughts(selected.value)
  }, [client, gate, lease])

  const clearForIdentityLoss = useCallback(() => {
    clearTodoAction()
    setHome(null)
    setRecentThoughts(null)
    setError(SURFACE_IDENTITY_ERROR)
    setLoading(false)
  }, [clearTodoAction])

  const load = useCallback(async () => {
    const request = gate.begin('home')
    // 卸载后仍可能有排队的 `void load()`；取号后立刻自查一次挂载与归属。
    if (!gate.isSameOwner(request)) return
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
      if (!gate.isSameOwner(request) || !capabilityLease.isCurrent('home')) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (result.ok) {
        let localRecentThoughts: readonly ThoughtReadModelItem[] | null = null
        const projection = gate.begin('thought-projection')
        if (lease?.context?.physicalNamespace) {
          const cached = await cacheServerThoughtPage(lease, result.data.recent_thoughts)
          if (!gate.isCurrent(request) || !gate.isCurrent(projection)) return
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
          if (!gate.isCurrent(request) || !gate.isCurrent(projection)) return
          if (selected.ok) localRecentThoughts = selected.value
        }
        setHome(result.data)
        setRecentThoughts(localRecentThoughts)
      }
      else if (isIdentityError(result.error)) clearForIdentityLoss()
      else setError(readerErrorMessage(result.error))
    } catch (cause) {
      if (!gate.isSameOwner(request) || !capabilityLease.isCurrent('home')) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(readerErrorMessage(cause))
    } finally {
      // 收 loading 用的是完整判断：身份还在、能力还在，才说明这个请求仍然是
      // 当前界面正在等的那一个。
      if (gate.isCurrent(request) && capabilityLease.isCurrent('home')) setLoading(false)
    }
  }, [capabilityLease, clearForIdentityLoss, client, gate, lease])

  useEffect(() => {
    const onSharedStateChange = () => { void load() }
    const onThoughtChange = () => { void refreshLocalThoughts() }
    const unsubscribeShared = subscribeReaderEvents(
      [READER_EVENTS.todosChanged, READER_EVENTS.homeChanged],
      onSharedStateChange,
    )
    const unsubscribeThoughts = subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged, READER_EVENTS.thoughtsSynced],
      onThoughtChange,
    )
    void load()
    // A durable offline thought can predate this surface and its event listener.
    void refreshLocalThoughts()
    return () => {
      unsubscribeShared()
      unsubscribeThoughts()
    }
  }, [load, refreshLocalThoughts])

  const toggleTodo = useCallback(async (todo: ReaderTodoResponse) => {
    const request = gate.begin('todos')
    if (!gate.isSameOwner(request) || !capabilityLease.isCurrent('todos')) return
    const action = beginTodoAction(todo.id)
    if (!action) return
    const patch = todoDesiredStatePatch(todo, !todo.done)
    if (!patch) {
      setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
      finishTodoAction(action)
      return
    }
    try {
      if (!identityIsCurrent(client) || !capabilityLease.isCurrent('todos')) {
        clearForIdentityLoss()
        return
      }
      const result = await client.patchTodo(todo.id, patch)
      if (!gate.isSameOwner(request) || !capabilityLease.isCurrent('todos')) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(readerErrorMessage(result.error))
        return
      }
      // Home is an aggregate, not a second TODO store. Re-read the aggregate so
      // its counts and preview rows advance together with the authoritative item.
      emitReaderEvent(READER_EVENTS.todosChanged)
    } catch (cause) {
      if (!gate.isSameOwner(request)) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(readerErrorMessage(cause))
    } finally {
      // 只有仍然持有的 token 才释放：身份撤销时 clearForIdentityLoss 已经把坑
      // 让出去了，这里不能把后来者的 busy 一并清掉。
      finishTodoAction(action)
    }
  }, [beginTodoAction, capabilityLease, clearForIdentityLoss, client, finishTodoAction, gate])

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
                        <ReaderListRow
                          key={item.key}
                          variant="compact"
                          source={item.source}
                          meta={formatRelativeDate(item.event_at)}
                          title={item.title || '未命名内容'}
                          summary={item.summary || '继续阅读'}
                          actions={item.link_id ? (
                            <button className="rvx-icon-button" type="button" aria-label="打开阅读" title="打开阅读" onClick={() => onOpenLink(item.link_id as string)}><Icon name="arrowright" size={15} /></button>
                          ) : policy.inbox && item.inbox_id ? (
                            <button className="rvx-icon-button" type="button" aria-label="打开收件箱" title="打开收件箱" onClick={() => navigate({ kind: 'library', id: 'pending', inboxId: item.inbox_id as string })}><Icon name="arrowright" size={15} /></button>
                          ) : null}
                        />
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
                      {openTodos.slice(0, 3).map((todo) => <TodoRow key={todo.id} todo={todo} onToggle={toggleTodo} disabled={todoBusy} />)}
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
