package nativeprompt

import (
	"errors"
	"math"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	desc "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// typedArg builds a declared argument with an arbitrary retained TypeExpr.
func typedArg(name string, t *bamlparser.TypeExpr) promptdescriptor.Argument {
	return promptdescriptor.Argument{Name: name, Type: t}
}

// assertStaticDecline asserts the four required properties for every decline
// row: a non-nil error, errors.Is(err, ErrUnsupported), errors.As to *Decline
// with the EXACT feature key, and RenderStatic returning a nil RenderedPrompt
// (with the same decline). A test that merely observes a MiniJinja render error
// is insufficient, so RenderStatic must fail in the preparer, before render.
func assertStaticDecline(t *testing.T, fn promptdescriptor.Function, values []promptdescriptor.ArgumentValue, wantKey string) {
	t.Helper()

	err := SupportsStatic(fn, values)
	if err == nil {
		t.Fatalf("SupportsStatic: expected a decline (%s), got nil", wantKey)
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SupportsStatic error %v does not unwrap to ErrUnsupported", err)
	}
	var d *Decline
	if !errors.As(err, &d) {
		t.Fatalf("SupportsStatic error %v is not a *Decline", err)
	}
	if d.Feature != wantKey {
		t.Fatalf("SupportsStatic feature = %q, want %q (detail: %s)", d.Feature, wantKey, d.Detail)
	}

	rp, rerr := RenderStatic(fn, values)
	if rp != nil {
		t.Fatalf("RenderStatic returned a non-nil prompt on decline: %+v", rp)
	}
	if !errors.Is(rerr, ErrUnsupported) {
		t.Fatalf("RenderStatic error %v does not unwrap to ErrUnsupported", rerr)
	}
	var d2 *Decline
	if !errors.As(rerr, &d2) || d2.Feature != wantKey {
		t.Fatalf("RenderStatic feature = %v, want %q", rerr, wantKey)
	}
}

// declineCase is one row of the decline matrix. build returns a fresh function
// descriptor and its argument map so cases can freely mutate the valid base.
type declineCase struct {
	name string
	// build returns a fresh descriptor and the PROJECTED argument vector the
	// generated projector would hand the binder. Stating the vector explicitly is
	// deliberate: its order, names, and per-value kinds ARE the contract the
	// binder validates, so a row that spelled it as a raw map would be testing a
	// translation the production path does not have.
	build   func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue)
	feature string
}

