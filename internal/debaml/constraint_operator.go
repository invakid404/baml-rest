package debaml

import (
	"strings"
)

// The OPERATOR gate.
//
// Rounds 13-20 built a default-decline admission table over every filter, test
// and global callable. Round 20's review found what that architecture cannot
// reach: an OPERATOR is a minijinja VM operation, not a callable, so no wrapper
// runs for it and no signature governs it.
//
//	1 in "1"      stock TRUE — its `contains` STRINGIFIES a non-string needle for
//	              a string haystack — where minijinja-Go evaluates `in` as
//	              right.Contains(left) and the string arm accepts only
//	              other.AsString(), so an integer needle is not found: native
//	              FALSE. `1 not in "1"` is the inverse.
//
// There is no hook to install, so the gate is STRUCTURAL and runs before
// evaluation, in the same posture as the numeric sublanguage of round 6: an
// expression is admitted only when the WHOLE of it parses as a closed predicate
// grammar whose every form has been proven identical to stock, and every other
// expression is refused.
//
// THE GRAMMAR:
//
//	pred     := cmp
//	cmp      := term (('=='|'!='|'<='|'>='|'<'|'>') term)?
//	term     := '-'? primary postfix*
//	primary  := NUMBER | STRING | 'true' | 'false' | 'none' | 'null' | 'this'
//	          | '[' literal-list ']'
//	postfix  := '.' IDENT | '[' INT ']' | '|' FILTER | 'is' 'not'? TEST ARGS?
//
// What that EXCLUDES, and why each exclusion is a divergence rather than a
// convenience:
//
//	in / not in   the reported case. Stock stringifies a non-string needle; the
//	              port does not, and its non-iterable container arm answers false
//	              where stock errors.
//	~             concatenation stringifies its operands, and the two engines do
//	              not render numbers, none or containers alike — the same reason
//	              `string` and `tojson` were withdrawn in round 20.
//	and / or      truthiness. is_true over none, an empty string, an empty
//	  / not       sequence or an empty mapping was never shown to agree, and a
//	              boolean operator turns any disagreement straight into the
//	              answer. (`is not <test>` is a TEST negation, not this operator,
//	              and stays admitted.)
//	if / else     the ternary selects on the same unproven truthiness.
//	+ - * / // %  arithmetic, which keeps its own closed sublanguage
//	  **          ([parseNumeric]) and is checked there instead.
//
// COMPARISONS are admitted but KIND-GATED, because the top-level predicate of a
// constraint is itself a comparison and declining them outright would decline
// everything. Both operands' kinds are INFERRED — exactly, not guessed: `this`
// and every field or index of it come from the value model, a literal is its own
// kind, and each surviving filter has a declared result kind. A comparison is
// admitted only when both sides are the SAME kind and that kind is one the two
// engines compare alike: number, string or bool. Mixed kinds (`1 == "1"`,
// `none == false`, `true == 1`) and container comparisons are refused, because
// each engine reaches them through its own coercion.
//
// An inference that cannot determine a kind returns kindUnknown, which never
// matches anything, so the fail-closed direction is the default.
//
// NESTED POSTFIXES RESOLVE AGAINST THE VALUE THEY REACH, NOT THE ROOT.
//
// This gate's first cut kept only a KIND as it walked a postfix chain, and
// answered every field/subscript question against the parser's root `this`. That
// is an OVER-CLAIM, the one direction the contract forbids. Given
// `this = {a: {name: 5}, name: "x"}`, the chain `this.a.name` was resolved by
// looking `name` up in the ROOT — finding the string "x" — so `this.a.name == "x"`
// was admitted as a proven string/string comparison when the value actually
// reached is the integer 5. The predicate the engines then ran is the mixed-kind
// `5 == "x"` this gate exists to refuse, and its answer would have been served.
// The stale [term.listElem] carried the same defect through index chains.
//
// So a term carries the VALUE it reaches, not just its kind ([term]), and each
// postfix resolves against its immediate subject. A term that is not a path into
// the value model — a literal, a slice result, a filter or test result — carries
// no value, and a field or index postfix on one refuses rather than falling back
// to the root. TestConstraintNestedPostfixResolvesAgainstReachedValue pins the
// classification directly, and the stock differential's `nested/*` group pins
// the same chains against real BAML.

