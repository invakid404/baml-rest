package buildrequest

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestBuildLegacyChildRegistryEntries_IncludesNonStrategyLeaves pins
// the rule that non-strategy leaf entries are unconditionally included.
// Required so dynamic leaves declared at request time (e.g. a tenant
// model under a runtime-redirected RR child) reach the BAML CFFI seam
// — without this, WithClient(leafName) would resolve to a missing
// client error before BAML could dispatch.
func TestBuildLegacyChildRegistryEntries_IncludesNonStrategyLeaves(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "TenantLeaf", Provider: "openai", ProviderSet: true,
				Options: map[string]any{"model": "tenant-model"}},
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"TenantLeaf"}}},
		},
	}
	introspectedProviders := map[string]string{
		"InnerRR":    "baml-roundrobin",
		"TenantLeaf": "openai",
	}
	introspectedChains := map[string][]string{
		"InnerRR": {"StaticBlue", "StaticGreen"},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerRR", introspectedProviders, introspectedChains)

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	if !got["TenantLeaf"] {
		t.Errorf("expected TenantLeaf in scoped registry; entries=%+v", entries)
	}
	if !got["InnerRR"] {
		t.Errorf("expected InnerRR in scoped registry (target strategy parent); entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_DropsUnreachableStrategyParents
// pins that explicit strategy parents NOT reachable from clientOverride
// are dropped. This is the core invariant separating the per-callback
// scoped helper from makeLegacyOptionsFromAdapter (top-level legacy):
// reusing the latter would let an unrelated invalid strategy parent
// poison this child via BAML's eager registry parse.
func TestBuildLegacyChildRegistryEntries_DropsUnreachableStrategyParents(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafA"}}},
			// Unrelated explicit strategy parent — must be dropped.
			{Name: "OtherFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafB"}}},
		},
	}
	introspectedProviders := map[string]string{
		"InnerRR":       "baml-roundrobin",
		"OtherFallback": "baml-fallback",
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerRR", introspectedProviders, nil)

	for _, e := range entries {
		if e.Name == "OtherFallback" {
			t.Errorf("OtherFallback (unreachable) leaked into scoped registry: entries=%+v", entries)
		}
	}
}

