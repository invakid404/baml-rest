//go:build integration

package debaml

// The comparison ENVELOPE both legs are rendered into.
//
// The oracle compares far more than JSON value equality. Per fixture it records,
// from each leg independently:
//
//	identity   the canonical value in ONE shared vocabulary — class and enum
//	           NAMES, field/entry ORDER, and every leaf typed
//	           (class:Hand{suit=enum:Suit=Hearts,bid=int:2}). Two documents that
//	           marshal to the same JSON but disagree about which class they are,
//	           or in which order the fields sit, are NOT equal here.
//	json       the serialized document, compared with the EXACT big.Rat
//	           comparator (constraintStateValueDiff), never float64.
//	sites      every constraint that RAN, in traversal-then-declaration order,
//	           with its path, level, label and exact expression text, and its
//	           result. Labels are never folded, so a duplicate label is two
//	           ordered sites rather than one.
//	kind       value / assertion-failure / evaluator-error / coercion-error /
//	           process-fatal. These are distinct outcomes and are never collapsed:
//	           a false @check is DATA in an emitted value, a false @assert REJECTS
//	           the node, and a predicate that could not be evaluated is neither.
//
// The stock envelope is read from the raw CFFI value tree rather than from a
// generated client's decoded structs, because the generated readback is LOSSY in
// exactly the place this slice has to measure: baml_go's decodeCheckedValue folds
// Checked.Checks into a map[string]Check, so two @check attributes sharing a label
// collapse to one and their order is gone. Reading the protobuf directly keeps the
// ordered []CFFICheckValue stock actually produced. TestServingOracleDuplicateLabel
// pins BOTH observations — the ordered pair and the fold — so the lossiness is a
// recorded property of stock's Go binding rather than a limitation this oracle
// papers over.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/boundaryml/baml/engine/language_client_go/pkg/cffi"

	"github.com/invakid404/baml-rest/internal/schema"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// servingOracleFixture is one serving-shaped row: an in-memory .baml function,
