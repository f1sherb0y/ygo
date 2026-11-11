// Holds yjs documents for the conformance tests. One JSON command per line on
// stdin, one JSON answer per line on stdout (see ../../driver.go).
import * as Y from 'yjs'
import * as presence from 'y-protocols/awareness'
import { createInterface } from 'node:readline'

const hex = (bytes) => Buffer.from(bytes).toString('hex')
const unhex = (text) => new Uint8Array(Buffer.from(text, 'hex'))
const at = (n, length) => Math.floor((n * (length + 1)) / 65536)
const within = (n, length) => Math.floor((n * length) / 65536)

function newDoc(clientID, gc) {
  const doc = new Y.Doc({ gc })
  doc.clientID = clientID
  // With a search marker, yjs starts an insert without attributes and every
  // delete from the marker with no formatting in effect, so what they do
  // depends on which markers the replica happens to hold; a delete can then
  // remove a closing format item and change the formatting of the text after
  // it (FINDINGS.md). Without markers yjs walks from the start of the text.
  doc.getText('text')._searchMarker = null
  return doc
}

function dumpValue(value) {
  if (value instanceof Y.Doc) return { $doc: value.guid }
  if (value instanceof Y.Text) return { $text: dumpText(value) }
  if (value instanceof Y.Map) {
    const out = {}
    for (const [key, item] of value) out[key] = dumpValue(item)
    return { $map: out }
  }
  if (value instanceof Y.Array) return { $array: value.toArray().map(dumpValue) }
  if (value instanceof Y.XmlElement) {
    return { $element: value.nodeName, attrs: value.getAttributes(), children: value.toArray().map(dumpValue) }
  }
  return value === undefined ? null : value
}
function dumpText(text) {
  return text.toDelta().map((op) => {
    const out = { insert: op.insert }
    if (op.attributes && Object.keys(op.attributes).length) out.attributes = op.attributes
    return out
  })
}
function dump(doc) {
  return {
    text: dumpText(doc.getText('text')),
    arr: doc.getArray('arr').toArray().map(dumpValue),
    map: dumpValue(doc.getMap('map')).$map,
    xml: doc.getXmlFragment('xml').toArray().map(dumpValue),
    subdocs: [...doc.getSubdocGuids()],
  }
}

function apply(doc, step) {
  const text = doc.getText('text'), arr = doc.getArray('arr'), map = doc.getMap('map'), xml = doc.getXmlFragment('xml')
  // With step.whole, text positions move off the middle of a surrogate pair:
  // a start back, an end forward.
  const units = step.whole ? text.toString() : ''
  const low = (p) => p > 0 && p < units.length && (units.charCodeAt(p) & 0xfc00) === 0xdc00
  const back = (p) => (low(p) ? p - 1 : p)
  const textRange = () => {
    const p = within(step.n, text.length)
    let end = p + Math.min(step.k, text.length - p)
    if (low(end)) end++
    return [back(p), end - back(p)]
  }
  switch (step.op) {
    case 'tins': text.insert(back(at(step.n, text.length)), step.s, step.attrs ?? undefined); break
    case 'tdel': if (text.length) { const [p, k] = textRange(); text.delete(p, k) } break
    case 'tfmt': if (text.length) { const [p, k] = textRange(); text.format(p, k, step.attrs) } break
    case 'temb': text.insertEmbed(at(step.n, text.length), step.val, step.attrs ?? undefined); break
    case 'ains': arr.insert(at(step.n, arr.length), step.vals); break
    case 'adel': if (arr.length) { const p = within(step.n, arr.length); arr.delete(p, Math.min(step.k, arr.length - p)) } break
    case 'mset': map.set(step.key, step.val); break
    case 'mdel': map.delete(step.key); break
    case 'msub': map.set(step.key, new Y.Doc()); break
    case 'mtext': map.set(step.key, new Y.Text(step.s)); break
    case 'amap': { const m = new Y.Map(); m.set(step.key, step.val); arr.insert(at(step.n, arr.length), [m]); break }
    case 'xel': {
      const el = new Y.XmlElement(step.tag)
      xml.insert(at(step.n, xml.length), [el])
      el.setAttribute(step.attr[0], step.attr[1])
      el.insert(0, [new Y.XmlText(step.text)])
      break
    }
    case 'xdel': if (xml.length) { const p = within(step.n, xml.length); xml.delete(p, Math.min(step.k, xml.length - p)) } break
    default: throw new Error(`unknown op ${step.op}`)
  }
}