// inferredKind is what the gate can prove about a term before evaluation.
type inferredKind uint8

const (
	kindUnknown inferredKind = iota
	kindNumberK
	kindStringK
	kindBoolK
	kindNoneK
	kindSeqK
	kindMapK
	// kindUndefK is what a MISSING field resolves to. It is a real kind here
	// because `is defined`/`is undefined` are admitted over it, and it is not
	// comparable, so `this.q == 1` still refuses.
	kindUndefK
)

// comparableKind reports whether two operands of this kind are compared
// identically by both engines: numerically for numbers, bytewise for strings,
// and by value for booleans. A container is not one — its comparison walks
// elements through the same coercions this gate exists to avoid — and neither is
// none, whose equality against a bool or a number is exactly the divergence.
func comparableKind(k inferredKind) bool {
	return k == kindNumberK || k == kindStringK || k == kindBoolK
}

// filterResultKind is the kind each ADMITTED filter returns. Only the filters
// that survived round 20's content-parity close appear here; anything else is
// already refused by the signature table, and returning kindUnknown for it keeps
// this gate fail-closed even if that ever changes.
func filterResultKind(name string, subject inferredKind) inferredKind {
	switch name {
	case "length", "count", "sum", "abs":
		return kindNumberK
	case "list", "reverse":
		return kindSeqK
	}
	return kindUnknown
}

// operatorShapeIsProven reports whether the whole expression parses as the
// closed predicate grammar AND every comparison in it has same-kind, comparable
// operands.
func operatorShapeIsProven(this ConstraintValue, expr string) bool {
	// The PURE-ARITHMETIC alternative. `1 + 2 == 3` is not in the predicate
	// grammar — arithmetic deliberately is not — but it has its own closed
	// sublanguage, which [exceedsExactIntegerRange] has already parsed and
	// bounded on the same expression. Its operands are numeric literals by
	// construction, so there is no mixed-kind comparison to gate.
	if _, ok := parseNumeric(expr); ok {
		return true
	}
	p := &predParser{src: expr, this: this}
	if !p.parsePredicate() {
		return false
	}
	p.skipSpace()
	return p.pos == len(p.src)
}

type predParser struct {
	src  string
	pos  int
	this ConstraintValue
}

// term is what the gate has proven about one parsed term.
//
// The kind is always meaningful. The other two fields say HOW a further postfix
// may be resolved, and exactly one of them can be set:
//
//   - isPath marks a term that denotes a place in the VALUE MODEL — `this`, and
//     any chain of field/subscript postfixes off it. reached is then the value
//     that chain arrives at, and the next postfix is answered against it.
//   - listElem is the common element kind of a LIST LITERAL, which has no
//     value-model value to read. kindUnknown means the elements are mixed, and
//     indexing such a literal refuses.
//
// A term with neither — a scalar literal, a slice result, a filter or test
// result — supports no field or subscript postfix at all. That is the whole
// fail-closed rule: the gate never answers a postfix from a value it did not
// actually reach.
type term struct {
	kind     inferredKind
	reached  ConstraintValue
	isPath   bool
	listElem inferredKind
}

// valueTerm is the term for a place in the value model: its kind read from the
// value, and the value retained so the NEXT postfix resolves against it.
func valueTerm(v ConstraintValue) term {
	return term{kind: constraintValueKind(v), reached: v, isPath: true}
}

// refuse is the fail-closed term. Its kind is kindUnknown, which matches nothing.
func refuse() term { return term{} }

func (p *predParser) skipSpace() {
	for p.pos < len(p.src) && isSpaceByte(p.src[p.pos]) {
		p.pos++
	}
}

func (p *predParser) accept(tok string) bool {
	save := p.pos
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	p.pos = save
	return false
}

// acceptWord accepts a keyword only on identifier boundaries, so `isodd` is not
// read as `is odd` and `information` is not read as `in`.
func (p *predParser) acceptWord(word string) bool {
	save := p.pos
	p.skipSpace()
	if !strings.HasPrefix(p.src[p.pos:], word) {
		p.pos = save
		return false
	}
	end := p.pos + len(word)
	if end < len(p.src) && isIdentByte(p.src[end]) {
		p.pos = save
		return false
	}
	if p.pos > 0 && isIdentByte(p.src[p.pos-1]) {
		p.pos = save
		return false
	}
	p.pos = end
	return true
}

