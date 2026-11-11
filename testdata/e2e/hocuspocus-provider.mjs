// Runs stock @hocuspocus/provider 4.8.0, 3.4.4 and 2.15.3, each on a socket
// of its own and several on one, against the ygo
// server in Hocuspocus mode (testdata/e2e/hpserver) and exits non-zero if
// any check fails.
//
//   node hocuspocus-provider.mjs path/to/hpserver
import { spawn } from 'node:child_process'
import WebSocket from 'ws'
import * as Y from 'yjs'

const bin = process.argv[2]
if (!bin) {
  console.error('usage: node hocuspocus-provider.mjs path/to/hpserver')
  process.exit(2)
}

const PACKAGES = { 4: '@hocuspocus/provider', 3: 'hocuspocus-provider-3', 2: 'hocuspocus-provider-2' }

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

async function waitFor (cond, ms = 5000) {
  const start = Date.now()
  while (Date.now() - start < ms) {
    if (await cond()) return true
    await sleep(20)
  }
  return false
}

let failures = 0
function check (name, ok, detail = '') {
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${ok || !detail ? '' : `: ${detail}`}`)
  if (!ok) failures++
}

async function startServer (args = []) {
  const proc = spawn(bin, ['-addr', '127.0.0.1:0', ...args], { stdio: ['ignore', 'ignore', 'pipe'] })
  let log = ''
  proc.stderr.on('data', (d) => { log += d })
  if (!await waitFor(() => /listening on \S+/.test(log), 10000)) {
    proc.kill()
    throw new Error(`hpserver did not start:\n${log}`)
  }
  const url = 'ws://' + log.match(/listening on (\S+)/)[1]
  return { url, stop: () => proc.kill(), log: () => log }
}

const open = []

async function connect (v, url, name, token = 'ok') {
  const { HocuspocusProvider } = await import(PACKAGES[v])
  const doc = new Y.Doc()
  const events = { failed: null, synced: false }
  const p = new HocuspocusProvider({
    url,
    name,
    document: doc,
    token,
    WebSocketPolyfill: WebSocket,
    onSynced: () => { events.synced = true },
    onAuthenticationFailed: ({ reason }) => { events.failed = reason }
  })
  open.push(p)
  return { p, doc, map: doc.getMap('m'), events }
}

async function run (v, url) {
  const tag = `provider ${v}:`
  const a = await connect(v, url, `doc-${v}`)
  const b = await connect(v, url, `doc-${v}`)
  check(`${tag} two providers sync`, await waitFor(() => a.events.synced && b.events.synced))

  a.map.set('from', 'a')
  check(`${tag} an edit reaches the other provider`, await waitFor(() => b.map.get('from') === 'a'))
  b.doc.getText('t').insert(0, 'hello 你好 🙂')
  check(`${tag} text comes back`, await waitFor(() => a.doc.getText('t').toString() === 'hello 你好 🙂'))
  check(`${tag} changes are acknowledged`, await waitFor(() => a.p.unsyncedChanges === 0 && b.p.unsyncedChanges === 0),
    `unsyncedChanges ${a.p.unsyncedChanges}, ${b.p.unsyncedChanges}`)

  a.p.awareness.setLocalStateField('user', { name: `a${v}` })
  check(`${tag} awareness reaches the other provider`, await waitFor(() =>
    [...b.p.awareness.getStates().values()].some((s) => s.user && s.user.name === `a${v}`)))

  // A provider that joins later gets the document, and one that leaves
  // disappears from the others' awareness.
  const c = await connect(v, url, `doc-${v}`)
  check(`${tag} a late provider gets the document`, await waitFor(() => c.map.get('from') === 'a' && c.doc.getText('t').length > 0))
  const clientA = a.doc.clientID
  a.p.destroy()
  check(`${tag} a provider that left is removed from awareness`, await waitFor(() => !b.p.awareness.getStates().has(clientA)))

  // Another document on the same URL is another room.
  const other = await connect(v, url, `other-${v}`)
  check(`${tag} another document syncs`, await waitFor(() => other.events.synced))
  other.map.set('only', 'here')
  await sleep(200)
  check(`${tag} documents are separate`, b.map.get('only') === undefined && other.map.get('from') === undefined)

  // A refused token: the reason arrives, the document does not.
  const bad = await connect(v, url, `doc-${v}`, 'bad')
  check(`${tag} a refused token is reported`, await waitFor(() => bad.events.failed !== null), 'no onAuthenticationFailed')
  check(`${tag} with the server's reason`, bad.events.failed === 'bad-token', `reason ${bad.events.failed}`)
  check(`${tag} and gets no content`, bad.map.get('from') === undefined && !bad.events.synced)

  // A read-only document: providers sync, their edits go nowhere.
  const r1 = await connect(v, url, `ro-${v}`)
  const r2 = await connect(v, url, `ro-${v}`)
  check(`${tag} a read-only document syncs`, await waitFor(() => r1.events.synced && r2.events.synced))
  check(`${tag} with the read-only scope`, await waitFor(() => r1.p.authorizedScope === 'readonly'), `scope ${r1.p.authorizedScope}`)
  r1.map.set('nope', 1)
  await sleep(300)
  check(`${tag} and takes no edits`, r2.map.get('nope') === undefined)
}

