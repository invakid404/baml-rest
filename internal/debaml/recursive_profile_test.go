package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// sd* helpers build schemadescriptor types tersely.
func sdString() schemadescriptor.Type {
	return schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}
}

// sdNullableClass is `T?` — a nullable union with the single non-null class variant,
// exactly how BAML lowers an optional class edge.
func sdNullableClass(name string) schemadescriptor.Type {
	return schemadescriptor.Type{
		Kind: schemadescriptor.TypeUnion,
		Union: &schemadescriptor.UnionType{
			Nullable: true,
			Variants: []schemadescriptor.Type{{Kind: schemadescriptor.TypeClass, Name: name, Mode: schemadescriptor.NonStreaming}},
		},
	}
}

// nodeDescriptor is the self-recursive Node{value string; next Node?}.
func nodeDescriptor() schemadescriptor.Bundle {
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticRecursiveNode",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Node", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "Node"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "value"}, Type: sdString()},
				{Name: schemadescriptor.Name{Name: "next"}, Type: sdNullableClass("Node")},
			},
		}},
		RecursiveClasses: []string{"Node"},
	}
}

// abDescriptor is the mutual A<->B SCC rooted at the given class.
func abDescriptor(root string) schemadescriptor.Bundle {
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticRecursive" + root,
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: root, Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{
			{
				Name: schemadescriptor.Name{Name: "A"}, Mode: schemadescriptor.NonStreaming,
				Fields: []schemadescriptor.ClassField{
					{Name: schemadescriptor.Name{Name: "value"}, Type: sdString()},
					{Name: schemadescriptor.Name{Name: "b"}, Type: sdNullableClass("B")},
				},
			},
			{
				Name: schemadescriptor.Name{Name: "B"}, Mode: schemadescriptor.NonStreaming,
				Fields: []schemadescriptor.ClassField{
					{Name: schemadescriptor.Name{Name: "value"}, Type: sdString()},
					{Name: schemadescriptor.Name{Name: "a"}, Type: sdNullableClass("A")},
				},
			},
		},
		RecursiveClasses: []string{"A", "B"},
	}
}

// loopDescriptor is the one-field implied-recursion shape Loop{next Loop?} — the
// pair-guard regression shape, NOT an admitted served schema.
func loopDescriptor() schemadescriptor.Bundle {
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "Loop",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Loop", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "Loop"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "next"}, Type: sdNullableClass("Loop")},
			},
		}},
		RecursiveClasses: []string{"Loop"},
	}
}

