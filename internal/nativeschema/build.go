// Package nativeschema is the de-BAML P3 native static-schema builder (issue
// #586). It consumes ONLY the native BAML AST produced by
// bamlutils/bamlparser (slice 1) plus baml-rest's own descriptor model — no
// BAML runtime, no generated client. For every function that declares an
// output type it builds a per-function bamlutils/schemadescriptor.Bundle,
// lowers it with internal/schema.FromStaticDescriptor, validates it with
// Bundle.ValidateOutput, and emits its enum/class slices in BAML
// output-format reachability order. A function is SUPPORTED only when its
// WHOLE output graph lowers, validates, and orders; otherwise it is DECLINED
// fail-closed with a stable reason and no descriptor is emitted for it.
//
// This package was extracted from cmd/introspect (slice 2 lived there) so the
// same builder can be driven both by the production introspect pipeline and by
// the byte-parity integration harness (slice 3), which needs to build a
// descriptor from parsed .baml bytes and render it. cmd/introspect calls
// BuildStaticSchemas and stores the results on *bamlConfig; nothing is wired
// into the request/response runtime.
//
// EMBED-REGEN / BUILD SITE (read before adding a file here): cmd/introspect
// imports this package, and the container build compiles cmd/introspect in
// PACKAGE form (`go run ./cmd/introspect`, see cmd/build/build.sh), so every
// import of cmd/introspect must ship in the customer/container embed.FS. When
// you add a .go file to this package, run `go run cmd/embed/main.go` and commit
// the regenerated root embed.go so the new file is embedded; otherwise the
// container introspection step fails to compile a sibling symbol as
// "undefined". See #586 and the slice-2 build fix.
//
// SCOPE (slice 6 — @skip + streaming decline, on top of slices 1-5; the FINAL
// P3-static slice):
//   - Lower primitives, named class/enum refs, non-recursive aliases (inlined),
//     lists, maps, optionals, unions, and string/int/bool literals, and emit
//     Enums/Classes in BAML output-format reachability order (see
//     [descriptorBuilder.reorderByReachability]); FromStaticDescriptor preserves
//     order verbatim, so the STORED descriptor must already be in BAML order for
//     the renderer to match byte-for-byte.
//   - Detect RECURSIVE CLASSES (class-reference cycles, an SCC over
//     class->field->class edges) and structural RECURSIVE ALIASES (alias cycles
//     that recurse THROUGH a list/map) via recursion.go's Tarjan SCC, mirroring
//     BAML's finite_recursive_cycles / recursive_alias_cycles. A recursive class
//     is emitted normally in Classes but listed in Bundle.RecursiveClasses (so
//     the renderer hoists it as BAML does); a structural recursive alias is
//     emitted as a Bundle.StructuralRecursiveAliases entry and referenced via a
//     TypeRecursiveAlias node. Both slices are ordered by reachability
//     (RecursiveClasses in BAML cycle-hit order, aliases in first-pop order).
//     A DIRECT/degenerate alias cycle (not mediated by a list/map, e.g.
//     `type A = A` / `type A = A | int` / `type A = A?`) is INVALID and DECLINES
//     fail-closed; a multi-alias structural cycle DECLINES too (single-alias
//     structural cycles only); recursion whose output graph reaches media/tuple
//     still DECLINES via ValidateOutput.
//   - @skip (slice 6, D11): a @skip-marked class field / enum value is DROPPED
//     exactly as BAML does (find_existing_class_field / find_enum_value return
//     Ok(None) before the definition is built), so it never renders and its type
//     is never reachable. This replaces the slice-2..5 @skip DECLINE. See
//     ensureClass / ensureEnum / hasSkipAttribute.
//   - STATIC STREAMING (slice 6, D12): stays a parity-DECLINE (BAML fallback).
//     The builder emits ONLY the NonStreaming descriptor. BAML never renders a
//     streaming output_format into the prompt: the Jinja RenderContext carries a
//     single output_format populated with the NON-streaming definitions
//     (engine/.../prompt_renderer/mod.rs render_prompt, line 149), while the
//     partialized/streaming OutputFormatContent (to_streaming_type partialize)
//     feeds ONLY the partial-response JSONish parser (mod.rs parse, allow_partials
//     branch). So `{{ ctx.output_format }}` resolves to the non-streaming block
//     for streaming AND non-streaming calls alike — there is NO streaming
//     output_format artifact for the byte-parity oracle to compare against.
//     Streaming partialization belongs to the response-PARSING subsystem
//     (internal/debaml partial coercion), not this output_format builder; see
//     #586 D12. @@dynamic recursive overlays stay DECLINED.
//   - @@dynamic (slice 6): stays fail-closed DECLINED. It is tied to runtime
//     TypeBuilder / dynamic schema mutation, NOT a static-schema feature, so it
//     is out of P3-static scope until the native runtime front-end owns it.
//   - Lower @alias -> Name.Alias and @description -> Description on class fields,
//     enum values, and class/enum-level block attributes.
//   - Lower @assert/@check -> opaque [schemadescriptor.Constraint] carrying the
//     level, optional label, and verbatim Jinja expression text, NEVER
//     evaluated. Repeated constraints are supported. A field constraint
//     reassociates onto the field's TYPE
//     (Type.Meta.Constraints, exactly as BAML's reassociate_type_attributes
//     does); a class/enum block constraint goes on ClassDef.Constraints /
//     EnumDef.Constraints.
//   - Lower @stream.done/@stream.not_null/@stream.with_state into the descriptor
//     streaming fields exactly as BAML models them: a field stream attribute
//     reassociates onto the field's TYPE (Type.Meta.Stream), a class-level
//     @@stream.* goes on ClassDef.Stream. Static STREAMING partialization stays
//     a parity-DECLINE (D12 below); the builder only carries these flags on the
//     FINAL (non-streaming) descriptor.
//   - Validate STRUCTURALLY via FromStaticDescriptor + ValidateOutput. Constraints
//     and streaming flags are metadata: they do NOT change the rendered
//     ctx.output_format (the parity harness proves this byte-for-byte).
//
// Lax-vs-BAML / decline decisions logged here are also recorded on #586:
//
//	D1. A reachable output node may carry @alias/@description (everywhere),
//	    @assert/@check, and @stream.* (where the descriptor can represent them),
//	    and a class field / enum value may carry @skip (which DROPS it — D11).
//	    ANY OTHER attribute (@@dynamic or an unknown attribute) DECLINES the
//	    function fail-closed rather than being silently dropped. Enum-LEVEL
//	    @@check/@@assert DO lower (to EnumDef.Constraints, as class @@check/@@assert
//	    lower to ClassDef.Constraints); but a constraint or stream attribute on a
//	    node with no descriptor home for it DECLINES: any @check/@assert/@stream.*
//	    on an enum VALUE (EnumValue has neither field), and an enum-LEVEL @@stream.*
//	    (EnumDef has no streaming field). An attribute on a NESTED type node (e.g.
//	    the inner `int` of `map<string, int @check>`, or a bare union variant)
//	    declines too: reassociation of nested / union-variant attributes is not
//	    performed — this slice handles only the field's OUTERMOST type, plus block
//	    and enum-value metadata.
//	D2. media and tuple output are lowered FAITHFULLY into the descriptor and
//	    left for ValidateOutput to reject (it errors on tuple/arrow/top/media),
//	    so the single authority on output-legality stays internal/schema.
//	D3. duplicate class/enum/alias names are POISONED in the semantic index; a
//	    function whose output graph reaches a poisoned name declines, while
//	    functions that never reach it still build. Per-method fail-closed.
//	D4. duplicate function names are last-wins (a later declaration's build
//	    outcome overrides an earlier one), matching the config walker.
//	D5. @alias/@description arguments must be a SINGLE plain string (a quoted
//	    string or a raw string). A missing, multi-valued, or non-string argument
//	    (e.g. a Jinja description) declines the function — the description is
//	    rendered verbatim, so an argument it cannot reproduce byte-for-byte fails
//	    closed rather than guessing.
//	D6. /// doc comments are CAPTURED-as-no-op. BAML lowers `///` to a docstring
//	    that its output_format renderer NEVER reads — only @description renders,
//	    confirmed against the v0.223 oracle (a doc-commented node byte-matches the
//	    same node without the comment). So a `///` doc comment contributes NOTHING
//	    to the descriptor, and a doc-commented class/field/enum/enum-value now
//	    builds SUPPORTED. This CORRECTS slice 3, which fail-closed DECLINED doc
//	    comments on the (then-untested) assumption they rendered as a description.
//	    @description always wins over a doc comment because it is the sole source;
//	    the doc comment never overrides or concatenates.
//	D7. @assert/@check are stored OPAQUELY. The expression is the verbatim inner
//	    text between {{ and }} (braces excluded, not trimmed) — matching BAML's
//	    stored JinjaExpression content except we skip BAML's backslash-doubling
//	    normalization (the value is never parsed or evaluated; evaluation is out
//	    of scope, deferred). The optional label is the bare leading identifier. A
//	    shape that cannot be split into (label?, {{expr}}) declines fail-closed.
//	D8. @stream.* map, exactly as BAML's IR does: a field @stream.done/not_null/
//	    with_state reassociates onto the field TYPE -> Type.Meta.Stream.{Done,
//	    Needed,State}; a class @@stream.* -> ClassDef.Stream. ClassField.
//	    StreamingNeeded is BAML's override-derived per-field bool (defaults false)
//	    and is NOT set by a static attribute, so the builder leaves it unset.
//	D9. type-alias RHS attributes (BAML permits @check/@assert on aliases) remain
//	    DECLINED: alias constraint inlining is deferred (not in the slice-4
//	    corpus), so an attributed alias fails closed rather than being dropped.
//	D10. RECURSION (slice 5). Recursive classes + structural recursive aliases
//	    (cycles through a list/map) are lowered and hoisted in BAML order (see
//	    recursion.go for the Tarjan detection and SCOPE above). The declines are
//	    fail-closed and never approximate: (a) a DIRECT/degenerate alias cycle
//	    (not mediated by a list/map — BAML rejects it at validation) declines;
//	    (b) a MULTI-alias structural cycle declines (only single-alias structural
//	    cycles, the JSON-value shape, are lowered here — a larger cycle's hoist
//	    order is not oracle-proven in this slice); (c) recursion whose output
//	    graph reaches media/tuple declines via ValidateOutput (a recursive class
//	    is legal, but media/tuple output is not); (d) @@dynamic recursive
//	    overlays and streaming partialization of recursive types stay DECLINED
//	    (@skip on a recursive-class field is DROPPED as anywhere — D11).
//	    recursion.go reproduces BAML's finite_recursive_cycles (class SCC) +
//	    recursive_alias_cycles (alias SCC through list/map) + non-structural alias
//	    graph (invalid-cycle detection); the class dependency graph is built from
//	    ALL fields (skip-inclusive, matching BAML's finalize_dependencies), so
//	    @skip does not change recursion classification.
//	D11. @skip (slice 6) — IMPLEMENTED. A @skip-marked class field
//	    (find_existing_class_field) or enum value (find_enum_value) is DROPPED
//	    before the definition is built: it does not render, and its type is never
//	    collected/reachable, so a class/enum reachable ONLY through a skipped
//	    field disappears from ctx.output_format — exactly as BAML does. skip is a
//	    field/value attribute taking no arguments; skip is checked first, so a
//	    skipped field's other attributes are irrelevant. This replaces the
//	    slice-2..5 @skip DECLINE. Site: ensureClass / ensureEnum /
//	    hasSkipAttribute. Proven byte-exact by the ParitySkip oracle fixture.
//	D12. STATIC STREAMING (slice 6) — parity-DECLINED, kept as BAML fallback.
//	    The builder produces ONLY the NonStreaming descriptor (Bundle.Stream is
//	    never set true, no class is emitted under StreamingMode::Streaming).
//	    Reason (not an approximation dodge — an architectural fact): BAML NEVER
//	    renders a streaming output_format into the prompt. render_prompt builds a
//	    Jinja RenderContext whose single output_format field is the NON-streaming
//	    definitions (prompt_renderer/mod.rs:149); the partialized/streaming
//	    OutputFormatContent (output.to_streaming_type(ir), render_output_format.rs
//	    relevant_data_models partialize=true) is built but consumed ONLY by the
//	    partial-response JSONish parser (mod.rs parse(), allow_partials branch),
//	    never injected into `{{ ctx.output_format }}`. So a streaming call's
//	    prompt carries the identical non-streaming output_format, and the static
//	    output_format byte-parity oracle has NO streaming artifact to compare
//	    against — "streaming output_format parity" is unprovable because the thing
//	    it would prove is never emitted. Streaming partialization affects only
//	    response PARSING (internal/debaml partial coercion), a separate subsystem
//	    out of this output_format builder's scope. Implementing partialization
//	    here would produce a descriptor whose streaming render BAML never emits,
//	    so per the parity-decline discipline it is deferred, not approximated. A
//	    decline-boundary test (TestBuildStaticSchemasStreamingNotEmitted) pins
//	    that the builder emits only the non-streaming descriptor.
package nativeschema

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// SourceFile wraps a parsed .baml [bamlparser.File]. It is a struct (rather than
// a bare *File) so the builder's input contract can grow without churning every
// caller.
//
// It no longer carries the raw source bytes. Slice 3 threaded the source in to
// DETECT `///` doc comments (which the shared lexer elides) and decline them;
// slice 4 confirmed against the v0.223 oracle that BAML's output_format renderer
// never reads doc comments (only @description renders — see #586 D6), so a
// doc-commented node builds byte-identically without any source inspection.
//
// Path is the .baml file path the File was parsed from. BuildStaticSchemas
// ignores it (schema reachability is path-independent), but the prompt
// descriptor builder (prompt.go) uses it for stable macro source-order
// diagnostics and to stamp each retained template string's SourcePath. It is
// optional: an empty Path does not change any build outcome.
type SourceFile struct {
	File *bamlparser.File
	Path string
}