func (p *predParser) parsePredicate() bool {
	left, ok := p.parseTerm()
	if !ok {
		return false
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if !p.accept(op) {
			continue
		}
		right, ok := p.parseTerm()
		if !ok {
			return false
		}
		// SAME KIND, and a kind the engines compare alike. This is the round-21
		// centre: the top-level predicate IS a comparison, so a mixed-kind one is
		// a divergence delivered straight to the caller as the answer.
		return left.kind == right.kind && comparableKind(left.kind)
	}
	// No comparison at all. There is no comparison operator to gate, and the
	// remaining grammar contains none of the excluded operators — so the term is
	// admitted whatever its kind. A non-boolean one simply fails the exact
	// "true"/"false" contract in EvaluateConstraint, which is where that rule
	// lives; RenderConstraintExpression legitimately observes such renders, and
	// BAML's own pinned cases (`1`, `[1,2]|sum`) are exactly those.
	return left.kind != kindUnknown
}

func (p *predParser) parseTerm() (term, bool) {
	if p.accept("-") {
		t, ok := p.parseTerm()
		// Negation produces a NEW number. It is not a place in the value model,
		// so the operand's path is deliberately not carried through.
		return term{kind: kindNumberK}, ok && t.kind == kindNumberK
	}
	t, ok := p.parsePrimary()
	if !ok {
		return refuse(), false
	}
	for {
		switch {
		case p.accept("."):
			name, ok := p.parseIdent()
			if !ok {
				return refuse(), false
			}
			// Resolved against t — the value the PRECEDING postfix reached — never
			// against p.this.
			t, ok = p.fieldTerm(t, name)
			if !ok {
				return refuse(), false
			}
		case p.accept("["):
			// A subscript. The bracket rule upstream has already proved the region
			// is a literal, so the only question here is what the subscript reaches,
			// and the subject term's own value answers it exactly.
			t, ok = p.subscriptTerm(t)
			if !ok || !p.accept("]") {
				return refuse(), false
			}
		case p.accept("|"):
			name, ok := p.parseIdent()
			if !ok {
				return refuse(), false
			}
			if p.accept("(") {
				// A filter argument is another expression, and its kind would have
				// to be inferred too. The surviving filters take none.
				return refuse(), false
			}
			if name == "first" || name == "last" {
				t, ok = p.edgeElementTerm(t)
				if !ok {
					return refuse(), false
				}
				continue
			}
			k := filterResultKind(name, t.kind)
			if k == kindUnknown {
				return refuse(), false
			}
			// A filter RESULT is a fresh value the gate did not build, so it carries
			// no path and no further postfix reads the original value.
			next := term{kind: k}
			if k == kindSeqK {
				// `list` and `reverse` are the only sequence-valued filters the
				// signature table admits, and both PERMUTE their subject: neither
				// drops an element nor changes one's kind. A uniform element kind
				// therefore survives them exactly, which is what keeps
				// `|reverse|first` as proven as `|first` — a claim about the
				// ELEMENTS, carried without any claim about the sequence itself.
				next.listElem = t.elementKind()
			}
			t = next
		case p.acceptWord("is"):
			p.acceptWord("not")
			if _, ok := p.parseIdent(); !ok {
				return refuse(), false
			}
			if p.accept("(") {
				if !p.skipTestArgument() {
					return refuse(), false
				}
			}
			t = term{kind: kindBoolK}
		default:
			return t, true
		}
	}
}

// edgeElementTerm resolves `|first` / `|last`.
//
// Unlike a subscript, whose index is written in the expression, the element a
// filter picks is the FILTER's choice. The gate models a filter by a declared
// result kind rather than by re-implementing it, so the claim it makes here is
// the strongest one that holds without asserting WHICH element comes back: the
// common element kind of the sequence. A mixed or unmodelled sequence has none,
// and refuses.
//
// The sequence is the one this term reached — the value model for a path, or a
// uniform list literal — so a nested `this.items|first` reads this.items, not the
// root. An empty sequence yields undefined, which has no common element kind and
// therefore refuses too.
func (p *predParser) edgeElementTerm(t term) (term, bool) {
	if t.kind != kindSeqK {
		return refuse(), false
	}
	elem := t.elementKind()
	if elem == kindUnknown {
		return refuse(), false
	}
	return term{kind: elem}, true
}

