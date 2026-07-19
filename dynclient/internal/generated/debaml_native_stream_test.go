package generated

import (
	"context"
	"testing"

	bamlutils "github.com/invakid404/baml-rest/bamlutils"
	buildrequest "github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/adapter"
)

// De-BAML Phase 7D public-mode gate regression tests (P1-a). The dynamic
// StreamRequest builder ALSO serves the UNARY /call{,-with-raw} compatibility
// bridge (StreamModeCall / StreamModeCallWithRaw route through it when BAML's
// non-streaming Request API is unavailable). maybeInstallNativeStream MUST install
// the native stream seam ONLY for the real public stream modes so a bridged unary
// call can never admit/claim a native stream and stays byte-identical BAML.

// newNativeStreamAdapter builds a real framework adapter with the flag on, a schema,
// a parser, and a native stream serve spy installed — everything the seam needs,
// so only the PUBLIC MODE gate decides whether the callback is installed.
func newNativeStreamAdapter(t *testing.T, mode bamlutils.StreamMode, spyCalls *int) *adapter.BamlAdapter {
	t.Helper()
	a := &adapter.BamlAdapter{}
	a.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: true})
	a.SetStreamMode(mode)
	a.SetDeBAMLOutputSchema(simpleSchema())
	a.SetDeBAMLParser(func(context.Context, bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
		return bamlutils.DeBAMLParseResult{JSON: []byte(`{}`)}, nil
	})
	a.SetNativeStreamServeComparator(func(context.Context, bamlutils.NativeStreamServeRequest) bamlutils.NativeStreamServeResult {
		*spyCalls++
		return bamlutils.NativeStreamServeResult{Disposition: bamlutils.NativeStreamDeclined}
	})
	return a
}

// TestMaybeInstallNativeStream_UnaryBridgeModesNotInstalled proves the P1-a fix: a
// unary /call or /call-with-raw compat-bridge request (StreamModeCall /
// StreamModeCallWithRaw) — even with the flag on and a native stream serve
// comparator installed — leaves StreamConfig.NativeAttempt nil, so the orchestrator
// never invokes the native lane and BAML serves the request. The serve spy stays
// at zero.
func TestMaybeInstallNativeStream_UnaryBridgeModesNotInstalled(t *testing.T) {
	for _, mode := range []bamlutils.StreamMode{bamlutils.StreamModeCall, bamlutils.StreamModeCallWithRaw} {
		spyCalls := 0
		a := newNativeStreamAdapter(t, mode, &spyCalls)
		cfg := &buildrequest.StreamConfig{NeedsRaw: mode.NeedsRaw()}
		maybeInstallNativeStream(a, cfg, nil, true, false, false, nil)

		if cfg.NativeAttemptEnabled {
			t.Errorf("mode %v: NativeAttemptEnabled must stay false for the unary compat-bridge", mode)
		}
		if cfg.NativeAttempt != nil {
			t.Errorf("mode %v: native stream callback must NOT be installed for a unary /call bridge (would let a public unary call claim a physical native stream)", mode)
		}
		if cfg.NativeParseStream != nil || cfg.NativeParseFinal != nil {
			t.Errorf("mode %v: native-only parser closures must NOT be installed for a unary bridge", mode)
		}
		if cfg.PlannedEngine != "" {
			t.Errorf("mode %v: planned_engine must stay empty for a unary bridge, got %q", mode, cfg.PlannedEngine)
		}
		if spyCalls != 0 {
			t.Errorf("mode %v: native stream serve spy called %d times, want 0 (never even wired)", mode, spyCalls)
		}
	}
}

// TestMaybeInstallNativeStream_StreamModesInstalled is the positive control: the two
// REAL public stream modes DO install the native seam, and NativeMode is derived
// from the actual public mode (not inferred from NeedsRaw).
func TestMaybeInstallNativeStream_StreamModesInstalled(t *testing.T) {
	cases := []struct {
		mode       bamlutils.StreamMode
		wantNative bamlutils.NativeStreamMode
	}{
		{bamlutils.StreamModeStream, bamlutils.NativeStreamModeStream},
		{bamlutils.StreamModeStreamWithRaw, bamlutils.NativeStreamModeStreamWithRaw},
	}
	for _, tc := range cases {
		spyCalls := 0
		a := newNativeStreamAdapter(t, tc.mode, &spyCalls)
		cfg := &buildrequest.StreamConfig{NeedsRaw: tc.mode.NeedsRaw()}
		maybeInstallNativeStream(a, cfg, nil, true, false, false, nil)

		if !cfg.NativeAttemptEnabled || cfg.NativeAttempt == nil {
			t.Fatalf("mode %v: native stream callback must be installed for a real stream request", tc.mode)
		}
		if cfg.NativeMode != tc.wantNative {
			t.Errorf("mode %v: NativeMode = %q, want %q (derived from the actual public mode)", tc.mode, cfg.NativeMode, tc.wantNative)
		}
		if cfg.NativeParseStream == nil || cfg.NativeParseFinal == nil {
			t.Errorf("mode %v: native-only parser closures must be installed", tc.mode)
		}
		if cfg.PlannedEngine != "native" {
			t.Errorf("mode %v: planned_engine = %q, want native", tc.mode, cfg.PlannedEngine)
		}
	}
}
