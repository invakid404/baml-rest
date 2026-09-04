package standardspineoracle_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
