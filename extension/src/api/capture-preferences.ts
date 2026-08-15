/**
 * Extension-only capture preferences.
 *
 * This preference is deliberately stored separately from the general WebTag
 * settings object: background capture and the options page need the same
 * value, while older settings snapshots must remain readable unchanged.
 */

export type CaptureDestination = 'inbox' | 'library'
export type CaptureRequestDestination = CaptureDestination | 'site'

export const DEFAULT_CAPTURE_DESTINATION: CaptureDestination = 'inbox'
export const CAPTURE_DESTINATION_STORAGE_KEY = 'webtag_capture_destination'

export function normalizeCaptureDestination(
  value: unknown,
): CaptureDestination {
  return value === 'library' || value === 'inbox'
    ? value
    : DEFAULT_CAPTURE_DESTINATION
}

export async function getCaptureDestination(): Promise<CaptureDestination> {
  try {
    const stored = await chrome.storage.local.get(
      CAPTURE_DESTINATION_STORAGE_KEY,
    )
    return normalizeCaptureDestination(stored[CAPTURE_DESTINATION_STORAGE_KEY])
  } catch {
    return DEFAULT_CAPTURE_DESTINATION
  }
}

export async function setCaptureDestination(
  destination: CaptureDestination,
): Promise<void> {
  await chrome.storage.local.set({
    [CAPTURE_DESTINATION_STORAGE_KEY]: normalizeCaptureDestination(destination),
  })
}
