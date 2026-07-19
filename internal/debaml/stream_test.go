package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// M4b/M4c streaming (raw_is_done=false) native-parse coverage. Each claimed case
// here mirrors a LIVE-CAPTURED success prefix from the bamlfuzz parse-recovery
// corpus (adapters/common/codegen/testdata/bamlfuzz/parse_recovery/20..23 for M4b,
// 173..177 for M4c), so the
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
	// Phase 7C eligible-prefix gap closure: an open root object with NO field value
	// yet (`{`, a dangling key, a dangling colon) is now CLAIMED as the all-filler
	// object, byte-exact with BAML parse-stream's LIVE-CAPTURED want (corpus 20
	// open_brace / first_key / colon → {"name":null,"age":null}). No BAML fallback.
	mustStream(t, s, `{`, `{"name":null,"age":null}`)
	mustStream(t, s, `{"name"`, `{"name":null,"age":null}`)
	mustStream(t, s, `{"name":`, `{"name":null,"age":null}`)
	// ≥1 field value available → CLAIMED, missing fields null-filled.
	mustStream(t, s, `{"name":"Ad`, `{"name":"Ad","age":null}`)
	mustStream(t, s, `{"name":"Ada"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}

// --- 21_streaming_markdown_fence (Root{name:string, age:int}) ---

func TestStream_MarkdownFence(t *testing.T) {
	s := personSchema()
	// Phase 7C: bare prose / just-opened fence (no `{`) for a >=2-field class now
	// CLAIM the all-filler object byte-exact with BAML (LIVE-CAPTURED corpus 21).
	mustStream(t, s, "Here you go:\n", `{"name":null,"age":null}`)
	mustStream(t, s, "Here you go:\n```json\n", `{"name":null,"age":null}`)
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

// TestStream_IncompleteDoneRequiredScalarNulls pins the M4c semantic-streaming
// behavior for an INCOMPLETE done-required class field: an int that never
// terminates is DELETED by BAML (must_be_done && state != Complete →
// Err(IncompleteDoneValue)) and, as a CLASS field, REPLACED WITH NULL (deletion_nulls
// in semantic_streaming.rs process_node). Native now CLAIMS the null-replacement
// rather than falling back. (Captured live: see corpus 174_streaming_class_incomplete_scalar_field.)
func TestStream_IncompleteDoneRequiredScalarNulls(t *testing.T) {
	s := personSchema()
	// age=3 runs to EOF inside an unterminated object → Incomplete int → deleted →
	// the class field is null-replaced; the completed string sibling is kept.
	mustStream(t, s, `{"name":"Ada","age":3`, `{"name":"Ada","age":null}`)
	// A trailing comma after an unquoted object value AT EOF (`36,`) cleanly
	// comma-terminates the value — nothing follows to greedy-read — so Phase 7C
	// now CLAIMS it as COMPLETE (age=36), matching BAML parse-stream byte-exact
	// (LIVE-CAPTURED). This closes the tight-comma-at-EOF transient.
	mustStream(t, s, `{"name":"Ada","age":36,`, `{"name":"Ada","age":36}`)
	// A comma FOLLOWED BY tight content (`36,"x"`) is BAML's GREEDY read: the
	// unquoted value is consumed past the comma in a cascade native's per-field
	// parse cannot reproduce (#546), so native DECLINES. Admission declines any
	// schema with a NON-LAST unquoted-scalar field so no admitted stream reaches
	// this prefix (here `age` IS last, so this raw is only an over-decline guard —
	// not an admitted prefix).
	requireStreamUnsupported(t, s, `{"name":"Ada","age":36,"extra"`)
}

// TestStream_NumbersListDropsIncompleteTail pins the NUMBERS behavior: a partial
// integer-list prefix KEEPS completed elements and DROPS the incomplete
// done-required trailing element (semantic_streaming.rs List arm filter_map). int is
// done-required, list is not. (Captured live: see corpus 173_streaming_numbers_list_int.)
func TestStream_NumbersListDropsIncompleteTail(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("nums", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
	}
	// `[1,2` → 1 is comma-terminated (Complete, kept); 2 runs to EOF (Incomplete,
	// dropped). `[1` → the lone 1 is the incomplete trailing element → dropped → [].
	mustStream(t, s, `{"nums":[`, `{"nums":[]}`)
	mustStream(t, s, `{"nums":[1`, `{"nums":[]}`)
	mustStream(t, s, `{"nums":[1,`, `{"nums":[1]}`)
	mustStream(t, s, `{"nums":[1,2`, `{"nums":[1]}`)
	mustStream(t, s, `{"nums":[1,2,3`, `{"nums":[1,2]}`)
	mustStream(t, s, `{"nums":[1,2,3]`, `{"nums":[1,2,3]}`)
	mustStream(t, s, `{"nums":[1,2,3]}`, `{"nums":[1,2,3]}`)
}

// TestStream_ListOfClassPartialFieldNulls pins that a list of NON-done-required
// class elements KEEPS a partial trailing element (a class is not done-required)
// while NULL-REPLACING that element's incomplete done-required field. Both list
// elements survive; only the incomplete int becomes null. (Captured live: see corpus
// 175_streaming_list_class_partial_field.)
func TestStream_ListOfClassPartialFieldNulls(t *testing.T) {
	inner := bamlutils.MustOrderedMap(kv("a", intProp()))
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Ref: "Inner"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Inner", &bamlutils.DynamicClass{Properties: inner})),
	}
	// element 0 `{"a":1}` closed → complete → {a:1}; element 1 `{"a":2` incomplete
	// object with incomplete int a → class kept, a null-replaced → {a:null}.
	mustStream(t, s, `{"items":[{"a":1},{"a":2`, `{"items":[{"a":1},{"a":null}]}`)
}

// TestStream_MissingFieldDefaultFillers pins that a MISSING class field is filled
// with BAML's TypeIR::default_value (list→[], map→{}, optional→null), NOT a blanket
// null: BAML's streaming filler reuses the coercion-layer default fill. A missing
// required scalar (int) still fills null (no default_value). (Captured live: see
// corpus 177_streaming_missing_field_fillers.)
func TestStream_MissingFieldDefaultFillers(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("a", intProp()),
			kv("tags", listProp(&bamlutils.DynamicTypeSpec{Type: "string"})),
			kv("scores", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
			kv("note", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})),
		),
	}
	// `{"a":1}` is strict-complete: a=1 kept; tags/scores/note missing → default
	// fillers (list→[], map→{}, optional→null). Driven via the stream coerce bypass
	// to isolate the streaming default fill (incl. the map→{} filler) directly.
	mustStreamCoerce(t, s, `{"a":1}`, `{"a":1,"tags":[],"scores":{},"note":null}`)
	// A missing required scalar has no default_value → null (b never streamed).
	s2 := &bamlutils.DynamicOutputSchema{Properties: props(kv("a", intProp()), kv("b", intProp()))}
	mustStream(t, s2, `{"a":1}`, `{"a":1,"b":null}`)
}

