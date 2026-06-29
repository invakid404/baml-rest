package generated

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/adapter"
	types "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client/types"
)

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

// fakeBlock is the stand-in for the native render output. The real
// renderer lives in the root module (internal/schema + outputformat) and
// is byte-pinned to BAML elsewhere; these tests inject it as a callback so
// they exercise the rewrite/gating/fallback logic without the cross-module
// internal dependency.
const fakeBlock = "<<NATIVE-BLOCK>>"

func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

// newTestAdapter builds a real framework adapter with the de-BAML config,
// carried schema, and (optionally) the native render callback installed.
func newTestAdapter(enabled bool, s *bamlutils.DynamicOutputSchema, render bamlutils.DeBAMLRenderFunc) bamlutils.Adapter {
	a := &adapter.BamlAdapter{}
	a.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: enabled})
	a.SetDeBAMLOutputSchema(s)
	a.SetDeBAMLRenderer(render)
	return a
}

func okRenderer(block string) bamlutils.DeBAMLRenderFunc {
	return func(*bamlutils.DynamicOutputSchema) (string, error) { return block, nil }
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
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(false, simpleSchema(), okRenderer(fakeBlock)), msgs)
	assertMarkerIntact(t, out, "flag off (returned)")
	assertMarkerIntact(t, msgs, "flag off (input)")
}

func TestMaybeApplyDeBAMLOutputFormat_NilSchema(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, nil, okRenderer(fakeBlock)), msgs)
	assertMarkerIntact(t, out, "nil schema (returned)")
	assertMarkerIntact(t, msgs, "nil schema (input)")
}

// TestMaybeApplyDeBAMLOutputFormat_NilRenderer pins the F1 decoupling
// fallback: enabled + schema present but no render callback wired (the
// dynclient module has no internal renderer of its own) → BAML-as-today.
func TestMaybeApplyDeBAMLOutputFormat_NilRenderer(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema(), nil), msgs)
	assertMarkerIntact(t, out, "nil renderer (returned)")
	assertMarkerIntact(t, msgs, "nil renderer (input)")
}

func TestMaybeApplyDeBAMLOutputFormat_Enabled(t *testing.T) {
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
		{Role: "user", Content: sp("see {output_format} here")},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema(), okRenderer(fakeBlock)), msgs)

	if p := (*out[0].Parts)[0]; p.Output_format != nil || p.Text == nil || *p.Text != fakeBlock {
		t.Errorf("enabled: part not rewritten to rendered block:\n got=%v\nwant=%q", p.Text, fakeBlock)
	}
	if out[1].Content == nil || *out[1].Content != "see "+fakeBlock+" here" {
		t.Errorf("enabled: string placeholder not substituted: %v", out[1].Content)
	}
	// The input (shared with legacy children) must remain untouched.
	assertMarkerIntact(t, msgs, "enabled (input)")
	if msgs[1].Content == nil || *msgs[1].Content != "see {output_format} here" {
		t.Errorf("enabled: input content mutated: %v", msgs[1].Content)
	}
}

// TestMaybeApplyDeBAMLOutputFormat_LegacyChildSeesOriginal is the F-V1
// regression: the value returned for the BuildRequest closures must be a
// SEPARATE rewritten slice, while the input slice — the one the generated
// dispatcher keeps passing to the legacy fallback children
// (legacyStreamChildFn / legacyCallChildFn) — must still carry the
// ORIGINAL output_format marker, so a mixed-mode fallback to a legacy
// child stays BAML-as-today even with de-BAML on (#537). The integration
// harness has no mockable unsupported provider to force a real legacy
// child, so this pins the contract at the seam where the bug lived.
func TestMaybeApplyDeBAMLOutputFormat_LegacyChildSeesOriginal(t *testing.T) {
	legacyMsgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	brMsgs := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema(), okRenderer(fakeBlock)), legacyMsgs)

	// BuildRequest copy is rewritten...
	if p := (*brMsgs[0].Parts)[0]; p.Output_format != nil || p.Text == nil || *p.Text != fakeBlock {
		t.Errorf("BuildRequest slice not rewritten: of=%v text=%v", p.Output_format, p.Text)
	}
	// ...but the legacy slice still has the original marker (BAML-as-today).
	assertMarkerIntact(t, legacyMsgs, "legacy child")
	// They must be distinct backing slices (the rewrite copied, not aliased).
	if &brMsgs[0] == &legacyMsgs[0] {
		t.Error("BuildRequest and legacy slices must not share backing storage")
	}
}

// TestMaybeApplyDeBAMLOutputFormat_RenderErrorFallback covers the
// render-first / rewrite-after contract: a render callback that returns an
// error returns the messages unchanged so BAML renders ctx.output_format
// as today.
func TestMaybeApplyDeBAMLOutputFormat_RenderErrorFallback(t *testing.T) {
	failing := func(*bamlutils.DynamicOutputSchema) (string, error) {
		return "", errors.New("boom")
	}
	msgs := []types.Baml_Rest_Message{
		{Role: "system", Parts: &[]types.Baml_Rest_ContentPart{{Output_format: bp(true)}}},
	}
	out := maybeApplyDeBAMLOutputFormat(newTestAdapter(true, simpleSchema(), failing), msgs)
	assertMarkerIntact(t, out, "render error (returned)")
	assertMarkerIntact(t, msgs, "render error (input)")
}

