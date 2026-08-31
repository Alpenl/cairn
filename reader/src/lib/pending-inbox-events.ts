import { emitReaderEvent, READER_EVENTS } from './reader-events'

export function refreshPendingInboxCount(): void {
  emitReaderEvent(READER_EVENTS.pendingInboxChanged)
}
