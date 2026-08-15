import http from 'node:http'
import { fileURLToPath } from 'node:url'
import { createServer as createViteServer } from 'vite'

const port = Number(process.env.READER_E2E_PORT ?? 4179)
const imageProbePort = Number(process.env.READER_E2E_IMAGE_PROBE_PORT ?? port + 1)
const root = fileURLToPath(new URL('..', import.meta.url))
const annotationBarrierTimeoutMs = 10_000
const minimumAnnotationBarrierTimeoutMs = 25
const readerSecurityCSP = [
  "default-src 'self'",
  "script-src 'self' 'nonce-reader-security-harness'",
  "style-src 'self' 'unsafe-inline'",
  "font-src 'self'",
  "img-src 'self' data: blob:",
  "media-src 'none'",
  "object-src 'none'",
  "connect-src 'self' ws:",
  "worker-src 'self'",
  "base-uri 'self'",
  "form-action 'none'",
].join('; ')
const namespaces = {
  A: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
  B: 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB',
}

function createGate() {
  let release
  const promise = new Promise((resolve) => {
    release = resolve
  })
  return { promise, release, waiters: 0 }
}

const state = {
  titles: { A: 'A private v1', B: 'B private v1' },
  identityGates: new Map(),
  linkGates: new Map(),
  annotationBarriers: new Map(),
  linksAfterOne: new Set(),
  legacySessionUpgrade: null,
  imageProbeRequests: [],
}

function identityFrom(value) {
  return value === 'B' ? 'B' : 'A'
}

function sessionCookieValue(request) {
  const cookies = String(request.headers.cookie ?? '')
    .split(';')
    .map((part) => part.trim())
  const session = cookies.find((part) => part.startsWith('webtag_session='))
  return session?.slice('webtag_session='.length)
}

function cookieIdentity(request) {
  return identityFrom(sessionCookieValue(request))
}

async function readJSONBody(request) {
  const chunks = []
  for await (const chunk of request) chunks.push(chunk)
  return JSON.parse(Buffer.concat(chunks).toString('utf8'))
}

function block(gates, identity) {
  gates.get(identity)?.release()
  gates.set(identity, createGate())
}

function release(gates, identity) {
  const gate = gates.get(identity)
  if (!gate) return
  gates.delete(identity)
  gate.release()
}

async function waitForGate(gates, identity) {
  const gate = gates.get(identity)
  if (!gate) return
  gate.waiters += 1
  try {
    await gate.promise
  } finally {
    gate.waiters -= 1
  }
}

function releaseAll(gates) {
  for (const identity of [...gates.keys()]) release(gates, identity)
}

class AnnotationBarrierError extends Error {
  constructor(status, message) {
    super(message)
    this.name = 'AnnotationBarrierError'
    this.status = status
  }
}

function removeAnnotationBarrier(barrier) {
  if (state.annotationBarriers.get(barrier.name) === barrier) {
    state.annotationBarriers.delete(barrier.name)
  }
  if (barrier.timer !== null) {
    clearTimeout(barrier.timer)
    barrier.timer = null
  }
}

function releaseAnnotationBarrier(barrier) {
  if (barrier.settled) return
  barrier.settled = true
  removeAnnotationBarrier(barrier)
  barrier.resolve()
}

function failAnnotationBarrier(barrier, error) {
  if (barrier.settled) return
  barrier.settled = true
  removeAnnotationBarrier(barrier)
  barrier.reject(error)
}

function createAnnotationBarrier(name, participants, timeoutMs) {
  let resolve
  let reject
  const promise = new Promise((promiseResolve, promiseReject) => {
    resolve = promiseResolve
    reject = promiseReject
  })
  const barrier = {
    name,
    participants,
    timeoutMs,
    parties: new Set(),
    promise,
    resolve,
    reject,
    settled: false,
    timer: null,
  }
  barrier.timer = setTimeout(() => {
    failAnnotationBarrier(
      barrier,
      new AnnotationBarrierError(504, 'annotation barrier timed out'),
    )
  }, timeoutMs)
  return barrier
}

function failAllAnnotationBarriers(message) {
  for (const barrier of [...state.annotationBarriers.values()]) {
    failAnnotationBarrier(barrier, new AnnotationBarrierError(503, message))
  }
}

