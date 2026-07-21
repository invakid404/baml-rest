//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 8C static serving CUTOVER MANIFEST + frozen zero-mismatch pin
// (review P1.2 + P1.3), driven end-to-end through the REAL generated static-serve
// fixture adapter over the loopback capture server. It is the shadow→serve cutover
// gate: it drives EVERY serve-enabled admitted fixture method through BOTH stages —
//
//	Stage-1 SHADOW (BAML sole sender; native parses the captured bytes with ZERO native
//	sends): plan-matched, shadow-parsed, response-match are MEASURED from the shadow
//	comparator, NOT inferred from a serve claim.
//	Stage-2 SERVE (native serves one send): claimed, native-win, BAML-parse-only-winner.
//
// It pins the exact tallies with an ANTI-OMISSION assertion tying the driven route set
// to the fixture's emitted/admitted route set (introspected.SyncMethods). Any drift
// (a new decline, a structured/order/typed mismatch, a plan mismatch, a post-claim
// resend, or an uncovered enabled route) breaks the pin. The FROZEN admitted set is the
// two v0.223-differential-proven return shapes: a top-level `string` (native SAP
// declines the bare string, BAML parse-only wins) and the flat
// StaticAnswer{answer:string, confidence:int} class (native structured win / shadow
// parse).

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticRoute is one serve-enabled admitted fixture method + its return kind.
type staticRoute struct {
	name    string
	isClass bool // StaticAnswer class return (native structured / shadow parse) vs top-level string
}

// staticServingCorpus is the FROZEN, explicitly-pinned serving corpus: every
// serve-enabled admitted fixture static method. The anti-omission test asserts this
// equals the fixture's emitted route set (introspected.SyncMethods).
var staticServingCorpus = []staticRoute{
	{"StaticCompletion", false},
	{"StaticCompletionOutputFormat", true},
	{"StaticOutputFormat", true},
	{"StaticPrimitiveArgs", false},
	{"StaticRoleChat", false},
}

