package roundrobin

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestResolve_NonRRClient_ReturnsAsIs(t *testing.T) {
	in := ResolveInput{
		ClientName:      "PlainClient",
		ClientProviders: map[string]string{"PlainClient": "openai"},
		Coordinator:     NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Selected != "PlainClient" {
		t.Fatalf("selected: want PlainClient, got %q", res.Selected)
	}
	if res.Info != nil {
		t.Fatalf("expected nil Info for non-RR, got %+v", res.Info)
	}
}

func TestResolve_FallbackClient_ReturnsAsIs(t *testing.T) {
	// A fallback strategy is not RR — resolver should leave it for the
	// fallback chain resolver downstream.
	in := ResolveInput{
		ClientName:      "MyFallback",
		ClientProviders: map[string]string{"MyFallback": "baml-fallback"},
		FallbackChains:  map[string][]string{"MyFallback": {"A", "B"}},
		Coordinator:     NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Selected != "MyFallback" {
		t.Fatalf("selected: want MyFallback, got %q", res.Selected)
	}
	if res.Info != nil {
		t.Fatalf("expected nil Info, got %+v", res.Info)
	}
}

func TestResolve_RRClient_PicksFromChain(t *testing.T) {
	in := ResolveInput{
		ClientName: "MyRR",
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai",
			"B":    "anthropic",
			"C":    "google-ai",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B", "C"}},
		Coordinator:    NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Info == nil {
		t.Fatalf("expected Info, got nil")
	}
	if res.Info.Name != "MyRR" {
		t.Fatalf("Info.Name: want MyRR, got %q", res.Info.Name)
	}
	if len(res.Info.Children) != 3 {
		t.Fatalf("Info.Children: want 3, got %v", res.Info.Children)
	}
	if res.Info.Selected != res.Selected {
		t.Fatalf("Selected mismatch: info=%q result=%q", res.Info.Selected, res.Selected)
	}
	// Selected must appear in the chain
	found := false
	for _, ch := range res.Info.Children {
		if ch == res.Selected {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Selected %q not in chain %v", res.Selected, res.Info.Children)
	}
}

func TestResolve_AlternateSpellings(t *testing.T) {
	cases := []string{"baml-roundrobin", "baml-round-robin", "round-robin"}
	for _, spelling := range cases {
		t.Run(spelling, func(t *testing.T) {
			in := ResolveInput{
				ClientName:      "MyRR",
				ClientProviders: map[string]string{"MyRR": spelling, "A": "openai", "B": "anthropic"},
				FallbackChains:  map[string][]string{"MyRR": {"A", "B"}},
				Coordinator:     NewCoordinator(),
			}
			res, err := Resolve(in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Info == nil {
				t.Fatalf("spelling %q not recognised as RR", spelling)
			}
		})
	}
}

func TestResolve_NestedRR_ReportsOutermost(t *testing.T) {
	// Outer RR [InnerRR, Plain]; InnerRR [X, Y].
	// Regardless of what each RR picks, result.Info must describe Outer.
	in := ResolveInput{
		ClientName: "Outer",
		ClientProviders: map[string]string{
			"Outer":   "baml-roundrobin",
			"InnerRR": "baml-roundrobin",
			"Plain":   "openai",
			"X":       "openai",
			"Y":       "anthropic",
		},
		FallbackChains: map[string][]string{
			"Outer":   {"InnerRR", "Plain"},
			"InnerRR": {"X", "Y"},
		},
		Coordinator: NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Info == nil {
		t.Fatalf("expected Info")
	}
	if res.Info.Name != "Outer" {
		t.Fatalf("Info.Name: want Outer (outermost), got %q", res.Info.Name)
	}
	// Selected might be "Plain", "X", or "Y" depending on random outer
	// selection and (when outer picked InnerRR) inner selection.
	switch res.Selected {
	case "Plain", "X", "Y":
	default:
		t.Fatalf("unexpected selected: %q", res.Selected)
	}
}

func TestResolve_CycleDetected(t *testing.T) {
	// Pathological: RR pointing to itself via a different name.
	in := ResolveInput{
		ClientName: "A",
		ClientProviders: map[string]string{
			"A": "baml-roundrobin",
			"B": "baml-roundrobin",
		},
		FallbackChains: map[string][]string{
			"A": {"B"},
			"B": {"A"},
		},
		Coordinator: NewCoordinator(),
	}
	_, err := Resolve(in)
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestResolve_EmptyChainErrors(t *testing.T) {
	in := ResolveInput{
		ClientName:      "RRNoKids",
		ClientProviders: map[string]string{"RRNoKids": "baml-roundrobin"},
		FallbackChains:  map[string][]string{},
		Coordinator:     NewCoordinator(),
	}
	_, err := Resolve(in)
	if err == nil {
		t.Fatal("expected error for empty chain")
	}
}

func TestResolve_EmptyClientName(t *testing.T) {
	res, err := Resolve(ResolveInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Selected != "" || res.Info != nil {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestResolve_RuntimeStrategyOverride(t *testing.T) {
	// Introspected chain is [A, B], but runtime override supplies [C, D].
	// Resolver must walk the override.
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:    "MyRR",
				Options: map[string]any{"strategy": []any{"C", "D"}},
			},
		},
	}
	in := ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin", // introspected; runtime does not override provider
			"C":    "openai",
			"D":    "anthropic",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B"}}, // should be ignored
		Coordinator:    NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Info == nil {
		t.Fatalf("expected Info")
	}
	if got := res.Info.Children; !sliceEqual(got, []string{"C", "D"}) {
		t.Fatalf("children: want [C D], got %v", got)
	}
}

func TestResolve_DynamicRRClient_DoesNotTouchCoordinator(t *testing.T) {
	// Runtime-registered client with RR provider. Counter state must not
	// persist across two Resolve calls — this matches BAML's fresh-Arc
	// lifecycle for override clients.
	coord := NewCoordinator()
	makeInput := func() ResolveInput {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "DynRR",
					Provider: "baml-roundrobin",
					Options:  map[string]any{"strategy": []any{"A", "B", "C"}},
				},
			},
		}
		return ResolveInput{
			ClientName:      "DynRR",
			Registry:        reg,
			ClientProviders: map[string]string{"A": "openai", "B": "anthropic", "C": "google-ai"},
			FallbackChains:  map[string][]string{},
			Coordinator:     coord,
		}
	}
	// Drive many resolutions; distribution should be effectively random.
	// If the coordinator were being used, the indices would be a contiguous
	// sequence instead. We verify here by checking that the coordinator's
	// counters map remains empty after repeated calls for the dynamic client.
	for i := 0; i < 20; i++ {
		if _, err := Resolve(makeInput()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	// coord.counters is a sync.Map; count entries by ranging.
	count := 0
	coord.counters.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatalf("dynamic RR leaked into coordinator: %d entries", count)
	}
}

func TestResolve_StrategyOnlyOverride_IsDynamic(t *testing.T) {
	// Static RR provider from .baml source, but registry overrides the
	// strategy list. Advancing the static counter in this state would
	// rotate through children the operator never configured, so the
	// resolver must switch to the fresh-per-request path and leave the
	// coordinator untouched.
	coord := NewCoordinator()
	makeInput := func() ResolveInput {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:    "MyRR", // no Provider override — same RR provider
					Options: map[string]any{"strategy": []any{"C", "D"}},
				},
			},
		}
		return ResolveInput{
			ClientName: "MyRR",
			Registry:   reg,
			ClientProviders: map[string]string{
				"MyRR": "baml-roundrobin",
				"C":    "openai",
				"D":    "anthropic",
			},
			FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
			Coordinator:    coord,
		}
	}
	for i := 0; i < 25; i++ {
		res, err := Resolve(makeInput())
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// Selection must always come from the override chain, never the
		// introspected one.
		switch res.Selected {
		case "C", "D":
		default:
			t.Fatalf("selected %q not in override chain [C D]", res.Selected)
		}
	}
	count := 0
	coord.counters.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatalf("strategy-only override leaked into coordinator: %d entries", count)
	}
}

func TestResolve_RegistryPresenceWithoutOverride_IsDynamic(t *testing.T) {
	// A registry entry with no strategy and no provider override still
	// counts as dynamic — BAML upstream rebuilds the Arc whenever the
	// registry touches a client. We mirror that by bypassing the
	// coordinator on any registry hit.
	coord := NewCoordinator()
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR"}, // presence only
		},
	}
	in := ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai",
			"B":    "anthropic",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
		Coordinator:    coord,
	}
	for i := 0; i < 10; i++ {
		if _, err := Resolve(in); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	count := 0
	coord.counters.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatalf("registry-present RR leaked into coordinator: %d entries", count)
	}
}