// TestAdmittedRecursiveClassProfile pins the narrow family: Node + mutual A/B (both
// roots) are admitted; Loop (one-field implied recursion) and a required class edge
// are NOT.
func TestAdmittedRecursiveClassProfile(t *testing.T) {
	admit := func(name string, d schemadescriptor.Bundle) {
		t.Run(name+"/admitted", func(t *testing.T) {
			b := lowerOrFatal(t, d)
			if _, ok := admittedRecursiveClassProfile(b); !ok {
				t.Fatalf("%s: expected admitted recursive-class profile", name)
			}
			if err := SupportsNativeFinalBundle(b); err != nil {
				t.Fatalf("%s: SupportsNativeFinalBundle declined an admitted family: %v", name, err)
			}
			// The STREAM lane keeps its blanket cycle/union decline.
			if err := SupportsNativeStreamBundle(b); err == nil {
				t.Fatalf("%s: SupportsNativeStreamBundle must still decline the recursive family", name)
			}
		})
	}
	admit("Node", nodeDescriptor())
	admit("A", abDescriptor("A"))
	admit("B", abDescriptor("B"))

	decline := func(name string, d schemadescriptor.Bundle) {
		t.Run(name+"/declined", func(t *testing.T) {
			b, err := schema.FromStaticDescriptor(d)
			if err != nil {
				return // rejected at lowering is also a valid decline
			}
			if _, ok := admittedRecursiveClassProfile(b); ok {
				t.Fatalf("%s: expected NON-admitted (out-of-family) recursive shape", name)
			}
			if IsProvenRecursiveStaticFamily(b) {
				t.Fatalf("%s: IsProvenRecursiveStaticFamily must be false for an out-of-family shape", name)
			}
			if err := SupportsNativeFinalBundle(b); err == nil {
				t.Fatalf("%s: SupportsNativeFinalBundle must decline an out-of-family recursive shape", name)
			}
		})
	}
	decline("Loop_one_field", loopDescriptor())

	// P0 exactness: a same-SHAPED but non-canonical class must NOT be admitted.
	// Other{payload string; child Other?} has the Node shape but the wrong class +
	// field names.
	other := schemadescriptor.Bundle{
		Version: schemadescriptor.Version, Method: "Other",
		Target: schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Other", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "Other"}, Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "payload"}, Type: sdString()},
				{Name: schemadescriptor.Name{Name: "child"}, Type: sdNullableClass("Other")},
			},
		}},
		RecursiveClasses: []string{"Other"},
	}
	decline("Other_non_canonical_names", other)

	// Node with a class-name @alias must decline.
	nodeAlias := "N"
	nodeAliased := nodeDescriptor()
	nodeAliased.Classes[0].Name.Alias = &nodeAlias
	decline("Node_class_alias", nodeAliased)

	// Node with an EXTRA recursive edge (a third field) must decline.
	nodeExtraEdge := nodeDescriptor()
	nodeExtraEdge.Classes[0].Fields = append(nodeExtraEdge.Classes[0].Fields,
		schemadescriptor.ClassField{Name: schemadescriptor.Name{Name: "prev"}, Type: sdNullableClass("Node")})
	decline("Node_extra_edge", nodeExtraEdge)

	// Node whose value field carries a constraint must decline.
	nodeConstrained := nodeDescriptor()
	nodeConstrained.Classes[0].Fields[0].Type.Meta.Constraints = []schemadescriptor.Constraint{{Level: schemadescriptor.ConstraintCheck, Expression: "true"}}
	decline("Node_constrained_value", nodeConstrained)

	// P0 (round 2): @stream.* annotations are PRESERVED through static lowering onto a
	// final non-streaming descriptor, so the exact profile must reject them at EVERY
	// carrier — otherwise `Node{value string @stream.done; next Node?}` would satisfy
	// the profile and claim a socket. One decline case per carrier.
	stream := schemadescriptor.StreamingBehavior{Done: true}

	nodeClassStream := nodeDescriptor() // class-level @stream (ClassDef.Stream)
	nodeClassStream.Classes[0].Stream = stream
	decline("Node_class_stream", nodeClassStream)

	nodeFieldNeeded := nodeDescriptor() // field-level @stream.needed (ClassField.StreamingNeeded)
	nodeFieldNeeded.Classes[0].Fields[0].StreamingNeeded = true
	decline("Node_field_streaming_needed", nodeFieldNeeded)

	nodeValueStream := nodeDescriptor() // value field TYPE @stream (Type.Meta.Stream)
	nodeValueStream.Classes[0].Fields[0].Type.Meta.Stream = stream
	decline("Node_value_type_stream", nodeValueStream)

	nodeTargetStream := nodeDescriptor() // TARGET type @stream (Type.Meta.Stream)
	nodeTargetStream.Target.Meta.Stream = stream
	decline("Node_target_stream", nodeTargetStream)

	nodeEdgeUnionStream := nodeDescriptor() // the nullable-union edge TYPE @stream
	nodeEdgeUnionStream.Classes[0].Fields[1].Type.Meta.Stream = stream
	decline("Node_edge_union_stream", nodeEdgeUnionStream)

	nodeVariantStream := nodeDescriptor() // the class VARIANT inside the edge @stream
	nodeVariantStream.Classes[0].Fields[1].Type.Union.Variants[0].Meta.Stream = stream
	decline("Node_edge_variant_stream", nodeVariantStream)
}

