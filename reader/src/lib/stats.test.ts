import { describe, expect, it } from 'vitest'
import { domainStats, relatedTags, tagStats } from './stats'
import { makeLink } from '../test/fixtures'

describe('tagStats', () => {
  it('聚合标签计数并取最近 lastAt', () => {
    const links = [
      makeLink({ id: '1', tags: ['a', 'b'], created_at: '2026-01-01T00:00:00Z' }),
      makeLink({ id: '2', tags: ['a'], created_at: '2026-02-01T00:00:00Z' }),
    ]
    const stats = tagStats(links)
    const a = stats.find((s) => s.tag === 'a')
    expect(a?.count).toBe(2)
    expect(a?.lastAt).toBe('2026-02-01T00:00:00Z')
  })
})

describe('domainStats', () => {
  it('忽略空域名并聚合', () => {
    const links = [
      makeLink({ id: '1', domain: 'x.com' }),
      makeLink({ id: '2', domain: 'x.com' }),
      makeLink({ id: '3', domain: null }),
    ]
    const stats = domainStats(links)
    expect(stats).toHaveLength(1)
    expect(stats[0].count).toBe(2)
  })
})

describe('relatedTags', () => {
  it('按共现 + 同域名加权排序相关标签，剔除自身标签', () => {
    const target = makeLink({ id: 't', tags: ['LLM'], domain: 'a.com' })
    const corpus = [
      target,
      makeLink({ id: '1', tags: ['LLM', 'RAG'], domain: 'a.com' }),
      makeLink({ id: '2', tags: ['LLM', 'Agent'], domain: 'b.com' }),
    ]
    const rel = relatedTags(target, corpus)
    expect(rel).not.toContain('LLM') // 自身标签剔除
    // RAG 同域名加权(1.5) > Agent(1)
    expect(rel[0]).toBe('RAG')
  })

  it('无 link 返回空', () => {
    expect(relatedTags(null, [])).toEqual([])
  })
})
