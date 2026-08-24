package debaml

// De-BAML burn-down batch 1 — the WORKER-BOUNDARY byte parity these fixtures used
// to miss.
//
// `/parse/_dynamic` is native-first behind a transition oracle that serves native's
// answer only when its bytes equal BAML's, compared at the WORKER boundary — before
// dynclient's absent-optional / reorder passes run. Five corpus fixtures that the
// native parser already CLAIMED nonetheless declined there (`result_drift`), for two
// non-semantic reasons: BAML spells an absent optional as an explicit `null` where
// native omitted the key, and BAML emits a class's fields in the order its
// TypeBuilder was populated (alphabetical for a preserve_order=false request) where
// native emitted them in the caller's declared order.
//
// Both are now closed. The parser emits the null itself (coerce.go's defaults pass),
// and the worker declares the schema in BAML's TypeBuilder order before calling it
// (worker/direct_parse_schema_order.go), so no downstream pass is needed for the two
// legs to agree.
//
// These tests pin the parser half BYTE-FOR-BYTE, one per fixture, with the schema
// declared in the order BAML's TypeBuilder receives it. The `want` strings are the
// BAML worker-boundary payloads observed on the deployed route (the #685 run's
// characterization of the five `result_drift` declines); the deployed-route
// differential in integration/debaml_direct_parse_route_test.go re-proves them
// against a live BAML container every run.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// bookCarUnionSchema is Root{u: Book | Car [| null]} with Book{pages,title} and
// Car{brand,wheels} declared in the ALPHABETICAL order applyDynamicTypes populates
// the TypeBuilder in for a preserve_order=false request. The corpus fixtures declare
// Book title-first; that difference is exactly what the worker's order pass erases,
// and what these expectations encode.
func bookCarUnionSchema(withNullArm bool) *bamlutils.DynamicOutputSchema {
	arms := []*bamlutils.DynamicTypeSpec{{Ref: "Book"}, {Ref: "Car"}}
	if withNullArm {
		arms = append(arms, &bamlutils.DynamicTypeSpec{Type: "null"})
	}
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{Type: "union", OneOf: arms})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Book", &bamlutils.DynamicClass{
				Properties: props(kv("pages", intProp()), kv("title", strProp())),
			}),
			bamlutils.OrderedKV("Car", &bamlutils.DynamicClass{
				Properties: props(kv("brand", strProp()), kv("wheels", intProp())),
			}),
		),
	}
}

// bookWithExtraKeys renders the corpus's extra-key raws: Book's two fields followed
// by n throwaway keys, each worth one ExtraKey point, which is how fixtures 142/143
// land on exactly 109 and 110 against the null arm's 110.
func bookWithExtraKeys(n int) string {
	var b strings.Builder
	b.WriteString(`{"u":{"title":"Go","pages":300`)
	for i := range n {
		b.WriteString(`,"e`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":1`)
	}
	b.WriteString(`}}`)
	return b.String()
}

// TestWorkerBoundary_ClassMissingOptionalNull is corpus fixture 127
// (class_missing_optional_null): C{name, nick?} with nick absent. BAML's parse
// carries the optional as an explicit null; the parser now emits the same bytes
// instead of leaving the key for a downstream injector to add.
func TestWorkerBoundary_ClassMissingOptionalNull(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{Ref: "C"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
				Properties: props(kv("name", strProp()), kv("nick", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
			}),
		),
	}
	mustParseExact(t, s, `{"u":{"name":"Ada"}}`, `{"u":{"name":"Ada","nick":null}}`)
}

// TestWorkerBoundary_ClassUnionStringificationOneSuccess is corpus fixture 138: the
// Book arm wins leniently (title=5 stringifies to "5") and comes back in the
// TypeBuilder's field order, pages before title.
func TestWorkerBoundary_ClassUnionStringificationOneSuccess(t *testing.T) {
	mustParseExact(t, bookCarUnionSchema(false),
		`{"u":{"title":5,"pages":300}}`,
		`{"u":{"pages":300,"title":"5"}}`)
}

// TestWorkerBoundary_ClassUnionExtraKeyScoreBoundaries is corpus fixtures 142 and
// 143: the Book arm beats the null arm at 109 extra keys and again at the 110 tie
// (lower arm index wins), and both winners are emitted pages-first.
func TestWorkerBoundary_ClassUnionExtraKeyScoreBoundaries(t *testing.T) {
	s := bookCarUnionSchema(true)
	for _, extras := range []int{109, 110} {
		mustParseExact(t, s, bookWithExtraKeys(extras), `{"u":{"pages":300,"title":"Go"}}`)
	}
}

// TestWorkerBoundary_UnicodeBoolAccentFold is corpus fixture 199: a root class whose
// declared order (ok, n) is not alphabetical, so BAML returns n first. The accent
// fold itself is unchanged — this pins only the order the folded result is emitted in.
func TestWorkerBoundary_UnicodeBoolAccentFold(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("n", intProp()), kv("ok", boolProp())),
	}
	mustParseExact(t, s, `{"ok": "trué", "n": 5}`, `{"n":5,"ok":true}`)
}

// TestWorkerBoundary_DeclaredOrderIsTheEmittedOrder is the control for the four
// order fixtures above: the parser has no ordering policy of its own — it emits a
// class's fields in the order the schema declares them, which is why handing it the
// TypeBuilder's order is sufficient. Declaring the SAME class the other way round
// produces the other order, so the expectations above are pinning the worker's
// declaration choice rather than an accident of the coercer.
func TestWorkerBoundary_DeclaredOrderIsTheEmittedOrder(t *testing.T) {
	wireOrder := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("ok", boolProp()), kv("n", intProp())),
	}
	mustParseExact(t, wireOrder, `{"ok": true, "n": 5}`, `{"ok":true,"n":5}`)
}
