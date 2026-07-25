package debaml

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3c — the `JsonValue` family's CGO-free unit surface: the second
// name-pinned fingerprint (its accept + the negatives that must keep declining), the
// float arm's byte-exact public formatting, the first-class typed null, and the
// BYTE-UNCHANGED regression that pins the shipped `JSON` family's outputs against the
// shared machinery this slice parameterized.

// jsonValueAliasBundle builds the exact served `JsonValue` alias bundle — the NULLABLE
// union with the six ordered stored variants — matching the generated introspected static
// descriptor for StaticRecursiveAliasJsonValue.
func jsonValueAliasBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	b := jsonValueBundleWith(func(*schema.Bundle) {})
	if err := b.RebuildIndexes(); err != nil {
		t.Fatalf("RebuildIndexes: %v", err)
	}
	return b
}

// jsonValueBundleWith builds the exact JsonValue bundle and applies mutate before the
// indexes are rebuilt, so a negative case can perturb exactly one fact.
func jsonValueBundleWith(mutate func(*schema.Bundle)) *schema.Bundle {
	ref := func() *schema.Type {
		return &schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue", Mode: schema.NonStreaming}
	}
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue", Mode: schema.NonStreaming},
		StructuralRecursiveAliases: []schema.RecursiveAliasDef{{
			Name: "JsonValue",
			Target: schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Nullable: true, Variants: []schema.Type{
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
				{Kind: schema.TypeList, Elem: ref()},
				{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: ref()},
			}}},
		}},
	}
	mutate(b)
	return b
}

// TestJsonValueFingerprint_AcceptsExactShape proves the second predicate admits exactly
// the ratified shape, and that the two family predicates are DISJOINT — neither family
// can ever be admitted by the other's fingerprint.
func TestJsonValueFingerprint_AcceptsExactShape(t *testing.T) {
	jv := jsonValueAliasBundle(t)
	if !IsProvenJsonValueRecursiveAliasStaticFamily(jv) {
		t.Fatal("the exact JsonValue shape must be admitted by the JsonValue predicate")
	}
	if IsProvenRecursiveAliasStaticFamily(jv) {
		t.Fatal("the JsonValue shape must NOT be admitted by the frozen JSON predicate")
	}
	if !IsProvenServedRecursiveAliasStaticFamily(jv) {
		t.Fatal("the JsonValue shape must be admitted by the either-family helper")
	}
	prof, ok := admittedJsonValueRecursiveAliasProfile(jv)
	if !ok || !prof.isJsonValue() || !prof.nullable || prof.aliasName != "JsonValue" {
		t.Fatalf("JsonValue profile = %+v, want {JsonValue, family=JsonValue, nullable}", prof)
	}
	// FINAL-served, STREAM-declined. The stream gate admits by descriptor SHAPE pre-socket
	// and a claimed stream has no route back to BAML, so a family whose parse can decline
	// on a VALUE must not claim a stream socket (static_stream_serve.go). The unary lane
	// repairs the same response through BAML parse-only, so it can.
	if err := SupportsNativeFinalBundle(jv); err != nil {
		t.Fatalf("JsonValue must be FINAL-supported: %v", err)
	}
	if IsProvenRecursiveAliasStaticStreamFamily(jv) {
		t.Fatal("JsonValue must NOT be the proven static-STREAM family")
	}
	if err := SupportsNativeStaticStreamBundle(jv); err == nil {
		t.Fatal("JsonValue must DECLINE SupportsNativeStaticStreamBundle")
	}
	// The JSON family's stream admission is untouched by that narrowing.
	if !IsProvenRecursiveAliasStaticStreamFamily(jsonAliasBundle(t)) {
		t.Fatal("the JSON family must still be the proven static-STREAM family")
	}

	js := jsonAliasBundle(t)
	if !IsProvenRecursiveAliasStaticFamily(js) {
		t.Fatal("the exact JSON shape must stay admitted by the frozen JSON predicate")
	}
	if IsProvenJsonValueRecursiveAliasStaticFamily(js) {
		t.Fatal("the JSON shape must NOT be admitted by the JsonValue predicate")
	}
	if jsProf, _ := admittedRecursiveAliasProfile(js); jsProf.isJsonValue() || jsProf.nullable {
		t.Fatalf("JSON profile = %+v, want family=JSON and nullable=false", jsProf)
	}
}

