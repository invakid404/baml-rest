package nativeprompt

import (
	"fmt"
	"strings"
)

// This file is the closed-allowlist static template analyzer. It is the crux of
// the narrow static claim: SupportsStatic must NOT be a denylist over arbitrary
// static source (that would out-claim BAML). Instead a lexical segment scanner
// splits the raw prompt into text / comment / statement / expression segments
// and inspects ONLY the MiniJinja tags — never matching forbidden words in
// ordinary prompt text. Every expression must be one of the exact accepted
// output-producing forms:
//
//	{{ argument_name }}      -> direct primitive interpolation
//	{{ ctx.output_format }}  -> bare output format
//	{{ _.role("system") }}   -> role open (positional literal role)
//	{{ _.role(role="user") }}
//	{{ _.chat("assistant") }}
//	{{ _.chat(role="user") }}
//
// with ordinary raw text / comments around them and {{- -}} whitespace-control
// variants. EVERYTHING else — any {% %} statement, filter, comparison, method
// call, subscript, attribute access, unknown global, exotic role shape, or bad
// chat layout — classifies into a specific decline. There is no "try rendering
// and see": the analyzer, not a successful MiniJinja compile, defines support.

// templatePlan is the analyzer's verdict for an accepted prompt: the ordered
// content/role events and whether bare ctx.output_format is referenced. Chat
// layout is validated LATER (in prepareStatic), once the bound argument values
// and the rendered output-format block are known, so emptiness can be decided
// from actual rendered content rather than the source shape alone.
type templatePlan struct {
	// events is the ordered stream of content and role events.
	events []event
	// usesOutputFormat is true iff the prompt references bare ctx.output_format,
	// so the preparer renders the output-format block only when needed.
	usesOutputFormat bool
}

// analyzeTemplate runs the closed-allowlist scan over the RAW prompt source
// (step 5). decls is the validated argument set; a bare {{ name }} is accepted
// only when name is a declared argument. It returns the ordered event plan or
// the first decline. It does NOT validate chat layout — that is value-aware and
// runs after argument binding and output-format rendering.
func analyzeTemplate(src string, decls []argDecl) (*templatePlan, error) {
	// The reserved-delimiter fence is applied byte-faithfully to the RENDERED
	// output (validateRenderedMarkers), not to the raw source here: a marker in a
	// comment (removed before render) or a marker synthesized only after
	// dedent/interpolation must be judged on what lower actually sees.
	argNames := make(map[string]bool, len(decls))
	for _, d := range decls {
		argNames[d.name] = true
	}

	segs, err := scanSegments(src)
	if err != nil {
		return nil, err
	}

	var events []event
	usesOutputFormat := false
	for _, s := range segs {
		switch s.kind {
		case segText:
			if strings.TrimSpace(s.body) != "" {
				events = append(events, event{kind: evText})
			}
		case segComment:
			// Ordinary comments (and the scoped {#- ... -#} hyphen control) are
			// inert and allowed. A `+` whitespace-control marker, however, changes
			// surrounding prompt bytes under trim_blocks/lstrip_blocks and is
			// outside the proven grammar, so it declines instead of silently
			// accepting.
			if s.plusControl {
				return nil, decline(FeatureUnrecognizedPrompt,
					"{#+ ... +#} whitespace-control comments are not supported")
			}
		case segStmt:
			return nil, declineStatement(s.body)
		case segExpr:
			ev, err := classifyExpr(s.body, argNames)
			if err != nil {
				return nil, err
			}
			if ev.kind == evOutputFormat {
				usesOutputFormat = true
			}
			events = append(events, ev)
		}
	}

	return &templatePlan{events: events, usesOutputFormat: usesOutputFormat}, nil
}

// --- lexical segment scanner --------------------------------------------------

type segKind int

const (
	segText    segKind = iota // literal text between tags
	segExpr                   // {{ ... }}
	segStmt                   // {% ... %}
	segComment                // {# ... #}
)

// segment is one lexical piece of the source. body is the raw text for segText
// and the trimmed inner (the scoped `-` whitespace-control marker and
// surrounding MiniJinja whitespace removed) for the tag kinds. plusControl
// records whether the raw tag carried a `+` whitespace-control marker at either
// edge ({{+ ... +}} / {#+ ... +#}); the `+` variants are outside the proven
// grammar and decline.
type segment struct {
	kind        segKind
	body        string
	plusControl bool
}

