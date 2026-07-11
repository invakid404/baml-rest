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
func assertStaticDecline(t *testing.T, fn promptdescriptor.Function, args map[string]any, wantKey string) {
	t.Helper()

	err := SupportsStatic(fn, args)
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

	rp, rerr := RenderStatic(fn, args)
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
	name    string
	build   func() (promptdescriptor.Function, map[string]any)
	feature string
}

func noArgs() map[string]any { return map[string]any{} }

// TestSupportsStaticDeclineMatrix is the complete static decline matrix. Every
// row proves the four properties in assertStaticDecline.
func TestSupportsStaticDeclineMatrix(t *testing.T) {
	cases := []declineCase{
		// --- FeatureStaticDescriptor: envelope + malformed bundle ------------
		{"descriptor_bad_version", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Version = 999
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_empty_method", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Method = ""
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_missing_client", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Client = ""
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_missing_provider", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Provider = ""
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_return_version_mismatch", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Return.Version = 999
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_return_method_mismatch", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("hi")
			fn.Return.Method = "Other"
			return fn, noArgs()
		}, FeatureStaticDescriptor},
		{"descriptor_malformed_bundle_dangling_ref", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("{{ ctx.output_format }}")
			fn.Return = desc.Bundle{
				Version: desc.Version,
				Method:  "F",
				Target:  desc.Type{Kind: desc.TypeClass, Name: "Ghost", Mode: desc.NonStreaming},
			}
			return fn, noArgs()
		}, FeatureStaticDescriptor},

		// --- FeatureTemplateString: any project macro set --------------------
		{"macro_set_present", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("{{ v }}", primArg("v", "string"))
			fn.Macros = []promptdescriptor.TemplateString{{Name: "Greet", Body: "hi"}}
			return fn, map[string]any{"v": "x"}
		}, FeatureTemplateString},

		// --- FeatureMacro: inline macro/import/include blocks ----------------
		{"inline_macro_block", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{% macro greet(x) %}hi {{ x }}{% endmacro %}{{ greet('a') }}"), noArgs()
		}, FeatureMacro},
		{"inline_import_block", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{% import 'other.j2' as o %}{{ o.x }}"), noArgs()
		}, FeatureMacro},
		{"inline_include_block", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{% include 'other.j2' %}"), noArgs()
		}, FeatureMacro},

		// --- FeatureStaticArgType: unsupported declared arg shapes -----------
		{"arg_bare_untyped", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", typedArg("v", nil)), noArgs()
		}, FeatureStaticArgType},
		{"arg_null_primitive", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", primArg("v", "null")), noArgs()
		}, FeatureStaticArgType},
		{"arg_optional", func() (promptdescriptor.Function, map[string]any) {
			opt := &bamlparser.TypeExpr{Kind: bamlparser.KindUnion, Nullable: true, Variants: []*bamlparser.TypeExpr{
				{Kind: bamlparser.KindPrimitive, Primitive: "string"},
			}}
			return staticFn("hi", typedArg("v", opt)), noArgs()
		}, FeatureStaticArgType},
		{"arg_list", func() (promptdescriptor.Function, map[string]any) {
			list := &bamlparser.TypeExpr{Kind: bamlparser.KindList, Dims: 1, Elem: &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: "string"}}
			return staticFn("hi", typedArg("v", list)), noArgs()
		}, FeatureStaticArgType},
		{"arg_literal", func() (promptdescriptor.Function, map[string]any) {
			lit := &bamlparser.TypeExpr{Kind: bamlparser.KindLiteral, LiteralKind: "string", LiteralValue: "hi"}
			return staticFn("hi", typedArg("v", lit)), noArgs()
		}, FeatureStaticArgType},
		{"arg_primitive_with_attributes", func() (promptdescriptor.Function, map[string]any) {
			at := &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: "string", Attributes: []*bamlparser.Attribute{{Name: "alias"}}}
			return staticFn("hi", typedArg("v", at)), noArgs()
		}, FeatureStaticArgType},
		{"arg_reserved_name_ctx", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", primArg("ctx", "string")), noArgs()
		}, FeatureStaticArgType},

		// --- FeatureEnumClassValue: named-type arg / attribute access --------
		{"arg_named_type", func() (promptdescriptor.Function, map[string]any) {
			nr := &bamlparser.TypeExpr{Kind: bamlparser.KindNameRef, Name: "Color"}
			return staticFn("hi", typedArg("c", nr)), noArgs()
		}, FeatureEnumClassValue},
		{"expr_enum_global_access", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ Color.RED }}"), noArgs()
		}, FeatureEnumClassValue},
		{"expr_class_field_access", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ obj.field }}"), noArgs()
		}, FeatureEnumClassValue},

		// --- FeatureUnsupportedMediaKind: media args (incl image URL) --------
		{"arg_media_image", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", typedArg("m", &bamlparser.TypeExpr{Kind: bamlparser.KindMedia, Media: "image"})), noArgs()
		}, FeatureUnsupportedMediaKind},
		{"arg_media_audio", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", typedArg("m", &bamlparser.TypeExpr{Kind: bamlparser.KindMedia, Media: "audio"})), noArgs()
		}, FeatureUnsupportedMediaKind},
		{"arg_media_pdf", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", typedArg("m", &bamlparser.TypeExpr{Kind: bamlparser.KindMedia, Media: "pdf"})), noArgs()
		}, FeatureUnsupportedMediaKind},
		{"arg_media_video", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi", typedArg("m", &bamlparser.TypeExpr{Kind: bamlparser.KindMedia, Media: "video"})), noArgs()
		}, FeatureUnsupportedMediaKind},

		// --- FeatureStaticArgValue: value gate (no coercion) -----------------
		{"value_missing", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "int")), noArgs()
		}, FeatureStaticArgValue},
		{"value_extra_key", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("hi"), map[string]any{"extra": int64(1)}
		}, FeatureStaticArgValue},
		{"value_wrong_type_int_as_go_int", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "int")), map[string]any{"v": 5} // Go int, not int64
		}, FeatureStaticArgValue},
		{"value_integral_float_for_int", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "int")), map[string]any{"v": 5.0} // float64 for int
		}, FeatureStaticArgValue},
		{"value_string_for_bool", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "bool")), map[string]any{"v": "true"}
		}, FeatureStaticArgValue},
		{"value_invalid_utf8", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "string")), map[string]any{"v": string([]byte{0xff, 0xfe})}
		}, FeatureStaticArgValue},
		{"value_float_nan", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "float")), map[string]any{"v": math.NaN()}
		}, FeatureStaticArgValue},
		{"value_float_inf", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "float")), map[string]any{"v": math.Inf(1)}
		}, FeatureStaticArgValue},
		{"value_duplicate_declaration", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "int"), primArg("v", "int")), map[string]any{"v": int64(1)}
		}, FeatureStaticArgValue},

		// --- FeatureCallableOutputFmt: callable ctx.output_format ------------
		{"callable_output_format_render_null_as", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx.output_format(render_null_as="null") }}`), noArgs()
		}, FeatureCallableOutputFmt},
		{"callable_output_format_empty", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx.output_format() }}`), noArgs()
		}, FeatureCallableOutputFmt},

		// --- FeatureUnsupportedCtx: other ctx members ------------------------
		{"ctx_client", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx.client }}`), noArgs()
		}, FeatureUnsupportedCtx},
		{"ctx_tags", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx.tags }}`), noArgs()
		}, FeatureUnsupportedCtx},
		{"ctx_unknown_member", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx.something }}`), noArgs()
		}, FeatureUnsupportedCtx},

		// --- FeatureUnknownFilter: any filter, incl replace ------------------
		{"filter_replace", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ v | replace("a","b") }}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnknownFilter},
		{"filter_format", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ x | format(type="yaml") }}`), noArgs()
		}, FeatureUnknownFilter},
		{"filter_regex_match", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ x | regex_match("a.*") }}`), noArgs()
		}, FeatureUnknownFilter},
		{"filter_sum", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ items | sum }}`), noArgs()
		}, FeatureUnknownFilter},

		// --- FeaturePyFormatMethod: .format() / method calls -----------------
		{"py_format_on_literal", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ "{}".format(x) }}`), noArgs()
		}, FeaturePyFormatMethod},
		{"method_call_on_arg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ v.upper() }}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeaturePyFormatMethod},

		// --- FeatureEnumComparison: comparison / containment -----------------
		{"cmp_enum_eq_string", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ Color.RED == "RED" }}`), noArgs()
		}, FeatureEnumComparison},
		{"cmp_enum_self_eq", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ Color.RED == Color.RED }}`), noArgs()
		}, FeatureEnumComparison},
		{"cmp_membership_in", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ "RED" in [Color.RED] }}`), noArgs()
		}, FeatureEnumComparison},
		{"cmp_ordering", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ a >= b }}`), noArgs()
		}, FeatureEnumComparison},

		// --- FeatureRoleCallShape: exotic role calls -------------------------
		{"role_cache_control_metadata", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system", cache_control={"type":"ephemeral"}) }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"role_allow_dupe", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("user", __baml_allow_dupe_role__=true) }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"role_custom", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("developer") }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"role_dynamic_arg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role(r) }}x`, primArg("r", "string")), map[string]any{"r": "user"}
		}, FeatureRoleCallShape},
		{"role_both_positional_and_kwarg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system", role="user") }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"role_missing", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role() }}x`), noArgs()
		}, FeatureRoleCallShape},

		// --- FeatureChatLayout: ordering / adjacency / emptiness -------------
		{"layout_content_before_role", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`hello{{ _.role("user") }}hi`), noArgs()
		}, FeatureChatLayout},
		{"layout_adjacent_duplicate_role", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("user") }}a{{ _.role("user") }}b`), noArgs()
		}, FeatureChatLayout},
		{"layout_empty_role_message", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}{{ _.role("user") }}hi`), noArgs()
		}, FeatureChatLayout},

		// --- FeatureReservedDelimiter: magic markers in source/value/block --
		{"reserved_delimiter_in_source", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("before " + roleDelim + " after"), noArgs()
		}, FeatureReservedDelimiter},
		{"reserved_delimiter_in_value", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "string")), map[string]any{"v": mediaDelim}
		}, FeatureReservedDelimiter},
		// P1a: the rendered ctx.output_format block is user-controlled (a field
		// description here) and passes the same delimiter fence, so a schema name
		// equal to a magic delimiter declines instead of being split by lower.
		{"reserved_delimiter_in_output_format_block", func() (promptdescriptor.Function, map[string]any) {
			fn := staticFn("{{ ctx.output_format }}")
			fn.Return = returnBundleWithFieldDescription("F", roleDelim)
			return fn, noArgs()
		}, FeatureReservedDelimiter},

		// --- P1b: value-aware chat layout (empty/whitespace content declines) -
		{"chat_empty_string_arg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("user") }}{{ v }}`, primArg("v", "string")), map[string]any{"v": ""}
		}, FeatureChatLayout},
		{"chat_whitespace_only_arg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("user") }}{{ v }}`, primArg("v", "string")), map[string]any{"v": "  \n\t"}
		}, FeatureChatLayout},
		{"chat_empty_middle_message", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ _.role("user") }}{{ b }}`,
					primArg("a", "string"), primArg("b", "string")),
				map[string]any{"a": "", "b": "hi"}
		}, FeatureChatLayout},

		// --- P2: `+` whitespace-control forms are outside the allowlist -------
		{"ws_plus_both_edges", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{+ v +}}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},
		{"ws_plus_leading", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{+ v }}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},
		{"ws_plus_trailing", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ v +}}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},

		// --- P3: callable/bracket ctx.output_format spellings -> callable key -
		{"callable_output_format_tab_before_paren", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ ctx.output_format\t() }}"), noArgs()
		}, FeatureCallableOutputFmt},
		{"callable_output_format_newline_before_paren", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ ctx.output_format\n() }}"), noArgs()
		}, FeatureCallableOutputFmt},
		{"bracket_output_format_call", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx["output_format"]() }}`), noArgs()
		}, FeatureCallableOutputFmt},
		{"bracket_output_format_bare", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ ctx['output_format'] }}`), noArgs()
		}, FeatureCallableOutputFmt},

		// --- Round 2 P1: token-aware whitespace (MiniJinja lexer semantics) ---
		// Split identifiers under ordinary ASCII whitespace: MiniJinja lexes two
		// tokens, never fusing them, so these are NOT the allowlisted forms.
		{"ws_split_ident_ctx_output_format", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ ctx.output _format }}"), noArgs()
		}, FeatureUnsupportedCtx},
		{"ws_split_ident_bare", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ foo bar }}"), noArgs()
		}, FeatureUnrecognizedPrompt},
		// Whitespace-broken operator glue in the structured forms declines
		// (stricter than MiniJinja, which is allowed).
		{"ws_broken_glue_role", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _ .role("system") }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"ws_broken_glue_ctx", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ ctx . output_format }}"), noArgs()
		}, FeatureUnsupportedCtx},
		// Non-lexer whitespace: form-feed (U+000C) and NBSP (U+00A0) are NOT
		// MiniJinja lexical whitespace, so the tag fails to lex and declines
		// (never a raw compile error after a nil SupportsStatic).
		{"nonlexer_ws_formfeed_bare", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{\f v }}", primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_formfeed_in_role", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ _.role(\f\"system\") }}x"), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_nbsp_bare", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ v }}", primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},
		{"nonlexer_ws_nbsp_split_ctx", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ ctx.output _format }}"), noArgs()
		}, FeatureUnrecognizedPrompt},

		// --- Round 2 P2: `+` whitespace control inside comments --------------
		{"comment_plus_control", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("a\n{# c +#}\nb"), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"comment_plus_leading", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("a\n{#+ c #}\nb"), noArgs()
		}, FeatureUnrecognizedPrompt},

		// --- Round 3 P1: escapes in role string literals ---------------------
		// A well-formed escape MiniJinja would DECODE to a non-standard role
		// (e.g. \t -> tab): declines FeatureRoleCallShape (never rendered as the
		// escape-free standard role). The prompt sources use backslash escapes
		// literally (raw strings), matching the .baml source bytes.
		{"role_escape_decodes_to_custom", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("sys\tem") }}x`), noArgs()
		}, FeatureRoleCallShape},
		{"role_escape_backslash", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("user\\") }}x`), noArgs()
		}, FeatureRoleCallShape},
		// A malformed/unvalidated escape (\u...) MiniJinja REJECTS: the tag fails
		// to lex, so it declines FeatureUnrecognizedPrompt with a nil prompt
		// instead of a raw "invalid unicode escape" compile error.
		{"role_escape_malformed_unicode", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("\user") }}x`), noArgs()
		}, FeatureUnrecognizedPrompt},

		// --- Round 3 P2: capitalized MiniJinja literals are not bare args -----
		// A declared string arg named True must NOT be interpolated: MiniJinja
		// renders the literal `true`, not the bound value, so it declines.
		{"bare_capital_true_arg", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ True }}", primArg("True", "string")), map[string]any{"True": "bound"}
		}, FeatureUnrecognizedPrompt},
		{"bare_capital_false", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ False }}"), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"bare_capital_none", func() (promptdescriptor.Function, map[string]any) {
			return staticFn("{{ None }}"), noArgs()
		}, FeatureUnrecognizedPrompt},

		// --- Round 4 P1: reserved markers synthesized across boundaries ------
		// No single input piece holds the full delimiter; the COMPOSED rendered
		// text does (comment removal / interpolation / whitespace-control joins /
		// output-format / multiple args). Each must decline before lower.
		{"composed_interp_split", func() (promptdescriptor.Function, map[string]any) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x"+pre+"{{ v }}", primArg("v", "string")), map[string]any{"v": last}
		}, FeatureReservedDelimiter},
		{"composed_comment_split", func() (promptdescriptor.Function, map[string]any) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x" + pre + "{# c #}" + last), noArgs()
		}, FeatureReservedDelimiter},
		{"composed_two_arg_split", func() (promptdescriptor.Function, map[string]any) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("{{ a }}{{ b }}", primArg("a", "string"), primArg("b", "string")),
				map[string]any{"a": pre, "b": last}
		}, FeatureReservedDelimiter},
		{"composed_media_split", func() (promptdescriptor.Function, map[string]any) {
			pre, last := mediaDelim[:len(mediaDelim)-1], mediaDelim[len(mediaDelim)-1:]
			return staticFn("x"+pre+"{{ v }}", primArg("v", "string")), map[string]any{"v": last}
		}, FeatureReservedDelimiter},
		{"composed_ws_control_join", func() (promptdescriptor.Function, map[string]any) {
			// `{{-` removes the separating space, joining the fragments.
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("x"+pre+" {{- v }}", primArg("v", "string")), map[string]any{"v": last}
		}, FeatureReservedDelimiter},

		// --- Round 5 P1: byte-faithful fence catches dedent + `-}}` synthesis --
		// BAML dedentTrim strips common leading indentation whose whitespace set
		// includes form-feed and NBSP; the trailing `-}}` then eats the newline,
		// joining the halves into a full delimiter in the ACTUAL rendered bytes.
		{"composed_ff_indent_join", func() (promptdescriptor.Function, map[string]any) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn("\fx"+pre+"{{ v -}}\n\f"+last, primArg("v", "string")), map[string]any{"v": ""}
		}, FeatureReservedDelimiter},
		{"composed_nbsp_indent_join", func() (promptdescriptor.Function, map[string]any) {
			pre, last := roleDelim[:len(roleDelim)-1], roleDelim[len(roleDelim)-1:]
			return staticFn(" x"+pre+"{{ v -}}\n "+last, primArg("v", "string")), map[string]any{"v": ""}
		}, FeatureReservedDelimiter},
		{"composed_ff_indent_join_media", func() (promptdescriptor.Function, map[string]any) {
			pre, last := mediaDelim[:len(mediaDelim)-1], mediaDelim[len(mediaDelim)-1:]
			return staticFn("\fx"+pre+"{{ v -}}\n\f"+last, primArg("v", "string")), map[string]any{"v": ""}
		}, FeatureReservedDelimiter},

		// --- Round 6 P1: standalone media markers (no media delimiter) --------
		// lower.parseBody recognizes a mediaMarkerPrefix..mediaMarkerSuffix body
		// segment as a MediaPart WITHOUT needing the media delimiter, so a chat
		// body composing those affixes (literal or from args, with a malformed or
		// a valid media-JSON body) must decline — never raw-error or synthesize a
		// MediaPart. mediaJSON is a well-formed media body that would otherwise
		// lower to a real image part.
		{"media_marker_literal_malformed", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}` + mediaMarkerPrefix + "V" + mediaMarkerSuffix), noArgs()
		}, FeatureReservedDelimiter},
		{"media_marker_literal_valid_json", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}` + mediaMarkerPrefix + mediaJSON + mediaMarkerSuffix), noArgs()
		}, FeatureReservedDelimiter},
		{"media_marker_arg_malformed", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ v }}{{ b }}`,
					primArg("a", "string"), primArg("v", "string"), primArg("b", "string")),
				map[string]any{"a": mediaMarkerPrefix, "v": "V", "b": mediaMarkerSuffix}
		}, FeatureReservedDelimiter},
		{"media_marker_arg_valid_json", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ _.role("system") }}{{ a }}{{ v }}{{ b }}`,
					primArg("a", "string"), primArg("v", "string"), primArg("b", "string")),
				map[string]any{"a": mediaMarkerPrefix, "v": mediaJSON, "b": mediaMarkerSuffix}
		}, FeatureReservedDelimiter},

		// --- FeatureUnrecognizedPrompt: catch-all ----------------------------
		{"stmt_if", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{% if x %}a{% endif %}`), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"stmt_for", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{% for m in messages %}{{ m }}{% endfor %}`), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"stmt_set", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{% set x = 1 %}{{ x }}`), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"expr_unknown_global", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ mystery }}`), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"expr_function_call", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ foo(1) }}`), noArgs()
		}, FeatureUnrecognizedPrompt},
		{"expr_subscript", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ v[0] }}`, primArg("v", "string")), map[string]any{"v": "x"}
		}, FeatureUnrecognizedPrompt},
		{"expr_unterminated_tag", func() (promptdescriptor.Function, map[string]any) {
			return staticFn(`{{ v `), noArgs()
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
		args map[string]any
	}{
		{"arg_interpolation", staticFn("{{ v }}", primArg("v", "string")), map[string]any{"v": "x"}},
		{"bare_output_format", staticFn("{{ ctx.output_format }}"), noArgs()},
		{"role_positional", staticFn(`{{ _.role("system") }}hi`), noArgs()},
		{"role_kwarg", staticFn(`{{ _.role(role="user") }}hi`), noArgs()},
		{"chat_positional", staticFn(`{{ _.chat("assistant") }}hi`), noArgs()},
		{"chat_kwarg", staticFn(`{{ _.chat(role="user") }}hi`), noArgs()},
		{"raw_text_and_comment", staticFn("plain text {# c #} more"), noArgs()},
		// The scoped hyphen whitespace-control variants stay accepted (only the
		// `+` variants, tested as declines, are outside the allowlist).
		{"hyphen_ws_control_both_edges", staticFn("a\n{{- v -}}\nb", primArg("v", "string")), map[string]any{"v": "x"}},
		{"hyphen_ws_control_leading", staticFn("a\n{{- v }}", primArg("v", "string")), map[string]any{"v": "x"}},
		// Ordinary MiniJinja whitespace between tokens is insignificant, so the
		// canonical forms are accepted with extra spaces around the whole tag and
		// around the call parens/args (but NOT breaking the operator glue).
		{"extra_spaces_around_arg", staticFn("{{   v   }}", primArg("v", "string")), map[string]any{"v": "x"}},
		{"role_space_before_paren", staticFn(`{{ _.role ("system") }}hi`), noArgs()},
		{"role_spaces_in_parens", staticFn(`{{ _.role( "user" ) }}hi`), noArgs()},
		{"comment_hyphen_control", staticFn("a\n{#- c -#}\nb"), noArgs()},
		// The composed reserved-delimiter fence is precise, not over-broad: a
		// non-string arg (rendered non-empty, marker-free) genuinely separates two
		// marker halves, and role-marker boundaries separate per-message content,
		// so neither synthesizes a delimiter — both still render.
		{"composed_num_separator", staticFn("{{ a }}{{ n }}{{ b }}",
			primArg("a", "string"), primArg("n", "int"), primArg("b", "string")),
			map[string]any{"a": rolePre, "n": int64(5), "b": roleLast}},
		{"composed_cross_role_halves", staticFn(`{{ _.role("system") }}` + rolePre + `{{ _.role("user") }}` + roleLast), noArgs()},
		// Round 5 P2: ordinary LITERAL whitespace is preserved by dedent/render/
		// lower (it is not whitespace-control), so a space/tab/NBSP or a
		// space-bearing value between two delimiter halves is a genuine separator —
		// no delimiter forms, and the byte-faithful fence must NOT falsely decline.
		{"composed_literal_space_separator", staticFn("x"+rolePre+" {{ v }}", primArg("v", "string")), map[string]any{"v": roleLast}},
		{"composed_value_leading_space", staticFn("x"+rolePre+"{{ v }}", primArg("v", "string")), map[string]any{"v": " " + roleLast}},
		{"composed_literal_tab_separator", staticFn("x"+rolePre+"\t{{ v }}", primArg("v", "string")), map[string]any{"v": roleLast}},
		{"composed_nbsp_literal_separator", staticFn("x"+rolePre+"{{ a }}"+" "+"{{ b }}", primArg("a", "string"), primArg("b", "string")), map[string]any{"a": "", "b": roleLast}},
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
