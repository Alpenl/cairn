// Reader 跨 Surface 的窗口事件。
//
// 这些事件名此前以裸字符串散落在 TODO、Home、Inbox、annotation hooks 和
// thought sync 里（各自还重复写 new Event + add/removeEventListener 配对）。
// 名字的字面值必须保持不变：扩展和旧的宿主页面也在监听同一批事件。
//
// 事件只做“某处数据变了，请自己重读”的通知，不携带 payload；emit 与
// subscribe 共用同一个 ReaderEventName 与 ReaderEventListener 类型，
// 保证两端看到的事件对象形状一致。
export const READER_EVENTS = {
  todosChanged: 'webtag:todos-change',
  homeChanged: 'webtag:home-change',
  pendingInboxChanged: 'webtag:pending-inbox-changed',
  annotationsChanged: 'webtag:annotations-change',
  thoughtsSynced: 'webtag:thoughts-sync',
  thoughtsSyncRequested: 'webtag:thoughts-sync-request',
} as const

export type ReaderEventName = (typeof READER_EVENTS)[keyof typeof READER_EVENTS]

/** 订阅端拿到的就是 emit 端派发的那个无 detail 的 Event。 */
export type ReaderEventListener = (event: Event) => void

export function emitReaderEvent(name: ReaderEventName): void {
  if (typeof window === 'undefined') return
  window.dispatchEvent(new Event(name))
}

/**
 * 订阅一组 Reader 事件，返回一次性的取消订阅函数。
 * 重复调用取消函数是安全的：只在第一次真正解绑。
 */
export function subscribeReaderEvents(
  names: readonly ReaderEventName[],
  listener: ReaderEventListener,
): () => void {
  if (typeof window === 'undefined') return () => {}
  const target = window
  const subscribed = [...new Set(names)]
  for (const name of subscribed) target.addEventListener(name, listener)
  let released = false
  return () => {
    if (released) return
    released = true
    for (const name of subscribed) target.removeEventListener(name, listener)
  }
}
