package roundrobin

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestResolve_NonRRClient_ReturnsAsIs(t *testing.T) {
	in := ResolveInput{
		ClientName:      "PlainClient",
		ClientProviders: map[string]string{"PlainClient": "openai"},
		Advancer:         NewCoordinator(),
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
		Advancer:         NewCoordinator(),
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
		Advancer:        NewCoordinator(),
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
				Advancer:         NewCoordinator(),
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
		Advancer:     NewCoordinator(),
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
		Advancer:     NewCoordinator(),
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
		Advancer:         NewCoordinator(),
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
		Advancer:        NewCoordinator(),
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

func TestResolve_RuntimeStrategyOverride_QuotedString(t *testing.T) {
	// Regression for the round-robin cold-review finding on parser
	// unification: a runtime client_registry strategy override passed
	// as a bracketed string ("strategy [\"A\", \"B\"]") must strip the
	// surrounding quotes before matching introspected client names.
	// Previously the RR resolver kept its own parser that preserved
	// quotes, silently collapsing the chain to unknown-named clients;
	// both strategies now share bamlutils/strategyparse.
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:    "MyRR",
				Options: map[string]any{"strategy": `strategy ["ClientC", "ClientD"]`},
			},
		},
	}
	in := ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR":    "baml-roundrobin",
			"ClientC": "openai",
			"ClientD": "anthropic",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
		Advancer:       NewCoordinator(),
	}
	res, err := Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Info == nil {
		t.Fatalf("expected Info")
	}
	if got := res.Info.Children; !sliceEqual(got, []string{"ClientC", "ClientD"}) {
		t.Fatalf("children: want [ClientC ClientD] (quotes stripped), got %v", got)
	}
	// Selected must be one of the unquoted child names, not a quoted
	// variant. Before the parser unification the resolver would
	// select `"ClientC"` (with quotes) which doesn't match any
	// introspected provider.
	if res.Selected != "ClientC" && res.Selected != "ClientD" {
		t.Fatalf("selected %q is not an unquoted configured child", res.Selected)
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
			Advancer:         coord,
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
			Advancer:        coord,
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
		Advancer:        coord,
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
		Advancer:     NewCoordinator(),
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
		Advancer:        coord,
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

// TestResolve_InvalidStrategyOverride_ReturnsSentinel covers PR #192
// cold-review-2 finding 1 for the round-robin path. A runtime
// client_registry entry whose `options.strategy` value cannot be
// parsed as a non-empty bracketed list must surface as
// ErrInvalidStrategyOverride so ResolveEffectiveClient skips the RR
// unwrap and lets the request fall through to legacy, where BAML's
// runtime emits the canonical ensure_strategy error rather than us
// silently using the introspected chain.
func TestResolve_InvalidStrategyOverride_ReturnsSentinel(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"empty []string", []string{}},
		{"empty []any", []any{}},
		{"empty bracket string", "[]"},
		{"prefix + empty brackets", "strategy []"},
		{"half-bracketed string", "strategy [A"},
		{"bare token string", "ClientA"},
		{"heterogeneous []any", []any{"A", 42}},
		{"only-blank []string", []string{"", "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyRR",
						Provider: "baml-roundrobin",
						Options:  map[string]any{"strategy": tc.raw},
					},
				},
			}
			res, err := Resolve(ResolveInput{
				ClientName:      "MyRR",
				Registry:        reg,
				ClientProviders: map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
				FallbackChains:  map[string][]string{"MyRR": {"A", "B"}},
				Advancer:        NewCoordinator(),
			})
			if !errors.Is(err, ErrInvalidStrategyOverride) {
				t.Fatalf("expected ErrInvalidStrategyOverride; got err=%v res=%+v", err, res)
			}
			if res != nil {
				t.Errorf("expected nil result alongside sentinel; got %+v", res)
			}
		})
	}
}