// tagOpen pairs an opening delimiter with its required close.
type tagOpen struct {
	open  string
	close string
	kind  segKind
}

var tagOpens = []tagOpen{
	{"{{", "}}", segExpr},
	{"{%", "%}", segStmt},
	{"{#", "#}", segComment},
}

// scanSegments tokenizes src into ordered text/expr/stmt/comment segments. It is
// a conservative lexer, not a full parser: it is only ever used to feed the
// allowlist classifier, so a mis-parse can at worst reject (fail closed), never
// over-accept. An unterminated tag declines.
func scanSegments(src string) ([]segment, error) {
	var segs []segment
	i := 0
	for i < len(src) {
		nextIdx := -1
		var open tagOpen
		for _, t := range tagOpens {
			if idx := strings.Index(src[i:], t.open); idx >= 0 {
				if nextIdx < 0 || idx < nextIdx {
					nextIdx = idx
					open = t
				}
			}
		}
		if nextIdx < 0 {
			segs = append(segs, segment{kind: segText, body: src[i:]})
			break
		}
		if nextIdx > 0 {
			segs = append(segs, segment{kind: segText, body: src[i : i+nextIdx]})
		}
		start := i + nextIdx + len(open.open)
		rel := strings.Index(src[start:], open.close)
		if rel < 0 {
			return nil, decline(FeatureUnrecognizedPrompt,
				fmt.Sprintf("unterminated %q tag", open.open))
		}
		inner := src[start : start+rel]
		segs = append(segs, segment{
			kind:        open.kind,
			body:        trimTagInner(inner),
			plusControl: hasPlusControl(inner),
		})
		i = start + rel + len(open.close)
	}
	return segs, nil
}

// trimTagInner strips a single leading/trailing hyphen whitespace-control marker
// (`-`, as in {{- ... -}}) and then surrounding MiniJinja whitespace, leaving
// the bare expression/statement/comment text. Only the scoped hyphen variant is
// normalized; the `+` variants are detected separately ([hasPlusControl]) and
// decline. It trims ONLY MiniJinja lexical whitespace (ASCII space/tab/CR/LF) —
// never arbitrary Unicode whitespace — so a non-lexer byte (form-feed, NBSP, …)
// survives to the tokenizer and forces a decline rather than a silent accept.
func trimTagInner(s string) string {
	if len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return mjTrimSpace(s)
}

// hasPlusControl reports whether the RAW tag inner carries a `+` whitespace
// control marker at either edge ({{+ ... +}} / {#+ ... +#}). It is computed
// before trimming so the marker is unambiguous (a `+` in the middle of an
// expression is a plain operator handled by the tokenizer, not a control mark).
func hasPlusControl(inner string) bool {
	return len(inner) > 0 && (inner[0] == '+' || inner[len(inner)-1] == '+')
}

// isMJSpace reports whether b is one of MiniJinja's lexical whitespace bytes:
// ASCII space, tab, newline, carriage return. NOTHING else (not form-feed,
// vertical tab, NBSP, or any other Unicode space) is insignificant to the
// MiniJinja lexer.
func isMJSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// mjTrimSpace trims leading/trailing MiniJinja whitespace bytes from s.
func mjTrimSpace(s string) string {
	i := 0
	for i < len(s) && isMJSpace(s[i]) {
		i++
	}
	j := len(s)
	for j > i && isMJSpace(s[j-1]) {
		j--
	}
	return s[i:j]
}

// --- ordered layout events ----------------------------------------------------

type evtKind int

const (
	evText         evtKind = iota // non-whitespace literal text (message content)
	evInterp                      // {{ arg }} (message content)
	evOutputFormat                // {{ ctx.output_format }} (message content)
	evRole                        // {{ _.role/_.chat(...) }} (opens a message)
)

type event struct {
	kind evtKind
	role string // set for evRole
	arg  string // set for evInterp: the declared argument name being interpolated
}

// --- statement / expression classification ------------------------------------

// classifyExpr maps one {{ ... }} inner expression to a layout event or the
// matching decline. It is TOKEN-AWARE: the body is lexed with MiniJinja's own
// whitespace rules (only ASCII space/tab/CR/LF separate tokens; identifiers are
// never split or fused by whitespace) and the token stream must be EXACTLY one
// of the allowlisted forms. Anything the MiniJinja lexer would itself reject
// (form-feed, NBSP, an unterminated string, …) fails to tokenize and declines,
// so a nil support result can never lead RenderStatic to a raw compile error.
func classifyExpr(inner string, argNames map[string]bool) (event, error) {
	toks, ok := mjTokenize(inner)
	if ok {
		if ev, matched := matchAllowlist(toks, argNames); matched {
			return ev, nil
		}
	}
	// Not an accepted form (or not even lexable): pick the specific decline key.
	return event{}, classifyExprDecline(inner, toks, ok)
}

