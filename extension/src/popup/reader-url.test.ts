import { describe, expect, it } from 'vitest'
import { resolveSubscriptionsReaderUrl } from './reader-url'

describe('resolveSubscriptionsReaderUrl', () => {
  it('prefers the separately configured Reader URL and preserves its path/query', () => {
    expect(
      resolveSubscriptionsReaderUrl({
        backendUrl: 'https://api.example.com',
        readerUrl: ' https://app.example.com/reader?source=popup ',
      }),
    ).toBe('https://app.example.com/reader?source=popup&view=subscriptions')
  })

  // 后端把 Reader 内嵌在 /reader/ 下，兜底直接给最终地址（带尾斜杠），
  // 不依赖 301。这条以前指向一条后端并不存在的路由，用户点了只会拿到 404。
  it('falls back to the backend-hosted Reader mount when no Reader URL is set', () => {
    expect(
      resolveSubscriptionsReaderUrl({
        backendUrl: 'https://api.example.com/',
        readerUrl: '',
      }),
    ).toBe('https://api.example.com/reader/?view=subscriptions')
  })

  it('rejects invalid or non-HTTP explicit Reader URLs', () => {
    expect(
      resolveSubscriptionsReaderUrl({
        backendUrl: 'https://api.example.com',
        readerUrl: 'javascript:alert(1)',
      }),
    ).toBeNull()
  })
})