// TestBuildLegacyChildRegistryEntries_TransitiveReachability covers
// nested compositions like InnerFallback → [LeafA, DeeperRR] where
// DeeperRR has a runtime override. BAML executes the whole nested
// strategy graph inside one legacy callback, so the registry must
// include strategy parents reachable transitively from clientOverride.
func TestBuildLegacyChildRegistryEntries_TransitiveReachability(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "InnerFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"DeeperRR"}}},
			{Name: "DeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"TenantLeaf"}}},
			{Name: "TenantLeaf", Provider: "openai", ProviderSet: true},
		},
	}
	introspectedProviders := map[string]string{
		"InnerFallback": "baml-fallback",
		"DeeperRR":      "baml-roundrobin",
		"TenantLeaf":    "openai",
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerFallback", introspectedProviders, nil)

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	if !got["InnerFallback"] {
		t.Errorf("expected InnerFallback in scoped registry; entries=%+v", entries)
	}
	if !got["DeeperRR"] {
		t.Errorf("expected DeeperRR (transitively reachable) in scoped registry; entries=%+v", entries)
	}
	if !got["TenantLeaf"] {
		t.Errorf("expected TenantLeaf (leaf) in scoped registry; entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_DropsInertPresenceOnlyParent
// pins the inert-parent drop rule. A presence-only entry on a static
// strategy parent ({Name: "MyRR"}) carries no provider, no strategy,
// and no start — forwarding it would re-trigger the missing-strategy
// CFFI failure that motivated the dual-view split.
func TestBuildLegacyChildRegistryEntries_DropsInertPresenceOnlyParent(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR"},
		},
	}
	introspectedProviders := map[string]string{
		"MyRR": "baml-roundrobin",
	}
	introspectedChains := map[string][]string{
		"MyRR": {"StaticBlue", "StaticGreen"},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "MyRR", introspectedProviders, introspectedChains)

	for _, e := range entries {
		if e.Name == "MyRR" {
			t.Errorf("inert presence-only MyRR leaked into scoped registry: entries=%+v", entries)
		}
	}
}

// TestBuildLegacyChildRegistryEntries_DynamicOnlyParent pins that a
// dynamic-only nested fallback parent (no compiled introspected entry,
// only runtime registry definition) is included when targeted. This
// closes the Scenario C regression: without inclusion, BAML resolves
// WithClient("DynamicFallback") against the empty compiled registry
// and returns client-not-found.
func TestBuildLegacyChildRegistryEntries_DynamicOnlyParent(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "DynamicFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"TenantLeaf"}}},
			{Name: "TenantLeaf", Provider: "openai", ProviderSet: true,
				Options: map[string]any{"model": "tenant-model"}},
		},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "DynamicFallback", nil, nil)

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	if !got["DynamicFallback"] {
		t.Errorf("expected DynamicFallback (dynamic-only nested parent) in scoped registry; entries=%+v", entries)
	}
	if !got["TenantLeaf"] {
		t.Errorf("expected TenantLeaf (dynamic leaf under DynamicFallback) in scoped registry; entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_ProviderTranslation verifies
// that canonical "baml-roundrobin" is translated to upstream
// "baml-round-robin" via UpstreamClientRegistryProvider. The codegen
// helper writes Provider verbatim into the BAML registry, so any
// translation drift here would re-trigger the CFFI rejection that the
// helper is designed to avoid.
func TestBuildLegacyChildRegistryEntries_ProviderTranslation(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "InnerRR", Provider: "baml-roundrobin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafA"}}},
			{Name: "LeafA", Provider: "openai", ProviderSet: true},
		},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerRR", nil, nil)

	var found bool
	for _, e := range entries {
		if e.Name == "InnerRR" {
			found = true
			if e.Provider != "baml-round-robin" {
				t.Errorf("InnerRR provider not translated: got %q, want %q (canonical → upstream)",
					e.Provider, "baml-round-robin")
			}
		}
	}
	if !found {
		t.Fatalf("InnerRR (target with explicit baml-roundrobin override) missing from scoped registry; entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_OmittedProviderMaterialised pins
// that omitted-provider entries (strategy-only / start-only overrides
// on a static client) get their provider filled from the introspected
// map. Without this, the registry would forward provider="" and BAML's
// CFFI would reject before WithClient(leaf) resolves anything.
func TestBuildLegacyChildRegistryEntries_OmittedProviderMaterialised(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// No Provider, no ProviderSet — pure strategy-override.
			{Name: "StaticRR",
				Options: map[string]any{"strategy": []any{"LeafA"}}},
		},
	}
	introspectedProviders := map[string]string{
		"StaticRR": "baml-roundrobin",
	}

	entries := BuildLegacyChildRegistryEntries(reg, "StaticRR", introspectedProviders, nil)

	var found bool
	for _, e := range entries {
		if e.Name == "StaticRR" {
			found = true
			if e.Provider != "baml-round-robin" {
				t.Errorf("StaticRR provider not materialised from introspected map: got %q, want %q",
					e.Provider, "baml-round-robin")
			}
		}
	}
	if !found {
		t.Errorf("StaticRR (target with strategy override) missing from scoped registry; entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_PreservesOrder pins deterministic
// output order across the reachable subgraph. The slice mirrors
// reg.Clients order so repeated calls emit identical AddLlmClient
// sequences — important for any future caller that fingerprints the
// registry shape (cache keys, log snapshots). All clients in this
// fixture are reachable from InnerRR (both leaves appear in the
// runtime strategy chain) so the ordering assertion is independent of
// the reachability gate.
func TestBuildLegacyChildRegistryEntries_PreservesOrder(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "First", Provider: "openai", ProviderSet: true},
			{Name: "Second", Provider: "anthropic", ProviderSet: true},
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"First", "Second"}}},
		},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerRR", nil, nil)

	gotNames := make([]string, len(entries))
	for i, e := range entries {
		gotNames[i] = e.Name
	}
	want := []string{"First", "Second", "InnerRR"}
	if !reflect.DeepEqual(gotNames, want) {
		t.Errorf("order: got %v, want %v", gotNames, want)
	}
}

