package conformance

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

// transactionUpdate is what one transaction emitted, in both formats.
type transactionUpdate struct{ v1, v2 []byte }

// capture runs a script on ygo replicas and returns every update their
// transactions emitted, local edits and applied syncs alike, with the content
// they converge on.
func capture(script Script) ([]transactionUpdate, Outcome, error) {
	var updates []transactionUpdate
	recording := recordingYgo{func(d *ygo.Doc) {
		// The V2 handler runs right after the V1 handler of the same
		// transaction, so the two fill one entry.
		d.OnUpdate(func(update []byte, _ any) {
			updates = append(updates, transactionUpdate{v1: append([]byte(nil), update...)})
		})
		d.OnUpdateV2(func(update []byte, _ any) {
			updates[len(updates)-1].v2 = append([]byte(nil), update...)
		})
	}}
	outcome, err := Run(script, recording)
	return updates, outcome, err
}

type recordingYgo struct{ watch func(*ygo.Doc) }

func (recordingYgo) Name() string { return "ygo" }

func (r recordingYgo) New(clientID uint64, gc bool) (Replica, error) {
	replica, err := Ygo.New(clientID, gc)
	r.watch(replica.(*ygoReplica).doc)
	return replica, err
}

func (recordingYgo) Merge(updates [][]byte, version int) ([]byte, error) {
	return Ygo.Merge(updates, version)
}

