package codegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
)

// Self-referential types for testing cycle detection in structContainsMedia.
// These simulate the patterns BAML supports via recursive type aliases.

// selfRefDirect is a struct with a pointer to itself (via slice).
// Simulates: class Node { children Node[]? }
type selfRefDirect struct {
	Value    string
	Children *[]selfRefDirect
}

// mutualA and mutualB form a mutual recursion cycle.
// Simulates: class A { b B? } / class B { a A? }
type mutualA struct {
	Value string
	B     *mutualB
}
type mutualB struct {
	Value string
	A     *mutualA
}

// deepCycle has a longer cycle: deepA -> deepB -> deepC -> deepA
type deepCycleA struct {
	Next *deepCycleB
}
type deepCycleB struct {
	Next *deepCycleC
}
type deepCycleC struct {
	Next *deepCycleA
}

// selfRefViaSlice uses a slice (not pointer) as the recursion vehicle.
// Simulates: type JsonValue = int | string | JsonValue[]
type selfRefViaSlice struct {
	Text     string
	Children []selfRefViaSlice
}

// selfRefViaMap uses a map value type (struct containing itself).
// Note: structContainsMedia only walks struct fields, not map values,
// so this tests that it doesn't crash on map types.
type selfRefViaMap struct {
	Data   string
	Nested map[string]selfRefViaMap
}

// flatStruct has no recursion and no media.
type flatStruct struct {
	Name string
	Age  int
}

func TestStructContainsMedia_SelfReferentialTypes(t *testing.T) {
	tests := []struct {
		name     string
		typ      reflect.Type
		expected bool
	}{
		{
			name:     "self-referential via pointer to slice",
			typ:      reflect.TypeOf(selfRefDirect{}),
			expected: false,
		},
		{
			name:     "pointer to self-referential",
			typ:      reflect.TypeOf((*selfRefDirect)(nil)),
			expected: false,
		},
		{
			name:     "slice of self-referential",
			typ:      reflect.TypeOf([]selfRefDirect{}),
			expected: false,
		},
		{
			name:     "mutual recursion A",
			typ:      reflect.TypeOf(mutualA{}),
			expected: false,
		},
		{
			name:     "mutual recursion B",
			typ:      reflect.TypeOf(mutualB{}),
			expected: false,
		},
		{
			name:     "deep cycle A->B->C->A",
			typ:      reflect.TypeOf(deepCycleA{}),
			expected: false,
		},
		{
			name:     "self-referential via slice field",
			typ:      reflect.TypeOf(selfRefViaSlice{}),
			expected: false,
		},
		{
			name:     "self-referential via map",
			typ:      reflect.TypeOf(selfRefViaMap{}),
			expected: false,
		},
		{
			name:     "flat struct (no recursion, no media)",
			typ:      reflect.TypeOf(flatStruct{}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The primary assertion: this must terminate (not stack overflow).
			// We call it in a goroutine-safe way; if cycle detection is broken,
			// this will stack overflow and crash the test process.
			result := structContainsMedia(tt.typ)
			if result != tt.expected {
				t.Errorf("structContainsMedia(%v) = %v, want %v", tt.typ, result, tt.expected)
			}
		})
	}
}

func TestStructContainsMediaVisited_CycleBreaks(t *testing.T) {
	// Verify that the visited set actually prevents re-entry.
	// Pre-populate visited with the type and confirm it returns false immediately.
	typ := reflect.TypeOf(selfRefDirect{})
	visited := map[reflect.Type]bool{typ: true}

	if structContainsMediaVisited(typ, visited) {
		t.Error("structContainsMediaVisited should return false for already-visited type")
	}
}

func TestStructContainsMediaVisited_SharedVisitedAcrossFields(t *testing.T) {
	// When scanning mutualA, visiting mutualB should add it to visited,
	// so if mutualB references mutualA again, it doesn't re-enter.
	visited := make(map[reflect.Type]bool)
	structContainsMediaVisited(reflect.TypeOf(mutualA{}), visited)

	// Both types should now be in visited
	if !visited[reflect.TypeOf(mutualA{})] {
		t.Error("mutualA should be in visited set")
	}
	if !visited[reflect.TypeOf(mutualB{})] {
		t.Error("mutualB should be in visited set after scanning mutualA")
	}
	if got := len(visited); got != 2 {
		t.Errorf("visited set: got len=%d (%v), want exactly 2 — only mutualA + mutualB should be recorded; any extra would indicate the walker descended past the cycle pair", got, visited)
	}
}

