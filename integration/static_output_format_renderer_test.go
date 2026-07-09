//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/nativeschema"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// TestNativeStaticOutputFormatParity is the de-BAML P3 load-bearing correctness
// proof (slices 3-4): the native STATIC-schema pipeline must reproduce BAML's
// real ctx.output_format byte-for-byte for a corpus of static .baml functions.
//
// It is the static-schema analogue of TestNativeOutputFormatRendererParity
// (which proves the same for the DYNAMIC-schema surface). ONE set of .baml
// bytes drives BOTH sides, so there is no parallel hand-built fixture that
// could silently drift:
//
//  1. The parity fixtures (integration/testdata/parity_baml_src) declare
//     functions whose prompt is ONLY `{{ ctx.output_format }}`, so the
//     upstream request's first message text is exactly BAML's rendered
//     output-format block and nothing else.
//  2. A DEDICATED baml-rest container is started with BAML_REST_USE_DEBAML=false
//     so ctx.output_format is rendered by the real BAML v0.223 runtime (the
//     ORACLE), NOT the native de-BAML seam. The shared TestEnv is default-ON
//     and cannot serve as that control, so this test owns its container (like
//     the de-BAML REST oracle test). The container is forced into SUBPROCESS
//     mode so that a response-decode panic in BAML's generated client (which
//     enum-key and literal-union-key maps trigger — a generated-code bug, not a
//     render bug) is isolated to the restartable worker process and never
//     crashes the server; the outbound request (carrying the rendered
//     output_format we compare) is captured BEFORE decode. Each function is
//     called through the HTTP /call endpoint and the mock LLM captures the
//     outbound provider body; extractFirstMessageText recovers BAML's string.
//  3. The SAME .baml bytes are parsed with the native parser
//     (bamlutils/bamlparser), the function's output type is lowered into a
//     schemadescriptor.Bundle in BAML reachability order
//     (internal/nativeschema.BuildStaticSchemas), re-lowered with
//     schema.FromStaticDescriptor, and rendered with
//     outputformat.Render(bundle, outputformat.Options{}) — RenderOptions::default,
//     matching the template's bare {{ ctx.output_format }}.
//
// The byte-exact comparison is the parity criterion. It proves the whole static
// chain — parse -> reachability ordering -> attribute lowering (@alias/
// @description/@assert/@check/@stream.*) -> FromStaticDescriptor -> render —
// matches BAML for every covered function, including map KEY shapes (string,
// enum, literal-union) and the key-then-value reachability order pinned by
// ParityEnumKeyMap (a hoisted value-enum block renders before the hoisted
// key-enum block). It also pins that metadata BAML does NOT render in the final
// output_format stays invisible on the native side too: `///` doc comments
// (ParityDocComments, and ParityDocVsDesc where @description wins), @assert/
// @check constraints (ParityConstraints), and @stream.* flags (ParityStream,
// ParityMixed) must not change a byte — BAML captures them but its output_format
// renderer never reads them (see #586 D6-D8), and neither does ours. @skip
// (ParitySkip, slice 6) is the inverse: it DOES change the block (a skipped
// class field / enum value is DROPPED, and a class reachable only through a
// skipped field disappears), and the native side must drop exactly the same
// bytes BAML does (#586 D11). The expected function set is asserted independently
// (P2) so a silently-skipped fixture cannot false-pass, and the run is pinned to
// the exact 0.223.0 oracle (P3). The corpus here is all-supported (a `///` doc
// comment is a no-op and @skip DROPS, neither is a decline); genuine declines
// (invalid recursion cycles, @@dynamic, static streaming, media/tuple output,
// ...) are covered by the unit tests in internal/nativeschema, not by this
// oracle.
func TestNativeStaticOutputFormatParity(t *testing.T) {
	// P3: this is the load-bearing v0.223.0 parity proof, so it asserts the
	// EXACT oracle version rather than a >=0.219 floor. The de-BAML=false render
	// seam and the native renderer both track BAML's output_format semantics,
	// which are stable across 0.219+, but the byte-for-byte CLAIM is against
	// 0.223.0 specifically; older cells skip so a green run cannot be
	// misattributed to the pinned oracle.
	const oracleVersion = "0.223.0"
	if BAMLVersion != oracleVersion {
		t.Skipf("static output_format parity is pinned to the BAML %s oracle; skipping on %s", oracleVersion, BAMLVersion)
	}
	t.Logf("static output_format parity oracle: BAML v%s", BAMLVersion)

	parityDir := parityBamlSrcPath(t)

	// Native descriptors from the SAME .baml bytes the container compiles. Built
	// BEFORE the (slow) container setup so a corpus regression fails fast.
	nativeBundles := buildNativeParityBundles(t, parityDir)

	// P2: independently derive the expected function-name set from the fixture
	// source (a raw-bytes scan, not the builder), and REQUIRE the native builder
	// produced a bundle for exactly that set. Without this, a fixture the builder
	// silently skips (built no bundle AND recorded no decline) would simply never
	// be oracle-tested and the run would false-pass.
	expected := parityFunctionNames(t, parityDir)
	assertBundleSetMatches(t, expected, nativeBundles)

	// Dedicated de-BAML-OFF container: renders ctx.output_format via BAML v0.223.
	opts := matrixSetupOptions()
	opts.BAMLSrcPath = parityDir
	opts.RuntimeEnv = map[string]string{
		bamlutils.EnvUseDeBAML: "false",
	}
	// P1(a): force SUBPROCESS mode for this dedicated container (independent of
	// the outer matrix arm). A function whose output reaches an enum-key or
	// literal-union-key map triggers a PANIC in BAML's generated response Decode
	// (a generated-code bug, not an output_format render bug). In subprocess
	// mode that panic is isolated to the worker PROCESS — which the pool restarts
	// — so it never crashes the server; the outbound request carrying the
	// rendered ctx.output_format is captured BEFORE the response is decoded, so
	// those map key shapes are still byte-compared against BAML.
	opts.InProcess = false

	setupCtx, setupCancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer setupCancel()
	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("Failed to set up dedicated de-BAML=false parity env: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpContainerLogs("Parity BAML REST", env.BAMLRest)
		}
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated parity env Terminate: %v", err)
		}
	})

	mock := mockllm.NewClient(env.MockLLMURL)
	client := testutil.NewBAMLRestClient(env.BAMLRestURL)

	names := make([]string, 0, len(nativeBundles))
	for name := range nativeBundles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, fnName := range names {
		t.Run(fnName, func(t *testing.T) {
			bamlRendered := captureStaticOutputFormat(t, client, mock, env.MockLLMInternal, fnName)

			bundle := nativeBundles[fnName]
			internal, err := schema.FromStaticDescriptor(bundle)
			if err != nil {
				t.Fatalf("FromStaticDescriptor(%q): %v", fnName, err)
			}
			got, err := outputformat.Render(internal, outputformat.Options{})
			if err != nil {
				t.Fatalf("outputformat.Render(%q): %v", fnName, err)
			}

			if got != bamlRendered {
				t.Errorf("native static renderer diverged from BAML ground truth\n--- native ---\n%q\n--- BAML ---\n%q\n\n--- native (raw) ---\n%s\n--- BAML (raw) ---\n%s",
					got, bamlRendered, got, bamlRendered)
			}
		})
	}
}

