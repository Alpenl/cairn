import { blockedImageLabel } from './untrusted-content-media'

// noscript 必须在列：DOMParser 在 scripting-disabled 文档里把 <noscript> 子树当
// 普通 markup 解析，序列化回 innerHTML 后再交给 dangerouslySetInnerHTML 时，活文档
// 会把它当 RAWTEXT 重新解析，属性值里的尖括号足以重组出真实标签（经典 mXSS）。
// 服务端 sanitizer 与 CSP 目前都挡得住，但本函数也会作用于已缓存的 HTML，
// 不能把自身正确性寄托在上游。
const BLOCKED_ELEMENTS = [
  'script',
  'style',
  'iframe',
  'object',
  'embed',
  'form',
  'input',
  'button',
  'select',
  'textarea',
  'meta',
  'link',
  'base',
  'svg',
  'math',
  'template',
  'noscript',
  'video',
  'audio',
  'source',
  'track',
].join(',')

function safeRemoteURL(raw: string, baseURL: string): string | null {
  try {
    const parsed = new URL(raw, baseURL)
    return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : null
  } catch {
    return null
  }
}

/**
 * Sanitize feed-supplied HTML before rendering it in Reader.
 * DOMParser is deliberately used instead of string rewriting so malformed HTML and
 * encoded attributes are interpreted by the browser before the allow-list is applied.
 */
export function sanitizeFeedHTML(html: string, baseURL: string): string {
  const documentNode = new DOMParser().parseFromString(html, 'text/html')
  documentNode.querySelectorAll(BLOCKED_ELEMENTS).forEach((element) => element.remove())

  documentNode.body.querySelectorAll('*').forEach((element) => {
    const isBlockedImagePlaceholder =
      element instanceof HTMLSpanElement &&
      element.getAttribute('data-blocked-content') === 'image'
    const allowedAttributes =
      element instanceof HTMLAnchorElement
        ? new Set(['href', 'title'])
        : element instanceof HTMLImageElement
          ? new Set(['src', 'alt', 'title'])
          : isBlockedImagePlaceholder
            ? new Set(['class', 'role', 'aria-label', 'data-blocked-content'])
          : element instanceof HTMLTimeElement
            ? new Set(['datetime', 'title'])
            : new Set<string>()
    for (const attribute of [...element.attributes]) {
      if (!allowedAttributes.has(attribute.name.toLowerCase())) {
        element.removeAttribute(attribute.name)
      }
    }

    if (isBlockedImagePlaceholder) {
      const label = blockedImageLabel(element.textContent)
      element.className = 'reader-blocked-image'
      element.setAttribute('role', 'img')
      element.setAttribute('aria-label', label)
      element.dataset.blockedContent = 'image'
      element.textContent = label
      return
    }

    if (element instanceof HTMLAnchorElement) {
      const href = element.getAttribute('href')
      const safeHref = href ? safeRemoteURL(href, baseURL) : null
      if (safeHref) {
        element.href = safeHref
        element.target = '_blank'
        element.rel = 'noopener noreferrer nofollow'
      } else {
        element.removeAttribute('href')
        element.removeAttribute('target')
      }
    }

    if (element instanceof HTMLImageElement) {
      const label = blockedImageLabel(element.getAttribute('alt'))
      const placeholder = documentNode.createElement('span')
      placeholder.className = 'reader-blocked-image'
      placeholder.setAttribute('role', 'img')
      placeholder.setAttribute('aria-label', label)
      placeholder.dataset.blockedContent = 'image'
      placeholder.textContent = label
      element.replaceWith(placeholder)
    }
  })

  // A picture wrapper is inert after every source is removed and every img is
  // replaced. Unwrap it so its accessible fallback remains part of the body.
  documentNode.body.querySelectorAll('picture').forEach((picture) => {
    const fragment = documentNode.createDocumentFragment()
    while (picture.firstChild) fragment.append(picture.firstChild)
    picture.replaceWith(fragment)
  })

  return documentNode.body.innerHTML
}
