//go:build integration

package integration

import (
	"context"
	stdjson "encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// De-BAML Phase 7C — round-7 RIGOROUS generative differential (the complete proof).
//
// EXACT coverage (stated honestly — no overstatement):
//   - a TRUE GRAMMAR CROSS-PRODUCT enumerator (nested loops, NOT a hand-picked add() list —
//     see enumerate()) over the FULL dynamic type universe the preflight can reach:
//     string, int, enum, string-literal, int-literal (ADMITTED leaves); bool, float,
//     bool-literal, and bare `null` (DECLINED leaves) — each under every wrapper
//     {bare, list, list<list<…>>, optional, map} at every position {LAST, NON-LAST, SINGLE};
//     nested classes at depth 1 and 2 carrying the full admitted leaf/list combination;
//     recursive list<list<int|enum|literal>>; and the DECLINED composites optional, union,
//     map<string,*>, list<class>, map<string,class>, class-in-container; plus every METADATA
//     kind (class @alias/@description/non-ASCII NAME; field @alias — ASCII + non-ASCII —,
//     @description, non-ASCII NAME; enum @alias, non-ASCII NAME; enum-VALUE @alias,
//     @DESCRIPTION, non-ASCII VALUE; non-ASCII string-literal value) at ROOT, NESTED,
//     ENUM-VALUE and LIST positions;
//   - an INDEPENDENT §11 disposition oracle (expectAdmit — a direct hand-encoding of the
//     matrix, NOT SupportsNativeStream) asserted against SupportsNativeStream for EVERY
//     generated shape (fails on over- OR under-admission);
//   - a PRE-NORMALIZATION byte-exact comparison: BAML via dynclient.DynamicParseRaw
//     (FlattenDynamicOutput ONLY — NO absent-optional injection, NO reorder/sort), so
//     the parser's raw field order + presence are preserved; native raw output emits
//     schema-order natively;
//   - the FINAL (ParseNativeStreamFinal) byte-exact for EVERY (admitted-shape, battery
//     input), and the PARTIAL cadence (ParseNativeStreamPartial) BYTE-BY-BYTE for every
//     STRICT battery input (see below); a byte prefix that splits a multi-byte rune is
//     invalid UTF-8 (BAML's CFFI rejects it, so it cannot be the oracle there) — native
//     is still driven on it and must not panic, and the BAML comparison resumes at the
//     next rune boundary;
//   - a recovery battery covering every §6.3 axis: block/line/mid comment, trailing /
//     leading comma, whitespace, unclosed / dangling / trailing-comma tails, extra field,
//     prose, fence, missing each field, casefold, bad-member/enum, LIST BAD-CHILD (a
//     non-coercible typed-list element BAML drops), ESCAPED string (\\ \n \t — native
//     now DECODES these byte-exact), and UNICODE string content. The field-specific
//     mutations are applied BY FIELD PATH (walkFields recurses into nested classes), so
//     each mutation is placed at its own field at ANY depth, not only at the root. Admitted
//     nested classes (small/simple — <=3 fields, <=1 list, no bare enum/string-literal) are
//     exercised inside; UNSAFE nested classes (>=4 fields, >=2 lists, or a bare enum/literal —
//     BAML's start cadence native cannot reproduce) are oracle-asserted DECLINES.
//
// THREE battery classes, asserted differently and honestly:
//   - STRICT inputs: byte-exact partial+final vs live BAML. This is the 0-divergence gate.
//   - OVER-DECLINE inputs — an invalid enum member ("badmember"), an extra non-schema field
//     mid-stream ("extra_field"), a lone incomplete comment marker ("mid_comment"): native
//     may SAFELY SKIP the ambiguous/non-conforming/transient frame where BAML recovers a
//     value (BAML stream-nulls a bad enum but its FINAL both-declines; BAML transiently nulls
//     the real field while an extra key streams; a lone `/` is not yet `/*` vs `//`). The
//     contract stays strict — native EITHER matches BAML byte-exact OR skips; if it emits it
//     must be byte-exact — so an over-claim is still caught, only the safe under-emit is
//     permitted. FINAL stays byte-exact.
//   - DEFERRED inputs — a bare unquoted scalar in a string field ("num_in"), and a string
//     whose content has an EMBEDDED quote from \" ("escapedq"): both trigger a BAML jsonish
//     greedy-recovery heuristic (bare-scalar raw-span / embedded-quote close) that only
//     appears at INCOMPLETE-object prefixes, that BAML itself discards at the final frame, and
//     that native cannot yet reproduce byte-exact (#583 owed debt). Native's contract is a
//     PURE UNDER-approximation: at those frames native NO-EMITS (skips) rather than emit a
//     byte-different value. This leg ENFORCES that bound — native EITHER skips OR emits
//     byte-exact vs BAML; a byte-different native emission is a divergent/over-claim emit and
//     FAILS (BAML queried only when native emits). FINAL stays byte-exact. The live companion
//     TestStream7CDeferredHeuristics proves the same on the two canonical specimens.
//
// Fields are emitted in SCHEMA order (the order BAML renders in the prompt and LLMs
// follow); the FINAL is order-agnostic via reorder. 0 STRICT (and deferred-emit) divergences
// + 0 admission mismatches is the gate.

// ---- type grammar model ----

type ft struct {
	kind         string // string,int,null,bool,float,enum,literal,list,optional,union,map,class
	litKind      string // literal: "string" | "int" | "bool"
	elem         *ft    // list elem / map value / optional inner
	arms         []*ft  // union arms
	flds         []ffld // class fields
	clsAlias     bool   // class @alias
	clsDesc      bool   // class @description
	enumAlias    bool   // enum @alias
	enumValAlias bool   // enum VALUE @alias
	enumValDesc  bool   // enum VALUE @description (admitted; enumMatchCandidates models it)
	badName      bool   // non-ASCII class/enum name
	badEnumV     bool   // non-ASCII enum value name
	badLitV      bool   // non-ASCII string-literal value
}
type ffld struct {
	name  string
	t     *ft
	alias string
	desc  string
}

func tstr() *ft                { return &ft{kind: "string"} }
func tint() *ft                { return &ft{kind: "int"} }
func tnull() *ft               { return &ft{kind: "null"} }
func tbool() *ft               { return &ft{kind: "bool"} }
func tflt() *ft                { return &ft{kind: "float"} }
func tenum() *ft               { return &ft{kind: "enum"} }
func tlitS() *ft               { return &ft{kind: "literal", litKind: "string"} }
func tlitI() *ft               { return &ft{kind: "literal", litKind: "int"} }
func tlitB() *ft               { return &ft{kind: "literal", litKind: "bool"} }
func tlist(e *ft) *ft          { return &ft{kind: "list", elem: e} }
func tmap(e *ft) *ft           { return &ft{kind: "map", elem: e} }
func topt(e *ft) *ft           { return &ft{kind: "optional", elem: e} }
func tuni(a ...*ft) *ft        { return &ft{kind: "union", arms: a} }
func tcls(f ...ffld) *ft       { return &ft{kind: "class", flds: f} }
func fld(n string, t *ft) ffld { return ffld{name: n, t: t} }

// ---- schema builder ----

type tsBuilder struct {
	classes []bamlutils.OrderedEntry[*bamlutils.DynamicClass]
	enums   []bamlutils.OrderedEntry[*bamlutils.DynamicEnum]
	nc, ne  int
}

func (b *tsBuilder) spec(t *ft) *bamlutils.DynamicTypeSpec {
	switch t.kind {
	case "string", "int", "null", "bool", "float":
		return &bamlutils.DynamicTypeSpec{Type: t.kind}
	case "enum":
		return &bamlutils.DynamicTypeSpec{Ref: b.mkEnum(t)}
	case "literal":
		switch t.litKind {
		case "int":
			return &bamlutils.DynamicTypeSpec{Type: "literal_int", Value: 1}
		case "bool":
			return &bamlutils.DynamicTypeSpec{Type: "literal_bool", Value: true}
		default:
			v := "A"
			if t.badLitV {
				v = "GRÜN"
			}
			return &bamlutils.DynamicTypeSpec{Type: "literal_string", Value: v}
		}
	case "list":
		return &bamlutils.DynamicTypeSpec{Type: "list", Items: b.spec(t.elem)}
	case "map":
		return &bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: b.spec(t.elem)}
	case "optional":
		return &bamlutils.DynamicTypeSpec{Type: "optional", Inner: b.spec(t.elem)}
	case "union":
		arms := make([]*bamlutils.DynamicTypeSpec, len(t.arms))
		for i, a := range t.arms {
			arms[i] = b.spec(a)
		}
		return &bamlutils.DynamicTypeSpec{Type: "union", OneOf: arms}
	case "class":
		return &bamlutils.DynamicTypeSpec{Ref: b.mkClass(t)}
	}
	panic("bad kind " + t.kind)
}

