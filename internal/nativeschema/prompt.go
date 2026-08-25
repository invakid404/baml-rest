package nativeschema

// prompt.go is the de-BAML Phase 1 slice-2 native PROMPT descriptor builder. It
// is a sibling of build.go's static OUTPUT-schema builder and runs AFTER it: it
// consumes the parsed native AST, the static-schema maps build.go already
// produced, and cmd/introspect's resolved client→provider map, then emits a
// per-function bamlutils/promptdescriptor.Function for every ELIGIBLE LLM
// function and a stable decline reason for every ineligible one.
//
// Like BuildStaticSchemas it is fail-closed PER FUNCTION: it returns a
// descriptor OR a decline for each method, never both, and never errors as a
// pipeline. Duplicate function names are last-wins, matching build.go. As of
// de-BAML Phase 8A (#602) cmd/introspect EMITS the descriptor it stores on
// *bamlConfig into the generated introspected package (StaticPromptDescriptors)
// as representation-only metadata that stays unrouted — no consumer, admission,
// or socket reads it. The raw prompt bytes and any inline client literals the
// descriptor carries are therefore sensitive generated-source + binary material
// (see promptdescriptor's package doc). This builder itself stays passive and
// reaches no request path.
//
// Decline contract (Phase 1 scope, "Precise decline contract"). Each decline
// carries a stable reason substring:
//
//	(a) "return bundle unavailable" — no native static return bundle. Inherits
//	    every static-schema decline (staticSchemaDeclines) under a prompt prefix,
//	    or reports the absence of a bundle.
//	(b) "no usable LLM function shape" — final prompt absent/non-raw, final client
//	    absent/non-scalar/unresolvable-to-a-provider, or a function field other
//	    than client/prompt.
//	(c) "@skip is reachable" — @skip reachable from a function input, return, or
//	    template-macro argument type. Intentionally STRICTER than the static
//	    schema builder's D11 (which DROPS a skipped OUTPUT field/value): native
//	    prompt rendering has not proven BAML's value/Jinja semantics around
//	    skipped declarations, so the descriptor declines rather than claims it.
//	(d) "@@dynamic/type_builder-like content is reachable" — @@dynamic (or
//	    type_builder-like content) reachable from those same roots. Output-side
//	    @@dynamic already arrives via (a); input/macro roots need this explicit
//	    scan.
//	(e) "cannot be resolved faithfully" — an input/macro type graph the scan
//	    cannot resolve (KindUnsupported, unresolved/ambiguous name, unsupported
//	    class/enum body, invalid alias cycle, unclassifiable attribute).
//	(f) "template string" body/"duplicate template string name" — a retained
//	    template declaration with a missing/non-raw/brace-tolerated body, a bad
//	    macro-argument type graph, or a duplicate macro name. Because BAML injects
//	    every template string into every function's prompt, such a macro poisons
//	    the descriptor for EVERY function (a global decline), rather than being
//	    silently ignored.
//	(g) "input value graph cannot be resolved faithfully" — de-BAML Slice 7.1b.
//	    A Version-3 descriptor REQUIRES the source-resolved input value universe
//	    (promptdescriptor.InputValueUniverse + a ValueType per argument), so a
//	    function is declined whenever any argument's type graph reaches a shape
//	    V3 does not claim: a map/tuple/union/literal/media node, a
//	    multi-dimensional list, an attributed type node, a bare/untyped argument,
//	    an ambiguous/unresolved name, an unsupported class/enum body, or a
//	    RECURSIVE input class graph. An unresolvable PROJECT enum (a dynamic /
//	    multi-valued @alias, a duplicate member, an enum-level block attribute)
//	    poisons every function, because BAML installs the enum namespace globals
//	    as a complete set. See inputvalues.go.
//
// Deliberately NOT declines (retained verbatim; Phase 3's renderer support
// predicate decides them later): Jinja syntax, macro calls, _.role/_.chat, media
// values, ctx.output_format, custom filters, enum/class values, python
// compatibility methods, the template_string declaration itself, and unreachable
// @skip/@@dynamic/invalid declarations.

