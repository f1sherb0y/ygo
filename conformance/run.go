package conformance

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
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
	return outcome, nil
}
