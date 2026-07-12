//go:build integration

// Package staticoracle is the de-BAML Phase 3 slice-2 STATIC differential
// oracle. It proves the native static prompt renderer
// (internal/nativeprompt.RenderStatic / SupportsStatic, landed in slice 1 /
// #603) reproduces BAML v0.223.0 byte-for-byte across the claimed static
// corpus, and it does so with a dedicated, test-owned, UNTOUCHED stock BAML
// v0.223.0 generated Go client as the authority.
//
// Why a separate package (not package nativeprompt). The #597 dynamic oracle in
// internal/nativeprompt drives the in-process dynclient, which links the
// PATCHED BAML module (dynclient/baml-patched). This oracle links the STOCK
// upstream github.com/boundaryml/baml@v0.223.0 via the generated fixture
// client. Keeping the two in separate test binaries avoids linking two BAML
// Go/CFFI runtimes into one binary and makes the stock provenance obvious.
//
// How it works (scope section 7). Two independent legs render the SAME checked-
// in .baml source:
//
//   - NATIVE leg: read internal/nativeprompt/testdata/static_oracle/baml_src in
//     deterministic lexical order -> bamlparser.ParseBytes each file ->
//     nativeschema.BuildStaticSchemas -> nativeschema.BuildPromptDescriptors
//     (with the fixture client->provider constant) -> promptdescriptor.Function
//     -> nativeprompt.SupportsStatic -> nativeprompt.RenderStatic. This mirrors
//     the build-time ordering at cmd/introspect/main.go:1375-1445 WITHOUT
//     importing cmd/introspect or emitting any generated metadata.
//   - BAML leg: call the generated Request.<Function>(...) (BAML's BuildRequest)
//     with the same typed scalar values, read req.Body().Text(), and strictly
//     decode the OpenAI provider body. Request builds and returns the provider
//     HTTP request BEFORE any send; no network, no response, no RoundTripper.
//
// The two RenderedPrompt values are compared with nativeprompt.Equal (canonical
// JSON).
//
// One material deviation from the scope's slice-2 sketch, forced by BAML
// v0.223's ACTUAL behavior and documented here and in the oracle's asserts:
// the stock `openai` provider ALWAYS emits a POST to /chat/completions with a
// `messages` array (content as text blocks), never the legacy /completions
// endpoint with a `prompt` string. A role-less (RenderedPrompt::Completion)
// prompt is realized by the provider as a SINGLE `system` message whose text is
// the completion string. The oracle therefore compares native's RenderedPrompt
// against the provider realization BAML v0.223 actually produces
// (expectedProviderForm): a native completion must equal exactly one system
// message carrying the completion text. This still proves the Phase 3 claim —
// byte-exact RENDER of the static surface — because the completion-vs-chat and
// endpoint choice are provider-layer concerns owned by Phase 4/8, not the
// render-bytes claim under test here.
//
// Run:
//
//	CGO_ENABLED=1 go test -tags integration ./internal/nativeprompt/staticoracle
//
// Requires: CGO + the stock BAML v0.223 CFFI library (auto-located under the
// user BAML cache dir; downloaded on first use), exactly as the #597 oracle.
package staticoracle

