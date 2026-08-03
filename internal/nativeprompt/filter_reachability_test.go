package nativeprompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// This file is the ENFORCED pre-wiring gate for #651.
//
// bamlprofile reproduces BAML's get_env `regex_match` filter with Go's regexp
// (RE2) instead of Rust's regex crate. They are not the same regex language, so
// the profile runs a strict decline-by-default whitelist: anything not provably
// Go==Rust-safe answers false. That is always an UNDER-match (it can never
// out-do BAML), but it is a real divergence — `\w`, `\s`, `\b` and `\D` are
// declined outright, including on ASCII input. #651 is open, and its stated
// close condition is a fork in the road: either close the gap before wiring, or
// prove the filter mechanically unreachable from every admitted template.
//
// This file is that proof, in the only form that survives a refactor: a test
// that fails if the reachable filter set ever grows. The claim is stronger than
// "regex_match is unreachable" — it is:
//
//	The ONLY filter any nativeprompt-admitted input can invoke is `replace`.
//
// which covers regex_match, BAML's `sum` override, `format`, and any filter a
// future fork bump adds, without this file needing to know their names.
//
// The proof has four legs, one per way a filter could be reached:
//
//	L1  the dynamic lane compiles exactly ONE fixed template — scan it;
//	L2  no other template can enter the dynamic lane — Supports is exact-match;
//	L3  the static lane declines EVERY filter application, at every expression
//	    length, present and future (see below);
//	L4  template-shaped DATA (message content, an argument, ctx.output_format)
//	    is never compiled, so it cannot smuggle a filter in.
//
// L3's load-bearing half is a PRODUCTION invariant, not an enumeration:
// classifyExpr applies filterFence BEFORE consulting matchAllowlist, so a pipe
// declines whatever the allowlist happens to contain. That is what makes the
// claim refactor-resistant. Enumerating today's four accepted forms cannot
// establish it: a future six- or eight-token accepted form such as
// `arg|future_filter("x")` would slip past any fixed-length enumeration, and no
// end-to-end row can name a filter that does not exist yet.
//
//	L3a  the fence is unconditional and comes FIRST — arbitrary length, unknown
//	     filter names, and every accepted form with a pipe spliced in;
//	L3b  defence in depth: today's matchAllowlist forms are themselves pipe-free
//	     (exhaustive over short token streams, plus pipe insertion);
//	L3c  end-to-end through the production SupportsStatic gate.
//
// If any leg fails, the honest response is not to relax the test: it is to close
// #651 (or stop the cutover), per the scope's parity-decline boundary.

// provenFilters is the reachable filter set this gate asserts. It is spelled out
// here rather than derived from allowedFilters so that widening allowedFilters
// cannot silently widen the proof too.
var provenFilters = map[string]bool{"replace": true}

// TestOnlyReplaceFilterIsReachable_L1_DynamicTemplateIsScanned proves the single
// compiled dynamic template applies no filter but `replace`.
func TestOnlyReplaceFilterIsReachable_L1_DynamicTemplateIsScanned(t *testing.T) {
	// Both spellings: the retained raw prompt (what Supports compares against)
	// and the dedented text actually handed to MiniJinja.
	for name, src := range map[string]string{
		"rawDynamicPrompt": rawDynamicPrompt,
		"dynamicTemplate":  dynamicTemplate,
	} {
		got := filterNames(src)
		for _, f := range got {
			if !provenFilters[f] {
				t.Errorf("%s applies filter %q, outside the proven set %v (see #651)", name, f, provenFilters)
			}
		}
		if len(got) != 1 || got[0] != "replace" {
			t.Errorf("%s filter set = %v, want exactly [replace]", name, got)
		}
	}

	// allowedFilters is the dynamic feature scan's whitelist; it must not have
	// grown past the proven set either.
	for f := range allowedFilters {
		if !provenFilters[f] {
			t.Errorf("allowedFilters admits %q, outside the proven set %v (see #651)", f, provenFilters)
		}
	}
}