// TestBuildLegacyChildRegistryEntries_DropsUnreachableLeaf pins the
// non-strategy leaf side of the reachability gate. A runtime registry
// can carry leaves unrelated to the legacy-callback subgraph (e.g.
// other request-level dynamic clients). Forwarding them into the
// per-callback registry would leak names BAML's CFFI shouldn't see for
// this child — exactly the leak the dual-view split exists to prevent.
//
// Reachable leaves stay (covered by IncludesNonStrategyLeaves and the
// PreservesOrder fixture above); unreachable ones drop here.
func TestBuildLegacyChildRegistryEntries_DropsUnreachableLeaf(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "ReachableLeaf", Provider: "openai", ProviderSet: true},
			// Not reachable from InnerRR — must be dropped.
			{Name: "UnrelatedLeaf", Provider: "anthropic", ProviderSet: true},
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"ReachableLeaf"}}},
		},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerRR", nil, nil)

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	if !got["ReachableLeaf"] {
		t.Errorf("ReachableLeaf must survive — it is in InnerRR.strategy; entries=%+v", entries)
	}
	if got["UnrelatedLeaf"] {
		t.Errorf("UnrelatedLeaf (unreachable) leaked into scoped registry; entries=%+v", entries)
	}
	if !got["InnerRR"] {
		t.Errorf("InnerRR (target) must survive; entries=%+v", entries)
	}
}

// TestBuildLegacyChildRegistryEntries_CycleSafe pins that a reciprocal
// strategy graph (A → B → A, hostile or accidental input) doesn't
// infinite-loop the reachability walk. The visited set guards
// re-entry; both nodes appear once each.
func TestBuildLegacyChildRegistryEntries_CycleSafe(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "RR_A", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"RR_B"}}},
			{Name: "RR_B", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"RR_A"}}},
		},
	}

	// If the visited set were missing the walk would never return.
	entries := BuildLegacyChildRegistryEntries(reg, "RR_A", nil, nil)

	// Both nodes should appear; cycle visited bookkeeping prevents
	// duplicates.
	counts := make(map[string]int, len(entries))
	for _, e := range entries {
		counts[e.Name]++
	}
	if counts["RR_A"] != 1 || counts["RR_B"] != 1 {
		t.Errorf("cycle walk: got counts %v, want each node once", counts)
	}
}

// TestBuildLegacyChildRegistryEntries_NilRegistry pins the nil-input
// contract. The codegen helper guards on `original != nil` before
// calling — but the helper itself returns nil rather than panicking,
// so a future caller doesn't have to mirror the guard.
func TestBuildLegacyChildRegistryEntries_NilRegistry(t *testing.T) {
	if got := BuildLegacyChildRegistryEntries(nil, "Anything", nil, nil); got != nil {
		t.Errorf("expected nil for nil registry, got %v", got)
	}
}

// TestBuildLegacyChildRegistryEntries_InvalidStrategyOverrideStopsWalk
// pins the three-state semantics of the reachability walker for an
// invalid runtime strategy override. When the override is present-
// but-invalid (e.g. `strategy:"garbage"` parses to an empty chain),
// the walker must NOT silently fall back to the introspected chain
// — doing so would leak static descendants into the scoped registry
// that the operator's override clearly didn't intend.
//
// The orchestrator's invalid-nested preflight preempts this code
// path in production today, but BuildLegacyChildRegistryEntries is
// exported and the contract holds for any direct caller.
//
// Composition: `Outer → InnerRR (present && !valid strategy) →
// [StaticBlue, StaticGreen]` static chain. Walking from Outer must
// include InnerRR (the strategy parent rooted in the override)
// without visiting StaticBlue / StaticGreen as deeper strategy
// parents — a leak guard would also surface as descendants creeping
// into the entries when those names happened to be strategy parents
// themselves. To make the leak directly observable here, the deeper
// names ARE strategy parents; if the walker fell through, they'd
// appear in the scoped entries.
func TestBuildLegacyChildRegistryEntries_InvalidStrategyOverrideStopsWalk(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// InnerFallback's runtime strategy is malformed — an
			// invalid present override.
			{Name: "InnerFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": "garbage"}},
			// Static descendants under the introspected chain are
			// themselves strategy parents — the leak would surface
			// here as their inclusion in the scoped entries.
			{Name: "StaticDeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafX", "LeafY"}}},
			{Name: "OtherStaticRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafZ"}}},
		},
	}
	introspectedProviders := map[string]string{
		"InnerFallback":  "baml-fallback",
		"StaticDeeperRR": "baml-roundrobin",
		"OtherStaticRR":  "baml-roundrobin",
	}
	introspectedChains := map[string][]string{
		"InnerFallback": {"StaticDeeperRR", "OtherStaticRR"},
	}

	entries := BuildLegacyChildRegistryEntries(reg, "InnerFallback", introspectedProviders, introspectedChains)

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	if !got["InnerFallback"] {
		t.Errorf("InnerFallback (target with present-invalid override) must still be in the scoped registry; entries=%+v", entries)
	}
	if got["StaticDeeperRR"] {
		t.Errorf("StaticDeeperRR leaked through fall-through to introspected chain — invalid runtime override should stop the walk, not silently use the static graph; entries=%+v", entries)
	}
	if got["OtherStaticRR"] {
		t.Errorf("OtherStaticRR leaked through fall-through to introspected chain — invalid runtime override should stop the walk; entries=%+v", entries)
	}
}

