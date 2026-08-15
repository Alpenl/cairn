/**
 * capture-normalize.test.ts — 采集数据归一化纯函数单元测试。
 *
 * 全部为纯计算测试：无 IO、无 mock、无 fake timer。
 *
 * 覆盖：
 *   - truncateToBytes：ASCII / 多字节 UTF-8 边界正确性、不切断多字节字符、
 *     512 KiB 边界、空串与无需截断的快速路径
 *   - byteLength：ASCII / 多字节字符的 UTF-8 字节长度
 *   - toIngestSource：browser_capture 组装、备注透传、正文截断，
 *     以及已废弃媒体字段的兼容空值
 */
import { describe, expect, it } from 'vitest'
import type { RawCapture } from '@/contentScripts/capture'
import {
  MAX_TEXT_BYTES,
  byteLength,
  toIngestSource,
  truncateToBytes,
} from './capture-normalize'

// ── byteLength ──────────────────────────────────────────────

describe('byteLength', () => {
  it('ASCII 字符 1 字节/字符', () => {
    expect(byteLength('hello')).toBe(5)
  })

  it('空串为 0 字节', () => {
    expect(byteLength('')).toBe(0)
  })

  it('中文字符 3 字节/字符（UTF-8）', () => {
    expect(byteLength('中文')).toBe(6)
  })

  it('emoji（4 字节）正确计数', () => {
    // 😀 在 UTF-8 下占 4 字节。
    expect(byteLength('😀')).toBe(4)
  })
})

// ── truncateToBytes ─────────────────────────────────────────

describe('truncateToBytes', () => {
  it('未超限时原样返回（快速路径）', () => {
    expect(truncateToBytes('hello', 100)).toBe('hello')
  })

  it('空串原样返回', () => {
    expect(truncateToBytes('', 10)).toBe('')
  })

  it('ASCII 超限时按字节截断', () => {
    expect(truncateToBytes('abcdefghij', 4)).toBe('abcd')
  })

  it('多字节字符不被切成两半 —— 截断结果仍是合法 UTF-8', () => {
    // '中' = 3 字节。上限 4 字节只能放下 1 个完整 '中'（3 字节），
    // 第 2 个 '中' 会让总数到 6 > 4，必须整体丢弃，不能留半个字符。
    const result = truncateToBytes('中中中', 4)
    expect(result).toBe('中')
    expect(byteLength(result)).toBeLessThanOrEqual(4)
  })

  it('上限恰好落在多字节字符中间时退回到上一个完整字符', () => {
    // '中' = 3 字节。上限 5：放 1 个 '中'（3 字节）后还剩 2 字节，
    // 放不下第 2 个 '中'（需 3 字节），结果只保留 1 个。
    const result = truncateToBytes('中中', 5)
    expect(result).toBe('中')
    expect(byteLength(result)).toBe(3)
  })

  it('上限恰好等于内容字节数时原样返回', () => {
    // '中中' = 6 字节，上限 6 应完整保留。
    expect(truncateToBytes('中中', 6)).toBe('中中')
  })

  it('混合 ASCII 与多字节字符按字节边界截断', () => {
    // 'ab' = 2 字节，'中' = 3 字节。上限 4：'ab' + 半个 '中' 放不下，
    // 结果为 'ab'（2 字节）。
    expect(truncateToBytes('ab中', 4)).toBe('ab')
  })

  it('上限容不下完整 emoji 时不返回孤立代理项', () => {
    expect(truncateToBytes('😀', 3)).toBe('')
  })

  it('代理对完整保留：上限足够容纳整个 emoji 时不切分', () => {
    // '😀' = 4 字节，上限 4 应完整保留。
    expect(truncateToBytes('😀', 4)).toBe('😀')
  })

  it('512 KiB 边界：恰好 MAX_TEXT_BYTES 的 ASCII 内容不被截断', () => {
    const exact = 'a'.repeat(MAX_TEXT_BYTES)
    expect(truncateToBytes(exact, MAX_TEXT_BYTES)).toBe(exact)
  })

  it('512 KiB 边界：超出 1 字节的 ASCII 内容被截到 MAX_TEXT_BYTES', () => {
    const over = 'a'.repeat(MAX_TEXT_BYTES + 1)
    const result = truncateToBytes(over, MAX_TEXT_BYTES)
    expect(result.length).toBe(MAX_TEXT_BYTES)
    expect(byteLength(result)).toBe(MAX_TEXT_BYTES)
  })

  it('512 KiB 边界：多字节内容超限后不溢出', () => {
    // 每个 '中' 3 字节，构造略超 512 KiB 的内容。
    const charCount = Math.ceil(MAX_TEXT_BYTES / 3) + 10
    const over = '中'.repeat(charCount)
    const result = truncateToBytes(over, MAX_TEXT_BYTES)
    expect(byteLength(result)).toBeLessThanOrEqual(MAX_TEXT_BYTES)
    // 截断后应尽量逼近上限：差距不超过一个字符的字节数（3）。
    expect(byteLength(result)).toBeGreaterThan(MAX_TEXT_BYTES - 3)
  })
})

// ── toIngestSource ──────────────────────────────────────────

/** 构造一个最小 RawCapture。 */
function makeRaw(overrides: Partial<RawCapture> = {}): RawCapture {
  return {
    url: 'https://example.com/article',
    title: '示例文章',
    text: '正文内容',
    html: '<html><body>正文</body></html>',
    imageUrls: [],
    metadata: { capture_source: 'browser_extension' },
    ...overrides,
  }
}

describe('toIngestSource', () => {
  it('组装为 browser_capture，透传 url/title/metadata', () => {
    const source = toIngestSource(makeRaw(), '')
    expect(source.kind).toBe('browser_capture')
    expect(source.url).toBe('https://example.com/article')
    expect(source.title).toBe('示例文章')
    expect(source.metadata.capture_source).toBe('browser_extension')
  })

  it('非空备注随 metadata.note 透传（trim 后）', () => {
    const source = toIngestSource(makeRaw(), '  我的备注  ')
    expect(source.metadata.note).toBe('我的备注')
  })

  it('空备注不写入 metadata.note', () => {
    const source = toIngestSource(makeRaw(), '   ')
    expect('note' in source.metadata).toBe(false)
  })

  it('普通采集发送正文结构但不发送页面图片列表', () => {
    const source = toIngestSource(
      makeRaw({
        html: '<html><body>完整页面</body></html>',
        imageUrls: [
          'https://example.com/cover.png',
          'https://example.com/content.png',
        ],
      }),
      '',
    )

    expect(source.html).toBe('<html><body>完整页面</body></html>')
    expect(source.image_urls).toEqual([])
  })

  it('正文最多发送 512 KiB', () => {
    const maxTextBytes = 512 * 1024
    const source = toIngestSource(
      makeRaw({ text: 'a'.repeat(maxTextBytes + 1) }),
      '',
    )

    expect(byteLength(source.text)).toBe(maxTextBytes)
  })

  it('正文 HTML 结构最多发送 512 KiB', () => {
    const maxHTMLBytes = 512 * 1024
    const source = toIngestSource(
      makeRaw({ html: `<article>${'界'.repeat(maxHTMLBytes)}</article>` }),
      '',
    )

    expect(byteLength(source.html)).toBeLessThanOrEqual(maxHTMLBytes)
  })
})
