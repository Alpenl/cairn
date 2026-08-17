import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { LinkConversionDialog } from './LinkConversionDialog'
import { makeLink } from '../test/fixtures'
import { enabledReaderCapabilityLease } from '../test/capabilities'
import type { ReaderClient } from '../lib/api/client'

function clientFixture() {
  return {
    isIdentityCurrent: vi.fn(() => true),
    previewLinkConversion: vi.fn(async () => ({ ok: true as const, data: {
      link_id: 'L1', current_kind: 'reading' as const, target_kind: 'site' as const,
      expected_content_revision: 7, destructive: true, saved_original: true,
      translation_count: 2, reparse_required: false,
      annotation_policy: 'extract_local_note_then_hide_stale',
      matching_site: { id: 'S1', name: 'Example', revision: 4 },
    } })),
    convertLink: vi.fn(async () => ({ ok: true as const, data: {
      link_id: 'L1', library_kind: 'site' as const, content_revision: 8, status: 'done',
      site_id: 'S1', site_revision: 5, entry_id: 'E1', reparse_required: false,
      reader_target: { view: 'sites' as const, link_id: 'L1', site_id: 'S1', entry_id: 'E1' },
    } })),
  } as unknown as ReaderClient
}

describe('LinkConversionDialog', () => {
  it('展示破坏性资产并以 preview 的 CAS 条件提交匹配站点', async () => {
    const client = clientFixture()
    const onConverted = vi.fn()
    render(<LinkConversionDialog client={client} capabilityLease={enabledReaderCapabilityLease()} link={makeLink({ id: 'L1', library_kind: 'reading', content_revision: 7, status: 'done' })} initialNote="保留的笔记" onClose={vi.fn()} onConverted={onConverted} onToast={vi.fn()} />)
    expect(await screen.findByText('译文：2 条将删除')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '确认转换' }))
    await waitFor(() => expect(client.convertLink).toHaveBeenCalledWith('L1', expect.objectContaining({ target_site_id: 'S1', expected_site_revision: 4, expected_content_revision: 7, confirm_destructive: true, preserved_user_note: '保留的笔记' })))
    expect(onConverted).toHaveBeenCalledOnce()
  })

  it('允许用户明确选择新建网站而非匹配站点', async () => {
    const client = clientFixture()
    render(<LinkConversionDialog client={client} capabilityLease={enabledReaderCapabilityLease()} link={makeLink({ id: 'L1', library_kind: 'reading', content_revision: 7, status: 'done' })} initialNote="" onClose={vi.fn()} onConverted={vi.fn()} onToast={vi.fn()} />)
    await screen.findByText('附加到“Example”')
    fireEvent.click(screen.getByLabelText('新建网站卡片'))
    fireEvent.click(screen.getByRole('button', { name: '确认转换' }))
    await waitFor(() => expect(client.convertLink).toHaveBeenCalledWith('L1', expect.objectContaining({ target_site_id: undefined, expected_site_revision: undefined })))
  })

  it('保留原有关闭规则：backdrop 可关、Escape 不关', async () => {
    const client = clientFixture()
    const onClose = vi.fn()
    render(<LinkConversionDialog client={client} capabilityLease={enabledReaderCapabilityLease()} link={makeLink({ id: 'L1', library_kind: 'reading', content_revision: 7, status: 'done' })} initialNote="" onClose={onClose} onConverted={vi.fn()} onToast={vi.fn()} />)
    const dialog = await screen.findByRole('dialog', { name: '移到网站收藏' })
    const backdrop = document.querySelector<HTMLElement>('.reader-dialog-backdrop')
    if (!backdrop) throw new Error('backdrop 未渲染')

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()

    fireEvent.mouseDown(dialog)
    expect(onClose).not.toHaveBeenCalled()

    fireEvent.mouseDown(backdrop)
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})
