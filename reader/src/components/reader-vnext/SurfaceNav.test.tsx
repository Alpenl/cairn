import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { SurfaceNav } from './SurfaceNav'

describe('SurfaceNav', () => {
  it('keeps exactly four library tabs and places surfaces/tools outside the rail', () => {
    const onNavigate = vi.fn<(route: ReaderRoute) => void>()

    render(<SurfaceNav activeRoute={{ kind: 'surface', id: 'home' }} capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} />)

    expect(screen.getAllByRole('tab')).toHaveLength(4)
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual([
      '收件箱',
      '阅读',
      '订阅',
      '笔记',
    ])
    expect(screen.getByRole('button', { name: '今天' })).toHaveAttribute('aria-current', 'page')
    expect(screen.queryByRole('button', { name: '混合 Feed' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '设置' })).not.toHaveAttribute('role', 'tab')
    // TODO moved into 今天 and settings; 想法 became a tab inside 笔记.
    expect(screen.queryByRole('button', { name: 'TODO' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '想法' })).not.toBeInTheDocument()
    expect(screen.getAllByRole('tab').every((tab) => tab.getAttribute('aria-selected') === 'false')).toBe(true)
  })

  it('emits canonical route values for a library tab and a tool', () => {
    const onNavigate = vi.fn<(route: ReaderRoute) => void>()

    render(<SurfaceNav active="reading" capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} />)

    fireEvent.click(screen.getByRole('tab', { name: '笔记' }))
    fireEvent.click(screen.getByRole('button', { name: '设置' }))

    expect(onNavigate.mock.calls).toEqual([
      [{ kind: 'library', id: 'notes' }],
      [{ kind: 'tool', id: 'settings' }],
    ])
  })

  it('supports roving keyboard navigation across the library tabs', () => {
    render(<SurfaceNav active="reading" capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()} />)
    const tabs = screen.getAllByRole('tab')
    tabs[1].focus()
    fireEvent.keyDown(tabs[1], { key: 'ArrowRight' })
    expect(document.activeElement).toBe(tabs[2])
    fireEvent.keyDown(tabs[2], { key: 'Home' })
    expect(document.activeElement).toBe(tabs[0])
    fireEvent.keyDown(tabs[0], { key: 'End' })
    expect(document.activeElement).toBe(tabs[tabs.length - 1])
  })

  it.each([
    ['home', 'button', '今天'],
    ['inbox', 'tab', '收件箱'],
    ['notes', 'tab', '笔记'],
  ] as const)('hides the %s-owned navigation entry when unavailable', (capability, role, label) => {
    render(
      <SurfaceNav
        active="reading"
        capabilityPolicy={enabledReaderCapabilityPolicy({ [capability]: false })}
        onNavigate={vi.fn()}
      />,
    )

    expect(screen.queryByRole(role, { name: label })).not.toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '阅读' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '设置' })).toBeInTheDocument()
  })
})
