// Holds yrs documents for the conformance tests. One JSON command per line on
// stdin, one JSON answer per line on stdout (see ../driver.go).
use serde_json::{json, Map as JsonMap, Value};
use std::collections::HashMap;
use std::io::{BufRead, Write};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::Arc;
use yrs::types::text::YChange;
use yrs::types::Attrs;
use yrs::updates::decoder::Decode;
use yrs::updates::encoder::Encode;
use yrs::{
    Any, Array, ArrayRef, Doc, GetString, Map, MapPrelim, MapRef, OffsetKind, Options, Out,
    ReadTxn, StateVector, Text, TextPrelim, TextRef, Transact, TransactionMut, Update, Xml,
    XmlElementPrelim, XmlFragment, XmlFragmentRef, XmlOut, XmlTextPrelim,
};

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
fn unhex(text: &str) -> Vec<u8> {
    (0..text.len() / 2)
        .map(|i| u8::from_str_radix(&text[2 * i..2 * i + 2], 16).unwrap())
        .collect()
}
fn at(n: u64, length: u32) -> u32 {
    ((n * (length as u64 + 1)) >> 16) as u32
}
fn within(n: u64, length: u32) -> u32 {
    ((n * length as u64) >> 16) as u32
}

// The three places where yrs 0.25 and 0.28 differ: numbers and client IDs.
#[cfg(feature = "yrs-025")]
mod version {
    use super::*;
    pub fn number(n: f64) -> Any {
        Any::Number(n)
    }
    pub fn from_number(n: &f64) -> Value {
        json!(n)
    }
    pub fn client(id: u64) -> u64 {
        id
    }
}
#[cfg(not(feature = "yrs-025"))]
mod version {
    use super::*;
    pub fn number(n: f64) -> Any {
        // What JavaScript stores: one kind of number.
        Any::Number(yrs::Number::Float(n))
    }
    pub fn from_number(n: &yrs::Number) -> Value {
        match n {
            yrs::Number::Int(n) => json!(n),
            yrs::Number::Float(n) => json!(n),
        }
    }
    pub fn client(id: u64) -> yrs::block::ClientID {
        yrs::block::ClientID::new(id)
    }
}
use version::{client, from_number, number};

fn to_any(value: &Value) -> Any {
    match value {
        Value::Null => Any::Null,
        Value::Bool(b) => Any::Bool(*b),
        Value::Number(n) => number(n.as_f64().unwrap()),
        Value::String(s) => Any::String(Arc::from(s.as_str())),
        Value::Array(items) => Any::Array(items.iter().map(to_any).collect()),
        Value::Object(entries) => Any::Map(Arc::new(
            entries.iter().map(|(k, v)| (k.clone(), to_any(v))).collect(),
        )),
    }
}
fn from_any(value: &Any) -> Value {
    match value {
        Any::Null | Any::Undefined => Value::Null,
        Any::Bool(b) => json!(b),
        Any::Number(n) => from_number(n),
        #[cfg(feature = "yrs-025")]
        Any::BigInt(n) => json!(n),
        Any::String(s) => json!(s.as_ref()),
        Any::Buffer(b) => json!(hex(b)),
        Any::Array(items) => Value::Array(items.iter().map(from_any).collect()),
        Any::Map(entries) => {
            Value::Object(entries.iter().map(|(k, v)| (k.clone(), from_any(v))).collect())
        }
    }
}
fn to_attrs(value: &Value) -> Attrs {
    value
        .as_object()
        .unwrap()
        .iter()
        .map(|(k, v)| (Arc::from(k.as_str()), to_any(v)))
        .collect()
}

