import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  emitReaderEvent,
  READER_EVENTS,
  subscribeReaderEvents,
  type ReaderEventName,
} from './reader-events'

const ALL_EVENTS = Object.values(READER_EVENTS) as readonly ReaderEventName[]

const cleanups: (() => void)[] = []

function track(unsubscribe: () => void): () => void {
  cleanups.push(unsubscribe)
  return unsubscribe
}

afterEach(() => {
  while (cleanups.length > 0) cleanups.pop()?.()
})

describe('READER_EVENTS', () => {
  // 扩展与旧宿主页面监听同一批名字，字面值不能改。
  it('keeps every event name literal', () => {
    expect(READER_EVENTS).toEqual({
      todosChanged: 'webtag:todos-change',
      homeChanged: 'webtag:home-change',
      pendingInboxChanged: 'webtag:pending-inbox-changed',
      annotationsChanged: 'webtag:annotations-change',
      thoughtsSynced: 'webtag:thoughts-sync',
      thoughtsSyncRequested: 'webtag:thoughts-sync-request',
    })
  })

  it('has no duplicate names', () => {
    expect(new Set(ALL_EVENTS).size).toBe(ALL_EVENTS.length)
  })
})

describe('emitReaderEvent', () => {
  it('dispatches a plain window event for every name', () => {
    for (const name of ALL_EVENTS) {
      const listener = vi.fn()
      window.addEventListener(name, listener)
      try {
        emitReaderEvent(name)
        expect(listener).toHaveBeenCalledTimes(1)
        const event = listener.mock.calls[0][0] as Event
        expect(event.type).toBe(name)
        expect(event).toBeInstanceOf(Event)
      } finally {
        window.removeEventListener(name, listener)
      }
    }
  })

  it('does not leak into other reader events', () => {
    const listener = vi.fn()
    track(subscribeReaderEvents([READER_EVENTS.homeChanged], listener))

    emitReaderEvent(READER_EVENTS.todosChanged)

    expect(listener).not.toHaveBeenCalled()
  })
})

describe('subscribeReaderEvents', () => {
  it('receives the emitted event on each subscribed name', () => {
    const seen: string[] = []
    track(subscribeReaderEvents(
      [READER_EVENTS.todosChanged, READER_EVENTS.homeChanged],
      (event) => { seen.push(event.type) },
    ))

    emitReaderEvent(READER_EVENTS.todosChanged)
    emitReaderEvent(READER_EVENTS.homeChanged)
    emitReaderEvent(READER_EVENTS.annotationsChanged)

    expect(seen).toEqual(['webtag:todos-change', 'webtag:home-change'])
  })

  it('stops delivering after unsubscribe', () => {
    const listener = vi.fn()
    const unsubscribe = subscribeReaderEvents(
      [READER_EVENTS.thoughtsSynced, READER_EVENTS.thoughtsSyncRequested],
      listener,
    )

    emitReaderEvent(READER_EVENTS.thoughtsSynced)
    unsubscribe()
    emitReaderEvent(READER_EVENTS.thoughtsSynced)
    emitReaderEvent(READER_EVENTS.thoughtsSyncRequested)

    expect(listener).toHaveBeenCalledTimes(1)
  })

  it('tolerates a repeated unsubscribe', () => {
    const listener = vi.fn()
    const unsubscribe = subscribeReaderEvents([READER_EVENTS.pendingInboxChanged], listener)

    unsubscribe()
    unsubscribe()
    emitReaderEvent(READER_EVENTS.pendingInboxChanged)

    expect(listener).not.toHaveBeenCalled()
  })

  it('delivers a duplicated name only once', () => {
    const listener = vi.fn()
    track(subscribeReaderEvents(
      [READER_EVENTS.annotationsChanged, READER_EVENTS.annotationsChanged],
      listener,
    ))

    emitReaderEvent(READER_EVENTS.annotationsChanged)

    expect(listener).toHaveBeenCalledTimes(1)
  })

  it('keeps independent subscribers isolated', () => {
    const first = vi.fn()
    const second = vi.fn()
    const stopFirst = subscribeReaderEvents([READER_EVENTS.todosChanged], first)
    track(subscribeReaderEvents([READER_EVENTS.todosChanged], second))

    stopFirst()
    emitReaderEvent(READER_EVENTS.todosChanged)

    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('subscribing to an empty list is a no-op that still returns a cleanup', () => {
    const listener = vi.fn()
    const unsubscribe = subscribeReaderEvents([], listener)

    emitReaderEvent(READER_EVENTS.homeChanged)
    unsubscribe()

    expect(listener).not.toHaveBeenCalled()
  })
})
