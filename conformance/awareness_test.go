package conformance

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

// presenceStep is one step of a presence script: client R sets or removes
// its state, or From tells To everything it knows.
type presenceStep struct {
	Op       string
	R        int
	State    any
	From, To int
}

func presenceScript(seed uint64, clients, steps int) []presenceStep {
	rng := rand.New(rand.NewPCG(seed, 0xa3a7))
	var script []presenceStep
	// Everyone says who they are first, as a client does when it connects.
	// Until it has, there is nothing to compare: a y-protocols instance
	// starts with an empty state that its peers never take (FINDINGS.md,
	// 12), the others start with none.
	for client := range clients {
		script = append(script, presenceStep{Op: "set", R: client, State: map[string]any{"user": map[string]any{"name": pick(rng, words)}}})
	}
	for range steps {
		switch k := rng.IntN(10); {
		case k < 4:
			from := rng.IntN(clients)
			script = append(script, presenceStep{Op: "sync", From: from, To: (from + 1 + rng.IntN(clients-1)) % clients})
		case k < 9:
			state := map[string]any{"user": map[string]any{"name": pick(rng, words), "color": pick(rng, []string{"#f00", "#0f0"})}}
			if rng.IntN(2) == 0 {
				state["cursor"] = map[string]any{"anchor": float64(rng.IntN(100)), "head": float64(rng.IntN(100))}
			}
			if rng.IntN(5) == 0 {
				state["extra"] = randomValue(rng, 0)
			}
			script = append(script, presenceStep{Op: "set", R: rng.IntN(clients), State: state})
		default:
			// Leaves; a later "set" of the same client is its coming back.
			script = append(script, presenceStep{Op: "set", R: rng.IntN(clients)})
		}
	}
	return script
}

// runPresence runs a script with client i held by impls[i%len(impls)],
// lets everyone tell everyone, and returns the states they then agree on.
func runPresence(script []presenceStep, clients int, impls ...PresenceImplementation) (map[string]any, error) {
	instances := make([]Presence, clients)
	name := func(i int) string { return fmt.Sprintf("client %d (%s)", i+1, impls[i%len(impls)].Name()) }
	for i := range instances {
		var err error
		if instances[i], err = impls[i%len(impls)].NewPresence(uint64(i + 1)); err != nil {
			return nil, fmt.Errorf("%s: %w", name(i), err)
		}
	}
	tell := func(from, to int) error {
		update, err := instances[from].Encode()
		if err != nil {
			return fmt.Errorf("%s encoding: %w", name(from), err)
		}
		if err := instances[to].Apply(update); err != nil {
			return fmt.Errorf("%s applying what %s knows (% x): %w", name(to), name(from), update, err)
		}
		return nil
	}
	for n, step := range script {
		var err error
		if step.Op == "sync" {
			err = tell(step.From, step.To)
		} else if err = instances[step.R].Set(step.State); err != nil {
			err = fmt.Errorf("%s: %w", name(step.R), err)
		}
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", n, err)
		}
	}
	for range 2 {
		for i := range instances {
			for j := range instances {
				if i != j {
					if err := tell(i, j); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	var agreed map[string]any
	for i, instance := range instances {
		states, err := instance.States()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name(i), err)
		}
		states, _ = normal(states).(map[string]any)
		if i == 0 {
			agreed = states
		} else if !reflect.DeepEqual(states, agreed) {
			a, _ := json.Marshal(agreed)
			b, _ := json.Marshal(states)
			return nil, fmt.Errorf("%s and %s see different states after telling each other everything:\n    %s\n    %s", name(0), name(i), a, b)
		}
	}
	return agreed, nil
}

// TestAwareness runs presence scripts (clients setting, changing and
// removing their state, and telling each other what they know in any
// order) through ygo, y-protocols and yrs, alone and with clients of each
// in one room. Everyone must end with the same states, and the same ones
// whoever held which client.
func TestAwareness(t *testing.T) {
	yjs, yrs := references(t)
	mine := Ygo.(PresenceImplementation)
	arrangements := [][]PresenceImplementation{{mine}}
	if yjs != nil {
		arrangements = append(arrangements, []PresenceImplementation{yjs}, []PresenceImplementation{mine, yjs}, []PresenceImplementation{yjs, mine})
	}
	if yrs != nil && !*oldYrs {
		arrangements = append(arrangements, []PresenceImplementation{yrs}, []PresenceImplementation{mine, yrs}, []PresenceImplementation{yrs, mine})
		if yjs != nil {
			arrangements = append(arrangements, []PresenceImplementation{yjs, yrs, mine})
		}
	}
	failed := 0
	for seed := *firstRun; seed < *firstRun+uint64(*seeds); seed++ {
		clients := 3 + int(seed%3)
		script := presenceScript(seed, clients, 50)
		var want map[string]any
		for n, impls := range arrangements {
			names := ""
			for _, impl := range impls {
				names += impl.Name() + " "
			}
			got, err := runPresence(script, clients, impls...)
			if err != nil {
				t.Errorf("seed %d [%s]: %v", seed, names, err)
				failed++
				break
			}
			if n == 0 {
				want = got
			} else if !reflect.DeepEqual(got, want) {
				a, _ := json.Marshal(want)
				b, _ := json.Marshal(got)
				t.Errorf("seed %d: [%s] end with other states than ygo alone:\n    %s\n    %s", seed, names, a, b)
				failed++
				break
			}
		}
		if failed >= 3 {
			t.Fatalf("stopping after %d failing seeds", failed)
		}
	}
}
