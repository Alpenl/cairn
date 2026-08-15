/**
 * @module popup/reader-url
 * 解析「在 Reader 中打开订阅视图」的目标地址。
 *
 * 两条来源，显式配置优先：
 *   1. 设置里手填的 readerUrl —— Reader 与后端分开部署时用（自己拿 nginx
 *      serve dist，或挂在另一个域名下）。
 *   2. 后端地址 + /reader/ —— Go 后端把 Reader 内嵌并挂在 /reader/ 下
 *      （internal/app/reader.go），这是默认部署形态。
 *
 * 兜底路径带尾斜杠：后端对 /reader 会 301 到 /reader/（并保留 query），直接
 * 给最终地址少一次往返，也少一个会出错的环节。
 *
 * 历史：这条兜底以前指向 `${backend}/reader`，而后端当时根本没有这条路由，
 * 未单独配 readerUrl 的用户点进来只会拿到 404。
 */
export interface ReaderUrlSettings {
  backendUrl: string
  readerUrl?: string
}

function httpUrl(raw: string): URL | null {
  try {
    const url = new URL(raw)
    return url.protocol === 'http:' || url.protocol === 'https:' ? url : null
  } catch {
    return null
  }
}

/** 解析订阅视图地址；地址不合法时返回 null，调用方据此隐藏入口。 */
export function resolveSubscriptionsReaderUrl(
  settings: ReaderUrlSettings,
): string | null {
  const configured = settings.readerUrl?.trim()
  if (configured) {
    const url = httpUrl(configured)
    if (!url) return null
    url.searchParams.set('view', 'subscriptions')
    return url.href
  }

  const backend = settings.backendUrl.trim().replace(/\/+$/, '')
  if (!backend || !httpUrl(backend)) return null
  return `${backend}/reader/?view=subscriptions`
}
