//go:build integration

package debaml

// Slice 7.2a-3 — the SERVING-SHAPED differential oracle.
//
// # What it proves
//
// For every row of the corpus: build an in-memory .baml project from the row's
// schema.Bundle, hand the SAME raw assistant text to stock BAML v0.223.0's CFFI
// and to the native coercion-state collector + evaluator, and require the native
// leg to reproduce stock exactly or refuse to decide. The stock envelope is the
// authority; the native leg is compared against it and is NEVER fed back into the
// CFFI.
//
// # What it does NOT do
//
// Nothing here changes admission. Every constraint-bearing fixture still declines
// through checkSupported / SupportsNativeFinalBundle / Parse / ParseStaticBundle —
// [TestServingOracleBoundaryLock] asserts exactly that, for every row, including
// the target-level ones — and no Checked[T] carrier, wire shape or mapper change
// is in this slice.
//
// # Running
//
//	CGO_ENABLED=1 go test -tags integration ./internal/debaml -run TestServingOracle
//
// Requires CGO and the stock BAML v0.223.0 CFFI (auto-located under the user BAML
// cache dir), exactly like the #597/#603/#649 oracles.
//
// # Recording the pins
//
// Stock and Native are RECORDINGS. To re-record after a corpus change:
//
//	BAML_SERVING_ORACLE_RECORD=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/debaml -run TestServingOracleDifferential -v
//
// which prints one RECORD line per row per leg. The pins are then compared for
// EQUALITY on every subsequent run, so neither leg can change shape unnoticed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

const (
	// soRecordEnv switches the differential into recording mode.
	soRecordEnv = "BAML_SERVING_ORACLE_RECORD"
	// soRootGoModPath is the root module's go.mod, from this package's directory.
	soRootGoModPath = "../../go.mod"
	soBAMLModule    = "github.com/boundaryml/baml"
	soBAMLVersion   = "v0.223.0"
)

// soWantProjectHash pins the SHA-256 of the rendered project. It is compared only
// after the golden byte-comparison succeeds, so it is not a second copy of the
// same check: it is what makes regenerating the golden a deliberate, reviewable
// act rather than a silent one.
const soWantProjectHash = "8ec226ef989f47367b003554349bf9227ba77e75b1ffd33b0786b20ade4a9c44"

// soStockCache memoizes one stock parse per fixture. The CFFI runtime is
// process-global and the suite deliberately does not parallelise over it, so no
// mutex is needed.
var soStockCache = map[string]soStockEnvelope{}

// soStockFor drives (or replays) the stock leg for one fixture.
func soStockFor(t *testing.T, f servingOracleFixture) soStockEnvelope {
	t.Helper()
	if f.Fatal {
		t.Fatalf("%s is a process-fatal row and must never be driven in-process; "+
			"it belongs to TestServingOracleFatalRowIsUnobservable", f.Name)
	}
	if e, ok := soStockCache[f.Name]; ok {
		return e
	}
	soEnsureRuntime(t)
	e, err := soDriveStock(f)
	if err != nil {
		t.Fatalf("%s: the stock leg could not be read: %v", f.Name, err)
	}
	soStockCache[f.Name] = e
	return e
}

// ---------------------------------------------------------------------------
// (1) The oracle really is stock BAML v0.223.0.
// ---------------------------------------------------------------------------

// TestServingOracleStockIsPinnedBAML requires the LOADED CFFI runtime and the root
// go.mod pin to be exactly v0.223.0, so a green differential can never be
// attributed to a different BAML. The runtime check is the load-bearing one: it
// reads the native library that actually parsed every row in this binary.
func TestServingOracleStockIsPinnedBAML(t *testing.T) {
	if got := soLoadedBAMLVersion(); got != soBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, soBAMLRuntimeVersion)
	}
	raw, err := os.ReadFile(soRootGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", soRootGoModPath, err)
	}
	if err := soCheckStockModulePin(raw); err != nil {
		t.Fatalf("%s: %v", soRootGoModPath, err)
	}
}

// soCheckStockModulePin requires go.mod to REQUIRE the stock BAML module at the
// pinned version and to REPLACE it with nothing.
//
// It parses rather than string-matches. The text check it replaces looked for
// `=> github.com/boundaryml/baml`, which catches a replacement whose NEW path is
// the stock module but not one whose OLD path is:
//
//	github.com/boundaryml/baml v0.223.0 => ./local-fork
//
// That form keeps the required-version text intact — so the version assertion still
// passed — while the module actually linked is a fork, which is precisely the
// substitution the pin exists to prevent.
func soCheckStockModulePin(gomod []byte) error {
	f, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	required := false
	for _, r := range f.Require {
		if r.Mod.Path != soBAMLModule {
			continue
		}
		if r.Mod.Version != soBAMLVersion {
			return fmt.Errorf("requires %s %s, want exactly %s; the oracle would be comparing against a "+
				"different BAML", soBAMLModule, r.Mod.Version, soBAMLVersion)
		}
		required = true
	}
	if !required {
		return fmt.Errorf("does not require %s at all", soBAMLModule)
	}
	// EITHER SIDE of a replacement is disqualifying: the old path being the stock
	// module means something else is linked in its place, and the new path being it
	// means some other module resolves to it.
	for _, r := range f.Replace {
		if r.Old.Path == soBAMLModule {
			return fmt.Errorf("REPLACES %s (%s %s => %s %s); the oracle must link the stock module, not a "+
				"fork", soBAMLModule, r.Old.Path, r.Old.Version, r.New.Path, r.New.Version)
		}
		if r.New.Path == soBAMLModule {
			return fmt.Errorf("replaces %s %s WITH %s; another module would resolve to the stock path",
				r.Old.Path, r.Old.Version, soBAMLModule)
		}
	}
	return nil
}

// TestServingOracleStockPinRejectsAFork drives the pin check over synthetic go.mod
// content, because the live one is (correctly) clean and would exercise only the
// accepting path.
func TestServingOracleStockPinRejectsAFork(t *testing.T) {
	const base = "module example.com/x\n\ngo 1.26.5\n\nrequire github.com/boundaryml/baml v0.223.0\n"
	if err := soCheckStockModulePin([]byte(base)); err != nil {
		t.Fatalf("the clean control was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mod  string
		want string
	}{
		{
			// The case the text check missed entirely.
			name: "a LEFT-HAND replacement onto a local fork",
			mod:  base + "\nreplace github.com/boundaryml/baml v0.223.0 => ./local-fork\n",
			want: "REPLACES",
		},
		{
			name: "a left-hand replacement with no version",
			mod:  base + "\nreplace github.com/boundaryml/baml => ./local-fork\n",
			want: "REPLACES",
		},
		{
			name: "a RIGHT-HAND replacement onto the stock module",
			mod:  base + "\nreplace example.com/other v1.0.0 => github.com/boundaryml/baml v0.223.0\n",
			want: "WITH",
		},
		{
			name: "a different required version",
			mod:  "module example.com/x\n\ngo 1.26.5\n\nrequire github.com/boundaryml/baml v0.222.0\n",
			want: "want exactly",
		},
		{
			name: "the module is not required at all",
			mod:  "module example.com/x\n\ngo 1.26.5\n",
			want: "does not require",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := soCheckStockModulePin([]byte(tc.mod))
			if err == nil {
				t.Fatal("accepted a go.mod that does not link the stock BAML module")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name the problem (%q): %v", tc.want, err)
			}
		})
	}
	// CONTROL: an unrelated replacement is fine, so the check is about the stock
	// module rather than about replacements in general.
	if err := soCheckStockModulePin([]byte(base + "\nreplace example.com/other => ./other\n")); err != nil {
		t.Fatalf("an unrelated replacement was rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (2) The corpus is well formed.
// ---------------------------------------------------------------------------

// TestServingOracleCorpusIsWellFormed pins the corpus's own invariants, so a row
// cannot be added in a shape that would make a later assertion vacuous.
func TestServingOracleCorpusIsWellFormed(t *testing.T) {
	if len(servingOracleFixtures) == 0 {
		t.Fatal("the corpus is empty; every claim this package makes would be vacuous")
	}
	seen := map[string]bool{}
	families := map[string]int{}
	fatal, unconstrained := 0, 0
	for _, f := range servingOracleFixtures {
		if seen[f.Name] {
			t.Fatalf("duplicate fixture name %q", f.Name)
		}
		seen[f.Name] = true
		if f.Doc == "" {
			t.Errorf("%s: no Doc; a row must say what it is evidence for", f.Name)
		}
		if f.Bundle == nil {
			t.Fatalf("%s: no bundle", f.Name)
		}
		if f.Raw == "" {
			t.Errorf("%s: no raw assistant text", f.Name)
		}
		families[f.Family]++
		if f.Fatal {
			fatal++
		}
		if f.Unconstrained {
			unconstrained++
			if soBundleHasConstraint(f.Bundle) {
				t.Errorf("%s is marked Unconstrained but its bundle carries a constraint; the control would "+
					"assert nothing", f.Name)
			}
		} else if !soBundleHasConstraint(f.Bundle) {
			t.Errorf("%s carries no constraint but is not marked Unconstrained; it would be counted as a "+
				"constraint-bearing decline it cannot witness", f.Name)
		}
	}
	// Every family the non-integration gate test covers must be exercised here, and
	// every family exercised here must be one the gate test covers. Neither
	// direction alone is enough: the first stops the gate test from covering a shape
	// the oracle never drives, the second stops the oracle from drifting into a
	// shape the gate test never drives.
	for _, want := range servingOracleGateFamilies {
		if families[want] == 0 {
			t.Errorf("gate family %q has no corpus fixture; the gate test would assert over a shape the "+
				"oracle never drives", want)
		}
	}
	for fam := range families {
		if !soContains(servingOracleGateFamilies, fam) {
			t.Errorf("fixture family %q is not in servingOracleGateFamilies; add it there so "+
				"checkSupported/checkSupportedFields/checkSupportedType are driven over it by name", fam)
		}
	}
	if fatal == 0 {
		t.Error("no fixture is marked Fatal; the subprocess-isolation arm would be vacuous")
	}
	if unconstrained == 0 {
		t.Error("no unconstrained control; every decline would be indistinguishable from a blanket refusal")
	}
	t.Logf("corpus: %d fixtures across %d families (%d process-fatal, %d unconstrained controls)",
		len(servingOracleFixtures), len(families), fatal, unconstrained)
}

// soBundleHasConstraint reports whether any node of the bundle carries a
// constraint — the property the boundary lock is about.
func soBundleHasConstraint(b *schema.Bundle) bool {
	found := false
	bundleWalkTypes(b.Target, func(t schema.Type) {
		if len(t.Meta.Constraints) > 0 {
			found = true
		}
	})
	for _, c := range b.Classes {
		if len(c.Constraints) > 0 {
			found = true
		}
		for _, f := range c.Fields {
			bundleWalkTypes(f.Type, func(t schema.Type) {
				if len(t.Meta.Constraints) > 0 {
					found = true
				}
			})
		}
	}
	for _, e := range b.Enums {
		if len(e.Constraints) > 0 {
			found = true
		}
	}
	// Structural-alias TARGETS are constraint carriers too. Missing them would
	// misclassify a carrier as unconstrained, and an "unconstrained control" that
	// actually carries a constraint asserts the opposite of what it claims.
	for _, a := range b.StructuralRecursiveAliases {
		bundleWalkTypes(a.Target, func(t schema.Type) {
			if len(t.Meta.Constraints) > 0 {
				found = true
			}
		})
	}
	return found
}

func soContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// (3) The project stock compiles is the project under review.
// ---------------------------------------------------------------------------

// TestServingOracleProjectDrift pins the rendered .baml as a pure function of the
// corpus, and pins its hash so the golden cannot be regenerated silently.
//
// The bytes compared are the ones the RUNTIME was created from (soRuntimeSource),
// not a fresh render, so this closes the loop the sibling oracles close with a
// generated-client source-map check: corpus -> project -> the compiled artifact
// the stock leg is actually driving.
func TestServingOracleProjectDrift(t *testing.T) {
	soEnsureRuntime(t)
	if os.Getenv(soWriteEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(soProjectGolden), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(soProjectGolden), err)
		}
		if err := os.WriteFile(soProjectGolden, []byte(soRuntimeSource), 0o644); err != nil {
			t.Fatalf("write %s: %v", soProjectGolden, err)
		}
		t.Logf("%s rewritten from the corpus; its SHA-256 is %s — update soWantProjectHash",
			soProjectGolden, soProjectHash(soRuntimeSource))
		return
	}
	golden, err := os.ReadFile(soProjectGolden)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with %s=1)", soProjectGolden, err, soWriteEnv)
	}
	if string(golden) != soRuntimeSource {
		t.Fatalf("%s is stale: it is not what the corpus renders and not what the stock runtime compiled.\n"+
			"Regenerate with %s=1 and update soWantProjectHash.\n  golden %d bytes, rendered %d bytes",
			soProjectGolden, soWriteEnv, len(golden), len(soRuntimeSource))
	}
	if got := soProjectHash(soRuntimeSource); got != soWantProjectHash {
		t.Fatalf("the rendered project's SHA-256 is %s, want %s.\n"+
			"The golden and the corpus agree, so this is a deliberate change that has to be acknowledged here.",
			got, soWantProjectHash)
	}
	// Every fixture's function must be present in the compiled text. A row whose
	// method is missing would fail its parse with "function not found", which is a
	// harness failure, not an observation.
	for _, f := range servingOracleFixtures {
		if !strings.Contains(soRuntimeSource, "function "+f.method()+"(") {
			t.Errorf("%s declares no function %s in the compiled project", f.Name, f.method())
		}
	}
}

