package nativespine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// The scalar-LIST input cohort's carrier fixture. Regenerate with:
//
//	go test ./internal/nativespine/ -run TestListArgsCodegenGolden -update-native-spine-goldens -count=1
//
// (shares the -update-native-spine-goldens flag defined in codegen_golden_test.go).

const listArgsFixturePackageName = "nativespinelistfixture"

func listArgsFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "nativespinelistfixture", "generated_list_args.go"))
}

// admittedListArgsMethod builds the scalar-list corpus and returns its single
// admitted method.
func admittedListArgsMethod(t *testing.T) projectdescriptor.Method {
	t.Helper()
	p, err := nativespine.BuildFromSource(nativespine.ListArgsFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	for _, m := range p.Methods {
		if m.Name == nativespine.ListArgsFixtureMethod {
			return m
		}
	}
	t.Fatalf("%s not admitted (methods=%d diagnostics=%v)", nativespine.ListArgsFixtureMethod, len(p.Methods), p.Diagnostics)
	return projectdescriptor.Method{}
}

// TestListArgsIsStreamClassWithResolvedListArgs is the classifier half of the
// widening: a method that mixes required scalars with required lists of required
// primitives and returns the UNCHANGED exact five-arm JSON alias is admitted and
// stamped ClassStaticStream, with the ordered STREAM capability set.
//
// Note the SOURCE classifier admits a wider input profile than the SERVING
// registry does (it would admit `float[]` and nested lists too, because codegen can
// emit carriers for them). That is deliberate and is why the serving predicate lives
// in nativeserve/spine and is tested there: this test pins what the classifier
// produces, not what the population admits.
//
// The class is what makes /call, /stream and /stream-with-raw widen TOGETHER: the
// shared population constructors read it, so a JSON-returning list-input method that
// only reached ClassStaticUnary would widen /call alone.
func TestListArgsIsStreamClassWithResolvedListArgs(t *testing.T) {
	m := admittedListArgsMethod(t)
	if m.Class != projectdescriptor.ClassStaticStream {
		t.Fatalf("class = %q, want %q", m.Class, projectdescriptor.ClassStaticStream)
	}
	want := nativespine.ClassRequiredCapabilities(projectdescriptor.ClassStaticStream)
	if !reflect.DeepEqual(m.RequiredCapabilities, want) {
		t.Fatalf("required capabilities = %v, want the stream set %v", m.RequiredCapabilities, want)
	}

	// The resolved ARGUMENT graph is the fact the registry gate reads, so it is
	// pinned here rather than inferred from the source text: five arguments, in
	// declared order, one required scalar plus one required single-level list of
	// each admitted primitive, no nullable edge anywhere.
	type wantArg struct {
		name string
		kind promptdescriptor.ValueKind
		elem promptdescriptor.ValueKind // "" when the argument is a scalar
	}
	wantArgs := []wantArg{
		{"topic", promptdescriptor.ValueString, ""},
		// The scalar float stays in the cohort; only float LIST ELEMENTS are excluded.
		{"ratio", promptdescriptor.ValueFloat, ""},
		{"tags", promptdescriptor.ValueList, promptdescriptor.ValueString},
		{"counts", promptdescriptor.ValueList, promptdescriptor.ValueInt},
		{"flags", promptdescriptor.ValueList, promptdescriptor.ValueBool},
	}
	if len(m.Args) != len(wantArgs) {
		t.Fatalf("argument count = %d, want %d", len(m.Args), len(wantArgs))
	}
	for i, w := range wantArgs {
		got := m.Args[i]
		if got.Name != w.name || got.Type.Kind != w.kind || got.Type.Nullable {
			t.Fatalf("argument %d = %+v, want name %q kind %q non-nullable", i, got, w.name, w.kind)
		}
		if w.elem == "" {
			if got.Type.Elem != nil {
				t.Fatalf("scalar argument %q carries an element edge", w.name)
			}
			continue
		}
		if got.Type.Elem == nil {
			t.Fatalf("list argument %q has no element type", w.name)
		}
		if got.Type.Elem.Kind != w.elem || got.Type.Elem.Nullable || got.Type.Elem.Elem != nil {
			t.Fatalf("list argument %q element = %+v, want a non-nullable %q with no further nesting", w.name, *got.Type.Elem, w.elem)
		}
	}
}

// TestListArgsCodegenGolden proves the committed scalar-list carrier fixture is
// exactly what the STREAM emitter produces from the neutral descriptor. With
// -update-native-spine-goldens it regenerates the committed file.
func TestListArgsCodegenGolden(t *testing.T) {
	m := admittedListArgsMethod(t)

	src, err := codegen.EmitNativeStaticStream(m, codegen.NativeSpineOptions{PackageName: listArgsFixturePackageName})
	if err != nil {
		t.Fatalf("EmitNativeStaticStream: %v", err)
	}

	path := listArgsFixturePath(t)
	if *updateGoldens {
		if err := os.WriteFile(path, src, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", path, len(src))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed golden: %v", err)
	}
	if string(want) != string(src) {
		t.Fatalf("committed %s is stale — re-run with -update-native-spine-goldens.", path)
	}
}

// TestListArgsFixtureSourcesAtRewritesBothLegs pins the base_url substitution the
// oracle differentials depend on. A differential that pointed the native leg at the
// loopback and left the BAML leg on the placeholder would compare two different
// requests and could still be green, so the rewrite is asserted directly.
func TestListArgsFixtureSourcesAtRewritesBothLegs(t *testing.T) {
	const loopback = "http://127.0.0.1:65500/v1"
	got := nativespine.ListArgsFixtureSourcesAt(loopback)
	if len(got) != len(nativespine.ListArgsFixtureSources) {
		t.Fatalf("rewritten corpus has %d files, want %d", len(got), len(nativespine.ListArgsFixtureSources))
	}
	joined := ""
	for _, src := range got {
		joined += src
	}
	if !contains(joined, loopback) {
		t.Fatalf("the rewritten corpus does not carry %q", loopback)
	}
	if contains(joined, nativespine.ListArgsFixtureBaseURL) {
		t.Fatalf("the rewritten corpus still carries the placeholder %q", nativespine.ListArgsFixtureBaseURL)
	}
	// The original map must be untouched — both legs call this helper, and a helper
	// that mutated the package-level corpus would make the second call see the
	// first call's URL.
	orig := ""
	for _, src := range nativespine.ListArgsFixtureSources {
		orig += src
	}
	if !contains(orig, nativespine.ListArgsFixtureBaseURL) || contains(orig, loopback) {
		t.Fatal("ListArgsFixtureSourcesAt mutated the shared corpus")
	}

	p, err := nativespine.BuildFromSource(got)
	if err != nil {
		t.Fatalf("BuildFromSource over the rewritten corpus: %v", err)
	}
	found := false
	for _, c := range p.Clients {
		for _, o := range c.Config.TransportOptions {
			if o.Key == "base_url" {
				found = true
				if o.Value.String != loopback {
					t.Fatalf("descriptor base_url = %q, want %q", o.Value.String, loopback)
				}
			}
		}
	}
	if !found {
		t.Fatal("the rewritten corpus produced no base_url transport option")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