// TestStream_MapIntDropsIncompleteValue pins the map arm: an entry whose
// done-required value is incomplete drops the WHOLE key/value pair (never
// null-kept), keeping completed sibling entries in input key order. (Captured live:
// see corpus 176_streaming_map_int_partial.)
func TestStream_MapIntDropsIncompleteValue(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
	}
	// a=1 comma-terminated (kept); b=2 incomplete (dropped entry). The space after
	// the comma keeps the unquoted object value out of streamFix's greedy-comma
	// decline (an unquoted object value before a TIGHT comma declines).
	mustStreamCoerce(t, s, `{"m":{"a":1, "b":2`, `{"m":{"a":1}}`)
}

// TestStream_NoStructureDeclines pins the content-free-prefix boundary. For a
// >=2-field class, bare prose (no `{`) now CLAIMS the all-filler object (matching
// BAML); an EMPTY string still declines (nothing to parse — never reached by the
// orchestrator, which only parses non-empty accumulated text). For a SINGLE-field
// class, prose declines (BAML errors via allow_as_string, native matches).
func TestStream_NoStructureDeclines(t *testing.T) {
	mustStream(t, personSchema(), "just some prose, no json", `{"name":null,"age":null}`)
	requireStreamUnsupported(t, personSchema(), "")
	requireStreamUnsupported(t, nameOnlySchema(), "just some prose, no json")
}

// TestStream_ScalarToClassDeclines pins that a scalar/array into a class declines
// in stream mode (implied-key / inferred-object / array-to-singular deferred).
func TestStream_ScalarToClassDeclines(t *testing.T) {
	requireStreamUnsupported(t, personSchema(), `"hello"`)
	requireStreamUnsupported(t, personSchema(), `[1,2,3]`)
}

