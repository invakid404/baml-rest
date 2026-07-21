package workerboot

import "testing"

// TestNativeServingLabel pins the native_serving eligibility contract: "eligible"
// requires BOTH a serve factory AND the umbrella flag enabled. The load-bearing
// case is flag-OFF + factory-present, which must report "off" (the generated seam
// gates every native callback on the flag, so nothing serves natively while off).
func TestNativeServingLabel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		factory, flag bool
		want          string
	}{
		{"serve profile, flag on", true, true, "eligible"},
		{"flag off + factory present", true, false, "off"},
		{"flag on, no factory", false, true, "off"},
		{"no factory, flag off", false, false, "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeServingLabel(tc.factory, tc.flag); got != tc.want {
				t.Errorf("nativeServingLabel(factory=%v, flag=%v) = %q, want %q", tc.factory, tc.flag, got, tc.want)
			}
		})
	}
}

// TestNativeLaneStatus pins that the rollout_mode / native_serving diagnostics are
// derived from BOTH the dynamic AND the static lanes. The load-bearing case is a
// STATIC-ONLY serve cohort (only NativeStaticServeFactory wired): it serves an
// admitted static /call natively and MUST report rollout_mode=serve /
// native_serving=eligible (flag on) — not BAML-only, the bug this fixes.
func TestNativeLaneStatus(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		dynServe, statServe, dynShadow, statShadow, flag bool
		wantMode, wantServing                            string
	}{
		{"static-only serve, flag on", false, true, false, false, true, "serve", "eligible"},
		{"static-only serve, flag off", false, true, false, false, false, "serve", "off"},
		{"dynamic-only serve, flag on", true, false, false, false, true, "serve", "eligible"},
		{"both serve lanes, flag on", true, true, false, false, true, "serve", "eligible"},
		{"static-only shadow, flag on", false, false, false, true, true, "shadow", "off"},
		{"dynamic-only shadow, flag on", false, false, true, false, true, "shadow", "off"},
		{"serve precedence over shadow", false, true, false, true, true, "serve", "eligible"},
		{"no lanes, flag on", false, false, false, false, true, "off", "off"},
		{"no lanes, flag off", false, false, false, false, false, "off", "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, serving := nativeLaneStatus(tc.dynServe, tc.statServe, tc.dynShadow, tc.statShadow, tc.flag)
			if mode != tc.wantMode || serving != tc.wantServing {
				t.Errorf("nativeLaneStatus(dynServe=%v statServe=%v dynShadow=%v statShadow=%v flag=%v) = (%q, %q), want (%q, %q)",
					tc.dynServe, tc.statServe, tc.dynShadow, tc.statShadow, tc.flag, mode, serving, tc.wantMode, tc.wantServing)
			}
		})
	}
}
