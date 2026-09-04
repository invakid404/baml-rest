package admission

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// static_spine_stream_test.go pins the M3e-A spine STREAM admission entry's own gates
// from THIS side of the module boundary, without a socket or any nanollm work: the
// totality cut it applies, the mode set it admits, and the fail-closed rewrite/proxy
// rule. The lane's cohort-gate exemption and its compensating default-deny proof live in
// cohort_test.go.

// aliasDescriptorBundle builds a schemadescriptor Bundle for a recursive alias with the
// given ordered arms — the descriptor form of the two served alias families.
func aliasDescriptorBundle(method, name string, nullable bool, arms []schemadescriptor.Type) schemadescriptor.Bundle {
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  method,
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeRecursiveAlias, Name: name, Mode: schemadescriptor.NonStreaming},
		StructuralRecursiveAliases: []schemadescriptor.RecursiveAliasDef{{
			Name:   name,
			Target: schemadescriptor.Type{Kind: schemadescriptor.TypeUnion, Union: &schemadescriptor.UnionType{Nullable: nullable, Variants: arms}},
		}},
	}
}

func descPrim(k schemadescriptor.PrimitiveKind) schemadescriptor.Type {
	return schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: k}
}

func descAliasRef(name string) *schemadescriptor.Type {
	return &schemadescriptor.Type{Kind: schemadescriptor.TypeRecursiveAlias, Name: name, Mode: schemadescriptor.NonStreaming}
}

// descJSONArms is the exact five-arm `JSON` alias — the ONE stream-admitted family.
func descJSONArms() []schemadescriptor.Type {
	return []schemadescriptor.Type{
		descPrim(schemadescriptor.PrimitiveInt),
		descPrim(schemadescriptor.PrimitiveString),
		descPrim(schemadescriptor.PrimitiveBool),
		{Kind: schemadescriptor.TypeList, Elem: descAliasRef("JSON")},
		{Kind: schemadescriptor.TypeMap, Key: &schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}, Value: descAliasRef("JSON")},
	}
}

// descJsonValueArms is the six ordered stored variants of `JsonValue` — the family the
// FINAL gate admits and the STREAM gate must NOT.
func descJsonValueArms() []schemadescriptor.Type {
	return []schemadescriptor.Type{
		descPrim(schemadescriptor.PrimitiveInt),
		descPrim(schemadescriptor.PrimitiveFloat),
		descPrim(schemadescriptor.PrimitiveBool),
		descPrim(schemadescriptor.PrimitiveString),
		{Kind: schemadescriptor.TypeList, Elem: descAliasRef("JsonValue")},
		{Kind: schemadescriptor.TypeMap, Key: &schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}, Value: descAliasRef("JsonValue")},
	}
}

// spineStreamInput is a StaticStreamInput that reaches the spine lane's return-shape
// gate: the layer-1 facts are the lane's constants, the orchestration plan is a single
// leaf, the descriptor envelope agrees, and the (empty) arg binder matches. Everything
// AFTER the totality gate (render / normalize / nanollm Prepare) is deliberately never
// reached by these rows.
func spineStreamInput(ret schemadescriptor.Bundle) StaticStreamInput {
	return StaticStreamInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           RouteKindStatic,
		Method:              ret.Method,
		Mode:                bamlutils.NativeStreamModeStream,
		SingleLeaf:          true,
		Provider:            "openai",
		Descriptor: promptdescriptor.Function{
			Version:  promptdescriptor.Version,
			Method:   ret.Method,
			Provider: "openai",
			Return:   ret,
		},
		WouldRewriteOrProxy: func(string) bool { return false },
	}
}

// declineOf drives the entry and returns its typed pre-socket decline.
func declineOf(t *testing.T, in StaticStreamInput) *StaticDecline {
	t.Helper()
	claim, err := AdmitStaticSpineStreamClaim(context.Background(), in)
	if claim != nil {
		claim.Close()
		t.Fatal("AdmitStaticSpineStreamClaim returned a claim where a decline was expected")
	}
	d, ok := err.(*StaticDecline)
	if !ok {
		t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
	}
	return d
}

