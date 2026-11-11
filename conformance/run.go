package conformance

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Outcome is the content every replica ended with, once all of them agree.
type Outcome struct {
	Content any
	// StateVector is the clock of every client after the final sync.
	StateVector map[uint64]uint64
	// Local is what each replica holds for itself, replica by replica:
	// the positions it made, resolved now, and the snapshots it took,
	// restored now. Implementations that do not report it leave nil.
	Local []any
}

// relabel replaces the GUIDs of subdocuments, which every run makes up
// anew, by labels in the order a walk of the content meets them, so that
// two runs of one script compare equal when they hold subdocuments in the
// same places. Replicas of one run are compared before this, GUIDs and
// all.
func relabel(content any) any {
	labels := map[string]string{}
	label := func(guid string) string {
		if _, ok := labels[guid]; !ok {
			labels[guid] = fmt.Sprintf("subdoc-%d", len(labels)+1)
		}
		return labels[guid]
	}
	var walk func(v any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := make(map[string]any, len(v))
			for _, k := range keys {
				if guid, ok := v[k].(string); ok && k == "$doc" {
					out[k] = label(guid)
				} else if k != "subdocs" {
					out[k] = walk(v[k])
				}
			}
			// The set of subdocuments last: the ones still referenced have
			// their labels by now.
			if set, ok := v["subdocs"].([]any); ok {
				named := make([]string, 0, len(set))
				for _, guid := range set {
					if guid, ok := guid.(string); ok {
						named = append(named, label(guid))
					}
				}
				sort.Strings(named)
				list := make([]any, len(named))
				for i, name := range named {
					list[i] = name
				}
				out["subdocs"] = list
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i := range v {
				out[i] = walk(v[i])
			}
			return out
		}
		return v
	}
	return walk(content)
}

// sortSubdocs puts the set of subdocuments of a dump in order.
func sortSubdocs(d map[string]any) {
	if set, ok := d["subdocs"].([]any); ok {
		sort.Slice(set, func(i, j int) bool { return fmt.Sprint(set[i]) < fmt.Sprint(set[j]) })
	}
}

// localKey is the key of a dump under which a replica reports what it
// holds for itself.
const localKey = "$local"

// normal brings a dump through JSON, so that numbers and containers compare
// the same whichever implementation produced them.
func normal(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return joinRuns(out)
}

// joinRuns joins neighbouring text runs with the same attributes. A delta
// starts a new run at every format item, also at one that changes nothing,
// and which of those a replica still holds depends on the cleanups it ran.
func joinRuns(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k := range v {
			v[k] = joinRuns(v[k])
		}
	case []any:
		out := v[:0]
		for _, item := range v {
			item = joinRuns(item)
			if run, ok := item.(map[string]any); ok && len(out) > 0 {
				if last, ok := out[len(out)-1].(map[string]any); ok {
					a, isA := last["insert"].(string)
					b, isB := run["insert"].(string)
					_, hasA := last["insert"]
					if isA && isB && hasA && len(last) == len(run) && reflect.DeepEqual(last["attributes"], run["attributes"]) {
						last["insert"] = a + b
						continue
					}
				}
			}
			out = append(out, item)
		}
		return out
	}
	return v
}

// decodeStateVector reads a state vector: implementations write the clients
// in different orders.
func decodeStateVector(raw []byte) map[uint64]uint64 {
	next := func() uint64 {
		v, n := binary.Uvarint(raw)
		if n <= 0 {
			raw = nil
			return 0
		}
		raw = raw[n:]
		return v
	}
	out := map[uint64]uint64{}
	for range next() {
		client := next()
		out[client] = next()
	}
	return out
}

func syncReplicas(from, to Replica, version int) error {
	sv, err := to.StateVector()
	if err != nil {
		return err
	}
	diff, err := from.Diff(sv, version)
	if err != nil {
		return err
	}
	return to.Apply(diff, version)
}

