/** Legacy shell adapter for the shared five-destination primary navigation. */
import { PrimaryNav, type LibraryView } from './PrimaryNav'
import type { ReaderRoute } from '../lib/navigation/route'
import type { ReaderCapabilityPolicy } from '../lib/capabilities'

export type { LibraryView } from './PrimaryNav'

export interface LibraryModeNavProps {
  view: LibraryView
  onView: (view: LibraryView) => void
  onNavigate?: (route: ReaderRoute) => void
  activeRoute?: ReaderRoute
  policy?: ReaderCapabilityPolicy
}

export function LibraryModeNav({ view, onView, onNavigate, activeRoute, policy }: LibraryModeNavProps) {
  const navigate = (route: ReaderRoute) => {
    if (onNavigate) {
      onNavigate(route)
      return
    }
    if (route.kind === 'library') {
      onView(route.id)
      return
    }

    const legacyNavigate = onView as unknown as (next: ReaderRoute) => void
    legacyNavigate(route)
  }

  return <PrimaryNav activeRoute={activeRoute} activeLibrary={view} onNavigate={navigate} policy={policy} />
}