// TestResolve_AbsentStrategyOverride_DoesNotTriggerSentinel guards
// against the inverse regression: an RR registry entry that lacks the
// `strategy` key must continue to fall back to the introspected chain
// rather than tripping ErrInvalidStrategyOverride.
func TestResolve_AbsentStrategyOverride_DoesNotTriggerSentinel(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"temperature": 0.7},
			},
		},
	}
	res, err := Resolve(ResolveInput{
		ClientName:      "MyRR",
		Registry:        reg,
		ClientProviders: map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
		FallbackChains:  map[string][]string{"MyRR": {"A", "B"}},
		Advancer:        NewCoordinator(),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res == nil || res.Selected == "" {
		t.Fatalf("expected resolved leaf, got %+v", res)
	}
}

// TestResolve_PresentEmptyProviderSkipsRRUnwrap covers PR #192
// cold-review-2 verdict-8 for the round-robin resolver. A registry
// entry with explicit "provider":"" must not be treated as RR — the
// resolver returns the un-unwrapped client name as the leaf so the
// dispatcher's BuildRequest gate fails and the request falls through
// to legacy, where BAML's runtime emits its native invalid-provider
// error. Without presence tracking the resolver previously fell
// through to the introspected RR provider and silently advanced the
// counter on a malformed override.
func TestResolve_PresentEmptyProviderSkipsRRUnwrap(t *testing.T) {
	coord := NewCoordinator()
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR", Provider: "", ProviderSet: true},
		},
	}
	res, err := Resolve(ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai",
			"B":    "anthropic",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
		Advancer:        coord,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Selected != "MyRR" {
		t.Fatalf("selected: got %q, want MyRR (RR unwrap must be skipped on present-empty provider)", res.Selected)
	}
	if res.Info != nil {
		t.Errorf("Info: got %+v, want nil (no RR decision occurred)", res.Info)
	}
	count := 0
	coord.counters.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Errorf("present-empty leaked into coordinator: %d entries", count)
	}
}

// TestResolve_DynamicStartOverride covers PR #192 cold-review-3
// finding 3. A registry-touched RR client must use `options.start` as
// the deterministic initial child index instead of the random
// AdvanceDynamic fallback. Behaviour mirrors BAML upstream's
// resolve_strategy in roundrobin.rs:64-65, which does
// `(start as usize) % strategy.len()` for fresh request-scoped RR.
//
// Accepted shapes match BAML's i32 ensure_int (helpers.rs:168-180,
// :917-930): signed integer kinds + finite whole float64 within
// [MinInt32, MaxInt32]. unsigned types and json.Number are rejected
// (covered by TestResolve_InvalidStartOverride_ReturnsSentinel) — they
// don't survive BAML's Go encoder. See cold-review-3 signoff-10 F3.
func TestResolve_DynamicStartOverride(t *testing.T) {
	cases := []struct {
		name      string
		start     any
		chain     []string
		wantIndex int
		wantLeaf  string
	}{
		{"start=0 picks first", 0, []string{"A", "B", "C"}, 0, "A"},
		{"start=1 picks second", 1, []string{"A", "B", "C"}, 1, "B"},
		{"start=2 picks third", 2, []string{"A", "B", "C"}, 2, "C"},
		{"start beyond length wraps", 5, []string{"A", "B"}, 1, "B"},
		{"negative start clamps to zero", -1, []string{"A", "B"}, 0, "A"},
		{"int64 within int32 honoured", int64(1), []string{"A", "B"}, 1, "B"},
		{"int32 max-1 honoured", int32(math.MaxInt32 - 1), []string{"A", "B"}, 0, "A"}, // (2^31-2) % 2 == 0
		{"int8 honoured", int8(1), []string{"A", "B"}, 1, "B"},
		{"float64 with no fraction honoured", float64(1), []string{"A", "B"}, 1, "B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coord := NewCoordinator()
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyRR",
						Provider: "baml-roundrobin",
						Options:  map[string]any{"start": tc.start},
					},
				},
			}
			providers := map[string]string{"MyRR": "baml-roundrobin"}
			for _, c := range tc.chain {
				providers[c] = "openai"
			}
			res, err := Resolve(ResolveInput{
				ClientName:      "MyRR",
				Registry:        reg,
				ClientProviders: providers,
				FallbackChains:  map[string][]string{"MyRR": tc.chain},
				Advancer:        coord,
			})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Info == nil {
				t.Fatalf("expected Info, got nil")
			}
			if res.Info.Index != tc.wantIndex {
				t.Errorf("Info.Index: got %d, want %d", res.Info.Index, tc.wantIndex)
			}
			if res.Selected != tc.wantLeaf {
				t.Errorf("Selected: got %q, want %q", res.Selected, tc.wantLeaf)
			}
			// Dynamic RR with start must NOT touch the static
			// coordinator — it's a fresh-per-request value.
			count := 0
			coord.counters.Range(func(_, _ any) bool { count++; return true })
			if count != 0 {
				t.Errorf("dynamic start leaked into coordinator: %d entries", count)
			}
		})
	}
}

