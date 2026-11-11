// Holds yjs documents for the conformance tests. One JSON command per line on
// stdin, one JSON answer per line on stdout (see ../../driver.go).
import * as Y from 'yjs'
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

function handle(request) {
  const doc = docs.get(request.id)
  switch (request.cmd) {
    case 'reset': docs.clear(); return {}
    case 'new': docs.set(request.id, newDoc(request.client, request.gc)); return {}
    case 'step': doc.transact(() => apply(doc, request.step)); return {}
    case 'sv': return { hex: hex(Y.encodeStateVector(doc)) }
    case 'diff': {
      const sv = unhex(request.sv)
      return { hex: hex(request.v === 2 ? Y.encodeStateAsUpdateV2(doc, sv) : Y.encodeStateAsUpdate(doc, sv)) }
    }
    case 'apply':
      if (request.v === 2) Y.applyUpdateV2(doc, unhex(request.update))
      else Y.applyUpdate(doc, unhex(request.update))
      return {}
    case 'dump': return { dump: dump(doc) }
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