// ---------------------------------------------------------------------------
// (4) The differential.
// ---------------------------------------------------------------------------

// TestServingOracleDifferential drives both legs over every non-fatal row and
// requires each to reproduce its RECORDED envelope exactly, then applies the
// contract live.
//
// Pinning both legs makes this a regression pin as well as a differential: a row
// where the two currently AGREE cannot silently start diverging, and one where
// they currently diverge cannot silently change shape.
func TestServingOracleDifferential(t *testing.T) {
	soEnsureRuntime(t)
	recording := os.Getenv(soRecordEnv) != ""
	for _, f := range servingOracleFixtures {
		if f.Fatal {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			stock := soStockFor(t, f)
			native := soRunNative(f)
			_, problems := soCompare(f, stock, native)

			if recording {
				fmt.Printf("RECORD\t%s\tSTOCK\t%s\n", f.Name, stock.render())
				fmt.Printf("RECORD\t%s\tNATIVE\t%s\n", f.Name, native.render())
			} else {
				if got := stock.render(); got != f.Stock {
					t.Errorf("the STOCK envelope changed:\n  got  %s\n  want %s%s",
						got, f.Stock, soReport(f, stock, native, problems))
				}
				if got := native.render(); got != f.Native {
					t.Errorf("the NATIVE envelope changed:\n  got  %s\n  want %s%s",
						got, f.Native, soReport(f, stock, native, problems))
				}
			}
			if v := soViolations(problems); len(v) > 0 {
				t.Errorf("the two legs disagree in a way the contract forbids:%s",
					soReport(f, stock, native, problems))
			}
		})
	}
}

// TestServingOracleFailClosed is the load-bearing assertion of this package.
//
// It runs over the LIVE legs rather than the pinned columns, so it cannot pass
// because the corpus was edited to match, and there is no bucket for a known
// difference: a native answer stock did not produce fails here.
func TestServingOracleFailClosed(t *testing.T) {
	soEnsureRuntime(t)
	var violations []string
	driven := 0
	for _, f := range servingOracleFixtures {
		if f.Fatal {
			continue
		}
		driven++
		stock := soStockFor(t, f)
		native := soRunNative(f)
		_, problems := soCompare(f, stock, native)
		for _, p := range soViolations(problems) {
			violations = append(violations, soReport(f, stock, native, []soMismatch{p}))
		}
	}
	if driven == 0 {
		t.Fatal("no row was driven; the fail-closed claim would be vacuous")
	}
	if len(violations) > 0 {
		t.Fatalf("the serving-shaped differential is NOT fail-closed; %d disagreement(s) over %d rows:%s",
			len(violations), driven, strings.Join(violations, "\n"))
	}
	t.Logf("fail-closed over %d driven rows", driven)
}

// soWantAgreement pins the population of each agreement bucket, so a change that
// quietly turned a decline into an answer — or an answer into a decline — has to
// be acknowledged.
var soWantAgreement = map[soAgreement]int{
	soAgreeValue:               30,
	soAgreeAssertFailure:       4,
	soAgreeRefusal:             1,
	soNativeDeclinesPredicate:  6,
	soNativeDeclinesCoercion:   3,
	soNativeDeclinesExtraction: 2,
	soCollectorRefuses:         1,
	soStateDiverges:            7,
	soEventShapeDiverges:       3,
	soStockUnobservable:        1,
}

// TestServingOracleAgreementTally pins the measured shape of the corpus.
//
// The tally is LABELLED: every bucket means exactly what its name says, and a row
// that lands in none is a failure rather than an unlabelled remainder.
func TestServingOracleAgreementTally(t *testing.T) {
	soEnsureRuntime(t)
	got := map[soAgreement]int{}
	for _, f := range servingOracleFixtures {
		if f.Fatal {
			got[soStockUnobservable]++
			soRequireDivergenceNote(t, f, soStockUnobservable)
			continue
		}
		stock := soStockFor(t, f)
		native := soRunNative(f)
		bucket, _ := soCompare(f, stock, native)
		got[bucket]++
		soRequireDivergenceNote(t, f, bucket)
	}
	if len(soWantAgreement) == 0 {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("RECORD-TALLY %s: %d", k, got[soAgreement(k)])
		}
		t.Fatal("soWantAgreement is empty; pin the measured buckets logged above")
	}
	total := 0
	for bucket, want := range soWantAgreement {
		if got[bucket] != want {
			t.Errorf("agreement bucket %s: got %d rows, want %d", bucket, got[bucket], want)
		}
		total += want
	}
	for bucket, n := range got {
		if _, ok := soWantAgreement[bucket]; !ok {
			t.Errorf("agreement bucket %s appeared with %d rows and is not pinned", bucket, n)
		}
	}
	if total != len(servingOracleFixtures) {
		t.Errorf("the pinned buckets cover %d rows, the corpus has %d", total, len(servingOracleFixtures))
	}
}

// soRequireDivergenceNote enforces the note contract in BOTH directions: every
// non-agreeing bucket must carry a one-sentence Divergence, and an agreeing bucket
// must carry none. A note that survives its cause is as misleading as a cost with
// no note.
func soRequireDivergenceNote(t *testing.T, f servingOracleFixture, bucket soAgreement) {
	t.Helper()
	if soIsAgreement(bucket) {
		if f.Divergence != "" {
			t.Errorf("%s: carries a Divergence note but the two legs AGREE (%s); the note is stale",
				f.Name, bucket)
		}
		return
	}
	if f.Divergence == "" {
		t.Errorf("%s: landed in %s but carries no Divergence note saying what the two legs do differently",
			f.Name, bucket)
	}
}

// ---------------------------------------------------------------------------
// (5) The boundary lock.
// ---------------------------------------------------------------------------

// TestServingOracleBoundaryLock is the boundary lock, and since de-BAML Slice 7.2b-3 it
// is an explicit PER-ROW expected disposition rather than a blanket refusal.
//
// Three populations, each with its own requirement:
//
//	Unconstrained (control)   ADMITTED by the constraint cut-line, and at least one
//	                          actually SERVES bytes.
//	Served (4 rows)           the ONE admitted fingerprint: ADMITTED by the native-final
//	                          support predicate and SERVED by the static unary /call
//	                          route — while still DECLINING on the direct parse endpoints
//	                          and through the generic shape cut-line.
//	Constrained (49 rows)     refused by every production entry point, with the refusal
//	                          caused by the constraint rather than by the shape.
//
// Six gates are driven for the declining population, not one:
//
//	checkSupported            the gate Parse runs
//	checkSupportedFields      its body, pinned to agree
//	checkSupportedType        per constrained type node
//	SupportsNativeFinalBundle the admission predicate
//	ParseStaticBundle         the static-final serving entry point
//	Parse                     the dynamic entry point, reached through the same gate
//
// Target-level rows (@check/@assert on the return type, and on a target list
// element) are included with nothing carved out: since #664 walked b.Target there
// is no exception left, and this test fails if one reappears.
func TestServingOracleBoundaryLock(t *testing.T) {
	constrained, controls, servedControls, attributed, namedConstraint, served := 0, 0, 0, 0, 0, 0
	for _, f := range servingOracleFixtures {
		t.Run(f.Name, func(t *testing.T) {
			if f.Unconstrained {
				if f.Served {
					t.Fatal("a row is BOTH unconstrained and Served; Served is the constraint-bearing " +
						"fingerprint the cutover admits")
				}
				controls++
				if soRequireAdmitted(t, f) {
					servedControls++
				}
				return
			}
			if f.Served {
				served++
				soRequireServed(t, f)
				return
			}
			constrained++
			attributedHere, namedHere := soRequireDeclined(t, f)
			if attributedHere {
				attributed++
			}
			if namedHere {
				namedConstraint++
			}
		})
	}
	if constrained == 0 {
		t.Fatal("no constraint-bearing fixture; the boundary lock would be vacuous")
	}
	if controls == 0 {
		t.Fatal("no unconstrained control; the decline could not be shown to be constraint-specific")
	}
	if servedControls == 0 {
		t.Fatal("no unconstrained control actually SERVED through ParseStaticBundle; the suite would only " +
			"have proven that nothing is ever emitted")
	}
	if attributed != constrained {
		t.Fatalf("only %d of %d constraint-bearing rows had their decline ATTRIBUTED to their constraints "+
			"(stripped twin admitted); the lock would prove only that something refuses", attributed, constrained)
	}
	if namedConstraint == 0 {
		t.Fatal("no row's decline message NAMED a constraint; the constraint cut-line itself would be " +
			"unwitnessed even though every stripped twin is admitted")
	}
	// The SERVED population is exactly the four companion rows — declared as data in
	// two places that have to agree, so a fifth row cannot acquire the flag quietly and
	// the four cannot lose it.
	if served != len(soCompanionRowNames) {
		t.Fatalf("%d rows carry Served but %d companion rows are named; the per-row disposition and the "+
			"named set have parted company", served, len(soCompanionRowNames))
	}
	byName := map[string]servingOracleFixture{}
	for _, f := range servingOracleFixtures {
		byName[f.Name] = f
	}
	for _, name := range soCompanionRowNames {
		f, ok := byName[name]
		if !ok {
			t.Fatalf("companion row %q is missing from the corpus", name)
		}
		if !f.Served {
			t.Fatalf("companion row %q does not carry Served; the cutover's four-row flip would be "+
				"unwitnessed for it", name)
		}
	}
	t.Logf("boundary lock: %d constraint-bearing bundles declined by all gates, all %d attributed to their "+
		"constraints (the stripped twin is admitted), %d of them named a constraint in the message; "+
		"%d rows SERVED through the static unary /call route; "+
		"%d unconstrained controls admitted (%d of them served bytes)",
		constrained, attributed, namedConstraint, served, controls, servedControls)
}

