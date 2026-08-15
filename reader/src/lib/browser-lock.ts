const localQueues = new Map<string, Promise<void>>()

async function withLocalQueue<T>(name: string, operation: () => Promise<T>): Promise<T> {
  const previous = localQueues.get(name) ?? Promise.resolve()
  let release!: () => void
  const current = new Promise<void>((resolve) => {
    release = resolve
  })
  const queued = previous.then(() => current)
  localQueues.set(name, queued)
  await previous
  try {
    return await operation()
  } finally {
    release()
    if (localQueues.get(name) === queued) localQueues.delete(name)
  }
}

/** Cross-tab exclusive mutation where supported, with a deterministic test fallback. */
export async function withBrowserLock<T>(
  name: string,
  operation: () => Promise<T>,
): Promise<T> {
  const locks = typeof navigator === 'undefined' ? undefined : navigator.locks
  if (locks) return locks.request(name, { mode: 'exclusive' }, operation)
  return withLocalQueue(name, operation)
}
