import { useCallback, useEffect, useId, useMemo, useState } from 'react'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type { ReaderTodoResponse } from '../../lib/api/types'
import type { ReaderNavigationRequest } from '../../lib/navigation/route'
import type { ReaderInboxTodosPort } from '../../lib/reader-api-ports'
import { useExclusiveAction } from '../../hooks/useExclusiveAction'
import { useSurfaceRequestGate, type SurfaceRequestToken } from '../../hooks/useSurfaceRequestGate'
import { emitReaderEvent, READER_EVENTS, subscribeReaderEvents } from '../../lib/reader-events'
import { asRecord, isRecord } from '../../lib/records'
import { describeTodoRow, type TodoRowDescriptor } from '../../lib/todo-row-descriptor'
import {
  readerErrorMessage,
  SURFACE_IDENTITY_ERROR,
  identityIsCurrent,
  isIdentityError,
  todoDesiredStatePatch,
} from '../../lib/reader-surface'
import { Icon } from '../Icon'
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'

export interface TodoSurfaceProps {
  readonly client: TodoSurfaceClient
  readonly onNavigate: ReaderNavigationRequest
  readonly onOpenLink: (id: string) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly completedExpanded: boolean
  readonly onCompletedExpandedChange: (expanded: boolean) => void
}

type TodoSurfaceClient = Pick<
  ReaderInboxTodosPort,
  'listTodos' | 'createTodo' | 'patchTodo' | 'deleteTodo' | 'isIdentityCurrent'
>

function timestamp(value: string | null | undefined): number | null {
  if (!value) return null
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? null : parsed
}

function dateTimeLocalValue(value: string | null | undefined): string {
  const parsed = value ? new Date(value) : null
  if (!parsed || Number.isNaN(parsed.getTime())) return ''
  const pad = (part: number) => String(part).padStart(2, '0')
  return `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())}T${pad(parsed.getHours())}:${pad(parsed.getMinutes())}`
}

function dateTimeLocalISO(value: string): string | null {
  if (!value) return null
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString()
}

const CREATE_BUSY_ID = '__create__'
const INCOMPLETE_TODO_MESSAGE = '服务端返回的 TODO 数据不完整，未应用本地更改。'

/**
 * 请求通道。
 *
 * `list` 是列表读取本身；`mutation` 承载 create / toggle / saveText / remove。
 * 分成两条是因为它们互相作废的方向不同：本地写入必须让在途的列表读取作废
 * （否则改动前的快照会覆盖改动后的界面），但列表读取不该反过来作废一次正在
 * 提交的写入。load 因此在发请求前 `capture('mutation')`，回包时确认「这中间
 * 没有人写过」，而 loading 的释放只看自己的 `list` token——把两件事挂在同一个
 * 代次上，会让一次 mutation 把在途 load 的 loading 永远卡住。
 */
type TodoChannel = 'list' | 'mutation'
type TodoRequest = SurfaceRequestToken<TodoChannel>

/** Metadata kept outside the wire contract for old projected responses. */
type TodoItem = ReaderTodoResponse & {
  readonly hostRevisionKnown: boolean
}

type TodoRecord = Record<string, unknown>

function readField(record: TodoRecord, ...keys: string[]): unknown {
  for (const key of keys) {
    if (Object.prototype.hasOwnProperty.call(record, key) && record[key] !== undefined) {
      return record[key]
    }
  }
  return undefined
}

function readNestedRecord(record: TodoRecord, key: string): TodoRecord | null {
  return asRecord(record[key])
}

function readString(record: TodoRecord, ...keys: string[]): string | null {
  const value = readField(record, ...keys)
  return typeof value === 'string' ? value : null
}

function readNullableString(record: TodoRecord, ...keys: string[]): string | null {
  const value = readField(record, ...keys)
  return typeof value === 'string' && value.trim() ? value : null
}

function readDate(record: TodoRecord, ...keys: string[]): string | null {
  const value = readField(record, ...keys)
  return typeof value === 'string' && value ? value : null
}

function readBoolean(record: TodoRecord, ...keys: string[]): boolean | null {
  const value = readField(record, ...keys)
  return typeof value === 'boolean' ? value : null
}

function readRevision(record: TodoRecord, ...keys: string[]): number | null {
  const value = readField(record, ...keys)
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null
}

