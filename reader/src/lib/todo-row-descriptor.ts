import type { ReaderTodoResponse } from './api/types'
import {
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from './capabilities'
import type {
  ReaderNavigationTarget,
  ReaderRoute,
  ReaderRouteTargets,
} from './navigation/route'
import { asRecord } from './records'
import { formatRelativeDate } from './reader-surface'

export type TodoRowMetaKey = 'due' | 'completed' | 'source'

export interface TodoRowMetaSegment {
  readonly key: TodoRowMetaKey
  readonly text: string
  readonly emphasis?: 'expired'
}

export type TodoRowTargetKind = 'thought' | 'note' | 'library' | 'pending-inbox'

type TodoRowTarget = {
  readonly kind: TodoRowTargetKind
  readonly id: string
}

interface TodoRowNavigationBase extends ReaderNavigationTarget {
  readonly targetKind: TodoRowTargetKind
  readonly available: boolean
  readonly label: string
  readonly title: string
}

export type TodoRowNavigation =
  | (TodoRowNavigationBase & {
    readonly action: 'open-link'
    readonly targetKind: 'library'
    readonly linkId: string
    readonly route: Extract<ReaderRoute, { readonly kind: 'library' }>
    readonly targets: ReaderRouteTargets & { readonly linkId: string }
  })
  | (TodoRowNavigationBase & {
    readonly action: 'navigate'
    readonly targetKind: Exclude<TodoRowTargetKind, 'library'>
  })

export interface TodoRowDescriptor {
  readonly title: string
  readonly toggleLabel: string
  readonly canEditText: boolean
  readonly canDelete: boolean
  readonly meta: readonly TodoRowMetaSegment[]
  readonly navigation: TodoRowNavigation | null
}

function normalizedKind(value: string | null | undefined): string | null {
  const normalized = value?.trim().toLowerCase()
  return normalized ? normalized : null
}

function normalizedID(value: string | null | undefined): string | null {
  const normalized = value?.trim()
  return normalized && !normalized.includes('\0') ? normalized : null
}

function recordString(record: Record<string, unknown> | null, ...keys: string[]): string | null {
  if (!record) return null
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'string') {
      const normalized = normalizedID(value)
      if (normalized) return normalized
    }
  }
  return null
}

function originRef(todo: ReaderTodoResponse): Record<string, unknown> | null {
  return asRecord(todo.origin_ref)
}

function sourceKind(todo: ReaderTodoResponse): string | null {
  const ref = originRef(todo)
  return normalizedKind(recordString(ref, 'source_kind', 'sourceKind', 'target_kind', 'targetKind', 'kind'))
}

function sourceID(todo: ReaderTodoResponse): string | null {
  const ref = originRef(todo)
  return recordString(ref, 'source_id', 'sourceId', 'target_id', 'targetId', 'id')
}

function originLinkID(todo: ReaderTodoResponse): string | null {
  if (normalizedKind(todo.origin_host_kind) === 'link') return normalizedID(todo.origin_host_id)
  const ref = originRef(todo)
  const linkID = recordString(ref, 'link_id', 'linkId')
  if (linkID) return linkID
  return sourceKind(todo) === 'link' ? sourceID(todo) : null
}

function todoRowTarget(todo: ReaderTodoResponse): TodoRowTarget | null {
  const linkID = originLinkID(todo)
  if (linkID) return { kind: 'library', id: linkID }

  const kind = sourceKind(todo) ??
    normalizedKind(todo.origin_host_kind) ??
    (normalizedKind(todo.origin_kind) === 'note' || normalizedKind(todo.origin_kind) === 'inbox'
      ? normalizedKind(todo.origin_kind)
      : null)
  const id = sourceID(todo) ?? normalizedID(todo.origin_host_id)
  if (!id) return null

  if (kind === 'note') return { kind: 'note', id }
  if (kind === 'inbox') return { kind: 'pending-inbox', id }
  if (kind === 'thought') return { kind: 'thought', id }
  return null
}

function sourceLabel(todo: ReaderTodoResponse, target: TodoRowTarget | null): string {
  if (normalizedKind(todo.origin_kind) === 'standalone') return '独立任务'
  if (target) {
    if (target.kind === 'library') return '来源：阅读'
    if (target.kind === 'note') return '来源：笔记'
    if (target.kind === 'pending-inbox') return '来源：收件箱'
    return '来源：想法'
  }

  const kind = normalizedKind(todo.origin_kind)
  if (kind === 'thought') return '来源：想法'
  if (kind === 'note') return '来源：笔记'
  if (kind === 'inbox') return '来源：收件箱'
  return `来源：${todo.origin_kind.trim() || '未知'}`
}

function targetRoute(target: TodoRowTarget): ReaderNavigationTarget {
  if (target.kind === 'library') {
    return {
      route: { kind: 'library', id: 'reading' },
      targets: { linkId: target.id },
    }
  }
  if (target.kind === 'note') {
    return {
      route: { kind: 'library', id: 'notes' },
      targets: { noteId: target.id },
    }
  }
  if (target.kind === 'pending-inbox') {
    return {
      route: { kind: 'library', id: 'pending', inboxId: target.id },
    }
  }
  return {
    route: { kind: 'tool', id: 'history' },
    targets: { thoughtView: 'live' },
  }
}

function navigationForTarget(
  target: TodoRowTarget | null,
  capabilityPolicy: ReaderCapabilityPolicy,
): TodoRowNavigation | null {
  if (!target) return null
  const navigation = targetRoute(target)
  const base = {
    ...navigation,
    available: readerRouteIsAvailable(navigation.route, capabilityPolicy),
    label: '打开来源',
    title: '打开来源',
  }

  if (target.kind === 'library') {
    return {
      ...base,
      action: 'open-link',
      targetKind: 'library',
      linkId: target.id,
      route: { kind: 'library', id: 'reading' },
      targets: { linkId: target.id },
    }
  }

  return {
    ...base,
    action: 'navigate',
    targetKind: target.kind,
  }
}

function todoMeta(todo: ReaderTodoResponse, label: string): TodoRowMetaSegment[] {
  return [
    ...(todo.due_at
      ? [{
        key: 'due' as const,
        text: `${todo.expired && !todo.done ? '已过期' : '截止'} · ${formatRelativeDate(todo.due_at)}`,
        ...(todo.expired && !todo.done ? { emphasis: 'expired' as const } : {}),
      }]
      : []),
    ...(todo.completed_at
      ? [{ key: 'completed' as const, text: `完成于 · ${formatRelativeDate(todo.completed_at)}` }]
      : []),
    { key: 'source' as const, text: label },
  ]
}

export function describeTodoRow(
  todo: ReaderTodoResponse,
  capabilityPolicy: ReaderCapabilityPolicy,
): TodoRowDescriptor {
  const target = todoRowTarget(todo)
  const label = sourceLabel(todo, target)
  const canMutateStandalone = normalizedKind(todo.origin_kind) === 'standalone'
  return {
    title: todo.text,
    toggleLabel: todo.done ? `重新打开 ${todo.text}` : `完成 ${todo.text}`,
    canEditText: canMutateStandalone,
    canDelete: canMutateStandalone,
    meta: todoMeta(todo, label),
    navigation: navigationForTarget(target, capabilityPolicy),
  }
}