// TestOnlyReplaceFilterIsReachable_L2_DynamicLaneIsExactMatch proves no other
// template can reach the dynamic renderer: Supports is an exact-match gate, and
// Render does not take a prompt source at all.
func TestOnlyReplaceFilterIsReachable_L2_DynamicLaneIsExactMatch(t *testing.T) {
	msgs := []Message{{Role: "user", Content: strptr("hi")}}

	// The exact prompt is admitted...
	if err := Supports(rawDynamicPrompt, msgs); err != nil {
		t.Fatalf("the generated dynamic prompt must stay admitted: %v", err)
	}

	// ...and every mutation carrying a filter is not. Including a one-character
	// edit of the admitted prompt, so this cannot pass by luck of the feature
	// scan alone.
	rejects := []struct {
		name string
		src  string
	}{
		{"regex_match_filter", `{{ _.role("user") }}{{ "x"|regex_match("\\w") }}`},
		{"sum_filter", `{{ _.role("user") }}{{ [1,2]|sum }}`},
		{"format_filter", `{{ _.role("user") }}{{ "x"|format("y") }}`},
		{"regex_match_in_statement", `{% if "x"|regex_match("\\s") %}a{% endif %}`},
		{"admitted_prompt_plus_filter", rawDynamicPrompt + `{{ "x"|regex_match("\\w") }}`},
		{"admitted_prompt_minus_one_byte", rawDynamicPrompt[:len(rawDynamicPrompt)-1]},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			err := Supports(tc.src, msgs)
			if err == nil {
				t.Fatalf("Supports admitted %q", tc.src)
			}
			assertUnsupported(t, err)
		})
	}
}

// --- L3a: the pre-allowlist filter fence (the load-bearing invariant) ---------

