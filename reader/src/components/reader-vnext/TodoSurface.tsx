import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import {
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from '../../lib/capabilities'
import type { ReaderTodoResponse } from '../../lib/api/types'
import { navigateReaderRoute, type ReaderRoute } from '../../lib/navigation/route'
import { Icon } from '../Icon'
import {
  navigateReaderTarget,
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

export interface TodoSurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly onNavigate: (route: ReaderRoute) => void
  readonly onOpenLink: (id: string) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly completedExpanded: boolean
  readonly onCompletedExpandedChange: (expanded: boolean) => void
}

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

const TODO_CHANGE_EVENT = 'webtag:todos-change'
const CREATE_BUSY_ID = '__create__'
const UNSUPPORTED_TODO_MESSAGE = '当前服务端不支持 TODO，未使用本地状态伪造结果。'
const INCOMPLETE_TODO_MESSAGE = '服务端返回的 TODO 数据不完整，未应用本地更改。'

type TodoClientMethod = 'listTodos' | 'createTodo' | 'patchTodo' | 'deleteTodo'
type TodoRecord = Record<string, unknown>

/** Metadata kept outside the wire contract for old projected responses. */
type TodoItem = ReaderTodoResponse & {
  readonly hostRevisionKnown: boolean
}

function isRecord(value: unknown): value is TodoRecord {
  return typeof value === 'object' && value !== null
}

function readField(record: TodoRecord, ...keys: string[]): unknown {
  for (const key of keys) {
    if (Object.prototype.hasOwnProperty.call(record, key) && record[key] !== undefined) {
      return record[key]
    }
  }
  return undefined
}

function readNestedRecord(record: TodoRecord, key: string): TodoRecord | null {
  const value = record[key]
  return isRecord(value) ? value : null
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

function supportsTodoMethod(client: IdentityBoundReaderClient, method: TodoClientMethod): boolean {
  return typeof (client as unknown as Record<string, unknown>)[method] === 'function'
}

function thrownErrorMessage(value: unknown): string {
  if (isRecord(value) && typeof value.message === 'string') {
    return errorMessage({ message: value.message, status: typeof value.status === 'number' ? value.status : undefined })
  }
  return '请求失败，请稍后重试。'
}

function emitTodoChange(): void {
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(TODO_CHANGE_EVENT))
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

function originLinkID(todo: ReaderTodoResponse): string | null {
  if (todo.origin_host_kind === 'link' && todo.origin_host_id) return todo.origin_host_id
  if (!todo.origin_ref || typeof todo.origin_ref !== 'object') return null
  const originRef = todo.origin_ref as TodoRecord
  const linkID = readNullableString(originRef, 'link_id', 'linkId')
  if (linkID) return linkID
  const sourceKind = readNullableString(originRef, 'source_kind', 'sourceKind', 'target_kind', 'targetKind', 'kind')
  const sourceID = readNullableString(originRef, 'source_id', 'sourceId', 'target_id', 'targetId', 'id')
  return sourceKind === 'link' ? sourceID : null
}

type TodoOriginTarget = {
  readonly kind: 'link' | 'note' | 'inbox' | 'thought'
  readonly id: string
}

function originTarget(todo: ReaderTodoResponse): TodoOriginTarget | null {
  const linkID = originLinkID(todo)
  if (linkID) return { kind: 'link', id: linkID }

  const originRef = todo.origin_ref && typeof todo.origin_ref === 'object' ? todo.origin_ref as TodoRecord : null
  const sourceKind = originRef
    ? readNullableString(originRef, 'source_kind', 'sourceKind', 'target_kind', 'targetKind', 'kind')
    : null
  const sourceID = originRef
    ? readNullableString(originRef, 'source_id', 'sourceId', 'target_id', 'targetId', 'id')
    : null
  const kind = sourceKind ?? todo.origin_host_kind ?? (todo.origin_kind === 'note' || todo.origin_kind === 'inbox' ? todo.origin_kind : null)
  const id = sourceID ?? todo.origin_host_id
  if ((kind === 'note' || kind === 'inbox' || kind === 'thought') && id) return { kind, id }
  return null
}

function originTargetRoute(target: TodoOriginTarget): ReaderRoute {
  if (target.kind === 'link') return { kind: 'library', id: 'reading' }
  if (target.kind === 'note') return { kind: 'library', id: 'notes' }
  if (target.kind === 'inbox') return { kind: 'library', id: 'pending', inboxId: target.id }
  return { kind: 'tool', id: 'history' }
}

function availableOriginTarget(
  todo: ReaderTodoResponse,
  capabilityPolicy: ReaderCapabilityPolicy,
): TodoOriginTarget | null {
  const target = originTarget(todo)
  return target && readerRouteIsAvailable(originTargetRoute(target), capabilityPolicy)
    ? target
    : null
}

export function TodoSurface({ client, onNavigate, onOpenLink, capabilityPolicy, completedExpanded, onCompletedExpandedChange }: TodoSurfaceProps) {
  const [items, setItems] = useState<TodoItem[]>([])
  const [text, setText] = useState('')
  const [dueAt, setDueAt] = useState('')
  const [editingID, setEditingID] = useState<string | null>(null)
  const [editingText, setEditingText] = useState('')
  const [editingDueAt, setEditingDueAt] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyID, setBusyID] = useState<string | null>(null)
  const busyIDRef = useRef<string | null>(null)
  const requestID = useRef(0)
  const mountedRef = useRef(false)
  const mutationEpochRef = useRef(0)

  const clearForIdentityLoss = useCallback(() => {
    if (!mountedRef.current) return
    busyIDRef.current = null
    setBusyID(null)
    setItems([])
    setEditingID(null)
    setEditingDueAt('')
    setError(SURFACE_IDENTITY_ERROR)
    setLoading(false)
  }, [])

  const ordered = useMemo(() => sortTodos(items), [items])
  const openItems = ordered.filter((item) => !item.done)
  const completedItems = ordered.filter((item) => item.done)
  const completedListID = useId()
  const navigate = useCallback((route: ReaderRoute) => navigateReaderRoute(route, onNavigate), [onNavigate])

  const load = useCallback(async (): Promise<boolean> => {
    const request = ++requestID.current
    const mutationEpoch = mutationEpochRef.current
    setLoading(true)
    setError(null)
    if (!identityIsCurrent(client)) {
      clearForIdentityLoss()
      return false
    }
    if (!supportsTodoMethod(client, 'listTodos')) {
      if (request === requestID.current && mountedRef.current) {
        setItems([])
        setError(UNSUPPORTED_TODO_MESSAGE)
        setLoading(false)
      }
      return false
    }
    try {
      const result = await client.listTodos()
      if (request !== requestID.current || mutationEpoch !== mutationEpochRef.current || !mountedRef.current) return false
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return false
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
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
      if (request === requestID.current && mountedRef.current) {
        if (!identityIsCurrent(client)) clearForIdentityLoss()
        else setError(thrownErrorMessage(cause))
      }
      return false
    } finally {
      if (request === requestID.current && mountedRef.current) setLoading(false)
    }
  }, [clearForIdentityLoss, client])

  useEffect(() => {
    mountedRef.current = true
    const onTodosChange = () => { void load() }
    window.addEventListener(TODO_CHANGE_EVENT, onTodosChange)
    void load()
    return () => {
      mountedRef.current = false
      requestID.current += 1
      window.removeEventListener(TODO_CHANGE_EVENT, onTodosChange)
    }
  }, [load])

  const beginBusy = useCallback((id: string): boolean => {
    if (busyIDRef.current !== null) return false
    mutationEpochRef.current += 1
    busyIDRef.current = id
    setBusyID(id)
    return true
  }, [])

  const endBusy = useCallback((id: string) => {
    if (busyIDRef.current !== id) return
    busyIDRef.current = null
    setBusyID(null)
  }, [])

  const create = useCallback(async () => {
    if (!text.trim()) return
    if (!beginBusy(CREATE_BUSY_ID)) return
    setSaving(true)
    try {
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!supportsTodoMethod(client, 'createTodo')) {
        setError(UNSUPPORTED_TODO_MESSAGE)
        return
      }
      const result = await client.createTodo({ text: text.trim(), due_at: dueAt ? new Date(dueAt).toISOString() : null })
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
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
      emitTodoChange()
    } catch (cause) {
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      setSaving(false)
      endBusy(CREATE_BUSY_ID)
    }
  }, [beginBusy, clearForIdentityLoss, client, dueAt, endBusy, text])

  const toggle = useCallback(async (todo: ReaderTodoResponse) => {
    const item = todo as TodoItem
    if (!beginBusy(todo.id)) return
    const desiredDone = !todo.done
    try {
      if (!supportsTodoMethod(client, 'patchTodo')) {
        setError(UNSUPPORTED_TODO_MESSAGE)
        return
      }
      if (item.origin_kind !== 'standalone' && !item.hostRevisionKnown) {
        setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
        return
      }
      const patch = todoDesiredStatePatch(todo, desiredDone)
      if (!patch) {
        setError('来源 TODO 缺少版本信息，未执行完成状态更改。')
        return
      }
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      const result = await client.patchTodo(todo.id, patch)
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
        return
      }
      const next = normalizeMutationTodo(result.data)
      if (!next || next.id !== todo.id || next.done !== desiredDone || (next.origin_kind !== 'standalone' && !next.hostRevisionKnown)) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => replaceTodo(current, next))
      emitTodoChange()
    } catch (cause) {
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      endBusy(todo.id)
    }
  }, [beginBusy, clearForIdentityLoss, client, endBusy])

  const saveText = useCallback(async (todo: ReaderTodoResponse) => {
    const nextText = editingText.trim()
    if (todo.origin_kind !== 'standalone' || !nextText || !beginBusy(todo.id)) return
    try {
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!supportsTodoMethod(client, 'patchTodo')) {
        setError(UNSUPPORTED_TODO_MESSAGE)
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
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
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
      emitTodoChange()
    } catch (cause) {
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      endBusy(todo.id)
    }
  }, [beginBusy, clearForIdentityLoss, client, editingDueAt, editingText, endBusy])

  const remove = useCallback(async (todo: ReaderTodoResponse) => {
    if (todo.origin_kind !== 'standalone' || !beginBusy(todo.id)) return
    try {
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!supportsTodoMethod(client, 'deleteTodo')) {
        setError(UNSUPPORTED_TODO_MESSAGE)
        return
      }
      const result = await client.deleteTodo(todo.id)
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) {
        clearForIdentityLoss()
        return
      }
      if (!result.ok) {
        if (isIdentityError(result.error)) clearForIdentityLoss()
        else setError(errorMessage(result.error))
        return
      }
      if (result.data !== true) {
        setError(INCOMPLETE_TODO_MESSAGE)
        return
      }
      setItems((current) => current.filter((item) => item.id !== todo.id))
      emitTodoChange()
    } catch (cause) {
      if (!mountedRef.current) return
      if (!identityIsCurrent(client)) clearForIdentityLoss()
      else setError(thrownErrorMessage(cause))
    } finally {
      endBusy(todo.id)
    }
  }, [beginBusy, clearForIdentityLoss, client, endBusy])

  const openOrigin = useCallback((todo: ReaderTodoResponse) => {
    const target = availableOriginTarget(todo, capabilityPolicy)
    if (!target) return
    if (target.kind === 'link') {
      onOpenLink(target.id)
      return
    }
    if (target.kind === 'note') {
      navigateReaderTarget({ kind: 'library', id: 'notes' }, onNavigate, { noteId: target.id })
    } else if (target.kind === 'inbox') {
      navigateReaderTarget({ kind: 'library', id: 'pending', inboxId: target.id }, onNavigate)
    } else if (target.kind === 'thought') {
      navigate({ kind: 'tool', id: 'history' })
    }
  }, [capabilityPolicy, navigate, onNavigate, onOpenLink])

  const renderTodoRow = (todo: ReaderTodoResponse) => {
    const targetAvailable = availableOriginTarget(todo, capabilityPolicy) !== null
    return <li key={todo.id} className={'rvx-todo-item' + (todo.done ? ' done' : '')}>
      <button className="rvx-check-button" type="button" disabled={busyID !== null} aria-label={todo.done ? `重新打开 ${todo.text}` : `完成 ${todo.text}`} aria-pressed={todo.done} onClick={() => void toggle(todo)}><Icon name="check" size={16} /></button>
      <div className="rvx-todo-content">
        {editingID === todo.id ? (
          <div className="rvx-todo-edit-fields">
            <input autoFocus disabled={busyID !== null} value={editingText} onChange={(event) => setEditingText(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); void saveText(todo) } if (event.key === 'Escape') { setEditingID(null); setEditingDueAt('') } }} />
            <label><span>截止时间</span><input type="datetime-local" value={editingDueAt} disabled={busyID !== null} onChange={(event) => setEditingDueAt(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); void saveText(todo) } if (event.key === 'Escape') { setEditingID(null); setEditingDueAt('') } }} /></label>
          </div>
        ) : todo.origin_kind === 'standalone' ? <button className="rvx-todo-text" type="button" disabled={busyID !== null} onClick={() => { setEditingID(todo.id); setEditingText(todo.text); setEditingDueAt(dateTimeLocalValue(todo.due_at)) }}>{todo.text}</button> : <span className="rvx-todo-text">{todo.text}</span>}
        <div className="rvx-todo-meta">{todo.due_at && <span className={todo.expired && !todo.done ? 'rvx-expired' : ''}>{todo.expired && !todo.done ? '已过期 · ' : '截止 · '}{formatRelativeDate(todo.due_at)}</span>}{todo.completed_at && <span>完成于 · {formatRelativeDate(todo.completed_at)}</span>}<span>{todo.origin_kind === 'standalone' ? '独立任务' : `来源：${todo.origin_kind}`}</span></div>
      </div>
      <div className="rvx-action-row"><button className="rvx-icon-button" type="button" title="打开来源" aria-label="打开来源" disabled={busyID !== null || !targetAvailable} onClick={() => openOrigin(todo)}><Icon name="arrowright" size={15} /></button>{todo.origin_kind === 'standalone' && <button className="rvx-icon-button danger" type="button" title="删除" aria-label="删除" disabled={busyID !== null} onClick={() => void remove(todo)}><Icon name="trash" size={15} /></button>}</div>
    </li>
  }

  return (
    <SurfaceShell
      title="TODO"
      subtitle={`${openItems.length} 项未完成`}
      activeRoute={{ kind: 'tool', id: 'todo' }}
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      actions={<button className="rvx-button secondary" type="button" disabled={loading || saving || busyID !== null} onClick={() => void load()}><Icon name="refresh" size={15} />刷新</button>}
    >
      {error && <SurfaceError message={error} onRetry={() => void load()} />}
      <section className="rvx-editor rvx-todo-create" aria-label="新建 TODO">
        <label><span>添加任务</span><input value={text} onChange={(event) => setText(event.target.value)} placeholder="下一步要完成什么？" onKeyDown={(event) => { if (event.key === 'Enter') void create() }} /></label>
        <label><span>截止时间</span><input type="datetime-local" value={dueAt} onChange={(event) => setDueAt(event.target.value)} /></label>
        <button className="rvx-button primary" type="button" disabled={saving || busyID !== null || !text.trim()} onClick={() => void create()}><Icon name="plus" size={15} />添加</button>
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
