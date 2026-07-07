package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateDeBAMLEmission pins the guard logic: de-BAML configured AND
// emitted is the happy path (no error); configured-but-not-emitted is the
// silent-inert case that must error; not-configured is always fine.
func TestValidateDeBAMLEmission(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		emitted bool
		wantErr bool
	}{
		{"not configured, not emitted", "", false, false},
		{"not configured, emitted (impossible but safe)", "", true, false},
		{"configured and emitted (happy path)", "Baml_Rest_Dynamic", true, false},
		{"configured but not emitted (silent-inert)", "Baml_Rest_Dynamic", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeBAMLEmission(tc.method, tc.emitted)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for method=%q emitted=%v, got nil", tc.method, tc.emitted)
				}
				if !strings.Contains(err.Error(), tc.method) {
					t.Errorf("error should name the configured method %q: %v", tc.method, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error for method=%q emitted=%v: %v", tc.method, tc.emitted, err)
			}
		})
	}
}

// TestValidateDeBAMLRenderEmission_KeyedOnRenderBit pins CU2: the render
// guard is satisfied ONLY by the render-injection bit
// (emittedDeBAMLRenderCall), never by the broader helper-needed bit
// (emittedDeBAMLCall) that a native-parse emission also sets. A generator
// that emitted a parse-side native call but NOT the render injection must
// still FAIL, so a render-side-inert de-BAML build cannot ship green.
func TestValidateDeBAMLRenderEmission_KeyedOnRenderBit(t *testing.T) {
	// Parse helper emitted, render injection NOT emitted -> must fail.
	g := &generator{
		opts:                    Options{DeBAMLDynamicMethod: "Baml_Rest_Dynamic"},
		emittedDeBAMLCall:       true,  // a native-parse call set this
		emittedDeBAMLRenderCall: false, // but the render injection never emitted
	}
	if err := g.validateDeBAMLRenderEmission(); err == nil {
		t.Fatal("guard must fail when only the parse call emitted (render injection missing)")
	}
	// Render injection emitted -> ok.
	g.emittedDeBAMLRenderCall = true
	if err := g.validateDeBAMLRenderEmission(); err != nil {
		t.Fatalf("guard must pass once the render injection is emitted: %v", err)
	}
	// Not configured -> always ok regardless of bits.
	unconfigured := &generator{opts: Options{}, emittedDeBAMLCall: true}
	if err := unconfigured.validateDeBAMLRenderEmission(); err != nil {
		t.Fatalf("unconfigured de-BAML must never error: %v", err)
	}
}

// TestGenerate_FailsWhenDeBAMLConfiguredButNotEmitted pins the generate()
// integration: a configured DeBAMLDynamicMethod that matches no emitted
// method (the stub root introspection emits no methods, so nothing sets
// emittedDeBAMLCall) must panic rather than silently shipping a
// de-BAML-inert build. This also verifies the emittedDeBAMLCall flag is
// genuinely threaded from emission into the guard: if it weren't, every
// build would trip this — but the dynclient genadapter (a real matching
// method) generates cleanly, proving the other direction.
func TestGenerate_FailsWhenDeBAMLConfiguredButNotEmitted(t *testing.T) {
	pkgs := DefaultPackageConfig()
	// Redirect output into a temp dir so a regression that lets generation
	// reach Save can't pollute the codegen package directory.
	pkgs.OutputPath = filepath.Join(t.TempDir(), "adapter.go")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: DeBAMLDynamicMethod configured but no closure emitted the call")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "no BuildRequest closure emitted") {
			t.Errorf("panic message missing the de-BAML emission guard text: %q", msg)
		}
		if !strings.Contains(msg, "Baml_Rest_Dynamic_NoSuchMethod") {
			t.Errorf("panic message should name the configured method: %q", msg)
		}
	}()

	generate(Options{
		SelfPkg:             "github.com/invakid404/baml-rest/adapters/test_debaml_guard",
		SupportsWithClient:  false, // stub introspection has nil Request/StreamRequest
		DeBAMLDynamicMethod: "Baml_Rest_Dynamic_NoSuchMethod",
		Packages:            pkgs,
	})
}

// TestMaybeWriteDeBAMLHelper_WritesWhenEmitted pins that the helper is
// written next to adapter.go (package + import paths parameterized) when a
// de-BAML call was emitted.
func TestMaybeWriteDeBAMLHelper_WritesWhenEmitted(t *testing.T) {
	dir := t.TempDir()
	g := &generator{
		pkgs: PackageConfig{
			OutputPath:         filepath.Join(dir, "adapter.go"),
			OutputPkgName:      "genx",
			InterfacesPkg:      "github.com/invakid404/baml-rest/bamlutils",
			GeneratedClientPkg: "example.com/x/baml_client",
		},
		emittedDeBAMLCall: true,
	}
	g.maybeWriteDeBAMLHelper()

	b, err := os.ReadFile(filepath.Join(dir, "debaml.go"))
	if err != nil {
		t.Fatalf("debaml.go not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"package genx",
		`bamlutils "github.com/invakid404/baml-rest/bamlutils"`,
		`types "example.com/x/baml_client/types"`,
		`streamtypes "example.com/x/baml_client/stream_types"`,
		"func maybeApplyDeBAMLOutputFormat(",
		"func rewriteOutputFormat(",
		// The parse-stream (M4d native-first) seam twin is always emitted with
		// the final seam.
		"func maybeParseDeBAMLFinal(",
		"func maybeParseDeBAMLStream(",
		"func wrapDeBAMLDynamicOutputStream(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted debaml.go missing %q\n%s", want, got)
		}
	}
}

// TestMaybeWriteDeBAMLHelper_RemovesStaleWhenNotEmitted pins the
// idempotency fix: a generation that emits no de-BAML call removes any
// debaml.go a prior generation left in the same output directory, so the
// package can't carry an orphaned helper.
func TestMaybeWriteDeBAMLHelper_RemovesStaleWhenNotEmitted(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "debaml.go")
	if err := os.WriteFile(stale, []byte("package genx\n"), 0o644); err != nil {
		t.Fatalf("seed stale helper: %v", err)
	}
	g := &generator{
		pkgs:              PackageConfig{OutputPath: filepath.Join(dir, "adapter.go")},
		emittedDeBAMLCall: false,
	}
	g.maybeWriteDeBAMLHelper()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale debaml.go not removed (stat err = %v)", err)
	}
}

// TestMaybeWriteDeBAMLHelper_NoopWhenNoneAndAbsent pins that the
// not-emitted path is a clean no-op when there is no helper to remove (the
// common case for every non-de-BAML generator).
func TestMaybeWriteDeBAMLHelper_NoopWhenNoneAndAbsent(t *testing.T) {
	dir := t.TempDir()
	g := &generator{
		pkgs:              PackageConfig{OutputPath: filepath.Join(dir, "adapter.go")},
		emittedDeBAMLCall: false,
	}
	g.maybeWriteDeBAMLHelper() // must not panic
	if _, err := os.Stat(filepath.Join(dir, "debaml.go")); !os.IsNotExist(err) {
		t.Errorf("debaml.go unexpectedly present: %v", err)
	}
}