// the raw assistant text, and the RECORDED envelope of each leg.
//
// Both pins are recordings read off the live legs (see the recording mode in
// [TestServingOracleDifferential]), not predictions, and both are re-verified on
// every run — so a change on either side has to be acknowledged rather than
// absorbed, and the corpus cannot be edited into agreement with itself.
type servingOracleFixture struct {
	// Name is unique and is the BAML function name once prefixed.
	Name string
	// Family is the shape family this fixture belongs to. Every family in
	// [servingOracleGateFamilies] must have at least one fixture, and every
	// fixture's family must be in that list — the two directions are what keep the
	// non-integration gate test (serving_oracle_gate_test.go, which drives
	// checkSupported/checkSupportedFields/checkSupportedType by name) covering the
	// same shapes the oracle drives.
	Family string
	// Doc says what this row is evidence FOR. Required.
	Doc string
	// Bundle is the native schema. The .baml is rendered from it.
	Bundle *schema.Bundle
	// Raw is the assistant text handed to BOTH legs.
	Raw string
	// Stock is the RECORDED stock envelope, rendered by soStockEnvelope.render.
	Stock string
	// Native is the RECORDED native envelope, rendered by soNativeEnvelope.render.
	Native string
	// Fatal marks a row stock cannot be asked in-process (it aborts or hangs the
	// CFFI). Such a row is driven ONLY from an isolated subprocess and never
	// contributes a fabricated boolean. See serving_oracle_fatal_test.go.
	Fatal bool
	// Unconstrained marks a CONTROL fixture that carries no constraint anywhere.
	// The boundary lock requires it to be ADMITTED, which is what makes the
	// decline of every other fixture constraint-specific rather than a blanket
	// refusal of the corpus.
	Unconstrained bool
	// Served marks a CONSTRAINT-BEARING fixture the de-BAML Slice 7.2b-3 cutover
	// ADMITS: the one production fingerprint, on the static unary /call route.
	//
	// It is the per-row expected disposition the scope replaced the blanket
	// "every constraint-bearing fixture declines" lock with. Exactly the companion
	// rows carry it — four under 7.2b-3, and 24 since the Slice 7.2c-3 predicate
	// widening (TestServingOracleBoundaryLock cross-checks the count against
	// soCompanionRowNames, so a further row cannot acquire it quietly) — and every
	// other constraint-bearing row keeps its existing decline.
	//
	// Served does NOT mean "admitted everywhere". EVERY named SCHEMA gate admits these
	// rows — the three generic ones included, since the cutover they consult the same
	// fingerprint — while the direct parse endpoints and the /stream admission predicate
	// keep declining them as a ROUTE decision. That split is the scope's `/call`-only
	// boundary, and soRequireServed drives both halves.
	Served bool
	// Divergence explains, in one sentence, why this row does NOT land in an
	// agreement bucket: a predicate native refuses to evaluate, a coercion native
	// declines outright, a value the two legs canonicalize differently, or a check
	// collection whose SHAPE differs. It is required for every non-agreeing bucket
	// and forbidden for an agreeing one (TestServingOracleAgreementTally asserts
	// both directions), so a cost can neither appear nor disappear silently.
	//
	// It is never an allowance to SERVE. Every such row is constraint-bearing, so
	// the production gate declines the bundle and native's value never reaches a
	// caller — which TestServingOracleBoundaryLock asserts for every row
	// independently of this field.
	Divergence string
	// Project names the ISOLATED in-memory .baml project this fixture belongs to.
	// Empty is the shared main project, which is where all but a handful of rows sit.
	//
	// De-BAML Slice 7.2c-3 introduced it, and for a reason the 7.2c scope names
	// directly: the cutover admits SIX predicate variants of the SAME two name-pinned
	// classes, and one BAML project cannot declare `StaticCheckedAnswer` six times.
	// The scope forbids the obvious workaround — renaming the classes — because a
	// renamed class is a different family with no capture behind it, and the live
	// fixture already carries a `StaticGtePredicateAnswer` sibling that must stay
	// DECLINED precisely so the name pin means something.
	//
	// So each extra operator gets its own project, each declaring the two pinned names
	// ONCE, exactly as internal/debaml/predicatewire's 32 isolated projects do. The
	// runtimes coexist in one process (7.2c-1 measured that first, with a throwaway
	// prototype, before any fixture was written) and every project is rendered,
	// golden-pinned and hashed independently.
	Project string
}

// method is the generated BAML function this fixture is driven through.
func (f servingOracleFixture) method() string { return soFunctionName(f.Name) }

// project is the isolated project this fixture belongs to, with the empty default
// resolved to the shared main project's name.
func (f servingOracleFixture) project() string {
	if f.Project == "" {
		return soMainProject
	}
	return f.Project
}

// source names the .baml the method lives in, for the per-case report.
func (f servingOracleFixture) source() string {
	return soProjectFileFor(f.project()) + ":" + f.method()
}

// ---------------------------------------------------------------------------
// Stock envelope
// ---------------------------------------------------------------------------

// soStockKind is what the stock leg DID, as the caller observes it.
type soStockKind string

const (
	// soStockValue: stock emitted a value. Any @check results ride inside it as
	// data; a false @check does NOT prevent this.
	soStockValue soStockKind = "value"
	// soStockAssertFailed: an @assert predicate rendered false, so stock REJECTED
	// the node ("Assertions failed."). There is no value.
	soStockAssertFailed soStockKind = "assertion-failure"
	// soStockEvaluatorError: a predicate failed to compile/evaluate, or did not
	// render exactly true/false. Stock rejects the node. It is NOT a failed check
	// and must never be mapped onto one.
	soStockEvaluatorError soStockKind = "evaluator-error"
	// soStockCoercionError: stock could not coerce the text into the target type at
	// all — a parse/coercion failure with no constraint involved.
	soStockCoercionError soStockKind = "coercion-error"
	// soStockProcessFatal: stock cannot be observed in-process (it aborts or
	// hangs). Recorded from a subprocess and NEVER converted into a boolean.
	soStockProcessFatal soStockKind = "process-fatal"
	// soStockUnrecognisedError: stock returned an error whose reason chain matches
	// NO known shape. It is a HARNESS FAILURE, not a bucket: reading a new error
	// class as an ordinary coercion failure would quietly fold an unknown stock
	// behaviour into a known one, which is the opposite of what the envelope
	// vocabulary is for. Every test treats it as fatal.
	soStockUnrecognisedError soStockKind = "unrecognised-stock-error"
)

