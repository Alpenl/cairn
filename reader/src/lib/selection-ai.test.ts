import { describe, expect, it } from 'vitest'

import { makeLink } from '../test/fixtures'
import { buildSelectionAIRequest, selectionAISourceKey, type SelectionAIDraft } from './selection-ai'

describe('selection AI request privacy', () => {
  it('identifies a Note session with only selected text and minimal position metadata', () => {
    const draft: SelectionAIDraft = {
      annotation: {
        id: 'annotation-note-1',
        blockKey: 'note',
        target: { kind: 'note', noteRevision: 7 },
      },
      text: 'selected phrase',
      nonce: 1,
      source: {
        type: 'note',
        hostId: 'note-1',
        revision: 7,
        start: 12,
        end: 27,
      },
    }
    const request = buildSelectionAIRequest(
      [{ role: 'user', text: '请解释选区' }],
      null,
      draft,
    )
    const serialized = JSON.stringify(request)

    expect(request).toEqual(expect.objectContaining({
      scope: 'selection',
      selected_text: 'selected phrase',
    }))
    expect(request).not.toHaveProperty('link_id')
    expect(request.prompt).toContain('"source_type":"note"')
    expect(request.prompt).toContain('"host_id":"note-1"')
    expect(request.prompt).toContain('"note_revision":7')
    expect(request.prompt).toContain('"start":12,"end":27')
    expect(serialized).not.toContain('UNSELECTED_PRIVATE_SENTINEL')
    expect(serialized).not.toContain('<article class="preview">')
    expect(selectionAISourceKey(null, draft)).toBe('note:note-1:7')
  })

  it('preserves the existing link session contract without serializing browser body content', () => {
    const link = makeLink({
      id: 'link-1',
      title: 'Article title',
      content: 'UNSELECTED_PRIVATE_SENTINEL',
    })
    const request = buildSelectionAIRequest([{ role: 'user', text: '概括' }], link, null)

    expect(request).toMatchObject({ scope: 'general', link_id: 'link-1' })
    expect(request.prompt).toContain('Article title')
    expect(JSON.stringify(request)).not.toContain('UNSELECTED_PRIVATE_SENTINEL')
  })
})