// TestOnlyReplaceFilterIsReachable_L3a_FenceIsUnconditionalAndFirst is the
// length- and name-independent half of L3.
//
// classifyExpr calls filterFence BEFORE matchAllowlist, so an expression that
// applies a filter declines regardless of what the allowlist contains. This test
// pins that ORDER by asserting the two consequences an "enumerate the current
// forms" proof cannot give:
//
//  1. arbitrary length and UNKNOWN filter names decline — including the
//     six/eight-token shapes a fixed-length enumeration would miss and filters
//     that do not exist yet;
//  2. for each currently ACCEPTED form, splicing a pipe in anywhere declines
//     with the FENCE's exact decline value — so if a future edit both widened
//     the allowlist and moved the fence after it, the result would differ here.
func TestOnlyReplaceFilterIsReachable_L3a_FenceIsUnconditionalAndFirst(t *testing.T) {
	// A minimal V3 type gate: one declared string argument named `arg` and an
	// empty enum universe (this suite is about the filter fence, not enums).
	gate := testTypeGate(map[string]promptdescriptor.ResolvedValueType{
		"arg": {Kind: promptdescriptor.ValueString},
	}, nil)

	// assertFenced requires the decline to be EXACTLY what filterFence produces
	// for this input — not merely "some decline", and not merely the right key.
	assertFenced := func(t *testing.T, inner string) {
		t.Helper()
		toks, lexOK := mjTokenize(inner)
		if !lexOK {
			t.Fatalf("%q does not lex; the fence is not what declined it", inner)
		}
		want := filterFence(inner, toks)
		if want == nil {
			t.Fatalf("%q carries no pipe; this row does not test the fence", inner)
		}
		_, got := classifyExpr(inner, gate)
		if got == nil {
			t.Fatalf("classifyExpr ACCEPTED a filter application: %q", inner)
		}
		if got.Error() != want.Error() {
			t.Errorf("classifyExpr(%q) = %v, want the filterFence decline %v "+
				"(the fence must run BEFORE matchAllowlist)", inner, got, want)
		}
		var d *Decline
		if !errors.As(got, &d) || d.Feature != FeatureUnknownFilter {
			t.Errorf("classifyExpr(%q) key = %v, want %s", inner, got, FeatureUnknownFilter)
		}
	}

	// (1) Arbitrary length, arbitrary/unknown filter names. `future_filter` names
	// nothing that exists today — which is the point: the fence does not consult
	// a name list, so a filter a later fork bump adds is fenced before it exists.
	t.Run("arbitrary_length_and_unknown_names", func(t *testing.T) {
		for _, inner := range []string{
			`arg|future_filter`,                              // 3 tokens
			`arg|future_filter("x")`,                         // 6 tokens
			`arg|future_filter(a="x")`,                       // 8 tokens
			`arg|future_filter("x")|another("y")|third("z")`, // 16 tokens
			`ctx.output_format|future_filter("x")`,           // filter on the output format
			`_.role("user")|future_filter("x")`,              // filter on a role call
			`"literal"|regex_match("\\w")`,                   // the #651 filter itself
			`arg|regex_match("\\s")|regex_match("\\b")`,      // chained
			`future_fn(arg|future_filter("x"))`,              // pipe nested in a call
			`arg|sum`,                                        // BAML's sum override
			`arg|format("json")`,                             // the render-layer format
		} {
			assertFenced(t, inner)
		}
	})

	// (2) Every currently accepted form, with a pipe spliced in at every position.
	// Today matchAllowlist rejects each of these anyway (L3b proves that); this
	// asserts the DECLINE COMES FROM THE FENCE, which stays true if the allowlist
	// is later widened to a form that contains one of these token sequences.
	t.Run("pipe_spliced_into_every_accepted_form", func(t *testing.T) {
		acceptedSpellings := []string{
			`arg`,
			`ctx.output_format`,
			`_.role("user")`,
			`_.chat(role="user")`,
		}
		for _, form := range acceptedSpellings {
			// Non-vacuity: the un-spliced form must still be ACCEPTED, or splicing
			// into it proves nothing.
			if _, err := classifyExpr(form, gate); err != nil {
				t.Fatalf("baseline form %q is not accepted (%v); the splice rows would be vacuous", form, err)
			}
			toks, ok := mjTokenize(form)
			if !ok {
				t.Fatalf("baseline form %q does not lex", form)
			}
			// Splice at every token boundary, using the real byte offsets so the
			// spliced source stays lexable.
			for i := 0; i <= len(toks); i++ {
				at := len(form)
				if i < len(toks) {
					at = toks[i].start
				}
				assertFenced(t, form[:at]+`|future_filter("x")`+form[at:])
			}
		}
	})
}

// --- L3b: defence in depth — today's allowlist forms are pipe-free ------------

// pipeProofAlphabet is the token vocabulary the L3b enumeration draws from. It
// contains every token spelling the four accepted static forms use, the pipe,
// and a filter name — so a stream that could invoke a filter is inside the
// enumerated space, not outside it.
func pipeProofAlphabet() []token {
	specs := []struct {
		kind tokKind
		text string
	}{
		{tokOp, "|"},
		{tokIdent, "regex_match"},
		{tokIdent, "arg"}, // a declared argument name
		{tokIdent, "ctx"},
		{tokIdent, "output_format"},
		{tokIdent, "_"},
		{tokIdent, "role"},
		{tokIdent, "chat"},
		{tokOp, "."},
		{tokOp, "("},
		{tokOp, ")"},
		{tokOp, "="},
		{tokString, "user"},
	}
	toks := make([]token, 0, len(specs))
	for _, s := range specs {
		toks = append(toks, token{kind: s.kind, text: s.text})
	}
	return toks
}

