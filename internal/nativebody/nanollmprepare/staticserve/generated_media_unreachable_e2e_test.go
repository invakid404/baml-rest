//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 7.1b — LIVE proof that MEDIA is unreachable from static admission
// (scope section 6, "Media stays closed").
//
// Media is structurally closed at descriptor construction: it is not a V3 input
// value node, so BuildPromptDescriptors emits no descriptor for a media-bearing
// signature (proven in internal/nativeschema/inputvalues_test.go and
// internal/nativeprompt/staticoracle/decline_test.go). That is a SOURCE-level
// fact, and on its own it does not prove the SEAM is closed — a future wiring
// change could route around the descriptor.
//
// It cannot, and this file is why. The generated static seam IS emitted for all
// three media routes (the codegen does not special-case media), so the only
// thing standing between a media request and a native attempt is
// introspected.StaticPromptDescriptor returning ok=false. These tests drive the
// REAL generated adapter with the umbrella flag ON and assert the outcome that
// depends on it:
//
//   - ZERO native serve/stream callback invocations,
//   - ZERO native sockets (the unary socket metric; the stream lane's
//     pre-claim transport observable),
//   - an EMPTY planned_engine (native was never even considered),
//   - exactly ONE ordinary BAML provider request whose winner is not native, and
//   - on the stream lane, NO drain error — a stream that failed before the BAML
//     call would show the same zeros, so a decline and a failure are separated
//     explicitly rather than left indistinguishable,
//
// on BOTH unary /call and /stream, for a DIRECT media argument, a media LIST,
// and media reached THROUGH a class — three different paths through the
// generated adapter's argument conversion.
//
// Each half carries its OWN non-vacuity control in this suite (the unary and
// stream harnesses are different), because every assertion here is a zero and a
// mis-wired harness would produce those zeros too.
//
// This is a PRE-CLAIM assertion: it fails if native so much as considers the
// route, not merely if the response comes back wrong. The broad Docker
// integration suite exercises media requests end to end but asserts nothing
// about native admission, so it cannot substitute for this.