// TestEmitMakeLegacyStreamOptionsFromAdapter_PinsPrimaryToClientOverride
// pins the contract that the top-level legacy streaming fallthrough's
// options builder sets the registry's primary to clientOverride. BAML's
// generated Stream.<Method> drops callOpts.client on the streaming
// path, so the registry's primary is the only seam BAML reads.
// _noRaw and _full both consume this helper (via makeLegacyPreamble),
// so a single rendered-output check covers both top-level fallthrough
// impls — they share the helper.
//
// A regression that re-introduced the helper from earlier shapes
// (passing adapter.LegacyClientRegistry through unchanged, ignoring
// clientOverride) would not break compilation but would silently
// dispatch the wrong client whenever ResolveEffectiveClient resolved
// a leaf distinct from the registry primary or the function's
// compiled default — exactly the production-reachable failure the
// child-callback fix already closed at the per-callback layer.
func TestEmitMakeLegacyStreamOptionsFromAdapter_PinsPrimaryToClientOverride(t *testing.T) {
	out := jen.NewFilePathName("github.com/example/test", "test")
	emitMakeLegacyStreamOptionsFromAdapter(out, "github.com/example/test/adapter", DefaultPackageConfig())
	rendered := out.GoString()

	// Positive: the registry-creation gate must include the
	// `clientOverride != ""` arm so the static fallthrough case
	// (no runtime registry, but routing resolved a leaf) gets a
	// fresh registry whose primary is the resolved leaf.
	if !strings.Contains(rendered, `registry != nil || clientOverride != ""`) {
		t.Errorf("registry-creation gate must include `clientOverride != \"\"` arm; rendered:\n%s", rendered)
	}

	// Positive: SetPrimaryClient(clientOverride) is the seam BAML
	// actually reads on the streaming path. Must be emitted.
	if !strings.Contains(rendered, `registry.SetPrimaryClient(clientOverride)`) {
		t.Errorf("SetPrimaryClient(clientOverride) must be emitted (BAML's streaming path reads only the registry's primary, WithClient is dropped); rendered:\n%s", rendered)
	}

	// Positive: WithClientRegistry must be appended within the gate.
	if !strings.Contains(rendered, "WithClientRegistry(registry)") {
		t.Errorf("WithClientRegistry(registry) append must be emitted; rendered:\n%s", rendered)
	}

	// Positive: the helper synthesises a fresh registry when the
	// adapter's legacy view is nil. NewClientRegistry call confirms
	// the no-runtime-registry-but-clientOverride arm.
	if !strings.Contains(rendered, "NewClientRegistry()") {
		t.Errorf("helper must call NewClientRegistry() to synthesise an empty registry when adapter.LegacyClientRegistry is nil; rendered:\n%s", rendered)
	}

	// Negative: the helper must read from adapter.LegacyClientRegistry
	// (the legacy view that preserves explicit strategy parents),
	// not adapter.ClientRegistry (the BuildRequest-safe view that
	// drops them) and not adapter.OriginalClientRegistry().Primary
	// (which would re-introduce the operator-primary anti-pattern).
	if strings.Contains(rendered, "adapter.ClientRegistry,") ||
		strings.Contains(rendered, "adapter.ClientRegistry\n") {
		t.Errorf("helper must use adapter.LegacyClientRegistry, not adapter.ClientRegistry; rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "OriginalClientRegistry") {
		t.Errorf("helper must not reach into OriginalClientRegistry — adapter.LegacyClientRegistry already encodes the legacy-keep semantics; rendered:\n%s", rendered)
	}

	// Pin nesting: NewClientRegistry, SetPrimaryClient, and
	// WithClientRegistry must all emit AFTER the outer
	// `registry != nil || clientOverride != ""` gate. A regression
	// that pulled any of them out of the gate would dispatch the
	// wrong client on the static-fallthrough arm (no runtime
	// registry, but routing resolved a leaf): SetPrimaryClient
	// missed → registry primary stays absent and BAML's
	// Stream.<Method> falls through to the function's compiled
	// default; WithClientRegistry missed → the registry never
	// reaches BAML; NewClientRegistry missed → SetPrimaryClient
	// would NPE on the nil registry.
	gate := `if registry != nil || clientOverride != "" {`
	gateIdx := strings.Index(rendered, gate)
	if gateIdx < 0 {
		t.Fatalf("expected `%s` block in rendered output; rendered:\n%s", gate, rendered)
	}
	newRegistryIdx := strings.Index(rendered, "NewClientRegistry()")
	setPrimaryIdx := strings.Index(rendered, `registry.SetPrimaryClient(clientOverride)`)
	withRegistryIdx := strings.Index(rendered, "WithClientRegistry(registry)")
	if newRegistryIdx < gateIdx {
		t.Errorf("NewClientRegistry() must emit after `%s`; gateIdx=%d newRegistryIdx=%d rendered:\n%s", gate, gateIdx, newRegistryIdx, rendered)
	}
	if setPrimaryIdx < gateIdx {
		t.Errorf("SetPrimaryClient(clientOverride) must emit after `%s`; gateIdx=%d setPrimaryIdx=%d rendered:\n%s", gate, gateIdx, setPrimaryIdx, rendered)
	}
	if withRegistryIdx < gateIdx {
		t.Errorf("WithClientRegistry(registry) must emit after `%s`; gateIdx=%d withRegistryIdx=%d rendered:\n%s", gate, gateIdx, withRegistryIdx, rendered)
	}

	// Negative: SetPrimaryClient must NOT be wrapped under the
	// inner `if registry == nil { ... }` synthesize-empty arm.
	// That arm fires only when no runtime legacy registry exists;
	// gating SetPrimaryClient on it would skip the per-attempt
	// primary pin whenever the operator supplied a runtime
	// registry, silently dispatching the wrong client.
	// WithClientRegistry shares the same risk — if it landed inside
	// the synth arm, the registry would never reach BAML on the
	// runtime-registry path.
	//
	// Detection: find the synthesize-empty block opener `{`, then
	// walk forward counting braces to find the matching `}`. Assert
	// SetPrimaryClient and WithClientRegistry emit AFTER that
	// closing brace. Brace counting (rather than substring) is
	// robust to the formatter's indentation depth, which depends on
	// surrounding context.
	synthOpener := "if registry == nil {"
	synthOpenerIdx := strings.Index(rendered, synthOpener)
	if synthOpenerIdx < 0 {
		t.Fatalf("expected `%s` block in rendered output; rendered:\n%s", synthOpener, rendered)
	}
	depth := 0
	synthCloseIdx := -1
	for i := synthOpenerIdx + len(synthOpener) - 1; i < len(rendered); i++ {
		switch rendered[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				synthCloseIdx = i
			}
		}
		if synthCloseIdx >= 0 {
			break
		}
	}
	if synthCloseIdx < 0 {
		t.Fatalf("could not locate matching `}` of `%s` block; rendered:\n%s", synthOpener, rendered)
	}
	if setPrimaryIdx < synthCloseIdx {
		t.Errorf("SetPrimaryClient(clientOverride) must emit AFTER the `%s` block closes — wrapping it under the synth arm would skip the per-attempt primary pin on the runtime-registry path; rendered:\n%s", synthOpener, rendered)
	}
	if withRegistryIdx < synthCloseIdx {
		t.Errorf("WithClientRegistry(registry) must emit AFTER the `%s` block closes — wrapping it under the synth arm would skip the registry attach on the runtime-registry path; rendered:\n%s", synthOpener, rendered)
	}

	// Outer-gate close pin: complement the inner synth-arm walker
	// above. A rendered output where any of the three calls
	// (NewClientRegistry / SetPrimaryClient / WithClientRegistry)
	// drifted AFTER the outer gate's matching `}` would still
	// satisfy the "after gate opener" checks at the top of this
	// block — the calls would end up outside the gate entirely,
	// firing unconditionally and breaking the no-runtime-registry-
	// no-clientOverride no-op contract.
	//
	// Detection: walk braces from the outer gate's opening `{` to
	// its matching `}`; assert all three indices are LESS than that
	// close.
	gateDepth := 0
	gateCloseIdx := -1
	for i := gateIdx + len(gate) - 1; i < len(rendered); i++ {
		switch rendered[i] {
		case '{':
			gateDepth++
		case '}':
			gateDepth--
			if gateDepth == 0 {
				gateCloseIdx = i
			}
		}
		if gateCloseIdx >= 0 {
			break
		}
	}
	if gateCloseIdx < 0 {
		t.Fatalf("could not locate matching `}` of `%s` block; rendered:\n%s", gate, rendered)
	}
	if newRegistryIdx > gateCloseIdx {
		t.Errorf("NewClientRegistry() must emit BEFORE the `%s` block closes (got newRegistryIdx=%d, gateCloseIdx=%d) — drifting out of the gate would unconditionally synthesise a registry even on the no-runtime-no-override no-op path; rendered:\n%s", gate, newRegistryIdx, gateCloseIdx, rendered)
	}
	if setPrimaryIdx > gateCloseIdx {
		t.Errorf("SetPrimaryClient(clientOverride) must emit BEFORE the `%s` block closes (got setPrimaryIdx=%d, gateCloseIdx=%d) — drifting out of the gate would mutate a registry the no-op path doesn't construct; rendered:\n%s", gate, setPrimaryIdx, gateCloseIdx, rendered)
	}
	if withRegistryIdx > gateCloseIdx {
		t.Errorf("WithClientRegistry(registry) must emit BEFORE the `%s` block closes (got withRegistryIdx=%d, gateCloseIdx=%d) — drifting out of the gate would unconditionally attach the registry, including the no-runtime-no-override no-op path; rendered:\n%s", gate, withRegistryIdx, gateCloseIdx, rendered)
	}
}

