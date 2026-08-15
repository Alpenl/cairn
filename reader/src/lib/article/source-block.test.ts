import { describe, expect, expectTypeOf, it } from 'vitest'

import {
  isValidLinkId,
  isValidSourceHash,
  sourceBlockKey,
  type SourceBlockId,
} from './source-block'

describe('SourceBlockId', () => {
  it('keeps summary identity independent from saved-content revision', () => {
    const id: SourceBlockId = {
      namespace: 'physical-A',
      linkId: 'L1',
      blockKind: 'summary',
      sourceHash: 'a'.repeat(64),
    }

    expect(sourceBlockKey(id)).toBe(`physical-A\0L1\0summary\0${'a'.repeat(64)}`)
    expectTypeOf<SourceBlockId>().not.toHaveProperty('contentRevision')
  })

  it('rejects retired deep-research as a schedulable source block kind', () => {
    const invalid = {
      namespace: 'physical-A',
      linkId: 'L1',
      blockKind: 'dr',
      sourceHash: 'b'.repeat(64),
    }

    expect(() => sourceBlockKey(invalid as SourceBlockId)).toThrow(/block kind/i)
  })

  it.each([
    ['', 'L1'],
    ['physical-A', ''],
    ['physical\0A', 'L1'],
    ['physical-A', 'L\u00001'],
  ])('rejects ambiguous identity components %#', (namespace, linkId) => {
    expect(() => sourceBlockKey({
      namespace,
      linkId,
      blockKind: 'summary',
      sourceHash: 'a'.repeat(64),
    })).toThrow(/identity/i)
  })

  it.each([
    'a'.repeat(63),
    'a'.repeat(65),
    'A'.repeat(64),
    'g'.repeat(64),
    `${'a'.repeat(32)}\0${'a'.repeat(31)}`,
  ])('rejects a non-canonical source hash %#', (sourceHash) => {
    expect(() => sourceBlockKey({
      namespace: 'physical-A',
      linkId: 'L1',
      blockKind: 'summary',
      sourceHash,
    })).toThrow(/hash/i)
  })

  it('exports the canonical validators used by document and durable stores', () => {
    expect(isValidLinkId('L1')).toBe(true)
    expect(isValidLinkId('L\0other')).toBe(false)
    expect(isValidSourceHash('a'.repeat(64))).toBe(true)
    expect(isValidSourceHash('A'.repeat(64))).toBe(false)
  })
})
