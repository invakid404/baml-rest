//go:build integration && nanollm_integration

package opharness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// TestCapturesAgreeWithPredicatewire parses internal/debaml/predicatewire's source and
// proves every literal in [Captures] is byte-identical to the stock capture it came
// from.
//
// It is the same mechanism internal/debaml applies to its own untagged copies, and it
// exists for the same reason: this module is OUTSIDE go.work, so it cannot import the
// root package's test code at all, and a copy that could drift from its authority
// would make the live proofs assertions about nothing.
//
// The guard walks the composite literal rather than matching text, so a capture that
// was renamed or reshaped there surfaces here as a missing pairing instead of a silent
// pass. Build tags do not affect the parser, so the integration-tagged capture is
// readable from here.
func TestCapturesAgreeWithPredicatewire(t *testing.T) {
	authority := parsePredicatewireCaptures(t)
	if len(authority) == 0 {
		t.Fatal("no captures were read from internal/debaml/predicatewire; this guard would be vacuous")
	}
	if len(authority) != len(Captures) {
		t.Fatalf("predicatewire pins %d operator captures and this package copies %d; a capture would "+
			"be unguarded or a copy would have no authority", len(authority), len(Captures))
	}
	fields := map[string]func(Capture) string{
		"checkTrue":  func(c Capture) string { return c.CheckTrue },
		"checkFalse": func(c Capture) string { return c.CheckFalse },
		"assertTrue": func(c Capture) string { return c.AssertTrue },
		"assertFail": func(c Capture) string { return c.AssertFail },
	}
	for id, mine := range Captures {
		theirs, ok := authority[id]
		if !ok {
			t.Errorf("predicatewire no longer pins operator %q, so its copy here has no stock authority", id)
			continue
		}
		for name, get := range fields {
			want, ok := theirs[name]
			if !ok {
				t.Errorf("%s.%s has no counterpart in predicatewire's capture", id, name)
				continue
			}
			if got := get(mine); got != want {
				t.Errorf("%s.%s has drifted from the stock capture:\n got %s\nwant %s",
					id, name, strconv.Quote(got), strconv.Quote(want))
			}
		}
	}
	// NON-VACUITY: the guard must be able to tell two captures apart, or agreement
	// above would be a comparison that never discriminates.
	if Captures["gt"].CheckTrue == Captures["ge"].CheckTrue {
		t.Fatal("two operators' captures are identical, so the comparison discriminates nothing")
	}
	// And every capture really is its own operator's: stock retained the operator's
	// canonical text in both the check wire bytes and the assertion cause.
	for id, c := range Captures {
		want := Expression(id)
		for name, s := range map[string]string{
			"CheckTrue": c.CheckTrue, "CheckFalse": c.CheckFalse, "AssertFail": c.AssertFail,
		} {
			if !contains(s, want) {
				t.Errorf("%s.%s does not quote %q; the capture and the operator it authorises are "+
					"mispaired", id, name, want)
			}
		}
	}
	t.Logf("capture authority: %d operators x 4 stock rows, byte-identical to "+
		"internal/debaml/predicatewire's pinned literals", len(Captures))
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// repoRoot walks up from this file until it finds the marker every checkout has at its
// root, so the guard does not depend on the working directory `go test` chose.
//
// It FAILS rather than skipping when the root cannot be found: a guard that cannot
// locate its authority has not verified anything, and reporting that as a pass is the
// exact false green this file exists to prevent.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".embedignore")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate the repo root above %s", filepath.Dir(file))
	return ""
}

