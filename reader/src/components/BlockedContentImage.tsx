import { blockedImageLabel } from '../lib/untrusted-content-media'

export interface BlockedContentImageProps {
  readonly alt?: string
}

/** Untrusted Markdown never receives a network-capable image element. */
export function BlockedContentImage({ alt }: BlockedContentImageProps) {
  const label = blockedImageLabel(alt)
  return (
    <span
      className="reader-blocked-image"
      role="img"
      aria-label={label}
      data-blocked-content="image"
    >
      {label}
    </span>
  )
}
