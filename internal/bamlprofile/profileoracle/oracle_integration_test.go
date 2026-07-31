//go:build integration

package profileoracle

// Stock BAML v0.223.0 differential for internal/bamlprofile's get_env engine
// config. It links the UNTOUCHED github.com/boundaryml/baml@v0.223.0 runtime via
// CFFI (exactly as internal/nativeprompt/staticoracle does) and renders the
// whole corpus through it, then compares byte-for-byte against the pure-Go
// profile leg. Stock BAML is the authority; a Go-only golden is not.
//
// Run:
//
//	CGO_ENABLED=1 go test -tags integration ./internal/bamlprofile/profileoracle
//
// Requires: CGO + the stock BAML v0.223 CFFI library (auto-located under the
// user BAML cache dir; downloaded on first use), exactly as the staticoracle.
//
// Regenerate the recorded version/source-map fixture after an intended corpus
// change:
//
//	WRITE_PROFILE_ORACLE_FIXTURE=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/bamlprofile/profileoracle -run TestProfileOracleFixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"unicode"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"golang.org/x/mod/modfile"
)

const (
	wantBAMLRuntimeVersion = "0.223.0"  // CFFI reports bare semver
	wantBAMLModuleVersion  = "v0.223.0" // go.mod pin
	bamlModulePath         = "github.com/boundaryml/baml"
	rootGoModPath          = "../../../go.mod"
	fixturePath            = "testdata/profile_oracle.json"
)

// guardFixture is the checked-in record tying a green run to its exact authority
// and corpus (the "BAML revision + source-map guard" the scope requires).
type guardFixture struct {
	BAMLRuntimeVersion string `json:"baml_runtime_version"`
	BAMLModuleVersion  string `json:"baml_module_version"`
	SourceSHA256       string `json:"baml_source_sha256"`
	RowCount           int    `json:"row_count"`
	Note               string `json:"note"`
}