// BuildStaticSchemas builds a native static output-schema descriptor for every
// function that has a parsed return type, from the already-parsed .baml files.
// It returns two always-non-nil maps keyed by function name: bundles for the
// SUPPORTED functions (whose whole output graph lowered, validated, and
// ordered) and declines for the unsupported ones (a stable reason string). A
// declined function never appears in bundles, and a supported one never in
// declines.
//
// It never errors: an output graph that cannot be represented faithfully is a
// per-function decline, not a pipeline failure, so this pass is safe to run on
// the production introspect path without changing any config extraction.
func BuildStaticSchemas(files []SourceFile) (map[string]sd.Bundle, map[string]string) {
	idx := buildSchemaTypeIndex(files)

	bundles := make(map[string]sd.Bundle)
	declines := make(map[string]string)

	for _, sf := range files {
		f := sf.File
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			fn := it.Function
			if fn == nil || fn.Name == "" {
				continue
			}
			bundle, err := buildFunctionDescriptor(idx, fn)
			if err != nil {
				// D4: last-wins on duplicate function names — a later
				// declaration's decline supersedes an earlier success.
				declines[fn.Name] = err.Error()
				delete(bundles, fn.Name)
				continue
			}
			bundles[fn.Name] = bundle
			delete(declines, fn.Name)
		}
	}

	return bundles, declines
}

