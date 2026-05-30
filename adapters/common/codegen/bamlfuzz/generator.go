package bamlfuzz

import (
	"fmt"
	"strings"

	"pgregory.net/rapid"
)

// Bounds for the v1 grammar. Tracked together so the matching
// invariant test can assert no schema/value exceeds the budget.
const (
	MaxClasses        = 4
	MaxEnums          = 3
	MinFieldsPerClass = 1
	MaxFieldsPerClass = 5
	MaxTypeDepth      = 4
	MaxListLen        = 3
	MaxMapLen         = 3
	MaxValueRecursion = 2
	EnumValuesMin     = 1
	EnumValuesMax     = 4
	// MinUnionVariants is the smallest legal arm count drawn by the
	// generator. Single-arm unions are reachable only through the
	// shrink-collapse pass (Move B), never the random draw.
	MinUnionVariants = 2
	// MaxUnionVariants caps random union arm counts. Larger unions
	// quickly dominate coverage without exercising additional code
	// paths in the emitters, so the upper bound stays modest.
	MaxUnionVariants = 3
	// UnionDrawProbability is the chance that an eligible drawType
	// slot is filled with a KindUnion. Tuned low enough that
	// non-union shapes still dominate random corpora — too high and
	// every random case becomes a union test.
	UnionDrawProbability = 0.15
)

// classNameFor / enumNameFor / fieldNameFor produce the stable
// generator-name convention: classes FuzzClass0..FuzzClass{N-1}, enums
// FuzzEnum0..FuzzEnum{N-1}, fields Fuzz_field_0.. per class (the index
// is local to the class so fields don't bleed names across
// declarations).
func classNameFor(i int) string { return fmt.Sprintf("FuzzClass%d", i) }
func enumNameFor(i int) string  { return fmt.Sprintf("FuzzEnum%d", i) }
func enumValueFor(eIdx, vIdx int) string {
	return fmt.Sprintf("FuzzEnum%d_V%d", eIdx, vIdx)
}
func fieldNameFor(i int) string { return fmt.Sprintf("Fuzz_field_%d", i) }

// SchemaGenOptions configures the schema generator. Callers usually
// pick the dynamic-safe or static entry-point helpers below; this
// struct is exposed for tests that need a specific shape.
type SchemaGenOptions struct {
	// AllowSelfRef, when true, lets the generator emit one
	// self-referential class (the root). Static emission supports
	// self-ref; dynamic emission does not.
	AllowSelfRef bool
	// SelfRefProbability is the chance, in [0,1], that AllowSelfRef
	// actually produces a self-ref class. The scope doc settled on
	// 10–20% of static random cases; tests pin a fixed value.
	SelfRefProbability float64
	// MutualCycleProbability is the chance, in [0,1], that the
	// generator injects a two-class mutual cycle (A → optional<B>
	// and B → optional<A>). Mutual-cycle schemas have
	// HasMutualCycle=true and stamp RequiresDynamicSkip=true: the
	// BAML cgo TypeBuilder aborts on mutual-cycle dynamic schemas
	// today (TODO(upstream-mutual-rec-dynamic-crash)), so dynamic
	// emission is gated. Static .baml emission supports mutual
	// cycles cleanly. The value generator terminates traversal via
	// the per-class recursion cap so the IR + walker side is ready
	// for either path.
	MutualCycleProbability float64
}

// DynamicSafeSchemaGen returns a rapid generator for FuzzSchema
// values that the dynamic TypeBuilder path can safely realize:
//   - no self-ref (upstream BAML TypeBuilder cannot express self-ref;
//     TODO(upstream-self-ref))
//   - no mutual cycle between distinct classes (the BAML cgo
//     TypeBuilder aborts on mutual-cycle dynamic schemas with a
//     signal-level fault; TODO(upstream-mutual-rec-dynamic-crash))
//
// Mutual cycles return to the rotation once upstream BAML stops
// aborting on them; the value generator already terminates cycles at
// the per-class recursion cap so the IR + walker side is ready.
func DynamicSafeSchemaGen() *rapid.Generator[FuzzSchema] {
	return SchemaGen(SchemaGenOptions{
		AllowSelfRef:           false,
		MutualCycleProbability: 0,
	})
}

// StaticSchemaGen returns a generator for FuzzSchema values the
// static .baml emitter can render. Self-ref schemas and mutual-cycle
// schemas are both permitted at the configured probabilities; both
// shapes stamp RequiresDynamicSkip=true (self-ref via
// TODO(upstream-self-ref), mutual cycle via
// TODO(upstream-mutual-rec-dynamic-crash)) and so are gated at the
// dynamic emitter, but the static .baml lowering supports them.
func StaticSchemaGen() *rapid.Generator[FuzzSchema] {
	return SchemaGen(SchemaGenOptions{
		AllowSelfRef:           true,
		SelfRefProbability:     0.15,
		MutualCycleProbability: 0.15,
	})
}

// SchemaGen returns a rapid generator for FuzzSchema honoring the
// given options. The output's graph-metadata flags (HasSelfRef etc.)
// are derived from the actual class-ref graph via AnalyzeGraph, not
// from `opts`: opts only influences the generation strategy.
func SchemaGen(opts SchemaGenOptions) *rapid.Generator[FuzzSchema] {
	return rapid.Custom(func(t *rapid.T) FuzzSchema {
		return drawSchema(t, opts)
	})
}

