package nativeprompt

import (
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// static_typegate.go is the de-BAML Slice 7.1b TYPE GATE for the static template
// allowlist: the small, exact set of enum-typed expressions the closed lexical
// analyzer may accept, and the V3 facts it consults to accept them.
//
// The analyzer stays a closed allowlist over TOKEN SHAPES; this file adds the
// TYPE question it asks about the operands. Both must pass:
//
//	token shape matches an exact admitted spelling
//	  AND every operand resolves in the descriptor's V3 universe
//
// so a shape can never be admitted because it "looks like" an enum comparison,
// and a real enum can never be admitted in a spelling the stock fixture does not
// prove. Every gate here runs AFTER the V3 binder has already bound the
// arguments, so "a directly declared V3 class with an acyclic, fully bindable
// closure" is a fact, not a re-derivation.
//
// # The admitted expressions (and why the neighbours are not)
//
// Let E be a bare declared argument whose V3 type is `enum Color`, OR the exact
// global member token `Color.RED`; let M be an exact global member token; and
// let S be a quoted string literal equal to one of that enum's CANONICAL member
// names.
//
//	{{ arg }}            direct render of a bound scalar / enum / class / list
//	{{ E == S }}         canonical-name equality, either operand order
//	{{ S == E }}
//	{{ E == M }}         same-namespace member equality, either operand order
//	{{ M == E }}
//	{{ S in [M] }}       the two stock-proven one-element membership forms
//	{{ M in [S] }}
//
// # Deviation from the 7.1b scope, stated plainly
//
// Section 4 of the slice scope lists a bare CLASS render (`{{ palette }}`) as an
// admitted row. This implementation does NOT admit it, and the row above says
// "scalar / enum / list-of-those" for that reason. The scope wrote that table
// before the stock differential existed; the differential then measured that
// BAML v0.223's Go client prints a class's fields in Go map order, which is not
// reproducible (see directlyRenderable). Under the scope's own governing rule —
// "differential output, not a locally plausible render, is the authority" — the
// row has to decline. It is a strict UNDER-approximation of the documented
// grammar: nothing outside the table was added, and the affected requests fall
// back to BAML.
//
// A DISPLAY ALIAS is deliberately NOT an identity: `Color.RED == 'rouge'`
// declines even though stock BAML answers `false` for it. That is the parity
// rule, not an omission — 7.1b claims CANONICAL identity only, and admitting the
// alias row would make a display string a second equality language whose full
// surface (ordering, membership, cross-enum alias collisions) has no
// differential. The alias row stays a named negative control in the oracle.
//
// Everything else — `!=`, ordering, `.value`, attribute/index access, a
// multi-element or dynamic list, `color in colors`, a bare enum variable on both
// sides, an unknown member, a cross-enum comparison, filters, methods — declines
// through the existing FeatureEnumComparison / FeatureEnumClassValue umbrellas.

// typeGate is the V3 type view the template analyzer consults. It is built once
// per prepareStatic run from the validated universe and the bound arguments.
type typeGate struct {
	// args maps a declared argument name to its V3 value type. Every entry is
	// already BOUND, so membership here means "directly renderable".
	args map[string]*promptdescriptor.ResolvedValueType
	// universe is the validated V3 universe: the only authority for which enum
	// namespaces exist and which canonical members they declare.
	universe *v3Universe
}

func newTypeGate(bindings []argBinding, u *v3Universe) *typeGate {
	g := &typeGate{args: make(map[string]*promptdescriptor.ResolvedValueType, len(bindings)), universe: u}
	for i := range bindings {
		g.args[bindings[i].name] = bindings[i].vtype
	}
	return g
}

// isRenderableArg reports whether name is a declared, bound argument. Binding
// already refused every value type this slice does not render directly (null,
// nullable, a recursive class closure, an unknown enum member), so a bound
// argument is renderable by construction.
func (g *typeGate) isRenderableArg(name string) bool {
	_, ok := g.args[name]
	return ok
}

// enumOperand is one resolved side of an admitted enum expression.
type enumOperand struct {
	// enumName is the source enum the operand belongs to.
	enumName string
	// member is the canonical member name when the operand is an exact global
	// member token (`Color.RED`), and "" when it is a bare declared argument.
	member string
	// width is how many tokens the operand consumed (1 for an argument, 3 for a
	// member token).
	width int
}

// isMember reports whether the operand is an exact global member token.
func (o enumOperand) isMember() bool { return o.member != "" }

// readEnumOperand resolves an enum operand starting at toks[i], or reports
// ok=false. The two spellings are:
//
//   - a lone identifier naming a declared argument whose V3 type is an enum;
//   - `Enum.MEMBER`, with CONTIGUOUS glue and both halves resolved in V3 (the
//     enum namespace must exist and MEMBER must be one of its canonical members).
//
// The glue requirement is stricter than MiniJinja (which allows `Color . RED`)
// and deliberately so: the admitted surface is exactly the documented spellings.
func (g *typeGate) readEnumOperand(toks []token, i int) (enumOperand, bool) {
	if i >= len(toks) || toks[i].kind != tokIdent {
		return enumOperand{}, false
	}
	name := toks[i].text
	if mjReserved[name] {
		return enumOperand{}, false
	}

	// `Enum.MEMBER` — checked FIRST so an argument that shares a name with an
	// enum can never be read as a namespace. (That collision is already declined
	// at the argument-declaration gate, so this order is belt-and-braces.)
	if i+2 < len(toks) && isOpTok(toks[i+1], ".") && toks[i+2].kind == tokIdent &&
		glued(toks[i], toks[i+1]) && glued(toks[i+1], toks[i+2]) {
		members, ok := g.universe.enumMembers[name]
		if !ok {
			return enumOperand{}, false
		}
		if _, ok := members[toks[i+2].text]; !ok {
			// `Color.NOPE`: a real namespace, not a real member. Declining is what
			// keeps an unknown member from rendering as undefined.
			return enumOperand{}, false
		}
		return enumOperand{enumName: name, member: toks[i+2].text, width: 3}, true
	}

	// A bare declared enum argument.
	vt, ok := g.args[name]
	if !ok || vt.Kind != promptdescriptor.ValueEnum {
		return enumOperand{}, false
	}
	return enumOperand{enumName: vt.EnumName, width: 1}, true
}

// isCanonicalString reports whether toks[i] is an escape-free quoted string
// literal whose value is a CANONICAL member name of enumName.
//
// The escape fence matches the role-literal rule: MiniJinja decodes escapes, so
// an escape-bearing literal is not the exact spelling this gate admits. (A BAML
// member name is an identifier, so a decoded escape could never equal one
// anyway; requiring it keeps the accepted surface stated rather than inferred.)
func (g *typeGate) isCanonicalString(toks []token, i int, enumName string) bool {
	if i >= len(toks) || toks[i].kind != tokString || toks[i].hasEscape {
		return false
	}
	members, ok := g.universe.enumMembers[enumName]
	if !ok {
		return false
	}
	_, ok = members[toks[i].text]
	return ok
}

// matchEnumPredicate matches the exact admitted enum equality / membership token
// shapes. It returns matched=false for everything else, which lets the existing
// decline classifier pick FeatureEnumComparison (or another key) as it does now.
func (g *typeGate) matchEnumPredicate(toks []token) (event, bool) {
	if g.matchEquality(toks) || g.matchMembership(toks) {
		return event{kind: evEnumPredicate}, true
	}
	return event{}, false
}

// matchEquality matches `E == S`, `S == E`, `E == M`, and `M == E`, requiring
// the WHOLE token stream to be the expression (no trailing tokens).
func (g *typeGate) matchEquality(toks []token) bool {
	// S == E : a canonical string on the left. The enum is only known after the
	// right operand resolves, so the string is checked against THAT enum.
	if len(toks) >= 3 && toks[0].kind == tokString && isOpTok(toks[1], "==") {
		right, ok := g.readEnumOperand(toks, 2)
		if ok && 2+right.width == len(toks) && g.isCanonicalString(toks, 0, right.enumName) {
			return true
		}
		return false
	}

	left, ok := g.readEnumOperand(toks, 0)
	if !ok {
		return false
	}
	eq := left.width
	if eq >= len(toks) || !isOpTok(toks[eq], "==") {
		return false
	}
	rest := eq + 1

	// E == S : a canonical member NAME of the SAME enum. A display alias is not
	// an identity here — that row stays declined (see the file doc).
	if rest < len(toks) && toks[rest].kind == tokString {
		return rest+1 == len(toks) && g.isCanonicalString(toks, rest, left.enumName)
	}

	// E == M / M == E : both operands resolve, the SAME namespace, and at least
	// one is an exact member token. Two bare enum arguments are NOT admitted:
	// the stock fixture proves the member forms, not variable-vs-variable.
	right, ok := g.readEnumOperand(toks, rest)
	if !ok || rest+right.width != len(toks) {
		return false
	}
	if left.enumName != right.enumName {
		return false
	}
	return left.isMember() || right.isMember()
}

// matchMembership matches exactly the two stock-proven one-element forms,
// `S in [M]` and `M in [S]`. It is NOT an admission for list construction or for
// membership in general: the list must be a literal with exactly one element,
// and the element must be an exact global member token / a canonical string.
func (g *typeGate) matchMembership(toks []token) bool {
	// S in [M] : string, `in`, `[`, member token, `]`.
	if len(toks) == 7 && toks[0].kind == tokString && isIdentTok(toks[1], "in") && isOpTok(toks[2], "[") {
		m, ok := g.readEnumOperand(toks, 3)
		if !ok || !m.isMember() || m.width != 3 || !isOpTok(toks[6], "]") {
			return false
		}
		return g.isCanonicalString(toks, 0, m.enumName)
	}

	// M in [S] : member token, `in`, `[`, string, `]`.
	if len(toks) == 7 && isIdentTok(toks[3], "in") && isOpTok(toks[4], "[") && isOpTok(toks[6], "]") {
		m, ok := g.readEnumOperand(toks, 0)
		if !ok || !m.isMember() || m.width != 3 {
			return false
		}
		return g.isCanonicalString(toks, 5, m.enumName)
	}

	return false
}