// schemaTypeIndex classifies every top-level type name (class/enum/alias)
// across all parsed files BEFORE any body is lowered, mirroring
// internal/schema.FromDynamicOutputSchema's classify-first approach so that a
// reference resolves regardless of declaration order or cross-file layout.
//
// A name declared more than once (in one category or across categories) is
// recorded in ambiguous; the builder declines any function that reaches a
// poisoned name (D3) rather than silently last-wins-ing type definitions.
type schemaTypeIndex struct {
	classes   map[string]*bamlparser.TypeBlock
	enums     map[string]*bamlparser.TypeBlock
	aliases   map[string]*bamlparser.TypeAlias
	ambiguous map[string]struct{}

	// classDeclOrder and aliasDeclOrder record canonical declaration order
	// (file order, then item order within a file) for classes and aliases. They
	// are the stable node-id spaces the recursion analysis (recursion.go) feeds
	// into Tarjan's SCC, reproducing BAML's TypeExpId / TypeAliasId ordering so
	// recursive-class and structural-recursive-alias cycles hoist in BAML order.
	// A duplicate (ambiguous) name is recorded once, at its first declaration.
	classDeclOrder []string
	aliasDeclOrder []string

	// rec is the lazily-computed recursion classification over the whole
	// project (recursive classes, structural recursive aliases, invalid alias
	// cycles). See [schemaTypeIndex.recursion].
	rec *recursionInfo
}

func (i *schemaTypeIndex) isAmbiguous(name string) bool {
	_, ok := i.ambiguous[name]
	return ok
}

// buildSchemaTypeIndex classifies all class/enum/alias names first. `test`
// blocks are not schema definitions and are skipped, matching the parser's
// metadata-only posture for them.
func buildSchemaTypeIndex(files []SourceFile) *schemaTypeIndex {
	idx := &schemaTypeIndex{
		classes:   make(map[string]*bamlparser.TypeBlock),
		enums:     make(map[string]*bamlparser.TypeBlock),
		aliases:   make(map[string]*bamlparser.TypeAlias),
		ambiguous: make(map[string]struct{}),
	}

	counts := make(map[string]int)
	seenClass := make(map[string]struct{})
	seenAlias := make(map[string]struct{})
	for _, sf := range files {
		f := sf.File
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			switch {
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "class":
				counts[it.TypeBlock.Name]++
				idx.classes[it.TypeBlock.Name] = it.TypeBlock
				if _, ok := seenClass[it.TypeBlock.Name]; !ok {
					seenClass[it.TypeBlock.Name] = struct{}{}
					idx.classDeclOrder = append(idx.classDeclOrder, it.TypeBlock.Name)
				}
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "enum":
				counts[it.TypeBlock.Name]++
				idx.enums[it.TypeBlock.Name] = it.TypeBlock
			case it.TypeAlias != nil && it.TypeAlias.Name != "":
				counts[it.TypeAlias.Name]++
				idx.aliases[it.TypeAlias.Name] = it.TypeAlias
				if _, ok := seenAlias[it.TypeAlias.Name]; !ok {
					seenAlias[it.TypeAlias.Name] = struct{}{}
					idx.aliasDeclOrder = append(idx.aliasDeclOrder, it.TypeAlias.Name)
				}
			}
		}
	}
	for name, c := range counts {
		if c > 1 {
			idx.ambiguous[name] = struct{}{}
		}
	}
	return idx
}

// buildFunctionDescriptor builds and validates the descriptor for one
// function's output type. It returns a decline error the moment any reachable
// construct cannot be represented faithfully; the resulting descriptor is only
// returned once FromStaticDescriptor and ValidateOutput both accept it and its
// definitions have been reordered into BAML output-format order.
func buildFunctionDescriptor(idx *schemaTypeIndex, fn *bamlparser.FunctionBlock) (sd.Bundle, error) {
	if fn.Return == nil {
		return sd.Bundle{}, fmt.Errorf("function %q has no parsed return type", fn.Name)
	}

	b := newDescriptorBuilder(idx)
	target, err := b.lowerType(fn.Return)
	if err != nil {
		return sd.Bundle{}, err
	}

	bundle := sd.Bundle{
		Version: sd.Version,
		Method:  fn.Name,
		Target:  target,
		// Discovery order for now; reorderByReachability below rewrites these
		// into BAML output-format order once the graph is lowered + validated.
		Enums:                      b.orderedEnums(),
		Classes:                    b.orderedClasses(),
		RecursiveClasses:           b.reachableRecursiveClasses(),
		StructuralRecursiveAliases: b.orderedStructuralAliases(),
	}

	// Lower into the internal model (fails closed on unresolved refs, malformed
	// payloads, duplicate rendered names, invalid map keys, null-as-variant),
	// then enforce the output profile (rejects tuple/arrow/top/media).
	internal, err := schema.FromStaticDescriptor(bundle)
	if err != nil {
		return sd.Bundle{}, err
	}
	if err := internal.ValidateOutput(); err != nil {
		return sd.Bundle{}, err
	}

	// SLICE 3 ORDERING: rewrite the enum/class slices into BAML's
	// render_output_format reachability order (reverse-of-reference DFS from the
	// target). FromStaticDescriptor preserves the descriptor's slice order
	// verbatim and never reruns reachability, so the STORED descriptor must
	// already be in BAML order for internal/schema/outputformat.Render to match
	// BAML byte-for-byte. The order is computed by the SAME orderByReachability
	// the dynamic path uses (via Bundle.ReachableOrder), so the two paths cannot
	// drift. Validation above is order-independent, so reordering a validated
	// bundle keeps it valid.
	b.reorderByReachability(&bundle, internal)
	return bundle, nil
}

// descriptorBuilder lowers one function's output graph. It is single-use: a
// fresh builder is created per function so the recursion-guard and collected
// definition sets are scoped to that function's reachable graph.
type descriptorBuilder struct {
	index *schemaTypeIndex
	// rec is the project-wide recursion classification (recursion.go): which
	// classes are recursive, which aliases are structural recursive vs invalid.
	// Shared across all functions (it is a whole-project property).
	rec *recursionInfo

	// classes/enums are the collected definitions (the reachable set), keyed by
	// canonical name and deduplicated. classOrder/enumOrder record discovery
	// order (dependencies first, post-order); reorderByReachability rewrites the
	// emitted order into BAML output-format order.
	classes    map[string]sd.ClassDef
	classOrder []string
	enums      map[string]sd.EnumDef
	enumOrder  []string

	// structuralAliases collects the lowered target of each STRUCTURAL recursive
	// alias reached from the output (keyed by canonical name);
	// structuralAliasOrder records discovery order. References to a structural
	// alias lower to a TypeRecursiveAlias node (never inlined), and its target is
	// lowered once into this set — reproducing BAML's structural_recursive_aliases
	// map. reorderByReachability rewrites the emitted order into BAML order.
	structuralAliases    map[string]sd.Type
	structuralAliasOrder []string

	// classVisiting / aliasVisiting are the active DFS paths. classVisiting
	// re-entry is a class cycle (the class is recursive: the reference is emitted
	// as a TypeClass and the cycle stops, with recursive_classes populated from
	// the analysis). aliasVisiting backstops the INLINING of non-recursive
	// aliases (structural/invalid aliases never inline), catching a detection
	// inconsistency rather than looping.
	classVisiting map[string]bool
	aliasVisiting map[string]bool
}

func newDescriptorBuilder(idx *schemaTypeIndex) *descriptorBuilder {
	return &descriptorBuilder{
		index:             idx,
		rec:               idx.recursion(),
		classes:           make(map[string]sd.ClassDef),
		enums:             make(map[string]sd.EnumDef),
		structuralAliases: make(map[string]sd.Type),
		classVisiting:     make(map[string]bool),
		aliasVisiting:     make(map[string]bool),
	}
}

func (b *descriptorBuilder) orderedClasses() []sd.ClassDef {
	if len(b.classOrder) == 0 {
		return nil
	}
	out := make([]sd.ClassDef, 0, len(b.classOrder))
	for _, name := range b.classOrder {
		out = append(out, b.classes[name])
	}
	return out
}

func (b *descriptorBuilder) orderedEnums() []sd.EnumDef {
	if len(b.enumOrder) == 0 {
		return nil
	}
	out := make([]sd.EnumDef, 0, len(b.enumOrder))
	for _, name := range b.enumOrder {
		out = append(out, b.enums[name])
	}
	return out
}

