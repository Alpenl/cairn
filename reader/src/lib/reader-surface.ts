// vNext Surface 共用的非组件工具：身份边界、错误文案、相对时间和 TODO 的
// CAS patch。这些实现原先挂在 SurfaceShell.tsx 上，导致组件文件必须为每个
// helper 写一条 react-refresh 例外；搬到这里后 SurfaceShell.tsx 只剩组件。
//
// 所有用户可见文案与 SurfaceShell 中的原实现逐字一致。
import { asRecord } from './records'
import type { ReaderTodoPatchRequest, ReaderTodoResponse } from './api/types'
import type { ReaderNavigationRequest, ReaderRoute, ReaderRouteTargets } from './navigation/route'

export const SURFACE_IDENTITY_ERROR = 'Reader 身份已失效，请重新连接。'

const GENERIC_REQUEST_ERROR = '请求失败，请稍后重试。'

// Keep the surface boundary tolerant of the small client doubles used by
// older embedded hosts while still failing closed for a revoked lease.
export function identityIsCurrent(client: { readonly isIdentityCurrent?: () => boolean }): boolean {
  try {
    return typeof client.isIdentityCurrent !== 'function' || client.isIdentityCurrent()
  } catch {
    return false
  }
}

export function isIdentityError(value: unknown): boolean {
  return asRecord(value)?.kind === 'identity-mismatch'
}

/**
 * 把任意抛出物或 API 错误对象翻译成 Surface 展示的文案。
 *
 * 状态码文案是宿主协议的一部分（409 / 412 / 403），逐字保持现状。既覆盖
 * `errorMessage({ message, status })` 这种结构化错误，也覆盖各 Surface 本地
 * `thrownErrorMessage(cause)` 处理的 unknown 抛出物。
 */
export function readerErrorMessage(value: unknown, fallback: string = GENERIC_REQUEST_ERROR): string {
  const record = asRecord(value)
  if (!record) return fallback
  const status = typeof record.status === 'number' ? record.status : undefined
  if (status === 409) return '内容已经被其他窗口更新，请刷新后重试。'
  if (status === 412) return '版本已经变化，请刷新后重试。'
  if (status === 403) return '当前身份没有执行此操作的权限。'
  return typeof record.message === 'string' && record.message ? record.message : fallback
}

/**
 * @deprecated 调用方改为 `readerErrorMessage` 后删除；行为与它完全一致。
 */
export function errorMessage(error: { readonly message: string; readonly status?: number }): string {
  return readerErrorMessage(error)
}

export function formatRelativeDate(value: string | null | undefined): string {
  if (!value) return '暂无时间'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '暂无时间'
  return new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date)
}

/** Build a patch from the user's desired done state, preserving the host CAS boundary. */
export function todoDesiredStatePatch(todo: ReaderTodoResponse, done: boolean): ReaderTodoPatchRequest | null {
  if (todo.origin_kind === 'standalone') return { done }
  if (!Number.isSafeInteger(todo.host_revision) || todo.host_revision < 0) return null
  return { done, expected_host_revision: todo.host_revision }
}

// Keep a source target in the URL when a surface delegates navigation to the
// legacy app shell. The route callback changes the rendered surface; the
// replace plus popstate carries the exact resource identity to MainView.
/** @deprecated Todo 改为直接调用 navigation callback 后删除。 */
export function navigateReaderTarget(
  route: ReaderRoute,
  onNavigate: ReaderNavigationRequest,
  targets: ReaderRouteTargets = {},
): void {
  if (Object.keys(targets).length === 0) onNavigate(route)
  else onNavigate(route, targets)
}
