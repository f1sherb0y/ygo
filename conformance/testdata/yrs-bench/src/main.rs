// Times yrs on the B4 editing trace (testdata/b4-trace.json at the root of
// the repository): all edits in one transaction, and one transaction per
// edit with an update observer, as b4.mjs does for yjs and
// benchmarks/b4*_test.go for ygo.
//
//   cargo run --release -- ../../../testdata/b4-trace.json [rounds [per-edit|one]]
//   cargo run --release -- synthetic
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

// synthetic times the shapes of ygo's B1.1, B1.3, B1.4 and B1.7 (6000 edits
// of one character in one transaction): the same workloads, with positions
// from another random number generator than Go's.
fn synthetic() {
    const N: u32 = 6000;
    let mut seed: u64 = 14;
    let mut next = move |below: u32| -> u32 {
        seed = seed.wrapping_mul(6364136223846793005).wrapping_add(1442695040888963407);
        ((seed >> 33) as u32) % below.max(1)
    };
    let mut time = |name: &str, edit: &mut dyn FnMut(&yrs::TextRef, &mut yrs::TransactionMut, u32)| {
        let mut best = f64::MAX;
        for _ in 0..30 {
            let mut options = Options::with_client_id(yrs::block::ClientID::new(1));
            options.offset_kind = OffsetKind::Utf16;
            let doc = Doc::with_options(options);
            let text = doc.get_or_insert_text("doc");
            let start = Instant::now();
            {
                let mut txn = doc.transact_mut();
                for j in 0..N {
                    edit(&text, &mut txn, j);
                }
            }
            best = best.min(start.elapsed().as_secs_f64() * 1000.0);
        }
        println!("{}: {:.2} ms (best of 30)", name, best);
    };
    time("B1.1 append text", &mut |t, txn, j| t.insert(txn, j, "a"));
    time("B1.3 prepend text", &mut |t, txn, _| t.insert(txn, 0, "a"));
    time("B1.4 insert at random positions", &mut |t, txn, j| t.insert(txn, next(j), "a"));
    time("B1.7 insert and delete at random positions", &mut |t, txn, _| {
        let length = t.len(txn);
        if next(10) < 3 && length > 0 {
            let del = (1 + next(3)).min(length);
            t.remove_range(txn, next(length - del + 1), del);
        } else {
            t.insert(txn, next(length), "a");
        }
    });
}

fn main() {
    if std::env::args().nth(1).as_deref() == Some("synthetic") {
        synthetic();
        return;
    }
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
