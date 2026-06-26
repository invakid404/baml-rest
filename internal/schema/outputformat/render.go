package outputformat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

// inlineRenderEnumMaxValues is BAML's INLINE_RENDER_ENUM_MAX_VALUES: an enum
// with more than this many values is hoisted rather than rendered inline.
const inlineRenderEnumMaxValues = 6

// Render produces the BAML-equivalent `ctx.output_format` text for bundle.
//
// It mirrors OutputFormatContent::render. A zero-value [Options] reproduces
// BAML RenderOptions::default. The bundle's ordered slices are the canonical
// render order; Render never reorders them and never lets Go map iteration
// drive emitted order.
//
// Render validates the bundle with [schema.Bundle.ValidateOutput] first,
// which both rebuilds the lookup indexes and rejects the output-illegal
// TypeIR variants (tuple, arrow, top, media). Like BAML — which returns
// `None` for an empty render and displays that as an empty string — Render
// returns ("", nil) when the rendered output is empty. It returns a non-nil
// error for an unresolved reference or an unsupported output type (matching
// BAML's render-time minijinja errors), so callers never silently get an
// empty string for a real failure.
func Render(bundle *schema.Bundle, opts Options) (string, error) {
	if bundle == nil {
		return "", fmt.Errorf("outputformat: nil bundle")
	}
	if err := bundle.ValidateOutput(); err != nil {
		return "", err
	}
	r := &renderer{b: bundle, opts: opts}
	return r.render()
}

// renderer carries the immutable inputs through the recursive render. The
// per-render hoist state lives in [renderCtx].
type renderer struct {
	b    *schema.Bundle
	opts Options
}

// renderCtx is BAML's RenderCtx: the set of enum and class names hoisted to
// top-level definitions for this render. Both are insertion-ordered sets
// (BAML IndexSet); membership decides whether a reference renders as a name
// or inline, and the order decides definition emission order.
type renderCtx struct {
	hoistedEnums   *orderedSet
	hoistedClasses *orderedSet
}

func (r *renderer) render() (string, error) {
	ctx := &renderCtx{
		hoistedEnums: newOrderedSet(),
		// Recursive classes are always hoisted, so seed with them in slice
		// order (BAML clones recursive_classes as the hoisted_classes base).
		hoistedClasses: newOrderedSet(),
	}
	for _, name := range r.b.RecursiveClasses {
		ctx.hoistedClasses.add(name)
	}

	// Precompute hoisted enums in enum-slice order: hoist when the value
	// count exceeds the inline max, any value carries a description, or the
	// always-hoist option forces it.
	for i := range r.b.Enums {
		e := &r.b.Enums[i]
		if len(e.Values) > inlineRenderEnumMaxValues || enumHasDescription(e) || r.opts.AlwaysHoistEnums.isTrue() {
			ctx.hoistedEnums.add(e.Name.Name)
		}
	}

	// Apply HoistClasses after recursive classes.
	switch r.opts.HoistClasses.Mode {
	case HoistAuto:
		// Nothing beyond recursive classes.
	case HoistAll:
		for i := range r.b.Classes {
			ctx.hoistedClasses.add(r.b.Classes[i].Name.Name)
		}
	case HoistSubset:
		notFound := newOrderedSet()
		for _, name := range r.opts.HoistClasses.Subset {
			if r.classExistsEitherMode(name) {
				ctx.hoistedClasses.add(name)
			} else {
				notFound.add(name)
			}
		}
		if len(notFound.items) > 0 {
			return "", hoistSubsetError(notFound.items)
		}
	}

	// Schema prefix ("Answer in JSON using ...").
	prefix, prefixPresent := r.prefix(ctx)

	message, hasMessage, err := r.message(ctx, prefixPresent)
	if err != nil {
		return "", err
	}

	// Top-level hoisted class uses its canonical name instead of the inline
	// schema (which is emitted as a hoisted definition instead).
	if r.b.Target.Kind == schema.TypeClass && ctx.hoistedClasses.contains(r.b.Target.Name) {
		message = r.b.Target.Name
		hasMessage = true
	}

	// Top-level hoisted enum suppresses the message; its definition (with the
	// prefix prepended) is emitted in the hoisted-enum block instead.
	targetIsHoistedEnum := r.b.Target.Kind == schema.TypeEnum &&
		ctx.hoistedEnums.contains(r.b.Target.Name)
	if targetIsHoistedEnum {
		message = ""
		hasMessage = false
	}

	classDefs, err := r.classDefinitions(ctx)
	if err != nil {
		return "", err
	}
	aliasDefs, err := r.aliasDefinitions(ctx)
	if err != nil {
		return "", err
	}
	enumDefs, err := r.enumDefinitions(ctx, targetIsHoistedEnum, prefix, prefixPresent)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	if len(enumDefs) > 0 {
		out.WriteString(strings.Join(enumDefs, "\n\n"))
		// Only add the blank-line separator when the target enum hasn't
		// already folded the prefix into its definition.
		if !targetIsHoistedEnum {
			out.WriteString("\n\n")
		}
	}
	if len(classDefs) > 0 {
		out.WriteString(strings.Join(classDefs, "\n\n"))
		out.WriteString("\n\n")
	}
	if len(aliasDefs) > 0 {
		// Recursive alias definitions are joined by single newlines, not
		// blank lines.
		out.WriteString(strings.Join(aliasDefs, "\n"))
		out.WriteString("\n\n")
	}
	if prefixPresent && !targetIsHoistedEnum {
		out.WriteString(prefix)
	}
	if hasMessage {
		out.WriteString(message)
	}

	// Trim trailing newlines only (BAML pops '\n' until none remain). An
	// empty result is BAML's None, surfaced as "".
	return strings.TrimRight(out.String(), "\n"), nil
}

