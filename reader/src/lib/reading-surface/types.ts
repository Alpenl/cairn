/**
 * Contract shared by every Reader surface that presents readable content.
 *
 * The contract is deliberately data-only. React consumers decide how to render
 * a source and which hooks to mount; this module only gives them one vocabulary
 * for source identity, capabilities, and optional surface slots.
 */

export type ReadingSourceKind = 'markdown' | 'plain' | 'html'

export interface ReadingSourceIdentity {
  /** Stable identity of the content host, for example a link or feed item id. */
  readonly hostId: string
  /** Opaque version discriminator. The owner interprets its value. */
  readonly version: string
}

export interface ReadingSourceBase {
  readonly kind: ReadingSourceKind
  readonly blockKey: string
  readonly identity: ReadingSourceIdentity
}

export interface MarkdownReadingSource extends ReadingSourceBase {
  readonly kind: 'markdown'
  readonly text: string
}

export interface PlainReadingSource extends ReadingSourceBase {
  readonly kind: 'plain'
  readonly text: string
}

export interface HTMLReadingSource extends ReadingSourceBase {
  readonly kind: 'html'
  readonly html: string
  readonly baseURL: string
}

export type ReadingSource =
  | MarkdownReadingSource
  | PlainReadingSource
  | HTMLReadingSource

export type ReadingSurfaceCapability =
  | 'focus'
  | 'preferences'
  | 'progress'
  | 'toc'
  | 'back-to-top'
  | 'pager'
  | 'annotations'
  | 'translation'
  | 'ai'
  | 'editing'

export interface ReadingSurfaceSlots {
  readonly toolbar?: 'default' | 'minimal'
  readonly rail?: 'default' | 'toc-only' | 'none'
  readonly annotation?: 'enabled' | 'disabled'
}

export interface ReadingSurfaceContract {
  readonly source: ReadingSource
  readonly capabilities: ReadonlySet<ReadingSurfaceCapability>
  readonly slots: ReadingSurfaceSlots
}

export function hasReadingCapability(
  surface: ReadingSurfaceContract,
  capability: ReadingSurfaceCapability,
): boolean {
  return surface.capabilities.has(capability)
}

export function capabilitySet(
  capabilities: readonly ReadingSurfaceCapability[],
): ReadonlySet<ReadingSurfaceCapability> {
  return new Set(capabilities)
}