function originRefFromLegacy(origin: TodoRecord | null): unknown {
  if (!origin) return null
  const blockRef = readString(origin, 'block_ref', 'blockRef')
  const occurrence = readRevision(origin, 'occurrence')
  const linkID = readNullableString(origin, 'link_id', 'linkId')
  if (!blockRef && !linkID) return null
  return {
    ...(blockRef ? { block_ref: blockRef } : {}),
    ...(occurrence !== null ? { occurrence } : {}),
    ...(linkID ? { link_id: linkID } : {}),
  }
}

function normalizeTodo(value: unknown): TodoItem | null {
  if (!isRecord(value)) return null
  const origin = readNestedRecord(value, 'origin')
  const originKind = readString(value, 'origin_kind', 'originKind') ?? readString(origin ?? {}, 'kind', 'origin_kind', 'originKind')
  const text = readString(value, 'text')
  const id = readString(value, 'id', 'todo_id', 'todoId')
  const done = readBoolean(value, 'done', 'completed')
  const createdAt = readDate(value, 'created_at', 'createdAt') ?? readDate(value, 'updated_at', 'updatedAt')
  const updatedAt = readDate(value, 'updated_at', 'updatedAt') ?? createdAt
  if (!id || text === null || done === null || !originKind || !createdAt || !updatedAt) return null

  const dueAt = readDate(value, 'due_at', 'dueAt')
  const expiredValue = readBoolean(value, 'expired', 'is_expired', 'isExpired')
  const expired = expiredValue ?? Boolean(dueAt && timestamp(dueAt) !== null && timestamp(dueAt)! < Date.now() && !done)
  const hostRevisionValue = readRevision(value, 'host_revision', 'hostRevision')
  const hostRevisionKnown = hostRevisionValue !== null || originKind === 'standalone'
  const originRef = Object.prototype.hasOwnProperty.call(value, 'origin_ref')
    ? value.origin_ref
    : Object.prototype.hasOwnProperty.call(value, 'originRef')
      ? value.originRef
      : originRefFromLegacy(origin)

  return {
    id,
    text,
    due_at: dueAt,
    done,
    origin_kind: originKind,
    origin_host_kind: readNullableString(value, 'origin_host_kind', 'originHostKind') ?? readNullableString(origin ?? {}, 'host_kind', 'hostKind'),
    origin_host_id: readNullableString(value, 'origin_host_id', 'originHostId') ?? readNullableString(origin ?? {}, 'host_id', 'hostId'),
    origin_ref: originRef ?? null,
    host_revision: hostRevisionValue ?? 0,
    completed_at: readDate(value, 'completed_at', 'completedAt'),
    created_at: createdAt,
    updated_at: updatedAt,
    expired,
    hostRevisionKnown,
  }
}

function unwrapTodo(value: unknown): unknown {
  if (!isRecord(value)) return value
  if (Object.prototype.hasOwnProperty.call(value, 'todo')) return value.todo
  if (Object.prototype.hasOwnProperty.call(value, 'item')) return value.item
  return value
}

function normalizeTodoList(value: unknown): TodoItem[] | null {
  const record = isRecord(value) ? value : null
  const rawItems = Array.isArray(value)
    ? value
    : Array.isArray(record?.items)
      ? record.items
      : Array.isArray(record?.todos)
        ? record.todos
        : null
  if (!rawItems) return null
  const normalized = rawItems.map(normalizeTodo)
  return normalized.every((item): item is TodoItem => item !== null) ? normalized : null
}

function normalizeMutationTodo(value: unknown): TodoItem | null {
  return normalizeTodo(unwrapTodo(value))
}

// eslint-disable-next-line react-refresh/only-export-components
export function sortTodos(items: readonly ReaderTodoResponse[]): ReaderTodoResponse[] {
  return [...items].sort((left, right) => {
    if (left.done !== right.done) return Number(left.done) - Number(right.done)
    const leftDue = timestamp(left.due_at)
    const rightDue = timestamp(right.due_at)
    if (leftDue !== null && rightDue !== null && leftDue !== rightDue) return leftDue - rightDue
    if (leftDue !== null && rightDue === null) return -1
    if (leftDue === null && rightDue !== null) return 1
    const created = (timestamp(right.created_at) ?? Number.NEGATIVE_INFINITY) - (timestamp(left.created_at) ?? Number.NEGATIVE_INFINITY)
    return created || left.id.localeCompare(right.id)
  })
}

