import { describe, expect, it } from 'vitest'
import { sanitizeFeedHTML } from './feed-html'

describe('sanitizeFeedHTML', () => {
  it('removes executable markup and unsafe attributes', () => {
    const html = sanitizeFeedHTML(
      '<p onclick="evil()" data-track="x">Safe<script>alert(1)</script></p><a href="javascript:evil()" ping="https://tracker.test">bad</a>',
      'https://example.com/post',
    )
    expect(html).toContain('<p>Safe</p>')
    expect(html).not.toContain('script')
    expect(html).not.toContain('onclick')
    expect(html).not.toContain('data-track')
    expect(html).not.toContain('ping=')
    expect(html).not.toContain('javascript:')
  })

  it('resolves relative links but replaces images with non-networking alt placeholders', () => {
    const html = sanitizeFeedHTML(
      '<a href="/docs">Docs</a><img src="./cover.png" style="width:100vw">',
      'https://example.com/posts/one',
    )
    expect(html).toContain('href="https://example.com/docs"')
    expect(html).not.toContain('<img')
    expect(html).not.toContain('cover.png')
    expect(html).toContain('role="img"')
    expect(html).toContain('图片已阻止')
    expect(html).not.toContain('style=')
  })

  it('removes secondary loading surfaces from feed content', () => {
    const html = sanitizeFeedHTML(
      '<img src="/safe.png" srcset="https://tracker.test/large.png 2x"><video poster="https://tracker.test/poster"><source src="https://tracker.test/movie"></video>',
      'https://example.com/post',
    )
    expect(html).not.toContain('<img')
    expect(html).not.toContain('safe.png')
    expect(html).not.toContain('srcset')
    expect(html).not.toContain('video')
    expect(html).not.toContain('tracker.test')
  })

  it('never retains remote, relative, protocol-relative, data, srcset, poster, SVG, or CSS image URLs', () => {
    const html = sanitizeFeedHTML(
      '<img alt="cover" src="http://127.0.0.1/pixel"><img src="http://10.0.0.1/private"><img src="http://169.254.169.254/link-local"><img src="http://192.0.2.1/reserved"><img src="http://2130706433/special"><img src="//tracker.test/pixel"><img src="data:image/png;base64,AA=="><picture><source srcset="https://tracker.test/a 2x"><img src="/relative.png"></picture><video poster="https://tracker.test/poster"></video><svg><image href="https://tracker.test/svg"></image></svg><p style="background:url(https://tracker.test/css)">body</p>',
      'https://example.com/post',
    )
    expect(html).not.toContain('<img')
    expect(html).not.toContain('src=')
    expect(html).not.toContain('srcset')
    expect(html).not.toContain('poster')
    expect(html).not.toContain('tracker.test')
    expect(html).not.toContain('127.0.0.1')
    expect(html).not.toContain('10.0.0.1')
    expect(html).not.toContain('169.254.169.254')
    expect(html).not.toContain('192.0.2.1')
    expect(html).not.toContain('2130706433')
    expect(html).not.toContain('data:image')
    expect(html).toContain('cover')
    expect(html).toContain('<p>body</p>')
  })

  it('preserves the accessible image alternative when a picture wrapper is blocked', () => {
    const html = sanitizeFeedHTML(
      '<picture><source srcset="https://tracker.test/large.png 2x"><img alt="Responsive cover" src="https://tracker.test/cover.png"></picture>',
      'https://example.com/post',
    )

    expect(html).not.toContain('<picture')
    expect(html).not.toContain('<source')
    expect(html).not.toContain('<img')
    expect(html).not.toContain('tracker.test')
    expect(html).toContain('role="img"')
    expect(html).toContain('aria-label="Responsive cover"')
    expect(html).toContain('Responsive cover')
  })

  it('keeps blocked-image placeholders accessible when cached HTML is sanitized again', () => {
    const first = sanitizeFeedHTML(
      '<p>body</p><img alt="Cached cover" src="https://tracker.test/private.png?token=secret">',
      'https://example.com/post',
    )
    const second = sanitizeFeedHTML(first, 'https://example.com/post')

    expect(second).toBe(first)
    expect(second).not.toContain('tracker.test')
    expect(second).not.toContain('token=secret')
    expect(second).toContain('role="img"')
    expect(second).toContain('aria-label="Cached cover"')
  })
})
