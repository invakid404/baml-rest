//go:build integration

package profileoracle

// Stock BAML v0.223.0 differential for the de-BAML TARGET-LEVEL constraint gate.
//
// WHAT IT PROVES, and why it lives here rather than in internal/debaml.
// internal/debaml's support gate now declines any bundle carrying a constraint on
// `b.Target` (checkSupportedFields -> checkTypeNoConstraints). That gate closed a
// real over-claim, but "over-claim" is a statement about what STOCK does, and no
// unit test in internal/debaml can make it: the native constraint evaluator's
// verdict is not stock's parse verdict. This package already links the untouched
// v0.223 runtime through CFFI and parses through CallFunctionParse — BAML's real
// coercer, run_user_checks -> evaluate_predicate -> validate_asserts — so the
// stock half is MEASURED here and the native half is asserted beside it.
//
// THE MEASUREMENT CAME FIRST, AND IT CORRECTED THE STORY. A target-level
// constraint does not have one stock behaviour, it has three, and only the first
// is an out-claim of the kind "native served where BAML raises":
//
//   - RAISES — a bare `int` return and a LIST-LEVEL constraint both reject the
//     parse with "Assertions failed.". Native served the value. That is the
//     out-claim the gate removes.
//   - SERVES A DIFFERENT VALUE — a constrained list ELEMENT does NOT reject.
//     coerce_array DROPS each element that fails, so stock returns `[]` where
//     native returned `[1,2]`. Still an out-claim, in value rather than in
//     outcome.
//   - SERVES THE SAME VALUE — a bare `string` return skips constraints entirely
//     (measured by [TestStockSkipsConstraintsOnBareStringReturn]), so native's
//     served value MATCHED stock's. Declining it is NOT an out-claim fix; it is
//     plain over-decline, which the parity principle permits and which this test
//     records as such rather than dressing up.
//
// Writing the bare-string row as "stock would have raised" would contradict this
// package's own measurement, so each row states its measured disposition and the
// assertions follow from it.

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// stockTargetDisposition is what stock v0.223 does with a target-level
// constraint, as measured by this test.
type stockTargetDisposition string

const (
	// stockRaises: the parse is rejected — "Assertions failed."
	stockRaises stockTargetDisposition = "raises"
	// stockServes: the parse succeeds; wantServed is the document it yields.
	stockServes stockTargetDisposition = "serves"
)

// targetConstraintCase pairs ONE .baml return type carrying a target-level
// constraint with the internal/schema Bundle that lowers to the same shape, so
// the stock leg and the native leg are talking about the same schema.
//
// bundle is hand-built rather than lowered from the source: BAML source ->
// Bundle is the introspection pipeline, not something a differential should
// depend on. The correspondence is instead held by the SHAPE assertions below
// (each case pins its target Kind and the exact constraints the Bundle declares)
// plus the served-value comparison for the stockServes rows — where native's own
// coercion of the same raw is compared against the bytes stock actually returned.
type targetConstraintCase struct {
	name string
	// returnType is spliced into `function <fn>() -> <returnType>`.
	returnType string
	// bareReturnType is the SAME shape with the constraint removed. It gives the
	// stock leg a constraint-free CONTROL for the same raw, which is what lets a
	// divergence be ATTRIBUTED to the constraint instead of merely observed —
	// see the three-way rule in [classifyTargetConstraintRow].
	bareReturnType string
	// decls is extra BAML declared before the function (a type alias, when the
	// constraint sits on a list ELEMENT — BAML has no inline spelling for that).
	decls string
	// raw is the response text handed to CallFunctionParse and to native.
	raw string

	disposition stockTargetDisposition
	// wantServedJSON is stock's parsed value, JSON-encoded, for a stockServes
	// row. JSON and not fmt %v: %v is a lossy Go rendering that cannot be
	// compared with native's JSON without discarding delimiters, and discarding
	// them is what would let `[1 2]` and `[12]` collapse into each other.
	wantServedJSON string
	// wantBareServedJSON is the constraint-free control's value, JSON-encoded.
	wantBareServedJSON string

	// bundle is the native-side equivalent, and wantTargetKind pins that it is
	// the shape the return type spells.
	bundle         func() *schema.Bundle
	wantTargetKind schema.TypeKind
	// wantNativeWouldServe is what native's coercer produces for raw — the value
	// native served while the gate admitted this bundle. It is not trusted: the
	// test DERIVES it by running ParseStaticBundle over the constraint-stripped
	// twin of bundle (native's coercer never reads Meta.Constraints, so the twin
	// coerces identically to what the constrained bundle served while admitted)
	// and requires the two to agree.
	wantNativeWouldServe string
	// outClaim records whether native's admitted behaviour diverged from stock
	// BECAUSE OF THE CONSTRAINT. Re-derived at runtime from the three measured
	// values, so a row cannot claim an out-claim its own numbers do not show.
	outClaim bool
}