import (
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// PreDeclineFeature is the STRUCTURAL classification a static prompt/schema
// decline carries out to internal/nativespine, so the stable capability code is
// chosen from a stamped verdict rather than by scraping the human-readable reason
// string. It is stamped by the decline path that actually fires, so it inherits
// that path's precedence and never a second, non-equivalent traversal.
//
// M1 scope (a "dark slice"): a feature is stamped only where the WINNING producer
// already knows it exactly — `FeatureDynamic` from the eligibility scan / project
// enum / macro-argument verdict — plus the reliable input-vs-return CONTEXT
// (`FeatureInputShape` / `FeatureOutputShape`) taken from which side of the
// signature the decline fired on. The static-SCHEMA builder is not yet
// instrumented to carry its causative node, so a schema-builder decline is named
// by its context, never by an independent re-walk that could pick an incidental
// earlier feature (an `@check` that lowered fine) over the real later cause (a
// `media`/union node). Precise media/checks/asserts sub-codes for those
// schema-builder pre-declines are M2 (full producer instrumentation). See
// docs/codegen-spine/04.
type PreDeclineFeature int

const (
	FeatureNone        PreDeclineFeature = iota // no carried structural cause (defensive default)
	FeatureDynamic                              // @@dynamic reachable — from the scan/enum/macro verdict
	FeatureOutputShape                          // a return-side decline (structural context)
	FeatureInputShape                           // an input/macro-side decline (structural context)
)

// featureError is a decline error that also carries the structural feature its
// producing path identified. Error() returns the SAME string the path emitted
// before, so every existing string-based decline assertion is unaffected; the
// feature rides alongside for the one seam (BuildPromptDescriptorsWithFeatures)
// that reads it.
type featureError struct {
	feature PreDeclineFeature
	msg     string
}

func (e *featureError) Error() string { return e.msg }

// declineFeature builds a decline error whose message is exactly the given
// formatted string and which carries feature for the structured seam.
func declineFeature(feature PreDeclineFeature, format string, args ...any) error {
	return &featureError{feature: feature, msg: fmt.Sprintf(format, args...)}
}

// featureOf extracts the structural feature a decline error carries, or
// FeatureNone for a plain error (a pure prompt-eligibility decline that named no
// type-graph cause).
func featureOf(err error) PreDeclineFeature {
	var fe *featureError
	if errors.As(err, &fe) {
		return fe.feature
	}
	return FeatureNone
}

// shapeFeature is the bare-shape feature for a scan root: a return root's defect
// is an output-shape decline, anything else (input/macro) an input-shape one.
func shapeFeature(rootKind string) PreDeclineFeature {
	if rootKind == "return" {
		return FeatureOutputShape
	}
	return FeatureInputShape
}

// BuildPromptDescriptors builds a native static prompt descriptor for every
// eligible LLM function from the already-parsed .baml files, the static-schema
// maps BuildStaticSchemas produced (schemas/schemaDeclines), and cmd/introspect's
// resolved clientProvider map (AFTER enrichShorthandClientProviders, so named
// and shorthand clients both resolve to a provider).
//
// It returns two always-non-nil maps keyed by function name: descriptors for the
// eligible functions and declines (a stable reason) for the ineligible ones. A
// declined function never appears in descriptors, and an eligible one never in
// declines. It never errors: an ineligible function is a per-function decline,
// not a pipeline failure, so this pass is safe to run on the production
// introspect path.
//
// This must run AFTER BuildStaticSchemas (it needs its output) and AFTER
// enrichShorthandClientProviders (it needs the enriched clientProvider map). The
// return bundle a descriptor carries is the EXACT ordered [sd.Bundle] from
// BuildStaticSchemas — its class/enum order is preserved verbatim.
// clientConfigs is the passive per-client body configuration built by
// [BuildClientConfigs] from the same files. It is stamped onto each eligible
// function's descriptor (de-BAML Phase 4a); a nil map (or a missing client
// entry) yields a Present==false ClientConfig, preserving Version-1 behavior for
// callers that do not supply it.
func BuildPromptDescriptors(
	files []SourceFile,
	schemas map[string]sd.Bundle,
	schemaDeclines map[string]string,
	clientProvider map[string]string,
	clientConfigs map[string]promptdescriptor.ClientConfig,
) (map[string]promptdescriptor.Function, map[string]string) {
	descriptors, declines, _ := BuildPromptDescriptorsWithFeatures(
		files, schemas, schemaDeclines, clientProvider, clientConfigs)
	return descriptors, declines
}

// BuildPromptDescriptorsWithFeatures is BuildPromptDescriptors plus a third map:
// for every declined function, the STRUCTURAL feature (PreDeclineFeature) the
// firing decline path identified from the parsed type graph. The reason strings
// in the second map are byte-identical to BuildPromptDescriptors — the feature is
// a strict addition — so cmd/introspect can classify a pre-decline by structure
// rather than by scraping words out of the reason. The feature map is keyed
// exactly like declines (a name is in one of descriptors/declines, never both).
func BuildPromptDescriptorsWithFeatures(
	files []SourceFile,
	schemas map[string]sd.Bundle,
	schemaDeclines map[string]string,
	clientProvider map[string]string,
	clientConfigs map[string]promptdescriptor.ClientConfig,
) (map[string]promptdescriptor.Function, map[string]string, map[string]PreDeclineFeature) {
	idx := buildSchemaTypeIndex(files)
	rec := idx.recursion()

	// Build the project macro set once. Every template_string is globally
	// injected by BAML, so a bad/duplicate/ambiguous macro is a GLOBAL decline:
	// buildMacros returns a non-empty reason that poisons every function.
	macros, macroDecline, macroDeclineFeature := buildMacros(files, idx, rec)

	// (g) Build the V3 project enum universe once. BAML installs one namespace
	// global per PROJECT enum, so the set is resolved whole and an unresolvable
	// enum is a GLOBAL decline — see inputvalues.go. enumDeclineFeature records
	// whether that global decline is @@dynamic (schema_dynamic_class) or another
	// input-universe defect.
	projectEnums, enumDecline, enumDeclineFeature := resolveProjectEnums(files, idx)

	pb := &promptBuilder{
		schemas:             schemas,
		schemaDeclines:      schemaDeclines,
		clientProvider:      clientProvider,
		clientConfigs:       clientConfigs,
		idx:                 idx,
		rec:                 rec,
		macros:              macros,
		macroDecline:        macroDecline,
		macroDeclineFeature: macroDeclineFeature,
		projectEnums:        projectEnums,
		enumDecline:         enumDecline,
		enumDeclineFeature:  enumDeclineFeature,
	}

	descriptors := make(map[string]promptdescriptor.Function)
	declines := make(map[string]string)
	features := make(map[string]PreDeclineFeature)

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
			desc, err := pb.buildFunction(fn)
			if err != nil {
				// Last-wins on duplicate function names — a later declaration's
				// outcome supersedes an earlier one (mirrors BuildStaticSchemas).
				declines[fn.Name] = err.Error()
				features[fn.Name] = featureOf(err)
				delete(descriptors, fn.Name)
				continue
			}
			descriptors[fn.Name] = desc
			delete(declines, fn.Name)
			delete(features, fn.Name)
		}
	}

	return descriptors, declines, features
}

