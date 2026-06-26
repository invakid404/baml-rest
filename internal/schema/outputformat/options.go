// Package outputformat is a native Go port of BAML's `ctx.output_format`
// renderer. It consumes the merged [schema.Bundle] (baml-rest's compact
// mirror of BAML's OutputFormatContent) and produces the byte-identical
// output-format text the BAML runtime would render, without invoking the
// BAML runtime in production.
//
// The port targets BAML engine commit
// e803afa246837a6bd38a15beb4aa403f7e3970e5,
// engine/baml-lib/jinja-runtime/src/output_format/types.rs. Every rendering
// decision in this package corresponds to a line in that file; the goal is
// byte-exact parity, including load-bearing whitespace, so prompt-cache keys
// stay stable. See the differential and golden tests for the parity gate.
//
// First production cut wires only zero-value [Options] (BAML
// RenderOptions::default), because baml-rest's dynamic template does not call
// ctx.output_format(...) with kwargs. The full option surface is implemented
// anyway so the renderer can be differentially tested against BAML's callable
// object later.
package outputformat

// settingMode is the discriminant for BAML's RenderSetting<T> tri-state:
// Auto (derive a default), Always (use the carried value), or Never
// (suppress). The zero value is Auto, so a zero-value [Options] matches
// BAML's RenderOptions::default.
type settingMode uint8

const (
	settingAuto settingMode = iota
	settingAlways
	settingNever
)

// StringSetting mirrors BAML's RenderSetting<String> (Auto/Always/Never).
// The zero value is Auto. Use [AlwaysString] / [NeverString] to build the
// other two states. It is used for the prefix, enum-value prefix, and
// hoisted-class prefix knobs, each of which maps an absent kwarg to Auto, a
// string kwarg to Always, and a null kwarg to Never.
type StringSetting struct {
	mode settingMode
	val  string
}

// AlwaysString returns a StringSetting that always renders exactly s,
// matching a BAML kwarg set to a concrete string.
func AlwaysString(s string) StringSetting { return StringSetting{mode: settingAlways, val: s} }

// NeverString returns a StringSetting that suppresses the value, matching a
// BAML kwarg explicitly set to null.
func NeverString() StringSetting { return StringSetting{mode: settingNever} }

// alwaysNonEmpty reports whether the setting is Always with a non-empty
// value. Several BAML sites (hoisted_class_prefix in particular) only honour
// an Always value when it is non-empty, treating Always("") like Auto.
func (s StringSetting) alwaysNonEmpty() (string, bool) {
	if s.mode == settingAlways && s.val != "" {
		return s.val, true
	}
	return "", false
}

// BoolSetting mirrors BAML's RenderSetting<bool> for always_hoist_enums.
// Only Auto and Always(bool) are reachable from BAML (a missing kwarg is
// Auto, a present bool is Always); Never is unused. The zero value is Auto.
type BoolSetting struct {
	mode settingMode
	val  bool
}

// AlwaysBool returns a BoolSetting carrying b. Only AlwaysBool(true) changes
// behaviour: it forces every enum to be hoisted. AlwaysBool(false) and the
// Auto zero value both leave BAML's automatic hoist conditions untouched.
func AlwaysBool(b bool) BoolSetting { return BoolSetting{mode: settingAlways, val: b} }

// isTrue reports whether the setting forces the on-behaviour, matching the
// Rust `matches!(opt, RenderSetting::Always(true))` guard.
func (s BoolSetting) isTrue() bool { return s.mode == settingAlways && s.val }

// HoistMode selects how [HoistClasses] behaves. The zero value is
// HoistAuto, matching BAML HoistClasses::Auto.
type HoistMode uint8

const (
	// HoistAuto hoists only recursive classes (BAML default behaviour).
	HoistAuto HoistMode = iota
	// HoistAll hoists every class in bundle order.
	HoistAll
	// HoistSubset hoists the named subset (in request order); a name that
	// resolves to no class under either streaming mode is a render error.
	HoistSubset
)

// HoistClasses mirrors BAML's HoistClasses enum. Recursive classes are
// always hoisted regardless of this setting; this only controls what else is
// hoisted. The zero value (HoistAuto, nil Subset) hoists nothing extra.
type HoistClasses struct {
	Mode   HoistMode
	Subset []string
}

// MapStyle selects how a map type renders. The zero value is MapStyleAngle
// (`map<K, V>`), matching BAML MapStyle::TypeParameters.
type MapStyle uint8

const (
	// MapStyleAngle renders `map<K, V>` (BAML default).
	MapStyleAngle MapStyle = iota
	// MapStyleObject renders `{K: V}`.
	MapStyleObject
)

// Options mirrors BAML's RenderOptions. The zero value is exactly
// RenderOptions::default: auto prefix, " or " splitter, auto enum-value
// prefix ("- "), auto hoisted-class prefix, auto class hoisting (recursive
// only), auto enum hoisting, angle map style, and unquoted class fields.
//
// OrSplitter is a plain string for fidelity with BAML's RenderOptions (where
// it is a String, not an Option): the empty zero value is normalised to the
// default " or " so a zero-value Options matches BAML's default. An empty
// splitter therefore cannot be expressed, an accepted trade-off for the
// defaults-only first production cut.
type Options struct {
	Prefix             StringSetting
	OrSplitter         string
	EnumValuePrefix    StringSetting
	HoistedClassPrefix StringSetting
	HoistClasses       HoistClasses
	AlwaysHoistEnums   BoolSetting
	MapStyle           MapStyle
	QuoteClassFields   bool
}

// defaultOrSplitter is BAML RenderOptions::DEFAULT_OR_SPLITTER.
const defaultOrSplitter = " or "

// defaultTypePrefixWord is BAML
// RenderOptions::DEFAULT_TYPE_PREFIX_IN_RENDER_MESSAGE — the noun used in the
// auto "Answer in JSON using this <word>:" prefix when no hoisted-class
// prefix is configured.
const defaultTypePrefixWord = "schema"

// orSplitter returns the effective union/enum alternative separator,
// substituting the BAML default for the empty zero value.
func (o *Options) orSplitter() string {
	if o.OrSplitter == "" {
		return defaultOrSplitter
	}
	return o.OrSplitter
}

// enumValuePrefix resolves the per-value bullet prefix: Auto is "- ",
// Always(p) is p exactly (including empty), Never is "".
func (o *Options) enumValuePrefix() string {
	switch o.EnumValuePrefix.mode {
	case settingAlways:
		return o.EnumValuePrefix.val
	case settingNever:
		return ""
	default:
		return "- "
	}
}

// typePrefixWord is the noun spliced into the auto prefix and used nowhere
// else: the configured hoisted-class prefix when it is a non-empty Always,
// otherwise "schema".
func (o *Options) typePrefixWord() string {
	if word, ok := o.HoistedClassPrefix.alwaysNonEmpty(); ok {
		return word
	}
	return defaultTypePrefixWord
}