// TestSupportsStaticDeclineMatrix is the complete static decline matrix. Every
// row proves the four properties in assertStaticDecline.
func TestSupportsStaticDeclineMatrix(t *testing.T) {
	cases := []declineCase{
		// --- FeatureStaticDescriptor: envelope + malformed bundle ------------
		{"descriptor_bad_version", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Version = 999
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_empty_method", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Method = ""
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_missing_client", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Client = ""
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_missing_provider", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Provider = ""
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_return_version_mismatch", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Return.Version = 999
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_return_method_mismatch", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("hi")
			fn.Return.Method = "Other"
			return fn, noVals()
		}, FeatureStaticDescriptor},
		{"descriptor_malformed_bundle_dangling_ref", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("{{ ctx.output_format }}")
			fn.Return = desc.Bundle{
				Version: desc.Version,
				Method:  "F",
				Target:  desc.Type{Kind: desc.TypeClass, Name: "Ghost", Mode: desc.NonStreaming},
			}
			return fn, noVals()
		}, FeatureStaticDescriptor},

		// --- FeatureTemplateString: any project macro set --------------------
		{"macro_set_present", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("{{ v }}", primArg("v", "string"))
			fn.Macros = []promptdescriptor.TemplateString{{Name: "Greet", Body: "hi"}}
			return fn, vals(argV("v", strV("x")))
		}, FeatureTemplateString},

		// --- FeatureMacro: inline macro/import/include blocks ----------------
		{"inline_macro_block", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{% macro greet(x) %}hi {{ x }}{% endmacro %}{{ greet('a') }}"), noVals()
		}, FeatureMacro},
		{"inline_import_block", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{% import 'other.j2' as o %}{{ o.x }}"), noVals()
		}, FeatureMacro},
		{"inline_include_block", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{% include 'other.j2' %}"), noVals()
		}, FeatureMacro},

		// --- FeatureStaticArgType: unsupported declared arg shapes -----------
		//
		// De-BAML Slice 7.1b: a Version-3 descriptor REQUIRES a resolved V3 value
		// type on every argument, so an argument shape V3 cannot state exactly
		// (bare/untyped, media, map, tuple, union, literal, a multi-dimensional
		// list, an attributed type node) never reaches this gate at all — the
		// BUILDER declines the whole function. Those rows live where the decline
		// now happens: internal/nativeschema/inputvalues_test.go (authoritative)
		// and internal/nativeprompt/staticoracle/decline_test.go (end-to-end).
		// What remains here is what a V3 descriptor CAN state but this slice
		// refuses to bind.
		{"arg_missing_v3_value_type", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("hi", typedArg("v", nil)), noVals()
		}, FeatureStaticArgType},
		{"arg_null_value_type", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "null")), vals(argV("v", nullV()))
		}, FeatureStaticArgType},
		{"arg_nullable_scalar", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", v3Arg("v", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueString, Nullable: true,
			})), vals(argV("v", strV("x")))
		}, FeatureStaticArgType},
		{"arg_nullable_enum", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := colorFn("{{ c }}", v3Arg("c", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueEnum, EnumName: "Color", Nullable: true,
			}))
			return fn, vals(argV("c", enumV("Color", "RED")))
		}, FeatureStaticArgType},
		{"arg_reserved_name_ctx", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("hi", primArg("ctx", "string")), vals(argV("ctx", strV("x")))
		}, FeatureStaticArgType},
		{"arg_shadows_enum_namespace", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			// An argument named `Color` would shadow the project enum namespace
			// global, so `Color.RED` in a template would silently mean "attribute
			// RED of the argument". Declining is the only honest answer.
			fn := colorFn("{{ Color }}", v3Arg("Color", promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}))
			return fn, vals(argV("Color", strV("x")))
		}, FeatureStaticArgType},
		{"arg_enum_not_in_universe", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			// The descriptor's own universe is the only authority: an edge naming
			// an enum it does not declare is a malformed descriptor.
			return staticFn("{{ c }}", v3Arg("c", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueEnum, EnumName: "Ghost",
			})), vals(argV("c", enumV("Ghost", "X")))
		}, FeatureStaticArgType},

		// --- FeatureEnumClassValue: attribute access / recursive closures -----
		{"expr_enum_global_bare_render", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			// A REAL, resolvable member token — and still declined: 7.1b renders a
			// BOUND enum argument, never a bare global member.
			return colorFn("{{ Color.RED }}"), noVals()
		}, FeatureEnumClassValue},
		{"expr_enum_global_access_unknown_enum", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ Color.RED }}"), noVals()
		}, FeatureEnumClassValue},
		{"expr_class_field_access", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ obj.field }}"), noVals()
		}, FeatureEnumClassValue},
		{"expr_enum_arg_value_attribute", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := colorFn("{{ c.value }}", v3Arg("c", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueEnum, EnumName: "Color",
			}))
			return fn, vals(argV("c", enumV("Color", "RED")))
		}, FeatureEnumClassValue},
		{"arg_recursive_class_closure", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("{{ n }}", v3Arg("n", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueClass, ClassName: "Node",
			}))
			fn.InputValues = promptdescriptor.InputValueUniverse{
				Classes: []promptdescriptor.ResolvedClass{{
					Name: "Node",
					Fields: []promptdescriptor.ResolvedClassField{
						{Canonical: "value", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
						{Canonical: "next", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Node"}},
					},
				}},
			}
			return fn, vals(argV("n", classV("Node", fieldV("value", strV("x")), fieldV("next", classV("Node")))))
		}, FeatureEnumClassValue},

		// --- FeatureStaticArgValue: the projected-vector gate (no coercion) ---
		//
		// A wrong GO type never reaches the binder: the generated projector
		// asserts the exact Go type and returns ok=false, so the seam is not even
		// installed (proven in cmd/introspect/projector_test.go). What the binder
		// owns is a projected vector that disagrees with the descriptor.
		{"value_vector_too_short", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "int")), noVals()
		}, FeatureStaticArgValue},
		{"value_vector_too_long", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("hi"), vals(argV("extra", intV(1)))
		}, FeatureStaticArgValue},
		{"value_vector_permuted", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			// Right names, wrong ORDER: the only evidence the projector and the
			// descriptor agree about which value is which.
			return staticFn("{{ a }}{{ b }}", primArg("a", "string"), primArg("b", "int")),
				vals(argV("b", intV(1)), argV("a", strV("x")))
		}, FeatureStaticArgValue},
		{"value_kind_string_for_int", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "int")), vals(argV("v", strV("5")))
		}, FeatureStaticArgValue},
		{"value_kind_float_for_int", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "int")), vals(argV("v", floatV(5.0)))
		}, FeatureStaticArgValue},
		{"value_kind_string_for_bool", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "bool")), vals(argV("v", strV("true")))
		}, FeatureStaticArgValue},
		{"value_null_for_scalar", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "string")), vals(argV("v", nullV()))
		}, FeatureStaticArgValue},
		{"value_invalid_utf8", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "string")), vals(argV("v", strV(string([]byte{0xff, 0xfe}))))
		}, FeatureStaticArgValue},
		{"value_float_nan", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "float")), vals(argV("v", floatV(math.NaN())))
		}, FeatureStaticArgValue},
		{"value_float_inf", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "float")), vals(argV("v", floatV(math.Inf(1))))
		}, FeatureStaticArgValue},
		{"value_duplicate_declaration", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "int"), primArg("v", "int")), vals(argV("v", intV(1)), argV("v", intV(1)))
		}, FeatureStaticArgValue},
		{"value_enum_wrong_type_name", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := colorFn("{{ c }}", v3Arg("c", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueEnum, EnumName: "Color",
			}))
			return fn, vals(argV("c", enumV("Other", "RED")))
		}, FeatureStaticArgValue},
		{"value_enum_unknown_member", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := colorFn("{{ c }}", v3Arg("c", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueEnum, EnumName: "Color",
			}))
			return fn, vals(argV("c", enumV("Color", "NOPE")))
		}, FeatureStaticArgValue},
		{"value_class_field_reordered", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := swatchFn("{{ s }}")
			return fn, vals(argV("s", classV("Swatch",
				fieldV("label", strV("l")), fieldV("color", enumV("Color", "RED")))))
		}, FeatureStaticArgValue},
		{"value_class_field_count", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := swatchFn("{{ s }}")
			return fn, vals(argV("s", classV("Swatch", fieldV("color", enumV("Color", "RED")))))
		}, FeatureStaticArgValue},
		{"value_list_item_kind", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := colorFn("{{ cs }}", v3Arg("cs", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueList,
				Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			}))
			return fn, vals(argV("cs", listV(enumV("Color", "RED"), strV("GREEN"))))
		}, FeatureStaticArgValue},

		// --- FeatureCallableOutputFmt: callable ctx.output_format ------------
		{"callable_output_format_render_null_as", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx.output_format(render_null_as="null") }}`), noVals()
		}, FeatureCallableOutputFmt},
		{"callable_output_format_empty", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx.output_format() }}`), noVals()
		}, FeatureCallableOutputFmt},

		// --- FeatureUnsupportedCtx: other ctx members ------------------------
		{"ctx_client", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx.client }}`), noVals()
		}, FeatureUnsupportedCtx},
		{"ctx_tags", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx.tags }}`), noVals()
		}, FeatureUnsupportedCtx},
		{"ctx_unknown_member", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx.something }}`), noVals()
		}, FeatureUnsupportedCtx},

		// --- FeatureUnknownFilter: any filter, incl replace ------------------
		{"filter_replace", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ v | replace("a","b") }}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnknownFilter},
		{"filter_format", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ x | format(type="yaml") }}`), noVals()
		}, FeatureUnknownFilter},
		{"filter_regex_match", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ x | regex_match("a.*") }}`), noVals()
		}, FeatureUnknownFilter},
		{"filter_sum", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ items | sum }}`), noVals()
		}, FeatureUnknownFilter},

		// --- FeaturePyFormatMethod: .format() / method calls -----------------
		{"py_format_on_literal", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ "{}".format(x) }}`), noVals()
		}, FeaturePyFormatMethod},
		{"method_call_on_arg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ v.upper() }}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeaturePyFormatMethod},

		// --- FeatureEnumComparison: comparison / containment -----------------
		{"cmp_enum_eq_string", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ Color.RED == "RED" }}`), noVals()
		}, FeatureEnumComparison},
		{"cmp_enum_self_eq", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ Color.RED == Color.RED }}`), noVals()
		}, FeatureEnumComparison},
		{"cmp_membership_in", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ "RED" in [Color.RED] }}`), noVals()
		}, FeatureEnumComparison},
		{"cmp_ordering", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ a >= b }}`), noVals()
		}, FeatureEnumComparison},

		// --- FeatureRoleCallShape: exotic role calls -------------------------
		{"role_cache_control_metadata", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system", cache_control={"type":"ephemeral"}) }}x`), noVals()
		}, FeatureRoleCallShape},
		{"role_allow_dupe", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("user", __baml_allow_dupe_role__=true) }}x`), noVals()
		}, FeatureRoleCallShape},
		{"role_custom", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("developer") }}x`), noVals()
		}, FeatureRoleCallShape},
		{"role_dynamic_arg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role(r) }}x`, primArg("r", "string")), vals(argV("r", strV("user")))
		}, FeatureRoleCallShape},
		{"role_both_positional_and_kwarg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system", role="user") }}x`), noVals()
		}, FeatureRoleCallShape},
		{"role_missing", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role() }}x`), noVals()
		}, FeatureRoleCallShape},

		// --- FeatureChatLayout: ordering / adjacency / emptiness -------------
		{"layout_content_before_role", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`hello{{ _.role("user") }}hi`), noVals()
		}, FeatureChatLayout},
		{"layout_adjacent_duplicate_role", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("user") }}a{{ _.role("user") }}b`), noVals()
		}, FeatureChatLayout},
		{"layout_empty_role_message", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}{{ _.role("user") }}hi`), noVals()
		}, FeatureChatLayout},

		// --- FeatureReservedDelimiter: magic markers in source/value/block --
		{"reserved_delimiter_in_source", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("before " + roleDelim + " after"), noVals()
		}, FeatureReservedDelimiter},
		{"reserved_delimiter_in_value", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "string")), vals(argV("v", strV(mediaDelim)))
		}, FeatureReservedDelimiter},
		// P1a: the rendered ctx.output_format block is user-controlled (a field
		// description here) and passes the same delimiter fence, so a schema name
		// equal to a magic delimiter declines instead of being split by lower.
		{"reserved_delimiter_in_output_format_block", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			fn := staticFn("{{ ctx.output_format }}")
			fn.Return = returnBundleWithFieldDescription("F", roleDelim)
			return fn, noVals()
		}, FeatureReservedDelimiter},

		// --- P1b: value-aware chat layout (empty/whitespace content declines) -
		{"chat_empty_string_arg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("user") }}{{ v }}`, primArg("v", "string")), vals(argV("v", strV("")))
		}, FeatureChatLayout},
		{"chat_whitespace_only_arg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("user") }}{{ v }}`, primArg("v", "string")), vals(argV("v", strV("  \n\t")))
		}, FeatureChatLayout},
		{"chat_empty_middle_message", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ _.role("user") }}{{ b }}`,
					primArg("a", "string"), primArg("b", "string")),
				vals(argV("a", strV("")), argV("b", strV("hi")))
		}, FeatureChatLayout},

		// --- P2: `+` whitespace-control forms are outside the allowlist -------
		{"ws_plus_both_edges", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{+ v +}}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},
		{"ws_plus_leading", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{+ v }}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},
		{"ws_plus_trailing", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ v +}}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},

		// --- P3: callable/bracket ctx.output_format spellings -> callable key -
		{"callable_output_format_tab_before_paren", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ ctx.output_format\t() }}"), noVals()
		}, FeatureCallableOutputFmt},
		{"callable_output_format_newline_before_paren", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ ctx.output_format\n() }}"), noVals()
		}, FeatureCallableOutputFmt},
		{"bracket_output_format_call", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx["output_format"]() }}`), noVals()
		}, FeatureCallableOutputFmt},
		{"bracket_output_format_bare", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ ctx['output_format'] }}`), noVals()
		}, FeatureCallableOutputFmt},

		// --- Round 2 P1: token-aware whitespace (MiniJinja lexer semantics) ---
		// Split identifiers under ordinary ASCII whitespace: MiniJinja lexes two
		// tokens, never fusing them, so these are NOT the allowlisted forms.
		{"ws_split_ident_ctx_output_format", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ ctx.output _format }}"), noVals()
		}, FeatureUnsupportedCtx},
		{"ws_split_ident_bare", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ foo bar }}"), noVals()
		}, FeatureUnrecognizedPrompt},
		// Whitespace-broken operator glue in the structured forms declines
		// (stricter than MiniJinja, which is allowed).
		{"ws_broken_glue_role", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _ .role("system") }}x`), noVals()
		}, FeatureRoleCallShape},
		{"ws_broken_glue_ctx", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ ctx . output_format }}"), noVals()
		}, FeatureUnsupportedCtx},
		// Non-lexer whitespace: form-feed (U+000C) and NBSP (U+00A0) are NOT
		// MiniJinja lexical whitespace, so the tag fails to lex and declines
		// (never a raw compile error after a nil SupportsStatic).
		{"nonlexer_ws_formfeed_bare", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{\f v }}", primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_formfeed_in_role", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ _.role(\f\"system\") }}x"), noVals()
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_nbsp_bare", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ v }}", primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_nbsp_split_ctx", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ ctx.output _format }}"), noVals()
		}, FeatureUnrecognizedPrompt},

		// --- Round 2 P2: `+` whitespace control inside comments --------------
		{"comment_plus_control", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("a\n{# c +#}\nb"), noVals()
		}, FeatureUnrecognizedPrompt},
		{"comment_plus_leading", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("a\n{#+ c #}\nb"), noVals()
		}, FeatureUnrecognizedPrompt},

		// --- Round 3 P1: escapes in role string literals ---------------------
		// A well-formed escape MiniJinja would DECODE to a non-standard role
		// (e.g. \t -> tab): declines FeatureRoleCallShape (never rendered as the
		// escape-free standard role). The prompt sources use backslash escapes
		// literally (raw strings), matching the .baml source bytes.
		{"role_escape_decodes_to_custom", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("sys\tem") }}x`), noVals()
		}, FeatureRoleCallShape},
		{"role_escape_backslash", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("user\\") }}x`), noVals()
		}, FeatureRoleCallShape},
		// A malformed/unvalidated escape (\u...) MiniJinja REJECTS: the tag fails
		// to lex, so it declines FeatureUnrecognizedPrompt with a nil prompt
		// instead of a raw "invalid unicode escape" compile error.
		{"role_escape_malformed_unicode", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("\user") }}x`), noVals()
		}, FeatureUnrecognizedPrompt},

		// --- Round 3 P2: capitalized MiniJinja literals are not bare args -----
		// A declared string arg named True must NOT be interpolated: MiniJinja
		// renders the literal `true`, not the bound value, so it declines.
		{"bare_capital_true_arg", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ True }}", primArg("True", "string")), vals(argV("True", strV("bound")))
		}, FeatureUnrecognizedPrompt},
		{"bare_capital_false", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ False }}"), noVals()
		}, FeatureUnrecognizedPrompt},
		{"bare_capital_none", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn("{{ None }}"), noVals()
		}, FeatureUnrecognizedPrompt},

		// --- Round 4 P1: reserved markers synthesized across boundaries ------
		// No single input piece holds the full delimiter; the COMPOSED rendered
		// text does (comment removal / interpolation / whitespace-control joins /
		// output-format / multiple args). Each must decline before lower.
		{"composed_interp_split", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x"+pre+"{{ v }}", primArg("v", "string")), vals(argV("v", strV(last)))
		}, FeatureReservedDelimiter},
		{"composed_comment_split", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x" + pre + "{# c #}" + last), noVals()
		}, FeatureReservedDelimiter},
		{"composed_two_arg_split", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("{{ a }}{{ b }}", primArg("a", "string"), primArg("b", "string")),
				vals(argV("a", strV(pre)), argV("b", strV(last)))
		}, FeatureReservedDelimiter},
		{"composed_media_split", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := mediaDelim[:len(mediaDelim)-1], mediaDelim[len(mediaDelim)-1:]
			return staticFn("x"+pre+"{{ v }}", primArg("v", "string")), vals(argV("v", strV(last)))
		}, FeatureReservedDelimiter},
		{"composed_ws_control_join", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			// `{{-` removes the separating space, joining the fragments.
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x"+pre+" {{- v }}", primArg("v", "string")), vals(argV("v", strV(last)))
		}, FeatureReservedDelimiter},

		// --- Round 5 P1: byte-faithful fence catches dedent + `-}}` synthesis --
		// BAML dedentTrim strips common leading indentation whose whitespace set
		// includes form-feed and NBSP; the trailing `-}}` then eats the newline,
		// joining the halves into a full delimiter in the ACTUAL rendered bytes.
		{"composed_ff_indent_join", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("\fx"+pre+"{{ v -}}\n\f"+last, primArg("v", "string")), vals(argV("v", strV("")))
		}, FeatureReservedDelimiter},
		{"composed_nbsp_indent_join", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn(" x"+pre+"{{ v -}}\n "+last, primArg("v", "string")), vals(argV("v", strV("")))
		}, FeatureReservedDelimiter},
		{"composed_ff_indent_join_media", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			pre, last := mediaDelim[:len(mediaDelim)-1], mediaDelim[len(mediaDelim)-1:]
			return staticFn("\fx"+pre+"{{ v -}}\n\f"+last, primArg("v", "string")), vals(argV("v", strV("")))
		}, FeatureReservedDelimiter},

		// --- Round 6 P1: standalone media markers (no media delimiter) --------
		// lower.parseBody recognizes a mediaMarkerPrefix..mediaMarkerSuffix body
		// segment as a MediaPart WITHOUT needing the media delimiter, so a chat
		// body composing those affixes (literal or from args, with a malformed or
		// a valid media-JSON body) must decline — never raw-error or synthesize a
		// MediaPart. mediaJSON is a well-formed media body that would otherwise
		// lower to a real image part.
		{"media_marker_literal_malformed", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}` + mediaMarkerPrefix + "V" + mediaMarkerSuffix), noVals()
		}, FeatureReservedDelimiter},
		{"media_marker_literal_valid_json", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}` + mediaMarkerPrefix + mediaJSON + mediaMarkerSuffix), noVals()
		}, FeatureReservedDelimiter},
		{"media_marker_arg_malformed", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ v }}{{ b }}`,
					primArg("a", "string"), primArg("v", "string"), primArg("b", "string")),
				vals(argV("a", strV(mediaMarkerPrefix)), argV("v", strV("V")), argV("b", strV(mediaMarkerSuffix)))
		}, FeatureReservedDelimiter},
		{"media_marker_arg_valid_json", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ v }}{{ b }}`,
					primArg("a", "string"), primArg("v", "string"), primArg("b", "string")),
				vals(argV("a", strV(mediaMarkerPrefix)), argV("v", strV(mediaJSON)), argV("b", strV(mediaMarkerSuffix)))
		}, FeatureReservedDelimiter},

		// --- FeatureUnrecognizedPrompt: catch-all ----------------------------
		{"stmt_if", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{% if x %}a{% endif %}`), noVals()
		}, FeatureUnrecognizedPrompt},
		{"stmt_for", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{% for m in messages %}{{ m }}{% endfor %}`), noVals()
		}, FeatureUnrecognizedPrompt},
		{"stmt_set", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{% set x = 1 %}{{ x }}`), noVals()
		}, FeatureUnrecognizedPrompt},
		{"expr_unknown_global", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ mystery }}`), noVals()
		}, FeatureUnrecognizedPrompt},
		{"expr_function_call", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ foo(1) }}`), noVals()
		}, FeatureUnrecognizedPrompt},
		{"expr_subscript", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ v[0] }}`, primArg("v", "string")), vals(argV("v", strV("x")))
		}, FeatureUnrecognizedPrompt},
		{"expr_unterminated_tag", func() (promptdescriptor.Function, []promptdescriptor.ArgumentValue) {
			return staticFn(`{{ v `), noVals()
		}, FeatureUnrecognizedPrompt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, args := tc.build()
			assertStaticDecline(t, fn, args, tc.feature)
		})
	}
}

// TestSupportsStaticAcceptsAllowlistedForms proves the closed allowlist accepts
// each exact output-producing form (SupportsStatic returns nil).
func TestSupportsStaticAcceptsAllowlistedForms(t *testing.T) {
	rolePre, roleLast := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
	cases := []struct {
		name string
		fn   promptdescriptor.Function
		args []promptdescriptor.ArgumentValue
	}{
		{"arg_interpolation", staticFn("{{ v }}", primArg("v", "string")), vals(argV("v", strV("x")))},
		{"bare_output_format", staticFn("{{ ctx.output_format }}"), noVals()},
		{"role_positional", staticFn(`{{ _.role("system") }}hi`), noVals()},
		{"role_kwarg", staticFn(`{{ _.role(role="user") }}hi`), noVals()},
		{"chat_positional", staticFn(`{{ _.chat("assistant") }}hi`), noVals()},
		{"chat_kwarg", staticFn(`{{ _.chat(role="user") }}hi`), noVals()},
		{"raw_text_and_comment", staticFn("plain text {# c #} more"), noVals()},
		// The scoped hyphen whitespace-control variants stay accepted (only the
		// `+` variants, tested as declines, are outside the allowlist).
		{"hyphen_ws_control_both_edges", staticFn("a\n{{- v -}}\nb", primArg("v", "string")), vals(argV("v", strV("x")))},
		{"hyphen_ws_control_leading", staticFn("a\n{{- v }}", primArg("v", "string")), vals(argV("v", strV("x")))},
		// Ordinary MiniJinja whitespace between tokens is insignificant, so the
		// canonical forms are accepted with extra spaces around the whole tag and
		// around the call parens/args (but NOT breaking the operator glue).
		{"extra_spaces_around_arg", staticFn("{{   v   }}", primArg("v", "string")), vals(argV("v", strV("x")))},
		{"role_space_before_paren", staticFn(`{{ _.role ("system") }}hi`), noVals()},
		{"role_spaces_in_parens", staticFn(`{{ _.role( "user" ) }}hi`), noVals()},
		{"comment_hyphen_control", staticFn("a\n{#- c -#}\nb"), noVals()},
		// The composed reserved-delimiter fence is precise, not over-broad: a
		// non-string arg (rendered non-empty, marker-free) genuinely separates two
		// marker halves, and role-marker boundaries separate per-message content,
		// so neither synthesizes a delimiter — both still render.
		{"composed_num_separator", staticFn("{{ a }}{{ n }}{{ b }}",
			primArg("a", "string"), primArg("n", "int"), primArg("b", "string")),
			vals(argV("a", strV(rolePre)), argV("n", intV(5)), argV("b", strV(roleLast)))},
		{"composed_cross_role_halves", staticFn(`{{ _.role("system") }}` + rolePre + `{{ _.role("user") }}` + roleLast), noVals()},
		// Round 5 P2: ordinary LITERAL whitespace is preserved by dedent/render/
		// lower (it is not whitespace-control), so a space/tab/NBSP or a
		// space-bearing value between two delimiter halves is a genuine separator —
		// no delimiter forms, and the byte-faithful fence must NOT falsely decline.
		{"composed_literal_space_separator", staticFn("x"+rolePre+" {{ v }}", primArg("v", "string")), vals(argV("v", strV(roleLast)))},
		{"composed_value_leading_space", staticFn("x"+rolePre+"{{ v }}", primArg("v", "string")), vals(argV("v", strV(" "+roleLast)))},
		{"composed_literal_tab_separator", staticFn("x"+rolePre+"\t{{ v }}", primArg("v", "string")), vals(argV("v", strV(roleLast)))},
		{"composed_nbsp_literal_separator", staticFn("x"+rolePre+"{{ a }}"+" "+"{{ b }}", primArg("a", "string"), primArg("b", "string")), vals(argV("a", strV("")), argV("b", strV(roleLast)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := SupportsStatic(tc.fn, tc.args); err != nil {
				t.Fatalf("SupportsStatic declined an allowlisted form: %v", err)
			}
			if _, err := RenderStatic(tc.fn, tc.args); err != nil {
				t.Fatalf("RenderStatic failed on an allowlisted form: %v", err)
			}
		})
	}
}
