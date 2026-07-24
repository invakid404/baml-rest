//go:build integration && nanollm_integration

package staticserve

// De-BAML Phase 2 (recursive classes) static-serving MANIFEST — the route-registry
// PARTITION companion to the legacy 8C cutover manifest.
//
// The 8C corpus (staticServingCorpus, 5 methods) and its frozen 5/5/5/5/5/2/3/1 pin
// (TestStaticServingCutoverManifest) + its independent anti-omission
// (TestStaticServingCutover_AntiOmission) are left byte-for-byte UNCHANGED. This file
// adds the recursive partition:
//
//   - staticServingCorpusRecursive: the three fingerprint-admitted served routes (self
//     Node, mutual A/B both roots) — each serves NATIVE structured over the FULL
//     endpoint matrix (depths 0/1/2/N × explicit-null/omitted terminals), asserting
//     plan parity, exactly one send / no resend, native winner, and the EXACT concrete
//     Node/A/B pointer tree.
//   - a Unicode `/call` route row proving native still WINS for HTML-metacharacter
//     values (the escaping-canonicalized comparator), not a silent BAML-parse fallback.
//   - staticServingCorpusDeclined: the one-field Loop route, which the served
//     fingerprint DECLINES pre-transport (ZERO native sockets, BAML serves).
//   - a partition-completeness anti-omission: legacy ∪ recursive ∪ declined ==
//     SyncMethods.

import (
	stdjson "encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

// staticServingCorpusRecursive is the FROZEN recursive SERVED corpus.
var staticServingCorpusRecursive = []staticRoute{
	{"StaticRecursiveNode", true},
	{"StaticRecursiveA", true},
	{"StaticRecursiveB", true},
}

// staticServingCorpusDeclined is the FROZEN recursive DECLINED corpus: routes that are
// emitted (they are static methods) but that the served fingerprint declines, so they
// fall back to BAML with ZERO native sockets. StaticRecursiveLoop is the one-field
// implied-recursion shape; StaticRecursiveNodeAnn carries a preserved @stream.done
// annotation on its recursive class (the stream-annotation zero-socket regression).
var staticServingCorpusDeclined = []staticRoute{
	{"StaticRecursiveLoop", false},
	{"StaticRecursiveNodeAnn", false},
}

// recChainNode builds a Node chain of the given depth (edge count) with terminal
// encoding; recChainAB builds a mutual A<->B chain rooted at "A"/"B".
func recChainNode(depth int, explicitNull bool) string {
	var b func(i int) string
	b = func(i int) string {
		v := `{"value":"n` + strconv.Itoa(i) + `"`
		if i == depth {
			if explicitNull {
				return v + `,"next":null}`
			}
			return v + `}`
		}
		return v + `,"next":` + b(i+1) + `}`
	}
	return b(0)
}

func recChainAB(root string, depth int, explicitNull bool) string {
	var b func(i int, typ string) string
	b = func(i int, typ string) string {
		edge, child := "b", "B"
		if typ == "B" {
			edge, child = "a", "A"
		}
		v := `{"value":"` + strings.ToLower(typ) + strconv.Itoa(i) + `"`
		if i == depth {
			if explicitNull {
				return v + `,"` + edge + `":null}`
			}
			return v + `}`
		}
		return v + `,"` + edge + `":` + b(i+1, child) + `}`
	}
	return b(0, root)
}

// recResponse is the raw assistant content for a row; recExpectedTree is the canonical
// SERVED bytes (native normalizes an omitted terminal to an explicit null, so the
// expected tree is always the explicit-null-terminated chain).
func recResponse(root string, depth int, explicitNull bool) string {
	if root == "Node" {
		return recChainNode(depth, explicitNull)
	}
	return recChainAB(root, depth, explicitNull)
}

func recExpectedTree(root string, depth int) string {
	if root == "Node" {
		return recChainNode(depth, true)
	}
	return recChainAB(root, depth, true)
}

// openAIContent wraps arbitrary assistant content in an OpenAI-shaped 2xx.
func openAIContent(content string) []byte {
	env, _ := stdjson.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
	})
	return env
}

