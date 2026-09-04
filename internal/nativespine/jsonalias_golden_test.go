package nativespine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// updateJSONAliasGolden regenerates the committed ExecBridge-U1 JSON-alias carrier
// fixture from the emitter. Run:
//
//	go test ./internal/nativespine/ -run TestJSONAliasCodegenGolden -update-native-spine-goldens -count=1
//
// (shares the -update-native-spine-goldens flag defined in codegen_golden_test.go).

const jsonAliasFixturePackageName = "nativespinejsonfixture"

func jsonAliasFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "nativespinejsonfixture", "generated_json_alias.go"))
}

// admittedJSONAliasMethod builds the JSON-alias corpus and returns the single
// admitted StaticRecursiveAliasJSON method.
func admittedJSONAliasMethod(t *testing.T) projectdescriptor.Method {
	t.Helper()
	p, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	for _, m := range p.Methods {
		if m.Name == "StaticRecursiveAliasJSON" {
			return m
		}
	}
	t.Fatalf("StaticRecursiveAliasJSON not admitted (methods=%d diagnostics=%d)", len(p.Methods), len(p.Diagnostics))
	return projectdescriptor.Method{}
}

// TestJSONAliasIsStreamClass proves the ONE population authority decides the class:
// the exact five-arm JSON alias — and only it — is stamped ClassStaticStream with the
// ordered stream capability set. The classifier asks
// debaml.SupportsNativeStaticStreamBundle rather than reproducing the fingerprint, so
// the descriptor class, spine registration, call admission, and the parse entrypoints
// can never disagree about the stream population.
func TestJSONAliasIsStreamClass(t *testing.T) {
	m := admittedJSONAliasMethod(t)
	if m.Class != projectdescriptor.ClassStaticStream {
		t.Fatalf("StaticRecursiveAliasJSON class = %q, want %q", m.Class, projectdescriptor.ClassStaticStream)
	}
	want := []projectdescriptor.CapabilityCode{
		"static_method", "final_call", "stream", "stream_with_raw", "provider_openai", "single_leaf_client",
	}
	if !reflect.DeepEqual(m.RequiredCapabilities, want) {
		t.Fatalf("required capabilities = %v, want %v", m.RequiredCapabilities, want)
	}
	// The capability manifest record must agree exactly (Project.Validate enforces it,
	// but read it explicitly so a class bump that forgets the manifest is loud here).
	p, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var found bool
	for _, c := range p.Capabilities {
		if c.Method != m.Name {
			continue
		}
		found = true
		if c.Class != projectdescriptor.ClassStaticStream || !reflect.DeepEqual(c.Required, want) {
			t.Fatalf("capability record = (class %q, required %v), want (%q, %v)", c.Class, c.Required, projectdescriptor.ClassStaticStream, want)
		}
	}
	if !found {
		t.Fatal("no capability record for StaticRecursiveAliasJSON")
	}
}