function replaceTodo(items: readonly TodoItem[], next: TodoItem): TodoItem[] {
  return items.map((item) => item.id === next.id ? next : item)
}

export function TodoSurface({ client, onNavigate, onOpenLink, capabilityPolicy, completedExpanded, onCompletedExpandedChange }: TodoSurfaceProps) {
  const [items, setItems] = useState<TodoItem[]>([])
  const [text, setText] = useState('')
  const [dueAt, setDueAt] = useState('')
  const [editingID, setEditingID] = useState<string | null>(null)
  const [editingText, setEditingText] = useState('')
  const [editingDueAt, setEditingDueAt] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const gate = useSurfaceRequestGate<TodoChannel>({
    owner: [client],
    authority: () => identityIsCurrent(client),
  })
  // 创建没有行 ID，用固定的 CREATE_BUSY_ID 占同一个单飞坑位，于是 CRUD 四条路径
  // 共用一份互斥状态。原来的 `saving` state 因此消失：它的生命周期与 busy 完全
  // 重合，每个使用点都写成 `saving || busyID !== null`；真要单独认出「正在创建」，
  // 从 `busyKey === CREATE_BUSY_ID` 派生即可，不必再维护第二份 state。
  const {
    busy: actionBusy,
    begin: beginAction,
    finish: finishAction,
    clear: clearAction,
  } = useExclusiveAction<string>()

  /**
   * 身份失效后的收口。用 `isSameOwner` 而不是 `isCurrent`：此刻 authority 已经
   * 是 false，但发起这次请求的那段代码仍然必须能把自己置的 loading 收掉并写出
   * 失效提示，否则界面永远停在加载中。generation 仍然要看——被顶掉的旧请求不该
   * 清掉新请求刚画上的东西。
   */
  const clearForIdentityLoss = useCallback((token: TodoRequest) => {
    if (!gate.isSameOwner(token)) return
    clearAction()
    setItems([])
    setEditingID(null)
    setEditingDueAt('')
    setError(SURFACE_IDENTITY_ERROR)
    setLoading(false)
  }, [clearAction, gate])

  const ordered = useMemo(() => sortTodos(items), [items])
  const openItems = ordered.filter((item) => !item.done)
  const completedItems = ordered.filter((item) => item.done)
  const completedListID = useId()
  const load = useCallback(async (): Promise<boolean> => {
    const token = gate.begin('list')
    // 本地写入会顶掉这个代次：回包时若代次已变，说明列表快照比界面还旧。
    const mutationEpoch = gate.capture('mutation')
    setLoading(true)
    setError(null)
    if (!gate.isCurrent(token)) {
      clearForIdentityLoss(token)
      return false
    }
    try {
      const result = await client.listTodos()
      if (!gate.isSameOwner(token) || !gate.isSameOwner(mutationEpoch)) return false
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return false
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(result.error))
        return false
      }
      const next = normalizeTodoList(result.data)
      if (!next) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return false
      }
      setItems(next)
      return true
    } catch (cause) {
      if (gate.isSameOwner(token)) {
        if (!gate.isCurrent(token)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(cause))
      }
      return false
    } finally {
      if (gate.isSameOwner(token)) setLoading(false)
    }
  }, [clearForIdentityLoss, client, gate])

  useEffect(() => {
    const unsubscribe = subscribeReaderEvents([READER_EVENTS.todosChanged], () => { void load() })
    void load()
    return unsubscribe
  }, [load])

  const create = useCallback(async () => {
    if (!text.trim()) return
    const busy = beginAction(CREATE_BUSY_ID)
    if (!busy) return
    const token = gate.begin('mutation')
    try {
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      const result = await client.createTodo({ text: text.trim(), due_at: dueAt ? new Date(dueAt).toISOString() : null })
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(result.error))
        return
      }
      const next = normalizeMutationTodo(result.data)
      if (!next || next.origin_kind !== 'standalone' || next.text !== text.trim()) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => [...current, next])
      setText('')
      setDueAt('')
      emitReaderEvent(READER_EVENTS.todosChanged)
    } catch (cause) {
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) clearForIdentityLoss(token)
      else setError(readerErrorMessage(cause))
    } finally {
      finishAction(busy)
    }
  }, [beginAction, clearForIdentityLoss, client, dueAt, finishAction, gate, text])

  const toggle = useCallback(async (todo: ReaderTodoResponse) => {
    const item = todo as TodoItem
    const busy = beginAction(todo.id)
    if (!busy) return
    const token = gate.begin('mutation')
    const desiredDone = !todo.done
    try {
      if (item.origin_kind !== 'standalone' && !item.hostRevisionKnown) {
        setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
        return
      }
      const patch = todoDesiredStatePatch(todo, desiredDone)
      if (!patch) {
        setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
        return
      }
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      const result = await client.patchTodo(todo.id, patch)
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(result.error))
        return
      }
      const next = normalizeMutationTodo(result.data)
      if (!next || next.id !== todo.id || next.done !== desiredDone || (next.origin_kind !== 'standalone' && !next.hostRevisionKnown)) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => replaceTodo(current, next))
      emitReaderEvent(READER_EVENTS.todosChanged)
    } catch (cause) {
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) clearForIdentityLoss(token)
      else setError(readerErrorMessage(cause))
    } finally {
      finishAction(busy)
    }
  }, [beginAction, clearForIdentityLoss, client, finishAction, gate])

  const saveText = useCallback(async (todo: ReaderTodoResponse) => {
    const nextText = editingText.trim()
    if (todo.origin_kind !== 'standalone' || !nextText) return
    const busy = beginAction(todo.id)
    if (!busy) return
    const token = gate.begin('mutation')
    try {
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      const originalDueAt = dateTimeLocalValue(todo.due_at)
      let duePatch: { readonly due_at?: string | null } = {}
      if (editingDueAt !== originalDueAt) {
        if (editingDueAt === '') {
          duePatch = { due_at: null }
        } else {
          const parsedDueAt = dateTimeLocalISO(editingDueAt)
          if (!parsedDueAt) {
            setError('截止时间格式无效，未应用本地更改。')
            return
          }
          duePatch = { due_at: parsedDueAt }
        }
      }
      const result = await client.patchTodo(todo.id, { text: nextText, ...duePatch })
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(result.error))
        return
      }
      const next = normalizeMutationTodo(result.data)
      const expectedDueAt = duePatch.due_at === undefined ? todo.due_at : duePatch.due_at
      const dueMatches = timestamp(next?.due_at) === timestamp(expectedDueAt)
      if (!next || next.id !== todo.id || next.origin_kind !== 'standalone' || next.text !== nextText || !dueMatches) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => replaceTodo(current, next))
      setEditingID(null)
      setEditingDueAt('')
      emitReaderEvent(READER_EVENTS.todosChanged)
    } catch (cause) {
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) clearForIdentityLoss(token)
      else setError(readerErrorMessage(cause))
    } finally {
      finishAction(busy)
    }
  }, [beginAction, clearForIdentityLoss, client, editingDueAt, editingText, finishAction, gate])

  const remove = useCallback(async (todo: ReaderTodoResponse) => {
    if (todo.origin_kind !== 'standalone') return
    const busy = beginAction(todo.id)
    if (!busy) return
    const token = gate.begin('mutation')
    try {
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      const result = await client.deleteTodo(todo.id)
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) {
        clearForIdentityLoss(token)
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss(token)
        else setError(readerErrorMessage(result.error))
        return
      }
      if (result.data !== true) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => current.filter((item) => item.id !== todo.id))
      emitReaderEvent(READER_EVENTS.todosChanged)
    } catch (cause) {
      if (!gate.isSameOwner(token)) return
      if (!gate.isCurrent(token)) clearForIdentityLoss(token)
      else setError(readerErrorMessage(cause))
    } finally {
      finishAction(busy)
    }
  }, [beginAction, clearForIdentityLoss, client, finishAction, gate])

  const openOrigin = useCallback((descriptor: TodoRowDescriptor) => {
    const navigation = descriptor.navigation
    if (!navigation?.available) return
    if (navigation.action === 'open-link') {
      onOpenLink(navigation.linkId)
      return
    }
    if (navigation.targets) onNavigate(navigation.route, navigation.targets)
    else onNavigate(navigation.route)
  }, [onNavigate, onOpenLink])

  const renderTodoRow = (todo: ReaderTodoResponse) => {
    const descriptor = describeTodoRow(todo, capabilityPolicy)
    return <li key={todo.id} className={'rvx-todo-item' + (todo.done ? ' done' : '')}>
      <button className="rvx-check-button" type="button" disabled={actionBusy} aria-label={descriptor.toggleLabel} aria-pressed={todo.done} onClick={() => void toggle(todo)}><Icon name="check" size={16} /></button>
      <div className="rvx-todo-content">
        {editingID === todo.id ? (
          <div className="rvx-todo-edit-fields">
            <input autoFocus disabled={actionBusy} value={editingText} onChange={(event) => setEditingText(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); void saveText(todo) } if (event.key === 'Escape') { setEditingID(null); setEditingDueAt('') } }} />
            <label><span>截止时间</span><input type="datetime-local" value={editingDueAt} disabled={actionBusy} onChange={(event) => setEditingDueAt(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); void saveText(todo) } if (event.key === 'Escape') { setEditingID(null); setEditingDueAt('') } }} /></label>
          </div>
        ) : descriptor.canEditText ? <button className="rvx-todo-text" type="button" disabled={actionBusy} onClick={() => { setEditingID(todo.id); setEditingText(todo.text); setEditingDueAt(dateTimeLocalValue(todo.due_at)) }}>{descriptor.title}</button> : <span className="rvx-todo-text">{descriptor.title}</span>}
        <div className="rvx-todo-meta">{descriptor.meta.map((segment) => <span key={segment.key} className={segment.emphasis === 'expired' ? 'rvx-expired' : undefined}>{segment.text}</span>)}</div>
      </div>
      <div className="rvx-action-row"><button className="rvx-icon-button" type="button" title={descriptor.navigation?.title ?? '打开来源'} aria-label={descriptor.navigation?.label ?? '打开来源'} disabled={actionBusy || !descriptor.navigation?.available} onClick={() => openOrigin(descriptor)}><Icon name="arrowright" size={15} /></button>{descriptor.canDelete && <button className="rvx-icon-button danger" type="button" title="删除" aria-label="删除" disabled={actionBusy} onClick={() => void remove(todo)}><Icon name="trash" size={15} /></button>}</div>
    </li>
  }

  return (
    <SurfaceShell
      title="TODO"
      subtitle={`${openItems.length} 项未完成`}
      activeRoute={{ kind: 'tool', id: 'todo' }}
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      actions={<button className="rvx-button secondary" type="button" disabled={loading || actionBusy} onClick={() => void load()}><Icon name="refresh" size={15} />刷新</button>}
    >
      {error && <SurfaceError message={error} onRetry={() => void load()} />}
      <section className="rvx-editor rvx-todo-create" aria-label="新建 TODO">
        <label><span>添加任务</span><input value={text} onChange={(event) => setText(event.target.value)} placeholder="下一步要完成什么？" onKeyDown={(event) => { if (event.key === 'Enter') void create() }} /></label>
        <label><span>截止时间</span><input type="datetime-local" value={dueAt} onChange={(event) => setDueAt(event.target.value)} /></label>
        <button className="rvx-button primary" type="button" disabled={actionBusy || !text.trim()} onClick={() => void create()}><Icon name="plus" size={15} />添加</button>
      </section>
      {loading && items.length === 0 ? <SurfaceLoading /> : !error && ordered.length === 0 ? <div className="rvx-empty"><Icon name="check" size={24} /><h2>没有 TODO</h2><p>把阅读和整理过程中的下一步写下来。</p></div> : (
        <>
          {openItems.length > 0 && <ul className="rvx-todo-list rvx-todo-page-list" aria-label="未完成 TODO">{openItems.map(renderTodoRow)}</ul>}
          {completedItems.length > 0 && (
            <section className="rvx-todo-completed" aria-label="已完成 TODO 分组">
              <button
                className={'rvx-todo-completed-toggle' + (completedExpanded ? ' expanded' : '')}
                type="button"
                aria-expanded={completedExpanded}
                aria-controls={completedListID}
                onClick={() => onCompletedExpandedChange(!completedExpanded)}
              >
                <Icon name="chevron" size={14} />
                <span>已完成 {completedItems.length}</span>
              </button>
              {completedExpanded && <ul id={completedListID} className="rvx-todo-list rvx-todo-page-list" aria-label="已完成 TODO">{completedItems.map(renderTodoRow)}</ul>}
            </section>
          )}
        </>
      )}
    </SurfaceShell>
  )
}
