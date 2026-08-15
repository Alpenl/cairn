export const BLOCKED_IMAGE_FALLBACK = '图片已阻止'

/**
 * The accessible replacement for an untrusted image deliberately contains no
 * source URL. Keeping URL-shaped data out of the DOM prevents later code from
 * accidentally turning a placeholder back into a network request.
 */
export function blockedImageLabel(alt: string | null | undefined): string {
  return alt?.trim() || BLOCKED_IMAGE_FALLBACK
}