func TestResolve_RRChildIsFallback_StopsAtFallback(t *testing.T) {
	// Outer RR whose children include a baml-fallback client. The fallback
	// is not a RR provider, so resolution must stop at the selected child
	// and leave chain resolution to the downstream fallback handler.
	in := ResolveInput{
		ClientName: "OuterRR",
		ClientProviders: map[string]string{
			"OuterRR": "baml-roundrobin",
			"Fb":      "baml-fallback",
			"Plain":   "openai",
			"A":       "openai",
			"B":       "anthropic",
		},
		FallbackChains: map[string][]string{
			"OuterRR": {"Fb", "Plain"},
			"Fb":      {"A", "B"},
		},
		Coordinator: NewCoordinator(),
	}
	for i := 0; i < 20; i++ {
		res, err := Resolve(in)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Info == nil || res.Info.Name != "OuterRR" {
			t.Fatalf("Info.Name: want OuterRR, got %+v", res.Info)
		}
		// Resolver must not unwrap the fallback — Selected is either "Fb"
		// or "Plain", never "A" or "B".
		switch res.Selected {
		case "Fb", "Plain":
		default:
			t.Fatalf("selected: got %q, want Fb or Plain (fallback unwrap leaked)", res.Selected)
		}
	}
}

func TestResolve_RespectsCoordinatorStartSeed(t *testing.T) {
	// End-to-end: when the coordinator was constructed with a start for
	// this RR client, the first resolution must pick the start index.
	coord := NewCoordinatorWithStarts(map[string]int{"MyRR": 2})
	in := ResolveInput{
		ClientName: "MyRR",
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai",
			"B":    "anthropic",
			"C":    "google-ai",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B", "C"}},
		Coordinator:    coord,
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Info == nil {
		t.Fatalf("expected Info")
	}
	if res.Info.Index != 2 || res.Info.Selected != "C" {
		t.Fatalf("first pick: got index=%d selected=%q, want index=2 selected=C", res.Info.Index, res.Info.Selected)
	}
}

func TestIsRoundRobinProvider_AcceptsSpellings(t *testing.T) {
	cases := map[string]bool{
		"baml-roundrobin":  true,
		"baml-round-robin": true,
		"round-robin":      true,
		"baml-fallback":    false,
		"openai":           false,
		"":                 false,
	}
	for in, want := range cases {
		if got := IsRoundRobinProvider(in); got != want {
			t.Errorf("IsRoundRobinProvider(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNormalizeProvider_CanonicalisesSpellings(t *testing.T) {
	cases := map[string]string{
		"baml-roundrobin":  "baml-roundrobin",
		"baml-round-robin": "baml-roundrobin",
		"round-robin":      "baml-roundrobin",
		"baml-fallback":    "baml-fallback",
		"openai":           "openai",
	}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
