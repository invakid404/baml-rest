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
	// listElem is the common element kind of the most recent LIST LITERAL
	// primary, or kindUnknown when its elements are not all the same kind. It
	// lets `[1,2,3][0]` infer a number without evaluating anything.
	listElem inferredKind
}

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
		return left == right && comparableKind(left)
	}
	// No comparison at all. There is no comparison operator to gate, and the
	// remaining grammar contains none of the excluded operators — so the term is
	// admitted whatever its kind. A non-boolean one simply fails the exact
	// "true"/"false" contract in EvaluateConstraint, which is where that rule
	// lives; RenderConstraintExpression legitimately observes such renders, and
	// BAML's own pinned cases (`1`, `[1,2]|sum`) are exactly those.
	return left != kindUnknown
}

func (p *predParser) parseTerm() (inferredKind, bool) {
	if p.accept("-") {
		k, ok := p.parseTerm()
		return k, ok && k == kindNumberK
	}
	k, ok := p.parsePrimary()
	if !ok {
		return kindUnknown, false
	}
	for {
		switch {
		case p.accept("."):
			name, ok := p.parseIdent()
			if !ok {
				return kindUnknown, false
			}
			k, ok = p.fieldKind(k, name)
			if !ok {
				return kindUnknown, false
			}
		case p.accept("["):
			// A subscript. The bracket rule upstream has already proved the region
			// is a literal, so the only question here is the resulting KIND, and the
			// value model answers it exactly.
			k, ok = p.subscriptKind(k)
			if !ok || !p.accept("]") {
				return kindUnknown, false
			}
		case p.accept("|"):
			name, ok := p.parseIdent()
			if !ok {
				return kindUnknown, false
			}
			if p.accept("(") {
				// A filter argument is another expression, and its kind would have
				// to be inferred too. The surviving filters take none.
				return kindUnknown, false
			}
			if name == "first" || name == "last" {
				// The element kind, taken from the sequence this term came from:
				// the value model for `this`, or a uniform list literal. A mixed
				// sequence leaves it unknown, and unknown refuses.
				if k != kindSeqK || p.listElem == kindUnknown {
					return kindUnknown, false
				}
				k = p.listElem
				continue
			}
			k = filterResultKind(name, k)
			if k == kindUnknown {
				return kindUnknown, false
			}
		case p.acceptWord("is"):
			p.acceptWord("not")
			if _, ok := p.parseIdent(); !ok {
				return kindUnknown, false
			}
			if p.accept("(") {
				if !p.skipTestArgument() {
					return kindUnknown, false
				}
			}
			k = kindBoolK
		default:
			return k, true
		}
	}
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

func (p *predParser) parsePrimary() (inferredKind, bool) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return kindUnknown, false
	}
	switch c := p.src[p.pos]; {
	case c == '"' || c == '\'':
		j, ok := skipStringLiteral(p.src, p.pos)
		if !ok {
			return kindUnknown, false
		}
		p.pos = j + 1
		return kindStringK, true
	case c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.src) && isNumericTokenByte(p.src[p.pos]) {
			p.pos++
		}
		if !isProvablySmallNumber(p.src[start:p.pos]) {
			return kindUnknown, false
		}
		return kindNumberK, true
	case c == '[':
		j, ok := matchingBracket(p.src, p.pos)
		if !ok || !listLiteralRegionIsSafe(p.src[p.pos+1:j]) {
			return kindUnknown, false
		}
		p.listElem = listLiteralElementKind(p.src[p.pos+1 : j])
		p.pos = j + 1
		return kindSeqK, true
	case c == '(':
		p.pos++
		if !p.parsePredicate() || !p.accept(")") {
			return kindUnknown, false
		}
		return kindBoolK, true
	}
	word, ok := p.parseIdent()
	if !ok {
		return kindUnknown, false
	}
	switch word {
	case "true", "false":
		return kindBoolK, true
	case "none", "null":
		return kindNoneK, true
	case "this":
		k := constraintValueKind(p.this)
		if k == kindSeqK {
			p.listElem = valueListElementKind(p.this)
		}
		return k, true
	}
	return kindUnknown, false // any other identifier
}

// subscriptKind resolves `x[...]` against the VALUE MODEL: a string key into a
// mapping, or an integer index into a list. Anything else refuses.
func (p *predParser) subscriptKind(subject inferredKind) (inferredKind, bool) {
	p.skipSpace()
	// A SLICE region — anything containing a colon — yields a sequence in both
	// engines, and the bracket rule has already proved every bound is an integer
	// literal or omitted.
	if close := strings.IndexByte(p.src[p.pos:], ']'); close >= 0 &&
		strings.Contains(p.src[p.pos:p.pos+close], ":") {
		if subject != kindSeqK && subject != kindStringK {
			return kindUnknown, false
		}
		p.pos += close
		if subject == kindStringK {
			return kindStringK, true
		}
		return kindSeqK, true
	}
	if p.pos < len(p.src) && (p.src[p.pos] == '"' || p.src[p.pos] == '\'') {
		j, ok := skipStringLiteral(p.src, p.pos)
		if !ok || subject != kindMapK {
			return kindUnknown, false
		}
		key := p.src[p.pos+1 : j]
		p.pos = j + 1
		for _, e := range p.this.entries {
			if e.Key == key {
				return constraintValueKind(e.Value), true
			}
		}
		return kindUndefK, true
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
		return kindUnknown, false
	}
	if subject == kindStringK {
		return kindStringK, true // a character of a string is a string in both
	}
	if subject != kindSeqK {
		return kindUnknown, false
	}
	if p.listElem != kindUnknown {
		return p.listElem, true // indexing a LIST LITERAL of uniform elements
	}
	idx := 0
	for _, c := range p.src[start:p.pos] {
		idx = idx*10 + int(c-'0')
	}
	if neg {
		idx = len(p.this.list) - idx
	}
	if idx < 0 || idx >= len(p.this.list) {
		return kindUndefK, true
	}
	return constraintValueKind(p.this.list[idx]), true
}

// fieldKind resolves `this.name` against the VALUE MODEL, so the kind is read
// rather than guessed. A subscript is handled the same way by the bracket rule
// upstream; a field of anything but a mapping refuses.
func (p *predParser) fieldKind(subject inferredKind, name string) (inferredKind, bool) {
	if subject != kindMapK {
		return kindUnknown, false
	}
	for _, e := range p.this.entries {
		if e.Key == name {
			return constraintValueKind(e.Value), true
		}
	}
	return kindUndefK, true // a missing field is undefined, which `is undefined` reads
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
