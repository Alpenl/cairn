/**
 * hast 节点的结构化子集 —— rehype 插件（划线注入、标题编号）共用的最小节点形状。
 * 只声明插件真正读写的字段，避免为一个类型引入 @types/hast 依赖。
 */
export interface HastNode {
  type: string
  tagName?: string
  value?: string
  properties?: Record<string, unknown>
  children?: HastNode[]
}
