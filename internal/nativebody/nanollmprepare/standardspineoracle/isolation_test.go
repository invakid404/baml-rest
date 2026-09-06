package standardspineoracle_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// standardSpineOraclePath is the import path of the standard-only oracle composite. The
// whole-command go-list-deps gate (cmd/worker-nativeonly.TestNativeOnlyWorkerHasNoBAML +
// the build.sh deny) is authoritative; this SOURCE import-direction test is the fast,
// local complement that points at the exact offending file.
const standardSpineOraclePath = "github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle"

// legacyStaticStreamServePath is the LEGACY/standard static-stream serve lane. It is
// BAML-plan-compare-bound (it builds BAML's StreamRequest plan before its claim), so
// M3e-A's stream-capable native-only graph must never reach it either: the native-only
// artifact streams through nativeserve/spine's BAML-free StreamExecutor, and importing
// this package would reintroduce the standard lane's BAML dependency. It mirrors the
// entry added to the whole-command go-list-deps gate.
const legacyStaticStreamServePath = "github.com/invakid404/baml-rest/nativeserve/canary"

// TestNativeOnlyPackagesDoNotImportStandardComposite proves the deletion-substrate
// invariant at source level: the BAML-free native-only packages (nativegenerated,
// nativeonlyboot, cmd/worker-nativeonly) must NEVER import the standard-only composite,
// which is BAML-aware and imported only by cmd/worker, nor the legacy BAML-plan-bound
// static-stream serve lane. It scans every non-test .go file (all build tags —
// parser.ImportsOnly ignores constraints) so a tag-gated aggregate is covered too.
func TestNativeOnlyPackagesDoNotImportStandardComposite(t *testing.T) {
	forbidden := []struct {
		path string
		// kind names WHAT the package is, so the diagnostic is accurate for each. The two
		// are different kinds of BAML-bound code and calling both "the standard-only
		// composite" would misdescribe the canary lane.
		kind string
	}{
		{standardSpineOraclePath, "the standard-only oracle composite"},
		{legacyStaticStreamServePath, "the legacy BAML-plan-bound static-stream serve lane"},
	}
	for _, rel := range []string{
		"../nativegenerated",
		"../nativeonlyboot",
		"../cmd/worker-nativeonly",
	} {
		for _, f := range forbidden {
			assertNoImport(t, rel, f.path, f.kind)
		}
	}
}

func assertNoImport(t *testing.T, dir, forbidden, kind string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == forbidden {
				t.Errorf("%s imports %s (%q); the native-only graph must never reach it", path, kind, forbidden)
			}
		}
	}
	// NON-VACUITY: a rename/move that emptied the dir must not make this guard pass by
	// scanning nothing.
	if scanned == 0 {
		t.Errorf("scanned no non-test .go files under %s; the import-direction guard would pass vacuously", dir)
	}
}

// bamlLinkedPaths are the import-path substrings that would mean this composite reached
// BAML itself rather than consuming the neutral closures the already-BAML-linked generated
// method captured.
var bamlLinkedPaths = []string{
	"baml_client",
	"github.com/boundaryml/baml",
	"language_client_go",
	"dynclient/baml-patched",
	"github.com/invakid404/baml-rest/dynclient",
	"github.com/invakid404/baml-rest/introspected",
	"github.com/invakid404/baml-rest/internal/rootruntime",
}

// composedPositivePaths must be present, so an empty or wrong go-list output cannot pass
// the gate above by absence.
var composedPositivePaths = []string{
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated",
	"github.com/invakid404/baml-rest/nativeserve/admission",
	"github.com/invakid404/baml-rest/bamlutils",
}

// TestStandardCompositeDoesNotLinkBAML is the ExecBridge-U1s BAML-isolation gate for the
// standard side of the boundary.
//
// It is the property that makes the whole neutral-closure design worth its indirection: the
// composite is BAML-AWARE (it forwards a live StreamRequest plan builder, a per-prefix
// ParseStream oracle and a final Parse oracle) yet links NO BAML at all, because those three
// are plain Go function values the generated method — the one package that IS linked against
// BAML — supplied. If this composite ever imported a generated client directly, the
// native-only artifact's dependency gate would still pass while the isolation the design
// rests on had quietly stopped being true.
func TestStandardCompositeDoesNotLinkBAML(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping import-graph assertion")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../nanollmprepare/standardspineoracle/isolation_test.go -> the module root.
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))

	cmd := exec.Command("go", "list", "-deps", standardSpineOraclePath)
	cmd.Dir = moduleRoot
	cmd.Env = append(cmd.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(deps) == 0 || deps[0] == "" {
		t.Fatal("go list -deps returned nothing; the isolation gate would pass vacuously")
	}
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		for _, bad := range bamlLinkedPaths {
			if strings.Contains(dep, bad) {
				t.Errorf("the standard composite links %q (matched %q); BAML must reach it only as neutral closures", dep, bad)
			}
		}
	}
	joined := strings.Join(deps, "\n")
	for _, want := range composedPositivePaths {
		if !strings.Contains(joined, want) {
			t.Errorf("the composite's dependency graph is missing %q; the isolation gate would pass by absence", want)
		}
	}
}
