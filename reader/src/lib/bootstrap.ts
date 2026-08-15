/**
 * 同源后端探测。
 *
 * Reader 现在由 Go 后端直接 serve（`/reader/`，见 internal/app/reader.go），这种
 * 形态下「后端地址」是已知的——就是当前 origin。再让用户手抄一遍地址是纯粹的
 * 摩擦：他刚刚就是从这个地址打开的页面。
 *
 * 探测分两步，都不抛异常：
 *   1. GET {origin}/health —— 判断同源是不是一台 Cairn 后端（而不是 nginx
 *      静态站或别的什么）。认 status + version 两个字段，比只看 200 可靠。
 *   2. 以 no-store GET {origin}/api/session 执行严格 v2 identity handshake。
 *      自托管开放模式下可以，此时整个连接引导页都可以跳过；私有集合在
 *      identity 安装前始终不读取。
 *
 * 独立部署（自己拿 nginx serve dist、后端在别的域名）时第 1 步就失败，行为
 * 与改造前完全一致：照常显示连接引导页。
 */

import { DATA_NAMESPACE_HEADER, isSessionIdentity } from './api/client'

/** 探测结果。baseURL 为 null 表示同源不是 Cairn 后端。 */
export interface BackendProbe {
  /** 同源后端地址（探测失败为 null）。 */
  baseURL: string | null
  /** 不带 token 即可读（自托管开放模式）。baseURL 为 null 时无意义。 */
  openAccess: boolean
}

const NOT_FOUND: BackendProbe = { baseURL: null, openAccess: false }

/** 探测超时。同源请求正常在毫秒级返回，2s 足够覆盖冷启动。 */
const PROBE_TIMEOUT_MS = 2000

async function getJSON(
  url: string,
  fetchImpl: typeof fetch,
  init: RequestInit = {},
): Promise<{ status: number; body: unknown; dataNamespace: string | null } | null> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), PROBE_TIMEOUT_MS)
  try {
    const headers = new Headers(init.headers)
    headers.set('Accept', 'application/json')
    const res = await fetchImpl(url, {
      ...init,
      signal: ctrl.signal,
      headers,
    })
    let body: unknown = null
    try {
      body = await res.json()
    } catch {
      body = null
    }
    return {
      status: res.status,
      body,
      dataNamespace: res.headers.get(DATA_NAMESPACE_HEADER),
    }
  } catch {
    return null
  } finally {
    clearTimeout(timer)
  }
}

/** /health 响应是否来自一台 Cairn 后端。 */
function looksLikeWebTagHealth(body: unknown): boolean {
  if (typeof body !== 'object' || body === null) return false
  const b = body as Record<string, unknown>
  return b.status === 'ok' && typeof b.version === 'string'
}

/**
 * 探测同源后端。origin 默认取当前页面，测试可注入。
 * 任何失败都收敛为 NOT_FOUND，调用方不需要 try/catch。
 */
export async function probeSameOriginBackend(
  origin: string = typeof window === 'undefined' ? '' : window.location.origin,
  fetchImpl: typeof fetch = typeof fetch === 'undefined'
    ? (() => Promise.reject(new Error('no fetch'))) as unknown as typeof fetch
    : fetch,
): Promise<BackendProbe> {
  const base = origin.trim().replace(/\/+$/, '')
  if (!base || !/^https?:\/\//.test(base)) return NOT_FOUND

  const health = await getJSON(`${base}/health`, fetchImpl)
  if (!health || health.status !== 200 || !looksLikeWebTagHealth(health.body)) return NOT_FOUND

  const probe = await getJSON(`${base}/api/session`, fetchImpl, { cache: 'no-store' })
  // 探测请求本身失败（网络抖动）时保守判为需要 token：引导页仍会出现，
  // 用户填完照样能用；反过来误判为开放会让人对着一个空列表猜错在哪。
  const openAccess =
    probe !== null &&
    probe.status === 200 &&
    isSessionIdentity(probe.body) &&
    probe.dataNamespace === probe.body.client_data_namespace

  return { baseURL: base, openAccess }
}
