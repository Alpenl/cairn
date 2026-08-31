import type { IconName } from '../Icon'

const INBOX_SOURCE_ICONS: Partial<Record<string, IconName>> = {
  browser_capture: 'link',
  extension: 'link',
  rss: 'rss',
  subscription: 'rss',
  manual: 'pencil',
}

/** Inbox source_kind -> UI icon name; unknown values fall back to inbox. */
export function inboxSourceIcon(kind: string): IconName {
  return INBOX_SOURCE_ICONS[kind] ?? 'inbox'
}