func targetConstraintCases() []targetConstraintCase {
	strT := func() schema.Type {
		return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	}
	intT := func() schema.Type {
		return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	}
	assert := func(t schema.Type, label, expr string) schema.Type {
		t.Meta.Constraints = append(append([]schema.Constraint(nil), t.Meta.Constraints...),
			schema.Constraint{Level: schema.ConstraintAssert, Expression: expr, Label: &label})
		return t
	}
	ptr := func(t schema.Type) *schema.Type { return &t }
	bundle := func(target schema.Type) func() *schema.Bundle {
		return func() *schema.Bundle { return &schema.Bundle{Target: target} }
	}

	return []targetConstraintCase{
		{
			// The headline row: a top-level @assert stock genuinely FAILS. Native
			// served `5`; stock rejects the parse. Declining restores the BAML
			// fallback that raises.
			name:                 "bare-int-return-assert",
			returnType:           "int @assert(f, {{ false }})",
			bareReturnType:       "int",
			raw:                  "5",
			disposition:          stockRaises,
			wantBareServedJSON:   "5",
			bundle:               bundle(assert(intT(), "f", "false")),
			wantTargetKind:       schema.TypePrimitive,
			wantNativeWouldServe: "5",
			outClaim:             true,
		},
		{
			// The constraint on the LIST ITSELF, not its element. Also raises.
			name:                 "target-list-level-assert",
			returnType:           "int[] @assert(f, {{ false }})",
			bareReturnType:       "int[]",
			raw:                  "[1,2]",
			disposition:          stockRaises,
			wantBareServedJSON:   "[1,2]",
			bundle:               bundle(assert(schema.Type{Kind: schema.TypeList, Elem: ptr(intT())}, "f", "false")),
			wantTargetKind:       schema.TypeList,
			wantNativeWouldServe: "[1,2]",
			outClaim:             true,
		},
		{
			// The constrained list ELEMENT. It does NOT raise: coerce_array drops
			// every element whose coercion fails, so stock serves the EMPTY list
			// where native served both. An out-claim in value — and attributable,
			// because the constraint-free control serves both elements too.
			name:                 "target-list-element-assert",
			returnType:           "TCBigNum[]",
			bareReturnType:       "int[]",
			decls:                "type TCBigNum = int @assert(big, {{ this > 100 }})\n",
			raw:                  "[1,2]",
			disposition:          stockServes,
			wantServedJSON:       "[]",
			wantBareServedJSON:   "[1,2]",
			bundle:               bundle(schema.Type{Kind: schema.TypeList, Elem: ptr(assert(intT(), "big", "this > 100"))}),
			wantTargetKind:       schema.TypeList,
			wantNativeWouldServe: "[1,2]",
			outClaim:             true,
		},
		{
			// The bare-`string` return, and the reason this test exists in this
			// shape. Stock SKIPS constraints on that route
			// ([TestStockSkipsConstraintsOnBareStringReturn]), so the constraint
			// changes NOTHING about what it serves — the control below returns the
			// identical document. NOT an out-claim: declining it is over-decline,
			// safe under the parity principle but not a fix for anything the
			// constraint caused.
			//
			// The raw is QUOTED. Stock's jsonish accepts a bare `hello`, but
			// native's static extractor requires a cleanly-claimable JSON candidate
			// and declines an unquoted token — which would make the twin decline for
			// a reason that has nothing to do with constraints.
			//
			// Stock's string short-circuit then hands that raw text back VERBATIM,
			// quotes included, so its value is the 7-character string `"hello"`
			// while native's JSON decodes to the 5-character `hello`. That is a real
			// representation difference on the bare-string route, and it is present
			// WITH AND WITHOUT the constraint — which is precisely why the
			// classification consults the control instead of comparing native
			// against the constrained leg alone. An earlier comparator papered over
			// it by stripping quotes before comparing, which is the lossiness this
			// row now documents rather than hides.
			name:                 "bare-string-return-assert",
			returnType:           `string @assert(f, {{ false }})`,
			bareReturnType:       "string",
			raw:                  `"hello"`,
			disposition:          stockServes,
			wantServedJSON:       `"\"hello\""`,
			wantBareServedJSON:   `"\"hello\""`,
			bundle:               bundle(assert(strT(), "f", "false")),
			wantTargetKind:       schema.TypePrimitive,
			wantNativeWouldServe: `"hello"`,
			outClaim:             false,
		},
	}
}