// TestResolve_DynamicStartOverride_DeterministicAcrossRequests pins
// the per-request semantics: with `start: N`, every fresh request
// picks the same first child. Mirrors BAML's "fresh Arc per context"
// where current_index is reset to start each request.
func TestResolve_DynamicStartOverride_DeterministicAcrossRequests(t *testing.T) {
	makeInput := func() ResolveInput {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyRR",
					Provider: "baml-roundrobin",
					Options:  map[string]any{"start": 1},
				},
			},
		}
		return ResolveInput{
			ClientName: "MyRR",
			Registry:   reg,
			ClientProviders: map[string]string{
				"MyRR": "baml-roundrobin",
				"A":    "openai",
				"B":    "anthropic",
				"C":    "google-ai",
			},
			FallbackChains: map[string][]string{"MyRR": {"A", "B", "C"}},
			Advancer:       NewCoordinator(),
		}
	}
	for i := 0; i < 20; i++ {
		res, err := Resolve(makeInput())
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if res.Selected != "B" {
			t.Fatalf("iteration %d: Selected = %q, want B (deterministic per-request start)", i, res.Selected)
		}
	}
}

// TestResolve_DynamicStartOverride_StrategyOnlyChain pins the
// composition with strategy-only overrides: a registry entry that
// supplies BOTH `strategy` and `start` must select from the override
// chain, not the introspected one.
func TestResolve_DynamicStartOverride_StrategyOnlyChain(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name: "MyRR",
				Options: map[string]any{
					"strategy": []any{"X", "Y", "Z"},
					"start":    2,
				},
			},
		},
	}
	res, err := Resolve(ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"X":    "openai",
			"Y":    "anthropic",
			"Z":    "google-ai",
		},
		FallbackChains: map[string][]string{"MyRR": {"A", "B", "C"}}, // ignored
		Advancer:       NewCoordinator(),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Selected != "Z" {
		t.Errorf("Selected: got %q, want Z (start=2 in override chain [X Y Z])", res.Selected)
	}
}

// TestResolve_InvalidStartOverride_ReturnsSentinel covers the
// invalid-shape arm of finding 3, tightened in signoff-10 to match
// BAML's i32 contract: values outside [MinInt32, MaxInt32] are
// rejected, and unsigned types + json.Number are rejected outright
// because they cannot survive BAML's Go-side CFFI encoder (no uint
// branch; json.Number encoded as string).
func TestResolve_InvalidStartOverride_ReturnsSentinel(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		// Wrong types
		{"numeric string", "1"},
		{"empty string", ""},
		{"fractional float", 1.5},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"boolean true", true},
		{"boolean false", false},
		{"slice", []any{1, 2}},
		{"map", map[string]any{"x": 1}},
		{"nil", nil},
		// Unsigned integer types — rejected outright per signoff-10
		// since BAML's Go encoder lacks a uint branch. CodeRabbit
		// verdict-21 finding 6: plain `uint` was missing from the
		// matrix; included here to pin that the platform-sized
		// unsigned kind also falls into the default rejection branch
		// rather than silently accepting via an unintended type
		// switch case.
		{"uint", uint(1)},
		{"uint8", uint8(1)},
		{"uint16", uint16(1)},
		{"uint32", uint32(1)},
		{"uint64", uint64(1)},
		{"oversized uint64", uint64(math.MaxUint64)},
		// json.Number — rejected outright; BAML encodes it as a
		// string which the upstream decoder rejects for an i32 option.
		{"json.Number string-form", json.Number("5")},
		{"json.Number invalid", json.Number("abc")},
		// Out-of-int32-range values — accepted before signoff-10 but
		// would never survive BAML's parse::<i32>().
		{"int64 above MaxInt32", int64(math.MaxInt32) + 1},
		{"int64 below MinInt32", int64(math.MinInt32) - 1},
		{"float64 above MaxInt32", float64(math.MaxInt32) + 1},
		{"float64 below MinInt32", float64(math.MinInt32) - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyRR",
						Provider: "baml-roundrobin",
						Options:  map[string]any{"start": tc.raw},
					},
				},
			}
			res, err := Resolve(ResolveInput{
				ClientName: "MyRR",
				Registry:   reg,
				ClientProviders: map[string]string{
					"MyRR": "baml-roundrobin",
					"A":    "openai",
					"B":    "anthropic",
				},
				FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
				Advancer:       NewCoordinator(),
			})
			if !errors.Is(err, ErrInvalidStartOverride) {
				t.Fatalf("expected ErrInvalidStartOverride; got err=%v res=%+v", err, res)
			}
			// Sentinel-error contract pin (CodeRabbit verdict-38
			// finding F3): mirror the InvalidStrategyOverride sibling
			// at resolver_test.go:533-535 — the sentinel must arrive
			// alongside a nil result so callers can rely on either-or
			// semantics rather than checking both fields.
			if res != nil {
				t.Errorf("expected nil result alongside sentinel; got %+v", res)
			}
		})
	}
}

