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
// # Host values on the admitted surface (Slice 7.1b)
//
// bamlprofile exposes EnumMember / ClassValue / ListValue for BAML's typed host
// values. Which of them PRODUCTION reaches is a property of the admission gates,
// not of this adapter:
//
//   - static ([SupportsStatic]) binds the V3 value graph: scalars, enum members,
//     class values and lists, all built by the V3 binder (static_bind.go) from
//     the descriptor's source-resolved universe and the generated projector's
//     vector. RENDERING a bound class directly is still declined — stock BAML
//     v0.223's Go client encodes a class through a Go map, so its printed field
//     order is not reproducible (see static_typegate.go) — but the class is
//     bound, so a function that merely declares one is not poisoned;
//   - the dynamic message tree is bound as fork-native maps/slices (see
//     input.go). It carries [mediaObject], nativeprompt's own media marker, and
//     bamlprofile deliberately has NO media host value: ClassValue/ListValue
//     REJECT a foreign object rather than render it unlike BAML (#602). Lowering
//     the message tree into host classes is therefore blocked on the media value
//     model, not merely unimplemented.
//
// The enum-namespace set is a real, typed input of this adapter: the STATIC
// constructor forwards the descriptor's WHOLE project enum set (reproducing
// v0.223's render_prompt, which installs one namespace global per IR enum), and
// the dynamic lane passes none because its one admitted template references no
// enum and there is no descriptor to resolve a set from.

// renderContext is one prepared MiniJinja render: the profile environment plus
// the named values bound into it. It is single-use — bamlprofile.New builds a
// fresh environment per render because `ctx` carries a per-render output_format.
type renderContext struct {
	env  *minijinja.Environment
	vars map[string]any
}

// newRenderContext builds a render context from the pre-rendered
// ctx.output_format block and the descriptor's RESOLVED PROJECT ENUM SET. It is
// the production static constructor.
//
// enums is the WHOLE project enum set the V3 descriptor carries, forwarded
// verbatim, because that is stock v0.223's model: render_prompt walks the IR
// enums and installs one namespace global per enum, so `Color.RED` resolves in a
// function that never takes a Color argument. Passing a subset would build a
// render context BAML never has. A caller with no enums (the dynamic lane)
// passes nil, which installs none.
func newRenderContext(outputFormat string, enums []bamlprofile.EnumDef) (*renderContext, error) {
	return renderContextFrom(bamlprofile.Config{OutputFormat: outputFormat, Enums: enums})
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
