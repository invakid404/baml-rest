package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Phase 7C native-only streaming serving surface: the pure schema-support
// preflights (SupportsNativeStream / SupportsNativeFinal) and the native-only
// partial/final parser closures (ParseNativeStreamPartial / ParseNativeStreamFinal)
// that own every admitted prefix WITHOUT ever falling back to BAML (I6).

// listMultiArmUnionSchema is Root{items:list<int|bool>} — a direct
// list<multi-arm-union> element, the deferred array union_variant_hint shape
// checkSupportedType declines (over-claim risk). Used as an UNSUPPORTED schema.
func listMultiArmUnionSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "bool"}},
		}))),
	}
}

// --- Schema-support preflight (pure, socket-free, input-free) ---

// numsListSchema is Root{nums:list<int>} — a SINGLE non-string-absorbing field
// root class (admissible: BAML strips the fence and emits the all-filler, matching
// native, and errors on a bare prose preamble which native declines).
func numsListSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("nums", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
	}
}

func TestSupportsNativeStream_Supported(t *testing.T) {
	// Admitted: >=2-field classes and a single NON-string-absorbing field class.
	for _, s := range []*bamlutils.DynamicOutputSchema{
		personSchema(),
		nameTagsSchema(),
		numsListSchema(),
	} {
		if err := SupportsNativeStream(s); err != nil {
			t.Errorf("SupportsNativeStream: expected nil for a supported schema, got %v", err)
		}
		if err := SupportsNativeFinal(s); err != nil {
			t.Errorf("SupportsNativeFinal: expected nil for a supported schema, got %v", err)
		}
	}
}

// TestSupportsNativeStream_SingleStringFieldDeclines pins the §5.9 admission
// narrowing: a single STRING-absorbing-field root class (Root{name:string}) is
// DECLINED pre-transport because BAML's allow_as_string->class diverges from native
// at a fenced prefix and a bare-string final.
func TestSupportsNativeStream_SingleStringFieldDeclines(t *testing.T) {
	s := nameOnlySchema() // Root{name:string}
	if err := SupportsNativeStream(s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeStream(single string field): want decline, got %v", err)
	}
	if err := SupportsNativeFinal(s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeFinal(single string field): want decline, got %v", err)
	}
	// An OPTIONAL single string field also absorbs a string -> declined.
	optStr := &bamlutils.DynamicOutputSchema{Properties: props(kv("note", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))}
	if err := SupportsNativeStream(optStr); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeStream(single optional-string field): want decline, got %v", err)
	}
}

// TestSupportsNativeStream_NonASCIIAdmitted pins #555 Slice 2: the stream lane now
// ADMITS non-ASCII string-literal / enum VALUES and non-ASCII field NAMES — their
// match_string fold is proven via bamlunicode, and the streaming cadence is
// character-agnostic — so a native streamed request no longer falls back to BAML for
// these. (Their per-prefix parity is locked by the streaming_unicode_* corpus.)
func TestSupportsNativeStream_NonASCIIAdmitted(t *testing.T) {
	enumS := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("status", &bamlutils.DynamicProperty{Ref: "Lam"}), kv("note", strProp())),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Lam", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "ƛ"}, {Name: "other"}}, // U+019B enum value
		})),
	}
	litS := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{Type: "literal_string", Value: "ƛ"}), kv("note", strProp())),
	}
	nameS := &bamlutils.DynamicOutputSchema{ // non-ASCII field NAME ƛ
		Properties: props(kv("ƛ", strProp()), kv("count", intProp())),
	}
	for _, c := range []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
	}{{"enum-value", enumS}, {"string-literal", litS}, {"field-name", nameS}} {
		if err := SupportsNativeStream(c.s); err != nil {
			t.Errorf("SupportsNativeStream(non-ASCII %s): want admit (nil), got %v", c.name, err)
		}
		if err := SupportsNativeFinal(c.s); err != nil {
			t.Errorf("SupportsNativeFinal(non-ASCII %s): want admit (nil), got %v", c.name, err)
		}
	}
}

