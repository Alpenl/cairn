import { negotiateSession } from '../src/lib/session-negotiation'
import { saveConnection, type Connection } from '../src/lib/settings'

interface ConnectionStorageHarness {
  save(connection: Connection): Promise<Connection>
  negotiateSuperseded(baseURL: string): Promise<string>
}

declare global {
  interface Window {
    connectionStorageHarness: ConnectionStorageHarness
  }
}

window.connectionStorageHarness = {
  save: saveConnection,
  async negotiateSuperseded(baseURL) {
    const outcome = await negotiateSession({
      baseURL,
      installationToken: 'legacy-secret',
      commit: async () => false,
    })
    return outcome.kind
  },
}

document.body.dataset.ready = 'true'