// classifyTargetConstraintRow decides, from three MEASURED values, whether
// native's admitted behaviour diverged from stock BECAUSE OF THE CONSTRAINT.
//
// A raise is unambiguous: stock produced no value at all, so anything native
// served is an out-claim.
//
// For a serving row the question is attribution, and one comparison cannot
// answer it. Native and stock can differ for reasons the constraint did not
// cause — the bare-string route hands back the raw text verbatim, quotes
// included, where native's JSON decodes them away, with or without any
// constraint. Charging that to the constraint gate would be a false claim of
// exactly the kind rounds 2-4 removed. So the CONTROL decides:
//
//   - native == stock(constrained)                  -> agreement, no out-claim;
//   - native != stock(constrained) and
//     native == stock(unconstrained)                -> the constraint caused the
//     divergence: native served what BAML serves WITHOUT it. OUT-CLAIM;
//   - native != stock(constrained) and
//     native != stock(unconstrained)                -> native differs from BAML
//     on this route regardless of the constraint. Not attributable to this gate,
//     so not an out-claim it removed.
//
// It is the same test the optional-target shape was dropped under: a difference
// that survives removing the constraint was never part of the gap.
func classifyTargetConstraintRow(disposition stockTargetDisposition, nativeJSON, stockJSON, bareJSON []byte) (bool, string, error) {
	if disposition == stockRaises {
		return true, "stock produced no value at all", nil
	}
	agreesWithStock, err := sameServedDocument(nativeJSON, stockJSON)
	if err != nil {
		return false, "", err
	}
	if agreesWithStock {
		return false, "native served exactly what stock served", nil
	}
	agreesWithControl, err := sameServedDocument(nativeJSON, bareJSON)
	if err != nil {
		return false, "", err
	}
	if agreesWithControl {
		return true, "native served what stock serves WITHOUT the constraint", nil
	}
	return false, "native differs from stock with AND without the constraint — a route-level " +
		"difference this gate does not cause", nil
}

