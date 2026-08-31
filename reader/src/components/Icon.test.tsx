import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Icon } from './Icon'

describe('Icon', () => {
  it('渲染 search 图标为 svg', () => {
    const { container } = render(<Icon name="search" />)
    const svg = container.querySelector('svg')
    expect(svg).toBeInTheDocument()
    expect(svg?.innerHTML).toContain('circle')
  })
})
