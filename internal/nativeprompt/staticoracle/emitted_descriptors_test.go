//go:build integration

package staticoracle

// Emitted-vs-source fidelity cross-check for the de-BAML Phase 8A static prompt
// descriptor emission. The byte-differential suites in this package already
// prove the EMITTED descriptor renders identically to stock BAML; this file adds
// the structural proof that the checked-in generated metadata fixture is a
// faithful, complete representation of what the native build produces from the
// SAME .baml source.
//
// Run: CGO_ENABLED=1 go test -tags integration ./internal/nativeprompt/staticoracle

import (
	"reflect"
	"sort"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/introspected"
)

// TestEmittedDescriptorsMatchSource requires the emitted fixture descriptor set
// to equal the source-built set (same methods) and each emitted descriptor to be
// semantically identical to its source-built counterpart. The claimed corpus
// uses only attribute-free primitive arguments, so the descriptor graph carries
// no bamlparser.Attribute — the only node with unexported parser scratch
// (argStart/argEnd) — and a plain reflect.DeepEqual is therefore an exact
// comparison of the complete exported semantic representation. Mismatches are
// reported by method name and the NAMES of the differing Function fields only,
// never by raw prompt or client-literal values (Phase 8A security posture: a
// descriptor's raw fields are never %v-formatted).
func TestEmittedDescriptorsMatchSource(t *testing.T) {
	emitted := buildDescriptors(t)
	source := buildDescriptorsFromSource(t)

	if len(emitted) != len(source) {
		t.Fatalf("emitted descriptor count %d != source count %d (emitted methods=%v, source methods=%v)",
			len(emitted), len(source), sortedDescriptorKeys(emitted), sortedDescriptorKeys(source))
	}

	for _, method := range sortedDescriptorKeys(source) {
		emt, ok := emitted[method]
		if !ok {
			t.Errorf("source method %q has no emitted descriptor", method)
			continue
		}
		src := source[method]
		if !reflect.DeepEqual(emt, src) {
			t.Errorf("emitted descriptor for %q diverges from source in fields: %v",
				method, divergentFunctionFields(emt, src))
		}
	}

	for method := range emitted {
		if _, ok := source[method]; !ok {
			t.Errorf("emitted descriptor %q is not a source-built descriptor", method)
		}
	}
}

// TestEmittedV3UniverseIsNonVacuous makes the DeepEqual above non-vacuous for
// the Slice 7.1b half: it asserts the emitted fixture actually carries a
// populated V3 universe and a ValueType on every argument. Without this, a
// regression that emitted an EMPTY InputValues on both legs would still pass
// TestEmittedDescriptorsMatchSource.
func TestEmittedV3UniverseIsNonVacuous(t *testing.T) {
	emitted := buildDescriptors(t)

	for _, method := range sortedDescriptorKeys(emitted) {
		fn := emitted[method]
		if fn.Version != promptdescriptor.Version {
			t.Errorf("%s: descriptor version %d, want %d", method, fn.Version, promptdescriptor.Version)
		}
		// BAML installs one namespace global per PROJECT enum, so EVERY function
		// in this fixture — including the no-enum ones — must carry the whole set.
		if len(fn.InputValues.ProjectEnums) == 0 {
			t.Errorf("%s: emitted V3 universe carries no project enums, but the fixture declares Color", method)
		}
		for _, a := range fn.Args {
			if a.ValueType == nil {
				t.Errorf("%s: argument %q has no V3 ValueType", method, a.Name)
			}
		}
	}

	// The resolved Color enum must carry canonical members in DECLARATION order
	// with their exact aliases — the facts the differential depends on.
	// Indexing without the ok-check would make a MISSING descriptor look like a
	// missing enum below, so two distinct regressions would report identically.
	fn, ok := emitted["StaticRenderEnum"]
	if !ok {
		t.Fatalf("fixture emitted no descriptor for StaticRenderEnum (declines: %v)", introspected.StaticPromptDeclines)
	}
	var color *promptdescriptor.ResolvedEnum
	for i := range fn.InputValues.ProjectEnums {
		if fn.InputValues.ProjectEnums[i].Name == "Color" {
			color = &fn.InputValues.ProjectEnums[i]
		}
	}
	if color == nil {
		t.Fatal("emitted universe has no Color enum")
	}
	wantCanonical := []string{"RED", "GREEN", "BLUE"}
	if len(color.Members) != len(wantCanonical) {
		t.Fatalf("Color has %d members, want %d", len(color.Members), len(wantCanonical))
	}
	for i, want := range wantCanonical {
		if color.Members[i].Canonical != want {
			t.Errorf("Color member %d = %q, want %q (source declaration order)", i, color.Members[i].Canonical, want)
		}
	}
	if color.Members[0].Alias == nil || *color.Members[0].Alias != "rouge" {
		t.Errorf("Color.RED alias = %v, want \"rouge\"", color.Members[0].Alias)
	}
	if color.Members[2].Alias != nil {
		t.Errorf("Color.BLUE alias = %q, want nil (unaliased)", *color.Members[2].Alias)
	}
}

// TestEmittedProjectorCoverage requires a generated argument projector for EVERY
// emitted descriptor and an empty projector-decline ledger. A missing projector
// silently removes a method from the native path, so the fixture asserts the
// partition rather than tolerating it.
func TestEmittedProjectorCoverage(t *testing.T) {
	if len(introspected.StaticPromptProjectorDeclines) != 0 {
		t.Errorf("fixture emitted projector declines: %v", introspected.StaticPromptProjectorDeclines)
	}
	for _, method := range sortedDescriptorKeys(buildDescriptors(t)) {
		if _, ok := introspected.StaticPromptArgumentProjectors[method]; !ok {
			t.Errorf("method %q has an emitted descriptor but NO argument projector", method)
		}
	}
}

func sortedDescriptorKeys(m map[string]promptdescriptor.Function) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// divergentFunctionFields names the top-level promptdescriptor.Function fields
// that differ between two descriptors, for a redacted mismatch report. It never
// returns field VALUES — only the field name — so a diff diagnostic cannot leak
// prompt bytes or inline client literals.
func divergentFunctionFields(a, b promptdescriptor.Function) []string {
	var diff []string
	if a.Version != b.Version {
		diff = append(diff, "Version")
	}
	if a.Method != b.Method {
		diff = append(diff, "Method")
	}
	if a.Prompt != b.Prompt {
		diff = append(diff, "Prompt")
	}
	if !reflect.DeepEqual(a.Args, b.Args) {
		diff = append(diff, "Args")
	}
	if a.Client != b.Client {
		diff = append(diff, "Client")
	}
	if a.Provider != b.Provider {
		diff = append(diff, "Provider")
	}
	if !reflect.DeepEqual(a.Return, b.Return) {
		diff = append(diff, "Return")
	}
	if !reflect.DeepEqual(a.Macros, b.Macros) {
		diff = append(diff, "Macros")
	}
	if !reflect.DeepEqual(a.ClientConfig, b.ClientConfig) {
		diff = append(diff, "ClientConfig")
	}
	return diff
}
