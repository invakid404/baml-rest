//go:build integration

package guardledger

import "github.com/invakid404/baml-rest/internal/debaml"

// The WITNESS CORPUS: one record per (guard, expression, value) the guard
// ledger cites as evidence.
//
// A row is the unit scope §1 defines: a generated .baml project method plus the
// exact raw model JSON, driven through stock BAML v0.223.0's CFFI. The STOCK
// envelope pinned here is a RECORDING, not a prediction — it was read off the
// live CFFI (see the recording mode in harness_test.go) and is re-verified
// against it on every run. The native envelope is pinned beside it so a change
// on either side has to be acknowledged rather than absorbed.

// envelope is the OUTCOME ENVELOPE of one leg on one row, i.e. what the caller
// observes rather than which value came back.
//
// The stock side is read from a real Parse: a check status, or the shape of the
// coercion error BAML raised. The native side is read from EvaluateConstraint.
// Neither side is allowed an "other" bucket — an unrecognised observation is a
// test failure (see classifyStock), never a silently defaulted envelope.
type envelope string

const (
	// envPass: a @check whose predicate rendered "true" (status "succeeded"), or
	// a @assert that held, in which case BAML emits the value with no error.
	envPass envelope = "pass"
	// envFailedCheck: a @check whose predicate rendered "false" (status
	// "failed"). The value is still emitted; the failure is DATA.
	envFailedCheck envelope = "failed-check"
	// envAssertError: a @assert whose predicate rendered "false". BAML rejects
	// the value with "Assertions failed." — the whole node fails to coerce.
	envAssertError envelope = "assertion-error"
	// envEvaluatorError: the predicate failed to compile or evaluate, or
	// rendered something that is not exactly "true"/"false". BAML reports
	// "Failed to evaluate constraints: ..." and rejects the node. It is NOT a
	// failed check and must never be mapped onto one.
	envEvaluatorError envelope = "evaluator-error"
	// envNoChecks: stock emitted the value carrying NO entry for the label.
	// Observed for an OPTIONAL field whose predicate errored: the optional
	// coercion swallows the failure and yields null instead of rejecting.
	envNoChecks envelope = "no-checks"
	// envProcessFatal: stock cannot be observed in-process at all — it aborts or
	// hangs the CFFI process. Recorded from an isolated subprocess and NEVER
	// converted into a boolean. See fatal_test.go.
	envProcessFatal envelope = "process-fatal"
	// envSourceRejected: the SOURCE SPELLING does not compile as a BAML
	// attribute at all — the project fails to build, so no method exists to
	// drive. It is a fact about the attribute language rather than about either
	// engine, and a row carrying it is excluded from the rendered fixture and
	// proved separately (see TestGuardLedgerRejectedSourceSpellings).
	envSourceRejected envelope = "source-rejected"
	// envUnsupported is NATIVE ONLY: EvaluateConstraint refused with
	// ErrConstraintUnsupported, i.e. it declined to decide. It is the only
	// native envelope allowed to differ from stock's.
	envUnsupported envelope = "native-unsupported"
)

// level is the BAML attribute a row is instantiated under.
type level string

const (
	levelCheck  level = "check"
	levelAssert level = "assert"
)

// agreement is how a row instance's two envelopes relate. Every instance falls
// in exactly one bucket, and the tally in harness_test.go pins the population of
// each — so a guard removal that quietly turned a decline into an answer, or an
// answer into a decline, has to be acknowledged.
type agreement string

const (
	// agAnswer: stock DECIDED (pass / failed-check / assertion-error) and native
	// decided the same thing. Proven parity.
	agAnswer agreement = "agree-answer"
	// agRefusal: stock REFUSED to produce a boolean (evaluator-error, no-checks)
	// and native also refused (ErrConstraintUnsupported). Neither engine serves a
	// boolean, so the envelopes agree in substance.
	agRefusal agreement = "agree-refusal"
	// agNativeDeclines: stock decided, native refused. SAFE — the caller declines
	// to BAML — but it is the measured COST of a guard, and a guard whose rows
	// land here is NOT green and may not be removed.
	agNativeDeclines agreement = "native-declines"
	// agFatal: stock is unobservable (process-fatal). Native must refuse; there
	// is no envelope to agree with, and no boolean may be fabricated.
	agFatal agreement = "stock-unobservable"
	// agSourceRejected: BAML will not compile the source spelling, so no stock
	// leg exists. Native must decline it too, which is asserted, but there is no
	// envelope to agree with.
	agSourceRejected agreement = "source-rejected"
)