async function waitForAnnotationBarrier(name, participants, party, timeoutMs) {
  let barrier = state.annotationBarriers.get(name)
  if (!barrier) {
    barrier = createAnnotationBarrier(name, participants, timeoutMs)
    state.annotationBarriers.set(name, barrier)
  }
  if (barrier.participants !== participants) {
    throw new AnnotationBarrierError(
      409,
      'annotation barrier participant count changed',
    )
  }
  if (barrier.timeoutMs !== timeoutMs) {
    throw new AnnotationBarrierError(409, 'annotation barrier timeout changed')
  }
  if (barrier.parties.has(party)) {
    throw new AnnotationBarrierError(409, 'annotation barrier party already arrived')
  }
  barrier.parties.add(party)
  const arrival = barrier.parties.size
  if (arrival === participants) {
    releaseAnnotationBarrier(barrier)
  }
  await barrier.promise
  return arrival
}

function resetState() {
  releaseAll(state.identityGates)
  releaseAll(state.linkGates)
  state.legacySessionUpgrade?.cookieVerificationGate?.release()
  failAllAnnotationBarriers('annotation barrier reset')
  state.linksAfterOne.clear()
  state.titles.A = 'A private v1'
  state.titles.B = 'B private v1'
  state.legacySessionUpgrade = null
  state.imageProbeRequests.length = 0
}

function sendJSON(response, status, body, headers = {}) {
  if (response.destroyed || response.writableEnded) return
  response.writeHead(status, {
    'Content-Type': 'application/json',
    'Cache-Control': 'no-store',
    ...headers,
  })
  response.end(JSON.stringify(body))
}

function sendText(response, status, body, headers = {}) {
  if (response.destroyed || response.writableEnded) return
  response.writeHead(status, headers)
  response.end(body)
}

function linkFor(identity) {
  return {
    id: `${identity}-link`,
    url: `https://${identity.toLowerCase()}.example.test/article`,
    title: state.titles[identity],
    summary: `${identity} private summary`,
    description: null,
    tags: [identity],
    content_type: 'article',
    status: 'done',
    domain: `${identity.toLowerCase()}.example.test`,
    path_depth: 1,
    parent_id: null,
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
    metadata_revision: 1,
    fetcher_type: 'http',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    parent_path: null,
    has_content: false,
  }
}

function siteFor(index) {
  const label = String(index).padStart(3, '0')
  return {
    id: `site-${label}`,
    name: `Pagination site ${label}`,
    intro: 'A site fixture for Reader pagination coverage.',
    display_host: `site-${label}.example.test`,
    tags: [],
    entry_count: 1,
    pinned: false,
    needs_review: false,
    revision: index,
    first_collected_at: '2026-08-01T00:00:00Z',
    last_collected_at: '2026-08-01T00:00:00Z',
  }
}

function gateWaiters(gates) {
  return Object.fromEntries([...gates].map(([identity, gate]) => [identity, gate.waiters]))
}