// TestFindInvalidReachableStrategyOverride_FallbackInvalidStartNotRR
// pins that a fallback strategy node with a malformed `options.start`
// is NOT misclassified as PathReasonInvalidRoundRobinStartOverride by
// the transitive walker. `start` is RR-specific
// (engine/baml-runtime/src/runtime/runtime_interface/strategies/round_robin.rs
// config); BAML's fallback parser ignores unknown options, so a
// malformed start on a fallback node is not a misconfiguration the
// walker should re-route on. The walker should keep walking the
// fallback's strategy chain.
//
// Mirrors the top-level metadata classifier (orchestrator.go
// baml-fallback arm), which only checks
// hasInvalidStartOverride under the baml-roundrobin arm.
func TestFindInvalidReachableStrategyOverride_FallbackInvalidStartNotRR(t *testing.T) {
	// Fallback root with a valid strategy chain leading to a single
	// openai leaf. The malformed `start` on the fallback root is
	// noise to BAML; the walker must not classify it as RR-invalid.
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "OuterFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"LeafOK"},
					"start":    "not-an-int",
				}},
			{Name: "LeafOK", Provider: "openai", ProviderSet: true},
		},
	}
	introspectedProviders := map[string]string{
		"OuterFallback": "baml-fallback",
		"LeafOK":        "openai",
	}

	if reason := FindInvalidReachableStrategyOverride(
		reg, "OuterFallback", introspectedProviders, nil,
	); reason != "" {
		t.Errorf("walker classified fallback-with-malformed-start as %q; want empty (start is RR-specific, fallback nodes ignore it)", reason)
	}
}