const docs = new Map()
// What a replica holds for itself: its undo manager, the relative positions
// it made and the snapshots it took.
const own = new Map()

function local(doc) {
  const mine = own.get(doc)
  return {
    positions: mine.positions.map((pos) => {
      const abs = Y.createAbsolutePositionFromRelativePosition(pos, doc)
      const entry = { bytes: hex(Y.encodeRelativePosition(pos)), index: null }
      if (abs) { entry.index = abs.index; entry.assoc = abs.assoc }
      return entry
    }),
    snapshots: mine.snapshots.map((snap) => {
      const then = dump(Y.createDocFromSnapshot(doc, Y.decodeSnapshot(Y.encodeSnapshot(snap))))
      return { text: then.text, arr: then.arr, map: then.map }
    }),
  }
}

// step runs one step: an edit in a transaction, or one of the steps that
// are no edit.
function step(doc, s) {
  const mine = own.get(doc)
  switch (s.op) {
    case 'undo': mine.undo.undo(); break
    case 'redo': mine.undo.redo(); break
    case 'rpos': {
      const text = doc.getText('text')
      mine.positions.push(Y.createRelativePositionFromTypeIndex(text, at(s.n, text.length), s.k ? -1 : 0))
      break
    }
    case 'snap': if (!doc.gc) mine.snapshots.push(Y.snapshot(doc)); break
    default: doc.transact(() => apply(doc, s))
  }
}

function handle(request) {
  const doc = docs.get(request.id)
  switch (request.cmd) {
    case 'reset': docs.clear(); own.clear(); return {}
    case 'new': {
      const made = newDoc(request.client, request.gc)
      docs.set(request.id, made)
      // Every transaction of the replica's own is one step to undo; what
      // arrives from others (origin 'remote') is none.
      const roots = [made.getText('text'), made.getArray('arr'), made.getMap('map'), made.getXmlFragment('xml')]
      own.set(made, { undo: new Y.UndoManager(roots, { captureTimeout: -1 }), positions: [], snapshots: [] })
      return {}
    }
    case 'step': step(doc, request.step); return {}
    case 'sv': return { hex: hex(Y.encodeStateVector(doc)) }
    case 'diff': {
      const sv = unhex(request.sv)
      return { hex: hex(request.v === 2 ? Y.encodeStateAsUpdateV2(doc, sv) : Y.encodeStateAsUpdate(doc, sv)) }
    }
    case 'apply':
      if (request.v === 2) Y.applyUpdateV2(doc, unhex(request.update), 'remote')
      else Y.applyUpdate(doc, unhex(request.update), 'remote')
      return {}
    case 'anew': {
      const owner = new Y.Doc()
      owner.clientID = request.client
      const instance = new presence.Awareness(owner)
      // Its timer would keep this process alive after stdin ends.
      clearInterval(instance._checkInterval)
      docs.set(request.id, instance)
      return {}
    }
    case 'aset': doc.setLocalState(request.state ?? null); return {}
    case 'aenc': return { hex: hex(presence.encodeAwarenessUpdate(doc, [...doc.meta.keys()])) }
    case 'aapply': presence.applyAwarenessUpdate(doc, unhex(request.update), 'remote'); return {}
    case 'astates': return { dump: Object.fromEntries(doc.getStates()) }
    case 'resolve': return {
      dump: {
        indexes: request.positions.map((bytes) =>
          Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(unhex(bytes)), doc)?.index ?? null),
      },
    }
    case 'dump': return { dump: { ...dump(doc), $local: local(doc) } }
    case 'merge': {
      const updates = request.updates.map(unhex)
      return { hex: hex(request.v === 2 ? Y.mergeUpdatesV2(updates) : Y.mergeUpdates(updates)) }
    }
    default: throw new Error(`unknown command ${request.cmd}`)
  }
}

for await (const line of createInterface({ input: process.stdin })) {
  if (!line.trim()) continue
  let response
  try {
    response = handle(JSON.parse(line))
  } catch (error) {
    response = { error: String(error && error.stack || error) }
  }
  process.stdout.write(JSON.stringify(response) + '\n')
}
