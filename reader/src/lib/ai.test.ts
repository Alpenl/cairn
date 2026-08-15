import { afterEach, describe, expect, it, vi } from 'vitest'
import { aiAvailable, askAI, UNAVAILABLE_REPLY, type ChatMessage } from './ai'
import { makeLink } from '../test/fixtures'

function setBridge(complete: (...args: never[]) => Promise<string>): void {
  const w = window as unknown as { claude?: { complete: typeof complete } }
  w.claude = { complete }
}

afterEach(() => {
  delete (window as { claude?: unknown }).claude
  vi.restoreAllMocks()
})

describe('askAI', () => {
  const link = makeLink({ title: '可扩展的方法论' })
  const history: ChatMessage[] = [{ role: 'user', text: '帮我总结一下' }]

  it('window.claude 可用时调用并返回其回复', async () => {
    const complete = vi.fn().mockResolvedValue('  这是真实回复  ')
    setBridge(complete)
    const r = await askAI(history, link)
    expect(r).toBe('这是真实回复')
    expect(complete).toHaveBeenCalledOnce()
    // framing 注入到首条 user 消息。
    const arg = complete.mock.calls[0][0] as { messages: Array<{ role: string; content: string }> }
    expect(arg.messages[0].role).toBe('user')
    expect(arg.messages[0].content).toContain('帮我总结一下')
    expect(arg.messages[0].content).toContain('可扩展的方法论')
  })

  // 这三条锁住的是同一件事：没有模型时不得编造回答。
  //
  // 之前的回退会返回一句关于用户划线的、具体的、听起来像读过原文的点评。
  // 而 window.claude 是宿主注入的对象，Reader 作为普通网页跑时它恒不存在——
  // 也就是所有用户、所有对话拿到的都是那段编出来的话。
  it('window.claude 不存在时明确说明未接入，不编造内容', async () => {
    const r = await askAI(history, link)
    expect(r).toBe(UNAVAILABLE_REPLY)
    // 不得出现任何针对当前内容的判断——连标题都不该拿来造句。
    expect(r).not.toContain('可扩展的方法论')
  })

  it('草稿模式同样不编造对划线的点评', async () => {
    const draft = '把判断建立在可验证的方法上'
    const r = await askAI(history, link, draft)
    expect(r).toBe(UNAVAILABLE_REPLY)
    expect(r).not.toContain(draft.slice(0, 6))
  })

  it('complete 抛错时给未接入提示，不抛出', async () => {
    setBridge(vi.fn().mockRejectedValue(new Error('boom')))
    expect(await askAI(history, link)).toBe(UNAVAILABLE_REPLY)
  })

  it('complete 返回空串时给未接入提示', async () => {
    setBridge(vi.fn().mockResolvedValue('   '))
    expect(await askAI(history, link)).toBe(UNAVAILABLE_REPLY)
  })

  it('取消中的宿主请求不会继续等待，也不返回旧回答', async () => {
    let resolveBridge: ((value: string) => void) | undefined
    const complete = vi.fn(
      () =>
        new Promise<string>((resolve) => {
          resolveBridge = resolve
        }),
    )
    setBridge(complete)
    const controller = new AbortController()
    const pending = askAI(history, link, undefined, { signal: controller.signal })
    await vi.waitFor(() => expect(complete).toHaveBeenCalledOnce())
    controller.abort()
    expect(await pending).toBe(UNAVAILABLE_REPLY)
    resolveBridge?.('迟到的宿主回复')
  })

  it('aiAvailable 反映桥接是否存在', () => {
    expect(aiAvailable()).toBe(false)
    setBridge(vi.fn().mockResolvedValue('x'))
    expect(aiAvailable()).toBe(true)
  })

  it('typing 占位消息被过滤，不进入发送 payload', async () => {
    const complete = vi.fn().mockResolvedValue('ok')
    setBridge(complete)
    const withTyping: ChatMessage[] = [
      { role: 'user', text: '问题一' },
      { role: 'ai', text: '', typing: true },
    ]
    await askAI(withTyping, link)
    const arg = complete.mock.calls[0][0] as { messages: Array<unknown> }
    expect(arg.messages).toHaveLength(1)
  })
})