// TestResolve_AbsentStartOverride_RandomSelection guards against the
// inverse regression: an RR registry entry that omits `start` must
// keep the existing AdvanceDynamic random fallback. Without this
// guard the F3 plumbing could leak a determinism into the absent
// case.
func TestResolve_AbsentStartOverride_RandomSelection(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B", "C", "D"}},
			},
		},
	}
	in := ResolveInput{
		ClientName: "MyRR",
		Registry:   reg,
		ClientProviders: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai", "B": "openai", "C": "openai", "D": "openai",
		},
		FallbackChains: map[string][]string{},
		Advancer:       NewCoordinator(),
	}
	// Run many iterations and verify that more than one distinct child
	// is observed. With deterministic start we'd see only one; with
	// AdvanceDynamic random we expect multiple.
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		res, err := Resolve(in)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		seen[res.Selected] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected random distribution across multiple children, only saw: %v", seen)
	}
}

// TestInspectStartOverride_AcceptedShapes pins the integer-shape
// contract independently of selectIndex so future refactors keep the
// same accept/reject set. Tightened in signoff-10 to match BAML's i32
// ensure_int (helpers.rs:168-180): only signed types within
// [MinInt32, MaxInt32] and finite whole float64 in the same range
// are accepted. Unsigned types and json.Number are rejected because
// they cannot survive BAML's Go-side CFFI encoder.
func TestInspectStartOverride_AcceptedShapes(t *testing.T) {
	cases := []struct {
		name        string
		raw         any
		wantPresent bool
		wantValid   bool
		wantStart   int
	}{
		{"absent (no start key)", nil, false, true, 0},
		// Accepted: signed ints + finite whole float64 in i32 range.
		{"int", int(5), true, true, 5},
		{"int8", int8(5), true, true, 5},
		{"int16", int16(5), true, true, 5},
		{"int32", int32(5), true, true, 5},
		{"int64 in range", int64(5), true, true, 5},
		{"int32 max", int32(math.MaxInt32), true, true, math.MaxInt32},
		{"int32 min", int32(math.MinInt32), true, true, math.MinInt32},
		{"float64 zero-fraction", float64(5), true, true, 5},
		{"negative int", int(-3), true, true, -3},
		// Rejected: out-of-i32-range.
		{"int64 MaxInt32+1", int64(math.MaxInt32) + 1, true, false, 0},
		{"int64 MinInt32-1", int64(math.MinInt32) - 1, true, false, 0},
		{"float64 MaxInt32+1", float64(math.MaxInt32) + 1, true, false, 0},
		{"float64 MinInt32-1", float64(math.MinInt32) - 1, true, false, 0},
		// Rejected: unsigned types — no upstream encoder branch.
		// CodeRabbit verdict-21 finding 6: include plain `uint` so the
		// platform-sized kind is also pinned to rejection.
		{"uint", uint(5), true, false, 0},
		{"uint8", uint8(5), true, false, 0},
		{"uint16", uint16(5), true, false, 0},
		{"uint32", uint32(5), true, false, 0},
		{"uint64 small", uint64(5), true, false, 0},
		{"uint32 above MaxInt32", uint32(math.MaxInt32) + 1, true, false, 0},
		{"uint64 max", uint64(math.MaxUint64), true, false, 0},
		// Rejected: json.Number — encoded as string upstream.
		{"json.Number valid", json.Number("5"), true, false, 0},
		{"json.Number invalid", json.Number("abc"), true, false, 0},
		// Rejected: wrong types.
		{"fractional float", 1.5, true, false, 0},
		{"NaN", math.NaN(), true, false, 0},
		{"+Inf", math.Inf(1), true, false, 0},
		{"numeric string", "5", true, false, 0},
		{"bool", true, true, false, 0},
		{"slice", []any{1}, true, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts map[string]any
			if tc.name != "absent (no start key)" {
				opts = map[string]any{"start": tc.raw}
			}
			start, present, valid := InspectStartOverride(opts)
			if present != tc.wantPresent || valid != tc.wantValid || start != tc.wantStart {
				t.Errorf("got (start=%d present=%v valid=%v); want (start=%d present=%v valid=%v)",
					start, present, valid, tc.wantStart, tc.wantPresent, tc.wantValid)
			}
		})
	}
}

