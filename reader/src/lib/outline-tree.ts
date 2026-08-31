import { MIN_TOC_HEADINGS } from '../hooks/useReaderToc'
import type { TocHeading } from './toc'

export function hasRenderableOutline(items: readonly TocHeading[]): boolean {
  return items.length >= MIN_TOC_HEADINGS
}