// TestSupportsNativeStream_MetadataAdmitted pins that the metadata cases with NO
// key-matching divergence are ADMITTED: a field @description (does not change the key
// candidate), and an enum @alias / @description (a complete enum VALUE routes through the
// final coercer whose enumMatchCandidates already models the rendered name, description,
// and "rendered: description" candidate). Class-type @alias/@description are likewise not
// matched as keys.
func TestSupportsNativeStream_MetadataAdmitted(t *testing.T) {
	fieldDescS := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("title", &bamlutils.DynamicProperty{Type: "string", Description: "the heading"}), kv("count", intProp())),
	}
	enumAliasS := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("status", &bamlutils.DynamicProperty{Ref: "St"}), kv("note", strProp())),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("St", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "OPEN", Alias: "opened"}, {Name: "SHUT", Description: "closed up"}},
		})),
	}
	for _, c := range []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
	}{{"field-description", fieldDescS}, {"enum-alias-desc", enumAliasS}} {
		if err := SupportsNativeStream(c.s); err != nil {
			t.Errorf("SupportsNativeStream(%s): want admit (nil), got %v", c.name, err)
		}
		if err := SupportsNativeFinal(c.s); err != nil {
			t.Errorf("SupportsNativeFinal(%s): want admit (nil), got %v", c.name, err)
		}
	}
}

// TestSupportsNativeStream_FieldAliasAdmitted pins the #583 teardown: a NON-colliding field
// @alias now ADMITS on both native serving lanes. The scope finding overturned the earlier
// "BAML matches the canonical key too" premise — that was an artifact of the DYNAMIC parse
// bridge dropping aliases, not static BAML v0.223. Static/jsonish BAML is ALIAS-ONLY (it
// matches a class key against the field's rendered name only), and native's coerceClass/
// coerceStreamClass do exactly that (Name.RenderedName() only), so removing the blanket gate
// makes native byte-exact vs static BAML.
func TestSupportsNativeStream_FieldAliasAdmitted(t *testing.T) {
	fieldAliasS := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("title", &bamlutils.DynamicProperty{Type: "string", Alias: "banner"}), kv("count", intProp())),
	}
	if err := SupportsNativeStream(fieldAliasS); err != nil {
		t.Errorf("SupportsNativeStream(field-alias): want admit (nil), got %v", err)
	}
	if err := SupportsNativeFinal(fieldAliasS); err != nil {
		t.Errorf("SupportsNativeFinal(field-alias): want admit (nil), got %v", err)
	}
}

// TestSupportsNativeStream_FieldAliasCollisionDeclines pins the retained #583 residual: a
// rendered-name COLLISION involving an @alias still declines, because BAML's collision
// resolution is order-dependent and NOT uniform across its two coercion paths, and no v0.223
// test pins whether the schema compiler even admits such a class.
//
//   - The BYTE-equal rendered collision (`a @alias("b")` + literal `b`) is rejected at
//     lowering by the rendered-name index (internal/schema/index.go), surfaced as the
//     unsupported sentinel by lowerForSupport — it never reaches the coercers.
//   - The FUZZY collision between two ALIASES (`@alias("Foo")` vs `@alias("foo")` — fold-equal,
//     byte-distinct) slips past the exact index, so aliasRenderedNameCollision declines it.
//   - The FUZZY collision between an ALIAS and a PLAIN unaliased canonical sibling
//     (`a @alias("Foo")` vs literal field `foo`) ALSO slips past the exact byte-equal index
//     ("Foo" != "foo"), and aliasRenderedNameCollision catches it too — the pair qualifies
//     because at least one side carries an @alias. (CodeRabbit r3616424315.)
func TestSupportsNativeStream_FieldAliasCollisionDeclines(t *testing.T) {
	exact := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("a", &bamlutils.DynamicProperty{Type: "string", Alias: "b"}), kv("b", strProp())),
	}
	fuzzy := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("a", &bamlutils.DynamicProperty{Type: "string", Alias: "Foo"}),
			kv("bb", &bamlutils.DynamicProperty{Type: "string", Alias: "foo"}),
		),
	}
	// alias @alias("Foo") vs a PLAIN unaliased canonical sibling `foo` (fold-collision the
	// byte-equal index MISSES; the gate must catch it).
	fuzzyPlainCanonical := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("a", &bamlutils.DynamicProperty{Type: "string", Alias: "Foo"}),
			kv("foo", strProp()),
		),
	}
	for _, c := range []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
	}{
		{"exact-collision(index)", exact},
		{"fuzzy-collision-alias-vs-alias(gate)", fuzzy},
		{"fuzzy-collision-alias-vs-plain-canonical(gate)", fuzzyPlainCanonical},
	} {
		if err := SupportsNativeStream(c.s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("SupportsNativeStream(%s): want decline, got %v", c.name, err)
		}
		if err := SupportsNativeFinal(c.s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("SupportsNativeFinal(%s): want decline, got %v", c.name, err)
		}
	}
}

