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
