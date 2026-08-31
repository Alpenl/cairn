import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ListEmptyState, ListErrorState, ListStateView } from './ListStateView'

function emptyState(title = '没有内容') {
  return <ListEmptyState icon="layers" title={title} description="内容会出现在这里。" />
}

describe('ListStateView', () => {
  it('chooses loading before empty content while exposing a stable status name', () => {
    render(
      <ListStateView loading error={null} empty emptyState={emptyState()} onRetry={vi.fn()}>
        <ul><li>loaded</li></ul>
      </ListStateView>,
    )

    expect(screen.getByRole('status', { name: '加载中' })).toBeInTheDocument()
    expect(screen.queryByText('loaded')).not.toBeInTheDocument()
  })

  it('chooses retryable error before empty content', () => {
    const onRetry = vi.fn()
    render(
      <ListStateView
        loading={false}
        error="请求失败"
        empty
        emptyState={emptyState()}
        emptyErrorState={<ListErrorState title="无法读取列表" message="请求失败" onRetry={onRetry} />}
        onRetry={onRetry}
      >
        <ul><li>loaded</li></ul>
      </ListStateView>,
    )

    expect(screen.getByRole('alert', { name: '无法读取列表' })).toHaveTextContent('请求失败')
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })

  it('renders empty and loaded refresh-error states without class-based assertions', () => {
    const { rerender } = render(
      <ListStateView loading={false} error={null} empty emptyState={emptyState('列表为空')}>
        <ul><li>loaded</li></ul>
      </ListStateView>,
    )
    expect(screen.getByRole('status', { name: '列表为空' })).toHaveTextContent('内容会出现在这里。')

    rerender(
      <ListStateView loading={false} error="刷新失败" empty={false} emptyState={emptyState('列表为空')}>
        <ul aria-label="结果列表"><li>loaded</li></ul>
      </ListStateView>,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('刷新失败')
    expect(screen.getByRole('list', { name: '结果列表' })).toHaveTextContent('loaded')
  })
})
