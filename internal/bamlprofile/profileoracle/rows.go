package profileoracle

// Corpus is the get_env + host-value-model differential corpus. The PR-1 rows
// cover the get_env engine config: whitespace (trim_blocks/lstrip_blocks),
// none/null formatting, the BAML-exact builtin filter/test/FUNCTION registry,
// regex_match, BAML's sum override, pycompat unknown-methods, macros, ctx, and
// the `_` role helper. The PR-2 rows (Surface enum/enum_cmp/enum_container/
// class/class_render) cover the enum & class host value model: the per-enum
// namespace globals, enum presentation (alias-or-canonical) and `.value`, the
// #597 enum-`==` closure in both operand orders + membership + ordering + the
// int/bool/none decline edges + cross-enum probes, enum-in-container/class, and
// class canonical access / map behavior / pretty-debug rendering.
//
// The shared enum/class declarations live in types.go (types.baml for the stock
// leg, profileEnums for the profile leg); host-shaped arguments are lowered to
// profile host values by hostValue. Media host values and BAML's render-layer
// format(type=json|yaml|toon) override are DECLINED (see host_test.go and #602),
// not silently emitted.
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

		// ===================== PR-2: enum & class host value model =====================

		// --- enum: presentation & namespace globals ---
		// Display is alias-or-canonical; .value is canonical ONLY; .name/.alias are
		// absent (undefined); a typed enum ARGUMENT lowers with its resolved alias
		// exactly like the global member does.
		{ID: "enum_display_alias", Surface: "enum", Template: `{{ Color.RED }}`},         // rouge
		{ID: "enum_display_noalias", Surface: "enum", Template: `{{ Color.GREEN }}`},     // GREEN
		{ID: "enum_value_alias", Surface: "enum", Template: `{{ Color.RED.value }}`},     // RED
		{ID: "enum_value_noalias", Surface: "enum", Template: `{{ Color.GREEN.value }}`}, // GREEN
		{ID: "enum_missing_attrs", Surface: "enum",
			Template: `{{ Color.RED.name is undefined }}/{{ Color.RED.alias is undefined }}/{{ Color.RED.value is undefined }}`}, // true/true/false
		{ID: "enum_unknown_member", Surface: "enum", Template: `{{ Color.PURPLE is undefined }}`}, // true
		{ID: "enum_arg_display_alias", Surface: "enum",
			Params: []Param{{Name: "e", BamlType: "Color"}}, Args: map[string]any{"e": "RED"},
			Template: `{{ e }}/{{ e.value }}`}, // rouge/RED
		{ID: "enum_arg_display_noalias", Surface: "enum",
			Params: []Param{{Name: "e", BamlType: "Color"}}, Args: map[string]any{"e": "GREEN"},
			Template: `{{ e }}/{{ e.value }}`}, // GREEN/GREEN

		// --- enum ValueCmp: the six #597 cases (both operand orders + membership) ---
		{ID: "e597_canon_eq_fwd", Surface: "enum_cmp", Template: `{{ Color.RED == 'RED' }}`},      // true
		{ID: "e597_canon_eq_rev", Surface: "enum_cmp", Template: `{{ 'RED' == Color.RED }}`},      // true
		{ID: "e597_same_member", Surface: "enum_cmp", Template: `{{ Color.RED == Color.RED }}`},   // true
		{ID: "e597_in_list", Surface: "enum_cmp", Template: `{{ 'RED' in [Color.RED] }}`},         // true
		{ID: "e597_alias_str_false", Surface: "enum_cmp", Template: `{{ Color.RED == 'rouge' }}`}, // false (compares canonical, NOT alias)
		{ID: "e597_diff_member", Surface: "enum_cmp", Template: `{{ Color.RED == Color.BLUE }}`},  // false

		// --- enum ValueCmp: extra edges the fence and comparator require ---
		{ID: "enum_cmp_ne_member", Surface: "enum_cmp",
			Template: `{{ Color.RED != Color.BLUE }}/{{ Color.RED != Color.RED }}`}, // true/false
		{ID: "enum_cmp_ne_str", Surface: "enum_cmp",
			Template: `{{ Color.RED != 'RED' }}/{{ Color.RED != 'rouge' }}`}, // false/true
		{ID: "enum_cmp_member_in_strlist", Surface: "enum_cmp", Template: `{{ Color.RED in ['RED'] }}`}, // true (reverse container orientation)
		{ID: "enum_cmp_alias_str_rev", Surface: "enum_cmp", Template: `{{ 'rouge' == Color.RED }}`},     // false
		// Cross-enum with SAME canonical+alias -> equal, proving enum_name is NOT
		// part of the comparator; SAME canonical/different alias (Some vs None) ->
		// not equal, proving the alias IS.
		{ID: "enum_cmp_cross_enum_eq", Surface: "enum_cmp", Template: `{{ Color.RED == Shade.RED }}`},        // true
		{ID: "enum_cmp_cross_enum_in_list", Surface: "enum_cmp", Template: `{{ Shade.RED in [Color.RED] }}`}, // true
		{ID: "enum_cmp_same_canon_diff_alias", Surface: "enum_cmp", Template: `{{ Color.RED == Size.RED }}`}, // false
		// Ordering: against a canonical string, and between members (None < Some alias).
		{ID: "enum_cmp_order_str", Surface: "enum_cmp",
			Template: `{{ Color.GREEN < 'RED' }}/{{ Color.RED < 'GREEN' }}`}, // true/false
		{ID: "enum_cmp_order_member", Surface: "enum_cmp",
			Template: `{{ Size.RED < Color.RED }}/{{ Color.RED < Size.RED }}`}, // true/false
		// Non-string primitive edges: DECLINE, never invent equality (all false;
		// the != negations are true).
		{ID: "enum_cmp_decline_int", Surface: "enum_cmp",
			Template: `{{ Color.RED == 5 }}/{{ Color.RED != 5 }}`}, // false/true
		{ID: "enum_cmp_decline_bool", Surface: "enum_cmp",
			Template: `{{ Color.RED == true }}/{{ Color.RED == false }}`}, // false/false
		{ID: "enum_cmp_decline_none", Surface: "enum_cmp",
			Template: `{{ Color.RED == none }}/{{ Color.RED != none }}`}, // false/true

		// --- enum in containers / classes ---
		// A Color[] argument lowers to a host list of enum members: rendered
		// directly it is a COMPACT debug-list of BARE aliases (a top-level list is
		// non-alternate, `[rouge, GREEN]`); it indexes/compares by canonical
		// identity and answers membership. An enum member as a class field renders
		// as its bare alias inside the class's pretty debug-map.
		{ID: "enum_list_render", Surface: "enum_container",
			Params: []Param{{Name: "cs", BamlType: "Color[]"}}, Args: map[string]any{"cs": []any{"RED", "GREEN"}},
			Template: `{{ cs }}`},
		{ID: "enum_list_index", Surface: "enum_container",
			Params: []Param{{Name: "cs", BamlType: "Color[]"}}, Args: map[string]any{"cs": []any{"RED", "BLUE"}},
			Template: `{{ cs[0] == 'RED' }}/{{ cs[0] }}/{{ cs[1] }}`}, // true/rouge/BLUE
		{ID: "enum_list_membership", Surface: "enum_container",
			Params: []Param{{Name: "cs", BamlType: "Color[]"}}, Args: map[string]any{"cs": []any{"RED", "BLUE"}},
			Template: `{{ 'RED' in cs }}/{{ 'GREEN' in cs }}`}, // true/false
		{ID: "enum_in_class_access", Surface: "enum_container",
			Params: []Param{{Name: "c", BamlType: "WithColor"}}, Args: map[string]any{"c": map[string]any{"color": "RED", "n": int64(1)}},
			Template: `{{ c.color == 'RED' }}/{{ c.color }}/{{ c.color.value }}`}, // true/rouge/RED
		{ID: "enum_in_class_render", Surface: "enum_container",
			Params: []Param{{Name: "c", BamlType: "Ecw"}}, Args: map[string]any{"c": map[string]any{"color": "RED"}},
			Template: `{{ c }}`}, // {\n    "colour": rouge,\n} (enum field renders as the BARE alias)

		// --- class: canonical access & map behavior ---
		// Access/iteration/length use CANONICAL field names; an alias is not an
		// alternate attribute; a missing field is undefined; a nonempty class is
		// truthy. Iteration uses a single-field class so the yielded key order is
		// deterministic (multi-field iteration order is a leaf-level proof).
		{ID: "class_canon_access", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c.prop1 }}`}, // value
		{ID: "class_alias_undef", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c.key1 is undefined }}/{{ c.prop1 is undefined }}`}, // true/false
		{ID: "class_missing_undef", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c.nope is undefined }}`}, // true
		{ID: "class_iter_key_canonical", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{% for k in c %}{{ k }}{% endfor %}`}, // prop1 (the CANONICAL name, not the alias key1)
		{ID: "class_length", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "WithColor"}}, Args: map[string]any{"c": map[string]any{"color": "RED", "n": int64(1)}},
			Template: `{{ c|length }}`}, // 2 (order-independent)
		{ID: "class_truthy_nonempty", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{% if c %}yes{% else %}no{% endif %}`}, // yes
		{ID: "class_cond_prop", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{% if c.prop1 == 'value' %}true{% else %}false{% endif %}`}, // true (mirrors BAML's render_class_with_if_condition)
		{ID: "class_int_compare", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "WithColor"}}, Args: map[string]any{"c": map[string]any{"color": "GREEN", "n": int64(4)}},
			Template: `{% if c.n < 40 %}true{% else %}false{% endif %}`}, // true (mirrors render_number_comparison_with_alias)
		// Ow exists for the CONSTRAINT corpus's order probes (its declared order is
		// the reverse of its sorted order); this row keeps it referenced on the
		// prompt leg too, so the generated project has no dead type. It is
		// deliberately ORDER-INDEPENDENT — attribute access only — because a class
		// argument's field order comes from the Go client's random map iteration
		// (see classDecls) and could not be byte-pinned here.
		{ID: "class_ow_access", Surface: "class",
			Params: []Param{{Name: "c", BamlType: "Ow"}}, Args: map[string]any{"c": map[string]any{"zeta": int64(1), "alpha": "v"}},
			Template: `{{ c.zeta }}/{{ c.alpha }}/{{ c.z is undefined }}/{{ c|length }}`}, // 1/v/true/2

		// --- class: direct pretty/debug rendering ({map:#?} bytes) ---
		// Aliased keys, four-space nesting, trailing commas, nested class/list,
		// nested none -> null, and scalar escaping — the exact Rust debug bytes.
		// Single-field wrappers keep sibling order out of the picture (see
		// classDecls); multi-field ordered rendering is proven at the leaf against
		// BAML's own golden.
		{ID: "class_render_single", Surface: "class_render",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c }}`}, // {\n    "key1": "value",\n}
		{ID: "class_render_nested", Surface: "class_render",
			Params: []Param{{Name: "c", BamlType: "Cw"}}, Args: map[string]any{"c": map[string]any{
				"inner": map[string]any{"items": []any{"item1", "item2"}}}},
			Template: `{{ c }}`}, // {\n    "nested": {\n        "data": [\n            "item1",\n            "item2",\n        ],\n    },\n}
		{ID: "class_render_nested_none", Surface: "class_render",
			Params: []Param{{Name: "c", BamlType: "Nw"}}, Args: map[string]any{"c": map[string]any{"maybe": nil}},
			Template: `{{ c }}`}, // {\n    "perhaps": null,\n}
		{ID: "class_render_present_optional", Surface: "class_render",
			Params: []Param{{Name: "c", BamlType: "Nw"}}, Args: map[string]any{"c": map[string]any{"maybe": "here"}},
			Template: `{{ c }}`}, // {\n    "perhaps": "here",\n}
		{ID: "class_render_escaping", Surface: "class_render",
			Params: []Param{{Name: "c", BamlType: "Ew"}}, Args: map[string]any{"c": map[string]any{"raw": "a\"b\tc\nd"}},
			Template: `{{ c }}`}, // {\n    "escaped": "a\"b\tc\nd",\n} (Rust Debug-escaped scalar)

		// ============ PR-2 remediation: the DISCRIMINATING host-object rows ============
		//
		// Everything above this line was green BEFORE the fork correction and cannot
		// detect either defect the PR-2 review found. These rows can: each one is a
		// direct stock-CFFI repro from the review or the settled scope.
		//
		// Do NOT reduce any of them to a top-level `{{ Color.RED }}` control — that
		// control is already green above (enum_display_alias) and is exactly what
		// missed the defects.

		// --- CRITICAL: opaque-object decline -> the map fallback ---
		//
		// BAML's MinijinjaBamlEnumValue/EnumType are ObjectRepr::Map but return
		// Enumerator::NonEnumerable with an UNKNOWN length: they are NOT empty maps.
		// After the leaf comparator's required (0, false) decline, the engine's map
		// arm (minijinja value/mod.rs:533-559) runs DIRECTIONALLY — it short-circuits
		// on the LEFT operand's absent pair iterator, and otherwise treats a
		// non-enumerable RIGHT side as length zero.
		//
		// Each operand orientation is enumerated separately on purpose: the profile
		// was true where BAML is false in BOTH equality directions, and the empty-map
		// asymmetry is invisible if only one orientation is asserted.
		{ID: "enum_cmp_opaque_namespace_eq_ne", Surface: "enum_cmp_opaque",
			Template: `{{ Color.RED == Color }}/{{ Color == Color.RED }}/{{ Color.RED != Color }}/{{ Color != Color.RED }}`}, // false/false/true/true
		{ID: "enum_cmp_opaque_namespace_membership", Surface: "enum_cmp_opaque",
			Template: `{{ Color.RED in [Color] }}/{{ Color in [Color.RED] }}/{{ Color in [Shade] }}/{{ Color.RED in [Color, Shade] }}`}, // false/false/false/false
		// Deliberately ASYMMETRIC, and pinned as stock authority: `{} == Color.RED`
		// and `Color.RED in [{}]` are TRUE (the engine's length fallback counts the
		// opaque right side as zero, and membership asks `{} == member`), while
		// `Color.RED == {}` and `{} in [Color.RED]` are FALSE. The profile used to be
		// true for the latter two — the out-dos — and false for the former two.
		{ID: "enum_cmp_opaque_empty_map_decline", Surface: "enum_cmp_opaque",
			Template: `{{ Color.RED == {} }}/{{ {} == Color.RED }}/{{ Color.RED != {} }}/{{ {} != Color.RED }}/{{ Color.RED in [{}] }}/{{ {} in [Color.RED] }}`}, // false/true/true/false/true/false
		// FAULT ROW. Stock BAML reaches minijinja's `unreachable!()`
		// (value/mod.rs:660) ordering two non-enumerable mappings. The profile must
		// fault too — a rendered `false` here would be a native success where stock
		// BAML fails internally. Compared by OUTCOME CLASS; the stock leg runs in a
		// subprocess because this panic hangs BuildRequest forever in-process.
		{ID: "enum_cmp_opaque_order_unreachable", Surface: "enum_cmp_opaque", Fault: OutcomePanic,
			Template: `{{ Color.RED < Color }}`},

		// --- HIGH: ObjectWithString through ordinary containers and coercions ---
		//
		// BAML object Display/Debug is one call, so a host object renders the same
		// alone, inside a native container, through a filter, and through `~`. Before
		// the fork correction every one of these leaked Go's `%v` (`[&{RED 0x… Color}]`),
		// which is also NONDETERMINISTIC output — the pointer changes per allocation.
		// The three consumers are independently important: |join builds its own
		// string, |upper coerces through Args.CoerceStr, and `~` goes through Concat.
		{ID: "object_string_native_enum_list", Surface: "object_string",
			Template: `{{ [Color.RED] }}`}, // [rouge]
		{ID: "object_string_join", Surface: "object_string",
			Template: `{{ [Color.RED]|join(",") }}`}, // rouge
		{ID: "object_string_upper", Surface: "object_string",
			Template: `{{ Color.RED|upper }}`}, // ROUGE
		{ID: "object_string_concat", Surface: "object_string",
			Template: `{{ Color.RED ~ "!" }}`}, // rouge!
		{ID: "object_string_filter_string", Surface: "object_string",
			Template: `{{ Color.RED|string }}`}, // rouge — the explicit coercion control
		// A HOST list inside a native list: the host list's own compact rendering has
		// to survive being an item of an ordinary Jinja sequence.
		{ID: "object_string_native_host_list", Surface: "object_string",
			Params: []Param{{Name: "xs", BamlType: "Color[]"}}, Args: map[string]any{"xs": []any{"RED"}},
			Template: `{{ [xs] }}`}, // [[rouge]]
		// A host CLASS inside a native list: its forced-alternate `{map:#?}` bytes,
		// newlines and all, must appear verbatim inside the compact native list.
		{ID: "object_string_native_class", Surface: "object_string",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ [c] }}`}, // [{\n    "key1": "value",\n}]
		// The enum NAMESPACE is the other host object that can reach a render path,
		// and it has no Display of its own in BAML — MiniJinja's default
		// Object::render for a Map builds a debug_map from its pairs, and it has
		// none, so it is `{}` everywhere. Bracketed so an empty rendering is still
		// visible in the compared bytes. Without an explicit ObjectString on the
		// namespace this leaked Go's `&{Color map[RED:0x…]}`, heap addresses and
		// all — the same defect class as the member rows above, on the object the
		// original fix missed.
		{ID: "object_string_namespace_bare", Surface: "object_string",
			Template: `[{{ Color }}]`}, // [{}]
		{ID: "object_string_namespace_composed", Surface: "object_string",
			Template: `{{ [Color] }}/{{ Color|string }}/{{ Color ~ "!" }}/{{ Color|upper }}/{{ {"k": Color} }}`}, // [{}]/{}/{}!/{}/{"k": {}}

		// --- LOW: coverage holes and retained regression rows ---

		// An unrelated HOST object (a class) also declines, and must then reach the
		// engine's ordinary fallback rather than an invented enum answer. Both
		// orientations, and both through a sequence.
		{ID: "enum_cmp_decline_class_eq_ne", Surface: "enum_cmp_decline",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ Color.RED == c }}/{{ c == Color.RED }}/{{ Color.RED != c }}/{{ c != Color.RED }}`}, // false/false/true/true
		{ID: "enum_cmp_decline_class_membership", Surface: "enum_cmp_decline",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ Color.RED in [c] }}/{{ c in [Color.RED] }}`}, // false/false
		// The missing float family, plus both hook orientations.
		{ID: "enum_cmp_decline_float_both_ways", Surface: "enum_cmp_decline",
			Template: `{{ Color.RED == 1.5 }}/{{ Color.RED != 1.5 }}/{{ 1.5 == Color.RED }}/{{ 1.5 != Color.RED }}`}, // false/true/false/true
		// The missing REVERSE int/bool/none declines (the forward ones are above).
		{ID: "enum_cmp_decline_reverse_primitives", Surface: "enum_cmp_decline",
			Template: `{{ 5 == Color.RED }}/{{ 5 != Color.RED }}/{{ true == Color.RED }}/{{ true != Color.RED }}/{{ none == Color.RED }}/{{ none != Color.RED }}`}, // false/true/false/true/false/true
		// The reverse comparison OPERATOR for the None < Some(alias) tie-break: the
		// existing rows only spell it with `<`.
		{ID: "enum_cmp_alias_option_reverse_order", Surface: "enum_cmp_decline",
			Template: `{{ Color.RED > Size.RED }}/{{ Size.RED > Color.RED }}`}, // true/false
		// A KNOWN-empty map is falsey, has length 0, and renders `{}` — the live-CFFI
		// control that separates it from the non-enumerable enum objects above, which
		// are truthy and have no length at all.
		{ID: "class_empty_truthiness", Surface: "class",
			Params: []Param{{Name: "e", BamlType: "Empty"}}, Args: map[string]any{"e": map[string]any{}},
			Template: `{% if e %}T{% else %}F{% endif %}/{{ e|length }}/{{ e }}`}, // F/0/{}
		// An EXPLICIT empty alias, Some(""), against a None control with the SAME
		// canonical name. Display is empty, but `.value` is still X. The comparator
		// compares the CANONICAL name against a string, never the alias, so comparing
		// the member to the empty string is false (and its negation true) even though
		// the member DISPLAYS as nothing — the row separates presentation from
		// comparison identity. Some("") then sorts strictly ABOVE None, so
		// NoAliasX.X < EmptyAlias.X, while the two are NOT equal: same canonical, and
		// the Option tie-break breaks it. None of this is expressible if an absent
		// alias and `@alias("")` collapse to the same value.
		{ID: "enum_empty_alias", Surface: "enum",
			Template: `{{ EmptyAlias.X }}/{{ EmptyAlias.X.value }}/{{ EmptyAlias.X == '' }}/{{ EmptyAlias.X != '' }}/{{ NoAliasX.X < EmptyAlias.X }}/{{ EmptyAlias.X == NoAliasX.X }}`}, // /X/false/true/true/false
		// The list-item none -> null display proof, as a checked-in CFFI row. The
		// element type is PARENTHESIZED: BAML v0.223 rejects the bare `string?[]`
		// spelling ("This line is invalid"), and `(string?)[]` is the accepted one.
		{ID: "list_item_none_null", Surface: "enum_container",
			Params: []Param{{Name: "xs", BamlType: "(string?)[]"}}, Args: map[string]any{"xs": []any{"a", nil}},
			Template: `{{ xs }}`}, // ["a", null]
		// Map REPRESENTATION, ENUMERABILITY and TRUTHINESS are three different things
		// and this row separates them: both objects are mappings, neither is iterable,
		// and both are truthy (no known length).
		{ID: "enum_non_enumerable_state", Surface: "enum_cmp_opaque",
			Template: `{{ Color.RED is iterable }}/{{ Color is iterable }}/{{ Color.RED is mapping }}/{{ Color is mapping }}/{% if Color.RED %}T{% else %}F{% endif %}/{% if Color %}T{% else %}F{% endif %}`}, // false/false/true/true/T/T
		// FAULT ROWS. Iterating either object RAISES; it is not silently an empty
		// loop. Compared by outcome class — the messages differ between Rust and Go.
		{ID: "enum_member_for_non_enumerable", Surface: "enum_cmp_opaque", Fault: OutcomeError,
			Template: `{% for x in Color.RED %}{{ x }}{% endfor %}`},
		{ID: "enum_namespace_for_non_enumerable", Surface: "enum_cmp_opaque", Fault: OutcomeError,
			Template: `{% for x in Color %}{{ x }}{% endfor %}`},

		// ============ PR-2 remediation round 2: the DISCRIMINATING host-map rows ============
		//
		// The cold re-review found three more surfaces that reach the SAME two
		// host-map shapes as the rows above — a non-enumerable enum member/namespace,
		// and an enumerable class map with an alias-aware render — but through
		// pycompat, the pprint/debug renderers and the map API, none of which the
		// existing rows exercise. Fixed generically in fork v2.16.0-baml.6
		// (PATCHES #106-#108). Each row is a direct stock-CFFI repro from the review.

		// --- CRITICAL: pycompat str.join over a non-enumerable map (finding #1) ---
		//
		// FAULT ROWS. `",".join(x)` takes `values.try_iter()?`; a non-enumerable map
		// has no iterator, so stock BAML errors `map is not iterable`. The profile
		// used to join the nil iterator as an EMPTY list and render "" — a native
		// success where BAML fails, the exact parity-decline violation. Compared by
		// outcome class; both legs must error.
		{ID: "pycompat_join_member_fault", Surface: "pycompat_map", Fault: OutcomeError,
			Template: `{{ ",".join(Color.RED) }}`},
		{ID: "pycompat_join_namespace_fault", Surface: "pycompat_map", Fault: OutcomeError,
			Template: `{{ ",".join(Color) }}`},
		// The control that separates a non-enumerable map from a KNOWN-EMPTY one: an
		// empty map still joins to "" because its pair iterator exists and is empty.
		{ID: "pycompat_join_empty_map_control", Surface: "pycompat_map",
			Template: `[{{ ",".join({}) }}]`}, // [] (empty join)

		// --- HIGH: pprint / debug honor the alias render (finding #2) ---
		//
		// `{value:#?}` of an object calls its render, and the class's alias-aware
		// render wins over a map rebuilt from its CANONICAL keys. The profile showed
		// `prop1` where stock shows the alias `key1`, in both alternate-debug
		// renderers, direct and nested. debug(x) is the same `{:#?}` call as pprint.
		{ID: "pprint_class_alias", Surface: "pprint_debug",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c|pprint }}`}, // {\n    "key1": "value",\n}
		{ID: "debug_class_alias", Surface: "pprint_debug",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ debug(c) }}`}, // {\n    "key1": "value",\n}
		// Nested: the object render is re-indented by the surrounding depth, the way
		// Rust's DebugList/DebugMap PadAdapter shifts a nested entry.
		{ID: "pprint_class_in_list", Surface: "pprint_debug",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ [c]|pprint }}`}, // [\n    {\n        "key1": "value",\n    },\n]
		{ID: "pprint_class_in_map", Surface: "pprint_debug",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ {"c": c}|pprint }}`}, // {\n    "c": {\n        "key1": "value",\n    },\n}
		// The enum member and namespace render bare under pprint/debug (no pairs),
		// the controls that keep the render dispatch from over-reaching.
		{ID: "pprint_enum_member", Surface: "pprint_debug",
			Template: `{{ Color.RED|pprint }}/{{ debug(Color.RED) }}`}, // rouge/rouge
		{ID: "pprint_enum_namespace", Surface: "pprint_debug",
			Template: `{{ Color|pprint }}`}, // {}

		// --- MEDIUM: the map API on a host map (finding #3) ---
		//
		// pycompat keys/values/items/get and dictsort reach a host map generically
		// (MapKeys/GetItem), not through a Go-map-only AsMap. A NON-enumerable map
		// yields empty views and a get-fallback; an ENUMERABLE class answers by its
		// CANONICAL keys. The profile used to decline both — `unknown method` and
		// `cannot convert value into pair list`.
		{ID: "pycompat_map_methods_enum", Surface: "pycompat_map",
			Template: `{{ Color.RED.keys()|list }}/{{ Color.RED.values()|list }}/{{ Color.RED.items()|list }}/{{ Color.RED.get("x", "fallback") }}`}, // []/[]/[]/fallback
		{ID: "pycompat_map_methods_class", Surface: "pycompat_map",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c.keys()|list }}/{{ c.values()|list }}/{{ c.items()|list }}/{{ c.get("prop1", "fallback") }}/{{ c.get("key1", "fallback") }}`}, // ["prop1"]/["value"]/[["prop1", "value"]]/value/fallback
		// dictsort for BOTH shapes: the class sorts its canonical pairs; the
		// non-enumerable enum member FAULTS `map is not iterable` (`ok!(v.try_iter())`).
		{ID: "dictsort_class", Surface: "pycompat_map",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c|dictsort }}`}, // [["prop1", "value"]]
		{ID: "dictsort_enum_member_fault", Surface: "pycompat_map", Fault: OutcomeError,
			Template: `{{ Color.RED|dictsort }}`},
		// The `|items|list` control, which already reached the class map through the
		// filter's GetItem fallback and must keep matching.
		{ID: "items_class_control", Surface: "pycompat_map",
			Params: []Param{{Name: "c", BamlType: "C"}}, Args: map[string]any{"c": map[string]any{"prop1": "value"}},
			Template: `{{ c|items|list }}`}, // [["prop1", "value"]]
		// A host LIST under pprint/debug. This is the discriminating control for
		// the scoping of the object-render dispatch (fork v2.16.0-baml.6): a
		// class render forces the alternate form and equals its pprint bytes, but
		// a list render RESPECTS the alternate flag — its debug_list is multi-line
		// under `{:#?}` while its top-level render is compact. So a host list
		// under pprint must be the MULTI-LINE debug_list, NOT the compact render
		// its ObjectString returns. baml.5 regressed this by using the render for
		// any object; baml.6 scopes the dispatch to map objects.
		{ID: "pprint_host_list_multiline", Surface: "pprint_debug",
			Params: []Param{{Name: "xs", BamlType: "Color[]"}}, Args: map[string]any{"xs": []any{"RED"}},
			Template: `{{ xs|pprint }}`}, // [\n    rouge,\n]
		{ID: "debug_host_list_multiline", Surface: "pprint_debug",
			Params: []Param{{Name: "xs", BamlType: "Color[]"}}, Args: map[string]any{"xs": []any{"RED"}},
			Template: `{{ debug(xs) }}`}, // [\n    rouge,\n]
	}
}