// soRequireServed is the per-row disposition for the FOUR rows the Slice 7.2b-3 cutover
// admits, and it asserts BOTH halves of the boundary.
//
// ADMITTED: EVERY named schema gate says yes — the three generic ones
// (checkSupported / checkSupportedFields / checkSupportedType, which since the cutover
// consult the same fingerprint), the native-final support predicate, and its exported
// twin (the one nativeserve's admission return-shape gate delegates to) — and the static
// unary /call route reaches one of the two public outcomes: bytes, or the CLAIMED
// assertion failure. DECLINED: the direct parse endpoints and the /stream admission
// predicate keep refusing, which is the scope's "static unary /call final parsing only"
// boundary, now expressed as a ROUTE decision rather than as a shape reject.
//
// Like soRequireDeclined it runs under BAML_REST_USE_DEBAML set both ways and unset, so
// the disposition is shown to be independent of the serve-level umbrella switch.
func soRequireServed(t *testing.T, f servingOracleFixture) {
	t.Helper()
	for _, flag := range []string{"true", "false", ""} {
		soWithDeBAMLFlag(t, flag, func() {
			if err := SupportsNativeFinalBundle(f.Bundle); err != nil {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: SupportsNativeFinalBundle DECLINED an admitted "+
					"fingerprint: %v", flag, err)
			}
			if !IsAdmittedStaticCheckedFamily(f.Bundle) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: the exported fingerprint (nativeserve's return-shape "+
					"delegate) rejected a row the support predicate admitted", flag)
			}
			// The /call route reaches a PUBLIC OUTCOME: bytes, or the claimed assertion
			// failure. A decline here would mean the row was admitted and then found
			// unservable AFTER transport — the exact thing the scope forbids.
			res, err := ParseStaticBundleUnaryCall(context.Background(), f.Bundle, f.Raw)
			switch {
			case err == nil:
				if len(res.JSON) == 0 {
					t.Fatalf("BAML_REST_USE_DEBAML=%q: the /call route succeeded but served no bytes", flag)
				}
			case errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported):
				t.Fatalf("BAML_REST_USE_DEBAML=%q: the /call route DECLINED an admitted fingerprint (%v); "+
					"admission happens before the socket, so a post-admission decline is a broken claim",
					flag, err)
			default:
				if len(res.JSON) != 0 {
					t.Fatalf("BAML_REST_USE_DEBAML=%q: the /call route claimed a failure but still produced "+
						"%s bytes", flag, res.JSON)
				}
				// The failure must be the RENDERED STOCK ASSERTION, not merely "some
				// non-sentinel error with no bytes". Without this the route-level proof
				// green-lit any unrelated empty-result failure — an extraction refusal, a
				// carrier build error, a renderer decline — as if it were the one public
				// outcome this branch documents. checked_static_test.go proves the MAPPER
				// produces that error class; this is the same claim for the INTEGRATED
				// /call route, which is what a caller actually reaches.
				if !staticCheckedIsAssertFailure(err) {
					t.Fatalf("BAML_REST_USE_DEBAML=%q: the /call route failed with %T (%v); an admitted "+
						"fingerprint has exactly two public outcomes, and the failing one is the "+
						"rendered stock assertion error", flag, err, err)
				}
			}

			// The DIRECT endpoints stay closed.
			dres, derr := ParseStaticBundle(context.Background(), f.Bundle, f.Raw)
			if !errors.Is(derr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: ParseStaticBundle returned (%s, %v); the direct endpoint "+
					"is outside the admitted route", flag, dres.JSON, derr)
			}
			pres, perr := Parse(context.Background(), soParseRequestFor(t, f.Bundle, f.Raw))
			if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: root Parse returned (%s, %v); the direct endpoint is "+
					"outside the admitted route", flag, pres.JSON, perr)
			}

			// And the GENERIC shape cut-line AGREES: since the cutover it answers the
			// ONE canonical fingerprint, so these three admit exactly what the support
			// predicate admits. A decline here would be the gates disagreeing about a
			// single schema, which the scope calls a bug in its own right.
			if err := checkSupported(f.Bundle); err != nil {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupported DECLINED an admitted fingerprint (%v); "+
					"the named schema gates must share one fingerprint", flag, err)
			}
			if err := checkSupportedFields(f.Bundle); err != nil {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupportedFields DECLINED an admitted fingerprint "+
					"(%v)", flag, err)
			}
			nodes := bundleConstrainedTypeNodes(f.Bundle)
			if len(nodes) == 0 {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: the row carries no constrained TYPE node; "+
					"checkSupportedType would be unasserted for it", flag)
			}
			for _, n := range nodes {
				if err := checkSupportedType(f.Bundle, n); err != nil {
					t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupportedType(%s) DECLINED the admitted "+
						"constrained node (%v)", flag, soTypeExpr(n), err)
				}
			}
			// The DYNAMIC and STREAM lanes stay closed — as a ROUTE decision now that
			// the shape gates agree, which is what keeps this admission /call-only.
			if err := SupportsNativeStreamBundle(f.Bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: the /stream admission predicate returned %v; the "+
					"stream lane is a zero-socket decline for this fingerprint", flag, err)
			}
		})
	}
}

// TestServingOracleSiblingsStillDeclineBeforeTransport is the guard the scope requires
// beside the four-row flip, at the ORACLE's own boundary.
//
// Each row is ONE property away from an admitted companion row — a second check, a
// duplicate label, an alias, a reordered pair, a different predicate, a non-ASCII
// label, a renamed class — and each must be refused by the admission-time predicates
// BEFORE any socket, and by every parse entry point.
//
// It is asserted here, beside the corpus, because these are the shapes the corpus's
// admitted rows are nearest to; internal/debaml's own untagged sweep
// (TestStaticCheckedOnePropertySiblingsDeclineEverywhere) covers the same property over
// a wider set without the CFFI.
func TestServingOracleSiblingsStillDeclineBeforeTransport(t *testing.T) {
	base := func() *schema.Bundle {
		return soBundle(soClassType("StaticCheckedAnswer"),
			[]schema.ClassDef{soClassOf("StaticCheckedAnswer", []schema.ClassField{
				soField("answer", stringType()),
				soField("confidence", soWith(intType(), soCheck("positive", "this > 0"))),
			})}, nil)
	}
	// CONTROL: the base IS the admitted fingerprint, so every rejection below is
	// attributable to the one property that was changed.
	if err := SupportsNativeFinalBundle(base()); err != nil {
		t.Fatalf("the sibling base is NOT the admitted fingerprint (%v); every assertion below is vacuous", err)
	}

	alias := func(s string) *string { return &s }
	mutate := func(fn func(*schema.Bundle)) *schema.Bundle {
		b := base()
		fn(b)
		return b
	}
	const raw = `{"answer":"sunny","confidence":9}`
	siblings := []struct {
		name string
		b    *schema.Bundle
	}{
		{"a second check", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(
				b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 5", Label: alias("big")})
		})},
		{"a duplicate check label", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(
				b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 5", Label: alias("positive")})
		})},
		{"a check plus an assert", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(
				b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintAssert, Expression: "this > 5", Label: alias("big")})
		})},
		{"an aliased field", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Name.Alias = alias("score")
		})},
		{"the two fields in the other order", mutate(func(b *schema.Bundle) {
			f := b.Classes[0].Fields
			f[0], f[1] = f[1], f[0]
		})},
		{"a different predicate", soBundle(soClassType("StaticCheckedAnswer"),
			[]schema.ClassDef{soClassOf("StaticCheckedAnswer", []schema.ClassField{
				soField("answer", stringType()),
				soField("confidence", soWith(intType(), soCheck("positive", "this >= 0"))),
			})}, nil)},
		{"a non-ASCII label", soBundle(soClassType("StaticCheckedAnswer"),
			[]schema.ClassDef{soClassOf("StaticCheckedAnswer", []schema.ClassField{
				soField("answer", stringType()),
				soField("confidence", soWith(intType(), soCheck("positifé", "this > 0"))),
			})}, nil)},
		{"a renamed class", soBundle(soClassType("SomeOtherAnswer"),
			[]schema.ClassDef{soClassOf("SomeOtherAnswer", []schema.ClassField{
				soField("answer", stringType()),
				soField("confidence", soWith(intType(), soCheck("positive", "this > 0"))),
			})}, nil)},
	}
	for _, s := range siblings {
		t.Run(s.name, func(t *testing.T) {
			// PRE-SOCKET: admission never sees a supported shape, so no socket can open.
			if err := SupportsNativeFinalBundle(s.b); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("SupportsNativeFinalBundle ADMITTED a one-property sibling: %v", err)
			}
			if IsAdmittedStaticCheckedFamily(s.b) {
				t.Error("the exported fingerprint ADMITTED a one-property sibling; nativeserve's " +
					"return-shape gate would let it claim a socket")
			}
			// …and every parse entry point refuses it too, so a sibling that somehow
			// reached one could still not be served.
			for _, tc := range []struct {
				name  string
				parse func(context.Context, *schema.Bundle, string) (bamlutils.DeBAMLParseResult, error)
			}{
				{"ParseStaticBundleUnaryCall", ParseStaticBundleUnaryCall},
				{"ParseStaticBundle", ParseStaticBundle},
			} {
				res, err := tc.parse(context.Background(), s.b, raw)
				if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Errorf("%s returned (%s, %v) for a one-property sibling; want the decline sentinel",
						tc.name, res.JSON, err)
				}
				if len(res.JSON) != 0 {
					t.Errorf("%s declined a sibling but still produced %s bytes", tc.name, res.JSON)
				}
			}
		})
	}
}

// soCompanionRowNames are the four Slice 7.2b-3 serving-shaped companion rows: the
// exact two-field fingerprint native now SERVES, in all four outcomes.
//
// They are listed EXPLICITLY rather than discovered by a name prefix, so a row that
// was renamed or dropped fails the guard instead of silently emptying it.
var soCompanionRowNames = []string{
	"static_answer_confidence_check_pass",
	"static_answer_confidence_check_fail",
	"static_answer_confidence_assert_pass",
	"static_answer_confidence_assert_fail",
}

// TestServingOracleCompanionRowsAreTheAdmittedFingerprint ties the corpus rows to the
// production classifier and to the mapper's byte proof.
//
// TestServingOracleBoundaryLock requires these four to be SERVED — but it would say the
// same for four rows that happened to be admitted for some other reason. This proves
// they are the fingerprint staticCheckedProfileOf classifies, and drives the mapper over
// each row's own raw text, so the corpus rows and the mapper's byte proof
// (checked_static_test.go) are known to be about the same four shapes rather than two
// similar-looking sets.
func TestServingOracleCompanionRowsAreTheAdmittedFingerprint(t *testing.T) {
	byName := map[string]servingOracleFixture{}
	for _, f := range servingOracleFixtures {
		byName[f.Name] = f
	}
	served, rejected := 0, 0
	for _, name := range soCompanionRowNames {
		f, ok := byName[name]
		if !ok {
			t.Fatalf("companion row %q is missing from the corpus; the 7.2b-2 candidate set would be "+
				"unwitnessed", name)
		}
		t.Run(name, func(t *testing.T) {
			prof, ok := staticCheckedProfileOf(f.Bundle)
			if !ok {
				t.Fatal("the row is NOT the admitted fingerprint, so its decline witnesses an unrecognised " +
					"shape rather than the closed seam")
			}
			// The mapper reaches one of the two public outcomes for every row — a value
			// or the rendered assertion failure — and never the decline sentinel, which
			// would mean the seam is not the only thing holding the row back.
			res, err := staticCheckedMap(f.Bundle, prof, f.Raw)
			switch {
			case err == nil:
				if len(res.JSON) == 0 {
					t.Fatal("the mapper succeeded but produced no bytes")
				}
				served++
			case staticCheckedIsAssertFailure(err):
				if len(res.JSON) != 0 {
					t.Fatalf("the mapper rejected the node but still produced %s bytes", res.JSON)
				}
				rejected++
			default:
				t.Fatalf("the mapper neither served nor rejected the row: %v", err)
			}
			// And the cutover really admits it: the mapper's outcome above is what the
			// route serves, not what a closed gate keeps to itself.
			if serr := SupportsNativeFinalBundle(f.Bundle); serr != nil {
				t.Fatalf("SupportsNativeFinalBundle DECLINED a companion row (%v); the mapper outcome "+
					"above would never reach a caller", serr)
			}
		})
	}
	// BOTH outcome classes must be represented, or the mapper half of this proof would
	// only cover one of the two public shapes the cutover has to get right.
	if served == 0 || rejected == 0 {
		t.Fatalf("the companion rows produced %d served and %d rejected outcomes; both classes must be "+
			"exercised", served, rejected)
	}
}

