//go:build integration && nanollm_prebuilt && nanollm_integration

package static

// De-BAML Phase 5.1 STATIC OpenAI prepared-request differential.
//
// For every Phase-4a-admitted static-corpus row this proves that nanollm's
// PreparedRequest (from Client.Prepare on the native 4a canonical body) matches
// the ACTUAL provider request stock BAML v0.223.0 builds — read from the
// generated Request.<Function>() BEFORE any send (no network, no RoundTripper,
// no response) — WITHOUT ever calling HTTPRequest / Do / DoStream /
// TranslateResponse or any baml-rest transport.
//
// Capture (pre-send): req.Method(), req.Url(), req.Headers() (a
// map[string]string), and req.Body().Text() — the exact provider request bytes.
//
// Differential (scope "Exact field policy"), all without decoding the body:
//   - the native canonical body equals BAML's body byte-for-byte, then nanollm's
//     plan body equals BOTH (triple byte equality);
//   - method == POST on both and == prep.Method;
//   - URL == prep.URL and contains the fake base + /v1 + /chat/completions;
//   - semantic headers (a unique ASCII-lowercase map; duplicates/multi-values
//     rejected) match by set + value exactly, BAML's internal `baml-original-url`
//     transport header explicitly declined (../testutil);
//   - exactly one authorization each side, exact `Bearer <fake key>`, redacted;
//   - nanollm's OWN header order (Content-Type then Authorization) asserted
//     independently — NEVER compared to BAML;
//   - prep.ResponseFormat == FormatJSON; plan meta resolves
//     alias/target/openai/ChatCompletion/non-stream, no transform, zero retries;
//     unsigned plan (SignedAt/ExpiresAt nil, !Expired()).
//
// It reuses the existing test-only StaticOracleClient fixture
// (internal/nativeprompt/testdata/static_oracle) whose literal model/key/base URL
// ARE the P5 fence values, so no fixture is regenerated to invent credentials.
// A version guard pins the loaded CFFI + module to stock v0.223.0.
//
// Runs in the SEPARATE, go.work-excluded nanollmprepare module; links stock BAML
// v0.223.0 and nanollm's Rust FFI, and must never share a binary with the
// patched-BAML dynamic leg (../dynamic).

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/planassert"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/testutil"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	"github.com/invakid404/baml-rest/internal/nativeschema"
	nanollm "github.com/viktordanov/nanollm/go"
)

// The literal auth/environment fence (scope "Concrete auth/environment fence"),
// aliased from the shared single source of truth in ../planassert. These are the
// exact literals declared in the reused StaticOracleClient fixture
// (testdata/static_oracle/baml_src/clients.baml), so the static BAML leg reads
// them straight from the fixture and the paired nanollm config restates them.
const (
	fenceModel   = planassert.FenceModel
	fenceBaseURL = planassert.FenceBaseURL
	fenceAPIKey  = planassert.FenceAPIKey
	fenceAlias   = planassert.FenceAlias

	wantURL = planassert.WantURL
)

const (
	// fixtureDir is the checked-in claimed .baml project (native leg source),
	// relative to this package directory.
	fixtureDir = "../../../nativeprompt/testdata/static_oracle/baml_src"
	// moduleGoModPath is this test-only module's go.mod (it, not the root, pins
	// the stock BAML the static binary links), relative to this package dir.
	moduleGoModPath = "../go.mod"
	// bamlModulePath / wantBAMLModuleVersion / wantBAMLRuntimeVersion pin stock
	// BAML so a green result can never be attributed to a different runtime.
	bamlModulePath         = "github.com/boundaryml/baml"
	wantBAMLModuleVersion  = "v0.223.0"
	wantBAMLRuntimeVersion = "0.223.0"
)

// fixtureClientProvider maps the fixture client to its provider for the native
// descriptor build (mirrors clients.baml; Phase 3 never renders ctx.client).
var fixtureClientProvider = map[string]string{"StaticOracleClient": "openai"}

// staticCase is one claimed-corpus row: a descriptor function name, the native
// args, and a typed closure driving the generated Request.<fn> with the SAME
// values. It mirrors the stock-BAML static body oracle's corpus
// (internal/nativeprompt/staticoracle).
type staticCase struct {
	name  string
	fn    string
	args  map[string]any
	build func() (baml.HTTPRequest, error)
}