func TestSupportsNativeStream_NilSchema(t *testing.T) {
	if err := SupportsNativeStream(nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeStream(nil): want ErrDeBAMLParseUnsupported, got %v", err)
	}
	if err := SupportsNativeFinal(nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeFinal(nil): want ErrDeBAMLParseUnsupported, got %v", err)
	}
}

func TestSupportsNativeStream_UnsupportedShapeDeclines(t *testing.T) {
	s := listMultiArmUnionSchema()
	if err := SupportsNativeStream(s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeStream(list<int|bool>): want ErrDeBAMLParseUnsupported, got %v", err)
	}
	if err := SupportsNativeFinal(s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("SupportsNativeFinal(list<int|bool>): want ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestSupportsNativeStream_CyclicClassGraphDeclines pins the round-13 (CodeRabbit
// discussion_r3610404766) guard: a self- or mutually-recursive class graph is outside the
// finite acyclic admitted core and previously stack-overflowed the recursive-descent gate
// (checkNested). It must now DECLINE cleanly (no crash), while a DAG "diamond" — the same
// class referenced by two fields on ACYCLIC paths — must NOT be mistaken for a cycle.
func TestSupportsNativeStream_CyclicClassGraphDeclines(t *testing.T) {
	ref := func(name string) *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Ref: name} }
	// Node{v:string, next:Node} — direct self-reference.
	selfRef := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("label", strProp()), kv("child", ref("Node"))),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Node", &bamlutils.DynamicClass{
			Properties: props(kv("v", strProp()), kv("next", ref("Node"))),
		})),
	}
	// A{x:string, toB:B}, B{y:string, toA:A} — mutual recursion.
	mutual := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("label", strProp()), kv("a", ref("A"))),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("x", strProp()), kv("toB", ref("B")))}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{Properties: props(kv("y", strProp()), kv("toA", ref("A")))}),
		),
	}
	for _, c := range []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
	}{{"self-ref", selfRef}, {"mutual", mutual}} {
		if err := SupportsNativeStream(c.s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("SupportsNativeStream(%s cyclic): want ErrDeBAMLParseUnsupported, got %v", c.name, err)
		}
	}
	// DAG diamond: root{x:Leaf, y:Leaf}, Leaf{v:string, n:int}. Leaf is reached twice but on
	// acyclic paths — a >=2-field root carrying a safe nested class stays ADMITTED, and the
	// cycle guard must not false-positive it.
	diamond := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("x", ref("Leaf")), kv("y", ref("Leaf"))),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Leaf", &bamlutils.DynamicClass{
			Properties: props(kv("v", strProp()), kv("n", intProp())),
		})),
	}
	if err := SupportsNativeStream(diamond); err != nil {
		t.Errorf("SupportsNativeStream(DAG diamond): want admitted (nil), got %v", err)
	}
}

