package spine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
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

// TestRegistrationDeclineMatrix is the negative admission matrix for NewUnaryExecutor,
// with a per-row zero-socket proof. It does NOT claim every row reaches register(): the
// constructor runs proj.Validate() FIRST (executor.go), so the version + capability-
// manifest-consistency rows decline THERE, before the register() loop even starts; every
// other row passes Validate and is declined inside register() (review-4 finding 1). Each
// row carries two labels, and the test CONSUMES both — it does not merely assert them in
// prose:
//
//   - layer: the exact gate that declines it — "validate:*" (before the register loop) or
//     "register:*". The loop CHECKS it per row: a "validate:*" row MUST fail
//     proj.Validate(); a "register:*" row MUST pass Validate (so its decline necessarily
//     happens inside register()). A mislabelled row fails the test.
//   - kind: "regression" for a row that exercises a gate one of this slice's review fixes
//     ADDED (so it ADMITTED on the corresponding pre-fix tip and discriminates it), or
//     "coverage" for a row documenting a gate that was ALREADY present (it would decline
//     on the pre-fix tip too). The invariant AFTER the loop consumes every kind and PINS
//     the exact regression-name set, so the classification cannot silently drift.
//
// The rows labelled kind=="regression" (and the gate each one exercises):
//   - selected_client_non_openai, invalid_utf8_model — the cycle-3 client-cohort fix:
//     CheckStaticClientCohort now runs SupportsOpenAIChat on the NORMALIZED intent, so a
//     selected-client provider / invalid-UTF-8-model divergence that ADMITTED on the
//     pre-fix tip bdd586e0 now declines, in lockstep with Call's BuildOpenAIChat.
//   - body_option_client_survives, capability_manifest_corruption — the review-2
//     registration client-cohort + capability-manifest gates; ADMITTED on the earlier
//     pre-cohort-gate tip, decline since review-2.
//
// This test does NOT re-open those pre-fix tips: it pins the classification (below), not
// the admit-on-pre-fix behaviour. That behaviour for the two cycle-3 rows was checked at
// FIX TIME by neutralising the SupportsOpenAIChat gate — a fix-time verification, not a
// committed assertion.
//
// Every other row is COVERAGE of a pre-existing gate — the totality predicate, the
// required-scalar gate, reconstructFunction's project/client cohort, the descriptor
// envelope, proj.Validate's version checks, or the binding checks — and would decline on
// the pre-fix tip too. Method survival (mutation rows inject a fact the source classifier
// cannot express onto the ADMITTED project) and the real wrapping alias are retained.
// Every row opens ZERO sockets (the constructor is pure — no nanollm New/Prepare).
func TestRegistrationDeclineMatrix(t *testing.T) {
	const jsonType = "type JSON = int | string | bool | JSON[] | map<string, JSON>"

	cases := []struct {
		name    string
		proj    projectdescriptor.Project
		binding bamlutils.NativeSpineUnaryBinding
		want    string
		layer   string // exact declining gate: "validate:*" (before the register loop) or "register:*"
		kind    string // "regression" (a gate a review fix ADDED — admits on the pre-fix tip) or "coverage" (a pre-existing gate)
	}{
		// --- output shape negatives — register:totality (PRE-EXISTING gate) ------
		{"jsonvalue_alias",
			projectFromCorpus(t, corpus("type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>", `function F(topic: string) -> JsonValue { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"renamed_exact_alias", // same five arms, different name -> not the pinned `JSON`
			projectFromCorpus(t, corpus("type Blob = int | string | bool | Blob[] | map<string, Blob>", `function F(topic: string) -> Blob { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"reordered_alias",
			projectFromCorpus(t, corpus("type JsonValueReordered = float | int | bool | string | null | JsonValueReordered[] | map<string, JsonValueReordered>", `function F(topic: string) -> JsonValueReordered { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"wrapper_alias", // a NAMED alias whose DEFINITION wraps the exact family (JSON[]) -> not the bare `JSON`
			projectFromCorpus(t, corpus(jsonType+"\ntype WrappedJson = JSON[]", `function F(topic: string) -> WrappedJson { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"class_return",
			projectFromCorpus(t, corpus("class Wrap { x string }", `function F(topic: string) -> Wrap { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"enum_return",
			projectFromCorpus(t, corpus("enum E {\n  A\n  B\n}", `function F(topic: string) -> E { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		{"scalar_return",
			projectFromCorpus(t, corpus("", `function F(topic: string) -> string { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "JSON alias cohort", "register:totality", "coverage"},
		// A @assert constraint and a @stream annotation on the exact family (injected
		// directly — BAML forbids them on this alias) both leave the exact fingerprint.
		{"constraint_on_alias",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Methods[0].Return.Target.Meta.Constraints = []schemadescriptor.Constraint{{Level: schemadescriptor.ConstraintAssert, Expression: "true"}}
			}),
			jsonAliasBinding(), "JSON alias cohort", "register:totality", "coverage"},
		{"stream_annotation_on_alias",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Methods[0].Return.Target.Meta.Stream = schemadescriptor.StreamingBehavior{Done: true}
			}),
			jsonAliasBinding(), "JSON alias cohort", "register:totality", "coverage"},

		// --- input shape negatives — register:required-scalar (PRE-EXISTING gate) -
		// nullable + list inputs SURVIVE the source classifier and reach register(),
		// where requiredScalarInputs refuses them (the classifier admits these input
		// shapes; the spine's required-scalar gate is what declines them).
		{"nullable_scalar_input",
			projectFromCorpus(t, corpus(jsonType, `function F(topic: string?) -> JSON { client C prompt #"{{ topic }}"# }`)),
			jsonAliasBinding("F"), "is nullable", "register:required-scalar", "coverage"},
		{"list_input",
			projectFromCorpus(t, corpus(jsonType, `function F(tags: string[]) -> JSON { client C prompt #"{{ tags }}"# }`)),
			jsonAliasBinding("F"), "required-scalar cohort", "register:required-scalar", "coverage"},
		// A class/enum/map/media input is removed by the SOURCE classifier and never
		// reaches the constructor, so injecting a non-scalar (class) type DIRECTLY onto
		// the admitted method's argument edge is the only way to exercise the spine's
		// requiredScalarInputs gate on a non-scalar input that SURVIVES to register()
		// (review-3 finding 3). requiredScalarInputs is a PRE-EXISTING gate, so this is
		// coverage, not a regression.
		{"nonscalar_class_input_survives",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Methods[0].Args[0].Type = promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Ghost"}
			}),
			jsonAliasBinding(), "required-scalar cohort", "register:required-scalar", "coverage"},

		// --- static-client cohort — register:client-cohort ----------------------
		// selected_client_non_openai + invalid_utf8_model are the cycle-3 REGRESSIONS
		// (review-3 finding 1): both mutate the ADMITTED JSON project so the Method
		// survives to register(); both ADMITTED on the pre-fix tip bdd586e0 (the earlier
		// CheckStaticClientCohort checked only the method-provider argument + body-
		// affecting options, never the NORMALIZED intent's own provider or model
		// validity), and DECLINE now because registration runs the ACTUAL call-time
		// client predicate (SupportsOpenAIChat) on the intent — exactly as Call's
		// BuildOpenAIChat.
		{"selected_client_non_openai", // Method.Provider=="openai" but the SELECTED client's provider is not
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				for i := range p.Clients {
					if p.Clients[i].Config.Name == p.Methods[0].Client {
						p.Clients[i].Config.Provider = "anthropic"
					}
				}
			}),
			jsonAliasBinding(), "not the proven openai", "register:client-cohort", "regression"},
		{"invalid_utf8_model", // a literal model that is not valid UTF-8: NormalizeStaticClient passes it, BuildOpenAIChat/Call declines it
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				for i := range p.Clients {
					if p.Clients[i].Config.Name == p.Methods[0].Client {
						p.Clients[i].Config.Model.Value = "gpt-\xff\xfe"
					}
				}
			}),
			jsonAliasBinding(), "valid UTF-8", "register:client-cohort", "regression"},

		// --- project / client cohort — register:reconstruct (PRE-EXISTING gate) --
		{"template_project",
			projectFromCorpus(t, map[string]string{
				"clients.baml":   clientBlock,
				"types.baml":     jsonType,
				"macros.baml":    `template_string Hdr(n: string) #"Hi {{ n }}"#`,
				"functions.baml": `function F(topic: string) -> JSON { client C prompt #"{{ topic }}"# }`,
			}),
			jsonAliasBinding("F"), "template-free", "register:reconstruct", "coverage"},
		{"retry_policy_client",
			projectFromCorpus(t, map[string]string{
				"clients.baml":   "client<llm> R {\n  provider openai\n  retry_policy Retry1\n  options { model \"gpt-4o-mini\" api_key \"sk-x\" base_url \"http://127.0.0.1:0/v1\" }\n}\n",
				"retries.baml":   "retry_policy Retry1 {\n  max_retries 3\n  strategy { type constant_delay delay_ms 200 }\n}\n",
				"types.baml":     jsonType,
				"functions.baml": `function F(topic: string) -> JSON { client R prompt #"{{ topic }}"# }`,
			}),
			jsonAliasBinding("F"), "forbids retries", "register:reconstruct", "coverage"},
		// A body-affecting client option injected DIRECTLY onto the admitted JSON
		// project's client — the Method survives to the constructor (the source
		// classifier never sees it), so this exercises the REGISTRATION client-cohort
		// gate that Call uses. That gate was ADDED in review-2, so this is a genuine
		// regression against the earlier pre-cohort-gate tip.
		{"body_option_client_survives",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				p.Clients[0].Config.RequestBodyPresent = true
			}),
			jsonAliasBinding(), "request_body option", "register:client-cohort", "regression"},

		// --- descriptor envelope — register:envelope (PRE-EXISTING gate) ---------
		{"return_method_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Methods[0].Return.Method = "Other" }),
			jsonAliasBinding(), "return names method", "register:envelope", "coverage"},
		{"return_stream_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Methods[0].Return.Stream = true }),
			jsonAliasBinding(), "streaming variant", "register:envelope", "coverage"},

		// --- version fences — validate:version (proj.Validate, BEFORE register) --
		{"project_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.Version += 99 }),
			jsonAliasBinding(), "project version", "validate:version", "coverage"},
		{"prompt_descriptor_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.PromptDescriptorVersion += 99 }),
			jsonAliasBinding(), "prompt-descriptor version", "validate:version", "coverage"},
		{"schema_version_mismatch",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) { p.SchemaVersion += 99 }),
			jsonAliasBinding(), "schema version", "validate:version", "coverage"},
		// The capability manifest is consulted by proj.Validate() (before register): a
		// method admitted in Methods but marked admitted=false in the manifest is an
		// inconsistency Validate rejects. proj.Validate() was ADDED in review-2, so this
		// is a genuine regression against the pre-Validate tip.
		{"capability_manifest_corruption",
			mutatedJSONProject(t, func(p *projectdescriptor.Project) {
				for i := range p.Capabilities {
					if p.Capabilities[i].Method == jsonAliasMethod {
						p.Capabilities[i].Admitted = false
					}
				}
			}),
			jsonAliasBinding(), "capability", "validate:capability", "regression"},

		// --- binding-level — register:binding (PRE-EXISTING gate) ---------------
		{"nil_project_input", jsonAliasProject(t), bamlutils.NativeSpineUnaryBinding{Method: jsonAliasMethod, ProjectInput: nil, DecodeFinal: jsonAliasBinding().DecodeFinal}, "ProjectInput is nil", "register:binding", "coverage"},
		{"nil_decode_final", jsonAliasProject(t), bamlutils.NativeSpineUnaryBinding{Method: jsonAliasMethod, ProjectInput: jsonAliasBinding().ProjectInput, DecodeFinal: nil}, "DecodeFinal is nil", "register:binding", "coverage"},
		{"binding_name_mismatch", jsonAliasProject(t), renameBinding(jsonAliasBinding(), "Other"), "did not admit", "register:binding", "coverage"},
	}

	// layerWants maps each SUB-gate label to the decline substring(s) its rows may carry,
	// so `layer` and `want` are checked for mutual consistency — not just the
	// validate/register PREFIX (CodeRabbit #10). A row mislabelled with the wrong
	// sub-gate (e.g. a totality row tagged register:binding) fails: its `want` is not in
	// the tagged sub-gate's set. Every layer used by a row above must appear here.
	layerWants := map[string][]string{
		"validate:version":        {"project version", "prompt-descriptor version", "schema version"},
		"validate:capability":     {"capability"},
		"register:totality":       {"JSON alias cohort"},
		"register:required-scalar": {"is nullable", "required-scalar cohort"},
		"register:client-cohort":  {"not the proven openai", "valid UTF-8", "request_body option"},
		"register:reconstruct":    {"template-free", "forbids retries"},
		"register:envelope":       {"return names method", "streaming variant"},
		"register:binding":        {"ProjectInput is nil", "DecodeFinal is nil", "did not admit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sub-gate consistency (CodeRabbit #10): the row's `want` must be one the
			// tagged `layer` can produce, so a mislabelled sub-gate cannot pass by only
			// matching the validate/register prefix below.
			allowed, known := layerWants[tc.layer]
			if !known {
				t.Fatalf("row %q has layer %q with no expected-want set (add it to layerWants)", tc.name, tc.layer)
			}
			okWant := false
			for _, w := range allowed {
				if w == tc.want {
					okWant = true
					break
				}
			}
			if !okWant {
				t.Fatalf("row %q layer %q expects want in %v, but the row's want is %q — layer/want mismatch (mislabelled sub-gate?)", tc.name, tc.layer, allowed, tc.want)
			}

			// Per-row zero-socket proof: point the project's client at a counting
			// loopback and assert the failed construction opens NO socket (the
			// constructor is pure — no nanollm New/Prepare — so it never dials).
			url, count := newCountingServer(t)
			proj := injectBaseURL(t, tc.proj, url)
			// Verify the `layer` claim (review-4 finding 1): a validate:* row must be
			// rejected by proj.Validate() BEFORE the register loop; a register:* row must
			// PASS Validate, so its decline necessarily happens inside register(). A
			// mislabelled row fails here, so the matrix cannot over-claim which layer
			// declines a row. (injectBaseURL only rewrites base_url, which Validate does
			// not inspect, so this Validate result is the constructor's.)
			validateErr := proj.Validate()
			switch {
			case strings.HasPrefix(tc.layer, "validate"):
				if validateErr == nil {
					t.Fatalf("row %q is labelled layer %q but proj.Validate() passed — it does NOT decline before register()", tc.name, tc.layer)
				}
			case strings.HasPrefix(tc.layer, "register"):
				if validateErr != nil {
					t.Fatalf("row %q is labelled layer %q (reaches register) but proj.Validate() rejected it first: %v", tc.name, tc.layer, validateErr)
				}
			default:
				t.Fatalf("row %q has an unclassified layer %q", tc.name, tc.layer)
			}
			_, err := newExec(t, proj, tc.binding)
			declinesOn(t, err, tc.want)
			if count() != 0 {
				t.Fatalf("registration opened %d sockets, want 0", count())
			}
		})
	}

	// The `kind` label is CONSUMED here so the regression classification is a checked
	// invariant, not dead prose: exactly these four rows are the genuine pre-fix
	// regressions (the gates this slice's review fixes added — cycle-3's client-cohort
	// intent predicate and review-2's registration cohort + capability-manifest gates).
	// Every other row is coverage of a pre-existing gate. If a future edit relabels a row
	// or adds a regression without updating this set, the test fails.
	gotRegressions := map[string]bool{}
	for _, tc := range cases {
		switch tc.kind {
		case "regression":
			gotRegressions[tc.name] = true
		case "coverage":
		default:
			t.Fatalf("row %q has an unclassified kind %q", tc.name, tc.kind)
		}
	}
	wantRegressions := []string{"selected_client_non_openai", "invalid_utf8_model", "body_option_client_survives", "capability_manifest_corruption"}
	if len(gotRegressions) != len(wantRegressions) {
		t.Fatalf("regression rows = %v, want exactly %v", gotRegressions, wantRegressions)
	}
	for _, n := range wantRegressions {
		if !gotRegressions[n] {
			t.Fatalf("expected row %q to be labelled kind=regression", n)
		}
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
