export interface SlashMenuPosition {
  readonly left: number
  readonly top: number
  readonly width: number
  readonly maxHeight: number
  readonly placement: 'above' | 'below'
}

const SLASH_MENU_WIDTH = 264
const SLASH_MENU_ESTIMATED_HEIGHT = 306
const VIEWPORT_PADDING = 8
const CARET_GAP = 6

export function positionSlashMenu(
  caretRect: Pick<DOMRect, 'left' | 'top' | 'bottom'>,
  viewportWidth: number,
  viewportHeight: number,
): SlashMenuPosition {
  const width = Math.min(SLASH_MENU_WIDTH, Math.max(0, viewportWidth - VIEWPORT_PADDING * 2))
  const left = Math.max(
    VIEWPORT_PADDING,
    Math.min(caretRect.left, viewportWidth - width - VIEWPORT_PADDING),
  )
  const below = Math.max(0, viewportHeight - caretRect.bottom - VIEWPORT_PADDING - CARET_GAP)
  const above = Math.max(0, caretRect.top - VIEWPORT_PADDING - CARET_GAP)
  const placement = below >= Math.min(132, SLASH_MENU_ESTIMATED_HEIGHT) || below >= above
    ? 'below'
    : 'above'
  const available = placement === 'below' ? below : above
  const maxHeight = Math.max(48, Math.min(SLASH_MENU_ESTIMATED_HEIGHT, available))
  const top = placement === 'below'
    ? caretRect.bottom + CARET_GAP
    : Math.max(VIEWPORT_PADDING, caretRect.top - maxHeight - CARET_GAP)
  return { left, top, width, maxHeight, placement }
}
