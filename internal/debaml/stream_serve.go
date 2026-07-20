package debaml

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// This file adds the Phase 7C NATIVE-ONLY streaming serving surface: a pure,
// socket-free, input-free schema-support preflight (SupportsNativeStream /
// SupportsNativeFinal) and the native-only partial/final parser closures
// (ParseNativeStreamPartial / ParseNativeStreamFinal) that own every admitted
// prefix and the final WITHOUT ever falling back to BAML (invariant I6).
//
// The distinction from the hybrid path (Parse, driven through the generated
// maybeParseDeBAMLStream/Final seam) is the fallback contract, NOT the parser:
// both call the SAME internal parseStream/coerceStream/coerce machinery. The
// hybrid returns bamlutils.ErrDeBAMLParseUnsupported so the caller re-parses the
// final through BAML; the native-only closures here NEVER surface that sentinel
// to a caller that could act on it — a partial decline is a silent no-emit and a
// final decline is a TERMINAL invariant (ErrDeBAMLNativeStreamUnsupported). No
// admitted prefix and no final can therefore invoke BAML on the native lane.
//
// The two SupportsNative* preflights are the pre-transport admission gate
// (§5.5 schema row / §5.9): they run the identical structural cut-line the
// runtime parser applies, but over the schema graph alone (no Raw, no FFI, no
// socket). Only schemas they fully cover are admitted; a runtime decline that
// is NOT a benign not-yet-parseable prefix is then an invariant, not a gap.

// ErrDeBAMLNativeStreamUnsupported is the TERMINAL native-only serving error: a
// native FINAL parse reported the BAML-fallback sentinel AFTER SupportsNativeFinal
// proved the schema. On the native-only lane this is never a BAML fallback (I6) —
// it is a terminal claimed failure a caller surfaces as a bounded terminal stream
// error and an "unexpected post-preflight unsupported" invariant metric. The 7C
// differential proves this never fires across the admitted corpus + fault set.
var ErrDeBAMLNativeStreamUnsupported = errors.New("debaml: native-only stream parser reported unsupported after schema preflight")

// SupportsNativeStream reports whether the native de-BAML STREAMING parser can
// own EVERY partial for s — a pure, socket-free, input-free preflight over the
// whole admitted type graph. It lowers and validates the dynamic output schema
// and runs the IDENTICAL structural cut-line parseStream applies at parse time
// (checkSupported + checkNoStreamAnnotations), returning a wrapped
// bamlutils.ErrDeBAMLParseUnsupported naming the first shape native cannot own.
//
// A nil return means the schema graph is fully covered: after it, a runtime
// streaming decline for a reason OTHER THAN a benign not-yet-parseable prefix is
// an invariant, not a schema gap (the raw-shape/coerce-parity claim is proven by
// the 7C differential, which the preflight cannot check without input). It is the
// schema-support half of native stream admission (§5.5): the caller admits only
// schemas it returns nil for, and declines everything else BEFORE transport claim.
func SupportsNativeStream(s *bamlutils.DynamicOutputSchema) error {
	bundle, err := lowerForSupport(s)
	if err != nil {
		return err
	}
	if err := checkSupported(bundle); err != nil {
		return err
	}
	if err := checkStreamRootSupported(bundle); err != nil {
		return err
	}
	// parseStream applies the stream-annotation guard on top of checkSupported;
	// mirror it exactly so the preflight and the runtime cut-line never diverge.
	// The dynamic bridge produces no annotations today, so this is defensive.
	return checkNoStreamAnnotations(bundle)
}