// TestSupportsNativeStream_MatchesRuntimeCutLine proves the preflight is a total
// schema-graph gate consistent with the runtime parser: a schema the preflight
// admits must NOT produce a schema-shape unsupported at runtime (only the benign
// not-yet-parseable prefix declines remain, which are input-dependent). We check
// the contrapositive here at the schema level; the differential proves the
// runtime parity per-prefix.
func TestSupportsNativeStream_ConsistentWithParse(t *testing.T) {
	// Admitted schema: a claimed prefix parses; a not-yet-parseable prefix declines
	// benignly — never a schema-shape unsupported.
	s := personSchema()
	if err := SupportsNativeStream(s); err != nil {
		t.Fatalf("precondition: personSchema must be supported, got %v", err)
	}
	// A declined schema: the runtime parser also declines every prefix with the
	// same sentinel (the shape gate fires first).
	bad := listMultiArmUnionSchema()
	if err := SupportsNativeStream(bad); err == nil {
		t.Fatalf("precondition: list<int|bool> must be unsupported")
	}
	_, perr := Parse(context.Background(), bamlutils.DeBAMLParseRequest{
		Raw: `{"items":[1,true]}`, OutputSchema: bad, Stream: true,
	})
	if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("runtime Parse of an unsupported schema: want ErrDeBAMLParseUnsupported, got %v", perr)
	}
}

// --- Native-only partial parser closure ---

func TestParseNativeStreamPartial_Emits(t *testing.T) {
	s := personSchema()
	cases := []struct {
		raw  string
		want string
	}{
		// Content-free open object (7C gap closure) — all-filler.
		{`{`, `{"name":null,"age":null}`},
		{`{"name"`, `{"name":null,"age":null}`},
		{`{"name":`, `{"name":null,"age":null}`},
		// Progressive content.
		{`{"name":"Ad`, `{"name":"Ad","age":null}`},
		{`{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`},
	}
	for _, c := range cases {
		got, err := ParseNativeStreamPartial(context.Background(), s, c.raw)
		if err != nil {
			t.Errorf("ParseNativeStreamPartial(%q): unexpected error %v", c.raw, err)
			continue
		}
		if got == nil {
			t.Errorf("ParseNativeStreamPartial(%q): expected a partial, got skip (nil)", c.raw)
			continue
		}
		if !jsonValueEqual(t, string(got), c.want) {
			t.Errorf("ParseNativeStreamPartial(%q):\n got %s\nwant %s", c.raw, got, c.want)
		}
	}
}

