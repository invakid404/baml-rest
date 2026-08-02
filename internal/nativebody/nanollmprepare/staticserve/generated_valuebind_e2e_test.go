//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 7.1b — LIVE generated-route proof for RESOLVED STATIC VALUES.
//
// The stock differential (internal/nativeprompt/staticoracle) is the semantic
// authority: it owns the exact rendered bytes. This file proves the other half —
// that the WIRING really carries them, through the REAL generated fixture
// adapter, for every newly admitted form:
//
//   - FLAG ON: the generated /call installs the native serve attempt, the
//     projector produces a vector, admission ACCEPTS, and native claims EXACTLY
//     ONE provider RoundTrip (native_sockets{flag=on} == 1) after the no-send
//     BAML plan comparator matched. BAML never sends a second time.
//   - FLAG OFF: the seam gate keeps the callback nil, the serve func is NEVER
//     invoked (ZERO native FFI/socket), and BAML serves the one request —
//     byte-identical to today.
//   - NEGATIVE NEIGHBOURS with the flag ON: the display-alias equality row and
//     the direct class render are PRE-CLAIM declines — the serve func runs and
//     steps aside with ZERO native sockets, and BAML serves.
//   - The STATIC STREAM route gets the same treatment, not only unary /call.
//
// Why the stream rows return `JSON` and the unary rows return `string`: the
// native static stream lane only claims the served recursive-alias family, so a
// `string`-returning method could never demonstrate a claimed stream. The two
// JSON-returning stream witnesses differ ONLY in their prompt, which isolates
// the value/grammar gate as the single variable on that route.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client/types"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusValueBind is the FROZEN Slice 7.1b SERVED corpus: every
// newly admitted form, driven over the unary /call route.
//
// All but the last return `string`, whose bare-string body the native static SAP
// declines — so native CLAIMS the socket and BAML's parse of the same bytes wins
// the RESULT. The claim, not the winner, is what those rows pin.
// StaticStreamRenderEnum is the exception: it returns the served recursive-alias
// family `JSON` (it exists for the /stream route, see the stream tests below)
// and is driven here too so its unary claim is covered by the same matrix. The
// bare-string SAP explanation does not apply to it.
var staticServingCorpusValueBind = []staticRoute{
	// Literal-only enum expressions (no arguments at all — they exercise the
	// project-wide namespace globals).
	{"StaticEnumCanonicalEq", false},
	{"StaticEnumSameMemberEq", false},
	{"StaticEnumCanonicalInMemberList", false},
	{"StaticEnumDifferentMemberEq", false},
	{"StaticEnumReverseCanonicalEq", false},
	{"StaticEnumMemberInCanonicalList", false},
	// Enum-ARGUMENT expressions, both operand orders.
	{"StaticEnumArgMemberEq", false},
	{"StaticEnumMemberArgEq", false},
	{"StaticEnumArgCanonicalEq", false},
	{"StaticEnumCanonicalArgEq", false},
	// Direct host-value renders.
	{"StaticRenderEnum", false},
	{"StaticRenderList", false},
	{"StaticRenderStrings", false},
	// The claimed STREAM witness also serves a unary /call.
	{"StaticStreamRenderEnum", false},
}

// staticServingCorpusValueBindDeclined is the FROZEN Slice 7.1b DECLINED corpus:
// routes that are emitted and have a V3 descriptor + a projector, but whose
// PROMPT the admission grammar refuses. They must reach ZERO native sockets.
var staticServingCorpusValueBindDeclined = []staticRoute{
	// A DISPLAY ALIAS is not an identity: stock answers `false`, and this slice
	// claims canonical identity only.
	{"StaticEnumDisplayAliasEq", false},
	{"StaticStreamAliasEq", false},
	// A direct CLASS render: stock BAML v0.223's Go client encodes a class
	// through a Go map, so its rendered field order is not reproducible.
	{"StaticRenderPalette", false},
	{"StaticRenderPalettes", false},
}

// valueBindPalette is the nested class value the declined class rows pass — a
// WELL-FORMED value, so the decline is provably about the prompt shape and not
// about a malformed argument.
func valueBindPalette() types.Palette {
	return types.Palette{
		Primary: types.ColorGREEN,
		Shades:  []types.Color{types.ColorBLUE, types.ColorRED},
		Swatch:  types.Swatch{Color: types.ColorRED, Label: "spring"},
		Name:    "café",
	}
}