func drawSchema(t *rapid.T, opts SchemaGenOptions) FuzzSchema {
	numEnums := rapid.IntRange(0, MaxEnums).Draw(t, "num_enums")
	numClasses := rapid.IntRange(1, MaxClasses).Draw(t, "num_classes")

	enums := make([]FuzzEnum, numEnums)
	for i := 0; i < numEnums; i++ {
		enums[i] = drawEnum(t, i)
	}

	classNames := make([]string, numClasses)
	for i := 0; i < numClasses; i++ {
		classNames[i] = classNameFor(i)
	}

	// Decide whether the root class gets a self-ref edge. Static
	// mode wires a self-ref through an optional or list/map so the
	// value walker can terminate by emitting null / empty at the
	// depth cap.
	selfRefRoot := false
	if opts.AllowSelfRef && numClasses >= 1 {
		// Bias deterministically based on opts.SelfRefProbability:
		// rapid.Float64Range(0, 1) for the dice; if <
		// probability, enable.
		dice := rapid.Float64Range(0, 1).Draw(t, "self_ref_dice")
		if dice < opts.SelfRefProbability {
			selfRefRoot = true
		}
	}

	classes := make([]FuzzClass, numClasses)
	for i := 0; i < numClasses; i++ {
		// Forward-only refs at draw time keep the initial graph a
		// DAG. The mutual-cycle injection below adds the only
		// back-edges in the schema; the value generator
		// terminates cycles via the per-class recursion cap.
		var refTargets []string
		if i+1 < numClasses {
			refTargets = append(refTargets, classNames[i+1:]...)
		}
		classes[i] = drawClass(t, i, classNames[i], refTargets, enums)
	}

	mutualCycle := false
	if numClasses >= 2 && opts.MutualCycleProbability > 0 {
		dice := rapid.Float64Range(0, 1).Draw(t, "mutual_cycle_dice")
		mutualCycle = dice < opts.MutualCycleProbability
	}
	if mutualCycle {
		injectMutualCycle(t, classes)
	}
	if selfRefRoot {
		injectSelfRef(&classes[0])
	}

	schema := FuzzSchema{
		Classes:   classes,
		Enums:     enums,
		RootClass: classNames[0],
	}
	return AnalyzeGraph(schema)
}

func drawEnum(t *rapid.T, idx int) FuzzEnum {
	numValues := rapid.IntRange(EnumValuesMin, EnumValuesMax).Draw(t, fmt.Sprintf("enum_%d_count", idx))
	values := make([]string, numValues)
	for j := 0; j < numValues; j++ {
		values[j] = enumValueFor(idx, j)
	}
	return FuzzEnum{
		Name:   enumNameFor(idx),
		Values: values,
	}
}

func drawClass(t *rapid.T, idx int, name string, refTargets []string, enums []FuzzEnum) FuzzClass {
	numFields := rapid.IntRange(MinFieldsPerClass, MaxFieldsPerClass).Draw(t, fmt.Sprintf("class_%d_fields", idx))
	props := make([]FuzzProperty, numFields)
	for j := 0; j < numFields; j++ {
		fieldType := drawType(t, fmt.Sprintf("class_%d_field_%d", idx, j), refTargets, enums, 0)
		props[j] = FuzzProperty{
			Name: fieldNameFor(j),
			Type: fieldType,
		}
	}
	return FuzzClass{Name: name, Properties: props}
}

// drawType picks a type spec, recursively. `depth` counts wrapper
// nesting (optional / list / map); class refs do not advance depth
// because their nesting is bounded by the class graph instead.
//
// `refTargets` is the set of class names this type may ClassRef.
// Empty disables class refs (last-in-DAG-order leaf classes), in
// which case the generator picks a primitive / literal / enum / list
// / map / optional instead.
func drawType(t *rapid.T, label string, refTargets []string, enums []FuzzEnum, depth int) FuzzType {
	// Atomic kinds always available; refs available only when
	// targets exist; wrappers available only while we have depth
	// budget remaining.
	atomicKinds := []FuzzTypeKind{KindString, KindInt, KindFloat, KindBool, KindNull, KindLiteral}
	if len(enums) > 0 {
		atomicKinds = append(atomicKinds, KindEnumRef)
	}
	if len(refTargets) > 0 {
		atomicKinds = append(atomicKinds, KindClassRef)
	}

	choices := append([]FuzzTypeKind(nil), atomicKinds...)
	if depth+1 < MaxTypeDepth {
		choices = append(choices, KindOptional, KindList, KindMap)
		// Union counts toward MaxTypeDepth the same way the other
		// wrappers do: each variant is drawn at depth+1 so a chain
		// of nested unions still terminates on the existing budget.
		// A separate dice roll gates the union path so the wrapper
		// mix isn't skewed by adding a fourth equally-weighted kind.
		if rapid.Float64Range(0, 1).Draw(t, label+":union_dice") < UnionDrawProbability {
			return drawUnion(t, label, refTargets, enums, depth+1)
		}
	}

	kind := drawKind(t, label, choices)
	switch kind {
	case KindString, KindInt, KindFloat, KindBool, KindNull:
		return FuzzType{Kind: kind}
	case KindLiteral:
		return FuzzType{Kind: KindLiteral, Literal: drawLiteral(t, label)}
	case KindEnumRef:
		eIdx := rapid.IntRange(0, len(enums)-1).Draw(t, label+":enum_idx")
		return FuzzType{Kind: KindEnumRef, Ref: enums[eIdx].Name}
	case KindClassRef:
		cIdx := rapid.IntRange(0, len(refTargets)-1).Draw(t, label+":cls_idx")
		return FuzzType{Kind: KindClassRef, Ref: refTargets[cIdx]}
	case KindOptional:
		inner := drawType(t, label+":opt_inner", refTargets, enums, depth+1)
		return FuzzType{Kind: KindOptional, Inner: &inner}
	case KindList:
		inner := drawType(t, label+":list_inner", refTargets, enums, depth+1)
		return FuzzType{Kind: KindList, Inner: &inner}
	case KindMap:
		// v1 grammar restricts map keys to string.
		key := FuzzType{Kind: KindString}
		inner := drawType(t, label+":map_inner", refTargets, enums, depth+1)
		return FuzzType{Kind: KindMap, Key: &key, Inner: &inner}
	}
	// Unreachable; drawKind always returns one of the choices above.
	return FuzzType{Kind: KindString}
}