// matchAllowlist returns the event for a token stream that is EXACTLY one of the
// accepted output-producing forms, or matched=false otherwise. Whitespace is
// already insignificant between tokens (the lexer dropped it), but the operator
// "glue" of the structured forms (`_.role`, `_.chat`, `ctx.output_format`) must
// be CONTIGUOUS in the source — a whitespace-broken spelling such as `_ .role`
// or `ctx . output_format` is declined (stricter than MiniJinja, which is
// allowed) so the accepted surface stays exactly the documented spellings.
func matchAllowlist(toks []token, argNames map[string]bool) (event, bool) {
	// {{ argument_name }} — a lone identifier naming a declared argument. A
	// MiniJinja keyword/literal (true/false/none/in/is/…) is not a variable
	// reference, so it is never treated as a bare-arg interpolation.
	if len(toks) == 1 && toks[0].kind == tokIdent {
		name := toks[0].text
		if argNames[name] && !mjReserved[name] {
			return event{kind: evInterp, arg: name}, true
		}
		return event{}, false
	}

	// {{ ctx.output_format }} — bare attribute access, contiguous glue.
	if len(toks) == 3 &&
		isIdentTok(toks[0], "ctx") && isOpTok(toks[1], ".") && isIdentTok(toks[2], "output_format") &&
		glued(toks[0], toks[1]) && glued(toks[1], toks[2]) {
		return event{kind: evOutputFormat}, true
	}

	// {{ _.role("system") }} / {{ _.chat("assistant") }} — positional literal role.
	// The role string must be an exact standard role with NO escape (an escaped
	// literal could decode to a different, custom role in MiniJinja).
	if len(toks) == 6 &&
		isIdentTok(toks[0], "_") && isOpTok(toks[1], ".") && isRoleMethod(toks[2]) &&
		glued(toks[0], toks[1]) && glued(toks[1], toks[2]) &&
		isOpTok(toks[3], "(") && isStdRoleToken(toks[4]) && isOpTok(toks[5], ")") {
		return event{kind: evRole, role: toks[4].text}, true
	}

	// {{ _.role(role="user") }} / {{ _.chat(role="user") }} — role= kwarg form.
	if len(toks) == 8 &&
		isIdentTok(toks[0], "_") && isOpTok(toks[1], ".") && isRoleMethod(toks[2]) &&
		glued(toks[0], toks[1]) && glued(toks[1], toks[2]) &&
		isOpTok(toks[3], "(") && isIdentTok(toks[4], "role") && isOpTok(toks[5], "=") &&
		isStdRoleToken(toks[6]) && isOpTok(toks[7], ")") {
		return event{kind: evRole, role: toks[6].text}, true
	}

	return event{}, false
}

// classifyExprDecline selects the specific decline key for a non-accepted
// expression. These checks only ever DECLINE, so conservative matching cannot
// cause a wrong accept. A body the MiniJinja lexer itself rejects (lexOK=false)
// is the catch-all FeatureUnrecognizedPrompt.
func classifyExprDecline(inner string, toks []token, lexOK bool) error {
	if !lexOK {
		return decline(FeatureUnrecognizedPrompt,
			fmt.Sprintf("expression is not valid MiniJinja (non-lexer whitespace/character): %q", inner))
	}
	if len(toks) == 0 {
		return decline(FeatureUnrecognizedPrompt, "empty {{ }} expression")
	}

	// Filters: any pipe, including replace/format/regex_match/sum.
	if hasOpTok(toks, "|") {
		return decline(FeatureUnknownFilter,
			fmt.Sprintf("filters are not supported in static prompts: %q", inner))
	}
	// Comparisons/containment: fences the known MiniJinja-Go value_cmp divergence.
	if hasComparisonToken(toks) {
		return decline(FeatureEnumComparison,
			fmt.Sprintf("comparisons/containment are not supported: %q", inner))
	}
	// ctx.* — only bare ctx.output_format is accepted.
	if isIdentTok(toks[0], "ctx") {
		return classifyCtxDecline(inner, toks)
	}
	// _.role / _.chat — any non-exact role helper shape.
	if isIdentTok(toks[0], "_") {
		return decline(FeatureRoleCallShape,
			fmt.Sprintf("unsupported role/chat call shape: %q", inner))
	}
	// Method call spelling `.name(` (e.g. .format()).
	if hasMethodCall(toks) {
		return decline(FeaturePyFormatMethod,
			fmt.Sprintf("method calls are not supported: %q", inner))
	}
	// Subscript / indexing.
	if hasOpTok(toks, "[") || hasOpTok(toks, "]") {
		return decline(FeatureUnrecognizedPrompt,
			fmt.Sprintf("subscript/indexing is not supported: %q", inner))
	}
	// Attribute access on a global / enum / class.
	if hasOpTok(toks, ".") {
		return decline(FeatureEnumClassValue,
			fmt.Sprintf("enum/class/global attribute access is not supported: %q", inner))
	}
	// A lone identifier that is not a declared argument (or is a keyword).
	if len(toks) == 1 && toks[0].kind == tokIdent {
		return decline(FeatureUnrecognizedPrompt,
			fmt.Sprintf("unknown variable/global %q", inner))
	}
	// Everything else (literals, operators, function calls, ...).
	return decline(FeatureUnrecognizedPrompt,
		fmt.Sprintf("expression outside the static allowlist: %q", inner))
}

