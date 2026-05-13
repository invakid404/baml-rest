package codegen

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
)

// GenerateFrameworkAdapter emits the per-adapter framework
// `adapter/adapter.go` for the supplied opts and writes it to
// outputPath. When outputPath is empty the path is derived from
// opts.SelfPkg as "<rel>/adapter/adapter.go" relative to the current
// working directory, where <rel> is opts.SelfPkg with the repository
// root prefix stripped.
//
// In customer builds the working directory is the per-customer
// baml_rest source root, so the derived path resolves to that build's
// copy of adapters/adapter_v*/adapter/adapter.go. This emitter is the
// sole source of truth for the framework adapter. The CI verifier
// (cmd/verify-framework-adapter) keeps a deterministic-emission check
// plus a temp-module behavioural test that compiles the emitted file
// against the per-adapter test harness.
func GenerateFrameworkAdapter(opts Options, outputPath string) {
	if outputPath == "" {
		rel := strings.TrimPrefix(opts.SelfPkg, common.RootPkg+"/")
		outputPath = filepath.Join(rel, "adapter", "adapter.go")
	}
	// Ensure the adapter/ parent directory exists. The per-adapter
	// adapter/ subtree may have no non-test files at all (v0.215,
	// v0.219), so an extracted customer-build workdir won't contain
	// the directory until something puts a file in it.
	if dir := filepath.Dir(outputPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
	}
	out := jen.NewFilePathName(opts.SelfPkg+"/adapter", "adapter")
	// BAML runtime's actual package name is "pkg"; alias it as "baml"
	// everywhere so the emitted file reads naturally. ImportAlias forces
	// an explicit `baml ...` clause in the import group; ImportName is
	// sufficient for the others where the natural and desired aliases
	// coincide.
	out.ImportAlias(BamlPkg, "baml")
	out.ImportName(common.InterfacesPkg, "bamlutils")
	out.ImportName(common.LLMHTTPPkg, "llmhttp")
	out.ImportName(common.IntrospectedPkg, "introspected")
	emitFrameworkAdapter(out, opts)
	if err := out.Save(outputPath); err != nil {
		panic(err)
	}
}

