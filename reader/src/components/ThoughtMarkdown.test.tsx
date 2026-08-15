import { fireEvent, render, screen } from '@testing-library/react'
import { ThoughtMarkdown } from './ThoughtMarkdown'

describe('ThoughtMarkdown', () => {
  it('renders GFM and exposes stable task callbacks without an annotation host', () => {
    const onToggleTask = vi.fn()
    const { container } = render(
      <ThoughtMarkdown
        source={'**important**\n\n- [ ] ship it'}
        onToggleTask={onToggleTask}
      />,
    )
    expect(screen.getByText('important')).toHaveStyle({ fontWeight: 'bold' })
    const checkbox = screen.getByRole('checkbox')
    fireEvent.click(checkbox)
    expect(onToggleTask).toHaveBeenCalledWith(
      expect.objectContaining({ text: 'ship it', done: false }),
      true,
    )
    expect(container.querySelector('[data-hl-block]')).not.toBeInTheDocument()
  })

  it('does not treat task-looking text inside a fenced code block as a task', () => {
    const onToggleTask = vi.fn()
    render(
      <ThoughtMarkdown
        source={'```md\n- [ ] example\n```\n\n- [ ] real task'}
        onToggleTask={onToggleTask}
      />,
    )

    const checkbox = screen.getByRole('checkbox')
    expect(checkbox).toHaveAttribute('data-block-ref')
    fireEvent.click(checkbox)
    expect(onToggleTask).toHaveBeenCalledWith(expect.objectContaining({ text: 'real task' }), true)
  })

  it('binds repeated tasks by stable blockRef and occurrence and reports the desired state', () => {
    const onToggleTask = vi.fn()
    render(
      <ThoughtMarkdown
        source={'## Plan\n\n- [ ] same\n- [x] same'}
        onToggleTask={onToggleTask}
      />,
    )

    const checkboxes = screen.getAllByRole('checkbox')
    expect(checkboxes[0]).toHaveAttribute('data-occurrence', '1')
    expect(checkboxes[1]).toHaveAttribute('data-occurrence', '2')
    expect(checkboxes[0].dataset.blockRef).toBe(checkboxes[1].dataset.blockRef)

    fireEvent.click(checkboxes[0])
    fireEvent.click(checkboxes[1])
    expect(onToggleTask).toHaveBeenNthCalledWith(
      1,
      expect.objectContaining({ blockRef: checkboxes[0].dataset.blockRef, occurrence: 1, done: false }),
      true,
    )
    expect(onToggleTask).toHaveBeenNthCalledWith(
      2,
      expect.objectContaining({ blockRef: checkboxes[1].dataset.blockRef, occurrence: 2, done: true }),
      false,
    )
  })

  it('does not create a network-capable element for Markdown images', () => {
    const { container } = render(
      <ThoughtMarkdown source="![Private diagram](https://127.0.0.1/pixel.png)" />,
    )
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByRole('img', { name: 'Private diagram' })).toHaveTextContent('Private diagram')
    expect(container.innerHTML).not.toContain('127.0.0.1')
  })
})