// promptBuilder carries the shared inputs for a single BuildPromptDescriptors
// run: the static-schema outputs, the resolved client→provider map, the schema
// type index / recursion classification (reused from build.go, not a second
// resolver), and the project macro set with any global macro decline.
type promptBuilder struct {
	schemas        map[string]sd.Bundle
	schemaDeclines map[string]string
	clientProvider map[string]string
	clientConfigs  map[string]promptdescriptor.ClientConfig
	idx            *schemaTypeIndex
	rec            *recursionInfo
	macros         []promptdescriptor.TemplateString
	macroDecline   string
	// macroDeclineFeature is the structural feature of macroDecline: FeatureDynamic
	// for an @@dynamic-reachable macro argument, FeatureInputShape for every other
	// macro decline (missing name, unusable body, duplicate name, @skip/unresolvable
	// argument). Zero (FeatureNone) only when there is no macro decline.
	macroDeclineFeature PreDeclineFeature
	// projectEnums is the V3 project-wide resolved enum set (every declared
	// enum, in source order); enumDecline is the global reason it could not be
	// resolved, which poisons V3 — and therefore the descriptor — for every
	// function. See inputvalues.go.
	projectEnums []promptdescriptor.ResolvedEnum
	enumDecline  string
	// enumDeclineFeature is the structural feature of enumDecline: FeatureDynamic
	// for an @@dynamic project enum, FeatureInputShape for any other unresolvable
	// project enum (both poison the V3 input universe). Zero (FeatureNone) when
	// there is no enum decline.
	enumDeclineFeature PreDeclineFeature
}