// newParserTestAdapter builds a real framework adapter with the de-BAML
// config, carried schema, and (optionally) the native parse callback
// installed — the parser-side twin of newTestAdapter.
func newParserTestAdapter(enabled bool, s *bamlutils.DynamicOutputSchema, parse bamlutils.DeBAMLParseFunc) bamlutils.Adapter {
	a := &adapter.BamlAdapter{}
	a.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: enabled})
	a.SetDeBAMLOutputSchema(s)
	a.SetDeBAMLParser(parse)
	return a
}

// parserReturning is an injected native parse callback that returns a fixed
// JSON payload and error, so the wrapper's claim/decline/propagate logic can
// be exercised without the root-module parser.
func parserReturning(json string, err error) bamlutils.DeBAMLParseFunc {
	return func(context.Context, bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
		return bamlutils.DeBAMLParseResult{JSON: []byte(json)}, err
	}
}

// TestMaybeParseDeBAMLFinal_WrapFailurePropagates is the R1 regression: when
// the native parser CLAIMS success (nil error) but returns JSON that cannot
// wrap into the dynamic-output envelope (scalar / non-object / malformed),
// the wrapper must PROPAGATE the failure as a native error rather than
// silently fall back to BAML. Per the seam contract only
// ErrDeBAMLParseUnsupported falls back; any other failure on a claimed
// result surfaces, so a parser/callback bug can't hide behind a BAML parse.
func TestMaybeParseDeBAMLFinal_WrapFailurePropagates(t *testing.T) {
	for _, badJSON := range []string{`123`, `"scalar"`, `[1,2,3]`, `{not json`} {
		_, ok, err := maybeParseDeBAMLFinal(
			newParserTestAdapter(true, simpleSchema(), parserReturning(badJSON, nil)),
			"raw", "final",
		)
		if ok {
			t.Errorf("%q: expected ok=false on wrap failure, got ok=true", badJSON)
		}
		if err == nil {
			t.Errorf("%q: expected wrap failure to PROPAGATE, got nil (silent BAML fallback)", badJSON)
		}
		if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%q: wrap failure must NOT be the unsupported sentinel (would fall back): %v", badJSON, err)
		}
	}
}

// TestMaybeParseDeBAMLFinal_ClaimsValidObject pins the success path: a native
// parser returning a flattened JSON object is claimed and wrapped into the
// dynamic-output envelope.
func TestMaybeParseDeBAMLFinal_ClaimsValidObject(t *testing.T) {
	out, ok, err := maybeParseDeBAMLFinal(
		newParserTestAdapter(true, simpleSchema(), parserReturning(`{"answer":"hi"}`, nil)),
		"raw", "final",
	)
	if err != nil || !ok {
		t.Fatalf("expected claimed success, got ok=%v err=%v", ok, err)
	}
	if v, present := out.DynamicProperties.Get("answer"); !present || v != "hi" {
		t.Errorf("envelope DynamicProperties.answer = %v (present=%v), want %q", v, present, "hi")
	}
}

// TestMaybeParseDeBAMLFinal_UnsupportedFallsBack pins that the sentinel — and
// only the sentinel — declines (ok=false, err=nil) so the caller falls back
// to BAML.
func TestMaybeParseDeBAMLFinal_UnsupportedFallsBack(t *testing.T) {
	_, ok, err := maybeParseDeBAMLFinal(
		newParserTestAdapter(true, simpleSchema(), parserReturning("", bamlutils.ErrDeBAMLParseUnsupported)),
		"raw", "final",
	)
	if ok || err != nil {
		t.Fatalf("unsupported must decline (ok=false, err=nil), got ok=%v err=%v", ok, err)
	}
}

// TestMaybeParseDeBAMLFinal_DeclinesWhenOff pins the runtime gates: flag off,
// nil schema, and nil parser all decline without invoking the callback.
func TestMaybeParseDeBAMLFinal_DeclinesWhenOff(t *testing.T) {
	mustDecline := func(name string, a bamlutils.Adapter) {
		t.Helper()
		if _, ok, err := maybeParseDeBAMLFinal(a, "raw", "final"); ok || err != nil {
			t.Errorf("%s: expected decline (ok=false, err=nil), got ok=%v err=%v", name, ok, err)
		}
	}
	claim := parserReturning(`{"answer":"hi"}`, nil)
	mustDecline("flag off", newParserTestAdapter(false, simpleSchema(), claim))
	mustDecline("nil schema", newParserTestAdapter(true, nil, claim))
	mustDecline("nil parser", newParserTestAdapter(true, simpleSchema(), nil))
}
