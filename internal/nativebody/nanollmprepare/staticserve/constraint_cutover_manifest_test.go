//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 7.2b-3 — the LIVE proof that the ONE admitted constraint fingerprint is
// SERVED end to end, and that its RETURN-SCHEMA siblings are not.
//
// The fixture project declares the two concrete return types the 7.2b scope admits as
// the first production-admission fingerprint:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(positive, {{ this > 0 }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(positive, {{ this > 0 }}) }
//
// so the generated client carries the real `Checked[int64]` carrier (re-pointed at
// bamlutils.Checked by the fixture transform) and the generated /call seam emits a
// `bamlutils.DecodeStaticFinal[types.StaticCheckedAnswer]` closure. That is the
// production path this slice is about — not a hand-written look-alike.
//
// It also declares TEN SIBLINGS, refused by TWO different mechanisms, which the rows
// declare individually and this test asserts separately — a row must not be able to pass
// by the wrong one:
//
//   - EIGHT reach ADMISSION. A second @check, the two fields reordered, the identical
//     shape under a different class name, an @alias on the constrained field, a different
//     predicate, and a float / list / optional constrained field each get a descriptor, a
//     projector and an installed seam exactly like the admitted pair, so their decline is
//     measured INSIDE admission rather than inferred from an un-emitted seam.
//   - TWO never get that far. The non-ASCII label and the union field are refused by the
//     DESCRIPTOR EXTRACTOR, so they carry no V3 descriptor and the seam installs nothing:
//     the serve callback is never invoked at all. That is a STRONGER guarantee than an
//     admission decline — native is unreachable rather than merely refused — and a
//     different mechanism, so it is asserted as such.
//
// "One property away" is exact for most of them, and deliberately approximate for three:
// list / union / optional necessarily change the constrained field's TYPE *and* its
// predicate, because `this > 0` is not a well-typed predicate for a list, a union or a
// null. The FLOAT row is the one that isolates the type axis alone (same label, same
// `this > 0`), and independent type-versus-predicate rejection is proven at the
// descriptor/Bundle level by internal/debaml's single-axis sibling corpus, which can vary
// one without the other because it never has to compile.
//
// # What each row asserts
//
// SERVED (flag on): the serve callback is invoked, planned_engine == "native", EXACTLY
// ONE native socket, the provider sees exactly one request, and the decoded final (or
// the returned error) is byte-compared against the FLAG-OFF full-BAML run over the same
// provider bytes. The flag-off leg is the differential's stock half: BAML alone builds,
// sends and parses, and the serve callback is never invoked at all.
//
// DECLINED (flag on): the serve callback IS invoked, planned_engine == "native", ZERO
// native sockets, the winner is not native, and exactly ONE ordinary BAML provider
// request served the call.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusConstraintServed is the FROZEN Slice 7.2b-3 partition: the routes
// whose RETURN carries the ONE admitted constraint fingerprint. Native serves them.
var staticServingCorpusConstraintServed = []staticRoute{
	{"StaticCheckedConfidence", true},
	{"StaticAssertConfidence", true},
}

// constraintSibling is one LIVE RETURN-SCHEMA sibling of the admitted fingerprint: the
// route, the property (or property PAIR — see the note on `property`) it changes, the
// provider body its shape can actually parse, and WHERE it is refused.
type constraintSibling struct {
	route string
	// property names WHAT differs from StaticCheckedAnswer, so a green row says which
	// category it covered rather than only that something declined.
	//
	// It is ONE property for most rows. It is deliberately TWO for list / union /
	// optional: `this > 0` is not a well-typed predicate for a list, a union or a null,
	// so a project changing only the type would not compile, and those rows necessarily
	// carry a type-appropriate predicate as well. What they prove live is that the family
	// is refused pre-socket — not which of the two properties refused it. The FLOAT row
	// is the single-axis type witness (same label, same `this > 0`), and independent
	// type-versus-predicate rejection is proven at the descriptor/Bundle level by
	// internal/debaml's sibling corpus, which never has to compile.
	property string
	// body is the provider response this shape parses cleanly, so BAML really SERVES the
	// declined call and the row is not passing on a parse failure.
	body []byte
	// buildTimeDeclined marks a route the DESCRIPTOR EXTRACTOR refuses, so it gets no V3
	// descriptor and the seam installs nothing: the callback never runs. That is a
	// stronger guarantee than an admission decline (native is unreachable, not merely
	// refused) and a DIFFERENT mechanism, so it is declared per row and asserted
	// separately — a row must not be able to pass by the wrong one.
	buildTimeDeclined bool
}

