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
	// and B → optional<A>). Mutual cycles do not require dynamic
	// skip — they are realizable through dynamic TypeBuilder — and
	// the value generator terminates them via the per-class
	// recursion cap.
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
// static .baml emitter can render. Self-ref schemas are permitted
// at the configured probability and stamped with
// RequiresDynamicSkip=true; mutual cycles are also permitted and
// remain dynamic-friendly.
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

func drawKind(t *rapid.T, label string, choices []FuzzTypeKind) FuzzTypeKind {
	idx := rapid.IntRange(0, len(choices)-1).Draw(t, label+":kind_idx")
	return choices[idx]
}

func drawLiteral(t *rapid.T, label string) *FuzzLiteral {
	kindChoices := []FuzzLiteralKind{LiteralString, LiteralInt, LiteralBool}
	kidx := rapid.IntRange(0, len(kindChoices)-1).Draw(t, label+":lit_kind")
	switch kindChoices[kidx] {
	case LiteralString:
		// Pick from a small pinned set of string literals — these
		// double as edge cases (empty, alphanumeric, contains
		// quote).
		options := []string{"alpha", "beta", "gamma", "", `"quoted"`}
		oidx := rapid.IntRange(0, len(options)-1).Draw(t, label+":lit_str")
		return &FuzzLiteral{Kind: LiteralString, String: options[oidx]}
	case LiteralInt:
		options := []int64{0, 1, -1, 42, -42}
		oidx := rapid.IntRange(0, len(options)-1).Draw(t, label+":lit_int")
		return &FuzzLiteral{Kind: LiteralInt, Int: options[oidx]}
	case LiteralBool:
		b := rapid.Bool().Draw(t, label+":lit_bool")
		return &FuzzLiteral{Kind: LiteralBool, Bool: b}
	}
	return &FuzzLiteral{Kind: LiteralString}
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
// to the schema's root class. The generator threads a per-class
// recursion depth counter through the walk and forces termination at
// the depth cap by emitting OptionalAbsent / empty list / empty map
// for any nested edge that would recurse further into the same
// class.
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
		return ctx.drawClass(t, schema.RootClass, "root")
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
		return c.drawMap(t, ft, label)
	case KindClassRef:
		// Required class ref: if recursion budget for the target
		// is exhausted, this shape is unreachable from the
		// schema generator — the only required class-ref edges
		// are DAG ones, where the target is a distinct class with
		// its own depth counter, so the cap never fires here.
		// Self-ref / cycle edges are always wrapped in optional/
		// list/map (handled above) where termination is enforced.
		return c.drawClass(t, ft.Ref, label)
	}
	return FuzzValue{Kind: KindString}
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

func (c *valueDrawCtx) drawMap(t *rapid.T, ft FuzzType, label string) FuzzValue {
	maxLen := MaxMapLen
	if c.wouldExceedDepth(ft.Inner) {
		maxLen = 0
	}
	length := rapid.IntRange(0, maxLen).Draw(t, label+":map_len")
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
