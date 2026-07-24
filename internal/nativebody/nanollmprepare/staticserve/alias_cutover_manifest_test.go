//go:build integration && nanollm_integration

// De-BAML Phase 3a (recursive ALIASES) — the ALIAS-ONLY static-serving cutover
// manifest, a sibling of the recursive-class manifest (recursive_cutover_manifest_test.go).
//
// It proves the alias partition end-to-end over the generated /call loopback WITHOUT
// touching the frozen legacy 8C 5/5/5/5/5/2/3/1 pin or the recursive-class partition:
//
//   - staticServingCorpusAlias: the ONE fingerprint-admitted served route
//     (StaticRecursiveAliasJSON — the direct five-arm JSON alias). Each row plan-matches
//     (planned=native), claims EXACTLY ONE native socket, never resends, serves the
//     NATIVE winner, and decodes to the byte-exact sorted-public FinalJSON.
//   - staticServingCorpusAliasDeclined: StaticRecursiveAliasJsonValue (the wider #583
//     float+null alias), which the served fingerprint DECLINES pre-transport — ZERO
//     native sockets, BAML serves.
//   - a full partition-completeness anti-omission: legacy ∪ recursive-served ∪
//     recursive-declined ∪ alias-served ∪ alias-declined == the fixture's full emitted
//     SyncMethods (no method silently unaccounted).

package staticserve

import (
	"sort"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusAlias is the FROZEN alias SERVED corpus (exactly the direct
// five-arm JSON alias).
var staticServingCorpusAlias = []staticRoute{
	{"StaticRecursiveAliasJSON", true},
}

// staticServingCorpusAliasDeclined is the FROZEN alias DECLINED corpus: the wider
// JsonValue (float + explicit-null arms, non-JSON name) which the served fingerprint
// declines, so it falls back to BAML with ZERO native sockets.
var staticServingCorpusAliasDeclined = []staticRoute{
	{"StaticRecursiveAliasJsonValue", false},
}

// aliasRow pairs an assistant content with the byte-exact SORTED-public FinalJSON the
// native serve must produce (json.Marshal canonical: sorted map keys + HTML escaping;
// null -> [] the list fallback; float -> int; numeric string stays string).
type aliasRow struct {
	name    string
	content string
	want    string
}

func aliasManifestRows() []aliasRow {
	return []aliasRow{
		{"int", `42`, `42`},
		{"string", `"hi"`, `"hi"`},
		{"numeric_string", `"1"`, `"1"`},
		{"bool", `true`, `true`},
		{"float_to_int", `1.5`, `2`},
		{"empty_list", `[]`, `[]`},
		{"empty_map", `{}`, `{}`},
		{"list_terminals", `[1,"1",true]`, `[1,"1",true]`},
		{"map_sorted", `{"z":1,"a":2}`, `{"a":2,"z":1}`},
		{"map_overwrite", `{"z":1,"a":2,"z":3}`, `{"a":2,"z":3}`},
		{"null_to_empty_list", `null`, `[]`},
		{"map_value_null", `{"n":null,"a":[1,2]}`, `{"a":[1,2],"n":[]}`},
		{"nested_alternating", `[{"a":[1]},{"b":["x"]}]`, `[{"a":[1]},{"b":["x"]}]`},
	}
}

// TestStaticServingCutoverManifest_Alias drives the served JSON alias route over the
// generated /call for every manifest row and asserts plan parity, one-send (no resend),
// a claimed native socket, the native winner, and the byte-exact sorted-public tree.
func TestStaticServingCutoverManifest_Alias(t *testing.T) {
	rows := aliasManifestRows()
	planMatched, oneSend, claimed, nativeWin, treeExact := 0, 0, 0, 0, 0

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			spy := newStaticServeSpy(t)
			server := newFixtureServer(t, 200, openAIContent(row.content))
			a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
			final, winner, planned, err := driveAliasRoute(t, a, "StaticRecursiveAliasJSON")
			server.close()
			if err != nil {
				t.Fatalf("serve %s: %v", row.name, err)
			}
			if planned == "native" {
				planMatched++
			} else {
				t.Errorf("%s: planned_engine=%q, want native (plan parity)", row.name, planned)
			}
			if server.count.Load() == 1 {
				oneSend++
			} else {
				t.Errorf("%s: provider saw %d requests, want 1 (one-send, no resend)", row.name, server.count.Load())
			}
			if spy.nativeSocketsRaw() == 1 {
				claimed++
			} else {
				t.Errorf("%s: native sockets=%v, want 1 (claimed)", row.name, spy.nativeSocketsRaw())
			}
			if winner == bamlutils.NativeStaticServeEngineNative {
				nativeWin++
			} else {
				t.Errorf("%s: winner=%q, want native (raw/type/order all pass)", row.name, winner)
			}
			if got := jsonOf(t, final); got == row.want {
				treeExact++
			} else {
				t.Errorf("%s: sorted-public tree mismatch:\n got:  %s\n want: %s", row.name, got, row.want)
			}
		})
	}

	n := len(rows)
	pin := map[string]int{"rows": len(rows), "plan-matched": planMatched, "one-send": oneSend, "claimed": claimed, "native-win": nativeWin, "tree-exact": treeExact}
	want := map[string]int{"rows": n, "plan-matched": n, "one-send": n, "claimed": n, "native-win": n, "tree-exact": n}
	for k, w := range want {
		if pin[k] != w {
			t.Errorf("alias serve matrix pin %s = %d, want %d", k, pin[k], w)
		}
	}
	t.Logf("alias serve matrix pin: %+v", pin)
}

