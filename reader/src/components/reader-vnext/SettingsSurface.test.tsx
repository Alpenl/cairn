import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { READER_STARTUP_PREFERENCE_STORAGE_KEY } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { SettingsSurface } from './SettingsSurface'

describe('SettingsSurface', () => {
  afterEach(() => {
    localStorage.clear()
  })

  it('shows and persists the Reader startup preference', () => {
    localStorage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, 'reading')

    render(<SettingsSurface capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()} onOpenConnectionSettings={vi.fn()} />)

    expect(screen.getByRole('button', { name: '直接阅读' })).toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(screen.getByRole('button', { name: '总是今天' }))

    expect(localStorage.getItem(READER_STARTUP_PREFERENCE_STORAGE_KEY)).toBe('home')
    expect(screen.getByRole('button', { name: '总是今天' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('已保存。')
  })

  it('keeps connection settings and workspace navigation separate from startup preference changes', () => {
    const onNavigate = vi.fn()
    const onOpenConnectionSettings = vi.fn()

    render(<SettingsSurface capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} onOpenConnectionSettings={onOpenConnectionSettings} />)

    fireEvent.click(screen.getByRole('button', { name: '记住上次位置' }))
    expect(onNavigate).not.toHaveBeenCalled()
    expect(onOpenConnectionSettings).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: /打开连接设置/ }))
    // TODO and the archived thoughts were demoted out of the rail; settings is
    // the explicit path that keeps them reachable.
    fireEvent.click(screen.getByRole('button', { name: /打开 TODO/ }))
    fireEvent.click(screen.getByRole('button', { name: /查看已归档/ }))

    expect(onOpenConnectionSettings).toHaveBeenCalledTimes(1)
    expect(onNavigate).toHaveBeenNthCalledWith(1, { kind: 'tool', id: 'todo' })
    expect(onNavigate).toHaveBeenNthCalledWith(2, { kind: 'tool', id: 'history' }, { thoughtView: 'history' })
  })

  it('hides an unavailable Home preference without rewriting the stored choice', () => {
    localStorage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, 'home')

    render(<SettingsSurface capabilityPolicy={enabledReaderCapabilityPolicy({ home: false })} onNavigate={vi.fn()} onOpenConnectionSettings={vi.fn()} />)

    expect(screen.queryByRole('button', { name: '总是首页' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '直接阅读' })).toHaveAttribute('aria-pressed', 'true')
    expect(localStorage.getItem(READER_STARTUP_PREFERENCE_STORAGE_KEY)).toBe('home')
  })
})