// constraintSiblings is the sibling half: each row differs from the fingerprint in one
// property (two, for the three rows noted on constraintSibling.property), and each is
// refused with ZERO native sockets while BAML serves. Eight are refused at ADMISSION and
// two by the DESCRIPTOR EXTRACTOR; the row declares which, and the test asserts that
// mechanism specifically.
//
// One category the scope names — a `map` -typed constrained field — is deliberately not
// here, and for a generated-client reason rather than a gate one: BAML v0.223's Go
// generator lowers a class-field map to `baml.OrderedMap[T]`, which this fixture's client
// does not carry (it is generated with --skip-baml-module-patch). The map family is
// covered at the descriptor/bundle level instead, by internal/debaml's sibling corpus and
// by nativeserve's admission sibling table, so the GATE is proven for it; only the live
// socket row is unavailable. baml_src/types.baml records the same note beside the classes.
func constraintSiblings() []constraintSibling {
	answer := func(confidence string) []byte {
		return openAIContent(`{"answer":"sunny","confidence":` + confidence + `}`)
	}
	return []constraintSibling{
		{route: "StaticCheckedTwoChecks", property: "a SECOND @check", body: answer("9")},
		{route: "StaticCheckedReordered", property: "the two fields REORDERED", body: answer("9")},
		{route: "StaticCheckedRenamedClass", property: "a DIFFERENT class name", body: answer("9")},
		{route: "StaticCheckedAliasedField", property: "an @alias on the constrained field",
			body: openAIContent(`{"answer":"sunny","score":9}`)},
		// De-BAML Slice 7.2c-3 SHARPENED this row rather than removing it, and the
		// change of label is the finding.
		//
		// It used to read "a DIFFERENT predicate (this >= 0)", and until the cutover it
		// declined for TWO reasons at once: the class is `StaticGtePredicateAnswer`
		// (not one of the two pinned names) AND `>=` was outside the manifest. The
		// cutover ADMITS `>=` — internal/nativebody/nanollmprepare/staticserve/opge
		// serves all four of its rows live, with one native socket each — so exactly
		// one reason is left, and this row is now a SINGLE-AXIS witness for the class
		// NAME.
		//
		// That is the 7.2c scope's risk 7 made measurable: "Fixture identity must not
		// become an accidental schema broadening. The existing live `>=` sibling has a
		// different class name, so a monolithic fixture project tempts an
		// implementation to unpin names rather than create isolated same-name
		// fixtures." The names were not unpinned; the isolated projects were built; and
		// this route still opens ZERO sockets while the identical predicate under the
		// pinned name serves.
		{route: "StaticCheckedGtePredicate",
			property: "a DIFFERENT class name carrying an ADMITTED predicate (StaticGtePredicateAnswer, this >= 0)",
			body:     answer("9")},
		{route: "StaticCheckedFloat", property: "a FLOAT constrained field", body: answer("9.5")},
		{route: "StaticCheckedList", property: "a LIST constrained field", body: answer("[1,2]")},
		{route: "StaticCheckedOptional", property: "an OPTIONAL constrained field", body: answer("9")},
		// The two the DESCRIPTOR EXTRACTOR refuses outright.
		{route: "StaticCheckedNonAsciiLabel", property: "a NON-ASCII constraint label",
			body: answer("9"), buildTimeDeclined: true},
		{route: "StaticCheckedUnion", property: "a UNION constrained field",
			body: answer("9"), buildTimeDeclined: true},
	}
}