// buildFunction evaluates one function against the full decline contract and
// returns its descriptor or the first applicable decline. The order is
// deliberate — cheapest/most-specific first: (b) shape, (a) return bundle, (f)
// global macro decline, then the (c)/(d)/(e) input+return type-graph scan.
func (pb *promptBuilder) buildFunction(fn *bamlparser.FunctionBlock) (promptdescriptor.Function, error) {
	// (b) No usable LLM function shape. Only `client` and `prompt` are eligible
	// function fields; any other field declines. The client is the FINAL scalar
	// client field (last-wins, matching cmd/introspect's functionClient walk);
	// the prompt is the projected final raw prompt (fn.PromptRaw/HasPrompt).
	clientName := ""
	haveClient := false
	for _, f := range fn.Fields {
		switch f.Key {
		case "client":
			if s, ok := functionClientScalar(f.Value); ok {
				clientName, haveClient = s, true
			} else {
				// A non-scalar final client field (list/block) is not usable;
				// last-wins means it clears any earlier scalar client too.
				clientName, haveClient = "", false
			}
		case "prompt":
			// The final prompt is projected onto fn.PromptRaw/HasPrompt during
			// parse normalization; nothing to read from Fields here.
		default:
			return promptdescriptor.Function{}, declineFeature(FeatureInputShape,
				"no usable LLM function shape: function %q has field %q other than client/prompt", fn.Name, f.Key)
		}
	}
	if !fn.HasPrompt {
		return promptdescriptor.Function{}, declineFeature(FeatureInputShape,
			"no usable LLM function shape: function %q has no prompt field", fn.Name)
	}
	if fn.PromptRaw == nil {
		return promptdescriptor.Function{}, declineFeature(FeatureInputShape,
			"no usable LLM function shape: function %q final prompt field is not a raw string", fn.Name)
	}
	if !haveClient {
		return promptdescriptor.Function{}, declineFeature(FeatureInputShape,
			"no usable LLM function shape: function %q has no usable scalar client field", fn.Name)
	}
	provider, ok := pb.clientProvider[clientName]
	if !ok {
		return promptdescriptor.Function{}, declineFeature(FeatureInputShape,
			"no usable LLM function shape: function %q client %q does not resolve to a provider after shorthand enrichment", fn.Name, clientName)
	}

	// (a) Return bundle unavailable. Inherit every static-schema decline under a
	// prompt-descriptor prefix; if the method was never built (no return type,
	// etc.) and left no decline, report the absent bundle directly.
	bundle, ok := pb.schemas[fn.Name]
	if !ok {
		if reason, has := pb.schemaDeclines[fn.Name]; has {
			// The static-schema builder is the winning path here, and it does not
			// (M1) carry which node it failed on. Rather than re-walk the raw return
			// graph independently — which can pick an incidental earlier feature over
			// the real later cause — name it by its reliable RETURN context. Precise
			// media/checks sub-codes for schema-builder declines are M2.
			return promptdescriptor.Function{}, declineFeature(
				FeatureOutputShape, "prompt descriptor return bundle unavailable: %s", reason)
		}
		return promptdescriptor.Function{}, declineFeature(FeatureOutputShape,
			"prompt descriptor return bundle unavailable: no native static return bundle")
	}

	// (f)+(c/d/e via macro) A poisoned project macro declines every function.
	if pb.macroDecline != "" {
		return promptdescriptor.Function{}, declineFeature(pb.macroDeclineFeature, "%s", pb.macroDecline)
	}

	// (c)/(d)/(e) Reachable-eligibility scan over every function INPUT type and
	// the RETURN type. The return is re-scanned even though its bundle exists:
	// the static builder DROPS @skip output fields (D11), so a @skip reachable
	// from the return graph must still decline the prompt descriptor here.
	for _, p := range fn.Params {
		if err := pb.scanRoot("input", p.Name, p.Type); err != nil {
			return promptdescriptor.Function{}, err
		}
	}
	if err := pb.scanRoot("return", "", fn.Return); err != nil {
		return promptdescriptor.Function{}, err
	}

	// (g) Slice 7.1b V3 input value graph. A descriptor is Version 3, and V3
	// REQUIRES the resolved universe, so a function whose argument graph reaches
	// a shape V3 cannot describe exactly gets no descriptor at all. The project
	// enum decline is checked first because it poisons every function.
	if pb.enumDecline != "" {
		return promptdescriptor.Function{}, declineFeature(pb.enumDeclineFeature, "%s", pb.enumDecline)
	}
	args, universe, err := pb.buildInputValues(fn)
	if err != nil {
		// The input value graph declined. Its @@dynamic/@skip cases already fired
		// in the scanRoot pass above (carried precisely); the shape/media cases the
		// input resolver reports are (M1) named by their reliable INPUT context
		// rather than an independent re-walk. Precise media sub-codes are M2.
		return promptdescriptor.Function{}, declineFeature(FeatureInputShape, "%s", err.Error())
	}

	// (Phase 4a) Stamp the passive client/options config. A missing entry (a
	// shorthand/enriched-only client with no declared block) yields the zero
	// ClientConfig (Present==false). Name/Provider are set to the resolved values
	// so ClientConfig.Provider always equals Function.Provider.
	clientConfig := pb.clientConfigs[clientName]
	clientConfig.Name = clientName
	clientConfig.Provider = provider

	return promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   fn.Name,
		Prompt:   *fn.PromptRaw,
		Args:     args,
		Client:   clientName,
		Provider: provider,
		Return:   bundle,
		// Every eligible function carries the whole project macro set (BAML
		// injects all template strings ahead of the selected prompt). The slice
		// is shared read-only; the descriptor is passive and never mutates it.
		Macros:       pb.macros,
		ClientConfig: clientConfig,
		InputValues:  universe,
	}, nil
}

