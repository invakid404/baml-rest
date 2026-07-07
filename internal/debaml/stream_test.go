package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// M4b streaming (raw_is_done=false) native-parse coverage. Each claimed case here
// mirrors a LIVE-CAPTURED success prefix from the bamlfuzz parse-recovery corpus
// (adapters/common/codegen/testdata/bamlfuzz/parse_recovery/20..23), so the
// asserted `want` is BAML v0.223.0 parse-stream's own output — the same value the
// Docker differential holds native to byte-exact. The declined cases pin the
// over-claim guards: native FALLS BACK (ErrDeBAMLParseUnsupported) so BAML
// parse-stream stays authoritative. These local tests prove value parity without
// BAML/Docker; the integration differential is the final byte-exact gate.

// parseStreamRaw drives the public callback for a streaming request.
func parseStreamRaw(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) (string, error) {
	t.Helper()
	res, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s, Stream: true})
	if err != nil {
		return "", err
	}
	return string(res.JSON), nil
}

// mustStream asserts a CLAIMED stream parse whose output semantically equals want.
func mustStream(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := parseStreamRaw(t, s, raw)
	if err != nil {
		t.Fatalf("stream Parse(%q): unexpected error: %v", raw, err)
	}
	if !jsonValueEqual(t, got, want) {
		t.Errorf("stream Parse(%q):\n got %s\nwant %s", raw, got, want)
	}
}

// requireStreamUnsupported asserts native FELL BACK (sentinel), not claimed.
func requireStreamUnsupported(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) {
	t.Helper()
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s, Stream: true})
	if err == nil {
		t.Fatalf("stream Parse(%q): expected ErrDeBAMLParseUnsupported, got success", raw)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("stream Parse(%q): expected ErrDeBAMLParseUnsupported, got %v", raw, err)
	}
}

// nameOnlySchema is Root{name:string} — the truncated_string corpus schema.
func nameOnlySchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: props(kv("name", strProp()))}
}

// nameTagsSchema is Root{name:string, tags:list<string>} — the trailing_comma
// corpus schema.
func nameTagsSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
}

// --- 20_streaming_growing_object (Root{name:string, age:int}) ---

func TestStream_GrowingObject(t *testing.T) {
	s := personSchema()
	// No field value available yet → FALLBACK (open root object before ≥1 field).
	requireStreamUnsupported(t, s, `{`)
	requireStreamUnsupported(t, s, `{"name"`)
	requireStreamUnsupported(t, s, `{"name":`)
	// ≥1 field value available → CLAIMED, missing fields null-filled.
	mustStream(t, s, `{"name":"Ad`, `{"name":"Ad","age":null}`)
	mustStream(t, s, `{"name":"Ada"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}

// --- 21_streaming_markdown_fence (Root{name:string, age:int}) ---

func TestStream_MarkdownFence(t *testing.T) {
	s := personSchema()
	// Bare prose / just-opened fence: no JSON content → FALLBACK.
	requireStreamUnsupported(t, s, "Here you go:\n")
	requireStreamUnsupported(t, s, "Here you go:\n```json\n")
	// Object emerging inside the (still-open) fence → CLAIMED.
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\"", `{"name":"Ada","age":null}`)
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\",\"age\":36}", `{"name":"Ada","age":36}`)
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\",\"age\":36}\n```", `{"name":"Ada","age":36}`)
}

// --- 22_streaming_truncated_string (Root{name:string}) ---

func TestStream_TruncatedString(t *testing.T) {
	s := nameOnlySchema()
	// An opened-but-empty string parses as "" (present, incomplete, kept — strings
	// are not done-required).
	mustStream(t, s, `{"name":"`, `{"name":""}`)
	mustStream(t, s, `{"name":"Ad`, `{"name":"Ad"}`)
	mustStream(t, s, `{"name":"Ada"`, `{"name":"Ada"}`)
	mustStream(t, s, `{"name":"Ada"}`, `{"name":"Ada"}`)
}

// --- 23_streaming_trailing_comma (Root{name:string, tags:list<string>}) ---

func TestStream_TrailingComma(t *testing.T) {
	s := nameTagsSchema()
	mustStream(t, s, `{"name":"Ada","tags":["x"`, `{"name":"Ada","tags":["x"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",`, `{"name":"Ada","tags":["x","y"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",]`, `{"name":"Ada","tags":["x","y"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",]}`, `{"name":"Ada","tags":["x","y"]}`)
}

// --- Over-claim guards ---

// TestStream_IncompleteDoneRequiredScalarDeclines pins that an INCOMPLETE
// done-required scalar (a partial int that never terminates) DECLINES: BAML would
// DELETE it (semantic streaming), which M4b does not model, so native falls back.
func TestStream_IncompleteDoneRequiredScalarDeclines(t *testing.T) {
	s := personSchema()
	// age=3 runs to EOF inside an unterminated object → Incomplete int → deleted by
	// BAML → native declines rather than reproduce the deletion.
	requireStreamUnsupported(t, s, `{"name":"Ada","age":3`)
	// A trailing comma after an UNQUOTED object value at EOF (`36,`) hits the same
	// greedy-unquoted-object-value guard the final fixer uses (BAML would consume the
	// comma and read greedily), so native over-declines it — safe, and not a corpus
	// prefix (the trailing_comma fixture uses quoted list elements).
	requireStreamUnsupported(t, s, `{"name":"Ada","age":36,`)
}

// TestStream_NoStructureDeclines pins that raw text with no recoverable JSON
// structure falls back (BAML's allow_as_string→class path is not claimed by M4b).
func TestStream_NoStructureDeclines(t *testing.T) {
	requireStreamUnsupported(t, personSchema(), "just some prose, no json")
	requireStreamUnsupported(t, personSchema(), "")
}

// TestStream_ScalarToClassDeclines pins that a scalar/array into a class declines
// in stream mode (implied-key / inferred-object / array-to-singular deferred).
func TestStream_ScalarToClassDeclines(t *testing.T) {
	requireStreamUnsupported(t, personSchema(), `"hello"`)
	requireStreamUnsupported(t, personSchema(), `[1,2,3]`)
}

// TestStream_NilSchemaDeclines pins the nil-schema fallback.
func TestStream_NilSchemaDeclines(t *testing.T) {
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: `{}`, OutputSchema: nil, Stream: true})
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil schema stream: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestStream_FinalParseUnchanged is a belt-and-suspenders check that the M4b
// stream path did not alter final (non-stream) parse behavior for the same input.
func TestStream_FinalParseUnchanged(t *testing.T) {
	// Final parse of a truncated object still DECLINES (M2a defers unterminated
	// structures); only the STREAM path recovers it.
	requireUnsupported(t, personSchema(), `{"name":"Ada"`)
	// Final parse of a complete object is unchanged.
	mustParse(t, personSchema(), `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}
