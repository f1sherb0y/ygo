package conformance

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// finding is a file in testdata/findings/: a reproducer for a defect found in a
// reference implementation. It is either updates to apply in order to a new
// document, or a script.
type finding struct {
	What    string   `json:"what"`
	Updates []string `json:"updates"`
	Script
}

// TestFindings replays the reproducers in testdata/findings/. ygo and yjs must agree
// on each; what yrs makes of it is logged, so that a new yrs release shows
// here whether it still has the defect.
func TestFindings(t *testing.T) {
	yjs, yrs := references(t)
	if yjs == nil {
		t.Skip("needs yjs")
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "findings", "*.json"))
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f finding
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			if f.Updates == nil {
				// A script: ygo and yjs alone and together.
				if problem := check(f.Script, yjs, nil); problem != "" {
					t.Error(problem)
				}
				if yrs != nil {
					problem := check(f.Script, nil, yrs)
					t.Logf("%s\n  yrs agrees: %v %s", f.What, problem == "", problem)
				}
				return
			}
			apply := func(impl Implementation) any {
				if d, ok := impl.(*Driver); ok {
					d.Reset()
				}
				replica, err := impl.New(999, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, u := range f.Updates {
					update, _ := hex.DecodeString(u)
					if err := replica.Apply(update, 1); err != nil {
						t.Fatalf("%s: %v", impl.Name(), err)
					}
				}
				d, err := replica.Dump()
				if err != nil {
					t.Fatal(err)
				}
				delete(d, localKey)
				return normal(d)
			}
			mine, theirs := apply(Ygo), apply(yjs)
			if !reflect.DeepEqual(mine, theirs) {
				a, _ := json.Marshal(mine)
				b, _ := json.Marshal(theirs)
				t.Errorf("ygo and yjs differ:\n  %s\n  %s", a, b)
			}
			if yrs != nil {
				got := apply(yrs)
				a, _ := json.Marshal(mine)
				b, _ := json.Marshal(got)
				t.Logf("%s\n  yrs agrees: %v\n  ygo, yjs: %s\n  yrs:      %s", f.What, reflect.DeepEqual(mine, got), a, b)
			}
		})
	}
}