// buildInputValues resolves one function's V3 arguments and input value
// universe (Slice 7.1b). It returns the descriptor arguments WITH their
// ValueType populated plus the universe (the project enum set + the transitive
// input-class closure in source declaration order), or the first decline.
//
// Every argument must resolve: a bare/untyped argument, a duplicate name, or any
// type outside the claimed value graph declines the whole function. There is no
// partial V3 — a descriptor either describes every argument exactly or does not
// exist.
func (pb *promptBuilder) buildInputValues(fn *bamlparser.FunctionBlock) ([]promptdescriptor.Argument, promptdescriptor.InputValueUniverse, error) {
	r := newInputValueResolver(pb.idx)

	args := make([]promptdescriptor.Argument, 0, len(fn.Params))
	seen := make(map[string]bool, len(fn.Params))
	for _, p := range fn.Params {
		if p.Name == "" {
			return nil, promptdescriptor.InputValueUniverse{}, fmt.Errorf(
				"input value graph cannot be resolved faithfully: function %q has an argument with an empty name", fn.Name)
		}
		if seen[p.Name] {
			return nil, promptdescriptor.InputValueUniverse{}, argValueDecline(p.Name, fmt.Errorf("declared more than once"))
		}
		seen[p.Name] = true
		if p.Type == nil {
			return nil, promptdescriptor.InputValueUniverse{}, argValueDecline(p.Name, fmt.Errorf("is bare/untyped"))
		}
		vt, err := r.resolveType(p.Type)
		if err != nil {
			return nil, promptdescriptor.InputValueUniverse{}, argValueDecline(p.Name, err)
		}
		args = append(args, promptdescriptor.Argument{Name: p.Name, Type: p.Type, ValueType: &vt})
	}
	if len(args) == 0 {
		args = nil
	}

	classes, err := r.closureInDeclarationOrder()
	if err != nil {
		return nil, promptdescriptor.InputValueUniverse{}, fmt.Errorf(
			"input value graph cannot be resolved faithfully: %w", err)
	}

	return args, promptdescriptor.InputValueUniverse{
		ProjectEnums: pb.projectEnums,
		Classes:      classes,
	}, nil
}

// buildMacros collects the project's template_string declarations into an
// ordered macro set: parsed SourceFile order, then File.Items order within each
// file — NEVER lexical or map order, so the macro order is a deterministic
// function of parse input order (SourcePath makes a later cross-file oracle
// diagnosable). It returns a non-empty decline reason (globally poisoning every
// function) the moment a macro is unusable per contract (f): a missing/non-raw/
// brace-tolerated body, a duplicate name, or a macro-argument type graph the
// eligibility scan rejects (c/d/e reachable from a macro-arg root).
func buildMacros(files []SourceFile, idx *schemaTypeIndex, rec *recursionInfo) ([]promptdescriptor.TemplateString, string, PreDeclineFeature) {
	var macros []promptdescriptor.TemplateString
	seen := make(map[string]bool)

	for _, sf := range files {
		f := sf.File
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			tb := it.Template
			if tb == nil {
				continue
			}
			if tb.Name == "" {
				return nil, "template string declaration has no name", FeatureInputShape
			}
			// (f) A brace-tolerated or non-raw body is never fabricated into a
			// Body string by the parser; decline rather than invent one. A poisoned
			// macro is a global, input-like decline; its context is stamped
			// STRUCTURALLY (FeatureInputShape) so the user-controlled macro name in
			// the reason can never influence the stable code.
			if tb.HasUnsupportedBody || tb.Body == nil {
				return nil, fmt.Sprintf(
					"template string %q has no usable raw body (missing, non-raw, or brace-tolerated)", tb.Name), FeatureInputShape
			}
			// (f) Duplicate macro names are ambiguous under global injection.
			if seen[tb.Name] {
				return nil, fmt.Sprintf("duplicate template string name %q", tb.Name), FeatureInputShape
			}
			seen[tb.Name] = true
			// (c)/(d)/(e) A macro-argument type is a scan root too. A macro argument
			// reaching @@dynamic is a schema_dynamic_class cause structurally, just
			// like a function argument reaching it — the feature comes from the scan
			// verdict, not from the reason string.
			for _, p := range tb.Args {
				if p.Type == nil {
					continue // a bare (untyped) macro argument reaches no type graph
				}
				s := newPromptTypeScanner(idx, rec)
				if res := s.scan(p.Type); res.kind != scanOK {
					return nil, macroArgDecline(tb.Name, p.Name, res), macroArgFeature(res)
				}
			}
			macros = append(macros, promptdescriptor.TemplateString{
				Name:       tb.Name,
				Args:       toArguments(tb.Args),
				Body:       *tb.Body,
				SourcePath: sf.Path,
			})
		}
	}
	return macros, "", FeatureNone
}

