import type { ReactNode } from 'react'

import { Icon, type IconName } from '../Icon'
import { SurfaceError, SurfaceLoading } from './SurfaceShell'

export interface ListEmptyStateProps {
  readonly icon: IconName
  readonly title: string
  readonly description: ReactNode
}

export interface ListErrorStateProps {
  readonly title: string
  readonly message: string
  readonly onRetry?: () => void
}

export interface ListStateViewProps {
  readonly loading: boolean
  readonly error: string | null | undefined
  readonly empty: boolean
  readonly emptyState: ReactNode
  readonly emptyErrorState?: ReactNode
  readonly onRetry?: () => void
  readonly children: ReactNode
}

export function ListEmptyState({ icon, title, description }: ListEmptyStateProps) {
  return (
    <div className="rvx-empty" role="status" aria-label={title}>
      <Icon name={icon} size={24} />
      <h2>{title}</h2>
      <p>{description}</p>
    </div>
  )
}

export function ListErrorState({ title, message, onRetry }: ListErrorStateProps) {
  return (
    <div className="rvx-empty" role="alert" aria-label={title}>
      <Icon name="alert" size={24} />
      <h2>{title}</h2>
      <p>{message}</p>
      {onRetry && (
        <button className="rvx-button secondary" type="button" onClick={onRetry}>
          重试
        </button>
      )}
    </div>
  )
}

export function ListStateView({
  loading,
  error,
  empty,
  emptyState,
  emptyErrorState,
  onRetry,
  children,
}: ListStateViewProps) {
  if (error && empty) return emptyErrorState ?? <SurfaceError message={error} onRetry={onRetry} />
  if (loading && empty) return <SurfaceLoading />
  if (empty) return <>{emptyState}</>
  return (
    <>
      {error && <SurfaceError message={error} onRetry={onRetry} />}
      {children}
    </>
  )
}
