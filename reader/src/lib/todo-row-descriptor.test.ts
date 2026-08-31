import { describe, expect, it } from 'vitest'
import type { ReaderTodoResponse } from './api/types'
import { describeTodoRow, type TodoRowDescriptor } from './todo-row-descriptor'
import { enabledReaderCapabilityPolicy } from '../test/capabilities'

function todo(overrides: Partial<ReaderTodoResponse> = {}): ReaderTodoResponse {
  return {
    id: 'todo-1',
    text: '任务',
    due_at: null,
    done: false,
    origin_kind: 'standalone',
    origin_host_kind: null,
    origin_host_id: null,
    origin_ref: null,
    host_revision: 0,
    completed_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    expired: false,
    ...overrides,
  }
}

function sourceMeta(descriptor: TodoRowDescriptor): string | undefined {
  return descriptor.meta.find((segment) => segment.key === 'source')?.text
}

describe('describeTodoRow', () => {
  it('describes standalone edit/delete affordances and source metadata', () => {
    const descriptor = describeTodoRow(todo({
      text: '独立任务',
      due_at: '2026-08-12T03:45:00Z',
      completed_at: '2026-08-13T03:45:00Z',
      done: true,
    }), enabledReaderCapabilityPolicy())

    expect(descriptor.title).toBe('独立任务')
    expect(descriptor.toggleLabel).toBe('重新打开 独立任务')
    expect(descriptor.canEditText).toBe(true)
    expect(descriptor.canDelete).toBe(true)
    expect(descriptor.navigation).toBeNull()
    expect(descriptor.meta.map((segment) => segment.key)).toEqual(['due', 'completed', 'source'])
    expect(sourceMeta(descriptor)).toBe('独立任务')
  })

  it('describes thought sources as live thought-history navigation', () => {
    const descriptor = describeTodoRow(todo({
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T9',
      origin_ref: { source_kind: 'thought', source_id: 'T9' },
      host_revision: 4,
    }), enabledReaderCapabilityPolicy())

    expect(descriptor.canEditText).toBe(false)
    expect(descriptor.canDelete).toBe(false)
    expect(sourceMeta(descriptor)).toBe('来源：想法')
    const navigation = descriptor.navigation
    if (!navigation || navigation.action !== 'navigate') throw new Error('expected route navigation')
    expect(navigation.targetKind).toBe('thought')
    expect(navigation.route).toEqual({ kind: 'tool', id: 'history' })
    expect(navigation.targets).toEqual({ thoughtView: 'live' })
    expect(navigation.available).toBe(true)
  })

  it('describes note targets from origin_ref metadata', () => {
    const descriptor = describeTodoRow(todo({
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T-note',
      origin_ref: { source_kind: 'note', source_id: 'N9' },
      host_revision: 5,
    }), enabledReaderCapabilityPolicy())

    expect(sourceMeta(descriptor)).toBe('来源：笔记')
    const navigation = descriptor.navigation
    if (!navigation || navigation.action !== 'navigate') throw new Error('expected route navigation')
    expect(navigation.targetKind).toBe('note')
    expect(navigation.route).toEqual({ kind: 'library', id: 'notes' })
    expect(navigation.targets).toEqual({ noteId: 'N9' })
  })

  it('describes library link targets as open-link navigation', () => {
    const descriptor = describeTodoRow(todo({
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T-link',
      origin_ref: { source_kind: 'link', source_id: 'source-link', link_id: 'L9' },
      host_revision: 6,
    }), enabledReaderCapabilityPolicy())

    expect(sourceMeta(descriptor)).toBe('来源：阅读')
    const navigation = descriptor.navigation
    if (!navigation || navigation.action !== 'open-link') throw new Error('expected link navigation')
    expect(navigation.targetKind).toBe('library')
    expect(navigation.linkId).toBe('L9')
    expect(navigation.route).toEqual({ kind: 'library', id: 'reading' })
    expect(navigation.targets).toEqual({ linkId: 'L9' })
  })

  it('describes pending inbox targets from origin_ref metadata', () => {
    const descriptor = describeTodoRow(todo({
      origin_kind: 'thought',
      origin_host_kind: 'thought',
      origin_host_id: 'T-inbox',
      origin_ref: { source_kind: 'inbox', source_id: 'I9' },
      host_revision: 7,
    }), enabledReaderCapabilityPolicy())

    expect(sourceMeta(descriptor)).toBe('来源：收件箱')
    const navigation = descriptor.navigation
    if (!navigation || navigation.action !== 'navigate') throw new Error('expected route navigation')
    expect(navigation.targetKind).toBe('pending-inbox')
    expect(navigation.route).toEqual({ kind: 'library', id: 'pending', inboxId: 'I9' })
    expect(navigation.targets).toBeUndefined()
  })

  it('keeps unknown projected targets readable but not navigable', () => {
    const descriptor = describeTodoRow(todo({
      text: '未知来源任务',
      origin_kind: 'external-system',
      origin_host_kind: 'site',
      origin_host_id: 'S9',
      origin_ref: { source_kind: 'site', source_id: 'S9' },
      host_revision: 8,
    }), enabledReaderCapabilityPolicy())

    expect(descriptor.title).toBe('未知来源任务')
    expect(descriptor.toggleLabel).toBe('完成 未知来源任务')
    expect(descriptor.canEditText).toBe(false)
    expect(descriptor.canDelete).toBe(false)
    expect(sourceMeta(descriptor)).toBe('来源：external-system')
    expect(descriptor.navigation).toBeNull()
  })
})
