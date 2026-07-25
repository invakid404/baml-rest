//go:build integration

// De-BAML Phase 3b — cross-package fingerprint AGREEMENT.
//
// The static-stream lane has TWO independently-maintained implementations of the same
// "exact five-arm JSON alias" admitted-shape contract, in DIFFERENT modules:
//
//   - codegen (adapters/common): codegen.IsAdmittedJSONAliasStreamCarrier — a reflect
//     fingerprint over the GENERATED ParseStream return type, used at generation time to
//     route the alias stream through the narrow DecodeStaticAliasStream decoder.
//   - runtime (internal/debaml): debaml.IsProvenRecursiveAliasStaticStreamFamily — over the
//     lowered Return Bundle, the admission gate that decides whether the alias claims a socket.
//
// The codegen fingerprint's own doc comment promises it "mirrors" the runtime predicate so
// the emitted decoder selection cannot drift from admission — but that guarantee otherwise
// rests entirely on manual synchronization. This test drives BOTH predicates over the SAME
// carriers (the exact five-arm JSON alias, the wider JsonValue, and representative
// non-matching shapes) and FAILS on any disagreement, so a future change to one predicate but
// not the other is caught in CI rather than silently mis-routing the decoder at runtime.

package staticoracle

import (
	"reflect"
	"testing"

	codegen "github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/internal/debaml"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
)

func TestStreamCarrierFingerprintAgreement(t *testing.T) {
	// carrier is the GENERATED ParseStream return type (exactly what codegen fingerprints as
	// methodEmitter.streamOutType); method's Return lowers to the runtime admission bundle.
	cases := []struct {
		method  string
		carrier reflect.Type
		admit   bool
		note    string
	}{
		{
			method:  "StaticRecursiveAliasJSON",
			carrier: reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveAliasJSON).Out(0),
			admit:   true,
			note:    "the exact five-arm JSON alias (int|string|bool|[]JSON|map<string,JSON>)",
		},
		{
			method:  "StaticRecursiveAliasJsonValue",
			carrier: reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveAliasJsonValue).Out(0),
			admit:   false,
			note:    "the WIDER JsonValue carrier (adds a float64 arm + a null-able alias)",
		},
		{
			method:  "StaticCompletion",
			carrier: reflect.TypeOf(bamlclient.ParseStream.StaticCompletion).Out(0),
			admit:   false,
			note:    "a top-level string scalar (not a pointer union)",
		},
		{
			method:  "StaticRecursiveNode",
			carrier: reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveNode).Out(0),
			admit:   false,
			note:    "a recursive CLASS carrier (pointer graph, not the alias union)",
		},
		{
			method:  "StaticOutputFormat",
			carrier: reflect.TypeOf(bamlclient.ParseStream.StaticOutputFormat).Out(0),
			admit:   false,
			note:    "a flat class carrier (StaticAnswer)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			bundle := lowerReturn(t, tc.method)
			runtimeAdmit := debaml.IsProvenRecursiveAliasStaticStreamFamily(bundle)
			codegenAdmit := codegen.IsAdmittedJSONAliasStreamCarrier(tc.carrier)

			if runtimeAdmit != codegenAdmit {
				t.Fatalf("FINGERPRINT DRIFT for %s (%s):\n  runtime debaml.IsProvenRecursiveAliasStaticStreamFamily = %v\n  codegen.IsAdmittedJSONAliasStreamCarrier(%s) = %v\nthe codegen decoder-selection fingerprint and the runtime admission gate disagree",
					tc.method, tc.note, runtimeAdmit, tc.carrier, codegenAdmit)
			}
			if runtimeAdmit != tc.admit {
				t.Fatalf("%s (%s): both predicates agree on %v, but the expected admitted verdict is %v",
					tc.method, tc.note, runtimeAdmit, tc.admit)
			}
		})
	}

	// Structural BOUND check (no BAML bundle counterpart — this exercises the codegen
	// fingerprint's EXACT-shape contract directly). A synthetic carrier with the exact five
	// arms + a SINGLE discriminator must be ADMITTED, but the SAME five arms PLUS a second
	// string-kind field must be REJECTED: the discriminator/field skip is bounded, so an extra
	// field can never silently widen the served fingerprint (CodeRabbit TpeYp).
	t.Run("exact-shape-bound", func(t *testing.T) {
		exact := reflect.TypeOf((*syntheticAliasCarrier)(nil))
		if !codegen.IsAdmittedJSONAliasStreamCarrier(exact) {
			t.Fatalf("synthetic exact five-arm + single-discriminator carrier must be ADMITTED (got reject); the fingerprint is too strict")
		}
		extra := reflect.TypeOf((*syntheticAliasCarrierExtraField)(nil))
		if codegen.IsAdmittedJSONAliasStreamCarrier(extra) {
			t.Fatalf("carrier with the five arms PLUS an extra string field must be REJECTED (got admit); the fingerprint is UNBOUNDED and would mis-route the decoder")
		}
	})
}

// syntheticAliasCarrier mirrors the generated five-arm pointer union (int|string|bool|[]self|
// map<string,self>) with exactly ONE discriminator — the exact admitted shape, built here so
// the fingerprint's admit path is exercised structurally (the list/map arms self-reference the
// carrier's own pointer type, as the real Union5 does).
type syntheticAliasCarrier struct {
	variant string

	vInt  *int64
	vStr  *string
	vBool *bool
	vList *[]*syntheticAliasCarrier
	vMap  *map[string]*syntheticAliasCarrier
}

// syntheticAliasCarrierExtraField is the SAME five arms plus a SECOND string-kind field — the
// drift case the tightened (bounded-discriminator) fingerprint must REJECT.
type syntheticAliasCarrierExtraField struct {
	variant  string
	extraTag string

	vInt  *int64
	vStr  *string
	vBool *bool
	vList *[]*syntheticAliasCarrierExtraField
	vMap  *map[string]*syntheticAliasCarrierExtraField
}
