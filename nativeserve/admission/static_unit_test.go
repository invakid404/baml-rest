package admission

// De-BAML Slice 8B — non-gated unit coverage for the STATIC admission gates that
// return BEFORE any nanollm FFI (route-kind, capability/flag, strategy/client, and
// the descriptor-envelope checks) plus the pure classification helpers. It links
// nanollm (the module does) but never invokes it — every path here returns before
// nanollm.New — so it runs under an ordinary `go test`.

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

func TestRouteKindString(t *testing.T) {
	if got := RouteKindDynamic.String(); got != "dynamic" {
		t.Errorf("RouteKindDynamic = %q, want dynamic", got)
	}
	if got := RouteKindStatic.String(); got != "static" {
		t.Errorf("RouteKindStatic = %q, want static", got)
	}
	if got := RouteKind(99).String(); got != "unknown" {
		t.Errorf("RouteKind(99) = %q, want unknown", got)
	}
}

func TestCheckArgBinder(t *testing.T) {
	declared := []promptdescriptor.Argument{{Name: "topic"}, {Name: "count"}}
	cases := []struct {
		name     string
		order    []string
		values   map[string]any
		wantOK   bool
		wantsSub Reason
	}{
		{"exact match", []string{"topic", "count"}, map[string]any{"topic": "x", "count": 1}, true, ""},
		{"arity mismatch", []string{"topic"}, map[string]any{"topic": "x", "count": 1}, false, reasonArgBinderArity},
		{"value count mismatch", []string{"topic", "count"}, map[string]any{"topic": "x"}, false, reasonArgBinderCount},
		{"order mismatch", []string{"count", "topic"}, map[string]any{"topic": "x", "count": 1}, false, reasonArgBinderOrder},
		{"missing value", []string{"topic", "count"}, map[string]any{"topic": "x", "other": 1}, false, reasonArgBinderMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkArgBinder(declared, c.order, c.values)
			if c.wantOK {
				if got != "" {
					t.Errorf("want match, got reason %q", got)
				}
				return
			}
			if got != c.wantsSub {
				t.Errorf("got reason %q, want %q", got, c.wantsSub)
			}
		})
	}
}

func TestStaticTransport(t *testing.T) {
	lit := func(s string) promptdescriptor.OptionValue {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: s}
	}
	env := promptdescriptor.OptionValue{Kind: promptdescriptor.OptionEnv, String: "OPENAI_KEY"}

	t.Run("literal base_url + api_key", func(t *testing.T) {
		cfg := promptdescriptor.ClientConfig{Present: true, TransportOptions: []promptdescriptor.ClientOption{
			{Key: "base_url", Value: lit("https://x.invalid/v1")},
			{Key: "api_key", Value: lit("sk-fake")},
		}}
		base, key, reason := staticTransport(cfg)
		if reason != "" || base != "https://x.invalid/v1" || key != "sk-fake" {
			t.Errorf("got base=%q key=%q reason=%q", base, key, reason)
		}
	})
	t.Run("absent client block", func(t *testing.T) {
		if _, _, r := staticTransport(promptdescriptor.ClientConfig{Present: false}); r != reasonClientBlockAbsent {
			t.Errorf("got %q, want %q", r, reasonClientBlockAbsent)
		}
	})
	t.Run("env api_key declines (not a build-time literal)", func(t *testing.T) {
		cfg := promptdescriptor.ClientConfig{Present: true, TransportOptions: []promptdescriptor.ClientOption{
			{Key: "base_url", Value: lit("https://x.invalid/v1")},
			{Key: "api_key", Value: env},
		}}
		if _, _, r := staticTransport(cfg); r != reasonAPIKeyNotLiteral {
			t.Errorf("got %q, want %q", r, reasonAPIKeyNotLiteral)
		}
	})
	t.Run("absent base_url declines", func(t *testing.T) {
		cfg := promptdescriptor.ClientConfig{Present: true, TransportOptions: []promptdescriptor.ClientOption{
			{Key: "api_key", Value: lit("sk-fake")},
		}}
		if _, _, r := staticTransport(cfg); r != reasonBaseURLAbsent {
			t.Errorf("got %q, want %q", r, reasonBaseURLAbsent)
		}
	})
}