async function handleTestRoute(request, response, url) {
  if (url.pathname === '/__test__/health') {
    sendJSON(response, 200, { ok: true })
    return true
  }
  if (url.pathname === '/__test__/image-probe-reset' && request.method === 'POST') {
    state.imageProbeRequests.length = 0
    sendJSON(response, 200, { ok: true })
    return true
  }
  if (url.pathname === '/__test__/image-probe-count') {
    sendJSON(response, 200, { count: state.imageProbeRequests.length })
    return true
  }
  if (url.pathname === '/__test__/blank') {
    sendText(response, 200, '<!doctype html><html><body>browser test</body></html>', {
      'Content-Type': 'text/html; charset=utf-8',
    })
    return true
  }
  if (url.pathname === '/__test__/reader-security-harness') {
    const html = `<!doctype html><html><body><div id="root"></div>
      <script nonce="reader-security-harness" type="module">
        import RefreshRuntime from '/@react-refresh'
        RefreshRuntime.injectIntoGlobalHook(window)
        window.$RefreshReg$ = () => undefined
        window.$RefreshSig$ = () => (type) => type
        window.__vite_plugin_react_preamble_installed__ = true
      </script>
      <script type="module" src="/e2e/reader-security-harness.tsx"></script>
    </body></html>`
    sendText(
      response,
      200,
      html,
      {
        'Content-Type': 'text/html; charset=utf-8',
        'Content-Security-Policy': readerSecurityCSP,
      },
    )
    return true
  }
  if (url.pathname === '/__test__/connection-storage-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module" src="/e2e/connection-storage-harness.ts"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }
  if (url.pathname === '/__test__/reader-sw.js' || url.pathname === '/__test__/other-sw.js') {
    sendText(response, 200, 'self.addEventListener("fetch", () => undefined)\n', {
      'Content-Type': 'text/javascript; charset=utf-8',
      'Service-Worker-Allowed': '/',
      'Cache-Control': 'no-store',
    })
    return true
  }
  if (url.pathname === '/__test__/storage-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module" src="/reader/e2e/storage-harness.ts"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }
  if (url.pathname === '/__test__/rf5b-annotations-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module" src="/reader/e2e/rf5b-annotations-harness.ts"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }

  if (url.pathname === '/__test__/thought-repair-harness') {
    response.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' })
    response.end(
      '<!doctype html><html><body><script type="module" src="/reader/e2e/thought-repair-harness.ts"></script></body></html>',
    )
    return
  }
  if (url.pathname === '/__test__/thought-sync-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module" src="/reader/e2e/thought-sync-harness.ts"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }
  if (url.pathname === '/__test__/navigation-guard-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module" src="/reader/e2e/navigation-guard-harness.ts"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }
  if (url.pathname === '/__test__/issue83-notes-harness') {
    sendText(
      response,
      200,
      '<!doctype html><html><body><script type="module">import RefreshRuntime from "/reader/@react-refresh";RefreshRuntime.injectIntoGlobalHook(window);window.$RefreshReg$=()=>{};window.$RefreshSig$=()=>type=>type;window.__vite_plugin_react_preamble_installed__=true;</script><script type="module" src="/reader/@vite/client"></script><script type="module" src="/reader/e2e/issue83-notes-harness.tsx"></script></body></html>',
      { 'Content-Type': 'text/html; charset=utf-8' },
    )
    return true
  }
  if (url.pathname === '/__test__/annotation-barrier') {
    const name = url.searchParams.get('name') ?? ''
    const party = url.searchParams.get('party') ?? ''
    const participants = Number(url.searchParams.get('participants'))
    const timeoutParam = url.searchParams.get('timeoutMs')
    const timeoutMs = timeoutParam === null
      ? annotationBarrierTimeoutMs
      : Number(timeoutParam)
    if (
      name.length === 0 ||
      name.length > 160 ||
      party.length === 0 ||
      party.length > 160 ||
      party.trim() !== party ||
      !Number.isSafeInteger(participants) ||
      participants < 2 ||
      participants > 16 ||
      !Number.isSafeInteger(timeoutMs) ||
      timeoutMs < minimumAnnotationBarrierTimeoutMs ||
      timeoutMs > annotationBarrierTimeoutMs
    ) {
      sendJSON(response, 400, { error: 'invalid annotation barrier' })
      return true
    }
    try {
      const arrival = await waitForAnnotationBarrier(
        name,
        participants,
        party,
        timeoutMs,
      )
      sendJSON(response, 200, { name, participants, party, arrival })
    } catch (error) {
      if (!(error instanceof AnnotationBarrierError)) throw error
      sendJSON(response, error.status, { error: error.message })
    }
    return true
  }
  if (url.pathname === '/__test__/reset') {
    resetState()
    sendJSON(response, 200, { ok: true }, {
      'Set-Cookie': 'webtag_session=A; Path=/; HttpOnly; SameSite=Lax',
    })
    return true
  }
  if (url.pathname === '/__test__/session') {
    const identity = identityFrom(url.searchParams.get('identity'))
    sendJSON(response, 200, { identity }, {
      'Set-Cookie': `webtag_session=${identity}; Path=/; HttpOnly; SameSite=Lax`,
    })
    return true
  }
  if (url.pathname === '/__test__/legacy-session-upgrade') {
    state.legacySessionUpgrade = {
      token: 'legacy-secret',
      bearerIdentity: identityFrom(url.searchParams.get('bearerIdentity')),
      sessionIdentity: identityFrom(url.searchParams.get('sessionIdentity')),
      bearerGets: 0,
      sessionGets: 0,
      exchanges: 0,
      deletes: 0,
      events: [],
      cookieVerificationGate:
        url.searchParams.get('blockCookieIdentity') === '1' ? createGate() : null,
      cookieVerificationClaimed: false,
    }
    sendJSON(response, 200, { enabled: true })
    return true
  }
  if (url.pathname === '/__test__/legacy-session-upgrade-state') {
    const upgrade = state.legacySessionUpgrade
    sendJSON(response, 200, upgrade === null ? { enabled: false } : {
      enabled: true,
      bearerIdentity: upgrade.bearerIdentity,
      sessionIdentity: upgrade.sessionIdentity,
      bearerGets: upgrade.bearerGets,
      sessionGets: upgrade.sessionGets,
      exchanges: upgrade.exchanges,
      deletes: upgrade.deletes,
      events: upgrade.events,
      cookieVerificationWaiters: upgrade.cookieVerificationGate?.waiters ?? 0,
    })
    return true
  }
  if (url.pathname === '/__test__/release-legacy-cookie-identity') {
    state.legacySessionUpgrade?.cookieVerificationGate?.release()
    sendJSON(response, 200, { released: true })
    return true
  }
  if (url.pathname === '/__test__/payload') {
    const identity = identityFrom(url.searchParams.get('identity'))
    const title = url.searchParams.get('title')
    if (title) state.titles[identity] = title
    sendJSON(response, 200, { identity, title: state.titles[identity] })
    return true
  }
  if (url.pathname === '/__test__/block-identity') {
    const identity = identityFrom(url.searchParams.get('identity'))
    block(state.identityGates, identity)
    sendJSON(response, 200, { identity })
    return true
  }
  if (url.pathname === '/__test__/release-identity') {
    const identity = identityFrom(url.searchParams.get('identity'))
    release(state.identityGates, identity)
    sendJSON(response, 200, { identity })
    return true
  }
  if (url.pathname === '/__test__/block-links') {
    const identity = identityFrom(url.searchParams.get('identity'))
    block(state.linkGates, identity)
    sendJSON(response, 200, { identity })
    return true
  }
  if (url.pathname === '/__test__/block-links-after-one') {
    const identity = identityFrom(url.searchParams.get('identity'))
    block(state.linkGates, identity)
    state.linksAfterOne.add(identity)
    sendJSON(response, 200, { identity })
    return true
  }
  if (url.pathname === '/__test__/release-links') {
    const identity = identityFrom(url.searchParams.get('identity'))
    release(state.linkGates, identity)
    state.linksAfterOne.delete(identity)
    sendJSON(response, 200, { identity })
    return true
  }
  if (url.pathname === '/__test__/state') {
    sendJSON(response, 200, {
      titles: state.titles,
      identityWaiters: gateWaiters(state.identityGates),
      linkWaiters: gateWaiters(state.linkGates),
      annotationBarriers: Object.fromEntries(
        [...state.annotationBarriers].map(([name, barrier]) => [name, {
          participants: barrier.participants,
          parties: [...barrier.parties],
        }]),
      ),
    })
    return true
  }
  return false
}

async function handleAPI(request, response, url) {
  const legacyUpgrade = state.legacySessionUpgrade
  if (legacyUpgrade !== null && url.pathname === '/api/session') {
    if (request.method === 'POST') {
      legacyUpgrade.exchanges += 1
      legacyUpgrade.events.push('post')
      const body = await readJSONBody(request)
      if (body?.token !== legacyUpgrade.token) {
        sendJSON(response, 401, { error: { code: 401, message: 'installation token is invalid' } })
        return true
      }
      const identity = legacyUpgrade.sessionIdentity
      const namespace = namespaces[identity]
      sendJSON(response, 201, {
        expires_at: '2030-01-01T00:00:00Z',
        client_data_namespace: namespace,
        representation_contract: 'v3',
      }, {
        'X-WebTag-Data-Namespace': namespace,
        'Set-Cookie': `webtag_session=${identity}; Path=/; HttpOnly; SameSite=Lax`,
      })
      return true
    }
    if (request.method === 'DELETE') {
      legacyUpgrade.deletes += 1
      legacyUpgrade.events.push('delete')
      sendJSON(response, 204, null, {
        'Set-Cookie': 'webtag_session=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0',
      })
      return true
    }
    if (request.method === 'GET') {
      const authorization = String(request.headers.authorization ?? '')
      let identity
      if (authorization === `Bearer ${legacyUpgrade.token}`) {
        legacyUpgrade.bearerGets += 1
        identity = legacyUpgrade.bearerIdentity
      } else if (
        request.headers['x-webtag-session'] === '1' &&
        ['A', 'B'].includes(sessionCookieValue(request))
      ) {
        legacyUpgrade.sessionGets += 1
        identity = cookieIdentity(request)
        const gate = legacyUpgrade.cookieVerificationGate
        if (gate !== null && !legacyUpgrade.cookieVerificationClaimed) {
          legacyUpgrade.cookieVerificationClaimed = true
          gate.waiters += 1
          try {
            await gate.promise
          } finally {
            gate.waiters -= 1
          }
        }
      } else {
        sendJSON(response, 401, { error: { code: 401, message: 'unauthorized' } })
        return true
      }
      const namespace = namespaces[identity]
      sendJSON(response, 200, {
        client_data_namespace: namespace,
        representation_contract: 'v3',
      }, { 'X-WebTag-Data-Namespace': namespace })
      return true
    }
  }

  const identity = cookieIdentity(request)
  const namespace = namespaces[identity]
  const ownershipHeaders = { 'X-WebTag-Data-Namespace': namespace }

  if (url.pathname === '/api/session' && request.method === 'GET') {
    await waitForGate(state.identityGates, identity)
    sendJSON(response, 200, {
      client_data_namespace: namespace,
      representation_contract: 'v3',
    }, ownershipHeaders)
    return true
  }
  if (url.pathname === '/api/capabilities' && request.method === 'GET') {
    sendJSON(response, 200, {
      library_kinds: true,
      site_library: true,
      site_auto_classification: false,
      site_management: false,
      site_advanced_management: false,
      archive_versions: [],
      reader_vnext: false,
      reader: {
        annotations: false,
        notes: false,
        inbox: false,
        todos: false,
        engagement: false,
        home: false,
        feed: false,
        ai: false,
        semantic: false,
        activity: false,
        history: false,
        trash: false,
      },
    }, ownershipHeaders)
    return true
  }
  if (url.pathname === '/api/links' && request.method === 'GET') {
    const bypassGate = state.linksAfterOne.delete(identity)
    if (!bypassGate) await waitForGate(state.linkGates, identity)
    sendJSON(response, 200, {
      items: [linkFor(identity)],
      total: 1,
      page: 1,
      limit: 30,
    }, ownershipHeaders)
    return true
  }
  if (url.pathname === '/api/tags') {
    sendJSON(response, 200, [], ownershipHeaders)
    return true
  }
  if (url.pathname === '/api/tree') {
    sendJSON(response, 200, { nodes: [], total: 1 }, ownershipHeaders)
    return true
  }
  if (url.pathname === '/api/sites') {
    const pageParam = Number(url.searchParams.get('page') ?? '1')
    const limitParam = Number(url.searchParams.get('limit') ?? '30')
    const page = Number.isSafeInteger(pageParam) && pageParam > 0 ? pageParam : 1
    const limit = Number.isSafeInteger(limitParam) && limitParam > 0 ? limitParam : 30
    const total = 61
    const start = (page - 1) * limit
    const items = Array.from({ length: total }, (_, index) => siteFor(index + 1))
      .slice(start, start + limit)
    sendJSON(response, 200, { items, total, page, limit }, ownershipHeaders)
    return true
  }

  sendJSON(response, 404, { error: { code: 404, message: 'not found' } }, ownershipHeaders)
  return true
}

const vite = await createViteServer({
  root,
  appType: 'spa',
  // Vite's middleware HMR defaults to the process-wide port 24678. Give each
  // isolated E2E server its own adjacent port so browser matrices can run in
  // parallel without surfacing an unrelated WebSocket page error.
  server: { middlewareMode: true, hmr: { port: port + 2 } },
})

const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url ?? '/', `http://127.0.0.1:${port}`)
    if (url.pathname.startsWith('/__test__/')) {
      if (!(await handleTestRoute(request, response, url))) {
        sendJSON(response, 404, { error: 'unknown test route' })
      }
      return
    }
    if (url.pathname.startsWith('/api/')) {
      await handleAPI(request, response, url)
      return
    }
    vite.middlewares(request, response)
  } catch (error) {
    sendJSON(response, 500, { error: error instanceof Error ? error.message : String(error) })
  }
})

const imageProbeServer = http.createServer((request, response) => {
  state.imageProbeRequests.push(request.url ?? '/')
  response.writeHead(200, {
    'Content-Type': 'image/png',
    'Cache-Control': 'no-store',
  })
  response.end(Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
    'base64',
  ))
})

server.listen(port, '127.0.0.1')
imageProbeServer.listen(imageProbePort, '127.0.0.1')

async function shutdown() {
  releaseAll(state.identityGates)
  releaseAll(state.linkGates)
  failAllAnnotationBarriers('annotation barrier server shutdown')
  await vite.close()
  await Promise.all([
    new Promise((resolve) => server.close(resolve)),
    new Promise((resolve) => imageProbeServer.close(resolve)),
  ])
  process.exit(0)
}

process.on('SIGINT', () => void shutdown())
process.on('SIGTERM', () => void shutdown())