fn dump_text<T: ReadTxn, X: Text>(text: &X, txn: &T) -> Value {
    let mut out = Vec::new();
    for chunk in text.diff(txn, YChange::identity) {
        let mut entry = JsonMap::new();
        entry.insert("insert".into(), dump_out(&chunk.insert, txn));
        if let Some(attrs) = chunk.attributes {
            if !attrs.is_empty() {
                let attrs: JsonMap<String, Value> =
                    attrs.iter().map(|(k, v)| (k.to_string(), from_any(v))).collect();
                entry.insert("attributes".into(), Value::Object(attrs));
            }
        }
        out.push(Value::Object(entry));
    }
    Value::Array(out)
}
fn dump_xml<T: ReadTxn>(node: &XmlOut, txn: &T) -> Value {
    match node {
        XmlOut::Text(text) => json!({ "$text": dump_text(text, txn) }),
        XmlOut::Element(el) => {
            let attrs: JsonMap<String, Value> = el
                .attributes(txn)
                .map(|(k, v)| (k.to_string(), json!(v.to_string(txn))))
                .collect();
            let children: Vec<Value> = el.children(txn).map(|c| dump_xml(&c, txn)).collect();
            json!({ "$element": el.tag().as_ref(), "attrs": attrs, "children": children })
        }
        XmlOut::Fragment(fragment) => {
            Value::Array(fragment.children(txn).map(|c| dump_xml(&c, txn)).collect())
        }
    }
}
fn dump_map<T: ReadTxn>(map: &MapRef, txn: &T) -> Value {
    Value::Object(map.iter(txn).map(|(k, v)| (k.to_string(), dump_out(&v, txn))).collect())
}
fn dump_out<T: ReadTxn>(value: &Out, txn: &T) -> Value {
    match value {
        Out::Any(any) => from_any(any),
        Out::YText(text) => json!({ "$text": dump_text(text, txn) }),
        Out::YMap(map) => json!({ "$map": dump_map(map, txn) }),
        Out::YArray(array) => {
            json!({ "$array": array.iter(txn).map(|v| dump_out(&v, txn)).collect::<Vec<_>>() })
        }
        Out::YXmlElement(el) => dump_xml(&XmlOut::Element(el.clone()), txn),
        Out::YXmlText(text) => dump_xml(&XmlOut::Text(text.clone()), txn),
        Out::YXmlFragment(fragment) => dump_xml(&XmlOut::Fragment(fragment.clone()), txn),
        Out::YDoc(doc) => json!({ "$doc": doc.guid().to_string() }),
        _ => Value::Null,
    }
}

struct Replica {
    doc: Doc,
    text: TextRef,
    arr: ArrayRef,
    map: MapRef,
    xml: XmlFragmentRef,
    // The relative positions the replica made, as hex.
    positions: std::cell::RefCell<Vec<String>>,
}

fn new_replica(client_id: u64, gc: bool) -> Replica {
    let mut options = Options::with_client_id(client(client_id));
    options.offset_kind = OffsetKind::Utf16;
    options.skip_gc = !gc;
    let doc = Doc::with_options(options);
    Replica {
        text: doc.get_or_insert_text("text"),
        arr: doc.get_or_insert_array("arr"),
        map: doc.get_or_insert_map("map"),
        xml: doc.get_or_insert_xml_fragment("xml"),
        doc,
        positions: Default::default(),
    }
}

fn dump(replica: &Replica) -> Value {
    let txn = replica.doc.transact();
    json!({
        "text": dump_text(&replica.text, &txn),
        "arr": replica.arr.iter(&txn).map(|v| dump_out(&v, &txn)).collect::<Vec<_>>(),
        "map": dump_map(&replica.map, &txn),
        "xml": replica.xml.children(&txn).map(|c| dump_xml(&c, &txn)).collect::<Vec<_>>(),
        "subdocs": txn.subdoc_guids().map(|guid| guid.to_string()).collect::<Vec<_>>(),
    })
}

