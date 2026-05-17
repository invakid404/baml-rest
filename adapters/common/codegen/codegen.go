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
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
)

const (
	// BamlPkg is the path to the BAML Go runtime package as built into
	// the server (github.com/boundaryml/baml/...). Kept as a package
	// constant so legacy callers that consume the symbol directly still
	// resolve; new code should prefer Options.Packages.BamlPkg so a
	// non-default runtime module (a vendored or forked BAML) can be
	// substituted without touching this file.
	BamlPkg = "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// PackageConfig collects every import path the generated adapter
// touches plus the output file's own package identity. The default
// configuration ([DefaultPackageConfig]) reproduces the server's
// root build layout; alternate layouts (e.g. an embedded dynamic
// client living under a sub-tree of this module) supply their own.
type PackageConfig struct {
	// OutputPkg is the full import path of the generated adapter.go's
	// home package, e.g. github.com/invakid404/baml-rest.
	OutputPkg string
	// OutputPkgName is the Go package identifier for the generated
	// adapter.go (the `package X` clause).
	OutputPkgName string
	// OutputPath is the filesystem path the generated adapter.go is
	// written to, resolved relative to the current working directory.
	OutputPath string
	// GeneratedClientPkg is the import path of the BAML-generated
	// client package (`baml_client/`) whose Stream / Parse / sync
	// functions the adapter dispatches into.
	GeneratedClientPkg string
	// IntrospectedPkg is the import path of the cmd/introspect-emitted
	// introspected package whose maps / singletons the adapter reads
	// at request time.
	IntrospectedPkg string
	// InterfacesPkg is the import path of the bamlutils interfaces
	// package (StreamResult, Metadata, Adapter, MediaKind, etc.).
	InterfacesPkg string
	// SSEPkg is the import path of bamlutils/sse.
	SSEPkg string
	// BuildRequestPkg is the import path of bamlutils/buildrequest.
	BuildRequestPkg string
	// LLMHTTPPkg is the import path of bamlutils/llmhttp.
	LLMHTTPPkg string
	// RetryPkg is the import path of bamlutils/retry.
	RetryPkg string
	// BamlPkg is the import path of the BAML Go runtime
	// (engine/language_client_go/pkg). A forked / vendored runtime
	// overrides this with its own module prefix.
	BamlPkg string
	// QueuePkg is the import path of the go-concurrent-queue dependency
	// used by the streaming helpers.
	QueuePkg string
}

// DefaultPackageConfig returns the PackageConfig that reproduces the
// server build's current import layout. Used by the legacy
// [Generate](selfPkg) wrapper to keep existing call sites working
// without an explicit Options struct.
func DefaultPackageConfig() PackageConfig {
	return PackageConfig{
		OutputPkg:          common.RootPkg,
		OutputPkgName:      common.RootPkgName,
		OutputPath:         common.OutputPath,
		GeneratedClientPkg: common.GeneratedClientPkg,
		IntrospectedPkg:    common.IntrospectedPkg,
		InterfacesPkg:      common.InterfacesPkg,
		SSEPkg:             common.SSEPkg,
		BuildRequestPkg:    common.BuildRequestPkg,
		LLMHTTPPkg:         common.LLMHTTPPkg,
		RetryPkg:           common.RetryPkg,
		BamlPkg:            BamlPkg,
		QueuePkg:           common.GoConcurrentQueuePkg,
	}
}

// Introspection mirrors the public surface of the root introspected
// package — the singletons, method maps, and reflection handles the
// generator reads to decide what to emit. It exists so callers can
// supply an alternate introspected package (rooted at a different
// import path) without the codegen package importing it directly.
type Introspection struct {
	SupportsWithClient bool
	Request            any
	StreamRequest      any
	StreamMethods      map[string][]string
	SyncMethods        map[string][]string
	SyncFuncs          map[string]any
	ParseMethods       map[string]struct{}
	ParseStreamMethods map[string]struct{}
	ParseStreamFuncs   map[string]any
	MediaParams        map[string]map[string]bamlutils.MediaKind
}

// RootIntrospection returns the Introspection populated from the
// root introspected package. The server build's [Generate] wrapper
// uses this so existing callers keep their behaviour without
// constructing a struct.
func RootIntrospection() Introspection {
	return Introspection{
		SupportsWithClient: introspected.SupportsWithClient,
		Request:            introspected.Request,
		StreamRequest:      introspected.StreamRequest,
		StreamMethods:      introspected.StreamMethods,
		SyncMethods:        introspected.SyncMethods,
		SyncFuncs:          introspected.SyncFuncs,
		ParseMethods:       introspected.ParseMethods,
		ParseStreamMethods: introspected.ParseStreamMethods,
		ParseStreamFuncs:   introspected.ParseStreamFuncs,
		MediaParams:        introspected.MediaParams,
	}
}

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
	// Packages collects every import path the generated adapter
	// references. An empty Packages is filled from
	// [DefaultPackageConfig] before generation.
	Packages PackageConfig
	// Introspection carries the runtime-detected facts the generator
	// reads from the introspected package (SupportsWithClient mirror,
	// Request / StreamRequest singletons, sync / parse method maps,
	// media-param map). An empty Introspection.SyncMethods is treated
	// as "use the root introspected package" for backwards
	// compatibility with [Generate].
	Introspection Introspection
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
	pkgs                 PackageConfig
	intro                Introspection
	out                  *jen.File
	selfPkg              string
	selfAdapterPkg       string
	selfUtilsPkg         string
	supportsWithClient   bool
	mirrors              *mirrorStructTracker
	emittedUnwrapHelpers map[string]bool
}

