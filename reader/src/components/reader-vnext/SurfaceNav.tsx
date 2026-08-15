import type { ReaderRoute } from '../../lib/navigation/route'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { PrimaryNav, type LibraryView } from '../PrimaryNav'

export interface SurfaceNavProps {
  readonly active?: LibraryView
  readonly activeRoute?: ReaderRoute
  readonly onNavigate: (route: ReaderRoute) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
}

export function SurfaceNav({ active, activeRoute, onNavigate, capabilityPolicy }: SurfaceNavProps) {
  return (
    <aside className="rvx-nav" aria-label="Reader 导航">
      <PrimaryNav
        activeRoute={activeRoute}
        activeLibrary={active ?? null}
        onNavigate={onNavigate}
        policy={capabilityPolicy}
      />
    </aside>
  )
}