// guardGroup binds one `this` value: the BAML field type that carries the
// attributes, the assistant text that produces the value, and the native
// ConstraintValue that must model the same document.
type guardGroup struct {
	Name     string
	BAMLType string
	// Input is the full assistant text fed to Parse.<Method>: the one-field
	// object {"v": <value>} every fixture class is shaped for.
	Input string
	This  debaml.ConstraintValue
}

// guardRow is one witness. It carries everything scope §1 requires a row to
// retain: declaration placement (Group + BAMLType), raw JSON (Group.Input),
// the exact bare expression SOURCE BYTES (Expr), the expression stock actually
// evaluates (Retained — BAML's attribute lexer doubles backslashes), the level,
// the label, and the complete stock envelope per level.
type guardRow struct {
	// ID is the witness id the ledger cites (N1…N12, O1…O9, …). It is also the
	// BAML check/assert LABEL, so a batched result is recoverable by name.
	ID string
	// Guards are the ledger guard keys this row is evidence for. A row may
	// witness more than one (a mapping row is evidence for both the dual-render
	// check and the filter guard it reaches).
	Guards []string
	Group  string
	// Expr is the byte-exact text written into the .baml attribute.
	Expr string
	// Retained is the JinjaExpression BAML evaluates and reports back in
	// Check.Expression. Empty means identical to Expr. It differs only where the
	// attribute lexer doubles backslashes — which is why the native leg is fed
	// this and not Expr, and why a backslash row is compared as ORIGINAL SOURCE
	// BYTES rather than a round-tripped string.
	Retained string
	// StockCheck is the recorded stock envelope at @check level. Every row has
	// one.
	StockCheck envelope
	// StockInner is the NORMALISED INNER stock error — what BAML reports after
	// its own "Failed to evaluate constraints: " prefix, with the source location
	// stripped. It is required exactly for a row whose envelope is
	// envEvaluatorError, and it is what keeps that envelope from being a lossy
	// bucket: an unknown-name witness that silently became a type or arity error
	// would still be "evaluator-error", but it would not be THIS text.
	StockInner string
	// StockAssert is the recorded stock envelope at @assert level, and
	// AssertOmitted says why a row carries no assert instance. Exactly one of
	// the two is set, and an omission is admitted only where stock genuinely
	// cannot be observed at that level (see TestGuardLedgerRowsAreWellFormed).
	StockAssert   envelope
	AssertOmitted string
	// NativeGuard is the guard the native evaluator's refusal is ATTRIBUTED to,
	// matched from the error text by attributeNativeGuard. It is empty exactly
	// when native did not refuse. Pinning it is what makes a removal proof
	// discriminating: a guard that is removed because another one already
	// refuses must show that OTHER guard's name here, before and after.
	NativeGuard string
	// AcceptedAlternative is the spelling BAML DOES accept, for a row whose own
	// spelling it refuses to compile. It is required exactly there, so such a row
	// states what the attribute language requires rather than only what it
	// rejects.
	AcceptedAlternative string
	// Note explains a native decline of an expression stock decided, i.e. the
	// measured cost. Required exactly there (enforced by TestGuardLedgerRowsAreWellFormed).
	Note string
}

// retainedExpr is the source the NATIVE leg is fed, so the differential compares
// two engines over the same bytes rather than comparing BAML's attribute lexer
// against Go's string literals.
func (r guardRow) retainedExpr() string {
	if r.Retained != "" {
		return r.Retained
	}
	return r.Expr
}

// bamlPrelude carries the declarations the group field types reference.
const bamlPrelude = `
class GLProbe {
  b int
  a string
}

class GLInner {
  name int
  tags string[]
}

class GLNest {
  a GLInner
  name string
  rows GLInner[]
}
`

