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
// consumed.
//
// As of de-BAML Phase 8A (#602) these descriptors — including [Function.Prompt]
// and [Function.ClientConfig] — ARE emitted into the generated introspected
// packages (as introspected.StaticPromptDescriptors, a map of fresh-per-call
// typed factories) as runtime REPRESENTATION ONLY: routed nowhere, with no
// consumer, admission, or socket in 8A. SECURITY: emission puts each function's
// raw prompt bytes and any INLINE client-option literals (including credentials
// declared as literals rather than env.X references) into generated source AND
// the compiled worker binary, so both are sensitive artifacts — never log,
// metric-label, %v-format, or error-wrap a descriptor or its raw fields, and
// prefer env.X references so secret material stays out of generated artifacts.
// Only the separate standalone static-schema sidecar stays un-emitted; a
// descriptor's [Function.Return] already carries the exact schemadescriptor.Bundle.
package promptdescriptor

import (
	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// Version is the current prompt-descriptor schema version. A built descriptor
// stamps [Function.Version] with the version it was produced for so a future
// consumer can reject an incompatible producer, mirroring
// schemadescriptor.Version.
//
// Version history:
//
//   - 1: Method/Prompt/Args/Client/Provider/Return/Macros only.
//   - 2 (de-BAML Phase 4a): adds the passive, versioned [Function.ClientConfig]
//     — the resolved literal model (with provenance), the ordered typed
//     request_body tree, and recognized transport-only vs body-affecting option
//     separation. The bump is deliberate: a Version-1 consumer must NOT silently
//     treat a Version-2 descriptor as if ClientConfig were absent, because a
//     later native body builder relies on ClientConfig to reproduce BAML's exact
//     request bytes. Existing fields keep their meaning, so a producer/consumer
//     that only reads the Version-1 fields is source-compatible; the version
//     fence exists so a stale consumer rejects rather than under-reads.
const Version = 2

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
//   - ClientConfig is the passive, versioned client/options representation for
//     the resolved default [Client] (descriptor Version 2+). It carries the
//     resolved literal model (with provenance), the ordered typed request_body
//     tree, and the recognized transport-only vs body-affecting option split. It
//     is [ClientConfig.Present]==false when the client has no declared
//     `client<llm>` block (a shorthand spec, or an enriched-but-blockless
//     client), in which case only Client/Provider are known. Passive: no serving
//     path consumes it — a later native body phase reads it to reproduce BAML's
//     exact request bytes.
type Function struct {
	Version      int
	Method       string
	Prompt       string
	Args         []Argument
	Client       string
	Provider     string
	Return       schemadescriptor.Bundle
	Macros       []TemplateString
	ClientConfig ClientConfig
}

// ModelProvenance classifies how a client's target model string was resolved
// from its declared `options { model ... }` field. Only [ModelProvenanceLiteral]
// yields a model a native body may put in the top-level JSON `model` field
// without further resolution; every other provenance is deliberately NOT a
// literal the native builder can claim byte-parity for.
type ModelProvenance string

const (
	// ModelProvenanceAbsent means no `model` option was declared.
	ModelProvenanceAbsent ModelProvenance = ""
	// ModelProvenanceLiteral means `model` was a quoted/raw string literal; Value
	// carries the resolved literal (quotes/delimiters stripped, no escape
	// processing — mirroring bamlparser's literal semantics).
	ModelProvenanceLiteral ModelProvenance = "literal"
	// ModelProvenanceEnv means `model env.NAME`; EnvVar carries NAME and the
	// literal is only resolvable at runtime, so it is not a build-time literal.
	ModelProvenanceEnv ModelProvenance = "env"
	// ModelProvenanceDynamic means `model` was a bare identifier / number / other
	// non-literal expression (e.g. a shorthand-derived or computed value); the
	// build cannot resolve a stable literal.
	ModelProvenanceDynamic ModelProvenance = "dynamic"
)

// ClientModel is a client's resolved target model with its provenance. Value is
// populated (and byte-parity-usable) only when Provenance is
// [ModelProvenanceLiteral]; EnvVar names the environment variable when
// Provenance is [ModelProvenanceEnv].
//
// RawString distinguishes the two literal spellings, which BAML treats
// differently: a RAW string literal (`model #"..."#`, RawString==true) is
// preserved VERBATIM, while a REGULAR string literal (`model "..."`,
// RawString==false) has its escape sequences DECODED by BAML before the request
// is built. Value always carries the retained bytes WITHOUT escape processing
// (mirroring the passive parser), so a consumer that needs BAML-exact bytes must
// treat a regular literal bearing an escape as unresolved (see
// nativebody.FeatureModelEscape). RawString is meaningful only for
// [ModelProvenanceLiteral].
type ClientModel struct {
	Value      string
	Provenance ModelProvenance
	EnvVar     string
	RawString  bool
}

// OptionValueKind tags the shape of one [OptionValue] node in the ordered
// request_body / option tree. It preserves BAML's source-level value shape
// (string vs number vs env-ref vs nested object/list) rather than lowering to a
// single Go any, so a later phase can serialize each node exactly as BAML would.
type OptionValueKind string

const (
	// OptionString is a quoted or raw string literal; String holds the payload.
	OptionString OptionValueKind = "string"
	// OptionNumber is a numeric literal kept as its raw lexeme in Number.
	OptionNumber OptionValueKind = "number"
	// OptionBool is a boolean literal (the identifiers true/false); Bool holds it.
	OptionBool OptionValueKind = "bool"
	// OptionIdent is a bare non-boolean identifier (e.g. an enum-like token);
	// String holds the identifier text.
	OptionIdent OptionValueKind = "ident"
	// OptionEnv is an `env.NAME` reference; String holds NAME (no "env." prefix).
	OptionEnv OptionValueKind = "env"
	// OptionList is a `[ ... ]` list; List holds the ordered element values.
	OptionList OptionValueKind = "list"
	// OptionObject is a brace-delimited nested block; Object holds its ordered,
	// last-wins-resolved entries.
	OptionObject OptionValueKind = "object"
)

// OptionValue is one typed value node in a client's ordered option/request_body
// tree. Exactly one shape field is meaningful per [OptionValueKind]. The tree is
// passive data: it is not serialized here, only retained so a later native body
// phase can reproduce BAML's exact bytes.
type OptionValue struct {
	Kind OptionValueKind
	// String carries the payload for OptionString / OptionIdent / OptionEnv.
	String string
	// Number carries the raw numeric lexeme for OptionNumber.
	Number string
	// Bool carries the value for OptionBool.
	Bool bool
	// List carries the ordered elements for OptionList.
	List []OptionValue
	// Object carries the ordered entries for OptionObject.
	Object []RequestBodyEntry
}

// RequestBodyEntry is one ordered key/value in a request_body object (or a
// nested object within it). Order is BAML source order. Duplicate keys are
// resolved last-wins: the surviving entry carries the final declaration's value
// and keeps the position of that LAST declaration, matching BAML's later-wins
// map construction. See BuildClientConfigs for the exact resolution.
type RequestBodyEntry struct {
	Key   string
	Value OptionValue
}

// ClientOption is one recognized non-request_body option retained for a later
// phase: either a transport-only value (base_url / api_key / headers) that does
// not belong in a chat body, or a body-affecting option other than request_body
// (e.g. temperature) that BAML flattens into the JSON body. Which bucket it
// lands in is recorded by [ClientConfig] (TransportOptions vs
// BodyAffectingOptions).
type ClientOption struct {
	Key   string
	Value OptionValue
}

// ClientConfig is the passive, versioned representation of a resolved client's
// body-relevant configuration (descriptor Version 2+). It is intentionally
// separate from the flat Client/Provider strings: those name the client, this
// describes what the client would put on the wire.
//
//   - Present is false when the client has no declared `client<llm>` block
//     (shorthand or enriched-only); then only Name/Provider are meaningful.
//   - Name / Provider mirror the resolved [Function.Client] / [Function.Provider].
//   - Model is the resolved target model with provenance (see [ClientModel]).
//   - RequestBodyPresent is true when the client declares a `request_body`
//     block AT ALL, even an empty `request_body {}`. This is load-bearing: BAML
//     v0.223 emits a `"request_body":{...}` object (empty or not) whenever the
//     block is present, so an explicit empty block is NOT equivalent to an absent
//     one and a native body must decline it. RequestBody may be empty while
//     RequestBodyPresent is true (the `request_body {}` case).
//   - RequestBody is the ordered typed tree of the client's explicit
//     `options { request_body { ... } }` block — the body-affecting object BAML
//     emits after model. Empty when the client declares `request_body {}` or none
//     (distinguish via RequestBodyPresent).
//   - TransportOptions are recognized transport-only options in source order
//     (base_url, api_key, headers); they are NOT chat-body fields and are
//     retained only as future client intent.
//   - BodyAffectingOptions are recognized/unrecognized options OTHER than
//     request_body that BAML flattens into the JSON body (proven for e.g.
//     temperature), in source order. Their presence means a native body builder
//     must decline until each is individually proven.
//
// Duplicate top-level option keys are resolved last-wins within each bucket,
// preserving the surviving key's final source position.
type ClientConfig struct {
	Present              bool
	Name                 string
	Provider             string
	Model                ClientModel
	RequestBodyPresent   bool
	RequestBody          []RequestBodyEntry
	TransportOptions     []ClientOption
	BodyAffectingOptions []ClientOption
}