// TestStockTargetLevelConstraintDispositionAndNativeDecline is the two-leg proof
// behind the target-level gate.
//
// PER ROW, in order:
//
//  1. STOCK, LIVE. Parse the row's raw through the untouched v0.223 runtime and
//     require the measured disposition — a classified "Assertions failed."
//     rejection, or a parse whose value equals the pinned document. This is a
//     measurement, not a golden: a stock version that changed any of these turns
//     this red rather than letting the narrative rot.
//  2. NATIVE DECLINES. The equivalent Bundle is refused by
//     SupportsNativeFinalBundle AND ParseStaticBundle with the
//     ErrDeBAMLParseUnsupported fallback sentinel, so the call routes to BAML —
//     which is the engine that produced leg 1.
//  3. THE CLASSIFICATION IS DERIVED. Whether the row was an out-claim is
//     recomputed from stock's own outcome versus what native served, and checked
//     against the row's claim. A row cannot assert "native out-claimed" unless
//     stock either rejected or returned different bytes.
//
// Step 3 is what keeps this honest about the bare-string row: it is carried here
// as a decline whose stock counterpart AGREES with what native served, i.e. an
// over-decline, and the test fails if anyone relabels it an out-claim.
func TestStockTargetLevelConstraintDispositionAndNativeDecline(t *testing.T) {
	assertBAMLAuthority(t)
	ensureConstraintTypeMap()

	cases := targetConstraintCases()
	if len(cases) != 4 {
		t.Fatalf("target-level cases = %d, want 4", len(cases))
	}
	// At least one row of each disposition, so neither arm of the switch below is
	// vacuous, and at least one genuine out-claim — without which the test would
	// prove the gate removed nothing.
	var raises, serves, outClaims int
	for _, c := range cases {
		switch c.disposition {
		case stockRaises:
			raises++
		case stockServes:
			serves++
		}
		if c.outClaim {
			outClaims++
		}
	}
	if raises == 0 || serves == 0 {
		t.Fatalf("cases cover raises=%d serves=%d; both dispositions must be exercised", raises, serves)
	}
	if outClaims == 0 {
		t.Fatal("no case claims an out-claim; the gate would be removing nothing")
	}
	if outClaims == len(cases) {
		t.Fatal("every case claims an out-claim; the safe-over-decline arm of the taxonomy would be " +
			"unrepresented and a misattribution could not be caught")
	}

	files := map[string]string{
		"clients.baml": clientSource(),
		"types.baml":   typesBAMLSource(),
	}
	fnName := func(c targetConstraintCase) string { return "TC_" + sanitizeTargetCaseName(c.name) }
	bareFnName := func(c targetConstraintCase) string { return "TCbare_" + sanitizeTargetCaseName(c.name) }
	fnSource := func(name, returnType string) string {
		return "function " + name + "() -> " + returnType + " {\n" +
			"  client " + clientName + "\n  prompt #\"\n" + constraintPromptBody + "\n\"#\n}\n"
	}
	for _, c := range cases {
		// Each row contributes TWO functions: the constrained subject and the
		// constraint-free CONTROL of the same shape. The control is what makes a
		// divergence attributable rather than merely observed.
		files["tc_"+sanitizeTargetCaseName(c.name)+".baml"] = c.decls +
			fnSource(fnName(c), c.returnType) + "\n" +
			fnSource(bareFnName(c), c.bareReturnType)
	}
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", files, env)
	if err != nil {
		t.Fatalf("CreateRuntime over the target-level cases: %v\n"+
			"Every predicate must survive BAML's jinja parser, so a malformed one takes the whole project down.", err)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parse := func(fn string) (any, error) {
				args := baml.BamlFunctionArguments{Kwargs: map[string]any{"text": c.raw, "stream": false}, Env: env}
				encoded, encErr := args.Encode()
				if encErr != nil {
					t.Fatalf("encode parse args: %v", encErr)
				}
				ctx, cancel := context.WithTimeout(context.Background(), constraintParseTimeout)
				defer cancel()
				return rt.CallFunctionParse(ctx, fn, encoded)
			}

			// --- leg 1: stock, live, WITH the constraint ----------------------
			result, stockErr := parse(fnName(c))

			var stockJSON []byte
			switch c.disposition {
			case stockRaises:
				if stockErr == nil {
					t.Fatalf("stock v0.223 PARSED %v; this row is pinned as an assert REJECTION.\n"+
						"The measurement is stale — the gate's out-claim story for this shape must be re-derived.", result)
				}
				if errors.Is(stockErr, context.DeadlineExceeded) {
					t.Fatalf("stock CallFunctionParse did not return within %s; that is a Rust panic unwinding "+
						"the tokio worker, not a classifiable outcome", constraintParseTimeout)
				}
				kind, ok := classifyStockConstraintError(stockErr)
				if !ok {
					t.Fatalf("stock failed with an error this harness cannot classify as a constraint outcome: %v\n"+
						"An unrecognized parse failure is not evidence the CONSTRAINT rejected anything.", stockErr)
				}
				if kind != ConstraintAssertFailed {
					t.Fatalf("stock failed as %s, want %s (%v)", kind, ConstraintAssertFailed, stockErr)
				}
			case stockServes:
				if stockErr != nil {
					t.Fatalf("stock v0.223 REJECTED the parse (%v); this row is pinned as serving %s.\n"+
						"The measurement is stale.", stockErr, c.wantServedJSON)
				}
				var jerr error
				stockJSON, jerr = stockServedJSON(result)
				if jerr != nil {
					t.Fatalf("%v", jerr)
				}
				if got := string(stockJSON); got != c.wantServedJSON {
					t.Fatalf("stock served %s, want %s; the row's classification is derived from "+
						"this value, so it must be re-measured", got, c.wantServedJSON)
				}
			}

			// --- leg 1b: stock, live, WITHOUT the constraint (the control) ----
			// The control must always PARSE: it is the same shape minus the
			// predicate, so a rejection here means the fixture — not the
			// constraint — is what stock is objecting to, and no attribution the
			// classifier makes below would mean anything.
			bareResult, bareErr := parse(bareFnName(c))
			if bareErr != nil {
				t.Fatalf("the constraint-FREE control `-> %s` was REJECTED by stock (%v); the row cannot "+
					"attribute anything to its constraint", c.bareReturnType, bareErr)
			}
			bareJSON, jerr := stockServedJSON(bareResult)
			if jerr != nil {
				t.Fatalf("%v", jerr)
			}
			if got := string(bareJSON); got != c.wantBareServedJSON {
				t.Fatalf("the constraint-free control served %s, want %s; attribution is derived from "+
					"this value, so it must be re-measured", got, c.wantBareServedJSON)
			}

			// --- leg 2: native declines ---------------------------------------
			b := c.bundle()
			if got := b.Target.Kind; got != c.wantTargetKind {
				t.Fatalf("the native Bundle's target kind is %q, want %q; it no longer mirrors the "+
					"return type %q the stock leg parsed", got, c.wantTargetKind, c.returnType)
			}
			if err := debaml.SupportsNativeFinalBundle(b); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("SupportsNativeFinalBundle = %v; want the ErrDeBAMLParseUnsupported fallback sentinel — "+
					"a target-level constraint must route this call to BAML", err)
			}
			res, perr := debaml.ParseStaticBundle(context.Background(), b, c.raw)
			if !errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("ParseStaticBundle = (%s, %v); want the fallback sentinel", res.JSON, perr)
			}
			if len(res.JSON) != 0 {
				t.Errorf("a declining ParseStaticBundle still returned %s", res.JSON)
			}

			// --- leg 3: the classification, derived ---------------------------
			// WHAT NATIVE SERVED, run rather than quoted. Native's coercer never
			// reads Meta.Constraints, so the constraint-stripped twin coerces raw
			// exactly as the constrained bundle did while the gate admitted it.
			// Deriving it closes the hole a literal would leave: the row cannot
			// claim native produced something it did not.
			twin, removed := withoutTargetConstraints(b)
			if removed == 0 {
				t.Fatalf("the case declares no target constraint; it cannot be about the target gate")
			}
			twinRes, twinErr := debaml.ParseStaticBundle(context.Background(), twin, c.raw)
			if twinErr != nil {
				t.Fatalf("the constraint-stripped twin DECLINED (%v); it must serve, both as the "+
					"no-over-decline control and as the source of what native served", twinErr)
			}
			if got := string(twinRes.JSON); got != c.wantNativeWouldServe {
				t.Fatalf("native serves %s for %s, not the pinned %s; the classification "+
					"is derived from this value", got, c.raw, c.wantNativeWouldServe)
			}
			nativeWouldServe := twinRes.JSON

			derived, why, cerr := classifyTargetConstraintRow(c.disposition, nativeWouldServe, stockJSON, bareJSON)
			if cerr != nil {
				t.Fatalf("classifying the row: %v", cerr)
			}
			if derived != c.outClaim {
				t.Fatalf("the row claims outClaim=%v, but the measurements derive %v: %s.\n"+
					"  stock(constrained) = %s\n  stock(control)     = %s\n  native served      = %s",
					c.outClaim, derived, why, stockOrRaised(c.disposition, stockJSON), bareJSON, nativeWouldServe)
			}
			if c.outClaim {
				t.Logf("OUT-CLAIM REMOVED (%s): stock %s for `-> %s` on %s; native served %s while the gate "+
					"admitted it, and now declines to BAML.", why, c.disposition, c.returnType, c.raw, nativeWouldServe)
			} else {
				t.Logf("OVER-DECLINE (safe, not an out-claim fix) (%s): stock serves %s for `-> %s` on %s, "+
					"control serves %s; the gate declines it anyway.",
					why, stockJSON, c.returnType, c.raw, bareJSON)
			}
		})
	}
}

