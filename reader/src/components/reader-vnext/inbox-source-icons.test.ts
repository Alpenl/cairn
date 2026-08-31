import { describe, expect, it } from 'vitest'
import { inboxSourceIcon } from './inbox-source-icons'

describe('inboxSourceIcon', () => {
  it('映射已知 Inbox 来源图标', () => {
    expect(inboxSourceIcon('browser_capture')).toBe('link')
    expect(inboxSourceIcon('manual')).toBe('pencil')
  })

  it('未知 Inbox 来源回退 inbox 图标', () => {
    expect(inboxSourceIcon('custom_source')).toBe('inbox')
  })
})