// TestJsonValueFingerprint_Negatives is the exhaustive per-fact negative matrix: each case
// perturbs EXACTLY ONE fact of the ratified shape and must decline at BOTH the JsonValue
// predicate and the either-family helper (and, since a decline must be total, at the
// final and static-stream support gates too).
func TestJsonValueFingerprint_Negatives(t *testing.T) {
	strP := func() *schema.Type {
		return &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	}
	cases := []struct {
		name   string
		mutate func(*schema.Bundle)
	}{
		{"renamed alias", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Name = "Blob"
			b.Target.Name = "Blob"
			def := &b.StructuralRecursiveAliases[0]
			vs := def.Target.Union.Variants
			vs[4].Elem.Name = "Blob"
			vs[5].Value.Name = "Blob"
		}},
		{"reordered arms (float before int)", func(b *schema.Bundle) {
			vs := b.StructuralRecursiveAliases[0].Target.Union.Variants
			vs[0], vs[1] = vs[1], vs[0]
		}},
		{"reordered arms (bool/string swapped)", func(b *schema.Bundle) {
			vs := b.StructuralRecursiveAliases[0].Target.Union.Variants
			vs[2], vs[3] = vs[3], vs[2]
		}},
		{"extra seventh arm", func(b *schema.Bundle) {
			def := &b.StructuralRecursiveAliases[0]
			def.Target.Union.Variants = append(def.Target.Union.Variants,
				schema.Type{Kind: schema.TypeMap, Key: strP(), Value: strP()})
		}},
		{"missing float arm", func(b *schema.Bundle) {
			def := &b.StructuralRecursiveAliases[0]
			vs := def.Target.Union.Variants
			def.Target.Union.Variants = append(append([]schema.Type{}, vs[0]), vs[2:]...)
		}},
		{"NON-nullable union (no null arm)", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Nullable = false
		}},
		{"float arm replaced by another int", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Variants[1].Primitive = schema.PrimitiveInt
		}},
		{"constrained target reference", func(b *schema.Bundle) {
			b.Target.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this > 0"}}
		}},
		{"dynamic target reference", func(b *schema.Bundle) { b.Target.Dynamic = true }},
		{"streaming-mode target reference", func(b *schema.Bundle) { b.Target.Mode = schema.Streaming }},
		{"constraint on the float arm", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Variants[1].Meta.Constraints =
				[]schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this > 0"}}
		}},
		{"constraint on the union itself", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Meta.Constraints =
				[]schema.Constraint{{Level: schema.ConstraintAssert, Expression: "this != null"}}
		}},
		{"non-string map key", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Variants[5].Key =
				&schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
		}},
		{"map value is not the self reference", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Variants[5].Value = strP()
		}},
		{"list element is not the self reference", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases[0].Target.Union.Variants[4].Elem = strP()
		}},
		{"target is not the alias reference", func(b *schema.Bundle) {
			b.Target = *strP()
		}},
		{"a second recursive alias in the bundle", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases = append(b.StructuralRecursiveAliases, schema.RecursiveAliasDef{
				Name:   "Other",
				Target: schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{*strP()}}},
			})
		}},
		{"a class alongside the alias", func(b *schema.Bundle) {
			b.Classes = append(b.Classes, schema.ClassDef{Name: schema.Name{Name: "C"}})
		}},
		{"an enum alongside the alias", func(b *schema.Bundle) {
			b.Enums = append(b.Enums, schema.EnumDef{Name: schema.Name{Name: "E"}})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := jsonValueBundleWith(tc.mutate)
			// RebuildIndexes may legitimately fail for a deliberately-malformed shape; the
			// predicates must decline either way, so a failure here is not fatal.
			_ = b.RebuildIndexes()
			if IsProvenJsonValueRecursiveAliasStaticFamily(b) {
				t.Error("must NOT be admitted by the JsonValue predicate")
			}
			if IsProvenRecursiveAliasStaticFamily(b) {
				t.Error("must NOT be admitted by the frozen JSON predicate either")
			}
			if IsProvenServedRecursiveAliasStaticFamily(b) {
				t.Error("must NOT be admitted by the either-family helper")
			}
			if IsProvenRecursiveAliasStaticStreamFamily(b) {
				t.Error("must NOT be admitted by the static-STREAM predicate")
			}
			if err := SupportsNativeFinalBundle(b); err == nil {
				t.Error("must DECLINE SupportsNativeFinalBundle")
			}
			if err := SupportsNativeStaticStreamBundle(b); err == nil {
				t.Error("must DECLINE SupportsNativeStaticStreamBundle")
			}
		})
	}
}