// driveRecursiveRoute invokes the generated /call for one recursive route and drains
// the stream, returning the final decoded pointer tree + winner/planned tokens.
func driveRecursiveRoute(t *testing.T, a bamlutils.Adapter, name string) (final any, winner, planned string, drainErr error) {
	t.Helper()
	var ch <-chan bamlutils.StreamResult
	var err error
	switch name {
	case "StaticRecursiveNode":
		ch, err = fixture.StaticRecursiveNode(a, &fixture.StaticRecursiveNodeInput{Topic: "a list"})
	case "StaticRecursiveA":
		ch, err = fixture.StaticRecursiveA(a, &fixture.StaticRecursiveAInput{Topic: "an a/b chain"})
	case "StaticRecursiveB":
		ch, err = fixture.StaticRecursiveB(a, &fixture.StaticRecursiveBInput{Topic: "a b/a chain"})
	case "StaticRecursiveLoop":
		ch, err = fixture.StaticRecursiveLoop(a, &fixture.StaticRecursiveLoopInput{Topic: "a loop"})
	case "StaticRecursiveNodeAnn":
		ch, err = fixture.StaticRecursiveNodeAnn(a, &fixture.StaticRecursiveNodeAnnInput{Topic: "a stream"})
	default:
		t.Fatalf("unknown recursive route %q", name)
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

// TestStaticServingCutoverManifest_Recursive drives the FULL endpoint matrix (Node/A/B
// × depths {0,1,2,32} × {explicit-null, omitted}) through the generated /call, and for
// EVERY row asserts plan parity (planned=native), exactly one provider send with no
// resend, the native structured winner, and the EXACT concrete Node/A/B pointer tree.
func TestStaticServingCutoverManifest_Recursive(t *testing.T) {
	depths := []int{0, 1, 2, 32}
	rows := 0
	planMatched, oneSend, claimed, nativeWin, treeExact := 0, 0, 0, 0, 0

	for _, r := range staticServingCorpusRecursive {
		root := strings.TrimPrefix(r.name, "StaticRecursive") // Node / A / B
		for _, depth := range depths {
			for _, explicitNull := range []bool{true, false} {
				rows++
				name := r.name + "/depth" + strconv.Itoa(depth)
				if explicitNull {
					name += "/explicit_null"
				} else {
					name += "/omitted"
				}
				t.Run(name, func(t *testing.T) {
					spy := newStaticServeSpy(t)
					server := newFixtureServer(t, 200, openAIContent(recResponse(root, depth, explicitNull)))
					a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
					final, winner, planned, err := driveRecursiveRoute(t, a, r.name)
					server.close()
					if err != nil {
						t.Fatalf("serve %s: %v", name, err)
					}
					if planned == "native" {
						planMatched++ // native planned == plan compared and matched (parity)
					} else {
						t.Errorf("%s: planned_engine=%q, want native (plan parity)", name, planned)
					}
					if server.count.Load() == 1 {
						oneSend++
					} else {
						t.Errorf("%s: provider saw %d requests, want 1 (one-send, no resend)", name, server.count.Load())
					}
					if spy.nativeSocketsRaw() == 1 {
						claimed++
					} else {
						t.Errorf("%s: native sockets=%v, want 1 (claimed)", name, spy.nativeSocketsRaw())
					}
					if winner == bamlutils.NativeStaticServeEngineNative {
						nativeWin++
					} else {
						t.Errorf("%s: winner=%q, want native", name, winner)
					}
					want := recExpectedTree(root, depth)
					if got := jsonOf(t, final); got == want {
						treeExact++
					} else {
						t.Errorf("%s: decoded pointer tree mismatch:\n got:  %s\n want: %s", name, got, want)
					}
				})
			}
		}
	}

	pin := map[string]int{"rows": rows, "plan-matched": planMatched, "one-send": oneSend, "claimed": claimed, "native-win": nativeWin, "tree-exact": treeExact}
	want := map[string]int{"rows": 24, "plan-matched": 24, "one-send": 24, "claimed": 24, "native-win": 24, "tree-exact": 24}
	for k, w := range want {
		if pin[k] != w {
			t.Errorf("recursive serve matrix pin %s = %d, want %d (frozen: 3 roots × depths 0/1/2/32 × explicit-null/omitted)", k, pin[k], w)
		}
	}
	t.Logf("recursive serve matrix pin (frozen): %+v", pin)
}

// TestStaticServingCutover_RecursiveUnicodeCall is the Unicode `/call` ROUTE row: a
// value carrying HTML metacharacters (`<tag> &`) still serves the NATIVE winner (the
// escaping-canonicalized comparator does not read native's SetEscapeHTML(false) bytes
// vs the production json.Marshal bytes as a mismatch), not a silent BAML-parse winner.
func TestStaticServingCutover_RecursiveUnicodeCall(t *testing.T) {
	content := `{"value":"café ☕ <tag> & </tag> \"q\"","next":{"value":"leaf","next":null}}`
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, 200, openAIContent(content))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
	final, winner, planned, err := driveRecursiveRoute(t, a, "StaticRecursiveNode")
	if err != nil {
		t.Fatalf("unicode /call: %v", err)
	}
	if planned != "native" {
		t.Errorf("unicode: planned_engine=%q, want native", planned)
	}
	if server.count.Load() != 1 {
		t.Errorf("unicode: provider saw %d requests, want 1 (one-send)", server.count.Load())
	}
	if spy.nativeSocketsRaw() != 1 {
		t.Errorf("unicode: native sockets=%v, want 1 (claimed)", spy.nativeSocketsRaw())
	}
	if winner != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("unicode: winner=%q, want native (escaping canonicalization must not force a BAML-parse fallback)", winner)
	}
	// The decoded value round-trips through the concrete carrier.
	if got := jsonOf(t, final); !strings.Contains(got, "café") || !strings.Contains(got, "tag") {
		t.Errorf("unicode: decoded tree lost content: %s", got)
	}
}