fn apply(replica: &Replica, txn: &mut TransactionMut, step: &Value) {
    let n = step["n"].as_u64().unwrap_or(0);
    let k = step["k"].as_u64().unwrap_or(0) as u32;
    let s = step["s"].as_str().unwrap_or("");
    let key = step["key"].as_str().unwrap_or("");
    let (text, arr, map, xml) = (&replica.text, &replica.arr, &replica.map, &replica.xml);
    // With "whole", text positions move off the middle of a surrogate pair:
    // a start back, an end forward.
    let units: Vec<u16> = if step["whole"] == true {
        text.get_string(txn).encode_utf16().collect()
    } else {
        Vec::new()
    };
    let low = |p: u32| p > 0 && (p as usize) < units.len() && units[p as usize] & 0xfc00 == 0xdc00;
    let back = |p: u32| if low(p) { p - 1 } else { p };
    let text_range = |length: u32| {
        let p = within(n, length);
        let mut end = p + k.min(length - p);
        if low(end) {
            end += 1;
        }
        (back(p), end - back(p))
    };
    match step["op"].as_str().unwrap() {
        "tins" => {
            let index = back(at(n, text.len(txn)));
            if step["attrs"].is_object() {
                text.insert_with_attributes(txn, index, s, to_attrs(&step["attrs"]))
            } else {
                text.insert(txn, index, s)
            }
        }
        "tdel" => {
            let length = text.len(txn);
            if length > 0 {
                let (p, k) = text_range(length);
                text.remove_range(txn, p, k)
            }
        }
        "tfmt" => {
            let length = text.len(txn);
            if length > 0 {
                let (p, k) = text_range(length);
                text.format(txn, p, k, to_attrs(&step["attrs"]))
            }
        }
        "temb" => {
            let index = at(n, text.len(txn));
            if step["attrs"].is_object() {
                text.insert_embed_with_attributes(txn, index, to_any(&step["val"]), to_attrs(&step["attrs"]));
            } else {
                text.insert_embed(txn, index, to_any(&step["val"]));
            }
        }
        "ains" => {
            let values: Vec<Any> = step["vals"].as_array().unwrap().iter().map(to_any).collect();
            arr.insert_range(txn, at(n, arr.len(txn)), values)
        }
        "adel" => {
            let length = arr.len(txn);
            if length > 0 {
                let p = within(n, length);
                arr.remove_range(txn, p, k.min(length - p))
            }
        }
        "mset" => {
            map.insert(txn, key, to_any(&step["val"]));
        }
        "mdel" => {
            map.remove(txn, key);
        }
        "msub" => {
            map.insert(txn, key, Doc::new());
        }
        "mtext" => {
            map.insert(txn, key, TextPrelim::new(s));
        }
        "amap" => {
            let index = at(n, arr.len(txn));
            arr.insert(txn, index, MapPrelim::from([(key.to_string(), to_any(&step["val"]))]));
        }
        "xel" => {
            let el = xml.insert(txn, at(n, xml.len(txn)), XmlElementPrelim::empty(step["tag"].as_str().unwrap()));
            let attr = step["attr"].as_array().unwrap();
            el.insert_attribute(txn, attr[0].as_str().unwrap(), attr[1].as_str().unwrap());
            el.insert(txn, 0, XmlTextPrelim::new(step["text"].as_str().unwrap()));
        }
        "xdel" => {
            let length = xml.len(txn);
            if length > 0 {
                let p = within(n, length);
                xml.remove_range(txn, p, k.min(length - p))
            }
        }
        "rpos" => positions::make(replica, txn, n, k),
        // Undo and snapshots are compared between ygo and yjs only; a yrs
        // replica lets these steps pass.
        "undo" | "redo" | "snap" => {}
        other => panic!("unknown op {other}"),
    }
}

// Relative positions (sticky indexes) in the root text.
#[cfg(not(feature = "yrs-025"))]
mod positions {
    use serde_json::{json, Value};
    use yrs::updates::decoder::Decode;
    use yrs::updates::encoder::Encode;
    use yrs::{Assoc, GetString, IndexedSequence, StickyIndex, Transact, TransactionMut};

    pub fn make(replica: &super::Replica, txn: &mut TransactionMut, n: u64, k: u32) {
        let length = replica.text.get_string(txn).encode_utf16().count() as u64;
        let index = (n * (length + 1) >> 16) as u32;
        let assoc = if k != 0 { Assoc::Before } else { Assoc::After };
        if let Some(position) = replica.text.sticky_index(txn, index, assoc) {
            replica.positions.borrow_mut().push(super::hex(&position.encode_v1()));
        }
    }