// drawUnion draws a KindUnion type with rapid-controlled arm count
// in [MinUnionVariants, MaxUnionVariants]. The arm count is drawn
// with a separate `variant_count` label so rapid's shrinker can drag
// it toward the lower bound independently of arm contents — this is
// the "Move A" shrink behaviour from the scope doc. Each variant is
// drawn at `depth` already advanced by the caller.
func drawUnion(t *rapid.T, label string, refTargets []string, enums []FuzzEnum, depth int) FuzzType {
	count := rapid.IntRange(MinUnionVariants, MaxUnionVariants).Draw(t, label+":variant_count")
	variants := make([]FuzzType, count)
	for i := 0; i < count; i++ {
		variants[i] = drawType(t, fmt.Sprintf("%s:variant_%d", label, i), refTargets, enums, depth)
	}
	return FuzzType{Kind: KindUnion, Variants: variants}
}

func drawKind(t *rapid.T, label string, choices []FuzzTypeKind) FuzzTypeKind {
	idx := rapid.IntRange(0, len(choices)-1).Draw(t, label+":kind_idx")
	return choices[idx]
}

func drawLiteral(t *rapid.T, label string) *FuzzLiteral {
	// Integer literals (LiteralInt) are intentionally excluded. BAML
	// validates `field (0 | bool | 42)` style class properties as
	// "not a valid field or attribute definition" — the grammar does
	// not accept integer literals as union variants, and the static
	// emitter regularly surfaces them inside unions. Negative integer
	// literals additionally tripped the Go codegen identifier path
	// (`expected ';', found '-'` in baml_client/stream_types/classes.go)
	// when they reached the post-codegen Go parser. Dropping the kind
	// entirely keeps every drawn schema compilable by BAML;
	// integer-literal coverage can be reinstated when BAML's grammar /
	// Go codegen is verified to accept the shape end-to-end.
	kindChoices := []FuzzLiteralKind{LiteralString, LiteralBool}
	kidx := rapid.IntRange(0, len(kindChoices)-1).Draw(t, label+":lit_kind")
	switch kindChoices[kidx] {
	case LiteralString:
		// Pick from a small pinned set of string literals — these
		// double as edge cases (empty, alphanumeric, contains
		// quote).
		options := []string{"alpha", "beta", "gamma", "", `"quoted"`}
		oidx := rapid.IntRange(0, len(options)-1).Draw(t, label+":lit_str")
		return &FuzzLiteral{Kind: LiteralString, String: options[oidx]}
	case LiteralBool:
		b := rapid.Bool().Draw(t, label+":lit_bool")
		return &FuzzLiteral{Kind: LiteralBool, Bool: b}
	}
	// Fail closed: a literal kind appeared in kindChoices that the
	// switch above does not handle. The previous string-literal
	// fallback silently masked a kindChoices reintroduction of
	// LiteralInt (the switch would skip the int arm and the function
	// would emit a string literal under the LiteralString tag),
	// defeating `TestSchemaGenDoesNotProduceLiteralInt`'s mutation
	// guard. A fatal here pins the contract: every entry in
	// kindChoices must have a matching arm, so a future widening is
	// visible at test time.
	t.Fatalf("drawLiteral: unhandled literal kind %v", kindChoices[kidx])
	return nil
}

// injectSelfRef replaces `cls`'s last property with a self-
// referential optional class field. Replacement (not append) keeps
// the per-class field count inside the [MinFieldsPerClass,
// MaxFieldsPerClass] budget. The wrapper is always KindOptional so
// the value generator terminates at depth cap by emitting null /
// absent. A required class-ref self-edge would never terminate.
func injectSelfRef(cls *FuzzClass) {
	if len(cls.Properties) == 0 {
		return
	}
	last := len(cls.Properties) - 1
	self := FuzzType{Kind: KindClassRef, Ref: cls.Name}
	opt := FuzzType{Kind: KindOptional, Inner: &self}
	cls.Properties[last] = FuzzProperty{
		Name: cls.Properties[last].Name,
		Type: opt,
	}
}

// injectMutualCycle replaces one field in each of two distinct
// classes with a back-edge optional<other>, creating an A↔B mutual
// cycle through "other" classes. Replacement (not append) keeps
// each class within the field-count budget. Cycle traversal at
// value-generation time is bounded by the per-class recursion cap.
func injectMutualCycle(t *rapid.T, classes []FuzzClass) {
	if len(classes) < 2 {
		return
	}
	a := rapid.IntRange(0, len(classes)-1).Draw(t, "mutual_a")
	b := rapid.IntRange(0, len(classes)-1).Draw(t, "mutual_b")
	if a == b {
		b = (a + 1) % len(classes)
	}
	wireBackEdge(&classes[a], classes[b].Name)
	wireBackEdge(&classes[b], classes[a].Name)
}

func wireBackEdge(cls *FuzzClass, targetClassName string) {
	if len(cls.Properties) == 0 {
		return
	}
	last := len(cls.Properties) - 1
	ref := FuzzType{Kind: KindClassRef, Ref: targetClassName}
	opt := FuzzType{Kind: KindOptional, Inner: &ref}
	cls.Properties[last] = FuzzProperty{
		Name: cls.Properties[last].Name,
		Type: opt,
	}
}

// ValueGen returns a generator that produces a FuzzValue conforming
// to the schema's effective root type. When the schema declares a
// non-class root via RootType the generator walks that type
// directly; otherwise it dispatches to the RootClass class
// generator. The generator threads a per-class recursion depth
// counter through the walk and forces termination at the depth cap
// by emitting OptionalAbsent / empty list / empty map for any
// nested edge that would recurse further into the same class.
func ValueGen(schema FuzzSchema) *rapid.Generator[FuzzValue] {
	return rapid.Custom(func(t *rapid.T) FuzzValue {
		// Rapid requires every Custom invocation to draw at
		// least once from the bitstream. Schemas whose property
		// graph is entirely literal would otherwise generate no
		// draws and trip rapid's "did not use any data" guard.
		_ = rapid.Bool().Draw(t, "value_gen_sentinel")
		ctx := &valueDrawCtx{
			schema: schema,
			depth:  make(map[string]int),
		}
		root := schema.EffectiveRoot()
		return ctx.drawValueForType(t, root, "root")
	})
}

type valueDrawCtx struct {
	schema FuzzSchema
	depth  map[string]int
}

