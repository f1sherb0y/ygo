# Findings

What the conformance tests in this directory turned up, in ygo and in the
implementations it was compared with. Reproducers are in
`testdata/findings/` and replayed by `TestFindings`.

Versions: ygo at this commit, yjs 13.6.33, yrs 0.25.0 and 0.28.1.

## ygo

### 1. `MergeUpdates` of no updates returned nil (fixed)

`MergeUpdates(nil)` and `MergeUpdatesV2(nil)` returned a nil slice, which
no reader, ygo's included, accepts as an update. yjs returns the empty
update (`00 00`, and its V2 form). ygo does now.

### 2. A Hocuspocus connection got the room's messages before its token (fixed; never released)

In the server's Hocuspocus mode, added on this branch, a connection joined
its room before its token was accepted, and the room's updates were sent to
it meanwhile. Found by the race detector on `server/stress_test.go`;
`TestHocuspocus_NoFanOutBeforeTheToken` holds it.

### 3. The server stored a row for every reconnect (fixed)

A client that has nothing new still answers the server's SyncStep1, with
the empty update. The server treated it as a change: broadcast to the
room, a row in the store, a call to `OnChange`. A backend that keeps a
row per update for replay got one for every connection anyone made. Found
by `testdata/e2e/backend.mjs`, where the row numbers after a restart were
one further than the edits.

### 4. A token could not decide the scope (added)

`Options.ReadOnly` sees the upgrade request, and a Hocuspocus provider
sends its token later, in a message. A document with editors and viewers
told apart by their tokens could not be served. `Options.OnAuthorize`
answers with the scope; with it came the ways for the server to answer a
connection, tell a document and close a connection
(`SendStateless`, `BroadcastStateless`, `Disconnect`), without which
a backend can neither confirm that an edit is stored nor end a revoked
session.

### 5. Speed

Not a difference in results, but what stood between ygo and a server that
commits every edit on its own. See BENCHMARKS.md: on the B4 editing trace a
transaction per edit took 15.1 s and takes 0.52 s.

## yrs 0.25.0 (fixed in 0.28.1)

Run `go test ./conformance/ -yrs-0.25` to see these fail.

### 6. A deletion is lost when it reaches past what the replica has

`yrs-0.25-lost-delete.json`. An update deletes clocks of a client of which
the replica holds only the first part, or nothing yet. yrs 0.25.0 applies
the part it can and should keep the rest until the items arrive. It keeps
the wrong range (from the start of the deleted block instead of from where
its own knowledge ends), and for a client it has never seen it keeps
nothing. When the items arrive later they stay alive: the replica shows
content every other replica has deleted, with the same state vector, so no
sync repairs it.

This needs nothing unusual: two updates of one client arriving in the other
order, as they do when a server relays them from two connections.

### 7. An index inside a surrogate pair corrupts the document

`yrs-0.25-surrogate-split-insert.json`, `-delete.json`. Inserting or
deleting at a UTF-16 index between the two halves of a character outside
the BMP (an emoji) leaves yrs 0.25.0 with block lengths that do not match
its content. Replicas diverge, and the state vector it reports no longer
describes what it holds. yjs splits the pair and replaces each half with
U+FFFD; ygo and yrs 0.28.1 do the same.

A browser client can send such an index: JavaScript strings are indexed in
UTF-16 units and an editor binding computes positions from them.

## Differences that are not defects

### 8. Where an insertion lands next to deleted content

An index next to deleted items names several places in the structure:
before the tombstones, after them, or between two. yjs and yrs do not pick
the same one for a local insertion. Both are valid; the document converges
either way. Two
replicas given the same edits by index, one yjs and one yrs, hold different
structures afterwards, which shows once concurrent edits arrive. ygo
follows yjs. This is why the tests hold yrs to convergence and not to
equality with yjs.

### 9. yjs formatting depends on its search markers

In yjs, what `insert` does with the formatting in force at the position
can depend on whether a search marker led there or the walk started at the
beginning: the two can stop on different sides of the format items at that
index. The scripts therefore give explicit attributes with
every rich-text insertion, and the yjs driver drops the markers of the root
text before each step.

### 10. Format cleanup can remove what a concurrent edit needed

After a remote transaction yjs removes format items that have become
redundant (`cleanupYTextAfterTransaction`). An item that is redundant now
can be the one that content arriving later was written against. ygo does
what yjs does, byte for byte; yrs cleans up as well, with small differences
in which items go. All three converge. The hazard is in the design, and is
recorded here because it looked like a lost deletion in yrs 0.28.1 until
the items were read one by one.

Cleanup runs for a text that was created as a text. ygo only knows that for
a root once the application has asked for it with `NewText`; a server that
applies updates to a document it never reads does no cleanup. The
conformance replicas create their roots first, as an application does.

## For a backend on yrs 0.25

Either of findings 6 and 7 moves a replica away from the others for good,
without an error. Upgrading yrs fixes both.

## What was run

On Windows 11, amd64, Go 1.27.3, Node 24.12:

- `TestDifferential` and `TestDelivery`: seeds 0 to 299 and 1000 to 1399
  of each of the ten profiles, every arrangement in README.md, with yjs
  13.6.33 and yrs 0.28.1.
- `TestFindings`: the three reproducers.
- The module's fuzz targets (13), 45 seconds each, and the three that
  apply updates and undo again after the speed changes, 40 seconds each.
- `edit_model_test.go`: 30,000 edits on a text and 20,000 on an array, a
  transaction each, against a slice, four seeds each.
- `go test -race ./...` on Linux (Go 1.26 in a container), with the stress
  test at `YGO_STRESS=2`, and the stress test alone under the race
  detector with 48 clients per document (192 connections, 240 edits each).
  With 200 clients per document the race detector's memory did not fit the
  container's 16 GB and the kernel ended the run; that size was run
  without the detector only.
- `server/stress_test.go`: 4 documents, 6 clients each, 120 edits per
  client, a third of the clients reconnecting halfway, one late joiner per
  document, over y-websocket and Hocuspocus frames. Also run with 960
  edits per client (`YGO_STRESS=8`), with 48 clients per document and 480
  edits each, and with 200 clients per document (800 connections, 96,000
  edits, each sent to 199 others), clients and server in one process.
- `testdata/e2e/backend.mjs`: 26 checks of an application backend on the
  server with provider 3.4.4 (see its head for the list).
- `benchmarks.TestBudget_EditingSession`: 100,000 generated edits within
  the ceilings set there.
- `testdata/e2e/hocuspocus-provider.mjs`: `@hocuspocus/provider` 4.8.0,
  3.4.4 and 2.15.3 against the ygo server, 48 checks.

None of this is a proof. See "What is not covered" in README.md.