// TestSupportsNativeStream_NonLastScalarDeclines pins the §5.9 admission narrowing
// for the BAML greedy-read cascade: a class with a NON-LAST unquoted-scalar field
// (int/float/bool before another field) is DECLINED pre-transport, because BAML
// greedy-reads such a value across a tight comma in a compact stream — a cascade
// native cannot reproduce. A LAST scalar field is fine (no following field), and
// string/list/map fields never greedy-read.
func TestSupportsNativeStream_NonLastScalarDeclines(t *testing.T) {
	declines := map[string]*bamlutils.DynamicOutputSchema{
		"int_then_int": {Properties: props(kv("a", intProp()), kv("b", intProp()))},
		"int_then_str": {Properties: props(kv("a", intProp()), kv("b", strProp()))},
		"bool_then_list": {Properties: props(
			kv("flag", &bamlutils.DynamicProperty{Type: "bool"}),
			kv("tags", listProp(&bamlutils.DynamicTypeSpec{Type: "string"})),
		)},
		// A map with an unquoted-scalar VALUE — its internal `1,"k"` greedy-reads.
		"str_then_mapint": {Properties: props(kv("name", strProp()), kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"})))},
		// An OPTIONAL/nullable field — mixed-union semantic streaming deferred.
		"str_then_optint": {Properties: props(
			kv("name", strProp()),
			kv("n", optProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
		)},
	}
	for name, s := range declines {
		if err := SupportsNativeStream(s); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("SupportsNativeStream(%s): want decline (non-last scalar), got %v", name, err)
		}
	}
	// A LAST scalar field is admissible (string non-last, int last).
	admits := map[string]*bamlutils.DynamicOutputSchema{
		"str_then_int": {Properties: props(kv("name", strProp()), kv("age", intProp()))},
		"list_then_int": {Properties: props(
			kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
			kv("total", intProp()),
		)},
	}
	for name, s := range admits {
		if err := SupportsNativeStream(s); err != nil {
			t.Errorf("SupportsNativeStream(%s): want admit (last scalar), got %v", name, err)
		}
	}
}

// TestParseNativeStreamPartial_MultiFieldProseEmitsAllFiller pins the closed
// eligible-prefix gap: for a >=2-field root class, a content-free prefix (bare
// prose / an opened-but-empty fence, no `{`) now CLAIMS the all-filler object
// byte-exact with BAML parse-stream (LIVE-CAPTURED corpus 21). No BAML fallback.
func TestParseNativeStreamPartial_MultiFieldProseEmitsAllFiller(t *testing.T) {
	s := personSchema()
	for _, raw := range []string{"Here you go:\n", "Here you go:\n```json\n"} {
		got, err := ParseNativeStreamPartial(context.Background(), s, raw)
		if err != nil {
			t.Errorf("ParseNativeStreamPartial(%q): unexpected error %v", raw, err)
			continue
		}
		if !jsonValueEqual(t, string(got), `{"name":null,"age":null}`) {
			t.Errorf("ParseNativeStreamPartial(%q): got %s, want all-filler", raw, got)
		}
	}
}

// TestParseNativeStreamPartial_SkipsWithoutBAML pins that a prefix BAML also does
// NOT emit a partial for returns (nil, nil) — the caller emits NOTHING and keeps
// draining — NEVER a BAML fallback and never an error. A single NON-string field
// root (nums:list) at a bare-prose prefix is such a case: BAML errors
// (allow_as_string on the incomplete preamble), native declines, both no-emit.
func TestParseNativeStreamPartial_SkipsWithoutBAML(t *testing.T) {
	s := numsListSchema()
	for _, raw := range []string{
		"Here you go:\n",          // bare prose — BAML errors, native declines
		"Here you go:\n```json\n", // just-opened fence — BAML errors, native declines
	} {
		got, err := ParseNativeStreamPartial(context.Background(), s, raw)
		if err != nil {
			t.Errorf("ParseNativeStreamPartial(%q): expected a silent skip, got error %v", raw, err)
		}
		if got != nil {
			t.Errorf("ParseNativeStreamPartial(%q): expected skip (nil), got %s", raw, got)
		}
	}
}

// --- Native-only final parser closure ---

func TestParseNativeStreamFinal_Claims(t *testing.T) {
	s := personSchema()
	got, err := ParseNativeStreamFinal(context.Background(), s, `{"name":"Ada","age":36}`)
	if err != nil {
		t.Fatalf("ParseNativeStreamFinal: unexpected error %v", err)
	}
	if !jsonValueEqual(t, string(got), `{"name":"Ada","age":36}`) {
		t.Errorf("ParseNativeStreamFinal: got %s, want %s", got, `{"name":"Ada","age":36}`)
	}
}

// TestParseNativeStreamFinal_UnsupportedIsTerminalInvariant pins that a final the
// native parser DECLINES is remapped to the TERMINAL ErrDeBAMLNativeStreamUnsupported
// invariant — NEVER a BAML re-parse (I6). The input `{"name":"Ada"` (a truncated
// object MISSING the required `age`) is a final BAML ALSO errors on (LIVE-CAPTURED:
// "Missing required field: age"), so native terminating here is contractual PARITY
// with BAML, not a divergence — this test pins the terminal-invariant MECHANISM
// (decline -> terminal, not fallback), which the differential exercises for real.
func TestParseNativeStreamFinal_UnsupportedIsTerminalInvariant(t *testing.T) {
	s := personSchema()
	_, err := ParseNativeStreamFinal(context.Background(), s, `{"name":"Ada"`)
	if err == nil {
		t.Fatalf("ParseNativeStreamFinal(truncated): expected a terminal error, got success")
	}
	if !errors.Is(err, ErrDeBAMLNativeStreamUnsupported) {
		t.Errorf("ParseNativeStreamFinal(truncated): want ErrDeBAMLNativeStreamUnsupported, got %v", err)
	}
	// It must NOT surface the BAML-fallback sentinel — a caller must never read this
	// as "fall back to BAML".
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("ParseNativeStreamFinal: terminal invariant must not unwrap to the BAML fallback sentinel")
	}
}