func newGenerator(opts Options) *generator {
	resolved := resolveOptions(opts)
	return &generator{
		opts:                 resolved,
		pkgs:                 resolved.Packages,
		intro:                resolved.Introspection,
		out:                  jen.NewFilePathName(resolved.Packages.OutputPkg, resolved.Packages.OutputPkgName),
		selfPkg:              resolved.SelfPkg,
		selfAdapterPkg:       resolved.SelfPkg + "/adapter",
		selfUtilsPkg:         resolved.SelfPkg + "/utils",
		supportsWithClient:   resolved.SupportsWithClient,
		mirrors:              newMirrorStructTracker(),
		emittedUnwrapHelpers: make(map[string]bool),
	}
}

// resolveOptions fills any unset fields on Options from the server
// build defaults so existing callers ([Generate], adapter binaries
// that build only an Options{SelfPkg, ...} value) keep working.
// The Introspection presence is detected via SyncMethods — every
// usable introspected output emits a non-nil SyncMethods map, even
// when the project has zero sync functions.
func resolveOptions(opts Options) Options {
	if opts.Packages == (PackageConfig{}) {
		opts.Packages = DefaultPackageConfig()
	}
	if opts.Introspection.SyncMethods == nil {
		opts.Introspection = RootIntrospection()
	}
	return opts
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
			jen.Qual(g.pkgs.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
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
	if !g.supportsWithClient && (g.intro.Request != nil || g.intro.StreamRequest != nil) {
		// Include the per-singleton presence flags so the panic
		// uniquely identifies which API surface is missing — a
		// substring match on "Request" alone can't distinguish a
		// Request-only-set case from a StreamRequest-only-set one.
		panic(fmt.Sprintf("codegen: SupportsWithClient=false is incompatible with introspected Request/StreamRequest (Request=%v, StreamRequest=%v); BuildRequest emission requires WithClient to honor per-attempt client overrides",
			g.intro.Request != nil, g.intro.StreamRequest != nil))
	}

	methods := g.emitMethods()
	g.emitMethodsMap(methods)
	parseMethods := g.emitParseMethods()
	g.emitParseMethodsMap(parseMethods)
	generateApplyDynamicTypes(g.out, g.pkgs)
	g.emitFactories()
	g.emitOptionsHelpers()
	emitMakeLegacyChildOptionsFromAdapter(g.out, g.selfAdapterPkg, g.pkgs)
	emitMakeLegacyStreamOptionsFromAdapter(g.out, g.selfAdapterPkg, g.pkgs)
	g.emitInitBamlRuntime()
	generateStreamHelpers(g.out, g.pkgs)

	if err := g.out.Save(g.pkgs.OutputPath); err != nil {
		panic(err)
	}
}