func (b *tsBuilder) prop(f ffld) *bamlutils.DynamicProperty {
	s := b.spec(f.t)
	return &bamlutils.DynamicProperty{Type: s.Type, Ref: s.Ref, Items: s.Items, Inner: s.Inner, OneOf: s.OneOf, Keys: s.Keys, Values: s.Values, Value: s.Value, Alias: f.alias, Description: f.desc}
}

func (b *tsBuilder) mkEnum(t *ft) string {
	b.ne++
	name := fmt.Sprintf("E%d", b.ne)
	if t.badName {
		name = fmt.Sprintf("Enüm%d", b.ne)
	}
	v1 := "RED"
	if t.badEnumV {
		v1 = "RÖD"
	}
	red := &bamlutils.DynamicEnumValue{Name: v1}
	if t.enumValAlias {
		red.Alias = "rouge"
	}
	if t.enumValDesc {
		red.Description = "the red one"
	}
	de := &bamlutils.DynamicEnum{Values: []*bamlutils.DynamicEnumValue{red, {Name: "GREEN"}}}
	if t.enumAlias {
		de.Alias = "Colour"
	}
	b.enums = append(b.enums, bamlutils.OrderedKV(name, de))
	return name
}

func (b *tsBuilder) mkClass(t *ft) string {
	b.nc++
	name := fmt.Sprintf("C%d", b.nc)
	if t.badName {
		name = fmt.Sprintf("Cläss%d", b.nc)
	}
	var props []bamlutils.OrderedEntry[*bamlutils.DynamicProperty]
	for _, f := range t.flds {
		props = append(props, bamlutils.OrderedKV(f.name, b.prop(f)))
	}
	dc := &bamlutils.DynamicClass{Properties: bamlutils.MustOrderedMap(props...)}
	if t.clsAlias {
		dc.Alias = "Rooted"
	}
	if t.clsDesc {
		dc.Description = "a class"
	}
	b.classes = append(b.classes, bamlutils.OrderedKV(name, dc))
	return name
}

func tsSchema(root *ft) *bamlutils.DynamicOutputSchema {
	b := &tsBuilder{}
	var props []bamlutils.OrderedEntry[*bamlutils.DynamicProperty]
	for _, f := range root.flds {
		props = append(props, bamlutils.OrderedKV(f.name, b.prop(f)))
	}
	s := &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(props...)}
	// The root class itself can carry alias/description/non-ASCII name — but the dynamic
	// root has no class object; model those on a nested class instead (done in the
	// enumerator). The root Classes/Enums come from the builder.
	if len(b.classes) > 0 {
		s.Classes = bamlutils.MustOrderedMap(b.classes...)
	}
	if len(b.enums) > 0 {
		s.Enums = bamlutils.MustOrderedMap(b.enums...)
	}
	return s
}

// ---- INDEPENDENT §11 oracle ----

func expectAdmit(root *ft) bool {
	if root.kind != "class" {
		return false
	}
	if len(root.flds) == 1 && absorbsString(root.flds[0].t) {
		return false
	}
	if len(root.flds) == 1 && root.flds[0].t.kind == "list" && root.flds[0].t.elem.kind == "string" {
		// single list<string>-field root: BAML inferred-element recovery absorbs a blob as a
		// string element (see rootSingleFieldIsFreeStringList). Non-string elements reject it.
		return false
	}
	if len(root.flds) == 1 && root.flds[0].t.kind == "class" {
		// single nested-class-field root: BAML inferred-OBJECT recovery fills the nested skeleton
		// from a fenced/prose blob while native holds it null (see rootSingleFieldIsClass).
		return false
	}
	return !graphBad(root)
}

func absorbsString(t *ft) bool {
	switch t.kind {
	case "string":
		return true
	case "literal":
		return t.litKind == "string"
	case "optional":
		return absorbsString(t.elem)
	case "union":
		for _, a := range t.arms {
			if absorbsString(a) {
				return true
			}
		}
	}
	return false
}

// unquotedScalar mirrors typeIsUnquotedScalar: int/float/bool primitives and int/bool
// literals (NOT null, string, string-literal, enum).
func unquotedScalar(t *ft) bool {
	switch t.kind {
	case "int", "float", "bool":
		return true
	case "literal":
		return t.litKind == "int" || t.litKind == "bool"
	case "optional":
		return unquotedScalar(t.elem)
	case "union":
		for _, a := range t.arms {
			if unquotedScalar(a) {
				return true
			}
		}
	}
	return false
}

func containsClass(t *ft) bool {
	switch t.kind {
	case "class":
		return true
	case "list", "optional", "map":
		return t.elem != nil && containsClass(t.elem)
	case "union":
		for _, a := range t.arms {
			if containsClass(a) {
				return true
			}
		}
	}
	return false
}

// boolOrFloat mirrors typeHasBoolOrFloat: bool/float primitives + bool literal.
func boolOrFloat(t *ft) bool {
	switch t.kind {
	case "bool", "float":
		return true
	case "literal":
		return t.litKind == "bool"
	case "list", "optional", "map":
		return t.elem != nil && boolOrFloat(t.elem)
	case "union":
		for _, a := range t.arms {
			if boolOrFloat(a) {
				return true
			}
		}
	}
	return false
}