// orderedStructuralAliases returns the collected structural recursive alias
// definitions in discovery order. reorderByReachability rewrites the emitted
// order into BAML output-format order once the graph is validated.
func (b *descriptorBuilder) orderedStructuralAliases() []sd.RecursiveAliasDef {
	if len(b.structuralAliasOrder) == 0 {
		return nil
	}
	out := make([]sd.RecursiveAliasDef, 0, len(b.structuralAliasOrder))
	for _, name := range b.structuralAliasOrder {
		out = append(out, sd.RecursiveAliasDef{Name: name, Target: b.structuralAliases[name]})
	}
	return out
}

// reachableRecursiveClasses returns the recursive classes reachable from the
// output, in discovery order. Only membership matters here — Validate checks
// each name resolves to a class — because reorderByReachability rewrites the
// slice into BAML recursive_classes order after validation.
func (b *descriptorBuilder) reachableRecursiveClasses() []string {
	var out []string
	for _, name := range b.classOrder {
		if b.rec.recursiveClass[name] {
			out = append(out, name)
		}
	}
	return out
}

// reorderByReachability rewrites bundle.Enums, bundle.Classes,
// bundle.RecursiveClasses, and bundle.StructuralRecursiveAliases into the BAML
// output-format hoist order reported by the validated internal bundle. The
// builder collected exactly the reachable definition set (it only lowers what a
// reachable reference names), so ReachableOrder returns each collected name once
// and in BAML order; every returned name therefore resolves in the builder's
// by-name maps. The builder never emits a streaming-MODE class (streaming is
// carried as metadata on the final descriptor, not as a separate streaming-mode
// class — static streaming partialization is a parity-DECLINE, see #586 D12), so
// a class key's mode is always NonStreaming and looking classes up by canonical
// name is sufficient.
func (b *descriptorBuilder) reorderByReachability(bundle *sd.Bundle, internal *schema.Bundle) {
	enumNames, classKeys, aliasNames := internal.ReachableOrder()

	if len(enumNames) == 0 {
		bundle.Enums = nil
	} else {
		enums := make([]sd.EnumDef, 0, len(enumNames))
		for _, name := range enumNames {
			if def, ok := b.enums[name]; ok {
				enums = append(enums, def)
			}
		}
		bundle.Enums = enums
	}

	if len(classKeys) == 0 {
		bundle.Classes = nil
	} else {
		classes := make([]sd.ClassDef, 0, len(classKeys))
		for _, key := range classKeys {
			if def, ok := b.classes[key.Name]; ok {
				classes = append(classes, def)
			}
		}
		bundle.Classes = classes
	}

	bundle.RecursiveClasses = b.orderRecursiveClasses(classKeys)
	bundle.StructuralRecursiveAliases = b.orderStructuralAliases(aliasNames)
}

// orderRecursiveClasses reproduces BAML's recursive_classes IndexSet order. In
// relevant_data_models BAML extends recursive_classes with a class's WHOLE cycle
// the first time any cycle member is popped, deduping via the IndexSet — so the
// order is: walk the classes in reachability pop order (classKeys, which is
// append-on-pop), and for each recursive class emit its cycle members (Tarjan
// order) once. Every cycle member is reachable (an SCC is mutually reachable),
// so all appear in classKeys and were collected. Returns nil when no recursive
// class is reachable.
func (b *descriptorBuilder) orderRecursiveClasses(classKeys []schema.ClassKey) []string {
	emitted := make(map[string]bool)
	var out []string
	for _, key := range classKeys {
		for _, member := range b.rec.classCycleOf[key.Name] {
			if !emitted[member] {
				emitted[member] = true
				out = append(out, member)
			}
		}
	}
	return out
}

// orderStructuralAliases reproduces BAML's structural_recursive_aliases IndexMap
// order: the discovery (first-pop) order of the reachable structural recursive
// aliases, which ReachableOrder returns as aliasNames. The builder supports only
// single-alias structural cycles (resolveAlias declines larger ones), so each
// alias is its own cycle and the discovery order is the final order. Returns nil
// when no structural recursive alias is reachable.
func (b *descriptorBuilder) orderStructuralAliases(aliasNames []string) []sd.RecursiveAliasDef {
	if len(aliasNames) == 0 {
		return nil
	}
	out := make([]sd.RecursiveAliasDef, 0, len(aliasNames))
	for _, name := range aliasNames {
		if target, ok := b.structuralAliases[name]; ok {
			out = append(out, sd.RecursiveAliasDef{Name: name, Target: target})
		}
	}
	return out
}

// lowerType lowers one parsed TypeExpr into a descriptor Type, collecting any
// reachable class/enum definitions along the way. It returns a decline error
// for any construct this slice cannot represent faithfully.
func (b *descriptorBuilder) lowerType(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t == nil {
		return sd.Type{}, fmt.Errorf("missing type expression")
	}

	// D1: attributes on a type node reaching here decline the function. A class
	// field's / enum value's own metadata (@alias/@description) and its
	// reassociated type metadata (@check/@assert/@stream.*) are consumed and the
	// field's OUTERMOST attributes are STRIPPED by the member handler before it
	// calls lowerFieldType. So by the time lowerType sees a node that still
	// carries attributes, they are on a NESTED type node (e.g. `map<string,
	// int @check>` or a bare union variant) — a shape this slice does not
	// reassociate — and it declines fail-closed rather than silently dropping.
	if len(t.Attributes) > 0 {
		return sd.Type{}, declineAttribute(t.Attributes[0])
	}

	switch t.Kind {
	case bamlparser.KindUnsupported:
		reason := t.Reason
		if reason == "" {
			reason = "unsupported type"
		}
		return sd.Type{}, fmt.Errorf("%s", reason)

	case bamlparser.KindPrimitive:
		return lowerPrimitive(t.Primitive)

	case bamlparser.KindMedia:
		// D2: lowered faithfully; ValidateOutput rejects media output.
		return lowerMedia(t.Media)

	case bamlparser.KindNameRef:
		return b.lowerNameRef(t)

	case bamlparser.KindList:
		return b.lowerList(t)

	case bamlparser.KindMap:
		return b.lowerMap(t)

	case bamlparser.KindUnion:
		return b.lowerUnion(t)

	case bamlparser.KindLiteral:
		return lowerLiteral(t)

	case bamlparser.KindTuple:
		// D2: lowered faithfully; ValidateOutput rejects tuple output.
		return b.lowerTuple(t)

	case bamlparser.KindGroup:
		// A parenthesized single type is not a runtime node; unwrap it. Any
		// group-level attribute was already rejected by the D1 check above.
		return b.lowerType(t.Inner)

	default:
		return sd.Type{}, fmt.Errorf("unhandled type kind %d", t.Kind)
	}
}

// lowerFieldType lowers a class-field type after its field-level metadata
// (@alias/@description) and its reassociated type metadata (@check/@assert/
// @stream.*) have been consumed by the caller ([extractFieldMeta], which
// declines any other attribute). The remaining attributes on the OUTERMOST node
// are therefore all recognized metadata; they are dropped via a shallow copy
// (never mutating the shared AST, which other functions also reference) before
// lowering, and the caller re-attaches the constraints/streaming to the lowered
// type's Meta. Nested-node attributes are untouched and still decline in
// lowerType.
func (b *descriptorBuilder) lowerFieldType(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t == nil {
		return sd.Type{}, fmt.Errorf("missing type expression")
	}
	if len(t.Attributes) > 0 {
		cp := *t
		cp.Attributes = nil
		return b.lowerType(&cp)
	}
	return b.lowerType(t)
}

// lowerPrimitive maps a bare primitive identifier to its descriptor primitive.
func lowerPrimitive(prim string) (sd.Type, error) {
	switch prim {
	case "string":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}, nil
	case "int":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveInt}, nil
	case "float":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveFloat}, nil
	case "bool":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveBool}, nil
	case "null":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveNull}, nil
	default:
		return sd.Type{}, fmt.Errorf("unknown primitive %q", prim)
	}
}