import (
	stdjson "encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

const (
	// fixtureDir is the checked-in claimed .baml project (native leg source).
	fixtureDir = "../testdata/static_oracle/baml_src"
	// sourceMapPath is the generated client's embedded source map (drift guard).
	sourceMapPath = "../testdata/static_oracle/baml_client/baml_source_map.go"
	// rootGoModPath is the root module's go.mod, from the package CWD, for the
	// static dependency-pin assertion.
	rootGoModPath = "../../../go.mod"
	// wantBAMLModuleVersion is the exact stock go.mod dependency pin required.
	wantBAMLModuleVersion = "v0.223.0"
	// wantBAMLRuntimeVersion is the exact loaded CFFI runtime version required.
	// The CFFI reports the bare semver (no "v" prefix).
	wantBAMLRuntimeVersion = "0.223.0"
	// bamlModulePath is the stock BAML Go module path.
	bamlModulePath = "github.com/boundaryml/baml"
)

// fixtureClientProvider is the client->provider map the native leg feeds
// BuildPromptDescriptors. It is a fixture constant derived from the SAME
// checked-in clients.baml (StaticOracleClient -> provider openai, no shorthand),
// so no cmd/introspect enrichment is needed; Phase 3 never renders ctx.client.
var fixtureClientProvider = map[string]string{"StaticOracleClient": "openai"}

// ---------------------------------------------------------------------------
// Guard (a): exact stock-dependency version.
// ---------------------------------------------------------------------------

// TestBAMLVersionPinned requires the stock BAML runtime and module pin to be
// EXACTLY v0.223.0, so a green result can never be attributed to a different
// BAML runtime. It proves the version three ways, strongest first:
//
//  1. The LOADED CFFI runtime (baml_go.BamlVersion(), read from the native
//     library that actually built every provider request in this binary) must
//     report the exact runtime version. This is the load-bearing check.
//  2. The root go.mod dependency PIN must require the module at exactly
//     v0.223.0 and must not replace it, so the linked Go source is pinned too.
//  3. If debug.ReadBuildInfo populates module deps (it does NOT under `go
//     test` in Go workspace mode, where Deps is empty), a present entry must
//     agree and be unreplaced; an absent/empty dep list is tolerated because
//     workspace builds legitimately omit it.
func TestBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}

	version, replaced, found := bamlPinFromGoMod(t, rootGoModPath)
	if !found {
		t.Fatalf("root go.mod does not pin %s; the test-only dependency must be present", bamlModulePath)
	}
	if replaced {
		t.Fatalf("root go.mod replaces %s; the static oracle must link the stock module", bamlModulePath)
	}
	if version != wantBAMLModuleVersion {
		t.Fatalf("root go.mod pins %s %s, want exactly %s", bamlModulePath, version, wantBAMLModuleVersion)
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

// bamlPinFromGoMod scans go.mod for the require directive pinning the stock BAML
// module, returning its version, whether it is replaced, and whether it was
// found. It is a small line scan (not a full go.mod parser) sufficient for the
// single well-formed require/replace lines this repo uses.
func bamlPinFromGoMod(t *testing.T, path string) (version string, replaced, found bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range splitLines(string(data)) {
		fields := goModFields(line)
		// `replace github.com/boundaryml/baml => ...` or `github.com/boundaryml/baml v.. => ..`
		for i, f := range fields {
			if f == "=>" && containsField(fields[:i], bamlModulePath) {
				replaced = true
			}
		}
		// A require entry, in either valid form:
		//   - single-line:  `require github.com/boundaryml/baml v0.223.0 [// indirect]`
		//   - block line:   `github.com/boundaryml/baml v0.223.0 [// indirect]` (inside `require ( ... )`)
		// Strip a leading `require` keyword so both forms parse identically; the
		// `require (` block opener has fields `["require", "("]` and simply doesn't
		// match the module path below.
		reqFields := fields
		if len(reqFields) > 0 && reqFields[0] == "require" {
			reqFields = reqFields[1:]
		}
		if len(reqFields) >= 2 && reqFields[0] == bamlModulePath && len(reqFields[1]) > 0 && reqFields[1][0] == 'v' {
			version = reqFields[1]
			found = true
		}
	}
	return version, replaced, found
}

func goModFields(line string) []string {
	// Strip a trailing `// ...` comment, then split on whitespace.
	if idx := indexOf(line, "//"); idx >= 0 {
		line = line[:idx]
	}
	var fields []string
	cur := ""
	flush := func() {
		if cur != "" {
			fields = append(fields, cur)
			cur = ""
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == ' ' || c == '\t' || c == '\r' {
			flush()
			continue
		}
		cur += string(c)
	}
	flush()
	return fields
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Guard (b): generated source-map == checked-in fixture bytes.
// ---------------------------------------------------------------------------

// TestSourceMapDriftGuard parses the generated baml_source_map.go file_map
// literal and requires every embedded .baml byte string to equal the checked-in
// baml_src file of the same name, and the key set to match exactly. This proves
// the SAME source drives both the BAML leg (via the runtime built from the
// embedded map) and the native leg (via the on-disk files) — a stale generated
// client cannot masquerade as a pass.
func TestSourceMapDriftGuard(t *testing.T) {
	embedded := parseSourceMap(t, sourceMapPath)

	onDisk := map[string]string{}
	for _, path := range fixtureBamlPaths(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		onDisk[filepath.Base(path)] = string(data)
	}

	if len(embedded) != len(onDisk) {
		t.Fatalf("source-map file set %v != on-disk baml_src file set %v", keysOf(embedded), keysOf(onDisk))
	}
	for name, want := range onDisk {
		got, ok := embedded[name]
		if !ok {
			t.Errorf("generated source map is missing %q (present on disk)", name)
			continue
		}
		if got != want {
			t.Errorf("generated source map for %q drifted from the checked-in file:\n--- embedded ---\n%q\n--- on disk ---\n%q", name, got, want)
		}
	}
}

// parseSourceMap extracts the `var file_map = map[string]string{...}` literal
// from the generated Go file into a name->contents map, unquoting each Go string
// literal.
func parseSourceMap(t *testing.T, path string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse source map %s: %v", path, err)
	}

	out := map[string]string{}
	found := false
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "file_map" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("file_map is not a composite literal: %T", vs.Values[i])
				}
				found = true
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						t.Fatalf("file_map element is not a key/value: %T", elt)
					}
					key := unquote(t, kv.Key)
					val := unquote(t, kv.Value)
					out[key] = val
				}
			}
		}
	}
	if !found {
		t.Fatalf("no `file_map` var found in %s", path)
	}
	if len(out) == 0 {
		t.Fatalf("`file_map` in %s is empty", path)
	}
	return out
}

