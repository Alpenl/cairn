import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { SurfaceShell } from './SurfaceShell'

describe('SurfaceShell', () => {
  it('applies the before-navigation guard to the back button', () => {
    const onBack = vi.fn()
    const onBeforeNavigate = vi.fn(() => false)

    render(
      <SurfaceShell
        title="测试表面"
        capabilityPolicy={enabledReaderCapabilityPolicy()}
        onNavigate={vi.fn()}
        onBack={onBack}
        onBeforeNavigate={onBeforeNavigate}
      >
        <p>内容</p>
      </SurfaceShell>,
    )

    fireEvent.click(screen.getByRole('button', { name: '返回' }))

    expect(onBeforeNavigate).toHaveBeenCalledTimes(1)
    expect(onBack).not.toHaveBeenCalled()
  })

  it('applies the before-navigation guard to nav commands', () => {
    const onNavigate = vi.fn()
    const onBeforeNavigate = vi.fn(() => false)

    render(
      <SurfaceShell
        title="首页"
        activeRoute={{ kind: 'surface', id: 'home' }}
        capabilityPolicy={enabledReaderCapabilityPolicy()}
        onNavigate={onNavigate}
        onBeforeNavigate={onBeforeNavigate}
      >
        <p>内容</p>
      </SurfaceShell>,
    )

    fireEvent.click(screen.getByRole('tab', { name: '阅读' }))

    expect(onBeforeNavigate).toHaveBeenCalledTimes(1)
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('leaves identity-owned route persistence to MainView', () => {
    const route: ReaderRoute = { kind: 'library', id: 'notes' }
    window.history.replaceState({}, '', '/?view=notes&note_id=N9')
    localStorage.clear()

    render(
      <SurfaceShell title="笔记" activeRoute={route} capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()}>
        <p>内容</p>
      </SurfaceShell>,
    )

    expect(localStorage.length).toBe(0)
  })
})