// emitFrameworkAdapter populates the supplied jen.File with the full
// framework adapter.go: package preamble, type aliases, BamlAdapter
// struct, and every accessor/setter the bamlutils.Adapter contract
// requires. Per-version divergence (the v0.204 WrapMapValues call
// inside SetClientRegistry, the v0.219 httpClient field +
// SetHTTPClient method) is gated on opts.HasWrapMapValues /
// opts.HasHTTPClient.
//
// Determinism: this emitter does NOT iterate any maps. Every field /
// method is laid down in a fixed order so two consecutive calls
// produce byte-identical output. The deterministic-emission CI check
// (cmd/verify-framework-adapter Check 1) is the safety net.
func emitFrameworkAdapter(out *jen.File, opts Options) {
	bamlPkg := BamlPkg
	bamlutilsPkg := common.InterfacesPkg
	llmhttpPkg := common.LLMHTTPPkg
	introspectedPkg := common.IntrospectedPkg

	// TypeBuilderFactory creates a TypeBuilder via the per-adapter
	// generated code (which has direct access to the baml_client
	// TypeBuilder methods) and is wired into BamlAdapter at construction
	// time by codegen-emitted MakeAdapter.
	out.Comment("TypeBuilderFactory creates a new TypeBuilder and applies the TypeBuilder config.")
	out.Comment("The generated code implements this to have direct access to the generated")
	out.Comment("TypeBuilder methods for processing DynamicTypes and BamlSnippets.")
	out.Type().Id("TypeBuilderFactory").Func().
		Params(jen.Id("tb").Op("*").Qual(bamlutilsPkg, "TypeBuilder")).
		Params(jen.Op("*").Qual(introspectedPkg, "TypeBuilder"), jen.Error())

	out.Comment("MediaFactory creates BAML media objects (image, audio, pdf, video) from URL or base64 data.")
	out.Comment("Populated by the generated code which has access to the baml_client package-level constructors.")
	out.Type().Id("MediaFactory").Func().
		Params(
			jen.Id("kind").Qual(bamlutilsPkg, "MediaKind"),
			jen.Id("url").Op("*").String(),
			jen.Id("base64").Op("*").String(),
			jen.Id("mimeType").Op("*").String(),
		).
		Params(jen.Any(), jen.Error())

	// BamlAdapter struct. Field order is fixed for deterministic
	// emission; two consecutive emit calls must produce byte-identical
	// output.
	structFields := []jen.Code{
		jen.Qual("context", "Context"),
		jen.Line(),
		jen.Id("TypeBuilderFactory").Id("TypeBuilderFactory"),
		jen.Id("MediaFactory").Id("MediaFactory"),
		jen.Line(),
		jen.Comment("ClientRegistry is the BuildRequest-safe view (drops every"),
		jen.Comment("baml-rest-resolved strategy parent). LegacyClientRegistry is"),
		jen.Comment("the top-level legacy view (keeps explicit parent overrides,"),
		jen.Comment("drops only inert presence-only static parents)."),
		jen.Id("ClientRegistry").Op("*").Qual(bamlPkg, "ClientRegistry"),
		jen.Id("LegacyClientRegistry").Op("*").Qual(bamlPkg, "ClientRegistry"),
		jen.Id("TypeBuilder").Op("*").Qual(introspectedPkg, "TypeBuilder"),
		jen.Line(),
		jen.Comment("streamMode controls how streaming results are processed."),
		jen.Id("streamMode").Qual(bamlutilsPkg, "StreamMode"),
		jen.Line(),
		jen.Comment("logger is used for debug output during dynamic type processing."),
		jen.Id("logger").Qual(bamlutilsPkg, "Logger"),
		jen.Line(),
		jen.Comment("retryConfig holds per-request retry overrides from __baml_options__.retry."),
		jen.Id("retryConfig").Op("*").Qual(bamlutilsPkg, "RetryConfig"),
		jen.Line(),
		jen.Comment("includeReasoning is the per-request opt-in for surfacing"),
		jen.Comment("provider-specific reasoning/thinking text on /with-raw's"),
		jen.Comment("reasoning field, distinct from raw (which stays text-only)."),
		jen.Comment("Mirrors __baml_options__.include_reasoning and is honored by"),
		jen.Comment("the SSE/non-streaming extractors. Never affects the parseable"),
		jen.Comment("text passed to Parse/ParseStream."),
		jen.Id("includeReasoning").Bool(),
		jen.Line(),
		jen.Comment("clientRegistryProvider is the provider of the primary client from"),
		jen.Comment("the runtime ClientRegistry override. Empty if no override."),
		jen.Id("clientRegistryProvider").String(),
		jen.Line(),
		jen.Comment("originalClientRegistry stores the original request ClientRegistry."),
		jen.Id("originalClientRegistry").Op("*").Qual(bamlutilsPkg, "ClientRegistry"),
	}

	if opts.HasHTTPClient {
		structFields = append(structFields,
			jen.Line(),
			jen.Comment("httpClient is an optional custom HTTP client for the BuildRequest path,"),
			jen.Comment("used by both streaming and non-streaming /call orchestration."),
			jen.Comment("When nil, llmhttp.DefaultClient is used."),
			jen.Id("httpClient").Op("*").Qual(llmhttpPkg, "Client"),
		)
	}

	structFields = append(structFields,
		jen.Line(),
		jen.Comment("rrAdvancer is the per-request round-robin Advancer installed by the"),
		jen.Comment("worker; nil falls back to the introspected Coordinator."),
		jen.Id("rrAdvancer").Qual(bamlutilsPkg, "RoundRobinAdvancer"),
		jen.Line(),
		jen.Comment("IntrospectedClientProvider is the build-time map of static"),
		jen.Comment(".baml client name -> provider string. Set by the generated"),
		jen.Comment("MakeAdapter so SetClientRegistry can materialise providers"),
		jen.Comment("for runtime registry entries that omit the `provider` key"),
		jen.Comment("(strategy-only / presence-only overrides)."),
		jen.Id("IntrospectedClientProvider").Map(jen.String()).String(),
		jen.Line(),
		jen.Comment("upstreamClientNames / legacyUpstreamClientNames record the order"),
		jen.Comment("of names passed to AddLlmClient on the BuildRequest-safe and"),
		jen.Comment("legacy registry views respectively. Test-only observability for"),
		jen.Comment("the dual-view forwarding rules and the strategy-parent drop."),
		jen.Id("upstreamClientNames").Index().String(),
		jen.Id("legacyUpstreamClientNames").Index().String(),
	)

	out.Type().Id("BamlAdapter").Struct(structFields...)

	emitFrameworkAdapterRetryConfig(out, bamlutilsPkg)
	emitFrameworkAdapterIncludeReasoning(out)
	emitFrameworkAdapterSetClientRegistry(out, opts, bamlPkg, bamlutilsPkg)
	emitFrameworkAdapterClientRegistryProvider(out)
	emitFrameworkAdapterOriginalClientRegistry(out, bamlutilsPkg)
	emitFrameworkAdapterSetTypeBuilder(out, bamlutilsPkg, introspectedPkg)
	emitFrameworkAdapterStreamModeAndLogger(out, bamlutilsPkg)
	emitFrameworkAdapterMediaConstructors(out, bamlutilsPkg)
	emitFrameworkAdapterHTTPClient(out, opts, llmhttpPkg)
	emitFrameworkAdapterRoundRobinAdvancer(out, bamlutilsPkg)

	// Compile-time interface conformance check. Catches drift if the
	// bamlutils.Adapter interface gains/loses a method without the
	// adapter being updated.
	out.Var().Id("_").Qual(bamlutilsPkg, "Adapter").Op("=").
		Parens(jen.Op("*").Id("BamlAdapter")).Parens(jen.Nil())
}

