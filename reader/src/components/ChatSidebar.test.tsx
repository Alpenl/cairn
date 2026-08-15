import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { StrictMode } from 'react'
import { ChatSidebar, type ChatDraft } from './ChatSidebar'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ReaderAIRequest } from '../lib/api/types'
import { makeLink } from '../test/fixtures'

afterEach(() => {
  delete (window as { claude?: unknown }).claude
  vi.restoreAllMocks()
})

const link = makeLink({ title: '测试文章' })
type ReaderAIResult = Awaited<ReturnType<IdentityBoundReaderClient['completeReaderAI']>>

function legacyClient(): IdentityBoundReaderClient {
  return { isIdentityCurrent: () => true } as unknown as IdentityBoundReaderClient
}

function readerAIClient(
  completeReaderAI: unknown,
  isIdentityCurrent: () => boolean = () => true,
): IdentityBoundReaderClient {
  return { completeReaderAI, isIdentityCurrent } as unknown as IdentityBoundReaderClient
}

function enabledReply(answer: string): ReaderAIResult {
  return { ok: true, data: { enabled: true, answer } }
}

describe('ChatSidebar', () => {
  it('一般模式：展示问候 + ctx-chip + 一般快捷提问', () => {
    render(
      <ChatSidebar
        client={legacyClient()}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )
    expect(screen.getByText(/链接库助手/)).toBeInTheDocument()
    expect(screen.getByText(/基于：测试文章/)).toBeInTheDocument()
    expect(screen.getByText('总结这条')).toBeInTheDocument()
  })

  // 未接入模型时输入框必须禁用。留着一个可点可输的面板、每次回同一句提示，
  // 是一条看不出尽头的死路——用户会反复尝试。
  it('未接入模型时禁用输入框与发送按钮，并在 placeholder 里说明', () => {
    render(
      <ChatSidebar
        client={legacyClient()}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )
    const ta = screen.getByPlaceholderText(/尚未接入模型/) as HTMLTextAreaElement
    expect(ta.disabled).toBe(true)
    const send = document.querySelector('.send-btn') as HTMLButtonElement
    expect(send.disabled).toBe(true)
    // 快捷提问也要一并禁用。只禁输入框的话，三个 chip 还通着——点「总结这条」
    // 仍会跑完整一轮然后吐同一句提示，那条「看不出尽头的死路」并没有关掉。
    const chips = Array.from(document.querySelectorAll('.quick')) as HTMLButtonElement[]
    expect(chips.length).toBeGreaterThan(0)
    for (const chip of chips) {
      expect(chip.disabled).toBe(true)
    }
  })

  // 宿主注入桥接时恢复正常：输入框可用，回复来自真实调用。
  it('接入模型后输入框可用且回复来自真实调用', async () => {
    const w = window as unknown as { claude?: { complete: () => Promise<string> } }
    w.claude = { complete: async () => '真实回复' }
    try {
      render(
        <ChatSidebar
          client={legacyClient()}
          link={link}
          draft={null}
          onAdopt={() => {}}
          onClearDraft={() => {}}
          onClose={() => {}}
        />,
      )
      const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
      expect(ta.disabled).toBe(false)
      fireEvent.change(ta, { target: { value: '这条讲什么' } })
      fireEvent.keyDown(ta, { key: 'Enter' })
      await waitFor(() => expect(screen.getByText('真实回复')).toBeInTheDocument())
    } finally {
      delete w.claude
    }
  })

  it('优先使用 identity-bound Reader AI，并让服务端按 link_id 解析正文上下文', async () => {
    const completeReaderAI = vi.fn(async (_request: ReaderAIRequest) => enabledReply('服务端回复'))
    const article = makeLink({ id: 'reader-ai-link', content: '这是当前链接的正文上下文。' })
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={article}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '请概括' } })
    fireEvent.keyDown(ta, { key: 'Enter' })

    expect(await screen.findByText('服务端回复')).toBeInTheDocument()
    expect(completeReaderAI).toHaveBeenCalledWith(
      expect.objectContaining({
        link_id: article.id,
        scope: 'general',
        prompt: expect.not.stringContaining('这是当前链接的正文上下文。'),
      }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
  })

  it('划线草稿传 selected_text，并保留可采用的回复语义', async () => {
    const completeReaderAI = vi.fn(async () => enabledReply('划线解读'))
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-reader-ai',
        blockKey: 'summary',
        target: { kind: 'summary', sourceHash: 'b'.repeat(64) },
      },
      text: '需要传给模型的划线原文',
      nonce: 7,
    }
    const onAdopt = vi.fn()
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={draft}
        onAdopt={onAdopt}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    expect(await screen.findByText('划线解读')).toBeInTheDocument()
    expect(completeReaderAI).toHaveBeenCalledWith(
      expect.objectContaining({
        link_id: link.id,
        selected_text: draft.text,
        scope: 'selection',
      }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    )
    fireEvent.click(screen.getByText('采用为笔记'))
    expect(onAdopt).toHaveBeenCalledWith(draft.annotation, '划线解读')
  })

  it('Note selection session sends no link or unselected Note content', async () => {
    const completeReaderAI = vi.fn(async (_request: ReaderAIRequest) => enabledReply('笔记选区解读'))
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-note-ai',
        blockKey: 'note',
        target: { kind: 'note', noteRevision: 9 },
      },
      text: '仅发送这一段',
      nonce: 8,
      source: {
        type: 'note',
        hostId: 'note-private',
        revision: 9,
        start: 4,
        end: 11,
      },
    }
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={null}
        draft={draft}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    expect(await screen.findByText('笔记选区解读')).toBeInTheDocument()
    const request = completeReaderAI.mock.calls[0]?.[0]
    expect(request).toBeDefined()
    expect(request).toMatchObject({ scope: 'selection', selected_text: '仅发送这一段' })
    expect(request).not.toHaveProperty('link_id')
    expect(request?.prompt).toContain('"source_type":"note"')
    expect(JSON.stringify(request)).not.toContain('UNSELECTED_PRIVATE_SENTINEL')
  })

  it('starts one automatic selection turn under React StrictMode', async () => {
    const completeReaderAI = vi.fn(async (_request: ReaderAIRequest) => enabledReply('严格模式解读'))
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-note-strict',
        blockKey: 'note',
        target: { kind: 'note', noteRevision: 9 },
      },
      text: '只发一次',
      nonce: 9,
      source: {
        type: 'note',
        hostId: 'note-strict',
        revision: 9,
        start: 0,
        end: 5,
      },
    }
    render(
      <StrictMode>
        <ChatSidebar
          client={readerAIClient(completeReaderAI)}
          link={null}
          draft={draft}
          onAdopt={() => {}}
          onClearDraft={() => {}}
          onClose={() => {}}
        />
      </StrictMode>,
    )

    expect(await screen.findByText('严格模式解读')).toBeInTheDocument()
    await waitFor(() => expect(completeReaderAI).toHaveBeenCalledOnce())
  })

  it('后端 capability-off 时明确提示并禁用后续请求', async () => {
    const completeReaderAI = vi.fn(async () => ({
      ok: true as const,
      data: { enabled: false, answer: '' },
    }))
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '测试 capability' } })
    fireEvent.keyDown(ta, { key: 'Enter' })

    expect(await screen.findByText(/AI 助手当前未启用/)).toBeInTheDocument()
    const disabledInput = screen.getByPlaceholderText(/AI 助手当前未启用/) as HTMLTextAreaElement
    expect(disabledInput.disabled).toBe(true)
    expect(completeReaderAI).toHaveBeenCalledOnce()
  })

  it.each([404, 405, 501])('旧服务缺少 /api/ai 路由时按 capability-off 降级（%s）', async (status) => {
    const completeReaderAI = vi.fn(async () => ({
      ok: false as const,
      error: { kind: 'other' as const, status, message: `HTTP ${status}` },
    }))
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '旧服务探测' } })
    fireEvent.keyDown(ta, { key: 'Enter' })

    expect(await screen.findByText(/AI 助手当前未启用/)).toBeInTheDocument()
    expect(screen.getByPlaceholderText(/AI 助手当前未启用/)).toBeDisabled()
    expect(completeReaderAI).toHaveBeenCalledOnce()
  })

  it('明确的应用 404 不被误判为旧服务缺少 AI 路由', async () => {
    const completeReaderAI = vi.fn(async () => ({
      ok: false as const,
      error: { kind: 'other' as const, status: 404, errorCode: 'reader_not_found', message: 'not found' },
    }))
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '应用错误' } })
    fireEvent.keyDown(ta, { key: 'Enter' })

    expect(await screen.findByText('AI 助手请求失败，请稍后重试。')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('问点什么…')).not.toBeDisabled()
  })

  it('新请求取消旧请求，并忽略旧回复', async () => {
    const pending: Array<{
      resolve: (result: ReaderAIResult) => void
      signal?: AbortSignal
    }> = []
    const completeReaderAI = vi.fn(
      (_request: ReaderAIRequest, options: { signal?: AbortSignal } = {}) =>
        new Promise<ReaderAIResult>((resolve) => {
          pending.push({ resolve, signal: options.signal })
        }),
    )
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    const ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '第一个问题' } })
    fireEvent.keyDown(ta, { key: 'Enter' })
    await waitFor(() => expect(completeReaderAI).toHaveBeenCalledOnce())
    fireEvent.change(ta, { target: { value: '第二个问题' } })
    fireEvent.keyDown(ta, { key: 'Enter' })
    await waitFor(() => expect(completeReaderAI).toHaveBeenCalledTimes(2))

    expect(pending[0]?.signal?.aborted).toBe(true)
    pending[0]?.resolve(enabledReply('旧回复'))
    pending[1]?.resolve(enabledReply('新回复'))

    expect(await screen.findByText('新回复')).toBeInTheDocument()
    expect(screen.queryByText('旧回复')).not.toBeInTheDocument()
  })

  it('切换链接时清空旧对话，并忽略旧链接的迟到回复', async () => {
    const pending: Array<{
      request: ReaderAIRequest
      resolve: (result: ReaderAIResult) => void
      signal?: AbortSignal
    }> = []
    const completeReaderAI = vi.fn(
      (request: ReaderAIRequest, options: { signal?: AbortSignal } = {}) =>
        new Promise<ReaderAIResult>((resolve) => {
          pending.push({ request, resolve, signal: options.signal })
        }),
    )
    const nextLink = makeLink({ id: 'next-link', title: '下一篇文章' })
    const view = render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    let ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '旧链接问题' } })
    fireEvent.keyDown(ta, { key: 'Enter' })
    await waitFor(() => expect(completeReaderAI).toHaveBeenCalledOnce())

    view.rerender(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={nextLink}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )
    await waitFor(() => expect(screen.queryByText('旧链接问题')).not.toBeInTheDocument())
    expect(pending[0]?.signal?.aborted).toBe(true)

    pending[0]?.resolve(enabledReply('旧链接回复'))
    await Promise.resolve()
    expect(screen.queryByText('旧链接回复')).not.toBeInTheDocument()

    ta = screen.getByPlaceholderText('问点什么…') as HTMLTextAreaElement
    fireEvent.change(ta, { target: { value: '新链接问题' } })
    fireEvent.keyDown(ta, { key: 'Enter' })
    await waitFor(() => expect(completeReaderAI).toHaveBeenCalledTimes(2))
    expect(pending[1]?.request.prompt).toContain('下一篇文章')
    expect(pending[1]?.request.prompt).not.toContain('旧链接问题')
  })

  it('identity 失效时不发起 Reader AI 请求', () => {
    let current = true
    const completeReaderAI = vi.fn(async () => enabledReply('不应出现'))
    const client = readerAIClient(completeReaderAI, () => current)
    const view = render(
      <ChatSidebar
        client={client}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    current = false
    view.rerender(
      <ChatSidebar
        client={client}
        link={link}
        draft={null}
        onAdopt={() => {}}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )
    expect(screen.getByPlaceholderText(/Reader 身份已失效/)).toBeDisabled()
    expect(completeReaderAI).not.toHaveBeenCalled()
  })

  it('未接入模型的草稿回复不可采用为笔记', async () => {
    const onAdopt = vi.fn()
    const onClearDraft = vi.fn()
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-1',
        blockKey: 'summary',
        target: { kind: 'summary', sourceHash: 'a'.repeat(64) },
      },
      text: '一段被划线的原文',
      nonce: 1,
    }
    render(
      <ChatSidebar
        client={legacyClient()}
        link={link}
        draft={draft}
        onAdopt={onAdopt}
        onClearDraft={onClearDraft}
        onClose={() => {}}
      />,
    )
    // 草稿头 + 草稿模式快捷提问。
    expect(screen.getByText('为这段划线记笔记')).toBeInTheDocument()
    expect(screen.getByText('更精简')).toBeInTheDocument()
    // 自动首轮的 AI 回复。未接入模型时它是一句明确的说明，不含对划线的点评。
    const reply = await screen.findByText(/尚未接入模型/)
    expect(reply).toBeInTheDocument()
    expect(screen.queryByText('采用为笔记')).not.toBeInTheDocument()
    expect(onAdopt).not.toHaveBeenCalled()
    expect(onClearDraft).not.toHaveBeenCalled()
  })

  it.each([
    {
      label: '客户端超时',
      error: { kind: 'timeout' as const, message: 'timeout' },
      reply: '请求超时',
    },
    {
      label: '服务端超时',
      error: { kind: 'other' as const, status: 504, errorCode: 'ai_timeout', message: 'timeout' },
      reply: '请求超时',
    },
    {
      label: '请求被限流',
      error: {
        kind: 'rate-limited' as const,
        status: 429,
        errorCode: 'rate_limit_exceeded',
        message: 'too many requests',
      },
      reply: 'AI 助手请求失败，请稍后重试。',
    },
  ])('$label 时显示可识别提示且不可采用', async ({ error, reply }) => {
    const completeReaderAI = vi.fn(async () => ({ ok: false as const, error }))
    const onAdopt = vi.fn()
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-error',
        blockKey: 'summary',
        target: { kind: 'summary', sourceHash: 'c'.repeat(64) },
      },
      text: '需要真实模型回答的划线',
      nonce: 11,
    }
    render(
      <ChatSidebar
        client={readerAIClient(completeReaderAI)}
        link={link}
        draft={draft}
        onAdopt={onAdopt}
        onClearDraft={() => {}}
        onClose={() => {}}
      />,
    )

    expect(await screen.findByText(new RegExp(reply))).toBeInTheDocument()
    expect(screen.queryByText('采用为笔记')).not.toBeInTheDocument()
    expect(onAdopt).not.toHaveBeenCalled()
  })

  it('退出草稿模式调用 onClearDraft', async () => {
    const onClearDraft = vi.fn()
    const draft: ChatDraft = {
      annotation: {
        id: 'ann-1',
        blockKey: 'content',
        target: { kind: 'saved-content', contentRevision: 7 },
      },
      text: '原文',
      nonce: 2,
    }
    render(
      <ChatSidebar
        client={legacyClient()}
        link={link}
        draft={draft}
        onAdopt={() => {}}
        onClearDraft={onClearDraft}
        onClose={() => {}}
      />,
    )
    // 先等草稿首轮的异步 AI 回复落地，再做同步断言——否则回复 resolve
    // 时组件 setState 发生在断言之后，触发 React act() 告警（测试卫生）。
    await screen.findByText(/尚未接入模型/)
    fireEvent.click(screen.getByTitle('退出记笔记模式'))
    await waitFor(() => expect(onClearDraft).toHaveBeenCalled())
  })
})
