//go:build integration && nanollm_integration

package static

// Emitted-vs-source fidelity cross-check for the de-BAML Phase 8A static prompt
// descriptor emission, in the gated nanollmprepare module. The prepared-request
// differential in this package already proves the EMITTED descriptor's native
// body equals nanollm's PreparedRequest.Body and stock BAML's body; this file
// adds the structural proof that the checked-in generated metadata fixture is a
// faithful, complete representation of what the native build produces from the
// SAME .baml source.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// TestEmittedDescriptorsMatchSource requires the emitted fixture descriptor set
// to equal the source-built set and each emitted descriptor to be semantically
// identical to its source-built counterpart. The claimed corpus uses only
// attribute-free primitive arguments, so the descriptor graph carries no
// bamlparser.Attribute (the only node with unexported parser scratch), and a
// plain reflect.DeepEqual is an exact comparison of the complete exported
// semantic representation. Mismatches are reported by method name and differing
// Function field NAMES only, never by raw prompt or client-literal values.
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
		if !reflect.DeepEqual(emt, source[method]) {
			t.Errorf("emitted descriptor for %q diverges from source in fields: %v",
				method, divergentFunctionFields(emt, source[method]))
		}
	}

	for method := range emitted {
		if _, ok := source[method]; !ok {
			t.Errorf("emitted descriptor %q is not a source-built descriptor", method)
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
// returns field VALUES — only field names — so a diff cannot leak prompt bytes
// or inline client literals.
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