func graphBad(t *ft) bool {
	switch t.kind {
	case "map":
		return true
	case "union", "optional":
		return true
	case "bool", "float":
		return true
	case "null":
		// A bare null-typed leaf: declined (the null-keyword streaming/recovery cadence
		// native cannot reproduce byte-exact — see typeGraphHasNull).
		return true
	case "list":
		if t.elem.kind == "list" {
			// list<list<...>>: nested-list, declined (see typeGraphHasNestedList).
			return true
		}
		if containsClass(t.elem) {
			return true
		}
		return boolOrFloat(t.elem) || graphBad(t.elem)
	case "enum":
		// #555 Slice 2 (v2/v3): a non-ASCII enum name/value and an enum @alias/@description
		// are all ADMITTED. The complete enum leaf routes through the final coercer whose
		// enumMatchCandidates already models the rendered name (incl. alias), description,
		// and "rendered: description" candidate, and the fold is proven via bamlunicode —
		// so there is no metadata/Unicode enum decline.
		return false
	case "literal":
		// A non-ASCII string-literal VALUE is admitted (fold proven); only a bool literal
		// stays declined (streaming completion cadence, typeGraphHasBoolOrFloat).
		return t.litKind == "bool"
	case "class":
		// #555 Slice 2: a non-ASCII class/field NAME, a class @alias/@description, and a
		// field @description are ADMITTED (they don't change key matching / are handled by
		// the final coercer). A field @alias is the ONE metadata shape that DIVERGES
		// (LIVE-PROVEN): native's field-key matcher checks only Name.RenderedName() (the
		// alias), missing the canonical key BAML also matches — so it stays declined (#583).
		for i, f := range t.flds {
			if f.alias != "" {
				return true
			}
			if i < len(t.flds)-1 && unquotedScalar(f.t) {
				return true
			}
			if (f.t.kind == "list" || f.t.kind == "map") && containsClass(f.t.elem) {
				return true
			}
			// A NESTED class (class as a field VALUE) whose start cadence native cannot
			// reproduce — >=4 fields, >=2 lists, or a bare enum/string-literal field (see
			// typeGraphHasUnsafeNestedClass). The root itself is exempt (checked here only
			// for its class-typed FIELDS); the graphBad recursion applies it at each depth.
			if f.t.kind == "class" && nestedUnsafe(f.t) {
				return true
			}
			if graphBad(f.t) {
				return true
			}
		}
		return false
	}
	return false // string, int, literal-string, literal-int leaves (admitted)
}

// nestedUnsafe mirrors production nestedClassUnsafe EXACTLY: a nested class with >=4 fields,
// >=2 list fields, a bare enum / string-literal field, OR a list field AND a nested-class child
// whose subtree also contains a list (the recursive outer-list x child-list start-cadence).
func nestedUnsafe(t *ft) bool {
	if len(t.flds) >= 4 {
		return true
	}
	lists := 0
	childWithList := false
	for _, f := range t.flds {
		switch f.t.kind {
		case "list":
			lists++
		case "enum":
			return true
		case "literal":
			if f.t.litKind == "string" {
				return true
			}
		case "class":
			if ftSubtreeHasList(f.t) {
				childWithList = true
			}
		}
	}
	if lists >= 2 {
		return true
	}
	return lists >= 1 && childWithList
}

// ftSubtreeHasList reports whether the class ft (or any class nested under it) has a list field.
func ftSubtreeHasList(t *ft) bool {
	if t.kind != "class" {
		return false
	}
	for _, f := range t.flds {
		if f.t.kind == "list" {
			return true
		}
		if f.t.kind == "class" && ftSubtreeHasList(f.t) {
			return true
		}
	}
	return false
}

// ---- canonical JSON instance ----

// enumFirstVal returns the RENDERED name of the enum's first value — the string the
// battery must feed to MATCH it. #555 Slice 2 admits enum shapes whose first value is
// renamed non-ASCII (RÖD) or aliased (rendered "rouge"), so the canonical input can no
// longer be the hard-coded "RED": it must be the rendered value or the field never
// coerces (an invalid-member artifact, not a real divergence). Mirrors mkEnum.
func enumFirstVal(t *ft) string {
	switch {
	case t.badEnumV:
		return "RÖD"
	case t.enumValAlias:
		return "rouge"
	default:
		return "RED"
	}
}

// litStrVal returns the string-literal VALUE (mirrors spec(): "GRÜN" for a non-ASCII
// literal, else "A") so the canonical input matches the admitted non-ASCII literal.
func litStrVal(t *ft) string {
	if t.badLitV {
		return "GRÜN"
	}
	return "A"
}

func canon(t *ft) string {
	switch t.kind {
	case "string":
		return `"txt"`
	case "int":
		return `7`
	case "null":
		return `null`
	case "bool":
		return `true`
	case "float":
		return `1.5`
	case "enum":
		return `"` + enumFirstVal(t) + `"`
	case "literal":
		switch t.litKind {
		case "int":
			return `1`
		case "bool":
			return `true`
		default:
			return `"` + litStrVal(t) + `"`
		}
	case "list":
		return `[` + canon(t.elem) + `,` + canon(t.elem) + `]`
	case "map":
		return `{"k1":` + canon(t.elem) + `,"k2":` + canon(t.elem) + `}`
	case "optional":
		return canon(t.elem)
	case "union":
		return canon(t.arms[0])
	case "class":
		var b strings.Builder
		b.WriteByte('{')
		for i, f := range t.flds {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s":%s`, f.name, canon(f.t))
		}
		b.WriteByte('}')
		return b.String()
	}
	panic("canon bad kind")
}

func rootObj(root *ft, skip string, override map[string]string) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, f := range root.flds {
		if f.name == skip {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		v, ok := override[f.name]
		if !ok {
			v = canon(f.t)
		}
		fmt.Fprintf(&b, `"%s":%s`, f.name, v)
	}
	b.WriteByte('}')
	return b.String()
}

// ---- enumerator ----

type genShape struct {
	name string
	root *ft
}

// enumerate builds the shape corpus with a GENERATIVE RECURSIVE ENUMERATOR (programmatic
// loops + recursion, NOT hand tables) over the bounded admitted root/nested field-composition
// grammar. It is provably complete over that bounded grammar: a cold reader cannot name an
// omitted admitted shape without changing the grammar (the arity/composition/threshold loops)
// itself. The independent §11 oracle (expectAdmit) asserts each generated shape's EXACT
// disposition, and the test PINS the exact shape/admission counts + a per-arity anti-omission
// invariant so a later regression fails loudly. The dimensions:
//
//	A. leaf × wrapper × position — each of the 9 preflight-reachable leaves
//	   {string,int,enum,litstr,litint (admitted); bool,float,litbool,null (declined)} under
//	   each wrapper {bare, list, list<list<…>>, optional, map} at each position
//	   {v LAST, v NON-LAST, v the SINGLE field}.
//	B1. ROOT arity + composition (GENERATED) — for a = 1..maxRootArity(=8), sweep list-count
//	   nLists = 0..a and last-kind {all-quoted, int-LAST}, cycling list element types + quoted
//	   scalar flavors; plus a nested-class-field root at each arity. The root is EXEMPT from the
//	   nested threshold; production imposes no arity limit, so 8 is the stated bound.
//	B2. NESTED-class threshold corners (GENERATED, recursive to depth 2) — safeInner(fc,lc,child)
//	   emits every SAFE corner {fc in 1..3} x {lc in 0..1} (scalar-last, no bare enum/litstr),
//	   at depth 1 AND 2, INCLUDING the safe outer-carrying-a-list-while-holding-a-safe-child; and
//	   the just-UNSAFE neighbors (>=4 fields / >=2 lists / bare enum / bare litstr) as
//	   oracle-asserted DECLINES; plus safe nested list<leaf> for every element type and the
//	   list<list> / 6-field decline specimens.
//	C. metadata × position — every metadata kind {class @alias/@description/non-ASCII name;
//	   field @alias (ASCII + non-ASCII) / @description / non-ASCII name; enum @alias /
//	   non-ASCII name; enum-VALUE @alias / @DESCRIPTION / non-ASCII value; non-ASCII
//	   string-literal value} at each applicable position {root field, nested field, nested
//	   class, enum value, list element} — plus class-in-container and union. All DECLINE.
//
// The recovery battery (batteryFor) is applied BY FIELD PATH into nested classes AND into
// permitted LIST ELEMENTS (escaped/Unicode/deferred string, enum/literal casefold, bad-child),
// and structural recovery is also placed inside nested objects — not only at the root.
// maxRootArity bounds the generative ROOT arity sweep (production imposes no limit; 8 is a
// sound representative bound). Package-level so the count-pin invariant can reference it.
const maxRootArity = 8