// message computes BAML's top-level `message`: the rendered target schema, or
// None for a bare top-level string with no prefix. The class/enum hoisting
// special-cases are applied by the caller.
func (r *renderer) message(ctx *renderCtx, prefixPresent bool) (string, bool, error) {
	t := &r.b.Target
	switch {
	case t.Kind == schema.TypePrimitive && t.Primitive == schema.PrimitiveString && !prefixPresent:
		return "", false, nil
	case t.Kind == schema.TypeEnum:
		enm, ok := r.b.FindEnum(t.Name)
		if !ok {
			return "", false, fmt.Errorf("outputformat: enum %q not found", t.Name)
		}
		return r.enumToString(enm), true, nil
	default:
		s, err := r.innerTypeRender(t, ctx)
		if err != nil {
			return "", false, err
		}
		return s, true, nil
	}
}

// prefix mirrors OutputFormatContent::prefix: an explicit Always/Never
// setting wins, otherwise the auto prefix is derived from the target type and
// hoist state. The bool reports presence (BAML's Option<String>).
func (r *renderer) prefix(ctx *renderCtx) (string, bool) {
	switch r.opts.Prefix.mode {
	case settingAlways:
		return r.opts.Prefix.val, true
	case settingNever:
		return "", false
	default:
		return r.autoPrefix(ctx)
	}
}

func (r *renderer) autoPrefix(ctx *renderCtx) (string, bool) {
	t := &r.b.Target
	switch t.Kind {
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveString {
			return "", false
		}
		return fmt.Sprintf("Answer as %s ", indefiniteArticle(string(t.Primitive))), true
	case schema.TypeLiteral:
		return "Answer using this specific value:\n", true
	case schema.TypeEnum:
		return "Answer with any of the categories:\n", true
	case schema.TypeClass:
		// A line break after the colon for an inline schema; a trailing space
		// so a hoisted class name follows on the same line.
		end := "\n"
		if ctx.hoistedClasses.contains(t.Name) {
			end = " "
		}
		return fmt.Sprintf("Answer in JSON using this %s:%s", r.opts.typePrefixWord(), end), true
	case schema.TypeRecursiveAlias:
		return fmt.Sprintf("Answer in JSON using this %s: ", r.opts.typePrefixWord()), true
	case schema.TypeList:
		return "Answer with a JSON Array using this schema:\n", true
	case schema.TypeUnion:
		switch unionView(t.Union) {
		case viewNull:
			return "Answer ONLY with null:\n", true
		case viewOptional:
			return "Answer in JSON using this schema:\n", true
		default: // viewOneOf, viewOneOfOptional
			return "Answer in JSON using any of these schemas:\n", true
		}
	case schema.TypeMap:
		return "Answer in JSON using this schema:\n", true
	default:
		// Tuple/arrow/top yield no prefix (and are rejected by validation).
		return "", false
	}
}

