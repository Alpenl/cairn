/**
 * AI 对话能力。
 *
 * 策略：
 *   · 现代 Reader 主路径由 ChatSidebar 通过 identity-bound ReaderClient 调 `POST /api/ai`。
 *   · 本文件只保留旧宿主 bridge fallback：`window.claude.complete` 存在时真调，
 *     不存在 / 抛错 / 返回空时返回一句**明确说明未接入**的提示，不报错。
 *
 * 关于那句提示为什么必须是「未接入」而不是一段像模像样的回答：
 *
 * 这里原本会回退到一段本地占位模板，内容形如「关于划线『…』：它的要点是把判断
 * 建立在可扩展、可验证的方法上」——一句关于用户划线的、具体的、听起来像是读过
 * 原文的判断。而 `window.claude` 是宿主（扩展 / 桌面壳）注入的对象，Reader 作为
 * 一个普通网页跑在浏览器里时它**恒不存在**。也就是说：所有用户、所有对话，拿到
 * 的都是这段编造的话。结尾那个「（离线示例回复）」并不足以抵消它——用户看到的
 * 首先是一句对自己划线的具体点评。
 *
 * 现在后端对话代理已经是主路径；这里继续保留的原因是让旧宿主 bridge 缺失时
 * fail closed，而不是重新引入本地编造回答。
 */
import type { LinkResponse } from './api/types'

/** 一条对话消息的角色。 */
export type ChatRole = 'user' | 'ai'

/** 一条对话消息。 */
export interface ChatMessage {
  role: ChatRole
  text: string
  /** typing 占位（AI 回复生成中）。 */
  typing?: boolean
  /** AI 回复可被「采用为笔记」（仅草稿模式 + 完成的 AI 回复）。 */
  adoptable?: boolean
}

/** 宿主注入的 Claude 桥接（如扩展 / 桌面壳）。运行时 feature-detect，可缺失。 */
interface ClaudeBridge {
  complete: (req: { messages: Array<{ role: 'user' | 'assistant'; content: string }> }) => Promise<string>
}

interface AskAIOptions {
  signal?: AbortSignal
}

declare global {
  interface Window {
    claude?: ClaudeBridge
  }
}

/**
 * legacy 多轮对话调用。`window.claude.complete` 可用则真调，否则返回明确的未接入提示。
 * 永不抛异常——失败一律落到未接入提示，保证 UI 不崩且不编造回答。
 */
export async function askAI(
  history: ChatMessage[],
  link: LinkResponse | null | undefined,
  draftText?: string | null,
  options: AskAIOptions = {},
): Promise<string> {
  const title = link ? link.title || link.url : ''
  const framing =
    `你是一位中文阅读助手，帮助用户理解他们在文章《${title}》中划线/收藏的内容，并能把讨论凝练成可直接保存的简洁笔记。` +
    `回答用中文，简洁具体，默认 2-4 句；当用户想要解读/点评划线时，给出可直接作为笔记的句子。请用纯文本，不要使用 Markdown 标记（如 ** 或 # 列表符号）。` +
    (draftText ? `\n\n用户当前划线的原文："""${draftText}"""` : '')

  const mapped = history
    .filter((m) => !m.typing)
    .map((m) => ({
      role: (m.role === 'user' ? 'user' : 'assistant') as 'user' | 'assistant',
      content: m.text,
    }))

  let msgs: Array<{ role: 'user' | 'assistant'; content: string }>
  if (mapped.length && mapped[0].role === 'user') {
    msgs = [{ role: 'user', content: framing + '\n\n' + mapped[0].content }, ...mapped.slice(1)]
  } else {
    msgs = [{ role: 'user', content: framing }, ...mapped]
  }

  try {
    const bridge = window.claude
    if (bridge && typeof bridge.complete === 'function') {
      const r = await completeBridge(bridge, { messages: msgs }, options.signal)
      if (r && r.trim() && !options.signal?.aborted) return r.trim()
    }
  } catch {
    // 真调失败 → 落到「未接入」提示，不抛异常。
  }

  return UNAVAILABLE_REPLY
}

/** Keep the legacy bridge call shape stable while allowing Reader to abandon stale turns. */
async function completeBridge(
  bridge: ClaudeBridge,
  request: { messages: Array<{ role: 'user' | 'assistant'; content: string }> },
  signal?: AbortSignal,
): Promise<string> {
  if (signal?.aborted) return ''
  return new Promise((resolve) => {
    let settled = false
    const finish = (value: string) => {
      if (settled) return
      settled = true
      signal?.removeEventListener('abort', abort)
      resolve(value)
    }
    const abort = () => finish('')
    signal?.addEventListener('abort', abort, { once: true })
    try {
      void bridge.complete(request).then(finish, () => finish(''))
    } catch {
      finish('')
    }
  })
}

/**
 * 未接入模型时的回复。刻意不含任何针对当前内容的判断——它不知道，就不该说。
 */
export const UNAVAILABLE_REPLY =
  'AI 助手尚未接入模型：当前部署没有可用的对话后端，所以这里给不出真实回答。' +
  '接入后端对话代理后本面板即可正常使用。'

/** 当前环境是否具备真实的对话能力。UI 据此决定是否禁用输入框。 */
export function aiAvailable(): boolean {
  const bridge = typeof window === 'undefined' ? undefined : window.claude
  return Boolean(bridge && typeof bridge.complete === 'function')
}