// staticCases is the Phase-4a-admitted static corpus (completions, primitive-arg
// binding across value rows, ordered role chat, and bare ctx.output_format in
// both chat and completion), identical to the stock-BAML static body oracle.
func staticCases() []staticCase {
	return []staticCase{
		{"StaticCompletion/ascii", "StaticCompletion", map[string]any{"topic": "cats and dogs"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("cats and dogs") }},
		{"StaticCompletion/unicode_quotes_backslash", "StaticCompletion", map[string]any{"topic": "café ☕ \"q\" \\b"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("café ☕ \"q\" \\b") }},
		{"StaticCompletion/embedded_newline", "StaticCompletion", map[string]any{"topic": "line1\nline2"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("line1\nline2") }},
		{"StaticCompletion/inner_spaces", "StaticCompletion", map[string]any{"topic": "  padded  "},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("  padded  ") }},

		{"StaticPrimitiveArgs/zeros", "StaticPrimitiveArgs",
			map[string]any{"text": "hi", "count": int64(0), "ratio": float64(0), "flag": false},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticPrimitiveArgs("hi", 0, 0, false) }},
		{"StaticPrimitiveArgs/neg_frac_true", "StaticPrimitiveArgs",
			map[string]any{"text": "héllo", "count": int64(-1), "ratio": float64(3.14), "flag": true},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("héllo", -1, 3.14, true)
			}},
		{"StaticPrimitiveArgs/int_max_exp", "StaticPrimitiveArgs",
			map[string]any{"text": "x", "count": int64(9223372036854775807), "ratio": float64(1e10), "flag": true},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("x", 9223372036854775807, 1e10, true)
			}},
		{"StaticPrimitiveArgs/int_min_negfrac", "StaticPrimitiveArgs",
			map[string]any{"text": "y", "count": int64(-9223372036854775808), "ratio": float64(-2.5), "flag": false},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("y", -9223372036854775808, -2.5, false)
			}},

		{"StaticRoleChat/basic", "StaticRoleChat", map[string]any{"topic": "weather", "count": int64(3)},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticRoleChat("weather", 3) }},
		{"StaticRoleChat/unicode_zero", "StaticRoleChat", map[string]any{"topic": "the ☀️", "count": int64(0)},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticRoleChat("the ☀️", 0) }},

		{"StaticOutputFormat/weather", "StaticOutputFormat", map[string]any{"topic": "weather"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticOutputFormat("weather") }},
		{"StaticOutputFormat/unicode", "StaticOutputFormat", map[string]any{"topic": "café"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticOutputFormat("café") }},

		{"StaticCompletionOutputFormat/weather", "StaticCompletionOutputFormat", map[string]any{"topic": "weather"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletionOutputFormat("weather") }},
		{"StaticCompletionOutputFormat/trees", "StaticCompletionOutputFormat", map[string]any{"topic": "trees"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletionOutputFormat("trees") }},
	}
}

