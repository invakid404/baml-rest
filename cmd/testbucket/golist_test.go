package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testToolchain is the subprocess runner the toolchain-backed tests use.
// Each `go` invocation gets its own deadline, so a hung one fails that
// single command instead of blocking the suite.
func testToolchain() toolchain { return toolchain{timeout: 2 * time.Minute} }

// TestListRunnableNamesAgainstTheRealToolchain is the one test in this
// package that shells out. Everything else runs against a synthetic tree,
// but the runnable universe is defined by `go test -list`'s own behaviour,
// and that is exactly what P0-1 got wrong: `-list '^Test'` hides Examples
// and Fuzz targets that `-run` would have selected. Asserting the filter in
// isolation cannot catch a wrong regexp handed to the toolchain, so this
// builds a throwaway module and asks the real `go`.
func TestListRunnableNamesAgainstTheRealToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a test binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/runnables\n\ngo 1.26.5\n")
	write("x.go", "package runnables\n\nimport \"fmt\"\n\n// Greet prints a greeting.\nfunc Greet() { fmt.Println(\"hi\") }\n")
	write("x_test.go", `package runnables

import "testing"

func TestGreet(t *testing.T) { Greet() }

func BenchmarkGreet(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Greet()
	}
}

func FuzzGreet(f *testing.F) {
	f.Fuzz(func(t *testing.T, s string) { _ = s })
}

func ExampleGreet() {
	Greet()
	// Output: hi
}

// ExampleGreet_second has no Output comment, so the test binary never
// registers it and -run cannot select it.
func ExampleGreet_second() { Greet() }
`)

	p := LivePackage{
		ImportPath: "example.com/runnables",
		Dir:        ".",
		Module:     ".",
		Mode:       modeOff, // resolve against this module alone, no go.work
		Atomic:     true,
		HasTests:   true,
	}
	got, err := listRunnableNames(testToolchain(), dir, p)
	if err != nil {
		t.Fatalf("listRunnableNames: %v", err)
	}

	// Exactly what `go test -run` can select: the test, the fuzz target and
	// the runnable example. Not the benchmark (-bench selects those), and
	// not the example the binary never registers.
	want := []string{"ExampleGreet", "FuzzGreet", "TestGreet"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runnable universe = %v, want %v", got, want)
	}
}

