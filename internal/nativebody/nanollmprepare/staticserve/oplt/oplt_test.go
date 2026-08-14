//go:build integration && nanollm_integration

// Package oplt is the LIVE socket proof for the `<` half of the de-BAML Slice
// 7.2c-3 admission cutover.
//
// # Why this is its own package
//
// The cutover admits six direct comparisons on the two PRODUCTION-PINNED class names,
// and `baml_go`'s type map is PROCESS-GLOBAL and keyed by class NAME. Two generated
// clients that both declare `StaticCheckedAnswer` would overwrite each other's entry,
// and the flag-OFF BAML leg — the differential's stock half — decodes THROUGH that map.
// A Go test binary is per package, so one package per operator is what keeps each
// fixture's type registration its own. See ../opharness for the full account.
//
// This package therefore links EXACTLY ONE generated client: the isolated `lt`
// project, which declares the two pinned names once with `{{ this < 0 }}` and bakes
// its own loopback port (17656) so it can run beside the others.
//
// The main staticserve package proves the same four rows for `>`; it is unchanged.
package oplt

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"

	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/staticserve/opharness"
	fixturebaml "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/lt/baml_client"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/lt/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/lt/introspected"
)

// loopbackAddr is the FIXED loopback this project's StaticOracleClient base_url bakes.
// A fixed port is required because the base_url is a baked literal; it is distinct from
// every other operator project's so the five packages can run concurrently.
const loopbackAddr = "127.0.0.1:17656"

// opID is this project's operator, as the capability table and the stock captures key
// it.
const opID = "lt"

// drain runs one generated /call to completion and returns the marshalled final, the
// outcome tokens and any error.
//
// The final is marshalled INSIDE the drain loop, before the result is released back to
// its pool, so the bytes compared later cannot be a view of a recycled struct — and it
// uses sonic, the WORKER's serializer (worker/parse.go), so they are the bytes a caller
// receives and the ones internal/debaml/predicatewire's stock captures are in.
// encoding/json would HTML-escape the `<`, `>` and `=` inside the carrier's
// `expression`, silently comparing a different string against the capture.
func drain(t *testing.T, ch <-chan bamlutils.StreamResult, err error) opharness.Outcome {
	t.Helper()
	if err != nil {
		return opharness.Outcome{Err: err}
	}
	out := opharness.Outcome{}
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			if f := r.Final(); f != nil {
				b, merr := sonic.Marshal(f)
				if merr != nil {
					t.Fatalf("marshal final: %v", merr)
				}
				out.FinalJSON = string(b)
			}
		case bamlutils.StreamResultKindError:
			out.Err = r.Error()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				out.Winner = md.WinnerEngine
				out.Planned = md.PlannedEngine
			}
		}
		r.Release()
	}
	return out
}

// project wires this fixture into the shared runner.
func project() opharness.Project {
	return opharness.Project{
		OpID:        opID,
		Addr:        loopbackAddr,
		InitRuntime: fixturebaml.InitRuntime,
		MakeAdapter: func(ctx context.Context) bamlutils.Adapter { return fixture.MakeAdapter(ctx) },
		DriveCheck: func(t *testing.T, a bamlutils.Adapter) opharness.Outcome {
			ch, err := fixture.StaticCheckedConfidence(a, &fixture.StaticCheckedConfidenceInput{Topic: "weather"})
			return drain(t, ch, err)
		},
		DriveAssert: func(t *testing.T, a bamlutils.Adapter) opharness.Outcome {
			ch, err := fixture.StaticAssertConfidence(a, &fixture.StaticAssertConfidenceInput{Topic: "weather"})
			return drain(t, ch, err)
		},
	}
}

// TestOperatorRoutes_SeamIsEmittedAndDescriptored is the build-time half, and it is
// what makes the live results below mean something.
//
// If a route carried no descriptor its zeros — or its socket — would witness an
// un-emitted seam rather than an admission decision. Both routes carry one, with a
// projector and no build-time decline, so admission is the only thing left to decide
// them.
func TestOperatorRoutes_SeamIsEmittedAndDescriptored(t *testing.T) {
	opharness.RequireSeamEmitted(t, opID,
		[]string{"StaticCheckedConfidence", "StaticAssertConfidence"},
		func(route string) (bool, bool, string, bool) {
			_, hasDescriptor := introspected.StaticPromptDescriptor(route)
			_, hasProjector := introspected.StaticPromptArgumentProjectors[route]
			reason, declined := introspected.StaticPromptDeclines[route]
			return hasDescriptor, hasProjector, reason, declined
		})
}

// TestOperatorRoutes_FlagOnServesNative is the LIVE admission proof for this operator:
// all four serving-shaped outcomes, flag ON and flag OFF over the same provider bytes,
// each requiring ONE native socket on and ZERO off, byte equality between the legs, and
// byte equality with the 7.2c-1 stock CFFI capture.
func TestOperatorRoutes_FlagOnServesNative(t *testing.T) {
	opharness.RunServedRows(t, project())
}

// TestOperatorRoutes_StreamIsAZeroSocketDecline keeps the scope's route boundary live
// for this operator: the SAME admitted return schema, on the /stream route, opens no
// socket at all.
//
// Widening the predicate widened the SHAPE. The route set is unchanged, and this is
// where that is measured rather than asserted about the gate.
func TestOperatorRoutes_StreamIsAZeroSocketDecline(t *testing.T) {
	opharness.RunStreamDecline(t, project())
}
