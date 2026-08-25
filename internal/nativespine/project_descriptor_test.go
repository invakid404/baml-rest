package nativespine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
)

var updateProjectGolden = flag.Bool("update-project-descriptor-golden", false,
	"rewrite the committed whole-project descriptor golden (testdata/project_descriptor.json)")

// m2FixtureSources is a whole-project fixture that exercises every M2 descriptor
// graph: multiple clients (an admitted leaf, a client with a retry reference, a
// fallback wrapper, and a round-robin wrapper with a start seed), a retry_policy
// block, a template_string (macro), and a mix of admitted and declined methods so
// the per-method capability manifest is non-trivial.
var m2FixtureSources = map[string]string{
	"clients.baml": `
client<llm> GPT4 {
  provider openai
  options { model "gpt-4o" api_key env.K }
}
client<llm> Fast {
  provider openai
  retry_policy Retry1
  options { model "gpt-4o-mini" api_key env.K }
}
client<llm> FB {
  provider baml-fallback
  options { strategy [GPT4, Fast] }
}
client<llm> RR {
  provider round-robin
  options { strategy [GPT4, Fast] start 1 }
}
`,
	"retries.baml": `
retry_policy Retry1 {
  max_retries 3
  strategy {
    type exponential_backoff
    delay_ms 200
    multiplier 2
    max_delay_ms 5000
  }
}
`,
	"macros.baml": `
template_string Header(name: string) #"Hello {{ name }}"#
`,
	"functions.baml": `
function Greet(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }
function FastGreet(name: string) -> string { client Fast prompt #"Hi {{ name }}"# }
function FallbackGreet(name: string) -> string { client FB prompt #"Hi {{ name }}"# }
function RRGreet(name: string) -> string { client RR prompt #"Hi {{ name }}"# }
function ImgFn() -> image { client GPT4 prompt #"x"# }
`,
}

// TestProjectDescriptorGolden proves the whole-project descriptor is exactly the
// committed golden — the deterministic-serialization proof for the M2 breadth
// (clients, retry policies, strategies, templates, and the per-method capability
// manifest), byte-for-byte. Regenerate with -update-project-descriptor-golden.
func TestProjectDescriptorGolden(t *testing.T) {
	proj, err := BuildFromSource(m2FixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if err := proj.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	got, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "project_descriptor.json")
	if *updateProjectGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-project-descriptor-golden): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("whole-project descriptor golden is stale — re-run with -update-project-descriptor-golden.\n--- got ---\n%s", got)
	}
}