// glueStream assigns byte spans so that EVERY adjacent token pair is "glued"
// (contiguous). The allowlist's glue requirement can only ever make it stricter,
// so proving rejection under maximal glue proves it for every spacing.
func glueStream(toks []token) []token {
	out := make([]token, len(toks))
	pos := 0
	for i, tk := range toks {
		tk.start = pos
		pos += len(tk.text)
		tk.end = pos
		out[i] = tk
	}
	return out
}

func streamHasPipe(toks []token) bool {
	for _, tk := range toks {
		if isOpTok(tk, "|") {
			return true
		}
	}
	return false
}

func formatStream(toks []token) string {
	parts := make([]string, len(toks))
	for i, tk := range toks {
		parts[i] = tk.text
	}
	return strings.Join(parts, " ")
}

// TestOnlyReplaceFilterIsReachable_L3b_NoShortPipedStreamReachesTheAllowlist
// enumerates EVERY token stream of length 1..4 over the pipe-bearing alphabet
// and asserts matchAllowlist accepts none that contains a pipe. 13^4 = 28,561
// streams, so this is exhaustive rather than sampled.
//
// This is DEFENCE IN DEPTH, not the guarantee: L3a's fence already declines a
// pipe before matchAllowlist is consulted. What this adds is that today's
// accepted forms are independently pipe-free, so the fence is not the only thing
// standing between a static prompt and a filter.
func TestOnlyReplaceFilterIsReachable_L3b_NoShortPipedStreamReachesTheAllowlist(t *testing.T) {
	alphabet := pipeProofAlphabet()
	// A minimal V3 type gate: one declared string argument named `arg` and an
	// empty enum universe (this suite is about the filter fence, not enums).
	gate := testTypeGate(map[string]promptdescriptor.ResolvedValueType{
		"arg": {Kind: promptdescriptor.ValueString},
	}, nil)

	// accepts counts the streams matchAllowlist DID accept. A classifier that
	// rejected everything would satisfy the assertion below vacuously, so the walk
	// must observe at least one acceptance (the alphabet contains `arg` and the
	// ctx.output_format spelling, both of which are accepted forms).
	accepts := 0

	var stream []token
	var walk func(depth int)
	walk = func(depth int) {
		if len(stream) > 0 {
			_, ok := matchAllowlist(glueStream(stream), gate)
			if ok {
				accepts++
				if streamHasPipe(stream) {
					t.Fatalf("matchAllowlist accepted a piped stream: %s", formatStream(stream))
				}
			}
		}
		if depth == 0 {
			return
		}
		for _, tk := range alphabet {
			stream = append(stream, tk)
			walk(depth - 1)
			stream = stream[:len(stream)-1]
		}
	}
	walk(4)

	if accepts == 0 {
		t.Fatal("matchAllowlist accepted nothing in the enumerated space; this proof would be vacuous")
	}
	t.Logf("enumerated all streams of length 1..%d over a %d-token alphabet; %d accepted, none piped",
		4, len(alphabet), accepts)
}

