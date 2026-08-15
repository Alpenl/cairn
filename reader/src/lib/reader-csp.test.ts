import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

describe('Reader Content-Security-Policy', () => {
  it('keeps untrusted images on static same-origin, data, and blob sources only', () => {
    const html = readFileSync(resolve(process.cwd(), 'index.html'), 'utf8')
    const content = html.match(
      /<meta\s+http-equiv="Content-Security-Policy"\s+content="([^"]+)"\s*\/>/,
    )?.[1]

    expect(content).toBeDefined()
    const imageDirective = content
      ?.split(';')
      .map((directive) => directive.trim())
      .find((directive) => directive.startsWith('img-src '))
    expect(imageDirective).toBe("img-src 'self' data: blob:")
    expect(imageDirective).not.toMatch(/https?:|\*/)
    expect(content).toContain("media-src 'none'")
    expect(content).toContain("object-src 'none'")
  })
})
