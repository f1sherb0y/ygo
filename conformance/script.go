// Package conformance runs the same randomly generated editing scripts
// through ygo, yjs and yrs and compares what each ends up with.
//
// A script is a number of replicas, a list of steps and an implied full sync
// at the end. Each step is one transaction on one replica, or a sync that
// brings one replica up to date with another. Positions are given as a
// fraction of the current length (n/65536), so a script is valid whatever
// state a replica is in and every implementation resolves it the same way.
package conformance

import (
	"math/rand/v2"
)

// Step is one transaction on replica R, or a sync From -> To.
type Step struct {
	Op    string         `json:"op"`
	R     int            `json:"r"`
	N     int            `json:"n"`
	K     int            `json:"k,omitempty"`
	S     string         `json:"s"`
	Attrs map[string]any `json:"attrs"`
	Val   any            `json:"val,omitempty"`
	Vals  []any          `json:"vals,omitempty"`
	Key   string         `json:"key"`
	Tag   string         `json:"tag,omitempty"`
	Attr  []string       `json:"attr,omitempty"`
	Text  string         `json:"text"`
	From  int            `json:"from"`
	To    int            `json:"to"`
	V     int            `json:"v,omitempty"`
	// Whole keeps a text position off the middle of a surrogate pair.
	Whole bool `json:"whole,omitempty"`
}

// Script is what every implementation runs.
type Script struct {
	Replicas int    `json:"replicas"`
	GC       bool   `json:"gc"`
	Steps    []Step `json:"steps"`
}

// Profile selects which kinds of steps a generated script contains.
type Profile struct {
	Text, Format, Embed, Array, Map, Nested, XML bool
	// Astral adds characters outside the Basic Multilingual Plane, which
	// take two UTF-16 units, to inserted text. A position may then fall
	// between the two, where yjs replaces both halves with U+FFFD.
	Astral bool
	// Whole keeps every position off the middle of those characters.
	Whole bool
	// Subdocs adds subdocuments set under keys of the map.
	Subdocs bool
	// Undo adds undo and redo: every replica has an undo manager over the
	// four roots that takes each of its own transactions as one step.
	Undo bool
	// Positions adds relative positions, made in the text of a replica and
	// resolved by it at the end, after everything that happened since.
	Positions bool
	// Snapshots adds snapshots, restored at the end. Only scripts without
	// garbage collection take them.
	Snapshots bool
}

// Everything is the profile with every kind of step.
var Everything = Profile{Text: true, Format: true, Embed: true, Array: true, Map: true, Nested: true, XML: true}

var (
	words  = []string{"a", "b", "ab", "hello", " ", "\n", "x y", "你好", "世界", "é", "ß", "קום", "~", "tab\t", "0", "\"q\"", "<&>"}
	astral = []string{"🙂", "𝒳", "a🙂b", "👨‍👩‍👧"}
	keys   = []string{"a", "b", "c", "key", ""}
	tags   = []string{"p", "div", "h1"}
)

func pick[T any](rng *rand.Rand, from []T) T { return from[rng.IntN(len(from))] }

func randomAttrs(rng *rand.Rand) map[string]any {
	attrs := map[string]any{}
	for range 1 + rng.IntN(2) {
		var value any
		switch rng.IntN(4) {
		case 0:
			value = nil // removes the attribute
		case 1:
			value = true
		case 2:
			value = "red"
		default:
			value = "blue"
		}
		attrs[pick(rng, []string{"bold", "color", "i"})] = value
	}
	return attrs
}

func randomValue(rng *rand.Rand, depth int) any {
	switch rng.IntN(9) {
	case 0:
		return nil
	case 1:
		return rng.IntN(2) == 0
	case 2:
		return float64(rng.IntN(2000) - 1000)
	case 3:
		return 1.5
	case 4:
		return float64(1 << 40)
	case 5, 6:
		return pick(rng, words)
	case 7:
		if depth < 2 {
			return []any{randomValue(rng, depth+1), randomValue(rng, depth+1)}
		}
	case 8:
		if depth < 2 {
			return map[string]any{pick(rng, keys[:4]): randomValue(rng, depth+1)}
		}
	}
	return "v"
}

// Generate returns a script of the given size. The same seed always gives
// the same script.
func Generate(seed uint64, replicas, steps int, profile Profile) Script {
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	script := Script{Replicas: replicas, GC: seed%4 != 0}
	var kinds []string
	add := func(on bool, names ...string) {
		if on {
			kinds = append(kinds, names...)
		}
	}
	add(profile.Text, "tins", "tins", "tins", "tdel")
	add(profile.Format, "tfmt", "tinsf")
	add(profile.Embed, "temb")
	add(profile.Array, "ains", "ains", "adel")
	add(profile.Map, "mset", "mset", "mdel")
	add(profile.Nested, "mtext", "amap")
	add(profile.XML, "xel", "xdel")
	add(profile.Subdocs, "msub")
	add(profile.Undo, "undo", "undo", "redo")
	add(profile.Positions, "rpos")
	add(profile.Snapshots, "snap")
	text := func() string {
		s := ""
		for range 1 + rng.IntN(3) {
			if profile.Astral && rng.IntN(3) == 0 {
				s += pick(rng, astral)
			} else {
				s += pick(rng, words)
			}
		}
		return s
	}
	for range steps {
		if replicas > 1 && rng.IntN(4) == 0 {
			from := rng.IntN(replicas)
			to := (from + 1 + rng.IntN(replicas-1)) % replicas
			script.Steps = append(script.Steps, Step{Op: "sync", From: from, To: to, V: 1 + rng.IntN(2)})
			continue
		}
		step := Step{Op: pick(rng, kinds), R: rng.IntN(replicas), N: rng.IntN(65536), Whole: profile.Whole}
		switch step.Op {
		case "tins":
			step.S = text()
			if profile.Format {
				// Without attributes, yjs inserts with the formatting it finds
				// at a search marker, which depends on what the replica did
				// before (see README, "Known differences"). Explicit
				// attributes mean the same everywhere.
				step.Attrs = map[string]any{}
			}
		case "tinsf":
			step.Op, step.S, step.Attrs = "tins", text(), randomAttrs(rng)
		case "tdel", "adel", "xdel":
			step.K = 1 + rng.IntN(4)
		case "tfmt":
			step.K, step.Attrs = 1+rng.IntN(6), randomAttrs(rng)
		case "temb":
			step.Val = map[string]any{"image": pick(rng, words)}
			step.Attrs = map[string]any{} // explicit, as for "tins"
		case "ains":
			for range 1 + rng.IntN(3) {
				step.Vals = append(step.Vals, randomValue(rng, 0))
			}
		case "mset":
			step.Key, step.Val = pick(rng, keys), randomValue(rng, 0)
		case "mdel", "msub":
			step.Key = pick(rng, keys)
		case "mtext":
			step.Key, step.S = pick(rng, keys), text()
		case "amap":
			step.Key, step.Val = pick(rng, keys), randomValue(rng, 0)
		case "xel":
			step.Tag, step.Attr, step.Text = pick(rng, tags), []string{pick(rng, keys[:4]), pick(rng, words)}, text()
		case "rpos":
			step.K = rng.IntN(2) // 1: the position belongs to what is before it
		}
		script.Steps = append(script.Steps, step)
	}
	return script
}