// elementKind is the common kind of this term's elements: read out of the value
// model for a path, or carried from a list literal (and through the permuting
// filters) otherwise.
//
// It is kindUnknown — which refuses — whenever the claim cannot be made: mixed
// elements, an empty sequence, a term that is not a sequence at all, and a
// sequence the gate did not build such as a slice result.
func (t term) elementKind() inferredKind {
	if t.isPath {
		return valueListElementKind(t.reached)
	}
	return t.listElem
}

// skipTestArgument consumes a single literal argument to a test — the only shape
// the signature table admits — up to its closing parenthesis.
func (p *predParser) skipTestArgument() bool {
	depth := 1
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '"', '\'':
			j, ok := skipStringLiteral(p.src, p.pos)
			if !ok {
				return false
			}
			p.pos = j
		case '(':
			return false // nesting is not analysed
		case ')':
			depth--
			p.pos++
			return depth == 0
		}
		p.pos++
	}
	return false
}

func (p *predParser) parseIdent() (string, bool) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && isIdentByte(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", false
	}
	return p.src[start:p.pos], true
}

func (p *predParser) parsePrimary() (term, bool) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return refuse(), false
	}
	switch c := p.src[p.pos]; {
	case c == '"' || c == '\'':
		j, ok := skipStringLiteral(p.src, p.pos)
		if !ok {
			return refuse(), false
		}
		p.pos = j + 1
		return term{kind: kindStringK}, true
	case c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.src) && isNumericTokenByte(p.src[p.pos]) {
			p.pos++
		}
		if !isProvablySmallNumber(p.src[start:p.pos]) {
			return refuse(), false
		}
		return term{kind: kindNumberK}, true
	case c == '[':
		j, ok := matchingBracket(p.src, p.pos)
		if !ok || !listLiteralRegionIsSafe(p.src[p.pos+1:j]) {
			return refuse(), false
		}
		// A LIST LITERAL is not a place in the value model, so it carries an
		// element kind instead of a value: `[1,2,3][0]` infers a number without
		// evaluating anything, and a mixed literal infers nothing.
		elem := listLiteralElementKind(p.src[p.pos+1 : j])
		p.pos = j + 1
		return term{kind: kindSeqK, listElem: elem}, true
	case c == '(':
		p.pos++
		if !p.parsePredicate() || !p.accept(")") {
			return refuse(), false
		}
		return term{kind: kindBoolK}, true
	}
	word, ok := p.parseIdent()
	if !ok {
		return refuse(), false
	}
	switch word {
	case "true", "false":
		return term{kind: kindBoolK}, true
	case "none", "null":
		return term{kind: kindNoneK}, true
	case "this":
		// The ROOT, and the only place p.this is read. Everything reached from
		// here is read out of the term the preceding postfix produced.
		return valueTerm(p.this), true
	}
	return refuse(), false // any other identifier
}

