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
		HeaderBAMLPath, HeaderBAMLPathReason, HeaderBAMLClient,
		HeaderBAMLWinnerClient, HeaderBAMLWinnerProvider,
		HeaderBAMLRetryMax, HeaderBAMLRetryCount, HeaderBAMLUpstreamDuration,
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
	for _, outcomeName := range []string{HeaderBAMLWinnerClient, HeaderBAMLWinnerProvider, HeaderBAMLRetryCount, HeaderBAMLUpstreamDuration} {
		if got := h.Get(outcomeName); got != "" {
			t.Errorf("%s should be absent when outcome is nil; got %q", outcomeName, got)
		}
	}
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

func TestSetBAMLHeaders_DropsControlChars(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	planned := &bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Path: "buildrequest", Client: "Bad\nClient"}
	setBAMLHeaders(netHTTPHeaderSetter(h), planned, nil)

	if got := h.Get(HeaderBAMLClient); got != "" {
		t.Errorf("Client header should be dropped on control chars; got %q", got)
	}
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
