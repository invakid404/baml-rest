package spine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

const jsonAliasMethodName = "StaticRecursiveAliasJSON"

// reconstructFromCorpus builds the project from a .baml corpus and reconstructs the
// named admitted method's descriptor. It is pure-Go (no nanollm) test support.
func reconstructFromCorpus(t *testing.T, corpus map[string]string, method string) promptdescriptor.Function {
	t.Helper()
	proj, err := nativespine.BuildFromSource(corpus)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	for _, m := range proj.Methods {
		if m.Name == method {
			fn, err := nativespine.ReconstructFunction(proj, m)
			if err != nil {
				t.Fatalf("ReconstructFunction(%s): %v", method, err)
			}
			return fn
		}
	}
	var declines []string
	for _, d := range proj.Diagnostics {
		declines = append(declines, string(d.Method)+"="+string(d.Code))
	}
	t.Fatalf("%s not admitted (declines: %s)", method, strings.Join(declines, ","))
	return promptdescriptor.Function{}
}

// jsonAliasFunction is the admitted five-arm JSON alias descriptor (the positive).
func jsonAliasFunction(t *testing.T) promptdescriptor.Function {
	t.Helper()
	return reconstructFromCorpus(t, nativespine.JSONAliasFixtureSources, jsonAliasMethodName)
}

// jsonAliasBinding returns the emitted JSON-alias binding, optionally renamed to
// match a corpus method (all decline-table corpora name their function "F").
func jsonAliasBinding(name ...string) bamlutils.NativeSpineUnaryBinding {
	b := nativespinejsonfixture.Binding()
	if len(name) > 0 {
		b.Method = name[0]
	}
	return b
}

// clientBlock is the shared client for the decline-table corpora.
const clientBlock = `client<llm> C {
  provider openai
  options { model "gpt-4o-mini" api_key "sk-x" base_url "http://127.0.0.1:0/v1" }
}
`

func corpus(types, fn string) map[string]string {
	return map[string]string{
		"clients.baml":   clientBlock,
		"types.baml":     types,
		"functions.baml": fn,
	}
}

// TestNewUnaryExecutor_AdmitsExactJSONAlias proves the positive registers.
func TestNewUnaryExecutor_AdmitsExactJSONAlias(t *testing.T) {
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: jsonAliasFunction(t), Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	got := e.Methods()
	if len(got) != 1 || got[0] != jsonAliasMethodName {
		t.Fatalf("Methods() = %v, want [%s]", got, jsonAliasMethodName)
	}
}

