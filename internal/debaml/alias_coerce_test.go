package debaml

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/internal/schema"
)

// jsonAliasBundle builds the exact served five-arm JSON alias bundle (matching the
// generated introspected static descriptor for StaticRecursiveAliasJSON).
func jsonAliasBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	jref := func() *schema.Type {
		return &schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JSON", Mode: schema.NonStreaming}
	}
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JSON", Mode: schema.NonStreaming},
		StructuralRecursiveAliases: []schema.RecursiveAliasDef{{
			Name: "JSON",
			Target: schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool},
				{Kind: schema.TypeList, Elem: jref()},
				{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: jref()},
			}}},
		}},
	}
	if err := b.RebuildIndexes(); err != nil {
		t.Fatalf("RebuildIndexes: %v", err)
	}
	return b
}

// TestAliasCoerce_ByteExact pins the native alias coercion FinalJSON bytes against the
// values captured from stock BAML v0.223 Parse.StaticRecursiveAliasJSON + json.Marshal
// (the live oracle probe). Every row settles a contract: numeric-string stays string,
// null -> [] (the non-nullable list fallback), float -> int (no float arm), map
// sorted-public with IndexMap overwrite, HTML escaping, and the scalar-vs-composite
// pick_best.
func TestAliasCoerce_ByteExact(t *testing.T) {
	b := jsonAliasBundle(t)
	cases := []struct {
		in, want string
	}{
		{`1`, `1`},
		{`"1"`, `"1"`},
		{`true`, `true`},
		{`false`, `false`},
		{`1.5`, `2`},
		{`3.0`, `3`},
		{`null`, `[]`},
		{`[]`, `[]`},
		{`{}`, `{}`},
		{`"1.5"`, `"1.5"`},
		{`[1,"1",true]`, `[1,"1",true]`},
		{`{"z":1,"a":2,"z":3}`, `{"a":2,"z":3}`},
		{`{"z":1,"a":2}`, `{"a":2,"z":1}`},
		{`{"b":2,"a":[1,{"z":3,"y":4}]}`, `{"a":[1,{"y":4,"z":3}],"b":2}`},
		{`[[1],{"k":"v"}]`, `[[1],{"k":"v"}]`},
		{`{"z":1,"a":"bad","z":3}`, `{"a":"bad","z":3}`},
		{`{"a":1,"a":"two"}`, `{"a":"two"}`},
		{`[1.5, 2, "3"]`, `[2,2,"3"]`},
		{`{"n": null}`, `{"n":[]}`},
		{`[null]`, `[[]]`},
		{`{"1":true}`, `{"1":true}`},
		{`[{"a":1}]`, `[{"a":1}]`},
	}
	for _, c := range cases {
		res, err := ParseStaticBundle(context.Background(), b, c.in)
		if err != nil {
			t.Errorf("ParseStaticBundle(%s) error: %v", c.in, err)
			continue
		}
		if string(res.JSON) != c.want {
			t.Errorf("ParseStaticBundle(%s) = %s, want %s", c.in, res.JSON, c.want)
		}
	}
}

// TestAliasCoerce_HTMLEscaping asserts the FinalJSON bytes HTML-escape < > & exactly
// like encoding/json.Marshal (the static callback), including a NESTED string, so the
// escaping matches BAML's Parse + json.Marshal byte-for-byte. The expected bytes are
// computed via json.Marshal of the equivalent Go value rather than hand-written.
func TestAliasCoerce_HTMLEscaping(t *testing.T) {
	b := jsonAliasBundle(t)
	cases := []struct {
		in   string
		want any
	}{
		{`"<tag> & \"x\""`, `<tag> & "x"`},
		{`{"k":"<v> & y"}`, map[string]any{"k": "<v> & y"}},
	}
	for _, c := range cases {
		res, err := ParseStaticBundle(context.Background(), b, c.in)
		if err != nil {
			t.Fatalf("ParseStaticBundle(%s): %v", c.in, err)
		}
		want, _ := json.Marshal(c.want)
		if string(res.JSON) != string(want) {
			t.Errorf("ParseStaticBundle(%s) = %s, want %s", c.in, res.JSON, want)
		}
	}
}