// staticServingCorpusConstraintDeclined is the same set as a route list, for the
// partition guard.
var staticServingCorpusConstraintDeclined = func() []staticRoute {
	var out []staticRoute
	for _, s := range constraintSiblings() {
		out = append(out, staticRoute{s.route, true})
	}
	return out
}()

// constraintRoutes is every route in both halves, which is what the partition guard in
// alias_cutover_manifest_test.go consumes.
func constraintRoutes() []staticRoute {
	return append(append([]staticRoute(nil), staticServingCorpusConstraintServed...),
		staticServingCorpusConstraintDeclined...)
}

// constraintOutcome is one drained /call: the marshalled final, the outcome tokens and
// any error.
//
// The final is marshalled INSIDE the drain loop, before the result is released back to
// its pool, so the bytes compared later cannot be a view of a recycled struct, and it
// uses the WORKER's serializer so they are the bytes a caller receives.
type constraintOutcome struct {
	finalJSON       string
	winner, planned string
	err             error
}

// driveConstraintRoute invokes the generated /call for one constraint route and drains
// the stream. A separate switch keeps the frozen sibling corpora untouched.
func driveConstraintRoute(t *testing.T, a bamlutils.Adapter, name string) constraintOutcome {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticCheckedConfidence":
		ch, err = fixture.StaticCheckedConfidence(a, &fixture.StaticCheckedConfidenceInput{Topic: "weather"})
	case "StaticAssertConfidence":
		ch, err = fixture.StaticAssertConfidence(a, &fixture.StaticAssertConfidenceInput{Topic: "weather"})
	case "StaticCheckedTwoChecks":
		ch, err = fixture.StaticCheckedTwoChecks(a, &fixture.StaticCheckedTwoChecksInput{Topic: "weather"})
	case "StaticCheckedReordered":
		ch, err = fixture.StaticCheckedReordered(a, &fixture.StaticCheckedReorderedInput{Topic: "weather"})
	case "StaticCheckedRenamedClass":
		ch, err = fixture.StaticCheckedRenamedClass(a, &fixture.StaticCheckedRenamedClassInput{Topic: "weather"})
	case "StaticCheckedAliasedField":
		ch, err = fixture.StaticCheckedAliasedField(a, &fixture.StaticCheckedAliasedFieldInput{Topic: "weather"})
	case "StaticCheckedGtePredicate":
		ch, err = fixture.StaticCheckedGtePredicate(a, &fixture.StaticCheckedGtePredicateInput{Topic: "weather"})
	case "StaticCheckedFloat":
		ch, err = fixture.StaticCheckedFloat(a, &fixture.StaticCheckedFloatInput{Topic: "weather"})
	case "StaticCheckedList":
		ch, err = fixture.StaticCheckedList(a, &fixture.StaticCheckedListInput{Topic: "weather"})
	case "StaticCheckedOptional":
		ch, err = fixture.StaticCheckedOptional(a, &fixture.StaticCheckedOptionalInput{Topic: "weather"})
	case "StaticCheckedNonAsciiLabel":
		ch, err = fixture.StaticCheckedNonAsciiLabel(a, &fixture.StaticCheckedNonAsciiLabelInput{Topic: "weather"})
	case "StaticCheckedUnion":
		ch, err = fixture.StaticCheckedUnion(a, &fixture.StaticCheckedUnionInput{Topic: "weather"})
	default:
		t.Fatalf("driveConstraintRoute: unknown route %q", name)
	}
	if err != nil {
		return constraintOutcome{err: err}
	}
	out := constraintOutcome{}
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			if f := r.Final(); f != nil {
				// sonic, NOT encoding/json: sonic is the WORKER's serializer
				// (worker/parse.go), so these are the bytes a caller actually
				// receives — and the ones internal/debaml/checkedwire's stock
				// captures are in. encoding/json would HTML-escape the `>` inside
				// the carrier's `expression`, silently comparing a different string
				// against the capture.
				b, merr := sonic.Marshal(f)
				if merr != nil {
					t.Fatalf("%s: marshal final: %v", name, merr)
				}
				out.finalJSON = string(b)
			}
		case bamlutils.StreamResultKindError:
			out.err = r.Error()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				out.winner = md.WinnerEngine
				out.planned = md.PlannedEngine
			}
		}
		r.Release()
	}
	return out
}

