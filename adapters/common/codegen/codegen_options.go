package codegen

import (
	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
)

// emitMakeLegacyChildOptionsFromAdapter writes the
// makeLegacyChildOptionsFromAdapter function into the generated
// adapter package. The emitted function is the per-callback scoped
// registry builder for mixed-mode legacy children, covering both
// runtime-override cases (where a request's client_registry shapes
// the chain) and the static mixed-mode case (where the chain is
// composed entirely from compiled definitions).
//
// The outer BuildRequest-safe options drops every resolved strategy
// parent (so BuildRequest leaf dispatch is shielded from BAML's
// eager parent parsing). Reusing those options in
// legacyStreamChildFn / legacyCallChildFn for a strategy-parent
// child means BAML never sees any runtime override on that nested
// parent — wrong-leaf dispatch (silent), suppressed validation, and
// client-not-found for dynamic-only nested parents are the
// observable symptoms when a runtime registry is present. Without
// one, BAML falls through to the function's compiled default
// client when no registry is forwarded, which silently re-runs the
// whole compiled chain instead of the targeted child.
//
// The emitted helper builds a fresh registry per callback that
// scopes in: every non-strategy runtime client (so dynamic leaves
// under a preserved parent resolve), the target strategy parent if
// it carries an explicit override, and any strategy parent
// transitively reachable from clientOverride. The reachable set
// guards against unrelated explicit strategy parents poisoning
// this child's BAML eager-parse, which is exactly what motivated
// the dual-view split in SetClientRegistry. When the request has
// no runtime client_registry the helper still emits a registry
// whose primary is clientOverride — so BAML's streaming path
// dispatches the targeted child rather than falling through to
// the function's compiled default.
//
// Provider materialisation goes through the same
// UpstreamClientRegistryProvider path as SetClientRegistry — the
// canonical "baml-roundrobin" spelling translates to upstream
// "baml-round-robin" and omitted-provider runtime entries get the
// introspected fallback so the CFFI seam doesn't reject them.
//
// Primary on the scoped registry is pinned to clientOverride.
// BAML's generated Stream.<Method> (functions_stream.go) consumes
// only callOpts.clientRegistry — callOpts.client is silently
// dropped on the streaming path. Mixed-mode legacy children
// (legacyStreamChildFn / legacyCallChildFn) always go through
// Stream.<Method>, so the registry's primary is the seam that
// selects which client BAML actually drives. Reapplying the outer
// original.Primary would either name a strategy parent filtered
// from the scoped registry (client-not-found) or drive a compiled
// outer client instead of the per-attempt clientOverride this
// callback exists to dispatch. An empty clientOverride is a no-op
// — the SetPrimaryClient call is gated to skip the empty case
// rather than forwarding "" into BAML's PromptRenderer.
//
// Extracted as a top-level function so the emit can be unit-tested
// against rendered output without running the full generate()
// pipeline (which writes adapter.go to disk and pollutes the repo).
func emitMakeLegacyChildOptionsFromAdapter(out *jen.File, selfAdapterPkg string) {
	out.Func().Id("makeLegacyChildOptionsFromAdapter").
		Params(
			jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("clientOverride").String(),
		).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.List(jen.Id("adapter"), jen.Id("ok")).Op(":=").Id("adapterIn").Assert(jen.Op("*").Qual(selfAdapterPkg, "BamlAdapter")),
			jen.If(jen.Op("!").Id("ok")).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
					jen.Lit("invalid adapter type: expected *BamlAdapter, got %T"),
					jen.Id("adapterIn"),
				)),
			),
			jen.Id("result").Op(":=").Make(
				jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
				jen.Lit(0), jen.Lit(3),
			),
			jen.Id("original").Op(":=").Id("adapter").Dot("OriginalClientRegistry").Call(),
			// Emit a scoped registry whenever there is something for it
			// to carry: either runtime client_registry overrides to
			// scope (original != nil) OR a per-attempt clientOverride
			// to pin as primary. The static mixed-mode case
			// (no runtime registry, but the chain has a legacy child)
			// is the second arm: BAML's Stream.<Method> drops
			// callOpts.client, so without a registry whose primary is
			// clientOverride, BAML would fall through to the function's
			// compiled default client (often the outer fallback parent
			// itself) and silently re-run the whole chain instead of
			// the targeted child.
			jen.If(jen.Id("original").Op("!=").Nil().Op("||").Id("clientOverride").Op("!=").Lit("")).Block(
				jen.Id("registry").Op(":=").Qual(BamlPkg, "NewClientRegistry").Call(),
				// Runtime overrides only when there's an original
				// registry to scope from. BuildLegacyChildRegistry-
				// Entries already filters to non-strategy leaves plus
				// strategy parents transitively reachable from
				// clientOverride; an empty clientOverride yields an
				// empty reachable set so only leaves survive.
				jen.If(jen.Id("original").Op("!=").Nil()).Block(
					jen.Id("entries").Op(":=").Qual(common.BuildRequestPkg, "BuildLegacyChildRegistryEntries").Call(
						jen.Id("original"),
						jen.Id("clientOverride"),
						jen.Id("adapter").Dot("IntrospectedClientProvider"),
						jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					),
					jen.For(jen.List(jen.Id("_"), jen.Id("e")).Op(":=").Range().Id("entries")).Block(
						jen.Id("registry").Dot("AddLlmClient").Call(
							jen.Id("e").Dot("Name"),
							jen.Id("e").Dot("Provider"),
							jen.Id("e").Dot("Options"),
						),
					),
				),
				// Pin the scoped registry's primary to clientOverride.
				// BAML's generated Stream.<Method> (functions_stream.go)
				// does not consume callOpts.client — only the non-
				// streaming Call path overlays WithClient onto the
				// registry's primary. Mixed-mode legacy children always
				// go through Stream.<Method> (legacyStreamChildFn /
				// legacyCallChildFn), so the registry's primary is the
				// only seam BAML reads. This fires whether or not the
				// request supplied a runtime registry — the static
				// mixed-mode case (compiled fallback parent + compiled
				// RR or unsupported-provider child) needs the same pin.
				jen.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(
					jen.Id("registry").Dot("SetPrimaryClient").Call(jen.Id("clientOverride")),
				),
				jen.Id("result").Op("=").Append(
					jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").Call(jen.Id("registry")),
				),
			),
			jen.If(jen.Id("adapter").Dot("TypeBuilder").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(
					jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").Call(jen.Id("adapter").Dot("TypeBuilder")),
				),
			),
			jen.Return(jen.Id("result"), jen.Nil()),
		)
}