// sourceSHA256 hashes the generated .baml project deterministically.
func sourceSHA256(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write([]byte(files[n]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestProfileOracleFixture is the version + source-map drift guard. It proves the
// loaded CFFI runtime, the go.mod pin, and the corpus source all match the
// checked-in record — so a green differential cannot be attributed to a
// different BAML or a drifted corpus.
func TestProfileOracleFixture(t *testing.T) {
	// Guard the authority FIRST — before either the write-regen or the compare
	// branch — so a regen in a bad environment (wrong CFFI runtime, replaced/
	// mispinned module) fails fast instead of baking bad values into the fixture.
	assertBAMLAuthority(t)

	// Record OBSERVED authority values (assertBAMLAuthority above already proved
	// they equal the wanted pins), not the constants — so the recorded module
	// version reflects what go.mod actually pins rather than being self-fulfilling.
	observedModule, _, _ := bamlPinFromGoMod(t, rootGoModPath)
	files := GenerateBAMLSource(Corpus())
	live := guardFixture{
		BAMLRuntimeVersion: baml_go.BamlVersion(),
		BAMLModuleVersion:  observedModule,
		SourceSHA256:       sourceSHA256(files),
		RowCount:           len(Corpus()),
		Note:               "stock BAML v0.223.0 get_env differential for internal/bamlprofile (Slice 2 PR-1)",
	}

	if os.Getenv("WRITE_PROFILE_ORACLE_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := stdjson.MarshalIndent(live, "", "  ")
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if err := os.WriteFile(fixturePath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", fixturePath)
		return
	}

	want := loadFixture(t)
	if live.BAMLRuntimeVersion != want.BAMLRuntimeVersion || live.BAMLModuleVersion != want.BAMLModuleVersion {
		t.Fatalf("version drift: live %+v, fixture %+v", live, want)
	}
	if live.SourceSHA256 != want.SourceSHA256 || live.RowCount != want.RowCount {
		t.Fatalf("corpus source drift: live sha=%s rows=%d, fixture sha=%s rows=%d.\n"+
			"If the corpus change is intended, regenerate:\n"+
			"  WRITE_PROFILE_ORACLE_FIXTURE=1 CGO_ENABLED=1 go test -tags integration ./internal/bamlprofile/profileoracle -run TestProfileOracleFixture",
			live.SourceSHA256, live.RowCount, want.SourceSHA256, want.RowCount)
	}
}

func loadFixture(t *testing.T) guardFixture {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture %s: %v (regenerate with WRITE_PROFILE_ORACLE_FIXTURE=1)", fixturePath, err)
	}
	var f guardFixture
	if err := stdjson.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

// assertBAMLAuthority fails the test unless the loaded CFFI runtime is exactly the
// pinned stock BAML version and the root go.mod pins that module unreplaced — the
// three-way check that makes a green (or a regenerated fixture) attributable to
// stock BAML v0.223 and nothing else.
func assertBAMLAuthority(t *testing.T) {
	t.Helper()
	// Load-bearing: the CFFI runtime that actually renders every row.
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}
	if v, replaced, found := bamlPinFromGoMod(t, rootGoModPath); !found {
		t.Fatalf("root go.mod does not pin %s", bamlModulePath)
	} else if replaced {
		t.Fatalf("root go.mod replaces %s; the differential must link the stock module", bamlModulePath)
	} else if v != wantBAMLModuleVersion {
		t.Fatalf("root go.mod pins %s %s, want %s", bamlModulePath, v, wantBAMLModuleVersion)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep == nil || dep.Path != bamlModulePath {
				continue
			}
			if dep.Replace != nil {
				t.Fatalf("build info reports %s replaced (%s)", bamlModulePath, dep.Replace.Path)
			}
			if dep.Version != wantBAMLModuleVersion {
				t.Fatalf("build info reports %s %s, want %s", bamlModulePath, dep.Version, wantBAMLModuleVersion)
			}
		}
	}
}

// TestProfileDifferential is the byte-exact proof: every corpus row rendered
// through stock BAML v0.223 equals the profile leg.
func TestProfileDifferential(t *testing.T) {
	assertBAMLAuthority(t)

	rows := Corpus()
	files := GenerateBAMLSource(rows)
	env := envVars()

	rt, err := baml.CreateRuntime("./baml_src", files, env)
	if err != nil {
		t.Fatalf("CreateRuntime from in-memory corpus source: %v", err)
	}

	for _, r := range rows {
		t.Run(r.ID, func(t *testing.T) {
			bamlMsgs := bamlRender(t, &rt, r, env)

			if r.Chat {
				// equalMessages byte-compares each (role, text). This is byte-exact
				// on the observable MESSAGE CONTENT: BAML trims each chat part and
				// drops empties when it builds the provider messages (jinja-runtime
				// lib.rs:448-449, verified via CFFI), and the profile's SplitChat
				// applies the identical trim+drop, so both legs are normalized the
				// same way. BAML never exposes the pre-lowering bytes, so the message
				// content is the strongest available comparison (see role_trim_content).
				profOut, err := RenderProfile(r)
				if err != nil {
					t.Fatalf("profile render: %v", err)
				}
				profMsgs, err := SplitChat(profOut)
				if err != nil {
					t.Fatalf("profile chat split: %v (rendered %q)", err, profOut)
				}
				if !equalMessages(profMsgs, bamlMsgs) {
					t.Errorf("chat mismatch\nprofile: %#v\nbaml:    %#v", profMsgs, bamlMsgs)
				}
				return
			}

			// Completion: stock openai realizes a role-less prompt as exactly one
			// system message whose text is the rendered bytes (staticoracle proves
			// this provider realization for v0.223).
			if len(bamlMsgs) != 1 || bamlMsgs[0].Role != "system" {
				t.Fatalf("completion expected exactly one system message, got %#v", bamlMsgs)
			}
			profOut, err := RenderProfile(r)
			if err != nil {
				t.Fatalf("profile render: %v", err)
			}
			if profOut != bamlMsgs[0].Text {
				t.Errorf("byte mismatch\nprofile: %q\nbaml:    %q", profOut, bamlMsgs[0].Text)
			}
		})
	}
}

// TestRawStringDelimiterRoundtrip proves the dynamic raw-string fence in
// functionSource lets a prompt body containing the raw-string terminator ("#,
// "##, ... up to BAML's 5-hash max) round-trip through stock BAML CreateRuntime
// with NO truncation: the bytes stock BAML parses and renders equal the profile
// leg exactly. The old fixed #"..."# fence would end the raw string at the first
// interior "#, so CreateRuntime would fail to parse the corpus (or render fewer
// bytes than the profile leg) — this is the differential proof of the fix.
func TestRawStringDelimiterRoundtrip(t *testing.T) {
	assertBAMLAuthority(t)
	rows := []Row{
		// Literal terminators of increasing hash length, each with a live {{ s }}
		// so the row is a real template, not an inert literal. hash4 needs a 5-hash
		// fence — BAML's maximum — exercising the top of the representable range.
		{ID: "raw_delim_hash1", Params: []Param{{Name: "s", BamlType: "string"}},
			Args: map[string]any{"s": "Z"}, Template: `head {{ s }} "#tail`},
		{ID: "raw_delim_hash2", Params: []Param{{Name: "s", BamlType: "string"}},
			Args: map[string]any{"s": "Z"}, Template: `head {{ s }} "##tail`},
		{ID: "raw_delim_hash4_max", Params: []Param{{Name: "s", BamlType: "string"}},
			Args: map[string]any{"s": "Z"}, Template: `head {{ s }} "####tail`},
		// The reported vector itself: an arbitrary regex pattern (rx) whose literal
		// carries a quote+hash. '"' and '#' are literals in both Rust `regex` and Go
		// (scanPattern accepts them), so the match is byte-exact — the point here is
		// that the generated .baml is not truncated by the interior "#.
		rx("raw_delim_rx", `x"#y`, `x\"#y`),
	}
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateBAMLSource(rows), env)
	if err != nil {
		t.Fatalf("CreateRuntime with raw-string-terminator bodies (fence truncated the .baml?): %v", err)
	}
	for _, r := range rows {
		t.Run(r.ID, func(t *testing.T) {
			bamlMsgs := bamlRender(t, &rt, r, env)
			if len(bamlMsgs) != 1 || bamlMsgs[0].Role != "system" {
				t.Fatalf("completion expected one system message, got %#v", bamlMsgs)
			}
			profOut, err := RenderProfile(r)
			if err != nil {
				t.Fatalf("profile render: %v", err)
			}
			if profOut != bamlMsgs[0].Text {
				t.Errorf("byte mismatch (truncation?)\nprofile: %q\nbaml:    %q", profOut, bamlMsgs[0].Text)
			}
		})
	}
}

// rx builds a regex_match row: subject s, and the regex arg literal exactly as it
// appears inside the jinja "..." (already jinja-escaped, e.g. \\d for a \d).
func rx(id, s, regexArgLiteral string) Row {
	return Row{
		ID:       id,
		Params:   []Param{{Name: "s", BamlType: "string"}},
		Args:     map[string]any{"s": s},
		Template: `{{ s|regex_match("` + regexArgLiteral + `") }}`,
	}
}

// TestRegexNeverOutdo is the regex_match G2 PROOF. It runs a WIDE spread — every
// retained accept family with a DISCRIMINATING subject (a matching ASCII range, a
// braced \x{<0x80}, a U-flag, a `\D` true on a non-digit, a {1000} whose subject
// is 1000 chars so the boundary is compiled AND matched) plus EVERY reviewer
// counterexample (digit/non-ASCII named groups, `\<`/`\>` and their class forms,
// `{,1}`/`{1,x}`/`{1`/`{}`/`{`/`{1,0}`, `[\d-\D]`/`[\d-a]`/`[\D-a]`/`[]`/`[^]`/
// `[z-a]`, `\xA`, `a**`) — through BOTH stock BAML v0.223 and the profile, and
// asserts profile => BAML for EVERY row (never profile-true / BAML-false).
//
// This holds by construction: rustRegexToGo is strict decline-by-default (accepts
// only the explicitly-recognized Go==Rust-safe grammar in scanPattern) with a
// compile-after-scan backstop (G1). Each row is either byte-exact (profile==BAML)
// or a conservative under-match (profile false, BAML true); ZERO rows may be
// profile-true/BAML-false. It supersedes the earlier "assert divergence" pins:
// with the strict whitelist the out-do constructs now DECLINE to false, so the
// boundary is one-directional. The reported count is logged.
func TestRegexNeverOutdo(t *testing.T) {
	assertBAMLAuthority(t)
	rows := []Row{
		// --- whitelist ACCEPT (expect byte-exact) ---
		rx("ok_ascii_class_t", "abc123", `[0-9]+`),
		rx("ok_ascii_class_f", "abcdef", `[0-9]+`),
		rx("ok_unicode_digit", "\u0663\u0664", `^\\d+$`),
		rx("ok_notdigit", "\u0663", `\\D`),
		rx("ok_digit_class", "\u0663", `^[\\d]+$`),
		rx("ok_range", "m", `^[a-z]$`),
		rx("ok_negclass", "z", `^[^abc]$`),
		rx("ok_alt", "dog", `^(cat|dog)$`),
		rx("ok_group", "abab", `^(?:ab)+$`),
		rx("ok_quant", "aaa", `^a{2,5}$`),
		rx("ok_casefold_ascii", "\u00c9", `(?i)^\u00e9$`),
		rx("ok_literal_nonascii", "caf\u00e9", `^caf\u00e9$`),
		rx("ok_escaped_dot_t", "a.b", `^a\\.b$`),
		rx("ok_escaped_dot_f", "axb", `^a\\.b$`),
		rx("ok_dot_cr", "\r", `^.$`),
		rx("ok_dot_nl", "\n", `^.$`),
		rx("ok_dot_s_nl", "\n", `(?s)^.$`),
		rx("ok_multiline", "a\nb", `(?m)^b$`),
		rx("ok_anchor_nl", "a\n", `^a$`),
		rx("ok_hex_lo", "A", `^\\x41$`),
		// --- round-4 G1 blocker: {n>1000} declined (was Go-invalid) ---
		rx("g1_repeat_over", "a", `^a{1001}$`),
		rx("g1_repeat_at_limit_f", "a", `^a{1000}$`),
		// --- round-4 out-do cases: now DECLINED (must be profile => BAML) ---
		rx("nou_D", "a", `(?-u:^\\D$)`),
		rx("nou_S", "a", `(?-u:^\\S$)`),
		rx("nou_W", "!", `(?-u:^\\W$)`),
		rx("nou_dot", "a", `(?-u:^.$)`),
		rx("nou_negclass", "b", `(?-u:^[^a]$)`),
		rx("nou_pASCII", "A", `(?-u:^\\p{ASCII}$)`),
		rx("nou_pGreek", "\u03b1", `(?-u:^\\p{Greek}$)`),
		rx("nou_Pcomplement", "1", `(?-u:^\\P{L}$)`),
		rx("nou_class_pGreek", "\u03b1", `(?-u:^[\\p{Greek}]$)`),
		rx("nou_inline_p", "\u03b1", `(?u)(?-u)^\\p{Greek}$`),
		rx("nou_posix_neg", "1", `(?-u:^[[:^alpha:]]$)`),
		rx("nou_class_nonascii", "\u00e9", `(?-u:^[\u00e9]$)`),
		rx("nou_class_range_nonascii", "\u00e9", `(?-u:^[a-\u00e9]$)`),
		rx("nou_hex_hi", "a", `(?-u:\\x{80})`),
		rx("nou_accept_range_underdo", "m", `(?-u:^[a-z]$)`),
		rx("nou_accept_digit_underdo", "3", `(?-u:\\d)`),
		rx("octal_backslash123", "S", `^\\123$`),
		rx("nul", "\x00", `^\\0$`),
		rx("quote_QE", "[a]", `^\\Q[a]\\E$`),
		rx("casefold_kelvin", "\u212a", `(?i-u:^k$)`),
		rx("casefold_longs", "\u017f", `(?-u)(?i)^s$`),
		rx("flag_R", "a\r\n", `(?mR:^a$)`),
		rx("flag_R_dot", "\r\n", `(?mR:^.$)`),
		rx("flag_x", "3", `(?x-u: \\d )`),
		rx("class_setop", "ab", `[a&&b]`),
		rx("pname_whitespace", "a b", `\\p{White_Space}`),
		rx("uni_S_nbsp", "\u00a0", `^\\S$`),
		rx("uni_W_eacute", "\u00e9", `^\\W$`),
		rx("uni_s_space", "a b", `\\s`),
		rx("uni_w_word", "abc_1", `^\\w+$`),
		rx("uni_w_nonascii", "\u00e9", `^\\w+$`),
		rx("uni_b_word", "\u00e9a", `^\u00e9\\ba$`),
		// --- retained accept families, discriminating subjects (expect byte-exact) ---
		rx("ok_hex_braced", "\u007f", `^\\x{7f}$`), // subject IS byte 0x7f (DEL) so the braced-hex accept genuinely matches
		// Escaped `/` is a literal in BOTH Rust `regex` and Go (escaping ASCII
		// punctuation is allowed), so `\/` is byte-exact — NOT an out-do (stock
		// BAML v0.223 CFFI: `^\/$` on "/" -> true; `^[\/]$` on "/" -> true).
		rx("ok_escaped_slash", "/", `^\\/$`),
		rx("ok_escaped_slash_class", "/", `^[\\/]$`),
		rx("ok_uflag", "aaa", `(?U)^a+$`),
		rx("ok_range_mid", "m", `^[a-z]$`),
		rx("ok_class_shorthand", "ab3", `^[a-z\\d]+$`),
		rx("ok_hyphen_literal", "-", `^[-a]$`),
		rx("ok_D_true", "a", `\\D`),
		rx("ok_quant_1000", strings.Repeat("a", 1000), `^a{1000}$`),
		// --- round-5 counterexamples: now DECLINED -> profile false (must be => BAML) ---
		rx("x_named_digit_P", "a", `^(?P<1>a)$`),
		rx("x_named_digit_lt", "a", `^(?<1>a)$`),
		rx("x_named_valid", "a", `^(?P<n>a)$`),
		rx("x_named_nonascii", "a", "^(?P<é>a)$"),
		rx("x_esc_lt", "<", `^\\<$`),
		rx("x_esc_gt", ">", `^\\>$`),
		rx("x_class_esc_lt", "<", `^[\\<]$`),
		rx("x_brace_comma1", "a{,1}", `^a{,1}$`),
		rx("x_brace_1x", "a{1,x}", `^a{1,x}$`),
		rx("x_brace_unclosed", "a{1", `^a{1$`),
		rx("x_brace_empty", "a{}", `^a{}$`),
		rx("x_brace_bare", "{", `^{$`),
		rx("x_brace_rev", "a", `^a{1,0}$`),
		rx("x_class_dD", "a", `[\\d-\\D]`),
		rx("x_class_da", "٣", `[\\d-a]`),
		rx("x_class_Da", "a", `[\\D-a]`),
		rx("x_class_empty", "a", `[]`),
		rx("x_class_empty_neg", "a", `[^]`),
		rx("x_class_reversed", "a", `[z-a]`),
		rx("x_hex_short", "a", `\\xA`),
		rx("x_star_star", "a", `a**`),
		// --- round-7 counterexamples: leading-zero repeats, duplicate flags, \D Unicode skew ---
		rx("x_lz_repeat", "a{01}", `^a{01}$`),
		rx("x_lz_repeat_pair", "a{0001,0002}", `^a{0001,0002}$`),
		rx("x_lz_repeat_open", "a{0001,}", `^a{0001,}$`),
		rx("x_dup_flag", "a", `(?ii)^a$`),
		rx("x_dup_flag_scoped", "a", `(?ii:^a$)`),
		rx("x_dup_flag_dash", "a", `(?i-i)^a$`),
		rx("x_D_uni16", "\U00010D40", `^\\D$`),
		rx("x_D_class_uni16", "\U00010D40", `^[\\D]$`),
		rx("ud_d_uni16", "\U00010D40", `^\\d$`),
	}
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateBAMLSource(rows), env)
	if err != nil {
		t.Fatalf("CreateRuntime for never-outdo rows: %v", err)
	}
	var byteExact, underMatch, outDo int
	for _, r := range rows {
		bamlMsgs := bamlRender(t, &rt, r, env)
		if len(bamlMsgs) != 1 || bamlMsgs[0].Role != "system" {
			t.Fatalf("%s: expected one system message, got %#v", r.ID, bamlMsgs)
		}
		bamlText := bamlMsgs[0].Text
		profText, err := RenderProfile(r)
		if err != nil {
			t.Fatalf("%s: profile render: %v", r.ID, err)
		}
		switch {
		case profText == "true" && bamlText != "true":
			outDo++
			t.Errorf("OUT-DO (G2 VIOLATION) %s: profile=true BAML=%q — profile more permissive than BAML", r.ID, bamlText)
		case profText == bamlText:
			byteExact++ // genuinely identical rendered bytes
		default: // profile under-matches (its "true" is a subset of BAML's)
			underMatch++
		}
	}
	t.Logf("never-outdo proof: %d rows — %d byte-exact, %d conservative under-match, %d OUT-DO",
		len(rows), byteExact, underMatch, outDo)
	if outDo != 0 {
		t.Fatalf("G2 VIOLATED: %d rows are profile-true/BAML-false (must be zero)", outDo)
	}
}