// lowerMedia maps a media identifier to a descriptor media primitive. The
// resulting type lowers fine but is rejected by ValidateOutput (media is not a
// usable output type), which is the intended decline path (D2).
func lowerMedia(media string) (sd.Type, error) {
	var kind sd.MediaKind
	switch media {
	case "image":
		kind = sd.MediaImage
	case "audio":
		kind = sd.MediaAudio
	case "pdf":
		kind = sd.MediaPDF
	case "video":
		kind = sd.MediaVideo
	default:
		return sd.Type{}, fmt.Errorf("unknown media type %q", media)
	}
	return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: kind}, nil
}

// lowerList expands a parsed KindList (element + dimension count) into nested
// descriptor TypeList nodes, one per dimension. An optional array suffix was
// already lowered by the parser into a nullable union around the whole list,
// so it is not this function's concern.
func (b *descriptorBuilder) lowerList(t *bamlparser.TypeExpr) (sd.Type, error) {
	elem, err := b.lowerType(t.Elem)
	if err != nil {
		return sd.Type{}, err
	}
	dims := t.Dims
	if dims < 1 {
		dims = 1
	}
	result := elem
	for i := 0; i < dims; i++ {
		child := result
		result = sd.Type{Kind: sd.TypeList, Elem: &child}
	}
	return result, nil
}

func (b *descriptorBuilder) lowerMap(t *bamlparser.TypeExpr) (sd.Type, error) {
	key, err := b.lowerType(t.Key)
	if err != nil {
		return sd.Type{}, err
	}
	val, err := b.lowerType(t.Value)
	if err != nil {
		return sd.Type{}, err
	}
	// ValidateOutput enforces BAML-legal map key shapes (string primitive,
	// enum, string literal, or a non-nullable union of string literals).
	return sd.Type{Kind: sd.TypeMap, Key: &key, Value: &val}, nil
}

// lowerUnion lowers every union member and then NORMALIZES the lowered result
// so the descriptor is BAML-equivalent regardless of how nesting arose. A union
// member is not always a bare AST child: a non-recursive alias or a
// parenthesized group can lower into its own TypeUnion, and an alias can lower
// to a bare null. Appending those verbatim would emit a nested union or a
// null-primitive variant (which FromStaticDescriptor rejects), so normalization
// flattens nested unions and folds ALL nullability sources (a trailing `?`, an
// explicit `null` member, a group?/alias-to-null) into a single Nullable flag —
// exactly like the dynamic builder's makeUnion/simplifyUnion path
// (internal/schema/build.go). See #586 (D1/union-normalization).
func (b *descriptorBuilder) lowerUnion(t *bamlparser.TypeExpr) (sd.Type, error) {
	choices := make([]sd.Type, 0, len(t.Variants))
	for _, v := range t.Variants {
		// Lower every member — including a bare `null` (which normalization
		// hoists into Nullable) and an attributed null (which lowerType's D1
		// guard declines). No special-casing here keeps the null-folding in one
		// place (normalizeDescriptorUnion).
		lowered, err := b.lowerType(v)
		if err != nil {
			return sd.Type{}, err
		}
		choices = append(choices, lowered)
	}
	return normalizeDescriptorUnion(choices, t.Nullable), nil
}

// normalizeDescriptorUnion mirrors internal/schema.simplifyUnion for descriptor
// types: it flattens nested (constraint-free) unions, deduplicates variants
// preserving first-seen order, hoists every null into the Nullable marker
// (never a null-primitive variant), collapses an all-null union to the null
// primitive, and collapses a single non-null variant to that variant unless the
// union is also nullable (then it stays an optional-of-one).
//
// Slice 3 never attaches constraints/streaming metadata to a union node (any
// attribute-bearing node is declined by D1), so every lowered union here is
// metadata-free and flattens fully — the constraint-carrying-union special case
// BAML preserves cannot arise yet.
func normalizeDescriptorUnion(choices []sd.Type, nullable bool) sd.Type {
	flattened := make([]sd.Type, 0, len(choices))
	for _, c := range choices {
		flattened = append(flattened, flattenDescriptorUnion(c)...)
	}
	if nullable {
		flattened = append(flattened, descriptorNull())
	}

	hasNull := false
	variants := make([]sd.Type, 0, len(flattened))
	for _, t := range flattened {
		if isDescriptorNull(t) {
			hasNull = true
			continue
		}
		if containsDescriptorType(variants, t) {
			continue
		}
		variants = append(variants, t)
	}

	switch len(variants) {
	case 0:
		return descriptorNull()
	case 1:
		if hasNull {
			v := variants[0]
			return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{v}, Nullable: true}}
		}
		return variants[0]
	default:
		return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: variants, Nullable: hasNull}}
	}
}

// flattenDescriptorUnion replaces a constraint-free union with its
// (recursively flattened) variants plus a trailing null sentinel when the union
// is nullable; every other type flattens to itself. Mirrors
// internal/schema.flattenForUnion.
func flattenDescriptorUnion(t sd.Type) []sd.Type {
	if t.Kind == sd.TypeUnion && t.Union != nil && len(t.Meta.Constraints) == 0 {
		out := make([]sd.Type, 0, len(t.Union.Variants)+1)
		for _, v := range t.Union.Variants {
			out = append(out, flattenDescriptorUnion(v)...)
		}
		if t.Union.Nullable {
			out = append(out, descriptorNull())
		}
		return out
	}
	return []sd.Type{t}
}

// descriptorNull is the canonical null primitive used as the nullability
// sentinel during union flattening.
func descriptorNull() sd.Type {
	return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveNull}
}

// isDescriptorNull reports whether t is the null primitive (BAML
// TypeGeneric::is_null: only Primitive(Null), not an optional union).
func isDescriptorNull(t sd.Type) bool {
	return t.Kind == sd.TypePrimitive && t.Primitive == sd.PrimitiveNull
}

// containsDescriptorType reports whether ts already holds a type structurally
// equal to t. Mirrors internal/schema.containsType: BAML's simplify()
// deduplicates with full structural equality, which for the metadata-free
// slice-3 path reduces to a deep comparison of the descriptor type trees.
func containsDescriptorType(ts []sd.Type, t sd.Type) bool {
	for i := range ts {
		if reflect.DeepEqual(ts[i], t) {
			return true
		}
	}
	return false
}

func (b *descriptorBuilder) lowerTuple(t *bamlparser.TypeExpr) (sd.Type, error) {
	items := make([]sd.Type, 0, len(t.Items))
	for _, it := range t.Items {
		lowered, err := b.lowerType(it)
		if err != nil {
			return sd.Type{}, err
		}
		items = append(items, lowered)
	}
	return sd.Type{Kind: sd.TypeTuple, Items: items}, nil
}

// lowerLiteral lowers a string/int/bool literal type. Float literals are
// declined: BAML v0.223 rejects them and the descriptor model has no float
// literal kind, so they cannot be represented faithfully.
func lowerLiteral(t *bamlparser.TypeExpr) (sd.Type, error) {
	switch t.LiteralKind {
	case "string":
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: t.LiteralValue}}, nil
	case "int":
		n, err := strconv.ParseInt(t.LiteralValue, 10, 64)
		if err != nil {
			return sd.Type{}, fmt.Errorf("invalid int literal %q: %w", t.LiteralValue, err)
		}
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralInt, Int: n}}, nil
	case "bool":
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralBool, Bool: t.LiteralValue == "true"}}, nil
	case "float":
		return sd.Type{}, fmt.Errorf("float literal types are not supported (BAML v0.223 rejects them)")
	default:
		return sd.Type{}, fmt.Errorf("unknown literal kind %q", t.LiteralKind)
	}
}