// checkStreamRootSupported declines pre-transport (I2) the root-class shapes whose
// admitted public stream could DIVERGE from BAML at a raw prefix the schema-only
// preflight cannot exclude — enforcing at admission the parity §5.9 requires,
// rather than leaving an unenforced runtime under/over-claim:
//
//   - a single STRING-absorbing-field root class (LIVE-CAPTURED, corpus-verified):
//     BAML does allow_as_string->class (InferedObject from the preamble) for it, so
//     a fenced prefix (```json{) and a bare completed string both make BAML error or
//     emit a value native cannot reproduce, while native would over-emit the
//     all-filler object.
//
// The admitted matrix that remains after ALL the narrows below is a SMALL provably-safe
// core (round-6/7/9, type-space-proven partial+final byte-exact over a true grammar
// cross-product with the recovery battery applied by field path): a required-field class
// (>=2 fields, or a single non-string-absorbing, non-list<string> field) whose field types
// are string / int (LAST position only) / enum / string-literal / int-literal / SINGLE-level
// list<string|int|enum|string-literal|int-literal> / a SAFE DIRECT nested class — a nested
// class with <=3 fields, <=1 list, and NO bare enum or string-literal field (recursively the
// same) — and deep nestings thereof.
// The gates below DECLINE everything else: a single string-absorbing field; a SINGLE
// list<string>-field root (BAML inferred-element recovery); a NESTED list list<list<...>>;
// an UNSAFE nested class — >=4 fields, >=2 lists, or a bare enum/string-literal field (BAML's
// nested-class start cadence there native cannot reproduce byte-exact); any non-last unquoted
// scalar; any union/optional; ANY map<string,*> (declined outright — BAML
// overwrite-on-duplicate-key); any non-string map key; a FUZZY field alias/canonical
// rendered-name COLLISION (two fields whose rendered names fold-collide through match_string,
// at least one an @alias — the byte-equal collision is already rejected at lowering,
// internal/schema/index.go; BAML's order-dependent collision resolution across ordinary-coerce
// vs try_cast is unpinned, a tracked #583 residual decline); ANY bool or float (incl. bool
// literal); ANY bare null-typed field (the null-keyword streaming cadence); and any class inside
// a list element or map value. (A NON-colliding field @alias is now ADMITTED — native reproduces
// static BAML v0.223's ALIAS-ONLY rendered-name matching byte-exact: coerceClass/coerceStreamClass
// match Name.RenderedName() only, so the alias is the sole key candidate and the canonical name is
// an extra key, exactly as jsonish coerce_class does. A field @description, class/enum
// @alias/@description, and non-ASCII names/values are ADMITTED too — no key-matching divergence.)
//
// NOTE (§5.9 owed debt, ledger #583): for these admitted schemas, five NON-conforming /
// BAML-jsonish greedy-recovery INPUT classes — a bare unquoted scalar in a string field, an
// embedded quote in a string, an invalid enum member, a transient extra non-schema field,
// and a lone incomplete comment marker `/` — still make BAML emit transient greedy-recovery
// partials that native under-emits/skips (final-consistent, never an over-claim). This is an
// EXPLICITLY DEFERRED must-close teardown-blocker, NOT a lowered contract: §5.9's exact
// per-boundary partial parity stays the target.
//
// After this returns nil, the FINAL for the admitted schema is reproduced byte-exact, and
// every BAML-success partial boundary is reproduced EXCEPT the five #583 owed-debt triggers
// above (for which native purely UNDER-emits / skips — never a divergent or over-claim emit;
// proven by the rigorous type-space differential's strict + deferred-byte-check legs and the
// corpus acceptance probe). The five-trigger exception is deferred debt, NOT a lowered §5.9
// bar — exact per-boundary parity for them stays the target (ledger #583).
func checkStreamRootSupported(b *schema.Bundle) error {
	if rootFieldIsStringAbsorbing(b) {
		return unsupported("stream: single string-absorbing-field root class (BAML allow_as_string->class diverges)")
	}
	if rootSingleFieldIsFreeStringList(b) {
		// A single-field root class whose ONE field is a list<string> (a FREE string element).
		// Like the single string-absorbing field, BAML applies aggressive single-field-root
		// inferred-element recovery: a fenced/prose blob becomes a string list ELEMENT
		// (LIVE-CAPTURED: partial ```json{ streams `{"v":["{"]}`), while native returns the
		// empty list. Only a FREE-string element absorbs that inferred content — list<int>,
		// list<enum>, list<literal> reject a non-coercible blob and both sides emit `[]`
		// (Root{nums:list<int>} stays admissible, corpus-verified). The recovery is
		// INPUT-dependent (admission cannot see the provider's prefix), so exclude the SHAPE;
		// a >=2-field class with a list<string> field has no single-field inferred trigger.
		return unsupported("stream: single list<string>-field root class (BAML inferred-element recovery native cannot reproduce)")
	}
	if rootSingleFieldIsClass(b) {
		// A single-field root class whose ONE field is a NESTED class. Like the single
		// string-absorbing / single list<string> field, BAML applies aggressive single-field-root
		// INFERRED-OBJECT recovery: from a fenced/prose blob it fills the nested class with its
		// streaming skeleton (LIVE-CAPTURED: partial ```json{ for Root{inner:Inner{list,...}}
		// streams `{"inner":{"l0":[],"s1":null,"age":null}}`), while native holds inner NULL. That
		// recovery is INPUT-dependent (admission cannot see the provider's prefix), so exclude the
		// SHAPE — a >=2-field root carrying a (safe) nested class has no single-field inferred
		// trigger and stays admissible. (A lone nested-class field is NOT string-absorbing per
		// rootFieldIsStringAbsorbing, but this separate single-field-root recovery still applies.)
		return unsupported("stream: single nested-class-field root class (BAML inferred-object recovery native cannot reproduce)")
	}
	if typeGraphHasClassCycle(b) {
		// A self- or mutually-recursive class graph (Node{next:Node}, A<->B). The proven-safe
		// admitted core is a FINITE acyclic class tree (the generative differential enumerates
		// only bounded acyclic shapes); a cyclic schema is outside it AND would make the
		// recursive-descent gate below (checkNested) non-terminating. Decline it pre-transport —
		// a conservative under-claim on an out-of-core shape (never an over-claim), consistent
		// with the native lane admitting only what it can prove byte-exact.
		return unsupported("stream: recursive/cyclic class graph (outside the finite acyclic admitted core)")
	}
	if typeGraphHasNestedList(b) {
		// A NESTED list (list<list<…>>) anywhere. When a class that is a FIELD VALUE (a
		// nested class) carries a list<list<…>> field, BAML's semantic streaming HOLDS the
		// whole nested class null at the empty-object boundary (`"inner":{` -> inner:null)
		// until it has real content, while native emits the null-filled skeleton eagerly
		// (LIVE-CAPTURED: inner with {s,e,list<string>} matches, adding a list<list<enum>>
		// field flips BAML to inner:null). Native cannot prove BAML's class-start cadence for
		// a nested-list-bearing class, so decline any list<list<…>> pre-transport (a
		// single-level list stays admissible). list<list<…>> is a rare 2-D schema.
		return unsupported("stream: nested list list<list<...>> (BAML nested-class start cadence for a list-of-list-bearing class native cannot reproduce)")
	}
	if graphHasGreedyCommaRisk(b) {
		return unsupported("stream: non-last unquoted-scalar class field or scalar map value (BAML greedy-read cascade across a tight comma diverges)")
	}
	if typeGraphHasUnion(b) {
		// A UNION or OPTIONAL/NULLABLE field whose non-null value STREAMS incomplete
		// declines at coerceStream ("mixed-union semantic streaming deferred"), while
		// BAML keeps the partial value — a divergence for any admitted stream that
		// could carry such a value. Decline any union anywhere in the graph.
		return unsupported("stream: union/optional field in the type graph (mixed-union semantic streaming deferred)")
	}
	if typeGraphHasNonStringMapKey(b) {
		// BAML admits enum / string-literal / union-of-literal map keys and, when a
		// provider key is NOT a valid member (28_map_bad_enum_key: Key{A,B} with key
		// "C"; 104_map_bad_key_original_order: union{"A","B"} with key "C"), KEEPS the
		// unmatched key verbatim in original order, while native's map-key coercer
		// DECLINES the whole map on the non-member key — a BAML-success/native-decline
		// divergence the schema-only preflight cannot rule out from any prefix. A plain
		// `string` key carries no membership constraint (every key is valid), so ONLY a
		// non-string key type declines. (checkSupportedMapKey still admits enum/literal
		// keys for the hybrid final, which MAY fall back to BAML; this native-only lane
		// cannot, so it excludes the shape pre-transport.)
		return unsupported("stream: map with a non-string key type (BAML keeps non-member enum/literal keys native declines)")
	}
	if aliasRenderedNameCollision(b) {
		// A FUZZY alias/canonical rendered-name COLLISION: two fields of one class whose
		// rendered names fold-collide through the SAME match_string path the coercers use
		// (case / accent / punctuation / NFKD), with at least one carrying an @alias, yet
		// are NOT byte-equal (the byte-equal collision — e.g. `a @alias("b")` + literal `b`,
		// or two equal aliases — is already rejected at lowering by the rendered-name index,
		// internal/schema/index.go:145). A non-colliding field @alias is otherwise ADMITTED:
		// native reproduces static BAML v0.223's alias-only rendered-name matching byte-exact
		// (coerceClass/coerceStreamClass match Name.RenderedName() only). The collision case is
		// held back because BAML's resolution is order-dependent and NOT uniform across the two
		// coercion paths (ordinary Class::coerce is find-first in declaration order; try_cast
		// builds a rendered-name map), and no v0.223 test pins whether the schema compiler even
		// admits such a class. Until a live probe pins that (schema admission + final + every
		// stream prefix, both declaration and key orders), native declines it pre-transport
		// rather than over-claim a parity it cannot prove — a tracked #583 residual.
		return unsupported("stream: fuzzy field alias/canonical rendered-name collision (BAML's order-dependent resolution unpinned — #583 residual)")
	}
	if typeGraphHasMap(b) {
		// ANY map<string,*>. BAML's generic map coercion is INSERT/OVERWRITE-on-
		// duplicate-key ({"a":"one","a":"two"} -> {"a":"two"}, corpus 106), while
		// native's coerceMap REJECTS duplicate original keys and declines the whole map.
		// Admission cannot see whether a provider will emit duplicate keys, so it must
		// exclude the SHAPE. Native cannot reproduce BAML's overwrite+insertion order
		// across the map recovery surface, so decline every map pre-transport (the core
		// carries no maps). Subsumes the earlier scalar-map-value / non-string-map-key
		// narrows.
		return unsupported("stream: map<string,*> (BAML overwrite-on-duplicate-key native cannot reproduce)")
	}
	if typeGraphHasBoolOrFloat(b) {
		// A bool or float field anywhere. In the STREAMING partial cadence BAML and
		// native diverge on the boundary/completion handling of these two: BAML KEEPS a
		// complete bool keyword (`true`) at an unclosed boundary while native marks the
		// last unquoted token incomplete and null-replaces it, and BAML NULLS an
		// incomplete float (`1.`) where native's stream scalar parse declines and
		// no-emits — both are BAML-visible partial events native cannot reproduce
		// byte-exact (semantic-streaming completion nuance). Their FINAL is byte-exact,
		// but the admitted matrix requires partial+final parity, so decline bool/float
		// pre-transport (int stays: a streamed int and BAML both null the incomplete
		// value, byte-exact). This keeps the core small and PROVABLY partial+final safe.
		return unsupported("stream: bool or float field (BAML streaming completion cadence native cannot reproduce byte-exact)")
	}
	if typeGraphHasNull(b) {
		// A bare `null`-typed leaf (a field whose ONLY value is JSON null) anywhere in the
		// graph. Like bool/float it is a done-required unquoted keyword with a streaming
		// cadence native cannot reproduce: BAML emits `null` from the FIRST byte of an
		// incomplete `n`/`nu`/`nul` keyword AND keeps null through the following comma/next
		// key, while native's stream scalar parse declines the partial keyword and no-emits
		// the WHOLE object; BAML also RECOVERS a null field under a block/line/mid comment
		// or an unclosed tail where native over-claims. Their final can even disagree
		// (comment+null: BAML declines, native claims). A `null`-only field is degenerate
		// (it can hold nothing else), so decline the shape pre-transport rather than model
		// BAML's null-keyword streaming — keeping the core PROVABLY partial+final safe.
		return unsupported("stream: bare null-typed field (BAML null-keyword streaming/recovery cadence native cannot reproduce byte-exact)")
	}
	if typeGraphHasClassInContainer(b) {
		// A CLASS reachable as a list ELEMENT or map VALUE (directly or nested).
		// BAML's list/map coercion DROPS a child whose class coercion fails a required
		// field (ArrayItemParseError / skipped map entry) and SUCCEEDS with the container
		// minus that child, while native's list/map coercer DECLINES the WHOLE container
		// because a class-targeting object is not a whitelisted proven-BAML-error
		// (provenListItemError / the map-value equivalent defer a valObject to avoid a
		// mis-drop). Root{items:Pair[]} with {"items":[{"a":"x"}]} -> BAML [], native
		// declines; Root{by_id:map<string,Item>} with an entry missing a field -> BAML
		// skips it, native declines. This is INPUT-dependent (a specific invalid child),
		// so admission cannot exclude it per-input — it must exclude the SHAPE. Native's
		// per-child class coercion cannot be proven byte-exact vs BAML's lenient drop
		// across the whole class-in-container recovery surface, so decline the shape
		// pre-transport (a >=2-field class as a DIRECT field stays admissible: a required
		// direct class with an invalid value makes BOTH BAML and native fail, no drop).
		return unsupported("stream: a class reachable inside a list element or map value (BAML drops an invalid child and succeeds; native declines the container)")
	}
	if typeGraphHasUnsafeNestedClass(b) {
		// A NESTED class (a class reached as a field value of another class) whose
		// semantic-streaming START cadence native cannot reproduce byte-exact. BAML HOLDS a
		// nested class NULL at its opening/pending prefixes until it has a "renderable" field,
		// where the threshold depends on the nested class's field COUNT and COMPOSITION in a
		// way native cannot determine (LIVE-BISECTED: a nested class with >=4 fields OR >=2
		// list fields holds null at the empty prefix `"inner":{"s":"` where native emits the
		// null-filled skeleton; and a nested class with a bare enum/string-literal field holds
		// null when a sibling is missing and that leaf becomes the only, incomplete, present
		// field). A nested class that is SMALL and simple — <=3 fields, <=1 list, and NO bare
		// enum or string-literal field (fields are string / int / int-literal / a single list
		// / a nested class recursively of the same shape) — matches BAML byte-exact and stays
		// admitted. Decline the rest pre-transport (the ROOT class is exempt — it is never held
		// null; only its class-typed field values are). Reproducing BAML's nested-class start
		// cadence exactly is future work.
		return unsupported("stream: nested class with a start cadence native cannot reproduce byte-exact (>=4 fields, >=2 lists, or a bare enum/string-literal field)")
	}
	return nil
}

