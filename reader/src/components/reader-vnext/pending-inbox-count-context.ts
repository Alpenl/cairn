import { createContext, useContext } from 'react'

export const PendingInboxCountContext = createContext<number | null>(null)

export function usePendingInboxCount(): number | null {
  return useContext(PendingInboxCountContext)
}