// macroArgFeature names the structural feature of a macro-argument scan decline:
// FeatureDynamic for an @@dynamic-reachable argument, FeatureInputShape for a
// @skip/unresolvable one (a macro argument is an input-like root).
func macroArgFeature(res scanResult) PreDeclineFeature {
	if res.kind == scanDynamic {
		return FeatureDynamic
	}
	return FeatureInputShape
}

// scanRoot runs the reachable-eligibility scan from one root type (a function
// input, the return, or — via buildMacros — a macro argument) and maps a scan
// decline onto the (c)/(d)/(e) contract wording with the root's context. A nil
// root type (a bare, untyped argument) reaches no type graph and is not a
// decline.
func (pb *promptBuilder) scanRoot(rootKind, name string, t *bamlparser.TypeExpr) error {
	if t == nil {
		return nil
	}
	s := newPromptTypeScanner(pb.idx, pb.rec)
	res := s.scan(t)
	if res.kind == scanOK {
		return nil
	}
	root := rootKind + " type"
	if name != "" {
		root = fmt.Sprintf("%s type %q", rootKind, name)
	}
	switch res.kind {
	case scanSkip:
		return declineFeature(shapeFeature(rootKind), "@skip is reachable from %s: %s", root, res.reason)
	case scanDynamic:
		return declineFeature(FeatureDynamic, "@@dynamic/type_builder-like content is reachable from %s: %s", root, res.reason)
	default:
		return declineFeature(shapeFeature(rootKind), "%s graph cannot be resolved faithfully: %s", root, res.reason)
	}
}

// scanKind classifies an eligibility-scan outcome. The non-OK kinds map onto the
// (c)/(d)/(e) declines.
type scanKind int

const (
	scanOK scanKind = iota
	scanSkip
	scanDynamic
	scanUnresolvable
)

// scanResult is one eligibility-scan outcome plus a human-readable reason
// detail (empty for scanOK).
type scanResult struct {
	kind   scanKind
	reason string
}

func scanOKResult() scanResult { return scanResult{kind: scanOK} }

// promptTypeScanner walks a type graph rooted at a function input / return /
// macro argument, resolving named references through the SHARED schema type
// index (build.go) rather than a second resolver, and reports the first
// eligibility decline it finds. visiting guards the active DFS path so a valid
// recursive class/alias does not loop; an INVALID alias cycle is classified up
// front via the recursion analysis.
type promptTypeScanner struct {
	idx      *schemaTypeIndex
	rec      *recursionInfo
	visiting map[string]bool
}

func newPromptTypeScanner(idx *schemaTypeIndex, rec *recursionInfo) *promptTypeScanner {
	return &promptTypeScanner{idx: idx, rec: rec, visiting: make(map[string]bool)}
}

// scan walks t and returns the first eligibility decline. It checks the node's
// own attributes first (so a @skip/@@dynamic on any node short-circuits), then
// recurses structurally. A nil child (e.g. a malformed list element) is an
// unresolvable graph — distinct from a nil ROOT, which scanRoot treats as
// "nothing to scan".
func (s *promptTypeScanner) scan(t *bamlparser.TypeExpr) scanResult {
	if t == nil {
		return scanResult{scanUnresolvable, "missing type expression"}
	}
	if r := s.classifyAttrs(t.Attributes); r.kind != scanOK {
		return r
	}
	switch t.Kind {
	case bamlparser.KindUnsupported:
		reason := t.Reason
		if reason == "" {
			reason = "unsupported type"
		}
		return scanResult{scanUnresolvable, reason}
	case bamlparser.KindPrimitive, bamlparser.KindMedia, bamlparser.KindLiteral:
		// Scalars reach no further graph; media/literal are prompt-inert here
		// (media as an INPUT is fine — only OUTPUT media is rejected, by (a)).
		return scanOKResult()
	case bamlparser.KindNameRef:
		return s.scanNameRef(t)
	case bamlparser.KindList:
		return s.scan(t.Elem)
	case bamlparser.KindMap:
		if r := s.scan(t.Key); r.kind != scanOK {
			return r
		}
		return s.scan(t.Value)
	case bamlparser.KindUnion:
		for _, v := range t.Variants {
			if r := s.scan(v); r.kind != scanOK {
				return r
			}
		}
		return scanOKResult()
	case bamlparser.KindTuple:
		for _, it := range t.Items {
			if r := s.scan(it); r.kind != scanOK {
				return r
			}
		}
		return scanOKResult()
	case bamlparser.KindGroup:
		return s.scan(t.Inner)
	default:
		return scanResult{scanUnresolvable, fmt.Sprintf("unhandled type kind %d", t.Kind)}
	}
}

