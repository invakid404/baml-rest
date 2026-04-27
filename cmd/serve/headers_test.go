package main

import (
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestSetBAMLHeaders_OmitsWhenNil(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	setBAMLHeaders(netHTTPHeaderSetter(h), nil, nil)

	for _, name := range []string{
		HeaderBAMLPath, HeaderBAMLPathReason, HeaderBAMLBuildRequestAPI, HeaderBAMLClient,
		HeaderBAMLWinnerClient, HeaderBAMLWinnerProvider,
		HeaderBAMLRetryMax, HeaderBAMLRetryCount, HeaderBAMLUpstreamDuration,
		HeaderBAMLBamlCallCount,
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("expected %s to be absent when both metadata args are nil; got %q", name, got)
		}
	}
}

func TestSetBAMLHeaders_PlannedOnly(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	max := 3
	planned := &bamlutils.Metadata{
		Phase:    bamlutils.MetadataPhasePlanned,
		Path:     "buildrequest",
		Client:   "MyClient",
		RetryMax: &max,
	}
	setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)

	if got := h.Get(HeaderBAMLPath); got != "buildrequest" {
		t.Errorf("Path: got %q, want %q", got, "buildrequest")
	}
	if got := h.Get(HeaderBAMLClient); got != "MyClient" {
		t.Errorf("Client: got %q, want %q", got, "MyClient")
	}
	if got := h.Get(HeaderBAMLRetryMax); got != "3" {
		t.Errorf("RetryMax: got %q, want %q", got, "3")
	}
	for _, outcomeName := range []string{HeaderBAMLWinnerClient, HeaderBAMLWinnerProvider, HeaderBAMLRetryCount, HeaderBAMLUpstreamDuration, HeaderBAMLBamlCallCount} {
		if got := h.Get(outcomeName); got != "" {
			t.Errorf("%s should be absent when outcome is nil; got %q", outcomeName, got)
		}
	}
}

func TestSetBAMLHeaders_BamlCallCount(t *testing.T) {
	t.Parallel()

	planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "legacy", Client: "MyClient"}

	t.Run("nil omitted", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		outcome := &bamlutils.Metadata{Phase: bamlutils.MetadataPhaseOutcome, WinnerPath: "legacy"}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, outcome)
		if got := h.Get(HeaderBAMLBamlCallCount); got != "" {
			t.Errorf("nil BamlCallCount should be omitted; got %q", got)
		}
	})

	t.Run("zero present", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		zero := 0
		outcome := &bamlutils.Metadata{Phase: bamlutils.MetadataPhaseOutcome, WinnerPath: "legacy", BamlCallCount: &zero}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, outcome)
		if got := h.Get(HeaderBAMLBamlCallCount); got != "0" {
			t.Errorf("BamlCallCount=&0 should emit \"0\" (distinct from absent); got %q", got)
		}
	})

	t.Run("nonzero present", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		three := 3
		outcome := &bamlutils.Metadata{Phase: bamlutils.MetadataPhaseOutcome, WinnerPath: "legacy", BamlCallCount: &three}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, outcome)
		if got := h.Get(HeaderBAMLBamlCallCount); got != "3" {
			t.Errorf("BamlCallCount: got %q, want 3", got)
		}
	})
}

func TestSetBAMLHeaders_OutcomeWinnerSameAsClient(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	zero := 0
	dur := int64(42)
	planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "MyClient"}
	outcome := &bamlutils.Metadata{
		Phase:          bamlutils.MetadataPhaseOutcome,
		WinnerClient:   "MyClient", // same as planned.Client → omitted
		WinnerProvider: "openai",
		RetryCount:     &zero,
		UpstreamDurMs:  &dur,
	}
	setBAMLHeaders(netHTTPHeaderSetter(h), planned, outcome)

	if got := h.Get(HeaderBAMLWinnerClient); got != "" {
		t.Errorf("WinnerClient should be omitted when same as planned.Client; got %q", got)
	}
	if got := h.Get(HeaderBAMLWinnerProvider); got != "openai" {
		t.Errorf("WinnerProvider: got %q, want %q", got, "openai")
	}
	if got := h.Get(HeaderBAMLRetryCount); got != "0" {
		t.Errorf("RetryCount=0 should be present (zero != absent); got %q", got)
	}
	if got := h.Get(HeaderBAMLUpstreamDuration); got != "42" {
		t.Errorf("UpstreamDuration: got %q, want %q", got, "42")
	}
}

func TestSetBAMLHeaders_OutcomeWinnerDiffers(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "Strategy"}
	outcome := &bamlutils.Metadata{Phase: bamlutils.MetadataPhaseOutcome, WinnerClient: "Backup"}
	setBAMLHeaders(netHTTPHeaderSetter(h), planned, outcome)

	if got := h.Get(HeaderBAMLWinnerClient); got != "Backup" {
		t.Errorf("WinnerClient: got %q, want %q", got, "Backup")
	}
}