// TestRegexDigitUnicodeGuard pins that Go's \p{Nd} set — the target of the
// `\d` -> `\p{Nd}` translation — is a SUBSET of stock BAML v0.223's `\d` set.
// That subset relation is exactly what makes `\d` never out-do: a Go match
// (rune in Go-Nd) implies a BAML match (rune in BAML-Nd). Go's Unicode table can
// lag BAML's, which only makes `\d` under-match on a newer digit (safe). This
// test FAILS only if Go's table ever contains a decimal digit BAML's lacks —
// the signal to decline `\d` too. It reuses one function and renders every Go
// \p{Nd} code point through the stock CFFI.
func TestRegexDigitUnicodeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the ~680-code-point Nd guard (one BuildRequest per point) in -short mode")
	}
	assertBAMLAuthority(t)
	guard := rx("guard_digit", "", `^\\d$`)
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateBAMLSource([]Row{guard}), env)
	if err != nil {
		t.Fatalf("CreateRuntime for digit guard: %v", err)
	}
	checked := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Nd, r) {
			continue
		}
		guard.Args = map[string]any{"s": string(r)}
		msgs := bamlRender(t, &rt, guard, env)
		if len(msgs) != 1 || msgs[0].Text != "true" {
			t.Errorf("Go \\p{Nd} U+%04X is NOT a stock-BAML \\d digit -> \\d could out-do; decline \\d", r)
		}
		checked++
	}
	t.Logf("Go-Nd subset of BAML-\\d guard: %d Go \\p{Nd} code points, all are stock-BAML digits", checked)
}