// Several providers on one websocket, as an application with several
// documents open uses them: each document is a session of its own.
async function shared (v, url) {
  const tag = `provider ${v}, shared socket:`
  const { HocuspocusProvider, HocuspocusProviderWebsocket } = await import(PACKAGES[v])
  const socket = new HocuspocusProviderWebsocket({ url, WebSocketPolyfill: WebSocket })
  const on = (name, token = 'ok') => {
    const doc = new Y.Doc()
    const events = { failed: null, synced: false }
    const p = new HocuspocusProvider({
      websocketProvider: socket,
      name,
      document: doc,
      token,
      onSynced: () => { events.synced = true },
      onAuthenticationFailed: ({ reason }) => { events.failed = reason }
    })
    if (typeof p.attach === 'function') p.attach()
    open.push(p)
    return { p, doc, map: doc.getMap('m'), events }
  }
  const one = on(`shared-one-${v}`)
  const two = on(`shared-two-${v}`)
  const peerOne = await connect(v, url, `shared-one-${v}`)
  const peerTwo = await connect(v, url, `shared-two-${v}`)
  check(`${tag} two documents sync over it`, await waitFor(() => one.events.synced && two.events.synced))
  one.map.set('doc', 'one')
  two.map.set('doc', 'two')
  check(`${tag} each reaches its own room`, await waitFor(() => peerOne.map.get('doc') === 'one' && peerTwo.map.get('doc') === 'two'))
  peerTwo.map.set('back', 'two')
  check(`${tag} and hears from it`, await waitFor(() => two.map.get('back') === 'two') && one.map.get('back') === undefined)
  check(`${tag} changes are acknowledged per document`, await waitFor(() => one.p.unsyncedChanges === 0 && two.p.unsyncedChanges === 0))

  const bad = on(`shared-three-${v}`, 'bad')
  check(`${tag} a refused token is reported for its document`, await waitFor(() => bad.events.failed === 'bad-token'), `reason ${bad.events.failed}`)
  one.map.set('after', 'refusal')
  check(`${tag} and the other documents go on`, await waitFor(() => peerOne.map.get('after') === 'refusal'))
  try { socket.destroy() } catch {}
}

const server = await startServer()
try {
  for (const v of [4, 3, 2]) await run(v, server.url)
  for (const v of [4, 3, 2]) await shared(v, server.url)
} catch (error) {
  console.error(error)
  failures++
}
for (const p of open) { try { p.destroy() } catch {} }
server.stop()

// Without a token check, a provider that sends no token connects too.
const openServer = await startServer(['-auth=false'])
try {
  for (const v of [4, 3, 2]) {
    const a = await connect(v, openServer.url, `open-${v}`, null)
    const b = await connect(v, openServer.url, `open-${v}`, null)
    a.map.set('k', v)
    check(`provider ${v}: syncs without a token when none is checked`, await waitFor(() => b.map.get('k') === v))
  }
} catch (error) {
  console.error(error)
  failures++
}
for (const p of open) { try { p.destroy() } catch {} }
openServer.stop()

console.log(failures === 0 ? 'all checks passed' : `${failures} checks failed`)
process.exit(failures === 0 ? 0 : 1)