// soKnownStockErrorShapes are the reason fragments that classify a stock error.
// The list is asserted non-empty and each entry is required to be matched by at
// least one corpus row, so a shape can neither be added without a witness nor
// removed while a row still depends on it.
var soKnownStockErrorShapes = []struct {
	Fragment string
	Kind     soStockKind
}{
	{"Failed to evaluate constraints:", soStockEvaluatorError},
	{"Assertions failed.", soStockAssertFailed},
	{"Failed while parsing required fields:", soStockCoercionError},
	{"Expected ", soStockCoercionError},
}

// soStockSite is one constraint stock evaluated, at the position it ran.
type soStockSite struct {
	Path       string
	Label      string
	Expression string
	// Status is stock's own word: "succeeded" or "failed". It is kept verbatim
	// rather than mapped to a bool so an unrecognised third status cannot be
	// silently read as one of the two.
	Status string
	// Certified says whether this site was READ FROM THE RAW CFFI TREE.
	//
	// A certified site carries stock's own order and multiplicity. An UNCERTIFIED one
	// was recovered from a root check collection baml_go had already folded into a
	// map[string]shared.Check, where order and multiplicity no longer exist — see
	// soChecked. The distinction is carried all the way into the rendered envelope
	// and into the comparator, which does not claim an ordering it cannot observe.
	Certified bool
}

func (s soStockSite) render() string {
	out := fmt.Sprintf("%s|%s|%s=%s", s.Path, strconv.Quote(s.Label), s.Expression, s.Status)
	if !s.Certified {
		// The marker is part of the PINNED envelope, so a row whose root evidence is
		// uncertified says so in the corpus rather than only in a comment.
		out += "~uncertified-order"
	}
	return out
}

// soStockEnvelope is one stock observation.
type soStockEnvelope struct {
	Kind     soStockKind
	Identity string
	JSON     string
	Sites    []soStockSite
	// Reasons is the ordered `reason:` chain of stock's ParsingError, unquoted.
	// It is what turns "an error" into an exact observation.
	Reasons []string
}

func (e soStockEnvelope) render() string {
	switch e.Kind {
	case soStockValue:
		parts := make([]string, len(e.Sites))
		for i, s := range e.Sites {
			parts[i] = s.render()
		}
		return fmt.Sprintf("value %s checks=[%s]", e.Identity, strings.Join(parts, " "))
	case soStockProcessFatal:
		return "process-fatal " + strings.Join(e.Reasons, " | ")
	default:
		return string(e.Kind) + " " + strings.Join(e.Reasons, " | ")
	}
}

// soReasonRe matches the `reason: "..."` fields of BAML's ParsingError Debug
// rendering, which is what the CFFI hands back as the error string.
var soReasonRe = regexp.MustCompile(`reason: ("(?:[^"\\]|\\.)*")`)