// soRequireDeclined drives every gate and requires the fallback sentinel from each.
func soRequireDeclined(t *testing.T, f servingOracleFixture) (attributedToConstraint, declineNamesConstraint bool) {
	t.Helper()
	attributed, namedConstraint := false, false
	// EVERY named seam, over EVERY constraint-bearing fixture — not a representative
	// subset. checkSupportedFields and checkSupportedType are the two the gate test's
	// hand-written rows cover; driving them here as well is what makes the
	// un-narrowed claim ("every constraint-bearing fixture, aliases and target-level
	// included, declines through all of them") a per-fixture fact.
	//
	// BAML_REST_USE_DEBAML is the serve-level umbrella switch. It is set BOTH ways
	// around every gate call — Parse included — so the decision is shown to be
	// independent of it. The structural half of that claim, that nothing in
	// internal/debaml reads the environment at all, is
	// TestServingOracleGateReadsNoEnvironment.
	for _, flag := range []string{"true", "false", ""} {
		soWithDeBAMLFlag(t, flag, func() {
			err := checkSupported(f.Bundle)
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupported returned %v, want ErrDeBAMLParseUnsupported "+
					"for the constraint-bearing bundle %s", flag, err, soTypeExpr(f.Bundle.Target))
			}
			// ATTRIBUTION, in both directions. A decline only WITNESSES the constraint
			// gate if the constraint caused it, which is proven by stripping the
			// constraints and finding the twin admitted. A row whose SHAPE the gate
			// refuses anyway must say so, and is then excluded from the attribution
			// count rather than counted as evidence it cannot give.
			if strippedErr := checkSupported(soStripConstraints(f.Bundle)); strippedErr != nil {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: the constraint-stripped twin of %s is ALSO declined (%v), "+
					"so this row's decline is not attributable to its constraints", flag, f.Name, strippedErr)
			}
			// The CAUSAL attribution is the stripped twin above: constraints removed ->
			// admitted, constraints present -> declined. The MESSAGE is a weaker signal
			// and deliberately not required: adding a constraint to a union variant
			// changes how checkSupportedUnionShape classifies it, so the gate refuses
			// with a shape message for a decline the constraint caused. Both are
			// counted, and the message-named population is required to be non-empty so
			// the constraint cut-line itself stays witnessed.
			attributed = true
			if strings.Contains(err.Error(), "constraint") {
				namedConstraint = true
			}
			fieldsErr := checkSupportedFields(f.Bundle)
			if !errors.Is(fieldsErr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupportedFields returned %v, want "+
					"ErrDeBAMLParseUnsupported", flag, fieldsErr)
			}
			// The two are pinned to agree. checkSupported's body is checkSupportedFields
			// after two blanket recursion rejects, and no corpus bundle is recursive, so
			// a constraint decline that lived in one and not the other would mean the
			// gate the ROUTE runs and the gate this test drives had parted company —
			// which is exactly what a same-outcome assertion catches and what driving
			// checkSupportedFields alongside checkSupported is for.
			if fieldsErr.Error() != err.Error() {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupported and checkSupportedFields disagree about "+
					"%s:\n  checkSupported       %v\n  checkSupportedFields %v",
					flag, f.Name, err, fieldsErr)
			}
			// checkSupportedType is driven on EVERY constrained type node of the
			// fixture, which for a target-level row is the return type itself and for a
			// list/map/union row is the nested node the constraint sits on.
			nodes := bundleConstrainedTypeNodes(f.Bundle)
			if len(nodes) == 0 && !soBundleHasDeclarationConstraint(f.Bundle) {
				t.Fatalf("%s carries no constrained TYPE node and no class/enum declaration constraint; "+
					"checkSupportedType would be unasserted for it", f.Name)
			}
			for _, n := range nodes {
				if err := checkSupportedType(f.Bundle, n); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Fatalf("BAML_REST_USE_DEBAML=%q: checkSupportedType(%s) returned %v, want "+
						"ErrDeBAMLParseUnsupported", flag, soTypeExpr(n), err)
				}
			}
			if err := SupportsNativeFinalBundle(f.Bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: SupportsNativeFinalBundle returned %v, want "+
					"ErrDeBAMLParseUnsupported", flag, err)
			}
			res, err := ParseStaticBundle(context.Background(), f.Bundle, f.Raw)
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: ParseStaticBundle returned (%v, %v); a constraint-bearing "+
					"bundle must decline rather than serve", flag, res, err)
			}
			if len(res.JSON) != 0 {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: ParseStaticBundle declined but still produced %s bytes",
					flag, res.JSON)
			}

			// Parse — the ROOT entry point the worker calls. The constrained bundle is
			// carried in through the STATIC descriptor lane, which is the only request
			// shape that can express a constraint at all: DynamicOutputSchema has no
			// constraint channel (the #572 dynamic-schema ceiling), so the dynamic lane
			// could never present one to the gate. soParseRequestFor proves the
			// descriptor lowers back to this exact bundle before driving it, so Parse is
			// declining THIS fixture rather than something adjacent.
			req := soParseRequestFor(t, f.Bundle, f.Raw)
			pres, perr := Parse(context.Background(), req)
			if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: Parse returned (%v, %v); a constraint-bearing bundle must "+
					"decline at the root entry point too", flag, pres, perr)
			}
			if len(pres.JSON) != 0 {
				t.Fatalf("BAML_REST_USE_DEBAML=%q: Parse declined but still produced %s bytes", flag, pres.JSON)
			}
		})
	}
	return attributed, namedConstraint
}

// soBundleHasDeclarationConstraint reports a constraint on a class or enum
// DEFINITION, which checkSupportedFields owns and checkSupportedType does not.
func soBundleHasDeclarationConstraint(b *schema.Bundle) bool {
	for _, c := range b.Classes {
		if len(c.Constraints) > 0 {
			return true
		}
	}
	for _, e := range b.Enums {
		if len(e.Constraints) > 0 {
			return true
		}
	}
	return false
}

// soParseRequestFor builds the Parse request that carries a constrained bundle.
//
// Parse's dynamic lane lowers a DynamicOutputSchema, which has NO constraint
// channel, so a constraint can only reach Parse through the STATIC descriptor lane
// (req.StaticStreamDescriptor with neither Stream nor StreamFinal set, which
// Parse lowers with schema.FromStaticDescriptor and routes to ParseStaticBundle).
//
// The descriptor is derived from the bundle and then lowered BACK, and the round
// trip is required to reproduce the bundle exactly. Without that, Parse could be
// declining a bundle that merely resembles the fixture's.
func soParseRequestFor(t *testing.T, b *schema.Bundle, raw string) bamlutils.DeBAMLParseRequest {
	t.Helper()
	desc := bundleDescriptorFor(b)
	lowered, err := schema.FromStaticDescriptor(desc)
	if err != nil {
		t.Fatalf("the descriptor derived from the fixture does not lower: %v", err)
	}
	if a, bb := soBundleJSON(t, lowered), soBundleJSON(t, b); a != bb {
		t.Fatalf("the descriptor round trip changed the bundle, so Parse would be driven over a DIFFERENT "+
			"schema than the rest of the oracle:\n  lowered %s\n  fixture %s", a, bb)
	}
	return bamlutils.DeBAMLParseRequest{
		Raw:                    raw,
		StaticStreamDescriptor: &promptdescriptor.Function{Method: "SO", Return: desc},
	}
}

// soBundleJSON renders a bundle's exported shape for comparison. The unexported
// index maps carry `json:"-"`, so this compares the declarative content and not
// the rebuilt lookup tables.
func soBundleJSON(t *testing.T, b *schema.Bundle) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return string(raw)
}

// soRequireAdmitted is the control direction: the same shapes WITHOUT constraints
// must be admitted by the CONSTRAINT gate, so a decline elsewhere is attributable.
//
// checkSupported and checkSupportedFields are the constraint cut-line and must
// admit outright. SupportsNativeFinalBundle adds a further STREAM cut-line that
// legitimately declines some unconstrained shapes for reasons that have nothing to
// do with constraints (a single string-absorbing-field root class, a scalar map
// value). Those are allowed here — but only after their reason is checked NOT to be
// a constraint, so the allowance cannot absorb a constraint decline.
//
// It reports whether the control actually SERVED, because at least one must, or the
// suite would only ever have proven that nothing is emitted.
func soRequireAdmitted(t *testing.T, f servingOracleFixture) (served bool) {
	t.Helper()
	if err := checkSupported(f.Bundle); err != nil {
		t.Fatalf("the unconstrained control %s was DECLINED by checkSupported: %v", f.Name, err)
	}
	if err := checkSupportedFields(f.Bundle); err != nil {
		t.Fatalf("the unconstrained control %s was DECLINED by checkSupportedFields: %v", f.Name, err)
	}
	if err := SupportsNativeFinalBundle(f.Bundle); err != nil {
		soRequireNotAConstraintDecline(t, f.Name, "SupportsNativeFinalBundle", err)
		return false
	}
	res, err := ParseStaticBundle(context.Background(), f.Bundle, f.Raw)
	if err != nil {
		soRequireNotAConstraintDecline(t, f.Name, "ParseStaticBundle", err)
		return false
	}
	if len(res.JSON) == 0 {
		t.Fatalf("the unconstrained control %s was admitted but served nothing", f.Name)
	}
	// The ROOT entry point must agree with the one below it: a control that serves
	// through ParseStaticBundle must serve the SAME bytes through Parse, or the two
	// lanes disagree about an admitted shape.
	pres, perr := Parse(context.Background(), soParseRequestFor(t, f.Bundle, f.Raw))
	if perr != nil {
		t.Fatalf("the unconstrained control %s serves through ParseStaticBundle but Parse declined it: %v",
			f.Name, perr)
	}
	if string(pres.JSON) != string(res.JSON) {
		t.Fatalf("the unconstrained control %s serves different bytes through Parse and ParseStaticBundle:"+
			"\n  Parse            %s\n  ParseStaticBundle %s", f.Name, pres.JSON, res.JSON)
	}
	return true
}

// soRequireNotAConstraintDecline pins the ATTRIBUTION of a decline: every
// constraint decline in internal/debaml names "constraints" in its message
// (unsupported("type constraints"), "class constraints", "enum constraints",
// "target type constraints", "map key constraints", …), so a decline that names one
// on a bundle carrying none would mean the gate is refusing for the wrong reason.
func soRequireNotAConstraintDecline(t *testing.T, fixture, gate string, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "constraint") {
		t.Fatalf("the unconstrained control %s was declined by %s for a CONSTRAINT reason (%v), but it "+
			"carries no constraint", fixture, gate, err)
	}
	t.Logf("%s: %s declines for a non-constraint reason (%v); admission is still proven by checkSupported",
		fixture, gate, err)
}

// soWithDeBAMLFlag runs fn with BAML_REST_USE_DEBAML set to v (or unset for ""),
// restoring the previous state afterwards.
func soWithDeBAMLFlag(t *testing.T, v string, fn func()) {
	t.Helper()
	const key = "BAML_REST_USE_DEBAML"
	prev, had := os.LookupEnv(key)
	defer func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	}()
	if v == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	} else if err := os.Setenv(key, v); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	fn()
}