// TestEmitMakeLegacyChildOptionsFromAdapter_StaticMixedModeEmitsRegistry
// pins the contract that the per-callback scoped registry helper
// emits a registry (and pins its primary to clientOverride) even
// when the request supplies no runtime client_registry — i.e. the
// static mixed-mode case where the chain is composed entirely from
// compiled definitions but contains a legacy child.
//
// BAML's generated Stream.<Method> (functions_stream.go) drops
// callOpts.client on the streaming path that legacy child callbacks
// use. Without a registry whose primary is clientOverride, BAML
// falls through to the function's compiled default client and
// silently re-runs the whole compiled chain instead of the targeted
// child. A regression that re-introduced the `if original != nil`
// gate around the registry creation would not break compilation but
// would silently mis-dispatch in production whenever the request
// has no client_registry — this assertion catches that.
func TestEmitMakeLegacyChildOptionsFromAdapter_StaticMixedModeEmitsRegistry(t *testing.T) {
	out := jen.NewFilePathName("github.com/example/test", "test")
	emitMakeLegacyChildOptionsFromAdapter(out, "github.com/example/test/adapter", DefaultPackageConfig())
	rendered := out.GoString()

	// Positive: the emit must conditionally create a registry on
	// EITHER `original != nil` OR `clientOverride != ""`. The static
	// mixed-mode arm is the second disjunct.
	if !strings.Contains(rendered, `original != nil || clientOverride != ""`) {
		t.Errorf("registry-creation gate must include the `clientOverride != \"\"` arm so the static mixed-mode case (no runtime registry, compiled chain has a legacy child) still gets a scoped registry; rendered:\n%s", rendered)
	}

	// Positive: SetPrimaryClient(clientOverride) must be reachable
	// when original is nil. The cleanest signal is the absence of an
	// `if original != nil` gate around the SetPrimaryClient call —
	// the gating must be on `clientOverride != ""` only, so the call
	// fires for the static mixed-mode arm too.
	if !strings.Contains(rendered, `registry.SetPrimaryClient(clientOverride)`) {
		t.Errorf("SetPrimaryClient(clientOverride) must be emitted (BAML's streaming path reads only the registry's primary); rendered:\n%s", rendered)
	}

	// Positive: WithClientRegistry must be appended whenever the
	// outer disjunction fires, not gated on `original != nil`. We
	// can't easily assert AST nesting here, but a body that emits
	// WithClientRegistry at all is good evidence given the only call
	// site is inside the new gate.
	if !strings.Contains(rendered, "WithClientRegistry(registry)") {
		t.Errorf("WithClientRegistry(registry) append must be emitted; rendered:\n%s", rendered)
	}

	// Pin nesting: both SetPrimaryClient and WithClientRegistry must
	// emit AFTER the `if clientOverride != "" {` block opens. A
	// regression that moves either call under `if original != nil`
	// (the runtime-overrides arm, which precedes the
	// `clientOverride != ""` arm in the emit) would land the call
	// BEFORE this index — the static mixed-mode case (no runtime
	// registry, but a compiled legacy child) would then either
	// dispatch to BAML's compiled default client (SetPrimaryClient
	// missed → registry primary stays as the outer fallback parent
	// and BAML's Stream.<Method> drops callOpts.client) or skip the
	// registry attach entirely (WithClientRegistry missed →
	// AddLlmClient entries / SetPrimaryClient become unreachable).
	overrideGate := `if clientOverride != "" {`
	gateIdx := strings.Index(rendered, overrideGate)
	if gateIdx < 0 {
		t.Fatalf("expected `%s` block in rendered output; rendered:\n%s", overrideGate, rendered)
	}
	setPrimaryIdx := strings.Index(rendered, `registry.SetPrimaryClient(clientOverride)`)
	withRegistryIdx := strings.Index(rendered, "WithClientRegistry(registry)")
	if setPrimaryIdx < gateIdx {
		t.Errorf("SetPrimaryClient(clientOverride) must emit after `%s` (any move under `if original != nil` would precede the gate); gateIdx=%d setPrimaryIdx=%d rendered:\n%s", overrideGate, gateIdx, setPrimaryIdx, rendered)
	}
	if withRegistryIdx < gateIdx {
		t.Errorf("WithClientRegistry(registry) must emit after `%s` (any move under `if original != nil` would precede the gate); gateIdx=%d withRegistryIdx=%d rendered:\n%s", overrideGate, gateIdx, withRegistryIdx, rendered)
	}

	// Negative: no `if original != nil` between the gate and either
	// call. Catches a refactor that wraps the calls in a fresh
	// runtime-only conditional after the gate (different shape, same
	// regression — the static mixed-mode arm would skip the calls).
	if setPrimaryIdx >= gateIdx {
		between := rendered[gateIdx:setPrimaryIdx]
		if strings.Contains(between, "if original != nil") {
			t.Errorf("`if original != nil` must not gate SetPrimaryClient(clientOverride) — the static mixed-mode arm has no original registry; rendered:\n%s", rendered)
		}
	}
	if withRegistryIdx >= gateIdx {
		between := rendered[gateIdx:withRegistryIdx]
		if strings.Contains(between, "if original != nil") {
			t.Errorf("`if original != nil` must not gate WithClientRegistry(registry) — the static mixed-mode arm has no original registry; rendered:\n%s", rendered)
		}
	}

	// Pin SetPrimaryClient and WithClientRegistry RELATIVE to the
	// override block's closing brace. The positional checks above
	// only assert the calls follow the opener, not whether each call
	// is inside or outside the override block. The expected emit
	// shape (codegen.go emitMakeLegacyChildOptionsFromAdapter):
	//
	//   if clientOverride != "" {
	//       registry.SetPrimaryClient(clientOverride)   // INSIDE
	//   }
	//   result = append(result, ... WithClientRegistry(registry)) // AFTER
	//
	// SetPrimaryClient inside the override block makes the
	// per-attempt primary pin fire only when clientOverride is set
	// (the static mixed-mode case). WithClientRegistry after the
	// override block's close lets the registry attach happen on
	// every outer-disjunction arm — runtime-registry only, runtime
	// + override, override only — not just when clientOverride is
	// set.
	//
	// Walk braces from the opening `{` of the override block (last
	// byte of overrideGate) forward to its matching `}` to find
	// overrideCloseIdx. Same approach as the synth-arm walker in
	// the stream-helper test.
	depth := 0
	overrideCloseIdx := -1
	for i := gateIdx + len(overrideGate) - 1; i < len(rendered); i++ {
		switch rendered[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				overrideCloseIdx = i
			}
		}
		if overrideCloseIdx >= 0 {
			break
		}
	}
	if overrideCloseIdx < 0 {
		t.Fatalf("could not locate matching `}` of `%s` block; rendered:\n%s", overrideGate, rendered)
	}
	if !(setPrimaryIdx > gateIdx && setPrimaryIdx < overrideCloseIdx) {
		t.Errorf("SetPrimaryClient(clientOverride) must emit INSIDE the `%s` block (between %d and %d), got %d; rendered:\n%s", overrideGate, gateIdx, overrideCloseIdx, setPrimaryIdx, rendered)
	}
	if withRegistryIdx <= overrideCloseIdx {
		t.Errorf("WithClientRegistry(registry) must emit AFTER the `%s` block closes (got withRegistryIdx=%d, overrideCloseIdx=%d) — wrapping it inside the override block would skip the registry attach on the runtime-only arm; rendered:\n%s", overrideGate, withRegistryIdx, overrideCloseIdx, rendered)
	}

	// Outer-gate close pin for WithClientRegistry. The
	// withRegistryIdx > overrideCloseIdx assertion above gives only a
	// lower bound; without an upper bound, withRegistryIdx could drift
	// PAST the outer `if original != nil || clientOverride != ""`
	// gate's closing brace and still satisfy the existing check. That
	// regression would unconditionally append WithClientRegistry on
	// the no-runtime-no-override no-op path, attaching a registry the
	// gate's "do nothing" branch was meant to skip.
	//
	// Walk braces from the outer gate's opening `{` forward to its
	// matching `}` to find outerCloseIdx; assert
	// withRegistryIdx < outerCloseIdx. Same walker shape as the
	// stream-helper test's outer-gate walker.
	outerGate := `if original != nil || clientOverride != "" {`
	outerGateIdx := strings.Index(rendered, outerGate)
	if outerGateIdx < 0 {
		t.Fatalf("expected outer gate `%s` block in rendered output; rendered:\n%s", outerGate, rendered)
	}
	outerDepth := 0
	outerCloseIdx := -1
	for i := outerGateIdx + len(outerGate) - 1; i < len(rendered); i++ {
		switch rendered[i] {
		case '{':
			outerDepth++
		case '}':
			outerDepth--
			if outerDepth == 0 {
				outerCloseIdx = i
			}
		}
		if outerCloseIdx >= 0 {
			break
		}
	}
	if outerCloseIdx < 0 {
		t.Fatalf("could not locate matching `}` of outer gate `%s`; rendered:\n%s", outerGate, rendered)
	}
	if withRegistryIdx >= outerCloseIdx {
		t.Errorf("WithClientRegistry(registry) must emit BEFORE the outer gate `%s` closes (got withRegistryIdx=%d, outerCloseIdx=%d) — drifting past the outer gate would unconditionally append the registry, including the no-runtime-no-override no-op path; rendered:\n%s", outerGate, withRegistryIdx, outerCloseIdx, rendered)
	}

	// Negative: the scoped registry's primary must be clientOverride,
	// not the outer original.Primary. BAML's Stream.<Method> consumes
	// only the registry's primary, so reapplying original.Primary
	// would name a strategy parent filtered from the scoped registry
	// (client-not-found) or drive a compiled outer client instead of
	// the per-attempt clientOverride this callback exists to dispatch.
	if strings.Contains(rendered, "original.Primary") {
		t.Errorf("scoped registry's primary must be clientOverride, not original.Primary (Stream.<Method> would drive the wrong client); rendered:\n%s", rendered)
	}
}