// driveValueBindRoute invokes the generated /call for one Slice 7.1b route and
// drains the stream, returning the outcome winner/planned tokens and any error.
// It mirrors driveRoute; a separate switch keeps the frozen legacy corpus
// untouched.
func driveValueBindRoute(t *testing.T, a bamlutils.Adapter, name string) (winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticEnumCanonicalEq":
		ch, err = fixture.StaticEnumCanonicalEq(a, &fixture.StaticEnumCanonicalEqInput{})
	case "StaticEnumSameMemberEq":
		ch, err = fixture.StaticEnumSameMemberEq(a, &fixture.StaticEnumSameMemberEqInput{})
	case "StaticEnumCanonicalInMemberList":
		ch, err = fixture.StaticEnumCanonicalInMemberList(a, &fixture.StaticEnumCanonicalInMemberListInput{})
	case "StaticEnumDifferentMemberEq":
		ch, err = fixture.StaticEnumDifferentMemberEq(a, &fixture.StaticEnumDifferentMemberEqInput{})
	case "StaticEnumReverseCanonicalEq":
		ch, err = fixture.StaticEnumReverseCanonicalEq(a, &fixture.StaticEnumReverseCanonicalEqInput{})
	case "StaticEnumMemberInCanonicalList":
		ch, err = fixture.StaticEnumMemberInCanonicalList(a, &fixture.StaticEnumMemberInCanonicalListInput{})
	case "StaticEnumDisplayAliasEq":
		ch, err = fixture.StaticEnumDisplayAliasEq(a, &fixture.StaticEnumDisplayAliasEqInput{})
	case "StaticEnumArgMemberEq":
		ch, err = fixture.StaticEnumArgMemberEq(a, &fixture.StaticEnumArgMemberEqInput{Color: types.ColorRED})
	case "StaticEnumMemberArgEq":
		ch, err = fixture.StaticEnumMemberArgEq(a, &fixture.StaticEnumMemberArgEqInput{Color: types.ColorRED})
	case "StaticEnumArgCanonicalEq":
		ch, err = fixture.StaticEnumArgCanonicalEq(a, &fixture.StaticEnumArgCanonicalEqInput{Color: types.ColorGREEN})
	case "StaticEnumCanonicalArgEq":
		ch, err = fixture.StaticEnumCanonicalArgEq(a, &fixture.StaticEnumCanonicalArgEqInput{Color: types.ColorRED})
	case "StaticRenderEnum":
		ch, err = fixture.StaticRenderEnum(a, &fixture.StaticRenderEnumInput{Color: types.ColorRED})
	case "StaticRenderList":
		ch, err = fixture.StaticRenderList(a, &fixture.StaticRenderListInput{Colors: []types.Color{types.ColorBLUE, types.ColorRED}})
	case "StaticRenderStrings":
		ch, err = fixture.StaticRenderStrings(a, &fixture.StaticRenderStringsInput{Tags: []string{"b", "a"}})
	case "StaticRenderPalette":
		ch, err = fixture.StaticRenderPalette(a, &fixture.StaticRenderPaletteInput{Palette: valueBindPalette()})
	case "StaticRenderPalettes":
		ch, err = fixture.StaticRenderPalettes(a, &fixture.StaticRenderPalettesInput{Palettes: []types.Palette{valueBindPalette()}})
	case "StaticStreamRenderEnum":
		ch, err = fixture.StaticStreamRenderEnum(a, &fixture.StaticStreamRenderEnumInput{Color: types.ColorRED})
	case "StaticStreamAliasEq":
		ch, err = fixture.StaticStreamAliasEq(a, &fixture.StaticStreamAliasEqInput{Color: types.ColorRED})
	default:
		t.Fatalf("driveValueBindRoute: unknown route %q", name)
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

// valueBindResponse is the provider reply every unary row receives: a bare
// assistant string, the natural shape of a `string` return.
func valueBindResponse() []byte { return openAIBareString("ok") }

// TestValueBindRoutes_FlagOnClaimsNativeSocket is the positive live proof: with
// the umbrella flag ON, every newly admitted form is ADMITTED by the generated
// seam and native claims exactly one provider RoundTrip.
func TestValueBindRoutes_FlagOnClaimsNativeSocket(t *testing.T) {
	for _, r := range staticServingCorpusValueBind {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, valueBindResponse())
			a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
			_, planned, err := driveValueBindRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}

			if got := spy.calls.Load(); got != 1 {
				t.Fatalf("%s: serve func invoked %d times, want 1", r.name, got)
			}
			disp, stage, reason := spy.lastDecline()
			if got := spy.nativeSockets(t, "on"); got != 1 {
				t.Fatalf("%s: native_sockets{on}=%v, want 1 (native must CLAIM); disposition=%d stage=%q reason=%q",
					r.name, got, disp, stage, reason)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: provider saw %d requests, want EXACTLY 1 (no hidden BAML resend)", r.name, got)
			}
			if planned != "native" {
				t.Errorf("%s: planned_engine=%q, want native", r.name, planned)
			}
		})
	}
}