import (
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client/types"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusMediaDeclined is the FROZEN media partition: routes that the
// generated adapter emits a static seam for but that have NO V3 descriptor, so
// the seam can never install a native attempt.
var staticServingCorpusMediaDeclined = []staticRoute{
	{"StaticMediaImage", false},
	{"StaticMediaImageList", false},
	{"StaticMediaInClass", false},
}

// mediaURLInput builds the fixture's media argument shape (a URL-form media
// input; the loopback capture server never fetches it).
func mediaURLInput() bamlutils.MediaInput {
	u := "https://example.test/i.png"
	mt := "image/png"
	return bamlutils.MediaInput{URL: &u, MediaType: &mt}
}

// driveMediaRoute invokes the generated /call for one media route and drains the
// stream, returning the outcome winner/planned engine tokens and any error.
func driveMediaRoute(t *testing.T, a bamlutils.Adapter, name string) (winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticMediaImage":
		ch, err = fixture.StaticMediaImage(a, &fixture.StaticMediaImageInput{Img: mediaURLInput()})
	case "StaticMediaImageList":
		ch, err = fixture.StaticMediaImageList(a, &fixture.StaticMediaImageListInput{
			Imgs: []bamlutils.MediaInput{mediaURLInput(), mediaURLInput()},
		})
	case "StaticMediaInClass":
		ch, err = fixture.StaticMediaInClass(a, &fixture.StaticMediaInClassInput{
			Bundle: fixture.MediaBundleMediaInput{Img: mediaURLInput(), Caption: "a caption"},
		})
	default:
		t.Fatalf("driveMediaRoute: unknown route %q", name)
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

// TestMediaRoutes_NoV3DescriptorIsEmitted is the build-time half: the emitted
// fixture carries NO descriptor and NO projector for any media route, and the
// recorded decline names media. This is what the live assertions below depend
// on, so it is asserted explicitly rather than assumed.
func TestMediaRoutes_NoV3DescriptorIsEmitted(t *testing.T) {
	for _, r := range staticServingCorpusMediaDeclined {
		if _, ok := introspected.StaticPromptDescriptor(r.name); ok {
			t.Errorf("%s: a V3 descriptor was emitted for a MEDIA signature; media is not a V3 value node", r.name)
		}
		if _, ok := introspected.StaticPromptArgumentProjectors[r.name]; ok {
			t.Errorf("%s: an argument projector was emitted for a MEDIA signature", r.name)
		}
		reason, ok := introspected.StaticPromptDeclines[r.name]
		if !ok {
			t.Errorf("%s: no build-time decline reason recorded", r.name)
			continue
		}
		if !contains(reason, "media types are not supported") {
			t.Errorf("%s: decline reason %q does not name media", r.name, reason)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestMediaRoutes_FlagOnNeverReachesNative is the LIVE unary proof: with the
// umbrella flag ON, a media request produces no native activity whatsoever and
// BAML serves it.
func TestMediaRoutes_FlagOnNeverReachesNative(t *testing.T) {
	// NON-VACUITY CONTROL. Everything below asserts a ZERO, which a
	// mis-wired harness would also produce. This proves the SAME adapter
	// configuration (flag on, serve callback installed, same loopback server)
	// really does drive the callback for an ADMITTED route — so the zeros that
	// follow are attributable to the media gate and nothing else.
	t.Run("control_admitted_route_does_invoke_native", func(t *testing.T) {
		spy := newStaticServeSpy(t)
		server := newFixtureServer(t, http.StatusOK, openAIBareString("ok"))
		a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
		_, planned, err := driveValueBindRoute(t, a, "StaticRenderEnum")
		server.close()
		if err != nil {
			t.Fatalf("control route: %v", err)
		}
		if got := spy.calls.Load(); got != 1 {
			t.Fatalf("control route invoked the serve func %d times, want 1; the media zeros below would be vacuous", got)
		}
		if planned != "native" {
			t.Fatalf("control route planned_engine=%q, want native; the media zeros below would be vacuous", planned)
		}
	})

	for _, r := range staticServingCorpusMediaDeclined {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, openAIBareString("ok"))
			a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
			winner, planned, err := driveMediaRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}

			// The serve callback is INSTALLED on the adapter (buildFixtureAdapter
			// wires it) and the seam IS emitted for this method, so a zero call
			// count means the descriptor lookup stopped the install — the exact
			// pre-claim gate under test.
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("%s: native serve func invoked %d times, want 0 (media must never reach native admission)", r.name, got)
			}
			if got := spy.nativeSockets(t, "on"); got != 0 {
				t.Errorf("%s: native_sockets{on}=%v, want 0", r.name, got)
			}
			if planned != "" {
				t.Errorf("%s: planned_engine=%q, want empty (native was never considered)", r.name, planned)
			}
			if winner == bamlutils.NativeStaticServeEngineNative {
				t.Errorf("%s: winner_engine=%q, want BAML", r.name, winner)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: provider saw %d requests, want 1 (an ordinary BAML request)", r.name, got)
			}
		})
	}
}

// driveMediaStream drives one media route over the /stream lane and returns the
// ordered public-event trace.
func driveMediaStream(t *testing.T, a bamlutils.Adapter, name string) streamTrace {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticMediaImage":
		ch, err = fixture.StaticMediaImage(a, &fixture.StaticMediaImageInput{Img: mediaURLInput()})
	case "StaticMediaImageList":
		ch, err = fixture.StaticMediaImageList(a, &fixture.StaticMediaImageListInput{
			Imgs: []bamlutils.MediaInput{mediaURLInput()},
		})
	case "StaticMediaInClass":
		ch, err = fixture.StaticMediaInClass(a, &fixture.StaticMediaInClassInput{
			Bundle: fixture.MediaBundleMediaInput{Img: mediaURLInput(), Caption: "c"},
		})
	default:
		t.Fatalf("driveMediaStream: unknown route %q", name)
	}
	return drainStreamTrace(t, ch, err)
}