func emitFrameworkAdapterRetryConfig(out *jen.File, bamlutilsPkg string) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetRetryConfig").Params(jen.Id("config").Op("*").Qual(bamlutilsPkg, "RetryConfig")).
		Block(
			jen.Id("b").Dot("retryConfig").Op("=").Id("config"),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("RetryConfig").Params().Op("*").Qual(bamlutilsPkg, "RetryConfig").
		Block(
			jen.Return(jen.Id("b").Dot("retryConfig")),
		)
}

func emitFrameworkAdapterIncludeReasoning(out *jen.File) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetIncludeReasoning").Params(jen.Id("includeReasoning").Bool()).
		Block(
			jen.Id("b").Dot("includeReasoning").Op("=").Id("includeReasoning"),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("IncludeReasoning").Params().Bool().
		Block(
			jen.Return(jen.Id("b").Dot("includeReasoning")),
		)
}

// addLlmClientOptionsExpr returns the jen expression passed to
// BAML's AddLlmClient as the per-client options map. v0.204 wraps it
// through the adapter-package-local WrapMapValues helper to satisfy
// the older CFFI shape; v0.215+ forwards client.Options verbatim.
func addLlmClientOptionsExpr(opts Options) jen.Code {
	if opts.HasWrapMapValues {
		return jen.Id("wrappedOptions")
	}
	return jen.Id("client").Dot("Options")
}