func (c *valueDrawCtx) drawClass(t *rapid.T, className, label string) FuzzValue {
	cls, ok := c.schema.FindClass(className)
	if !ok {
		t.Fatalf("bamlfuzz: drawClass cannot find class %q (label=%s); generator and schema state are inconsistent",
			className, label)
	}
	c.depth[className]++
	defer func() { c.depth[className]-- }()
	fields := make([]FuzzFieldValue, len(cls.Properties))
	for i, prop := range cls.Properties {
		flabel := label + "." + prop.Name
		fv := c.drawValueForType(t, prop.Type, flabel)
		fields[i] = FuzzFieldValue{Name: prop.Name, Value: fv}
	}
	return FuzzValue{
		Kind:      KindClassRef,
		ClassName: className,
		Fields:    fields,
	}
}

func (c *valueDrawCtx) drawValueForType(t *rapid.T, ft FuzzType, label string) FuzzValue {
	switch ft.Kind {
	case KindString:
		return FuzzValue{Kind: KindString, String: drawString(t, label)}
	case KindInt:
		return FuzzValue{Kind: KindInt, Int: drawInt(t, label)}
	case KindFloat:
		return FuzzValue{Kind: KindFloat, Float: drawFloat(t, label)}
	case KindBool:
		return FuzzValue{Kind: KindBool, Bool: rapid.Bool().Draw(t, label)}
	case KindNull:
		return FuzzValue{Kind: KindNull}
	case KindLiteral:
		return c.drawLiteralValue(ft.Literal)
	case KindEnumRef:
		e, ok := c.schema.FindEnum(ft.Ref)
		if !ok {
			t.Fatalf("bamlfuzz: enum ref %q not found in schema (label=%s)", ft.Ref, label)
		}
		if len(e.Values) == 0 {
			t.Fatalf("bamlfuzz: enum %q has no values (label=%s); generator and schema state are inconsistent",
				ft.Ref, label)
		}
		idx := rapid.IntRange(0, len(e.Values)-1).Draw(t, label+":enum_pick")
		return FuzzValue{Kind: KindEnumRef, Enum: e.Values[idx]}
	case KindOptional:
		return c.drawOptional(t, ft, label)
	case KindList:
		return c.drawList(t, ft, label)
	case KindMap:
		return c.drawMap(t, ft, 0, label)
	case KindClassRef:
		// Required class ref: if recursion budget for the target
		// is exhausted, this shape is unreachable from the
		// schema generator — the only required class-ref edges
		// are DAG ones, where the target is a distinct class with
		// its own depth counter, so the cap never fires here.
		// Self-ref / cycle edges are always wrapped in optional/
		// list/map (handled above) where termination is enforced.
		return c.drawClass(t, ft.Ref, label)
	case KindUnion:
		return c.drawUnion(t, ft, label)
	}
	return FuzzValue{Kind: KindString}
}

// drawUnion picks one variant arm and recurses into its type.
// Variants whose realization would blow the per-class recursion
// budget are filtered out before the arm draw so the rapid bitstream
// shrinks toward terminating arms first. When every arm is unsafe
// the helper falls back to any arm (the inner draw still enforces
// optional/list/map termination); this is a defensive branch the
// schema generator's depth bounds make unreachable.
//
// When the picked arm is a direct KindMap and a sibling arm reaches a
// class_ref (directly or through nested unions/optional wrappers), the
// map is drawn with len >= 1. An empty map renders as the wire bytes
// `{}`, which BAML's union resolver coerces to the class arm with
// null-materialized fields — diverging from the walker's prediction.
// Forcing the map to carry at least one entry keeps the walker's
// recorded arm choice the one BAML lands on.
func (c *valueDrawCtx) drawUnion(t *rapid.T, ft FuzzType, label string) FuzzValue {
	if len(ft.Variants) == 0 {
		t.Fatalf("bamlfuzz: union has no variants (label=%s); schema is malformed", label)
	}
	safe := make([]int, 0, len(ft.Variants))
	for i := range ft.Variants {
		v := ft.Variants[i]
		if !c.wouldExceedDepth(&v) {
			safe = append(safe, i)
		}
	}
	if len(safe) == 0 {
		// Every arm reaches an already-exhausted class. Fall through
		// to the full index range — the inner draw still enforces
		// termination at the next optional/list/map wrapper.
		for i := range ft.Variants {
			safe = append(safe, i)
		}
	}
	pick := rapid.IntRange(0, len(safe)-1).Draw(t, label+":variant_pick")
	idx := safe[pick]
	arm := ft.Variants[idx]
	armLabel := fmt.Sprintf("%s:variant_%d", label, idx)
	var inner FuzzValue
	if arm.Kind == KindMap && unionHasClassReachableSibling(ft, idx) {
		inner = c.drawMap(t, arm, 1, armLabel)
	} else {
		inner = c.drawValueForType(t, arm, armLabel)
	}
	return FuzzValue{
		Kind:         KindUnion,
		VariantIndex: idx,
		Variant:      &inner,
	}
}

// unionHasClassReachableSibling reports whether any variant of `union`
// at an index other than `excludeIdx` is, or reaches via nested unions
// and optional wrappers, a KindClassRef. The check exists so the
// value generator can avoid producing an empty map `{}` for a union's
// direct map arm when a class is reachable through a sibling: an
// empty `{}` is byte-identical to an empty class instance, and BAML's
// union resolver prefers the class — null-materializing its declared
// fields — over the empty map. Recursion is bounded by MaxTypeDepth +
// 1 so a malformed cyclic union literal cannot loop the walk.
func unionHasClassReachableSibling(union FuzzType, excludeIdx int) bool {
	if union.Kind != KindUnion {
		return false
	}
	for i, v := range union.Variants {
		if i == excludeIdx {
			continue
		}
		if typeReachesClassRef(v, 0) {
			return true
		}
	}
	return false
}