// soClassifyStockError turns a stock error into a kind plus its exact reason
// chain.
//
// The classification is driven by stock's OWN words and has no default bucket: an
// error whose reasons match none of the known shapes is a coercion error, which is
// the conservative reading (it claims nothing about constraints), and the reasons
// are pinned either way so a re-classified error cannot pass unnoticed.
func soClassifyStockError(err error) soStockEnvelope {
	msg := err.Error()
	var reasons []string
	for _, m := range soReasonRe.FindAllStringSubmatch(msg, -1) {
		unq, uerr := strconv.Unquote(m[1])
		if uerr != nil {
			// Keep the raw form rather than dropping it: a reason we cannot unquote
			// is still evidence, and silently skipping it would shorten the chain the
			// pin compares.
			unq = m[1]
		}
		reasons = append(reasons, unq)
	}
	if len(reasons) == 0 {
		reasons = []string{soCollapse(msg)}
	}
	// NO DEFAULT BUCKET, and that is load-bearing. An error shape this vocabulary
	// does not recognise is reported as [soStockUnrecognisedError], which every
	// caller treats as a harness failure; defaulting it to a coercion error would
	// silently absorb a new stock behaviour into a known one.
	kind := soStockUnrecognisedError
	for _, r := range reasons {
		for _, shape := range soKnownStockErrorShapes {
			if !strings.Contains(r, shape.Fragment) {
				continue
			}
			// Precedence: an evaluator failure outranks the assertion/parse wrappers
			// BAML nests it inside, and an assertion failure outranks the generic
			// required-fields wrapper. Without it the outermost wrapper would win and
			// every nested failure would read as a plain coercion error.
			if kind == soStockUnrecognisedError ||
				soStockKindRank(shape.Kind) > soStockKindRank(kind) {
				kind = shape.Kind
			}
		}
	}
	out := make([]string, len(reasons))
	for i, r := range reasons {
		out[i] = soCollapse(r)
	}
	return soStockEnvelope{Kind: kind, Reasons: out}
}

// soStockKindRank orders the error kinds by specificity, so the innermost
// (most specific) reason decides the classification rather than the outermost
// wrapper BAML happens to nest it in.
func soStockKindRank(k soStockKind) int {
	switch k {
	case soStockEvaluatorError:
		return 3
	case soStockAssertFailed:
		return 2
	case soStockCoercionError:
		return 1
	}
	return 0
}

// soCollapse folds a multi-line message onto one line so a pinned envelope stays
// comparable for EQUALITY. Only the framing is touched.
func soCollapse(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " / ")
	return strings.Join(strings.Fields(s), " ")
}