// staticCaseByName returns the claimed-corpus row with the given name, failing
// loudly if it is absent. Negative controls select rows by NAME (not index) so a
// reorder/extend of staticCases() can never silently rebind a control to a
// different fixture.
func staticCaseByName(t *testing.T, name string) staticCase {
	t.Helper()
	for _, c := range staticCases() {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no static case named %q", name)
	return staticCase{}
}

// staticRow is the assembled evidence for one row.
type staticRow struct {
	native   *nativebody.CanonicalBody
	bamlBody string
	prep     *nanollm.PreparedRequest
	bamlSnap testutil.Snapshot
	prepSnap testutil.Snapshot
}

// captureAndPrepare builds the native body, captures BAML's pre-send request, and
// runs nanollm Prepare for one row with the given nanollm client. It fails closed
// on a declined native build, an empty BAML body, a duplicate/multi-value header,
// or a Prepare error — it does NOT assert cross-leg parity (callers do).
func captureAndPrepare(t *testing.T, descriptors map[string]promptdescriptor.Function, nano *nanollm.Client, c staticCase) staticRow {
	t.Helper()

	fn, ok := descriptors[c.fn]
	if !ok {
		t.Fatalf("no built descriptor for %q", c.fn)
	}

	// --- Native leg: render + normalize client + build canonical body. ---
	if err := nativeprompt.SupportsStatic(fn, c.args); err != nil {
		t.Fatalf("SupportsStatic declined a claimed case: %v", err)
	}
	rendered, err := nativeprompt.RenderStatic(fn, c.args)
	if err != nil {
		t.Fatalf("RenderStatic failed after SupportsStatic accepted: %v", err)
	}
	intent, err := nativebody.NormalizeStaticClient(fn.ClientConfig, fenceAlias, false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient declined the fixture client: %v", err)
	}
	native, err := nativebody.BuildOpenAIChat(rendered, intent)
	if err != nil {
		t.Fatalf("BuildOpenAIChat declined a claimed case: %v", err)
	}

	// --- BAML leg: build the provider request (no send) and read the full request. ---
	req, err := c.build()
	if err != nil {
		t.Fatalf("BAML Request.%s: %v", c.fn, err)
	}
	method, err := req.Method()
	if err != nil {
		t.Fatalf("BAML Method(): %v", err)
	}
	url, err := req.Url()
	if err != nil {
		t.Fatalf("BAML Url(): %v", err)
	}
	hdrs, err := req.Headers()
	if err != nil {
		t.Fatalf("BAML Headers(): %v", err)
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("BAML Body(): %v", err)
	}
	bamlBody, err := body.Text()
	if err != nil {
		t.Fatalf("BAML body Text(): %v", err)
	}
	if bamlBody == "" {
		t.Fatal("BAML body is empty — request build failed before producing a body")
	}
	if method == "" || url == "" || len(hdrs) == 0 {
		t.Fatalf("incomplete BAML capture: method=%q url=%q headers=%d", method, url, len(hdrs))
	}

	// --- nanollm leg: Prepare on the native raw body (no send). ---
	prep, err := nano.Prepare(nanollm.Request{
		Model:  fenceAlias,
		Body:   native.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if err != nil {
		t.Fatalf("Prepare rejected a claimed body: %v", err)
	}

	bamlSnap, err := testutil.NewSnapshot(method, url, testutil.PairsFromStringMap(hdrs), []byte(bamlBody))
	if err != nil {
		t.Fatalf("BAML request has duplicate/multi-value headers: %v", err)
	}
	prepSnap, err := testutil.NewSnapshot(prep.Method, prep.URL, prep.Headers, prep.Body)
	if err != nil {
		t.Fatalf("nanollm plan has duplicate/multi-value headers: %v", err)
	}

	return staticRow{native: native, bamlBody: bamlBody, prep: prep, bamlSnap: bamlSnap, prepSnap: prepSnap}
}

// TestStaticPreparedRequestParity is the core static differential.
func TestStaticPreparedRequestParity(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	for _, c := range staticCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			row := captureAndPrepare(t, descriptors, nano, c)

			// (1) Raw body: native == BAML, then nanollm plan == both. bytes.Equal on
			// the raw []byte with NO textual conversion before the assertion (the BAML
			// Body().Text() string is converted with []byte(...) for the compare only).
			bamlBodyBytes := []byte(row.bamlBody)
			if !bytes.Equal(row.native.Bytes(), bamlBodyBytes) {
				t.Errorf("native body != BAML body\n--- native ---\n%s\n--- BAML ---\n%s", row.native.Bytes(), row.bamlBody)
			}
			if !bytes.Equal(row.prep.Body, row.native.Bytes()) {
				t.Errorf("plan body != native body\n--- plan ---\n%s\n--- native ---\n%s", row.prep.Body, row.native.Bytes())
			}
			if !bytes.Equal(row.prep.Body, bamlBodyBytes) {
				t.Errorf("plan body != BAML body")
			}

			// (2) Method / URL exact.
			if row.prep.Method != "POST" || row.bamlSnap.Method != "POST" {
				t.Errorf("method: baml=%q nanollm=%q, want POST", row.bamlSnap.Method, row.prep.Method)
			}
			if row.prep.URL != wantURL || row.bamlSnap.URL != wantURL {
				t.Errorf("url: baml=%q nanollm=%q, want %q", row.bamlSnap.URL, row.prep.URL, wantURL)
			}
			planassert.AssertFenceURL(t, row.prep.URL)

			// (3) method + url + body + semantic headers via the shared comparator.
			if diffs := testutil.Diff(row.bamlSnap, row.prepSnap); len(diffs) != 0 {
				t.Errorf("prepared-request differential mismatch:\n%s", strings.Join(diffs, "\n"))
			}

			// (4) Auth: exactly one authorization each side, exact fake value (redacted).
			planassert.AssertAuth(t, row.bamlSnap.Headers, row.prepSnap.Headers)

			// (5) nanollm's OWN emitted header order — asserted independently.
			planassert.AssertNanollmHeaderOrder(t, row.prep.Headers)

			// (6) Response format + plan meta + unsigned-plan invariants.
			planassert.AssertResponseFormat(t, row.prep)
			planassert.AssertMeta(t, row.prep, fenceModel)
			planassert.AssertUnsigned(t, row.prep)
		})
	}
}

