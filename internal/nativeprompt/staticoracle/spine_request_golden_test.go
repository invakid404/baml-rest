//go:build integration

package staticoracle

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
)

// spine_request_golden_test.go generates + freshness-checks the FROZEN BAML v0.223
// provider REQUEST golden for the exact ExecBridge-U1 population method
// StaticRecursiveAliasJSON with a fixed scalar input. It is the REAL v0.223 oracle
// the isolated nativeserve/spine exact-transport test asserts against (Codex review
// #2 finding 3): spine cannot link the BAML CFFI, so the oracle is frozen HERE — in
// the root module that already links it via the static_oracle baml_client — and the
// spine test reads the committed golden.
//
// The golden is BAML's own Request.StaticRecursiveAliasJSON(...) no-send plan
// (method / URL / body / sorted headers), read straight from the generated builder
// via the CFFI. The static_oracle client uses FAKE literal credentials and a
// `.invalid` base URL, so the golden carries no secret and is never contacted.
//
// -update rewrites the golden; without it, this test asserts the committed golden
// STILL equals what BAML v0.223 produces live — so the frozen oracle can never
// silently drift from the pinned toolchain (TestBAMLVersionPinned proves v0.223).

var updateSpineGolden = flag.Bool("update-spine-request-golden", false,
	"regenerate nativeserve/spine/testdata/staticrecursivealiasjson_v0223_request.json from BAML v0.223")

// spineGoldenInput is the fixed scalar input frozen into the golden (and used by the
// spine test that asserts against it).
const spineGoldenInput = "arbitrary json"

// frozenRequest is the committed shape of a BAML v0.223 provider request plan.
type frozenRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Body    string      `json:"body"`
	Headers [][2]string `json:"headers"` // name-sorted
}

func spineGoldenPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <repo>/internal/nativeprompt/staticoracle/spine_request_golden_test.go
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(repo, "nativeserve", "spine", "testdata", "staticrecursivealiasjson_v0223_request.json")
}

// bamlV0223Request captures BAML's live no-send request plan for the frozen method+input.
func bamlV0223Request(t *testing.T) frozenRequest {
	t.Helper()
	req, err := bamlclient.Request.StaticRecursiveAliasJSON(spineGoldenInput)
	if err != nil {
		t.Fatalf("BAML Request.StaticRecursiveAliasJSON(%q): %v", spineGoldenInput, err)
	}
	method, err := req.Method()
	if err != nil {
		t.Fatalf("BAML Method(): %v", err)
	}
	url, err := req.Url()
	if err != nil {
		t.Fatalf("BAML Url(): %v", err)
	}
	body, err := bodyText(req)
	if err != nil {
		t.Fatalf("BAML Body().Text(): %v", err)
	}
	hdrs, err := req.Headers()
	if err != nil {
		t.Fatalf("BAML Headers(): %v", err)
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(names))
	for _, n := range names {
		pairs = append(pairs, [2]string{n, hdrs[n]})
	}
	return frozenRequest{Method: method, URL: url, Body: body, Headers: pairs}
}

// TestStaticSpineRequestGoldenIsCurrentV0223 regenerates the golden with -update, and
// otherwise asserts the committed golden still equals BAML v0.223's live output.
func TestStaticSpineRequestGoldenIsCurrentV0223(t *testing.T) {
	got := bamlV0223Request(t)
	if got.Body == "" || got.URL == "" || got.Method == "" {
		t.Fatal("BAML produced an empty request plan")
	}
	enc, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	enc = append(enc, '\n')
	path := spineGoldenPath(t)

	if *updateSpineGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, enc, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", path, len(enc))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed golden (regenerate with -update-spine-request-golden): %v", err)
	}
	if string(want) != string(enc) {
		t.Fatalf("committed BAML v0.223 request golden is stale vs live BAML — re-run with -update-spine-request-golden.\n--- committed ---\n%s\n--- live ---\n%s", want, enc)
	}
}