// TestStaticServingCutover_AliasJsonValueDeclines is the wider-JsonValue zero-socket
// ROUTE regression: the served fingerprint declines JsonValue (float + null arms,
// non-JSON name) PRE-TRANSPORT, so the generated /call opens ZERO native sockets and
// BAML serves the single request. The serve callback IS invoked (flag on) but declines
// before claiming a socket.
func TestStaticServingCutover_AliasJsonValueDeclines(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, 200, openAIContent(`null`))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
	_, winner, _, err := driveAliasRoute(t, a, "StaticRecursiveAliasJsonValue")
	if err != nil {
		t.Fatalf("jsonvalue /call: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("jsonvalue: serve func invoked %d times, want 1 (flag on: callback runs then declines)", got)
	}
	if got := spy.nativeSocketsRaw(); got != 0 {
		t.Fatalf("jsonvalue: native sockets=%v, want 0 (fingerprint declines PRE-TRANSPORT)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("jsonvalue: provider saw %d requests, want 1 (BAML serves the declined route)", got)
	}
	if winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("jsonvalue: winner=%q, must NOT be native (JsonValue declines pre-claim)", winner)
	}
}

// TestStaticServingCutover_AliasFlagOff is the de-BAML-OFF identity for the served
// alias route: with the umbrella flag OFF the generated /call installs NO native
// attempt, so the serve callback never runs, ZERO native sockets open, and BAML serves
// the single request — byte-identical to a client generated before Phase 3a. This is
// the flag-off lane the Phase-2 embed lesson (cmd/embed on new root files) protects.
func TestStaticServingCutover_AliasFlagOff(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, 200, openAIContent(`{"z":1,"a":2}`))
	defer server.close()
	a := buildFixtureAdapter(t, spy, false /* flag OFF */, bamlutils.StreamModeCall)
	_, winner, planned, err := driveAliasRoute(t, a, "StaticRecursiveAliasJSON")
	if err != nil {
		t.Fatalf("flag-off alias /call: %v", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("flag-off: serve func invoked %d times, want 0 (no native attempt installed)", got)
	}
	if got := spy.nativeSocketsRaw(); got != 0 {
		t.Errorf("flag-off: native sockets=%v, want 0 (BAML serves)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("flag-off: provider saw %d requests, want 1", got)
	}
	if planned == "native" {
		t.Errorf("flag-off: planned_engine=%q, must NOT be native", planned)
	}
	if winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("flag-off: winner=%q, must NOT be native", winner)
	}
}

// TestStaticServingCutover_AliasPartition is the alias anti-omission + the FULL
// partition-completeness: the alias served corpus is EXACTLY {StaticRecursiveAliasJSON},
// and legacy ∪ recursive-served ∪ recursive-declined ∪ alias-served ∪ alias-declined ==
// the fixture's full emitted SyncMethods.
func TestStaticServingCutover_AliasPartition(t *testing.T) {
	wantAlias := []string{"StaticRecursiveAliasJSON"}
	gotAlias := make([]string, 0, len(staticServingCorpusAlias))
	for _, r := range staticServingCorpusAlias {
		gotAlias = append(gotAlias, r.name)
	}
	sort.Strings(wantAlias)
	sort.Strings(gotAlias)
	if strings.Join(wantAlias, ",") != strings.Join(gotAlias, ",") {
		t.Fatalf("alias served corpus %v != frozen %v", gotAlias, wantAlias)
	}

	partition := map[string]bool{}
	for _, m := range legacyStaticMethods {
		partition[m] = true
	}
	for _, r := range staticServingCorpusRecursive {
		partition[r.name] = true
	}
	for _, r := range staticServingCorpusDeclined {
		partition[r.name] = true
	}
	for _, r := range staticServingCorpusAlias {
		partition[r.name] = true
	}
	for _, r := range staticServingCorpusAliasDeclined {
		partition[r.name] = true
	}
	if len(partition) != len(introspected.SyncMethods) {
		t.Fatalf("partition covers %d methods but SyncMethods has %d", len(partition), len(introspected.SyncMethods))
	}
	for m := range introspected.SyncMethods {
		if !partition[m] {
			t.Errorf("emitted method %q is not covered by any partition (legacy/recursive/alias/declined)", m)
		}
	}
}

// driveAliasRoute invokes the generated /call for one alias route and drains the
// stream, returning the final decoded union + winner/planned tokens.
func driveAliasRoute(t *testing.T, a bamlutils.Adapter, name string) (final any, winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticRecursiveAliasJSON":
		ch, err = fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	case "StaticRecursiveAliasJsonValue":
		ch, err = fixture.StaticRecursiveAliasJsonValue(a, &fixture.StaticRecursiveAliasJsonValueInput{Topic: "wide json"})
	default:
		t.Fatalf("unknown alias route %q", name)
	}
	if err != nil {
		return nil, "", "", err
	}
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			final = r.Final()
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
	return final, winner, planned, drainErr
}
