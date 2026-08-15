import 'fake-indexeddb/auto'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MainView } from './MainView'
import { DATA_NAMESPACE_HEADER, IdentityBoundReaderClient } from '../lib/api/client'
import { makeLink } from '../test/fixtures'
import { IdentityAuthority } from '../lib/identity'

beforeEach(() => {
  window.history.replaceState({}, '', '/?view=reading')
})

afterEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('MainView read model', () => {
  it('以 Reading 摘要 envelope 的独立 total 显示 done 总数，不漏掉无域名链接', async () => {
    const requested: URL[] = []
    const currentPage = makeLink({ id: 'visible', domain: 'example.com' })
    const authority = new IdentityAuthority()
    const identity = authority.install({
      serverClientDataNamespace: 'server-test',
      physicalNamespace: 'physical-test',
    })
    const ownershipHeaders = { [DATA_NAMESPACE_HEADER]: 'server-test' }
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL | Request) => {
        const raw = input instanceof Request ? input.url : String(input)
        const url = new URL(raw)
        requested.push(url)

        if (url.pathname === '/api/tags') {
          return new Response('[]', { status: 200, headers: ownershipHeaders })
        }
        if (url.pathname === '/api/tree' && url.searchParams.get('view') === 'domains') {
          return new Response(
            JSON.stringify({
              library_kind: 'reading',
              domains: [
                { domain: 'example.com', count: 240 },
                { domain: 'docs.example.org', count: 10 },
              ],
              total: 251,
            }),
            { status: 200, headers: ownershipHeaders },
          )
        }
        if (url.pathname === '/api/links') {
          return new Response(
            JSON.stringify({
              items: [currentPage],
              total: 251,
              page: url.searchParams.has('after') ? 0 : 1,
              limit: Math.min(Number(url.searchParams.get('limit') || 20), 100),
            }),
            { status: 200, headers: ownershipHeaders },
          )
        }
        throw new Error(`unexpected request: ${url}`)
      }),
    )

    render(
      <MainView
        client={new IdentityBoundReaderClient({ baseURL: 'http://localhost:8080', identity })}
        onOpenSettings={() => {}}
      />,
    )

    await waitFor(() => expect(screen.getByText('251')).toBeInTheDocument())
    expect(
      requested.some(
        (url) =>
          url.pathname === '/api/tree' &&
          url.searchParams.get('view') === 'domains' &&
          url.searchParams.get('library_kind') === 'reading',
      ),
    ).toBe(true)
    expect(requested.some((url) => url.searchParams.get('limit') === '500')).toBe(false)
    expect(
      requested.some(
        (url) =>
          url.pathname === '/api/links' &&
          url.searchParams.get('status') === 'done' &&
          url.searchParams.has('after'),
      ),
    ).toBe(false)
  })
})