// TestParseStaticBundle_RecursiveNodeClaim proves the admitted self-recursive family
// now PARSES an explicit-null terminal to byte-exact canonical JSON (schema field
// order, terminal `next` as JSON null). The omitted-terminal byte-exactness is proven
// separately once the absent-optional normalizer lands.
func TestParseStaticBundle_RecursiveNodeClaim(t *testing.T) {
	b := lowerOrFatal(t, nodeDescriptor())
	// depth-2 chain, explicit null terminal.
	raw := "```json\n{\"value\":\"a\",\"next\":{\"value\":\"b\",\"next\":{\"value\":\"c\",\"next\":null}}}\n```"
	res, err := ParseStaticBundle(context.Background(), b, raw)
	if err != nil {
		t.Fatalf("ParseStaticBundle declined the admitted recursive family: %v", err)
	}
	want := `{"value":"a","next":{"value":"b","next":{"value":"c","next":null}}}`
	if string(res.JSON) != want {
		t.Fatalf("recursive JSON mismatch:\n got: %s\nwant: %s", res.JSON, want)
	}
}

// TestParseStaticBundle_RecursiveABClaim proves both mutual roots parse an
// alternating A/B chain to byte-exact canonical JSON.
func TestParseStaticBundle_RecursiveABClaim(t *testing.T) {
	b := lowerOrFatal(t, abDescriptor("A"))
	raw := `{"value":"a0","b":{"value":"b0","a":{"value":"a1","b":null}}}`
	res, err := ParseStaticBundle(context.Background(), b, raw)
	if err != nil {
		t.Fatalf("ParseStaticBundle declined the admitted mutual family: %v", err)
	}
	want := `{"value":"a0","b":{"value":"b0","a":{"value":"a1","b":null}}}`
	if string(res.JSON) != want {
		t.Fatalf("mutual JSON mismatch:\n got: %s\nwant: %s", res.JSON, want)
	}
}

// TestParseStaticBundle_OmittedTerminalNormalized proves the absent-optional
// normalizer: an OMITTED `next` terminal produces the SAME canonical bytes as an
// EXPLICIT `next:null` terminal ({"value":"x","next":null}) — BAML marshals the nil
// *Node pointer as null, so native must too (byte-exact FinalJSON, not just a
// semantically-equal decode).
func TestParseStaticBundle_OmittedTerminalNormalized(t *testing.T) {
	b := lowerOrFatal(t, nodeDescriptor())
	cases := []struct {
		name, raw, want string
	}{
		{"omitted_depth0", `{"value":"x"}`, `{"value":"x","next":null}`},
		{"explicit_null_depth0", `{"value":"x","next":null}`, `{"value":"x","next":null}`},
		{"omitted_terminal_depth2", `{"value":"a","next":{"value":"b","next":{"value":"c"}}}`,
			`{"value":"a","next":{"value":"b","next":{"value":"c","next":null}}}`},
		{"mixed_omitted_and_null", `{"value":"a","next":{"value":"b"}}`,
			`{"value":"a","next":{"value":"b","next":null}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := ParseStaticBundle(context.Background(), b, c.raw)
			if err != nil {
				t.Fatalf("ParseStaticBundle: %v", err)
			}
			if string(res.JSON) != c.want {
				t.Fatalf("normalized JSON mismatch:\n got: %s\nwant: %s", res.JSON, c.want)
			}
		})
	}
}

// TestParseStaticBundle_OmittedTerminalAB proves the normalizer inserts the absent
// mutual-edge null for both A/B roots.
func TestParseStaticBundle_OmittedTerminalAB(t *testing.T) {
	b := lowerOrFatal(t, abDescriptor("A"))
	res, err := ParseStaticBundle(context.Background(), b, `{"value":"a0","b":{"value":"b0"}}`)
	if err != nil {
		t.Fatalf("ParseStaticBundle: %v", err)
	}
	want := `{"value":"a0","b":{"value":"b0","a":null}}`
	if string(res.JSON) != want {
		t.Fatalf("mutual omitted-terminal mismatch:\n got: %s\nwant: %s", res.JSON, want)
	}
}

// TestParseStaticBundle_LoopDeclines proves the one-field implied-recursion shape
// declines pre-parse (BAML fallback), never a native claim.
func TestParseStaticBundle_LoopDeclines(t *testing.T) {
	b, err := schema.FromStaticDescriptor(loopDescriptor())
	if err != nil {
		return
	}
	_, perr := ParseStaticBundle(context.Background(), b, `{"next":null}`)
	if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("Loop one-field implied recursion must decline, got %v", perr)
	}
}
