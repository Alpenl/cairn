import { describe, expect, it, vi } from 'vitest'
import type { HealthResponse } from './api/types'
import {
  CORE_RELEASE_REPO,
  coreBuildIdentity,
  coreReleaseTag,
  coreReleaseURL,
  formatBuildTime,
  isDevelopmentBuild,
  lookupCoreRelease,
  shortCommit,
} from './core-version'

function health(overrides: Partial<HealthResponse> = {}): HealthResponse {
  return {
    status: 'ok',
    version: '1.4.0',
    commit: '0123456789abcdef0123456789abcdef01234567',
    build_time: '2026-08-01T10:00:00Z',
    ...overrides,
  }
}

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response
}

describe('core build identity', () => {
  it('reads the four fields /health actually returns', () => {
    expect(coreBuildIdentity(health())).toEqual({
      version: '1.4.0',
      commit: '0123456789abcdef0123456789abcdef01234567',
      buildTime: '2026-08-01T10:00:00Z',
    })
  })

  it('treats the buildinfo placeholders as a development build', () => {
    // internal/buildinfo falls back to these exact values when ldflags were
    // never injected, so no extra endpoint is needed to detect a dev binary.
    expect(isDevelopmentBuild(coreBuildIdentity(health({ version: '0.0.0', commit: 'unknown' })))).toBe(true)
    expect(isDevelopmentBuild(coreBuildIdentity(health({ commit: 'unknown' })))).toBe(true)
    expect(isDevelopmentBuild(coreBuildIdentity(health()))).toBe(false)
  })

  it('shortens git object names but leaves other identifiers intact', () => {
    expect(shortCommit('0123456789abcdef0123456789abcdef01234567')).toBe('0123456')
    expect(shortCommit('unknown')).toBe('unknown')
    expect(shortCommit(' abcdef1 ')).toBe('abcdef1')
  })

  it('reports an unusable build time as null instead of printing a placeholder', () => {
    expect(formatBuildTime('unknown')).toBeNull()
    expect(formatBuildTime('')).toBeNull()
    expect(formatBuildTime('not-a-timestamp')).toBeNull()
    expect(formatBuildTime('2026-08-01T10:00:00Z')).not.toBeNull()
  })

  it('builds the release tag and link locally from the pinned repository', () => {
    expect(coreReleaseTag('1.4.0')).toBe('v1.4.0')
    expect(coreReleaseTag('v1.4.0')).toBe('v1.4.0')
    expect(coreReleaseURL('v1.4.0')).toBe(`https://github.com/${CORE_RELEASE_REPO}/releases/tag/v1.4.0`)
  })
})

describe('lookupCoreRelease', () => {
  it('asks for the exact tag of the running version, unauthenticated', async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(200, {
      name: 'Cairn 1.4.0',
      published_at: '2026-08-01T10:00:00Z',
      html_url: 'https://evil.example/not-github',
    }))

    const lookup = await lookupCoreRelease('1.4.0', { fetchImpl: fetchImpl as unknown as typeof fetch })

    expect(fetchImpl).toHaveBeenCalledTimes(1)
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe(`https://api.github.com/repos/${CORE_RELEASE_REPO}/releases/tags/v1.4.0`)
    expect(init.credentials).toBe('omit')
    expect(lookup.kind).toBe('found')
    // The link is rebuilt from the pinned repo, never taken from the payload.
    expect(lookup).toMatchObject({
      tag: 'v1.4.0',
      title: 'Cairn 1.4.0',
      url: `https://github.com/${CORE_RELEASE_REPO}/releases/tag/v1.4.0`,
    })
  })

  it('reports a version without a release as missing, not as a failure', async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(404, { message: 'Not Found' }))

    await expect(lookupCoreRelease('1.4.0', { fetchImpl: fetchImpl as unknown as typeof fetch }))
      .resolves.toEqual({ kind: 'missing', tag: 'v1.4.0' })
  })

  it('collapses network failure, rate limiting and unreadable bodies into unavailable', async () => {
    const rejecting = vi.fn(async () => { throw new TypeError('Failed to fetch') })
    await expect(lookupCoreRelease('1.4.0', { fetchImpl: rejecting as unknown as typeof fetch }))
      .resolves.toEqual({ kind: 'unavailable' })

    const rateLimited = vi.fn(async () => jsonResponse(403, { message: 'rate limit exceeded' }))
    await expect(lookupCoreRelease('1.4.0', { fetchImpl: rateLimited as unknown as typeof fetch }))
      .resolves.toEqual({ kind: 'unavailable' })

    const unreadable = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => { throw new SyntaxError('not json') },
    } as unknown as Response))
    await expect(lookupCoreRelease('1.4.0', { fetchImpl: unreadable as unknown as typeof fetch }))
      .resolves.toEqual({ kind: 'unavailable' })
  })
})
