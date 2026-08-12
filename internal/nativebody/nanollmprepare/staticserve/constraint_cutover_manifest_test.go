//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 7.2b-2 — the LIVE proof that the two constraint-bearing static
// returns are DECLINED, end to end, through the REAL generated adapter.
//
// The fixture project now declares the two concrete return types the 7.2b scope
// admits as the first production-admission fingerprint:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(positive, {{ this > 0 }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(positive, {{ this > 0 }}) }
//
// so the generated client carries the real `Checked[int64]` carrier (re-pointed at
// bamlutils.Checked by the fixture transform) and the generated /call seam emits a
// `bamlutils.DecodeStaticFinal[types.StaticCheckedAnswer]` closure. That is the
// production path this slice is about — not a hand-written look-alike.
//
// NOTHING ADMITS THEM YET. These routes differ from the ADMITTED StaticOutputFormat
// route in exactly one property — the constraint on `confidence` — so the decline
// asserted here is attributable to the constraint and to nothing else. Unlike the
// MEDIA rows (which have no V3 descriptor at all), these routes DO get a descriptor,
// DO get a projector, and DO install a native attempt: the serve callback runs and
// declines PRE-SOCKET inside admission, which is a strictly harder thing to observe
// and the exact behaviour 7.2b-3 will flip.
//
// Each row therefore asserts the FULL set rather than a bare zero:
//
//   - the serve callback IS invoked (so this is a decline, not an un-emitted seam),
//   - planned_engine == "native" (native WAS considered),
//   - ZERO native sockets — the pre-claim transport observable,
//   - the winner is NOT native, and
//   - exactly ONE ordinary BAML provider request served the call.

import (
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusConstraintDeclined is the FROZEN Slice 7.2b-2 partition: the
// routes whose RETURN carries a constraint. They are emitted, descriptored and
// seam-installed, and they decline at admission until 7.2b-3.
var staticServingCorpusConstraintDeclined = []staticRoute{
	{"StaticCheckedConfidence", true},
	{"StaticAssertConfidence", true},
}

// driveConstraintRoute invokes the generated /call for one constraint route and
// drains the stream. A separate switch keeps the frozen sibling corpora untouched.
func driveConstraintRoute(t *testing.T, a bamlutils.Adapter, name string) (winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticCheckedConfidence":
		ch, err = fixture.StaticCheckedConfidence(a, &fixture.StaticCheckedConfidenceInput{Topic: "weather"})
	case "StaticAssertConfidence":
		ch, err = fixture.StaticAssertConfidence(a, &fixture.StaticAssertConfidenceInput{Topic: "weather"})
	default:
		t.Fatalf("driveConstraintRoute: unknown route %q", name)
	}
	if err != nil {
		return "", "", err
	}
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindError:
			drainErr = r.Error()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				winner = md.WinnerEngine
				planned = md.PlannedEngine
			}
		}
		r.Release()
	}
	return winner, planned, drainErr
}

// TestConstraintRoutes_SeamIsEmittedAndDescriptored is the build-time half, and it
// is what makes the live decline below mean something.
//
// If these routes carried no descriptor (the MEDIA situation) their zeros would
// witness an un-emitted seam rather than a closed admission gate. They DO carry one,
// with a projector and no build-time decline, so the only thing left to refuse them
// is admission itself.
func TestConstraintRoutes_SeamIsEmittedAndDescriptored(t *testing.T) {
	for _, r := range staticServingCorpusConstraintDeclined {
		if _, ok := introspected.StaticPromptDescriptor(r.name); !ok {
			t.Errorf("%s: NO V3 descriptor was emitted; the live decline below would witness an "+
				"un-emitted seam rather than a closed admission gate", r.name)
		}
		if _, ok := introspected.StaticPromptArgumentProjectors[r.name]; !ok {
			t.Errorf("%s: no argument projector was emitted", r.name)
		}
		if reason, declined := introspected.StaticPromptDeclines[r.name]; declined {
			t.Errorf("%s: a BUILD-TIME decline was recorded (%q); these routes must reach admission", r.name, reason)
		}
	}
}

// TestConstraintRoutes_FlagOnDeclinesPreSocket is the LIVE unary proof.
func TestConstraintRoutes_FlagOnDeclinesPreSocket(t *testing.T) {
	// NON-VACUITY CONTROL, in the SAME adapter configuration the rows below use: an
	// ADMITTED class route reaches native and claims its socket. Without it, every
	// zero below would be satisfied by a harness that never reaches native at all.
	t.Run("control_admitted_class_route_claims_a_socket", func(t *testing.T) {
		spy := newStaticServeSpy(t)
		server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
		a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
		winner, planned, err := driveRoute(t, a, "StaticOutputFormat")
		server.close()
		if err != nil {
			t.Fatalf("control route: %v", err)
		}
		if got := spy.calls.Load(); got != 1 {
			t.Fatalf("control route invoked the serve func %d times, want 1; the rows below would be vacuous", got)
		}
		if planned != "native" || winner != bamlutils.NativeStaticServeEngineNative {
			t.Fatalf("control route planned=%q winner=%q, want both native; the rows below would be vacuous",
				planned, winner)
		}
		if got := spy.nativeSockets(t, "on"); got != 1 {
			t.Fatalf("control route native_sockets{on}=%v, want 1; the zero-socket assertions below could "+
				"not distinguish a decline from a harness that never opens one", got)
		}
	})

	for _, r := range staticServingCorpusConstraintDeclined {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
			a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
			winner, planned, err := driveConstraintRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}

			// The seam IS installed and the callback DOES run: this is a decline
			// inside admission, not an absent seam.
			if got := spy.calls.Load(); got != 1 {
				t.Errorf("%s: native serve func invoked %d times, want 1 (the seam is installed; the "+
					"constraint is refused INSIDE it)", r.name, got)
			}
			if planned != "native" {
				t.Errorf("%s: planned_engine=%q, want native (native was considered)", r.name, planned)
			}
			// …and it refuses BEFORE transport.
			if got := spy.nativeSockets(t, "on"); got != 0 {
				t.Errorf("%s: native_sockets{on}=%v, want 0 (a constraint-bearing return must decline "+
					"pre-socket)", r.name, got)
			}
			if winner == bamlutils.NativeStaticServeEngineNative {
				t.Errorf("%s: winner_engine=%q, want BAML", r.name, winner)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: provider saw %d requests, want 1 (one ordinary BAML request)", r.name, got)
			}
		})
	}
}
