// Reader 共享的对象守卫。
//
// 统一规则：非 null 的 object 且**不是数组**。Reader 内目前有 18 处本地
// isRecord / isObject / asRecord / recordValue，其中 3 处（FeedSurface、
// TodoSurface、feed-scroll-anchor）只判断 `typeof value === 'object' &&
// value !== null`，会让 JSON 里 malformed 的数组通过第一层检查，再靠后续
// 字段读取碰运气兜底。这里一律收紧为拒绝数组：数组永远不是这些调用方想要的
// “键值对象”，提前拒绝比在下游读到 undefined 更早暴露坏数据。

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function asRecord(value: unknown): Record<string, unknown> | null {
  return isRecord(value) ? value : null
}