// TestMediaRoutes_FlagOnStreamNeverReachesNative is the LIVE stream proof: the
// static-STREAM seam is likewise never installed for a media route, and BAML
// serves the stream.
//
// Every row asserts the FULL set, not just a zero callback count. A stream that
// failed BEFORE producing outcome metadata would also show zero callbacks and an
// empty winner, so a decline and a failure would be indistinguishable — the
// drain-error and provider-request assertions are what separate them, and
// nativeSocketsZero is the pre-claim transport observable rather than a proxy
// for it.
func TestMediaRoutes_FlagOnStreamNeverReachesNative(t *testing.T) {
	// NON-VACUITY CONTROL, local to this suite and using the SAME stream adapter
	// configuration the media rows use. It proves the harness can OBSERVE a
	// claimed native stream — one callback, planned/winner native, and a
	// nativeSocketsZero that reports FALSE — so the all-zero media rows below are
	// attributable to the media gate rather than to a stream harness that never
	// reaches native for anything.
	t.Run("control_admitted_stream_route_does_invoke_native", func(t *testing.T) {
		spy := newStreamServeSpy(t)
		server := newFixtureStreamServer(t, contentSSE([]string{`"`, `rouge`, `"`}, nil))
		a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
		ch, err := fixture.StaticStreamRenderEnum(a, &fixture.StaticStreamRenderEnumInput{Color: types.ColorRED})
		tr := drainStreamTrace(t, ch, err)
		server.close()

		if tr.drainer != nil {
			t.Fatalf("control stream drained with an error: %v; the media zeros below would be vacuous", tr.drainer)
		}
		if got := spy.calls.Load(); got != 1 {
			t.Fatalf("control stream invoked the serve func %d times, want 1; the media zeros below would be vacuous", got)
		}
		if tr.planned != "native" {
			t.Fatalf("control stream planned_engine=%q, want native; the media zeros below would be vacuous", tr.planned)
		}
		if tr.winner != bamlutils.NativeServeEngineNative {
			t.Fatalf("control stream winner=%q, want native; the media zeros below would be vacuous", tr.winner)
		}
		if spy.nativeSocketsZero() {
			t.Fatalf("control stream reports nativeSocketsZero()=true for a CLAIMED stream; " +
				"the observable cannot discriminate, so the media assertions below would be vacuous")
		}
		if got := server.count.Load(); got != 1 {
			t.Fatalf("control stream: provider saw %d requests, want 1", got)
		}
	})

	for _, r := range staticServingCorpusMediaDeclined {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStreamServeSpy(t)
			server := newFixtureStreamServer(t, contentSSE([]string{"ok"}, nil))
			a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
			tr := driveMediaStream(t, a, r.name)
			server.close()

			// A drain error would make every zero below meaningless: a stream that
			// failed before the BAML call could not produce outcome metadata either.
			// Fail FIRST so a "decline" can never be confused with a failure.
			if tr.drainer != nil {
				t.Fatalf("%s: stream drained with an error (%v); the decline assertions cannot be distinguished from a failure",
					r.name, tr.drainer)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("%s: native STREAM serve func invoked %d times, want 0 (media must never reach native stream admission)", r.name, got)
			}
			// The pre-claim TRANSPORT condition, not merely the callback count: a
			// claimed stream opens a socket and reports Completed/FailedAfterClaim,
			// so this is false there (proven by the control above).
			if !spy.nativeSocketsZero() {
				t.Errorf("%s: the native stream lane opened a socket; a media route must never claim one", r.name)
			}
			if tr.planned != "" {
				t.Errorf("%s: stream planned_engine=%q, want empty (installNativeStaticStream must never run)", r.name, tr.planned)
			}
			if tr.winner == bamlutils.NativeServeEngineNative {
				t.Errorf("%s: stream winner=%q, want BAML", r.name, tr.winner)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: provider saw %d requests, want 1 (one ordinary BAML stream)", r.name, got)
			}
		})
	}
}

// TestMediaRoutes_FlagOffIsPureBAML pins the kill switch for the media routes
// too, so the flag-on result above is attributable to the media gate rather than
// to the flag being off by accident.
func TestMediaRoutes_FlagOffIsPureBAML(t *testing.T) {
	for _, r := range staticServingCorpusMediaDeclined {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, openAIBareString("ok"))
			a := buildFixtureAdapter(t, spy, false, bamlutils.StreamModeCall)
			_, planned, err := driveMediaRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("%s: flag-off invoked the serve func %d times, want 0", r.name, got)
			}
			if planned != "" {
				t.Errorf("%s: flag-off planned_engine=%q, want empty", r.name, planned)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: flag-off provider saw %d requests, want 1 (BAML serves)", r.name, got)
			}
		})
	}
}