func TestExcludedMatchesWholePathElementsAtAnyDepth(t *testing.T) {
	// The module set is a coverage boundary: a module wrongly excluded here
	// takes every test in it out of the plan, and the coverage gate cannot
	// notice, because it only ever sees the packages `go list` returned.
	// So both directions matter, and both are pinned.
	patterns := defaultExcludedModules
	cases := []struct {
		rel  string
		want bool
		why  string
	}{
		// --- must be excluded ---
		{"adapters/adapter_v0_204_0", true, "the glob's direct match"},
		{"adapters/adapter_v0_204_0/adapter", true, "one level under the glob"},
		{"adapters/adapter_v0_219_0/internal/tool", true, "TWO levels under the glob — the Major: `*` does not cross `/`, so a one-level pattern missed this and the literal prefix check compared against the un-expanded `adapters/adapter_*/` text"},
		{"adapters/adapter_v1/a/b/c/d", true, "arbitrary depth under the glob"},
		{"dynclient/baml-patched", true, "literal exclusion"},
		{"dynclient/baml-patched/internal/x", true, "nested under a literal exclusion"},
		{"nativeserve", true, "literal exclusion"},
		{"nativeserve/deep/nest", true, "nested under a literal exclusion"},
		{"internal/nativebody/nanollmprepare", true, "literal exclusion"},
		{"internal/nativebody/nanollmprepare/sub", true, "nested under a literal exclusion"},

		// --- must NOT be excluded: each of these is a real module or a
		// name that only LOOKS like an excluded one ---
		{".", false, "the root module"},
		{"adapters/common", false, "adapters/common is IN the module set; the adapter glob must not reach it"},
		{"adapters/common/codegen", false, "and neither must its packages"},
		{"adapters", false, "the bare parent of the excluded adapters is not itself excluded"},
		{"bamlutils", false, "the module the whole exercise exists to bucket"},
		{"bamlutils/llmhttp", false, "the whale"},
		{"dynclient", false, "dynclient is in the set; only dynclient/baml-patched is out"},
		{"dynclientfoo", false, "prefix collision must not exclude a different module"},
		{"nativeservefoo", false, "prefix collision: a pattern matches whole path elements, not text prefixes"},
		{"nativeserve-tools", false, "another prefix collision"},
		{"internal/nativebody", false, "the parent of an excluded module stays in the set"},
		{"internal/nativebody/nanollmprepare2", false, "sibling whose name extends an excluded one"},
		{"pool", false, ""},
		{"worker", false, ""},
		{"workerplugin", false, ""},
		{"introspected", false, ""},
	}
	for _, tc := range cases {
		got := excluded(tc.rel, patterns)
		if got != tc.want {
			verb := "was excluded"
			if !got {
				verb = "was NOT excluded"
			}
			t.Errorf("%s %s, want excluded=%v — %s", tc.rel, verb, tc.want, tc.why)
		}
	}

	// An exclusion pattern is normalised the same way the candidate dir is.
	// A perfectly reasonable spelling that silently matched nothing would
	// be the worst outcome for a knob whose only job is to scope the module
	// set — the user believes a module is excluded and it quietly is not.
	spellings := []struct {
		pat  string
		rel  string
		want bool
		why  string
	}{
		{"./nativeserve", "nativeserve", true, "a leading ./ is how most people type a repo-relative path"},
		{"./nativeserve", "nativeserve/deep", true, "and it must still nest"},
		{"nativeserve//x", "nativeserve/x", true, "a doubled separator"},
		{"adapters/./adapter_*", "adapters/adapter_v1", true, "a /./ segment inside a glob pattern"},
		{"adapters/./adapter_*", "adapters/adapter_v1/a/b", true, "and it must still nest at depth"},
		{"nativeserve/", "nativeserve", true, "a trailing slash was already handled"},
		{"  nativeserve  ", "nativeserve", true, "surrounding whitespace was already handled"},
		// Normalising must not widen a pattern into matching more.
		{"./nativeserve", "nativeservefoo", false, "normalisation must not turn a pattern into a text prefix"},
		{"adapters/./adapter_*", "adapters/common", false, "normalisation must not reach a sibling"},
	}
	for _, tc := range spellings {
		if got := excluded(tc.rel, []string{tc.pat}); got != tc.want {
			t.Errorf("excluded(%q, [%q]) = %v, want %v — %s", tc.rel, tc.pat, got, tc.want, tc.why)
		}
	}
}