// TestBuildLegacyChildCallParams_FirstArgIsCtx pins the contract
// that legacyStreamChildFn / legacyCallChildFn invoke BAML's
// generated Stream.<Method> with the closure's per-attempt `ctx`
// as the first argument. Threading `adapter` (which embeds the
// request-wide context.Context) compiles, but routes the BAML call
// through the outer context and silently ignores per-attempt
// cancellation from RunStreamOrchestration / RunCallOrchestration.
//
// A regression that re-introduced `adapter` as the first arg would
// not break compilation (adapter satisfies context.Context via
// embedding), but would silently drop child-scoped cancellation —
// this string-level assertion catches that.
func TestBuildLegacyChildCallParams_FirstArgIsCtx(t *testing.T) {
	argResolver := func(arg string) jen.Code {
		return jen.Id("input").Dot(arg)
	}
	params := buildLegacyChildCallParams([]string{"Topic"}, argResolver)

	f := jen.NewFile("test")
	f.Func().Id("test").Params(
		jen.Id("ctx").Qual("context", "Context"),
		jen.Id("input").Op("*").Id("Input"),
		jen.Id("opts").Op("[]").Id("CallOptionFunc"),
	).Block(
		jen.Qual("baml_client", "Stream").Dot("Method").Call(params...),
	)
	output := f.GoString()

	if !strings.Contains(output, "Stream.Method(ctx, input.Topic, opts...)") {
		t.Errorf("expected 'Stream.Method(ctx, input.Topic, opts...)' in rendered output, got:\n%s", output)
	}
	// Negative assertion: a regression to `adapter` first arg is
	// silently fatal at runtime (per-attempt cancellation lost), so
	// pin the absence directly.
	if strings.Contains(output, "Stream.Method(adapter,") {
		t.Errorf("legacy child stream call must not pass adapter as first arg (silently routes BAML through request-wide ctx); rendered: %s", output)
	}
}

