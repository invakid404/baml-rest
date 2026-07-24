package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// staticAnswerDescriptor is the flat StaticAnswer{answer:string, confidence:int}
// return Bundle the 8C admitted class shape uses (the static_oracle
// StaticOutputFormat / StaticCompletionOutputFormat return).
func staticAnswerDescriptor() schemadescriptor.Bundle {
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticOutputFormat",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "StaticAnswer", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "StaticAnswer"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "answer"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "confidence"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
			},
		}},
	}
}

func lowerOrFatal(t *testing.T, d schemadescriptor.Bundle) *schema.Bundle {
	t.Helper()
	b, err := schema.FromStaticDescriptor(d)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	return b
}

// TestParseStaticBundle_ClassClaim proves the flat-class admitted shape parses to
// the SAME flattened canonical JSON the dynamic SAP produces (class-field order,
// int as bare number, string escaped), CLAIMING the response.
func TestParseStaticBundle_ClassClaim(t *testing.T) {
	bundle := lowerOrFatal(t, staticAnswerDescriptor())
	// Extra whitespace + a markdown fence + trailing prose the extractor must
	// discard, proving the same extract→fix→coerce pipeline as the dynamic final.
	raw := "Here you go:\n```json\n{ \"answer\": \"it is 21°C\", \"confidence\": 7 }\n```\nhope that helps"
	res, err := ParseStaticBundle(context.Background(), bundle, raw)
	if err != nil {
		t.Fatalf("ParseStaticBundle claimed shape errored: %v", err)
	}
	got := string(res.JSON)
	want := `{"answer":"it is 21°C","confidence":7}`
	if got != want {
		t.Fatalf("flattened JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestParseStaticBundle_NoCandidateDeclines proves a response with no
// cleanly-claimable JSON candidate DECLINES (ErrDeBAMLParseUnsupported → BAML
// serves), never a claimed parse error.
func TestParseStaticBundle_NoCandidateDeclines(t *testing.T) {
	bundle := lowerOrFatal(t, staticAnswerDescriptor())
	_, err := ParseStaticBundle(context.Background(), bundle, "just some prose, no json here")
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("want ErrDeBAMLParseUnsupported decline, got %v", err)
	}
}

// TestParseStaticBundle_NilBundleDeclines proves a nil bundle declines rather than
// panicking.
func TestParseStaticBundle_NilBundleDeclines(t *testing.T) {
	_, err := ParseStaticBundle(context.Background(), nil, `{"answer":"x","confidence":1}`)
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("want ErrDeBAMLParseUnsupported for nil bundle, got %v", err)
	}
}

// TestParseStaticBundle_RecursiveDeclines proves a REQUIRED class edge
// (Node{value; next Node} — NOT the admitted nullable `next Node?`) stays a #583
// residual decline even after Phase 2: admittedRecursiveClassProfile rejects the
// non-optional edge, so ParseStaticBundle falls to the blanket recursive-class
// decline. (The admitted nullable family is covered by
// recursive_profile_test.go's TestParseStaticBundle_RecursiveNodeClaim.)
func TestParseStaticBundle_RecursiveDeclines(t *testing.T) {
	d := schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "Recursive",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Node", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "Node"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "value"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "next"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Node", Mode: schemadescriptor.NonStreaming}},
			},
		}},
		RecursiveClasses: []string{"Node"},
	}
	bundle, err := schema.FromStaticDescriptor(d)
	if err != nil {
		// Some recursive descriptors are rejected at lowering; that is also a valid
		// (pre-parse) decline. Only a successful lower must decline at checkSupported.
		return
	}
	_, perr := ParseStaticBundle(context.Background(), bundle, `{"value":"a","next":null}`)
	if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("recursive class must decline, got %v", perr)
	}
}