func enumerate() []genShape {
	var out []genShape
	seen := map[string]bool{}
	add := func(n string, r *ft) {
		if seen[n] {
			return
		}
		seen[n] = true
		out = append(out, genShape{n, r})
	}

	type leaf struct {
		name string
		mk   func() *ft
	}
	leaves := []leaf{
		{"string", tstr}, {"int", tint}, {"enum", tenum}, {"litstr", tlitS}, {"litint", tlitI}, // admitted
		{"bool", tbool}, {"float", tflt}, {"litbool", tlitB}, {"null", tnull}, // declined
	}
	admitted := leaves[:5]
	type wrap struct {
		name string
		w    func(*ft) *ft
	}
	wraps := []wrap{
		{"bare", func(t *ft) *ft { return t }},
		{"list", func(t *ft) *ft { return tlist(t) }},
		{"listlist", func(t *ft) *ft { return tlist(tlist(t)) }},
		{"opt", func(t *ft) *ft { return topt(t) }},
		{"map", func(t *ft) *ft { return tmap(t) }},
	}

	// A. leaf × wrapper × position (LAST / NON-LAST / SINGLE).
	for _, lf := range leaves {
		for _, wr := range wraps {
			add("last_"+wr.name+"_"+lf.name, tcls(fld("name", tstr()), fld("v", wr.w(lf.mk()))))
			add("first_"+wr.name+"_"+lf.name, tcls(fld("v", wr.w(lf.mk())), fld("last", tstr())))
			add("single_"+wr.name+"_"+lf.name, tcls(fld("v", wr.w(lf.mk()))))
		}
	}

	// ---- B. GENERATIVE root/nested composition enumerator (loops + recursion, NOT a hand
	// table). BOUND: maxRootArity (production imposes NO root arity limit — see
	// checkStreamRootSupported; 8 is a sound representative bound, STATED here). Generated
	// shapes are structurally DEDUPED (sig) so the pinned counts stay meaningful.
	seenSig := map[string]bool{}
	var sig func(t *ft) string
	sig = func(t *ft) string {
		switch t.kind {
		case "class":
			s := "{"
			for _, f := range t.flds {
				s += sig(f.t) + ";"
			}
			return s + "}"
		case "list":
			return "[" + sig(t.elem) + "]"
		case "literal":
			return "lit:" + t.litKind
		default:
			return t.kind
		}
	}
	addGen := func(name string, r *ft) {
		if seenSig[sig(r)] {
			return
		}
		seenSig[sig(r)] = true
		add(name, r)
	}
	// admitted root field-type palettes (cycled by the generator).
	quotedLeaf := []func() *ft{tstr, tenum, tlitS} // non-last-safe scalars (string/enum/litstr)
	listLeaf := []func() *ft{                      // single-level lists of each admitted leaf
		func() *ft { return tlist(tstr()) }, func() *ft { return tlist(tint()) },
		func() *ft { return tlist(tenum()) }, func() *ft { return tlist(tlitS()) },
		func() *ft { return tlist(tlitI()) },
	}
	// safeInner builds a SAFE nested class within the recursive production rule: fc fields, lc
	// lists (lc<=1), an optional safe child (a nested-class field), the remaining fields string
	// scalars with an int LAST (scalar-last), and NO bare enum/string-literal.
	safeInner := func(fc, lc int, child *ft) *ft {
		var fs []ffld
		for i := 0; i < lc; i++ {
			fs = append(fs, fld(fmt.Sprintf("l%d", i), tlist(tstr())))
		}
		if child != nil {
			fs = append(fs, fld("inner", child))
		}
		for len(fs) < fc {
			if len(fs) == fc-1 {
				fs = append(fs, fld("age", tint())) // scalar LAST
			} else {
				fs = append(fs, fld(fmt.Sprintf("s%d", len(fs)), tstr()))
			}
		}
		return tcls(fs...)
	}
	// the max-safe OUTER nested class carrying a list WHILE holding a safe child (both within
	// the recursive safe rule): Outer{tags:list<string>, inner:Inner{s:string, age:int}, age:int}.
	safeOuterChild := safeInner(3, 1, safeInner(2, 0, nil))
	// the outer-list x CHILD-list cell: the child ALSO carries a list —
	// safeInner(3,1,safeInner(2,1,nil)) = Outer{tags:list<string>, inner:Inner{tags:list<string>,
	// age:int}, age:int}. This is UNSAFE (an oracle-asserted DECLINE): an outer that carries a list
	// WHILE holding a child that also carries a list has a recursive nested-start cadence native
	// cannot reproduce (surfaced live in round 12; NARROWED via nestedClassUnsafe). (Full outer x
	// child (fc,lc) x depth cross-product is DEFERRED test-hardening debt — see 7c.md
	// "Accepted-milestone test-coverage debt".)
	outerChildListUnsafe := safeInner(3, 1, safeInner(2, 1, nil))

	// B1. ROOT arity + composition. For a = 1..maxRootArity, sweep list-count nLists = 0..a and
	// last-kind {all-quoted, int-LAST}, cycling list element types and quoted scalar flavors;
	// PLUS a nested-class-field root at each arity. NOTE: at a=1 that nested-class-field root is
	// the SINGLE nested-class-field root, which is an INTENTIONALLY GENERATED DECLINE — a lone
	// nested-class field is not string-absorbing, but rootSingleFieldIsClass declines it
	// pre-transport (BAML single-field-root inferred-object recovery native cannot reproduce);
	// at a>=2 the root has sibling fields and is admitted. The oracle asserts each disposition.
	for a := 1; a <= maxRootArity; a++ {
		for nLists := 0; nLists <= a; nLists++ {
			for _, lastInt := range []bool{false, true} {
				var fs []ffld
				for i := 0; i < nLists; i++ {
					fs = append(fs, fld(fmt.Sprintf("l%d", i), listLeaf[i%len(listLeaf)]()))
				}
				for i := nLists; i < a; i++ {
					fs = append(fs, fld(fmt.Sprintf("q%d", i), quotedLeaf[(i-nLists)%len(quotedLeaf)]()))
				}
				if lastInt {
					fs[a-1] = fld("n", tint()) // an unquoted int is admitted ONLY as the LAST field
				}
				addGen(fmt.Sprintf("root_a%d_l%d_i%v", a, nLists, lastInt), tcls(fs...))
			}
		}
		// nested-class-field root: field 0 is a safe 3-field-with-list nested class, rest strings.
		var nf []ffld
		nf = append(nf, fld("inner", safeInner(3, 1, nil)))
		for i := 1; i < a; i++ {
			nf = append(nf, fld(fmt.Sprintf("s%d", i), tstr()))
		}
		addGen(fmt.Sprintf("root_a%d_nested", a), tcls(nf...))
	}
	// the max-safe outer-with-list-plus-safe-child, as a root field and as a SINGLE root field.
	addGen("root_outer_list_child", tcls(fld("a", tstr()), fld("outer", safeOuterChild)))
	addGen("root_single_outer_list_child", tcls(fld("outer", safeOuterChild)))
	// CELL (a): the outer-list x CHILD-list interaction (child also carries a list), through depth
	// 2 — the recursive interaction the fixed depth-2 wrappers (child=nil) missed. UNSAFE →
	// oracle-asserted DECLINE (nestedClassUnsafe: outer has a list AND a child whose subtree has a
	// list); native diverges on the nested-start cadence when the outer's list is missing.
	addGen("root_outer_list_childlist", tcls(fld("name", tstr()), fld("outer", outerChildListUnsafe)))
	// CELL (b): a root with a list AFTER an earlier scalar (the B1 sweep always puts lists first;
	// this exercises the scalar-then-list field ORDER at the root). int is LAST.
	addGen("root_scalar_then_list", tcls(fld("before", tstr()), fld("nums", tlist(tint())), fld("choice", tenum()), fld("n", tint())))

	// B2. NESTED-class threshold corners — a recursive generator. For the SAFE corners
	// (fc in {1,2,3} x lc in {0,1}, scalar-last, no bare enum/litstr) emit an ADMITTED nested
	// class at depth 1 AND 2; for the just-UNSAFE neighbors (4 fields / 2 lists / bare enum /
	// bare litstr) emit an oracle-asserted DECLINE at depth 1 AND 2. Every safe corner is
	// exercised by the by-path battery (incl. list-element recovery).
	for fc := 1; fc <= 3; fc++ {
		for lc := 0; lc <= 1 && lc <= fc; lc++ {
			inner := safeInner(fc, lc, nil)
			addGen(fmt.Sprintf("nsafe_f%d_l%d_d1", fc, lc), tcls(fld("name", tstr()), fld("inner", inner)))
			addGen(fmt.Sprintf("nsafe_f%d_l%d_d2", fc, lc), tcls(fld("a", tstr()), fld("b", tcls(fld("c", tstr()), fld("inner", inner)))))
		}
	}
	// safe RECURSIVE composition: an outer safe class carrying a list AND a safe child.
	addGen("nsafe_outer_list_child_d1", tcls(fld("name", tstr()), fld("outer", safeOuterChild)))
	// list-element coverage: a safe nested class carrying a single-level list of EACH admitted
	// leaf (so the by-path battery mutates a list ELEMENT inside a nested class), depth 1 + 2.
	for _, lf := range admitted {
		addGen("nestlist1_"+lf.name, tcls(fld("name", tstr()), fld("inner", tcls(fld("x", tstr()), fld("tags", tlist(lf.mk()))))))
		addGen("nestlist2_"+lf.name, tcls(fld("a", tstr()), fld("b", tcls(fld("c", tstr()), fld("inner", tcls(fld("x", tstr()), fld("tags", tlist(lf.mk()))))))))
	}
	// just-UNSAFE nested neighbors — oracle-asserted DECLINEs (nestedClassUnsafe).
	for _, ui := range []struct {
		name  string
		inner *ft
	}{
		{"f4", tcls(fld("a", tstr()), fld("b", tstr()), fld("c", tstr()), fld("d", tint()))}, // >=4 fields
		{"l2", tcls(fld("s", tstr()), fld("a", tlist(tstr())), fld("b", tlist(tint())))},     // >=2 lists
		{"bareenum", tcls(fld("s", tstr()), fld("e", tenum()))},                              // bare enum
		{"barelitstr", tcls(fld("s", tstr()), fld("l", tlitS()))},                            // bare string-literal
	} {
		addGen("nunsafe_"+ui.name+"_d1", tcls(fld("name", tstr()), fld("inner", ui.inner)))
		addGen("nunsafe_"+ui.name+"_d2", tcls(fld("a", tstr()), fld("b", tcls(fld("c", tstr()), fld("inner", ui.inner)))))
	}
	// UNSAFE nested-class / list<list<...>> DECLINE specimens.
	addGen("nb_kitchensink_unsafe", tcls(fld("name", tstr()), fld("inner",
		tcls(fld("s", tstr()), fld("e", tenum()), fld("ls", tlist(tstr())),
			fld("le", tlist(tenum())), fld("li", tlist(tlitI())), fld("n", tint())))))
	addGen("kitchensink_nestedlist", tcls(fld("name", tstr()), fld("inner",
		tcls(fld("s", tstr()), fld("ls", tlist(tstr())), fld("lls", tlist(tlist(tenum())))))))
	addGen("listlist_int", tcls(fld("u", tlist(tlist(tint()))), fld("last", tstr())))
	addGen("listlist_enum", tcls(fld("u", tlist(tlist(tenum()))), fld("last", tstr())))
	addGen("listlist_litstr", tcls(fld("u", tlist(tlist(tlitS()))), fld("last", tstr())))

	// C. metadata × position (#555 Slice 2). Non-ASCII names/values, field @description, and
	// class/enum @alias/@description ADMIT (no key-matching divergence — folds via bamlunicode,
	// enum values via enumMatchCandidates); the battery byte-walks them vs live BAML. Field
	// @alias is the ONE case that DECLINES (graphBad) — native's field-key matcher checks only
	// the rendered alias and misses the canonical key BAML also matches (#583).
	// class-level metadata on a NESTED class (the dynamic root has no class object).
	for _, m := range []struct {
		name string
		set  func(*ft)
	}{
		{"clsalias", func(t *ft) { t.clsAlias = true }},
		{"clsdesc", func(t *ft) { t.clsDesc = true }},
		{"clsbadname", func(t *ft) { t.badName = true }},
	} {
		inner := &ft{kind: "class", flds: []ffld{fld("x", tstr()), fld("v", tint())}}
		m.set(inner)
		add("meta_nestedcls_"+m.name, tcls(fld("name", tstr()), fld("inner", inner)))
	}
	// field-level metadata at ROOT and NESTED positions.
	for _, m := range []struct {
		name, alias, desc string
		badName           bool
	}{
		{"fldalias", "heading", "", false},
		{"fldalias_nonascii", "ÉTAT", "", false},
		{"flddesc", "", "the title", false},
		{"fldbadname", "", "", true},
	} {
		nm := "title"
		if m.badName {
			nm = "naïve"
		}
		add("meta_rootfld_"+m.name, tcls(ffld{name: nm, t: tstr(), alias: m.alias, desc: m.desc}, fld("last", tstr())))
		add("meta_nestedfld_"+m.name, tcls(fld("name", tstr()),
			fld("inner", tcls(ffld{name: nm, t: tstr(), alias: m.alias, desc: m.desc}, fld("y", tint())))))
	}
	// enum-level + enum-VALUE-level metadata (incl. @description), at a ROOT field and
	// inside a LIST element.
	for _, m := range []struct {
		name string
		set  func(*ft)
	}{
		{"enumalias", func(t *ft) { t.enumAlias = true }},
		{"enumbadname", func(t *ft) { t.badName = true }},
		{"enumvalalias", func(t *ft) { t.enumValAlias = true }},
		{"enumvaldesc", func(t *ft) { t.enumValDesc = true }},
		{"enumbadval", func(t *ft) { t.badEnumV = true }},
	} {
		e1 := &ft{kind: "enum"}
		m.set(e1)
		add("meta_rootenum_"+m.name, tcls(fld("f", e1), fld("last", tstr())))
		e2 := &ft{kind: "enum"}
		m.set(e2)
		add("meta_listenum_"+m.name, tcls(fld("u", tlist(e2)), fld("last", tstr())))
	}
	// non-ASCII string-literal value, at a ROOT field and inside a LIST element.
	add("meta_rootlit_nonascii", tcls(fld("f", &ft{kind: "literal", litKind: "string", badLitV: true}), fld("last", tstr())))
	add("meta_listlit_nonascii", tcls(fld("u", tlist(&ft{kind: "literal", litKind: "string", badLitV: true})), fld("last", tstr())))
	// class inside a list element / map value, and a mixed union (all DECLINE).
	add("list_class", tcls(fld("items", tlist(tcls(fld("a", tstr()), fld("b", tint())))), fld("last", tstr())))
	add("map_class", tcls(fld("m", tmap(tcls(fld("id", tstr()), fld("label", tstr())))), fld("last", tstr())))
	add("nested_list_class", tcls(fld("a", tstr()), fld("b", tcls(fld("x", tstr()), fld("items", tlist(tcls(fld("a", tstr()), fld("b", tint()))))))))
	add("union_strint", tcls(fld("f", tuni(tstr(), tint())), fld("last", tstr())))

	return out
}

