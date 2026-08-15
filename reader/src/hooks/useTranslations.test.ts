import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { translationsKey, useTranslations } from './useTranslations'
import type { ReaderClient } from '../lib/api/client'
import type { TranslationListResponse, TranslationResponse } from '../lib/api/types'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'

const SUMMARY_HASH_A = 'a'.repeat(64)
const SUMMARY_HASH_B = 'b'.repeat(64)

function translation(over: Partial<TranslationResponse> = {}): TranslationResponse {
  return {
    id: 'T1',
    link_id: 'L1',
    scope: 'full',
    block_key: 'content',
    start_offset: 0,
    end_offset: 5,
    source_text: 'hello',
    translated_text: '你好',
    source_format: 'plain',
    target_language: 'zh-CN',
    status: 'done',
    model: 'grok-4.3-fast',
    error_msg: null,
    source_content_revision: 7,
    stale: false,
    created_at: '2026-07-15T00:00:00Z',
    updated_at: '2026-07-15T00:00:00Z',
    ...over,
  }
}

function translationList(
  items: TranslationResponse[],
  currentContentRevision = 7,
  currentSummarySourceHash: string | null = null,
): TranslationListResponse {
  return {
    current_content_revision: currentContentRevision,
    current_summary_source_hash: currentSummarySourceHash,
    items,
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('useTranslations', () => {
  it('partitions cached envelopes by saved-content revision', async () => {
    const revisionSeven = translation({
      id: 'saved-rev-7',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
    })
    const revisionEight = translation({
      id: 'saved-rev-8',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 8,
    })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([revisionSeven], 7) })
        .mockResolvedValueOnce({ ok: true as const, data: translationList([revisionEight], 8) }),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ contentRevision }) => useTranslations(client, 'L1', { contentRevision }),
      { initialProps: { contentRevision: 7 } },
    )

    await waitFor(() => expect(result.current.items).toEqual([revisionSeven]))
    rerender({ contentRevision: 8 })
    await waitFor(() => expect(result.current.items).toEqual([revisionEight]))

    expect(translationsKey('L1', 7)).not.toBe(translationsKey('L1', 8))
    expect(
      resourceStore.peek<TranslationListResponse>(translationsKey('L1', 7)).data?.items,
    ).toEqual([revisionSeven])
    expect(
      resourceStore.peek<TranslationListResponse>(translationsKey('L1', 8)).data?.items,
    ).toEqual([revisionEight])
  })

  it('打开文章时恢复数据库中已保存的译文', async () => {
    const saved = translation()
    const client = {
      getTranslations: vi.fn(async () => ({ ok: true as const, data: translationList([saved]) })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() => useTranslations(client, 'L1'))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.items).toEqual([saved])
    expect(client.getTranslations).toHaveBeenCalledWith('L1')
  })

  it('只把匹配权威 saved-content revision 的译文暴露为当前译文', async () => {
    const current = translation({
      id: 'current',
      scope: 'full',
      block_key: 'content',
      source_text: 'same body',
      source_content_revision: 8,
    })
    const stale = translation({
      id: 'stale',
      scope: 'full',
      block_key: 'content',
      source_text: 'same body',
      source_content_revision: 7,
    })
    const legacy = translation({
      id: 'legacy',
      scope: 'full',
      block_key: 'content',
      source_text: 'same body',
      source_content_revision: null,
    })
    const currentSummary = translation({
      id: 'summary-current',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    const staleSummary = translation({
      id: 'summary-stale',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
      stale: true,
    })
    const retiredDeepResearch = translation({
      id: 'retired-dr',
      scope: 'selection',
      block_key: 'dr',
      source_content_revision: null,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList(
          [current, stale, legacy, currentSummary, staleSummary, retiredDeepResearch],
          8,
          SUMMARY_HASH_A,
        ),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTranslations(client, 'L1', { summarySourceHash: SUMMARY_HASH_A }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.currentContentRevision).toBe(8)
    expect(result.current.items.map((item) => item.id)).toEqual(['current', 'summary-current'])
    expect(result.current.staleItems.map((item) => item.id)).toEqual(['stale', 'summary-stale'])
    expect(result.current.legacyItems.map((item) => item.id)).toEqual(['legacy', 'retired-dr'])
  })

  it('将缺失 revision envelope 的旧缓存 saved-content 译文关在 legacy 区', async () => {
    const legacyRuntimePayload = {
      items: [
        {
          ...translation({
            id: 'legacy-v4',
            scope: 'full',
            block_key: 'content',
          }),
          source_content_revision: undefined,
        },
      ],
    } as unknown as TranslationListResponse
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: legacyRuntimePayload,
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() => useTranslations(client, 'L1'))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.currentContentRevision).toBeNull()
    expect(result.current.items).toEqual([])
    expect(result.current.staleItems).toEqual([])
    expect(result.current.legacyItems.map((item) => item.id)).toEqual(['legacy-v4'])
  })

  it('零 envelope revision 表示尚无 saved generation，不能证明译文当前有效', async () => {
    const invalidIdentity = translation({
      id: 'invalid-zero-revision',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 0,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([invalidIdentity], 0),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() => useTranslations(client, 'L1'))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.currentContentRevision).toBe(0)
    expect(result.current.items).toEqual([])
    expect(result.current.staleItems).toEqual([invalidIdentity])
  })

  it('同一正文文本跨 revision 仍只认 envelope 指定的那一代', async () => {
    const oldRevision = translation({
      id: 'rev-7',
      scope: 'selection',
      block_key: 'content',
      source_text: 'identical text',
      source_content_revision: 7,
    })
    const currentRevision = translation({
      id: 'rev-8',
      scope: 'selection',
      block_key: 'content',
      source_text: 'identical text',
      source_content_revision: 8,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([oldRevision, currentRevision], 8),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() => useTranslations(client, 'L1'))

    await waitFor(() => expect(result.current.items).toEqual([currentRevision]))
    expect(result.current.staleItems).toEqual([oldRevision])
  })

  it('正文恢复后按服务端 source identity 保留旧译文、摘要和 legacy 分区', async () => {
    const restoredOldFull = translation({
      id: 'restored-old-full',
      scope: 'full',
      block_key: 'content',
      source_text: 'Alpha beta',
      source_content_revision: 4,
      stale: true,
    })
    const restoredOldSelection = translation({
      id: 'restored-old-selection',
      scope: 'selection',
      block_key: 'content',
      source_text: 'Alpha',
      source_content_revision: 4,
      status: 'failed',
      translated_text: null,
      model: null,
      stale: true,
    })
    const currentSummary = translation({
      id: 'restored-current-summary',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
      stale: false,
    })
    const legacy = translation({
      id: 'restored-legacy',
      scope: 'selection',
      block_key: 'content',
      source_content_revision: null,
      stale: false,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList(
          [restoredOldFull, restoredOldSelection, currentSummary, legacy],
          5,
          SUMMARY_HASH_A,
        ),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTranslations(client, 'L1', { contentRevision: 5, summarySourceHash: SUMMARY_HASH_A }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.items.map((item) => item.id)).toEqual(['restored-current-summary'])
    expect(result.current.staleItems.map((item) => item.id)).toEqual([
      'restored-old-full',
      'restored-old-selection',
    ])
    expect(result.current.legacyItems.map((item) => item.id)).toEqual(['restored-legacy'])
  })

  it('active revision 高于内部自洽的缓存 envelope 时立即隔离旧译文并回源', async () => {
    const cachedRevision = translation({
      id: 'cached-rev-7',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
    })
    const currentRevision = translation({
      id: 'current-rev-8',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 8,
    })
    resourceStore.set(translationsKey('L1', 7), translationList([cachedRevision], 7))
    let resolveList!: (value: { ok: true; data: TranslationListResponse }) => void
    const listRequest = new Promise<{ ok: true; data: TranslationListResponse }>((resolve) => {
      resolveList = resolve
    })
    const client = {
      getTranslations: vi.fn(() => listRequest),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTranslations(client, 'L1', {
        contentRevision: 8,
      }),
    )

    expect(result.current.items).toEqual([])
    expect(result.current.staleItems).toEqual([])
    expect(
      resourceStore.peek<TranslationListResponse>(translationsKey('L1', 7)).data?.items,
    ).toEqual([cachedRevision])
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(1))

    await act(async () => {
      resolveList({ ok: true, data: translationList([currentRevision], 8) })
      await listRequest
    })
    expect(result.current.items).toEqual([currentRevision])
  })

  it('未绑定当前 summary hash 的缓存译文保持隔离直到当前来源回源完成', async () => {
    const cachedSummary = translation({
      id: 'cached-summary',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    const currentSummary = translation({
      id: 'current-summary',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    resourceStore.set(translationsKey('L1', 7), translationList([cachedSummary]))
    let resolveList!: (value: { ok: true; data: TranslationListResponse }) => void
    const listRequest = new Promise<{ ok: true; data: TranslationListResponse }>((resolve) => {
      resolveList = resolve
    })
    const client = {
      getTranslations: vi.fn(() => listRequest),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTranslations(client, 'L1', {
        contentRevision: 7,
        summarySourceHash: SUMMARY_HASH_A,
      }),
    )

    expect(result.current.items).toEqual([])
    expect(result.current.legacyItems).toEqual([cachedSummary])
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(1))

    await act(async () => {
      resolveList({ ok: true, data: translationList([currentSummary], 7, SUMMARY_HASH_A) })
      await listRequest
    })
    expect(result.current.items).toEqual([currentSummary])
  })

  it('只信服务端返回的 summary identity，不用本地 observed hash 给响应背书', async () => {
    const summary = translation({
      id: 'server-summary-b',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([summary], 7, SUMMARY_HASH_B),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() =>
      useTranslations(client, 'L1', {
        contentRevision: 7,
        summarySourceHash: SUMMARY_HASH_A,
      }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.items).toEqual([])
    expect(result.current.legacyItems).toEqual([summary])
  })

  it('summary hash 未验证时仍恢复并轮询 saved-content，且隔离 summary 子资源', async () => {
    vi.useFakeTimers()
    const savedPending = translation({
      id: 'saved-pending',
      translated_text: null,
      status: 'pending',
      model: null,
    })
    const savedDone = translation({ id: 'saved-done' })
    const summary = translation({
      id: 'summary-hidden',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({
          ok: true as const,
          data: translationList([savedPending, summary], 7, SUMMARY_HASH_A),
        })
        .mockResolvedValueOnce({
          ok: true as const,
          data: translationList([savedDone, summary], 7, SUMMARY_HASH_A),
        }),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() =>
      useTranslations(client, 'L1', {
        contentRevision: 7,
        summarySourceHash: null,
      }),
    )

    await act(async () => {})
    expect(client.getTranslations).toHaveBeenCalledTimes(1)
    expect(result.current.items).toEqual([savedPending])
    expect(result.current.legacyItems).toEqual([summary])
    expect(result.current.hasActiveJobs).toBe(true)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1200)
    })
    expect(client.getTranslations).toHaveBeenCalledTimes(2)
    expect(result.current.items).toEqual([savedDone])
    expect(result.current.legacyItems).toEqual([summary])
    expect(result.current.hasActiveJobs).toBe(false)
  })

  it('summary identity 变化时继续权威回源', async () => {
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({
          ok: true as const,
          data: translationList([], 7, SUMMARY_HASH_A),
        })
        .mockResolvedValueOnce({
          ok: true as const,
          data: translationList([], 7, SUMMARY_HASH_B),
        }),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { rerender } = renderHook(
      ({ summarySourceHash }) =>
        useTranslations(client, 'L1', { contentRevision: 7, summarySourceHash }),
      { initialProps: { summarySourceHash: SUMMARY_HASH_A as string | null } },
    )
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(1))

    rerender({ summarySourceHash: null })
    await act(async () => {})
    expect(client.getTranslations).toHaveBeenCalledTimes(1)

    rerender({ summarySourceHash: SUMMARY_HASH_B })
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(2))
  })

  it('同一链接的 cache generation 变化只由资源层发起一次请求', async () => {
    let resolveNextGeneration!: (value: {
      ok: true
      data: TranslationListResponse
    }) => void
    const nextGenerationRequest = new Promise<{
      ok: true
      data: TranslationListResponse
    }>((resolve) => {
      resolveNextGeneration = resolve
    })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({
          ok: true as const,
          data: translationList([], 7, SUMMARY_HASH_A),
        })
        .mockReturnValue(nextGenerationRequest),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ contentRevision, summarySourceHash }) =>
        useTranslations(client, 'L1', { contentRevision, summarySourceHash }),
      {
        initialProps: {
          contentRevision: 7,
          summarySourceHash: SUMMARY_HASH_A,
        },
      },
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(client.getTranslations).toHaveBeenCalledTimes(1)

    resourceStore.set(
      translationsKey('L1', 8),
      translationList([], 8, SUMMARY_HASH_A),
    )
    rerender({ contentRevision: 8, summarySourceHash: SUMMARY_HASH_B })

    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(2))
    await act(async () => {})
    expect(client.getTranslations).toHaveBeenCalledTimes(2)

    await act(async () => {
      resolveNextGeneration({
        ok: true,
        data: translationList([], 8, SUMMARY_HASH_B),
      })
      await nextGenerationRequest
    })
  })

  it('创建pending任务后立即合并状态供轮询使用', async () => {
    const pending = translation({ translated_text: null, status: 'pending', model: null })
    const client = {
      getTranslations: vi.fn(async () => ({ ok: true as const, data: translationList([]) })),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: pending })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() =>
      useTranslations(client, 'L1', { contentRevision: 7 }),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      const response = await result.current.create({
        scope: 'full',
        force: false,
      })
      expect(response.ok).toBe(true)
    })

    expect(result.current.items).toEqual([pending])
    expect(result.current.hasActiveJobs).toBe(true)
    expect(resourceStore.peek<TranslationListResponse>(translationsKey('L1', 7)).data).toEqual({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [pending],
    })
  })

  it('内部派生 saved-content revision 并覆盖旧调用方夹带的 identity', async () => {
    const pending = translation({
      scope: 'full',
      block_key: 'content',
      source_content_revision: 8,
      translated_text: null,
      status: 'pending',
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([], 8),
      })),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: pending })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() =>
      useTranslations(client, 'L1', { contentRevision: 8 }),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.create({
        scope: 'full',
        force: false,
        expected_content_revision: 7,
      } as unknown as Parameters<typeof result.current.create>[0])
    })

    expect(client.createTranslation).toHaveBeenCalledWith('L1', {
      scope: 'full',
      force: false,
      expected_content_revision: 8,
    })
  })

  it('内部派生 summary hash，hash 尚未验证时不发排期请求', async () => {
    const hash = SUMMARY_HASH_A
    const pending = translation({
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
      translated_text: null,
      status: 'pending',
    })
    let resolveVerifiedList!: (value: {
      ok: true
      data: TranslationListResponse
    }) => void
    const verifiedListRequest = new Promise<{
      ok: true
      data: TranslationListResponse
    }>((resolve) => {
      resolveVerifiedList = resolve
    })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([]) })
        .mockReturnValue(verifiedListRequest),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: pending })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ summarySourceHash }) =>
        useTranslations(client, 'L1', {
          contentRevision: 7,
          summarySourceHash,
      }),
      { initialProps: { summarySourceHash: null as string | null } },
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    await expect(result.current.create({
      scope: 'selection',
      block_key: 'summary',
      start_offset: 0,
      end_offset: 5,
      source_text: 'hello',
      force: false,
    })).resolves.toMatchObject({ ok: false })
    expect(client.createTranslation).not.toHaveBeenCalled()

    rerender({ summarySourceHash: hash })
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(2))
    await act(async () => {
      resolveVerifiedList({
        ok: true,
        data: translationList([], 7, SUMMARY_HASH_A),
      })
      await verifiedListRequest
    })
    await act(async () => {
      await result.current.create({
        scope: 'selection',
        block_key: 'summary',
        start_offset: 0,
        end_offset: 5,
        source_text: 'hello',
        force: false,
      })
    })
    expect(client.createTranslation).toHaveBeenCalledWith('L1', {
      scope: 'selection',
      block_key: 'summary',
      start_offset: 0,
      end_offset: 5,
      source_text: 'hello',
      force: false,
      expected_source_hash: hash,
    })
  })

  it('不把 saved create 响应并入不同 revision 的 list envelope', async () => {
    const created = translation({
      id: 'created-rev-8',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 8,
    })
    const authoritative = translationList([created], 8)
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([], 7) })
        .mockResolvedValueOnce({ ok: true as const, data: authoritative }),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: created })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await waitFor(() => expect(result.current.currentContentRevision).toBe(7))

    await act(async () => {
      const response = await result.current.create({
        scope: 'full',
        force: false,
      })
      expect(response.ok).toBe(true)
    })

    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(result.current.currentContentRevision).toBe(8))
    expect(result.current.items).toEqual([created])
    expect(resourceStore.peek<TranslationListResponse>(translationsKey('L1')).data).toEqual(
      authoritative,
    )
  })

  it('summary 改变后拒绝旧来源的迟到 create 响应且不污染当前缓存', async () => {
    const createdForOldSummary = translation({
      id: 'created-for-old-summary',
      scope: 'selection',
      block_key: 'summary',
      source_content_revision: null,
    })
    let resolveCreate!: (value: { ok: true; data: TranslationResponse }) => void
    const createRequest = new Promise<{ ok: true; data: TranslationResponse }>((resolve) => {
      resolveCreate = resolve
    })
    const client = {
      getTranslations: vi.fn(async () => ({ ok: true as const, data: translationList([]) })),
      createTranslation: vi.fn(() => createRequest),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ summarySourceHash }) =>
        useTranslations(client, 'L1', {
          contentRevision: 7,
          summarySourceHash,
        }),
      { initialProps: { summarySourceHash: SUMMARY_HASH_A } },
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    let pending!: ReturnType<typeof result.current.create>
    act(() => {
      pending = result.current.create({
        scope: 'selection',
        block_key: 'summary',
        start_offset: 0,
        end_offset: 5,
        source_text: 'hello',
        force: false,
      })
    })
    rerender({ summarySourceHash: SUMMARY_HASH_B })
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(2))

    await act(async () => {
      resolveCreate({ ok: true, data: createdForOldSummary })
    })
    await expect(pending).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(result.current.items).toEqual([])
    expect(
      resourceStore
        .peek<TranslationListResponse>(translationsKey('L1', 7))
        .data?.items.some((item) => item.id === createdForOldSummary.id),
    ).toBe(false)
    await waitFor(() => expect(client.getTranslations).toHaveBeenCalledTimes(3))
  })

  it('does not let an A-era create continuation patch B translation cache', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('translation create test')
    const existingA = translation({ id: 'A-existing' })
    const existingB = translation({ id: 'B-existing', source_text: 'belongs to B' })
    const createdByA = translation({ id: 'A-created', translated_text: null, status: 'pending' })
    let resolveCreate!: (value: { ok: true; data: TranslationResponse }) => void
    const createRequest = new Promise<{ ok: true; data: TranslationResponse }>((resolve) => {
      resolveCreate = resolve
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([existingA]),
      })),
      createTranslation: vi.fn(() => createRequest),
      isIdentityCurrent: vi.fn(() => leaseA.isCurrent(ownershipA)),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await waitFor(() => expect(result.current.items).toEqual([existingA]))

    let pending!: Promise<unknown>
    act(() => {
      pending = result.current.create({
        scope: 'full',
        force: false,
      })
    })
    await waitFor(() => expect(client.createTranslation).toHaveBeenCalledTimes(1))

    const bSnapshot = translationList([existingB], 11)
    act(() => {
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
      resourceStore.set(translationsKey('L1'), bSnapshot)
    })

    await act(async () => {
      resolveCreate({ ok: true, data: createdByA })
      await pending
    })

    expect(resourceStore.peek(translationsKey('L1')).data).toBe(bSnapshot)
  })

  it('仅在存在活动任务时轮询并合并完成结果', async () => {
    vi.useFakeTimers()
    const pending = translation({ translated_text: null, status: 'pending', model: null })
    const done = translation({ translated_text: '你好', status: 'done' })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([]) })
        .mockResolvedValueOnce({ ok: true as const, data: translationList([done]) }),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: pending })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await act(async () => {})

    await act(async () => {
      await result.current.create({
        scope: 'full',
        force: false,
      })
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1200)
    })

    expect(client.getTranslations).toHaveBeenCalledTimes(2)
    expect(result.current.items).toEqual([done])
    expect(result.current.hasActiveJobs).toBe(false)
  })

  it('过期或无法验证的 pending 译文不会维持轮询', async () => {
    vi.useFakeTimers()
    const stalePending = translation({
      id: 'stale-pending',
      scope: 'full',
      block_key: 'content',
      translated_text: null,
      status: 'pending',
      model: null,
      source_content_revision: 7,
    })
    const legacyPending = translation({
      id: 'legacy-pending',
      scope: 'selection',
      block_key: 'content',
      translated_text: null,
      status: 'processing',
      model: null,
      source_content_revision: null,
    })
    const client = {
      getTranslations: vi.fn(async () => ({
        ok: true as const,
        data: translationList([stalePending, legacyPending], 8),
      })),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient

    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await act(async () => {})

    expect(result.current.items).toEqual([])
    expect(result.current.hasActiveJobs).toBe(false)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2400)
    })
    expect(client.getTranslations).toHaveBeenCalledTimes(1)
  })

  it('慢请求期间不会启动重叠轮询，完成后仍能收敛', async () => {
    vi.useFakeTimers()
    const pending = translation({ translated_text: null, status: 'pending', model: null })
    const done = translation({ translated_text: '你好', status: 'done' })
    let resolvePoll!: (value: { ok: true; data: TranslationListResponse }) => void
    const slowPoll = new Promise<{ ok: true; data: TranslationListResponse }>(
      (resolve) => {
        resolvePoll = resolve
      },
    )
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([]) })
        .mockReturnValueOnce(slowPoll),
      createTranslation: vi.fn(async () => ({ ok: true as const, data: pending })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await act(async () => {})

    await act(async () => {
      await result.current.create({
        scope: 'full',
        force: false,
      })
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2400)
    })
    expect(client.getTranslations).toHaveBeenCalledTimes(2)

    await act(async () => {
      resolvePoll({ ok: true, data: translationList([done]) })
      await slowPoll
    })
    expect(result.current.items).toEqual([done])
    expect(result.current.hasActiveJobs).toBe(false)
  })

  it('切换文章后丢弃上一条链接的迟到响应', async () => {
    let resolveFirst!: (value: { ok: true; data: TranslationListResponse }) => void
    const firstRequest = new Promise<{ ok: true; data: TranslationListResponse }>(
      (resolve) => {
        resolveFirst = resolve
      },
    )
    const second = translation({ id: 'T2', link_id: 'L2', source_text: 'second' })
    const client = {
      getTranslations: vi.fn((id: string) =>
        id === 'L1'
          ? firstRequest
          : Promise.resolve({ ok: true as const, data: translationList([second]) }),
      ),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ linkId }) => useTranslations(client, linkId),
      { initialProps: { linkId: 'L1' } },
    )

    rerender({ linkId: 'L2' })
    await waitFor(() => expect(result.current.items).toEqual([second]))
    await act(async () => {
      resolveFirst({ ok: true, data: translationList([translation({ source_text: 'first' })]) })
    })

    expect(result.current.items).toEqual([second])
  })

  it('显式重载会绕过同链接旧请求并丢弃其迟到响应', async () => {
    let resolveOld!: (value: { ok: true; data: TranslationListResponse }) => void
    const oldRequest = new Promise<{ ok: true; data: TranslationListResponse }>(
      (resolve) => {
        resolveOld = resolve
      },
    )
    const fresh = translation({ id: 'fresh', stale: true })
    const client = {
      getTranslations: vi
        .fn()
        .mockReturnValueOnce(oldRequest)
        .mockResolvedValueOnce({ ok: true as const, data: translationList([fresh]) }),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))

    await act(async () => {
      await result.current.reload()
    })
    expect(client.getTranslations).toHaveBeenCalledTimes(2)
    expect(result.current.staleItems).toEqual([fresh])

    await act(async () => {
      resolveOld({ ok: true, data: translationList([translation({ id: 'old' })]) })
      await oldRequest
    })
    expect(result.current.staleItems).toEqual([fresh])
    expect(result.current.loading).toBe(false)
  })

  it('原文变化重载会在网络返回前先使全文译文失效', async () => {
    const full = translation({
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
      stale: false,
    })
    let resolveReload!: (value: { ok: true; data: TranslationListResponse }) => void
    const reloadRequest = new Promise<{ ok: true; data: TranslationListResponse }>(
      (resolve) => {
        resolveReload = resolve
      },
    )
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([full]) })
        .mockReturnValueOnce(reloadRequest),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await waitFor(() => expect(result.current.items).toEqual([full]))

    let pendingReload!: Promise<void>
    act(() => {
      pendingReload = result.current.reloadAfterSourceChange('L1')
    })
    expect(result.current.items).toEqual([])
    expect(result.current.staleItems[0]?.id).toBe(full.id)

    await act(async () => {
      resolveReload({ ok: true, data: translationList([{ ...full, stale: true }]) })
      await pendingReload
    })
    expect(result.current.staleItems[0]?.stale).toBe(true)
  })

  it('原文变化后的 GET 失败也不会恢复旧全文译文', async () => {
    const full = translation({
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
      stale: false,
    })
    const client = {
      getTranslations: vi
        .fn()
        .mockResolvedValueOnce({ ok: true as const, data: translationList([full]) })
        .mockResolvedValueOnce({
          ok: false as const,
          error: { kind: 'other' as const, message: 'network unavailable' },
        }),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTranslations(client, 'L1'))
    await waitFor(() => expect(result.current.items).toEqual([full]))

    await act(async () => {
      await result.current.reloadAfterSourceChange('L1')
    })

    expect(result.current.items).toEqual([])
    expect(result.current.staleItems[0]?.id).toBe(full.id)
    expect(result.current.error?.message).toBe('network unavailable')
  })

  it('旧链接原文变化完成时不会污染已切换链接或丢弃其响应', async () => {
    const first = translation({
      id: 'T1',
      link_id: 'L1',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
      stale: false,
    })
    const second = translation({
      id: 'T2',
      link_id: 'L2',
      scope: 'full',
      block_key: 'content',
      source_content_revision: 7,
      stale: false,
    })
    let resolveSecond!: (value: {
      ok: true
      data: TranslationListResponse
    }) => void
    const secondRequest = new Promise<{
      ok: true
      data: TranslationListResponse
    }>((resolve) => {
      resolveSecond = resolve
    })
    const client = {
      getTranslations: vi.fn((id: string) =>
        id === 'L1'
          ? Promise.resolve({ ok: true as const, data: translationList([first]) })
          : secondRequest,
      ),
      createTranslation: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result, rerender } = renderHook(
      ({ linkId }) => useTranslations(client, linkId),
      { initialProps: { linkId: 'L1' } },
    )
    await waitFor(() => expect(result.current.items).toEqual([first]))
    const staleOldLink = result.current.reloadAfterSourceChange

    rerender({ linkId: 'L2' })
    await act(async () => {
      await staleOldLink('L1')
      resolveSecond({ ok: true, data: translationList([second]) })
      await secondRequest
    })

    expect(client.getTranslations).toHaveBeenCalledTimes(2)
    expect(result.current.items).toEqual([second])
    expect(result.current.items[0]?.stale).toBe(false)
  })
})