// --- version guard: pin the stock BAML runtime + module to v0.223.0. ---

// TestBAMLVersionPinned requires the LOADED CFFI runtime AND this module's go.mod
// pin to be exactly stock BAML v0.223.0, so a green result can never be
// attributed to a different runtime.
func TestBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}
	version, replaced, found := bamlPinFromGoMod(t, moduleGoModPath)
	if !found {
		t.Fatalf("module go.mod does not pin %s", bamlModulePath)
	}
	if replaced {
		t.Fatalf("module go.mod replaces %s; the static oracle must link the stock module", bamlModulePath)
	}
	if version != wantBAMLModuleVersion {
		t.Fatalf("module go.mod pins %s %s, want exactly %s", bamlModulePath, version, wantBAMLModuleVersion)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep == nil || dep.Path != bamlModulePath {
				continue
			}
			if dep.Replace != nil {
				t.Fatalf("build info reports %s replaced (%s %s)", bamlModulePath, dep.Replace.Path, dep.Replace.Version)
			}
			if dep.Version != wantBAMLModuleVersion {
				t.Fatalf("build info reports %s %s, want %s", bamlModulePath, dep.Version, wantBAMLModuleVersion)
			}
		}
	}
}

// --- controls: each must FAIL as a negative to keep the positives meaningful. ---

// TestStaticBodyByteMutationControl proves the body comparison is byte-sensitive.
func TestStaticBodyByteMutationControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, descriptors, nano, staticCaseByName(t, "StaticRoleChat/basic"))
	mutated := row.native.Bytes()
	mutated[len(mutated)/2] ^= 0x20
	if bytes.Equal(mutated, []byte(row.bamlBody)) {
		t.Fatal("a one-byte body mutation was NOT detected — the differential is not byte-sensitive")
	}
}

// TestStaticURLMutationControl proves a URL divergence is caught by the comparator.
func TestStaticURLMutationControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, descriptors, nano, staticCaseByName(t, "StaticCompletion/ascii"))
	bad := row.prepSnap
	bad.URL = row.prepSnap.URL + "/extra"
	if diffs := testutil.Diff(row.bamlSnap, bad); len(diffs) == 0 {
		t.Fatal("a mutated URL was NOT detected by the differential")
	}
}

// TestStaticAuthorizationMutationControl proves an authorization divergence is
// caught AND that the secret value never leaks into diagnostics.
func TestStaticAuthorizationMutationControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, descriptors, nano, staticCaseByName(t, "StaticCompletion/ascii"))
	bad := testutil.Snapshot{Method: row.prepSnap.Method, URL: row.prepSnap.URL, Body: row.prepSnap.Body,
		Headers: map[string]string{}}
	for k, v := range row.prepSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["authorization"] = "Bearer tampered-key"
	diffs := testutil.Diff(row.bamlSnap, bad)
	if len(diffs) == 0 {
		t.Fatal("a mutated authorization value was NOT detected")
	}
	joined := strings.Join(diffs, "\n")
	if strings.Contains(joined, "tampered-key") || strings.Contains(joined, fenceAPIKey) {
		t.Fatalf("authorization value leaked into diagnostics: %q", joined)
	}
}

// TestStaticDuplicateHeaderControl proves the comparator REJECTS a duplicate
// nanollm header rather than silently merging it.
func TestStaticDuplicateHeaderControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, descriptors, nano, staticCaseByName(t, "StaticCompletion/ascii"))
	dup := append(append([][2]string(nil), row.prep.Headers...), [2]string{"Authorization", "Bearer second"})
	if _, err := testutil.NewSnapshot(row.prep.Method, row.prep.URL, dup, row.prep.Body); err == nil {
		t.Fatal("a duplicate nanollm header was NOT rejected by the comparator")
	}
}