// TestServingOracleCanonicalMatchesProductionServing proves the decline is the
// ONLY thing the constraints change.
//
// For every constraint-bearing row it strips the constraints from the bundle,
// requires the stripped twin to be ADMITTED, and requires ParseStaticBundle — the
// real static serving entry point — to emit EXACTLY the canonical bytes the
// collector recorded for the constrained bundle.
//
// That makes the collector's CanonicalJSON an artifact of production serving
// rather than of the collector: if the two ever parted, one of them would be
// describing a value the serving path does not produce.
func TestServingOracleCanonicalMatchesProductionServing(t *testing.T) {
	compared, notServable := 0, 0
	for _, f := range servingOracleFixtures {
		if f.Fatal || f.Unconstrained {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			native := soRunNative(f)
			if native.Kind == soNativeCoercionError || native.Kind == soNativeNoCandidate ||
				native.Kind == soNativeUnmodelled {
				t.Skipf("no canonical value to compare: %s %s", native.Kind, native.Message)
			}
			stripped := soStripConstraints(f.Bundle)
			if err := checkSupported(stripped); err != nil {
				t.Fatalf("the constraint-stripped twin was still declined (%v); the decline is then NOT "+
					"constraint-specific for this shape", err)
			}
			res, err := ParseStaticBundle(context.Background(), stripped, f.Raw)
			if err != nil {
				// The stream cut-line inside SupportsNativeFinalBundle declines some
				// shapes for reasons that have nothing to do with constraints. Those are
				// reported, and their reason is checked NOT to be a constraint, so the
				// allowance cannot absorb a constraint decline.
				soRequireNotAConstraintDecline(t, f.Name, "ParseStaticBundle (stripped twin)", err)
				notServable++
				t.Skipf("the stripped twin is not servable for a non-constraint reason: %v", err)
			}
			if diff, ok := constraintStateJSONEquivalent([]byte(native.JSON), res.JSON); !ok {
				t.Fatalf("the collector's canonical document is not what production serving emits for the same "+
					"raw text — %s\n  collector %s\n  serving   %s", diff, native.JSON, res.JSON)
			}
			compared++
		})
	}
	if compared == 0 {
		t.Fatal("no row was compared against production serving; the claim would be vacuous")
	}
	if compared+notServable != soWantServingComparable+soWantServingNotComparable {
		t.Errorf("the serving comparison covered %d rows (%d compared, %d not servable) but the corpus "+
			"pins %d + %d", compared+notServable, compared, notServable,
			soWantServingComparable, soWantServingNotComparable)
	}
	if compared != soWantServingComparable || notServable != soWantServingNotComparable {
		t.Errorf("serving comparability changed: compared=%d (want %d), not-servable=%d (want %d). Both are "+
			"pinned so a shape moving between them has to be acknowledged.",
			compared, soWantServingComparable, notServable, soWantServingNotComparable)
	}
	t.Logf("collector canonical == ParseStaticBundle output for %d rows; %d rows are not servable for "+
		"non-constraint reasons", compared, notServable)
}

// soWantServingComparable / soWantServingNotComparable pin how much of the corpus
// can be compared against production serving, and how much cannot because the
// stream cut-line declines the shape for a NON-constraint reason. Both are pinned
// so a shape moving between them is acknowledged rather than absorbed.
const (
	soWantServingComparable    = 30
	soWantServingNotComparable = 17
)

// soStripConstraints returns a deep copy of b with every constraint removed.
func soStripConstraints(b *schema.Bundle) *schema.Bundle {
	out := &schema.Bundle{
		Target:           soStripType(b.Target),
		RecursiveClasses: append([]string(nil), b.RecursiveClasses...),
	}
	// The alias TARGETS are types and can carry constraints of their own; copying the
	// slice shallowly left them in the "stripped" twin.
	for _, a := range b.StructuralRecursiveAliases {
		out.StructuralRecursiveAliases = append(out.StructuralRecursiveAliases,
			schema.RecursiveAliasDef{Name: a.Name, Target: soStripType(a.Target)})
	}
	for _, c := range b.Classes {
		nc := c
		nc.Constraints = nil
		nc.Fields = nil
		for _, f := range c.Fields {
			nf := f
			nf.Type = soStripType(f.Type)
			nc.Fields = append(nc.Fields, nf)
		}
		out.Classes = append(out.Classes, nc)
	}
	for _, e := range b.Enums {
		ne := e
		ne.Constraints = nil
		out.Enums = append(out.Enums, ne)
	}
	if err := out.RebuildIndexes(); err != nil {
		panic("serving oracle: stripped bundle indexes: " + err.Error())
	}
	return out
}

// soStripType removes the constraints from a type and every type nested inside it.
// It must cover the same carriers as soBundleHasConstraint and
// servingOracleGateStripType, or a stripped twin and the detector would disagree
// about what a constraint-free clone is.
func soStripType(t schema.Type) schema.Type {
	out := t
	out.Meta.Constraints = nil
	if t.Elem != nil {
		out.Elem = ptr(soStripType(*t.Elem))
	}
	if t.Key != nil {
		out.Key = ptr(soStripType(*t.Key))
	}
	if t.Value != nil {
		out.Value = ptr(soStripType(*t.Value))
	}
	if len(t.Items) > 0 {
		out.Items = nil
		for _, it := range t.Items {
			out.Items = append(out.Items, soStripType(it))
		}
	}
	if t.Union != nil {
		u := *t.Union
		u.Variants = nil
		for _, v := range t.Union.Variants {
			u.Variants = append(u.Variants, soStripType(v))
		}
		out.Union = &u
	}
	if t.Arrow != nil {
		a := *t.Arrow
		a.Return = soStripType(t.Arrow.Return)
		a.Params = nil
		for _, p := range t.Arrow.Params {
			a.Params = append(a.Params, soStripType(p))
		}
		out.Arrow = &a
	}
	return out
}

// ---------------------------------------------------------------------------
// (6) The three asymmetries, asserted directly.
// ---------------------------------------------------------------------------

// TestServingOracleDuplicateLabel pins ASYMMETRY 2 from BOTH readbacks.
//
// From the raw CFFI tree, two @check attributes sharing one label are two ORDERED
// results with different outcomes. Through baml_go's own decoded readback they
// FOLD into a single map entry. Both are recorded: the ordered pair is the fact
// about BAML, the fold is a fact about its Go binding, and 7.2b owns the wire
// question with both on the table.
func TestServingOracleDuplicateLabel(t *testing.T) {
	f := soFixture(t, "duplicate_label")
	stock := soStockFor(t, f)
	if stock.Kind != soStockValue {
		t.Fatalf("expected a value, got %s", stock.render())
	}
	if len(stock.Sites) != 2 {
		t.Fatalf("the raw CFFI check collection has %d entries, want 2 — the duplicate-label observation "+
			"depends on both being present:\n  %s", len(stock.Sites), stock.render())
	}
	a, b := stock.Sites[0], stock.Sites[1]
	if a.Label != "dup" || b.Label != "dup" {
		t.Fatalf("both checks must carry the label %q; got %q and %q", "dup", a.Label, b.Label)
	}
	if a.Expression == b.Expression {
		t.Fatalf("the two checks must carry DIFFERENT expressions or the ordering claim is untestable; "+
			"both are %q", a.Expression)
	}
	if a.Status != "succeeded" || b.Status != "failed" {
		t.Fatalf("the two checks must have DIFFERENT results or a fold would be undetectable; got %q then %q",
			a.Status, b.Status)
	}

	// The FOLD itself is not reconstructed here — reconstructing it would prove
	// nothing about baml_go. It is measured against the real decode by
	// TestServingOracleRootCheckFoldIsLossy, which drives a root duplicate-label
	// probe through the CFFI and observes the decoded map keep one entry where this
	// nested pair keeps two.

	// Native keeps both, in declaration order.
	native := soRunNative(f)
	checks := soCheckSites(native)
	if len(checks) != 2 {
		t.Fatalf("native folded or dropped a duplicate label: %s", native.render())
	}
	if checks[0].Expression != a.Expression || checks[1].Expression != b.Expression {
		t.Fatalf("native's events are not in stock's declaration order:\n  stock  %s\n  native %s",
			soRenderStockSites(stock.Sites), soRenderNativeSites(checks))
	}
	// The field's kind is pinned structurally so a future refactor cannot quietly
	// turn the ordered slice into a map.
	if k := reflect.TypeOf(constraintCoercionState{}).Field(soFieldIndex(t, "Events")).Type.Kind(); k != reflect.Slice {
		t.Fatalf("constraintCoercionState.Events is a %s; it must stay a slice or duplicate labels fold", k)
	}
}

// soFieldIndex returns the index of a named field of constraintCoercionState.
func soFieldIndex(t *testing.T, name string) int {
	t.Helper()
	rt := reflect.TypeOf(constraintCoercionState{})
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Name == name {
			return i
		}
	}
	t.Fatalf("constraintCoercionState has no field %q", name)
	return -1
}

// TestServingOracleBareStringReturnSkips pins ASYMMETRY 1 from the stock side and
// the native side at once, with the CONTROL that makes it a property of the ROUTE
// rather than of strings.
func TestServingOracleBareStringReturnSkips(t *testing.T) {
	for _, name := range []string{"target_string_check_skipped", "target_string_assert_skipped"} {
		f := soFixture(t, name)
		stock := soStockFor(t, f)
		if stock.Kind != soStockValue {
			t.Fatalf("%s: a bare-string return must still SERVE the value; got %s", name, stock.render())
		}
		// Stock takes the assistant text VERBATIM for a bare-string return, so the
		// quotes are part of the value. Pinned exactly, because it is also what makes
		// this row a state divergence.
		if stock.Identity != `string:"\"actual\""` {
			t.Fatalf("%s: stock served %s, want the raw text verbatim", name, stock.Identity)
		}
		if len(stock.Sites) != 0 {
			t.Fatalf("%s: stock evaluated %d constraint(s) on a bare-string return; the skip is the "+
				"observation:\n  %s", name, len(stock.Sites), stock.render())
		}
		native := soRunNative(f)
		if native.Identity != `string:"actual"` {
			t.Fatalf("%s: native canonicalized %s, want the JSON string the text denotes", name, native.Identity)
		}
		if len(soCheckSites(native)) != 0 || len(native.Sites) != 0 {
			t.Fatalf("%s: native EVALUATED a predicate stock skipped: %s", name, native.render())
		}
		// Positive evidence, not absence: the collector records the counterfactual,
		// so the predicate is known to have been reached and to have decided false.
		if len(native.Skips) == 0 {
			t.Fatalf("%s: native recorded no skipped predicate; a skip proven only by absence is not "+
				"evidence: %s", name, native.render())
		}
		if !strings.Contains(native.render(), "~would-be-false") {
			t.Fatalf("%s: the skipped predicate carries no false counterfactual, so nothing shows it was "+
				"reached: %s", name, native.render())
		}
	}
	// CONTROL: the same false predicate one level down, on a string FIELD, DOES run
	// on both legs — so the skip is a property of the bare-string return route.
	ctl := soFixture(t, "scalar_string_check_fail")
	stock := soStockFor(t, ctl)
	if len(stock.Sites) != 1 || stock.Sites[0].Status != "failed" {
		t.Fatalf("control: the same predicate on a string FIELD must run and fail; got %s", stock.render())
	}
}

// TestServingOracleAliasCanonicalization pins ASYMMETRY 3 live: the model writes
// the alias, the predicate observes the canonical name, and the two spellings
// produce OPPOSITE results.
func TestServingOracleAliasCanonicalization(t *testing.T) {
	f := soFixture(t, "enum_alias_ingress")
	stock := soStockFor(t, f)
	if stock.Identity != "class:SoEnumAlias{v=enum:SoSuit=Hearts}" {
		t.Fatalf("stock did not canonicalize the alias: %s", stock.render())
	}
	if len(stock.Sites) != 2 {
		t.Fatalf("expected the canonical and alias predicates, got %s", stock.render())
	}
	if stock.Sites[0].Status != "succeeded" || stock.Sites[1].Status != "failed" {
		t.Fatalf("the canonical spelling must hold and the alias spelling must not; got %q then %q:\n  %s",
			stock.Sites[0].Status, stock.Sites[1].Status, stock.render())
	}
	native := soRunNative(f)
	if native.Identity != stock.Identity {
		t.Fatalf("native canonicalized differently:\n  stock  %s\n  native %s", stock.Identity, native.Identity)
	}
	checks := soCheckSites(native)
	if len(checks) != 2 || checks[0].Outcome != constraintOutcomeTrue || checks[1].Outcome != constraintOutcomeFalse {
		t.Fatalf("native did not reproduce the alias asymmetry: %s", native.render())
	}
}

