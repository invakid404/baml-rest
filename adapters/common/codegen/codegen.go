// Package codegen emits the per-adapter generated Go code (the
// streaming/call routers, ParseStream / Parse helpers, mirror struct
// converters, options builders, dynamic-type TypeBuilder helpers) and
// the framework adapter struct that satisfies bamlutils.Adapter. The
// public surface (Options, Generate, GenerateWithOptions,
// GenerateFrameworkAdapter) lives in this file; per-concern emission
// is split across sibling files in the same package.
package codegen

import (
	"fmt"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
)

const (
	// BamlPkg is the path to the BAML Go runtime package
	BamlPkg = "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// Options configures code generation per adapter. Each adapter's
// cmd/main.go populates this with its BAML-version-specific feature
// flags before invoking GenerateWithOptions.
type Options struct {
	// SelfPkg is the adapter module's import path, e.g.
	// "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0".
	SelfPkg string
	// SupportsWithClient reports whether the bundled BAML runtime
	// exposes the `WithClient(clientName)` CallOption. Added in BAML
	// 0.219.0; older runtimes (v0.204, v0.215) lack the symbol and
	// emitting a reference to it produces an "undefined: WithClient"
	// compile error. When false, the generated router skips the
	// per-call client override and the legacy path relies on BAML's
	// own strategy rotation for baml-roundrobin chains.
	SupportsWithClient bool
	// HasWrapMapValues reports whether the adapter module ships a
	// WrapMapValues helper (defined in the adapter package's
	// dynamic_value.go) that the framework SetClientRegistry must apply
	// to per-client Options before forwarding into BAML's
	// AddLlmClient. v0.204 only — the older CFFI shape rejects nested
	// maps that aren't pre-wrapped; v0.215.0+ handles nested maps
	// natively, so the framework adapter passes Options directly.
	HasWrapMapValues bool
	// HasHTTPClient reports whether the framework BamlAdapter exposes
	// an injectable httpClient field plus a SetHTTPClient setter.
	// v0.219 only — the BuildRequest path's per-request HTTP client
	// override is the only consumer; v0.204/v0.215 lack BuildRequest
	// support and their HTTPClient() unconditionally returns nil.
	HasHTTPClient bool
}

// Generate generates the adapter.go file for the given adapter
// package. selfPkg should be the full package path, e.g.
// "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0".
// Feature flags are derived from the introspected output (currently
// SupportsWithClient, which the introspect step detects by AST-
// walking baml_client/*.go).
//
// Deprecated: prefer GenerateWithOptions for explicit per-adapter
// feature flags. This wrapper is kept for backward compatibility.
func Generate(selfPkg string) {
	GenerateWithOptions(Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: introspected.SupportsWithClient,
	})
}

// GenerateWithOptions runs code generation with per-adapter feature
// flags. See Options for the available knobs.
func GenerateWithOptions(opts Options) {
	generate(opts)
}

// generator carries the file-level state threaded through every
// per-concern emit method. Construction goes through newGenerator so
// the derived selfAdapterPkg / selfUtilsPkg / supportsWithClient
// fields are populated consistently. Per-method state lives on the
// methodEmitter sub-context (codegen_methods.go).
type generator struct {
	opts                 Options
	out                  *jen.File
	selfPkg              string
	selfAdapterPkg       string
	selfUtilsPkg         string
	supportsWithClient   bool
	mirrors              *mirrorStructTracker
	emittedUnwrapHelpers map[string]bool
}

func newGenerator(opts Options) *generator {
	return &generator{
		opts:                 opts,
		out:                  common.MakeFile(),
		selfPkg:              opts.SelfPkg,
		selfAdapterPkg:       opts.SelfPkg + "/adapter",
		selfUtilsPkg:         opts.SelfPkg + "/utils",
		supportsWithClient:   opts.SupportsWithClient,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: make(map[string]bool),
	}
}