// TestAliasCoerce_OrderedInternalVsSortedPublic asserts the ordered-INTERNAL carrier
// (insertion order, IndexMap last-value-wins-at-first-position) SEPARATELY from the
// SORTED-public bytes — the crux of the map materialization contract. Input
// {"z":1,"a":2,"z":3}: internal entries [z=3, a=2] (z overwritten in place); public
// bytes {"a":2,"z":3}.
func TestAliasCoerce_OrderedInternalVsSortedPublic(t *testing.T) {
	b := jsonAliasBundle(t)
	parsed, ok := extractCandidate(stripJSONComments(`{"z":1,"a":2,"z":3}`))
	if !ok {
		t.Fatal("extractCandidate failed")
	}
	tree, err := coerceAliasTree(b, parsed, &coerceCtx{})
	if err != nil {
		t.Fatalf("coerceAliasTree: %v", err)
	}
	if tree.kind != akMap {
		t.Fatalf("root kind = %v, want map", tree.kind)
	}
	// INTERNAL: insertion order [z, a]; z's value overwritten to 3 at position 0.
	if len(tree.obj.entries) != 2 {
		t.Fatalf("ordered entries = %d, want 2", len(tree.obj.entries))
	}
	if tree.obj.entries[0].key != "z" || tree.obj.entries[0].val.kind != akInt || tree.obj.entries[0].val.i != 3 {
		t.Errorf("entry[0] = %+v, want z=3 (overwrite-in-place)", tree.obj.entries[0])
	}
	if tree.obj.entries[1].key != "a" || tree.obj.entries[1].val.i != 2 {
		t.Errorf("entry[1] = %+v, want a=2", tree.obj.entries[1])
	}
	// PUBLIC: sorted keys.
	pub, err := tree.marshalPublic()
	if err != nil {
		t.Fatalf("marshalPublic: %v", err)
	}
	if string(pub) != `{"a":2,"z":3}` {
		t.Errorf("public bytes = %s, want {\"a\":2,\"z\":3}", pub)
	}
}

// TestAliasCoerce_MapKeyAlwaysStringSucceeds documents+tests that the bare-string map
// key never fails (BAML's key-failure branch is unreachable for this family): every
// object key coerces cleanly, so no entry is ever dropped for a key reason.
func TestAliasCoerce_MapKeyAlwaysStringSucceeds(t *testing.T) {
	b := jsonAliasBundle(t)
	// Non-ASCII / numeric-looking / empty keys all succeed as bare string keys.
	res, err := ParseStaticBundle(context.Background(), b, `{"é":1,"":2,"1":3}`)
	if err != nil {
		t.Fatalf("ParseStaticBundle: %v", err)
	}
	if string(res.JSON) != `{"":2,"1":3,"é":1}` {
		t.Errorf("got %s, want sorted all-keys-kept", res.JSON)
	}
}

// TestAliasFingerprint asserts the exact fingerprint admits the JSON alias and
// declines the wider JsonValue and near-miss shapes (lockstep with nativeserve).
func TestAliasFingerprint(t *testing.T) {
	if !IsProvenRecursiveAliasStaticFamily(jsonAliasBundle(t)) {
		t.Fatal("JSON alias bundle must be admitted")
	}
	// JsonValue: float + null arms, different name -> declined.
	jvref := func() *schema.Type {
		return &schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue", Mode: schema.NonStreaming}
	}
	jv := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue", Mode: schema.NonStreaming},
		StructuralRecursiveAliases: []schema.RecursiveAliasDef{{
			Name: "JsonValue",
			Target: schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Nullable: true, Variants: []schema.Type{
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool},
				{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
				{Kind: schema.TypeList, Elem: jvref()},
				{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: jvref()},
			}}},
		}},
	}
	_ = jv.RebuildIndexes()
	if IsProvenRecursiveAliasStaticFamily(jv) {
		t.Fatal("JsonValue (float+null, non-JSON name) must be DECLINED")
	}
	// Renamed same-shape alias -> declined (name pinned).
	blob := jsonAliasBundle(t)
	blob.StructuralRecursiveAliases[0].Name = "Blob"
	blob.Target.Name = "Blob"
	_ = blob.RebuildIndexes()
	if IsProvenRecursiveAliasStaticFamily(blob) {
		t.Fatal("renamed Blob alias must be DECLINED (canonical name pinned)")
	}
}
