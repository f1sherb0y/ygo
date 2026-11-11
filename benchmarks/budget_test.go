package benchmarks

import (
	"math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
)

// The budgets below are ceilings on what an editing session may cost, set
// at about twice what it cost when they were written (BENCHMARKS.md has
// the measurements). They are not benchmarks: they exist so that a change
// which makes every edit walk the document again, or keep something for
// every edit, fails a test and not a user. The session is generated, so
// they run everywhere, with go test ./benchmarks/.
//
// Time is the one budget a slow or busy machine can break without anything
// being wrong: YGO_BUDGET_TIME=off skips it, and under the race detector
// it is skipped by itself.
const (
	budgetEdits = 100_000

	// Wall time for the whole session on one core, typing side and
	// receiving side each. Measured: about 0.35 s and 0.25 s.
	budgetTypingTime    = 3 * time.Second
	budgetReceivingTime = 4 * time.Second

	// Heap allocations per edit, a transaction each, on the typing side
	// (with an update handler) and on the side applying its updates.
	budgetTypingAllocs    = 60
	budgetReceivingAllocs = 90 // measured: 28 and 46

	// Live heap of the finished document for each UTF-16 unit of text it
	// holds, deleted text counted in: the history is kept, as a server
	// keeps it.
	budgetHeapPerUnit = 200 // measured: 94

	// The stored document against the text it holds.
	budgetV1PerUnit = 6 // measured: 2.9
)

// session replays budgetEdits edits of generated typing into d: runs of
// characters, backspaces, selections deleted, the cursor jumping now and
// then. It returns how many UTF-16 units were ever inserted.
func session(d *ygo.Doc, text *ygo.Text) (inserted int) {
	rng := rand.New(rand.NewPCG(2026, 11))
	length, cursor := 0, 0
	for range budgetEdits {
		if rng.IntN(50) == 0 {
			cursor = rng.IntN(length + 1)
		}
		txn := d.WriteTxn()
		switch k := rng.IntN(10); {
		case k < 6 || length == 0:
			s := string(rune('a' + rng.IntN(26)))
			if rng.IntN(20) == 0 {
				s = "\n    return value // 注释\n"
			}
			_ = text.Insert(txn, uint64(cursor), s)
			n := len([]rune(s)) // BMP only: one rune is one unit
			length, cursor, inserted = length+n, cursor+n, inserted+n
		case k < 9 && cursor > 0:
			_ = text.Delete(txn, uint64(cursor-1), 1)
			length, cursor = length-1, cursor-1
		case cursor < length:
			n := min(1+rng.IntN(8), length-cursor)
			_ = text.Delete(txn, uint64(cursor), uint64(n))
			length -= n
		}
		txn.Commit()
	}
	return inserted
}

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestBudget_EditingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("budgets are not checked with -short")
	}
	timed := os.Getenv("YGO_BUDGET_TIME") != "off" && !raceEnabled

	before := liveHeap()
	var mallocs runtime.MemStats
	runtime.ReadMemStats(&mallocs)

	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	text := ygo.NewText(d, "text")
	var updates [][]byte
	d.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	start := time.Now()
	inserted := session(d, text)
	typing := time.Since(start)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	typingAllocs := float64(after.Mallocs-mallocs.Mallocs) / budgetEdits

	// The updates are the test's, not the document's: let them go before
	// the document is weighed.
	kept := updates
	updates = nil
	d.OnUpdate(func([]byte, any) {})
	heap := liveHeap() - before
	// kept is live until here; it was allocated after before was read, so
	// take it off.
	var keptBytes uint64
	for _, u := range kept {
		keptBytes += uint64(cap(u)) + 24
	}
	heap -= min(heap, keptBytes)

	state := ygo.EncodeStateAsUpdate(d)

	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	peerText := ygo.NewText(peer, "text")
	runtime.ReadMemStats(&mallocs)
	start = time.Now()
	for _, u := range kept {
		if err := ygo.ApplyUpdate(peer, u); err != nil {
			t.Fatal(err)
		}
	}
	receiving := time.Since(start)
	runtime.ReadMemStats(&after)
	receivingAllocs := float64(after.Mallocs-mallocs.Mallocs) / float64(len(kept))

	rtxn := d.ReadTxn()
	want := text.String()
	rtxn.Close()
	rtxn = peer.ReadTxn()
	got := peerText.String()
	rtxn.Close()
	if got != want {
		t.Fatal("the receiving document differs from the typing one")
	}
	runtime.KeepAlive(d)
	runtime.KeepAlive(peer)

	heapPerUnit := float64(heap) / float64(inserted)
	v1PerUnit := float64(len(state)) / float64(inserted)
	t.Logf("%d edits, %d units inserted, %d left; typing %v, %.1f allocs/edit; receiving %v, %.1f allocs/update; heap %.0f bytes/unit; V1 %.2f bytes/unit",
		budgetEdits, inserted, len([]rune(want)), typing.Round(time.Millisecond), typingAllocs,
		receiving.Round(time.Millisecond), receivingAllocs, heapPerUnit, v1PerUnit)

	if timed && typing > budgetTypingTime {
		t.Errorf("typing took %v, budget %v", typing, budgetTypingTime)
	}
	if timed && receiving > budgetReceivingTime {
		t.Errorf("receiving took %v, budget %v", receiving, budgetReceivingTime)
	}
	if typingAllocs > budgetTypingAllocs {
		t.Errorf("typing made %.1f allocations per edit, budget %d", typingAllocs, budgetTypingAllocs)
	}
	if receivingAllocs > budgetReceivingAllocs {
		t.Errorf("receiving made %.1f allocations per update, budget %d", receivingAllocs, budgetReceivingAllocs)
	}
	if heapPerUnit > budgetHeapPerUnit {
		t.Errorf("the document holds %.0f bytes of heap per unit of text, budget %d", heapPerUnit, budgetHeapPerUnit)
	}
	if v1PerUnit > budgetV1PerUnit {
		t.Errorf("the encoded document is %.2f bytes per unit of text, budget %d", v1PerUnit, budgetV1PerUnit)
	}
}
