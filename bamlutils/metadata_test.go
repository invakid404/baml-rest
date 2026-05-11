package bamlutils

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestMetadata_JSONRoundtrip verifies that all Metadata fields survive a
// JSON encode → decode cycle, with pointer fields preserving the
// "absent" / "zero" distinction that the absence-as-unknown contract relies on.
func TestMetadata_JSONRoundtrip(t *testing.T) {
	t.Parallel()

	zero := 0
	five := 5
	dur := int64(1234)

	cases := []struct {
		name string
		md   Metadata
	}{
		{
			name: "minimal planned",
			md: Metadata{
				Phase: MetadataPhasePlanned,
				Path:  "buildrequest",
			},
		},
		{
			name: "single-provider planned with retry",
			md: Metadata{
				Phase:       MetadataPhasePlanned,
				Path:        "buildrequest",
				Client:      "MyClient",
				Provider:    "openai",
				RetryMax:    &five,
				RetryPolicy: "exp:200ms:1.50:10000ms",
			},
		},
		{
			name: "fallback chain planned",
			md: Metadata{
				Phase:          MetadataPhasePlanned,
				Path:           "buildrequest",
				Client:         "Strategy",
				Strategy:       "baml-fallback",
				Chain:          []string{"Primary", "Backup"},
				LegacyChildren: []string{"Backup"},
			},
		},
		{
			name: "outcome with all fields",
			md: Metadata{
				Phase:          MetadataPhaseOutcome,
				Attempt:        2,
				Path:           "buildrequest",
				WinnerClient:   "Backup",
				WinnerProvider: "anthropic",
				WinnerPath:     "buildrequest",
				RetryCount:     &zero,
				UpstreamDurMs:  &dur,
			},
		},
		{
			name: "legacy outcome with baml call count",
			md: Metadata{
				Phase:          MetadataPhaseOutcome,
				Path:           "legacy",
				WinnerClient:   "MyClient",
				WinnerProvider: "openai",
				WinnerPath:     "legacy",
				UpstreamDurMs:  &dur,
				BamlCallCount:  &five,
			},
		},
		{
			name: "legacy planned with reason",
			md: Metadata{
				Phase:      MetadataPhasePlanned,
				Path:       "legacy",
				PathReason: "unsupported-provider",
				Client:     "MyClient",
				Provider:   "aws-bedrock",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(&tc.md)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Metadata
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Phase != tc.md.Phase {
				t.Errorf("Phase: got %q, want %q", got.Phase, tc.md.Phase)
			}
			if got.Attempt != tc.md.Attempt {
				t.Errorf("Attempt: got %d, want %d", got.Attempt, tc.md.Attempt)
			}
			if got.Path != tc.md.Path {
				t.Errorf("Path: got %q, want %q", got.Path, tc.md.Path)
			}
			if got.PathReason != tc.md.PathReason {
				t.Errorf("PathReason: got %q, want %q", got.PathReason, tc.md.PathReason)
			}
			if got.Client != tc.md.Client {
				t.Errorf("Client: got %q, want %q", got.Client, tc.md.Client)
			}
			if got.Provider != tc.md.Provider {
				t.Errorf("Provider: got %q, want %q", got.Provider, tc.md.Provider)
			}
			if got.WinnerClient != tc.md.WinnerClient {
				t.Errorf("WinnerClient: got %q, want %q", got.WinnerClient, tc.md.WinnerClient)
			}
			if got.WinnerProvider != tc.md.WinnerProvider {
				t.Errorf("WinnerProvider: got %q, want %q", got.WinnerProvider, tc.md.WinnerProvider)
			}
			if got.WinnerPath != tc.md.WinnerPath {
				t.Errorf("WinnerPath: got %q, want %q", got.WinnerPath, tc.md.WinnerPath)
			}
			// Pointer fields: nil-ness and value both matter.
			equalPtrInt := func(a, b *int) bool {
				if a == nil || b == nil {
					return a == b
				}
				return *a == *b
			}
			equalPtrInt64 := func(a, b *int64) bool {
				if a == nil || b == nil {
					return a == b
				}
				return *a == *b
			}
			if !equalPtrInt(got.RetryMax, tc.md.RetryMax) {
				t.Errorf("RetryMax: got %v, want %v", got.RetryMax, tc.md.RetryMax)
			}
			if !equalPtrInt(got.RetryCount, tc.md.RetryCount) {
				t.Errorf("RetryCount: got %v, want %v", got.RetryCount, tc.md.RetryCount)
			}
			if !equalPtrInt64(got.UpstreamDurMs, tc.md.UpstreamDurMs) {
				t.Errorf("UpstreamDurMs: got %v, want %v", got.UpstreamDurMs, tc.md.UpstreamDurMs)
			}
			if !equalPtrInt(got.BamlCallCount, tc.md.BamlCallCount) {
				t.Errorf("BamlCallCount: got %v, want %v", got.BamlCallCount, tc.md.BamlCallCount)
			}
		})
	}
}