// soFixture looks a fixture up by name, failing loudly on a rename so a test that
// names a row cannot silently start testing nothing.
func soFixture(t *testing.T, name string) servingOracleFixture {
	t.Helper()
	for _, f := range servingOracleFixtures {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no fixture named %q; the corpus has %v", name, soSortedNames(servingOracleFixtures))
	return servingOracleFixture{}
}

// ---------------------------------------------------------------------------
// (7) The path alignment is exercised, not merely defined.
// ---------------------------------------------------------------------------

// TestServingOracleAlignmentIsExercised proves the path normalisation is live.
//
// A normalisation that no row exercises makes the comparison looser than it reads,
// and its removal would not be noticed. It therefore requires a row that actually
// produces a dropped element, and asserts the mapping's effect on it in both
// directions.
func TestServingOracleAlignmentIsExercised(t *testing.T) {
	// dropped list element — the one normalisation, and it must bite.
	dn := soRunNative(soFixture(t, "list_dropped_elem"))
	drops := soDropsByPrefix(dn)
	if len(drops) == 0 {
		t.Fatalf("the dropped-element row produced no skipped element, so the index shift is dead code: %s",
			dn.render())
	}
	shifted := 0
	for _, s := range soCheckSites(dn) {
		if got := soAlignNativePath(s.Path, drops); got != s.Path {
			shifted++
		}
	}
	if shifted == 0 {
		t.Fatalf("no site's index was shifted by the drop, so the normalisation changes nothing here: %s",
			dn.render())
	}
	// And the mapping itself, in both directions, so a no-op implementation fails.
	if got := soShiftDroppedIndexes("$.v[2]", map[string][]int{"$.v": {1}}); got != "$.v[1]" {
		t.Fatalf("soShiftDroppedIndexes($.v[2], drop 1) = %q, want $.v[1]", got)
	}
	if got := soShiftDroppedIndexes("$.v[0]", map[string][]int{"$.v": {1}}); got != "$.v[0]" {
		t.Fatalf("soShiftDroppedIndexes must not shift an index BEFORE the drop; got %q", got)
	}

	// The PRODUCER half, on a REAL fixture rather than a hand-built map. A drop
	// inside a NESTED list must be recorded under its owning list's own indexed path
	// ($.v[1].w), which is the key the consumer looks up. Keying by the FIRST index
	// and discarding anything with a remaining suffix — which is what this did —
	// produced no key at all for a nested drop, so the nested support below could
	// only ever be exercised by a map no producer could emit.
	nested := soRunNative(soFixture(t, "list_nested_dropped_elem"))
	nestedDrops := soDropsByPrefix(nested)
	const nestedOwner = "$.v[1].w"
	if got, ok := nestedDrops[nestedOwner]; !ok || len(got) != 1 || got[0] != 1 {
		t.Fatalf("soDropsByPrefix produced %v; the nested drop must be keyed by its owning list %q with "+
			"input index 1, or the consumer's nested lookup can never match anything a producer emits",
			nestedDrops, nestedOwner)
	}
	// …and the CONSUMER half over the same run: the site after the dropped element
	// sits at input index 2 and must align onto stock's emitted index 1.
	alignedNested := 0
	for _, site := range soCheckSites(nested) {
		if site.Path != nestedOwner+"[2]" {
			continue
		}
		alignedNested++
		if got := soAlignNativePath(site.Path, nestedDrops); got != nestedOwner+"[1]" {
			t.Fatalf("nested producer->consumer: %s aligned to %q, want %q", site.Path, got, nestedOwner+"[1]")
		}
	}
	if alignedNested != 1 {
		t.Fatalf("the nested fixture produced %d site(s) after the dropped element, want 1: %s",
			alignedNested, nested.render())
	}

	// NESTED (hand-built): an inner list under an outer element whose earlier sibling was dropped.
	// The drop map is keyed in the collector's INPUT coordinates, so the inner lookup
	// must use the unshifted outer index even though the rendered path carries the
	// shifted one. Building the key from the emitted rendering — which is what this
	// did — misses, and the inner index is silently left unshifted.
	handBuilt := map[string][]int{
		"$.v":      {0}, // outer element 0 dropped: input [1] becomes emitted [0]
		"$.v[1].w": {0}, // inner element 0 of the SURVIVING outer element dropped
		"$.v[0].w": {3}, // a decoy under the emitted coordinate: must never be used
	}
	if got := soShiftDroppedIndexes("$.v[1].w[1]", handBuilt); got != "$.v[0].w[0]" {
		t.Fatalf("nested alignment: soShiftDroppedIndexes($.v[1].w[1]) = %q, want $.v[0].w[0]. The inner "+
			"index must be shifted using the INPUT-coordinate prefix $.v[1].w, not the emitted $.v[0].w.", got)
	}
	// The decoy proves the lookup is not merely succeeding by coincidence: keyed by
	// the emitted prefix it would shift the inner index by a different amount.
	if got := soShiftDroppedIndexes("$.v[1].w[4]", handBuilt); got != "$.v[0].w[3]" {
		t.Fatalf("nested alignment used the wrong coordinate system: got %q, want $.v[0].w[3]", got)
	}
}

// ---------------------------------------------------------------------------
// (8) A shape BAML will not even compile.
// ---------------------------------------------------------------------------

// TestServingOracleForeignMapKeyIsSourceRejected records the foreign non-string
// map key as what it actually is: not a divergence between two engines, but a
// shape the BAML PROJECT LANGUAGE refuses, so no stock leg can exist for it.
//
// It is asserted rather than assumed — a runtime is created from a project
// carrying exactly that declaration and the creation must FAIL — and native's
// decline of the same shape is pinned beside it.
func TestServingOracleForeignMapKeyIsSourceRejected(t *testing.T) {
	src := soPrelude + `
class SoForeignKey {
  m map<int, string>
}

function SoForeignKeyFn(topic: string) -> SoForeignKey {
  client ` + soClient + `
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
`
	_, err := soCreateRuntimeForSource(src)
	if err == nil {
		t.Fatal("stock BAML v0.223.0 now compiles a map with a non-string key; the source-rejected " +
			"classification has to be re-derived")
	}
	// The CFFI reports only "failed to create BAML runtime" through the error value
	// (the diagnostic goes to its own log), so the discrimination is a CONTROL rather
	// than a message match: the identical project with a STRING key must compile. The
	// only difference between the two sources is the key type, so a compile failure
	// on one and a success on the other attributes the rejection to it.
	ctlSrc := strings.Replace(src, "map<int, string>", "map<string, string>", 1)
	if ctlSrc == src {
		t.Fatal("the control source is identical to the rejected one; the substitution did not apply")
	}
	if _, ctlErr := soCreateRuntimeForSource(ctlSrc); ctlErr != nil {
		t.Fatalf("the string-keyed CONTROL project does not compile either (%v), so the rejection above "+
			"is not attributable to the key type", ctlErr)
	}
	t.Logf("recorded: stock refuses to COMPILE map<int, string> (%s) while the string-keyed twin compiles",
		soCollapse(err.Error()))

	// Native declines the same shape, and declines it for the map key rather than
	// incidentally.
	b := soBundle(soClassType("SoForeignKey"),
		[]schema.ClassDef{soClassOf("SoForeignKey", []schema.ClassField{
			soField("m", soMapOf(intType(), stringType())),
		})}, nil)
	if err := checkSupported(b); err == nil {
		t.Fatal("native ADMITTED a map with a non-string key that BAML will not even compile")
	} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("native declined the foreign map key with %v, want ErrDeBAMLParseUnsupported", err)
	}
	// CONTROL: the string-keyed twin IS admitted, so the decline is about the key.
	ctl := soBundle(soClassType("SoForeignKeyCtl"),
		[]schema.ClassDef{soClassOf("SoForeignKeyCtl", []schema.ClassField{
			soField("m", soMapOf(stringType(), stringType())),
		})}, nil)
	if err := checkSupported(ctl); err != nil {
		t.Fatalf("the string-keyed control was declined (%v); the foreign-key decline is then not "+
			"key-specific", err)
	}
}

// TestServingOracleNoUnionArmIsCoerced asserts the ABSENCE the alignment relies on,
// with the reason as its evidence.
//
// soAlignNativePath carries no union-arm normalisation because production coerce
// declines every constrained union — at a class field AND at the return type. That
// is a fact, not an assumption: both union rows are driven here and required to
// decline with the ErrDeBAMLParseUnsupported sentinel, and NO native site anywhere
// in the corpus may carry a `|armN` segment.
//
// If a future change admits constrained unions, this fails and whoever made the
// change has to re-derive the alignment with a fixture behind it.
func TestServingOracleNoUnionArmIsCoerced(t *testing.T) {
	unions := 0
	for _, f := range servingOracleFixtures {
		if f.Family != "union" {
			continue
		}
		unions++
		native := soRunNative(f)
		if native.Kind != soNativeCoercionError {
			t.Errorf("%s: production coerce no longer declines this constrained union (%s); the "+
				"union-arm path alignment removed from soAlignNativePath has to be restored, with this row "+
				"as its fixture", f.Name, native.render())
			continue
		}
		if !errors.Is(native.Err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: the union decline is not the unsupported sentinel (%v), so it is a failure rather "+
				"than a fall back to BAML", f.Name, native.Err)
		}
	}
	if unions == 0 {
		t.Fatal("no union fixture; the absence claim would be vacuous")
	}
	// And nothing anywhere produces an arm segment.
	for _, f := range servingOracleFixtures {
		if f.Fatal {
			continue
		}
		for _, s := range soRunNative(f).Sites {
			if soUnionArmRe.MatchString(s.Path) {
				t.Errorf("%s: a native site carries the union-arm segment %s, which stock's tree has no step "+
					"for; soAlignNativePath must normalise it away again", f.Name, s.Path)
			}
		}
	}
	t.Logf("no union arm is coerced: %d union fixtures, all declined by production coerce", unions)
}

// TestServingOracleKnownGap_BareStringQuotedText records a PRE-EXISTING divergence
// this oracle found, in an UNCONSTRAINED shape that production admits and serves.
//
// A bare-string return type is admitted (boundary_decline_test.go's "bare string
// return" control) and served by ParseStaticBundle. Given QUOTED assistant text the
// two engines produce different values: stock takes the text verbatim, quotes
// included, while the native static path extracts the JSON string it denotes.
//
// Nothing in Slice 7.2a-3 causes this and nothing here fixes it — this slice is
// test-only and changes no production byte. The test exists so the gap is measured
// rather than remembered, and it FAILS when the gap closes, with instructions.
func TestServingOracleKnownGap_BareStringQuotedText(t *testing.T) {
	f := soFixture(t, "bare_string_quoted_text")
	if !f.Unconstrained {
		t.Fatal("the row must be UNCONSTRAINED; a constraint-bearing bundle declines at the gate and would " +
			"demonstrate nothing about what production serves")
	}
	stock := soStockFor(t, f)
	if stock.Kind != soStockValue {
		t.Fatalf("stock did not serve a value: %s", stock.render())
	}
	if stock.Identity != `string:"\"actual\""` {
		t.Fatalf("GAP CLOSED or CHANGED — stock now canonicalizes the quoted text as %s. Re-derive this "+
			"test, or delete it if the two engines now agree.", stock.Identity)
	}

	// The production serving path, not the collector: this is what a caller receives.
	if err := checkSupported(f.Bundle); err != nil {
		t.Fatalf("GAP CLOSED — the bare-string target no longer admits (%v). Delete this test and the "+
			"bare_string_quoted_text row.", err)
	}
	res, err := ParseStaticBundle(context.Background(), f.Bundle, f.Raw)
	if err != nil {
		t.Fatalf("GAP CLOSED — ParseStaticBundle now declines the quoted bare-string text (%v). Delete this "+
			"test and the bare_string_quoted_text row.", err)
	}
	if got := string(res.JSON); got != `"actual"` {
		t.Fatalf("GAP CLOSED or CHANGED — production serving now emits %s. Re-derive this test.", got)
	}
	if diff, ok := constraintStateJSONEquivalent(res.JSON, []byte(stock.JSON)); ok {
		t.Fatalf("GAP CLOSED — production serving and stock now agree on the quoted bare-string text. "+
			"Delete this test and the bare_string_quoted_text row. (%s)", diff)
	}
	t.Logf("KNOWN GAP measured: stock serves %s, production ParseStaticBundle serves %s for the same "+
		"assistant text %q on an ADMITTED, unconstrained bare-string return. Pre-existing, unrelated to "+
		"constraints, and out of scope for this TEST-ONLY slice.", stock.JSON, res.JSON, f.Raw)
}