// classDefinitions renders the hoisted classes in hoist order. Each
// definition uses the canonical class name (never the alias) and is rendered
// via inner_type_render directly, so the class body is the full inline schema
// even though the class is hoisted; nested hoisted references inside it still
// collapse to names.
func (r *renderer) classDefinitions(ctx *renderCtx) ([]string, error) {
	var defs []string
	for _, name := range ctx.hoistedClasses.items {
		// BAML re-renders through TypeIR::class(name), which forces
		// NonStreaming. The class body is the inline schema.
		ref := schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming}
		body, err := r.innerTypeRender(&ref, ctx)
		if err != nil {
			return nil, err
		}
		if prefix, ok := r.opts.HoistedClassPrefix.alwaysNonEmpty(); ok {
			defs = append(defs, fmt.Sprintf("%s %s %s", prefix, name, body))
		} else {
			defs = append(defs, fmt.Sprintf("%s %s", name, body))
		}
	}
	return defs, nil
}

// aliasDefinitions renders the structural recursive aliases in slice order,
// each as `Name = <target>` (with the optional hoisted-class prefix).
func (r *renderer) aliasDefinitions(ctx *renderCtx) ([]string, error) {
	var defs []string
	for i := range r.b.StructuralRecursiveAliases {
		a := &r.b.StructuralRecursiveAliases[i]
		target, err := r.innerTypeRender(&a.Target, ctx)
		if err != nil {
			return nil, err
		}
		if prefix, ok := r.opts.HoistedClassPrefix.alwaysNonEmpty(); ok {
			defs = append(defs, fmt.Sprintf("%s %s = %s", prefix, a.Name, target))
		} else {
			defs = append(defs, fmt.Sprintf("%s = %s", a.Name, target))
		}
	}
	return defs, nil
}

// enumDefinitions renders the hoisted enums in hoist order using rendered
// names. When the target itself is the hoisted enum, the prefix is folded
// into its definition (and the normal prefix/message emission is suppressed
// by the caller), avoiding a duplicate prefix.
func (r *renderer) enumDefinitions(ctx *renderCtx, targetIsHoistedEnum bool, prefix string, prefixPresent bool) ([]string, error) {
	var defs []string
	for _, name := range ctx.hoistedEnums.items {
		enm, ok := r.b.FindEnum(name)
		if !ok {
			return nil, fmt.Errorf("outputformat: enum %q not found", name)
		}
		enumStr := r.enumToString(enm)
		if targetIsHoistedEnum && name == enm.Name.Name && prefixPresent {
			enumStr = prefix + enumStr
		}
		defs = append(defs, enumStr)
	}
	return defs, nil
}

// innerTypeRender is the recursive schema-rendering entry point
// (OutputFormatContent::inner_type_render). It always renders a schema, never
// a hoisted name; nested children go through renderPossiblyHoisted.
func (r *renderer) innerTypeRender(t *schema.Type, ctx *renderCtx) (string, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		switch t.Primitive {
		case schema.PrimitiveString:
			return "string", nil
		case schema.PrimitiveInt:
			return "int", nil
		case schema.PrimitiveFloat:
			return "float", nil
		case schema.PrimitiveBool:
			return "bool", nil
		case schema.PrimitiveNull:
			return "null", nil
		default: // media — rejected by ValidateOutput, but fail closed.
			return "", fmt.Errorf("outputformat: type %q is not supported in outputs", t.Primitive)
		}
	case schema.TypeLiteral:
		return literalDisplay(t.Literal), nil
	case schema.TypeEnum:
		enm, ok := r.b.FindEnum(t.Name)
		if !ok {
			return "", fmt.Errorf("outputformat: enum %q not found", t.Name)
		}
		if ctx.hoistedEnums.contains(enm.Name.Name) {
			return enm.Name.RenderedName(), nil
		}
		parts := make([]string, 0, len(enm.Values))
		for i := range enm.Values {
			parts = append(parts, "'"+enm.Values[i].Name.RenderedName()+"'")
		}
		return strings.Join(parts, r.opts.orSplitter()), nil
	case schema.TypeClass:
		cls, ok := r.b.FindClass(t.Name, t.Mode)
		if !ok {
			return "", fmt.Errorf("outputformat: class %q not found", t.Name)
		}
		return r.classRender(cls, ctx)
	case schema.TypeRecursiveAlias:
		return t.Name, nil
	case schema.TypeList:
		return r.listRender(t, ctx)
	case schema.TypeUnion:
		members := unionIterIncludeNull(t.Union)
		parts := make([]string, 0, len(members))
		for i := range members {
			s, err := r.renderPossiblyHoisted(&members[i], ctx)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, r.opts.orSplitter()), nil
	case schema.TypeMap:
		return r.mapRender(t, ctx)
	default: // tuple, arrow, top — rejected by ValidateOutput.
		return "", fmt.Errorf("outputformat: %q type is not supported in outputs", t.Kind)
	}
}