// classifyCtxDecline keys a non-accepted `ctx`-headed expression. Every
// callable/bracket/subscript spelling of output_format declines
// FeatureCallableOutputFmt; any other ctx member declines FeatureUnsupportedCtx.
func classifyCtxDecline(inner string, toks []token) error {
	// ctx.output_format( ... )  or  ctx.output_format[ ... ]
	if len(toks) >= 4 &&
		isOpTok(toks[1], ".") && isIdentTok(toks[2], "output_format") &&
		(isOpTok(toks[3], "(") || isOpTok(toks[3], "[")) {
		return decline(FeatureCallableOutputFmt,
			fmt.Sprintf("callable/bracket ctx.output_format spelling is not supported; only bare ctx.output_format: %q", inner))
	}
	// ctx["output_format"] ...  (bracket-spelled member access)
	if len(toks) >= 4 &&
		isOpTok(toks[1], "[") && toks[2].kind == tokString && toks[2].text == "output_format" && isOpTok(toks[3], "]") {
		return decline(FeatureCallableOutputFmt,
			fmt.Sprintf("callable/bracket ctx.output_format spelling is not supported; only bare ctx.output_format: %q", inner))
	}
	return decline(FeatureUnsupportedCtx,
		fmt.Sprintf("only bare ctx.output_format is supported; %q is not", inner))
}

// hasMethodCall reports whether the token stream contains a `. name (`
// method-call spelling (e.g. `.format(`).
func hasMethodCall(toks []token) bool {
	for i := 0; i+2 < len(toks); i++ {
		if isOpTok(toks[i], ".") && toks[i+1].kind == tokIdent && isOpTok(toks[i+2], "(") {
			return true
		}
	}
	return false
}

// hasComparisonToken reports whether the token stream contains a comparison or
// containment operator/keyword. A lone `=` (the role= kwarg) is not a comparison.
func hasComparisonToken(toks []token) bool {
	for _, t := range toks {
		if t.kind == tokOp {
			switch t.text {
			case "==", "!=", "<", "<=", ">", ">=":
				return true
			}
		}
		if t.kind == tokIdent && (t.text == "in" || t.text == "not" || t.text == "is") {
			return true
		}
	}
	return false
}

// declineStatement rejects every {% ... %} statement. macro/import/include/
// extends/from/call get the macro key; the rest get the catch-all.
func declineStatement(inner string) error {
	switch leadingIdent(inner) {
	case "macro", "import", "include", "extends", "from", "call":
		return decline(FeatureMacro,
			fmt.Sprintf("{%% %s %%} blocks are not supported", leadingIdent(inner)))
	default:
		return decline(FeatureUnrecognizedPrompt,
			fmt.Sprintf("{%% ... %%} statements are not supported: %q", inner))
	}
}

// leadingIdent returns the maximal leading identifier of s (matching
// [A-Za-z_][A-Za-z0-9_]*), or "" when s does not start with one.
func leadingIdent(s string) string {
	n := 0
	for n < len(s) {
		c := s[n]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (n > 0 && c >= '0' && c <= '9') {
			n++
			continue
		}
		break
	}
	return s[:n]
}