// bamlRender builds the provider request for a row via stock BAML and decodes the
// rendered messages. It never sends the request.
func bamlRender(t *testing.T, rt *baml.BamlRuntime, r Row, env map[string]string) []Message {
	t.Helper()
	kwargs := map[string]any{"stream": false}
	for k, v := range r.Args {
		kwargs[k] = v
	}
	args := baml.BamlFunctionArguments{Kwargs: kwargs, Env: env}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	req, err := rt.BuildRequest(context.Background(), r.FuncName(), encoded)
	if err != nil {
		t.Fatalf("BuildRequest(%s): %v", r.FuncName(), err)
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("request Body(): %v", err)
	}
	text, err := body.Text()
	if err != nil {
		t.Fatalf("body Text(): %v", err)
	}
	msgs, err := decodeMessages([]byte(text))
	if err != nil {
		t.Fatalf("decode provider body: %v\nbody: %s", err, text)
	}
	return msgs
}

// decodeMessages strictly decodes BAML v0.223's OpenAI chat-completions body
// into ordered role+text messages, failing closed on any shape outside the
// text-only get_env claim (a legacy `prompt`, a non-text content block, a
// missing role/content). Mirrors staticoracle.decodeOpenAIChat, reduced to the
// role+concatenated-text this differential compares.
func decodeMessages(body []byte) ([]Message, error) {
	var root map[string]any
	if err := stdjson.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	if _, ok := root["prompt"]; ok {
		return nil, fmt.Errorf("unexpected legacy `prompt` field")
	}
	msgsAny, ok := root["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("body missing a `messages` array (got %T)", root["messages"])
	}
	if len(msgsAny) == 0 {
		return nil, fmt.Errorf("empty `messages` array")
	}
	out := make([]Message, 0, len(msgsAny))
	for i, ma := range msgsAny {
		msg, ok := ma.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] is not an object: %T", i, ma)
		}
		role, ok := msg["role"].(string)
		if !ok || role == "" {
			return nil, fmt.Errorf("messages[%d] missing/empty role", i)
		}
		text, err := decodeContentText(msg["content"])
		if err != nil {
			return nil, fmt.Errorf("messages[%d] content: %w", i, err)
		}
		out = append(out, Message{Role: role, Text: text})
	}
	return out, nil
}