// withClientOverrideBlock wraps a `clientOverride != ""` guard around
// WithClient(clientOverride) appends. When the target BAML runtime
// lacks the WithClient CallOption (< 0.219.0), the whole block is
// elided — emitting jen.Empty() instead of a reference that would
// produce "undefined: WithClient" at adapter compile time. Only the
// WithClient CallOption is gated; the legacy streaming path still
// consumes clientOverride via SetPrimaryClient(clientOverride) on
// the registry primary in makeLegacyStreamOptionsFromAdapter
// (BAML's Stream.<Method> drops callOpts.client and reads only the
// registry's primary). So on pre-0.219 runtimes the per-attempt
// override is dropped at non-streaming dispatch sites but still
// honoured by the legacy streaming path via the registry seam.
func (g *generator) withClientOverrideBlock(body ...jen.Code) jen.Code {
	if !g.supportsWithClient {
		return jen.Empty()
	}
	return jen.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(body...)
}

// withClientCloneAndAppend emits the clone-and-append-WithClient
// idiom that runs at every dispatch site where a per-attempt
// client override may have been resolved (BuildRequest leaf
// dispatch, mixed-mode bridge legacy children, top-level legacy
// fallthrough). The clone is load-bearing: the base options slice
// is shared with sibling requests via a backing array that may or
// may not have been reallocated by the upstream Append, and
// appending WithClient without cloning would mutate that shared
// state. Six call sites previously inlined this pattern with
// subtly different destination/source spellings; collapsing into
// one helper makes the invariant explicit and removes an
// opportunity for the clone to drift between sites. dst is the
// variable being reassigned, src is the slice to clone (often,
// but not always, the same name).
func (g *generator) withClientCloneAndAppend(dst, src string) jen.Code {
	return g.withClientOverrideBlock(
		jen.Id(dst).Op("=").Append(
			jen.Qual("slices", "Clone").Call(jen.Id(src)),
			jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
		),
	)
}

// generate is the orchestrator: validates the SupportsWithClient
// invariant, then delegates to per-concern emit methods on
// generator. Emission order is load-bearing — Jen's *jen.File
// preserves declaration order, and the per-adapter behavioural tests
// (streaming/call routers) plus the framework-adapter deterministic-
// emission check pin the file's shape. Reordering anything below
// changes the generated file byte-for-byte.
func generate(opts Options) {
	g := newGenerator(opts)

	// Fail-fast invariant: BuildRequest emission requires the target
	// BAML runtime to expose WithClient — without it the generated
	// router cannot honor the
	// per-attempt clientOverride that fallback / round-robin / dynamic-
	// primary semantics depend on. The introspected Request /
	// StreamRequest singletons are the BR API presence signal; if
	// they exist while SupportsWithClient is false, codegen would emit
	// BuildRequest paths that compile but silently dispatch to the
	// wrong client (planned metadata reports the resolved leaf, BAML
	// builds the HTTP request for the static default).
	//
	// In the shipped matrix this is unreachable: v0.204/v0.215 expose
	// neither Request/StreamRequest nor WithClient, and v0.219 sets
	// SupportsWithClient=true. The guard catches custom BAML Go
	// libraries, future partial API shapes, and direct
	// GenerateWithOptions misuse — all paths where the configuration
	// can drift out of sync without anyone noticing until production.
	if !g.supportsWithClient && (introspected.Request != nil || introspected.StreamRequest != nil) {
		// Include the per-singleton presence flags so the panic
		// uniquely identifies which API surface is missing — a
		// substring match on "Request" alone can't distinguish a
		// Request-only-set case from a StreamRequest-only-set one.
		panic(fmt.Sprintf("codegen: SupportsWithClient=false is incompatible with introspected Request/StreamRequest (Request=%v, StreamRequest=%v); BuildRequest emission requires WithClient to honor per-attempt client overrides",
			introspected.Request != nil, introspected.StreamRequest != nil))
	}

	methods := g.emitMethods()
	g.emitMethodsMap(methods)
	parseMethods := g.emitParseMethods()
	g.emitParseMethodsMap(parseMethods)
	generateApplyDynamicTypes(g.out)
	g.emitFactories()
	g.emitOptionsHelpers()
	emitMakeLegacyChildOptionsFromAdapter(g.out, g.selfAdapterPkg)
	emitMakeLegacyStreamOptionsFromAdapter(g.out, g.selfAdapterPkg)
	g.emitInitBamlRuntime()
	generateStreamHelpers(g.out)

	if err := common.Commit(g.out); err != nil {
		panic(err)
	}
}