// TestOnlyReplaceFilterIsReachable_L3b_PipeInsertionBreaksEveryAcceptedForm
// covers the accepted lengths the exhaustive walk above stops short of (6 and
// 8). For each of the four ACCEPTED forms it inserts a pipe token — and a
// `|regex_match` pair — at every position and asserts matchAllowlist rejects the
// result.
//
// Also defence in depth. It says nothing about a future accepted form of some
// other length; that is precisely why L3a fences the pipe in production before
// the allowlist is reached, rather than leaving the guarantee to this
// enumeration.
func TestOnlyReplaceFilterIsReachable_L3b_PipeInsertionBreaksEveryAcceptedForm(t *testing.T) {
	// A minimal V3 type gate: one declared string argument named `arg` and an
	// empty enum universe (this suite is about the filter fence, not enums).
	gate := testTypeGate(map[string]promptdescriptor.ResolvedValueType{
		"arg": {Kind: promptdescriptor.ValueString},
	}, nil)
	ident := func(s string) token { return token{kind: tokIdent, text: s} }
	op := func(s string) token { return token{kind: tokOp, text: s} }
	str := func(s string) token { return token{kind: tokString, text: s} }

	accepted := map[string][]token{
		"bare_arg":        {ident("arg")},
		"output_format":   {ident("ctx"), op("."), ident("output_format")},
		"role_positional": {ident("_"), op("."), ident("role"), op("("), str("user"), op(")")},
		"role_kwarg": {ident("_"), op("."), ident("role"), op("("), ident("role"), op("="),
			str("user"), op(")")},
	}

	// Sanity: each baseline form really is accepted, so the insertions below are
	// perturbing something that was accepted rather than something already dead.
	for name, form := range accepted {
		if _, ok := matchAllowlist(glueStream(form), gate); !ok {
			t.Fatalf("baseline form %q is not accepted; the proof would be vacuous", name)
		}
	}

	insertions := map[string][]token{
		"pipe":             {op("|")},
		"pipe_regex_match": {op("|"), ident("regex_match")},
		"pipe_regex_call":  {op("|"), ident("regex_match"), op("("), str("\\w"), op(")")},
		"pipe_sum":         {op("|"), ident("sum")},
	}

	for formName, form := range accepted {
		for insName, ins := range insertions {
			for pos := 0; pos <= len(form); pos++ {
				mutated := make([]token, 0, len(form)+len(ins))
				mutated = append(mutated, form[:pos]...)
				mutated = append(mutated, ins...)
				mutated = append(mutated, form[pos:]...)
				if _, ok := matchAllowlist(glueStream(mutated), gate); ok {
					t.Errorf("%s + %s at %d accepted: %s", formName, insName, pos, formatStream(mutated))
				}
			}
		}
	}
}

// TestOnlyReplaceFilterIsReachable_L3c_StaticSourceDeclinesEveryFilterSpelling
// is the end-to-end half of L3: real prompt sources, through the production
// SupportsStatic gate.
func TestOnlyReplaceFilterIsReachable_L3c_StaticSourceDeclinesEveryFilterSpelling(t *testing.T) {
	declines := []struct {
		name   string
		prompt string
		want   string
	}{
		{"expr_regex_match", `{{ _.role("user") }}{{ arg|regex_match("\\w") }}`, FeatureUnknownFilter},
		{"expr_regex_match_spaced", `{{ _.role("user") }}{{ arg | regex_match("\\s") }}`, FeatureUnknownFilter},
		{"expr_regex_match_on_literal", `{{ _.role("user") }}{{ "x"|regex_match("\\b") }}`, FeatureUnknownFilter},
		{"expr_replace_is_also_declined", `{{ _.role("user") }}{{ arg|replace("a","b") }}`, FeatureUnknownFilter},
		{"expr_chained", `{{ _.role("user") }}{{ arg|upper|regex_match("\\w") }}`, FeatureUnknownFilter},
		{"expr_output_format_piped", `{{ _.role("user") }}{{ ctx.output_format|regex_match("\\w") }}`, FeatureUnknownFilter},
		{"stmt_filter_block", `{{ _.role("user") }}{% filter regex_match("\\w") %}x{% endfilter %}`, FeatureUnrecognizedPrompt},
		{"stmt_if_regex_match", `{{ _.role("user") }}{% if arg|regex_match("\\w") %}x{% endif %}`, FeatureUnrecognizedPrompt},
		{"stmt_set_regex_match", `{{ _.role("user") }}{% set x = arg|regex_match("\\w") %}x`, FeatureUnrecognizedPrompt},
		{"method_call_spelling", `{{ _.role("user") }}{{ arg.regex_match("\\w") }}`, FeaturePyFormatMethod},
	}
	for _, tc := range declines {
		t.Run(tc.name, func(t *testing.T) {
			assertStaticDecline(t, staticFn(tc.prompt, primArg("arg", "string")),
				vals(argV("arg", strV("x"))), tc.want)
		})
	}

	// DISCRIMINATION: the gate must reject filter SYNTAX, not the word. A prompt
	// whose literal text merely mentions regex_match is admitted and renders it
	// verbatim — proving the declines above come from the tag scanner, not a
	// substring search that would make this whole proof vacuous.
	t.Run("literal_text_is_not_a_filter", func(t *testing.T) {
		fn := staticFn(`{{ _.role("user") }}` + "\n" + `use regex_match(x) | sum when unsure`)
		if err := SupportsStatic(fn, noVals()); err != nil {
			t.Fatalf("literal text mentioning a filter must stay admitted: %v", err)
		}
		rp, err := RenderStatic(fn, noVals())
		if err != nil {
			t.Fatalf("RenderStatic: %v", err)
		}
		if len(rp.Messages) != 1 || len(rp.Messages[0].Parts) != 1 ||
			rp.Messages[0].Parts[0].Text == nil ||
			*rp.Messages[0].Parts[0].Text != "use regex_match(x) | sum when unsure" {
			t.Fatalf("literal filter text not rendered verbatim: %+v", rp.Messages)
		}
	})
}