// TestStaticTargetRewriteControl proves a mismatched configured target is a
// negative control: nanollm rewrites the top-level JSON model, so the plan body
// diverges from BAML's body.
func TestStaticTargetRewriteControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	rw := planassert.NewPrepareClient(t, "rewrite-target-model")

	row := captureAndPrepare(t, descriptors, rw, staticCaseByName(t, "StaticCompletion/ascii"))
	if bytes.Equal(row.prep.Body, []byte(row.bamlBody)) {
		t.Fatal("a rewritten target did NOT change the plan body — the body differential is not sensitive")
	}
	if !strings.Contains(string(row.prep.Body), `"model":"rewrite-target-model"`) {
		t.Errorf("expected nanollm to rewrite the JSON model to the configured target, got %s", row.prep.Body)
	}
}

// TestStaticUnsignedExpiryControl proves the unsigned-plan assertion is
// meaningful: the real openai plan carries no expiry, while a synthetic plan with
// a past ExpiresAt reports Expired()==true.
func TestStaticUnsignedExpiryControl(t *testing.T) {
	descriptors := buildDescriptors(t)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, descriptors, nano, staticCaseByName(t, "StaticCompletion/ascii"))
	planassert.AssertUnsigned(t, row.prep)

	past := time.Now().Add(-time.Hour)
	expired := &nanollm.PreparedRequest{Meta: nanollm.PreparedMeta{ExpiresAt: &past}}
	if !expired.Expired() {
		t.Fatal("a plan with a past ExpiresAt must report Expired()==true — the unsigned assertion is not meaningful")
	}
}

// --- native-leg / fixture helpers. ---

// buildDescriptors runs the native build order over the fixture source and
// returns the prompt descriptors, asserting no declines.
func buildDescriptors(t *testing.T) map[string]promptdescriptor.Function {
	t.Helper()
	var files []nativeschema.SourceFile
	for _, path := range fixtureBamlPaths(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := bamlparser.ParseBytes(path, data)
		if err != nil {
			t.Fatalf("ParseBytes %s: %v", path, err)
		}
		files = append(files, nativeschema.SourceFile{File: f, Path: path})
	}
	if len(files) == 0 {
		t.Fatalf("no .baml files under %s", fixtureDir)
	}
	schemas, schemaDeclines := nativeschema.BuildStaticSchemas(files)
	descriptors, promptDeclines := nativeschema.BuildPromptDescriptors(files, schemas, schemaDeclines, fixtureClientProvider, nativeschema.BuildClientConfigs(files))
	if len(schemaDeclines) != 0 {
		t.Fatalf("unexpected BuildStaticSchemas declines: %v", schemaDeclines)
	}
	if len(promptDeclines) != 0 {
		t.Fatalf("unexpected BuildPromptDescriptors declines: %v", promptDeclines)
	}
	return descriptors
}

// fixtureBamlPaths returns the fixture .baml file paths in lexical order.
func fixtureBamlPaths(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", fixtureDir, err)
	}
	var paths []string
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".baml" {
			continue
		}
		paths = append(paths, filepath.Join(fixtureDir, e.Name()))
	}
	sort.Strings(paths)
	return paths
}

// bamlPinFromGoMod parses a go.mod with the canonical golang.org/x/mod/modfile
// parser (not a hand-rolled scanner) and returns the EXACT stock-BAML require
// version, whether the module is replaced, and whether the require was found.
// modfile handles require/replace, block form, and comments correctly, so the
// version comparison is an exact string match — a prerelease pin like
// v0.223.0-rc1 is reported verbatim and thus fails the == check, never satisfies
// a prefix.
func bamlPinFromGoMod(t *testing.T, path string) (version string, replaced, found bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mf, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, r := range mf.Require {
		if r.Mod.Path == bamlModulePath {
			version = r.Mod.Version
			found = true
		}
	}
	for _, rep := range mf.Replace {
		if rep.Old.Path == bamlModulePath {
			replaced = true
		}
	}
	return version, replaced, found
}
