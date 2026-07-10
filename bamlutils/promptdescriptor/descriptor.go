// Package promptdescriptor is the public, passive descriptor model for a
// BAML function's static PROMPT, analogous in role to
// bamlutils/schemadescriptor (the static OUTPUT-schema mirror). It is the
// stable data shape the de-BAML native front-end retains at build time so a
// later phase can render a function's prompt without reparsing .baml or
// querying a BAML runtime.
//
// Passive by design. This package defines DATA ONLY: it does not serialize,
// render, dedent, inject macros, evaluate Jinja, or resolve anything. It
// imports nothing from the root module's internal/* tree, nothing from BAML,
// and no renderer — only bamlutils' own passive AST
// (bamlutils/bamlparser, for the retained argument TypeExpr) and the static
// output-schema mirror (bamlutils/schemadescriptor, for the return Bundle).
// Keeping it dependency-light lets a generated introspection package name
// these types without pulling in the builder or any runtime.
//
// The builder that populates these descriptors lives on the internal side
// (internal/nativeschema.BuildPromptDescriptors); like schemadescriptor, this
// package stays a passive contract with no knowledge of how it is built or
// consumed. In Phase 1 the descriptor is a build-only sidecar in
// cmd/introspect: it is NOT emitted into the generated introspected package
// and reaches no request path. See the de-BAML Phase 1 scope (slice 2).
package promptdescriptor

import (
	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// Version is the current prompt-descriptor schema version. A built descriptor
// stamps [Function.Version] with the version it was produced for so a future
// consumer can reject an incompatible producer, mirroring
// schemadescriptor.Version.
const Version = 1

// Argument is one named argument of a function signature or a template-string
// macro. Type is the argument's retained parsed type expression; it is nil for
// a bare argument written without a `: type` annotation (BAML permits this in a
// named-argument list), exactly like [bamlparser.Param]. The type is retained
// (not lowered) so a later value-conversion phase can bind incoming static
// arguments to the BAML-value/Jinja model.
type Argument struct {
	Name string
	Type *bamlparser.TypeExpr
}

// TemplateString is a retained BAML `template_string` (macro) declaration.
// BAML injects every project template string as an ordered Jinja macro ahead of
// the selected function's prompt, so every eligible function descriptor carries
// the whole project macro set (see [Function.Macros]).
//
//   - Name is the macro name.
//   - Args are its named arguments, in source order.
//   - Body is the raw template body with only the raw-string delimiters removed
//     (never dedented, trimmed, unescaped, or otherwise normalized — render-time
//     dedent/trim is a later phase's concern).
//   - SourcePath is the .baml file the declaration was parsed from. It makes a
//     later cross-file macro-ordering oracle diagnosable; it is not otherwise
//     load-bearing.
type TemplateString struct {
	Name       string
	Args       []Argument
	Body       string
	SourcePath string
}

// Function is the retained static prompt descriptor for one eligible BAML LLM
// function. It is fail-closed: the builder emits a Function OR records a decline
// reason for the method, never both.
//
//   - Version is the descriptor schema version; see [Version].
//   - Method is the BAML function name.
//   - Prompt is the function's final prompt raw-string body, delimiters removed
//     and otherwise byte-for-byte (no dedent/trim — a later phase owns that).
//   - Args are the function's parameters, in signature order.
//   - Client is the function's resolved default client name (a named client or a
//     shorthand spec), matching cmd/introspect's client conventions.
//   - Provider is the provider Client resolves to after shorthand enrichment.
//   - Return is the EXACT ordered [schemadescriptor.Bundle] the native static
//     schema builder produced for this method — its class/enum order is BAML
//     output-format order and is preserved verbatim.
//   - Macros are every retained project template string, ordered by parsed
//     source-file order and then by within-file declaration order (NEVER lexical
//     or map order).
type Function struct {
	Version  int
	Method   string
	Prompt   string
	Args     []Argument
	Client   string
	Provider string
	Return   schemadescriptor.Bundle
	Macros   []TemplateString
}
