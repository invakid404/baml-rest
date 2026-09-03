//go:build !debamlnativespinegenerated

package standardspineoracle

// This test asserts the STUB (fail-loud) behavior of the deployment-generated registry, so it
// is meaningful ONLY in the untagged build: under the debamlnativespinegenerated tag the
// generated NewExecutor succeeds, NewStaticServe does not error, and the assertion below would
// fail. Gating the whole file (not just the test) also drops the stub-only
// nativegenerated.ErrRuntimeNotGenerated reference from the tagged build, where the generated
// variant does not export it.

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated"
)

// TestNewStaticServeFailsLoudWithoutGeneratedRegistry proves the fail-loud guard: in a
// source checkout (no debamlnativespinegenerated tag) nativegenerated is the stub, so
// NewStaticServe surfaces the generation error rather than silently degrading to all-BAML.
func TestNewStaticServeFailsLoudWithoutGeneratedRegistry(t *testing.T) {
	_, err := NewStaticServe(prometheus.NewRegistry())
	if err == nil {
		t.Fatal("NewStaticServe succeeded without a generated registry; it must fail loud so the standard build never silently degrades to all-BAML")
	}
	if !errors.Is(err, nativegenerated.ErrRuntimeNotGenerated) {
		t.Errorf("err = %v, want it to wrap nativegenerated.ErrRuntimeNotGenerated", err)
	}
}
