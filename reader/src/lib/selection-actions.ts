/** 浮层动作类型。 */
export type PopoverAction = 'highlight' | 'note' | 'translate' | 'ai' | 'copy'

export const ARTICLE_SELECTION_ACTIONS: readonly PopoverAction[] = Object.freeze([
  'highlight', 'note', 'translate', 'ai', 'copy',
])

export const NOTE_SELECTION_ACTIONS: readonly PopoverAction[] = Object.freeze([
  'highlight', 'note', 'ai', 'copy',
])
