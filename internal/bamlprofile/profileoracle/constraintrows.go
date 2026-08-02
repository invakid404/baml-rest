package profileoracle

// newlineExpr is a jinja expression evaluating to a single "\n", derived from the
// ENGINE's own alternate-debug renderer rather than written as a literal.
//
// It exists because BAML's constraint-attribute jinja parser does not honour a
// `\n` escape inside a string literal (measured: `'a\nb'` compares as the literal
// four characters, so an exact-bytes comparison written that way silently fails
// on BOTH legs and proves nothing). `[1]|pprint` renders "[\n    1,\n]" on both
// engines, so index 1 of it IS the newline.
//
// The pprint rows below use it as `|replace(newlineExpr, '|')`, which makes their
// comparison an EXACT byte pin under a documented 1:1 substitution — every byte
// of the rendering is spelled out, with only the newline glyph written as `|`.
// Each such row also pins `|pprint|length`, so the substitution cannot be hiding
// a different number of newlines, and asserts `newlineExpr|length == 1` so the
// derivation itself is checked rather than assumed.
const newlineExpr = `([1]|pprint)[1]`

// ConstraintCorpus is the constraint differential corpus: the rows that must be
// proven against LIVE stock BAML v0.223 CFFI, not against hand-written Go
// goldens.
//
// Every row declares a RETURN TYPE carrying @check/@assert attributes, an
// unambiguous raw response text, and the plain-Go value that text coerces to.
// The stock leg parses the text through BamlRuntime.CallFunctionParse (which runs
// BAML's real coercer -> run_user_checks -> evaluate_predicate -> validate_asserts);
// the profile leg lowers the same value through hostValue and calls
// bamlprofile.EvaluateConstraints. The two outcomes are compared by class, and a
// parsed row additionally compares its evaluated checks.
//
// # Raw text uses ALIASES; `this` uses CANONICAL names
//
// This is not incidental — it is what makes the projection rows discriminating,
// and it was MEASURED, not assumed:
//
//   - an enum variant coerces from its resolved ALIAS. `Color.RED` is
//     @alias("rouge"), so the raw response must say `rouge`; the raw text `RED`
//     is a coercion ERROR ("Expected Color ... enum value, got String(\"RED\")").
//   - a class field coerces from its ALIAS KEY. `C.prop1` is @alias("key1"), so
//     the raw JSON must say `{"key1": ...}`; the canonical spelling produces a
//     different, nonsensical value.
//
// So the wire says `rouge`/`key1` and the constraint's `this` says `RED`/`prop1`.
// A row like enum_canonical_equal therefore cannot pass by accident: the string
// it asserts appears NOWHERE in the input.
//
// SCOPE OF THE PROOF — read before treating a green run as full constraint parity:
//
//   - A bare `string` return type is NOT here, and cannot be. Stock BAML's
//     jsonish::from_str short-circuits `TypeIR::Primitive(TypeValue::String, _)`
//     before any coercion (jsonish/src/lib.rs:233-237), so its constraints are
//     never evaluated at all. That measured asymmetry is pinned by
//     TestStockSkipsConstraintsOnBareStringReturn and ledgered for Slice 7.2;
//     string-valued predicates are exercised here through a class FIELD instead.
//   - The expression SURFACE is earned row by row. The fork is a BAML-exact
//     engine, but this corpus only claims the constructs it actually exercises.
//     Anything else stays unclaimed rather than assumed (see doc.go's declines).
//   - Media values and BamlValue::Map have no proven constraint ingress, because
//     PR-2's host model provides no constructor for them. They are declined by
//     the projection and are deliberately absent here; the gap is ledgered on
//     #583/#602/#572 as applicable.
//   - Duplicate check LABELS are probed, not policed. Stock's Go client collapses
//     the ordered CFFI check list into a map, so the observable collapse is
//     measured here and the stable policy is Slice 7.2's to define.
//   - Error TEXT is diagnostic. A fault row compares the outcome CLASS, exactly
//     as the prompt corpus's fault rows do, because Rust's and Go's messages for
//     the same failure are different strings.
//
// The shared enum/class declarations come from types.go (the same types.baml the
// prompt corpus generates), so an enum's resolved alias and a class's declared
// field order describe one set of declarations on both legs.
func ConstraintCorpus() []ConstraintRow {
	return []ConstraintRow{
		// ===================== core predicate =====================
		// The evaluator itself: a true/false predicate over `this`, the literal
		// true/false forms, and the rendered-TEXT classifier — including the
		// string-looking "true"/"false" that a boolean DOWNCAST would reject and the
		// near-miss texts that a TRIMMING classifier would wrongly accept.
		{ID: "core_assert_pass", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this > 0")}},
		{ID: "core_assert_fail", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this > 100")}, Expect: ConstraintAssertFailed},
		{ID: "core_check_pass", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{ck("gt_zero", "this > 0")}},
		{ID: "core_check_fail", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{ck("gt_hundred", "this > 100")}},
		{ID: "core_literal_true", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("true")}},
		{ID: "core_literal_false", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("false")}, Expect: ConstraintAssertFailed},
		// A real boolean `this` renders the text "true" — the case where
		// rendered-text and boolean-downcast semantics agree, and the control for
		// the two rows below where they do not.
		{ID: "core_bool_this_true", Surface: "core", ReturnType: "bool", Raw: "true", This: true,
			Constraints: []ConstraintDecl{as_("this")}},
		{ID: "core_bool_this_false", Surface: "core", ReturnType: "bool", Raw: "false", This: false,
			Constraints: []ConstraintDecl{as_("this")}, Expect: ConstraintAssertFailed},
		// STRING-looking booleans: the predicate never produces a bool at all, yet
		// its rendered text is exactly "true"/"false". BAML accepts both, because it
		// matches the rendered string.
		{ID: "core_string_true_passes", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`"true"`)}},
		{ID: "core_string_false_fails", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`"false"`)}, Expect: ConstraintAssertFailed},
		// NON-BOOLEAN output: an evaluator error, never a failed predicate. The
		// whitespace and capitalization rows are the ones that separate exact
		// matching from a trimming/case-folding classifier.
		{ID: "core_nonboolean_number", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this")}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_string_field", Surface: "core", ReturnType: "C", Raw: `{"key1": "value"}`,
			This:        map[string]any{"prop1": "value"},
			Constraints: []ConstraintDecl{as_("this.prop1")}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_leading_space", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`" true"`)}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_trailing_space", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`"true "`)}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_capitalized", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`"True"`)}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_empty", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`""`)}, Expect: ConstraintEvalError},
		{ID: "core_nonboolean_none", Surface: "core", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("none")}, Expect: ConstraintEvalError},

		// ===================== levels & labels =====================
		// A check must carry a label; an assert may or may not. A false ASSERT is
		// terminal; a false CHECK survives as metadata on a successfully parsed
		// value. Order and level are both preserved across a mixed batch.
		{ID: "levels_assert_unlabelled_pass", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this > 0")}},
		{ID: "levels_assert_labelled_pass", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{asl("positive", "this > 0")}},
		{ID: "levels_assert_labelled_fail", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{asl("too_small", "this > 100")}, Expect: ConstraintAssertFailed},
		// THE assert/check split, both halves, each with the OTHER level present so
		// neither row can pass by accident:
		//   - a false CHECK next to a passing assert -> the value parses and the
		//     failed check comes back as metadata;
		//   - a false ASSERT next to a passing check -> the parse is rejected and no
		//     check metadata survives at all.
		{ID: "levels_false_check_survives", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{asl("positive", "this > 0"), ck("big", "this > 100")}},
		{ID: "levels_false_assert_rejects", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{ck("positive", "this > 0"), as_("this > 100")},
			Expect:      ConstraintAssertFailed},
		// A mixed multi-constraint batch: two checks with different outcomes and two
		// asserts, all passing at the assert level so the row parses and every check
		// is observable.
		{ID: "levels_mixed_multi", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{
				ck("c_first", "this > 0"),
				as_("this > 1"),
				ck("c_second", "this > 100"),
				asl("a_last", "this > 2"),
			}},
		// DUPLICATE-LABEL PROBE (scope open question #2). BAML evaluates both in
		// order, but the Go client folds the ordered CFFI check list into a map, so
		// only one label survives the response representation. The differential
		// measures which; Slice 7.2 owns the documented policy. The two predicates
		// have OPPOSITE outcomes so the collapse is visible rather than idempotent.
		{ID: "levels_duplicate_label_probe", Surface: "levels", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{ck("dup", "this > 0"), ck("dup", "this > 100")}},

		// ===================== get_env inside a predicate =====================
		// The predicate environment is BAML's get_env(): the builtin registry plus
		// BAML's regex_match and sum overrides and the pycompat unknown-method
		// callback. Each row reaches one of them from inside a constraint.
		//
		// The string-valued rows go through a class FIELD rather than a `string`
		// return type, because stock never evaluates constraints on the latter (see
		// the file doc).
		{ID: "getenv_length", Surface: "get_env", ReturnType: "string[]", Raw: `["a", "b", "c"]`,
			This:        []any{"a", "b", "c"},
			Constraints: []ConstraintDecl{as_("this|length == 3")}},
		{ID: "getenv_sum_ints", Surface: "get_env", ReturnType: "int[]", Raw: "[1, 2, 3]",
			This:        []any{int64(1), int64(2), int64(3)},
			Constraints: []ConstraintDecl{as_("this|sum == 6")}},
		// BAML's sum OVERRIDE returns 0 for an empty list rather than the builtin's
		// behavior — the discriminating arm of the override.
		{ID: "getenv_sum_empty", Surface: "get_env", ReturnType: "int[]", Raw: "[]",
			This:        []any{},
			Constraints: []ConstraintDecl{as_("this|sum == 0")}},
		{ID: "getenv_regex_match_true", Surface: "get_env", ReturnType: "C", Raw: `{"key1": "abc123"}`,
			This:        map[string]any{"prop1": "abc123"},
			Constraints: []ConstraintDecl{as_(`this.prop1|regex_match("[0-9]+")`)}},
		{ID: "getenv_regex_match_false", Surface: "get_env", ReturnType: "C", Raw: `{"key1": "abcdef"}`,
			This:        map[string]any{"prop1": "abcdef"},
			Constraints: []ConstraintDecl{as_(`this.prop1|regex_match("[0-9]+")`)}, Expect: ConstraintAssertFailed},
		{ID: "getenv_pycompat_upper", Surface: "get_env", ReturnType: "C", Raw: `{"key1": "abc"}`,
			This:        map[string]any{"prop1": "abc"},
			Constraints: []ConstraintDecl{as_(`this.prop1.upper() == "ABC"`)}},
		{ID: "getenv_pycompat_startswith", Surface: "get_env", ReturnType: "C", Raw: `{"key1": "hello"}`,
			This:        map[string]any{"prop1": "hello"},
			Constraints: []ConstraintDecl{as_(`this.prop1.startswith("he")`)}},

		// ===================== enum serde projection =====================
		// THE host-model split. BAML's PROMPT lowering makes an enum a host object
		// whose display is the alias and whose `.value` is the canonical name; its
		// CONSTRAINT lowering makes it the canonical STRING and nothing else.
		//
		// Color.RED is @alias("rouge"), and the raw response says `rouge` because
		// that is what stock's coercer accepts — so "RED" appears nowhere in the
		// input and every passing row below is proving the canonical projection.
		{ID: "enum_canonical_equal", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{as_(`this == "RED"`)}},
		{ID: "enum_alias_not_equal", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{as_(`this == "rouge"`)}, Expect: ConstraintAssertFailed},
		{ID: "enum_is_string", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{as_("this is string")}},
		// The prompt host object answers `.value`; the projected string has no
		// attributes. This is the display-sensitive probe that fails loudly if the
		// PR-2 object is ever bound directly.
		{ID: "enum_no_value_attribute", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{as_("this.value is undefined")}},
		// Under the prompt lowering this would be "ROUGE".
		{ID: "enum_upper_is_canonical", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{as_(`this|upper == "RED"`)}},
		// An UNALIASED variant, where the wire and canonical spellings coincide —
		// the control that keeps the aliased rows from being read as "the projection
		// always differs from the wire".
		{ID: "enum_unaliased_variant", Surface: "enum", ReturnType: "Color", Raw: "GREEN", This: "GREEN",
			Constraints: []ConstraintDecl{as_(`this == "GREEN"`)}},
		{ID: "enum_check_canonical", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{ck("is_red", `this == "RED"`)}},
		{ID: "enum_check_alias_fails", Surface: "enum", ReturnType: "Color", Raw: "rouge", This: "RED",
			Constraints: []ConstraintDecl{ck("is_rouge", `this == "rouge"`)}},

		// ===================== class serde projection =====================
		// A class is a plain mapping keyed by CANONICAL field names. C.prop1 is
		// @alias("key1"), and the wire uses `key1`, so the canonical key is a
		// property of the PROJECTION and not an echo of the input.
		{ID: "class_canonical_field", Surface: "class", ReturnType: "C", Raw: `{"key1": "value"}`,
			This:        map[string]any{"prop1": "value"},
			Constraints: []ConstraintDecl{as_(`this.prop1 == "value"`)}},
		{ID: "class_alias_absent", Surface: "class", ReturnType: "C", Raw: `{"key1": "value"}`,
			This:        map[string]any{"prop1": "value"},
			Constraints: []ConstraintDecl{as_("this.key1 is undefined")}},
		{ID: "class_is_mapping", Surface: "class", ReturnType: "C", Raw: `{"key1": "value"}`,
			This:        map[string]any{"prop1": "value"},
			Constraints: []ConstraintDecl{as_("this is mapping"), as_(`this|list|join(",") == "prop1"`)}},
		{ID: "class_length", Surface: "class", ReturnType: "WithColor", Raw: `{"colour": "rouge", "n": 4}`,
			This:        map[string]any{"color": "RED", "n": int64(4)},
			Constraints: []ConstraintDecl{as_("this|length == 2")}},
		// ORDER-SENSITIVE probes. They use Ow, whose DECLARED order (zeta, alpha) is
		// the reverse of both its sorted order (alpha, zeta) and its alias order
		// (a, z) — so a passing row can only be explained by BAML's insertion-ordered
		// BamlMap, reproduced by the fork's ordered map in the projection. Every
		// other multi-field class in the corpus is declared in sorted order, where a
		// plain Go map would look identical.
		{ID: "class_order_declared_not_sorted", Surface: "class", ReturnType: "Ow", Raw: `{"z": 1, "a": "v"}`,
			This: map[string]any{"zeta": int64(1), "alpha": "v"},
			Constraints: []ConstraintDecl{
				as_(`this|list|join(",") == "zeta,alpha"`),
				as_(`this|list|join(",") != "alpha,zeta"`),
			}},
		// The raw JSON deliberately presents the fields in the OPPOSITE order to the
		// declaration, and in sorted-alias order at that. Stock still yields DECLARED
		// order, so this is the row that separates "declared" from "wire" while the
		// row above separates "declared" from "sorted".
		{ID: "class_order_wire_reversed", Surface: "class", ReturnType: "Ow", Raw: `{"a": "v", "z": 1}`,
			This:        map[string]any{"zeta": int64(1), "alpha": "v"},
			Constraints: []ConstraintDecl{as_(`this|list|join(",") == "zeta,alpha"`)}},
		// Order-sensitive probes that do NOT go through |list, so the ordering claim
		// does not rest on one filter's implementation. `|first` and `|items` follow
		// the mapping's own iteration order (zeta first), while `|dictsort` sorts by
		// key (alpha first) — the pair shows the natural order is genuinely
		// insertion order rather than sorted order that happens to agree.
		{ID: "class_order_iteration", Surface: "class", ReturnType: "Ow", Raw: `{"z": 1, "a": "v"}`,
			This: map[string]any{"zeta": int64(1), "alpha": "v"},
			Constraints: []ConstraintDecl{
				as_(`this|first == "zeta"`),
				as_(`this|items|first|first == "zeta"`),
				as_(`this|dictsort|first|first == "alpha"`),
			}},
		// The projected class's STRING rendering is the ordinary fork mapping
		// render, in insertion order — NOT the PR-2 prompt class's forced-alternate
		// Rust `{map:#?}` debug bytes with alias keys. This is the class analogue of
		// enum_no_value_attribute: it fails loudly if the prompt host object is ever
		// bound to `this`.
		// The ALTERNATE-DEBUG renderers, |pprint and debug(), pinned byte-exactly.
		//
		// This is a separate fork code path from |string (fork v2.16.0-baml.6
		// PATCHES #106-#108 scope the object-render dispatch to the MAP arm, and the
		// list arm respects the ambient alternate flag), so a green |string row does
		// not cover it. It is exactly where PR-2's classObject would leak its
		// ALIAS-keyed `{map:#?}` rendering into a predicate if the prompt object were
		// ever bound to `this`: the prompt lowering renders
		// `{|    "z": 1,|    "a": "v",|}` where the serde projection renders
		// `{|    "zeta": 1,|    "alpha": "v",|}` (newlines shown as `|`; see
		// newlineExpr). Ow's aliases are one character long, so the two spellings
		// even differ in LENGTH, which the |length assertion pins independently.
		{ID: "class_pprint_is_serde_map", Surface: "class", ReturnType: "Ow", Raw: `{"z": 1, "a": "v"}`,
			This: map[string]any{"zeta": int64(1), "alpha": "v"},
			Constraints: []ConstraintDecl{
				// The newline derivation itself, checked rather than assumed.
				as_(newlineExpr + `|length == 1`),
				// Exact byte count of the rendering — pins the newline count too.
				as_(`this|pprint|length == 36`),
				// Exact bytes, newline written as `|`.
				as_(`this|pprint|replace(` + newlineExpr + `, '|') == '{|    "zeta": 1,|    "alpha": "v",|}'`),
				// The recorded counter-value: the PROMPT lowering's exact bytes, which
				// must NOT be what a predicate sees.
				as_(`this|pprint|replace(` + newlineExpr + `, '|') != '{|    "z": 1,|    "a": "v",|}'`),
				// debug() is the same `{:#?}` call through a different entry point.
				as_(`debug(this)|replace(` + newlineExpr + `, '|') == '{|    "zeta": 1,|    "alpha": "v",|}'`),
			}},
		{ID: "class_string_render_is_not_prompt_debug", Surface: "class", ReturnType: "Ow", Raw: `{"z": 1, "a": "v"}`,
			This: map[string]any{"zeta": int64(1), "alpha": "v"},
			// Single-quoted on purpose: BAML's jinja expression parser rejects a
			// backslash-escaped quote inside a constraint attribute ("unexpected input
			// after expression"), so the double quotes have to be the literal's
			// contents rather than its delimiters.
			Constraints: []ConstraintDecl{as_(`this|string == '{"zeta": 1, "alpha": "v"}'`)}},
		{ID: "class_enum_field_canonical", Surface: "class", ReturnType: "Ecw", Raw: `{"colour": "rouge"}`,
			This:        map[string]any{"color": "RED"},
			Constraints: []ConstraintDecl{as_(`this.color == "RED"`), as_("this.colour is undefined")}},
		{ID: "class_enum_field_alias_fails", Surface: "class", ReturnType: "Ecw", Raw: `{"colour": "rouge"}`,
			This:        map[string]any{"color": "RED"},
			Constraints: []ConstraintDecl{as_(`this.color == "rouge"`)}, Expect: ConstraintAssertFailed},
		// The RENDER of a class holding an enum. Attribute-access rows cannot tell
		// the two lowerings apart — PR-2's classObject also answers `.color` by the
		// canonical name, and its enum member's comparator also equals the canonical
		// string — so this is the row that does: the prompt lowering would render
		// "{\n    \"colour\": rouge,\n}" (alias key, bare alias value, forced
		// alternate debug) where the serde projection renders {"color": "RED"}.
		{ID: "class_enum_render_is_not_prompt_debug", Surface: "class", ReturnType: "Ecw", Raw: `{"colour": "rouge"}`,
			This:        map[string]any{"color": "RED"},
			Constraints: []ConstraintDecl{as_(`this|string == '{"color": "RED"}'`)}},
		// Nested class -> class -> list recursion, with both alias keys absent at
		// both depths.
		{ID: "class_nested_recursion", Surface: "class", ReturnType: "Cw",
			Raw:  `{"nested": {"data": ["a", "b"]}}`,
			This: map[string]any{"inner": map[string]any{"items": []any{"a", "b"}}},
			Constraints: []ConstraintDecl{
				as_(`this.inner.items[1] == "b"`),
				as_("this.nested is undefined"),
				as_("this.inner.data is undefined"),
				as_("this.inner.items|length == 2"),
			}},
		// A null optional field is PRESENT as none, not absent: length is 1 and the
		// key iterates. That distinction is invisible to `is none` alone (undefined
		// is not none, but an absent key would also make the length 0), so both are
		// asserted.
		{ID: "class_nested_none", Surface: "class", ReturnType: "Nw", Raw: `{"perhaps": null}`,
			This:        map[string]any{"maybe": nil},
			Constraints: []ConstraintDecl{as_("this.maybe is none"), as_("this|length == 1"), as_(`this|list|join(",") == "maybe"`)}},
		// The same, with the optional field OMITTED from the wire entirely — stock
		// still materializes it as a present none.
		{ID: "class_omitted_optional_is_none", Surface: "class", ReturnType: "Nw", Raw: `{}`,
			This:        map[string]any{},
			Constraints: []ConstraintDecl{as_("this.maybe is none"), as_("this|length == 1")}},
		{ID: "class_nested_present_optional", Surface: "class", ReturnType: "Nw", Raw: `{"perhaps": "here"}`,
			This:        map[string]any{"maybe": "here"},
			Constraints: []ConstraintDecl{as_(`this.maybe == "here"`)}},
		{ID: "class_check_field", Surface: "class", ReturnType: "C", Raw: `{"key1": "value"}`,
			This:        map[string]any{"prop1": "value"},
			Constraints: []ConstraintDecl{ck("has_value", `this.prop1 == "value"`)}},

		// ===================== list serde projection =====================
		// A list is an ordinary sequence: length, index, membership, iteration. The
		// PR-2 host list's compact/alternate debug rendering is prompt-only and must
		// not be reachable.
		{ID: "list_length", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{as_("this|length == 2")}},
		{ID: "list_index", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{as_(`this[0] == "a"`)}},
		{ID: "list_membership", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{as_(`"b" in this`)}},
		{ID: "list_is_sequence", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{as_("this is sequence")}},
		{ID: "list_join", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{as_(`this|join(",") == "a,b"`)}},
		// Enum items inside a list project to their canonical strings too, so the
		// recursion is proven at the list level and not just the class level.
		{ID: "list_enum_items_canonical", Surface: "list", ReturnType: "Color[]", Raw: `["rouge", "GREEN"]`,
			This:        []any{"RED", "GREEN"},
			Constraints: []ConstraintDecl{as_(`this[0] == "RED"`), as_(`this[1] == "GREEN"`), as_("this|length == 2")}},
		// The RENDER of a list of enums — the list analogue of
		// class_enum_render_is_not_prompt_debug, and the only list row that is
		// discriminating. Length, index, membership, `is sequence` and |join all
		// answer identically for PR-2's listObject (it is an ObjectReprSeq with
		// SeqLen/SeqItem) and for a plain projected slice, and its enum items even
		// compare EQUAL to the canonical string through the PR-2 comparator. The
		// rendering is where they part: the prompt lowering is "[rouge, GREEN]"
		// (bare aliases, Rust debug_list) and the projection is ["RED", "GREEN"].
		{ID: "list_enum_render_is_not_prompt_debug", Surface: "list", ReturnType: "Color[]", Raw: `["rouge", "GREEN"]`,
			This:        []any{"RED", "GREEN"},
			Constraints: []ConstraintDecl{as_(`this|string == '["RED", "GREEN"]'`)}},
		// The list half of the alternate-debug pin (see class_pprint_is_serde_map).
		// A host list's |pprint is NOT its |string: the fork's list arm respects the
		// ambient alternate flag, so the compact top-level render and the multi-line
		// debug_list are different code paths, and only this row covers the second.
		// PR-2's listObject would render its items as BARE aliases —
		// `[|    rouge,|    GREEN,|]` — where the serde projection renders quoted
		// canonical strings.
		{ID: "list_pprint_is_serde_seq", Surface: "list", ReturnType: "Color[]", Raw: `["rouge", "GREEN"]`,
			This: []any{"RED", "GREEN"},
			Constraints: []ConstraintDecl{
				as_(newlineExpr + `|length == 1`),
				as_(`this|pprint|length == 27`),
				as_(`this|pprint|replace(` + newlineExpr + `, '|') == '[|    "RED",|    "GREEN",|]'`),
				// The recorded counter-value: PR-2's prompt-list debug spelling.
				as_(`this|pprint|replace(` + newlineExpr + `, '|') != '[|    rouge,|    GREEN,|]'`),
				as_(`debug(this)|replace(` + newlineExpr + `, '|') == '[|    "RED",|    "GREEN",|]'`),
			}},
		{ID: "list_enum_items_alias_fails", Surface: "list", ReturnType: "Color[]", Raw: `["rouge"]`,
			This:        []any{"RED"},
			Constraints: []ConstraintDecl{as_(`this[0] == "rouge"`)}, Expect: ConstraintAssertFailed},
		{ID: "list_check", Surface: "list", ReturnType: "string[]", Raw: `["a", "b"]`,
			This:        []any{"a", "b"},
			Constraints: []ConstraintDecl{ck("nonempty", "this|length > 0")}},

		// ===================== context isolation =====================
		// MEASURED rows proving the constraint environment is a bare get_env(): the
		// three globals BAML's PROMPT renderer injects are undefined here. Adding
		// any of them would be an out-do — the profile would answer where stock
		// BAML has nothing.
		{ID: "iso_ctx_undefined", Surface: "isolation", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("ctx is undefined")}},
		{ID: "iso_role_helper_undefined", Surface: "isolation", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("_ is undefined")}},
		{ID: "iso_enum_namespace_undefined", Surface: "isolation", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("Color is undefined")}},
		// Reaching THROUGH an absent global is an evaluator error rather than a
		// quietly-undefined value, on both legs.
		{ID: "iso_enum_member_access_errors", Surface: "isolation", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`Color.RED == "RED"`)}, Expect: ConstraintEvalError},
		{ID: "iso_ctx_output_format_errors", Surface: "isolation", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_(`ctx.output_format == ""`)}, Expect: ConstraintEvalError},

		// ===================== faults =====================
		// Predicates stock BAML actually FAILS on. Each is compared by outcome
		// class through the same discipline the prompt corpus's fault rows use: the
		// declaration is asserted against the live stock leg, so it cannot rot, and
		// a profile that renders a conservative `false` where stock errors is NOT
		// green.
		{ID: "fault_unknown_filter", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this|no_such_filter")}, Expect: ConstraintEvalError},
		{ID: "fault_length_of_int", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this|length > 0")}, Expect: ConstraintEvalError},
		{ID: "fault_attribute_chain_through_undefined", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this.a.b == 1")}, Expect: ConstraintEvalError},
		{ID: "fault_floordiv_by_zero", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this // 0 > 0")}, Expect: ConstraintEvalError},
		{ID: "fault_mod_by_zero", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this % 0 > 0")}, Expect: ConstraintEvalError},
		// TRUE division by zero is NOT an error in either engine — it is f64
		// infinity, so the comparison succeeds. The row is here precisely because it
		// looks like a fault and is not: without it, the two `... by zero` rows above
		// would read as a general rule the engines do not actually have.
		{ID: "fault_truediv_by_zero_is_infinity", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this / 0 > 0")}},
		// An error in a LATER constraint aborts the batch: the earlier passing check
		// must not come back as a partial report on either leg.
		{ID: "fault_aborts_batch", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{ck("ok", "this > 0"), as_("this|no_such_filter")},
			Expect:      ConstraintEvalError},
		// An error alongside a FAILING assert: the evaluator error wins, because
		// run_user_checks aborts before validate_asserts ever runs.
		{ID: "fault_beats_failing_assert", Surface: "fault", ReturnType: "int", Raw: "5", This: int64(5),
			Constraints: []ConstraintDecl{as_("this > 100"), as_("this|no_such_filter")},
			Expect:      ConstraintEvalError},
	}
}
