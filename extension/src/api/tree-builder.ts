import type { Link, TreeNode, TreeResponse } from './types'

const MAX_TREE_DEPTH = 32

function normalizeURL(rawURL: string): string {
  try {
    return new URL(rawURL).href
  } catch {
    return rawURL.trim()
  }
}

function urlLookupKeys(rawURL: string): string[] {
  const normalized = normalizeURL(rawURL)
  const keys = [normalized]
  try {
    const parsed = new URL(normalized)
    if (parsed.pathname !== '/' && !parsed.search && !parsed.hash) {
      parsed.pathname = parsed.pathname.endsWith('/')
        ? parsed.pathname.replace(/\/+$/, '')
        : `${parsed.pathname}/`
      if (parsed.pathname === '') parsed.pathname = '/'
      if (parsed.href !== normalized) keys.push(parsed.href)
    }
  } catch {
    // Invalid URLs only participate by their trimmed raw string.
  }
  return keys
}

function ancestorURLs(rawURL: string, maxDepth: number): string[] {
  try {
    const parsed = new URL(rawURL)
    const normalized = parsed.href
    const base = `${parsed.origin}/`
    const segments = parsed.pathname.split('/').filter(Boolean)
    const limit = Math.min(segments.length, maxDepth)
    if (limit === 0) return []
    const out: string[] = [base]
    for (let i = 1; i < limit; i += 1) {
      out.push(`${parsed.origin}/${segments.slice(0, i).join('/')}/`)
    }
    return out.filter((candidate) => candidate !== normalized)
  } catch {
    return []
  }
}

function virtualID(url: string): string {
  return `virtual:${url}`
}

function virtualTitle(url: string): string {
  try {
    const parsed = new URL(url)
    const segments = parsed.pathname.split('/').filter(Boolean)
    if (segments.length === 0) return parsed.hostname
    const last = segments[segments.length - 1] ?? parsed.hostname
    try {
      return decodeURIComponent(last)
    } catch {
      return last
    }
  } catch {
    return url
  }
}

function virtualDomain(url: string): string | null {
  try {
    return new URL(url).hostname
  } catch {
    return null
  }
}

function virtualDepth(url: string): number | null {
  try {
    return new URL(url).pathname.split('/').filter(Boolean).length
  } catch {
    return null
  }
}

function compareLinks(a: Link, b: Link): number {
  const depthA = a.path_depth ?? virtualDepth(a.url) ?? Number.MAX_SAFE_INTEGER
  const depthB = b.path_depth ?? virtualDepth(b.url) ?? Number.MAX_SAFE_INTEGER
  if (depthA !== depthB) return depthA - depthB
  if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1
  return a.id.localeCompare(b.id)
}

function compareNodes(a: TreeNode, b: TreeNode): number {
  const depthA = a.path_depth ?? Number.MAX_SAFE_INTEGER
  const depthB = b.path_depth ?? Number.MAX_SAFE_INTEGER
  if (depthA !== depthB) return depthA - depthB
  if (a.virtual !== b.virtual) return a.virtual ? -1 : 1
  return (a.title || a.url).localeCompare(b.title || b.url)
}

function toTreeNode(link: Link, parentId: string | null): TreeNode {
  return {
    id: link.id,
    url: link.url,
    title: link.title,
    summary: link.summary,
    description: link.description,
    tags: [...link.tags],
    content_type: link.content_type,
    status: link.status,
    domain: link.domain,
    path_depth: link.path_depth,
    parent_id: parentId,
    fetcher_type: link.fetcher_type,
    is_low_confidence: link.is_low_confidence,
    low_confidence_reason: link.low_confidence_reason,
    created_at: link.created_at,
    updated_at: link.updated_at,
    children: [],
  }
}

function toVirtualNode(
  url: string,
  parentId: string | null,
  source: Link,
): TreeNode {
  return {
    id: virtualID(url),
    url,
    title: virtualTitle(url),
    summary: null,
    description: null,
    tags: [],
    content_type: 'unknown',
    status: 'virtual',
    domain: virtualDomain(url) ?? source.domain,
    path_depth: virtualDepth(url),
    parent_id: parentId,
    fetcher_type: null,
    is_low_confidence: false,
    low_confidence_reason: null,
    created_at: source.created_at,
    updated_at: source.updated_at,
    virtual: true,
    children: [],
  }
}

function attachChildren(node: TreeNode, depth: number): TreeNode {
  if (depth >= MAX_TREE_DEPTH) {
    if (node.children.length > 0) {
      return { ...node, truncated: true, children: [] }
    }
    return { ...node, children: [] }
  }
  return {
    ...node,
    children: node.children
      .sort(compareNodes)
      .map((child) => attachChildren(child, depth + 1)),
  }
}

export function buildTreeFromLinks(links: Link[]): TreeResponse {
  const sorted = [...links].sort(compareLinks)
  const nodesByID = new Map<string, TreeNode>()
  const idByURL = new Map<string, string>()
  const parentByID = new Map<string, string | null>()

  for (const link of sorted) {
    for (const key of urlLookupKeys(link.url)) {
      if (!idByURL.has(key) || key === normalizeURL(link.url))
        idByURL.set(key, link.id)
    }
    nodesByID.set(link.id, toTreeNode(link, null))
  }

  for (const link of sorted) {
    let parentId: string | null = null
    for (const ancestor of ancestorURLs(link.url, MAX_TREE_DEPTH)) {
      const normalizedAncestor = normalizeURL(ancestor)
      let ancestorId = idByURL.get(normalizedAncestor) ?? null
      if (ancestorId === link.id) continue
      if (!ancestorId) {
        ancestorId = virtualID(normalizedAncestor)
        for (const key of urlLookupKeys(normalizedAncestor)) {
          idByURL.set(key, ancestorId)
        }
        nodesByID.set(
          ancestorId,
          toVirtualNode(normalizedAncestor, parentId, link),
        )
      }
      if (!parentByID.has(ancestorId)) {
        parentByID.set(ancestorId, parentId)
        const ancestorNode = nodesByID.get(ancestorId)
        if (ancestorNode) ancestorNode.parent_id = parentId
      }
      parentId = ancestorId
    }
    parentByID.set(link.id, parentId)
    const node = nodesByID.get(link.id)
    if (node) node.parent_id = parentId
  }

  const roots: TreeNode[] = []
  for (const [id, node] of nodesByID) {
    const parentId = parentByID.get(id) ?? null
    if (parentId) {
      const parent = nodesByID.get(parentId)
      if (parent) {
        parent.children.push(node)
        continue
      }
    }
    roots.push(node)
  }

  return {
    nodes: roots.sort(compareNodes).map((root) => attachChildren(root, 0)),
    total: sorted.length,
  }
}
