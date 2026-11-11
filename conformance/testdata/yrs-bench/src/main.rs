// Times yrs on the B4 editing trace (testdata/b4-trace.json at the root of
// the repository): all edits in one transaction, and one transaction per
// edit with an update observer, as b4.mjs does for yjs and
// benchmarks/b4*_test.go for ygo.
//
//   cargo run --release -- ../../../testdata/b4-trace.json [rounds [per-edit|one]]
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Instant;
use yrs::{Doc, GetString, OffsetKind, Options, ReadTxn, StateVector, Text, Transact};

struct Edit {
    pos: u32,
    del: u32,
    ins: String,
}

fn run(edits: &[Edit], final_text: &str, per_edit: bool) -> (f64, usize) {
    let mut options = Options::with_client_id(yrs::block::ClientID::new(1));
    // The trace has JavaScript string indexes.
    options.offset_kind = OffsetKind::Utf16;
    let doc = Doc::with_options(options);
    let text = doc.get_or_insert_text("text");
    let bytes = Arc::new(AtomicUsize::new(0));
    if per_edit {
        let bytes = bytes.clone();
        doc.observe_update_v1("bench", move |_, e| {
            bytes.fetch_add(e.update.len(), Ordering::Relaxed);
        })
            .unwrap();
    }
    let start = Instant::now();
    if per_edit {
        for e in edits {
            let mut txn = doc.transact_mut();
            if e.del > 0 {
                text.remove_range(&mut txn, e.pos, e.del);
            }
            if !e.ins.is_empty() {
                text.insert(&mut txn, e.pos, &e.ins);
            }
        }
    } else {
        let mut txn = doc.transact_mut();
        for e in edits {
            if e.del > 0 {
                text.remove_range(&mut txn, e.pos, e.del);
            }
            if !e.ins.is_empty() {
                text.insert(&mut txn, e.pos, &e.ins);
            }
        }
    }
    let ms = start.elapsed().as_secs_f64() * 1000.0;
    let txn = doc.transact();
    assert!(text.get_string(&txn) == final_text, "final text mismatch");
    let size = txn.encode_state_as_update_v1(&StateVector::default()).len();
    (ms, size)
}

fn main() {
    let path = std::env::args().nth(1).expect("usage: ygo-bench-yrs b4-trace.json");
    let raw = std::fs::read_to_string(path).unwrap();
    let trace: serde_json::Value = serde_json::from_str(&raw).unwrap();
    let edits: Vec<Edit> = trace["edits"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| Edit {
            pos: e[0].as_u64().unwrap() as u32,
            del: e[1].as_u64().unwrap() as u32,
            ins: e.get(2).and_then(|s| s.as_str()).unwrap_or("").to_string(),
        })
        .collect();
    let final_text = trace["finalText"].as_str().unwrap();
    let rounds: usize = std::env::args().nth(2).map_or(3, |n| n.parse().unwrap());
    // The transaction per edit first: it is the one a server does.
    // The third argument picks one of the two: "per-edit" or "one".
    let only = std::env::args().nth(3);
    for per_edit in [true, false] {
        match only.as_deref() {
            Some("per-edit") if !per_edit => continue,
            Some("one") if per_edit => continue,
            _ => {}
        }
        let name = if per_edit { "a transaction per edit" } else { "one transaction" };
        for _ in 0..rounds {
            let (ms, size) = run(&edits, final_text, per_edit);
            println!("{}: {:.0} ms (V1 document {} bytes)", name, ms, size);
        }
    }
}