// TestInspectStrategyOverride_DirectShapes pins the contract for the
// exported helper so the orchestrator-level metadata classifier and
// the resolver-level RR sentinel both stay aligned. Previously each
// side kept a private copy that risked drifting on quote handling,
// empty-list semantics, or bracketed-string parsing — verdict-11
// finding 1 unified them on this implementation.
func TestInspectStrategyOverride_DirectShapes(t *testing.T) {
	cases := []struct {
		name        string
		client      *bamlutils.ClientProperty
		wantPresent bool
		wantValid   bool
		wantChain   []string
	}{
		{
			name:        "no registry entry → absent",
			client:      nil,
			wantPresent: false,
			wantValid:   true,
		},
		{
			name:        "entry with no options → absent",
			client:      &bamlutils.ClientProperty{Name: "MyRR", Provider: "baml-roundrobin"},
			wantPresent: false,
			wantValid:   true,
		},
		{
			name: "entry with options but no strategy key → absent",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"temperature": 0.7},
			},
			wantPresent: false,
			wantValid:   true,
		},
		{
			name: "valid []any chain",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
			wantPresent: true,
			wantValid:   true,
			wantChain:   []string{"A", "B"},
		},
		{
			name: "valid bracketed string with quoted tokens",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": `["ClientC","ClientD"]`},
			},
			wantPresent: true,
			wantValid:   true,
			wantChain:   []string{"ClientC", "ClientD"},
		},
		{
			name: "empty array → present-but-invalid",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": []any{}},
			},
			wantPresent: true,
			wantValid:   false,
		},
		{
			name: "bare token string → present-but-invalid",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": "ClientA"},
			},
			wantPresent: true,
			wantValid:   false,
		},
		{
			name: "half-bracketed string → present-but-invalid",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": "[A"},
			},
			wantPresent: true,
			wantValid:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reg *bamlutils.ClientRegistry
			if tc.client != nil {
				reg = &bamlutils.ClientRegistry{
					Clients: []*bamlutils.ClientProperty{tc.client},
				}
			}
			chain, present, valid := InspectStrategyOverride(reg, "MyRR")
			if present != tc.wantPresent || valid != tc.wantValid {
				t.Errorf("got (present=%v valid=%v); want (present=%v valid=%v)",
					present, valid, tc.wantPresent, tc.wantValid)
			}
			if !sliceEqual(chain, tc.wantChain) {
				t.Errorf("chain: got %v, want %v", chain, tc.wantChain)
			}
		})
	}
}

// outOfRangeAdvancer is a stub Advancer that returns whatever index
// it was constructed with, regardless of childCount. Used to exercise
// the resolver's defensive bounds check on indices outside [0, n)
// (CodeRabbit verdict-33 finding F3): every shipped advancer respects
// the contract, but a buggy custom implementation could panic the
// resolver via `chain[idx]` without the guard.
type outOfRangeAdvancer struct {
	idx int
}