// TestNonStreamFamiliesStayUnary is the negative half: a method the stream predicate
// declines keeps ClassStaticUnary and the unary capability set, so no near-miss family
// silently acquires a stream promise. It covers a scalar return, a non-alias class
// return, a REORDERED five-arm alias, and the FINAL-served-but-stream-declined
// JsonValue family.
func TestNonStreamFamiliesStayUnary(t *testing.T) {
	const clients = `client<llm> C {
  provider openai
  options { model "gpt-4o-mini" api_key "sk-x" base_url "http://127.0.0.1:0/v1" }
}
`
	cases := []struct {
		name  string
		types string
		fn    string
	}{
		{
			name:  "scalar_return",
			types: "",
			fn: `function F(topic: string) -> string {
  client C
  prompt #"{{ topic }}"#
}
`,
		},
		{
			name: "class_return",
			types: `class Answer { text string }
`,
			fn: `function F(topic: string) -> Answer {
  client C
  prompt #"{{ topic }}"#
}
`,
		},
		{
			name: "reordered_arms",
			types: `type JSON = string | int | bool | JSON[] | map<string, JSON>
`,
			fn: `function F(topic: string) -> JSON {
  client C
  prompt #"{{ topic }}"#
}
`,
		},
		{
			name: "json_value_family",
			types: `type JsonValue = string | int | float | bool | JsonValue[] | map<string, JsonValue> | null
`,
			fn: `function F(topic: string) -> JsonValue {
  client C
  prompt #"{{ topic }}"#
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := nativespine.BuildFromSource(map[string]string{
				"clients.baml": clients, "types.baml": tc.types, "functions.baml": tc.fn,
			})
			if err != nil {
				t.Fatalf("BuildFromSource: %v", err)
			}
			// NON-VACUITY: the method must actually have been RETAINED and admitted.
			// Without this flag a family that was dropped from Project.Methods entirely
			// (declined upstream, renamed, or lost to a classifier change) would satisfy
			// the loop below by never entering it, and the unary-class claim would be
			// proven of nothing.
			found := false
			for _, m := range p.Methods {
				if m.Name != "F" {
					continue
				}
				found = true
				if m.Class != projectdescriptor.ClassStaticUnary {
					t.Fatalf("class = %q, want %q (only the exact five-arm JSON alias is stream-capable)", m.Class, projectdescriptor.ClassStaticUnary)
				}
				// The capability set must be exactly the unary one — not merely free of
				// the two stream codes, which a truncated or reordered set would also be.
				want := nativespine.ClassRequiredCapabilities(projectdescriptor.ClassStaticUnary)
				if !reflect.DeepEqual(m.RequiredCapabilities, want) {
					t.Fatalf("required capabilities = %v, want the unary-only set %v", m.RequiredCapabilities, want)
				}
			}
			if !found {
				t.Fatalf("method %q is not in Project.Methods (%d admitted, %d declined); the unary-class proof would be vacuous",
					"F", len(p.Methods), len(p.Diagnostics))
			}
		})
	}
}

// TestBootedE2ECorpusClasses mirrors, IN PROCESS, the exact three-method corpus the
// booted native-only e2e generates its deployment registry from
// (internal/nativebody/nanollmprepare/cmd/worker-nativeonly/e2e_support_test.go), and
// pins the class the classifier stamps on each.
//
// It exists because that corpus is only classified inside a booted, container-tagged
// build, so a wrong expectation there costs a full CI round-trip to discover — as one
// did. The classification itself is pure: it lowers the Return and asks the one totality
// predicate, so it reproduces exactly here.
//
// The property it pins is the one that is easy to get wrong: the class is derived from
// the RETURN SHAPE ALONE and says nothing about the client. RetryPolicyMethod returns the
// exact five-arm JSON alias, so it is stream-CLASS even though its retry-policy client
// puts it outside the runtime cohort — shape and client are independent axes, and only
// the runtime classifier consults the second.
func TestBootedE2ECorpusClasses(t *testing.T) {
	const clients = `client<llm> JSONOracle {
  provider openai
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1-not-a-real-secret"
    base_url "http://127.0.0.1:9/v1"
  }
}

retry_policy U1bRetry {
  max_retries 2
  strategy { type constant_delay delay_ms 100 }
}

client<llm> RetryingOracle {
  provider openai
  retry_policy U1bRetry
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1b-not-a-real-secret"
    base_url "http://127.0.0.1:9/v1"
  }
}
`
	const functions = `function StaticRecursiveAliasJSON(topic: string) -> JSON {
  client JSONOracle
  prompt #"Return a JSON document describing {{ topic }}."#
}

function NonCohortStringReturn(topic: string) -> string {
  client JSONOracle
  prompt #"Return a string describing {{ topic }}."#
}

function RetryPolicyMethod(topic: string) -> JSON {
  client RetryingOracle
  prompt #"Return a JSON document describing {{ topic }}."#
}
`

	p, err := nativespine.BuildFromSource(map[string]string{
		"clients.baml":   clients,
		"types.baml":     "type JSON = int | string | bool | JSON[] | map<string, JSON>\n",
		"functions.baml": functions,
	})
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	want := map[string]projectdescriptor.MethodClass{
		// The served method: exact five-arm JSON alias on an admitted client.
		"StaticRecursiveAliasJSON": projectdescriptor.ClassStaticStream,
		// Out of the population by SHAPE — a plain string return is not the alias.
		"NonCohortStringReturn": projectdescriptor.ClassStaticUnary,
		// Out of the population by CLIENT, not by shape: its return IS the exact alias,
		// so it carries the STREAM class and is still declined at runtime registration
		// (reconstructFunction's retry-policy population gate) and omitted at boot.
		"RetryPolicyMethod": projectdescriptor.ClassStaticStream,
	}

	got := map[string]projectdescriptor.MethodClass{}
	for _, m := range p.Methods {
		got[m.Name] = m.Class
	}
	// NON-VACUITY: all three must be RETAINED as admitted candidates, or the class
	// assertions below would hold over an empty set — which is what the booted suite's
	// own generate-then-decline proof depends on too.
	if len(got) != len(want) {
		t.Fatalf("admitted methods = %v, want exactly the three corpus methods %v (declined: %d)", got, want, len(p.Diagnostics))
	}
	for name, wantClass := range want {
		gotClass, ok := got[name]
		if !ok {
			t.Fatalf("method %q was not admitted; the booted e2e's generated candidate set would be missing it", name)
		}
		if gotClass != wantClass {
			t.Fatalf("method %q has class %q, want %q", name, gotClass, wantClass)
		}
	}
}

// TestJSONAliasCodegenGolden proves the M3e-A JSON-alias carrier fixture is exactly
// what the STREAM emitter produces from the neutral descriptor. With -update it
// regenerates the committed file.
func TestJSONAliasCodegenGolden(t *testing.T) {
	m := admittedJSONAliasMethod(t)

	src, err := codegen.EmitNativeStaticStream(m, codegen.NativeSpineOptions{PackageName: jsonAliasFixturePackageName})
	if err != nil {
		t.Fatalf("EmitNativeStaticStream: %v", err)
	}

	path := jsonAliasFixturePath(t)
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
