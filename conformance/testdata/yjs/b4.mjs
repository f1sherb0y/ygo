// Times yjs on the B4 editing trace (testdata/b4-trace.json), for the
// comparison in BENCHMARKS.md: all edits in one transaction, and one
// transaction per edit with an update handler.
import * as Y from 'yjs'
import { readFileSync } from 'node:fs'

const { edits, finalText } = JSON.parse(readFileSync(new URL('../../../testdata/b4-trace.json', import.meta.url)))

function run (perEdit) {
  const doc = new Y.Doc()
  doc.clientID = 1
  const text = doc.getText('text')
  let bytes = 0
  if (perEdit) doc.on('update', (u) => { bytes += u.length })
  const apply = ([pos, del, ins]) => {
    if (del > 0) text.delete(pos, del)
    if (ins) text.insert(pos, ins)
  }
  const start = performance.now()
  if (perEdit) for (const e of edits) doc.transact(() => apply(e))
  else doc.transact(() => { for (const e of edits) apply(e) })
  const ms = performance.now() - start
  if (text.toString() !== finalText) throw new Error('final text mismatch')
  return ms
}

for (const perEdit of [false, true]) {
  const times = [run(perEdit), run(perEdit), run(perEdit)].map((ms) => ms.toFixed(0))
  console.log(`${perEdit ? 'a transaction per edit' : 'one transaction'}: ${times.join(', ')} ms`)
}