// driveRoute invokes the generated /call for one route and drains the stream, returning
// the outcome winner/planned engine tokens and any error.
func driveRoute(t *testing.T, a bamlutils.Adapter, name string) (winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticCompletion":
		ch, err = fixture.StaticCompletion(a, &fixture.StaticCompletionInput{Topic: "cats"})
	case "StaticCompletionOutputFormat":
		ch, err = fixture.StaticCompletionOutputFormat(a, &fixture.StaticCompletionOutputFormatInput{Topic: "weather"})
	case "StaticOutputFormat":
		ch, err = fixture.StaticOutputFormat(a, &fixture.StaticOutputFormatInput{Topic: "weather"})
	case "StaticPrimitiveArgs":
		ch, err = fixture.StaticPrimitiveArgs(a, &fixture.StaticPrimitiveArgsInput{Text: "hi", Count: 3, Ratio: 1.5, Flag: true})
	case "StaticRoleChat":
		ch, err = fixture.StaticRoleChat(a, &fixture.StaticRoleChatInput{Topic: "weather", Count: 2})
	default:
		t.Fatalf("unknown route %q", name)
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

// routeResponse is the loopback body for a route: StaticAnswer JSON for a class return,
// a bare string for a top-level string return.
func routeResponse(isClass bool) []byte {
	if isClass {
		return openAIStaticAnswer("sunny", 9)
	}
	return openAIBareString("Here is a short answer.")
}

// gatherCounter sums the named counter over a registry for the given exact label set.
func gatherCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			match := true
			for wantK, wantV := range labels {
				found := false
				for _, lp := range mm.GetLabel() {
					if lp.GetName() == wantK && lp.GetValue() == wantV {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				sum += mm.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// TestStaticServingCutover_AntiOmission asserts the driven serving corpus is EXACTLY
// the fixture's emitted/admitted static route set — no enabled route may be silently
// omitted from the frozen pin.
func TestStaticServingCutover_AntiOmission(t *testing.T) {
	emitted := make([]string, 0, len(introspected.SyncMethods))
	for m := range introspected.SyncMethods {
		emitted = append(emitted, m)
	}
	driven := make([]string, 0, len(staticServingCorpus))
	for _, r := range staticServingCorpus {
		driven = append(driven, r.name)
	}
	sort.Strings(emitted)
	sort.Strings(driven)
	if len(emitted) != len(driven) {
		t.Fatalf("corpus omits enabled routes: driven=%v emitted=%v", driven, emitted)
	}
	for i := range emitted {
		if emitted[i] != driven[i] {
			t.Fatalf("corpus route set != emitted route set:\n driven  =%v\n emitted =%v", driven, emitted)
		}
	}
}

// TestStaticServingCutoverManifest freezes the shadow→serve pin over the full admitted
// generated-route corpus, with shadow-parsed MEASURED from the Stage-1 shadow.
func TestStaticServingCutoverManifest(t *testing.T) {
	var planMatched, shadowParsed, responseMatch int
	var claimed, nativeWin, bamlParseWinner int

	// --- Stage-1 SHADOW: BAML sole sender; native parses the captured bytes (0 sends).
	for _, r := range staticServingCorpus {
		spy := newStaticShadowSpy(t)
		server := newFixtureServer(t, 200, routeResponse(r.isClass))
		a := buildFixtureShadowAdapter(t, spy, true)
		_, planned, err := driveRoute(t, a, r.name)
		if err != nil {
			server.close()
			t.Fatalf("shadow %s: %v", r.name, err)
		}
		// BAML sole sender, ZERO native sends.
		if got := server.count.Load(); got != 1 {
			t.Errorf("shadow %s: provider saw %d requests, want 1 (BAML sole sender)", r.name, got)
		}
		if got := spy.counter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
			t.Errorf("shadow %s: native_sockets{on}=%v, want 0 (ZERO native sends)", r.name, got)
		}
		if planned != "native" {
			t.Errorf("shadow %s: planned_engine=%q, want native", r.name, planned)
		}
		// plan-matched (MEASURED plan_compare), shadow-parsed (native pipeline ran clean
		// over BAML's bytes), response-match (no facet mismatch, translate matched).
		if spy.counter(t, "baml_rest_debaml_plan_compare_total", "result", "match") == 1 {
			planMatched++
		}
		if gatherCounter(t, spy.reg, "baml_rest_debaml_response_compare_total", map[string]string{"field": "error", "result": "match"}) == 1 {
			shadowParsed++
		}
		if gatherCounter(t, spy.reg, "baml_rest_debaml_response_compare_total", map[string]string{"result": "mismatch"}) == 0 &&
			spy.responseCompareMatch(t, "translate") == 1 {
			responseMatch++
		}
		server.close()
	}

	// --- Stage-2 SERVE: native serves one send.
	for _, r := range staticServingCorpus {
		spy := newStaticServeSpy(t)
		server := newFixtureServer(t, 200, routeResponse(r.isClass))
		a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
		winner, _, err := driveRoute(t, a, r.name)
		if err != nil {
			server.close()
			t.Fatalf("serve %s: %v", r.name, err)
		}
		if got := server.count.Load(); got != 1 {
			t.Errorf("serve %s: provider saw %d requests, want 1 (one-send, no resend)", r.name, got)
		}
		if spy.nativeSocketsRaw() == 1 {
			claimed++
		}
		switch winner {
		case bamlutils.NativeStaticServeEngineNative:
			nativeWin++
		case bamlutils.NativeStaticServeEngineBAMLParse:
			bamlParseWinner++
		}
		server.close()
	}

	// --- Pre-claim decline control: call-with-raw declines to BAML (0 native / 1 BAML).
	preclaimDeclined := 0
	{
		spy := newStaticServeSpy(t)
		server := newFixtureServer(t, 200, routeResponse(true))
		a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCallWithRaw)
		if _, _, err := driveRoute(t, a, "StaticOutputFormat"); err != nil {
			server.close()
			t.Fatalf("call-with-raw decline: %v", err)
		}
		if spy.nativeSocketsRaw() == 0 && spy.calls.Load() == 1 && server.count.Load() == 1 {
			preclaimDeclined++
		}
		server.close()
	}

	n := len(staticServingCorpus)
	pin := map[string]int{
		"total":                  n,
		"plan-matched":           planMatched,
		"shadow-parsed":          shadowParsed,
		"response-match":         responseMatch,
		"claimed":                claimed,
		"native-win":             nativeWin,
		"BAML-parse-only-winner": bamlParseWinner,
		"preclaim-declined":      preclaimDeclined,
	}
	want := map[string]int{
		"total":                  5,
		"plan-matched":           5, // every admitted route's plan matched (shadow stage)
		"shadow-parsed":          5, // every admitted route's native pipeline ran clean over BAML's bytes
		"response-match":         5, // every admitted route: native == BAML on every compared facet
		"claimed":                5, // every admitted route claimed one native socket (serve stage)
		"native-win":             2, // the two StaticAnswer class routes served native structured
		"BAML-parse-only-winner": 3, // the three top-level string routes served the BAML parse
		"preclaim-declined":      1, // call-with-raw declines pre-socket
	}
	for k, w := range want {
		if pin[k] != w {
			t.Errorf("cutover pin %s = %d, want %d (frozen)", k, pin[k], w)
		}
	}
	t.Logf("static serving cutover pin (frozen): %+v", pin)
}