// constraintLiveRow is one of the FOUR serving-shaped outcomes of the two admitted
// fixtures — the same four the #665 companion rows name, driven here through the real
// generated /call over a loopback provider.
type constraintLiveRow struct {
	name       string
	route      string
	confidence int
	// wantErr records that this row's predicate is a FALSE @assert, which stock
	// rejects with no value at all. The exact bytes are not restated here: the
	// flag-off leg supplies them, and equality with it is the assertion.
	wantErr bool
}

func constraintLiveRows() []constraintLiveRow {
	return []constraintLiveRow{
		{name: "check_pass", route: "StaticCheckedConfidence", confidence: 9},
		{name: "check_fail", route: "StaticCheckedConfidence", confidence: -1},
		{name: "assert_pass", route: "StaticAssertConfidence", confidence: 9},
		{name: "assert_fail", route: "StaticAssertConfidence", confidence: -1, wantErr: true},
	}
}

// TestConstraintRoutes_SeamIsEmittedAndDescriptored is the build-time half, and it is
// what makes the live results below mean something.
//
// If a route carried no descriptor (the MEDIA situation) its zeros would witness an
// un-emitted seam rather than an admission decision. They ALL carry one, with a
// projector and no build-time decline, so admission is the only thing left to decide
// them — served or declined.
func TestConstraintRoutes_SeamIsEmittedAndDescriptored(t *testing.T) {
	buildTime := map[string]bool{}
	for _, s := range constraintSiblings() {
		buildTime[s.route] = s.buildTimeDeclined
	}
	reachesAdmission := 0
	for _, r := range constraintRoutes() {
		_, hasDescriptor := introspected.StaticPromptDescriptor(r.name)
		reason, declined := introspected.StaticPromptDeclines[r.name]
		if buildTime[r.name] {
			// The DESCRIPTOR EXTRACTOR refuses it, so there is no descriptor and the
			// seam installs nothing. The REASON is required: a route that lost its
			// descriptor for some unrelated reason would otherwise look the same.
			if hasDescriptor {
				t.Errorf("%s: a V3 descriptor WAS emitted, but this row is declared build-time "+
					"declined; the live assertion for it would be about the wrong mechanism", r.name)
			}
			if !declined {
				t.Errorf("%s: no build-time decline was recorded, but this row is declared "+
					"build-time declined", r.name)
			} else if !strings.Contains(reason, "prompt descriptor return bundle unavailable") {
				t.Errorf("%s: build-time decline reason %q is not a RETURN-BUNDLE refusal; the row "+
					"claims its RETURN is what stopped it", r.name, reason)
			}
			continue
		}
		if !hasDescriptor {
			t.Errorf("%s: NO V3 descriptor was emitted; its live result would witness an un-emitted "+
				"seam rather than an admission decision", r.name)
		}
		if _, ok := introspected.StaticPromptArgumentProjectors[r.name]; !ok {
			t.Errorf("%s: no argument projector was emitted", r.name)
		}
		if declined {
			t.Errorf("%s: a BUILD-TIME decline was recorded (%q); this route must reach admission",
				r.name, reason)
		}
		reachesAdmission++
	}
	// Most of the corpus must reach admission, or the live proof would be mostly about
	// routes native can never see.
	if reachesAdmission < len(constraintRoutes())-2 {
		t.Fatalf("only %d of %d constraint routes reach admission; the zero-socket proof would be "+
			"dominated by un-emitted seams", reachesAdmission, len(constraintRoutes()))
	}
}

