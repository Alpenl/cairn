/**
 * 当前运行的 Core 构建标识，以及一次**只读**的 Release 查询。
 *
 * 事实来源是后端已有的 `/health`（internal/app/router_static.go）：它返回
 * status / version / commit / build_time 四个字段，由构建期 `-ldflags` 注入
 * `internal/buildinfo`。未注入时后端自己兜底成 `0.0.0` / `unknown`，这正是
 * 本地 `go run` 与开发镜像的形态，所以「开发构建」不需要额外接口去判断。
 *
 * Release 查询是可选的旁支：它只读 GitHub 上与**当前版本完全对应**的那个
 * Release，用来把 changelog 链接出去。任何失败都收敛成一个状态值，绝不能影响
 * 当前版本的展示——GitHub 挂了不等于你不知道自己在跑哪个版本。这里没有、也
 * 不会有任何写路径：不下载制品、不比较「有没有新版本」、不触发升级。
 */

import type { HealthResponse } from './api/types'

/** 正式 Release 的唯一来源仓库。 */
export const CORE_RELEASE_REPO = 'Alpenl/cairn'

/** `internal/buildinfo` 在未注入 ldflags 时的占位值。 */
const PLACEHOLDER_VERSION = '0.0.0'
const PLACEHOLDER_UNKNOWN = 'unknown'

/** Release 查询超时。只读旁支，宁可早点放弃也不要卡住设置页。 */
const RELEASE_LOOKUP_TIMEOUT_MS = 6000

/** 页面显示的 Core 构建标识。 */
export interface CoreBuildIdentity {
  readonly version: string
  readonly commit: string
  readonly buildTime: string
}

/** 把 `/health` 响应收敛为展示用标识。 */
export function coreBuildIdentity(health: HealthResponse): CoreBuildIdentity {
  return {
    version: health.version.trim(),
    commit: health.commit.trim(),
    buildTime: health.build_time.trim(),
  }
}

/**
 * 是否是没有注入构建信息的开发构建。
 *
 * 判据就是后端的兜底值本身：版本还是 `0.0.0`，或 commit 还是 `unknown`。
 * 开发构建在 GitHub 上没有对应 Release，查询它只会得到一个误导性的 404。
 */
export function isDevelopmentBuild(identity: CoreBuildIdentity): boolean {
  return identity.version === '' ||
    identity.version === PLACEHOLDER_VERSION ||
    identity.commit === '' ||
    identity.commit === PLACEHOLDER_UNKNOWN
}

/**
 * 短 commit。
 *
 * 只对看起来像 git 对象名的值截断；其它值原样返回，免得把一个已经很短的
 * 标识切成看不出来源的碎片。
 */
export function shortCommit(commit: string): string {
  const trimmed = commit.trim()
  if (!/^[0-9a-f]{7,40}$/i.test(trimmed)) return trimmed
  return trimmed.slice(0, 7)
}

/** 可读的构建时间；`unknown` 或无法解析时返回 null，由调用方决定怎么显示。 */
export function formatBuildTime(buildTime: string): string | null {
  const trimmed = buildTime.trim()
  if (!trimmed || trimmed === PLACEHOLDER_UNKNOWN) return null
  const parsed = new Date(trimmed)
  if (Number.isNaN(parsed.getTime())) return null
  return parsed.toLocaleString('zh-CN', { hour12: false })
}

/** 当前版本对应的 Release tag。 */
export function coreReleaseTag(version: string): string {
  const trimmed = version.trim()
  return trimmed.startsWith('v') ? trimmed : `v${trimmed}`
}

/**
 * Release 页面地址。
 *
 * 由仓库常量 + tag 在本地拼出，**不采用** GitHub 响应体里的 `html_url`：
 * 页面上的链接不应该由一个远端 JSON 字段决定指向哪里。
 */
export function coreReleaseURL(tag: string): string {
  return `https://github.com/${CORE_RELEASE_REPO}/releases/tag/${encodeURIComponent(tag)}`
}

/** 精确 Release 查询结果。三种终态都不抛异常。 */
export type CoreReleaseLookup =
  | {
      readonly kind: 'found'
      readonly tag: string
      readonly title: string | null
      readonly publishedAt: string | null
      readonly url: string
    }
  /** GitHub 可达，但这个版本没有对应的正式 Release。 */
  | { readonly kind: 'missing'; readonly tag: string }
  /** GitHub 不可达、限流或返回了读不懂的东西。 */
  | { readonly kind: 'unavailable' }

interface ReleaseLookupOptions {
  readonly fetchImpl?: typeof fetch
  readonly signal?: AbortSignal
  readonly timeoutMs?: number
}

function releaseTitle(body: unknown): string | null {
  if (typeof body !== 'object' || body === null) return null
  const name = (body as Record<string, unknown>).name
  if (typeof name !== 'string') return null
  const trimmed = name.trim()
  return trimmed === '' ? null : trimmed
}

function releasePublishedAt(body: unknown): string | null {
  if (typeof body !== 'object' || body === null) return null
  const published = (body as Record<string, unknown>).published_at
  if (typeof published !== 'string') return null
  const parsed = new Date(published)
  if (Number.isNaN(parsed.getTime())) return null
  return parsed.toLocaleDateString('zh-CN')
}

/**
 * 读取与 `version` 精确对应的 Release。
 *
 * 只做一次匿名 GET，不带任何凭证，不跟随到 `latest`。调用方拿到的永远是一个
 * 状态值——网络失败、超时、限流、非 JSON 全部归一为 `unavailable`。
 */
export async function lookupCoreRelease(
  version: string,
  options: ReleaseLookupOptions = {},
): Promise<CoreReleaseLookup> {
  const tag = coreReleaseTag(version)
  const fetchImpl = options.fetchImpl ?? (typeof fetch === 'undefined' ? null : fetch)
  if (!fetchImpl) return { kind: 'unavailable' }

  const controller = new AbortController()
  const abortForCaller = () => controller.abort()
  if (options.signal?.aborted) controller.abort()
  else options.signal?.addEventListener('abort', abortForCaller, { once: true })
  const timer = setTimeout(() => controller.abort(), options.timeoutMs ?? RELEASE_LOOKUP_TIMEOUT_MS)

  try {
    const url = `https://api.github.com/repos/${CORE_RELEASE_REPO}/releases/tags/${encodeURIComponent(tag)}`
    let response: Response
    try {
      response = await fetchImpl(url, {
        method: 'GET',
        cache: 'no-store',
        credentials: 'omit',
        referrerPolicy: 'no-referrer',
        headers: { Accept: 'application/vnd.github+json' },
        signal: controller.signal,
      })
    } catch {
      return { kind: 'unavailable' }
    }

    if (response.status === 404) return { kind: 'missing', tag }
    if (!response.ok) return { kind: 'unavailable' }

    let body: unknown = null
    try {
      body = await response.json()
    } catch {
      return { kind: 'unavailable' }
    }

    return {
      kind: 'found',
      tag,
      title: releaseTitle(body),
      publishedAt: releasePublishedAt(body),
      url: coreReleaseURL(tag),
    }
  } finally {
    clearTimeout(timer)
    options.signal?.removeEventListener('abort', abortForCaller)
  }
}