    pub fn made(replica: &super::Replica) -> Vec<Value> {
        replica.positions.borrow().iter().map(|bytes| json!({ "bytes": bytes })).collect()
    }

    pub fn resolve(replica: &super::Replica, request: &Value) -> Value {
        let txn = replica.doc.transact();
        let indexes: Vec<Value> = request["positions"]
            .as_array()
            .unwrap()
            .iter()
            .map(|bytes| match StickyIndex::decode_v1(&super::unhex(bytes.as_str().unwrap())) {
                Ok(position) => position.get_offset(&txn).map(|offset| json!(offset.index)).unwrap_or(Value::Null),
                Err(_) => json!("undecodable"),
            })
            .collect();
        json!({ "dump": { "indexes": indexes } })
    }
}

#[cfg(feature = "yrs-025")]
mod positions {
    use serde_json::{json, Value};
    pub fn make(_: &super::Replica, _: &mut yrs::TransactionMut, _: u64, _: u32) {}
    pub fn made(_: &super::Replica) -> Vec<Value> {
        Vec::new()
    }
    pub fn resolve(_: &super::Replica, _: &Value) -> Value {
        json!({ "error": "positions are not driven on yrs 0.25" })
    }
}

// Awareness instances, for the presence scripts.
#[cfg(not(feature = "yrs-025"))]
mod presence {
    use serde_json::{json, Value};
    use std::cell::RefCell;
    use std::collections::HashMap;
    use yrs::sync::awareness::AwarenessUpdate;
    use yrs::sync::Awareness;
    use yrs::updates::decoder::Decode;
    use yrs::updates::encoder::Encode;

    thread_local! {
        static INSTANCES: RefCell<HashMap<u64, Awareness>> = RefCell::new(HashMap::new());
        // Every client an instance has heard of, the removed ones included:
        // what "everything it knows" is encoded from.
        static KNOWN: RefCell<HashMap<u64, Vec<yrs::block::ClientID>>> = RefCell::new(HashMap::new());
    }

    pub fn handle(id: u64, request: &Value) -> Value {
        INSTANCES.with(|instances| {
            let mut instances = instances.borrow_mut();
            match request["cmd"].as_str().unwrap() {
                "anew" => {
                    let number = request["client"].as_u64().unwrap();
                    let client = super::version::client(number);
                    let mut awareness = Awareness::new(yrs::Doc::with_client_id(number));
                    // The empty state every y-protocols instance starts with.
                    awareness.set_local_state_raw("{}");
                    instances.insert(id, awareness);
                    KNOWN.with(|known| known.borrow_mut().insert(id, vec![client]));
                    json!({})
                }
                "aset" => {
                    let awareness = instances.get_mut(&id).unwrap();
                    if request["state"].is_null() {
                        awareness.clean_local_state();
                    } else {
                        awareness.set_local_state_raw(request["state"].to_string());
                    }
                    json!({})
                }
                "aenc" => {
                    let clients = KNOWN.with(|known| known.borrow()[&id].clone());
                    match instances[&id].update_with_clients(clients) {
                        Ok(update) => json!({ "hex": super::hex(&update.encode_v1()) }),
                        Err(error) => json!({ "error": format!("encode: {error}") }),
                    }
                }
                "aapply" => {
                    let bytes = super::unhex(request["update"].as_str().unwrap());
                    match AwarenessUpdate::decode_v1(&bytes) {
                        Err(error) => json!({ "error": format!("decode: {error}") }),
                        Ok(update) => {
                            KNOWN.with(|known| {
                                let mut known = known.borrow_mut();
                                let list = known.get_mut(&id).unwrap();
                                for client in update.clients.keys() {
                                    if !list.contains(client) {
                                        list.push(*client);
                                    }
                                }
                            });
                            match instances.get_mut(&id).unwrap().apply_update(update) {
                                Err(error) => json!({ "error": format!("apply: {error}") }),
                                Ok(()) => json!({}),
                            }
                        }
                    }
                }
                _ => {
                    let mut states = serde_json::Map::new();
                    for (client, state) in instances[&id].iter() {
                        if let Some(data) = state.data {
                            states.insert(client.to_string(), serde_json::from_str(&data).unwrap());
                        }
                    }
                    json!({ "dump": states })
                }
            }
        })
    }
}

