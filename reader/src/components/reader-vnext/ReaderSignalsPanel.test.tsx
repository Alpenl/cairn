import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'

import { err, ok } from '../../lib/api/result'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderActivityResponse, ReaderRelatedTagsResponse } from '../../lib/api/types'
import { IdentityLease } from '../../lib/identity'
import { resourceStore } from '../../lib/cache/store'
import { makeLink } from '../../test/fixtures'
import { ReaderActivityPanel, ReaderSemanticTagsPanel } from './ReaderSignalsPanel'

let lease: IdentityLease
let serial = 0

function makeClient(options: {
  getRelatedTags?: IdentityBoundReaderClient['getRelatedTags']
  getReaderActivity?: IdentityBoundReaderClient['getReaderActivity']
} = {}): IdentityBoundReaderClient {
  return {
    identityLease: lease,
    isIdentityCurrent: vi.fn(() => true),
    ...options,
  } as unknown as IdentityBoundReaderClient
}

beforeEach(() => {
  serial += 1
  lease = new IdentityLease({
    serverClientDataNamespace: `signals-server-${serial}`,
    physicalNamespace: `signals-physical-${serial}`,
    localEpoch: serial,
  })
  resourceStore.activateIdentity(lease)
})

afterEach(() => {
  resourceStore.deactivateIdentity(lease)
})

describe('ReaderSignalsPanel', () => {
  it('shows semantic model generation and degraded mode without treating it as local data', async () => {
    const getRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['semantic-tag'],
      model: 'semantic-v2:embed-2026-08',
      degraded: false,
    }))
    const link = makeLink({ id: `signals-${serial}`, tags: ['local-tag'] })

    render(
      <ReaderSemanticTagsPanel
        link={link}
        corpus={[link]}
        client={makeClient({ getRelatedTags })}
      />,
    )

    await waitFor(() => {
      expect(screen.getByTestId('related-tags-mode')).toHaveTextContent('语义')
      expect(screen.getByRole('button', { name: '#semantic-tag' })).toBeInTheDocument()
      expect(screen.getByTestId('related-tags-model')).toHaveTextContent('semantic-v2:embed-2026-08')
      expect(screen.queryByTestId('related-tags-degraded')).not.toBeInTheDocument()
    })
  })

  it('shows the server cooccurrence model as a degraded Reader result', async () => {
    const getRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['cooccurrence-tag'],
      model: 'cooccurrence-v1',
      degraded: true,
    }))
    const link = makeLink({ id: `cooccurrence-${serial}`, tags: ['local-tag'] })

    render(
      <ReaderSemanticTagsPanel
        link={link}
        corpus={[link]}
        client={makeClient({ getRelatedTags })}
      />,
    )

    await waitFor(() => {
      expect(screen.getByTestId('related-tags-mode')).toHaveTextContent('共现降级')
      expect(screen.getByTestId('related-tags-model')).toHaveTextContent('cooccurrence-v1')
      expect(screen.getByTestId('related-tags-degraded')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '#cooccurrence-tag' })).toBeInTheDocument()
    })
  })

  it('marks unavailable semantic data as a local fallback', () => {
    const link = makeLink({ id: `local-${serial}`, tags: ['source'] })
    render(
      <ReaderSemanticTagsPanel
        link={link}
        corpus={[link, makeLink({ id: 'other', tags: ['source', 'local-related'] })]}
        client={makeClient()}
      />,
    )

    expect(screen.getByTestId('related-tags-mode')).toHaveTextContent('本地近似')
    expect(screen.getByTestId('related-tags-degraded')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '#local-related' })).toBeInTheDocument()
  })

  it('uses created_at links when the activity endpoint is unavailable and labels the degradation', async () => {
    const getReaderActivity = vi.fn(async () => err<ReaderActivityResponse>({
      kind: 'other',
      status: 404,
      message: 'old backend',
    }))
    const links = [
      makeLink({ id: 'activity-old', tags: ['old'], domain: 'old.example', created_at: '2026-08-10T01:00:00Z' }),
      makeLink({ id: 'activity-new', tags: ['new'], domain: 'new.example', created_at: '2026-08-10T02:00:00Z' }),
    ]
    render(<ReaderActivityPanel links={links} client={makeClient({ getReaderActivity })} />)

    await waitFor(() => {
      expect(getReaderActivity).toHaveBeenCalled()
      expect(screen.getByTestId('activity-source')).toHaveTextContent('本地近似')
      expect(screen.getByTestId('activity-degraded')).toBeInTheDocument()
      const tagList = screen.getByRole('list', { name: '活跃标签' })
      expect(tagList.textContent).toMatch(/#new.*#old/)
      expect(tagList).toHaveTextContent('2026-08-10T02:00:00Z')
    })
  })

  it('本地降级只显示投影字段，不泄漏链接正文或后端错误文本', async () => {
    const secret = 'private body should stay out of the signals panel'
    const getRelatedTags = vi.fn(async () => err<ReaderRelatedTagsResponse>({
      kind: 'other',
      status: 503,
      message: secret,
    }))
    const link = makeLink({
      id: `private-${serial}`,
      tags: ['source'],
      summary: secret,
      content: secret,
    })
    const relatedLink = makeLink({
      id: `private-related-${serial}`,
      tags: ['source', 'local-related'],
      summary: 'not rendered either',
    })

    render(
      <ReaderSemanticTagsPanel
        link={link}
        corpus={[link, relatedLink]}
        client={makeClient({ getRelatedTags })}
      />,
    )

    await waitFor(() => expect(getRelatedTags).toHaveBeenCalled())
    expect(screen.getByTestId('related-tags-mode')).toHaveTextContent('本地近似')
    expect(screen.getByRole('button', { name: '#local-related' })).toBeInTheDocument()
    expect(screen.queryByText(secret)).not.toBeInTheDocument()
  })

  it('请求失败时提供不泄漏错误正文的重试入口，并可恢复到服务端结果', async () => {
    const link = makeLink({ id: `retry-${serial}`, tags: ['source'] })
    const getRelatedTags = vi
      .fn<IdentityBoundReaderClient['getRelatedTags']>()
      .mockResolvedValueOnce(err<ReaderRelatedTagsResponse>({
        kind: 'other',
        status: 503,
        message: 'backend failure details must stay hidden',
      }))
      .mockResolvedValueOnce(ok<ReaderRelatedTagsResponse>({
        items: ['recovered-tag'],
        model: 'semantic-v1:embed-current',
        degraded: false,
      }))

    render(
      <ReaderSemanticTagsPanel
        link={link}
        corpus={[link]}
        client={makeClient({ getRelatedTags })}
      />,
    )

    await waitFor(() => expect(screen.getByRole('button', { name: '重试相关标签' })).toBeInTheDocument())
    expect(screen.queryByText('backend failure details must stay hidden')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '重试相关标签' }))
    await waitFor(() => {
      expect(getRelatedTags).toHaveBeenCalledTimes(2)
      expect(screen.getByRole('button', { name: '#recovered-tag' })).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '重试相关标签' })).not.toBeInTheDocument()
    })
  })
})