// TestJsonValueCoerce_ByteExact pins the native JsonValue FinalJSON bytes against the
// values captured from stock BAML v0.223 Parse.StaticRecursiveAliasJsonValue +
// json.Marshal (the live oracle probe). It is the CGO-free twin of the integration
// differential: every row settles one of the two family deltas.
func TestJsonValueCoerce_ByteExact(t *testing.T) {
	b := jsonValueAliasBundle(t)
	cases := []struct{ in, want string }{
		// int arm (as_i64 Some)
		{`1`, `1`}, {`0`, `0`}, {`-7`, `-7`},
		{`9223372036854775807`, `9223372036854775807`},
		{`-9223372036854775808`, `-9223372036854775808`},
		// float arm (as_i64 None) — public bytes are Go json.Marshal of the float64, so
		// the provider's lexeme is intentionally lost.
		{`1.0`, `1`}, {`3.0`, `3`}, {`1.5`, `1.5`}, {`-2.5`, `-2.5`}, {`0.0`, `0`},
		{`1e3`, `1000`}, {`1.2e5`, `120000`}, {`1.2e-5`, `0.000012`},
		{`1e-7`, `1e-7`}, {`1e20`, `100000000000000000000`}, {`1e21`, `1e+21`},
		{`5e-324`, `5e-324`}, {`1.7976931348623157e308`, `1.7976931348623157e+308`},
		{`9223372036854775808`, `9223372036854776000`},
		{`-9223372036854775809`, `-9223372036854776000`},
		{`123456789012345678901234567890`, `1.2345678901234568e+29`},
		// the NULL arm — a first-class typed null, NOT the JSON family's [] trap
		{`null`, `null`}, {`[null]`, `[null]`}, {`{"n":null}`, `{"n":null}`},
		{`[1,null,2]`, `[1,null,2]`}, {`[null,null]`, `[null,null]`},
		{`{"a":1,"b":null}`, `{"a":1,"b":null}`}, {`{"k":{"n":null}}`, `{"k":{"n":null}}`},
		// numeric strings stay strings (strict string arm)
		{`"1"`, `"1"`}, {`"1.5"`, `"1.5"`}, {`"-0"`, `"-0"`},
		// bool / string / composites / map order
		{`true`, `true`}, {`false`, `false`}, {`""`, `""`}, {`[]`, `[]`}, {`{}`, `{}`},
		{`{"z":1,"a":2,"z":3}`, `{"a":2,"z":3}`},
		{`{"z":1,"a":1.5,"z":null}`, `{"a":1.5,"z":null}`},
		{`[1,1.5,"x",true,null]`, `[1,1.5,"x",true,null]`},
		{`[1.0,2]`, `[1,2]`},
		// the FIXING-parsed number path (alias_number.go PATH B)
		{`[-0,]`, `[0]`}, {`[007,]`, `[7]`}, {`[1.,]`, `[1]`}, {`[.5,]`, `[0.5]`},
		{`[1e,]`, `["1e"]`}, {`[nul,]`, `["nul"]`}, {`[1e400,]`, `["1e400"]`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			out := coerceJsonValueForTest(t, b, tc.in)
			if out != tc.want {
				t.Fatalf("JsonValue(%s) = %s, want %s", tc.in, out, tc.want)
			}
		})
	}
}

// TestJsonValueCoerce_NegativeZeroDeclines is the CGO-free twin of the integration
// negative-zero proof: a STRICT-parsed negative zero declines (the generated union carrier
// decodes `-0` back into its int arm), while the FIXING-parsed `-0` is the integer 0 and
// serves.
func TestJsonValueCoerce_NegativeZeroDeclines(t *testing.T) {
	b := jsonValueAliasBundle(t)
	for _, in := range []string{`-0`, `-0.0`, `[-0]`, `{"z":-0}`, `[1,-0,2]`, `[[-0.0]]`} {
		// Require the DECLINE SENTINEL specifically. A bare err != nil would also accept a
		// claimed parse failure, which is a different (and much worse) outcome: the unary
		// lane only repairs the sentinel through BAML parse-only.
		_, err := ParseStaticBundle(context.Background(), b, in)
		if err == nil {
			t.Errorf("%s: must DECLINE a coerced negative zero", in)
		} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: want the decline sentinel, got a claimed parse failure: %v", in, err)
		}
	}
	if got := coerceJsonValueForTest(t, b, `[-0,]`); got != `[0]` {
		t.Errorf("fixing-parsed -0 = %s, want [0]", got)
	}
}