// Run runs a script with replica i held by impls[i%len(impls)], syncs all
// replicas with each other and checks that they hold the same content.
func Run(script Script, impls ...Implementation) (Outcome, error) {
	replicas := make([]Replica, script.Replicas)
	name := func(i int) string { return fmt.Sprintf("replica %d (%s)", i, impls[i%len(impls)].Name()) }
	for i := range replicas {
		var err error
		if replicas[i], err = impls[i%len(impls)].New(uint64(i+1), script.GC); err != nil {
			return Outcome{}, fmt.Errorf("%s: %w", name(i), err)
		}
	}
	for n, step := range script.Steps {
		if step.Op == "sync" {
			if err := syncReplicas(replicas[step.From], replicas[step.To], step.V); err != nil {
				return Outcome{}, fmt.Errorf("step %d: sync %s -> %s: %w", n, name(step.From), name(step.To), err)
			}
		} else if err := replicas[step.R].Step(step); err != nil {
			return Outcome{}, fmt.Errorf("step %d: %s on %s: %w", n, step.Op, name(step.R), err)
		}
	}
	// Twice round: the first pass can leave an early replica without what a
	// later one only learned during it.
	for pass := range 2 {
		for i := range replicas {
			for j := range replicas {
				if i == j {
					continue
				}
				if err := syncReplicas(replicas[i], replicas[j], 1+(i+j+pass)%2); err != nil {
					return Outcome{}, fmt.Errorf("final sync %s -> %s: %w", name(i), name(j), err)
				}
			}
		}
	}
	var outcome Outcome
	for i, r := range replicas {
		d, err := r.Dump()
		if err != nil {
			return Outcome{}, fmt.Errorf("%s: %w", name(i), err)
		}
		sv, err := r.StateVector()
		if err != nil {
			return Outcome{}, fmt.Errorf("%s: %w", name(i), err)
		}
		local := d[localKey]
		delete(d, localKey)
		sortSubdocs(d)
		outcome.Local = append(outcome.Local, normal(local))
		content, clocks := normal(d), decodeStateVector(sv)
		if i == 0 {
			outcome.Content, outcome.StateVector = content, clocks
			continue
		}
		if !reflect.DeepEqual(content, outcome.Content) {
			a, _ := json.Marshal(outcome.Content)
			b, _ := json.Marshal(content)
			return Outcome{}, fmt.Errorf("%s and %s hold different content after syncing:\n    %s\n    %s", name(0), name(i), a, b)
		}
		if !reflect.DeepEqual(clocks, outcome.StateVector) {
			return Outcome{}, fmt.Errorf("%s and %s have different state vectors after syncing: %v, %v", name(0), name(i), outcome.StateVector, clocks)
		}
	}
	// Every position any replica made, resolved by every replica: they hold
	// the same document now, so each position is at one index for all of
	// them, whoever made it and whoever reads it.
	var positions [][]byte
	for _, local := range outcome.Local {
		mine, _ := local.(map[string]any)
		made, _ := mine["positions"].([]any)
		for _, entry := range made {
			entry, _ := entry.(map[string]any)
			if text, ok := entry["bytes"].(string); ok {
				raw, err := hex.DecodeString(text)
				if err != nil {
					return Outcome{}, err
				}
				positions = append(positions, raw)
			}
		}
	}
	// Not after an undo: the replica that undid a deletion follows the
	// deleted item to its restored copy, which only it knows of, and the
	// others resolve to where the deleted item was. yjs does the same
	// (FINDINGS.md).
	undid := false
	for _, step := range script.Steps {
		undid = undid || step.Op == "undo" || step.Op == "redo"
	}
	if len(positions) > 0 && !undid {
		var first []any
		firstName := ""
		for i, r := range replicas {
			resolver, ok := r.(Resolver)
			if !ok {
				continue
			}
			got, err := resolver.Resolve(positions)
			if err != nil {
				return Outcome{}, fmt.Errorf("%s resolving positions: %w", name(i), err)
			}
			got, _ = normal(got).([]any)
			if firstName == "" {
				first, firstName = got, name(i)
			} else if !reflect.DeepEqual(got, first) {
				return Outcome{}, fmt.Errorf("%s and %s resolve the same relative positions to different indexes: %v, %v", firstName, name(i), first, got)
			}
		}
	}
	outcome.Content = relabel(outcome.Content)
	for i := range outcome.Local {
		outcome.Local[i] = relabel(outcome.Local[i])
	}
	return outcome, nil
}
