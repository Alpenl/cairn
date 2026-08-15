import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ThoughtSyncSnapshot } from '../lib/user-data/thought-sync'
import { Titlebar } from './Titlebar'

function renderTitlebar(thoughtSync: ThoughtSyncSnapshot): void {
  render(
    <Titlebar
      theme="light"
      chatOpen={false}
      navigationOpen={false}
      sidebarCollapsed={false}
      syncing={false}
      onSync={vi.fn()}
      onToggleNavigation={vi.fn()}
      onAddLink={vi.fn()}
      onOpenCmdk={vi.fn()}
      onToggleTheme={vi.fn()}
      onToggleChat={vi.fn()}
      onOpenSettings={vi.fn()}
      thoughtSync={thoughtSync}
      archiveDownloading={false}
      onDownloadArchive={vi.fn()}
      canUseAI={true}
      semanticSearchEnabled={true}
      canDownloadArchive={true}
    />,
  )
}

describe('Titlebar Thought sync status', () => {
  it.each([
    [
      'offline',
      { phase: 'offline', pendingCount: 4, blockedCount: 1 },
      '离线 · 4 项待同步',
    ],
    [
      'syncing',
      { phase: 'syncing', pendingCount: 4, blockedCount: 1 },
      '同步中 · 4 项待同步',
    ],
    [
      'failed',
      {
        phase: 'failed',
        pendingCount: 4,
        blockedCount: 1,
        errorCode: 'other:invalid_thought_payload:422',
      },
      '同步失败 · 4 项待同步，1 项阻塞 · other:invalid_thought_payload:422',
    ],
    [
      'pending',
      { phase: 'pending', pendingCount: 4, blockedCount: 0 },
      '待同步 · 4 项待同步',
    ],
    [
      'synced',
      { phase: 'synced', pendingCount: 0, blockedCount: 0, lastSuccessfulSyncAt: 1 },
      '已同步',
    ],
  ] satisfies ReadonlyArray<readonly [string, ThoughtSyncSnapshot, string]>)('renders %s with the durable operation count', (_phase, snapshot, label) => {
    renderTitlebar(snapshot)

    const status = screen.getByRole('status')
    expect(status).toHaveTextContent(label)
    expect(status).toHaveAttribute('data-phase', snapshot.phase)
    expect(status).toHaveAttribute('aria-label', `想法同步状态：${label}`)
  })

  it('keeps the stable error classification readable from the status entry', () => {
    renderTitlebar({
      phase: 'failed',
      pendingCount: 2,
      blockedCount: 1,
      errorCode: 'other:invalid_thought_payload:422',
    })

    const status = screen.getByRole('status')
    expect(status).toHaveAttribute('data-error-code', 'other:invalid_thought_payload:422')
    expect(status).toHaveAttribute(
      'title',
      '想法同步状态：同步失败 · 2 项待同步，1 项阻塞 · other:invalid_thought_payload:422',
    )
  })
})