func emitFrameworkAdapterSetClientRegistry(out *jen.File, opts Options, bamlPkg, bamlutilsPkg string) {
	// Inner-loop body for materialising both registry views.
	innerLoopStmts := []jen.Code{
		jen.If(jen.Id("client").Op("==").Nil()).Block(jen.Continue()),
		jen.Id("upstreamProvider").Op(":=").Qual(bamlutilsPkg, "UpstreamClientRegistryProvider").
			Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
	}
	if opts.HasWrapMapValues {
		innerLoopStmts = append(innerLoopStmts,
			jen.Id("wrappedOptions").Op(":=").Id("WrapMapValues").Call(jen.Id("client").Dot("Options")),
		)
	}
	innerLoopStmts = append(innerLoopStmts,
		jen.If(
			jen.Op("!").Qual(bamlutilsPkg, "IsResolvedStrategyParent").
				Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
		).Block(
			jen.Id("b").Dot("ClientRegistry").Dot("AddLlmClient").Call(
				jen.Id("client").Dot("Name"),
				jen.Id("upstreamProvider"),
				addLlmClientOptionsExpr(opts),
			),
			jen.Id("b").Dot("upstreamClientNames").Op("=").
				Append(jen.Id("b").Dot("upstreamClientNames"), jen.Id("client").Dot("Name")),
		),
		jen.If(
			jen.Op("!").Qual(bamlutilsPkg, "ShouldDropStrategyParentForTopLevelLegacy").
				Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
		).Block(
			jen.Id("b").Dot("LegacyClientRegistry").Dot("AddLlmClient").Call(
				jen.Id("client").Dot("Name"),
				jen.Id("upstreamProvider"),
				addLlmClientOptionsExpr(opts),
			),
			jen.Id("b").Dot("legacyUpstreamClientNames").Op("=").
				Append(jen.Id("b").Dot("legacyUpstreamClientNames"), jen.Id("client").Dot("Name")),
		),
	)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetClientRegistry").Params(jen.Id("clientRegistry").Op("*").Qual(bamlutilsPkg, "ClientRegistry")).
		Error().
		Block(
			// Nil-registry guard: clear all dependent state.
			jen.If(jen.Id("clientRegistry").Op("==").Nil()).Block(
				jen.Id("b").Dot("ClientRegistry").Op("=").Nil(),
				jen.Id("b").Dot("LegacyClientRegistry").Op("=").Nil(),
				jen.Id("b").Dot("clientRegistryProvider").Op("=").Lit(""),
				jen.Id("b").Dot("originalClientRegistry").Op("=").Nil(),
				jen.Id("b").Dot("upstreamClientNames").Op("=").
					Id("b").Dot("upstreamClientNames").Index(jen.Empty(), jen.Lit(0)),
				jen.Id("b").Dot("legacyUpstreamClientNames").Op("=").
					Id("b").Dot("legacyUpstreamClientNames").Index(jen.Empty(), jen.Lit(0)),
				jen.Return(jen.Nil()),
			),
			// Reject duplicate runtime client names before mutating
			// adapter state. BAML's AddLlmClient is map-backed (last-
			// wins) while baml-rest is slice-forward (first-match);
			// duplicates would let the two halves classify against
			// different definitions.
			jen.If(
				jen.Err().Op(":=").Id("clientRegistry").Dot("Validate").Call(),
				jen.Err().Op("!=").Nil(),
			).Block(jen.Return(jen.Err())),
			jen.Id("b").Dot("originalClientRegistry").Op("=").Id("clientRegistry"),
			jen.Id("b").Dot("clientRegistryProvider").Op("=").Lit(""),
			jen.Id("b").Dot("ClientRegistry").Op("=").Qual(bamlPkg, "NewClientRegistry").Call(),
			jen.Id("b").Dot("LegacyClientRegistry").Op("=").Qual(bamlPkg, "NewClientRegistry").Call(),
			jen.Id("b").Dot("upstreamClientNames").Op("=").
				Id("b").Dot("upstreamClientNames").Index(jen.Empty(), jen.Lit(0)),
			jen.Id("b").Dot("legacyUpstreamClientNames").Op("=").
				Id("b").Dot("legacyUpstreamClientNames").Index(jen.Empty(), jen.Lit(0)),
			// Materialise the dual views.
			jen.For(
				jen.List(jen.Id("_"), jen.Id("client")).Op(":=").Range().Id("clientRegistry").Dot("Clients"),
			).Block(innerLoopStmts...),
			// Empty-primary guard: treat `Primary != nil &&
			// *Primary == ""` the same as `Primary == nil`.
			jen.If(
				jen.Id("clientRegistry").Dot("Primary").Op("!=").Nil().
					Op("&&").
					Op("*").Id("clientRegistry").Dot("Primary").Op("!=").Lit(""),
			).Block(
				jen.Id("b").Dot("ClientRegistry").Dot("SetPrimaryClient").Call(jen.Op("*").Id("clientRegistry").Dot("Primary")),
				jen.Id("b").Dot("LegacyClientRegistry").Dot("SetPrimaryClient").Call(jen.Op("*").Id("clientRegistry").Dot("Primary")),
				jen.Id("foundPrimary").Op(":=").False(),
				jen.For(
					jen.List(jen.Id("_"), jen.Id("client")).Op(":=").Range().Id("clientRegistry").Dot("Clients"),
				).Block(
					jen.If(jen.Id("client").Op("==").Nil()).Block(jen.Continue()),
					jen.If(jen.Id("client").Dot("Name").Op("!=").Op("*").Id("clientRegistry").Dot("Primary")).Block(jen.Continue()),
					jen.Id("droppedFromBuildRequest").Op(":=").Qual(bamlutilsPkg, "IsResolvedStrategyParent").
						Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
					jen.Id("droppedFromLegacy").Op(":=").Qual(bamlutilsPkg, "ShouldDropStrategyParentForTopLevelLegacy").
						Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
					jen.If(jen.Id("droppedFromBuildRequest").Op("&&").Id("droppedFromLegacy")).Block(jen.Continue()),
					jen.Id("b").Dot("clientRegistryProvider").Op("=").Qual(bamlutilsPkg, "UpstreamClientRegistryProvider").
						Call(jen.Id("client"), jen.Id("b").Dot("IntrospectedClientProvider")),
					jen.Id("foundPrimary").Op("=").True(),
					jen.Break(),
				),
				// Synthesise from the introspected map when primary
				// names a static client without a surviving runtime
				// entry.
				jen.If(jen.Op("!").Id("foundPrimary")).Block(
					jen.Id("b").Dot("clientRegistryProvider").Op("=").Qual(bamlutilsPkg, "UpstreamClientRegistryProvider").
						Call(
							jen.Op("&").Qual(bamlutilsPkg, "ClientProperty").Values(
								jen.Dict{jen.Id("Name"): jen.Op("*").Id("clientRegistry").Dot("Primary")},
							),
							jen.Id("b").Dot("IntrospectedClientProvider"),
						),
				),
			),
			jen.Return(jen.Nil()),
		)
}