// TestConstraintRoutes_FlagOnServesNative is the LIVE admission proof and the live half
// of the byte differential.
//
// For each of the four serving-shaped outcomes it runs the SAME route over the SAME
// provider bytes twice — flag ON (native serves) and flag OFF (full BAML) — and requires
// the two to produce the SAME bytes, or the SAME error text. The flag-off leg is not a
// convenience: it is the stock half of the differential, produced by BAML alone through
// the unchanged route, and it is what the native bytes are compared against.
func TestConstraintRoutes_FlagOnServesNative(t *testing.T) {
	for _, row := range constraintLiveRows() {
		t.Run(row.name, func(t *testing.T) {
			body := openAIStaticAnswer("sunny", row.confidence)

			// ---- FLAG OFF: the unchanged full-BAML route, no native call at all ----
			offSpy := newStaticServeSpy(t)
			offServer := newFixtureServer(t, http.StatusOK, body)
			off := driveConstraintRoute(t, buildFixtureAdapter(t, offSpy, false, bamlutils.StreamModeCall), row.route)
			offServer.close()
			if got := offSpy.calls.Load(); got != 0 {
				t.Fatalf("flag OFF invoked the serve func %d times, want 0 (hard-off); this leg is the "+
					"stock half of the differential and must not involve native at all", got)
			}
			if got := offSpy.nativeSockets(t, "on"); got != 0 {
				t.Fatalf("flag OFF opened %v native sockets, want 0", got)
			}
			if got := offServer.count.Load(); got != 1 {
				t.Fatalf("flag OFF: provider saw %d requests, want exactly 1 (BAML serves)", got)
			}
			if off.planned != "" {
				t.Errorf("flag OFF planned_engine = %q, want empty", off.planned)
			}

			// ---- FLAG ON: native admits, opens ONE socket and serves ----
			onSpy := newStaticServeSpy(t)
			onServer := newFixtureServer(t, http.StatusOK, body)
			on := driveConstraintRoute(t, buildFixtureAdapter(t, onSpy, true, bamlutils.StreamModeCall), row.route)
			onServer.close()
			if got := onSpy.calls.Load(); got != 1 {
				t.Fatalf("the native serve func was invoked %d times, want exactly 1", got)
			}
			// planned_engine rides the OUTCOME metadata, which a claimed FAILURE never
			// reaches (the error short-circuits the emission) — for that row the socket
			// count below is what proves native claimed the attempt, and it is a stronger
			// witness than a metadata token anyway.
			if !row.wantErr && on.planned != "native" {
				t.Errorf("planned_engine = %q, want native", on.planned)
			}
			disp, stage, reason := onSpy.lastDecline()
			if got := onSpy.nativeSockets(t, "on"); got != 1 {
				t.Fatalf("native_sockets{flag=on} = %v, want 1 (the admitted fingerprint must CLAIM a "+
					"socket); serve disposition=%d stage=%q reason=%q", got, disp, stage, reason)
			}
			if got := onServer.count.Load(); got != 1 {
				t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
			}

			// ---- The differential itself ----
			if row.wantErr {
				if on.err == nil {
					t.Fatalf("a FALSE @assert SERVED %s; stock emits no value at all", on.finalJSON)
				}
				if off.err == nil {
					t.Fatalf("the flag-off (BAML) leg did not reject the FALSE @assert; it returned %s, so "+
						"the comparison below would have no stock half", off.finalJSON)
				}
				if on.err.Error() != off.err.Error() {
					t.Fatalf("native and BAML disagree on the assertion error bytes:\n native %q\n BAML   %q",
						on.err.Error(), off.err.Error())
				}
				if on.finalJSON != "" {
					t.Fatalf("the failed assert produced BOTH an error and a final: %s", on.finalJSON)
				}
				// A CLAIMED failure, not a hidden resend: exactly one provider request
				// was already asserted above, and the winner must not be native's success
				// token.
				if on.winner == bamlutils.NativeStaticServeEngineNative {
					t.Errorf("winner_engine = %q on a claimed parse failure", on.winner)
				}
				return
			}

			if on.err != nil {
				t.Fatalf("the admitted route errored: %v", on.err)
			}
			if off.err != nil {
				t.Fatalf("the flag-off (BAML) leg errored: %v; the comparison would have no stock half", off.err)
			}
			if on.finalJSON == "" {
				t.Fatal("the admitted route produced no final")
			}
			if on.finalJSON != off.finalJSON {
				t.Fatalf("native and BAML disagree on the served bytes:\n native %s\n BAML   %s",
					on.finalJSON, off.finalJSON)
			}
			// NATIVE really produced them. `native_baml_parse` would mean native claimed
			// the socket and then fell back to BAML's parse of the same bytes — a real
			// outcome, and not the one this row is about.
			if on.winner != bamlutils.NativeStaticServeEngineNative {
				t.Errorf("winner_engine = %q, want %q (a structured native win)",
					on.winner, bamlutils.NativeStaticServeEngineNative)
			}
			t.Logf("%s: native == BAML == %s", row.name, on.finalJSON)
		})
	}
}