// scanNameRef resolves a named reference against the schema index and scans the
// referenced definition. Path/namespaced identifiers, ambiguous names, and
// unresolved names all decline (e); a re-entry on the active DFS path is a valid
// recursive type and stops the walk.
func (s *promptTypeScanner) scanNameRef(t *bamlparser.TypeExpr) scanResult {
	if t.Namespaced || t.Path {
		return scanResult{scanUnresolvable, fmt.Sprintf("path/namespaced identifier %q is not supported in a type position", t.Name)}
	}
	name := t.Name
	if s.idx.isAmbiguous(name) {
		return scanResult{scanUnresolvable, fmt.Sprintf("type name %q is declared more than once (duplicate class/enum/alias)", name)}
	}
	if s.visiting[name] {
		return scanOKResult()
	}
	if tb, ok := s.idx.classes[name]; ok {
		s.visiting[name] = true
		defer delete(s.visiting, name)
		return s.scanClass(name, tb)
	}
	if tb, ok := s.idx.enums[name]; ok {
		return s.scanEnum(name, tb)
	}
	if alias, ok := s.idx.aliases[name]; ok {
		if s.rec.invalidAlias[name] {
			return scanResult{scanUnresolvable, fmt.Sprintf("type alias %q forms an invalid recursive cycle", name)}
		}
		s.visiting[name] = true
		defer delete(s.visiting, name)
		if r := s.classifyAttrs(alias.Attributes); r.kind != scanOK {
			return r
		}
		if alias.Expr == nil {
			return scanResult{scanUnresolvable, fmt.Sprintf("type alias %q has an unparsed right-hand side", name)}
		}
		return s.scan(alias.Expr)
	}
	return scanResult{scanUnresolvable, fmt.Sprintf("unresolved type reference %q", name)}
}

// scanClass scans a class definition's block attributes, body shape, and every
// field. A @skip on a field short-circuits (c); @@dynamic on the block or an
// unsupported/parameterized body declines (d/e). Field types are scanned with
// their outermost attributes already classified via memberAttributes (the same
// field-vs-type attribute reassociation build.go uses).
func (s *promptTypeScanner) scanClass(name string, tb *bamlparser.TypeBlock) scanResult {
	if r := s.classifyAttrs(tb.Attributes); r.kind != scanOK {
		return r
	}
	if tb.HasUnsupportedContent {
		return scanResult{scanUnresolvable, fmt.Sprintf("class %q has unsupported body content (methods or nested blocks)", name)}
	}
	if len(tb.Args) > 0 {
		return scanResult{scanUnresolvable, fmt.Sprintf("class %q has a named-argument list (parameterized classes are not supported)", name)}
	}
	for _, m := range tb.Fields {
		if r := s.classifyAttrs(memberAttributes(m)); r.kind != scanOK {
			return r
		}
		if m.Type == nil {
			return scanResult{scanUnresolvable, fmt.Sprintf("class %q field %q has no type", name, m.Name)}
		}
		if r := s.scanFieldType(m.Type); r.kind != scanOK {
			return r
		}
	}
	return scanOKResult()
}

// scanEnum scans an enum definition's block attributes, body shape, and every
// value's attributes (@skip on a value short-circuits; enum values carry no
// type to recurse into).
func (s *promptTypeScanner) scanEnum(name string, tb *bamlparser.TypeBlock) scanResult {
	if r := s.classifyAttrs(tb.Attributes); r.kind != scanOK {
		return r
	}
	if tb.HasUnsupportedContent {
		return scanResult{scanUnresolvable, fmt.Sprintf("enum %q has unsupported body content", name)}
	}
	if len(tb.Args) > 0 {
		return scanResult{scanUnresolvable, fmt.Sprintf("enum %q has a named-argument list (parameterized enums are not supported)", name)}
	}
	for _, m := range tb.Fields {
		if r := s.classifyAttrs(m.Attributes); r.kind != scanOK {
			return r
		}
	}
	return scanOKResult()
}

// scanFieldType scans a class field's type after its outermost attributes have
// already been classified by the caller (via memberAttributes). It strips those
// outermost attributes before recursing so they are not re-examined, mirroring
// build.go's lowerFieldType; nested-node attributes are still classified by scan.
func (s *promptTypeScanner) scanFieldType(t *bamlparser.TypeExpr) scanResult {
	if t == nil {
		return scanResult{scanUnresolvable, "missing type expression"}
	}
	if len(t.Attributes) > 0 {
		cp := *t
		cp.Attributes = nil
		return s.scan(&cp)
	}
	return s.scan(t)
}

