package profileoracle

// Corpus is the PR-1 get_env differential corpus. It covers what Slice 2 PR-1
// builds: whitespace (trim_blocks/lstrip_blocks), none/null formatting, the
// BAML-exact builtin filter/test/FUNCTION registry, regex_match, BAML's sum
// override, pycompat unknown-methods, macros, ctx, and the `_` role helper. The
// host value model (enum/class/map/media, enum globals, the #597 ValueCmp) is a
// later PR and is deliberately absent.
//
// SCOPE OF THE PROOF — read before treating a green run as full get_env parity:
//
//   - ctx: every corpus function returns `string`, for which BAML's
//     ctx.output_format is EMPTY. These rows therefore prove ctx is wired and
//     that output_format is byte-exact EMPTY for a schemaless return — they do
//     NOT prove a nonempty output_format. Computing a nonempty output_format
//     needs the return-type SCHEMA RENDERER, which is later-slice/host-value
//     work; nonempty ctx.output_format parity is deferred to that slice. (See
//     TestProfile... registry inventory in bamlprofile for the effective
//     filter/test/function registry snapshot.)
//   - regex_match: STRICT decline-by-default (rustRegexToGo/scanPattern) — it
//     accepts ONLY explicitly-recognized Go==Rust-safe grammar (ASCII literals &
//     escaped metacharacters, Unicode \d [guarded; \D DECLINED for Unicode-table
//     skew], ^/$/./|/balanced groups/quantifiers {m,n} 0<=m<=n<=1000 with NO
//     leading-zero bounds, ASCII classes with ASCII-endpoint ranges + \d members,
//     i/m/s/U flag groups in Unicode-ON with NO DUPLICATES) and DECLINES
//     everything else to false, so it never out-does BAML; a compile-after-scan
//     backstop makes it never Go-invalid. TestRegexNeverOutdo (never-out-do,
//     exhaustively differential-verified) and TestRegexDigitUnicodeGuard (Go-Nd ⊆
//     BAML-\d) are the authorities. Full parity is #651, close before serving.
//   - The `format` filter is excluded: BAML overrides it at the render layer with
//     a host-value (yaml/json/toon) version (jinja-runtime/src/lib.rs:219-266),
//     which is a later-slice concern, not get_env.
//
// Templates are column-0 so BAML's render-layer dedent is a no-op.
func Corpus() []Row {
	return []Row{
		// --- whitespace: trim_blocks + lstrip_blocks ---
		{ID: "ws_for_trim", Surface: "whitespace",
			Template: "{% for i in [1, 2, 3] %}\n{{ i }}\n{% endfor %}"},
		{ID: "ws_if_lstrip", Surface: "whitespace",
			Template: "start\n    {% if true %}\nyes\n    {% endif %}\nend"},
		{ID: "ws_set_block", Surface: "whitespace",
			Template: "{% set x = 5 %}\nx={{ x }}"},

		// --- none / null formatting ---
		{ID: "none_literal", Surface: "none", Template: "{{ none }}"},
		{ID: "none_var", Surface: "none",
			Params: []Param{{Name: "x", BamlType: "string?"}}, Args: map[string]any{"x": nil},
			Template: "{{ x }}"},
		{ID: "none_in_text", Surface: "none",
			Params: []Param{{Name: "x", BamlType: "int?"}}, Args: map[string]any{"x": nil},
			Template: "value is {{ x }}!"},

		// --- builtin filters (fork default registry) ---
		{ID: "filter_upper", Surface: "filter", Template: `{{ "hello world"|upper }}`},
		{ID: "filter_lower", Surface: "filter", Template: `{{ "HeLLo"|lower }}`},
		{ID: "filter_length", Surface: "filter", Template: `{{ [1, 2, 3, 4]|length }}`},
		{ID: "filter_join", Surface: "filter", Template: `{{ ["a", "b", "c"]|join(", ") }}`},
		{ID: "filter_replace", Surface: "filter", Template: `{{ "a-b-c"|replace("-", "_") }}`},
		{ID: "filter_default", Surface: "filter",
			Params: []Param{{Name: "x", BamlType: "string?"}}, Args: map[string]any{"x": nil},
			Template: `{{ x|default("fallback") }}`},
		{ID: "filter_reverse", Surface: "filter", Template: `{{ [1, 2, 3]|reverse|join(",") }}`},
		{ID: "filter_sort", Surface: "filter", Template: `{{ [3, 1, 2]|sort|join(",") }}`},
		{ID: "filter_min_max", Surface: "filter", Template: `{{ [3, 1, 2]|min }}-{{ [3, 1, 2]|max }}`},
		{ID: "filter_first_last", Surface: "filter", Template: `{{ [10, 20, 30]|first }}/{{ [10, 20, 30]|last }}`},
		{ID: "filter_round", Surface: "filter", Template: `{{ 3.14159|round(2) }}`},
		{ID: "filter_int", Surface: "filter", Template: `{{ "42"|int }}`},
		{ID: "filter_trim", Surface: "filter", Template: `[{{ "  hi  "|trim }}]`},
		{ID: "filter_tojson", Surface: "filter", Template: `{{ [1, 2]|tojson }}`},

		// --- builtin tests ---
		{ID: "test_even", Surface: "test", Template: `{{ 4 is even }}/{{ 3 is even }}`},
		{ID: "test_defined", Surface: "test",
			Params: []Param{{Name: "x", BamlType: "int?"}}, Args: map[string]any{"x": nil},
			Template: `{{ x is defined }}/{{ y is defined }}`},
		{ID: "test_string_number", Surface: "test", Template: `{{ "a" is string }}/{{ 1 is number }}`},
		{ID: "test_in", Surface: "test", Template: `{{ 2 is in([1, 2, 3]) }}`},

		// --- regex_match (BAML get_env addition) ---
		// These are BYTE-EXACT rows (profile == BAML): whitelisted constructs the
		// profile reproduces, plus declined constructs where BAML also returns false
		// (so both are false). The never-out-do PROOF (whitelisted + declined
		// under-match + every round-4 out-do case) is TestRegexNeverOutdo.
		//
		// Whitelist ACCEPT (reproduced byte-exact):
		{ID: "regex_digits_true", Surface: "regex_match", // ASCII class
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "abc123"},
			Template: `{{ s|regex_match("[0-9]+") }}`},
		{ID: "regex_digits_false", Surface: "regex_match",
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "abcdef"},
			Template: `{{ s|regex_match("[0-9]+") }}`},
		{ID: "regex_unicode_digit_true", Surface: "regex_match", // \d -> \p{Nd}, Arabic digits
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "٣٤"}, // ٣٤
			Template: `{{ s|regex_match("^\\d+$") }}`},
		{ID: "regex_notdigit_declined", Surface: "regex_match", // \D DECLINED (Unicode skew) -> false; BAML: ٣ is a digit so \D no-match -> false
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "٣"}, // ٣
			Template: `{{ s|regex_match("\\D") }}`},
		{ID: "regex_unicode_digit_class", Surface: "regex_match", // \d inside a class -> [\p{Nd}]
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "٣"}, // ٣
			Template: `{{ s|regex_match("^[\\d]+$") }}`},
		{ID: "regex_escaped_literal", Surface: "regex_match", // \\d = literal backslash + d
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": `\d`},
			Template: `{{ s|regex_match("\\\\d") }}`},
		{ID: "regex_ascii_range", Surface: "regex_match", // ASCII range class
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "m"},
			Template: `{{ s|regex_match("^[a-z]$") }}`},
		{ID: "regex_alternation", Surface: "regex_match", // groups + alternation
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "dog"},
			Template: `{{ s|regex_match("^(cat|dog)$") }}`},
		{ID: "regex_quant_bounded", Surface: "regex_match", // {m,n} with n<=1000
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "aaa"},
			Template: `{{ s|regex_match("^a{2,5}$") }}`},
		{ID: "regex_escaped_dot", Surface: "regex_match", // escaped punctuation literal
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "a.b"},
			Template: `{{ s|regex_match("^a\\.b$") }}`},
		{ID: "regex_casefold_ascii", Surface: "regex_match", // (?i) in Unicode-ON mode
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "É"}, // É
			Template: `{{ s|regex_match("(?i)^é$") }}`}, // é
		{ID: "regex_bad_pattern", Surface: "regex_match", // malformed -> both reject -> false
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "x"},
			Template: `{{ s|regex_match("(") }}`},
		// Whitelist DECLINE where BAML ALSO returns false (so both false, byte-exact):
		{ID: "regex_nou_reject_D", Surface: "regex_match", // (?-u:\D) BAML rejects; profile declines
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "a"},
			Template: `{{ s|regex_match("(?-u:^\\D$)") }}`},
		{ID: "regex_nou_reject_dot", Surface: "regex_match",
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "a"},
			Template: `{{ s|regex_match("(?-u:^.$)") }}`},
		{ID: "regex_flag_nou_asciimiss", Surface: "regex_match", // (?-u:\d) on ٣: BAML false, declined false
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "٣"}, // ٣
			Template: `{{ s|regex_match("(?-u:\\d)") }}`},

		// --- builtin FUNCTIONS (get_env registry: range/dict/namespace/debug) ---
		{ID: "fn_range", Surface: "function", Template: `{{ range(3)|list|join(",") }}`},
		{ID: "fn_range_start_stop", Surface: "function", Template: `{{ range(2, 5)|list|join(",") }}`},
		{ID: "fn_dict", Surface: "function", Template: `{{ dict(a=1, b=2)|length }}`},
		{ID: "fn_namespace", Surface: "function",
			Template: `{% set ns = namespace(total=0) %}{% for i in [1, 2, 3, 4] %}{% set ns.total = ns.total + i %}{% endfor %}{{ ns.total }}`},

		// --- sum (BAML get_env override) ---
		{ID: "sum_ints", Surface: "sum", Template: `{{ [1, 2, 3]|sum }}`},
		{ID: "sum_mixed_float", Surface: "sum", Template: `{{ [1, 2.5]|sum }}`},
		{ID: "sum_empty", Surface: "sum", Template: `{{ []|sum }}`},
		{ID: "sum_floats", Surface: "sum", Template: `{{ [1.5, 2.5, 3.0]|sum }}`},
		{ID: "sum_negatives", Surface: "sum", Template: `{{ [-5, 10, -2]|sum }}`},
		// Edge: a whole float alongside ints decides whether the int path uses
		// i64::try_from (which the profile models via Value.AsInt). "3" means the
		// whole float converts to int; "3.0" means it does not and the float path
		// wins. Stock BAML settles it — this row is why the profile is not a guess.
		{ID: "sum_whole_float", Surface: "sum", Template: `{{ [1, 2.0]|sum }}`},
		// The all-fail `else 0` branch: a nonnumeric element fails BOTH the all-int
		// and all-float passes, so BAML's sum_filter returns 0. sum_empty cannot
		// reach this (empty Vec takes the int path); this row does.
		{ID: "sum_nonnumeric", Surface: "sum", Template: `{{ [1, "x"]|sum }}`},

		// --- pycompat unknown-methods ---
		{ID: "py_upper", Surface: "pycompat", Template: `{{ "abc".upper() }}`},
		{ID: "py_strip", Surface: "pycompat", Template: `[{{ "  x  ".strip() }}]`},
		{ID: "py_split", Surface: "pycompat", Template: `{{ "a,b,c".split(",")|join("|") }}`},
		{ID: "py_replace", Surface: "pycompat", Template: `{{ "aaa".replace("a", "b", 2) }}`},
		{ID: "py_find", Surface: "pycompat", Template: `{{ "hello".find("l") }}`},
		{ID: "py_startswith", Surface: "pycompat", Template: `{{ "hello".startswith("he") }}`},
		{ID: "py_count", Surface: "pycompat", Template: `{{ "banana".count("a") }}`},
		{ID: "py_dict_get", Surface: "pycompat",
			Template: `{{ {"a": 1, "b": 2}.get("a") }}/{{ {"a": 1}.get("z", 9) }}`},
		{ID: "py_format_commas", Surface: "pycompat", Template: `{{ "{:,}".format(1234567) }}`},
		{ID: "py_format_float", Surface: "pycompat", Template: `{{ "{:.2f}".format(3.14159) }}`},

		// --- macros ---
		{ID: "macro_inline", Surface: "macro",
			Template: `{% macro greet(name) %}Hello, {{ name }}!{% endmacro %}{{ greet("world") }}`},
		{ID: "macro_loop", Surface: "macro",
			Template: `{% macro item(x) %}[{{ x }}]{% endmacro %}{% for i in [1, 2, 3] %}{{ item(i) }}{% endfor %}`},

		// --- ctx ---
		// Proves ctx is wired and ctx.output_format is byte-exact EMPTY for a
		// string-return function (BAML renders no schema for `string`). This is
		// EMPTY-ONLY coverage by design: a nonempty output_format needs the
		// return-type schema renderer (later slice), so nonempty ctx.output_format
		// parity is explicitly out of scope here (see the corpus doc above).
		{ID: "ctx_output_format_empty", Surface: "ctx", Template: `before{{ ctx.output_format }}after`},

		// --- _ role helper (chat) ---
		{ID: "role_two", Surface: "role", Chat: true,
			Params: []Param{{Name: "topic", BamlType: "string"}}, Args: map[string]any{"topic": "cats"},
			Template: "{{ _.role(\"system\") }}\nYou are concise.\n{{ _.role(\"user\") }}\nTell me about {{ topic }}."},
		{ID: "role_kwarg", Surface: "role", Chat: true,
			Template: "{{ _.role(\"system\") }}\nsys\n{{ _.chat(role=\"assistant\") }}\nasst"},
		// Discriminating chat-part trim: the message content has intentional
		// leading/trailing whitespace. BAML trims each chat part (jinja-runtime
		// lib.rs:448-449, verified via CFFI: "   spaced   " -> "spaced"), and the
		// profile's SplitChat mirrors that trim, so the message content is
		// byte-exact on both legs. Without a faithful trim this row would diverge.
		{ID: "role_trim_content", Surface: "role", Chat: true,
			Params: []Param{{Name: "s", BamlType: "string"}}, Args: map[string]any{"s": "spaced"},
			Template: "{{ _.role(\"user\") }}\n   {{ s }}   \n"},
	}
}