func (a outOfRangeAdvancer) Advance(_ string, _ int) (int, error) {
	return a.idx, nil
}

func TestResolve_AdvancerOutOfRangeReturnsErrorNotPanic(t *testing.T) {
	cases := []struct {
		name string
		idx  int
	}{
		{name: "negative index", idx: -1},
		{name: "index equals childCount", idx: 2},
		{name: "index greatly above childCount", idx: 999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ResolveInput{
				ClientName: "MyRR",
				ClientProviders: map[string]string{
					"MyRR": "baml-roundrobin",
					"A":    "openai",
					"B":    "anthropic",
				},
				FallbackChains: map[string][]string{"MyRR": {"A", "B"}},
				Advancer:       outOfRangeAdvancer{idx: tc.idx},
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Resolve panicked on out-of-range advancer index %d: %v", tc.idx, r)
				}
			}()
			res, err := Resolve(in)
			if err == nil {
				t.Fatalf("expected resolver error for advancer idx=%d, got selected=%q", tc.idx, res.Selected)
			}
			if msg := err.Error(); !contains(msg, "out of range") {
				t.Errorf("error message should mention 'out of range'; got %q", msg)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestResolve_RuntimeRRWithoutStrategy_ReturnsInvalidStrategySentinel
// pins CodeRabbit verdict-39 finding F11. When the runtime
// client_registry declares an RR provider (`provider:"baml-roundrobin"`)
// but does NOT supply `options.strategy`, the resolver previously
// returned the generic "has no children" error. That bypassed the
// ErrInvalid* sentinel routing — ResolveEffectiveClient only converts
// ErrInvalidStrategyOverride / ErrInvalidStartOverride to legacy
// fallthrough, so operators saw an opaque request error instead of
// the canonical BAML ensure_strategy message they get for the
// invalid-strategy-array case at line 151.
//
// The detection mirrors ResolveEffectiveClient's recognition
// condition: registry entry exists for the client AND has its
// Provider field explicitly set (IsProviderPresent). InspectStrategy
// Override already covered present-but-unparseable; this case is
// "provider declared, strategy absent".
func TestResolve_RuntimeRRWithoutStrategy_ReturnsInvalidStrategySentinel(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:        "MyRR",
				Provider:    "baml-roundrobin", // operator declared RR via runtime override
				ProviderSet: true,
				// No options.strategy, no introspected chain → empty chain.
			},
		},
	}
	res, err := Resolve(ResolveInput{
		ClientName:      "MyRR",
		Registry:        reg,
		ClientProviders: map[string]string{}, // no static entry; provider comes solely from runtime
		FallbackChains:  map[string][]string{},
		Advancer:        NewCoordinator(),
	})
	if !errors.Is(err, ErrInvalidStrategyOverride) {
		t.Fatalf("expected ErrInvalidStrategyOverride; got err=%v res=%+v", err, res)
	}
	if res != nil {
		t.Errorf("expected nil result alongside sentinel; got %+v", res)
	}
}

// TestResolve_StaticRREmptyChain_KeepsGenericError pins the inverse:
// when the RR provider was declared in the .baml source (introspected
// map) and somehow has no chain — a configuration error in static
// config rather than in a runtime override — the generic
// "has no children" error is still appropriate. Without this guard
// the F11 branch could over-broaden and convert static-config
// failures into legacy-fallthrough territory.
func TestResolve_StaticRRWithoutStrategy_KeepsGenericError(t *testing.T) {
	res, err := Resolve(ResolveInput{
		ClientName:      "StaticRR",
		Registry:        nil, // no runtime override
		ClientProviders: map[string]string{"StaticRR": "baml-roundrobin"},
		FallbackChains:  map[string][]string{}, // no introspected chain
		Advancer:        NewCoordinator(),
	})
	if errors.Is(err, ErrInvalidStrategyOverride) {
		t.Fatalf("static-config RR with no chain should NOT use the runtime sentinel; got ErrInvalidStrategyOverride")
	}
	if err == nil {
		t.Fatalf("expected non-nil error for empty chain; got res=%+v", res)
	}
	if res != nil {
		t.Errorf("expected nil result on error; got %+v", res)
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