// TestMetadata_FallbackTargetsAndRoundRobinRoundtrip verifies that the
// fallback-target vocabulary (FallbackTargets, FallbackRoundRobin)
// survives a JSON encode → decode cycle, AND that both fields honour
// their `omitempty` JSON tags for the nil and empty-map cases.
//
// The nil / empty-map invariant is load-bearing: only the narrow set of
// plans that come out of a centralised RR-child resolution populates
// these fields, so the wire shape (no `fallback_targets` /
// `fallback_round_robin` keys at all) must hold for every other plan
// the builders emit. A regression that emits empty-but-non-nil maps
// would change the wire shape for every existing consumer.
func TestMetadata_FallbackTargetsAndRoundRobinRoundtrip(t *testing.T) {
	t.Parallel()

	t.Run("populated", func(t *testing.T) {
		t.Parallel()
		md := Metadata{
			Phase:    MetadataPhasePlanned,
			Path:     "buildrequest",
			Client:   "Strategy",
			Strategy: "baml-fallback",
			Chain:    []string{"InnerRR", "Backup"},
			FallbackTargets: map[string]string{
				"InnerRR": "A",
			},
			FallbackRoundRobin: map[string]*RoundRobinInfo{
				"InnerRR": {
					Name:     "InnerRR",
					Children: []string{"A", "B"},
					Index:    0,
					Selected: "A",
				},
			},
		}
		data, err := json.Marshal(&md)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Contains(data, []byte(`"fallback_targets"`)) {
			t.Errorf("populated FallbackTargets should appear on the wire; got %s", data)
		}
		if !bytes.Contains(data, []byte(`"fallback_round_robin"`)) {
			t.Errorf("populated FallbackRoundRobin should appear on the wire; got %s", data)
		}
		var got Metadata
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.FallbackTargets["InnerRR"] != "A" {
			t.Errorf("FallbackTargets[InnerRR]: got %q, want A", got.FallbackTargets["InnerRR"])
		}
		rr, ok := got.FallbackRoundRobin["InnerRR"]
		if !ok || rr == nil {
			t.Fatalf("FallbackRoundRobin[InnerRR] missing; got map=%v", got.FallbackRoundRobin)
		}
		if rr.Name != "InnerRR" || rr.Selected != "A" || rr.Index != 0 || len(rr.Children) != 2 {
			t.Errorf("FallbackRoundRobin[InnerRR]: got %+v", rr)
		}
	})

	t.Run("nil fields are omitted", func(t *testing.T) {
		t.Parallel()
		md := Metadata{Phase: MetadataPhasePlanned, Path: "buildrequest"}
		data, err := json.Marshal(&md)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(data, []byte(`"fallback_targets"`)) {
			t.Errorf("nil FallbackTargets must be omitted from wire; got %s", data)
		}
		if bytes.Contains(data, []byte(`"fallback_round_robin"`)) {
			t.Errorf("nil FallbackRoundRobin must be omitted from wire; got %s", data)
		}
	})

	t.Run("empty maps are omitted", func(t *testing.T) {
		t.Parallel()
		md := Metadata{
			Phase:              MetadataPhasePlanned,
			Path:               "buildrequest",
			FallbackTargets:    map[string]string{},
			FallbackRoundRobin: map[string]*RoundRobinInfo{},
		}
		data, err := json.Marshal(&md)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// Maps with `omitempty` drop on len()==0, so the wire shape
		// for an explicit empty map is identical to nil. Only the
		// narrow set of plans coming out of a centralised RR-child
		// resolution populates these maps; every other plan keeps
		// the existing wire payload byte-for-byte.
		if bytes.Contains(data, []byte(`"fallback_targets"`)) {
			t.Errorf("empty FallbackTargets must be omitted from wire; got %s", data)
		}
		if bytes.Contains(data, []byte(`"fallback_round_robin"`)) {
			t.Errorf("empty FallbackRoundRobin must be omitted from wire; got %s", data)
		}
	})
}

// TestMetadata_AbsenceVsZero verifies the distinction between an absent
// pointer field and a pointer to zero — the legacy v1 outcome semantics
// rely on this so "RetryCount=0 (first attempt succeeded)" can be told
// apart from "RetryCount unknown (legacy path)".
func TestMetadata_AbsenceVsZero(t *testing.T) {
	t.Parallel()

	zero := 0
	withZero := Metadata{Phase: MetadataPhaseOutcome, RetryCount: &zero}
	withoutCount := Metadata{Phase: MetadataPhaseOutcome}

	dataWith, _ := json.Marshal(withZero)
	dataWithout, _ := json.Marshal(withoutCount)

	if string(dataWith) == string(dataWithout) {
		t.Fatalf("zero-pointer should be distinguishable from nil pointer; both encoded as %q", dataWith)
	}
	// Decode and verify nil-ness round-trips.
	var dec Metadata
	if err := json.Unmarshal(dataWithout, &dec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dec.RetryCount != nil {
		t.Fatalf("RetryCount should be nil when omitted; got %v", *dec.RetryCount)
	}
}