// TestAdmitStaticEarlyDeclines pins the pre-FFI decline gates: none of these reach
// nanollm.New, so the observation is always a bounded decline with the right family.
func TestAdmitStaticEarlyDeclines(t *testing.T) {
	ctx := context.Background()
	staticOK := StaticInput{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		RouteKind: RouteKindStatic, Mode: bamlutils.NativeStaticModeFinal, SingleLeaf: true, Method: "M",
	}
	cases := []struct {
		name       string
		mutate     func(StaticInput) StaticInput
		wantFamily bamlutils.NativeStaticObserveFamily
	}{
		{"route kind not static", func(in StaticInput) StaticInput { in.RouteKind = RouteKindDynamic; return in }, bamlutils.NativeStaticFamilyCapability},
		{"worker not capable", func(in StaticInput) StaticInput { in.WorkerCapable = false; return in }, bamlutils.NativeStaticFamilyCapability},
		{"flag disabled", func(in StaticInput) StaticInput { in.FlagEnabled = false; return in }, bamlutils.NativeStaticFamilyCapability},
		// P1b: streaming and call-with-raw FAIL CLOSED at the mode gate (family=client),
		// before any render/Prepare.
		{"stream mode", func(in StaticInput) StaticInput { in.Mode = bamlutils.NativeStaticModeStream; return in }, bamlutils.NativeStaticFamilyClient},
		{"call-with-raw", func(in StaticInput) StaticInput { in.Raw = true; return in }, bamlutils.NativeStaticFamilyClient},
		{"not single leaf", func(in StaticInput) StaticInput { in.SingleLeaf = false; return in }, bamlutils.NativeStaticFamilyClient},
		{"fallback chain", func(in StaticInput) StaticInput { in.HasFallbackChain = true; return in }, bamlutils.NativeStaticFamilyClient},
		{"client override", func(in StaticInput) StaticInput { in.ClientOverride = "other"; return in }, bamlutils.NativeStaticFamilyClient},
		{"descriptor absent", func(in StaticInput) StaticInput { return in }, bamlutils.NativeStaticFamilyDescriptorEnvelope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := AdmitStatic(ctx, c.mutate(staticOK))
			if obs.Observation != bamlutils.NativeStaticObserveDecline {
				t.Fatalf("observation=%q, want decline", obs.Observation)
			}
			if obs.Family != c.wantFamily {
				t.Errorf("family=%q, want %q (stage=%q reason=%q)", obs.Family, c.wantFamily, obs.Stage, obs.Reason)
			}
		})
	}

	// Cancelled context declines at the capability stage before any work.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if obs := AdmitStatic(cancelled, staticOK); obs.Observation != bamlutils.NativeStaticObserveDecline {
		t.Errorf("cancelled ctx: observation=%q, want decline", obs.Observation)
	}
}

// TestAdmitStaticParseEarlyDeclines pins the parse-only predicate's pre-FFI gates:
// a non-static route kind declines (capability) and an absent descriptor declines
// (descriptor_envelope), without reaching any bundle lowering that would need a
// real descriptor.
func TestAdmitStaticParseEarlyDeclines(t *testing.T) {
	ctx := context.Background()
	base := StaticInput{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		RouteKind: RouteKindStatic, Method: "M", Mode: bamlutils.NativeStaticModeParseOnly,
	}

	dyn := base
	dyn.RouteKind = RouteKindDynamic
	if obs := AdmitStaticParse(ctx, dyn); obs.Observation != bamlutils.NativeStaticObserveDecline ||
		obs.Family != bamlutils.NativeStaticFamilyCapability {
		t.Errorf("parse non-static route: observation=%q family=%q, want capability decline", obs.Observation, obs.Family)
	}

	// Absent descriptor (zero Method) declines at the envelope stage.
	if obs := AdmitStaticParse(ctx, base); obs.Observation != bamlutils.NativeStaticObserveDecline ||
		obs.Family != bamlutils.NativeStaticFamilyDescriptorEnvelope {
		t.Errorf("parse absent descriptor: observation=%q family=%q, want descriptor_envelope decline", obs.Observation, obs.Family)
	}
}