// subscriptTerm resolves `x[...]` against the SUBJECT TERM's own value: a string
// key into the mapping it reached, or an integer index into the list it reached.
// Anything else refuses.
func (p *predParser) subscriptTerm(t term) (term, bool) {
	p.skipSpace()
	// A SLICE region — anything containing a colon — yields a sequence in both
	// engines, and the bracket rule has already proved every bound is an integer
	// literal or omitted.
	if close := strings.IndexByte(p.src[p.pos:], ']'); close >= 0 &&
		strings.Contains(p.src[p.pos:p.pos+close], ":") {
		if t.kind != kindSeqK && t.kind != kindStringK {
			return refuse(), false
		}
		p.pos += close
		if t.kind == kindStringK {
			return term{kind: kindStringK}, true
		}
		// A slice of a sequence is a NEW sequence the gate did not build. It has
		// no place in the value model and no proven element kind, so indexing it
		// again — or taking its first/last — refuses.
		return term{kind: kindSeqK}, true
	}
	if p.pos < len(p.src) && (p.src[p.pos] == '"' || p.src[p.pos] == '\'') {
		j, ok := skipStringLiteral(p.src, p.pos)
		// A string key needs a mapping the gate can actually read. A kindMapK term
		// that is not a path — which the grammar cannot produce today, since there
		// is no map literal and no filter declaring a mapping result — refuses
		// rather than falling back to the root.
		if !ok || t.kind != kindMapK || !t.isPath {
			return refuse(), false
		}
		key := p.src[p.pos+1 : j]
		p.pos = j + 1
		for _, e := range t.reached.entries {
			if e.Key == key {
				return valueTerm(e.Value), true
			}
		}
		return term{kind: kindUndefK}, true
	}
	neg := false
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		neg = true
		p.pos++
	}
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start {
		return refuse(), false
	}
	if t.kind == kindStringK {
		return term{kind: kindStringK}, true // a character of a string is a string in both
	}
	if t.kind != kindSeqK {
		return refuse(), false
	}
	if !t.isPath {
		// A LIST LITERAL, or a slice result. There is no value to index, so the
		// only available claim is the literal's uniform element kind; a mixed
		// literal and an unmodelled sequence both refuse.
		if t.listElem == kindUnknown {
			return refuse(), false
		}
		return term{kind: t.listElem}, true
	}
	idx := 0
	for _, c := range p.src[start:p.pos] {
		idx = idx*10 + int(c-'0')
	}
	if neg {
		idx = len(t.reached.list) - idx
	}
	if idx < 0 || idx >= len(t.reached.list) {
		return term{kind: kindUndefK}, true
	}
	return valueTerm(t.reached.list[idx]), true
}

// fieldTerm resolves `x.name` against the SUBJECT TERM's own value, so the kind
// is read rather than guessed — and read from the mapping the preceding postfix
// actually reached. A field of anything but a mapping the gate can read refuses.
func (p *predParser) fieldTerm(t term, name string) (term, bool) {
	if t.kind != kindMapK || !t.isPath {
		return refuse(), false
	}
	for _, e := range t.reached.entries {
		if e.Key == name {
			return valueTerm(e.Value), true
		}
	}
	// A missing field is undefined, which `is defined` / `is undefined` reads.
	// It is not a path, so a FURTHER postfix off it refuses.
	return term{kind: kindUndefK}, true
}

// constraintValueKind maps a value-model value to the gate's kind lattice.
func constraintValueKind(v ConstraintValue) inferredKind {
	switch v.kind {
	case ConstraintKindInt, ConstraintKindFloat:
		return kindNumberK
	case ConstraintKindString, ConstraintKindEnum:
		return kindStringK
	case ConstraintKindBool:
		return kindBoolK
	case ConstraintKindNull:
		return kindNoneK
	case ConstraintKindList:
		return kindSeqK
	case ConstraintKindMap, ConstraintKindClass:
		return kindMapK
	}
	return kindUnknown
}

// listLiteralElementKind is the common kind of a literal list's elements, or
// kindUnknown when they are mixed — in which case indexing it refuses, because
// the element kind cannot be established without evaluating.
func listLiteralElementKind(region string) inferredKind {
	if strings.TrimSpace(region) == "" {
		return kindUnknown
	}
	common := kindUnknown
	for _, element := range splitTopLevelCommas(region) {
		t := strings.TrimSpace(element)
		var k inferredKind
		switch {
		case t == "":
			return kindUnknown
		case t[0] == '"' || t[0] == '\'':
			k = kindStringK
		default:
			k = kindNumberK
		}
		if common == kindUnknown {
			common = k
		} else if common != k {
			return kindUnknown
		}
	}
	return common
}

// valueListElementKind is the common element kind of a value-model list, or
// kindUnknown when the elements are mixed.
func valueListElementKind(v ConstraintValue) inferredKind {
	if len(v.list) == 0 {
		return kindUnknown
	}
	common := constraintValueKind(v.list[0])
	for _, item := range v.list[1:] {
		if constraintValueKind(item) != common {
			return kindUnknown
		}
	}
	return common
}
