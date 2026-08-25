package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultExcludedModules mirrors the module set the current unit-tests
// workflow sweeps: adapters pin their own BAML versions and are tested by
// the integration workflow, baml-patched is vendored upstream source, and
// nanollmprepare / nativeserve are cgo modules deliberately kept out of
// go.work and out of the pure-Go lane.
//
// This is the "module set" the coverage gate intersects `go list ./...`
// with. It is a scoping knob, not an escape hatch: anything inside the set
// must be scheduled, and the set itself is visible in the plan output.
var defaultExcludedModules = []string{
	"adapters/adapter_*",
	"dynclient/baml-patched",
	"internal/nativebody/nanollmprepare",
	"nativeserve",
}

// toolchain runs `go` subprocesses, giving EACH ONE its own deadline.
//
// The deadline is per subprocess, not per discovery pass, because that is
// what --toolchain-timeout promises and what actually protects the job:
// `plan` runs `go work edit`, one `go list` per module and one
// `go test -list` per name-sliced package, all sequentially. A single
// shared context.WithTimeout would turn the flag into a budget for the
// whole sweep, so a slow-but-healthy `go list` could consume it and make a
// later `go test -list` fail the instant it started — a false failure
// charged to the wrong command, and one that would get worse as the module
// set grew.
//
// Carrying the duration rather than a context is the point: there is no
// shared context to accidentally reuse, so the property is enforced by the
// signatures rather than by everyone remembering.
type toolchain struct {
	// timeout bounds each subprocess. Zero disables the deadline.
	timeout time.Duration
}

// context returns a FRESH deadline for one subprocess.
func (t toolchain) context() (context.Context, context.CancelFunc) {
	if t.timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), t.timeout)
}

// run executes one `go` invocation under its own deadline and returns its
// stdout. A deadline hit is reported as such, naming the flag, so a timeout
// never reads as a broken repository.
func (t toolchain) run(dir string, extraEnv []string, args ...string) ([]byte, error) {
	ctx, cancel := t.context()
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("go %s timed out after %s (raise or disable --toolchain-timeout)",
				strings.Join(args, " "), t.timeout)
		}
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type moduleSpec struct {
	Dir    string // repo-relative; "." for the root module
	Mode   string // modeWork | modeOff
	Atomic bool
}

// findRepoRoot walks up from dir looking for the workspace file, then the
// git dir. Everything this tool emits is expressed relative to that root.
func findRepoRoot(dir string) (string, error) {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(cur, "go.work")); err == nil {
			return cur, nil
		}
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no go.work or .git found above %s", dir)
		}
		cur = parent
	}
}

