import { describe, expect, it } from 'vitest'
import {
  ARTICLE_IMAGE_EXPAND_MIN_WIDTH,
  observeArticleImages,
  updateArticleImageSizing,
} from './useArticleImageSizing'

function setNaturalWidth(image: HTMLImageElement, width: number): void {
  Object.defineProperty(image, 'naturalWidth', {
    configurable: true,
    value: width,
  })
}

describe('article image sizing', () => {
  it('expands article-scale images while preserving small images', () => {
    const small = document.createElement('img')
    const large = document.createElement('img')
    setNaturalWidth(small, ARTICLE_IMAGE_EXPAND_MIN_WIDTH - 1)
    setNaturalWidth(large, ARTICLE_IMAGE_EXPAND_MIN_WIDTH)

    updateArticleImageSizing(small)
    updateArticleImageSizing(large)

    expect(small).not.toHaveClass('reader-image-expand')
    expect(large).toHaveClass('reader-image-expand')
  })

  it('classifies deferred images when they finish loading', () => {
    const root = document.createElement('div')
    const image = document.createElement('img')
    root.append(image)
    setNaturalWidth(image, 0)

    const cleanup = observeArticleImages(root)
    expect(image).not.toHaveClass('reader-image-expand')

    setNaturalWidth(image, 750)
    image.dispatchEvent(new Event('load'))
    expect(image).toHaveClass('reader-image-expand')

    image.dispatchEvent(new Event('error'))
    expect(image).not.toHaveClass('reader-image-expand')
    cleanup()
  })
})