func TestEnumValueAttrsCode(t *testing.T) {
	// Get the generated code
	code := enumValueAttrsCode()

	// Should have 3 statements (Description, Alias, Skip)
	if len(code) != 3 {
		t.Errorf("enumValueAttrsCode() returned %d statements, want 3", len(code))
	}

	// Render and verify the output
	// We'll create a dummy function to contain the code so we can render it
	f := jen.NewFile("test")
	f.Func().Id("test").Params().Block(code...)

	output := f.GoString()

	// Check for Description handling
	if !strings.Contains(output, `v.Description != ""`) {
		t.Error("enumValueAttrsCode() missing Description check")
	}
	if !strings.Contains(output, `vb.SetDescription(v.Description)`) {
		t.Error("enumValueAttrsCode() missing SetDescription call")
	}

	// Check for Alias handling
	if !strings.Contains(output, `v.Alias != ""`) {
		t.Error("enumValueAttrsCode() missing Alias check")
	}
	if !strings.Contains(output, `vb.SetAlias(v.Alias)`) {
		t.Error("enumValueAttrsCode() missing SetAlias call")
	}

	// Check for Skip handling
	if !strings.Contains(output, `v.Skip`) {
		t.Error("enumValueAttrsCode() missing Skip check")
	}
	if !strings.Contains(output, `vb.SetSkip(true)`) {
		t.Error("enumValueAttrsCode() missing SetSkip call")
	}
}