// TestRegistrationDeclineTable proves the registry REJECTS every non-cohort shape
// before serving begins — the lockstep totality gate (debaml.SupportsNativeStaticStreamBundle)
// and the required-scalar input gate, plus the callback/name/duplicate/version checks.
// Emittable is not population-admitted.
func TestRegistrationDeclineTable(t *testing.T) {
	pos := jsonAliasFunction(t)

	// Corpus-driven output/input negatives (the classifier admits these recursive
	// aliases / classes / list inputs; the spine registry declines them).
	jsonValue := corpus(
		"type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>",
		`function F(topic: string) -> JsonValue { client C prompt #"{{ topic }}"# }`)
	reordered := corpus(
		"type JsonValueReordered = float | int | bool | string | null | JsonValueReordered[] | map<string, JsonValueReordered>",
		`function F(topic: string) -> JsonValueReordered { client C prompt #"{{ topic }}"# }`)
	classRet := corpus(
		"class Wrap { x string }",
		`function F(topic: string) -> Wrap { client C prompt #"{{ topic }}"# }`)
	scalarRet := corpus("", `function F(topic: string) -> string { client C prompt #"{{ topic }}"# }`)
	listInput := corpus(
		"type JSON = int | string | bool | JSON[] | map<string, JSON>",
		`function F(tags: string[]) -> JSON { client C prompt #"{{ tags }}"# }`)

	cases := []struct {
		name string
		m    spine.SpineMethod
		want string // substring of the registration error
	}{
		{"jsonvalue_alias", method(reconstructFromCorpus(t, jsonValue, "F"), jsonAliasBinding("F")), "JSON alias cohort"},
		{"reordered_alias", method(reconstructFromCorpus(t, reordered, "F"), jsonAliasBinding("F")), "JSON alias cohort"},
		{"class_return", method(reconstructFromCorpus(t, classRet, "F"), jsonAliasBinding("F")), "JSON alias cohort"},
		{"scalar_return", method(reconstructFromCorpus(t, scalarRet, "F"), jsonAliasBinding("F")), "JSON alias cohort"},
		{"non_scalar_list_input", method(reconstructFromCorpus(t, listInput, "F"), jsonAliasBinding("F")), "required-scalar cohort"},
		{"nil_project_input", spine.SpineMethod{Function: pos, Binding: bamlutils.NativeSpineUnaryBinding{Method: pos.Method, ProjectInput: nil, DecodeFinal: jsonAliasBinding().DecodeFinal}}, "ProjectInput is nil"},
		{"nil_decode_final", spine.SpineMethod{Function: pos, Binding: bamlutils.NativeSpineUnaryBinding{Method: pos.Method, ProjectInput: jsonAliasBinding().ProjectInput, DecodeFinal: nil}}, "DecodeFinal is nil"},
		{"binding_name_mismatch", spine.SpineMethod{Function: pos, Binding: renameBinding(jsonAliasBinding(), "Other")}, "name mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spine.NewUnaryExecutor([]spine.SpineMethod{tc.m}, nil)
			if err == nil {
				t.Fatalf("NewUnaryExecutor admitted %s, want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error %q, want substring %q", tc.name, err, tc.want)
			}
		})
	}

	// Duplicate method: two admitted positives with the same name.
	if _, err := spine.NewUnaryExecutor([]spine.SpineMethod{
		{Function: pos, Binding: jsonAliasBinding()},
		{Function: pos, Binding: jsonAliasBinding()},
	}, nil); err == nil || !strings.Contains(err.Error(), "duplicate method") {
		t.Fatalf("duplicate registration error = %v, want 'duplicate method'", err)
	}
}

// method pairs a reconstructed function with the JSON-alias binding renamed to the
// function's method name (the corpora all name their function "F").
func method(fn promptdescriptor.Function, b bamlutils.NativeSpineUnaryBinding) spine.SpineMethod {
	return spine.SpineMethod{Function: fn, Binding: b}
}

func renameBinding(b bamlutils.NativeSpineUnaryBinding, name string) bamlutils.NativeSpineUnaryBinding {
	b.Method = name
	return b
}

