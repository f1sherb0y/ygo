// Drives the application backend in ./backend with stock
// @hocuspocus/provider 3.4.4 clients, the way a web application's editor
// does: a token function, a provider per document, the stateless messages
// of the backend's contract, replay from the stored rows. Exits non-zero
// if any check fails.
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
  const proc = spawn(bin, ['-addr', '127.0.0.1:0', '-data', data, '-heartbeat', '500ms'], { stdio: ['ignore', 'ignore', 'pipe'] })
  let log = ''
  proc.stderr.on('data', (d) => { log += d })
  if (!await waitFor(() => /listening on \S+/.test(log), 10000)) {
    proc.kill()
    throw new Error(`backend did not start:\n${log}`)
  }
  const host = log.match(/listening on (\S+)/)[1]
  const exited = new Promise((resolve) => proc.on('exit', resolve))
  const post = (path) => fetch(`http://${host}${path}`, { method: 'POST' })
  return {
    ws: `ws://${host}/api/ws`,
    http: `http://${host}`,
    post,
    stop: async () => { proc.kill(); await exited },
    log: () => log
  }
}

// token makes the kind of token the backend reads: a JWT whose signature
// it does not check.
function token (userId, { ro = false, ttl = 3600 } = {}) {
  const enc = (v) => Buffer.from(JSON.stringify(v)).toString('base64url')
  return `${enc({ alg: 'none' })}.${enc({ type: 'user', userId, username: userId, ro, exp: Math.floor(Date.now() / 1000) + ttl })}.test`
}

const open = []

// connect opens a provider the way the application does: the token comes
// from a function that is asked on every connection.
function connect (server, name, tok) {
  const doc = new Y.Doc()
  const state = { failed: null, synced: false, scope: null, stateless: [], closes: [], tokenCalls: 0, token: tok }
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
    onClose: ({ event }) => { state.closes.push({ code: event.code, reason: event.reason }) }
  })
  open.push(p)
  const last = (type) => state.stateless.findLast((m) => m.type === type)
  return { p, doc, text: doc.getText('codemirror'), state, last }
}

// saved asks the backend whether everything sent so far is stored and
// returns the sequence number it answers with.
async function saved (client) {
  const id = Math.random().toString(36).slice(2)
  client.p.sendStateless(JSON.stringify({ type: 'durability-barrier', id }))
  let reply
  await waitFor(() => (reply = client.state.stateless.find((m) => m.type === 'durability-ack' && m.id === id)))
  return reply ? reply.durableSeq : -1
}

async function rows (server, name) {
  const res = await fetch(`${server.http}/rows?doc=${encodeURIComponent(name)}`)
  return (await res.json()).map((r) => ({ ...r, update: new Uint8Array(Buffer.from(r.update, 'base64')) }))
}

// replay rebuilds the text from the first n stored rows, as a replay page
// does when the timeline is dragged.
function replay (stored, n) {
  const doc = new Y.Doc()
  for (const r of stored.slice(0, n)) Y.applyUpdate(doc, r.update)
  return doc.getText('codemirror').toString()
}