func typeReachesClassRef(t FuzzType, depth int) bool {
	if depth > MaxTypeDepth {
		return false
	}
	switch t.Kind {
	case KindClassRef:
		return true
	case KindOptional:
		if t.Inner == nil {
			return false
		}
		return typeReachesClassRef(*t.Inner, depth+1)
	case KindUnion:
		for _, v := range t.Variants {
			if typeReachesClassRef(v, depth+1) {
				return true
			}
		}
		return false
	}
	return false
}

func (c *valueDrawCtx) drawOptional(t *rapid.T, ft FuzzType, label string) FuzzValue {
	// If the optional's inner type can reach a class we're already
	// nested inside at >= MaxValueRecursion, force termination by
	// emitting OptionalAbsent. This is the dominant termination
	// path for self-ref / cycle schemas.
	if c.wouldExceedDepth(ft.Inner) {
		// Pick between absent / null at random; both terminate.
		shape := OptionalAbsent
		if rapid.Bool().Draw(t, label+":term_shape") {
			shape = OptionalNull
		}
		return FuzzValue{Kind: KindOptional, OptionalShape: shape}
	}

	shape := drawOptionalShape(t, label)
	if shape != OptionalPresent {
		return FuzzValue{Kind: KindOptional, OptionalShape: shape}
	}
	inner := c.drawValueForType(t, *ft.Inner, label+":opt_present")
	return FuzzValue{
		Kind:          KindOptional,
		OptionalShape: OptionalPresent,
		Inner:         &inner,
	}
}

func drawOptionalShape(t *rapid.T, label string) FuzzOptionalShape {
	// Three-way split kept equally biased so every shape gets
	// regular exposure in random runs.
	idx := rapid.IntRange(0, 2).Draw(t, label+":opt_shape")
	switch idx {
	case 0:
		return OptionalPresent
	case 1:
		return OptionalAbsent
	default:
		return OptionalNull
	}
}

func (c *valueDrawCtx) drawList(t *rapid.T, ft FuzzType, label string) FuzzValue {
	maxLen := MaxListLen
	if c.wouldExceedDepth(ft.Inner) {
		maxLen = 0
	}
	length := rapid.IntRange(0, maxLen).Draw(t, label+":list_len")
	items := make([]FuzzValue, length)
	for i := 0; i < length; i++ {
		items[i] = c.drawValueForType(t, *ft.Inner, fmt.Sprintf("%s[%d]", label, i))
	}
	return FuzzValue{Kind: KindList, Items: items}
}

// drawMap draws a KindMap value with length in [minLen, MaxMapLen].
// When recursion budget is exhausted for the inner type the cap drops
// to 0 and a minLen > 0 is clamped down with it — termination beats
// the non-emptiness preference. Callers that want the standard "may
// be empty" behaviour pass minLen = 0; the union-ambiguity branch in
// drawUnion passes minLen = 1.
func (c *valueDrawCtx) drawMap(t *rapid.T, ft FuzzType, minLen int, label string) FuzzValue {
	maxLen := MaxMapLen
	if c.wouldExceedDepth(ft.Inner) {
		maxLen = 0
	}
	if minLen > maxLen {
		minLen = maxLen
	}
	length := rapid.IntRange(minLen, maxLen).Draw(t, label+":map_len")
	used := make(map[string]struct{}, length)
	entries := make([]FuzzMapEntry, 0, length)
	for i := 0; i < length; i++ {
		key := uniqueKey(t, fmt.Sprintf("%s:map_key_%d", label, i), used)
		val := c.drawValueForType(t, *ft.Inner, fmt.Sprintf("%s[%s]", label, key))
		entries = append(entries, FuzzMapEntry{Key: key, Value: val})
	}
	return FuzzValue{Kind: KindMap, MapEntries: entries}
}

// wouldExceedDepth reports whether realising `t` would recurse into
// a class we're already at the MaxValueRecursion depth for. Treats
// every class-ref edge reachable through the type as an immediate
// concern.
func (c *valueDrawCtx) wouldExceedDepth(t *FuzzType) bool {
	if t == nil {
		return false
	}
	refs := make(map[string]bool)
	collectClassRefs(*t, refs)
	if len(refs) == 0 {
		return false
	}
	reach := ReachabilityClosure(c.schema)
	for ref := range refs {
		if c.depth[ref] >= MaxValueRecursion {
			return true
		}
		for tgt, ok := range reach[ref] {
			if !ok {
				continue
			}
			if c.depth[tgt] >= MaxValueRecursion {
				return true
			}
		}
	}
	return false
}

func (c *valueDrawCtx) drawLiteralValue(lit *FuzzLiteral) FuzzValue {
	if lit == nil {
		return FuzzValue{Kind: KindLiteral}
	}
	switch lit.Kind {
	case LiteralString:
		return FuzzValue{Kind: KindLiteral, String: lit.String}
	case LiteralInt:
		return FuzzValue{Kind: KindLiteral, Int: lit.Int}
	case LiteralBool:
		return FuzzValue{Kind: KindLiteral, Bool: lit.Bool}
	}
	return FuzzValue{Kind: KindLiteral}
}

// CoupledCase pairs a schema + value with the Walk output computed
// from them. CoupledCaseGen returns these so the integration test
// can build an OracleCase without redoing the (schema, value, walk)
// dance per call site.
type CoupledCase struct {
	Schema FuzzSchema
	Value  FuzzValue
	Walk   WalkResult
}