func emitFrameworkAdapterClientRegistryProvider(out *jen.File) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("ClientRegistryProvider").Params().String().
		Block(
			jen.Return(jen.Id("b").Dot("clientRegistryProvider")),
		)
}

func emitFrameworkAdapterOriginalClientRegistry(out *jen.File, bamlutilsPkg string) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("OriginalClientRegistry").Params().Op("*").Qual(bamlutilsPkg, "ClientRegistry").
		Block(
			jen.Return(jen.Id("b").Dot("originalClientRegistry")),
		)
}

func emitFrameworkAdapterSetTypeBuilder(out *jen.File, bamlutilsPkg, introspectedPkg string) {
	_ = introspectedPkg
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetTypeBuilder").Params(jen.Id("tb").Op("*").Qual(bamlutilsPkg, "TypeBuilder")).
		Error().
		Block(
			jen.If(jen.Id("b").Dot("TypeBuilderFactory").Op("==").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("adapter: TypeBuilderFactory not set"))),
			),
			jen.List(jen.Id("typeBuilder"), jen.Err()).Op(":=").
				Id("b").Dot("TypeBuilderFactory").Call(jen.Id("tb")),
			jen.If(jen.Err().Op("!=").Nil()).Block(jen.Return(jen.Err())),
			jen.Id("b").Dot("TypeBuilder").Op("=").Id("typeBuilder"),
			jen.Return(jen.Nil()),
		)
}