// stockOrRaised renders the constrained leg's outcome for a failure message: a
// raising row has no served document, and printing an empty one would read as
// "it served nothing" rather than "it errored".
func stockOrRaised(d stockTargetDisposition, stockJSON []byte) string {
	if d == stockRaises {
		return "<raised>"
	}
	return string(stockJSON)
}

// withoutTargetConstraints returns a copy of b whose target type tree carries no
// constraints, plus the number removed. Only the target is walked: these bundles
// have no classes or enums, and the target is the whole subject.
func withoutTargetConstraints(b *schema.Bundle) (*schema.Bundle, int) {
	removed := 0
	var strip func(schema.Type) schema.Type
	strip = func(t schema.Type) schema.Type {
		removed += len(t.Meta.Constraints)
		t.Meta.Constraints = nil
		if t.Elem != nil {
			e := strip(*t.Elem)
			t.Elem = &e
		}
		if t.Key != nil {
			k := strip(*t.Key)
			t.Key = &k
		}
		if t.Value != nil {
			v := strip(*t.Value)
			t.Value = &v
		}
		if t.Union != nil {
			u := *t.Union
			u.Variants = make([]schema.Type, len(t.Union.Variants))
			for i := range t.Union.Variants {
				u.Variants[i] = strip(t.Union.Variants[i])
			}
			t.Union = &u
		}
		return t
	}
	return &schema.Bundle{Target: strip(b.Target)}, removed
}