// TestAdmitStaticSpineStreamClaimTotalityGate is the mutation bite for the one that
// matters most: the spine stream lane must apply the exact STREAM totality predicate,
// never the wider FINAL one. The `JsonValue` family passes the final gate and would sail
// through a widened predicate — but a value-scoped decline on a claimed stream has no
// route back, so it must decline PRE-SOCKET here.
func TestAdmitStaticSpineStreamClaimTotalityGate(t *testing.T) {
	cases := []struct {
		name string
		ret  schemadescriptor.Bundle
		// atTotalityGate marks the rows that MUST reach — and be cut by — the spine
		// lane's own totality gate. They are the ones that pass every SHARED gate before
		// it (in particular the wider final-support gate), so a widened predicate would
		// let them through to a socket. The other rows are cut EARLIER by a shared gate,
		// which is equally pre-socket; asserting a specific earlier reason would just
		// pin an unrelated gate's message.
		atTotalityGate bool
		why            string
	}{
		{
			name:           "json_value_family_declines",
			ret:            aliasDescriptorBundle("M", "JsonValue", true, descJsonValueArms()),
			atTotalityGate: true,
			why:            "FINAL-served but STREAM-declined; a wider final predicate here would claim a socket it cannot own",
		},
		{
			name:           "scalar_return_declines",
			ret:            schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "M", Target: descPrim(schemadescriptor.PrimitiveString)},
			atTotalityGate: true,
			why:            "a scalar return passes final support but is not the alias family",
		},
		{
			name: "reordered_json_arms_decline",
			ret: aliasDescriptorBundle("M", "JSON", false, func() []schemadescriptor.Type {
				a := descJSONArms()
				a[0], a[1] = a[1], a[0]
				return a
			}()),
			why: "the ordered arm list is part of the fingerprint (cut by the shared final-support gate first)",
		},
		{
			name: "nullable_json_declines",
			ret:  aliasDescriptorBundle("M", "JSON", true, descJSONArms()),
			why:  "the frozen JSON predicate requires a non-nullable union (cut by the shared final-support gate first)",
		},
		{
			name: "renamed_json_declines",
			ret:  aliasDescriptorBundle("M", "Blob", false, descJSONArms()),
			why:  "the canonical alias name is pinned (the self-references no longer resolve, so lowering rejects it first)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := declineOf(t, spineStreamInput(tc.ret))
			if tc.atTotalityGate {
				if Reason(d.Reason) != reasonSpineNotExactAlias {
					t.Fatalf("(stage, reason) = (%q, %q), want reason %q (%s)", d.Stage, d.Reason, reasonSpineNotExactAlias, tc.why)
				}
				return
			}
			// Cut earlier, but still PRE-SOCKET and never a claim (declineOf proves the
			// claim half).
			if d.Reason == "" {
				t.Fatalf("decline carries no bounded reason (%s)", tc.why)
			}
		})
	}
}

// TestAdmitStaticSpineStreamClaimModeGate proves the stream lane admits EXACTLY the two
// real streaming modes: a unary/parse/unknown native mode declines at the mode gate,
// before the descriptor, the totality cut, or any nanollm work.
func TestAdmitStaticSpineStreamClaimModeGate(t *testing.T) {
	for _, mode := range []bamlutils.NativeStreamMode{"", "final", "call_with_raw", "parse"} {
		in := spineStreamInput(aliasDescriptorBundle("M", "JSON", false, descJSONArms()))
		in.Mode = mode
		d := declineOf(t, in)
		if Stage(d.Stage) != StageMode {
			t.Fatalf("mode %q declined at stage %q (reason %q), want the mode gate", mode, d.Stage, d.Reason)
		}
	}
}

// The MANDATORY fail-closed rewrite/proxy gate is proven in
// static_spine_stream_prepare_test.go, against a fully valid input that reaches the
// PREPARED plan and ADMITS when the predicate reports an untouched target. It cannot be
// proven from the incomplete fixture here: this one declines during render/prepare
// anyway, so "it declined" would hold even with the gate deleted.