func decodeContentText(content any) (string, error) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var b strings.Builder
		for j, ba := range c {
			block, ok := ba.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content block[%d] is not an object: %T", j, ba)
			}
			if bt, _ := block["type"].(string); bt != "text" {
				return "", fmt.Errorf("content block[%d] is a %q block, not text", j, bt)
			}
			s, ok := block["text"].(string)
			if !ok {
				return "", fmt.Errorf("content block[%d] text missing/non-string", j)
			}
			b.WriteString(s)
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("missing/unexpected content shape %T", content)
	}
}

func equalMessages(a, b []Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// envVars returns the process environment as BAML expects it (matching the
// generated client's getEnvVars). No corpus client reads an env var — every
// client carries a literal key — so this only satisfies the runtime's signature.
func envVars() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// bamlPinFromGoMod parses go.mod (via golang.org/x/mod/modfile) for the stock
// BAML require/replace, returning the pinned version, whether it is replaced, and
// whether it was found.
func bamlPinFromGoMod(t *testing.T, path string) (version string, replaced, found bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, req := range f.Require {
		if req.Mod.Path == bamlModulePath {
			version = req.Mod.Version
			found = true
		}
	}
	for _, rep := range f.Replace {
		if rep.Old.Path == bamlModulePath {
			replaced = true
		}
	}
	return version, replaced, found
}