// sameServedDocument compares two served documents STRUCTURALLY and with EXACT
// numeric semantics.
//
// Both sides are decoded into `any` trees and walked by [equalServedValue], so
// the comparison is over JSON structure and values rather than over spelling: a
// list is a list, a string is a string, whitespace and object-key order do not
// matter, and `5` equals `5.0`.
//
// IT REPLACES TWO WEAKER COMPARATORS, and both failures pointed the same way —
// two genuinely different served documents reading as agreement, in the one
// place the taxonomy claims to be DERIVED:
//
//   - the first version deleted quotes, commas and spaces from each side to
//     reconcile stock's Go `%v` rendering with native's JSON, which cannot tell
//     `[1 2]` from `[12]`;
//   - the second decoded both sides but compared with reflect.DeepEqual over
//     plain `encoding/json` output. That stores EVERY JSON number as float64, so
//     `9007199254740992` and `9007199254740993` — distinct integers either side
//     of 2^53, and native's int path carries exact int64 — collapsed into one
//     float64 and compared equal.
//
// So decoding alone was not enough: the decode must PRESERVE the number, which
// is why [decodeServedDocument] uses json.Decoder.UseNumber and the walk
// compares json.Number values as exact rationals. Raw json.Number STRING
// equality would not do either — it would make `5` differ from `5.0` and break
// the spelling-freedom contract this comparator deliberately keeps.
//
// A decode failure is returned, never swallowed. "It did not parse" is not
// evidence of agreement.
func sameServedDocument(a, b []byte) (bool, error) {
	av, err := decodeServedDocument(a)
	if err != nil {
		return false, err
	}
	bv, err := decodeServedDocument(b)
	if err != nil {
		return false, err
	}
	return equalServedValue(av, bv)
}

// decodeServedDocument decodes one served document into an `any` tree whose
// numbers are json.Number — the exact source text — rather than float64.
//
// It also rejects TRAILING DATA. `5 6` decoding to `5` and comparing equal to a
// document that really is `5` would be the same kind of quiet agreement this
// comparator exists to prevent.
func decodeServedDocument(b []byte) (any, error) {
	dec := stdjson.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode %s: %w", b, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: trailing data after the document (%v)", b, err)
	}
	return v, nil
}

