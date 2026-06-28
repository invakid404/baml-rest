package generated

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/adapter"
	types "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client/types"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("count", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

func renderSimple(t *testing.T) string {
	t.Helper()
	bundle, err := schema.FromDynamicOutputSchema(simpleSchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("FromDynamicOutputSchema: %v", err)
	}
	block, err := outputformat.Render(bundle, outputformat.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return block
}

func newTestAdapter(enabled bool, s *bamlutils.DynamicOutputSchema) bamlutils.Adapter {
	a := &adapter.BamlAdapter{}
	a.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: enabled})
	a.SetDeBAMLOutputSchema(s)
	return a
}

// assertMarkerIntact fails if the first message's first part is no longer
// an untouched output_format marker.
func assertMarkerIntact(t *testing.T, msgs []types.Baml_Rest_Message, ctx string) {
	t.Helper()
	if p := (*msgs[0].Parts)[0]; p.Text != nil || p.Output_format == nil || !*p.Output_format {
		t.Errorf("%s: marker mutated: text=%v of=%v", ctx, p.Text, p.Output_format)
	}
}

// TestRewriteOutputFormat pins the pure rewrite: it returns a COPY where
// output_format parts become plain text parts carrying the block and
// literal {output_format} tokens in string content are replaced, while
// the INPUT slice (and its parts/content) are never mutated — the
// BuildRequest-only-copy contract that keeps legacy children on the
// original (#537).
func TestRewriteOutputFormat(t *testing.T) {
	const block = "<<BLOCK>>"
	origParts := &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}
	origContent := "before {output_format} after {output_format} end"
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: origParts},
		{Role: "user", Content: sp(origContent)},
		{Role: "user", Parts: &[]types.Baml_Rest_ContentPart{
			{Text: sp("keep me")},
			{Output_format: bp(true)},
		}},
		// A part whose output_format is explicitly false must be left alone.
		{Role: "user", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(false)}}},
	}

	out := rewriteOutputFormat(msgs, block)

	// --- input must be completely untouched ---
	if msgs[0].Parts != origParts || (*origParts)[0].Output_format == nil || !*(*origParts)[0].Output_format || (*origParts)[0].Text != nil {
		t.Errorf("input msg0 was mutated: %#v", (*msgs[0].Parts)[0])
	}
	if msgs[1].Content == nil || *msgs[1].Content != origContent {
		t.Errorf("input msg1 content was mutated: %v", msgs[1].Content)
	}

	// --- copy must be rewritten ---
	if p := (*out[0].Parts)[0]; p.Output_format != nil || p.Text == nil || *p.Text != block {
		t.Errorf("out msg0: part not rewritten: of=%v text=%v", p.Output_format, p.Text)
	}
	if out[1].Content == nil || *out[1].Content != "before "+block+" after "+block+" end" {
		t.Errorf("out msg1: placeholders not replaced: %v", out[1].Content)
	}
	if got := (*out[2].Parts)[0]; got.Text == nil || *got.Text != "keep me" {
		t.Errorf("out msg2 part0 changed: %v", got.Text)
	}
	if got := (*out[2].Parts)[1]; got.Output_format != nil || got.Text == nil || *got.Text != block {
		t.Errorf("out msg2 part1 not rewritten: of=%v text=%v", got.Output_format, got.Text)
	}
	if got := (*out[3].Parts)[0]; got.Text != nil || got.Output_format == nil || *got.Output_format {
		t.Errorf("out msg3 part0 should be untouched (of=false is not a marker): %v / %v", got.Text, got.Output_format)
	}
}

func TestMaybeApplyDeBAMLOutputFormat_FlagOff(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(false, simpleSchema()), msgs)
	assertMarkerIntact(t, out, "flag off (returned)")
	assertMarkerIntact(t, msgs, "flag off (input)")
}

func TestMaybeApplyDeBAMLOutputFormat_NilSchema(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, nil), msgs)
	assertMarkerIntact(t, out, "nil schema (returned)")
	assertMarkerIntact(t, msgs, "nil schema (input)")
}

func TestMaybeApplyDeBAMLOutputFormat_Enabled(t *testing.T) {
	want := renderSimple(t)
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
		{Role: "user", Content: sp("see {output_format} here")},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema()), msgs)

	if p := (*out[0].Parts)[0]; p.Output_format != nil || p.Text == nil || *p.Text != want {
		t.Errorf("enabled: part not rewritten to rendered block:\n got=%v\nwant=%q", p.Text, want)
	}
	if out[1].Content == nil || *out[1].Content != "see "+want+" here" {
		t.Errorf("enabled: string placeholder not substituted: %v", out[1].Content)
	}
	// The input (shared with legacy children) must remain untouched.
	assertMarkerIntact(t, msgs, "enabled (input)")
	if msgs[1].Content == nil || *msgs[1].Content != "see {output_format} here" {
		t.Errorf("enabled: input content mutated: %v", msgs[1].Content)
	}
}

// TestMaybeApplyDeBAMLOutputFormat_RenderErrorFallback covers the
// render-first / rewrite-after contract: a schema that fails native
// lowering (unresolved reference) returns the messages unchanged so BAML
// renders ctx.output_format as today.
func TestMaybeApplyDeBAMLOutputFormat_RenderErrorFallback(t *testing.T) {
	bad := &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("x", &bamlutils.DynamicProperty{Ref: "DoesNotExist"}),
		),
	}
	// Sanity: this really is a lowering error.
	if _, err := schema.FromDynamicOutputSchema(bad, schema.BuildOptions{}); err == nil {
		t.Fatal("expected FromDynamicOutputSchema to fail on unresolved ref")
	}

	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, bad), msgs)
	assertMarkerIntact(t, out, "render error (returned)")
	assertMarkerIntact(t, msgs, "render error (input)")
}
