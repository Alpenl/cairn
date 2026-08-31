import type { ReaderTodoResponse } from './api/types'

export function todoTimestamp(value: string | null | undefined): number | null {
  if (!value) return null
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? null : parsed
}

export function sortTodos(items: readonly ReaderTodoResponse[]): ReaderTodoResponse[] {
  return [...items].sort((left, right) => {
    if (left.done !== right.done) return Number(left.done) - Number(right.done)
    const leftDue = todoTimestamp(left.due_at)
    const rightDue = todoTimestamp(right.due_at)
    if (leftDue !== null && rightDue !== null && leftDue !== rightDue) return leftDue - rightDue
    if (leftDue !== null && rightDue === null) return -1
    if (leftDue === null && rightDue !== null) return 1
    const created = (todoTimestamp(right.created_at) ?? Number.NEGATIVE_INFINITY) -
      (todoTimestamp(left.created_at) ?? Number.NEGATIVE_INFINITY)
    return created || left.id.localeCompare(right.id)
  })
}
