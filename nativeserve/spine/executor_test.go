package spine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// newExec builds the executor over proj + the given bindings (nil exec = default).
func newExec(t *testing.T, proj projectdescriptor.Project, bindings ...bamlutils.NativeSpineUnaryBinding) (*spine.UnaryExecutor, error) {
	t.Helper()
	return spine.NewUnaryExecutor(proj, bindings, nil)
}

// jsonAliasExec builds the admitted JSON-alias executor (the positive).
func jsonAliasExec(t *testing.T) *spine.UnaryExecutor {
	t.Helper()
	e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	return e
}

// TestNewUnaryExecutor_AdmitsExactJSONAlias proves the positive registers.
func TestNewUnaryExecutor_AdmitsExactJSONAlias(t *testing.T) {
	e := jsonAliasExec(t)
	got := e.Methods()
	if len(got) != 1 || got[0] != jsonAliasMethod {
		t.Fatalf("Methods() = %v, want [%s]", got, jsonAliasMethod)
	}
}

// TestRegistrationDeclineMatrix is the complete registration/zero-socket negative
// matrix (Codex review finding 5). Every non-cohort shape/input/cohort/binding fact
// is REJECTED before any executor is built — so no socket is even possible. Each row
// must fail on the current tip; the JSON-alias predicate, the required-scalar gate,
// the project/client cohort gate (templates/retry/strategy), the descriptor envelope,
// and the binding checks are all exercised.
func TestRegistrationDeclineMatrix(t *testing.T) {
	const jsonType = "type JSON = int | string | bool | JSON[] | map<string, JSON>"

	cases := []struct {
		name    string
		proj    projectdescriptor.Project
		binding bamlutils.NativeSpineUnaryBinding
		want    string
	}{
		// --- output shape negatives (the totality predicate) --------------------
		{"jsonvalue_alias",
			projectFromCorpus(t, corpus("type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>", `function F(topic: string) -> JsonValue { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"renamed_exact_alias", // same five arms, different name -> not the pinned `JSON`
			projectFromCorpus(t, corpus("type Blob = int | string | bool | Blob[] | map<string, Blob>", `function F(topic: string) -> Blob { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"reordered_alias",
			projectFromCorpus(t, corpus("type JsonValueReordered = float | int | bool | string | null | JsonValueReordered[] | map<string, JsonValueReordered>", `function F(topic: string) -> JsonValueReordered { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"wrapper_alias", // wraps the exact family in extra structure -> not the bare `JSON`
			projectFromCorpus(t, corpus(jsonType, `function F(topic: string) -> JSON[] { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"class_return",
			projectFromCorpus(t, corpus("class Wrap { x string }", `function F(topic: string) -> Wrap { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"enum_return",
			projectFromCorpus(t, corpus("enum E {\n  A\n  B\n}", `function F(topic: string) -> E { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		{"scalar_return",
			projectFromCorpus(t, corpus("", `function F(topic: string) -> string { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort"},
		// A @assert constraint and a @stream annotation on the exact family (injected
		// directly — BAML forbids them on this alias) both leave the exact fingerprint.
		{"constraint_on_alias",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Methods[0].Return.Target.Meta.Constraints = []schemadescriptor.Constraint{{Level: schemadescriptor.ConstraintAssert, Expression: "true"}}
			}),
			jsonAliasBinding(), "JSON alias cohort"},
		{"stream_annotation_on_alias",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Methods[0].Return.Target.Meta.Stream = schemadescriptor.StreamingBehavior{Done: true}
			}),
			jsonAliasBinding(), "JSON alias cohort"},

		// --- input shape negatives ---------------------------------------------
		{"nullable_scalar_input",
			projectFromCorpus(t, corpus(jsonType, `function F(topic: string?) -> JSON { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "is nullable"},
		{"list_input",
			projectFromCorpus(t, corpus(jsonType, `function F(tags: string[]) -> JSON { client C prompt #"{{ tags }}"# }`)),
			jsonAliasBinding("F"), "required-scalar cohort"},
		// class / enum / map / media inputs are declined by the M1 classifier itself,
		// so the method never reaches the project — the binding names a non-admitted
		// method (still a pre-socket registration decline).
		{"class_input",
			projectFromCorpus(t, corpus(jsonType+"\nclass In { x string }", `function F(c: In) -> JSON { client C prompt #"{{ c }}"# }`)),
			jsonAliasBinding("F"), "did not admit"},
		{"enum_input",
			projectFromCorpus(t, corpus(jsonType+"\nenum Col {\n  R\n  G\n}", `function F(c: Col) -> JSON { client C prompt #"{{ c }}"# }`)),
			jsonAliasBinding("F"), "did not admit"},
		{"map_input",
			projectFromCorpus(t, corpus(jsonType, `function F(m: map<string, string>) -> JSON { client C prompt #"{{ m }}"# }`)),
			jsonAliasBinding("F"), "did not admit"},
		{"media_input",
			projectFromCorpus(t, corpus(jsonType, `function F(img: image) -> JSON { client C prompt #"{{ img }}"# }`)),
			jsonAliasBinding("F"), "did not admit"},

		// --- project / client cohort negatives ---------------------------------
		{"template_project",
			projectFromCorpus(t, map[string]string{
				"clients.baml":   clientBlock,
				"types.baml":     jsonType,
				"macros.baml":    `template_string Hdr(n: string) #"Hi {{ n }}"#`,
				"functions.baml": `function F(topic: string) -> JSON { client C prompt #"{{ topic }}"# }`,
			}),
			jsonAliasBinding("F"), "template-free"},
		{"retry_policy_client",
			projectFromCorpus(t, map[string]string{
				"clients.baml":   "client<llm> R {\n  provider openai\n  retry_policy Retry1\n  options { model \"gpt-4o-mini\" api_key \"sk-x\" base_url \"http://127.0.0.1:0/v1\" }\n}\n",
				"retries.baml":   "retry_policy Retry1 {\n  max_retries 3\n  strategy { type constant_delay delay_ms 200 }\n}\n",
				"types.baml":     jsonType,
				"functions.baml": `function F(topic: string) -> JSON { client R prompt #"{{ topic }}"# }`,
			}),
			jsonAliasBinding("F"), "forbids retries"},
		// A body-affecting client option injected DIRECTLY onto the admitted JSON
		// project's client — the Method survives to the constructor (the source
		// classifier never sees it), so this exercises the REGISTRATION client-cohort
		// gate that Call uses (finding 2), unlike a body option in source (which the
		// classifier removes upstream).
		{"body_option_client_survives",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Clients[0].Config.RequestBodyPresent = true
			}),
			jsonAliasBinding(), "body-affecting option"},

		// --- descriptor envelope + version fences -------------------------------
		{"project_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Version += 99 }),
			jsonAliasBinding(), "project version"},
		{"prompt_descriptor_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.PromptDescriptorVersion += 99 }),
			jsonAliasBinding(), "prompt-descriptor version"},
		{"schema_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.SchemaVersion += 99 }),
			jsonAliasBinding(), "schema version"},
		{"return_method_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Methods[0].Return.Method = "Other" }),
			jsonAliasBinding(), "return names method"},
		{"return_stream_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Methods[0].Return.Stream = true }),
			jsonAliasBinding(), "streaming variant"},
		{"capability_manifest_corruption",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				for i := range p.Capabilities {
					if p.Capabilities[i].Method == jsonAliasMethod {
						p.Capabilities[i].Admitted = false
					}
				}
			}),
			jsonAliasBinding(), "capability"},

		// --- binding-level ------------------------------------------------------
		{"nil_project_input", jsonAliasProject(t), bamlutils.NativeSpineUnaryBinding{Method: jsonAliasMethod, ProjectInput: nil, DecodeFinal: jsonAliasBinding().DecodeFinal}, "ProjectInput is nil"},
		{"nil_decode_final", jsonAliasProject(t), bamlutils.NativeSpineUnaryBinding{Method: jsonAliasMethod, ProjectInput: jsonAliasBinding().ProjectInput, DecodeFinal: nil}, "DecodeFinal is nil"},
		{"binding_name_mismatch", jsonAliasProject(t), renameBinding(jsonAliasBinding(), "Other"), "did not admit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Per-row zero-socket proof: point the project's client at a counting
			// loopback and assert the failed construction opens NO socket (the
			// constructor is pure — no nanollm New/Prepare — so it never dials).
			url, count := newCountingServer(t)
			_, err := newExec(t, injectBaseURL(t, tc.proj, url), tc.binding)
			declinesOn(t, err, tc.want)
			if count() != 0 {
				t.Fatalf("registration opened %d sockets, want 0", count())
			}
		})
	}

	// Duplicate method: two bindings for the same admitted method.
	if _, err := newExec(t, jsonAliasProject(t), jsonAliasBinding(), jsonAliasBinding()); err == nil || !strings.Contains(err.Error(), "duplicate method") {
		t.Fatalf("duplicate registration error = %v, want 'duplicate method'", err)
	}
}

// TestRequestScopedFactsDeclinePreSocket proves the executor reads request-scoped
// routing/orchestration facts off the adapter and DECLINES pre-socket, opening zero
// sockets (Codex review finding 1). Each of these facts is cohort-forbidden and
// declines before any nanollm work.
func TestRequestScopedFactsDeclinePreSocket(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(*testAdapter)
	}{
		{"client_registry", func(a *testAdapter) { _ = a.SetClientRegistry(&bamlutils.ClientRegistry{}) }},
		{"dynamic_output_schema", func(a *testAdapter) { a.SetDeBAMLOutputSchema(&bamlutils.DynamicOutputSchema{}) }},
		{"request_retry_override", func(a *testAdapter) { a.SetRetryConfig(&bamlutils.RetryConfig{MaxRetries: 2}) }},
		{"round_robin_advancer", func(a *testAdapter) { a.SetRoundRobinAdvancer(stubAdvancer{}) }},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			e := jsonAliasExec(t)
			ad := newTestAdapter()
			tc.mutate(ad)
			res := e.Call(ad, jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "x"})
			if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
				t.Fatalf("disposition = %v (reason %q), want declined_pre_socket", res.Disposition, res.Reason)
			}
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Failures != 0 {
				t.Fatalf("metrics = %+v, want zero sockets/claims/failures", snap)
			}
		})
	}
}

// stubAdvancer is a no-op round-robin advancer for the finding-1 test.
type stubAdvancer struct{}

func (stubAdvancer) Advance(string, int) (int, error) { return 0, nil }

// TestCallRegistryMissDeclinesPreSocket proves an unregistered method Call declines
// pre-socket with the typed capability error and zero sockets.
func TestCallRegistryMissDeclinesPreSocket(t *testing.T) {
	e := jsonAliasExec(t)
	res := e.Call(context.Background(), "Nope", &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "x"})
	if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
		t.Fatalf("disposition = %v, want declined_pre_socket", res.Disposition)
	}
	var typed *bamlutils.NativeSpineUnsupportedMethodError
	if !errors.As(res.Err, &typed) {
		t.Fatalf("decline err = %v (%T), want *NativeSpineUnsupportedMethodError", res.Err, res.Err)
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
		t.Fatalf("metrics = %+v, want zero sockets/claims and one decline", snap)
	}
}

// TestParseRoute proves the socket-free parse route: an admitted method natively
// parses+decodes raw into the concrete carrier; an unadmitted method returns the typed
// capability decline; malformed raw is an ORDINARY parse error (not a capability decline).
func TestParseRoute(t *testing.T) {
	e := jsonAliasExec(t)
	ctx := context.Background()

	out, err := e.Parse(ctx, jsonAliasMethod, `[1,"two",true]`)
	if err != nil {
		t.Fatalf("Parse(admitted): %v", err)
	}
	carrier, ok := out.(nativespinejsonfixture.OutputJson)
	if !ok {
		t.Fatalf("parse output type = %T, want nativespinejsonfixture.OutputJson", out)
	}
	if !carrier.IsVariant3() {
		b, _ := json.Marshal(carrier)
		t.Fatalf("parsed carrier is not the list arm: %s", b)
	}

	if _, err := e.Parse(ctx, "Nope", `1`); err == nil {
		t.Fatal("Parse(unadmitted) succeeded, want typed decline")
	} else {
		var typed *bamlutils.NativeSpineUnsupportedMethodError
		if !errors.As(err, &typed) {
			t.Fatalf("Parse(unadmitted) err = %v (%T), want *NativeSpineUnsupportedMethodError", err, err)
		}
	}

	if _, err := e.Parse(ctx, jsonAliasMethod, `not json at all !!!`); err == nil {
		t.Fatal("Parse(malformed) succeeded, want parse error")
	} else {
		var typed *bamlutils.NativeSpineUnsupportedMethodError
		if errors.As(err, &typed) {
			t.Fatalf("Parse(malformed) returned a capability decline (%v); malformed raw for an admitted method must be an ordinary parse error", err)
		}
	}
}

// TestCarrierRoundTripParity replays canonical JSON values through the socket-free
// parse route + emitted carrier re-marshal, asserting byte-identical round-trips (the
// carrier half of the frozen-v0.223 parity; ParseStaticBundleUnaryCall's byte
// equivalence is the reused internal/debaml + staticoracle differential).
func TestCarrierRoundTripParity(t *testing.T) {
	e := jsonAliasExec(t)
	ctx := context.Background()
	for _, canonical := range []string{
		`1`, `"hello"`, `true`, `[1,"two",true]`, `{"k":1}`,
		`{"a":[1,2],"b":{"c":3}}`, `[]`, `[[1],[2,3]]`, `{"nested":{"deep":[true,false]}}`,
	} {
		out, err := e.Parse(ctx, jsonAliasMethod, canonical)
		if err != nil {
			t.Errorf("Parse(%s): %v", canonical, err)
			continue
		}
		got, err := json.Marshal(out)
		if err != nil {
			t.Errorf("re-marshal %s: %v", canonical, err)
			continue
		}
		if string(got) != canonical {
			t.Errorf("round-trip: %s -> carrier -> %s (want byte-identical)", canonical, got)
		}
	}
}

// fallbackComposite is the OUTER injected transition composite: an executor that
// wraps the inner spine executor and may invoke a fallback/oracle ONLY on a matched
// pre-socket decline. Succeeded / FailedAfterClaim pass through UNCHANGED.
type fallbackComposite struct {
	inner         bamlutils.NativeSpineUnaryExecutor
	fallbackCalls int
	fallbackFinal any
}

func (c *fallbackComposite) Call(ctx context.Context, method string, input any) bamlutils.NativeSpineUnaryResult {
	r := c.inner.Call(ctx, method, input)
	if r.Disposition == bamlutils.NativeSpineDeclinedPreSocket {
		c.fallbackCalls++
		return bamlutils.SucceededSpineResult(c.fallbackFinal)
	}
	return r
}

func (c *fallbackComposite) Parse(ctx context.Context, method string, raw string) (any, error) {
	return c.inner.Parse(ctx, method, raw)
}

// TestFallbackCompositeInvokesOnceOnlyOnPreSocketDecline proves the outer composite
// invokes the fallback EXACTLY once on a pre-socket decline (registry miss), and the
// inner spine executor opened no socket.
func TestFallbackCompositeInvokesOnceOnlyOnPreSocketDecline(t *testing.T) {
	inner := jsonAliasExec(t)
	comp := &fallbackComposite{inner: inner, fallbackFinal: "fallback-served"}
	res := comp.Call(context.Background(), "Unregistered", &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "x"})
	if comp.fallbackCalls != 1 {
		t.Fatalf("fallback invoked %d times, want exactly 1", comp.fallbackCalls)
	}
	if res.Disposition != bamlutils.NativeSpineSucceeded || res.Final != "fallback-served" {
		t.Fatalf("composite result = %+v, want fallback success", res)
	}
	if snap := inner.Metrics().Snapshot(); snap.Sockets != 0 {
		t.Fatalf("inner opened %d sockets on a pre-socket decline, want 0", snap.Sockets)
	}
}