// TestStaticServingCutover_LoopRouteDeclines is the one-field Loop ROUTE regression:
// the served fingerprint declines Loop PRE-TRANSPORT, so the generated /call opens
// ZERO native sockets and BAML serves the single request. The serve callback IS
// invoked (flag on) but declines before claiming a socket.
func TestStaticServingCutover_LoopRouteDeclines(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, 200, openAIContent(`{"next":null}`))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
	_, winner, _, err := driveRecursiveRoute(t, a, "StaticRecursiveLoop")
	if err != nil {
		t.Fatalf("loop /call: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("loop: serve func invoked %d times, want 1 (flag on: callback runs then declines)", got)
	}
	if got := spy.nativeSocketsRaw(); got != 0 {
		t.Fatalf("loop: native sockets=%v, want 0 (fingerprint declines PRE-TRANSPORT)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("loop: provider saw %d requests, want 1 (BAML serves the declined route)", got)
	}
	if winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("loop: winner=%q, must NOT be native (Loop declines pre-claim)", winner)
	}
}

// TestStaticServingCutover_StreamAnnotatedRouteDeclines is the @stream.* zero-socket
// ROUTE regression: StaticRecursiveNodeAnn returns a recursive class whose value
// field carries a @stream.done annotation that static lowering PRESERVES into the
// descriptor's Return bundle (verified: introspected carries TypeMeta{Stream:{Done}}).
// The exact recursive fingerprint rejects any @stream.* carrier, so the generated
// /call declines PRE-TRANSPORT — ZERO native sockets, BAML serves — rather than
// claiming a socket on an annotated (non-annotation-free) recursive schema.
func TestStaticServingCutover_StreamAnnotatedRouteDeclines(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, 200, openAIContent(`{"value":"root","next":null}`))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)
	_, winner, _, err := driveRecursiveRoute(t, a, "StaticRecursiveNodeAnn")
	if err != nil {
		t.Fatalf("stream-annotated /call: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("stream-annotated: serve func invoked %d times, want 1 (flag on: callback runs then declines)", got)
	}
	if got := spy.nativeSocketsRaw(); got != 0 {
		t.Fatalf("stream-annotated: native sockets=%v, want 0 (@stream.* fingerprint declines PRE-TRANSPORT)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("stream-annotated: provider saw %d requests, want 1 (BAML serves the declined route)", got)
	}
	if winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("stream-annotated: winner=%q, must NOT be native (a @stream.*-annotated recursive schema declines pre-claim)", winner)
	}
}

// TestStaticServingCutover_RecursivePartition is the recursive anti-omission + the
// partition-completeness check: the recursive served corpus is EXACTLY {Node, A, B},
// and legacy ∪ recursive-served ∪ declined == the fixture's full emitted SyncMethods
// (no method is silently unaccounted).
func TestStaticServingCutover_RecursivePartition(t *testing.T) {
	// Recursive served anti-omission against the explicit set.
	wantRecursive := []string{"StaticRecursiveNode", "StaticRecursiveA", "StaticRecursiveB"}
	gotRecursive := make([]string, 0, len(staticServingCorpusRecursive))
	for _, r := range staticServingCorpusRecursive {
		gotRecursive = append(gotRecursive, r.name)
	}
	sort.Strings(wantRecursive)
	sort.Strings(gotRecursive)
	if strings.Join(wantRecursive, ",") != strings.Join(gotRecursive, ",") {
		t.Fatalf("recursive served corpus %v != frozen %v", gotRecursive, wantRecursive)
	}

	// Partition completeness: legacy ∪ recursive-served ∪ recursive-declined ∪
	// alias-served ∪ alias-declined == SyncMethods. The alias corpora (de-BAML Phase 3a,
	// alias_cutover_manifest_test.go) are folded in so this anti-omission stays exact as
	// new families are added — the frozen recursive corpus {Node, A, B} above is
	// unchanged.
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