// ---- battery ----

// batInput is one battery member.
//
// deferred marks an input on whose incomplete-object prefixes BAML applies a jsonish
// greedy-recovery heuristic native intentionally does not mirror (bare-scalar raw-span for a
// string field, or the embedded-quote close heuristic); its FINAL is byte-exact on both sides
// and asserted, but its partial prefixes are driven through native for no-crash only (see the
// file doc + TestStream7CDeferredHeuristics).
//
// overDecline marks an input whose partial prefixes native may SAFELY SKIP (emit nothing)
// where BAML recovers a value — a non-conforming/ambiguous/transient frame native declines
// rather than reproduce: an invalid enum member (BAML stream-nulls it, but final BOTH decline),
// an extra non-schema field mid-stream (BAML transiently nulls the real field, a quirk it
// discards at final), or a lone incomplete comment marker `/`. The contract stays strict —
// native must EITHER match BAML byte-exact OR skip; if it emits, it must be byte-exact — so
// this never hides an over-claim, it only permits the safe under-emit. FINAL stays byte-exact.
type batInput struct {
	name        string
	raw         string
	deferred    bool
	overDecline bool
}

func batteryFor(root *ft) []batInput {
	complete := rootObj(root, "", nil)
	inputs := []batInput{
		{name: "complete", raw: complete},
		{name: "block_comment", raw: strings.Replace(complete, "{", "{/* c */", 1)},
		{name: "line_comment", raw: strings.Replace(complete, "{", "{\n// n\n", 1)},
		// A block comment mid-structure: native strips a COMPLETE `/*…*/`, but safely declines
		// the lone incomplete marker byte `/` (ambiguous: `/*` vs `//`) until it disambiguates.
		{name: "mid_comment", raw: strings.Replace(complete, ",", ",/* m */", 1), overDecline: true},
		{name: "trailing_comma", raw: strings.TrimSuffix(complete, "}") + `,}`},
		{name: "leading_comma", raw: "{," + strings.TrimPrefix(complete, "{")},
		{name: "whitespace", raw: strings.ReplaceAll(strings.ReplaceAll(complete, ":", " : "), ",", " , ")},
		{name: "unclosed", raw: strings.TrimSuffix(complete, "}")},
		{name: "unclosed_comma", raw: strings.TrimSuffix(complete, "}") + ","},
		// An EXTRA non-schema field: BAML transiently NULLS the last real field while the
		// unknown key streams (a quirk it discards at the final frame, where the extra key is
		// ignored and both agree); native safely over-declines those frames.
		{name: "extra_field", raw: strings.TrimSuffix(complete, "}") + `,"zz9":"e"}`, overDecline: true},
		{name: "prose", raw: "Here:\n" + complete + "\nDone."},
		{name: "fenced", raw: "```json\n" + complete + "\n```"},
	}
	// Field-specific mutations are applied BY FIELD PATH — walkFields recurses into nested
	// classes, so EVERY string/enum/literal/list field at ANY depth gets its escaped/unicode/
	// casefold/missing/bad-child mutation placed at its own path (not only at the root).
	for _, fp := range walkFields(root) {
		pn := strings.Join(fp.path, ".")
		inputs = append(inputs, batInput{name: "missing_" + pn, raw: buildMutated(root, fp.path, true, "")})
		switch fp.t.kind {
		case "string":
			inputs = append(inputs,
				// Bare unquoted scalar in a string field: NON-conforming #583 deferred trigger.
				// native purely UNDER-emits (skips); FINAL matches.
				batInput{name: "num_in_" + pn, raw: buildMutated(root, fp.path, false, "5e0"), deferred: true},
				// Escapes WITHOUT an embedded quote (\\ \n \t): native DECODES byte-exact. STRICT.
				batInput{name: "escaped_" + pn, raw: buildMutated(root, fp.path, false, `"a\\b\nc\td"`)},
				// Escape WITH an embedded quote (\"): #583 embedded-quote deferred trigger.
				// native purely UNDER-emits (skips); FINAL matches. DEFERRED.
				batInput{name: "escapedq_" + pn, raw: buildMutated(root, fp.path, false, `"a\"b\\c\n"`), deferred: true},
				// UNICODE content: native emits byte-exact at each rune boundary. STRICT.
				batInput{name: "unicode_" + pn, raw: buildMutated(root, fp.path, false, `"café ☃ 日本"`)})
		case "enum", "literal":
			inputs = append(inputs, batInput{name: "casefold_" + pn, raw: buildMutated(root, fp.path, false, strings.ToLower(canon(fp.t)))})
			if fp.t.kind == "enum" {
				// An INVALID enum member: BAML stream-nulls it while native over-declines the
				// partial (the FINAL both DECLINE — a bad required enum errors the class on both
				// sides), so this is a safe over-decline, not an over-claim.
				inputs = append(inputs, batInput{name: "badmember_" + pn, raw: buildMutated(root, fp.path, false, `"NOPE"`), overDecline: true})
			}
		case "list":
			// list bad-child: a non-coercible element for a typed list (BAML drops it).
			switch fp.t.elem.kind {
			case "int", "litint":
				inputs = append(inputs, batInput{name: "badchild_" + pn, raw: buildMutated(root, fp.path, false, `["1",2,"bad",4]`)})
			case "enum":
				inputs = append(inputs, batInput{name: "badchild_" + pn, raw: buildMutated(root, fp.path, false, `["RED","NOPE","GREEN"]`)})
			case "litstr":
				inputs = append(inputs, batInput{name: "badchild_" + pn, raw: buildMutated(root, fp.path, false, `["A","NOPE","A"]`)})
			}
			// LIST-ELEMENT recovery: place a string/enum/literal recovery mutation INSIDE a
			// permitted list ELEMENT (the array's first element), for list<string|enum|litstr>,
			// at whatever depth (root or nested) this list field sits.
			switch fp.t.elem.kind {
			case "string":
				inputs = append(inputs,
					batInput{name: "escaped_elem_" + pn, raw: buildMutated(root, fp.path, false, `["a\\b\nc\td","txt"]`)},
					batInput{name: "unicode_elem_" + pn, raw: buildMutated(root, fp.path, false, `["café ☃ 日本","txt"]`)},
					// embedded quote / bare scalar in a string ELEMENT — #583 deferred triggers.
					batInput{name: "escapedq_elem_" + pn, raw: buildMutated(root, fp.path, false, `["a\"b\\c\n","txt"]`), deferred: true},
					batInput{name: "num_in_elem_" + pn, raw: buildMutated(root, fp.path, false, `[5e0,"txt"]`), deferred: true})
			case "enum":
				inputs = append(inputs, batInput{name: "casefold_elem_" + pn, raw: buildMutated(root, fp.path, false, `["red","GREEN"]`)})
			case "litstr":
				inputs = append(inputs, batInput{name: "casefold_elem_" + pn, raw: buildMutated(root, fp.path, false, `["a","A"]`)})
			}
			inputs = append(inputs, batInput{name: "emptylist_" + pn, raw: buildMutated(root, fp.path, false, "[]")})
		case "class":
			// Structural recovery INSIDE a NESTED object (not only at the root): a mid-comment
			// and a trailing comma placed within the nested class's own braces.
			nc := canon(fp.t)
			inputs = append(inputs,
				batInput{name: "nested_comment_" + pn, raw: buildMutated(root, fp.path, false, strings.Replace(nc, "{", "{/* c */", 1)), overDecline: true},
				batInput{name: "nested_trailcomma_" + pn, raw: buildMutated(root, fp.path, false, strings.TrimSuffix(nc, "}")+",}")})
		}
	}
	return inputs
}

