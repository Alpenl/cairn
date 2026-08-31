import { describe, expect, it } from 'vitest'
import { makeReaderTodo as todo } from '../test/fixtures'
import {
  formatRelativeDate,
  identityIsCurrent,
  isIdentityError,
  readerErrorMessage,
  SURFACE_IDENTITY_ERROR,
  todoDesiredStatePatch,
} from './reader-surface'

describe('SURFACE_IDENTITY_ERROR', () => {
  it('keeps the user-visible identity copy', () => {
    expect(SURFACE_IDENTITY_ERROR).toBe('Reader 身份已失效，请重新连接。')
  })
})

describe('identityIsCurrent', () => {
  it('treats a client without the probe as current', () => {
    expect(identityIsCurrent({})).toBe(true)
  })

  it('follows the probe result', () => {
    expect(identityIsCurrent({ isIdentityCurrent: () => true })).toBe(true)
    expect(identityIsCurrent({ isIdentityCurrent: () => false })).toBe(false)
  })

  it('fails closed when the probe throws', () => {
    expect(identityIsCurrent({ isIdentityCurrent: () => { throw new Error('revoked') } })).toBe(false)
  })
})

describe('isIdentityError', () => {
  it('recognises the identity mismatch shape', () => {
    expect(isIdentityError({ kind: 'identity-mismatch' })).toBe(true)
  })

  it('rejects everything else', () => {
    expect(isIdentityError({ kind: 'http' })).toBe(false)
    expect(isIdentityError({})).toBe(false)
    expect(isIdentityError(null)).toBe(false)
    expect(isIdentityError('identity-mismatch')).toBe(false)
    expect(isIdentityError([{ kind: 'identity-mismatch' }])).toBe(false)
  })
})

describe('readerErrorMessage', () => {
  it('keeps the status copy verbatim', () => {
    expect(readerErrorMessage({ message: '冲突', status: 409 })).toBe('内容已经被其他窗口更新，请刷新后重试。')
    expect(readerErrorMessage({ message: '过期', status: 412 })).toBe('版本已经变化，请刷新后重试。')
    expect(readerErrorMessage({ message: '拒绝', status: 403 })).toBe('当前身份没有执行此操作的权限。')
  })

  it('prefers the server message for other statuses', () => {
    expect(readerErrorMessage({ message: '服务端说明', status: 500 })).toBe('服务端说明')
    expect(readerErrorMessage({ message: '没有状态码的说明' })).toBe('没有状态码的说明')
  })

  it('falls back for empty, missing or non-string messages', () => {
    expect(readerErrorMessage({ message: '' })).toBe('请求失败，请稍后重试。')
    expect(readerErrorMessage({})).toBe('请求失败，请稍后重试。')
    expect(readerErrorMessage({ message: 42 })).toBe('请求失败，请稍后重试。')
  })

  it('falls back for non-record throws', () => {
    expect(readerErrorMessage(undefined)).toBe('请求失败，请稍后重试。')
    expect(readerErrorMessage(null)).toBe('请求失败，请稍后重试。')
    expect(readerErrorMessage('boom')).toBe('请求失败，请稍后重试。')
    expect(readerErrorMessage(['boom'])).toBe('请求失败，请稍后重试。')
  })

  it('reads the message off a thrown Error', () => {
    expect(readerErrorMessage(new Error('想法加载炸了'))).toBe('想法加载炸了')
  })

  it('honours a caller supplied fallback', () => {
    expect(readerErrorMessage(new Error(''), '想法加载失败，请稍后重试。')).toBe('想法加载失败，请稍后重试。')
    expect(readerErrorMessage('boom', '历史想法加载失败，请稍后重试。')).toBe('历史想法加载失败，请稍后重试。')
    // 状态码文案优先于调用方 fallback：宿主协议不受 Surface 措辞影响。
    expect(readerErrorMessage({ message: '', status: 409 }, '想法加载失败，请稍后重试。'))
      .toBe('内容已经被其他窗口更新，请刷新后重试。')
  })
})

// 迁移前 SurfaceShell 导出的 errorMessage 已经删掉，这条留作它的行为契约：
// 结构化 API 错误走同一批文案，一个字都不能变。
describe('readerErrorMessage 对 SurfaceShell.errorMessage 的行为兼容', () => {
  it('matches the shell behaviour it replaces', () => {
    expect(readerErrorMessage({ message: '冲突', status: 409 })).toBe('内容已经被其他窗口更新，请刷新后重试。')
    expect(readerErrorMessage({ message: '过期', status: 412 })).toBe('版本已经变化，请刷新后重试。')
    expect(readerErrorMessage({ message: '拒绝', status: 403 })).toBe('当前身份没有执行此操作的权限。')
    expect(readerErrorMessage({ message: '服务端说明', status: 500 })).toBe('服务端说明')
    expect(readerErrorMessage({ message: '' })).toBe('请求失败，请稍后重试。')
  })
})

describe('formatRelativeDate', () => {
  it('reports missing and malformed timestamps', () => {
    expect(formatRelativeDate(null)).toBe('暂无时间')
    expect(formatRelativeDate(undefined)).toBe('暂无时间')
    expect(formatRelativeDate('')).toBe('暂无时间')
    expect(formatRelativeDate('not-a-date')).toBe('暂无时间')
  })

  it('formats a valid timestamp with the zh-CN surface format', () => {
    const value = '2026-08-10T01:00:00Z'
    const expected = new Intl.DateTimeFormat('zh-CN', {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    }).format(new Date(value))

    expect(formatRelativeDate(value)).toBe(expected)
  })
})

describe('todoDesiredStatePatch', () => {
  it('sends a bare done flag for standalone todos', () => {
    expect(todoDesiredStatePatch(todo(), true)).toEqual({ done: true })
    expect(todoDesiredStatePatch(todo({ done: true }), false)).toEqual({ done: false })
  })

  it('carries the host revision for projected todos', () => {
    const projection = todo({ origin_kind: 'thought', host_revision: 11 })
    expect(todoDesiredStatePatch(projection, true)).toEqual({ done: true, expected_host_revision: 11 })
  })

  it('refuses to patch a projection without a usable host revision', () => {
    expect(todoDesiredStatePatch(todo({ origin_kind: 'thought', host_revision: -1 }), true)).toBeNull()
    expect(todoDesiredStatePatch(todo({ origin_kind: 'thought', host_revision: 1.5 }), true)).toBeNull()
    expect(todoDesiredStatePatch(todo({ origin_kind: 'thought', host_revision: Number.NaN }), true)).toBeNull()
  })
})
