export interface ReaderNavigationGuard {
  readonly blocksNavigation: () => boolean
  readonly requestNavigation: () => boolean | Promise<boolean>
}

/** Shared guard collection for the currently mounted Reader editors. */
export class ReaderNavigationGuardRegistry {
  private readonly guards = new Map<string, ReaderNavigationGuard>()

  register(owner: string, guard: ReaderNavigationGuard): () => void {
    this.guards.set(owner, guard)
    return () => {
      if (this.guards.get(owner) === guard) this.guards.delete(owner)
    }
  }

  hasBlocker(): boolean {
    for (const guard of this.guards.values()) {
      try {
        if (guard.blocksNavigation()) return true
      } catch {
        return true
      }
    }
    return false
  }

  requestNavigation(): boolean | Promise<boolean> {
    const guards = [...this.guards.values()]
    const requestFrom = (index: number): boolean | Promise<boolean> => {
      for (let current = index; current < guards.length; current += 1) {
        const guard = guards[current]
        try {
          if (!guard.blocksNavigation()) continue
          const result = guard.requestNavigation()
          if (typeof (result as Promise<boolean>)?.then === 'function') {
            return Promise.resolve(result).then((allowed) => allowed ? requestFrom(current + 1) : false).catch(() => false)
          }
          if (!result) return false
        } catch {
          return false
        }
      }
      return true
    }
    return requestFrom(0)
  }
}