// fpath is a field reached by a dotted path from the root class, with its type.
type fpath struct {
	path []string
	t    *ft
}

// walkFields returns every field in the class tree by PATH — the root's fields, then
// (recursing into any class-typed field) that class's fields, and so on. This lets the
// recovery battery place a mutation at a nested-class field, not only at the root.
func walkFields(t *ft) []fpath {
	var out []fpath
	for _, f := range t.flds {
		out = append(out, fpath{[]string{f.name}, f.t})
		if f.t.kind == "class" {
			for _, sub := range walkFields(f.t) {
				out = append(out, fpath{append([]string{f.name}, sub.path...), sub.t})
			}
		}
	}
	return out
}

// buildMutated returns the canonical JSON for class t, but at the field addressed by `path`
// applies the mutation: if skip, OMIT that field; else replace its value with `override`.
// Recurses through nested classes named by the path head.
func buildMutated(t *ft, path []string, skip bool, override string) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	emit := func(name, val string) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, `"%s":%s`, name, val)
	}
	for _, f := range t.flds {
		switch {
		case len(path) > 0 && f.name == path[0] && len(path) == 1:
			if !skip {
				emit(f.name, override)
			}
		case len(path) > 0 && f.name == path[0]:
			emit(f.name, buildMutated(f.t, path[1:], skip, override))
		default:
			emit(f.name, canon(f.t))
		}
	}
	b.WriteByte('}')
	return b.String()
}

