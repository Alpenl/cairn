import { caseFold } from 'unicode-case-folding'

// Unicode default case folding is for comparison keys only. Callers retain the
// original string for display so the first submitted spelling remains visible.
export function foldUnicodeCase(value: string): string {
  return caseFold(value)
}