// TestCarrierRoundTripParity replays a corpus of canonical JSON values through the
// socket-free parse route (native exact-JSON final parse -> canonical JSON -> emitted
// DecodeStaticAliasFinal[OutputJson]) and re-marshals the concrete carrier, asserting
// byte-identical round-trips. Each input is already in the native canonical form
// (sorted keys, no whitespace), so a faithful carrier reproduces it exactly. This is
// the carrier half of the frozen-v0.223 parity: ParseStaticBundleUnaryCall's byte
// equivalence to stock v0.223 is proven by the internal/debaml + staticoracle
// differentials this lane reuses unchanged.
func TestCarrierRoundTripParity(t *testing.T) {
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: jsonAliasFunction(t), Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	ctx := context.Background()
	for _, canonical := range []string{
		`1`,
		`"hello"`,
		`true`,
		`[1,"two",true]`,
		`{"k":1}`,
		`{"a":[1,2],"b":{"c":3}}`,
		`[]`,
		`[[1],[2,3]]`,
		`{"nested":{"deep":[true,false]}}`,
	} {
		out, err := e.Parse(ctx, jsonAliasMethodName, canonical)
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
// pre-socket decline. A Succeeded or FailedAfterClaim result passes through UNCHANGED
// — the composite never falls back after the claim. It proves the ownership boundary
// the emitted module never sees (the module knows nothing about any oracle).
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
	// Succeeded / FailedAfterClaim: terminal — pass through, NEVER fall back.
	return r
}

func (c *fallbackComposite) Parse(ctx context.Context, method string, raw string) (any, error) {
	return c.inner.Parse(ctx, method, raw)
}

// TestFallbackCompositeInvokesOnceOnlyOnPreSocketDecline proves the outer composite
// invokes the fallback EXACTLY once on a pre-socket decline (registry miss), and the
// inner spine executor opened no socket.
func TestFallbackCompositeInvokesOnceOnlyOnPreSocketDecline(t *testing.T) {
	inner, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: jsonAliasFunction(t), Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
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

// TestCallRegistryMissDeclinesPreSocket proves an unregistered method Call declines
// pre-socket with the typed capability error and zero sockets.
func TestCallRegistryMissDeclinesPreSocket(t *testing.T) {
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: jsonAliasFunction(t), Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
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

// TestCallProviderDeclinesPreSocket proves a per-call admission decline (a non-openai
// provider) is a pre-socket decline with zero sockets — reached before any nanollm work.
func TestCallProviderDeclinesPreSocket(t *testing.T) {
	fn := jsonAliasFunction(t)
	fn.Provider = "anthropic"
	fn.ClientConfig.Provider = "anthropic"
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: fn, Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	res := e.Call(context.Background(), jsonAliasMethodName, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "x"})
	if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
		t.Fatalf("disposition = %v (reason %q), want declined_pre_socket", res.Disposition, res.Reason)
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Failures != 0 {
		t.Fatalf("metrics = %+v, want zero sockets/claims/failures", snap)
	}
}

// TestParseRoute proves the socket-free parse route: an admitted method natively
// parses+decodes raw into the concrete carrier; an unadmitted method returns the typed
// capability decline; malformed raw is an ORDINARY parse error (not a capability decline).
func TestParseRoute(t *testing.T) {
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: jsonAliasFunction(t), Binding: jsonAliasBinding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	ctx := context.Background()

	// Admitted method: raw model text -> native final parse -> emitted carrier.
	out, err := e.Parse(ctx, jsonAliasMethodName, `[1,"two",true]`)
	if err != nil {
		t.Fatalf("Parse(admitted): %v", err)
	}
	carrier, ok := out.(nativespinejsonfixture.OutputJson)
	if !ok {
		t.Fatalf("parse output type = %T, want nativespinejsonfixture.OutputJson", out)
	}
	if !carrier.IsVariant3() { // top-level JSON array -> list arm
		b, _ := json.Marshal(carrier)
		t.Fatalf("parsed carrier is not the list arm: %s", b)
	}

	// Unadmitted method: typed capability decline.
	if _, err := e.Parse(ctx, "Nope", `1`); err == nil {
		t.Fatal("Parse(unadmitted) succeeded, want typed decline")
	} else {
		var typed *bamlutils.NativeSpineUnsupportedMethodError
		if !errors.As(err, &typed) {
			t.Fatalf("Parse(unadmitted) err = %v (%T), want *NativeSpineUnsupportedMethodError", err, err)
		}
	}

	// Malformed raw for an admitted method: an ordinary terminal parse error, NOT a
	// capability decline.
	if _, err := e.Parse(ctx, jsonAliasMethodName, `not json at all !!!`); err == nil {
		t.Fatal("Parse(malformed) succeeded, want parse error")
	} else {
		var typed *bamlutils.NativeSpineUnsupportedMethodError
		if errors.As(err, &typed) {
			t.Fatalf("Parse(malformed) returned a capability decline (%v); malformed raw for an admitted method must be an ordinary parse error", err)
		}
	}
}