// TestValueBindRoutes_FlagOffIsPureBAML pins the kill switch: with the flag OFF
// the generated seam never resolves a serve callback, so it performs no
// descriptor lookup, runs no projector, invokes no serve func, and opens no
// native socket — BAML serves the one request exactly as before this slice.
func TestValueBindRoutes_FlagOffIsPureBAML(t *testing.T) {
	all := append(append([]staticRoute{}, staticServingCorpusValueBind...), staticServingCorpusValueBindDeclined...)
	for _, r := range all {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, valueBindResponse())
			a := buildFixtureAdapter(t, spy, false, bamlutils.StreamModeCall)
			_, planned, err := driveValueBindRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("%s: flag-off invoked the serve func %d times, want 0", r.name, got)
			}
			if got := spy.nativeSockets(t, "on"); got != 0 {
				t.Errorf("%s: flag-off native_sockets{on}=%v, want 0", r.name, got)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: flag-off provider saw %d requests, want 1 (BAML serves)", r.name, got)
			}
			if planned != "" {
				t.Errorf("%s: flag-off planned_engine=%q, want empty (no native considered)", r.name, planned)
			}
		})
	}
}

// TestValueBindRoutes_NegativeNeighboursDeclineWithFlagOn is the parity fence,
// live: with the flag ON the near neighbours reach the serve func and it steps
// aside PRE-SOCKET, so BAML serves them. A regression that widened the grammar
// would show up here as a native socket.
func TestValueBindRoutes_NegativeNeighboursDeclineWithFlagOn(t *testing.T) {
	for _, r := range staticServingCorpusValueBindDeclined {
		r := r
		t.Run(r.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, valueBindResponse())
			a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
			_, _, err := driveValueBindRoute(t, a, r.name)
			server.close()
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			if got := spy.calls.Load(); got != 1 {
				t.Fatalf("%s: serve func invoked %d times, want 1 (it must RUN and decline)", r.name, got)
			}
			disp, stage, reason := spy.lastDecline()
			if disp != int64(bamlutils.NativeStaticServeDeclined) {
				t.Fatalf("%s: disposition=%d, want declined (stage=%q reason=%q)", r.name, disp, stage, reason)
			}
			// The POSITIVE half. These rows are a PROMPT-GRAMMAR fence, so the
			// decline has to come from the prompt stage. A row that lost its
			// descriptor — or declined on the client/envelope — would still be
			// "declined" with zero sockets and would pass every other check here,
			// silently retiring the grammar coverage this fence exists to provide.
			if stage != "prompt" || reason != "static_prompt_unsupported" {
				t.Errorf("%s: declined at stage=%q reason=%q, want stage=%q reason=%q (the fence must trip on the PROMPT shape)",
					r.name, stage, reason, "prompt", "static_prompt_unsupported")
			}
			if got := spy.nativeSockets(t, "on"); got != 0 {
				t.Errorf("%s: native_sockets{on}=%v, want 0 (a PRE-CLAIM decline opens none)", r.name, got)
			}
			if got := server.count.Load(); got != 1 {
				t.Errorf("%s: provider saw %d requests, want 1 (BAML serves the declined row)", r.name, got)
			}
			t.Logf("%s declined pre-socket at stage=%q reason=%q", r.name, stage, reason)
		})
	}
}

// TestValueBindStreamRoute_FlagOnClaimsNativeStream is the STATIC STREAM half of
// the positive proof: the /stream route for an enum-bound prompt over the served
// alias family claims a native stream.
func TestValueBindStreamRoute_FlagOnClaimsNativeStream(t *testing.T) {
	events := contentSSE([]string{`"`, `rouge`, `"`}, nil)
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticStreamRenderEnum(a, &fixture.StaticStreamRenderEnumInput{Color: types.ColorRED})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("stream serve func invoked %d times, want 1", got)
	}
	if tr.winner != bamlutils.NativeServeEngineNative {
		t.Fatalf("stream winner=%q, want native (the enum-bound prompt must be admitted on the stream route)", tr.winner)
	}
	// A native winner alone cannot see a native stream followed by a hidden BAML
	// resend; counting provider requests is what makes this the no-resend fence
	// the file header claims.
	if got := server.count.Load(); got != 1 {
		t.Fatalf("stream claimed native but the provider saw %d requests, want EXACTLY 1 (no hidden BAML resend)", got)
	}
}

// TestValueBindStreamRoute_FlagOffIsPureBAML pins the stream kill switch.
func TestValueBindStreamRoute_FlagOffIsPureBAML(t *testing.T) {
	events := contentSSE([]string{`"`, `rouge`, `"`}, nil)
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, false, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticStreamRenderEnum(a, &fixture.StaticStreamRenderEnumInput{Color: types.ColorRED})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if got := spy.calls.Load(); got != 0 {
		t.Errorf("flag-off stream serve func invoked %d times, want 0", got)
	}
	if tr.winner == bamlutils.NativeServeEngineNative {
		t.Errorf("flag-off stream winner=%q, want BAML", tr.winner)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("flag-off stream provider saw %d requests, want 1 (BAML serves the stream)", got)
	}
}