// equalServedValue walks two decoded trees. It is a hand-written walk rather
// than reflect.DeepEqual because numbers need value equality, not
// representation equality: json.Number is a string, so DeepEqual over it would
// report `5` != `5.0`.
//
// An unexpected dynamic type is an ERROR, not `false`. UseNumber decoding can
// only produce nil, bool, string, json.Number, []any and map[string]any, so
// anything else means the input did not come from where this function assumes —
// and answering "not equal" would be a guess.
func equalServedValue(a, b any) (bool, error) {
	switch av := a.(type) {
	case nil:
		return b == nil, nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv, nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv, nil
	case stdjson.Number:
		bv, ok := b.(stdjson.Number)
		if !ok {
			return false, nil
		}
		return equalJSONNumber(av, bv)
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false, nil
		}
		for i := range av {
			eq, err := equalServedValue(av[i], bv[i])
			if err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false, nil
		}
		for k, v := range av {
			w, present := bv[k]
			if !present {
				return false, nil
			}
			eq, err := equalServedValue(v, w)
			if err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("decoded tree holds %T, which UseNumber decoding cannot produce", a)
	}
}

// equalJSONNumber compares two JSON numbers by EXACT value.
//
// big.Rat parses the literal decimal (including an exponent) with no precision
// loss at any magnitude, so `5` == `5.0` and `1e2` == `100` while
// `9007199254740992` != `9007199254740993`. float64 would lose the second pair
// and int64 would reject the first.
func equalJSONNumber(a, b stdjson.Number) (bool, error) {
	ar, ok := new(big.Rat).SetString(a.String())
	if !ok {
		return false, fmt.Errorf("JSON number %q is not an exact rational", a.String())
	}
	br, ok := new(big.Rat).SetString(b.String())
	if !ok {
		return false, fmt.Errorf("JSON number %q is not an exact rational", b.String())
	}
	return ar.Cmp(br) == 0, nil
}

