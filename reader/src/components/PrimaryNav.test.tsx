import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ReaderRoute } from '../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../test/capabilities'
import { PrimaryNav } from './PrimaryNav'

describe('PrimaryNav', () => {
  it('marks the active mode and routes tab selections', () => {
    const onNavigate = vi.fn<(route: ReaderRoute) => void>()
    render(<PrimaryNav activeLibrary="notes" policy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} />)

    expect(screen.getByRole('tab', { name: '笔记' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tab', { name: '阅读' })).toHaveAttribute('aria-selected', 'false')

    fireEvent.click(screen.getByRole('tab', { name: '订阅' }))
    expect(onNavigate).toHaveBeenCalledWith({ kind: 'library', id: 'subs' })
  })

  it('highlights 阅读 for the sites route it now hosts', () => {
    render(<PrimaryNav activeLibrary="sites" onNavigate={vi.fn()} />)

    // 网站 lost its own entry but kept its route. The highlight has to land on
    // the library that adopted it, otherwise the rail claims the user is
    // nowhere.
    expect(screen.queryByRole('tab', { name: '网站' })).not.toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '阅读' })).toHaveAttribute('aria-selected', 'true')
  })

  it('keeps exactly four library tabs and places surface/tools outside the tablist', () => {
    const onNavigate = vi.fn<(route: ReaderRoute) => void>()
    render(<PrimaryNav activeLibrary="reading" policy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} />)

    expect(screen.getAllByRole('tab')).toHaveLength(4)
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual([
      '收件箱',
      '阅读',
      '订阅',
      '笔记',
    ])
    expect(screen.getByRole('button', { name: '今天' })).not.toHaveAttribute('role', 'tab')
    expect(screen.queryByRole('button', { name: '混合 Feed' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '设置' })).not.toHaveAttribute('role', 'tab')
    // TODO and 想法 were demoted out of the rail: TODO lives in 今天 and in
    // settings, 想法 became a tab inside 笔记. Both routes still work.
    expect(screen.queryByRole('button', { name: 'TODO' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '想法' })).not.toBeInTheDocument()
    // The legacy shell used to draw its own inline divider above 笔记. Both
    // shells now render the shared PrimaryNav, so the only separators are the
    // group borders and every entry keeps one position across routes.
    expect(screen.getByRole('tab', { name: '笔记' })).not.toHaveClass('divider-before')
    expect(screen.getByRole('tablist').closest('.wt-primary-nav')).not.toBeNull()
    expect(screen.getByRole('button', { name: '设置' }).closest('.wt-nav-tools')).not.toBeNull()

    fireEvent.click(screen.getByRole('button', { name: '今天' }))
    fireEvent.click(screen.getByRole('button', { name: '设置' }))

    expect(onNavigate.mock.calls).toEqual([
      [{ kind: 'surface', id: 'home' }],
      [{ kind: 'tool', id: 'settings' }],
    ])
  })

  it('does not select a library tab when a surface route is active', () => {
    render(
      <PrimaryNav
        activeLibrary="reading"
        policy={enabledReaderCapabilityPolicy()}
        activeRoute={{ kind: 'surface', id: 'home' }}
        onNavigate={vi.fn()}
      />,
    )

    expect(screen.getByRole('button', { name: '今天' })).toHaveAttribute('aria-current', 'page')
    expect(screen.getAllByRole('tab').every((tab) => tab.getAttribute('aria-selected') === 'false')).toBe(true)
  })

  it('supports roving keyboard navigation across the library tabs', () => {
    render(<PrimaryNav activeLibrary="reading" policy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()} />)
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
  ] as const)('hides the %s-owned legacy navigation entry when unavailable', (capability, role, label) => {
    render(
      <PrimaryNav
        activeLibrary="reading"
        policy={enabledReaderCapabilityPolicy({ [capability]: false })}
        onNavigate={vi.fn()}
      />,
    )

    expect(screen.queryByRole(role, { name: label })).not.toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '阅读' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '设置' })).toBeInTheDocument()
  })
})