// TestStream_Comments pins Phase 7C JSONish comment recovery on the STREAM path:
// a string-aware pre-pass (stripJSONComments) drops `/* */` block and `//` line
// comments — INCLUDING unterminated ones — outside strings, so a comment-bearing
// streamed prefix parses to the SAME partial BAML recovers (LIVE-CAPTURED, native-
// only, no fallback). A comment INSIDE a string value is kept verbatim.
func TestStream_Comments(t *testing.T) {
	s := personSchema()
	mustStream(t, s, `{"name": "Ada", /* mid */ "age": 3`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name": "Ada", /* unterm`, `{"name":"Ada","age":null}`)
	mustStream(t, s, "{\"name\": \"Ada\", // unterm line", `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name": "Ada", /* mid */ "age": 36}`, `{"name":"Ada","age":36}`)
	mustStream(t, s, `{"name": "Ada", "age": 36, /* n */`, `{"name":"Ada","age":36}`)
	mustStream(t, s, `{"name": "Ada", /* c1 */ "age`, `{"name":"Ada","age":null}`)
	// A comment marker inside a STRING value is not a comment — kept verbatim.
	mustStream(t, s, `{"name": "a/*b*/c", "age": 36}`, `{"name":"a/*b*/c","age":36}`)
}

// TestStream_NilSchemaDeclines pins the nil-schema fallback.
func TestStream_NilSchemaDeclines(t *testing.T) {
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: `{}`, OutputSchema: nil, Stream: true})
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil schema stream: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestStream_WithStateAndAnnotationsDecline pins the M4d part-A boundary:
// @stream.with_state (StreamingBehavior.State) and the other BAML stream
// annotations — @@stream.done (Done), @stream.not_null (class Needed /
// ClassField.StreamingNeeded), and any type-level Stream flag — are
// UNREPRESENTABLE on a dynamic schema. bamlutils.DynamicProperty / DynamicClass /
// DynamicTypeSpec carry NO stream-annotation channel, and
// schema.FromDynamicOutputSchema lowers every class with a zero
// StreamingBehavior (see its doc), so BAML's own dynamic parse-stream never sees
// one either — there is nothing to capture and nothing to claim, and with_state
// STAYS BAML fallback.
//
// checkNoStreamAnnotations is the DEFENSIVE gate that keeps that true even if a
// future bridge extension somehow carried an annotation: any non-zero stream flag
// DECLINES so BAML parse-stream stays authoritative. Because the live dynamic
// bridge cannot produce such a bundle, this white-box test INJECTS each annotation
// onto a freshly-lowered bundle (which the bridge cannot) and asserts the decline —
// pinning that @stream.with_state can never be silently claimed on the stream path.
func TestStream_WithStateAndAnnotationsDecline(t *testing.T) {
	lower := func() *schema.Bundle {
		t.Helper()
		b, err := schema.FromDynamicOutputSchema(personSchema(), schema.BuildOptions{})
		if err != nil {
			t.Fatalf("lower personSchema: %v", err)
		}
		return b
	}
	requireGateDeclines := func(b *schema.Bundle, what string) {
		t.Helper()
		err := checkNoStreamAnnotations(b)
		if err == nil {
			t.Fatalf("%s: expected checkNoStreamAnnotations to DECLINE, got nil", what)
		}
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("%s: expected ErrDeBAMLParseUnsupported, got %v", what, err)
		}
	}
	// A lowered dynamic schema carries NO annotations, so the gate passes (this is
	// the ONLY shape the live bridge can ever produce — hence with_state is always
	// unrepresentable, not merely declined).
	if err := checkNoStreamAnnotations(lower()); err != nil {
		t.Fatalf("annotation-free dynamic schema must pass the gate, got %v", err)
	}
	// Each injected annotation declines. b.Classes[0] is the synthetic
	// Baml_Rest_DynamicOutput class (fields name, age); no nested classes.
	b := lower()
	b.Classes[0].Stream.State = true // @stream.with_state
	requireGateDeclines(b, "@stream.with_state (class State)")

	b = lower()
	b.Classes[0].Stream.Done = true // @@stream.done
	requireGateDeclines(b, "@@stream.done (class Done)")

	b = lower()
	b.Classes[0].Stream.Needed = true // @stream.not_null (class level)
	requireGateDeclines(b, "@stream.not_null (class Needed)")

	b = lower()
	b.Classes[0].Fields[0].StreamingNeeded = true // @stream.not_null (field level)
	requireGateDeclines(b, "@stream.not_null (field StreamingNeeded)")

	b = lower()
	b.Classes[0].Fields[0].Type.Meta.Stream.State = true // @stream.with_state on a field type
	requireGateDeclines(b, "@stream.with_state (field type State)")
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
