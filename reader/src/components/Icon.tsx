/**
 * 共享图标库（stroke 风格，macOS 质感）。
 *
 * 图标以内联 SVG path 字符串存储，经 dangerouslySetInnerHTML 注入——内容是源码中
 * 写死的静态常量，不含用户输入，无 XSS 风险。
 */
import { memo } from 'react'
import type { CSSProperties } from 'react'

/** 全量图标 path 字典（与 components.jsx PATHS 逐项一致，不增删）。 */
export const PATHS = {
  sidebar: '<rect x="3" y="4.5" width="18" height="15" rx="2.5"/><path d="M9.5 4.5v15"/><path d="M15.8 9.6L13.4 12l2.4 2.4"/>',
  focus: '<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/>',
  focus_exit: '<path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5"/>',
  inbox: '<path d="M4 13l2.5-7A2 2 0 018.4 4.7h7.2A2 2 0 0117.5 6L20 13M4 13v5a2 2 0 002 2h12a2 2 0 002-2v-5M4 13h4l1.5 2.5h5L16 13h4"/>',
  folder: '<path d="M4 7a2 2 0 012-2h4l2 2.5h6a2 2 0 012 2V18a2 2 0 01-2 2H6a2 2 0 01-2-2V7z"/>',
  stack: '<path d="M12 3l8 4.5-8 4.5-8-4.5L12 3z"/><path d="M4 12l8 4.5 8-4.5M4 16.5L12 21l8-4.5"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  dot: '<circle cx="12" cy="12" r="4.5"/>',
  star: '<path d="M12 3.5l2.6 5.3 5.9.86-4.25 4.14 1 5.85L12 16.9l-5.25 2.76 1-5.85L3.5 9.66l5.9-.86L12 3.5z"/>',
  tag: '<path d="M3.5 11.5l8-8H20v8.5l-8 8a1.5 1.5 0 01-2.1 0l-6.4-6.4a1.5 1.5 0 010-2.1z"/><circle cx="15.5" cy="8.5" r="1.3"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  chevron: '<path d="M9 6l6 6-6 6"/>',
  sparkles: '<path d="M12 3l1.6 4.4L18 9l-4.4 1.6L12 15l-1.6-4.4L6 9l4.4-1.6L12 3z"/><path d="M19 14l.7 1.9L21.5 16.5l-1.8.7L19 19l-.7-1.8L16.5 16.5l1.8-.6L19 14z"/>',
  translate: '<path d="M4 5h10M9 3v2M11.5 5c0 4-3 7.5-7 9M6.5 9c1.3 2.3 3.3 3.8 5.5 4.8"/><path d="M13 21l4-9 4 9M14.4 18h5.2"/>',
  chat: '<path d="M21 11.5a8 8 0 01-11.5 7.2L4 20l1.3-4.6A8 8 0 1121 11.5z"/>',
  star_fill: '<path d="M12 3.5l2.6 5.3 5.9.86-4.25 4.14 1 5.85L12 16.9l-5.25 2.76 1-5.85L3.5 9.66l5.9-.86L12 3.5z" fill="currentColor" stroke="none"/>',
  moon: '<path d="M20 14.5A8 8 0 119.5 4a6.5 6.5 0 1010.5 10.5z"/>',
  rss: '<path d="M5 18.5a1 1 0 100-2 1 1 0 000 2z" fill="currentColor" stroke="none"/><path d="M5 11a8 8 0 018 8M5 5a14 14 0 0114 14"/>',
  x: '<path d="M4 4l16 16M20 4L4 20" stroke-width="2.2"/>',
  weibo: '<ellipse cx="10" cy="14" rx="6.5" ry="4.8"/><circle cx="18" cy="7" r="2.6"/>',
  youtube: '<rect x="3" y="6" width="18" height="12" rx="3.5"/><path d="M10.5 9.5l4 2.5-4 2.5z" fill="currentColor" stroke="none"/>',
  link: '<path d="M9 15l6-6M10.5 7.5l1.8-1.8a3.5 3.5 0 015 5L16.5 11M13.5 16.5l-1.8 1.8a3.5 3.5 0 01-5-5L8.5 11"/>',
  doc: '<path d="M7 3h7l5 5v11a2 2 0 01-2 2H7a2 2 0 01-2-2V5a2 2 0 012-2z"/><path d="M14 3v5h5M9 13h6M9 17h6"/>',
  edit: '<path d="M5 19h14M14.5 5.5l3 3M6 17l9.5-9.5 2.5 2.5L8.5 19.5 5 20l.5-3z"/>',
  more: '<circle cx="6" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="18" cy="12" r="1.4" fill="currentColor" stroke="none"/>',
  type: '<path d="M5 7V5h14v2M12 5v14M9 19h6"/>',
  sort: '<path d="M7 4v16M7 20l-3-3M7 4l3 3M17 20V4M17 4l-3 3M17 20l3-3"/>',
  send: '<path d="M4 12l16-7-7 16-2.5-6.5L4 12z"/>',
  close: '<path d="M6 6l12 12M18 6L6 18"/>',
  refresh: '<path d="M4 9a8 8 0 0114-3l2 2M20 15a8 8 0 01-14 3l-2-2M20 4v4h-4M4 20v-4h4"/>',
  pencil: '<path d="M14.5 5.5l4 4M4 20l1-4L16 5a2 2 0 013 3L8 19l-4 1z"/>',
  explain: '<circle cx="12" cy="12" r="9"/><path d="M9.5 9.5a2.5 2.5 0 014.5 1.5c0 1.5-2 2-2 3.5M12 17.5v.01"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 00-2-2H6a2 2 0 00-2 2v8a2 2 0 002 2h2"/>',
  command: '<path d="M9 6a3 3 0 10-3 3h12a3 3 0 10-3-3v12a3 3 0 103-3H6a3 3 0 10-3 3z" stroke-width="1.4"/>',
  globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 2.5 15.4 0 18M12 3c-2.5 2.6-2.5 15.4 0 18"/>',
  layers: '<path d="M12 4l8 4-8 4-8-4 8-4z"/><path d="M4 12l8 4 8-4M4 16l8 4 8-4"/>',
  arrowright: '<path d="M5 12h14M13 6l6 6-6 6"/>',
  external: '<path d="M14 5h5v5M19 5l-8 8"/><path d="M19 13.5V18a2 2 0 01-2 2H6a2 2 0 01-2-2V7a2 2 0 012-2h4.5"/>',
  alert: '<path d="M12 4L2.8 19.5h18.4L12 4z"/><path d="M12 10.5v4M12 17.2v.01"/>',
  check: '<circle cx="12" cy="12" r="9"/><path d="M8.5 12.5l2.5 2.5 4.5-5.5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  marker: '<path d="M15 4l5 5-9 9H6v-5l9-9z"/><path d="M5 21h14" stroke-width="2.4"/>',
  trash: '<path d="M4 7h16M9 7V5a1 1 0 011-1h4a1 1 0 011 1v2M6 7l1 12a2 2 0 002 2h6a2 2 0 002-2l1-12"/>',
  loader: '<path d="M12 3a9 9 0 109 9"/>',
  code: '<path d="M8 6l-6 6 6 6M16 6l6 6-6 6"/>',
  tree: '<rect x="3" y="4" width="7" height="5" rx="1.5"/><rect x="14" y="10" width="7" height="5" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/><path d="M10 6.5h2a2 2 0 012 2v2M10 18.5h2a2 2 0 002-2v-2"/>',
  pin: '<path d="M12 16.5V22M7.5 16.5h9L15 11V4.5h1V3H8v1.5h1V11l-1.5 5.5z"/>',
  hash: '<path d="M9.5 4L7.5 20M16.5 4l-2 16M4 9.5h16M4 14.5h16"/>',
  download: '<path d="M12 3v12M7 10l5 5 5-5"/><path d="M4 19v2h16v-2"/>',
  upload: '<path d="M12 17V5M7 10l5-5 5 5"/><path d="M4 19v2h16v-2"/>',
  bookmark: '<path d="M6 4.5A1.5 1.5 0 017.5 3h9A1.5 1.5 0 0118 4.5V21l-6-3.8L6 21V4.5z"/>',
} as const

/** 图标名联合类型（来自 PATHS 的键，避免裸字符串）。 */
export type IconName = keyof typeof PATHS

export interface IconProps {
  name: IconName
  size?: number
  /** 是否填充（保留以兼容设计签名；当前 PATHS 内已自带 fill，故仅占位）。 */
  fill?: boolean
  /** stroke 宽度。 */
  sw?: number
  style?: CSSProperties
}

/** 渲染单个图标。viewBox 固定 24×24，stroke 跟随 currentColor。 */
function IconInner({ name, size = 18, sw = 1.6, style }: IconProps) {
  const inner = PATHS[name] ?? ''
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={sw}
      strokeLinecap="round"
      strokeLinejoin="round"
      style={style}
      dangerouslySetInnerHTML={{ __html: inner }}
    />
  )
}

// React.memo：图标遍布列表/侧栏每行，props 多为基本类型。配合 LinkCard 等的 memo，
// 父级无关重渲染时不再逐个重新 set innerHTML。注意 style 对象若由父级内联新建仍会
// 破坏浅比较——调用方传字面量对象的少数场景不受益，但绝大多数无 style 的图标命中。
export const Icon = memo(IconInner)
