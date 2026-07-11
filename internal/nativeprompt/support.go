package nativeprompt

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ErrUnsupported is the sentinel wrapped by every decline. A caller uses
// errors.Is(err, ErrUnsupported) to route the request to the authoritative BAML
// path instead of treating the decline as a hard failure.
var ErrUnsupported = errors.New("nativeprompt: unsupported prompt shape")

// Decline is the concrete decline error. Feature names the unsupported feature
// class; Detail is a specific human-readable reason. It unwraps to
// [ErrUnsupported].
type Decline struct {
	Feature string
	Detail  string
}

func (d *Decline) Error() string {
	return fmt.Sprintf("%s: %s (%s)", ErrUnsupported.Error(), d.Detail, d.Feature)
}

func (d *Decline) Unwrap() error { return ErrUnsupported }

func decline(feature, detail string) *Decline { return &Decline{Feature: feature, Detail: detail} }

// Feature keys for declines. These enumerate the parity boundary of this spike:
// the dynamic template uses none of them, so a prompt (or input) that does is
// out of the proven claim and must fall back to BAML.
const (
	FeatureTemplateString       = "template_string"
	FeatureMacro                = "macro/import/include"
	FeatureCallableOutputFmt    = "callable_ctx_output_format"
	FeatureUnknownFilter        = "unknown_filter"
	FeaturePyFormatMethod       = "python_format_method"
	FeatureUnrecognizedPrompt   = "unrecognized_prompt"
	FeatureUnsupportedMediaKind = "unsupported_media_kind"
	FeatureInvalidMedia         = "invalid_media"
	FeatureNilOutputSchema      = "nil_output_schema"

	// Static feature keys for the test-only narrow static candidate
	// (SupportsStatic / RenderStatic). They name the boundary of the static
	// claim: a static prompt, function envelope, or argument that uses any of
	// them is outside the proven surface and declines through the same
	// ErrUnsupported/Decline contract. See static_support.go / static_render.go.
	FeatureStaticDescriptor  = "static_descriptor"
	FeatureStaticArgType     = "static_argument_type"
	FeatureStaticArgValue    = "static_argument_value"
	FeatureRoleCallShape     = "role_call_shape"
	FeatureChatLayout        = "chat_layout"
	FeatureEnumClassValue    = "enum_or_class_value"
	FeatureEnumComparison    = "enum_or_class_comparison"
	FeatureUnsupportedCtx    = "unsupported_ctx"
	FeatureReservedDelimiter = "reserved_delimiter"
)

// outputFormatPlaceholder is the literal token the dynamic template's content
// branch substitutes via `replace("{output_format}", ctx.output_format)`.
const outputFormatPlaceholder = "{output_format}"

// allowedFilters is the set of Jinja filters the dynamic renderer reproduces.
// Everything else — including BAML's custom format / regex_match / sum — is a
// decline until it is proven against the oracle.
var allowedFilters = map[string]bool{
	"replace": true,
}

var (
	// filterPipeRe matches a filter application `| name`, capturing the filter
	// name. It is a conservative lexical scan, not a full parse.
	filterPipeRe = regexp.MustCompile(`\|\s*([A-Za-z_][A-Za-z0-9_]*)`)
	// callableOutputFmtRe matches the callable form ctx.output_format(...),
	// e.g. ctx.output_format(render_null_as="null"). The bare attribute
	// ctx.output_format (no open paren) is supported and does not match.
	callableOutputFmtRe = regexp.MustCompile(`ctx\.output_format\s*\(`)
	// blockKeywordRe matches macro/import/include/extends/from/call block tags.
	blockKeywordRe = regexp.MustCompile(`\{%-?\s*(macro|import|include|extends|from|call)\b`)
)

// Supports is the fail-closed native-front-end predicate. It returns nil when
// the native renderer is proven to reproduce (promptSrc, msgs), and a *Decline
// (unwrapping to [ErrUnsupported]) otherwise.
//
// It applies, in order: a feature scan of the template source (specific decline
// reasons for template_string, macros, the callable ctx.output_format form,
// unknown/custom filters, and the python .format() method); an exact-match
// catch-all so that only the proven Baml_Rest_Dynamic prompt is ever claimed
// (this fail-closes on enum globals, class-alias objects, and any other shape a
// lexical scan cannot enumerate); and an input scan that declines media kinds
// beyond the proven corpus (only image is proven).
func Supports(promptSrc string, msgs []Message) error {
	if reasons := UnsupportedTemplateFeatures(promptSrc); len(reasons) > 0 {
		return decline(reasons[0].Feature, reasons[0].Detail)
	}

	if promptSrc != rawDynamicPrompt {
		return decline(FeatureUnrecognizedPrompt,
			"only the generated Baml_Rest_Dynamic prompt is proven byte-parity in this spike")
	}

	if d := unsupportedInput(msgs); d != nil {
		return d
	}
	return nil
}