// TestConstraintRoutes_ServedBytesAreTheStockCaptures is the second half of the byte
// proof: the LIVE served bytes are the ones internal/debaml/checkedwire captured from
// the real BAML v0.223.0 CFFI, restated here as literals.
//
// The flag-on/flag-off equality above proves native and BAML agree in this process. This
// proves what they agree ON is what stock produced in a completely different harness —
// so an error that moved BOTH legs (a changed carrier, a changed field order) is caught
// rather than cancelling out.
func TestConstraintRoutes_ServedBytesAreTheStockCaptures(t *testing.T) {
	// Captured by internal/debaml/checkedwire from the real CFFI
	// (wireNestedCheck / wireNestedCheckFail / wireNestedAssertPass).
	for _, tc := range []struct {
		name       string
		route      string
		confidence int
		want       string
	}{{
		"check_pass", "StaticCheckedConfidence", 9,
		`{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`,
	}, {
		"check_fail", "StaticCheckedConfidence", -1,
		`{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`,
	}, {
		"assert_pass", "StaticAssertConfidence", 9,
		`{"answer":"sunny","confidence":9}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", tc.confidence))
			got := driveConstraintRoute(t, buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall), tc.route)
			server.close()
			if got.err != nil {
				t.Fatalf("route errored: %v", got.err)
			}
			if got.winner != bamlutils.NativeStaticServeEngineNative {
				t.Fatalf("winner_engine = %q; these bytes must be NATIVE's", got.winner)
			}
			if got.finalJSON != tc.want {
				t.Fatalf("served bytes:\n got %s\nwant %s (the stock CFFI capture)", got.finalJSON, tc.want)
			}
		})
	}
}

// TestConstraintRoutes_SiblingsDeclinePreSocket is the live sibling guard: a route whose
// RETURN SCHEMA differs from the admitted fingerprint opens ZERO native sockets and is
// served by BAML — for EVERY category the scope leaves declined, named per row.
//
// Most rows differ in exactly one property. The list, union and optional rows differ in
// TWO (type and predicate together), because `this > 0` is not well-typed for a list, a
// union or a null; the float row is the single-axis type witness. What every row proves
// is the same thing — the family is refused BEFORE transport — and for the paired rows
// that is all it proves: WHICH axis refused them is established at the descriptor/Bundle
// level by internal/debaml's single-axis corpus.
func TestConstraintRoutes_SiblingsDeclinePreSocket(t *testing.T) {
	// NON-VACUITY CONTROL, in the SAME adapter configuration the rows below use: an
	// ADMITTED constraint route reaches native and claims EXACTLY ONE socket. Without it,
	// every zero below would be satisfied by a harness that never reaches native at all.
	t.Run("control_admitted_constraint_route_claims_a_socket", func(t *testing.T) {
		spy := newStaticServeSpy(t)
		server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
		got := driveConstraintRoute(t, buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall), "StaticCheckedConfidence")
		server.close()
		if got.err != nil {
			t.Fatalf("control route: %v", got.err)
		}
		if calls := spy.calls.Load(); calls != 1 {
			t.Fatalf("control route invoked the serve func %d times, want 1; the rows below would be vacuous", calls)
		}
		if got.planned != "native" || got.winner != bamlutils.NativeStaticServeEngineNative {
			t.Fatalf("control route planned=%q winner=%q, want both native; the rows below would be vacuous",
				got.planned, got.winner)
		}
		if n := spy.nativeSockets(t, "on"); n != 1 {
			t.Fatalf("control route native_sockets{on}=%v, want EXACTLY 1; the zero-socket assertions "+
				"below could not distinguish a decline from a harness that never opens one", n)
		}
	})

	siblings := constraintSiblings()
	// Every category the scope names must be represented, checked by NAME rather than by
	// count: a row that was renamed or dropped fails here instead of silently shrinking
	// the guard.
	wantCategories := []string{
		"a SECOND @check", "the two fields REORDERED", "a DIFFERENT class name",
		"an @alias on the constrained field",
		"a DIFFERENT class name carrying an ADMITTED predicate (StaticGtePredicateAnswer, this >= 0)",
		"a FLOAT constrained field", "a LIST constrained field",
		"an OPTIONAL constrained field", "a NON-ASCII constraint label",
		"a UNION constrained field",
	}
	have := map[string]bool{}
	for _, s := range siblings {
		have[s.property] = true
	}
	for _, want := range wantCategories {
		if !have[want] {
			t.Fatalf("no live sibling covers %q; the zero-socket proof would leave that category "+
				"untested against a real generated route", want)
		}
	}

	for _, sib := range siblings {
		t.Run(sib.route, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, http.StatusOK, sib.body)
			got := driveConstraintRoute(t, buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall), sib.route)
			server.close()
			if got.err != nil {
				t.Fatalf("%s (%s): %v", sib.route, sib.property, got.err)
			}

			if sib.buildTimeDeclined {
				// No descriptor, so the seam installs NOTHING and the callback never
				// runs. Asserted as its own mechanism: counting this as an admission
				// decline would let a route that lost its descriptor pass for one that
				// admission refused.
				if calls := spy.calls.Load(); calls != 0 {
					t.Errorf("%s: native serve func invoked %d times, want 0 (no descriptor, no seam)",
						sib.route, calls)
				}
				if got.planned == "native" {
					t.Errorf("%s: planned_engine=native, but no seam is installed", sib.route)
				}
			} else {
				// The seam IS installed and the callback DOES run: this is a decline
				// inside admission, not an absent seam.
				if calls := spy.calls.Load(); calls != 1 {
					t.Errorf("%s: native serve func invoked %d times, want 1 (the seam is installed; the "+
						"sibling is refused INSIDE it)", sib.route, calls)
				}
				if got.planned != "native" {
					t.Errorf("%s: planned_engine=%q, want native (native was considered)", sib.route, got.planned)
				}
				// …and it refuses BEFORE transport, at the stage the fingerprint owns.
				disp, stage, reason := spy.lastDecline()
				if disp != int64(bamlutils.NativeStaticServeDeclined) {
					t.Errorf("%s: serve disposition=%d, want declined", sib.route, disp)
				}
				if stage != "prompt" {
					t.Errorf("%s: declined at stage %q, want %q (PRE-SOCKET)", sib.route, stage, "prompt")
				}
				if reason == "" {
					t.Errorf("%s: declined with no reason token", sib.route)
				}
			}

			// BOTH mechanisms share the property this test exists for.
			if n := spy.nativeSockets(t, "on"); n != 0 {
				t.Errorf("%s (%s): native_sockets{on}=%v, want 0 — a return-schema sibling must never "+
					"reach transport", sib.route, sib.property, n)
			}
			if got.winner == bamlutils.NativeStaticServeEngineNative {
				t.Errorf("%s: winner_engine=%q, want BAML", sib.route, got.winner)
			}
			if n := server.count.Load(); n != 1 {
				t.Errorf("%s: provider saw %d requests, want 1 (one ordinary BAML request)", sib.route, n)
			}
			// BAML really supplied a result, rather than the route quietly producing
			// nothing at all.
			if got.finalJSON == "" {
				t.Errorf("%s: the declined route produced no final; BAML must supply the result", sib.route)
			}
		})
	}
}

// TestConstraintRoutes_StreamIsAZeroSocketDecline is the /stream half of the boundary,
// live.
//
// The scope keeps `/stream` a ZERO-SOCKET decline for the admitted fingerprint, and until
// now that was only asserted at the predicate. This drives the EXACT admitted route —
// StaticCheckedConfidence, the one the /call lane serves natively in this same file — in
// StreamModeStream over a real SSE provider, and requires native to open no socket and
// BAML to produce the stream.
func TestConstraintRoutes_StreamIsAZeroSocketDecline(t *testing.T) {
	// NON-VACUITY CONTROL: an ADMITTED STREAM route claims its socket in this harness,
	// so the zeros below are the constraint's doing rather than a stream harness that
	// never reaches native.
	t.Run("control_admitted_stream_route_claims_native", func(t *testing.T) {
		events := contentSSE([]string{"[1,", `"x",`, "true]"}, nil)
		spy := newStreamServeSpy(t)
		server := newFixtureStreamServer(t, events)
		a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
		ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
		tr := drainStreamTrace(t, ch, err)
		server.close()
		if tr.drainer != nil {
			t.Fatalf("control stream route: %v", tr.drainer)
		}
		if tr.planned != "native" || tr.winner != bamlutils.NativeServeEngineNative {
			t.Fatalf("control stream planned=%q winner=%q, want native; the rows below would be vacuous",
				tr.planned, tr.winner)
		}
		if spy.nativeSocketsZero() {
			t.Fatal("the control stream route opened ZERO native sockets; the zero-socket assertions " +
				"below could not distinguish a decline from a harness that never opens one")
		}
	})

	for _, route := range []string{"StaticCheckedConfidence", "StaticAssertConfidence"} {
		t.Run(route, func(t *testing.T) {
			events := contentSSE([]string{`{"answer":`, `"sunny",`, `"confidence":9}`}, nil)
			spy := newStreamServeSpy(t)
			server := newFixtureStreamServer(t, events)
			a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)

			var ch <-chan bamlutils.StreamResult
			var err error
			switch route {
			case "StaticCheckedConfidence":
				ch, err = fixture.StaticCheckedConfidence(a, &fixture.StaticCheckedConfidenceInput{Topic: "weather"})
			case "StaticAssertConfidence":
				ch, err = fixture.StaticAssertConfidence(a, &fixture.StaticAssertConfidenceInput{Topic: "weather"})
			}
			tr := drainStreamTrace(t, ch, err)
			server.close()

			if tr.drainer != nil {
				t.Fatalf("%s stream: %v", route, tr.drainer)
			}
			// ZERO native sockets: either the seam declined pre-transport or it was never
			// installed. Both are the required outcome; neither may be a claim.
			disp, stage, reason := spy.lastDecline()
			if !spy.nativeSocketsZero() {
				t.Fatalf("%s: the /stream lane CLAIMED a native socket (disposition=%d stage=%q "+
					"reason=%q); the constraint fingerprint is a zero-socket decline on /stream",
					route, disp, stage, reason)
			}
			// The MECHANISM is logged rather than pinned, deliberately. This shape has
			// TWO independent pre-socket defences — nativeserve's own stream return-shape
			// gate, which is narrower than the final one and refuses the class outright,
			// and the root-owned stream ROUTE boundary this slice added — and either is
			// sufficient. Pinning one would make the test go red on a change that is
			// still correct; the OUTCOME the scope requires is the zero socket, and that
			// is what is asserted. The root boundary is attributed on its own terms by
			// internal/debaml's TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed,
			// which drives SupportsNativeStreamBundle directly and fails if it is removed.
			t.Logf("%s: /stream zero-socket (serve callback invoked %d time(s), disposition=%d "+
				"stage=%q reason=%q)", route, spy.calls.Load(), disp, stage, reason)
			if tr.winner == bamlutils.NativeServeEngineNative {
				t.Errorf("%s: stream winner_engine=%q, must NOT be native", route, tr.winner)
			}
			// BAML served it: exactly one provider request, and a final arrived.
			if n := server.count.Load(); n != 1 {
				t.Errorf("%s: provider saw %d stream requests, want 1 (BAML serves)", route, n)
			}
			var final string
			for _, e := range tr.events {
				if e.kind == bamlutils.StreamResultKindFinal {
					final = e.final
				}
			}
			if final == "" {
				t.Errorf("%s: the declined stream produced no final; BAML must supply it", route)
			}
		})
	}
}
