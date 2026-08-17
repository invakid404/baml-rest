//go:build integration && nanollm_integration

package opharness

// The STOCK v0.223.0 byte authority for the six direct comparisons, restated here so
// the LIVE proofs can compare what the generated route actually served against what
// stock produced in a completely different harness.
//
// # Why restating them is not a second source of truth
//
// The flag-on/flag-off equality each row asserts proves native and BAML agree IN THIS
// PROCESS. That is necessary and not sufficient: an error that moved BOTH legs — a
// changed carrier, a changed field order, a changed expression spelling — would cancel
// out and leave the comparison green. Pinning stock's own bytes is what catches it.
//
// The literals cannot drift away from their source: [TestCapturesAgreeWithPredicatewire]
// parses internal/debaml/predicatewire's `pwOperatorCaptures` composite literal and
// requires every string here to be byte-identical to the capture it came from. That is
// the same guard internal/debaml applies to its own untagged copies, for the same
// reason — a copy that could drift is a proof about nothing.
//
// Stock produced these bytes. Native output is NEVER re-fed to the CFFI.

// Capture is stock's four outputs for one operator on the two name-pinned classes, at
// the canonical literal `0`.
//
// The field names deliberately match internal/debaml/predicatewire's
// pwOperatorCapture, because the agreement guard pairs them by name.
type Capture struct {
	// CheckTrue / CheckFalse: sonic.Marshal of the decoded CHECK family with the
	// predicate holding and failing. Both carry the value — a false @check is DATA.
	CheckTrue  string
	CheckFalse string
	// AssertTrue: sonic.Marshal of the decoded ASSERT family with the predicate
	// holding — an ordinary int, no wrapper, no check entry.
	AssertTrue string
	// AssertFail: the UNMODIFIED err.Error() of the ASSERT family with the predicate
	// FALSE. The embedded `\n` is stock's Rust DEBUG escape (two bytes), never a real
	// newline.
	AssertFail string
	// TrueVal / FalseVal are the `confidence` values that make `this OP 0` hold and
	// fail. They are part of the capture: the raw assistant text stock was given is
	// what produced the bytes above.
	TrueVal  int64
	FalseVal int64
}

// Captures is the 7.2c-1 CFFI corpus, keyed by the operator ID the capability table
// uses (`gt`, `ge`, `lt`, `le`, `eq`, `ne`).
//
// One literal (`0`) across all six is deliberate and comes from 7.2c-1: it makes every
// byte difference between two operator captures attributable to the OPERATOR alone.
var Captures = map[string]Capture{
	"gt": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":9}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this > 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }] }] }] }`,
		TrueVal:    9, FalseVal: -1,
	},
	"ge": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":0}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this >= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this >= 0", causes: [] }] }] }] }`,
		TrueVal:    0, FalseVal: -1,
	},
	"lt": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":-1}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this < 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this < 0", causes: [] }] }] }] }`,
		TrueVal:    -1, FalseVal: 9,
	},
	"le": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":0}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this <= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this <= 0", causes: [] }] }] }] }`,
		TrueVal:    0, FalseVal: 9,
	},
	"eq": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":0}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this == 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this == 0", causes: [] }] }] }] }`,
		TrueVal:    0, FalseVal: 9,
	},
	"ne": {
		CheckTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"succeeded"}}}}`,
		CheckFalse: `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"failed"}}}}`,
		AssertTrue: `{"answer":"sunny","confidence":9}`,
		AssertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this != 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this != 0", causes: [] }] }] }] }`,
		TrueVal:    9, FalseVal: 0,
	},
}

// Tokens maps an operator ID to its BAML source token, so a row can render the
// canonical expression its project declares.
var Tokens = map[string]string{
	"gt": ">", "ge": ">=", "lt": "<", "le": "<=", "eq": "==", "ne": "!=",
}

// Expression is the canonical predicate text of one operator at the captured literal —
// the exact bytes stock retains in Check.Expression and quotes in an assertion cause.
func Expression(opID string) string { return "this " + Tokens[opID] + " 0" }
