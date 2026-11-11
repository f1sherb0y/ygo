// Drives the application backend in ./backend (the ygo server with tokens
// that decide the scope, a row per update, stateless answers and
// revocation) with stock @hocuspocus/provider 3.4.4 clients, the way a web
// application's editor does: a token function, a provider per document,
// stateless messages, replay from the stored rows. Exits non-zero if any
// check fails.
//
//   node backend.mjs path/to/backend
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import WebSocket from 'ws'
import * as Y from 'yjs'
import { HocuspocusProvider } from 'hocuspocus-provider-3'

const bin = process.argv[2]
if (!bin) {
  console.error('usage: node backend.mjs path/to/backend')
  process.exit(2)
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

async function waitFor (cond, ms = 8000) {
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

const data = mkdtempSync(join(tmpdir(), 'ygo-backend-'))

async function startServer () {
  const proc = spawn(bin, ['-addr', '127.0.0.1:0', '-data', data], { stdio: ['ignore', 'ignore', 'pipe'] })
  let log = ''
  proc.stderr.on('data', (d) => { log += d })
  if (!await waitFor(() => /listening on \S+/.test(log), 10000)) {
    proc.kill()
    throw new Error(`backend did not start:\n${log}`)
  }
  const host = log.match(/listening on (\S+)/)[1]
  const exited = new Promise((resolve) => proc.on('exit', resolve))
  return {
    ws: `ws://${host}/api/ws`,
    http: `http://${host}`,
    stop: async () => { proc.kill(); await exited },
    log: () => log
  }
}

const open = []

// connect opens a provider the way the application does: the token comes
// from a function that is asked on every connection.
function connect (server, name, token) {
  const doc = new Y.Doc()
  const state = { failed: null, synced: false, scope: null, stateless: [], closed: 0, tokenCalls: 0, token }
  const p = new HocuspocusProvider({
    url: server.ws,
    name,
    document: doc,
    token: async () => { state.tokenCalls++; return state.token },
    WebSocketPolyfill: WebSocket,
    onSynced: () => { state.synced = true },
    onAuthenticated: ({ scope }) => { state.scope = scope },
    onAuthenticationFailed: ({ reason }) => { state.failed = reason },
    onStateless: ({ payload }) => { state.stateless.push(JSON.parse(payload)) },
    onClose: () => { state.closed++ }
  })
  open.push(p)
  return { p, doc, text: doc.getText('code'), state }
}

// barrier asks the server whether everything sent so far is stored, and
// returns the sequence number it answers with.
async function barrier (client) {
  const id = Math.random().toString(36).slice(2)
  client.p.sendStateless(JSON.stringify({ type: 'durability-barrier', id }))
  let reply
  await waitFor(() => (reply = client.state.stateless.find((m) => m.type === 'durability-barrier' && m.id === id)))
  return reply ? reply.seq : -1
}

async function rows (server, name) {
  const res = await fetch(`${server.http}/rows?doc=${encodeURIComponent(name)}`)
  return (await res.json()).map((r) => ({ seq: r.seq, update: new Uint8Array(Buffer.from(r.update, 'base64')) }))
}

// replay rebuilds the text from the first n stored rows, as a replay page
// does when the timeline is dragged.
function replay (stored, n) {
  const doc = new Y.Doc()
  for (const r of stored.slice(0, n)) Y.applyUpdate(doc, r.update)
  return doc.getText('code').toString()
}

async function main () {
  let server = await startServer()
  const room = 'room/3f2c 你好'

  // Two tokens for one document: an editor and a viewer.
  const editor = connect(server, room, 'edit:ana')
  const viewer = connect(server, room, 'view:guest')
  check('an editor and a viewer of one document sync', await waitFor(() => editor.state.synced && viewer.state.synced))
  check('the editor may write', editor.state.scope === 'read-write', `scope ${editor.state.scope}`)
  check('the viewer may not', viewer.state.scope === 'readonly', `scope ${viewer.state.scope}`)

  const refused = connect(server, room, 'nonsense')
  check('a token the backend does not know is refused with its reason',
    await waitFor(() => refused.state.failed === 'Invalid token'), `reason ${refused.state.failed}`)
  check('and gets nothing', !refused.state.synced)

  // Typing: a transaction per keystroke, positions counted as JavaScript
  // counts them (an emoji is two).
  const typed = 'fn main() {\n  // 你好 🙂 𝒳\n  println!("hi");\n}\n'
  for (const ch of typed) editor.text.insert(editor.text.length, ch)
  editor.text.delete(typed.indexOf('🙂'), 2)
  editor.text.insert(typed.indexOf('🙂'), '🚀')
  const expected = typed.replace('🙂', '🚀')
  check('the viewer sees what the editor types', await waitFor(() => viewer.text.toString() === expected),
    JSON.stringify(viewer.text.toString()))
  check('the editor has no unsynced changes', await waitFor(() => editor.p.unsyncedChanges === 0), `${editor.p.unsyncedChanges}`)

  viewer.text.insert(0, 'viewer was here ')
  await sleep(300)
  check("the viewer's edits reach nobody", !editor.text.toString().includes('viewer was here'))

  // Is it stored? The answer carries the number of the last row.
  const seq = await barrier(editor)
  const stored = await rows(server, room)
  check('the durability barrier is answered with the last row', seq > 0 && seq === stored.length, `seq ${seq}, rows ${stored.length}`)
  check('there is a row per update', stored.length >= [...typed].length, `${stored.length} rows`)
  check('the rows are numbered without gaps', stored.every((r, i) => r.seq === i + 1))

  // Replay from the rows alone, at the end and at every point before.
  check('the rows replay to the text', replay(stored, stored.length) === expected, JSON.stringify(replay(stored, stored.length)))
  let prefixOk = true
  for (let n = 0; n <= [...typed].length; n++) {
    if (replay(stored, n) !== [...typed].slice(0, n).join('')) { prefixOk = false; break }
  }
  check('and to every state on the way', prefixOk)

  // Presence over stateless messages: asked by one, told to all.
  editor.p.sendStateless(JSON.stringify({ type: 'presence', clientId: editor.doc.clientID }))
  const present = (c) => c.state.stateless.some((m) => m.type === 'presence' && m.users.join() === 'ana,guest')
  check('a stateless broadcast reaches every connection of the document', await waitFor(() => present(editor) && present(viewer)))
  await fetch(`${server.http}/announce?doc=${encodeURIComponent(room)}&text=rust`, { method: 'POST' })
  const announced = (c) => c.state.stateless.some((m) => m.type === 'announce' && m.text === 'rust')
  check('the backend tells the room something on its own', await waitFor(() => announced(editor) && announced(viewer)))

  // Another document is another room, with its own rows.
  const other = connect(server, 'room/other', 'edit:ana')
  await waitFor(() => other.state.synced)
  other.text.insert(0, 'elsewhere')
  await barrier(other)
  check('documents do not mix', (await rows(server, 'room/other')).length === 1 && !viewer.text.toString().includes('elsewhere') &&
    !other.state.stateless.some((m) => m.type === 'announce'))

  // Edits made without a connection arrive when it is back.
  const second = connect(server, room, 'edit:ben')
  await waitFor(() => second.state.synced)
  second.p.disconnect()
  await waitFor(() => second.p.configuration.websocketProvider.status === 'disconnected')
  second.text.insert(0, '// offline\n')
  editor.text.insert(editor.text.length, '// meanwhile\n')
  await second.p.connect()
  const merged = '// offline\n' + expected + '// meanwhile\n'
  check('offline edits and the edits made meanwhile merge', await waitFor(() =>
    editor.text.toString() === merged && second.text.toString() === merged &&
    // The viewer still has the edit it made for itself.
    viewer.text.toString().replace('viewer was here ', '') === merged),
  JSON.stringify(editor.text.toString()))
  check('the token function is asked again on reconnect', second.state.tokenCalls >= 2, `${second.state.tokenCalls} calls`)

  // A session that turns into a viewer: the new token narrows the scope.
  second.p.disconnect()
  await waitFor(() => second.p.configuration.websocketProvider.status === 'disconnected')
  second.state.token = 'view:ben'
  await second.p.connect()
  check('a new token changes the scope', await waitFor(() => second.state.scope === 'readonly'), `scope ${second.state.scope}`)

  // Access that ends: the viewer's connection is closed, the others stay.
  const before = viewer.state.closed
  await fetch(`${server.http}/revoke?user=guest`, { method: 'POST' })
  check('revoking a user closes their connection', await waitFor(() => viewer.state.closed > before))
  check('and the provider is told why when it tries again',
    await waitFor(() => viewer.state.failed === 'Access revoked', 20000), `reason ${viewer.state.failed}`)
  viewer.p.destroy()
  editor.text.insert(editor.text.length, '// after\n')
  check('the others carry on', await waitFor(() => second.text.toString().endsWith('// after\n')))

  // A snapshot, more edits, and the process is gone. What comes back is
  // the snapshot and the rows after it.
  await barrier(editor)
  const checkpoint = Number(await (await fetch(`${server.http}/checkpoint?doc=${encodeURIComponent(room)}`, { method: 'POST' })).text())
  editor.text.insert(editor.text.length, '// after the snapshot\n')
  const last = await barrier(editor)
  check('rows continue after a snapshot', checkpoint > 0 && last === checkpoint + 1, `checkpoint ${checkpoint}, last ${last}`)
  const final = editor.text.toString()
  for (const p of open.splice(0)) p.destroy()
  await server.stop()

  server = await startServer()
  const again = connect(server, room, 'view:carol')
  check('after a restart the document is loaded from the snapshot and the rows',
    await waitFor(() => again.text.toString() === final), JSON.stringify(again.text.toString()))
  const writer = connect(server, room, 'edit:ana')
  await waitFor(() => writer.state.synced)
  writer.text.insert(0, '// second run\n')
  const afterRestart = await barrier(writer)
  check('and the rows go on from where they were', afterRestart === last + 1, `row ${afterRestart}, want ${last + 1}`)
  const all = await rows(server, room)
  check('the whole history still replays', replay(all, all.length) === '// second run\n' + final)

  for (const p of open.splice(0)) p.destroy()
  await server.stop()
}

try {
  await main()
} catch (err) {
  console.error(err)
  failures++
} finally {
  for (const p of open) { try { p.destroy() } catch {} }
  try { rmSync(data, { recursive: true, force: true }) } catch {}
}
console.log(failures === 0 ? 'all checks passed' : `${failures} checks failed`)
process.exit(failures === 0 ? 0 : 1)