// renderPossiblyHoisted stops recursion at a hoisted class, returning its
// canonical name instead of descending into the schema
// (OutputFormatContent::render_possibly_hoisted_type).
func (r *renderer) renderPossiblyHoisted(t *schema.Type, ctx *renderCtx) (string, error) {
	if t.Kind == schema.TypeClass && ctx.hoistedClasses.contains(t.Name) {
		return t.Name, nil
	}
	return r.innerTypeRender(t, ctx)
}

// classRender renders a class schema (ClassRender's Display impl): an opening
// brace, optional class-description comment block, then each field's optional
// description comment and `name: type,` line, closing with a brace. Field
// names use rendered (alias) names; child types are indented one level.
func (r *renderer) classRender(cls *schema.ClassDef, ctx *renderCtx) (string, error) {
	var sb strings.Builder
	sb.WriteString("{\n")
	if cls.Description != nil {
		desc := strings.TrimSpace(*cls.Description)
		if desc != "" {
			for _, line := range strings.Split(desc, "\n") {
				sb.WriteString("  // ")
				sb.WriteString(line)
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}
	for i := range cls.Fields {
		f := &cls.Fields[i]
		if f.Description != nil {
			sb.WriteString("  // ")
			sb.WriteString(strings.ReplaceAll(*f.Description, "\n", "\n  // "))
			sb.WriteString("\n")
		}
		typeStr, err := r.renderPossiblyHoisted(&f.Type, ctx)
		if err != nil {
			return "", err
		}
		typeStr = strings.ReplaceAll(typeStr, "\n", "\n  ")
		name := f.Name.RenderedName()
		if r.opts.QuoteClassFields {
			sb.WriteString("  \"")
			sb.WriteString(name)
			sb.WriteString("\": ")
		} else {
			sb.WriteString("  ")
			sb.WriteString(name)
			sb.WriteString(": ")
		}
		sb.WriteString(typeStr)
		sb.WriteString(",\n")
	}
	sb.WriteString("}")
	return sb.String(), nil
}

// enumToString renders an enum definition (EnumRender's Display impl): the
// rendered enum name, a `----` delimiter, then one prefixed line per value
// using the rendered value name and its optional description.
func (r *renderer) enumToString(enm *schema.EnumDef) string {
	var sb strings.Builder
	sb.WriteString(enm.Name.RenderedName())
	sb.WriteString("\n----")
	valuePrefix := r.opts.enumValuePrefix()
	for i := range enm.Values {
		v := &enm.Values[i]
		sb.WriteString("\n")
		sb.WriteString(valuePrefix)
		if v.Description != nil {
			sb.WriteString(v.Name.RenderedName())
			sb.WriteString(": ")
			sb.WriteString(strings.ReplaceAll(*v.Description, "\n", "\n  "))
		} else {
			sb.WriteString(v.Name.RenderedName())
		}
	}
	return sb.String()
}

// listRender renders a list type, reproducing BAML's irregular wrapping
// rules: primitives stay inline as `<elem>[]`; a hoisted element stays a bare
// name; an inline enum wraps only when its rendered string exceeds 15 chars; a
// union wraps to a multiline array only when every member (including the
// appended null) is non-primitive, otherwise renders as `(<union>)[]`; every
// other complex element wraps to a multiline array.
func (r *renderer) listRender(t *schema.Type, ctx *renderCtx) (string, error) {
	inner := t.Elem

	isHoisted := false
	switch inner.Kind {
	case schema.TypeClass:
		isHoisted = ctx.hoistedClasses.contains(inner.Name)
	case schema.TypeRecursiveAlias:
		_, isHoisted = r.b.FindRecursiveAlias(inner.Name)
	}

	innerStr, err := r.renderPossiblyHoisted(inner, ctx)
	if err != nil {
		return "", err
	}

	complex := false
	switch inner.Kind {
	case schema.TypePrimitive:
		complex = false
	case schema.TypeEnum:
		complex = len(innerStr) > 15
	case schema.TypeUnion:
		complex = true
		for _, m := range unionIterIncludeNull(inner.Union) {
			if isPrimitive(&m) {
				complex = false
				break
			}
		}
	default:
		complex = true
	}

	switch {
	case !isHoisted && complex:
		return "[\n  " + strings.ReplaceAll(innerStr, "\n", "\n  ") + "\n]", nil
	case inner.Kind == schema.TypeUnion:
		return "(" + innerStr + ")[]", nil
	default:
		return innerStr + "[]", nil
	}
}

// mapRender renders a map type through the configured style. Key and value go
// through renderPossiblyHoisted so a hoisted class value collapses to a name.
func (r *renderer) mapRender(t *schema.Type, ctx *renderCtx) (string, error) {
	key, err := r.renderPossiblyHoisted(t.Key, ctx)
	if err != nil {
		return "", err
	}
	val, err := r.renderPossiblyHoisted(t.Value, ctx)
	if err != nil {
		return "", err
	}
	if r.opts.MapStyle == MapStyleObject {
		return "{" + key + ": " + val + "}", nil
	}
	return "map<" + key + ", " + val + ">", nil
}

// classExistsEitherMode reports whether a class with the canonical name
// exists under either streaming mode, matching BAML's subset-hoist existence
// check.
func (r *renderer) classExistsEitherMode(name string) bool {
	if _, ok := r.b.FindClass(name, schema.NonStreaming); ok {
		return true
	}
	_, ok := r.b.FindClass(name, schema.Streaming)
	return ok
}

// literalDisplay reproduces BAML's LiteralValue Display: strings are wrapped
// in double quotes verbatim (no JSON escaping), ints and bools render bare.
func literalDisplay(l *schema.LiteralValue) string {
	switch l.Kind {
	case schema.LiteralString:
		return "\"" + l.String + "\""
	case schema.LiteralInt:
		return strconv.FormatInt(l.Int, 10)
	case schema.LiteralBool:
		return strconv.FormatBool(l.Bool)
	default:
		return ""
	}
}

// enumHasDescription reports whether any value of the enum carries a
// description (one of BAML's automatic hoist conditions).
func enumHasDescription(e *schema.EnumDef) bool {
	for i := range e.Values {
		if e.Values[i].Description != nil {
			return true
		}
	}
	return false
}

// indefiniteArticle is BAML's indefinite_article_a_or_an: "an" before a word
// starting with a/e/i/o/u, else "a". It is a simple heuristic, not full
// English grammar.
func indefiniteArticle(word string) string {
	if word == "" {
		return "a"
	}
	switch word[0] {
	case 'a', 'A', 'e', 'E', 'i', 'I', 'o', 'O', 'u', 'U':
		return "an"
	default:
		return "a"
	}
}

// hoistSubsetError reproduces BAML's "Cannot hoist ..." render error for a
// HoistClasses::Subset naming classes that do not exist.
func hoistSubsetError(notFound []string) error {
	classOrClasses, itDoesOrTheyDo := "classes", "they do"
	if len(notFound) == 1 {
		classOrClasses, itDoesOrTheyDo = "class", "it does"
	}
	quoted := make([]string, len(notFound))
	for i, c := range notFound {
		quoted[i] = "\"" + c + "\""
	}
	return fmt.Errorf("outputformat: Cannot hoist %s %s because %s not exist",
		classOrClasses, strings.Join(quoted, ", "), itDoesOrTheyDo)
}