// CoupledCaseGen returns a rapid generator that draws a schema from
// schemaGen, draws a value conforming to that schema, walks the
// pair to compute MockLLMContent + Expected, and — at a rapid-
// controlled probability — applies the union-collapse pass
// ("Move B" in the scope doc) that rewrites unions where the value
// tree picked a single arm consistently.
//
// The collapse is gated by a rapid boolean draw so the shrinker can
// independently bias toward the collapsed shape: when the rapid
// engine flips the dice off the original schema is returned,
// exercising the union-shape contract; when it flips on the
// collapsed schema is returned, which proves the behaviour replays
// correctly without the union wrapper. The collapse falls back to
// the original (uncollapsed) shape when union choices disagree
// across class instances or when re-walking the collapsed value
// fails for any reason — coupled cases must always be walkable.
func CoupledCaseGen(schemaGen *rapid.Generator[FuzzSchema]) *rapid.Generator[CoupledCase] {
	return rapid.Custom(func(t *rapid.T) CoupledCase {
		schema := schemaGen.Draw(t, "schema")
		value := ValueGen(schema).Draw(t, "value")
		walk, err := Walk(schema, value)
		if err != nil {
			t.Fatalf("bamlfuzz: walk drawn (schema, value): %v", err)
		}
		out := CoupledCase{Schema: schema, Value: value, Walk: walk}
		if rapid.Bool().Draw(t, "collapse_unions") {
			cs, cv, cerr := collapseUnionsToPicked(schema, value)
			if cerr == nil {
				cw, werr := Walk(cs, cv)
				if werr == nil {
					out.Schema = cs
					out.Value = cv
					out.Walk = cw
				}
			}
		}
		return out
	})
}

// collapseUnionsToPicked rewrites every KindUnion node in
// (schema, value) for which the value tree's observed arm choices
// are consistent. A union node is consistent when every instance
// of the surrounding class field selected the same variant — this
// keeps the schema-level rewrite valid for all callers of the class.
// Returns the original (schema, value) when no union qualifies and
// an error only when the schema/value pair is structurally
// inconsistent (defensive — the generator never produces such pairs).
//
// Move B applies at three positions:
//   - the effective root type (RootType when non-nil),
//   - each class property type,
//   - and recursively inside wrappers (optional/list/map) and
//     other unions.
func collapseUnionsToPicked(schema FuzzSchema, value FuzzValue) (FuzzSchema, FuzzValue, error) {
	visits := gatherClassVisits(schema.EffectiveRoot(), value, schema)
	collapseMap, err := planCollapses(schema, visits)
	if err != nil {
		return FuzzSchema{}, FuzzValue{}, err
	}
	out := schema
	out.Classes = make([]FuzzClass, len(schema.Classes))
	for i, cls := range schema.Classes {
		newProps := make([]FuzzProperty, len(cls.Properties))
		for j, prop := range cls.Properties {
			plan := collapseMap[fieldKey{cls.Name, prop.Name}]
			newProps[j] = FuzzProperty{
				Name: prop.Name,
				Type: rewriteTypeWithPlan(prop.Type, plan),
			}
		}
		out.Classes[i] = FuzzClass{Name: cls.Name, Properties: newProps}
	}
	rootPlan := newCollapsePlan()
	if schema.RootType != nil {
		rootPlan = planRoot(*schema.RootType, value)
		newRoot := rewriteTypeWithPlan(*schema.RootType, rootPlan)
		out.RootType = &newRoot
	}
	// Drive the value rewrite off the ORIGINAL schema's type tree +
	// the same per-field plans the schema rewrite used. The previous
	// implementation walked the NEW (post-collapse) schema and used
	// `v.VariantIndex` to descend into arms, which silently dropped
	// alignment whenever a nested union was collapsed: the surviving
	// outer wrapper's index then pointed at a different post-collapse
	// arm than the value's leaf chose, and the walker landed on the
	// wrong schema kind (e.g. enum_ref with an empty-string value).
	rootType := schema.EffectiveRoot()
	newValue := rewriteValueByPlans(schema, rootType, value, collapseMap, rootPlan, "")
	out = AnalyzeGraph(out)
	return out, newValue, nil
}

// fieldKey names one (class, property) slot — Move B's collapse
// decisions key off these so rewrites stay class-scoped.
type fieldKey struct {
	Class string
	Field string
}

// collapsePlan records, for one position in a type tree, which
// union arm index to keep (-1 means "no collapse here"). The
// position is identified by the structural path through the type,
// represented as the sequence of accessor steps from the type
// root. Plans are merged across visits: a position is collapsible
// only when every observation agrees on the same arm index.
type collapsePlan struct {
	// At each path the entry maps "yes-collapse-to-N" or
	// "saw-disagreement" (encoded as -1). Missing entries mean the
	// path was not a union at any visit and stays untouched.
	choices map[string]int
}

func newCollapsePlan() collapsePlan { return collapsePlan{choices: map[string]int{}} }

// gatherClassVisits scans the (root-type, root-value) tree and
// returns, for each class instance in the value tree, the union
// choices it made for each of its fields keyed by the structural
// path through the field's type.
type classVisit struct {
	className string
	fieldName string
	plan      collapsePlan
}

func gatherClassVisits(t FuzzType, v FuzzValue, schema FuzzSchema) []classVisit {
	var out []classVisit
	walkClassesInValue(t, v, schema, func(className string, fieldName string, ft FuzzType, fv FuzzValue) {
		p := newCollapsePlan()
		recordUnionChoices(ft, fv, "", p)
		out = append(out, classVisit{className: className, fieldName: fieldName, plan: p})
	})
	return out
}

func walkClassesInValue(t FuzzType, v FuzzValue, schema FuzzSchema, visit func(className, fieldName string, ft FuzzType, fv FuzzValue)) {
	switch t.Kind {
	case KindClassRef:
		if v.Kind != KindClassRef {
			return
		}
		cls, ok := schema.FindClass(v.ClassName)
		if !ok {
			return
		}
		for _, prop := range cls.Properties {
			fv, ok := v.LookupField(prop.Name)
			if !ok {
				continue
			}
			visit(v.ClassName, prop.Name, prop.Type, fv)
			walkClassesInValue(prop.Type, fv, schema, visit)
		}
	case KindOptional:
		if v.OptionalShape == OptionalPresent && v.Inner != nil && t.Inner != nil {
			walkClassesInValue(*t.Inner, *v.Inner, schema, visit)
		}
	case KindList:
		if t.Inner == nil {
			return
		}
		for _, item := range v.Items {
			walkClassesInValue(*t.Inner, item, schema, visit)
		}
	case KindMap:
		if t.Inner == nil {
			return
		}
		for _, e := range v.MapEntries {
			walkClassesInValue(*t.Inner, e.Value, schema, visit)
		}
	case KindUnion:
		if v.Kind != KindUnion || v.Variant == nil {
			return
		}
		if v.VariantIndex < 0 || v.VariantIndex >= len(t.Variants) {
			return
		}
		walkClassesInValue(t.Variants[v.VariantIndex], *v.Variant, schema, visit)
	}
}