// UnsupportedTemplateFeatures returns every unsupported feature detected in a
// prompt template source. An empty result means the lexical feature scan found
// nothing out of scope (it does not by itself imply the template is the proven
// dynamic prompt — Supports still requires an exact match).
func UnsupportedTemplateFeatures(src string) []Decline {
	var out []Decline

	if strings.Contains(src, "template_string") {
		out = append(out, *decline(FeatureTemplateString,
			"template_string macros are not retained/injected by this renderer"))
	}
	if m := blockKeywordRe.FindStringSubmatch(src); m != nil {
		out = append(out, *decline(FeatureMacro,
			fmt.Sprintf("{%% %s %%} blocks are not supported", m[1])))
	}
	if callableOutputFmtRe.MatchString(src) {
		out = append(out, *decline(FeatureCallableOutputFmt,
			"callable ctx.output_format(...) options (e.g. render_null_as) are not supported; only bare ctx.output_format"))
	}
	if strings.Contains(src, ".format(") {
		out = append(out, *decline(FeaturePyFormatMethod,
			"the python-compat string .format() method is not supported"))
	}
	for _, name := range filterNames(src) {
		if !allowedFilters[name] {
			out = append(out, *decline(FeatureUnknownFilter,
				fmt.Sprintf("filter %q is not reproduced (only replace is)", name)))
		}
	}
	return out
}

// filterNames returns the sorted, de-duplicated set of filter names applied in
// the source.
func filterNames(src string) []string {
	seen := map[string]bool{}
	for _, m := range filterPipeRe.FindAllStringSubmatch(src, -1) {
		seen[m[1]] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// unsupportedInput declines input messages that use an unproven feature. Each
// present media part is first checked against the URL/Base64 one-of invariant
// (matching bamlutils.MediaInput.Validate, which BAML enforces) and then against
// the proven-kind gate: only image media is proven against the oracle;
// audio/pdf/video render through the same mechanism but are not part of the
// proven corpus, so they fail closed.
func unsupportedInput(msgs []Message) *Decline {
	for mi := range msgs {
		for pi := range msgs[mi].Parts {
			if m := partMedia(&msgs[mi].Parts[pi]); m != nil {
				if d := declineMediaPart(m); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

// partMedia returns the single media value set on a content part, or nil.
func partMedia(p *ContentPart) *Media {
	switch {
	case p.Image != nil:
		return p.Image
	case p.Audio != nil:
		return p.Audio
	case p.PDF != nil:
		return p.PDF
	case p.Video != nil:
		return p.Video
	}
	return nil
}

// declineMediaPart returns a decline when a media value is invalid (fails the
// one-of invariant) or of an unproven kind, else nil.
func declineMediaPart(m *Media) *Decline {
	if err := validateMediaOneOf(m); err != nil {
		return decline(FeatureInvalidMedia, err.Error())
	}
	if m.Kind != MediaImage {
		return declineMedia(m.Kind)
	}
	return nil
}

// validateMediaOneOf enforces the same one-of contract as
// bamlutils.MediaInput.Validate: exactly one of URL / Base64 must be present.
// The native Media uses string fields, so "present" means non-empty; rendering
// a both/neither shape would let native claim parity for media BAML rejects.
func validateMediaOneOf(m *Media) error {
	hasURL := m.URL != ""
	hasBase64 := m.Base64 != ""
	switch {
	case hasURL && hasBase64:
		return fmt.Errorf("media kind %q must provide either url or base64, not both", m.Kind)
	case !hasURL && !hasBase64:
		return fmt.Errorf("media kind %q must provide either url or base64", m.Kind)
	}
	return nil
}

func declineMedia(kind MediaKind) *Decline {
	return decline(FeatureUnsupportedMediaKind,
		fmt.Sprintf("media kind %q is beyond the proven corpus (only image is proven)", kind))
}

// inputReachesOutputFormat reports whether rendering these messages can reach
// ctx.output_format under the dynamic template's branch selection: a message
// with parts reaches it only via an output_format part; a message without parts
// reaches it only when its content carries the {output_format} placeholder.
func inputReachesOutputFormat(msgs []Message) bool {
	for mi := range msgs {
		m := &msgs[mi]
		if len(m.Parts) > 0 {
			for pi := range m.Parts {
				if m.Parts[pi].OutputFormat {
					return true
				}
			}
			continue // parts branch wins; content is never rendered
		}
		if m.Content != nil && strings.Contains(*m.Content, outputFormatPlaceholder) {
			return true
		}
	}
	return false
}