// TestProjectDescriptorDeterministic proves the descriptor JSON is byte-identical
// across repeated runs and independent of file-name (walk) ordering.
func TestProjectDescriptorDeterministic(t *testing.T) {
	marshal := func(sources map[string]string) []byte {
		p, err := BuildFromSource(sources)
		if err != nil {
			t.Fatalf("BuildFromSource: %v", err)
		}
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	a := marshal(m2FixtureSources)
	b := marshal(m2FixtureSources)
	if string(a) != string(b) {
		t.Fatal("descriptor serialization is not deterministic across repeated runs")
	}

	// Rename every file with a numeric prefix that REVERSES the sorted (walk)
	// order, so the files are genuinely fed in a different order. A source-order
	// regression in any whole-project list (clients/retries/strategies/templates/
	// methods/diagnostics/capabilities) would then diverge; content-derived
	// name-ordering must keep the bytes identical.
	orig := make([]string, 0, len(m2FixtureSources))
	for name := range m2FixtureSources {
		orig = append(orig, name)
	}
	sort.Strings(orig)
	reordered := map[string]string{}
	for i, name := range orig {
		reordered[fmt.Sprintf("%02d_%s", len(orig)-i, name)] = m2FixtureSources[name]
	}
	if string(marshal(reordered)) != string(a) {
		t.Fatal("descriptor serialization depends on file ordering")
	}
}

// TestProjectDescriptorWholeProject asserts the M2 whole-project graphs and the
// per-method capability manifest are populated correctly.
func TestProjectDescriptorWholeProject(t *testing.T) {
	proj, err := BuildFromSource(m2FixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}

	// Clients: all four, name-ordered, with the retry reference preserved.
	var clientNames []string
	retryOf := map[string]string{}
	for _, c := range proj.Clients {
		clientNames = append(clientNames, c.Config.Name)
		retryOf[c.Config.Name] = c.RetryPolicy
	}
	if !reflect.DeepEqual(clientNames, []string{"FB", "Fast", "GPT4", "RR"}) {
		t.Errorf("clients = %v, want [FB Fast GPT4 RR]", clientNames)
	}
	if retryOf["Fast"] != "Retry1" {
		t.Errorf("Fast retry = %q, want Retry1", retryOf["Fast"])
	}

	// Retry policy.
	if len(proj.RetryPolicies) != 1 || proj.RetryPolicies[0].Name != "Retry1" {
		t.Fatalf("retry policies = %+v", proj.RetryPolicies)
	}
	rp := proj.RetryPolicies[0]
	if rp.MaxRetries != 3 || rp.Strategy != "exponential_backoff" || rp.DelayMs != 200 || rp.Multiplier != 2 || rp.MaxDelayMs != 5000 {
		t.Errorf("retry policy = %+v", rp)
	}

	// Strategies: FB (fallback) and RR (round_robin, start=1), name-ordered.
	if len(proj.Strategies) != 2 {
		t.Fatalf("strategies = %+v", proj.Strategies)
	}
	fb, rr := proj.Strategies[0], proj.Strategies[1]
	if fb.Name != "FB" || fb.Kind != projectdescriptor.StrategyFallback || !reflect.DeepEqual(fb.Children, []string{"GPT4", "Fast"}) || fb.Start != nil {
		t.Errorf("fallback strategy = %+v", fb)
	}
	if rr.Name != "RR" || rr.Kind != projectdescriptor.StrategyRoundRobin || !reflect.DeepEqual(rr.Children, []string{"GPT4", "Fast"}) || rr.Start == nil || *rr.Start != 1 {
		t.Errorf("round-robin strategy = %+v", rr)
	}

	// Template.
	if len(proj.Templates) != 1 || proj.Templates[0].Name != "Header" || !reflect.DeepEqual(proj.Templates[0].Args, []string{"name"}) {
		t.Fatalf("templates = %+v", proj.Templates)
	}

	// Per-method capability manifest: one record per retained method, name-ordered,
	// covering every method exactly once (admitted or blocked).
	wantCaps := map[string]struct {
		admitted bool
		blocked  projectdescriptor.CapabilityCode
	}{
		"Greet":         {admitted: true},
		"FastGreet":     {admitted: true},
		"FallbackGreet": {blocked: "strategy_fallback"},
		"RRGreet":       {blocked: "strategy_round_robin"},
		"ImgFn":         {blocked: "unsupported_output_shape"},
	}
	if len(proj.Capabilities) != len(wantCaps) {
		t.Fatalf("capabilities count = %d, want %d: %+v", len(proj.Capabilities), len(wantCaps), proj.Capabilities)
	}
	var prev string
	for _, c := range proj.Capabilities {
		if c.Method < prev {
			t.Errorf("capabilities not name-ordered at %q (after %q)", c.Method, prev)
		}
		prev = c.Method
		w, ok := wantCaps[c.Method]
		if !ok {
			t.Errorf("unexpected capability method %q", c.Method)
			continue
		}
		if c.Admitted != w.admitted {
			t.Errorf("%s admitted = %v, want %v", c.Method, c.Admitted, w.admitted)
		}
		if w.admitted {
			if c.Class != projectdescriptor.ClassStaticUnary || len(c.Required) == 0 {
				t.Errorf("%s admitted capability = %+v", c.Method, c)
			}
		} else if c.Blocked != w.blocked {
			t.Errorf("%s blocked = %q, want %q", c.Method, c.Blocked, w.blocked)
		}
	}
}