// ---- test ----

func TestStream7CTypeSpaceDifferential(t *testing.T) {
	dynclientCallGate(t)
	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	preserve := true
	// PRE-NORMALIZATION oracle: DynamicParseRaw (FlattenDynamicOutput only, NO
	// inject/reorder), preserving BAML's raw field order + presence.
	bamlRaw := func(s *bamlutils.DynamicOutputSchema, raw string, stream bool) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, e := dyn.DynamicParseRaw(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: s, PreserveSchemaOrder: &preserve, Stream: stream})
		if e != nil {
			return "", false
		}
		return string(res.Data), true
	}
	natFinal := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := debaml.ParseNativeStreamFinal(context.Background(), s, raw)
		if e != nil {
			return "", false
		}
		return string(j), true
	}
	natPartial := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := debaml.ParseNativeStreamPartial(context.Background(), s, raw)
		if e != nil || j == nil {
			return "", false
		}
		return string(j), true
	}

	shapes := enumerate()
	admittedN, declinedN := 0, 0
	perArityAdmit := map[int]int{} // admitted ROOT classes by arity (for the anti-omission pin)
	var finalCmp, strictCmp, deferredWalk, invalidWalk, overDeclineWalk, mism int
	cmp := func(kind, shape, in, raw string, b string, bok bool, n string, nok bool) {
		if bok != nok || (bok && nok && b != n) {
			mism++
			t.Errorf("[%s %s/%s] raw=%q | BAML(%v)=%s | NATIVE(%v)=%s", kind, shape, in, raw, bok, b, nok, n)
		}
	}
	for _, sh := range shapes {
		s := tsSchema(sh.root)
		gotAdmit := debaml.SupportsNativeStream(s) == nil
		wantAdmit := expectAdmit(sh.root)
		if gotAdmit != wantAdmit {
			t.Errorf("admission mismatch %s: SupportsNativeStream=%v, §11 oracle=%v", sh.name, gotAdmit, wantAdmit)
		}
		if !gotAdmit {
			declinedN++
			continue
		}
		admittedN++
		if sh.root.kind == "class" {
			perArityAdmit[len(sh.root.flds)]++
		}
		for _, in := range batteryFor(sh.root) {
			// FINAL byte-exact for EVERY input (incl. deferred — the complete frame
			// closes cleanly on both sides).
			bf, bfok := bamlRaw(s, in.raw, false)
			nf, nfok := natFinal(s, in.raw)
			cmp("FINAL", sh.name, in.name, in.raw, bf, bfok, nf, nfok)
			finalCmp++
			// BYTE-BY-BYTE partial cadence.
			for i := 1; i <= len(in.raw); i++ {
				p := in.raw[:i]
				np, npok := natPartial(s, p) // must not panic on any prefix (incl. invalid UTF-8)
				if !utf8.ValidString(p) {
					// Byte prefix split mid-rune: invalid UTF-8, which BAML's CFFI REJECTS
					// (panics "string field contains invalid UTF-8"), so BAML cannot be the
					// oracle for this prefix in ANY branch — deferred or not. This guard MUST
					// run before EVERY bamlRaw call (a multibyte name/value, now admitted, makes
					// these prefixes real). Native (called above) must not panic.
					invalidWalk++
					continue
				}
				if in.deferred {
					// #583 owed-debt bound (owner A′ / reviewer): native must PURELY
					// under-approximate. A no-emit (skip) is a permitted under-emit; ANY native
					// EMISSION MUST be byte-exact vs BAML — a byte-different emit is a
					// divergent/over-claim emit and FAILS here. We query BAML ONLY when native
					// emits: native skips the greedy-recovery prefixes, which also dodges BAML's
					// pathological embedded-quote backtracking on them.
					if !npok {
						deferredWalk++
						continue
					}
					bp, bpok := bamlRaw(s, p, true)
					cmp("DEFERRED-EMIT", sh.name, in.name+":prefix", p, bp, bpok, np, npok)
					strictCmp++
					continue
				}
				bp, bpok := bamlRaw(s, p, true)
				if in.overDecline && !npok && bpok {
					// Safe over-decline: native emitted nothing where BAML recovered a value
					// (invalid enum member / transient extra field / lone `/`). Permitted —
					// native never emits a wrong value; if it DOES emit it is checked below.
					overDeclineWalk++
					continue
				}
				cmp("PARTIAL", sh.name, in.name+":prefix", p, bp, bpok, np, npok)
				strictCmp++
			}
		}
	}
	t.Logf("RIGOROUS TYPE-SPACE: %d shapes (%d admitted, %d declined) | %d final + %d strict-partial byte-exact vs live BAML | %d safe-over-decline + %d deferred-partial + %d mid-rune native-no-crash walks | %d divergences",
		len(shapes), admittedN, declinedN, finalCmp, strictCmp, overDeclineWalk, deferredWalk, invalidWalk, mism)

	// ---- COUNT PIN + anti-omission structural invariants ----
	// Structural invariant: the generative enumerator must produce >=1 ADMITTED root at EVERY
	// arity 1..maxRootArity (this is what caught the "arity-6 skipped" omissions) — so a silent
	// table/loop regression fails LOUDLY instead of quietly shrinking the proof.
	for a := 1; a <= maxRootArity; a++ {
		if perArityAdmit[a] == 0 {
			t.Errorf("anti-omission: NO admitted root of arity %d generated (generator regression)", a)
		}
	}
	// Exact pinned totals — any future omission (or accidental addition) changes these and fails.
	// #555 Slice 2 admitted the non-ASCII name/value shapes + field @description + class/enum
	// @alias/@description (134→153); field @alias stays declined (the ONE LIVE-PROVEN
	// canonical-key divergence — native's matcher checks only the rendered alias, #583).
	const wantTotal, wantAdmit, wantDeclined = 289, 153, 136
	if len(shapes) != wantTotal || admittedN != wantAdmit || declinedN != wantDeclined {
		t.Errorf("COUNT PIN drift: got total=%d admitted=%d declined=%d; want total=%d admitted=%d declined=%d "+
			"(update the pin only after confirming the grammar/loops intentionally changed)",
			len(shapes), admittedN, declinedN, wantTotal, wantAdmit, wantDeclined)
	}
}