// classifyAttrs walks a node's attributes and returns the first that forces a
// decline: a @skip (c), a @@dynamic (d), or an attribute the scan cannot
// classify (e). Known prompt-inert metadata (@alias/@description/@check/@assert/
// @stream.*, block or field) is skipped — it does not affect prompt eligibility.
func (s *promptTypeScanner) classifyAttrs(attrs []*bamlparser.Attribute) scanResult {
	for _, a := range attrs {
		switch classifyPromptAttr(a) {
		case promptAttrSkip:
			return scanResult{scanSkip, "@skip attribute present"}
		case promptAttrDynamic:
			return scanResult{scanDynamic, fmt.Sprintf("%s attribute present", attrSigil(a))}
		case promptAttrBenign:
			// prompt-inert metadata: nothing to do.
		default: // promptAttrUnknown
			return scanResult{scanUnresolvable, fmt.Sprintf("unclassifiable attribute %s", attrSigil(a))}
		}
	}
	return scanOKResult()
}

// promptAttrClass buckets an attribute for the eligibility scan.
type promptAttrClass int

const (
	promptAttrUnknown promptAttrClass = iota
	promptAttrBenign
	promptAttrSkip
	promptAttrDynamic
)

// classifyPromptAttr buckets an attribute: @skip → skip decline (c), @@dynamic →
// dynamic decline (d), the known metadata set → benign, and everything else →
// unknown (e). The known set mirrors build.go's supported attribute names so the
// prompt scan and the schema builder agree on what is representable.
func classifyPromptAttr(a *bamlparser.Attribute) promptAttrClass {
	if a.Block {
		if a.Name == "dynamic" {
			return promptAttrDynamic
		}
		if isKnownMetadataAttr(a.Name) {
			return promptAttrBenign
		}
		return promptAttrUnknown
	}
	if a.Name == "skip" {
		return promptAttrSkip
	}
	if isKnownMetadataAttr(a.Name) {
		return promptAttrBenign
	}
	return promptAttrUnknown
}

// isKnownMetadataAttr reports whether name is a prompt-inert metadata attribute
// (alias/description/check/assert/stream.*). @skip and @@dynamic are handled by
// the caller before this is consulted.
func isKnownMetadataAttr(name string) bool {
	switch name {
	case "alias", "description", "check", "assert",
		"stream.done", "stream.not_null", "stream.with_state":
		return true
	}
	return false
}

// macroArgDecline frames a macro-argument scan decline for the global-macro
// path (f), preserving the (c)/(d)/(e) reason substrings.
func macroArgDecline(macro, arg string, res scanResult) string {
	switch res.kind {
	case scanSkip:
		return fmt.Sprintf("@skip is reachable from template string %q argument %q type: %s", macro, arg, res.reason)
	case scanDynamic:
		return fmt.Sprintf("@@dynamic/type_builder-like content is reachable from template string %q argument %q type: %s", macro, arg, res.reason)
	default:
		return fmt.Sprintf("template string %q argument %q type graph cannot be resolved faithfully: %s", macro, arg, res.reason)
	}
}

// toArguments projects a parsed parameter list onto the passive descriptor
// argument list, retaining each parsed type (nil for a bare argument).
//
// It is the MACRO-argument projection only. A macro argument deliberately
// carries NO V3 ValueType: BAML injects every project template_string into every
// function, so a project that declares one declines at the macro gate and no
// macro argument is ever bound. Function arguments go through
// [promptBuilder.buildInputValues] instead, which resolves a ValueType for each.
func toArguments(params []*bamlparser.Param) []promptdescriptor.Argument {
	if len(params) == 0 {
		return nil
	}
	out := make([]promptdescriptor.Argument, 0, len(params))
	for _, p := range params {
		out = append(out, promptdescriptor.Argument{Name: p.Name, Type: p.Type})
	}
	return out
}

// functionClientScalar extracts a function's `client` field value as the scalar
// client name, mirroring cmd/introspect's bamlValueScalar so the descriptor's
// Client matches the key cmd/introspect uses for clientProvider (named clients,
// shorthand specs, and enriched shorthands alike). A list/block/absent value is
// NOT a scalar and reports ok=false — the caller declines it as an unusable
// client shape (b).
func functionClientScalar(v *bamlparser.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	switch {
	case v.IsLiteral():
		s, _ := v.LiteralValue()
		return s, true
	case v.IsIdent():
		s, _ := v.IdentValue()
		return s, true
	case v.IsNumber():
		s, _ := v.NumberValue()
		return s, true
	case v.IsRaw():
		s, _ := v.RawValue()
		return s, true
	case v.IsEnvRef():
		name, _ := v.EnvName()
		return "env." + name, true
	}
	return "", false
}

// attrSigil renders an attribute name with its @/@@ sigil for decline messages.
func attrSigil(a *bamlparser.Attribute) string {
	if a.Block {
		return "@@" + a.Name
	}
	return "@" + a.Name
}