// captureStaticOutputFormat drives one static /call through the dedicated
// container and returns the first message's rendered output_format text from
// the captured provider body. The call's response parse may fail (the mock
// returns a benign body that need not satisfy the function's schema); the
// provider request is captured BEFORE the response is parsed, so the body is
// available regardless.
func captureStaticOutputFormat(t *testing.T, client *testutil.BAMLRestClient, mock *mockllm.Client, mockInternalURL, fnName string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scenarioID := "parity-static-" + fnName
	if err := mock.RegisterScenario(ctx, &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        "{}",
		ChunkSize:      0,
		InitialDelayMs: 10,
	}); err != nil {
		t.Fatalf("RegisterScenario(%s): %v", scenarioID, err)
	}

	if _, err := client.Call(ctx, testutil.CallRequest{
		Method: fnName,
		Input:  map[string]any{"input": "x"},
		Options: &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(mockInternalURL, scenarioID),
		},
	}); err != nil {
		// Response parsing may fail for a bare-primitive/typed output vs the
		// benign mock body; the request is captured before parsing.
		t.Logf("Call(%s) (request still captured): %v", fnName, err)
	}

	body, err := mock.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest(%s): %v", scenarioID, err)
	}
	text, err := extractFirstMessageText(body)
	if err != nil {
		t.Fatalf("extractFirstMessageText(%s): %v\ncaptured: %s", scenarioID, err, string(body))
	}
	return text
}