// soFailedAssert extracts the (label, expression) pairs stock names in a
// "Failed: <label> <expression>" reason, which is how it reports WHICH assertion
// rejected the node.
func soFailedAssert(reasons []string) []string {
	var out []string
	for _, r := range reasons {
		if rest, ok := strings.CutPrefix(r, "Failed: "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reading the raw CFFI value
// ---------------------------------------------------------------------------

// soReadStockValue walks the raw CFFI value tree, producing the shared-vocabulary
// identity, the serialized document and every constraint site in traversal order.
//
// It walks the PROTOBUF rather than a decoded struct on purpose (see the file
// comment): the ordered []CFFICheckValue is only available here.
func soReadStockValue(h *cffi.CFFIValueHolder, path string, sites *[]soStockSite) (identity string, doc json.RawMessage, err error) {
	if h == nil {
		return "", nil, fmt.Errorf("%s: nil value holder", path)
	}
	switch v := h.GetValue().(type) {
	case *cffi.CFFIValueHolder_CheckedValue:
		c := v.CheckedValue
		if c == nil {
			return "", nil, fmt.Errorf("%s: nil checked value", path)
		}
		for _, chk := range c.Checks {
			if chk == nil {
				return "", nil, fmt.Errorf("%s: nil check entry", path)
			}
			*sites = append(*sites, soStockSite{
				Path: path, Label: chk.Name, Expression: chk.Expression, Status: chk.Status,
				// Read from the raw protobuf: stock's own order and multiplicity.
				Certified: true,
			})
		}
		// The Checked wrapper is transparent to the VALUE: its checks are recorded
		// above and the identity is the value they ran against.
		return soReadStockValue(c.Value, path, sites)

	case *cffi.CFFIValueHolder_UnionVariantValue:
		u := v.UnionVariantValue
		if u == nil {
			return "", nil, fmt.Errorf("%s: nil union variant", path)
		}
		// Transparent as well. Which ARM won is observable in the identity of the
		// value itself (an int arm renders int:…, a string arm string:…), and the
		// native side records the arm index separately.
		return soReadStockValue(u.Value, path, sites)

	case *cffi.CFFIValueHolder_NullValue:
		return "null", json.RawMessage("null"), nil

	case *cffi.CFFIValueHolder_BoolValue:
		return "bool:" + strconv.FormatBool(v.BoolValue), json.RawMessage(strconv.FormatBool(v.BoolValue)), nil

	case *cffi.CFFIValueHolder_IntValue:
		s := strconv.FormatInt(v.IntValue, 10)
		return "int:" + s, json.RawMessage(s), nil

	case *cffi.CFFIValueHolder_FloatValue:
		s := strconv.FormatFloat(v.FloatValue, 'g', -1, 64)
		return "float:" + s, json.RawMessage(s), nil

	case *cffi.CFFIValueHolder_StringValue:
		q := strconv.Quote(v.StringValue)
		enc, merr := json.Marshal(v.StringValue)
		if merr != nil {
			return "", nil, fmt.Errorf("%s: marshal string: %w", path, merr)
		}
		return "string:" + q, enc, nil

	case *cffi.CFFIValueHolder_EnumValue:
		e := v.EnumValue
		if e == nil || e.Name == nil {
			return "", nil, fmt.Errorf("%s: nil enum value", path)
		}
		enc, merr := json.Marshal(e.Value)
		if merr != nil {
			return "", nil, fmt.Errorf("%s: marshal enum: %w", path, merr)
		}
		return "enum:" + e.Name.Name + "=" + e.Value, enc, nil

	case *cffi.CFFIValueHolder_ListValue:
		l := v.ListValue
		if l == nil {
			return "", nil, fmt.Errorf("%s: nil list value", path)
		}
		ids := make([]string, len(l.Items))
		docs := make([]string, len(l.Items))
		for i, it := range l.Items {
			id, d, ierr := soReadStockValue(it, fmt.Sprintf("%s[%d]", path, i), sites)
			if ierr != nil {
				return "", nil, ierr
			}
			ids[i], docs[i] = id, string(d)
		}
		return "list[" + strings.Join(ids, ",") + "]", json.RawMessage("[" + strings.Join(docs, ",") + "]"), nil

	case *cffi.CFFIValueHolder_MapValue:
		m := v.MapValue
		if m == nil {
			return "", nil, fmt.Errorf("%s: nil map value", path)
		}
		ids := make([]string, len(m.Entries))
		docs := make([]string, len(m.Entries))
		seen := map[string]bool{}
		for i, e := range m.Entries {
			if e == nil {
				return "", nil, fmt.Errorf("%s: nil map entry", path)
			}
			if seen[e.Key] {
				// A duplicate key would make the document depend on which entry a
				// decoder kept. Refuse rather than record one of them.
				return "", nil, fmt.Errorf("%s: duplicate map key %q in stock readback", path, e.Key)
			}
			seen[e.Key] = true
			id, d, ierr := soReadStockValue(e.Value, fmt.Sprintf("%s[%q]", path, e.Key), sites)
			if ierr != nil {
				return "", nil, ierr
			}
			kq, merr := json.Marshal(e.Key)
			if merr != nil {
				return "", nil, fmt.Errorf("%s: marshal map key: %w", path, merr)
			}
			ids[i] = e.Key + "=" + id
			docs[i] = string(kq) + ":" + string(d)
		}
		return "map{" + strings.Join(ids, ",") + "}", json.RawMessage("{" + strings.Join(docs, ",") + "}"), nil

	case *cffi.CFFIValueHolder_ClassValue:
		c := v.ClassValue
		if c == nil || c.Name == nil {
			return "", nil, fmt.Errorf("%s: nil class value", path)
		}
		ids := make([]string, len(c.Fields))
		docs := make([]string, len(c.Fields))
		seen := map[string]bool{}
		for i, f := range c.Fields {
			if f == nil {
				return "", nil, fmt.Errorf("%s: nil class field", path)
			}
			if seen[f.Key] {
				return "", nil, fmt.Errorf("%s: duplicate field %q in stock readback", path, f.Key)
			}
			seen[f.Key] = true
			id, d, ierr := soReadStockValue(f.Value, path+"."+f.Key, sites)
			if ierr != nil {
				return "", nil, ierr
			}
			kq, merr := json.Marshal(f.Key)
			if merr != nil {
				return "", nil, fmt.Errorf("%s: marshal field key: %w", path, merr)
			}
			ids[i] = f.Key + "=" + id
			docs[i] = string(kq) + ":" + string(d)
		}
		return "class:" + c.Name.Name + "{" + strings.Join(ids, ",") + "}",
			json.RawMessage("{" + strings.Join(docs, ",") + "}"), nil
	}
	// Every remaining CFFI variant (literal, media/raw object, streaming state) is
	// outside what a non-streaming constraint fixture can produce. Refusing is the
	// point: a silently rendered "unknown" would let an unmodelled shape compare
	// equal to itself.
	return "", nil, fmt.Errorf("%s: unmodelled CFFI value %T", path, h.GetValue())
}

// ---------------------------------------------------------------------------
// Native envelope
// ---------------------------------------------------------------------------

// soNativeKind is what the NATIVE leg did.
type soNativeKind string

const (
	// soNativeValue: coercion produced a canonical value and every attached
	// predicate that ran produced a boolean.
	soNativeValue soNativeKind = "value"
	// soNativeAssertFailed: an @assert-level event evaluated FALSE. Native does not
	// act on it (it declines the whole bundle at admission), but the state records
	// it, and it is what stock's "Assertions failed." must line up with.
	soNativeAssertFailed soNativeKind = "assertion-failure"
	// soNativeUnsupported: at least one predicate was refused with
	// ErrConstraintUnsupported. Native declined to decide.
	soNativeUnsupported soNativeKind = "evaluator-unsupported"
	// soNativeCoercionError: production coerce refused the value.
	soNativeCoercionError soNativeKind = "coercion-error"
	// soNativeNoCandidate: extraction found no cleanly-claimable candidate, which
	// is a DECLINE in the serving path rather than a parse result.
	soNativeNoCandidate soNativeKind = "no-candidate"
	// soNativeUnmodelled: the collector refuses to model this coercion shape.
	soNativeUnmodelled soNativeKind = "collector-unmodelled"
	// soNativeCollectorDiverged: the collector's traversal did not serialize to the
	// document production coerce produced, so it REFUSED to report a state. It is
	// distinct from a coercion error: production coerced the value fine, and it is
	// the test-only collector that could not mirror it.
	soNativeCollectorDiverged soNativeKind = "collector-diverged"
)

// soNativeSite is one predicate the native collector ran, at the node it ran on.
type soNativeSite struct {
	Path       string
	Origin     constraintStateOrigin
	Level      schema.ConstraintLevel
	Labeled    bool
	Label      string
	Expression string
	Outcome    constraintStateOutcome
}

func (s soNativeSite) render() string {
	label := "-"
	if s.Labeled {
		label = strconv.Quote(s.Label)
	}
	return fmt.Sprintf("%s|%s/%s/%s/%s=%s", s.Path, s.Origin, s.Level, label, s.Expression, s.Outcome)
}

// soNativeSkip is one predicate that did NOT run, with the counterfactual the
// collector recorded — positive evidence that the predicate was reached rather
// than merely absent.
type soNativeSkip struct {
	Path   string
	Detail string
}

func (s soNativeSkip) render() string { return s.Path + "|" + s.Detail }

// soNativeEnvelope is one native observation.
type soNativeEnvelope struct {
	Kind     soNativeKind
	Identity string
	JSON     string
	Sites    []soNativeSite
	Skips    []soNativeSkip
	// Support is checkSupported(bundle) VERBATIM — recorded on every row, never
	// acted on, so a fixture cannot quietly become admitted.
	Support error
	// Err is the coercion/collector error itself, retained so the contract can tell
	// a production DECLINE (the ErrDeBAMLParseUnsupported sentinel — fail-closed by
	// construction) from a collector refusal or a real failure, rather than reading
	// all three off one message string.
	Err     error
	Message string
}

func (e soNativeEnvelope) render() string {
	switch e.Kind {
	case soNativeCoercionError, soNativeNoCandidate, soNativeUnmodelled, soNativeCollectorDiverged:
		return string(e.Kind) + " " + e.Message
	}
	sites := make([]string, len(e.Sites))
	for i, s := range e.Sites {
		sites[i] = s.render()
	}
	skips := make([]string, len(e.Skips))
	for i, s := range e.Skips {
		skips[i] = s.render()
	}
	out := fmt.Sprintf("%s %s events=[%s]", e.Kind, e.Identity, strings.Join(sites, " "))
	if len(skips) > 0 {
		out += " skipped=[" + strings.Join(skips, " ") + "]"
	}
	return out
}

// soRunNative drives the NATIVE leg end to end, exactly as the static serving path
// does: strip JSONish comments, extract the cleanly-claimable candidate, then
// coerce and evaluate through the test-only coercion-state collector.
//
// The extraction half is production's own (stripJSONComments +
// extractCandidateMode + bundleNumMode — the same three calls ParseStaticBundle
// makes), so a fixture whose raw text is fenced or commented exercises the real
// front half rather than a decoded shortcut.
func soRunNative(f servingOracleFixture) soNativeEnvelope {
	support := checkSupported(f.Bundle)
	in, ok := extractCandidateMode(stripJSONComments(f.Raw), bundleNumMode(f.Bundle))
	if !ok {
		return soNativeEnvelope{Kind: soNativeNoCandidate, Support: support,
			Message: "no cleanly-claimable JSON candidate"}
	}
	c := &constraintStateCollector{bundle: f.Bundle}
	root, err := c.node(f.Bundle.Target, in, nil, constraintStatePath{{Kind: constraintPathRoot}}, false, true)
	if err != nil {
		kind := soNativeCoercionError
		switch {
		case soIsUnmodelled(err):
			kind = soNativeUnmodelled
		case strings.Contains(err.Error(), errConstraintStateDiverged.Error()):
			kind = soNativeCollectorDiverged
		}
		return soNativeEnvelope{Kind: kind, Support: support, Err: err, Message: soCollapse(err.Error())}
	}

	env := soNativeEnvelope{Kind: soNativeValue, Support: support,
		Identity: constraintStateDescribe(root.Canonical), JSON: string(root.CanonicalJSON)}
	root.walk(func(n *constraintCoercionState) {
		p := n.Path.String()
		for _, ev := range n.Events {
			env.Sites = append(env.Sites, soNativeSite{
				Path: p, Origin: ev.Origin, Level: ev.Level, Labeled: ev.Labeled,
				Label: ev.Label, Expression: ev.Expression, Outcome: ev.Outcome,
			})
		}
		for _, sk := range n.Skipped {
			env.Skips = append(env.Skips, soNativeSkip{Path: p, Detail: sk.describe()})
		}
		if n.Disposition == constraintDispositionSkippedPath || n.Disposition == constraintDispositionPolicyDeclined {
			env.Skips = append(env.Skips, soNativeSkip{Path: p, Detail: "node:" + string(n.Disposition) + ":" + n.SkipReason})
		}
	})
	for _, s := range env.Sites {
		if s.Outcome == constraintOutcomeUnsupported {
			env.Kind = soNativeUnsupported
		}
	}
	if env.Kind == soNativeValue {
		for _, s := range env.Sites {
			if s.Level == schema.ConstraintAssert && s.Outcome == constraintOutcomeFalse {
				env.Kind = soNativeAssertFailed
			}
		}
	}
	return env
}

// soIsUnmodelled keeps the collector's "I refuse to model this shape" refusal
// distinguishable from a real coercion failure. Folding the two would let an
// unmodelled shape be reported as stock-disagreement, or worse, as agreement.
func soIsUnmodelled(err error) bool {
	return strings.Contains(err.Error(), errConstraintStateUnmodelled.Error())
}

// ---------------------------------------------------------------------------
// Path alignment between the two legs
// ---------------------------------------------------------------------------

// soUnionArmRe matches the collector's union-arm path segment.
var soUnionArmRe = regexp.MustCompile(`\|arm[0-9]+`)

// soAlignNativePath maps a native constraint-state path onto the path the STOCK
// readback reports for the same node.
//
// ONE normalisation, and it is derived rather than assumed. The collector indexes
// list elements by their INPUT position, so an element BAML dropped keeps the index
// it arrived at while stock's emitted list has closed the gap. The shift is computed
// from the collector's OWN skipped-path records, so it comes from the same run.
//
// There is deliberately NO union-arm normalisation. The collector renders a winning
// union arm as `|armN`, which stock's tree does not carry — but production coerce
// declines every constrained union in this corpus (at a class field AND at the
// return type), so no such path is ever produced and a normalisation for it would
// be dead code that silently loosened the comparison.
// TestServingOracleNoUnionArmIsCoerced asserts that absence positively, with the
// decline as its evidence, so a future change that admits constrained unions fails
// here and forces the alignment to be re-derived with a fixture behind it.
func soAlignNativePath(path string, dropsByPrefix map[string][]int) string {
	return soShiftDroppedIndexes(path, dropsByPrefix)
}

// soIndexRe matches the FIRST list index segment of a path, splitting it into the
// prefix, the index and the remaining tail. soShiftDroppedIndexes walks a path
// left to right with it.
var soIndexRe = regexp.MustCompile(`^(.*?)\[([0-9]+)\](.*)$`)

// soLastIndexRe matches a path that ENDS in a list index, splitting it into the
// owning list's full path and that index.
//
// The producer keys drops by the owning list, which for a NESTED element is
// `$.v[2].w` rather than `$.v` — the greedy prefix and the end anchor are what
// make that so. Matching the first index instead (and discarding anything with a
// remaining suffix, as this did) meant a skip at `$.v[2].w[1]` produced no key at
// all, so the consumer's nested support could only ever be exercised by a
// hand-built map.
var soLastIndexRe = regexp.MustCompile(`^(.*)\[([0-9]+)\]$`)

// soShiftDroppedIndexes rewrites every `[i]` segment of path into the index the
// element has in stock's EMITTED list, given the input indexes dropped under each
// list prefix.
//
// TWO COORDINATE SYSTEMS, tracked separately. The collector's paths — and therefore
// the keys of dropsByPrefix — are in INPUT coordinates; the rendered result is in
// stock's EMITTED coordinates. Once an outer index has been shifted, the emitted
// rendering is no longer a valid key: a nested list under an outer element whose
// earlier sibling was dropped would look up `$.v[1].w` when the collector recorded
// its drops under `$.v[2].w`, miss, and leave the inner index unshifted.
//
// So the input-coordinate prefix is carried alongside the emitted one and is what
// the map is queried with.
func soShiftDroppedIndexes(path string, dropsByPrefix map[string][]int) string {
	if len(dropsByPrefix) == 0 {
		return path
	}
	var emitted strings.Builder // what is returned: stock's coordinates
	var input strings.Builder   // what the drop map is keyed by: the collector's
	rest := path
	for {
		m := soIndexRe.FindStringSubmatch(rest)
		if m == nil {
			emitted.WriteString(rest)
			return emitted.String()
		}
		prefix, idxText, tail := m[1], m[2], m[3]
		idx, err := strconv.Atoi(idxText)
		if err != nil {
			// Unreachable: the regexp only matches digits.
			emitted.WriteString(rest)
			return emitted.String()
		}
		shift := 0
		for _, d := range dropsByPrefix[input.String()+prefix] {
			if d < idx {
				shift++
			}
		}
		fmt.Fprintf(&emitted, "%s[%d]", prefix, idx-shift)
		fmt.Fprintf(&input, "%s[%d]", prefix, idx)
		rest = tail
	}
}

// soDropsByPrefix reads the collector's skipped LIST ELEMENT paths out of a native
// envelope, keyed by the owning list's path. Only node-level skips are counted: a
// per-predicate skip (a bare-string return, say) does not remove an element from
// the emitted list.
func soDropsByPrefix(env soNativeEnvelope) map[string][]int {
	out := map[string][]int{}
	for _, s := range env.Skips {
		if !strings.HasPrefix(s.Detail, "node:") {
			continue
		}
		// Key by the OWNING LIST, which is everything up to the final index. A
		// nested element's owner is itself an indexed path, and that is exactly the
		// key soShiftDroppedIndexes builds as it walks.
		m := soLastIndexRe.FindStringSubmatch(s.Path)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out[m[1]] = append(out[m[1]], idx)
	}
	return out
}