// TestSameServedDocumentIsNotLossy pins the comparator against the failure modes
// its two predecessors had. Both lost information in the same DIRECTION —
// distinct documents reading as agreement — which is the direction that would
// let [classifyTargetConstraintRow] miss a real value divergence.
//
//   - The string normalizer deleted quotes, commas and spaces, so `[1 2]` and
//     `[12]` were both `[12]` and a two-element list equalled a one-element one.
//   - The decode-then-reflect.DeepEqual version stored every number as float64,
//     so `9007199254740992` and `9007199254740993` — either side of 2^53, and
//     native's int path carries exact int64 — collapsed into one value.
//
// Both pairs are cases here, and both must be UNEQUAL.
//
// The equal cases are the other half of the claim, and they are what rules out
// the cheap over-corrections: comparing raw json.Number strings would make `5`
// differ from `5.0`, and comparing bytes would make whitespace and key order
// significant. The comparator must see through JSON's spelling freedom while
// still preserving value, so trading false agreement for false divergence fails
// here just as loudly.
func TestSameServedDocumentIsNotLossy(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		// THE FIRST REGRESSION. Two genuinely different documents the string
		// normalizer collapsed into one.
		{"two-element list vs one-element list", `[1,2]`, `[12]`, false},
		{"list vs concatenated string", `[1,2]`, `"12"`, false},
		// A string whose CONTENT is a quoted string is not the same document as
		// that string — exactly the bare-string route's stock-vs-native
		// difference, which the old comparator erased.
		{"quoted content vs bare content", `"\"hello\""`, `"hello"`, false},
		{"empty list vs two-element list", `[]`, `[1,2]`, false},
		{"empty list vs empty string", `[]`, `""`, false},
		{"nested depth differs", `[[1]]`, `[1]`, false},
		// THE SECOND REGRESSION. Adjacent integers just past 2^53: the same
		// float64 once decoded, distinct as written, and distinct in native's
		// int64 path. In a list, bare, and under an object key, because the walk
		// reaches the number by a different route in each.
		{"adjacent integers past 2^53 in a list", `[9007199254740992]`, `[9007199254740993]`, false},
		{"adjacent integers past 2^53, bare", `9007199254740992`, `9007199254740993`, false},
		{"adjacent integers past 2^53 under a key", `{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		// Spelling freedom that must NOT read as divergence. These are what a raw
		// json.Number string comparison would get wrong.
		{"whitespace is not structure", `[1, 2]`, `[1,2]`, true},
		{"int and float spelling of one number", `5`, `5.0`, true},
		{"exponent spelling of one number", `1e2`, `100`, true},
		// Value equality holds at large magnitude too — the exact-rational
		// comparison is not a special case bolted onto small numbers.
		{"large integer and its decimal spelling", `9007199254740993`, `9007199254740993.0`, true},
		{"object key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"identical strings", `"hello"`, `"hello"`, true},
	}
	if len(cases) != 15 {
		t.Fatalf("comparator cases = %d, want 15", len(cases))
	}
	var equalCases, unequalCases int
	for _, c := range cases {
		if c.equal {
			equalCases++
		} else {
			unequalCases++
		}
		t.Run(c.name, func(t *testing.T) {
			got, err := sameServedDocument([]byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("sameServedDocument(%s, %s): %v", c.a, c.b, err)
			}
			if got != c.equal {
				t.Errorf("sameServedDocument(%s, %s) = %v, want %v", c.a, c.b, got, c.equal)
			}
			// The comparison must not depend on argument order.
			rev, err := sameServedDocument([]byte(c.b), []byte(c.a))
			if err != nil {
				t.Fatalf("sameServedDocument(%s, %s): %v", c.b, c.a, err)
			}
			if rev != got {
				t.Errorf("sameServedDocument is not symmetric for %s / %s: %v then %v", c.a, c.b, got, rev)
			}
		})
	}
	if equalCases == 0 || unequalCases == 0 {
		t.Fatalf("cases cover equal=%d unequal=%d; a comparator test that only exercises one "+
			"answer cannot distinguish a correct comparator from a constant", equalCases, unequalCases)
	}

	// A decode failure is an ERROR, never a silent "not equal" — "it did not
	// parse" is not evidence about the documents.
	if _, err := sameServedDocument([]byte(`{`), []byte(`{}`)); err == nil {
		t.Error("sameServedDocument accepted undecodable input; a decode failure must surface")
	}
	// So is trailing data: decoding only the prefix and calling it equal to a
	// document that really is that prefix would be the same quiet agreement.
	if _, err := sameServedDocument([]byte(`5 6`), []byte(`5`)); err == nil {
		t.Error("sameServedDocument accepted trailing data; the prefix must not stand in for the document")
	}

	// The exact-value claim, checked at the seam rather than only through the
	// document API: adjacent integers past 2^53 must not be one number, and the
	// spelling-freedom pair must be.
	distinct, err := equalJSONNumber(stdjson.Number("9007199254740992"), stdjson.Number("9007199254740993"))
	if err != nil {
		t.Fatalf("equalJSONNumber on adjacent 2^53 integers: %v", err)
	}
	if distinct {
		t.Error("equalJSONNumber says 9007199254740992 == 9007199254740993; the comparison went " +
			"through float64 instead of an exact rational")
	}
	same, err := equalJSONNumber(stdjson.Number("5"), stdjson.Number("5.0"))
	if err != nil {
		t.Fatalf("equalJSONNumber on 5 vs 5.0: %v", err)
	}
	if !same {
		t.Error("equalJSONNumber says 5 != 5.0; the comparison is over the literal text, which breaks " +
			"the spelling-freedom contract")
	}
}

// stockServedJSON re-encodes a stock parse result as JSON so it can be compared
// with native's bytes on equal terms. The stock client decodes into Go values
// (string, []int64, ...); JSON is the common ground both legs already speak, and
// encoding rather than fmt-rendering is what keeps the comparison lossless.
func stockServedJSON(v any) ([]byte, error) {
	out, err := stdjson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encode stock result %T: %w", v, err)
	}
	return out, nil
}

// sanitizeTargetCaseName turns a case name into a BAML identifier fragment.
func sanitizeTargetCaseName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '-' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