// --- MiniJinja-faithful token lexer -------------------------------------------

type tokKind int

const (
	tokIdent  tokKind = iota // [A-Za-z_][A-Za-z0-9_]*
	tokString                // quoted literal; text is the unquoted value
	tokNumber                // digits (with optional dot); allowlist-irrelevant
	tokOp                    // a single- or multi-char operator/punctuation
)

// token is one lexed unit plus its byte span in the tag body, so callers can
// require operator "glue" contiguity.
type token struct {
	kind tokKind
	text string
	// hasEscape is set on a tokString that contained a backslash escape. A role
	// literal carrying an escape is NOT the accepted standard-literal-role form
	// (MiniJinja decodes escapes, so the decoded role could differ), so the
	// allowlist rejects it — only the exact, escape-free roles are accepted.
	hasEscape bool
	start     int
	end       int
}

// mjTokenize lexes a tag body into MiniJinja tokens, or reports ok=false when a
// byte cannot begin a token under MiniJinja's lexer (form-feed, NBSP and other
// non-ASCII-whitespace, an unterminated string, ...). Only ASCII space/tab/CR/LF
// separate tokens; whitespace never splits or fuses an identifier. A lex failure
// means MiniJinja itself would reject the tag, so the caller declines.
func mjTokenize(s string) ([]token, bool) {
	var toks []token
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case isMJSpace(c):
			i++
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentContinue(s[j]) {
				j++
			}
			toks = append(toks, token{kind: tokIdent, text: s[i:j], start: i, end: j})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			toks = append(toks, token{kind: tokNumber, text: s[i:j], start: i, end: j})
			i = j
		case c == '"' || c == '\'':
			val, j, esc, ok := scanString(s, i)
			if !ok {
				return nil, false
			}
			toks = append(toks, token{kind: tokString, text: val, hasEscape: esc, start: i, end: j})
			i = j
		default:
			op, j, ok := scanOp(s, i)
			if !ok {
				return nil, false
			}
			toks = append(toks, token{kind: tokOp, text: op, start: i, end: j})
			i = j
		}
	}
	return toks, true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentContinue(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// scanString reads a quoted string starting at s[i] (the opening quote) and
// returns its content, the index just past the closing quote, whether it
// contained a backslash escape, and ok.
//
// It does NOT reproduce MiniJinja's escape decoder. Instead it fails closed on
// anything that would decode/validate differently: only the simple single-char
// escapes MiniJinja definitely supports (\\ \" \' \n \r \t) are accepted, and
// they set hasEscape so the string can never match a standard literal role
// (MiniJinja would decode e.g. "sys\tem" to a tab-bearing custom role). Any
// other escape — the multi-char \u / \x forms, or an unknown escape letter that
// MiniJinja rejects outright — makes the whole tag fail to lex (ok=false), so
// the caller declines instead of manufacturing a role or hitting a raw compile
// error. An unterminated string also returns ok=false.
func scanString(s string, i int) (val string, next int, hasEscape bool, ok bool) {
	q := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); {
		c := s[j]
		if c == '\\' {
			if j+1 >= len(s) {
				return "", 0, false, false // dangling backslash: unterminated
			}
			switch s[j+1] {
			case '\\', '"', '\'', 'n', 'r', 't':
				hasEscape = true
				b.WriteByte(s[j+1])
				j += 2
				continue
			default:
				// \u, \x, or any escape this narrow gate does not validate.
				// MiniJinja decodes/validates these; rather than reproduce that,
				// fail closed so a nil support result cannot diverge at render.
				return "", 0, false, false
			}
		}
		if c == q {
			return b.String(), j + 1, hasEscape, true
		}
		b.WriteByte(c)
		j++
	}
	return "", 0, false, false
}

// mjOps is the set of operator/punctuation byte spellings the MiniJinja lexer
// recognizes that this gate needs to lex (multi-char first for greedy matching).
// A byte outside this set (and not whitespace/ident/digit/quote) fails to lex.
var mjMultiOps = []string{"==", "!=", "<=", ">=", "**", "//"}

func scanOp(s string, i int) (string, int, bool) {
	for _, op := range mjMultiOps {
		if strings.HasPrefix(s[i:], op) {
			return op, i + len(op), true
		}
	}
	switch s[i] {
	case '(', ')', '[', ']', '{', '}', '.', ',', ':', ';', '|', '~', '+', '-', '*', '/', '%', '<', '>', '=', '!':
		return s[i : i+1], i + 1, true
	}
	return "", 0, false
}