// TestJsonValueStream_NoDropCadence pins the streaming finding in the CGO-free lane: the
// `JsonValue` partial for a prefix equals its final coercion, with float prefixes kept and
// the typed null emitted as a PRESENT `null` — never `[]` and never a no-emit.
//
// It drives the GATE-FREE parser entry on purpose. The family is STREAM-DECLINED at
// admission (see TestJsonValueFingerprint_AcceptsExactShape), so this proves the parser
// the future gate-flip depends on, not a currently-served path.
func TestJsonValueStream_NoDropCadence(t *testing.T) {
	b := jsonValueAliasBundle(t)
	cases := []struct{ in, want string }{
		{`1`, `1`}, {`1.`, `1`}, {`1.2`, `1.2`}, {`1.2e`, `"1.2e"`}, {`1.2e5`, `120000`},
		{`n`, `"n"`}, {`nu`, `"nu"`}, {`nul`, `"nul"`}, {`null`, `null`},
		{`t`, `"t"`}, {`true`, `true`}, {`-`, `"-"`},
		{`[`, `[]`}, {`[1`, `[1]`}, {`[1.`, `[1]`}, {`[1.2`, `[1.2]`},
		{`[n`, `["n"]`}, {`[nu`, `["nu"]`}, {`[nul`, `["nul"]`}, {`[null`, `[null]`},
		{`{"a":`, `{}`}, {`{"a":1.`, `{"a":1}`}, {`{"a":n`, `{"a":"n"}`},
		{`{"a":null`, `{"a":null}`}, {`[null,null`, `[null,null]`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			out, emit, err := ParseAliasStreamPartial(b, tc.in)
			if err != nil {
				t.Fatalf("ParseAliasStreamPartial(%q): %v", tc.in, err)
			}
			if !emit {
				t.Fatalf("ParseAliasStreamPartial(%q): NO-EMIT, want %s (this family never drops)", tc.in, tc.want)
			}
			if string(out) != tc.want {
				t.Fatalf("ParseAliasStreamPartial(%q) = %s, want %s", tc.in, out, tc.want)
			}
		})
	}
}

// TestJsonValueFloatFormattingIsGoMarshal proves the float arm's public projection is
// EXACTLY Go's encoding/json float64 encoder — the same encoder the generated
// Union6.MarshalJSON applies to BAML's decoded f64 pointer — over the exponent and
// boundary values the scope calls out. A drift here (e.g. keeping the provider's lexeme,
// or a strconv 'g' spelling) changes served bytes.
func TestJsonValueFloatFormattingIsGoMarshal(t *testing.T) {
	b := jsonValueAliasBundle(t)
	vals := []float64{
		1, 3, 1.5, -2.5, 0, 0.1, 1e3, 1.2e5, 1.2e-5, 1e-7, 1e20, 1e21, 1e-306,
		5e-324, 1.7976931348623157e308, 2.2250738585072014e-308, 0.30000000000000004,
		9223372036854775808, 1.2345678901234568e+29,
	}
	for _, f := range vals {
		want, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("json.Marshal(%v): %v", f, err)
		}
		// Feed the value back in through a spelling that cannot hit the int arm, so the
		// float arm is definitely the one under test.
		in := strings.TrimSuffix(string(want), ".0")
		if !strings.ContainsAny(in, ".eE") {
			in += ".0"
		}
		got := coerceJsonValueForTest(t, b, in)
		if got != string(want) {
			t.Fatalf("float %v (input %s): native = %s, want json.Marshal = %s", f, in, got, want)
		}
	}
	// And a negative zero is the documented decline, not a formatting case.
	if !math.Signbit(math.Copysign(0, -1)) {
		t.Fatal("sanity: Copysign(0,-1) must be a negative zero")
	}
}

func coerceJsonValueForTest(t *testing.T, b *schema.Bundle, in string) string {
	t.Helper()
	res, err := ParseStaticBundle(context.Background(), b, in)
	if err != nil {
		t.Fatalf("ParseStaticBundle(%q): %v", in, err)
	}
	return string(res.JSON)
}