async function main () {
  let server = await startServer()
  const room = 'room/3f2c 你好'
  const q = encodeURIComponent(room)
  await server.post(`/language?doc=${q}&value=rust`)

  // Two tokens for one document: an editor and a viewer.
  const editor = connect(server, room, token('ana'))
  const viewer = connect(server, room, token('guest', { ro: true }))
  check('an editor and a viewer of one document sync', await waitFor(() => editor.state.synced && viewer.state.synced))
  check('the editor may write', editor.state.scope === 'read-write', `scope ${editor.state.scope}`)
  check('the viewer may not', viewer.state.scope === 'readonly', `scope ${viewer.state.scope}`)

  // What a session is sent when it opens.
  check('each session is sent its colour', await waitFor(() => editor.last('session-color') && viewer.last('session-color')) &&
    editor.last('session-color').slot !== viewer.last('session-color').slot)
  check('the state of the store', await waitFor(() => editor.last('persistence-status')?.protocol === 1))
  check('and the language of the room', await waitFor(() => viewer.last('room-language')?.language === 'rust'))

  const refused = connect(server, room, 'nonsense')
  check('a token the backend cannot read is refused with its reason',
    await waitFor(() => refused.state.failed === 'Invalid token'), `reason ${refused.state.failed}`)
  check('and gets nothing', !refused.state.synced && refused.state.stateless.length === 0)
  const expired = connect(server, room, token('late', { ttl: -60 }))
  check('an expired token is refused as expired', await waitFor(() => expired.state.failed === 'Token expired'), `reason ${expired.state.failed}`)

  // Who is here: each client says which Yjs client it is, everyone is told.
  for (const c of [editor, viewer]) c.p.sendStateless(JSON.stringify({ type: 'presence', clientId: c.doc.clientID }))
  const members = (c) => (c.last('presence-state')?.members ?? []).map((m) => `${m.id}:${m.clientId}`).sort().join()
  const expectedMembers = [`ana:${editor.doc.clientID}`, `guest:${viewer.doc.clientID}`].sort().join()
  check('the room is told who is in it', await waitFor(() => members(editor) === expectedMembers && members(viewer) === expectedMembers),
    `${members(editor)} / ${members(viewer)}`)

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

  // Is it stored? The answer comes when it is, with the number of the
  // last row.
  const seq = await saved(editor)
  const stored = await rows(server, room)
  check('the durability barrier is answered with the last row', seq > 0 && seq === stored.length, `seq ${seq}, rows ${stored.length}`)
  check('there is a row per update', stored.length >= [...typed].length, `${stored.length} rows`)
  check('the rows are numbered without gaps', stored.every((r, i) => r.seq === i + 1))
  check('each row has its author', stored.every((r) => r.userId === 'ana'), [...new Set(stored.map((r) => r.userId))].join())
  check('the room is told when nothing is waiting',
    await waitFor(() => viewer.last('persistence-status')?.pending === 0 && viewer.last('persistence-status')?.durableSeq === seq))

  // A store that is slow: the answer waits for it, and the room knows.
  await server.post('/stall?on=1')
  editor.text.insert(editor.text.length, '// slow\n')
  const id = 'slow-barrier'
  editor.p.sendStateless(JSON.stringify({ type: 'durability-barrier', id }))
  check('the room is told that updates are waiting', await waitFor(() => viewer.last('persistence-status')?.pending > 0))
  await sleep(400)
  check('a barrier is not answered before the store has the update', !editor.state.stateless.some((m) => m.type === 'durability-ack' && m.id === id))
  await server.post('/stall?on=0')
  check('and is answered once it has',
    await waitFor(() => editor.state.stateless.some((m) => m.type === 'durability-ack' && m.id === id && m.durableSeq === seq + 1)))
  const text1 = expected + '// slow\n'

  // Replay from the rows alone, at the end and at every point before.
  const now = await rows(server, room)
  check('the rows replay to the text', replay(now, now.length) === text1, JSON.stringify(replay(now, now.length)))
  let prefixOk = true
  for (let n = 0; n <= [...typed].length; n++) {
    if (replay(now, n) !== [...typed].slice(0, n).join('')) { prefixOk = false; break }
  }
  check('and to every state on the way', prefixOk)

  check('a heartbeat arrives on a timer', await waitFor(() => editor.last('heartbeat') && viewer.last('heartbeat')))
  await server.post(`/language?doc=${q}&value=go`)
  check('a change of the room is told to everyone in it',
    await waitFor(() => editor.last('room-language')?.language === 'go' && viewer.last('room-language')?.language === 'go'))

  // Another document is another room, with its own rows.
  const other = connect(server, 'room/other', token('ana'))
  await waitFor(() => other.state.synced)
  other.text.insert(0, 'elsewhere')
  await saved(other)
  check('documents do not mix', (await rows(server, 'room/other')).length === 1 && !viewer.text.toString().includes('elsewhere') &&
    !other.last('room-language'))

  // Edits made without a connection arrive when it is back, under the
  // name of who made them.
  const second = connect(server, room, token('ben'))
  await waitFor(() => second.state.synced)
  second.p.disconnect()
  await waitFor(() => second.p.configuration.websocketProvider.status === 'disconnected')
  check('someone who left is no longer listed', await waitFor(() => !members(editor).includes('ben')))
  second.text.insert(0, '// offline\n')
  editor.text.insert(editor.text.length, '// meanwhile\n')
  await second.p.connect()
  const merged = '// offline\n' + text1 + '// meanwhile\n'
  check('offline edits and the edits made meanwhile merge', await waitFor(() =>
    editor.text.toString() === merged && second.text.toString() === merged &&
    // The viewer still has the edit it made for itself.
    viewer.text.toString().replace('viewer was here ', '') === merged),
  JSON.stringify(editor.text.toString()))
  check('the token function is asked again on reconnect', second.state.tokenCalls >= 2, `${second.state.tokenCalls} calls`)
  await saved(second)
  check('the offline edit is stored as its author\'s', (await rows(server, room)).some((r) => r.userId === 'ben'))

  // A renewed token on the open connection: the same user goes on.
  const callsBefore = second.state.tokenCalls
  const closesBefore = second.state.closes.length
  second.state.token = token('ben', { ttl: 7200 })
  second.state.scope = null
  await second.p.sendToken()
  check('a renewed token is accepted on the open connection',
    await waitFor(() => second.state.scope === 'read-write') && second.state.tokenCalls > callsBefore && second.state.closes.length === closesBefore,
    `scope ${second.state.scope}, calls ${second.state.tokenCalls} (${callsBefore} before), closes ${JSON.stringify(second.state.closes)}`)
  second.text.insert(0, '// renewed\n')
  check('and the session goes on', await waitFor(() => editor.text.toString().startsWith('// renewed\n')))
  // Another user's token on it is refused.
  second.state.token = token('mallory')
  await second.p.sendToken()
  check("another user's token on the same connection is refused",
    await waitFor(() => second.state.failed === 'Access revoked'), `reason ${second.state.failed}`)
  second.p.destroy()

  // Access that ends: the viewer's connection is closed, the others stay.
  await server.post('/revoke?user=guest')
  check('revoking a user closes their connection with 4403',
    await waitFor(() => viewer.state.closes.some((c) => c.code === 4403 && c.reason === 'Access revoked')), JSON.stringify(viewer.state.closes))
  check('and their next token is refused',
    await waitFor(() => viewer.state.failed === 'Access revoked', 20000), `reason ${viewer.state.failed}`)
  viewer.p.destroy()
  editor.text.insert(editor.text.length, '// after\n')
  const third = connect(server, room, token('cy'))
  check('the others carry on', await waitFor(() => third.text.toString().endsWith('// after\n')))

  // A snapshot, more edits, and the room is ended: everyone is told, and
  // nobody writes any more, without reconnecting.
  await saved(editor)
  const checkpoint = Number(await (await server.post(`/checkpoint?doc=${q}`)).text())
  editor.text.insert(editor.text.length, '// after the snapshot\n')
  const last = await saved(editor)
  check('rows continue after a snapshot', checkpoint > 0 && last === checkpoint + 1, `checkpoint ${checkpoint}, last ${last}`)
  const final = editor.text.toString()
  await waitFor(() => third.text.toString() === final)
  await server.post(`/end?doc=${q}`)
  check('ending the room is told to everyone in it',
    await waitFor(() => editor.last('room-status')?.status === 'ended' && third.last('room-status')?.status === 'ended'))
  editor.text.insert(0, '// too late\n')
  await sleep(400)
  check('and nobody writes to it any more', !third.text.toString().includes('too late') && await saved(third) === last)
  const lateJoiner = connect(server, room, token('dee'))
  check('whoever joins an ended room is a reader', await waitFor(() => lateJoiner.state.scope === 'readonly' && lateJoiner.text.toString() === final),
    `scope ${lateJoiner.state.scope}`)

  for (const p of open.splice(0)) p.destroy()
  await server.stop()

  // The process is gone. What comes back is the snapshot and the rows
  // after it.
  server = await startServer()
  const again = connect(server, room, token('carol', { ro: true }))
  check('after a restart the document is loaded from the snapshot and the rows',
    await waitFor(() => again.text.toString() === final), JSON.stringify(again.text.toString()))
  check('with the store where it was', await waitFor(() => again.last('persistence-status')?.durableSeq === last))
  const writer = connect(server, room, token('ana'))
  await waitFor(() => writer.state.synced)
  writer.text.insert(0, '// second run\n')
  const afterRestart = await saved(writer)
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