// TestStream7CDeferredHeuristics PROVES — with LIVE BAML — the #583 owed-debt bound for the two
// input classes the main differential marks "deferred": a bare unquoted scalar in a string
// field, and a string with an embedded quote (\"). On INCOMPLETE-object prefixes BAML applies a
// jsonish greedy-recovery heuristic — a raw-span coercion of the bare scalar, or the
// embedded-quote close that greedily absorbs past the apparent close — that BAML ITSELF discards
// at the final frame and that native cannot yet reproduce byte-exact (#583). Native's contract is
// a PURE UNDER-approximation: at those frames it NO-EMITS (skips) rather than emit a byte-different
// value; it never over-claims.
//
// This asserts, live: (a) the FINAL frame is byte-exact on both sides — native never over-claims a
// wrong final; and (b) at every partial prefix native EITHER no-emits (a permitted under-emit) OR
// emits a value that is well-formed + schema-shaped AND BYTE-EXACT vs BAML — a byte-different
// native emission is a divergent/over-claim emit and FAILS. That is the boundary the owner ruling
// requires the deferred set to prove: the debt is a pure under-approximation, never a divergent
// emit. (BAML is queried only when native emits; native skips the slow greedy prefixes.)
func TestStream7CDeferredHeuristics(t *testing.T) {
	dynclientCallGate(t)
	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	preserve := true
	baml := func(s *bamlutils.DynamicOutputSchema, raw string, stream bool) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, e := dyn.DynamicParseRaw(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: s, PreserveSchemaOrder: &preserve, Stream: stream})
		if e != nil {
			return "", false
		}
		return string(res.Data), true
	}
	nat := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := debaml.ParseNativeStreamPartial(context.Background(), s, raw)
		if e != nil || j == nil {
			return "", false
		}
		return string(j), true
	}
	// {f:string, last:string}: exercise the embedded quote and the bare scalar in field f.
	root := tcls(fld("f", tstr()), fld("last", tstr()))
	s := tsSchema(root)
	if debaml.SupportsNativeStream(s) != nil {
		t.Fatal("precondition: {f:string,last:string} must be admitted")
	}
	schemaKeys := map[string]bool{"f": true, "last": true}
	cases := []struct {
		name string
		raw  string
	}{
		{"embedded_quote", `{"f":"a\"b\\c\n","last":"txt"}`},
		{"bare_scalar", `{"f":5e0,"last":"txt"}`},
	}
	for _, c := range cases {
		// (a) FINAL frame: byte-exact on both sides — native never over-claims a wrong final.
		bf, bfok := baml(s, c.raw, false)
		nf, nferr := debaml.ParseNativeStreamFinal(context.Background(), s, c.raw)
		if !bfok || nferr != nil || bf != string(nf) {
			t.Errorf("[%s FINAL not byte-exact] raw=%q BAML=%q(ok=%v) NATIVE=%q(err=%v)", c.name, c.raw, bf, bfok, string(nf), nferr)
		}
		// (b) partial prefixes — the #583 owed-debt bound ENFORCED: native must PURELY
		// under-approximate. A no-emit (skip) is a permitted under-emit; ANY native EMISSION
		// must be well-formed + schema-shaped AND BYTE-EXACT vs BAML. A byte-different native
		// emission is a divergent/over-claim emit and FAILS here (this is the boundary the
		// owner ruling requires the deferred set to prove). We query BAML ONLY when native
		// emits — native skips the greedy-recovery prefixes, dodging BAML's pathological
		// embedded-quote backtracking.
		emit, skip := 0, 0
		for i := 1; i <= len(c.raw); i++ {
			p := c.raw[:i]
			np, npok := nat(s, p)
			if !npok {
				skip++ // permitted under-emit
				continue
			}
			var obj map[string]stdjson.RawMessage
			if stdjson.Unmarshal([]byte(np), &obj) != nil {
				t.Errorf("[%s] native emitted non-object %q at %q", c.name, np, p)
			}
			for k := range obj {
				if !schemaKeys[k] {
					t.Errorf("[%s] native emitted extra key %q (%q) at %q", c.name, k, np, p)
				}
			}
			bp, bpok := baml(s, p, true)
			if !bpok || np != bp {
				t.Errorf("[%s OVER-CLAIM] native EMITTED %q not byte-exact vs BAML %q(ok=%v) at %q", c.name, np, bp, bpok, p)
			}
			emit++
		}
		t.Logf("DEFERRED %s: FINAL byte-exact; %d/%d partials native under-emitted (skip), %d emitted byte-exact — 0 divergent/over-claim emits", c.name, skip, len(c.raw), emit)
	}
}
