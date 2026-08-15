import { type RefObject, useEffect } from 'react'

export const ARTICLE_IMAGE_EXPAND_MIN_WIDTH = 640

export function updateArticleImageSizing(image: HTMLImageElement): void {
  image.classList.toggle(
    'reader-image-expand',
    image.naturalWidth >= ARTICLE_IMAGE_EXPAND_MIN_WIDTH,
  )
}

export function observeArticleImages(root: HTMLElement): () => void {
  const cleanups = Array.from(root.querySelectorAll('img')).map((image) => {
    const sync = () => updateArticleImageSizing(image)
    const clear = () => image.classList.remove('reader-image-expand')

    sync()
    image.addEventListener('load', sync)
    image.addEventListener('error', clear)

    return () => {
      image.removeEventListener('load', sync)
      image.removeEventListener('error', clear)
    }
  })

  return () => cleanups.forEach((cleanup) => cleanup())
}

export function useArticleImageSizing<T extends HTMLElement>(
  rootRef: RefObject<T>,
  contentKey: string,
): void {
  useEffect(() => {
    const root = rootRef.current
    if (!root) return
    return observeArticleImages(root)
  }, [contentKey, rootRef])
}