// workspaceMembers reads go.work through `go work edit -json` rather than
// parsing it: the file in this repo is mostly a long rationale comment, and
// the toolchain is the only correct parser of the directive it wraps.
func workspaceMembers(tc toolchain, repoRoot string) (map[string]bool, error) {
	workFile := filepath.Join(repoRoot, "go.work")
	if _, err := os.Stat(workFile); err != nil {
		// Any stat failure other than absence — a permission problem, an
		// I/O error — must NOT be read as "there is no workspace". Doing so
		// would flip every module to GOWORK=off and pack each as a
		// whole-module atom, silently rescheduling the entire tree. There
		// is no coverage-gate backstop for this: it changes discovery
		// before the final plan is ever checked.
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat go.work in %s: %w", repoRoot, err)
		}
		// Stat FOLLOWS symlinks, so ENOENT here is ambiguous: either there
		// is no directory entry at all, or there is one that points at
		// nothing. Only the first is a repo that legitimately has no
		// workspace; the second is a broken workspace and must be loud, or
		// it degrades into exactly the silent rescheduling above. Lstat
		// answers the question by not following the link.
		if _, lerr := os.Lstat(workFile); lerr == nil {
			return nil, fmt.Errorf("go.work in %s is a dangling symlink: %w", repoRoot, err)
		} else if !os.IsNotExist(lerr) {
			return nil, fmt.Errorf("lstat go.work in %s: %w", repoRoot, lerr)
		}
		// No directory entry at all: a repo with no workspace file is a
		// legitimate shape, and every module then resolves standalone.
		return map[string]bool{}, nil
	}
	out, err := tc.run(repoRoot, nil, "work", "edit", "-json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Use []struct {
			DiskPath string `json:"DiskPath"`
		} `json:"Use"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse go work edit -json: %w", err)
	}
	members := map[string]bool{}
	for _, u := range doc.Use {
		members[path.Clean(filepath.ToSlash(u.DiskPath))] = true
	}
	return members, nil
}

// discoverModules finds every go.mod under repoRoot that the module set
// includes, and tags each with the resolution mode its packages must run in.
func discoverModules(tc toolchain, repoRoot string, excludes []string) ([]moduleSpec, error) {
	members, err := workspaceMembers(tc, repoRoot)
	if err != nil {
		return nil, err
	}
	var mods []moduleSpec
	err = filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// testdata is skipped for the same reason the Go toolchain
			// ignores it: a go.mod fixture there is data, not a module of
			// this repo. Discovering one would run `go list ./...` inside
			// it, and a fixture that does not build would abort the whole
			// plan.
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(p))
		if err != nil {
			return err
		}
		rel = path.Clean(filepath.ToSlash(rel))
		if excluded(rel, excludes) {
			return nil
		}
		spec := moduleSpec{Dir: rel, Mode: modeOff, Atomic: true}
		if members[rel] {
			spec.Mode = modeWork
			spec.Atomic = false
		}
		mods = append(mods, spec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Dir < mods[j].Dir })
	return mods, nil
}

// excluded matches a repo-relative dir against the exclusion patterns,
// including anything nested beneath an excluded module at ANY depth.
//
// The pattern is tested against the dir and against every ancestor of it,
// which is the only formulation that works for a glob. The three checks this
// replaced covered one level and could not nest a glob at all: `*` does not
// cross `/`, so "adapters/adapter_*/*" misses "adapters/adapter_x/a/b", and
// the literal prefix check compared against the un-expanded pattern text
// ("adapters/adapter_*/"), which matches nothing. A module two levels under
// an excluded adapter was therefore discovered and scheduled, contradicting
// the documented module set.
//
// Matching ancestors cannot over-exclude: a pattern only ever matches a
// COMPLETE path element sequence, so "nativeserve" excludes "nativeserve"
// and "nativeserve/x" but never "nativeservefoo", and "adapters/adapter_*"
// never touches "adapters/common". The tests pin both directions, because
// dropping a module here silently drops every test in it.
func excluded(rel string, patterns []string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSpace(strings.TrimSuffix(pat, "/"))
		if pat == "" {
			continue
		}
		// Normalise the pattern the same way rel is normalised, or a
		// perfectly reasonable `--exclude-module ./nativeserve` matches no
		// cleaned dir and the exclusion silently does nothing — the worst
		// outcome for a knob whose only job is to scope the module set.
		// Clean collapses "./", "//" and "/./" and leaves globs alone.
		pat = path.Clean(pat)
		for cur := path.Clean(rel); ; cur = path.Dir(cur) {
			if ok, _ := path.Match(pat, cur); ok {
				return true
			}
			if !strings.Contains(cur, "/") {
				// path.Dir of a single element is ".", which can only
				// match a pattern of "." — not a shape the module set uses.
				break
			}
		}
	}
	return false
}

// listPackages resolves the live package set — the authority on what must
// run. A package with no _test.go files is reported with HasTests=false: it
// is not bucketed (running it is a no-op) but the moment it gains a test
// file the next `go list` schedules it, with no store update needed.
func listPackages(tc toolchain, repoRoot string, mods []moduleSpec) ([]LivePackage, error) {
	var out []LivePackage
	seen := map[string]bool{}
	for _, m := range mods {
		const format = "{{.ImportPath}}\t{{.Dir}}\t{{len .TestGoFiles}}\t{{len .XTestGoFiles}}"
		var env []string
		if m.Mode == modeOff {
			env = []string{"GOWORK=off"}
		}
		stdout, err := tc.run(filepath.Join(repoRoot, filepath.FromSlash(m.Dir)), env, "list", "-f", format, "./...")
		if err != nil {
			return nil, fmt.Errorf("module %s (mode=%s): %w", m.Dir, m.Mode, err)
		}
		for _, line := range strings.Split(string(stdout), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) < 4 {
				return nil, fmt.Errorf("go list in %s: unexpected line %q", m.Dir, line)
			}
			nTest, _ := strconv.Atoi(fields[2])
			nXTest, _ := strconv.Atoi(fields[3])
			dir, err := filepath.Rel(repoRoot, fields[1])
			if err != nil {
				return nil, fmt.Errorf("relativize %s: %w", fields[1], err)
			}
			p := LivePackage{
				ImportPath: fields[0],
				Dir:        path.Clean(filepath.ToSlash(dir)),
				Module:     m.Dir,
				Mode:       m.Mode,
				Atomic:     m.Atomic,
				HasTests:   nTest+nXTest > 0,
			}
			// A package reachable from two modules (e.g. through a
			// replace) must be scheduled once, not twice.
			if seen[p.ImportPath] {
				continue
			}
			seen[p.ImportPath] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	return out, nil
}

// runnablePrefixes are the top-level name prefixes that `go test -run`
// actually selects: tests, examples and fuzz targets. Benchmarks are
// deliberately absent — `-run` does not select them (`-bench` does), so
// putting a Benchmark name into a slice's alternation would cover nothing
// while claiming a weight the slicer would then balance around.
var runnablePrefixes = []string{"Test", "Example", "Fuzz"}

// isRunnable reports whether a name listed by `go test -list` is something
// the emitted `-run` alternation can actually select.
func isRunnable(name string) bool {
	for _, pre := range runnablePrefixes {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// listRunnableNames enumerates a package's complete top-level RUNNABLE set —
// every name the emitted `-run '^(...)$'` would select. It is only ever
// called for packages the store has flagged split=run — at most a couple —
// because it compiles the test binary.
//
// The universe must be enumerated with the SAME selection semantics as the
// invocation it feeds, or the slices are complete against a set narrower
// than the one that runs. `go test -run` selects tests, examples AND fuzz
// targets; listing only `^Test` would leave a package's ExampleXxx in no
// slice at all, and since no slice names it, no slice runs it — a silently
// skipped runnable behind a green matrix. That is exactly the failure the
// coverage gate exists to prevent, so the list is taken wide (`-list '.*'`)
// and narrowed here by the documented `-run` rule.
//
// Using the toolchain instead of grepping for `func Test` is deliberate on
// two counts: it respects build tags, so a tag-gated file cannot contribute
// a name to a -run slice that would then match nothing; and it lists only
// examples the test binary actually registers, so an example with no Output
// comment — compiled but never run — never enters the universe the gate
// insists on covering.
func listRunnableNames(tc toolchain, repoRoot string, p LivePackage) ([]string, error) {
	target := p.ImportPath
	dir := repoRoot
	var env []string
	if p.Mode == modeOff {
		dir = filepath.Join(repoRoot, filepath.FromSlash(p.Module))
		env = []string{"GOWORK=off"}
		target = p.pattern()
	}
	stdout, err := tc.run(dir, env, "test", "-list", ".*", target)
	if err != nil {
		return nil, fmt.Errorf("package %s: %w", p.ImportPath, err)
	}
	var names []string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		// -list prints one name per line, then a trailing "ok <pkg> <t>".
		if line == "" || strings.ContainsAny(line, " \t") || !isRunnable(line) {
			continue
		}
		names = append(names, line)
	}
	sort.Strings(names)
	return names, nil
}

// loadLivePackages reads a live set from a JSON file instead of shelling out
// to the toolchain. It exists so `plan` can be run — and reviewed — against
// a recorded tree with no build, which is also how the tests drive it.
func loadLivePackages(path string) ([]LivePackage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read live set: %w", err)
	}
	var live []LivePackage
	if err := json.Unmarshal(data, &live); err != nil {
		return nil, fmt.Errorf("parse live set %s: %w", path, err)
	}
	for i := range live {
		if live[i].Mode == "" {
			live[i].Mode = modeWork
		}
		if live[i].Module == "" {
			live[i].Module = "."
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ImportPath < live[j].ImportPath })
	return live, nil
}
