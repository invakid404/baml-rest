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
// carriers (the exact five-arm JSON alias, the six-arm JsonValue, and representative
// non-matching shapes) and FAILS on any disagreement, so a future change to one predicate but
// not the other is caught in CI rather than silently mis-routing the decoder at runtime.
//
// De-BAML Phase 3c keeps the agreement EXACT rather than one-way. `JsonValue` is served on
// the FINAL lane but declined on the STREAM lane (internal/debaml/static_stream_serve.go
// explains why), so the stream-side codegen fingerprint was deliberately left five-arm-only:
// both sides say "no" for the JsonValue carrier, and [TestAliasFinalVsStreamAdmissionSplit]
// below pins that the FINAL side still says "yes".

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
			carrier: streamCarrierFor(t, "StaticRecursiveAliasJSON"),
			admit:   true,
			note:    "the exact five-arm JSON alias (int|string|bool|[]JSON|map<string,JSON>)",
		},
		{
			method:  "StaticRecursiveAliasJsonValue",
			carrier: streamCarrierFor(t, "StaticRecursiveAliasJsonValue"),
			admit:   false,
			note:    "the six-arm nullable JsonValue carrier — FINAL-served since Phase 3c, but STREAM-declined",
		},
		{
			method:  "StaticRecursiveAliasJsonValueReordered",
			carrier: streamCarrierFor(t, "StaticRecursiveAliasJsonValueReordered"),
			admit:   false,
			note:    "the Phase-3c residual decline witness (JsonValue's arm set, float before int)",
		},
		{
			method:  "StaticCompletion",
			carrier: streamCarrierFor(t, "StaticCompletion"),
			admit:   false,
			note:    "a top-level string scalar (not a pointer union)",
		},
		{
			method:  "StaticRecursiveNode",
			carrier: streamCarrierFor(t, "StaticRecursiveNode"),
			admit:   false,
			note:    "a recursive CLASS carrier (pointer graph, not the alias union)",
		},
		{
			method:  "StaticOutputFormat",
			carrier: streamCarrierFor(t, "StaticOutputFormat"),
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

// TestAliasFinalVsStreamAdmissionSplit pins the Phase-3c admission ASYMMETRY end to end: the
// `JsonValue` family is admitted by the FINAL predicates and the FINAL decoder selection,
// and declined by the STREAM predicate and the STREAM decoder selection — while `JSON` is
// admitted by both.
//
// The split is not incidental. The static-stream gate admits by descriptor SHAPE pre-socket
// and a claimed stream has no route back to BAML, so a family whose parse can decline on a
// VALUE must not claim a stream socket; the unary lane repairs the same response through
// BAML parse-only, so it can. Asserting both halves here means neither can drift on its own.
func TestAliasFinalVsStreamAdmissionSplit(t *testing.T) {
	cases := []struct {
		method string
		// wantAliasFinal: admitted by the served-ALIAS final predicate.
		// wantFinalSupported: final-supported at all (a Phase-2 class or an 8C scalar is
		// final-supported without being an alias family).
		// wantStrm: admitted by the static-STREAM gate.
		wantAliasFinal, wantFinalSupported, wantStrm bool
	}{
		{"StaticRecursiveAliasJSON", true, true, true},
		{"StaticRecursiveAliasJsonValue", true, true, false},
		{"StaticRecursiveAliasJsonValueReordered", false, false, false},
		{"StaticRecursiveNode", false, true, false},
		{"StaticCompletion", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			bundle := lowerReturn(t, tc.method)
			if got := debaml.IsProvenServedRecursiveAliasStaticFamily(bundle); got != tc.wantAliasFinal {
				t.Errorf("served-ALIAS final predicate = %v, want %v", got, tc.wantAliasFinal)
			}
			if got := debaml.SupportsNativeFinalBundle(bundle) == nil; got != tc.wantFinalSupported {
				t.Errorf("SupportsNativeFinalBundle ok = %v, want %v", got, tc.wantFinalSupported)
			}
			if got := debaml.IsProvenRecursiveAliasStaticStreamFamily(bundle); got != tc.wantStrm {
				t.Errorf("STREAM served = %v, want %v", got, tc.wantStrm)
			}
			if got := debaml.SupportsNativeStaticStreamBundle(bundle) == nil; got != tc.wantStrm {
				t.Errorf("SupportsNativeStaticStreamBundle ok = %v, want %v", got, tc.wantStrm)
			}
			// The codegen STREAM carrier fingerprint must track the runtime STREAM gate
			// exactly — including saying "no" for the FINAL-served JsonValue carrier.
			carrier := streamCarrierFor(t, tc.method)
			if got := codegen.IsAdmittedJSONAliasStreamCarrier(carrier); got != tc.wantStrm {
				t.Errorf("codegen stream carrier admit = %v, want %v (must equal the runtime STREAM gate)", got, tc.wantStrm)
			}
		})
	}
}

// streamCarrierFor returns the generated ParseStream return type for one fixture method —
// exactly what codegen fingerprints as methodEmitter.streamOutType.
//
// It is the SINGLE source of truth for the fingerprint input: both the agreement table and
// TestAliasFinalVsStreamAdmissionSplit resolve carriers through it, so a future method
// rename cannot be applied in one place only and leave the other test silently
// fingerprinting the wrong carrier. It t.Fatalf's on an unwired method rather than
// returning a nil type, so a new table row must be wired here explicitly.
func streamCarrierFor(t *testing.T, method string) reflect.Type {
	t.Helper()
	switch method {
	case "StaticRecursiveAliasJSON":
		return reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveAliasJSON).Out(0)
	case "StaticRecursiveAliasJsonValue":
		return reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveAliasJsonValue).Out(0)
	case "StaticRecursiveAliasJsonValueReordered":
		return reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveAliasJsonValueReordered).Out(0)
	case "StaticRecursiveNode":
		return reflect.TypeOf(bamlclient.ParseStream.StaticRecursiveNode).Out(0)
	case "StaticCompletion":
		return reflect.TypeOf(bamlclient.ParseStream.StaticCompletion).Out(0)
	case "StaticOutputFormat":
		return reflect.TypeOf(bamlclient.ParseStream.StaticOutputFormat).Out(0)
	}
	t.Fatalf("no stream carrier wired for %q", method)
	return nil
}
