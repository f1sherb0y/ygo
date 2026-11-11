package conformance

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

var (
	seeds    = flag.Int("seeds", 200, "scripts per profile")
	firstRun = flag.Uint64("seed", 0, "first seed")
	one      = flag.String("script", "", "a script file for TestScript")
	failures = flag.String("failures", "", "directory to write shrunk failing scripts to")
	oldYrs   = flag.Bool("yrs-0.25", false, "use yrs 0.25.0 (conformance/testdata/yrs-0.25), which has the defects in FINDINGS.md")
)

// references starts the reference implementations that are installed (see
// README.md); either may be nil.
func references(t *testing.T) (yjs, yrs *Driver) {
	t.Helper()
	if _, err := os.Stat(filepath.Join("testdata", "yjs", "node_modules", "yjs")); err == nil && haveNode() {
		d, err := StartDriver("yjs", filepath.Join("testdata", "yjs"), "node", "driver.mjs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Close)
		yjs = d
	}
	exe := filepath.Join("testdata", "yrs", "target", "release", "ygo-conformance-yrs")
	if *oldYrs {
		exe = filepath.Join("testdata", "yrs-0.25", "target", "release", "ygo-conformance-yrs-025")
	}
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if path, err := filepath.Abs(exe); err == nil {
		if _, err := os.Stat(path); err == nil {
			d, err := StartDriver("yrs", filepath.Join("testdata", "yrs"), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Close)
			yrs = d
		}
	}
	if yjs == nil && yrs == nil {
		t.Log("no reference implementation is installed (see conformance/README.md): checking ygo against itself only")
	}
	return yjs, yrs
}

// haveNode reports whether there is a node to run the yjs driver with.
func haveNode() bool {
	_, err := exec.LookPath("node")
	return err == nil
}

type profileCase struct {
	name     string
	profile  Profile
	replicas int
	steps    int
}

var profiles = []profileCase{
	{"text", Profile{Text: true}, 3, 60},
	{"text-astral", Profile{Text: true, Astral: true}, 3, 60},
	{"text-astral-whole", Profile{Text: true, Astral: true, Whole: true}, 3, 60},
	{"rich-text", Profile{Text: true, Format: true, Embed: true}, 3, 60},
	{"array", Profile{Array: true}, 3, 60},
	{"map", Profile{Map: true}, 3, 40},
	{"nested", Profile{Array: true, Map: true, Nested: true}, 3, 60},
	{"xml", Profile{XML: true}, 3, 40},
	{"everything", Everything, 4, 120},
	{"everything-long", Everything, 6, 400},
}

// check runs one script in every arrangement and returns what went wrong,
// "" when nothing did.
func check(script Script, yjs, yrs *Driver) string {
	run := func(impls ...Implementation) (Outcome, string) {
		for _, d := range []*Driver{yjs, yrs} {
			if d != nil {
				d.Reset()
			}
		}
		outcome, err := Run(script, impls...)
		if err != nil {
			names := ""
			for _, impl := range impls {
				names += impl.Name() + " "
			}
			return outcome, fmt.Sprintf("[%s] %v", names[:len(names)-1], err)
		}
		return outcome, ""
	}
	mine, problem := run(Ygo)
	if problem != "" {
		return problem
	}
	if yjs != nil {
		// yjs alone must end with what ygo alone ends with: the same edits
		// give the same document.
		theirs, problem := run(yjs)
		if problem != "" {
			return problem
		}
		if !reflect.DeepEqual(mine, theirs) {
			a, _ := json.Marshal(mine.Content)
			b, _ := json.Marshal(theirs.Content)
			return fmt.Sprintf("ygo and yjs end with different content:\n    %s\n    %s", a, b)
		}
		// Replicas of both, exchanging updates, must agree with that too.
		for _, impls := range [][]Implementation{{Ygo, yjs}, {yjs, Ygo}} {
			mixed, problem := run(impls...)
			if problem != "" {
				return problem
			}
			if !reflect.DeepEqual(mine, mixed) {
				return "ygo and yjs replicas together end with other content than ygo alone"
			}
		}
	}
	if yrs != nil {
		// yrs resolves an index next to deleted items to another position
		// than yjs (FINDINGS.md), so its documents are not compared with
		// ygo's: replicas of both must agree with each other.
		arrangements := [][]Implementation{{yrs}, {Ygo, yrs}, {yrs, Ygo}}
		if yjs != nil {
			arrangements = append(arrangements, []Implementation{Ygo, yjs, yrs}, []Implementation{yrs, yjs, Ygo})
		}
		for _, impls := range arrangements {
			if _, problem := run(impls...); problem != "" {
				return problem
			}
		}
	}
	return ""
}

// shrink drops steps while the script still fails.
func shrink(script Script, yjs, yrs *Driver) Script {
	for size := len(script.Steps) / 2; size >= 1; size /= 2 {
		for start := 0; start+size <= len(script.Steps); {
			smaller := script
			smaller.Steps = append(append([]Step{}, script.Steps[:start]...), script.Steps[start+size:]...)
			if check(smaller, yjs, yrs) != "" {
				script = smaller
			} else {
				start += size
			}
		}
	}
	return script
}

// TestDifferential runs generated scripts through ygo, yjs and yrs, alone
// and with replicas of each exchanging updates.
func TestDifferential(t *testing.T) {
	yjs, yrs := references(t)
	for _, c := range profiles {
		t.Run(c.name, func(t *testing.T) {
			failed := 0
			for i := range *seeds {
				seed := *firstRun + uint64(i)
				script := Generate(seed, c.replicas, c.steps, c.profile)
				if check(script, yjs, yrs) == "" {
					continue
				}
				small := shrink(script, yjs, yrs)
				raw, _ := json.Marshal(small)
				t.Errorf("seed %d, shrunk to %d steps: %s\n  %s", seed, len(small.Steps), check(small, yjs, yrs), raw)
				if failed++; failed >= 3 {
					t.Fatalf("stopping after %d failing seeds", failed)
				}
			}
		})
	}
}

// TestScript runs the script in the file given by -script.
func TestScript(t *testing.T) {
	if *one == "" {
		t.Skip("no -script")
	}
	raw, err := os.ReadFile(*one)
	if err != nil {
		t.Fatal(err)
	}
	var script Script
	if err := json.Unmarshal(raw, &script); err != nil {
		t.Fatal(err)
	}
	yjs, yrs := references(t)
	impls := []Implementation{Ygo}
	if yjs != nil {
		impls = append(impls, yjs)
	}
	if yrs != nil {
		impls = append(impls, yrs)
	}
	for _, impl := range impls {
		outcome, err := Run(script, impl)
		content, _ := json.Marshal(outcome.Content)
		t.Logf("%s alone: error=%v\n  %s", impl.Name(), err, content)
	}
	if problem := check(script, yjs, yrs); problem != "" {
		t.Error(problem)
	}
}