// TestValueBindStreamRoute_AliasNeighbourDeclines is the stream-route parity
// fence: the SAME return family and the SAME bound argument, differing only in
// the prompt expression, must decline pre-transport so BAML serves the stream.
func TestValueBindStreamRoute_AliasNeighbourDeclines(t *testing.T) {
	events := contentSSE([]string{`"`, `false`, `"`}, nil)
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticStreamAliasEq(a, &fixture.StaticStreamAliasEqInput{Color: types.ColorRED})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("stream serve func invoked %d times, want 1 (it must RUN and decline)", got)
	}
	// The callback RAN (above), so its result carries the declined-only stage/reason
	// tokens — the POSITIVE half. Without them a regression that still invoked the
	// callback but refused at the envelope/client stage would satisfy calls==1, a
	// non-native winner and one provider request, while no longer proving the
	// PROMPT-grammar refusal this fence exists for.
	disp, stage, reason := spy.lastDecline()
	if disp != int64(bamlutils.NativeStreamDeclined) {
		t.Fatalf("stream disposition=%d, want declined (stage=%q reason=%q)", disp, stage, reason)
	}
	if !spy.nativeSocketsZero() {
		t.Fatalf("display-alias neighbour opened a native stream socket (disposition=%d), want a PRE-CLAIM decline", disp)
	}
	if stage != "prompt" || reason != "static_prompt_unsupported" {
		t.Errorf("stream declined at stage=%q reason=%q, want stage=%q reason=%q (the fence must trip on the PROMPT shape)",
			stage, reason, "prompt", "static_prompt_unsupported")
	}
	if tr.winner == bamlutils.NativeServeEngineNative {
		t.Fatalf("the display-alias neighbour CLAIMED a native stream; it must decline to BAML")
	}
	// A non-native winner alone cannot see a native send followed by a fallback:
	// the winner token would still read BAML while the provider had been hit
	// twice. Counting the provider requests is what makes this a no-resend fence.
	if got := server.count.Load(); got != 1 {
		t.Fatalf("display-alias neighbour sent %d provider requests, want exactly 1 (a pre-claim decline, no hidden resend)", got)
	}
}

// TestValueBindPartitionIsExhaustive is the anti-omission for this slice: the
// served and declined Slice 7.1b corpora together account for EXACTLY the new
// fixture methods, so a route cannot be silently dropped from the live proof.
func TestValueBindPartitionIsExhaustive(t *testing.T) {
	got := map[string]bool{}
	for _, r := range staticServingCorpusValueBind {
		got[r.name] = true
	}
	for _, r := range staticServingCorpusValueBindDeclined {
		if got[r.name] {
			t.Errorf("%q is in BOTH the served and declined corpora", r.name)
		}
		got[r.name] = true
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)

	// valueBindPrefixes is the Slice 7.1b naming convention, and the SINGLE source
	// of truth for both directions below so the two can never disagree.
	valueBindPrefixes := []string{"StaticEnum", "StaticRender", "StaticStream"}
	hasValueBindPrefix := func(m string) bool {
		for _, p := range valueBindPrefixes {
			if strings.HasPrefix(m, p) {
				return true
			}
		}
		return false
	}

	// SCOPE — this fence is PREFIX-SCOPED, not whole-set exhaustive: it proves every
	// emitted method under the naming convention is claimed by exactly one of the two
	// valuebind corpora. Whole-set completeness — a route under some OTHER prefix
	// escaping every corpus — is owned by TestStaticServingCutover_CompletePartition
	// (alias_cutover_manifest_test.go), which scans all SyncMethods; duplicating that
	// union here would just give two copies to drift apart.
	for m := range introspected.SyncMethods {
		if !hasValueBindPrefix(m) {
			continue
		}
		if !got[m] {
			t.Errorf("emitted value-binding route %q is covered by neither corpus (have %v)", m, names)
		}
	}

	// ...and this is what keeps the prefix list honest, so the scan above cannot be
	// silently narrowed: a corpus entry outside the convention means a value-binding
	// family was added under a new prefix, and valueBindPrefixes must learn it or the
	// scan stops covering that family.
	for _, m := range names {
		if !hasValueBindPrefix(m) {
			t.Errorf("corpus entry %q matches no prefix in %v — add its prefix, or the emitted-route scan above silently skips its family", m, valueBindPrefixes)
		}
	}
}