// TestFindInvalidReachableStrategyOverride_RRInvalidStartStillCaught
// is the positive counterpart to FallbackInvalidStartNotRR: an RR
// node with a malformed `options.start` MUST surface as
// PathReasonInvalidRoundRobinStartOverride. Pins that the RR-only
// gate didn't accidentally suppress the legitimate RR-invalid-start
// classification.
func TestFindInvalidReachableStrategyOverride_RRInvalidStartStillCaught(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR", Provider: "baml-roundrobin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"LeafA", "LeafB"},
					"start":    "not-an-int",
				}},
			{Name: "LeafA", Provider: "openai", ProviderSet: true},
			{Name: "LeafB", Provider: "openai", ProviderSet: true},
		},
	}
	introspectedProviders := map[string]string{
		"MyRR":  "baml-roundrobin",
		"LeafA": "openai",
		"LeafB": "openai",
	}

	if reason := FindInvalidReachableStrategyOverride(
		reg, "MyRR", introspectedProviders, nil,
	); reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("walker reason for RR-invalid-start: got %q, want %q", reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestFindInvalidReachableStrategyOverride_FallbackDescendsToRRChild
// is the positive counterpart to FallbackInvalidStartNotRR: when a
// fallback strategy parent reaches an RR descendant with a malformed
// `options.start`, the walker MUST descend past the fallback and
// surface the RR child's invalid start. The fallback's own `start`
// (if any) is irrelevant — `start` is RR-specific — but the RR
// child's malformed start is the real misconfiguration that must be
// caught. Without this descent guarantee, a deeply-nested RR with a
// bad start under a fallback parent would slip through the walker
// and surface only as a per-callback failure inside RunStream-/
// CallOrchestration, where a later sibling could mask it.
func TestFindInvalidReachableStrategyOverride_FallbackDescendsToRRChild(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Outer fallback parent — its strategy chain points to a
			// deeper RR child. The fallback itself has no malformed
			// option; the bug to surface is on the RR descendant.
			{Name: "OuterFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"RRChild"}}},
			// RR descendant with malformed start. The walker must
			// reach this node and classify it as
			// PathReasonInvalidRoundRobinStartOverride.
			{Name: "RRChild", Provider: "baml-roundrobin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"LeafOK"},
					"start":    "not-an-int",
				}},
			{Name: "LeafOK", Provider: "openai", ProviderSet: true},
		},
	}
	introspectedProviders := map[string]string{
		"OuterFallback": "baml-fallback",
		"RRChild":       "baml-roundrobin",
		"LeafOK":        "openai",
	}

	if reason := FindInvalidReachableStrategyOverride(
		reg, "OuterFallback", introspectedProviders, nil,
	); reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("walker reason for fallback-reaches-RR-with-invalid-start: got %q, want %q (walker must descend past fallback to surface deep RR's invalid start)", reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestFindInvalidReachableStrategyOverride_TransitiveLeafEmptyProvider
// pins that a reachable non-strategy leaf with a runtime
// `Provider:"" + ProviderSet:true` (operator-typo'd empty-provider
// override) deep in a strategy chain surfaces
// PathReasonInvalidProviderOverride from the walker, NOT silently
// flow through into the scoped legacy child registry.
//
// Fixture: OuterFallback → [InnerRR, LaterLeaf]
//
//	InnerRR       → [LeafBad]
//	LeafBad       has Provider:"" + ProviderSet:true
//
// Without the fix, inStrategyGraph returns false for LeafBad (empty
// provider isn't an RR/fallback spelling and no introspected entry
// exists), so the walker short-circuits before checking
// hasInvalidProviderOverride. LeafBad would then be forwarded into
// the per-callback scoped registry by BuildLegacyChildRegistryEntries,
// BAML's eager registry parse would reject the empty-provider entry
// inside InnerRR's callback, and fallback orchestration could
// continue to the LaterLeaf sibling — silently masking the
// operator's invalid-provider override.
//
// LaterLeaf is load-bearing in the fixture: it's the trailing valid
// child that would have absorbed the deep failure under the buggy
// preflight. Its presence forces this test to depend on the
// pre-strategy-graph hasInvalidProviderOverride check specifically.
func TestFindInvalidReachableStrategyOverride_TransitiveLeafEmptyProvider(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "OuterFallback", Provider: "baml-fallback", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"InnerRR", "LaterLeaf"}}},
			{Name: "InnerRR", Provider: "baml-roundrobin", ProviderSet: true,
				Options: map[string]any{"strategy": []any{"LeafBad"}}},
			// Operator-typo'd: provider key is present but empty. BAML
			// rejects empty provider strings in ClientProvider::from_str.
			{Name: "LeafBad", Provider: "", ProviderSet: true},
			{Name: "LaterLeaf", Provider: "openai", ProviderSet: true},
		},
	}
	introspectedProviders := map[string]string{
		"OuterFallback": "baml-fallback",
		"InnerRR":       "baml-roundrobin",
		"LaterLeaf":     "openai",
		// LeafBad is intentionally NOT in the introspected map — the
		// runtime present-empty override is the operator's only signal
		// for this name, and the walker must classify on it directly
		// rather than fall through to a missing introspected entry.
	}

	if reason := FindInvalidReachableStrategyOverride(
		reg, "OuterFallback", introspectedProviders, nil,
	); reason != PathReasonInvalidProviderOverride {
		t.Errorf("walker reason for transitive empty-provider leaf: got %q, want %q (reachable non-strategy leaf with operator-typo'd empty provider must surface invalid-provider-override; otherwise BAML's eager registry parse rejects inside the callback and a later fallback sibling can mask it)", reason, PathReasonInvalidProviderOverride)
	}
}