// recordUnionChoices walks a (type, value) pair and stamps observed
// union-arm choices into `p` keyed by structural position in the type
// tree. The path scheme matches rewriteTypeAtPath exactly: union arms
// use ":v%d" so distinct variants of an outer union resolve to
// distinct slots; list elements all collapse onto ":l" and map values
// onto ":m" so observations across siblings merge at the element-type
// position. Merging at the element position is what makes "every
// element picked the same arm" collapse correctly and "elements
// disagree" invalidate cleanly.
//
// These paths are Move B's internal collapse-plan keys. They are
// SEPARATE from the JSON-path convention used by the walker,
// normalizer, and order checker (which key into
// CaseMetadata.UnionChoices and use `.<field>[idx][key]:v` paths);
// changing one does not require changing the other.
func recordUnionChoices(t FuzzType, v FuzzValue, path string, p collapsePlan) {
	switch t.Kind {
	case KindUnion:
		if v.Kind != KindUnion || v.Variant == nil {
			p.choices[path] = -1
			return
		}
		existing, seen := p.choices[path]
		switch {
		case !seen:
			p.choices[path] = v.VariantIndex
		case existing != v.VariantIndex:
			p.choices[path] = -1
		}
		recordUnionChoices(t.Variants[v.VariantIndex], *v.Variant, fmt.Sprintf("%s:v%d", path, v.VariantIndex), p)
	case KindOptional:
		if v.OptionalShape == OptionalPresent && v.Inner != nil && t.Inner != nil {
			recordUnionChoices(*t.Inner, *v.Inner, path+":o", p)
		}
	case KindList:
		if t.Inner == nil {
			return
		}
		for _, item := range v.Items {
			recordUnionChoices(*t.Inner, item, path+":l", p)
		}
	case KindMap:
		if t.Inner == nil {
			return
		}
		for _, e := range v.MapEntries {
			recordUnionChoices(*t.Inner, e.Value, path+":m", p)
		}
	}
}

func planCollapses(schema FuzzSchema, visits []classVisit) (map[fieldKey]collapsePlan, error) {
	merged := map[fieldKey]collapsePlan{}
	for _, v := range visits {
		key := fieldKey{Class: v.className, Field: v.fieldName}
		existing, ok := merged[key]
		if !ok {
			merged[key] = v.plan
			continue
		}
		for path, idx := range v.plan.choices {
			prev, seen := existing.choices[path]
			switch {
			case !seen:
				existing.choices[path] = idx
			case prev == -1:
				// already invalidated
			case prev != idx:
				existing.choices[path] = -1
			}
		}
		merged[key] = existing
	}
	// Strip disagreement markers so rewriteTypeWithPlan can treat
	// missing entries uniformly.
	for k, p := range merged {
		for path, idx := range p.choices {
			if idx < 0 {
				delete(p.choices, path)
			}
		}
		merged[k] = p
	}
	return merged, nil
}

// planRoot collapses the unions in the effective root type using
// the value at the root as the single observation.
func planRoot(rootType FuzzType, value FuzzValue) collapsePlan {
	p := newCollapsePlan()
	recordUnionChoices(rootType, value, "", p)
	for path, idx := range p.choices {
		if idx < 0 {
			delete(p.choices, path)
		}
	}
	return p
}

func rewriteTypeWithPlan(t FuzzType, plan collapsePlan) FuzzType {
	if len(plan.choices) == 0 {
		return t
	}
	return rewriteTypeAtPath(t, "", plan)
}

func rewriteTypeAtPath(t FuzzType, path string, plan collapsePlan) FuzzType {
	switch t.Kind {
	case KindUnion:
		if idx, ok := plan.choices[path]; ok && idx >= 0 && idx < len(t.Variants) {
			// Step the path with ":v<idx>" (matching the
			// recordUnionChoices scheme) so collapse descents land
			// at the same key used when stamping nested unions
			// inside the picked variant. Mismatched keys here would
			// silently leave nested unions uncollapsed.
			return rewriteTypeAtPath(t.Variants[idx], fmt.Sprintf("%s:v%d", path, idx), plan)
		}
		out := t
		out.Variants = make([]FuzzType, len(t.Variants))
		for i, v := range t.Variants {
			out.Variants[i] = rewriteTypeAtPath(v, fmt.Sprintf("%s:v%d", path, i), plan)
		}
		return out
	case KindOptional:
		if t.Inner == nil {
			return t
		}
		inner := rewriteTypeAtPath(*t.Inner, path+":o", plan)
		out := t
		out.Inner = &inner
		return out
	case KindList:
		if t.Inner == nil {
			return t
		}
		inner := rewriteTypeAtPath(*t.Inner, path+":l", plan)
		out := t
		out.Inner = &inner
		return out
	case KindMap:
		if t.Inner == nil {
			return t
		}
		inner := rewriteTypeAtPath(*t.Inner, path+":m", plan)
		out := t
		out.Inner = &inner
		return out
	}
	return t
}

