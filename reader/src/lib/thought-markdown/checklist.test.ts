import {
  HostChecklistAdapter,
  listChecklistBlocks,
  updateChecklistState,
  updateChecklistStateCAS,
} from './checklist'

describe('thought Markdown checklist adapter', () => {
  it('uses surrounding source context instead of line indexes', () => {
    const source = '# Plan\n\n- [ ] First\n- [x] Second\n'
    const blocks = listChecklistBlocks(source)
    expect(blocks).toHaveLength(2)
    const first = blocks[0]
    const result = updateChecklistState('# Plan\n\nIntro inserted.\n- [ ] First\n- [x] Second\n', first.blockRef, true)
    expect(result.status).toBe('updated')
    if (result.status === 'updated') expect(result.source).toContain('- [x] First')
  })

  it('keeps the blockRef stable when unrelated content follows the task', () => {
    const before = listChecklistBlocks('# Plan\n\n- [ ] First\nContext\n')[0]
    const after = listChecklistBlocks('# Plan\n\n- [ ] First\nContext\n\nA paragraph was inserted.\n')[0]
    expect(after.blockRef).toBe(before.blockRef)
  })

  it('keeps the blockRef stable when an adjacent line is inserted or deleted', () => {
    const before = listChecklistBlocks('# Plan\n\n- [ ] First\nContext\n')[0]
    const inserted = listChecklistBlocks('# Plan\n\n- [ ] First\nInserted nearby\nContext\n')[0]
    const deleted = listChecklistBlocks('# Plan\n\n- [ ] First\n')[0]
    expect(inserted.blockRef).toBe(before.blockRef)
    expect(deleted.blockRef).toBe(before.blockRef)
  })

  it('ignores checklist-looking lines inside fenced code', () => {
    const blocks = listChecklistBlocks('```md\n- [ ] example\n```\n# Plan\n- [ ] real\n')
    expect(blocks).toHaveLength(1)
    expect(blocks[0].text).toBe('real')
  })

  it('matches the server blockRef test vector', () => {
    const blocks = listChecklistBlocks('# Plan\r\n\r\n- [ ] First\r\n- [x] Second\r\n')
    expect(blocks[0].blockRef).toBe('task:2d2c2e9f')
    expect(listChecklistBlocks('## 中文\n- [ ] 同一个任务\n下一行\n')[0].blockRef).toBe('task:ded004d5')
  })

  it('keeps short server blockRef hashes unpadded', () => {
    expect(listChecklistBlocks('- [ ] task-23\n')[0].blockRef).toBe('task:330a484')
  })

  it('can still update a projection carrying the old padded anchor', () => {
    const result = updateChecklistState('- [ ] task-23\n', 'task:0330a484', true, 1)
    expect(result).toMatchObject({ status: 'updated', source: '- [x] task-23\n' })
  })

  it('can update a projection carrying the pre-stability anchor', () => {
    const result = updateChecklistState(
      '# Plan\n\n- [ ] First\n- [x] Second\n',
      'task:307967ac',
      true,
      1,
    )
    expect(result).toMatchObject({ status: 'updated' })
    if (result.status === 'updated') expect(result.source).toContain('- [x] First')
  })

  it('only changes the selected checkbox marker and preserves all other bytes', () => {
    const source = '- [ ] keep  \r\ntext\n'
    const block = listChecklistBlocks(source)[0]
    const result = updateChecklistState(source, block.blockRef, true)
    expect(result).toMatchObject({ status: 'updated' })
    if (result.status === 'updated') expect(result.source).toBe('- [x] keep  \r\ntext\n')
  })

  it('reports ambiguous repeated tasks instead of writing the wrong one', () => {
    const source = '- [ ] same\ncontext\n- [ ] same\ncontext\n'
    const [first, second] = listChecklistBlocks(source)
    expect(first.blockRef).toBe(second.blockRef)
    expect(first.occurrence).toBe(1)
    expect(second.occurrence).toBe(2)
    const updated = updateChecklistState(source, second.blockRef, true, second.occurrence)
    expect(updated.status).toBe('updated')
    if (updated.status === 'updated') expect(updated.source).toBe('- [ ] same\ncontext\n- [x] same\ncontext\n')
    const legacyDefault = updateChecklistState(source, first.blockRef, true, 0)
    expect(legacyDefault).toMatchObject({ status: 'updated', source: '- [x] same\ncontext\n- [ ] same\ncontext\n' })
    const missing = updateChecklistState(source, 'task:does-not-exist', true)
    expect(missing).toEqual({ status: 'not-found' })
    expect(updateChecklistState(source, first.blockRef, true)).toEqual({ status: 'ambiguous' })
  })

  it('uses expected state as a compare-and-swap guard while writing a desired state', () => {
    const source = '- [ ] ship it\n'
    const block = listChecklistBlocks(source)[0]
    expect(updateChecklistState(source, block.blockRef, true, block.occurrence, true)).toEqual({
      status: 'conflict',
      expectedDone: true,
      actualDone: false,
    })

    const updated = updateChecklistStateCAS(source, block.blockRef, false, true, block.occurrence)
    expect(updated).toMatchObject({ status: 'updated' })
    if (updated.status === 'updated') {
      expect(updated.source).toBe('- [x] ship it\n')
      expect(updated.block.done).toBe(true)

      const undone = updateChecklistStateCAS(updated.source, updated.block.blockRef, true, false, updated.block.occurrence)
      expect(undone).toMatchObject({ status: 'updated' })
      if (undone.status === 'updated') {
        expect(undone.source).toBe('- [ ] ship it\n')
        expect(undone.block.done).toBe(false)
      }
    }
  })

  it('combines desired-state checkbox CAS with host revision CAS', async () => {
    let snapshot = { source: '- [ ] ship it\n', revision: 4 }
    const write = vi.fn(async ({ source, expectedRevision }: { source: string; expectedRevision: number }) => {
      if (expectedRevision !== snapshot.revision) {
        return { status: 'conflict' as const, revision: snapshot.revision }
      }
      snapshot = { source, revision: snapshot.revision + 1 }
      return { status: 'updated' as const, source, revision: snapshot.revision }
    })
    const adapter = new HostChecklistAdapter({
      read: () => snapshot,
      write,
    })
    const block = listChecklistBlocks(snapshot.source)[0]
    await expect(adapter.update({
      blockRef: block.blockRef,
      occurrence: block.occurrence,
      expectedDone: false,
      desiredDone: true,
      expectedRevision: 4,
    })).resolves.toMatchObject({ status: 'updated', revision: 5 })
    expect(write).toHaveBeenCalledWith({ source: '- [x] ship it\n', expectedRevision: 4 })

    await expect(adapter.update({
      blockRef: block.blockRef,
      occurrence: block.occurrence,
      expectedDone: true,
      desiredDone: false,
      expectedRevision: 4,
    })).resolves.toMatchObject({
      status: 'conflict',
      conflict: 'host',
      expectedRevision: 4,
      actualRevision: 5,
    })
  })

  it('does not write when the desired checkbox state is already present', async () => {
    const write = vi.fn()
    const adapter = new HostChecklistAdapter({
      read: () => ({ source: '- [x] already done\n', revision: 8 }),
      write,
    })
    const block = listChecklistBlocks('- [x] already done\n')[0]

    await expect(adapter.update({
      blockRef: block.blockRef,
      occurrence: block.occurrence,
      expectedDone: true,
      desiredDone: true,
    })).resolves.toMatchObject({ status: 'updated', revision: 8 })
    expect(write).not.toHaveBeenCalled()
  })
})
