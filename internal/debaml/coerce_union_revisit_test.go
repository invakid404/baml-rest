package debaml

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Mcoerce-d PR 3 — UNION REVISIT. The leaf/structural coercers (coerceLiteral,
// coerceClass) became lenient in PR 1/PR 2 (ObjectToPrimitive / ObjectToString /
// JsonToString / class defaults), and the safe-family union counters
// (coerceLiteralUnion / coerceFlatClassUnion) already COUNT successes through
// them. These tests pin that the counters now CLAIM the newly-deterministic
// one-success cases while keeping the no-over-claim guard (count != 1 declines)
// and the nullable clean-only rule (a flagged non-null winner declines vs the
// scored null arm). No pick_best, no broadening beyond the two safe families.

// TestLiteralUnion_ObjectToPrimitive_OneSuccess pins that a homogeneous
// int-literal union claims when a single-key-object extraction (ObjectToPrimitive)
// makes EXACTLY one arm coerce to its value: 1|2 with {"value":1} -> 1.
func TestLiteralUnion_ObjectToPrimitive_OneSuccess(t *testing.T) {
	s := litIntUnion(1, 2)
	mustParse(t, s, `{"u":{"value":1}}`, `{"u":1}`) // extract 1 -> matches arm 1 only
	mustParse(t, s, `{"u":{"any":2}}`, `{"u":2}`)   // key ignored, extract 2 -> arm 2 only
	// Extracted inner value matches NO arm -> decline (BAML errors/pick_best neither).
	requireUnsupported(t, s, `{"u":{"value":7}}`)
}

// TestLiteralUnion_ObjectToString_OneSuccess pins that a homogeneous
// string-literal union claims when stringification (ObjectToString) makes
// EXACTLY one arm match: "5"|"6" with 5 -> "5".
func TestLiteralUnion_ObjectToString_OneSuccess(t *testing.T) {
	s := unionSchema(litStr("5"), litStr("6"))
	mustParse(t, s, `{"u":5}`, `{"u":"5"}`) // Display "5" matches literal "5" only
	mustParse(t, s, `{"u":6}`, `{"u":"6"}`)
	// Display matches neither literal -> decline.
	requireUnsupported(t, s, `{"u":7}`)
}

// TestLiteralUnion_TwoSuccesses_Decline pins the over-claim guard: a stringified
// value that substring-matches BOTH disjoint literal arms is two lenient
// successes -> BAML pick_best (M3) -> decline (native must never pick one).
func TestLiteralUnion_TwoSuccesses_Decline(t *testing.T) {
	s := unionSchema(litStr("foo"), litStr("bar"))
	// Array Display "[foo, bar]" substring-matches both "foo" and "bar" arms.
	requireUnsupported(t, s, `{"u":["foo","bar"]}`)
}

// TestLiteralUnion_NullableFlaggedArm_Declines pins the nullable clean-only rule
// for both literal families: a non-null arm won only via ObjectToPrimitive /
// ObjectToString is FLAGGED, so it declines against the scored null arm (M3),
// while the JSON-null fast path still claims null.
func TestLiteralUnion_NullableFlaggedArm_Declines(t *testing.T) {
	sInt := oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "literal_int", Value: int64(1)},
			{Type: "literal_int", Value: int64(2)},
			{Type: "null"},
		},
	})
	mustParse(t, sInt, `{"u":null}`, `{"u":null}`)   // null fast path
	requireUnsupported(t, sInt, `{"u":{"value":1}}`) // ObjectToPrimitive flag -> decline

	sStr := unionSchema(litStr("5"), litStr("6"), &bamlutils.DynamicTypeSpec{Type: "null"})
	mustParse(t, sStr, `{"u":null}`, `{"u":null}`)
	mustParse(t, sStr, `{"u":"5"}`, `{"u":"5"}`) // clean exact string still claims
	requireUnsupported(t, sStr, `{"u":5}`)       // ObjectToString flag -> decline
}

// TestClassUnion_Stringification_OneSuccess pins that a flat disjoint-key class
// union claims when field stringification (JsonToString) inside EXACTLY one arm
// makes it succeed: Book{title,pages}|Car{brand,wheels}, title <- 5 -> "5".
func TestClassUnion_Stringification_OneSuccess(t *testing.T) {
	s := classUnionSchema()
	mustParse(t, s, `{"u":{"title":5,"pages":300}}`, `{"u":{"title":"5","pages":300}}`)
}

// TestClassUnion_Stringification_TwoSuccesses_Decline pins the over-claim guard:
// an input carrying BOTH arms' full field sets makes both succeed -> BAML
// pick_best (M3) -> decline (native must never emit either arm).
func TestClassUnion_Stringification_TwoSuccesses_Decline(t *testing.T) {
	s := classUnionSchema()
	requireUnsupported(t, s, `{"u":{"title":5,"pages":300,"brand":"Audi","wheels":4}}`)
}

// TestClassUnion_NullableStringified_Declines pins the nullable clean-only rule:
// a JsonToString-flagged winning class arm declines against the scored null arm,
// while the JSON-null fast path still claims null.
func TestClassUnion_NullableStringified_Declines(t *testing.T) {
	s := nullableClassUnionSchema()
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	requireUnsupported(t, s, `{"u":{"title":5,"pages":300}}`) // JsonToString flag -> decline
}

// TestNullableOptionalString_JsonToString_Declines pins that an optional string
// with a NON-string input coerces via JsonToString (flagged) and so declines
// under the nullable clean-only rule, while a clean string passthrough claims.
func TestNullableOptionalString_JsonToString_Declines(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "string"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"hi"}`, `{"u":"hi"}`) // clean passthrough -> claim
	requireUnsupported(t, s, `{"u":5}`)         // JsonToString -> decline
}

// TestNullableOptionalClassDefault_Declines pins that an optional class whose
// lone arm fills a required-field DEFAULT (DefaultFromNoValue) is flagged, so it
// declines under the nullable clean-only rule (C{items int[]} with {} -> []).
func TestNullableOptionalClassDefault_Declines(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", optProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
			Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
		})),
	}
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	requireUnsupported(t, s, `{"u":{}}`) // DefaultFromNoValue -> decline
}