// rewriteValueByPlans rewrites the value to match a schema rewritten
// via collapse plans. It walks the ORIGINAL pre-collapse schema type
// `t` in parallel with `v`. At every KindUnion node it consults the
// active `currentPlan` keyed by `path`: a recorded non-negative arm
// index means that union collapsed in the new schema, so the value's
// wrapper at this position is stripped to keep the post-collapse
// schema and the rewritten value aligned. Without this strip the
// retained wrapper's index would slot in at the wrong arm of the
// post-collapse union (whose arity is unchanged but whose arm types
// have shifted) and the walker would dispatch on a mismatched schema
// kind — landing, for example, on KindEnumRef with a union-shaped
// value whose Enum field is empty.
//
// The path scheme matches recordUnionChoices exactly: ":v<idx>" for a
// union arm step, ":o" for an optional Inner, ":l" for any list
// element, ":m" for any map value. Path resets to "" at every class-
// instance boundary because collapse plans are per-field; the global
// `plans` map supplies the field's plan on entry.
func rewriteValueByPlans(schema FuzzSchema, t FuzzType, v FuzzValue, plans map[fieldKey]collapsePlan, currentPlan collapsePlan, path string) FuzzValue {
	switch t.Kind {
	case KindUnion:
		if v.Kind != KindUnion || v.Variant == nil {
			return v
		}
		if v.VariantIndex < 0 || v.VariantIndex >= len(t.Variants) {
			return v
		}
		if idx, ok := currentPlan.choices[path]; ok && idx >= 0 {
			// Union collapsed in the new schema: drop the wrapper and
			// descend into the picked arm. v.VariantIndex equals idx
			// for every visit whose observations were merged (the plan
			// only retains paths where every visiting instance agreed
			// on the same arm), so the picked arm unambiguously
			// selects the surviving subtree.
			return rewriteValueByPlans(schema, t.Variants[idx], *v.Variant, plans, currentPlan, fmt.Sprintf("%s:v%d", path, idx))
		}
		newInner := rewriteValueByPlans(schema, t.Variants[v.VariantIndex], *v.Variant, plans, currentPlan, fmt.Sprintf("%s:v%d", path, v.VariantIndex))
		out := v
		out.Variant = &newInner
		return out
	case KindClassRef:
		if v.Kind != KindClassRef {
			return v
		}
		cls, ok := schema.FindClass(v.ClassName)
		if !ok {
			return v
		}
		newFields := make([]FuzzFieldValue, len(v.Fields))
		for i, fv := range v.Fields {
			var propType FuzzType
			for _, p := range cls.Properties {
				if p.Name == fv.Name {
					propType = p.Type
					break
				}
			}
			fieldPlan := plans[fieldKey{v.ClassName, fv.Name}]
			newFields[i] = FuzzFieldValue{
				Name:  fv.Name,
				Value: rewriteValueByPlans(schema, propType, fv.Value, plans, fieldPlan, ""),
			}
		}
		out := v
		out.Fields = newFields
		return out
	case KindOptional:
		if v.OptionalShape == OptionalPresent && v.Inner != nil && t.Inner != nil {
			newInner := rewriteValueByPlans(schema, *t.Inner, *v.Inner, plans, currentPlan, path+":o")
			out := v
			out.Inner = &newInner
			return out
		}
		return v
	case KindList:
		if t.Inner == nil {
			return v
		}
		newItems := make([]FuzzValue, len(v.Items))
		for i, item := range v.Items {
			newItems[i] = rewriteValueByPlans(schema, *t.Inner, item, plans, currentPlan, path+":l")
		}
		out := v
		out.Items = newItems
		return out
	case KindMap:
		if t.Inner == nil {
			return v
		}
		newEntries := make([]FuzzMapEntry, len(v.MapEntries))
		for i, e := range v.MapEntries {
			newEntries[i] = FuzzMapEntry{
				Key:   e.Key,
				Value: rewriteValueByPlans(schema, *t.Inner, e.Value, plans, currentPlan, path+":m"),
			}
		}
		out := v
		out.MapEntries = newEntries
		return out
	}
	return v
}

// drawString uses rapid's biased generator: short strings, common
// edge cases (empty, ASCII, multibyte) get sampled regularly.
func drawString(t *rapid.T, label string) string {
	// Mix between a curated edge-case set and rapid's generator
	// so the corpus covers both anomalous values and arbitrary
	// strings.
	options := []string{
		"",
		"alpha",
		" ",
		"a/b",
		"line\nbreak",
		`with "quote"`,
		"emoji_ø_ascii",
	}
	useEdge := rapid.Bool().Draw(t, label+":use_edge")
	if useEdge {
		idx := rapid.IntRange(0, len(options)-1).Draw(t, label+":edge_idx")
		return options[idx]
	}
	return rapid.StringN(0, 16, -1).Draw(t, label+":str")
}

func drawInt(t *rapid.T, label string) int64 {
	useEdge := rapid.Bool().Draw(t, label+":int_edge")
	if useEdge {
		edges := []int64{0, 1, -1, 42, -42, 1 << 31, -(1 << 31)}
		idx := rapid.IntRange(0, len(edges)-1).Draw(t, label+":int_edge_idx")
		return edges[idx]
	}
	return rapid.Int64Range(-1<<31, 1<<31).Draw(t, label+":int")
}

func drawFloat(t *rapid.T, label string) float64 {
	useEdge := rapid.Bool().Draw(t, label+":float_edge")
	if useEdge {
		edges := []float64{0, 1, -1, 0.5, -0.5}
		idx := rapid.IntRange(0, len(edges)-1).Draw(t, label+":float_edge_idx")
		return edges[idx]
	}
	// Bound floats to a sane range so encoding/json produces
	// short, comparable output. NaN / Inf are intentionally
	// excluded — they are not legitimate JSON.
	return rapid.Float64Range(-1e6, 1e6).Draw(t, label+":float")
}

// uniqueKey draws a short alphanumeric key that hasn't been used in
// the current map. Constrained keys keep the JSON output small and
// stable.
func uniqueKey(t *rapid.T, label string, used map[string]struct{}) string {
	pool := []string{"k0", "k1", "k2", "k3", "kA", "kB", "kC", "kD"}
	// Iterate until we find an unused key. Pool ≥ MaxMapLen+1, so
	// progress is guaranteed.
	for attempt := 0; attempt < len(pool); attempt++ {
		idx := rapid.IntRange(0, len(pool)-1).Draw(t, fmt.Sprintf("%s:pool_pick_%d", label, attempt))
		candidate := pool[idx]
		if _, taken := used[candidate]; taken {
			continue
		}
		used[candidate] = struct{}{}
		return candidate
	}
	// Fallback when every pool slot is taken (>= MaxMapLen calls
	// in: shouldn't happen given pool size > maxLen).
	candidate := "k_" + strings.Repeat("x", len(used)+1)
	used[candidate] = struct{}{}
	return candidate
}