#[cfg(feature = "yrs-025")]
mod presence {
    pub fn handle(_: u64, _: &serde_json::Value) -> serde_json::Value {
        serde_json::json!({ "error": "presence is not driven on yrs 0.25" })
    }
}

fn handle(replicas: &mut HashMap<u64, Replica>, request: &Value) -> Value {
    let id = request["id"].as_u64().unwrap_or(0);
    let v2 = request["v"] == 2;
    match request["cmd"].as_str().unwrap() {
        "reset" => {
            replicas.clear();
            json!({})
        }
        "new" => {
            let gc = request["gc"].as_bool().unwrap_or(true);
            replicas.insert(id, new_replica(request["client"].as_u64().unwrap(), gc));
            json!({})
        }
        "step" => {
            let replica = &replicas[&id];
            let mut txn = replica.doc.transact_mut();
            apply(replica, &mut txn, &request["step"]);
            json!({})
        }
        "sv" => json!({ "hex": hex(&replicas[&id].doc.transact().state_vector().encode_v1()) }),
        "diff" => {
            let sv = StateVector::decode_v1(&unhex(request["sv"].as_str().unwrap())).unwrap();
            let txn = replicas[&id].doc.transact();
            let diff = if v2 { txn.encode_diff_v2(&sv) } else { txn.encode_diff_v1(&sv) };
            json!({ "hex": hex(&diff) })
        }
        "apply" => {
            let bytes = unhex(request["update"].as_str().unwrap());
            let update = if v2 { Update::decode_v2(&bytes) } else { Update::decode_v1(&bytes) };
            match update {
                Err(error) => json!({ "error": format!("decode: {error}") }),
                Ok(update) => match replicas[&id].doc.transact_mut().apply_update(update) {
                    Err(error) => json!({ "error": format!("apply: {error}") }),
                    Ok(()) => json!({}),
                },
            }
        }
        "dump" => {
            let mut out = dump(&replicas[&id]);
            out["$local"] = json!({ "positions": positions::made(&replicas[&id]) });
            json!({ "dump": out })
        }
        "resolve" => positions::resolve(&replicas[&id], request),
        "anew" | "aset" | "aenc" | "aapply" | "astates" => presence::handle(id, request),
        "merge" => {
            let updates: Vec<Vec<u8>> =
                request["updates"].as_array().unwrap().iter().map(|u| unhex(u.as_str().unwrap())).collect();
            let merged = if v2 { yrs::merge_updates_v2(&updates) } else { yrs::merge_updates_v1(&updates) };
            match merged {
                Ok(merged) => json!({ "hex": hex(&merged) }),
                Err(error) => json!({ "error": format!("merge: {error}") }),
            }
        }
        other => json!({ "error": format!("unknown command {other}") }),
    }
}

fn main() {
    std::panic::set_hook(Box::new(|_| {}));
    let stdin = std::io::stdin();
    let mut stdout = std::io::stdout();
    let mut replicas: HashMap<u64, Replica> = HashMap::new();
    for line in stdin.lock().lines() {
        let line = line.unwrap();
        if line.trim().is_empty() {
            continue;
        }
        let request: Value = serde_json::from_str(&line).unwrap();
        let response = catch_unwind(AssertUnwindSafe(|| handle(&mut replicas, &request)))
        .unwrap_or_else(|panic| {
            let message = panic
                .downcast_ref::<String>()
                .cloned()
                .or_else(|| panic.downcast_ref::<&str>().map(|s| s.to_string()))
                .unwrap_or_else(|| "panic".into());
            json!({ "error": format!("panic: {message}") })
        });
        writeln!(stdout, "{response}").unwrap();
        stdout.flush().unwrap();
    }
}