// checkDelivery gives every implementation the updates of a ygo run in a
// random order, some of them twice, in alternating formats, and then merged
// into one update by each implementation. Whatever the route, the content
// must be what the replicas that made the updates hold.
func checkDelivery(script Script, seed uint64, impls []Implementation) string {
	updates, want, err := capture(script)
	if err != nil {
		return "ygo: " + err.Error()
	}
	for _, u := range updates {
		if u.v1 == nil || u.v2 == nil {
			return "a transaction emitted only one update format"
		}
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	order := rng.Perm(len(updates))
	for range len(updates) / 3 {
		order = append(order, rng.IntN(len(updates)))
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	list := func(version int) [][]byte {
		out := make([][]byte, len(order))
		for n, i := range order {
			out[n] = updates[i].v1
			if version == 2 {
				out[n] = updates[i].v2
			}
		}
		return out
	}
	content := func(r Replica) (any, error) {
		d, err := r.Dump()
		delete(d, localKey) // delivery is about the document
		return relabel(normal(d)), err
	}
	differs := func(what string, got any) string {
		a, _ := json.Marshal(want.Content)
		b, _ := json.Marshal(got)
		return fmt.Sprintf("%s differs from what the replicas hold:\n    %s\n    %s", what, a, b)
	}
	// deliver gives a new replica the updates in the given sequence, in
	// alternating formats.
	deliver := func(impl Implementation, sequence []int) (Replica, error) {
		replica, err := impl.New(999, script.GC)
		if err != nil {
			return nil, err
		}
		for n, i := range sequence {
			version := 1 + n%2
			update := updates[i].v1
			if version == 2 {
				update = updates[i].v2
			}
			if err := replica.Apply(update, version); err != nil {
				return nil, fmt.Errorf("applying update %d (v%d): %w", i, version, err)
			}
		}
		return replica, nil
	}
	for _, impl := range impls {
		if d, ok := impl.(*Driver); ok {
			d.Reset()
		}
		if impl == Ygo {
			continue
		}
		sequence := order
		// A replica that receives formatted text cleans up format items
		// that have become redundant, and which ones those are depends on
		// what it has received so far. So the replicas that made the
		// updates are not the measure here: ygo, given the same updates
		// in the same order, is.
		mineReplica, err := deliver(Ygo, sequence)
		if err != nil {
			return "ygo: " + err.Error()
		}
		theirReplica, err := deliver(impl, sequence)
		if err != nil {
			return impl.Name() + ": " + err.Error()
		}
		compare := func() (string, error) {
			mine, err := content(mineReplica)
			if err != nil {
				return "", err
			}
			theirs, err := content(theirReplica)
			if err != nil || reflect.DeepEqual(mine, theirs) {
				return "", err
			}
			a, _ := json.Marshal(mine)
			b, _ := json.Marshal(theirs)
			return fmt.Sprintf(":\n    %s\n    %s", a, b), nil
		}
		difference, err := compare()
		if err != nil {
			return impl.Name() + ": " + err.Error()
		}
		if difference != "" && impl.Name() == "yrs" {
			// yrs cleans up other format items than yjs and ygo do
			// (FINDINGS.md), so right after delivery the two may show
			// other formatting. What they must do is agree once each has
			// what the other deleted.
			for _, pair := range [][2]Replica{{mineReplica, theirReplica}, {theirReplica, mineReplica}, {mineReplica, theirReplica}} {
				if err := syncReplicas(pair[0], pair[1], 1); err != nil {
					return "syncing ygo and yrs after delivery: " + err.Error()
				}
			}
			if difference, err = compare(); err != nil {
				return impl.Name() + ": " + err.Error()
			}
			if difference != "" {
				return "ygo and yrs, given the same updates in the same order and then synced with each other, hold different content" + difference
			}
		} else if difference != "" {
			return fmt.Sprintf("ygo and %s, given the same updates in the same order, hold different content%s", impl.Name(), difference)
		}
	}
	if len(impls) == 1 {
		// Without a reference, at least out of order must not matter
		// where no formatting is involved.
		if _, err := deliver(Ygo, order); err != nil {
			return "ygo: " + err.Error()
		}
	}
	// Merged by each, read by each.
	for version := 1; version <= 2; version++ {
		for _, merger := range impls {
			merged, err := merger.Merge(list(version), version)
			if err != nil {
				return fmt.Sprintf("%s: merging v%d updates: %v", merger.Name(), version, err)
			}
			for _, reader := range impls {
				replica, err := reader.New(998, script.GC)
				if err == nil {
					err = replica.Apply(merged, version)
				}
				if err != nil {
					return fmt.Sprintf("%s reading the v%d update %s merged: %v", reader.Name(), version, merger.Name(), err)
				}
				got, err := content(replica)
				if err != nil {
					return reader.Name() + ": " + err.Error()
				}
				if !reflect.DeepEqual(got, want.Content) {
					return differs(fmt.Sprintf("%s reading the v%d update %s merged", reader.Name(), version, merger.Name()), got)
				}
			}
		}
	}
	return ""
}

// TestDelivery checks out-of-order, duplicated and merged updates.
func TestDelivery(t *testing.T) {
	yjs, yrs := references(t)
	impls := []Implementation{Ygo}
	if yjs != nil {
		impls = append(impls, yjs)
	}
	if yrs != nil {
		impls = append(impls, yrs)
	}
	for _, c := range profiles {
		t.Run(c.name, func(t *testing.T) {
			failed := 0
			for i := range *seeds {
				seed := *firstRun + uint64(i)
				script := Generate(seed, c.replicas, c.steps, c.profile)
				if checkDelivery(script, seed, impls) == "" {
					continue
				}
				// Shrink while it still fails.
				for size := len(script.Steps) / 2; size >= 1; size /= 2 {
					for start := 0; start+size <= len(script.Steps); {
						smaller := script
						smaller.Steps = append(append([]Step{}, script.Steps[:start]...), script.Steps[start+size:]...)
						if checkDelivery(smaller, seed, impls) != "" {
							script = smaller
						} else {
							start += size
						}
					}
				}
				raw, _ := json.Marshal(script)
				if *failures != "" {
					_ = os.MkdirAll(*failures, 0o755)
					_ = os.WriteFile(filepath.Join(*failures, fmt.Sprintf("delivery-%s-%d.json", c.name, seed)), raw, 0o644)
				}
				t.Errorf("seed %d, shrunk to %d steps: %s\n  %s", seed, len(script.Steps), checkDelivery(script, seed, impls), raw)
				if failed++; failed >= 3 {
					t.Fatalf("stopping after %d failing seeds", failed)
				}
			}
		})
	}
}