func TestSetBAMLHeaders_BuildRequestAPI(t *testing.T) {
	t.Parallel()

	t.Run("absent when empty", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "MyClient"}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		if got := h.Get(HeaderBAMLBuildRequestAPI); got != "" {
			t.Errorf("header should be absent when BuildRequestAPI is empty; got %q", got)
		}
	})

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{
			Phase:           bamlutils.MetadataPhasePlanned,
			Path:            "buildrequest",
			BuildRequestAPI: "request",
			Client:          "MyClient",
		}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		if got := h.Get(HeaderBAMLBuildRequestAPI); got != "request" {
			t.Errorf("header: got %q, want request", got)
		}
	})

	t.Run("streamrequest", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{
			Phase:           bamlutils.MetadataPhasePlanned,
			Path:            "buildrequest",
			BuildRequestAPI: "streamrequest",
			Client:          "MyClient",
		}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		if got := h.Get(HeaderBAMLBuildRequestAPI); got != "streamrequest" {
			t.Errorf("header: got %q, want streamrequest", got)
		}
	})
}

func TestSetBAMLHeaders_DropsControlChars(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "Bad\nClient"}
	setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)

	if got := h.Get(HeaderBAMLClient); got != "" {
		t.Errorf("Client header should be dropped on control chars; got %q", got)
	}
}

func TestSetBAMLHeaders_RoundRobin(t *testing.T) {
	t.Parallel()

	t.Run("emits rr headers when present", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{
			Phase:  bamlutils.MetadataPhasePlanned,
			Path:   "buildrequest",
			Client: "ChildA",
			RoundRobin: &bamlutils.RoundRobinInfo{
				Name:     "MyRR",
				Children: []string{"ChildA", "ChildB"},
				Index:    0,
				Selected: "ChildA",
			},
		}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		if got := h.Get(HeaderBAMLRoundRobinName); got != "MyRR" {
			t.Errorf("RoundRobinName: got %q, want MyRR", got)
		}
		if got := h.Get(HeaderBAMLRoundRobinSelected); got != "ChildA" {
			t.Errorf("RoundRobinSelected: got %q, want ChildA", got)
		}
		if got := h.Get(HeaderBAMLRoundRobinIndex); got != "0" {
			t.Errorf("RoundRobinIndex: got %q, want 0", got)
		}
	})

	t.Run("omits rr headers when absent", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "MyClient"}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		for _, name := range []string{HeaderBAMLRoundRobinName, HeaderBAMLRoundRobinSelected, HeaderBAMLRoundRobinIndex} {
			if got := h.Get(name); got != "" {
				t.Errorf("%s should be absent when RoundRobin is nil; got %q", name, got)
			}
		}
	})

	t.Run("drops control chars in rr name", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		planned := &bamlutils.Metadata{
			Phase:  bamlutils.MetadataPhasePlanned,
			Path:   "buildrequest",
			Client: "Child",
			RoundRobin: &bamlutils.RoundRobinInfo{
				Name:     "Bad\nRR",
				Selected: "Good",
				Index:    1,
			},
		}
		setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)
		if got := h.Get(HeaderBAMLRoundRobinName); got != "" {
			t.Errorf("RoundRobinName should be dropped on control chars; got %q", got)
		}
		// Selected is sanitised independently of Name — a clean
		// Selected must survive even when the sibling Name is
		// dropped. CodeRabbit verdict-25 finding F5: the previous
		// shape of this subtest only asserted the Name drop and
		// Index emission, which would have passed if a regression
		// dropped Selected too (the clean-name subtest above wouldn't
		// catch that since it never dirties Name).
		if got := h.Get(HeaderBAMLRoundRobinSelected); got != "Good" {
			t.Errorf("RoundRobinSelected should still emit when Name is dropped; got %q", got)
		}
		// Index is a number — always safe to emit.
		if got := h.Get(HeaderBAMLRoundRobinIndex); got != "1" {
			t.Errorf("RoundRobinIndex should still emit when Name is dropped; got %q", got)
		}
	})
}

func TestDecodeMetadataJSON_EmptyAndInvalid(t *testing.T) {
	t.Parallel()

	if md := decodeMetadataJSON(nil); md != nil {
		t.Errorf("nil bytes should decode to nil; got %+v", md)
	}
	if md := decodeMetadataJSON([]byte{}); md != nil {
		t.Errorf("empty bytes should decode to nil; got %+v", md)
	}
	if md := decodeMetadataJSON([]byte("{not json")); md != nil {
		t.Errorf("invalid JSON should decode to nil; got %+v", md)
	}
}
