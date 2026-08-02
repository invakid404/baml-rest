package nativeprompt

import (
	"fmt"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/value"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// This file is nativeprompt's internal render-context adapter onto the
// internal/bamlprofile facade. It is the SINGLE seam through which the prompt
// renderer reaches a MiniJinja environment and a template render:
//
//	nativeprompt (prompt language, admission, markers, lowering)
//	  -> renderContext (this file)
//	    -> bamlprofile.New   = BAML v0.223 get_env() + the prompt globals
//	      -> github.com/invakid404/minijinja-go/v2 (the BAML-exact fork)
//
// Before Slice 7.1a nativeprompt built its own environment (buildEnv) over the
// PRE-fork external engine github.com/mitsuhiko/minijinja/minijinja-go/v2 and
// re-implemented BAML's `_` role helper and `ctx` globals locally. Both local
// duplicates are gone: the globals now come from the profile, which is the
// package whose behavior is proven against stock BAML v0.223 by
// internal/bamlprofile/profileoracle.
//
// # Ownership boundary
//
// The profile owns BAML's environment configuration (whitespace, autoescape,
// the none -> "null" formatter, the filter/test/function registry including
// get_env's regex_match and sum, the pycompat unknown-method callback), the
// `_` / `ctx` prompt globals, the per-enum namespace globals, and the
// enum/class/list host value model.
//
// nativeprompt keeps everything the generic leaf must not know about: which
// prompt sources it recognizes ([Supports] / [SupportsStatic]), the exact
// template text, the pre-rendered ctx.output_format string, the media marker
// object below, the post-render marker lowering ([lower]), and every serving
// decision. bamlprofile must never import nativeprompt.
//
// # Host values on the Slice 7.1a admitted surface
//
// bamlprofile exposes EnumMember / ClassValue / ListValue for BAML's typed host
// values. The surface admitted in 7.1a reaches NONE of them, and that is a
// property of the admission gates, not an omission here:
//
//   - static ([SupportsStatic]) binds attribute-free primitives only; a named
//     class/enum argument declines FeatureEnumClassValue and a comparison
//     declines FeatureEnumComparison, both unchanged in this slice;
//   - the dynamic message tree is bound as fork-native maps/slices (see
//     input.go). It carries [mediaObject], nativeprompt's own media marker, and
//     bamlprofile deliberately has NO media host value: ClassValue/ListValue
//     REJECT a foreign object rather than render it unlike BAML (#602). Lowering
//     the message tree into host classes is therefore blocked on the media value
//     model, not merely unimplemented.
//
// Config is forwarded verbatim so the enum-namespace path is a real, typed input
// of this adapter rather than a future edit: renderContextFrom takes whatever
// bamlprofile.Config the caller resolved. Production resolves an empty enum set
// today because no admitted template references an enum global; the #597 fence
// test drives the same adapter with a populated one. Wiring resolved enum/class
// definitions into production is Slice 7.1b's descriptor work.

// renderContext is one prepared MiniJinja render: the profile environment plus
// the named values bound into it. It is single-use — bamlprofile.New builds a
// fresh environment per render because `ctx` carries a per-render output_format.
type renderContext struct {
	env  *minijinja.Environment
	vars map[string]any
}

// newRenderContext builds a render context whose only environment input is the
// pre-rendered ctx.output_format block. It is the production constructor: every
// admitted 7.1a template needs `_`, `ctx`, and no enum namespace.
func newRenderContext(outputFormat string) (*renderContext, error) {
	return renderContextFrom(bamlprofile.Config{OutputFormat: outputFormat})
}

// renderContextFrom builds a render context from a resolved profile config. It
// exists so the enum-namespace input is exercised through this adapter rather
// than only through bamlprofile's own tests; production reaches it via
// newRenderContext.
//
// A malformed config (a duplicate/empty enum name, a name colliding with a
// get_env global) is a resolved-metadata contract violation: bamlprofile fails
// loud and so does this, rather than rendering through a half-built environment.
func renderContextFrom(cfg bamlprofile.Config) (*renderContext, error) {
	env, err := bamlprofile.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("nativeprompt: build profile environment: %w", err)
	}
	return &renderContext{env: env, vars: make(map[string]any)}, nil
}

// bind sets one top-level template variable. Values are constructed with the
// explicit fork value.From* constructors (or bamlprofile's host constructors) by
// the callers — never value.FromAny, reflection, or JSON coercion — so what the
// template sees is always a deliberate choice.
func (rc *renderContext) bind(name string, v value.Value) {
	rc.vars[name] = v
}

// renderToString compiles src in the profile environment and renders it with the
// bound variables, returning the raw rendered string (the exact bytes [lower]
// consumes). what names the template in error messages ("dynamic"/"static").
func (rc *renderContext) renderToString(src, what string) (string, error) {
	tmpl, err := rc.env.TemplateFromString(src)
	if err != nil {
		return "", fmt.Errorf("nativeprompt: compile %s template: %w", what, err)
	}
	rendered, err := tmpl.Render(rc.vars)
	if err != nil {
		return "", fmt.Errorf("nativeprompt: render %s template: %w", what, err)
	}
	return rendered, nil
}

// renderPrompt renders src and lowers the result into the structured
// [RenderedPrompt]. It is the single MiniJinja boundary the dynamic [Render]
// path uses.
func (rc *renderContext) renderPrompt(src, what string) (*RenderedPrompt, error) {
	rendered, err := rc.renderToString(src, what)
	if err != nil {
		return nil, err
	}
	return lower(rendered)
}