// emitMakeLegacyStreamOptionsFromAdapter writes
// makeLegacyStreamOptionsFromAdapter into the generated adapter
// package. The emitted function is the top-level legacy streaming
// fallthrough's options builder, used by the _noRaw and _full
// dispatch sites that fall through from the BuildRequest path when
// the resolved provider can't be driven via BuildRequest.
//
// It exists because BAML's generated Stream.<Method>
// (functions_stream.go) does not consume callOpts.client — the
// non-streaming Call path overlays WithClient onto the registry's
// primary, but the streaming path does not. The _noRaw / _full
// preambles append WithClient(clientOverride) for routing
// observability, but on the streaming path that option is dropped:
// the registry's primary is the only seam BAML reads. Reusing
// makeLegacyOptionsFromAdapter here passes through the legacy
// registry view as-is, with whatever primary the operator
// supplied (or none) — so when the request's __effective leaf
// differs from the registry primary (most often after
// ResolveEffectiveClient unwraps a top-level RR), BAML executes the
// wrong client.
//
// Behaviour:
//
//   - Reuses adapter.LegacyClientRegistry verbatim when it is
//     non-nil — the legacy view already keeps explicit strategy
//     parents and drops only inert presence-only static parents,
//     matching the contract makeLegacyOptionsFromAdapter offered
//     before.
//   - When clientOverride is non-empty, calls
//     SetPrimaryClient(clientOverride) on the registry. The mutation
//     is scope-bound: cmd/worker creates a fresh adapter per
//     CallStream request, the registry never crosses request
//     boundaries, and only one of _noRaw / _full executes per
//     request, so the call is idempotent within its lifetime.
//   - When the request supplied no client_registry but
//     clientOverride is set, creates a fresh empty registry whose
//     primary is clientOverride. BAML resolves leaves against its
//     compiled clients map by name, so an empty-with-primary
//     registry is enough to redirect dispatch.
//   - When LegacyClientRegistry is nil and clientOverride is empty,
//     emits no WithClientRegistry option (matches the prior
//     no-runtime-registry behaviour).
//   - Preserves WithTypeBuilder handling.
//
// Extracted as a top-level function so the emit can be unit-tested
// against rendered output without running the full generate()
// pipeline (which writes adapter.go to disk).
func emitMakeLegacyStreamOptionsFromAdapter(out *jen.File, selfAdapterPkg string) {
	out.Func().Id("makeLegacyStreamOptionsFromAdapter").
		Params(
			jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("clientOverride").String(),
		).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.List(jen.Id("adapter"), jen.Id("ok")).Op(":=").Id("adapterIn").Assert(jen.Op("*").Qual(selfAdapterPkg, "BamlAdapter")),
			jen.If(jen.Op("!").Id("ok")).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
					jen.Lit("invalid adapter type: expected *BamlAdapter, got %T"),
					jen.Id("adapterIn"),
				)),
			),
			jen.Id("result").Op(":=").Make(
				jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
				jen.Lit(0), jen.Lit(3),
			),
			jen.Id("registry").Op(":=").Id("adapter").Dot("LegacyClientRegistry"),
			// Emit a registry whenever there's something to forward:
			// the adapter's legacy view (runtime entries the
			// operator supplied) OR a per-request clientOverride to
			// pin as primary. The static fallthrough case (no
			// runtime registry, but the routing plan resolved an
			// __effective leaf distinct from the function's
			// compiled default) is the second arm.
			jen.If(jen.Id("registry").Op("!=").Nil().Op("||").Id("clientOverride").Op("!=").Lit("")).Block(
				// Synthesize an empty registry only when the adapter
				// had nothing to forward. Reusing the adapter's
				// LegacyClientRegistry preserves the keep-strategy-
				// parent semantics SetClientRegistry built into it.
				jen.If(jen.Id("registry").Op("==").Nil()).Block(
					jen.Id("registry").Op("=").Qual(BamlPkg, "NewClientRegistry").Call(),
				),
				// SetPrimaryClient is the seam BAML's streaming path
				// actually reads. WithClient(clientOverride) at the
				// dispatch site is silently dropped on Stream.<Method>
				// — only callOpts.clientRegistry survives. The
				// mutation is scope-bound to a single per-request
				// adapter (cmd/worker recreates the adapter per
				// CallStream).
				jen.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(
					jen.Id("registry").Dot("SetPrimaryClient").Call(jen.Id("clientOverride")),
				),
				jen.Id("result").Op("=").Append(
					jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").Call(jen.Id("registry")),
				),
			),
			jen.If(jen.Id("adapter").Dot("TypeBuilder").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(
					jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").Call(jen.Id("adapter").Dot("TypeBuilder")),
				),
			),
			jen.Return(jen.Id("result"), jen.Nil()),
		)
}