// typeGraphHasUnsafeNestedClass reports whether any NESTED class (a class reached as a field
// value of the root class, directly or deeper — NOT the root itself) is "unsafe": it has >=4
// fields, >=2 list fields, or a bare enum / string-literal field. See checkStreamRootSupported
// for why the native lane declines those (the nested-class semantic-streaming start cadence).
func typeGraphHasUnsafeNestedClass(b *schema.Bundle) bool {
	if b.Target.Kind != schema.TypeClass {
		return false
	}
	root, ok := b.FindClass(b.Target.Name, b.Target.Mode)
	if !ok {
		return false
	}
	var checkNested func(t schema.Type, seen map[string]bool) bool
	checkNested = func(t schema.Type, seen map[string]bool) bool {
		if t.Kind != schema.TypeClass {
			return false
		}
		cls, ok := b.FindClass(t.Name, t.Mode)
		if !ok || seen[t.Name] {
			// seen guard: a class already visited on this walk has had its whole subtree
			// checked; re-descending would loop forever on a cyclic graph (matches the
			// sibling classSubtreeHasList). checkStreamRootSupported already declines a
			// cyclic graph up front, so this is defense-in-depth for a direct caller.
			return false
		}
		seen[t.Name] = true
		if nestedClassUnsafe(b, cls) {
			return true
		}
		for i := range cls.Fields { // recurse into deeper nested classes
			if checkNested(cls.Fields[i].Type, seen) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for i := range root.Fields {
		if checkNested(root.Fields[i].Type, seen) {
			return true
		}
	}
	return false
}

// typeGraphHasClassCycle reports whether the class-reference graph reachable from the bundle
// target contains a cycle — a class that (directly, through a container element/value, or
// transitively via other classes) refers back to a class already on the current reference
// path (Node{next:Node}; A{b:B}, B{a:A}). It is a proper 3-colour DFS: a back-edge to a class
// still on the DFS stack (gray) is a cycle, whereas re-reaching a fully-explored class (black)
// via a second acyclic path — a DAG diamond — is NOT. The native stream lane admits only a
// FINITE acyclic class tree, so checkStreamRootSupported declines a cyclic graph before the
// recursive-descent gates (which would otherwise not terminate on one).
func typeGraphHasClassCycle(b *schema.Bundle) bool {
	if b.Target.Kind != schema.TypeClass {
		return false
	}
	const (
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored, acyclic
	)
	color := map[string]int{}
	// classRefs collects every class TYPE referenced by t (a bare class, or a class inside a
	// list/map/tuple/union), without following the reference — the DFS follows them.
	var classRefs func(t schema.Type, out *[]schema.Type)
	classRefs = func(t schema.Type, out *[]schema.Type) {
		switch t.Kind {
		case schema.TypeClass:
			*out = append(*out, t)
		case schema.TypeList:
			if t.Elem != nil {
				classRefs(*t.Elem, out)
			}
		case schema.TypeMap:
			if t.Key != nil {
				classRefs(*t.Key, out)
			}
			if t.Value != nil {
				classRefs(*t.Value, out)
			}
		case schema.TypeTuple:
			for i := range t.Items {
				classRefs(t.Items[i], out)
			}
		case schema.TypeUnion:
			if t.Union != nil {
				for i := range t.Union.Variants {
					classRefs(t.Union.Variants[i], out)
				}
			}
		}
	}
	var dfs func(t schema.Type) bool
	dfs = func(t schema.Type) bool {
		cls, ok := b.FindClass(t.Name, t.Mode)
		if !ok {
			return false
		}
		switch color[t.Name] {
		case gray:
			return true // back-edge to a class on the current path
		case black:
			return false // already fully explored, acyclic
		}
		color[t.Name] = gray
		for i := range cls.Fields {
			var refs []schema.Type
			classRefs(cls.Fields[i].Type, &refs)
			for _, r := range refs {
				if dfs(r) {
					return true
				}
			}
		}
		color[t.Name] = black
		return false
	}
	return dfs(b.Target)
}

func nestedClassUnsafe(b *schema.Bundle, cls *schema.ClassDef) bool {
	if len(cls.Fields) >= 4 {
		return true
	}
	lists := 0
	childWithList := false
	for i := range cls.Fields {
		ft := cls.Fields[i].Type
		switch ft.Kind {
		case schema.TypeList:
			lists++
		case schema.TypeEnum:
			return true
		case schema.TypeLiteral:
			if ft.Literal != nil && ft.Literal.Kind == schema.LiteralString {
				return true
			}
		case schema.TypeClass:
			if classSubtreeHasList(b, ft, map[string]bool{}) {
				childWithList = true
			}
		}
	}
	if lists >= 2 {
		return true
	}
	// A nested class that carries a LIST AND holds a nested-class child whose subtree ALSO
	// contains a list has a recursive nested-start cadence native cannot reproduce (LIVE-CAPTURED:
	// Outer{list, Inner{list, int}, int} with the outer's list MISSING streams outer:null in BAML
	// while native emits the skeleton). Either alone is fine — an outer-list with a no-list child,
	// or a no-list outer holding a child-with-list, both stay admitted.
	return lists >= 1 && childWithList
}

// classSubtreeHasList reports whether the class t (or any class nested under it) has a list field.
func classSubtreeHasList(b *schema.Bundle, t schema.Type, seen map[string]bool) bool {
	if t.Kind != schema.TypeClass {
		return false
	}
	cls, ok := b.FindClass(t.Name, t.Mode)
	if !ok || seen[t.Name] {
		return false
	}
	seen[t.Name] = true
	for i := range cls.Fields {
		ft := cls.Fields[i].Type
		if ft.Kind == schema.TypeList {
			return true
		}
		if ft.Kind == schema.TypeClass && classSubtreeHasList(b, ft, seen) {
			return true
		}
	}
	return false
}

// aliasRenderedNameCollision reports whether any class has two fields whose RENDERED names
// fuzzy-collide — i.e. some response key could match BOTH through the coercers' field-key
// matcher (matchesStringToString: trim / case-fold / accent / punctuation-strip / NFKD, no
// substring) — where at least one of the two carries an @alias. The BYTE-equal rendered
// collision (a @alias("b") + literal b, or two equal aliases) is already rejected earlier at
// lowering by the rendered-name index (internal/schema/index.go:145), so this catches only the
// FUZZY residue the exact index misses (e.g. @alias("Foo") vs @alias("foo"); @alias("héading")
// vs literal heading).
//
// It is scoped to alias-bearing collisions — the shapes admission newly reaches now that the
// blanket field-@alias decline is gone. A pure canonical/canonical fuzzy pair (no alias) is a
// pre-existing shape outside this teardown and is intentionally left untouched. Genuine
// alias/canonical collisions stay a tracked #583 residual decline until a live BAML probe pins
// the order-dependent resolution; see checkStreamRootSupported.
func aliasRenderedNameCollision(b *schema.Bundle) bool {
	for i := range b.Classes {
		fields := b.Classes[i].Fields
		for a := range fields {
			for c := a + 1; c < len(fields); c++ {
				if fields[a].Name.Alias == nil && fields[c].Name.Alias == nil {
					continue
				}
				ra, rc := fields[a].Name.RenderedName(), fields[c].Name.RenderedName()
				// Check both directions: a response key equal to either rendered form
				// must be unambiguous. The fold is symmetric for our passes, but probe
				// both to be defensive against any combining-mark edge asymmetry.
				if matchesStringToString(ra, rc) || matchesStringToString(rc, ra) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasBoolOrFloat reports whether a bool or float primitive (or a bool/float
// literal) is reachable anywhere in the bundle graph. See checkStreamRootSupported for
// why the native lane declines them (streaming completion cadence divergence).
func typeGraphHasBoolOrFloat(b *schema.Bundle) bool {
	if typeHasBoolOrFloat(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasBoolOrFloat(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

// rootSingleFieldIsFreeStringList reports whether the bundle's output target is a class with
// EXACTLY ONE field whose type is a list<string> (a FREE string primitive element) — the
// single-field-root case where BAML's inferred-element recovery absorbs a fenced/prose blob
// as a string list element and native diverges. A non-string element (int/enum/literal)
// rejects the blob so both emit []. See checkStreamRootSupported.
func rootSingleFieldIsFreeStringList(b *schema.Bundle) bool {
	if b.Target.Kind != schema.TypeClass {
		return false
	}
	cls, ok := b.FindClass(b.Target.Name, b.Target.Mode)
	if !ok || len(cls.Fields) != 1 {
		return false
	}
	f := cls.Fields[0].Type
	return f.Kind == schema.TypeList && f.Elem != nil &&
		f.Elem.Kind == schema.TypePrimitive && f.Elem.Primitive == schema.PrimitiveString
}

// rootSingleFieldIsClass reports whether the bundle's output target is a class with EXACTLY
// ONE field whose type is a (nested) class — the single-field-root case where BAML's
// inferred-object recovery fills the nested class skeleton from a fenced/prose blob and native
// holds it null. See checkStreamRootSupported.
func rootSingleFieldIsClass(b *schema.Bundle) bool {
	if b.Target.Kind != schema.TypeClass {
		return false
	}
	cls, ok := b.FindClass(b.Target.Name, b.Target.Mode)
	if !ok || len(cls.Fields) != 1 {
		return false
	}
	return cls.Fields[0].Type.Kind == schema.TypeClass
}

// typeGraphHasNestedList reports whether a list<list<…>> (a list whose element is itself a
// list) is reachable anywhere in the bundle graph. See checkStreamRootSupported for why the
// native lane declines it (the nested-class start cadence for a list-of-list-bearing class).
func typeGraphHasNestedList(b *schema.Bundle) bool {
	if typeHasNestedList(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasNestedList(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

func typeHasNestedList(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeList:
		if t.Elem != nil && t.Elem.Kind == schema.TypeList {
			return true
		}
		return t.Elem != nil && typeHasNestedList(*t.Elem)
	case schema.TypeMap:
		return (t.Key != nil && typeHasNestedList(*t.Key)) || (t.Value != nil && typeHasNestedList(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasNestedList(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasNestedList(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasNull reports whether a bare null-typed leaf is reachable anywhere in the
// bundle graph. See checkStreamRootSupported for why the native lane declines it (the
// null-keyword streaming/recovery cadence). Note: this matches only Primitive(Null), NOT
// an optional/union (those already decline via typeGraphHasUnion).
func typeGraphHasNull(b *schema.Bundle) bool {
	if typeHasNull(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasNull(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

func typeHasNull(t schema.Type) bool {
	switch t.Kind {
	case schema.TypePrimitive:
		return t.Primitive == schema.PrimitiveNull
	case schema.TypeList:
		return t.Elem != nil && typeHasNull(*t.Elem)
	case schema.TypeMap:
		return (t.Key != nil && typeHasNull(*t.Key)) || (t.Value != nil && typeHasNull(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasNull(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasNull(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

func typeHasBoolOrFloat(t schema.Type) bool {
	switch t.Kind {
	case schema.TypePrimitive:
		return t.Primitive == schema.PrimitiveBool || t.Primitive == schema.PrimitiveFloat
	case schema.TypeLiteral:
		return t.Literal != nil && t.Literal.Kind == schema.LiteralBool
	case schema.TypeList:
		return t.Elem != nil && typeHasBoolOrFloat(*t.Elem)
	case schema.TypeMap:
		return (t.Key != nil && typeHasBoolOrFloat(*t.Key)) || (t.Value != nil && typeHasBoolOrFloat(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasBoolOrFloat(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasBoolOrFloat(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasMap reports whether any map is reachable anywhere in the bundle graph.
// See checkStreamRootSupported for why the native lane declines all maps.
func typeGraphHasMap(b *schema.Bundle) bool {
	if typeHasMap(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasMap(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

func typeHasMap(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeMap:
		return true
	case schema.TypeList:
		return t.Elem != nil && typeHasMap(*t.Elem)
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasMap(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasMap(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasClassInContainer reports whether any class is reachable as a LIST
// element or MAP value anywhere in the bundle graph (directly, e.g. list<class> /
// map<string,class>, or nested, e.g. a class field whose type transitively contains
// one). See checkStreamRootSupported for why the native-only lane declines it.
func typeGraphHasClassInContainer(b *schema.Bundle) bool {
	if typeHasClassContainer(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasClassContainer(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

// typeHasClassContainer reports whether t is (or nests) a list whose element type
// or a map whose value type transitively CONTAINS a class. A bare class TYPE is not
// itself a container hit (a direct class field is admissible); this only fires when a
// class sits inside a list/map. The b.Classes walk in typeGraphHasClassInContainer
// covers every reachable class's own fields, so a container nested inside a class is
// still caught.
func typeHasClassContainer(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeList:
		return t.Elem != nil && (typeContainsClass(*t.Elem) || typeHasClassContainer(*t.Elem))
	case schema.TypeMap:
		if t.Value != nil && typeContainsClass(*t.Value) {
			return true
		}
		return (t.Key != nil && typeHasClassContainer(*t.Key)) || (t.Value != nil && typeHasClassContainer(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasClassContainer(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasClassContainer(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeContainsClass reports whether a class appears anywhere in t's type graph
// (t itself is a class, or it is a composite whose sub-types transitively contain
// a class). Used to decide whether a list element / map value carries a class.
func typeContainsClass(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeClass:
		return true
	case schema.TypeList:
		return t.Elem != nil && typeContainsClass(*t.Elem)
	case schema.TypeMap:
		return (t.Key != nil && typeContainsClass(*t.Key)) || (t.Value != nil && typeContainsClass(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeContainsClass(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeContainsClass(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasNonStringMapKey reports whether any map reachable from the bundle
// target has a KEY type other than a plain `string` primitive (an enum, a string
// literal, or a union of literals). See checkStreamRootSupported for why the
// native-only stream lane declines those keys.
func typeGraphHasNonStringMapKey(b *schema.Bundle) bool {
	if typeHasNonStringMapKey(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasNonStringMapKey(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

func typeHasNonStringMapKey(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeMap:
		if t.Key != nil && !(t.Key.Kind == schema.TypePrimitive && t.Key.Primitive == schema.PrimitiveString) {
			return true
		}
		if t.Key != nil && typeHasNonStringMapKey(*t.Key) {
			return true
		}
		return t.Value != nil && typeHasNonStringMapKey(*t.Value)
	case schema.TypeList:
		return t.Elem != nil && typeHasNonStringMapKey(*t.Elem)
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasNonStringMapKey(t.Items[i]) {
				return true
			}
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if typeHasNonStringMapKey(t.Union.Variants[i]) {
					return true
				}
			}
		}
	}
	return false
}

// typeGraphHasUnion reports whether any type reachable from the bundle target is a
// union (which includes an optional/nullable field, lowered as a union with a null
// arm). See checkStreamRootSupported for why the stream lane declines them.
func typeGraphHasUnion(b *schema.Bundle) bool {
	if typeHasUnion(b.Target) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasUnion(b.Classes[i].Fields[j].Type) {
				return true
			}
		}
	}
	return false
}

func typeHasUnion(t schema.Type) bool {
	switch t.Kind {
	case schema.TypeUnion:
		return true
	case schema.TypeList:
		return t.Elem != nil && typeHasUnion(*t.Elem)
	case schema.TypeMap:
		return (t.Key != nil && typeHasUnion(*t.Key)) || (t.Value != nil && typeHasUnion(*t.Value))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasUnion(t.Items[i]) {
				return true
			}
		}
	}
	return false
}

// SupportsNativeFinal is the FINAL-parse twin of SupportsNativeStream: it runs the
// exact schema cut-line the non-stream Parse path applies (lower + ValidateOutput
// + checkSupported — no stream-annotation guard, since final parse rejects no
// annotations). nil means native can own the final for every raw shape BAML can,
// proven per-corpus by the 7C differential.
func SupportsNativeFinal(s *bamlutils.DynamicOutputSchema) error {
	bundle, err := lowerForSupport(s)
	if err != nil {
		return err
	}
	if err := checkSupported(bundle); err != nil {
		return err
	}
	// The single string-absorbing-field root class diverges on the FINAL too
	// (LIVE-CAPTURED: a bare `"just a string"` final makes BAML error while native
	// over-emits {"field":"just a string"}), so decline it here as well.
	return checkStreamRootSupported(bundle)
}

// lowerForSupport lowers + validates a dynamic output schema exactly as the
// runtime Parse / parseStream paths do (nil-check, FromDynamicOutputSchema,
// ValidateOutput), returning a wrapped bamlutils.ErrDeBAMLParseUnsupported on any
// failure so the two SupportsNative* preflights share one lowering contract with
// the runtime parsers.
func lowerForSupport(s *bamlutils.DynamicOutputSchema) (*schema.Bundle, error) {
	if s == nil {
		return nil, unsupported("nil output schema")
	}
	bundle, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		return nil, unsupportedErr("lower schema", err)
	}
	if err := bundle.ValidateOutput(); err != nil {
		return nil, unsupportedErr("validate schema", err)
	}
	return bundle, nil
}

// ParseNativeStreamPartial is the NATIVE-ONLY streaming partial parser closure.
// It NEVER invokes BAML (I6). It returns:
//
//   - (json, nil)  native produced a partial for accumulated → the caller emits
//     it as the partial StreamResult for this cadence tick;
//   - (nil,  nil)  native has no partial yet for accumulated (a benign
//     not-yet-parseable prefix, or a raw shape it does not claim) → the caller
//     emits NOTHING this tick and keeps draining, exactly as RunStreamOrchestration
//     skips a BAML parse-stream that returns an error/nil (orchestrator.go: the
//     `parseErr == nil && parsed != nil` gate).
//
// It maps the BAML-fallback sentinel AND any non-sentinel claimed parse failure
// to the (nil, nil) skip WITHOUT calling BAML — a partial never terminates a
// stream, and there is no per-prefix fallback on the native lane. Whether native
// emits WHEREVER BAML emits (so the cadence matches) is proven by the 7C
// differential, not asserted here. schema must already have passed
// SupportsNativeStream (the caller's pre-transport gate).
func ParseNativeStreamPartial(ctx context.Context, s *bamlutils.DynamicOutputSchema, accumulated string) (json.RawMessage, error) {
	res, err := Parse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          accumulated,
		OutputSchema: s,
		Stream:       true,
	})
	if err != nil {
		// Decline (sentinel) OR claimed failure: on the partial lane both are a
		// silent no-emit. NEVER a BAML fallback. The stream continues.
		return nil, nil
	}
	if len(res.JSON) == 0 {
		return nil, nil
	}
	return res.JSON, nil
}

// ParseNativeStreamFinal is the NATIVE-ONLY final parser closure for a streamed
// response. It NEVER invokes BAML (I6). It returns the flattened final JSON, or a
// TERMINAL error:
//
//   - a native CLAIMED parse failure (a non-sentinel error — one BAML would also
//     hit, e.g. a missing required field) propagates unchanged, so the differential
//     catches drift instead of masking it;
//   - the BAML-fallback sentinel is REMAPPED to ErrDeBAMLNativeStreamUnsupported —
//     a terminal invariant on the native lane, since SupportsNativeFinal already
//     admitted the schema, so a runtime unsupported is a contract violation
//     (§5.9 "runtime unsupported after this preflight is a bug and terminal
//     claimed failure"), never a per-prefix BAML fallback.
//
// schema must already have passed SupportsNativeFinal.
func ParseNativeStreamFinal(ctx context.Context, s *bamlutils.DynamicOutputSchema, accumulated string) (json.RawMessage, error) {
	res, err := Parse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          accumulated,
		OutputSchema: s,
		Stream:       false,
	})
	if err == nil {
		return res.JSON, nil
	}
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		// EOF-object completion (§5.9): an ordinary completed stream ([DONE] /
		// finish_reason:stop) whose accumulated model text is a complete-but-unclosed
		// object is a SUCCESS for BAML, whose stream final runs the non-stream Parse
		// that closes the object at EOF. The native final path declines an unclosed
		// structure (the extractor defers it), so reproduce BAML's EOF close here —
		// admission (SupportsNativeFinal) already ran, so completeUnclosedFinal is
		// scoped to admitted schemas — and re-Parse the completed (now-closed) text,
		// which is byte-identical to the equivalent closed input. This does NOT change
		// the streaming PARTIAL cadence (ParseNativeStreamPartial is untouched): only
		// the FINAL at stream end behaves like BAML's non-stream Parse.
		if completed, changed := completeUnclosedFinal(accumulated); changed {
			if res2, err2 := Parse(ctx, bamlutils.DeBAMLParseRequest{
				Raw:          completed,
				OutputSchema: s,
				Stream:       false,
			}); err2 == nil {
				return res2.JSON, nil
			} else {
				// The completed text still does not coerce (e.g. a missing required
				// scalar): carry that verdict, which matches BAML erroring on the
				// same incomplete final.
				err = err2
			}
		}
	}
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		// Native declined a final it should own after preflight. On the native-only
		// lane this is terminal, NOT a BAML re-parse.
		return nil, fmt.Errorf("%w: %v", ErrDeBAMLNativeStreamUnsupported, err)
	}
	// A claimed native parse failure — propagate verbatim (terminal).
	return nil, err
}