// TestServingOracleRootCheckFoldIsLossy MEASURES the one place the stock envelope
// cannot be read raw, using the real baml_go decode rather than a reconstruction.
//
// A root Checked's collection reaches Go already folded into a
// map[string]shared.Check (see soChecked for why there is no hook). This drives a
// root function declaring TWO @check attributes under one label through the actual
// CFFI and observes the decoded map keep ONE entry — and drives the SAME two checks
// one level down, where the raw tree keeps BOTH, in order, with different results.
//
// The pair is what makes the loss a measurement: without the nested control, one
// entry could mean stock evaluated once.
func TestServingOracleRootCheckFoldIsLossy(t *testing.T) {
	soEnsureRuntime(t)

	root := soProbe(t, "probe_root_duplicate_label")
	declared := root.Bundle.Target.Meta.Constraints
	if len(declared) != 2 || *declared[0].Label != *declared[1].Label {
		t.Fatalf("the probe must declare TWO checks under ONE label or it measures nothing; got %d",
			len(declared))
	}
	if declared[0].Expression == declared[1].Expression {
		t.Fatal("the two checks must carry DIFFERENT expressions, or the fold would be undetectable")
	}

	res, err := soDriveProbe(root)
	if err != nil {
		t.Fatalf("drive %s: %v", root.method(), err)
	}
	folded, ok := res.(soChecked)
	if !ok {
		t.Fatalf("a root-level @check must decode as a Checked; got %T", res)
	}
	if len(folded.Checks) != 1 {
		t.Fatalf("baml_go's ROOT readback now keeps %d entries for two @checks sharing a label. The fold "+
			"this oracle works around is gone: read the ordered collection directly and delete "+
			"soFoldedSites' schema-derived ordering.", len(folded.Checks))
	}
	kept, present := folded.Checks[*declared[0].Label]
	if !present {
		t.Fatalf("the folded map does not carry the declared label %q: %v", *declared[0].Label, folded.Checks)
	}
	// The surviving entry is one of the two declared expressions — which one is
	// baml_go's map-write order and is not a claim this test makes.
	if kept.Expression != declared[0].Expression && kept.Expression != declared[1].Expression {
		t.Fatalf("the surviving entry carries %q, which is neither declared expression", kept.Expression)
	}

	// The NESTED twin: the raw tree keeps both, ordered, with different results.
	nested := soProbe(t, "probe_nested_duplicate_label")
	nres, err := soDriveProbe(nested)
	if err != nil {
		t.Fatalf("drive %s: %v", nested.method(), err)
	}
	var sites []soStockSite
	if _, _, err := soReadStockResult(nested.Bundle, nres, "$", &sites); err != nil {
		t.Fatalf("read %s: %v", nested.method(), err)
	}
	if len(sites) != 2 {
		t.Fatalf("the NESTED twin must keep BOTH checks from the raw tree, or the root count of 1 is not a "+
			"loss; got %d: %s", len(sites), soRenderStockSites(sites))
	}
	if sites[0].Label != sites[1].Label {
		t.Fatalf("the nested pair lost the shared label: %s", soRenderStockSites(sites))
	}
	if sites[0].Expression == sites[1].Expression || sites[0].Status == sites[1].Status {
		t.Fatalf("the nested pair must differ in expression AND result, or the ordering claim is "+
			"untestable: %s", soRenderStockSites(sites))
	}
	if sites[0].Expression != declared[0].Expression || sites[1].Expression != declared[1].Expression {
		t.Fatalf("the nested pair is not in DECLARATION order:\n  got  %s\n  want %s then %s",
			soRenderStockSites(sites), declared[0].Expression, declared[1].Expression)
	}
	t.Logf("measured: baml_go's root readback keeps %d of %d checks sharing a label; the nested twin keeps "+
		"both in declaration order", len(folded.Checks), len(declared))
}

// TestServingOracleRootFoldCompletenessBites proves the guard that stands in for
// the missing raw root: soFoldedSites fails when the fold LOST an entry.
//
// The root duplicate-label probe is exactly that case, so it is fed through the
// same path a fixture takes and required to be REFUSED rather than silently
// reported as one check.
func TestServingOracleRootFoldCompletenessBites(t *testing.T) {
	soEnsureRuntime(t)
	root := soProbe(t, "probe_root_duplicate_label")
	res, err := soDriveProbe(root)
	if err != nil {
		t.Fatalf("drive %s: %v", root.method(), err)
	}
	var sites []soStockSite
	if _, _, err := soReadStockResult(root.Bundle, res, "$", &sites); err == nil {
		t.Fatalf("a LOST folded entry was accepted and reported as %s; the root envelope would be "+
			"silently incomplete", soRenderStockSites(sites))
	} else if !strings.Contains(err.Error(), "folds the check collection") {
		t.Fatalf("the refusal does not name the fold: %v", err)
	}
	// UNCONDITIONALITY, in the two states the old map-conditional refusal let
	// through. A declared duplicate cannot be represented by a map-keyed readback
	// whatever the map contains, so both of these must refuse.
	dupBundle := root.Bundle
	for name, checks := range map[string]map[string]shared.Check{
		"an EMPTY folded map": {},
		"a map whose surviving entry is a DIFFERENT label": {
			"other": {Name: "other", Expression: "this > 1", Status: "succeeded"},
		},
	} {
		if _, err := soFoldedSites(dupBundle, "$", checks); err == nil {
			t.Errorf("a root node declaring a duplicate label was ACCEPTED with %s; the refusal must be "+
				"driven by the DECLARATION, not by what survived the fold", name)
		} else if !strings.Contains(err.Error(), "folds the check collection") {
			t.Errorf("with %s the refusal does not name the fold: %v", name, err)
		}
	}

	// The COUNT half of the guard, witnessed directly: stock reporting a label the
	// schema does not declare there means the readback and the schema describe
	// different nodes, and no corpus row can produce it.
	lossless := soFixture(t, "target_int_check_fail")
	extra := map[string]shared.Check{
		"gt":       {Name: "gt", Expression: "this > 100", Status: "failed"},
		"surprise": {Name: "surprise", Expression: "this > 1", Status: "succeeded"},
	}
	if _, err := soFoldedSites(lossless.Bundle, "$", extra); err == nil {
		t.Error("a folded collection carrying a label the schema does not declare was ACCEPTED; the root " +
			"envelope would silently describe a different node")
	}
	// ...and the same call without the stray entry is accepted, so the count check
	// is about the mismatch rather than about folded collections in general.
	delete(extra, "surprise")
	if got, err := soFoldedSites(lossless.Bundle, "$", extra); err != nil {
		t.Errorf("the matching control was refused: %v", err)
	} else if len(got) != 1 || got[0].Path != "$" {
		t.Errorf("the matching control produced %s", soRenderStockSites(got))
	}

	// CONTROL: a root fixture whose fold is LOSSLESS is accepted, so the guard is
	// about the loss rather than about root checks in general.
	f := soFixture(t, "target_int_check_fail")
	stock := soStockFor(t, f)
	if len(stock.Sites) != 1 || stock.Sites[0].Path != "$" {
		t.Fatalf("the lossless root control must report exactly one site at $; got %s",
			soRenderStockSites(stock.Sites))
	}
}

// soProbe looks a probe up by name.
func soProbe(t *testing.T, name string) servingOracleProbe {
	t.Helper()
	for _, p := range servingOracleProbes {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no probe named %q", name)
	return servingOracleProbe{}
}

// soDriveProbe runs one probe through the stock CFFI and returns the DECODED
// result, so a test can observe what baml_go itself produced.
func soDriveProbe(p servingOracleProbe) (any, error) {
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": p.Raw, "stream": false},
		Env:    soRuntimeEnv,
	}
	encoded, err := args.Encode()
	if err != nil {
		return nil, err
	}
	return soRuntime.CallFunctionParse(context.Background(), p.method(), encoded)
}

// TestServingOracleRootEnvelopeCertificationIsUnavailable states, as an assertion
// rather than as a comment, what this oracle CANNOT certify.
//
// baml_go folds a root Checked's collection into a map before any registered type
// is touched, so at a root position stock's ORDER and MULTIPLICITY are destroyed
// before the oracle can observe them. The honest response is not to reconstruct
// them from the schema and present the result as observed — it is to mark those
// sites uncertified, carry the mark into the pinned envelope, and stop the
// comparator from claiming an order it never saw.
//
// This test pins all three, and pins that the limitation is NARROW: every nested
// site in the corpus is certified.
func TestServingOracleRootEnvelopeCertificationIsUnavailable(t *testing.T) {
	soEnsureRuntime(t)
	certified, uncertified := 0, 0
	rowsWithUncertified := []string{}
	for _, f := range servingOracleFixtures {
		if f.Fatal {
			continue
		}
		stock := soStockFor(t, f)
		rowUncertified := 0
		for _, s := range stock.Sites {
			if s.Certified {
				certified++
				if strings.Contains(s.render(), "~uncertified-order") {
					t.Errorf("%s: a CERTIFIED site renders the uncertified marker: %s", f.Name, s.render())
				}
				continue
			}
			uncertified++
			rowUncertified++
			// The mark must reach the PINNED envelope, or the corpus would not record
			// which rows rest on unobservable order.
			if !strings.Contains(s.render(), "~uncertified-order") {
				t.Errorf("%s: an uncertified site does not render the marker: %s", f.Name, s.render())
			}
			// Only ROOT positions can be uncertified: everything nested is read from
			// the raw tree.
			if !soIsRootPath(s.Path) {
				t.Errorf("%s: the NESTED site %s is uncertified; only a root collection is folded",
					f.Name, s.render())
			}
		}
		if rowUncertified > 0 {
			rowsWithUncertified = append(rowsWithUncertified, f.Name)
			if !strings.Contains(f.Stock, "~uncertified-order") {
				t.Errorf("%s has uncertified root evidence but its PINNED stock envelope does not say so: %s",
					f.Name, f.Stock)
			}
		}
	}
	if uncertified == 0 {
		t.Fatal("no fixture produced an uncertified root site; the unavailability this test describes would " +
			"be a claim about nothing")
	}
	if certified == 0 {
		t.Fatal("no fixture produced a certified site; the limitation would look total rather than narrow")
	}
	sort.Strings(rowsWithUncertified)
	t.Logf("root-envelope certification UNAVAILABLE for %d site(s) across %v; %d nested sites are certified "+
		"from the raw CFFI tree", uncertified, rowsWithUncertified, certified)
}

// soIsRootPath reports whether a path is one of the three ROOT positions a folded
// collection can occur at.
func soIsRootPath(path string) bool {
	if path == "$" {
		return true
	}
	m := soIndexRe.FindStringSubmatch(path)
	if m != nil && m[1] == "$" && m[3] == "" {
		return true
	}
	return strings.HasPrefix(path, `$["`) && strings.HasSuffix(path, `"]`)
}

// TestServingOracleUncertifiedComparisonStillBinds proves the weaker standard is
// still a standard: order is not claimed, but label, expression, path and result
// are, in both directions.
func TestServingOracleUncertifiedComparisonStillBinds(t *testing.T) {
	site := func(label, expr, status string) soStockSite {
		return soStockSite{Path: "$", Label: label, Expression: expr, Status: status}
	}
	nat := func(label, expr string, o constraintStateOutcome) soNativeSite {
		return soNativeSite{Path: "$", Level: schema.ConstraintCheck, Labeled: true,
			Label: label, Expression: expr, Outcome: o}
	}
	stock := []soStockSite{site("a", "this > 0", "succeeded"), site("b", "this > 9", "failed")}

	// Order is NOT claimed: the same pair in the opposite order is accepted.
	swapped := []soNativeSite{nat("b", "this > 9", constraintOutcomeFalse), nat("a", "this > 0", constraintOutcomeTrue)}
	if got := soCompareUncertifiedSites(stock, swapped); len(got) > 0 {
		t.Fatalf("a reordered but otherwise identical collection must be accepted, because the order was "+
			"never observed; got %v", got)
	}
	// Everything else still binds.
	for _, tc := range []struct {
		name   string
		native []soNativeSite
	}{
		{"a differing result", []soNativeSite{
			nat("a", "this > 0", constraintOutcomeFalse), nat("b", "this > 9", constraintOutcomeFalse)}},
		{"a differing expression", []soNativeSite{
			nat("a", "this > 1", constraintOutcomeTrue), nat("b", "this > 9", constraintOutcomeFalse)}},
		{"a missing predicate", []soNativeSite{nat("a", "this > 0", constraintOutcomeTrue)}},
		{"an extra DECIDED predicate", []soNativeSite{
			nat("a", "this > 0", constraintOutcomeTrue), nat("b", "this > 9", constraintOutcomeFalse),
			nat("c", "this > 2", constraintOutcomeTrue)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := soCompareUncertifiedSites(stock, tc.native); len(soViolations(got)) == 0 {
				t.Fatalf("%s was accepted; the uncertified comparison drops ORDER only", tc.name)
			}
		})
	}
	// An extra DECLINED predicate is not a violation: declining is always allowed.
	extraDeclined := append(append([]soNativeSite(nil), swapped...), nat("c", "this > 2", constraintOutcomeUnsupported))
	if got := soCompareUncertifiedSites(stock, extraDeclined); len(soViolations(got)) > 0 {
		t.Fatalf("an extra DECLINED predicate must be accepted; got %v", got)
	}
}

