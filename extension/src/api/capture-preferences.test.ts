import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  CAPTURE_DESTINATION_STORAGE_KEY,
  DEFAULT_CAPTURE_DESTINATION,
  getCaptureDestination,
  normalizeCaptureDestination,
  setCaptureDestination,
} from './capture-preferences'

beforeEach(async () => {
  await chrome.storage.local.clear()
})

afterEach(async () => {
  vi.restoreAllMocks()
  await chrome.storage.local.clear()
})

describe('capture destination preference', () => {
  it('缺少设置时默认为 Inbox', async () => {
    await expect(getCaptureDestination()).resolves.toBe(
      DEFAULT_CAPTURE_DESTINATION,
    )
  })

  it('非法值归一化为 Inbox', async () => {
    expect(normalizeCaptureDestination('reading')).toBe('inbox')
    expect(normalizeCaptureDestination(undefined)).toBe('inbox')
    expect(normalizeCaptureDestination(null)).toBe('inbox')

    await chrome.storage.local.set({
      [CAPTURE_DESTINATION_STORAGE_KEY]: 'unknown',
    })
    await expect(getCaptureDestination()).resolves.toBe('inbox')
  })

  it('library 可以保存并读取', async () => {
    await setCaptureDestination('library')

    expect(
      await chrome.storage.local.get(CAPTURE_DESTINATION_STORAGE_KEY),
    ).toEqual({ [CAPTURE_DESTINATION_STORAGE_KEY]: 'library' })
    await expect(getCaptureDestination()).resolves.toBe('library')
  })

  it('读取 storage 失败时回退到 Inbox', async () => {
    vi.spyOn(chrome.storage.local, 'get').mockRejectedValueOnce(
      new Error('storage unavailable'),
    )

    await expect(getCaptureDestination()).resolves.toBe('inbox')
  })

  it('写入 storage 失败时保留错误给调用方', async () => {
    vi.spyOn(chrome.storage.local, 'set').mockRejectedValueOnce(
      new Error('quota exceeded'),
    )

    await expect(setCaptureDestination('library')).rejects.toThrow(
      'quota exceeded',
    )
  })
})