// lowerNameRef resolves a named reference against the semantic index and lowers
// the referenced definition (collecting classes/enums, inlining non-recursive
// aliases). Path/namespaced identifiers, ambiguous names, and unresolved names
// all decline.
func (b *descriptorBuilder) lowerNameRef(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t.Namespaced || t.Path {
		return sd.Type{}, fmt.Errorf("path/namespaced identifier %q is not supported in a type position", t.Name)
	}
	name := t.Name
	if b.index.isAmbiguous(name) {
		return sd.Type{}, fmt.Errorf("type name %q is declared more than once (duplicate class/enum/alias)", name)
	}

	if tb, ok := b.index.classes[name]; ok {
		if err := b.ensureClass(name, tb); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeClass, Name: name, Mode: sd.NonStreaming}, nil
	}
	if tb, ok := b.index.enums[name]; ok {
		if err := b.ensureEnum(name, tb); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeEnum, Name: name}, nil
	}
	if alias, ok := b.index.aliases[name]; ok {
		return b.resolveAlias(name, alias)
	}
	return sd.Type{}, fmt.Errorf("unresolved type reference %q", name)
}

// ensureClass lowers a class definition into the collected set exactly once.
// It declines unsupported body content (methods / nested blocks) and
// named-argument (generic) class blocks. A RECURSIVE class re-entered on the
// active DFS path stops the recursion (the reference is emitted as a TypeClass
// and recursive_classes is populated from the cycle analysis) rather than
// declining. Class-level @@alias/@@description/@@check/@@assert/@@stream.* block
// attributes lower to the class's alias/description/constraints/streaming; any
// other block attribute (@@dynamic, ...) declines. A field's @alias/@description
// lower to the field, and its reassociated @check/@assert/@stream.* lower onto
// the field's TYPE metadata; a field's @skip DROPS it (D11); any other field
// attribute declines. A `///` doc comment is captured as a no-op (BAML never
// renders it — see #586 D6).
func (b *descriptorBuilder) ensureClass(name string, tb *bamlparser.TypeBlock) error {
	if _, done := b.classes[name]; done {
		return nil
	}
	if b.classVisiting[name] {
		// Re-entry on the active DFS path: `name` participates in a class cycle,
		// so it is a recursive class. Stop the recursion here — the reference is
		// emitted as a TypeClass by lowerNameRef and the class body is collected
		// by the outer ensureClass call; recursive_classes is populated from the
		// cycle analysis (reorderByReachability), so the class hoists correctly.
		// A cycle the analysis did NOT classify recursive is a detection
		// inconsistency: fail closed rather than emit a class ref that would
		// render-loop (a non-hoisted recursive class recurses forever).
		if !b.rec.recursiveClass[name] {
			return fmt.Errorf("class %q recursed during lowering but was not detected as recursive (recursion-analysis inconsistency)", name)
		}
		return nil
	}
	if tb.HasUnsupportedContent {
		return fmt.Errorf("class %q has unsupported body content (methods or nested blocks)", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("class %q has a named-argument list (parameterized classes are not supported)", name)
	}

	block, err := extractBlockMeta(tb.Attributes)
	if err != nil {
		return fmt.Errorf("class %q: %w", name, err)
	}

	b.classVisiting[name] = true
	defer delete(b.classVisiting, name)

	fields := make([]sd.ClassField, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type == nil {
			return fmt.Errorf("class %q field %q has no type", name, m.Name)
		}
		attrs := memberAttributes(m)
		// D11: @skip DROPS the field exactly as BAML does. BAML's
		// find_existing_class_field returns Ok(None) for a @skip-marked field
		// BEFORE the class definition is built, so the field never renders in
		// ctx.output_format AND its type is never pushed onto the reachability
		// stack (a class/enum reachable ONLY through a skipped field correctly
		// disappears from the output_format). The skipped field's other
		// attributes are irrelevant — skip is checked first — so we drop it
		// without lowering its type or metadata.
		if hasSkipAttribute(attrs) {
			continue
		}
		fm, err := extractFieldMeta(attrs)
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		ft, err := b.lowerFieldType(m.Type)
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		// BAML reassociates @check/@assert/@stream.* from the field onto its
		// TYPE, so the constraints/streaming ride on the field type's Meta, not
		// the field itself. The lowered type here is metadata-free (any nested
		// attribute already declined), so assigning Meta cannot clobber anything.
		ft.Meta.Constraints = fm.constraints
		ft.Meta.Stream = fm.stream
		fields = append(fields, sd.ClassField{
			Name:        sd.Name{Name: m.Name, Alias: fm.alias},
			Type:        ft,
			Description: fm.description,
		})
	}

	b.classes[name] = sd.ClassDef{
		Name:        sd.Name{Name: name, Alias: block.alias},
		Description: block.description,
		Mode:        sd.NonStreaming,
		Fields:      fields,
		Constraints: block.constraints,
		Stream:      block.stream,
	}
	b.classOrder = append(b.classOrder, name)
	return nil
}

// ensureEnum lowers an enum definition into the collected set exactly once. It
// declines unsupported body content and named-argument enum blocks. Enum-level
// @@alias lowers to the enum's alias and @@check/@@assert to its constraints;
// @@description and @@stream.* decline because the descriptor (like BAML's
// OutputFormatContent) has no enum-level description or streaming home for them.
// An enum value's @alias/@description lower to the value; @check/@assert/
// @stream.* on a value decline (EnumValue carries no constraints/streaming
// field); a value's @skip DROPS it (D11); any other value attribute declines. A
// `///` doc comment is captured as a no-op (BAML never renders it — see #586 D6).
func (b *descriptorBuilder) ensureEnum(name string, tb *bamlparser.TypeBlock) error {
	if _, done := b.enums[name]; done {
		return nil
	}
	if tb.HasUnsupportedContent {
		return fmt.Errorf("enum %q has unsupported body content", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("enum %q has a named-argument list (parameterized enums are not supported)", name)
	}

	block, err := extractBlockMeta(tb.Attributes)
	if err != nil {
		return fmt.Errorf("enum %q: %w", name, err)
	}
	// The descriptor model (and BAML's OutputFormatContent) has no enum-LEVEL
	// description or streaming — only enum VALUES carry a description, and BAML
	// renders neither an enum-level description nor enum-level streaming in
	// ctx.output_format. An @@description / @@stream.* on an enum is therefore
	// not representable; decline fail-closed rather than silently drop it.
	if block.description != nil {
		return fmt.Errorf("enum %q carries @@description, which has no rendered representation (only enum values carry descriptions)", name)
	}
	if !block.stream.IsZero() {
		return fmt.Errorf("enum %q carries an @@stream.* attribute, which has no enum-level representation", name)
	}

	values := make([]sd.EnumValue, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type != nil {
			return fmt.Errorf("enum %q value %q unexpectedly carries a type", name, m.Name)
		}
		// D11: @skip DROPS the value exactly as BAML does — find_enum_value
		// returns Ok(None) for a @skip-marked value before the enum definition
		// is built, so the value never renders in ctx.output_format.
		if hasSkipAttribute(m.Attributes) {
			continue
		}
		alias, desc, err := extractMemberMeta(m.Attributes)
		if err != nil {
			return fmt.Errorf("enum %q value %q: %w", name, m.Name, err)
		}
		values = append(values, sd.EnumValue{
			Name:        sd.Name{Name: m.Name, Alias: alias},
			Description: desc,
		})
	}

	b.enums[name] = sd.EnumDef{
		Name:        sd.Name{Name: name, Alias: block.alias},
		Values:      values,
		Constraints: block.constraints,
	}
	b.enumOrder = append(b.enumOrder, name)
	return nil
}

// resolveAlias lowers a reference to a type alias, dispatching on the project's
// recursion classification (recursion.go):
//
//   - a STRUCTURAL recursive alias (a cycle THROUGH a list/map) lowers to a
//     TypeRecursiveAlias reference and its target is collected once into
//     StructuralRecursiveAliases (see ensureStructuralAlias);
//   - an INVALID (direct/degenerate) alias cycle — one not mediated by a list or
//     map (`type A = A`, `type A = A | int`, `type A = A?`) — declines
//     fail-closed. BAML rejects these at validation, so production never
//     produces one; the builder never emits an approximation for one either;
//   - a NON-recursive alias is inlined by lowering its right-hand side in place
//     (BAML substitutes non-recursive aliases).
//
// D9: alias RHS attributes remain declined — BAML permits @check/@assert on
// aliases, but inlining a constraint-bearing alias onto its use sites is
// deferred, so an attributed alias fails closed rather than dropping the
// constraint.
func (b *descriptorBuilder) resolveAlias(name string, alias *bamlparser.TypeAlias) (sd.Type, error) {
	if len(alias.Attributes) > 0 {
		return sd.Type{}, fmt.Errorf("type alias %q carries attribute @%s (alias constraint inlining is deferred — see #586 D9)", name, alias.Attributes[0].Name)
	}
	if b.rec.invalidAlias[name] {
		return sd.Type{}, fmt.Errorf("type alias %q forms an invalid recursive cycle (a direct/degenerate alias cycle not mediated by a list or map)", name)
	}
	if b.rec.structuralAlias[name] {
		// Only single-alias structural cycles (the JSON-value shape) are lowered
		// in slice 5; a multi-alias structural cycle's hoist order is not
		// oracle-proven here, so decline it fail-closed rather than guess.
		if size := b.rec.structuralAliasCycleSize[name]; size > 1 {
			return sd.Type{}, fmt.Errorf("structural recursive alias %q participates in a %d-alias cycle, which is not supported yet (only single-alias structural cycles are lowered in slice 5)", name, size)
		}
		if err := b.ensureStructuralAlias(name, alias); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeRecursiveAlias, Name: name, Mode: sd.NonStreaming}, nil
	}

	// Non-recursive alias: inline. aliasVisiting backstops a detection
	// inconsistency (a non-recursive alias is acyclic, so it never re-enters).
	if b.aliasVisiting[name] {
		return sd.Type{}, fmt.Errorf("type alias %q recursed while inlining but was not detected as recursive (recursion-analysis inconsistency)", name)
	}
	if alias.Expr == nil {
		return sd.Type{}, fmt.Errorf("type alias %q has an unparsed right-hand side", name)
	}
	b.aliasVisiting[name] = true
	defer delete(b.aliasVisiting, name)
	return b.lowerType(alias.Expr)
}

// ensureStructuralAlias lowers a structural recursive alias's target into the
// collected StructuralRecursiveAliases set exactly once. The slot is reserved
// BEFORE the target is lowered so a self-reference inside the target resolves to
// a TypeRecursiveAlias (via resolveAlias) instead of recursing forever; the
// placeholder is overwritten with the real target once lowering completes. A
// target-lowering failure (e.g. a reachable media/tuple node) propagates and
// declines the whole function — the per-function builder is discarded on error,
// so the reserved placeholder never leaks.
func (b *descriptorBuilder) ensureStructuralAlias(name string, alias *bamlparser.TypeAlias) error {
	if _, done := b.structuralAliases[name]; done {
		return nil
	}
	if alias.Expr == nil {
		return fmt.Errorf("structural recursive alias %q has an unparsed right-hand side", name)
	}
	b.structuralAliases[name] = sd.Type{}
	b.structuralAliasOrder = append(b.structuralAliasOrder, name)
	target, err := b.lowerType(alias.Expr)
	if err != nil {
		return err
	}
	b.structuralAliases[name] = target
	return nil
}

// memberAttributes returns the field-level attributes of a class member: the
// member's own attributes plus the trailing field attributes the parser attaches
// to the OUTERMOST type node (it does not reassociate field-vs-type attributes,
// so both @alias/@description AND @check/@assert/@stream.* on `name string
// @alias("x") @check(...)` land on the string node). [extractFieldMeta] routes
// each by name — @alias/@description to the field, @check/@stream.* to the
// field's type — reproducing BAML's reassociate_type_attributes without needing
// the parser to have done it. Nested-node attributes are NOT included (they
// decline in lowerType).
func memberAttributes(m *bamlparser.TypeMember) []*bamlparser.Attribute {
	if m.Type == nil || len(m.Type.Attributes) == 0 {
		return m.Attributes
	}
	if len(m.Attributes) == 0 {
		return m.Type.Attributes
	}
	out := make([]*bamlparser.Attribute, 0, len(m.Attributes)+len(m.Type.Attributes))
	out = append(out, m.Attributes...)
	out = append(out, m.Type.Attributes...)
	return out
}

// hasSkipAttribute reports whether attrs carries a @skip field/value marker
// (D11). BAML models @skip as a field/value attribute — never reassociated onto
// the type (it is NOT in BAML's TYPE_ATTRIBUTE_NAMES) and taking no arguments
// (set_skip -> Some(true)) — and filters a skipped field/value out
// (find_existing_class_field / find_enum_value return Ok(None)) BEFORE the
// class/enum definition is built. Treating any non-block @skip as the marker is
// laxer-than-BAML-safe: production only ever sees BAML-valid input, where @skip
// is always the bare argument-less form. A @@skip block form is not a BAML
// construct; it is left to decline via extractBlockMeta.
func hasSkipAttribute(attrs []*bamlparser.Attribute) bool {
	for _, a := range attrs {
		if !a.Block && a.Name == "skip" {
			return true
		}
	}
	return false
}

// fieldMeta is a class field's lowered attribute metadata. BAML routes
// @alias/@description to the field itself and reassociates @assert/@check and
// @stream.* onto the field's TYPE, so the caller attaches constraints/stream to
// the lowered field-type's Meta and alias/description to the field.
type fieldMeta struct {
	alias       *string
	description *string
	constraints []sd.Constraint
	stream      sd.StreamingBehavior
}

// extractFieldMeta partitions a class field's attributes: @alias/@description
// (field metadata), @assert/@check (opaque constraints, reassociated to the
// type), and @stream.done/@stream.not_null/@stream.with_state (streaming, also
// reassociated to the type). It DECLINES fail-closed on any other (unknown)
// attribute, a duplicate @alias/@description, or a stray @@ block attribute
// (malformed on a field). Constraints are collected in source order. @skip never
// reaches here: a skipped field is dropped by ensureClass before this call (D11).
func extractFieldMeta(attrs []*bamlparser.Attribute) (fieldMeta, error) {
	var m fieldMeta
	for _, a := range attrs {
		if a.Block {
			return fieldMeta{}, declineAttribute(a)
		}
		switch {
		case a.Name == "alias":
			v, err := attributeStringArg(a)
			if err != nil {
				return fieldMeta{}, err
			}
			if m.alias != nil {
				return fieldMeta{}, fmt.Errorf("duplicate @alias attribute")
			}
			m.alias = &v
		case a.Name == "description":
			v, err := attributeStringArg(a)
			if err != nil {
				return fieldMeta{}, err
			}
			if m.description != nil {
				return fieldMeta{}, fmt.Errorf("duplicate @description attribute")
			}
			m.description = &v
		case isConstraintAttr(a.Name):
			c, err := constraintFromAttribute(a)
			if err != nil {
				return fieldMeta{}, err
			}
			m.constraints = append(m.constraints, c)
		case applyStreamAttribute(&m.stream, a):
			// flag set in place
		default:
			return fieldMeta{}, declineAttribute(a)
		}
	}
	return m, nil
}

// extractMemberMeta partitions an ENUM VALUE's attributes into the supported
// metadata (@alias, @description) and everything else. An EnumValue carries no
// constraints or streaming field, so @check/@assert/@stream.* — like unknown
// attributes — DECLINE fail-closed; a repeated @alias/@description and a stray @@
// block attribute also decline. @skip never reaches here: a skipped value is
// dropped by ensureEnum before this call (D11).
func extractMemberMeta(attrs []*bamlparser.Attribute) (alias, description *string, err error) {
	for _, a := range attrs {
		if a.Block {
			return nil, nil, declineAttribute(a)
		}
		switch a.Name {
		case "alias":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if alias != nil {
				return nil, nil, fmt.Errorf("duplicate @alias attribute")
			}
			alias = &v
		case "description":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if description != nil {
				return nil, nil, fmt.Errorf("duplicate @description attribute")
			}
			description = &v
		default:
			return nil, nil, declineAttribute(a)
		}
	}
	return alias, description, nil
}

// blockMeta is a class/enum-level block's lowered attribute metadata.
type blockMeta struct {
	alias       *string
	description *string
	constraints []sd.Constraint
	stream      sd.StreamingBehavior
}

// extractBlockMeta partitions a class/enum-level block's attributes: @@alias
// (name alias), @@description (description), @@check/@@assert (constraints), and
// @@stream.* (streaming). The enum caller further declines @@description /
// @@stream.* because an enum has no descriptor home for them. Any non-block
// attribute here is malformed, and @@dynamic / an unknown block attribute
// decline fail-closed.
func extractBlockMeta(attrs []*bamlparser.Attribute) (blockMeta, error) {
	var m blockMeta
	for _, a := range attrs {
		if !a.Block {
			return blockMeta{}, declineAttribute(a)
		}
		switch {
		case a.Name == "alias":
			v, err := attributeStringArg(a)
			if err != nil {
				return blockMeta{}, err
			}
			if m.alias != nil {
				return blockMeta{}, fmt.Errorf("duplicate @@alias attribute")
			}
			m.alias = &v
		case a.Name == "description":
			v, err := attributeStringArg(a)
			if err != nil {
				return blockMeta{}, err
			}
			if m.description != nil {
				return blockMeta{}, fmt.Errorf("duplicate @@description attribute")
			}
			m.description = &v
		case isConstraintAttr(a.Name):
			c, err := constraintFromAttribute(a)
			if err != nil {
				return blockMeta{}, err
			}
			m.constraints = append(m.constraints, c)
		case applyStreamAttribute(&m.stream, a):
			// flag set in place
		default:
			return blockMeta{}, declineAttribute(a)
		}
	}
	return m, nil
}

// isConstraintAttr reports whether name is a constraint attribute (@check or
// @assert, block or field).
func isConstraintAttr(name string) bool {
	return name == "check" || name == "assert"
}

// applyStreamAttribute sets the streaming flag named by a @stream.* / @@stream.*
// attribute on sb and reports whether a was a stream attribute at all (so a
// switch arm can both match and apply in one call). Stream markers take no
// arguments; any are ignored (harmless — BAML-valid input never carries them).
func applyStreamAttribute(sb *sd.StreamingBehavior, a *bamlparser.Attribute) bool {
	switch a.Name {
	case "stream.done":
		sb.Done = true
	case "stream.not_null":
		sb.Needed = true
	case "stream.with_state":
		sb.State = true
	default:
		return false
	}
	return true
}

// constraintFromAttribute lowers a @check/@assert (field or block) attribute
// into an opaque [sd.Constraint]. D7: the Jinja expression is preserved verbatim
// (the raw inner text between {{ and }}) and NEVER parsed or evaluated —
// evaluation is out of scope (deferred). A shape that is not (label?, {{expr}})
// declines fail-closed.
func constraintFromAttribute(a *bamlparser.Attribute) (sd.Constraint, error) {
	var level sd.ConstraintLevel
	switch a.Name {
	case "assert":
		level = sd.ConstraintAssert
	case "check":
		level = sd.ConstraintCheck
	default:
		return sd.Constraint{}, fmt.Errorf("attribute @%s is not a constraint", a.Name)
	}
	if !a.HasParens {
		return sd.Constraint{}, fmt.Errorf("@%s requires a ({{ expression }}) argument", a.Name)
	}
	label, expr, err := splitConstraintArgs(a.RawArgs)
	if err != nil {
		return sd.Constraint{}, fmt.Errorf("@%s: %w", a.Name, err)
	}
	return sd.Constraint{Level: level, Label: label, Expression: expr}, nil
}

// splitConstraintArgs splits a @check/@assert argument list into its optional
// leading label (a bare identifier) and its mandatory `{{ ... }}` Jinja
// expression, reading the RAW source between the attribute's parentheses so the
// Jinja text survives byte-for-byte. It never parses or evaluates the expression
// (D7 — evaluation is deferred).
//
// BAML's grammar is `@check(label, {{ expr }})` or a lone `@assert({{ expr }})`
// (a lone @check is a BAML validation error, but laxer-than-BAML is fine here:
// we only ever see BAML-valid input in production). The stored expression is the
// verbatim inner text between {{ and }} (braces excluded, not trimmed), matching
// BAML's JinjaExpression content modulo BAML's backslash-doubling normalization,
// which is deliberately skipped (the value is opaque and never evaluated).
func splitConstraintArgs(rawArgs string) (label *string, expression string, err error) {
	open := strings.Index(rawArgs, "{{")
	if open < 0 {
		return nil, "", fmt.Errorf("constraint expression must be a {{ ... }} Jinja block")
	}
	rest := rawArgs[open+2:]
	closeRel := strings.Index(rest, "}}")
	if closeRel < 0 {
		return nil, "", fmt.Errorf("constraint expression is missing its closing }}")
	}
	expression = rest[:closeRel]
	// The Jinja block is the LAST argument; anything after its closing }} is an
	// unsupported shape (BAML permits at most a label plus one expression).
	if trailing := strings.TrimSpace(rest[closeRel+2:]); trailing != "" {
		return nil, "", fmt.Errorf("unexpected trailing argument after the {{ ... }} expression")
	}
	labelPart := strings.TrimSpace(rawArgs[:open])
	labelPart = strings.TrimSpace(strings.TrimSuffix(labelPart, ","))
	if labelPart == "" {
		return nil, expression, nil
	}
	if !isBareIdentifier(labelPart) {
		return nil, "", fmt.Errorf("constraint label %q must be a bare identifier", labelPart)
	}
	l := labelPart
	return &l, expression, nil
}

// isBareIdentifier reports whether s is a single BAML-style identifier
// (`[A-Za-z_][A-Za-z0-9_]*`), used to validate a constraint label.
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// attributeStringArg extracts the single plain-string argument of an @alias/
// @description attribute (a quoted string or a raw string; both arrive with
// their delimiters stripped by the parser's normalization pass). D5: a missing,
// multi-valued, or non-string argument declines — the alias/description is
// rendered verbatim, so an argument it cannot reproduce byte-for-byte fails
// closed rather than guessing.
func attributeStringArg(a *bamlparser.Attribute) (string, error) {
	if len(a.Args) != 1 {
		return "", fmt.Errorf("@%s expects a single string argument", attrDisplayName(a))
	}
	v := a.Args[0]
	switch {
	case v.Literal != nil:
		return *v.Literal, nil
	case v.Raw != nil:
		return *v.Raw, nil
	default:
		return "", fmt.Errorf("@%s argument must be a plain string", attrDisplayName(a))
	}
}

// declineAttribute produces the stable per-function decline error for an
// attribute this slice does not lower on this node. The substrings "attribute"
// (field) and "block attribute" (block) are load-bearing for the decline-reason
// tests. Reached for @@dynamic / unknown attributes, a constraint or stream
// attribute on a node with no descriptor home (an enum value / enum-level
// block), and any attribute on a nested type node. @skip is NOT reached here for
// a class field / enum value: it is intercepted and DROPS the member (D11)
// before this point; a @skip on a nested type node (invalid BAML) still declines
// here.
func declineAttribute(a *bamlparser.Attribute) error {
	if a.Block {
		return fmt.Errorf("block attribute @@%s is not supported here (@@dynamic and unknown block attributes are declined; @@alias/@@description/@@check/@@assert/@@stream.* are lowered where the descriptor can represent them)", a.Name)
	}
	return fmt.Errorf("attribute @%s is not supported here (unknown attributes are declined, and @check/@assert/@stream.* have no home on this node; @alias/@description are lowered everywhere, constraints/streaming where representable, and @skip drops a class field / enum value)", a.Name)
}

// attrDisplayName renders an attribute's name with its @/@@ sigil for messages.
func attrDisplayName(a *bamlparser.Attribute) string {
	if a.Block {
		return "@" + a.Name
	}
	return a.Name
}
