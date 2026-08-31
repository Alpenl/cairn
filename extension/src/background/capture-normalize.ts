/**
 * @module background/capture-normalize
 * 采集数据归一化 —— 纯函数，无 IO，无副作用。
 *
 * 职责：把页面抓取的原始 RawCapture 裁剪、组装为后端 browser_capture
 * IngestSource。所有逻辑都是纯计算（字节长度、UTF-8 截断、字段裁剪），
 * 不触碰 chrome.* / 网络 / 内存状态，因此可被独立单元测试。
 *
 * 普通采集请求只携带正文：正文按 UTF-8 字节安全截断到 512 KiB，
 * html 发送脱敏后的结构快照；image_urls 保留协议字段但固定为空
 * （扩展从不发送任何图像 URL，见 contentScripts/capture.ts）。
 *
 * 消费者：src/background/captureHandler.ts（采集编排）。
 */

import type { RawCapture } from '@/contentScripts/capture'
import type { IngestSource } from '@/api/types'

// ── 调优常量 ────────────────────────────────────────────────

/** 普通浏览器采集正文的请求预算（512 KiB）。超出则截断。 */
export const MAX_TEXT_BYTES = 512 * 1024
const MAX_HTML_BYTES = 512 * 1024

// ── 字节工具 ────────────────────────────────────────────────

/** UTF-8 字节长度。 */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

/**
 * 按 UTF-8 字节长度截断字符串到 maxBytes 以内。
 *
 * 用二分定位「编码后不超过 maxBytes」的最长前缀。每个候选位置
 * 都会退回到完整 Unicode 码点边界，因此中文和 emoji 都不会被切成半个字符。
 */
export function truncateToBytes(value: string, maxBytes: number): string {
  const encoder = new TextEncoder()
  // 整体编码一次：未超上限直接返回原值，省去二分。
  if (encoder.encode(value).length <= maxBytes) return value
  let lo = 0
  let hi = value.length
  let best = 0
  // 二分找到「编码后不超过 maxBytes」的最长完整码点前缀。
  while (lo <= hi) {
    const mid = Math.floor((lo + hi) / 2)
    let end = mid
    if (
      end > 0 &&
      end < value.length &&
      value.charCodeAt(end - 1) >= 0xd800 &&
      value.charCodeAt(end - 1) <= 0xdbff &&
      value.charCodeAt(end) >= 0xdc00 &&
      value.charCodeAt(end) <= 0xdfff
    ) {
      end -= 1
    }

    if (encoder.encode(value.slice(0, end)).length <= maxBytes) {
      best = Math.max(best, end)
      lo = mid + 1
    } else {
      hi = mid - 1
    }
  }
  return value.slice(0, best)
}

// ── 归一化 ──────────────────────────────────────────────────

/**
 * browser_capture 归一化结果。
 *
 * 与 IngestSource 同形，但收紧了可选性：toIngestSource 必定填充
 * text / html / image_urls / metadata，因此这里把它们标为必填，
 * 让调用方无需对 `undefined` 做无意义的窄化。仍可直接赋给 IngestSource。
 */
export interface BrowserCaptureSource extends IngestSource {
  kind: 'browser_capture'
  url: string
  title: string
  text: string
  html: string
  image_urls: string[]
  metadata: Record<string, unknown>
}

/**
 * 把页面抓取的 RawCapture 归一化为后端 browser_capture IngestSource。
 * 负责分别裁剪纯文本和脱敏结构快照，并将媒体列表置空。
 *
 * @param raw  内容脚本 capturePageContent 的返回
 * @param note 用户备注（右键菜单为空串）；非空时随 metadata.note 透传给后端
 */
export function toIngestSource(
  raw: RawCapture,
  note: string,
): BrowserCaptureSource {
  const metadata: Record<string, unknown> = { ...raw.metadata }
  const trimmedNote = note.trim()
  if (trimmedNote) {
    // 用户备注随 metadata.note 透传给后端。
    metadata.note = trimmedNote
  }

  return {
    kind: 'browser_capture',
    url: raw.url,
    title: raw.title,
    text: truncateToBytes(raw.text, MAX_TEXT_BYTES),
    html: truncateToBytes(raw.html, MAX_HTML_BYTES),
    image_urls: [],
    metadata,
  }
}