// TestServingOracleStripAndDetectCoverEveryCarrier pins that the constraint
// DETECTOR and BOTH stripped-twin builders traverse the same schema surface, and
// that stripping removes constraints without erasing anything else.
//
// They had drifted apart in three ways, each of which would have made an
// attribution claim about a bundle the gate never saw: the untagged stripper
// skipped Type.Items and rebuilt a bundle without RecursiveClasses or the
// structural aliases; the integration stripper shallow-copied the aliases, leaving
// their targets' constraints in the "stripped" twin; and the detector never looked
// inside an alias target, so a carrier could be classified unconstrained.
//
// The witness is synthetic on purpose: no corpus fixture uses these carriers today,
// which is exactly why the drift was invisible.
func TestServingOracleStripAndDetectCoverEveryCarrier(t *testing.T) {
	label := func(s string) *string { return &s }
	constrained := func(base schema.Type) schema.Type {
		base.Meta.Constraints = []schema.Constraint{
			{Level: schema.ConstraintCheck, Expression: "this > 0", Label: label("carrier")},
		}
		return base
	}

	// shapeGate names which production gate owns each carrier's SHAPE rejection, so
	// the control below asserts the accurate thing rather than a blanket "still
	// declines". They are different gates and Parse runs both: ValidateOutput rejects
	// tuple/arrow/top/media before checkSupported ever sees the bundle, while a
	// structural recursive alias is checkSupported's own blanket decline.
	const (
		shapeGateValidateOutput = "ValidateOutput"
		shapeGateCheckSupported = "checkSupported"
	)
	carriers := []struct {
		name      string
		shapeGate string
		// gateSeesConstraint says whether checkSupported's own target walk REACHES a
		// constraint in this carrier. Measured, not assumed, and it is not uniform:
		//
		//   tuple item      YES — checkTypeNoConstraints descends into Type.Items
		//   arrow param     NO  — it does not descend into Arrow at all
		//   arrow return    NO  — likewise
		//   alias target    NO  — the blanket structural-recursive-alias decline fires
		//                         first, so the walk never reaches the target
		//
		// That is not an over-claim anywhere: ValidateOutput rejects tuple/arrow/top
		// before Parse reaches checkSupported, and the alias declines outright. It does
		// mean the detector/stripper symmetry is the ONLY thing keeping those carriers
		// honest, which is exactly why this witness exists.
		gateSeesConstraint bool
		bundle             func() *schema.Bundle
	}{
		{"a constrained TUPLE item", shapeGateValidateOutput, true, func() *schema.Bundle {
			return &schema.Bundle{Target: schema.Type{
				Kind:  schema.TypeTuple,
				Items: []schema.Type{stringType(), constrained(intType())},
			}}
		}},
		{"a constrained STRUCTURAL-ALIAS target", shapeGateCheckSupported, false, func() *schema.Bundle {
			return &schema.Bundle{
				Target: stringType(),
				StructuralRecursiveAliases: []schema.RecursiveAliasDef{{
					Name:   "JsonValue",
					Target: schema.Type{Kind: schema.TypeList, Elem: ptr(constrained(intType()))},
				}},
			}
		}},
		{"a constrained ARROW return", shapeGateValidateOutput, false, func() *schema.Bundle {
			return &schema.Bundle{Target: schema.Type{
				Kind:  schema.TypeArrow,
				Arrow: &schema.ArrowType{Params: []schema.Type{stringType()}, Return: constrained(intType())},
			}}
		}},
		// Arrow.Params is a SECOND []schema.Type carrier inside the same node, and the
		// return witness above cannot speak for it: with only that row, deleting the
		// Params traversal from the detector or from either stripper left every witness
		// green. The unconstrained sibling parameter and the unconstrained return are
		// what make the structural-preservation assertions below discriminating — a
		// "fix" that dropped Params entirely would remove the constraint and destroy
		// the shape.
		{"a constrained ARROW parameter", shapeGateValidateOutput, false, func() *schema.Bundle {
			return &schema.Bundle{Target: schema.Type{
				Kind: schema.TypeArrow,
				Arrow: &schema.ArrowType{
					Params: []schema.Type{stringType(), constrained(intType())},
					Return: stringType(),
				},
			}}
		}},
	}

	for _, c := range carriers {
		t.Run(c.name, func(t *testing.T) {
			b := c.bundle()
			// 1. The DETECTOR sees it.
			if !soBundleHasConstraint(b) {
				t.Fatalf("soBundleHasConstraint does not look inside %s; a carrier would be misclassified "+
					"as an unconstrained control and assert the opposite of what it claims", c.name)
			}
			// 2. BOTH strippers remove it.
			for name, stripped := range map[string]*schema.Bundle{
				"soStripConstraints":     soStripConstraints(b),
				"servingOracleGateStrip": servingOracleGateStrip(b),
			} {
				if soBundleHasConstraint(stripped) {
					t.Errorf("%s left the constraint in %s; the 'stripped twin is admitted' attribution "+
						"would be about a bundle that still carries one", name, c.name)
				}
			}
			// 3. The twin still declines, and NOT for a constraint. These shapes
			// (tuple, arrow, structural alias) are outside the native profile on their
			// own, so the strip must remove the constraint reason while leaving the
			// shape reason — claiming a constraint-specific admission here would be
			// claiming something the gate never grants.
			// Whether checkSupported REACHES this carrier's constraint is measured and
			// pinned, in both directions, so the table's claim about each gate stays
			// true rather than becoming folklore.
			gateErr := checkSupported(b)
			sawConstraint := gateErr != nil && strings.Contains(gateErr.Error(), "constraint")
			if sawConstraint != c.gateSeesConstraint {
				t.Fatalf("%s: checkSupported reaching this carrier's constraint = %v, want %v (err %v). The "+
					"table records which gate owns which carrier; a change here means the walk moved.",
					c.name, sawConstraint, c.gateSeesConstraint, gateErr)
			}
			for name, stripped := range map[string]*schema.Bundle{
				"soStripConstraints":     soStripConstraints(b),
				"servingOracleGateStrip": servingOracleGateStrip(b),
			} {
				// The CONSTRAINT reason must be gone — asserted where checkSupported
				// reaches the carrier at all, and vacuously true where it does not.
				if err := checkSupported(stripped); err != nil && strings.Contains(err.Error(), "constraint") {
					t.Errorf("%s left %s declining for a CONSTRAINT reason (%v); the constraint was supposed "+
						"to be gone", name, c.name, err)
				} else if c.gateSeesConstraint && err != nil {
					t.Errorf("%s left %s declining at checkSupported for %v; the constraint was the only "+
						"reason that gate had", name, c.name, err)
				}
				// …and the SHAPE reason must remain, at whichever gate owns it.
				switch c.shapeGate {
				case shapeGateValidateOutput:
					if err := stripped.ValidateOutput(); err == nil {
						t.Errorf("%s produced a twin of %s that ValidateOutput now ACCEPTS; the shape fact "+
							"was erased along with the constraint", name, c.name)
					}
				case shapeGateCheckSupported:
					err := checkSupported(stripped)
					if err == nil {
						t.Errorf("%s produced a twin of %s that checkSupported now ADMITS; the shape fact "+
							"(the structural recursive alias) was erased along with the constraint",
							name, c.name)
					}
				default:
					t.Fatalf("%s: unknown shape gate %q", c.name, c.shapeGate)
				}
			}

			// 4. And nothing ELSE is erased: the strip must be a constraint-free CLONE,
			// not a different bundle. A recursion fact dropped here would remove an
			// independently decline-causing property from the twin.
			for name, stripped := range map[string]*schema.Bundle{
				"soStripConstraints":     soStripConstraints(b),
				"servingOracleGateStrip": servingOracleGateStrip(b),
			} {
				if len(stripped.StructuralRecursiveAliases) != len(b.StructuralRecursiveAliases) {
					t.Errorf("%s dropped %d structural recursive alias(es) from %s", name,
						len(b.StructuralRecursiveAliases)-len(stripped.StructuralRecursiveAliases), c.name)
				}
				for i, a := range stripped.StructuralRecursiveAliases {
					if a.Name != b.StructuralRecursiveAliases[i].Name {
						t.Errorf("%s renamed a structural alias: %q -> %q", name,
							b.StructuralRecursiveAliases[i].Name, a.Name)
					}
				}
				if len(stripped.Target.Items) != len(b.Target.Items) {
					t.Errorf("%s dropped tuple items from %s (%d -> %d)", name, c.name,
						len(b.Target.Items), len(stripped.Target.Items))
				}
				if (stripped.Target.Arrow == nil) != (b.Target.Arrow == nil) {
					t.Errorf("%s dropped the arrow payload from %s", name, c.name)
					continue
				}
				if b.Target.Arrow != nil {
					// Removing a parameter's constraint by removing the PARAMETER would
					// satisfy the detector while destroying the shape, so the arity and
					// each parameter's kind are pinned, as is the return.
					if len(stripped.Target.Arrow.Params) != len(b.Target.Arrow.Params) {
						t.Errorf("%s changed the arrow arity of %s (%d -> %d)", name, c.name,
							len(b.Target.Arrow.Params), len(stripped.Target.Arrow.Params))
						continue
					}
					for i, p := range stripped.Target.Arrow.Params {
						if p.Kind != b.Target.Arrow.Params[i].Kind {
							t.Errorf("%s changed arrow parameter %d of %s from %s to %s", name, i, c.name,
								b.Target.Arrow.Params[i].Kind, p.Kind)
						}
						if len(p.Meta.Constraints) > 0 {
							t.Errorf("%s left a constraint on arrow parameter %d of %s", name, i, c.name)
						}
					}
					if stripped.Target.Arrow.Return.Kind != b.Target.Arrow.Return.Kind {
						t.Errorf("%s changed the arrow return of %s from %s to %s", name, c.name,
							b.Target.Arrow.Return.Kind, stripped.Target.Arrow.Return.Kind)
					}
				}
			}
		})
	}

	// RECURSION METADATA, which is the fact whose loss would be most misleading: a
	// bundle that declines for recursion must still decline after stripping, or the
	// attribution would credit the constraints for a shape decline.
	rec := &schema.Bundle{
		Target:           soClassType("Node"),
		Classes:          []schema.ClassDef{soClassOf("Node", []schema.ClassField{soField("v", constrained(intType()))})},
		RecursiveClasses: []string{"Node"},
	}
	for name, stripped := range map[string]*schema.Bundle{
		"soStripConstraints":     soStripConstraints(rec),
		"servingOracleGateStrip": servingOracleGateStrip(rec),
	} {
		if len(stripped.RecursiveClasses) != 1 || stripped.RecursiveClasses[0] != "Node" {
			t.Errorf("%s erased RecursiveClasses; the stripped twin would be ADMITTED for a bundle whose "+
				"recursion declines independently of its constraints, and the attribution would be false",
				name)
		}
		if err := checkSupported(stripped); err == nil {
			t.Errorf("%s produced a twin that is ADMITTED even though the original declines for recursion; "+
				"the constraint attribution would be crediting the wrong cause", name)
		}
	}
}