func TestEnumValueAttrsCode_Order(t *testing.T) {
	// The order should be: Description, Alias, Skip
	code := enumValueAttrsCode()

	f := jen.NewFile("test")
	f.Func().Id("test").Params().Block(code...)
	output := f.GoString()

	descIdx := strings.Index(output, "SetDescription")
	aliasIdx := strings.Index(output, "SetAlias")
	skipIdx := strings.Index(output, "SetSkip")

	if descIdx == -1 || aliasIdx == -1 || skipIdx == -1 {
		t.Fatal("Missing expected method calls in output")
	}

	if !(descIdx < aliasIdx && aliasIdx < skipIdx) {
		t.Error("enumValueAttrsCode() methods not in expected order (Description, Alias, Skip)")
	}
}

// TestEmitRouter_RetryResolutionUsesStrategyAwareHelper pins the
// post-fix shape of the per-method router: retry resolution calls
// ResolveStrategyAwareRetryPolicy and is keyed on BOTH __retryClient
// (the pre-unwrap strategy / primary-override target) and __effective
// (the post-RR-unwrap leaf). The legacy retry-resolution emit at the
// bottom of the router is always emitted, so this test asserts on
// that site directly. With the introspected.Request /
// introspected.StreamRequest singletons set non-nil, the BuildRequest
// landing-block retry calls are also emitted and the assertion
// catches a regression at every site.
//
// A regression to the previous shape — keying retry on __effective
// only, after ResolveEffectiveClient overwrote it to the RR leaf —
// silently dropped any retry_policy declared on a strategy wrapper
// (RR or fallback) on the v0.219+ BuildRequest path, contradicting
// BAML's LLMStrategyProvider::WithRetryPolicy semantics.
func TestEmitRouter_RetryResolutionUsesStrategyAwareHelper(t *testing.T) {
	// Save + restore the introspected BuildRequest singletons. The
	// codegen package treats both nil as "legacy-only adapter", which
	// would skip the BR landing blocks (and their resolveRetryPolicy
	// emits) entirely. Setting both non-nil exercises every retry
	// emit site so the assertions catch a regression at any of them.
	savedRequest := introspected.Request
	savedStreamRequest := introspected.StreamRequest
	t.Cleanup(func() {
		introspected.Request = savedRequest
		introspected.StreamRequest = savedStreamRequest
	})
	introspected.Request = struct{}{}
	introspected.StreamRequest = struct{}{}

	// Minimal generator: only fields emitRouter reads.
	pkgs := DefaultPackageConfig()
	g := &generator{
		opts:               Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
		pkgs:               pkgs,
		intro:              RootIntrospection(),
		out:                common.MakeFile(),
		supportsWithClient: true,
	}
	me := &methodEmitter{
		g:                          g,
		methodName:                 "GreetUser",
		buildRequestMethodName:     "greetUser_buildRequest",
		buildCallRequestMethodName: "greetUser_buildCallRequest",
		noRawMethodName:            "greetUser_noRaw",
		fullMethodName:             "greetUser_full",
	}

	me.emitRouter()
	rendered := g.out.GoString()

	// Pin the __retryClient declaration. The pre-unwrap client name
	// must be captured BEFORE __effective is overwritten by
	// ResolveEffectiveClient; without this, the wrapper-first retry
	// resolution downstream cannot see the strategy-wrapper name.
	if !strings.Contains(rendered, "__retryClient :=") {
		t.Errorf("router must declare __retryClient (pre-unwrap client name); rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "buildrequest.ResolvePrimaryClient(adapter, introspected.FunctionClient[\"GreetUser\"])") {
		t.Errorf("__retryClient must seed from ResolvePrimaryClient (applies primary override but no RR unwrap); rendered:\n%s", rendered)
	}

	// Pin that __effective starts at __retryClient and is only
	// overwritten by the conditional ResolveEffectiveClient block.
	// "__effective := __retryClient" couples the two so we never
	// emit the legacy shape that seeded __effective directly from
	// ResolvePrimaryClient (which would lose the wrapper name when
	// the conditional block reassigns __effective).
	if !strings.Contains(rendered, "__effective := __retryClient") {
		t.Errorf("__effective must alias __retryClient at seed time so the wrapper name survives the RR-unwrap reassignment; rendered:\n%s", rendered)
	}

	// Pin: every retry resolution call must be the strategy-aware
	// helper. The plain ResolveRetryPolicy call (the pre-fix shape)
	// must not appear anywhere in the router.
	if strings.Contains(rendered, "buildrequest.ResolveRetryPolicy(") {
		t.Errorf("router must not call buildrequest.ResolveRetryPolicy directly — wrapper-first semantics require ResolveStrategyAwareRetryPolicy; rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "buildrequest.ResolveStrategyAwareRetryPolicy(") {
		t.Errorf("router must call buildrequest.ResolveStrategyAwareRetryPolicy for retry resolution; rendered:\n%s", rendered)
	}

	// Pin the actual argument shape. Both __retryClient and
	// __effective (and the corresponding ClientRetryPolicy lookups
	// keyed by each) must be passed into every helper invocation.
	// A regression that passed __effective in both wrapper and leaf
	// slots would compile but quietly revert to leaf-only retry.
	wantArgs := "ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)"
	if !strings.Contains(rendered, wantArgs) {
		t.Errorf("ResolveStrategyAwareRetryPolicy call missing expected (strategy, effective) argument shape:\nwant fragment:\n%s\nrendered:\n%s", wantArgs, rendered)
	}
	// Negative pin: the symmetric wrong-shape — keying both wrapper
	// and leaf slots on __effective — would silently revert to
	// leaf-only retry. The strategy-aware helper short-circuits its
	// leaf-fallback when strategyClient == effectiveClient, so a
	// regression that passed __effective in both positions would
	// type-check and compile cleanly.
	wrongArgs := "ResolveStrategyAwareRetryPolicy(adapter, __effective, __effective,"
	if strings.Contains(rendered, wrongArgs) {
		t.Errorf("ResolveStrategyAwareRetryPolicy must not be called with __effective in both strategy and effective slots — that silently reverts to leaf-only retry; rendered:\n%s", rendered)
	}

	// Pin: the legacy retry resolution (always-emitted, at router
	// bottom) is keyed on __retryClient too. Detect by counting
	// __legacyRetryPolicy occurrences and asserting the strategy-
	// aware helper appears between them and the next blank line.
	legacyIdx := strings.Index(rendered, "__legacyRetryPolicy :=")
	if legacyIdx < 0 {
		t.Fatalf("router must declare __legacyRetryPolicy for the legacy fallthrough path; rendered:\n%s", rendered)
	}
	helperAfterLegacy := strings.Index(rendered[legacyIdx:], "buildrequest.ResolveStrategyAwareRetryPolicy(")
	if helperAfterLegacy < 0 {
		t.Errorf("__legacyRetryPolicy must be assigned from ResolveStrategyAwareRetryPolicy (not the leaf-only ResolveRetryPolicy); rendered:\n%s", rendered)
	}
}

// TestEmitRouter_CallModeFallbackDefersToBridgeWhenAvailable pins the
// single-resolver-per-request invariant for call-mode requests when a
// StreamRequest bridge is available: the call block emits ONLY the
// single-provider arm and the bridge owns the sole typed fallback
// resolution. A regression that emits ResolveFallbackChainPlanForClient
// in both the call and bridge blocks would double-advance the
// SharedState advancer on a mixed chain — the call block's old
// consume gate (no-call-legacy-children) would decline a centralised-
// RR-plus-call-legacy-sibling chain after the resolver already
// committed an RR advance, the chain would fall through to the bridge,
// and the bridge's own ResolveFallbackChainPlanForClient call would
// advance a second time. One request → two cross-worker RR slots
// burned, rotation skewed for that shape.
//
// When the bridge does not exist (introspected.StreamRequest == nil),
// the call block IS the sole BuildRequest landing spot for call modes
// and must emit its own fallback resolver.
//
// Counts ResolveFallbackChainPlanForClient occurrences positionally:
//
//   - Request + StreamRequest both non-nil: exactly 2 occurrences,
//     one in the streaming block (for stream modes) and one in the
//     bridge block (the sole call-mode resolver).
//   - Request only: exactly 1 occurrence, inside the call block (call
//     block is the only landing spot — no bridge, no stream block).
//   - StreamRequest only: exactly 1 occurrence, inside the streaming
//     block (no call block, bridge serves call modes alongside).
func TestEmitRouter_CallModeFallbackDefersToBridgeWhenAvailable(t *testing.T) {
	// Save + restore the introspected BuildRequest singletons across
	// sub-tests so each scenario can pin a specific (Request,
	// StreamRequest) presence combination.
	savedRequest := introspected.Request
	savedStreamRequest := introspected.StreamRequest
	t.Cleanup(func() {
		introspected.Request = savedRequest
		introspected.StreamRequest = savedStreamRequest
	})

	pkgs := DefaultPackageConfig()
	renderRouter := func(t *testing.T) string {
		t.Helper()
		g := &generator{
			opts:               Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
			pkgs:               pkgs,
			intro:              RootIntrospection(),
			out:                common.MakeFile(),
			supportsWithClient: true,
		}
		me := &methodEmitter{
			g:                          g,
			methodName:                 "GreetUser",
			buildRequestMethodName:     "greetUser_buildRequest",
			buildCallRequestMethodName: "greetUser_buildCallRequest",
			noRawMethodName:            "greetUser_noRaw",
			fullMethodName:             "greetUser_full",
		}
		me.emitRouter()
		return g.out.GoString()
	}

	// resolverFn is the symbol whose call-site count drives the
	// single-advance assertion. The bridge fix is what removes the
	// duplicate emission; counting at this granularity catches any
	// regression that re-introduces a call-block fallback resolver
	// alongside the bridge.
	const resolverFn = "buildrequest.ResolveFallbackChainPlanForClient("

	t.Run("call-bridge-both-available-bridge-owns-fallback", func(t *testing.T) {
		// Both APIs non-nil — the bridge exists. Call block emits
		// only the single-provider arm; the bridge owns fallback for
		// call modes. Total resolver calls in the router: 2 (stream
		// block + bridge block).
		introspected.Request = struct{}{}
		introspected.StreamRequest = struct{}{}
		rendered := renderRouter(t)
		got := strings.Count(rendered, resolverFn)
		if got != 2 {
			t.Errorf("with Request + StreamRequest non-nil, expected exactly 2 ResolveFallbackChainPlanForClient call sites "+
				"(streaming block + bridge block; the call block must defer to the bridge to avoid double-advance), got %d.\nrendered:\n%s",
				got, rendered)
		}
		// Positional pin: the bridge call site is gated by StreamMode-
		// Call / StreamModeCallWithRaw. If the call block re-introduced
		// the fallback resolver, it would also be gated on call modes
		// — and a second resolver call would appear before the
		// streaming block's gate. Pin that the FIRST resolver call site
		// is the streaming block, identified by IsProviderSupported
		// being the support predicate. The call-block fallback path
		// uses IsCallProviderSupported, so a regression where the call
		// block re-emits the resolver would surface IsCallProviderSupp-
		// orted as the first predicate AND increase the count above 2.
		firstIdx := strings.Index(rendered, resolverFn)
		if firstIdx < 0 {
			t.Fatalf("expected at least one ResolveFallbackChainPlanForClient call site; rendered:\n%s", rendered)
		}
		secondIdx := firstIdx + strings.Index(rendered[firstIdx+1:], resolverFn) + 1
		// IsCallProviderSupported must not appear inside the first
		// resolver invocation's argument list — that's the streaming
		// block's resolver and it uses the stream-side predicate. The
		// only path that could put IsCallProviderSupported in the
		// first invocation is a regression where the call block emits
		// its own fallback resolver first.
		argsRegion := rendered[firstIdx : secondIdx+len(resolverFn)+200]
		if strings.Count(argsRegion, "buildrequest.IsCallProviderSupported") > 0 {
			t.Errorf("first resolver call site contains IsCallProviderSupported, suggesting the call block re-introduced a fallback resolver; rendered region:\n%s", argsRegion)
		}
	})

	t.Run("call-only-no-bridge-call-block-owns-fallback", func(t *testing.T) {
		// Request non-nil, StreamRequest nil — no bridge. Call block
		// is the sole BuildRequest landing spot for call modes and
		// MUST emit its own fallback resolver. Total resolver calls
		// in the router: 1 (call block only — no stream block, no
		// bridge).
		introspected.Request = struct{}{}
		introspected.StreamRequest = nil
		rendered := renderRouter(t)
		got := strings.Count(rendered, resolverFn)
		if got != 1 {
			t.Errorf("with Request-only adapter (no bridge), expected exactly 1 ResolveFallbackChainPlanForClient call site "+
				"(the call block is the sole landing spot), got %d.\nrendered:\n%s",
				got, rendered)
		}
		// The single resolver invocation must use the call-side
		// support predicate. Stream-side predicate would mean the
		// call block accepted chains the call orchestrator cannot
		// dispatch (call-legacy-but-stream-supported children).
		if !strings.Contains(rendered, "buildrequest.IsCallProviderSupported") {
			t.Errorf("call-only adapter must use IsCallProviderSupported in its sole resolver call; rendered:\n%s", rendered)
		}
	})

	t.Run("stream-only-no-call-stream-block-owns-fallback", func(t *testing.T) {
		// Request nil, StreamRequest non-nil — no call block, just
		// stream + bridge. Stream block handles stream modes; bridge
		// handles call modes. Two resolver call sites total.
		introspected.Request = nil
		introspected.StreamRequest = struct{}{}
		rendered := renderRouter(t)
		got := strings.Count(rendered, resolverFn)
		if got != 2 {
			t.Errorf("with StreamRequest-only adapter, expected exactly 2 ResolveFallbackChainPlanForClient call sites "+
				"(streaming block + bridge block), got %d.\nrendered:\n%s",
				got, rendered)
		}
	})
}