func unquote(t *testing.T, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("expected a string literal, got %T", e)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("unquote %q: %v", lit.Value, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Descriptor-set guard: built set == independently scanned source set, zero
// declines (scope section 6).
// ---------------------------------------------------------------------------

// TestDescriptorSetMatchesSource independently scans the fixture source for
// function declarations and requires the built prompt-descriptor key set to
// equal that source set exactly, with zero BuildStaticSchemas and
// BuildPromptDescriptors declines. This proves the claimed corpus is exactly
// the descriptor set — no function silently dropped, no extra descriptor.
func TestDescriptorSetMatchesSource(t *testing.T) {
	files := readFixtureFiles(t)

	schemas, schemaDeclines := nativeschema.BuildStaticSchemas(files)
	descriptors, promptDeclines := nativeschema.BuildPromptDescriptors(files, schemas, schemaDeclines, fixtureClientProvider, nativeschema.BuildClientConfigs(files))

	if len(schemaDeclines) != 0 {
		t.Errorf("expected zero BuildStaticSchemas declines, got %v", schemaDeclines)
	}
	if len(promptDeclines) != 0 {
		t.Errorf("expected zero BuildPromptDescriptors declines, got %v", promptDeclines)
	}

	got := map[string]bool{}
	for name := range descriptors {
		got[name] = true
	}
	want := scanSourceFunctionNames(t)

	if len(got) != len(want) {
		t.Fatalf("descriptor set %v != scanned source function set %v", keysOfBool(got), keysOfBool(want))
	}
	for name := range want {
		if !got[name] {
			t.Errorf("scanned source function %q has no built descriptor", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("built descriptor %q is not a scanned source function", name)
		}
	}

	// Every declared function must also have a typed generated-client closure and
	// a native args row in the parity table (and no extra closure), per scope
	// section 6 ("a table entry ... for every declared function and no extra").
	tableFns := map[string]bool{}
	for _, c := range oracleCases() {
		tableFns[c.fn] = true
	}
	for name := range want {
		if !tableFns[name] {
			t.Errorf("declared function %q has no parity-table case", name)
		}
	}
	for name := range tableFns {
		if !want[name] {
			t.Errorf("parity-table case %q is not a declared source function", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The claimed corpus + main byte-exact parity proof (scope section 6/7).
// ---------------------------------------------------------------------------

// oracleCase is one claimed-corpus row: a native args map keyed by descriptor
// argument name and a typed closure that drives the generated Request.<fn>
// method with the SAME Go values. wantKind pins the native RenderedPrompt kind
// so a native completion/chat regression is caught independently of BAML.
type oracleCase struct {
	name     string
	fn       string
	args     map[string]any
	wantKind nativeprompt.Kind
	build    func() (baml.HTTPRequest, error)
}

// oracleCases is the claimed corpus. It uses the same Go values on both legs
// (int64/float64 match the generated Go signatures) and runs at least two value
// rows for every scalar-sensitive function.
func oracleCases() []oracleCase {
	return []oracleCase{
		// StaticCompletion: literal text + direct string interpolation, KindCompletion.
		completionCase("StaticCompletion/ascii", "StaticCompletion",
			map[string]any{"topic": "cats and dogs"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("cats and dogs") }),
		completionCase("StaticCompletion/unicode_quotes_backslash", "StaticCompletion",
			map[string]any{"topic": "café ☕ \"q\" \\b"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("café ☕ \"q\" \\b") }),
		completionCase("StaticCompletion/embedded_newline", "StaticCompletion",
			map[string]any{"topic": "line1\nline2"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("line1\nline2") }),
		completionCase("StaticCompletion/inner_spaces", "StaticCompletion",
			map[string]any{"topic": "  padded  "},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletion("  padded  ") }),

		// StaticPrimitiveArgs: exact string/int/float/bool binding + scalar display.
		completionCase("StaticPrimitiveArgs/zeros", "StaticPrimitiveArgs",
			map[string]any{"text": "hi", "count": int64(0), "ratio": float64(0), "flag": false},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticPrimitiveArgs("hi", 0, 0, false) }),
		completionCase("StaticPrimitiveArgs/neg_frac_true", "StaticPrimitiveArgs",
			map[string]any{"text": "héllo", "count": int64(-1), "ratio": float64(3.14), "flag": true},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("héllo", -1, 3.14, true)
			}),
		completionCase("StaticPrimitiveArgs/int_max_exp", "StaticPrimitiveArgs",
			map[string]any{"text": "x", "count": int64(9223372036854775807), "ratio": float64(1e10), "flag": true},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("x", 9223372036854775807, 1e10, true)
			}),
		completionCase("StaticPrimitiveArgs/int_min_negfrac", "StaticPrimitiveArgs",
			map[string]any{"text": "y", "count": int64(-9223372036854775808), "ratio": float64(-2.5), "flag": false},
			func() (baml.HTTPRequest, error) {
				return bamlclient.Request.StaticPrimitiveArgs("y", -9223372036854775808, -2.5, false)
			}),

		// StaticRoleChat: ordered text-only system -> user -> assistant chat.
		chatCase("StaticRoleChat/basic", "StaticRoleChat",
			map[string]any{"topic": "weather", "count": int64(3)},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticRoleChat("weather", 3) }),
		chatCase("StaticRoleChat/unicode_zero", "StaticRoleChat",
			map[string]any{"topic": "the ☀️", "count": int64(0)},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticRoleChat("the ☀️", 0) }),

		// StaticOutputFormat: bare ctx.output_format in a chat message + primitive.
		chatCase("StaticOutputFormat/weather", "StaticOutputFormat",
			map[string]any{"topic": "weather"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticOutputFormat("weather") }),
		chatCase("StaticOutputFormat/unicode", "StaticOutputFormat",
			map[string]any{"topic": "café"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticOutputFormat("café") }),

		// StaticCompletionOutputFormat: bare ctx.output_format in a Completion.
		completionCase("StaticCompletionOutputFormat/weather", "StaticCompletionOutputFormat",
			map[string]any{"topic": "weather"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletionOutputFormat("weather") }),
		completionCase("StaticCompletionOutputFormat/trees", "StaticCompletionOutputFormat",
			map[string]any{"topic": "trees"},
			func() (baml.HTTPRequest, error) { return bamlclient.Request.StaticCompletionOutputFormat("trees") }),
	}
}

func completionCase(name, fn string, args map[string]any, build func() (baml.HTTPRequest, error)) oracleCase {
	return oracleCase{name: name, fn: fn, args: args, wantKind: nativeprompt.KindCompletion, build: build}
}

func chatCase(name, fn string, args map[string]any, build func() (baml.HTTPRequest, error)) oracleCase {
	return oracleCase{name: name, fn: fn, args: args, wantKind: nativeprompt.KindChat, build: build}
}

// TestStaticOracleParity is the core proof: for every claimed-corpus row, the
// native RenderStatic output equals the provider realization BAML v0.223.0
// actually builds, byte-for-byte via nativeprompt.Equal.
func TestStaticOracleParity(t *testing.T) {
	descriptors := buildDescriptors(t)

	for _, c := range oracleCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fn, ok := descriptors[c.fn]
			if !ok {
				t.Fatalf("no built descriptor for %q", c.fn)
			}

			// Native leg: SupportsStatic must accept, RenderStatic must succeed
			// with the pinned kind. After SupportsStatic returns nil, a render
			// failure is an invariant/parity failure, surfaced loudly.
			if err := nativeprompt.SupportsStatic(fn, c.args); err != nil {
				t.Fatalf("SupportsStatic declined a claimed case: %v", err)
			}
			native, err := nativeprompt.RenderStatic(fn, c.args)
			if err != nil {
				t.Fatalf("RenderStatic failed after SupportsStatic accepted: %v", err)
			}
			if native == nil {
				t.Fatal("RenderStatic returned a nil prompt after accepting")
			}
			if native.Kind != c.wantKind {
				t.Fatalf("native RenderStatic kind = %q, want %q", native.Kind, c.wantKind)
			}

			// BAML leg: build the provider request (no send) and read its body.
			req, err := c.build()
			if err != nil {
				t.Fatalf("BAML Request.%s: %v", c.fn, err)
			}
			assertChatCompletionsEndpoint(t, req)

			body, err := req.Body()
			if err != nil {
				t.Fatalf("BAML request Body(): %v", err)
			}
			text, err := body.Text()
			if err != nil {
				t.Fatalf("BAML body Text(): %v", err)
			}
			if text == "" {
				t.Fatal("BAML body is empty — request build failed before producing a body")
			}

			oracle, err := decodeOpenAIChat([]byte(text))
			if err != nil {
				t.Fatalf("decode BAML provider body: %v\nbody: %s", err, text)
			}

			// Compare native's RenderedPrompt (via its v0.223 provider realization)
			// against BAML's actual provider body.
			want := expectedProviderForm(native)
			equal, wantJSON, gotJSON, err := nativeprompt.Equal(want, oracle)
			if err != nil {
				t.Fatalf("Equal: %v", err)
			}
			if !equal {
				t.Errorf("native RenderStatic diverged from BAML v0.223 provider body\n--- native (provider form) ---\n%s\n--- BAML oracle ---\n%s\n--- raw body ---\n%s",
					wantJSON, gotJSON, text)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Guard (c): mismatch sensitivity.
// ---------------------------------------------------------------------------

// TestMismatchSensitivity mutates ONE in-memory native text byte and proves
// nativeprompt.Equal reports a mismatch, so a real divergence could never slip
// through as a false pass. No source is mutated.
func TestMismatchSensitivity(t *testing.T) {
	descriptors := buildDescriptors(t)
	fn, ok := descriptors["StaticCompletion"]
	if !ok {
		t.Fatalf("no built descriptor for %q", "StaticCompletion")
	}

	native, err := nativeprompt.RenderStatic(fn, map[string]any{"topic": "cats and dogs"})
	if err != nil {
		t.Fatalf("RenderStatic: %v", err)
	}
	baseline := expectedProviderForm(native)

	// Build the BAML oracle for the SAME case and confirm the un-mutated form
	// matches (so the mutation below is the only difference in play).
	req, err := bamlclient.Request.StaticCompletion("cats and dogs")
	if err != nil {
		t.Fatalf("BAML Request.StaticCompletion: %v", err)
	}
	text, err := bodyText(req)
	if err != nil {
		t.Fatalf("BAML body: %v", err)
	}
	oracle, err := decodeOpenAIChat([]byte(text))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if equal, _, _, _ := nativeprompt.Equal(baseline, oracle); !equal {
		t.Fatalf("precondition: baseline native must equal BAML oracle before mutation\nbody: %s", text)
	}

	// Flip exactly one byte of the native completion text in memory.
	mutated := mutateOneByte(native)
	equal, _, _, err := nativeprompt.Equal(expectedProviderForm(mutated), oracle)
	if err != nil {
		t.Fatalf("Equal after mutation: %v", err)
	}
	if equal {
		t.Fatal("Equal did not detect a one-byte native text mutation — the oracle is not sensitive")
	}
}

// mutateOneByte returns a copy of rp with exactly one byte of its first text
// payload flipped (completion string or first message's first text part).
func mutateOneByte(rp *nativeprompt.RenderedPrompt) *nativeprompt.RenderedPrompt {
	out := *rp
	switch out.Kind {
	case nativeprompt.KindCompletion:
		out.Completion = flipFirstByte(out.Completion)
	case nativeprompt.KindChat:
		msgs := make([]nativeprompt.RenderedMessage, len(out.Messages))
		copy(msgs, out.Messages)
		if len(msgs) > 0 && len(msgs[0].Parts) > 0 && msgs[0].Parts[0].Text != nil {
			parts := make([]nativeprompt.Part, len(msgs[0].Parts))
			copy(parts, msgs[0].Parts)
			flipped := flipFirstByte(*parts[0].Text)
			parts[0].Text = &flipped
			msgs[0].Parts = parts
		}
		out.Messages = msgs
	}
	return &out
}

func flipFirstByte(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	b[0] ^= 0x20
	return string(b)
}

// ---------------------------------------------------------------------------
// Native-leg helpers.
// ---------------------------------------------------------------------------

// readFixtureFiles reads and parses every fixture .baml file in deterministic
// lexical path order, exactly as the native build pipeline does.
func readFixtureFiles(t *testing.T) []nativeschema.SourceFile {
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
		t.Fatalf("no .baml files found under %s", fixtureDir)
	}
	return files
}

// buildDescriptors runs the native build order and returns the prompt
// descriptors, asserting no declines occurred.
func buildDescriptors(t *testing.T) map[string]promptdescriptor.Function {
	t.Helper()
	files := readFixtureFiles(t)
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

// scanSourceFunctionNames independently scans the raw fixture source for
// top-level `function <Name>(` declarations, without going through the
// descriptor builder, so the descriptor-set guard has an independent authority.
func scanSourceFunctionNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range fixtureBamlPaths(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, name := range scanFunctionDecls(string(data)) {
			out[name] = true
		}
	}
	return out
}

// scanFunctionDecls returns the names of top-level `function Name(` declarations
// in a .baml source. A declaration begins at the start of a line (comments start
// with `//`, so this never matches prose that mentions "function").
func scanFunctionDecls(src string) []string {
	var names []string
	for _, line := range splitLines(src) {
		rest, ok := cutPrefix(trimLeadingSpace(line), "function ")
		if !ok {
			continue
		}
		rest = trimLeadingSpace(rest)
		name := leadingIdent(rest)
		if name == "" {
			continue
		}
		after := trimLeadingSpace(rest[len(name):])
		if len(after) > 0 && after[0] == '(' {
			names = append(names, name)
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// BAML-leg strict decoder + comparison.
// ---------------------------------------------------------------------------

// assertChatCompletionsEndpoint is the harness-sanity guard (scope guard d,
// adapted to v0.223 reality): the stock openai provider POSTs every request —
// completion-shaped or chat-shaped — to /chat/completions. URL/header/body
// ordering are not a Phase 3 parity claim; this only confirms the harness hit
// the endpoint it thinks it did.
func assertChatCompletionsEndpoint(t *testing.T, req baml.HTTPRequest) {
	t.Helper()
	method, err := req.Method()
	if err != nil {
		t.Fatalf("request Method(): %v", err)
	}
	if method != "POST" {
		t.Fatalf("expected POST, got %q", method)
	}
	url, err := req.Url()
	if err != nil {
		t.Fatalf("request Url(): %v", err)
	}
	if !hasSuffix(url, "/chat/completions") {
		t.Fatalf("expected a /chat/completions endpoint, got %q", url)
	}
}

// decodeOpenAIChat strictly decodes BAML v0.223's OpenAI chat-completions body
// into a canonical KindChat RenderedPrompt, FAILING CLOSED on any shape outside
// the narrow text-only static claim: a legacy `prompt` field, a missing/non-
// array messages, a message with any key other than role/content, a missing/
// empty/non-string role, a null/missing content, a non-text content block, a
// content block with any key other than type/text, a non-string block text, an
// empty content array, tools, metadata, or an unknown shape. A malformed body
// is a HARNESS FAILURE, never a skipped part or a false pass — mirroring the
// #597 dynamic oracle's posture, not a permissive "best effort" normalizer.
func decodeOpenAIChat(body []byte) (*nativeprompt.RenderedPrompt, error) {
	var root map[string]any
	if err := stdjson.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	if _, ok := root["prompt"]; ok {
		return nil, fmt.Errorf("unexpected legacy `prompt` field: v0.223 openai must emit chat `messages`")
	}
	msgsAny, ok := root["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("body missing a `messages` array (got %T)", root["messages"])
	}
	if len(msgsAny) == 0 {
		return nil, fmt.Errorf("body has an empty `messages` array")
	}

	rp := &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindChat}
	for i, ma := range msgsAny {
		msg, ok := ma.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] is not an object: %T", i, ma)
		}
		// Fail closed on any key beyond role/content (tool_calls, name, refusal,
		// cache_control/metadata, function_call, ...). The static claim carries
		// none of these.
		for k := range msg {
			if k != "role" && k != "content" {
				return nil, fmt.Errorf("messages[%d] has an unexpected key %q (tools/metadata are outside the static claim)", i, k)
			}
		}
		role, ok := msg["role"].(string)
		if !ok || role == "" {
			return nil, fmt.Errorf("messages[%d] has a missing/empty/non-string role (%T %v)", i, msg["role"], msg["role"])
		}
		parts, err := decodeContent(msg["content"])
		if err != nil {
			return nil, fmt.Errorf("messages[%d] content: %w", i, err)
		}
		rp.Messages = append(rp.Messages, nativeprompt.RenderedMessage{
			Role:               role,
			AllowDuplicateRole: false, // not on the wire; the static corpus never sets it
			Meta:               nil,   // the static corpus carries no message metadata
			Parts:              parts,
		})
	}
	return rp, nil
}

// decodeContent canonicalizes an OpenAI message `content` (a plain string or an
// array of typed blocks) into ordered text Parts, failing closed on any non-text
// block or unexpected shape. v0.223 emits arrays of {"type":"text","text":...}.
func decodeContent(content any) ([]nativeprompt.Part, error) {
	switch c := content.(type) {
	case string:
		return []nativeprompt.Part{makeTextPart(c)}, nil
	case []any:
		if len(c) == 0 {
			return nil, fmt.Errorf("empty content array")
		}
		parts := make([]nativeprompt.Part, 0, len(c))
		for j, ba := range c {
			block, ok := ba.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content block[%d] is not an object: %T", j, ba)
			}
			for k := range block {
				if k != "type" && k != "text" {
					return nil, fmt.Errorf("content block[%d] has an unexpected key %q (non-text block outside the static claim)", j, k)
				}
			}
			btype, _ := block["type"].(string)
			if btype != "text" {
				return nil, fmt.Errorf("content block[%d] is a %q block, not text (media/tools are outside the static claim)", j, btype)
			}
			s, ok := block["text"].(string)
			if !ok {
				return nil, fmt.Errorf("content block[%d] text is missing or non-string (%T)", j, block["text"])
			}
			parts = append(parts, makeTextPart(s))
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("missing or unexpected content shape %T", content)
	}
}

// expectedProviderForm maps a native RenderedPrompt to the provider realization
// BAML v0.223's stock openai client actually builds:
//
//   - KindChat is emitted as-is (each message a text-only chat message).
//   - KindCompletion is realized as EXACTLY ONE `system` message whose single
//     text part is the completion string (the provider wraps a role-less prompt
//     rather than using the legacy /completions endpoint). Asserting exactly
//     this shape keeps the completion proof fail-closed: any other provider
//     realization makes nativeprompt.Equal report a mismatch.
func expectedProviderForm(native *nativeprompt.RenderedPrompt) *nativeprompt.RenderedPrompt {
	if native.Kind == nativeprompt.KindChat {
		return native
	}
	return &nativeprompt.RenderedPrompt{
		Kind: nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{{
			Role:               "system",
			AllowDuplicateRole: false,
			Meta:               nil,
			Parts:              []nativeprompt.Part{makeTextPart(native.Completion)},
		}},
	}
}

func makeTextPart(s string) nativeprompt.Part {
	v := s
	return nativeprompt.Part{Text: &v}
}

func bodyText(req baml.HTTPRequest) (string, error) {
	body, err := req.Body()
	if err != nil {
		return "", err
	}
	return body.Text()
}

// ---------------------------------------------------------------------------
// Small, dependency-free string helpers (kept local to the oracle binary).
// ---------------------------------------------------------------------------

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func trimLeadingSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return s[i:]
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// leadingIdent returns the maximal leading [A-Za-z_][A-Za-z0-9_]* of s.
func leadingIdent(s string) string {
	n := 0
	for n < len(s) {
		c := s[n]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (n > 0 && c >= '0' && c <= '9') {
			n++
			continue
		}
		break
	}
	return s[:n]
}
