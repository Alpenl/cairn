import {
  buildReanchorQuote,
  reanchorAnnotation,
  type ReanchorAnnotation,
} from './index'

function annotation(source: string, start: number, end: number): ReanchorAnnotation {
  return {
    id: 'thought-1',
    blockKey: 'content',
    range: { start, end },
    quote: buildReanchorQuote(source, { start, end }),
    target: { kind: 'link-content', hostId: 'link-1', version: 'content:1' },
    thought: 'keep this',
  }
}

describe('reanchor kernel', () => {
  it('follows a unique quote after insertion before the range', () => {
    const oldSource = 'A paragraph with a durable phrase in it.'
    const currentSource = 'A paragraph with an inserted sentence and a durable phrase in it.'
    const start = oldSource.indexOf('durable phrase')
    const result = reanchorAnnotation(annotation(oldSource, start, start + 'durable phrase'.length), oldSource, currentSource, {
      kind: 'link-content',
      hostId: 'link-1',
      version: 'content:2',
    })
    expect(result.status).toBe('reanchored')
    if (result.status === 'reanchored') {
      expect(currentSource.slice(result.annotation.range.start, result.annotation.range.end)).toBe('durable phrase')
      expect(result.annotation.target.version).toBe('content:2')
    }
  })

  it('uses prefix and suffix to disambiguate repeated text', () => {
    const oldSource = 'alpha target omega; beta target theta'
    const start = oldSource.indexOf('target')
    const result = reanchorAnnotation(annotation(oldSource, start, start + 6), 'old ' + oldSource, oldSource)
    expect(result.status).toBe('reanchored')
    if (result.status === 'reanchored') expect(result.annotation.range.start).toBe(start)
  })

  it('archives ambiguous or missing quotes instead of guessing', () => {
    const oldSource = 'same text appears once here and same text appears once there'
    const start = oldSource.indexOf('same text')
    const ambiguous = reanchorAnnotation({
      ...annotation(oldSource, start, start + 9),
      quote: { exact: 'same text', prefix: '', suffix: '' },
    }, oldSource, oldSource)
    expect(ambiguous).toMatchObject({ status: 'historical', reason: 'ambiguous-quote' })

    const missing = reanchorAnnotation(annotation('keep phrase', 5, 11), 'keep phrase', 'removed entirely')
    expect(missing).toMatchObject({ status: 'historical', reason: 'missing-quote' })
  })

  it('maps a replaced selection through one unique pair of context anchors', () => {
    const oldSource = 'Intro: keep the durable phrase before the ending.'
    const currentSource = 'Intro: keep the rewritten sentence before the ending.'
    const start = oldSource.indexOf('durable phrase')
    const result = reanchorAnnotation(
      annotation(oldSource, start, start + 'durable phrase'.length),
      oldSource,
      currentSource,
      { kind: 'note', hostId: 'note-1', version: 'note:2' },
    )

    expect(result).toMatchObject({ status: 'reanchored', reason: 'diff-context' })
    if (result.status === 'reanchored') {
      expect(currentSource.slice(result.annotation.range.start, result.annotation.range.end)).toBe('rewritten sentence')
      expect(result.annotation.quote.exact).toBe('rewritten sentence')
    }
  })

  it('keeps a changed selection historical when the context pair is ambiguous', () => {
    const oldSource = 'A durable phrase appears here; another durable phrase appears there.'
    const currentSource = 'A changed phrase appears here; another changed phrase appears there.'
    const start = oldSource.indexOf('durable phrase')
    const result = reanchorAnnotation(
      {
        ...annotation(oldSource, start, start + 'durable phrase'.length),
        quote: { exact: 'durable phrase', prefix: 'A ', suffix: ' appears' },
      },
      oldSource,
      currentSource,
    )

    expect(result).toMatchObject({ status: 'historical', reason: 'ambiguous-quote' })
  })

  it('does not accept a unique exact when its stored context is invalid', () => {
    const source = 'A durable phrase appears here.'
    const start = source.indexOf('durable phrase')
    const result = reanchorAnnotation(
      {
        ...annotation(source, start, start + 'durable phrase'.length),
        quote: { exact: 'durable phrase', prefix: 'unrelated ', suffix: ' context' },
      },
      source,
      source,
    )

    expect(result).toMatchObject({ status: 'historical', reason: 'missing-quote' })
  })

  it('keeps a unique exact quote after its surrounding context is deleted', () => {
    const oldSource = 'before durable phrase after'
    const start = oldSource.indexOf('durable phrase')
    const result = reanchorAnnotation(
      annotation(oldSource, start, start + 'durable phrase'.length),
      oldSource,
      'durable phrase',
    )

    expect(result).toMatchObject({ status: 'reanchored', reason: 'diff-context' })
    if (result.status === 'reanchored') {
      expect(result.annotation.range).toEqual({ start: 0, end: 'durable phrase'.length })
    }
  })

  it('keeps repeated exact quotes historical when context no longer disambiguates them', () => {
    const oldSource = 'before durable phrase after; another durable phrase later'
    const start = oldSource.indexOf('durable phrase')
    const result = reanchorAnnotation(
      annotation(oldSource, start, start + 'durable phrase'.length),
      oldSource,
      'durable phrase; durable phrase',
    )

    expect(result).toMatchObject({ status: 'historical', reason: 'ambiguous-quote' })
  })
})