var guardGroups = []guardGroup{
	{Name: "int1", BAMLType: "int", Input: `{"v":1}`, This: debaml.IntValue(1)},
	{Name: "int2", BAMLType: "int", Input: `{"v":2}`, This: debaml.IntValue(2)},
	{Name: "intneg7", BAMLType: "int", Input: `{"v":-7}`, This: debaml.IntValue(-7)},
	// 2^53+1: the exact integer the port's float64 core could not tell from 2^53.
	{Name: "bigint", BAMLType: "int", Input: `{"v":9007199254740993}`, This: debaml.IntValue(9007199254740993)},
	{Name: "f15", BAMLType: "float", Input: `{"v":1.5}`, This: debaml.FloatValue(1.5)},
	// 2^63 as a float: the AsInt hazard, where Go's conversion is
	// implementation-defined and Rust's saturates.
	{Name: "fbig", BAMLType: "float", Input: `{"v":9223372036854775808.0}`, This: debaml.FloatValue(9223372036854775808.0)},
	{Name: "strnum", BAMLType: "string", Input: `{"v":"9007199254740993"}`, This: debaml.StringValue("9007199254740993")},
	{Name: "strab", BAMLType: "string", Input: `{"v":"a b"}`, This: debaml.StringValue("a b")},
	{Name: "strhello", BAMLType: "string", Input: `{"v":"hello"}`, This: debaml.StringValue("hello")},
	{Name: "boolt", BAMLType: "bool", Input: `{"v":true}`, This: debaml.BoolValue(true)},
	{Name: "nullint", BAMLType: "int?", Input: `{"v":null}`, This: debaml.NullValue()},
	{Name: "list123", BAMLType: "int[]", Input: `{"v":[1,2,3]}`, This: debaml.ListValue([]debaml.ConstraintValue{
		debaml.IntValue(1), debaml.IntValue(2), debaml.IntValue(3),
	})},
	// DECLARATION ORDER b,a — deliberately NOT alphabetical, so any observation
	// that reports a,b is reporting a sorted enumeration rather than BAML's
	// insertion order.
	{Name: "probe", BAMLType: "GLProbe", Input: `{"v":{"b":1,"a":"x"}}`, This: debaml.ClassValue("GLProbe", []debaml.ConstraintEntry{
		{Key: "b", Value: debaml.IntValue(1)},
		{Key: "a", Value: debaml.StringValue("x")},
	})},
	{Name: "mapba", BAMLType: "map<string, int>", Input: `{"v":{"b":1,"a":2}}`, This: debaml.MapValue([]debaml.ConstraintEntry{
		{Key: "b", Value: debaml.IntValue(1)},
		{Key: "a", Value: debaml.IntValue(2)},
	})},
	{Name: "nestmap", BAMLType: "map<string, map<string, int>>", Input: `{"v":{"outer":{"b":1,"a":2}}}`, This: debaml.MapValue([]debaml.ConstraintEntry{
		{Key: "outer", Value: debaml.MapValue([]debaml.ConstraintEntry{
			{Key: "b", Value: debaml.IntValue(1)},
			{Key: "a", Value: debaml.IntValue(2)},
		})},
	})},
	// The inner and outer classes both declare `name` and hold DIFFERENT kinds,
	// so a chain resolved against the root rather than against the value it
	// reaches is observable rather than merely suspected.
	{Name: "nest", BAMLType: "GLNest", Input: `{"v":{"a":{"name":5,"tags":["x","y"]},"name":"x","rows":[{"name":7,"tags":["z"]}]}}`,
		This: debaml.ClassValue("GLNest", []debaml.ConstraintEntry{
			{Key: "a", Value: debaml.ClassValue("GLInner", []debaml.ConstraintEntry{
				{Key: "name", Value: debaml.IntValue(5)},
				{Key: "tags", Value: debaml.ListValue([]debaml.ConstraintValue{debaml.StringValue("x"), debaml.StringValue("y")})},
			})},
			{Key: "name", Value: debaml.StringValue("x")},
			{Key: "rows", Value: debaml.ListValue([]debaml.ConstraintValue{
				debaml.ClassValue("GLInner", []debaml.ConstraintEntry{
					{Key: "name", Value: debaml.IntValue(7)},
					{Key: "tags", Value: debaml.ListValue([]debaml.ConstraintValue{debaml.StringValue("z")})},
				}),
			})},
		})},
}