// TestOnlyReplaceFilterIsReachable_L4_DataIsNeverCompiled proves the last route:
// template-shaped DATA cannot invoke a filter, because nothing re-compiles it.
// A message body, a bound static argument, and the ctx.output_format block are
// all inserted as text.
func TestOnlyReplaceFilterIsReachable_L4_DataIsNeverCompiled(t *testing.T) {
	const payload = `{{ "x"|regex_match("\\w") }}`

	t.Run("dynamic_message_content", func(t *testing.T) {
		rp, err := Render([]Message{{Role: "user", Content: strptr(payload)}}, nil)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got := *rp.Messages[0].Parts[0].Text; got != payload {
			t.Errorf("message content = %q, want it rendered verbatim (%q)", got, payload)
		}
	})

	t.Run("dynamic_text_part", func(t *testing.T) {
		rp, err := Render([]Message{{Role: "user", Parts: []ContentPart{{Text: strptr(payload)}}}}, nil)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got := *rp.Messages[0].Parts[0].Text; got != payload {
			t.Errorf("text part = %q, want it rendered verbatim (%q)", got, payload)
		}
	})

	t.Run("static_argument_value", func(t *testing.T) {
		fn := staticFn(`{{ _.role("user") }}`+"\n"+`{{ arg }}`, primArg("arg", "string"))
		rp := mustRenderStaticValues(t, fn, vals(argV("arg", strV(payload))))
		if got := *rp.Messages[0].Parts[0].Text; got != payload {
			t.Errorf("static argument = %q, want it rendered verbatim (%q)", got, payload)
		}
	})

	t.Run("output_format_block", func(t *testing.T) {
		// A field description carries the payload into the rendered
		// ctx.output_format block, which the template then interpolates.
		fn := staticFn(`{{ _.role("user") }}` + "\n" + `{{ ctx.output_format }}`)
		fn.Return = returnBundleWithFieldDescription("F", payload)
		rp := mustRenderStaticValues(t, fn, noVals())
		if got := *rp.Messages[0].Parts[0].Text; !strings.Contains(got, payload) {
			t.Errorf("output_format block = %q, want it to contain %q verbatim", got, payload)
		}
	})
}

