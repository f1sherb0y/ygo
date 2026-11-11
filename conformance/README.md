# Conformance tests

The fixtures in `testdata/` at the root of the repository compare ygo with
bytes yjs wrote in advance. The tests here compare it with running
implementations on editing scripts nobody wrote by hand: `yjs` 13.6.33 and
`yrs` 0.28.1 (and, on request, `yrs` 0.25.0).

What they found is in [FINDINGS.md](FINDINGS.md).

## Running

```sh
go test ./conformance/                    # 200 scripts per profile
go test ./conformance/ -seeds 2000        # more of them
go test ./conformance/ -seed 5000         # other ones: seeds 5000 to 5199
go test ./conformance/ -failures out/     # write shrunk failing scripts to out/
go test ./conformance/ -run TestScript -script out/text-17.json   # replay one
go test ./conformance/ -yrs-0.25          # against yrs 0.25.0; fails, see FINDINGS.md
```

Without a reference implementation installed the tests still run and check
ygo against itself: several replicas, V1 and V2, every delivery order. To
install the references:

```sh
cd conformance/testdata/yjs && npm ci                          # needs node
cd conformance/testdata/yrs && cargo build --release           # needs cargo
cd conformance/testdata/yrs-0.25 && cargo build --release      # only for -yrs-0.25
```

With `YGO_CONFORMANCE_REQUIRE=1` in the environment the tests fail when
either is missing, as CI has it.

Each is a small program that reads commands on stdin, one JSON object per
line, and answers on stdout (`driver.go` has the commands). The Go side
holds no state of theirs.

## What is compared

A script (`script.go`) is a list of steps for a number of replicas: an edit
on one replica, or a sync from one replica to another. Positions are
fractions of the current length, so the same script is valid whatever the
replicas hold. A seed and a profile decide the script; the same seed gives
the same script on every machine.

**`TestDifferential`** runs every script in these arrangements and compares
the outcome of each with ygo's:

| Arrangement | Compared |
|---|---|
| ygo alone, updates as V1 | the reference for the rest |
| ygo alone, updates as V2 | content and state vectors |
| yjs alone | content and state vectors, replica by replica |
| ygo and yjs replicas mixed, V1 and V2 | content and state vectors, replica by replica |
| yrs alone | that its replicas converge once all are synced |
| ygo and yrs mixed, yjs and yrs mixed, all three mixed, V1 and V2 | that all replicas converge to one document |

"Content" is the text with its formatting as a delta, the array, the map and
the XML fragment of the document, nested types included, read through each
implementation's own API. After every script all replicas are synced with
each other and must hold the same document with the same state vector.

yrs is held to convergence, not to equality with yjs: next to deleted
content it does not place a local insertion where yjs does (FINDINGS.md, 6), so two runs of the same script that never exchange updates
need not agree. Mixed with the others it must still converge with them.

The profiles:

| Profile | Replicas | Steps | Edits |
|---|---|---|---|
| text | 3 | 60 | insert and delete in a text |
| text-astral | 3 | 60 | the same with characters outside the BMP; indexes may fall inside a surrogate pair |
| text-astral-whole | 3 | 60 | the same, indexes only between characters |
| rich-text | 3 | 60 | text with formatting ranges and embeds |
| array | 3 | 60 | insert, delete, values of every Any kind |
| map | 3 | 40 | set and delete keys |
| nested | 3 | 60 | arrays and maps inside each other, created and deleted |
| xml | 3 | 40 | elements, attributes, text nodes |
| everything | 4 | 120 | all of the above in one document |
| everything-long | 6 | 400 | the same, longer |

**`TestDelivery`** takes the updates one replica produced and gives them to
a fresh replica of each implementation in ways a network does: in order,
shuffled (so that updates wait for the ones they depend on), each one twice,
alternating V1 and V2, and merged into one update with `mergeUpdates`
first, by each implementation, the result read by each other one.

**`TestFindings`** replays the scripts in `testdata/findings/`: ygo and yjs
must agree on each; the implementation a finding names is expected to fail
it until the version noted there.

A failing script is shrunk before it is reported: steps are removed while
it still fails. With `-failures` the shrunk script is written out as JSON
and `TestScript -script` replays it.

## What is not covered

- Undo, snapshots, relative positions, subdocuments and awareness are not in
  the scripts. They have fixtures against yjs in the root package.
- XML is compared between ygo and yjs only up to what the script edits
  (elements, attributes, text); `XmlHook` is not generated.
- Moved ranges (yrs `move`) are not generated: yjs 13 does not have them.
- Scripts are sequential: one step at a time, concurrency is edits made
  before a sync. Concurrent goroutines on one document and on one server are
  the business of `go test -race` and `server/stress_test.go`.
- Two hundred scripts per profile is a sample. The seeds run so far are in
  the commit messages and in FINDINGS.md; passing them shows no difference
  on those scripts and nothing beyond.
