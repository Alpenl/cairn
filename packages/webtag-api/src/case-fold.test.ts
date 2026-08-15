import { describe, expect, it } from 'vitest'
import { foldUnicodeCase } from './case-fold'

describe('foldUnicodeCase', () => {
  it('uses Unicode default case folding for Greek sigma variants', () => {
    expect(foldUnicodeCase('\u03a3')).toBe('\u03c3')
    expect(foldUnicodeCase('\u03a3')).toBe(foldUnicodeCase('\u03c2'))
  })

  it('uses full multi-code-point folding for German sharp S', () => {
    expect(foldUnicodeCase('Stra\u00dfe')).toBe('strasse')
    expect(foldUnicodeCase('Stra\u00dfe')).toBe(foldUnicodeCase('STRASSE'))
  })
})
