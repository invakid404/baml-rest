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

// TestApplyOutputFormatBlock pins the pure rewrite: output_format parts
// become plain text parts carrying the block, literal {output_format}
// tokens in string content are replaced, and every other part / message
// is left untouched.
func TestApplyOutputFormatBlock(t *testing.T) {
	const block = "<<BLOCK>>"
	imgURL := "http://x/y.png"
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
		{Role: "user", Content: sp("before {output_format} after {output_format} end")},
		{Role: "user", Parts: &[]types.Baml_Rest_ContentPart{
			{Text: sp("keep me")},
			{Output_format: bp(true)},
		}},
		// A part whose output_format is explicitly false must be left alone.
		{Role: "user", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(false)}}},
	}
	_ = imgURL

	applyOutputFormatBlock(msgs, block)

	// msg0: output_format part rewritten to text.
	p0 := (*msgs[0].Parts)[0]
	if p0.Output_format != nil {
		t.Errorf("msg0: Output_format not cleared: %v", *p0.Output_format)
	}
	if p0.Text == nil || *p0.Text != block {
		t.Errorf("msg0: Text = %v, want %q", p0.Text, block)
	}

	// msg1: both placeholders replaced.
	if msgs[1].Content == nil || *msgs[1].Content != "before "+block+" after "+block+" end" {
		t.Errorf("msg1: Content = %v", msgs[1].Content)
	}

	// msg2: text part untouched, output_format part rewritten.
	if got := (*msgs[2].Parts)[0]; got.Text == nil || *got.Text != "keep me" {
		t.Errorf("msg2 part0 mutated: %v", got.Text)
	}
	if got := (*msgs[2].Parts)[1]; got.Output_format != nil || got.Text == nil || *got.Text != block {
		t.Errorf("msg2 part1 not rewritten: of=%v text=%v", got.Output_format, got.Text)
	}

	// msg3: output_format=false is not a marker — untouched.
	if got := (*msgs[3].Parts)[0]; got.Text != nil || got.Output_format == nil || *got.Output_format {
		t.Errorf("msg3 part0 should be untouched: %v / %v", got.Text, got.Output_format)
	}
}

func TestMaybeApplyDeBAMLOutputFormat_FlagOff(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	maybeApplyDeBAMLOutputFormat(newTestAdapter(false, simpleSchema()), msgs)
	if p := (*msgs[0].Parts)[0]; p.Text != nil || p.Output_format == nil || !*p.Output_format {
		t.Errorf("flag off must leave the marker untouched: text=%v of=%v", p.Text, p.Output_format)
	}
}

func TestMaybeApplyDeBAMLOutputFormat_NilSchema(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	maybeApplyDeBAMLOutputFormat(newTestAdapter(true, nil), msgs)
	if p := (*msgs[0].Parts)[0]; p.Text != nil || p.Output_format == nil || !*p.Output_format {
		t.Errorf("nil schema must leave the marker untouched: text=%v of=%v", p.Text, p.Output_format)
	}
}

func TestMaybeApplyDeBAMLOutputFormat_Enabled(t *testing.T) {
	want := renderSimple(t)
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
		{Role: "user", Content: sp("see {output_format} here")},
	}
	maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema()), msgs)

	if p := (*msgs[0].Parts)[0]; p.Output_format != nil || p.Text == nil || *p.Text != want {
		t.Errorf("enabled: part not rewritten to rendered block:\n got=%v\nwant=%q", p.Text, want)
	}
	if msgs[1].Content == nil || *msgs[1].Content != "see "+want+" here" {
		t.Errorf("enabled: string placeholder not substituted: %v", msgs[1].Content)
	}
}

// TestMaybeApplyDeBAMLOutputFormat_RenderErrorFallback covers the
// render-first / rewrite-after contract: a schema that fails native
// lowering (unresolved reference) leaves the messages untouched so BAML
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
	maybeApplyDeBAMLOutputFormat(newTestAdapter(true, bad), msgs)
	if p := (*msgs[0].Parts)[0]; p.Text != nil || p.Output_format == nil || !*p.Output_format {
		t.Errorf("render error must fall back (marker untouched): text=%v of=%v", p.Text, p.Output_format)
	}
}
