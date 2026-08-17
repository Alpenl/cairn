import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ReaderRoute } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { navigateReaderTarget } from '../../lib/reader-surface'
import { SurfaceShell } from './SurfaceShell'

describe('SurfaceShell', () => {
  it('does not replace the URL when the navigation callback rejects a target', () => {
    window.history.replaceState({}, '', '/?view=reading&link_id=keep-me')
    const onNavigate = vi.fn(() => false)
    const onPopState = vi.fn()
    window.addEventListener('popstate', onPopState)

    try {
      navigateReaderTarget({ kind: 'library', id: 'notes' }, onNavigate, { noteId: 'N9' })

      expect(onNavigate).toHaveBeenCalledWith(
        { kind: 'library', id: 'notes' },
        { noteId: 'N9' },
      )
      expect(window.location.search).toBe('?view=reading&link_id=keep-me')
      expect(onPopState).not.toHaveBeenCalled()
    } finally {
      window.removeEventListener('popstate', onPopState)
    }
  })

  it('delegates a resource target without committing history in the surface', () => {
    window.history.replaceState({}, '', '/?view=reading&link_id=keep-me')
    const onNavigate = vi.fn(() => true)
    const onPopState = vi.fn()
    window.addEventListener('popstate', onPopState)

    try {
      navigateReaderTarget({ kind: 'library', id: 'notes' }, onNavigate, { noteId: 'N9' })

      expect(onNavigate).toHaveBeenCalledWith(
        { kind: 'library', id: 'notes' },
        { noteId: 'N9' },
      )
      expect(window.location.search).toBe('?view=reading&link_id=keep-me')
      expect(onPopState).not.toHaveBeenCalled()
    } finally {
      window.removeEventListener('popstate', onPopState)
    }
  })

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