// TestRegexMatchIsUnreachableButStillDiverges is the #651 ledger row. It pins
// that the filter genuinely IS present in the wired production environment — an
// unreachable-by-admission filter, not an absent one — and that it still
// under-matches stock BAML, so the gate above can never be misread as "the gap
// is closed".
//
// It also records the current SIZE of the gap, which is wider than #651's
// issue text (written before the filter was tightened to a decline-by-default
// whitelist): `\w`, `\s`, `\b` and `\D` are declined outright and answer false
// on ASCII input too, not only on non-ASCII — `"a!"` against `a\b` is an ASCII
// subject where stock BAML matches and the profile does not. That widening is a
// pure under-match — it can never out-do BAML — but it does mean the
// reachability proof above, not an "ASCII is exact" argument, is what makes the
// cutover safe.
// Every stock-BAML value asserted below was MEASURED against the stock BAML
// v0.223 CFFI, not reasoned about: each row corresponds to a row of
// internal/bamlprofile/profileoracle's TestRegexNeverOutdo, which renders the
// same subject+pattern through the real runtime. That matters because a
// plausible-looking regex row can silently stop discriminating — `"ab"` against
// `a\b` was such a row here (both sides of the boundary are word characters, so
// stock answers false too, and the native false proved nothing). It is kept
// below as an explicit CONTROL, and the discriminating word/non-word transitions
// replace it as the evidence.
func TestRegexMatchIsUnreachableButStillDiverges(t *testing.T) {
	// (1) Present, and exact against BAML on the accepted grammar.
	// profileoracle rows: ok_ascii_class_t, ok_range_mid, ud_*, ok_D_true.
	for _, exact := range []struct{ src, want string }{
		{`{{ "abc"|regex_match("[a-z]+") }}`, "true"},
		{`{{ "abc"|regex_match("^abc$") }}`, "true"},
		{`{{ "abc123"|regex_match("\\d") }}`, "true"},
		{`{{ "abc"|regex_match("\\d") }}`, "false"},
	} {
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, exact.src); got != exact.want {
			t.Errorf("%s = %q, want %q (accepted, BAML-exact grammar)", exact.src, got, exact.want)
		}
	}

	// (2) CONTROLS: the profile answers false here, and so does stock BAML. These
	// are NOT evidence of an under-match; they are here so a future edit cannot
	// mistake a same-answer row for a divergence the way `"ab"` / `a\b` was.
	// profileoracle row: uni_b_word (byte-exact).
	for _, control := range []string{
		`{{ "ab"|regex_match("a\\b") }}`,    // no boundary between two word chars
		`{{ "éa"|regex_match("^é\\ba$") }}`, // same, with a non-ASCII word char
	} {
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, control); got != "false" {
			t.Errorf("%s = %q, want %q (control: stock BAML also answers false)", control, got, "false")
		}
	}

	// (3) The real under-match: stock BAML (Rust regex) answers TRUE for every row
	// below while the profile's decline-by-default whitelist answers false. Each
	// pairs with a profileoracle row that measured stock's "true".
	for _, diverging := range []struct{ src, oracleRow string }{
		{`{{ "abc"|regex_match("\\w") }}`, "uni_w_word"},              // \w declined (Rust's is Unicode-aware)
		{`{{ "é"|regex_match("^\\w+$") }}`, "uni_w_nonascii"},         // the original non-ASCII \w case
		{`{{ " "|regex_match("\\s") }}`, "uni_s_space"},               // \s declined
		{`{{ "a!"|regex_match("a\\b") }}`, "uni_b_boundary_ascii"},    // \b at a word/non-word transition — ASCII
		{`{{ "é!"|regex_match("é\\b") }}`, "uni_b_boundary_nonascii"}, // …and non-ASCII
		{`{{ "abc"|regex_match("\\D") }}`, "ok_notdigit"},             // \D declined (differing Unicode tables)
	} {
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, diverging.src); got != "false" {
			t.Errorf("%s = %q, want %q — the #651 under-match (stock BAML answers true; "+
				"measured by profileoracle row %s). If this now matches BAML, the gap has been "+
				"closed and this row should be retired", diverging.src, got, "false", diverging.oracleRow)
		}
	}

	t.Log("#651 remains an OPEN, DECLARED divergence; TestOnlyReplaceFilterIsReachable_L1..L4 " +
		"prove it unreachable from every admitted template")
}