func isOpTok(t token, op string) bool      { return t.kind == tokOp && t.text == op }
func isIdentTok(t token, name string) bool { return t.kind == tokIdent && t.text == name }

func hasOpTok(toks []token, op string) bool {
	for _, t := range toks {
		if isOpTok(t, op) {
			return true
		}
	}
	return false
}

// glued reports whether two tokens are byte-adjacent (no whitespace between),
// used to require contiguous operator glue in the structured allowlist forms.
func glued(a, b token) bool { return a.end == b.start }

// isRoleMethod reports whether t is the `role` or `chat` method identifier.
func isRoleMethod(t token) bool {
	return t.kind == tokIdent && (t.text == "role" || t.text == "chat")
}

// isStdRoleToken reports whether t is a string literal holding exactly a
// standard literal role AND carrying no escape. An escaped role literal is
// rejected: MiniJinja decodes escapes, so "sys\tem" is a tab-bearing custom
// role, and "\u..." forms are validated/rejected by MiniJinja — neither is the
// accepted escape-free standard-role form.
func isStdRoleToken(t token) bool {
	return t.kind == tokString && !t.hasEscape && isStdRole(t.text)
}

// isStdRole reports whether s is one of the standard literal roles.
func isStdRole(s string) bool {
	return s == "system" || s == "user" || s == "assistant"
}

// mjReserved lists MiniJinja keywords/literals/operators that cannot be a bare
// variable reference, so an argument that happens to share such a name is never
// treated as a `{{ name }}` interpolation (it would render the keyword or fail
// to compile). Over-declining such an argument is safe.
var mjReserved = map[string]bool{
	// MiniJinja accepts both the lowercase and the capitalized spellings of its
	// boolean/none literals, so all are fenced.
	"true": true, "false": true, "none": true, "null": true,
	"True": true, "False": true, "None": true,
	"and": true, "or": true, "not": true, "in": true, "is": true,
	"if": true, "else": true, "elif": true, "for": true, "endfor": true,
	"endif": true, "do": true, "with": true, "without": true, "set": true,
	"block": true, "endblock": true, "filter": true, "endfilter": true,
	"macro": true, "endmacro": true, "call": true, "endcall": true,
	"import": true, "from": true, "include": true, "extends": true,
	"as": true, "recursive": true, "loop": true, "self": true,
}

// --- chat layout --------------------------------------------------------------

// validateChatLayout enforces the ordered chat-layout invariants that keep the
// provider-body oracle lossless: no non-whitespace content before the first
// role, no adjacent duplicate roles (BAML merges those), and no empty role
// message. A prompt with no role calls is a completion and is always
// layout-valid.
//
// It is VALUE-AWARE: contentful reports whether a content event actually renders
// a non-whitespace string (an interpolated argument bound to "" or "  \n\t", or
// an empty output-format block, contributes no message content, exactly as the
// lowerer trims and drops empty text). Deciding emptiness from the source shape
// alone would accept a chat whose sole message drops to zero parts after render,
// diverging from RenderStatic — the CRUX this predicate must not violate.
func validateChatLayout(events []event, contentful func(event) bool) error {
	type layoutMsg struct {
		role       string
		hasContent bool
	}
	var msgs []layoutMsg
	preRoleContent := false
	sawRole := false

	for _, ev := range events {
		switch ev.kind {
		case evRole:
			if !sawRole && preRoleContent {
				return decline(FeatureChatLayout,
					"non-whitespace content appears before the first role")
			}
			if len(msgs) > 0 && msgs[len(msgs)-1].role == ev.role {
				return decline(FeatureChatLayout,
					fmt.Sprintf("adjacent duplicate role %q", ev.role))
			}
			msgs = append(msgs, layoutMsg{role: ev.role})
			sawRole = true
		case evText, evInterp, evOutputFormat:
			if !contentful(ev) {
				continue // renders to whitespace-only: no message content
			}
			if len(msgs) == 0 {
				preRoleContent = true
			} else {
				msgs[len(msgs)-1].hasContent = true
			}
		}
	}

	if !sawRole {
		// Completion: any pre-role content is the completion body.
		return nil
	}
	for i := range msgs {
		if !msgs[i].hasContent {
			return decline(FeatureChatLayout,
				fmt.Sprintf("role %q message renders no content", msgs[i].role))
		}
	}
	return nil
}