// buildNativeParityBundles parses every .baml file in dir and runs the native
// static-schema builder over the whole set, returning the per-function
// descriptor bundles. The corpus is all-supported by design, so any decline is
// a fixture/build regression and fails the test up front.
func buildNativeParityBundles(t *testing.T, dir string) map[string]sd.Bundle {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read parity baml_src %s: %v", dir, err)
	}
	var files []nativeschema.SourceFile
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".baml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := bamlparser.ParseBytes(e.Name(), data)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, nativeschema.SourceFile{File: f})
	}

	bundles, declines := nativeschema.BuildStaticSchemas(files)
	if len(declines) > 0 {
		for name, reason := range declines {
			t.Errorf("parity corpus function %q was declined (corpus must be all-supported): %s", name, reason)
		}
		t.Fatalf("native builder declined %d parity function(s); see errors above", len(declines))
	}
	if len(bundles) == 0 {
		t.Fatalf("no parity functions built from %s", dir)
	}
	return bundles
}

// parityBamlSrcPath resolves the dedicated parity baml_src directory, derived
// from the shared corpus path so it tracks the same testdata root regardless of
// where the suite is run from.
func parityBamlSrcPath(t *testing.T) string {
	t.Helper()
	if bamlSrcPath == "" {
		t.Skip("shared bamlSrcPath not resolved; cannot locate parity baml_src")
	}
	dir := filepath.Join(filepath.Dir(bamlSrcPath), "parity_baml_src")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("parity baml_src not present at %s: %v", dir, err)
	}
	return dir
}

// parityFunctionDeclRE matches a BAML `function Name(` declaration at the start
// of a (possibly indented) line. It is deliberately a raw-bytes scan so the
// expected function set is derived INDEPENDENTLY of the native parser/builder
// under test.
var parityFunctionDeclRE = regexp.MustCompile(`(?m)^\s*function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// parityFunctionNames returns the set of function names declared across every
// .baml file in dir, scanned from raw source (not via the parser under test).
func parityFunctionNames(t *testing.T, dir string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read parity baml_src %s: %v", dir, err)
	}
	names := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".baml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range parityFunctionDeclRE.FindAllSubmatch(data, -1) {
			names[string(m[1])] = struct{}{}
		}
	}
	if len(names) == 0 {
		t.Fatalf("no function declarations found in %s", dir)
	}
	return names
}

// assertBundleSetMatches requires the native builder produced a bundle for
// EXACTLY the expected function set — no missing (silently skipped) function and
// no unexpected extra. A missing function would otherwise never reach the BAML
// oracle, letting the parity run false-pass on incomplete coverage.
func assertBundleSetMatches(t *testing.T, expected map[string]struct{}, bundles map[string]sd.Bundle) {
	t.Helper()
	var missing []string
	for name := range expected {
		if _, ok := bundles[name]; !ok {
			missing = append(missing, name)
		}
	}
	var extra []string
	for name := range bundles {
		if _, ok := expected[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("native builder produced no descriptor for expected fixture function(s) %v — they would never be oracle-tested", missing)
	}
	if len(extra) > 0 {
		t.Errorf("native builder produced a descriptor for unexpected function(s) %v not found in the fixture source", extra)
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("parity function set mismatch: expected %d, built %d", len(expected), len(bundles))
	}
}
