package ygo_test

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestText_LongEditingSessionMatchesModel types, backspaces and jumps
// around in a text the way an editor does, a transaction per edit, and
// compares the text with a plain slice after every one. Position lookups
// in a long text go through the search markers, from either side, and the
// blocks typed are merged at each commit: what this test is for is that
// none of that ever finds the wrong place. A second document gets every
// update and must hold the same text.
func TestText_LongEditingSessionMatchesModel(t *testing.T) {
	for seed := range uint64(4) {
		rng := rand.New(rand.NewPCG(seed, 99))
		d, mirror := ygo.NewDoc(), ygo.NewDoc()
		text, mirrorText := ygo.NewText(d, "t"), ygo.NewText(mirror, "t")
		var updates [][]byte
		d.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })

		var model []rune // BMP only: one rune is one index
		cursor := 0
		edits := 30000
		if testing.Short() {
			edits = 4000
		}
		for i := range edits {
			// Mostly carry on where the last edit was; sometimes jump.
			if rng.IntN(40) == 0 {
				cursor = rng.IntN(len(model) + 1)
			}
			cursor = min(cursor, len(model))
			txn := d.WriteTxn()
			var err error
			switch k := rng.IntN(10); {
			case k < 5 || len(model) == 0: // type
				s := []rune{rune('a' + rng.IntN(26))}
				if rng.IntN(8) == 0 {
					s = []rune("word " + string(rune('A'+rng.IntN(26))))
				}
				err = text.Insert(txn, uint64(cursor), string(s))
				model = slices.Insert(model, cursor, s...)
				cursor += len(s)
			case k < 8 && cursor > 0: // backspace
				n := min(1+rng.IntN(2), cursor)
				err = text.Delete(txn, uint64(cursor-n), uint64(n))
				model = slices.Delete(model, cursor-n, cursor)
				cursor -= n
			case cursor < len(model): // delete forward, sometimes a selection
				n := min(1+rng.IntN(1+rng.IntN(30)), len(model)-cursor)
				err = text.Delete(txn, uint64(cursor), uint64(n))
				model = slices.Delete(model, cursor, cursor+n)
			}
			txn.Commit()
			if err != nil {
				t.Fatalf("seed %d, edit %d: %v", seed, i, err)
			}
			if i%7 == 0 || i == edits-1 {
				rtxn := d.ReadTxn()
				got, length := text.String(), text.Length()
				rtxn.Close()
				if got != string(model) || length != uint64(len(model)) {
					t.Fatalf("seed %d, edit %d: text (length %d) differs from the model (length %d)\n got %q\nwant %q",
						seed, i, length, len(model), tail(got), tail(string(model)))
				}
			}
		}

		for _, u := range updates {
			if err := ygo.ApplyUpdate(mirror, u); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		rtxn := mirror.ReadTxn()
		got := mirrorText.String()
		rtxn.Close()
		if got != string(model) {
			t.Errorf("seed %d: the document that got the updates differs from the model", seed)
		}
		// And the whole state at once, into a document that saw none of it.
		fresh := ygo.NewDoc()
		freshText := ygo.NewText(fresh, "t")
		if err := ygo.ApplyUpdate(fresh, ygo.EncodeStateAsUpdate(d)); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		rtxn = fresh.ReadTxn()
		got = freshText.String()
		rtxn.Close()
		if got != string(model) {
			t.Errorf("seed %d: the encoded state differs from the model", seed)
		}
	}
}

// TestArray_LongEditingSessionMatchesModel is the same for an array, whose
// lookups and deletions share the marker code with the text.
func TestArray_LongEditingSessionMatchesModel(t *testing.T) {
	for seed := range uint64(4) {
		rng := rand.New(rand.NewPCG(seed, 7))
		d := ygo.NewDoc()
		arr := ygo.NewArray(d, "a")
		var model []any
		cursor := 0
		for i := range 20000 {
			if rng.IntN(30) == 0 {
				cursor = rng.IntN(len(model) + 1)
			}
			cursor = min(cursor, len(model))
			txn := d.WriteTxn()
			switch k := rng.IntN(10); {
			case k < 5 || len(model) == 0:
				v := float64(i)
				arr.Insert(txn, uint64(cursor), v)
				model = slices.Insert(model, cursor, any(v))
				cursor++
			case k < 8 && cursor > 0:
				n := min(1+rng.IntN(2), cursor)
				arr.Delete(txn, uint64(cursor-n), uint64(n))
				model = slices.Delete(model, cursor-n, cursor)
				cursor -= n
			case cursor < len(model):
				n := min(1+rng.IntN(1+rng.IntN(20)), len(model)-cursor)
				arr.Delete(txn, uint64(cursor), uint64(n))
				model = slices.Delete(model, cursor, cursor+n)
			}
			txn.Commit()
			if i%7 == 0 || i == 19999 {
				rtxn := d.ReadTxn()
				got := arr.ToSlice()
				rtxn.Close()
				if !slices.Equal(got, model) {
					t.Fatalf("seed %d, edit %d: array of %d differs from the model of %d", seed, i, len(got), len(model))
				}
			}
		}
	}
}

// tail returns the end of s, enough to see where two long texts part.
func tail(s string) string {
	if len(s) > 80 {
		return "…" + s[len(s)-80:]
	}
	return s
}
