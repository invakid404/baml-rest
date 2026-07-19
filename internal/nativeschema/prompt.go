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
//
// Deliberately NOT declines (retained verbatim; Phase 3's renderer support
// predicate decides them later): Jinja syntax, macro calls, _.role/_.chat, media
// values, ctx.output_format, custom filters, enum/class values, python
// compatibility methods, the template_string declaration itself, and unreachable
// @skip/@@dynamic/invalid declarations.

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

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
	idx := buildSchemaTypeIndex(files)
	rec := idx.recursion()

	// Build the project macro set once. Every template_string is globally
	// injected by BAML, so a bad/duplicate/ambiguous macro is a GLOBAL decline:
	// buildMacros returns a non-empty reason that poisons every function.
	macros, macroDecline := buildMacros(files, idx, rec)

	pb := &promptBuilder{
		schemas:        schemas,
		schemaDeclines: schemaDeclines,
		clientProvider: clientProvider,
		clientConfigs:  clientConfigs,
		idx:            idx,
		rec:            rec,
		macros:         macros,
		macroDecline:   macroDecline,
	}

	descriptors := make(map[string]promptdescriptor.Function)
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
			desc, err := pb.buildFunction(fn)
			if err != nil {
				// Last-wins on duplicate function names — a later declaration's
				// outcome supersedes an earlier one (mirrors BuildStaticSchemas).
				declines[fn.Name] = err.Error()
				delete(descriptors, fn.Name)
				continue
			}
			descriptors[fn.Name] = desc
			delete(declines, fn.Name)
		}
	}

	return descriptors, declines
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
			return promptdescriptor.Function{}, fmt.Errorf(
				"no usable LLM function shape: function %q has field %q other than client/prompt", fn.Name, f.Key)
		}
	}
	if !fn.HasPrompt {
		return promptdescriptor.Function{}, fmt.Errorf(
			"no usable LLM function shape: function %q has no prompt field", fn.Name)
	}
	if fn.PromptRaw == nil {
		return promptdescriptor.Function{}, fmt.Errorf(
			"no usable LLM function shape: function %q final prompt field is not a raw string", fn.Name)
	}
	if !haveClient {
		return promptdescriptor.Function{}, fmt.Errorf(
			"no usable LLM function shape: function %q has no usable scalar client field", fn.Name)
	}
	provider, ok := pb.clientProvider[clientName]
	if !ok {
		return promptdescriptor.Function{}, fmt.Errorf(
			"no usable LLM function shape: function %q client %q does not resolve to a provider after shorthand enrichment", fn.Name, clientName)
	}

	// (a) Return bundle unavailable. Inherit every static-schema decline under a
	// prompt-descriptor prefix; if the method was never built (no return type,
	// etc.) and left no decline, report the absent bundle directly.
	bundle, ok := pb.schemas[fn.Name]
	if !ok {
		if reason, has := pb.schemaDeclines[fn.Name]; has {
			return promptdescriptor.Function{}, fmt.Errorf(
				"prompt descriptor return bundle unavailable: %s", reason)
		}
		return promptdescriptor.Function{}, fmt.Errorf(
			"prompt descriptor return bundle unavailable: no native static return bundle")
	}

	// (f)+(c/d/e via macro) A poisoned project macro declines every function.
	if pb.macroDecline != "" {
		return promptdescriptor.Function{}, fmt.Errorf("%s", pb.macroDecline)
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
		Args:     toArguments(fn.Params),
		Client:   clientName,
		Provider: provider,
		Return:   bundle,
		// Every eligible function carries the whole project macro set (BAML
		// injects all template strings ahead of the selected prompt). The slice
		// is shared read-only; the descriptor is passive and never mutates it.
		Macros:       pb.macros,
		ClientConfig: clientConfig,
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
func buildMacros(files []SourceFile, idx *schemaTypeIndex, rec *recursionInfo) ([]promptdescriptor.TemplateString, string) {
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
				return nil, "template string declaration has no name"
			}
			// (f) A brace-tolerated or non-raw body is never fabricated into a
			// Body string by the parser; decline rather than invent one.
			if tb.HasUnsupportedBody || tb.Body == nil {
				return nil, fmt.Sprintf(
					"template string %q has no usable raw body (missing, non-raw, or brace-tolerated)", tb.Name)
			}
			// (f) Duplicate macro names are ambiguous under global injection.
			if seen[tb.Name] {
				return nil, fmt.Sprintf("duplicate template string name %q", tb.Name)
			}
			seen[tb.Name] = true
			// (c)/(d)/(e) A macro-argument type is a scan root too.
			for _, p := range tb.Args {
				if p.Type == nil {
					continue // a bare (untyped) macro argument reaches no type graph
				}
				s := newPromptTypeScanner(idx, rec)
				if res := s.scan(p.Type); res.kind != scanOK {
					return nil, macroArgDecline(tb.Name, p.Name, res)
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
	return macros, ""
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
		return fmt.Errorf("@skip is reachable from %s: %s", root, res.reason)
	case scanDynamic:
		return fmt.Errorf("@@dynamic/type_builder-like content is reachable from %s: %s", root, res.reason)
	default:
		return fmt.Errorf("%s graph cannot be resolved faithfully: %s", root, res.reason)
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
// argument list, retaining each parsed type (nil for a bare argument) for a
// later value-conversion phase.
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
