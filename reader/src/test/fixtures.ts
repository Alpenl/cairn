/**
 * 测试用链接 fixture，形态对齐 LinkResponse（DTO 线缆形状）。仅供单测使用。
 */
import type { LinkResponse } from '../lib/api/types'

/** 构造一条 LinkResponse，按需覆盖字段。 */
export function makeLink(over: Partial<LinkResponse> = {}): LinkResponse {
  return {
    id: 'id-' + (over.id ?? Math.random().toString(36).slice(2)),
    url: 'https://example.com/a',
    title: '示例标题',
    summary: '这是一段 AI 摘要正文。',
    description: null,
    tags: ['LLM'],
    content_type: 'article',
    status: 'done',
    domain: 'example.com',
    path_depth: 1,
    parent_id: null,
    created_at: '2026-06-10T10:00:00Z',
    updated_at: '2026-06-10T10:05:00Z',
    fetcher_type: 'http',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    parent_path: null,
    metadata_revision: 1,
    ...over,
    // has_content 在后端是 `content IS NOT NULL` 的生成列（migrate/steps.go），
    // 「有 content 但 has_content:false」这个组合线上不可能出现。fixture 必须照这个
    // 来——否则用例会跑在一个现实中不存在的响应形态上，而渲染路径正是靠 has_content
    // 判断「服务端还有没有这份正文」的。显式传 has_content 时仍以调用方为准（要造
    // 「正文被服务端清空、本地还留着一份」那种时序，就得显式写）。
    has_content: over.has_content ?? Boolean(over.content),
  }
}