// parsePredicatewireCaptures reads pwOperatorCaptures out of
// internal/debaml/predicatewire's source as a map of operator id -> field -> literal.
func parsePredicatewireCaptures(t *testing.T) map[string]map[string]string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "debaml", "predicatewire", "operators_test.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "pwOperatorCaptures" || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("pwOperatorCaptures is no longer a composite literal; the guard cannot read it")
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					t.Fatal("a pwOperatorCaptures entry is not a key/value pair")
				}
				keyLit, ok := kv.Key.(*ast.BasicLit)
				if !ok || keyLit.Kind != token.STRING {
					t.Fatal("a pwOperatorCaptures key is not a string literal")
				}
				id, err := strconv.Unquote(keyLit.Value)
				if err != nil {
					t.Fatalf("unquote pwOperatorCaptures key %s: %v", keyLit.Value, err)
				}
				body, ok := kv.Value.(*ast.CompositeLit)
				if !ok {
					t.Fatalf("pwOperatorCaptures[%q] is not a composite literal", id)
				}
				fields := map[string]string{}
				for _, f := range body.Elts {
					fkv, ok := f.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					name, ok := fkv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					val, ok := fkv.Value.(*ast.BasicLit)
					if !ok || val.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(val.Value)
					if err != nil {
						t.Fatalf("unquote pwOperatorCaptures[%q].%s: %v", id, name.Name, err)
					}
					fields[name.Name] = unquoted
				}
				if len(fields) == 0 {
					t.Fatalf("pwOperatorCaptures[%q] yielded no string fields", id)
				}
				out[id] = fields
			}
		}
		return true
	})
	return out
}

// TestLiveManifestIsComplete is the completeness guard for the LIVE half of the Slice
// 7.2c-3 served manifest: 6 operators x 4 outcomes = 24 rows, split across six test
// binaries.
//
// The split is what makes the guard necessary. Each operator is proven in its own
// package (see this package's doc for why), so no single test can count the 24 — and a
// package that was dropped, or a fixture project that was never generated, would simply
// stop contributing rows with every remaining assertion still green. This walks the
// FILESYSTEM instead of a list: every non-`gt` capture must have both an isolated
// fixture project (with its own baml_src) and a test package that drives it, and `gt`
// must have neither, because it belongs to the main staticserve fixture.
func TestLiveManifestIsComplete(t *testing.T) {
	root := repoRoot(t)
	rows := 0
	for id := range Captures {
		project := filepath.Join(root, "internal", "nativeprompt", "testdata",
			"staticserve_op_fixtures", id)
		pkg := filepath.Join(root, "internal", "nativebody", "nanollmprepare", "staticserve", "op"+id)
		_, projectErr := os.Stat(filepath.Join(project, "baml_src"))
		_, pkgErr := os.Stat(pkg)

		if id == "gt" {
			// `>` is the MAIN staticserve fixture's, and it must not have acquired an
			// isolated twin — two live proofs of one operator would make the 24-row
			// tally wrong in the other direction.
			if projectErr == nil {
				t.Errorf("operator %q has an isolated fixture project, but `>` is the MAIN fixture's; "+
					"the live manifest would count it twice", id)
			}
			if pkgErr == nil {
				t.Errorf("operator %q has an isolated test package, but `>` is proven by the main "+
					"staticserve package", id)
			}
			rows += 4
			continue
		}
		if projectErr != nil {
			t.Errorf("operator %q has a stock capture but NO isolated fixture project (%v); its four "+
				"live rows are not driven", id, projectErr)
			continue
		}
		if pkgErr != nil {
			t.Errorf("operator %q has an isolated fixture project but NO test package (%v); the "+
				"project is generated and never driven", id, pkgErr)
			continue
		}
		// The project really declares THIS operator, so a package cannot be pointed at
		// the wrong fixture and still count.
		src, err := os.ReadFile(filepath.Join(project, "baml_src", "main.baml"))
		if err != nil {
			t.Errorf("operator %q: read baml_src/main.baml: %v", id, err)
			continue
		}
		if !contains(string(src), "{{ "+Expression(id)+" }}") {
			t.Errorf("operator %q's isolated project does not declare %q; the package and the fixture "+
				"have parted company", id, Expression(id))
			continue
		}
		// And it declares the two PINNED names — never a renamed one, which is the
		// broadening the 7.2c scope names as risk 7.
		for _, pinned := range []string{"class StaticCheckedAnswer {", "class StaticAssertAnswer {"} {
			if !contains(string(src), pinned) {
				t.Errorf("operator %q's isolated project does not declare `%s`; the class names must "+
					"stay pinned", id, pinned)
			}
		}
		rows += 4
	}
	if rows != 24 {
		t.Fatalf("the LIVE served manifest covers %d rows, want 24 (6 operators x 4 outcomes)", rows)
	}
	t.Logf("live manifest: %d rows across %d operators — `gt` in the main staticserve fixture and "+
		"%d isolated same-name projects, each with its own test binary", rows, len(Captures), len(Captures)-1)
}