func emitFrameworkAdapterStreamModeAndLogger(out *jen.File, bamlutilsPkg string) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetStreamMode").Params(jen.Id("mode").Qual(bamlutilsPkg, "StreamMode")).
		Block(
			jen.Id("b").Dot("streamMode").Op("=").Id("mode"),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("StreamMode").Params().Qual(bamlutilsPkg, "StreamMode").
		Block(
			jen.Return(jen.Id("b").Dot("streamMode")),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetLogger").Params(jen.Id("logger").Qual(bamlutilsPkg, "Logger")).
		Block(
			jen.Id("b").Dot("logger").Op("=").Id("logger"),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("Logger").Params().Qual(bamlutilsPkg, "Logger").
		Block(
			jen.Return(jen.Id("b").Dot("logger")),
		)
}

func emitFrameworkAdapterMediaConstructors(out *jen.File, bamlutilsPkg string) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("NewMediaFromURL").
		Params(
			jen.Id("kind").Qual(bamlutilsPkg, "MediaKind"),
			jen.Id("url").String(),
			jen.Id("mimeType").Op("*").String(),
		).
		Params(jen.Any(), jen.Error()).
		Block(
			jen.If(jen.Id("b").Dot("MediaFactory").Op("==").Nil()).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("adapter: MediaFactory not set"))),
			),
			jen.Return(
				jen.Id("b").Dot("MediaFactory").Call(
					jen.Id("kind"),
					jen.Op("&").Id("url"),
					jen.Nil(),
					jen.Id("mimeType"),
				),
			),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("NewMediaFromBase64").
		Params(
			jen.Id("kind").Qual(bamlutilsPkg, "MediaKind"),
			jen.Id("base64").String(),
			jen.Id("mimeType").Op("*").String(),
		).
		Params(jen.Any(), jen.Error()).
		Block(
			jen.If(jen.Id("b").Dot("MediaFactory").Op("==").Nil()).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("adapter: MediaFactory not set"))),
			),
			jen.Return(
				jen.Id("b").Dot("MediaFactory").Call(
					jen.Id("kind"),
					jen.Nil(),
					jen.Op("&").Id("base64"),
					jen.Id("mimeType"),
				),
			),
		)
}

func emitFrameworkAdapterHTTPClient(out *jen.File, opts Options, llmhttpPkg string) {
	if opts.HasHTTPClient {
		out.Comment("SetHTTPClient injects a custom HTTP client for BuildRequest request execution.")
		out.Comment("When set, the generated router uses this client instead of llmhttp.DefaultClient")
		out.Comment("for both streaming and non-streaming paths.")
		out.Comment("Pass nil to revert to the default client.")
		out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
			Id("SetHTTPClient").Params(jen.Id("c").Op("*").Qual(llmhttpPkg, "Client")).
			Block(
				jen.Id("b").Dot("httpClient").Op("=").Id("c"),
			)

		out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
			Id("HTTPClient").Params().Op("*").Qual(llmhttpPkg, "Client").
			Block(
				jen.Return(jen.Id("b").Dot("httpClient")),
			)
		return
	}

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("HTTPClient").Params().Op("*").Qual(llmhttpPkg, "Client").
		Block(
			jen.Return(jen.Nil()),
		)
}

func emitFrameworkAdapterRoundRobinAdvancer(out *jen.File, bamlutilsPkg string) {
	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("SetRoundRobinAdvancer").Params(jen.Id("advancer").Qual(bamlutilsPkg, "RoundRobinAdvancer")).
		Block(
			jen.Id("b").Dot("rrAdvancer").Op("=").Id("advancer"),
		)

	out.Func().Params(jen.Id("b").Op("*").Id("BamlAdapter")).
		Id("RoundRobinAdvancer").Params().Qual(bamlutilsPkg, "RoundRobinAdvancer").
		Block(
			jen.Return(jen.Id("b").Dot("rrAdvancer")),
		)
}
