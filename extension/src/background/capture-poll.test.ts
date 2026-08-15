/**
 * capture-poll.test.ts — 采集轮询决策纯函数单元测试。
 *
 * decidePollOutcome 是无 await / 无 setTimeout 的纯函数，因此本测试
 * 不需要 fake timer，所有分支由纯计算覆盖。
 *
 * 覆盖：
 *   - mapJobStatus：四种后端 JobStatus → CaptureStage 映射
 *   - decidePollOutcome：
 *       · done    —— 后端 done
 *       · failed  —— 后端 failed（透传 error_msg / error_category / 皆空回退）
 *       · retry   —— pending / processing 中间态、getJob 网络抖动
 *       · 次数耗尽 —— 最后一次仍非终态 / 最后一次 getJob 仍失败 → failed
 */
import { describe, expect, it } from 'vitest'
import type { ApiResult } from '@/api/webtag-client'
import type { Job } from '@/api/types'
import {
  MAX_POLL_ATTEMPTS,
  decidePollOutcome,
  mapJobStatus,
} from './capture-poll'

// ── 夹具 ────────────────────────────────────────────────────

const ok = (data: Job): ApiResult<Job> => ({ ok: true, data })
const fail = (kind: string): ApiResult<Job> => ({
  ok: false,
  error: { kind: kind as never, message: 'err' },
})

function job(
  status: Job['status'],
  errorMsg: string | null = null,
  errorCategory: string | null = null,
): Job {
  return {
    id: 'job-1',
    link_id: 'link-1',
    status,
    error_category: errorCategory,
    error_msg: errorMsg,
    link: null,
  }
}

// ── mapJobStatus ────────────────────────────────────────────

describe('mapJobStatus', () => {
  it('pending → submitted', () => {
    expect(mapJobStatus('pending')).toBe('submitted')
  })
  it('processing → parsing', () => {
    expect(mapJobStatus('processing')).toBe('parsing')
  })
  it('done → done', () => {
    expect(mapJobStatus('done')).toBe('done')
  })
  it('failed → failed', () => {
    expect(mapJobStatus('failed')).toBe('failed')
  })
})

// ── decidePollOutcome：done ─────────────────────────────────

describe('decidePollOutcome — done', () => {
  it('后端 done 时返回 action=done', () => {
    const outcome = decidePollOutcome(ok(job('done')), 0, MAX_POLL_ATTEMPTS)
    expect(outcome.action).toBe('done')
  })

  it('即使在最后一次轮询命中 done 也返回 done', () => {
    const outcome = decidePollOutcome(
      ok(job('done')),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome.action).toBe('done')
  })
})

// ── decidePollOutcome：failed ───────────────────────────────

describe('decidePollOutcome — failed', () => {
  it('后端 failed 且有 error_msg 时透传 errorMessage', () => {
    const outcome = decidePollOutcome(
      ok(job('failed', '抓取失败', 'fetch_error')),
      0,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({
      action: 'failed',
      errorKind: 'job-failed',
      errorMessage: '抓取失败',
    })
  })

  it('后端 failed 仅有 error_category 时用 error_category 作 errorMessage', () => {
    const outcome = decidePollOutcome(
      ok(job('failed', null, 'parse_error')),
      0,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({
      action: 'failed',
      errorKind: 'job-failed',
      errorMessage: 'parse_error',
    })
  })

  it('后端 failed 且 error_msg/error_category 皆空时不带 errorMessage', () => {
    const outcome = decidePollOutcome(
      ok(job('failed', null, null)),
      0,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'failed', errorKind: 'job-failed' })
    expect('errorMessage' in outcome).toBe(false)
  })

  it('error_msg 优先于 error_category', () => {
    const outcome = decidePollOutcome(
      ok(job('failed', '具体原因', 'category-x')),
      0,
      MAX_POLL_ATTEMPTS,
    )
    if (outcome.action !== 'failed') throw new Error('期望 failed')
    expect(outcome.errorMessage).toBe('具体原因')
  })
})

// ── decidePollOutcome：retry ────────────────────────────────

describe('decidePollOutcome — retry', () => {
  it('后端 pending 时返回 retry，stage=submitted', () => {
    const outcome = decidePollOutcome(ok(job('pending')), 0, MAX_POLL_ATTEMPTS)
    expect(outcome).toEqual({ action: 'retry', stage: 'submitted' })
  })

  it('后端 processing 时返回 retry，stage=parsing', () => {
    const outcome = decidePollOutcome(
      ok(job('processing')),
      5,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'retry', stage: 'parsing' })
  })

  it('getJob 网络失败但非最后一次时返回 retry，stage=parsing', () => {
    const outcome = decidePollOutcome(
      fail('network-unreachable'),
      0,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'retry', stage: 'parsing' })
  })
})

// ── decidePollOutcome：次数耗尽 ─────────────────────────────

describe('decidePollOutcome — 轮询次数耗尽', () => {
  it('最后一次仍 processing 时返回 still-processing（非失败）', () => {
    // UX 加固：轮询预算耗尽但后端仍在解析，不再误报 failed，
    // 而是独立的中性 still-processing 终态（LLM 慢解析常超出轮询窗口）。
    const outcome = decidePollOutcome(
      ok(job('processing')),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'still-processing' })
  })

  it('最后一次仍 pending 时返回 still-processing（非失败）', () => {
    const outcome = decidePollOutcome(
      ok(job('pending')),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'still-processing' })
  })

  it('still-processing 不带 errorKind / errorMessage', () => {
    const outcome = decidePollOutcome(
      ok(job('pending')),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect('errorKind' in outcome).toBe(false)
    expect('errorMessage' in outcome).toBe(false)
  })

  it('最后一次 getJob 仍网络失败时返回 failed，透传 API errorKind（仍是失败）', () => {
    // 注意：getJob 网络失败与「后端仍 pending/processing」是两回事。
    // 前者是客户端侧请求失败 → failed；后者是后端还在跑 → still-processing。
    const outcome = decidePollOutcome(
      fail('timeout'),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'failed', errorKind: 'timeout' })
  })

  it('倒数第二次 getJob 失败仍返回 retry（仅最后一次才判定）', () => {
    const outcome = decidePollOutcome(
      fail('timeout'),
      MAX_POLL_ATTEMPTS - 2,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({ action: 'retry', stage: 'parsing' })
  })

  it('小 maxAttempts 下 attempt=0 即为最后一次的边界处理（仍 processing → still-processing）', () => {
    // maxAttempts=1 时 attempt=0 就是最后一次。
    const outcome = decidePollOutcome(ok(job('processing')), 0, 1)
    expect(outcome).toEqual({ action: 'still-processing' })
  })

  it('后端最后一次明确返回 failed 时仍是 failed（区别于 still-processing）', () => {
    // 预算耗尽不一律 still-processing：后端明确 failed 优先走 failed 分支。
    const outcome = decidePollOutcome(
      ok(job('failed', '后端解析失败')),
      MAX_POLL_ATTEMPTS - 1,
      MAX_POLL_ATTEMPTS,
    )
    expect(outcome).toEqual({
      action: 'failed',
      errorKind: 'job-failed',
      errorMessage: '后端解析失败',
    })
  })
})