func TestDiscoverModulesKeepsEveryLiveModuleAndDropsOnlyExcludedOnes(t *testing.T) {
	// The end-to-end form of the same guarantee, on a hermetic tree shaped
	// like this repo: a workspace with members, one GOWORK=off module that
	// must stay, the excluded adapters INCLUDING one nested two levels
	// down, a prefix-colliding sibling, and a go.mod fixture under testdata.
	if testing.Short() {
		t.Skip("runs `go work edit -json`; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mod := func(dir, name string) {
		t.Helper()
		write(filepath.ToSlash(filepath.Join(dir, "go.mod")), "module "+name+"\n\ngo 1.26.5\n")
	}

	write("go.work", "go 1.26.5\n\nuse (\n\t.\n\t./bamlutils\n\t./pool\n)\n")
	mod(".", "example.com/root")
	mod("bamlutils", "example.com/root/bamlutils")
	mod("pool", "example.com/root/pool")
	// Out of the workspace but IN the module set: must be discovered as an
	// atomic GOWORK=off module.
	mod("adapters/common", "example.com/root/adapters/common")
	// Excluded by the glob, at three different depths.
	mod("adapters/adapter_v1", "example.com/root/adapters/adapter_v1")
	mod("adapters/adapter_v1/internal/tool", "example.com/root/adapters/adapter_v1/internal/tool")
	mod("adapters/adapter_v2/a/b/c", "example.com/root/adapters/adapter_v2/a/b/c")
	// Excluded by literal patterns.
	mod("nativeserve", "example.com/root/nativeserve")
	mod("dynclient/baml-patched", "example.com/root/dynclient/baml-patched")
	// Prefix collision: must survive.
	mod("nativeservefoo", "example.com/root/nativeservefoo")
	// A fixture module under testdata: data, not a module of this repo.
	mod("internal/schema/testdata/broken", "example.com/fixture")

	mods, err := discoverModules(testToolchain(), root, defaultExcludedModules)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	got := map[string]moduleSpec{}
	for _, m := range mods {
		got[m.Dir] = m
	}

	want := map[string]moduleSpec{
		".":               {Dir: ".", Mode: modeWork, Atomic: false},
		"bamlutils":       {Dir: "bamlutils", Mode: modeWork, Atomic: false},
		"pool":            {Dir: "pool", Mode: modeWork, Atomic: false},
		"adapters/common": {Dir: "adapters/common", Mode: modeOff, Atomic: true},
		"nativeservefoo":  {Dir: "nativeservefoo", Mode: modeOff, Atomic: true},
	}
	for dir, spec := range want {
		g, ok := got[dir]
		if !ok {
			t.Errorf("module %s was DROPPED from the module set; every test in it would silently stop running", dir)
			continue
		}
		if g != spec {
			t.Errorf("module %s = %+v, want %+v", dir, g, spec)
		}
	}
	for dir := range got {
		if _, ok := want[dir]; !ok {
			t.Errorf("module %s was discovered but should be outside the module set", dir)
		}
	}
	if len(got) != len(want) {
		t.Errorf("discovered %d modules, want %d: %v", len(got), len(want), sortedKeys(got))
	}
}

func TestWorkspaceMembersDistinguishesAbsenceFromFailure(t *testing.T) {
	// A stat failure that is not "absent" must not be read as "there is no
	// workspace": that would flip every module to GOWORK=off and pack each
	// as a whole-module atom, silently rescheduling the tree on the back of
	// an I/O error.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	root := t.TempDir()

	// No go.work at all: a legitimate shape, no error, no members.
	members, err := workspaceMembers(testToolchain(), root)
	if err != nil {
		t.Fatalf("a missing go.work must not be an error: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want none", members)
	}

	// A DANGLING SYMLINK is the ambiguous case, and the one that used to
	// slip through: os.Stat follows links, so a go.work pointing at nothing
	// reports the missing TARGET as ENOENT and looks exactly like "no
	// workspace here". A broken workspace must be loud — silently treating
	// it as absent flips every module to GOWORK=off/atomic and reschedules
	// the whole tree, and discovery runs before the final plan is ever
	// gated, so nothing downstream would catch it.
	dangling := t.TempDir()
	if err := os.Symlink(filepath.Join(dangling, "no-such-file"), filepath.Join(dangling, "go.work")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	// Sanity: confirm the fixture really is the ambiguous shape — Stat says
	// "does not exist" while the directory entry is right there.
	if _, err := os.Stat(filepath.Join(dangling, "go.work")); !os.IsNotExist(err) {
		t.Fatalf("fixture is not a dangling symlink: stat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dangling, "go.work")); err != nil {
		t.Fatalf("fixture has no directory entry: lstat err = %v", err)
	}
	members, err = workspaceMembers(testToolchain(), dangling)
	if err == nil {
		t.Errorf("a dangling go.work symlink was reported as an empty workspace (%v); every module would silently become GOWORK=off/atomic", members)
	} else if !strings.Contains(err.Error(), "dangling symlink") {
		t.Errorf("error does not name the cause: %v", err)
	}

	// A symlink that RESOLVES is a normal workspace and must still work.
	linked := t.TempDir()
	real := filepath.Join(linked, "real.work")
	if err := os.WriteFile(real, []byte("go 1.26.5\n\nuse (\n\t.\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "go.mod"), []byte("module example.com/linked\n\ngo 1.26.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(linked, "go.work")); err != nil {
		t.Fatal(err)
	}
	if members, err := workspaceMembers(testToolchain(), linked); err != nil {
		t.Errorf("a resolvable go.work symlink was rejected: %v", err)
	} else if !members["."] {
		t.Errorf("members = %v, want the root module", members)
	}

	// go.work present but unreadable as a directory entry: `go work edit`
	// fails and the error must surface rather than degrade to "no members".
	if err := os.Mkdir(filepath.Join(root, "go.work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceMembers(testToolchain(), root); err == nil {
		t.Error("an unusable go.work was reported as an empty workspace")
	}
}