// emitOptionsHelpers emits the makeOptionsFromAdapterInternal /
// makeOptionsFromAdapter / makeLegacyOptionsFromAdapter trio. The
// shared body builds the WithClientRegistry + WithTypeBuilder slice
// off either adapter.ClientRegistry (BuildRequest-safe view) or
// adapter.LegacyClientRegistry (legacy view), selected by the
// internal `legacy` parameter; the two thin wrappers pin the choice
// at each dispatch site.
func (g *generator) emitOptionsHelpers() {
	out := g.out

	// makeOptionsFromAdapter and makeLegacyOptionsFromAdapter share
	// every step except which registry field they read. Emit a single
	// makeOptionsFromAdapterInternal that owns the type assertion +
	// slice build + WithClientRegistry-and-TypeBuilder appends, then
	// emit two thin wrappers that select the registry view via a
	// boolean. Trades two duplicate generated functions for three
	// smaller ones; the assertion happens once per call instead of
	// once per emitted function.

	// Generate `makeOptionsFromAdapterInternal` — shared body.
	// `legacy` selects the registry view: false → ClientRegistry
	// (BuildRequest-safe, drops every baml-rest-resolved strategy
	// parent); true → LegacyClientRegistry (preserves explicit
	// strategy-parent overrides).
	out.Func().Id("makeOptionsFromAdapterInternal").
		Params(
			jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("legacy").Bool(),
		).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.List(jen.Id("adapter"), jen.Id("ok")).Op(":=").Id("adapterIn").Assert(jen.Op("*").Qual(g.selfAdapterPkg, "BamlAdapter")),
			jen.If(jen.Op("!").Id("ok")).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
					jen.Lit("invalid adapter type: expected *BamlAdapter, got %T"),
					jen.Id("adapterIn"),
				)),
			),
			// Select the registry view at runtime so the same body
			// covers both wrappers. The two fields have identical
			// types, so the conditional collapses to a simple
			// pointer pick.
			jen.Id("registry").Op(":=").Id("adapter").Dot("ClientRegistry"),
			jen.If(jen.Id("legacy")).Block(
				jen.Id("registry").Op("=").Id("adapter").Dot("LegacyClientRegistry"),
			),
			// Pre-size with capacity 3: room for ClientRegistry + TypeBuilder + WithOnTick
			jen.Id("result").Op(":=").Make(jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"), jen.Lit(0), jen.Lit(3)),
			jen.If(jen.Id("registry").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").
						Call(jen.Id("registry"))),
			),
			jen.If(jen.Id("adapter").Dot("TypeBuilder").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").
						Call(jen.Id("adapter").Dot("TypeBuilder"))),
			),
			jen.Return(jen.Id("result"), jen.Nil()),
		)

	// Generate `makeOptionsFromAdapter` — BuildRequest-safe wrapper.
	// Used by BuildRequest leaf dispatch, where every WithClient call
	// targets a resolved leaf so the parent shape is irrelevant and
	// dropping resolved strategy parents shields the request from
	// BAML's eager parse of unrelated parent entries. Mixed-mode
	// legacy-child callbacks for strategy-parent children build their
	// own per-callback scoped registry via
	// makeLegacyChildOptionsFromAdapter rather than reusing this view.
	out.Func().Id("makeOptionsFromAdapter").
		Params(jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter")).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.Return(jen.Id("makeOptionsFromAdapterInternal").Call(jen.Id("adapterIn"), jen.False())),
		)

	// Generate `makeLegacyOptionsFromAdapter` — legacy wrapper.
	// Used by the top-level legacy fallthrough (_noRaw / _full impls
	// invoked via __legacyClientOverride) so BAML can honour runtime
	// strategy-parent overrides or emit canonical errors.
	out.Func().Id("makeLegacyOptionsFromAdapter").
		Params(jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter")).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.Return(jen.Id("makeOptionsFromAdapterInternal").Call(jen.Id("adapterIn"), jen.True())),
		)
}
