//go:build integration

package predicatewire

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The RESIDUAL LEDGER guard.
//
// residuals.md is proof material, not prose: it is the durable record of what 7.2c
// defers and on whose authority. A ledger nobody checks is a ledger that goes stale, so
// this file ties it to the code — every form captured from stock in this package must
// have a row, every row must be a decline, and every authority naming a test in this
// package must name one that exists.

const pwLedgerPath = "residuals.md"

// pwLedgerRow is one parsed ledger row.
type pwLedgerRow struct {
	id          string
	form        string
	disposition string
	authority   string
}

// pwParseLedger reads residuals.md and returns its table rows.
//
// The parse is strict on purpose: a malformed row is an error rather than a skipped line,
// because a silently skipped row is exactly how a deferral would stop being covered.
func pwParseLedger(t *testing.T) []pwLedgerRow {
	t.Helper()
	raw, err := os.ReadFile(pwLedgerPath)
	if err != nil {
		t.Fatalf("read %s: %v", pwLedgerPath, err)
	}
	var out []pwLedgerRow
	inTable := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 4 {
			t.Fatalf("ledger row has %d cells, want 4: %q", len(cells), line)
		}
		if cells[0] == "id" {
			inTable = true
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		if !inTable {
			t.Fatalf("a table row appears before the header: %q", line)
		}
		for i, cell := range cells {
			if cell == "" {
				t.Fatalf("ledger row %q has an empty cell at position %d", cells[0], i)
			}
		}
		out = append(out, pwLedgerRow{id: cells[0], form: cells[1], disposition: cells[2], authority: cells[3]})
	}
	if len(out) == 0 {
		t.Fatalf("%s has no table rows; every claim this file makes would be vacuous", pwLedgerPath)
	}
	return out
}

// pwLedgerAdmissionViolations is THE disposition checker: it returns one line per row
// whose disposition is not exactly `DECLINED`.
//
// It is a function rather than an inline loop so the assertion and its mutation proof
// drive the SAME code. A negative control that re-implements the check — or, worse, that
// only compares a fabricated field back to the row it was built from — proves that two
// strings differ, not that the invariant bites.
func pwLedgerAdmissionViolations(rows []pwLedgerRow) []string {
	var out []string
	for _, r := range rows {
		if r.disposition != "DECLINED" {
			out = append(out, fmt.Sprintf("row %q has disposition %q; every residual is DECLINED in 7.2c-1",
				r.id, r.disposition))
		}
	}
	return out
}

// TestResidualLedgerCoversEveryDeferral is the ledger's own guard.
func TestResidualLedgerCoversEveryDeferral(t *testing.T) {
	rows := pwParseLedger(t)
	byID := map[string]pwLedgerRow{}
	for _, r := range rows {
		if _, dup := byID[r.id]; dup {
			t.Errorf("ledger row %q appears twice", r.id)
		}
		byID[r.id] = r
	}
	// (1) NOTHING in the ledger may be admitted. This is the whole no-flip invariant,
	// restated over the document a reviewer reads. It runs through the SAME checker the
	// bite test drives a mutant through, so the two cannot drift apart.
	if got := pwLedgerAdmissionViolations(rows); len(got) != 0 {
		t.Errorf("the ledger records a non-DECLINED disposition:\n  %s", strings.Join(got, "\n  "))
	}

	// (2) Every form CAPTURED from stock in this package must have a row, or a
	// measurement would exist with no recorded disposition behind it.
	for _, res := range pwResiduals() {
		if _, ok := byID[res.ID]; !ok {
			t.Errorf("residual %q is captured from stock but has no ledger row", res.ID)
		}
	}
	// (3) And every operator this package captured but did NOT admit must have one too.
	for _, o := range pwOperators() {
		if o.Op == ">" {
			continue // the one admitted predicate; it is not a residual
		}
		if _, ok := byID["operator_"+o.ID]; !ok {
			t.Errorf("operator %q is captured and still declined, but has no ledger row", o.Op)
		}
	}
	if _, ok := byID["operator_gt"]; ok {
		t.Error("the ledger carries a row for `this > I`, which is the ADMITTED predicate, not a residual")
	}

	// (4) The scope's own deferral list, written out here independently so the ledger
	// and the scope can disagree. These are the families §"#583 durable deferrals"
	// names; a missing one is scope that quietly stopped being tracked.
	for _, want := range []string{
		"two_checks", "duplicate_labels", "check_then_assert", "assert_then_check",
		"compound_predicate", "filters_and_arithmetic",
		"type_float", "type_string", "type_bool", "type_enum", "type_nullable",
		"type_list", "type_map", "type_union", "type_nested_class", "type_media",
		"target_constraint", "class_constraint", "toplevel_constrained",
		"shape_third_field", "shape_reordered_fields", "shape_constraint_on_answer",
		"shape_two_constrained_fields", "shape_class_name", "shape_extra_definitions",
		"meta_alias", "meta_description", "meta_stream_dynamic",
		"label_non_ascii", "label_empty_present",
		"route_static_stream", "route_dynamic_final", "route_direct_parse", "route_call_with_raw",
	} {
		if _, ok := byID[want]; !ok {
			t.Errorf("the scope defers %q but the ledger has no row for it", want)
		}
	}

	// (4b) Every NUMERIC REFUSAL CLAUSE the boundary matrix can attribute a decline to
	// must have its own row. The three clauses are independent — a literal can be too
	// long while its value is well inside 2^53 (`9007199254740991` is 2^53-1 and sixteen
	// digits) — so one row cannot stand for another without misdescribing the residual
	// and understating what 7.2c-2 has to close.
	for _, c := range pwRefusalClauses() {
		if _, ok := byID[c.ledgerID]; !ok {
			t.Errorf("the boundary matrix attributes refusals to %q but the ledger has no row %q",
				c.name, c.ledgerID)
		}
	}

	// (5) Every authority naming a test must name one that EXISTS, in the package it
	// says. A citation to a test that was renamed away is a ledger row with nothing
	// behind it — and that is the failure mode a ledger has, so it is checked rather
	// than trusted.
	measured, cited, scoped := 0, 0, 0
	for _, r := range rows {
		pkg, name, ok := strings.Cut(r.authority, ":")
		if !ok {
			t.Errorf("ledger row %q has an authority with no package prefix: %q", r.id, r.authority)
			continue
		}
		switch pkg {
		case "predicatewire":
			measured++
		case "scope":
			scoped++
			continue
		default:
			cited++
		}
		dir, ok := pwAuthorityDirs()[pkg]
		if !ok {
			t.Errorf("ledger row %q cites unknown authority package %q", r.id, pkg)
			continue
		}
		if !pwAuthorityExists(t, dir, name) {
			t.Errorf("ledger row %q cites %s, which package %s does not declare", r.id, name, pkg)
		}
	}
	// (6) The BOUND, logged rather than implied. Rows whose authority is `scope:` carry
	// a decision with no measurement in this PR, and a reader must be able to see how
	// many there are without counting the table by hand.
	t.Logf("residual ledger: %d rows — %d MEASURED in this package against stock v0.223.0, "+
		"%d cited from sibling stock oracles, %d recorded from the scope with NO new measurement "+
		"in this PR (those are the rows a later slice must measure before moving them)",
		len(rows), measured, cited, scoped)
	if measured == 0 {
		t.Error("no ledger row is backed by a measurement in this package, which is what this PR exists to add")
	}
}

// pwAuthorityDirs maps an authority package prefix to the directory its declarations live
// in, relative to this package.
func pwAuthorityDirs() map[string]string {
	return map[string]string{
		"predicatewire": ".",
		"checkedwire":   "../checkedwire",
		"debaml":        "..",
	}
}

// pwAuthorityExists reports whether a cited authority is really declared in dir.
//
// An authority is either a Go test function or a checked-in proof FILE (checkedwire's
// asymmetries.md is the one of the latter kind: the non-ASCII truncation boundary is
// recorded there as an UNMEASURED hazard, which is a disposition without a test).
func pwAuthorityExists(t *testing.T, dir, name string) bool {
	t.Helper()
	if strings.HasSuffix(name, ".md") {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
		return true
	}
	return strings.Contains(pwPackageSources(t, dir), "func "+name+"(t *testing.T)")
}

// pwSourceCache memoizes each scanned directory: the sibling debaml package is ~500 KB of
// _test.go and every cited row would otherwise re-read it.
var pwSourceCache = map[string]string{}

// pwPackageSources concatenates a package's _test.go sources, so a citation can be checked
// against the declarations that actually exist.
func pwPackageSources(t *testing.T, dir string) string {
	t.Helper()
	if s, ok := pwSourceCache[dir]; ok {
		return s
	}
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatalf("glob %s sources: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s declares no _test.go files, so no citation into it can be verified", dir)
	}
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(raw)
	}
	pwSourceCache[dir] = b.String()
	return b.String()
}

// TestResidualLedgerGuardIsProvenToBite feeds the ledger parser and its checks a
// deliberately broken row and requires each to be reported.
//
// Without it, a parser that silently skipped malformed lines would make every assertion
// above vacuous — the ledger could be empty and the guard green.
func TestResidualLedgerGuardIsProvenToBite(t *testing.T) {
	rows := pwParseLedger(t)
	// (1) The parse must actually have found the table, not a prefix of it.
	if len(rows) < 40 {
		t.Fatalf("the parser found %d rows; the ledger covers substantially more, so it is stopping "+
			"early and the coverage checks above are only seeing part of the table", len(rows))
	}
	// (2) An ADMITTED disposition must be DETECTED BY THE REAL CHECKER. The mutant is
	// routed through [pwLedgerAdmissionViolations] — the same function
	// TestResidualLedgerCoversEveryDeferral asserts on — rather than compared against
	// itself, so this proves the invariant bites instead of proving that "ADMITTED"
	// differs from "DECLINED".
	if got := pwLedgerAdmissionViolations(rows); len(got) != 0 {
		t.Fatalf("the UNMUTATED ledger already reports a violation, so the mutant below would prove "+
			"nothing:\n  %s", strings.Join(got, "\n  "))
	}
	for i := range rows {
		mutated := append([]pwLedgerRow(nil), rows...)
		mutated[i].disposition = "ADMITTED"
		got := pwLedgerAdmissionViolations(mutated)
		if len(got) != 1 {
			t.Fatalf("flipping row %q to ADMITTED produced %d violation(s), want exactly 1: %v",
				rows[i].id, len(got), got)
		}
		if !strings.Contains(got[0], rows[i].id) {
			t.Errorf("flipping row %q to ADMITTED was reported against a different row: %s",
				rows[i].id, got[0])
		}
	}
	// And a disposition that is neither word — the silent-typo case — must also be
	// reported, so the check is "is DECLINED" rather than "is not ADMITTED".
	typo := append([]pwLedgerRow(nil), rows...)
	typo[0].disposition = "declined"
	if got := pwLedgerAdmissionViolations(typo); len(got) != 1 {
		t.Fatalf("a lower-case `declined` produced %d violation(s), want exactly 1; the checker accepts "+
			"a disposition it never validated: %v", len(got), got)
	}
	// (3) The citation check must report a name that is NOT declared. The absent name is
	// assembled at run time from fragments, so the literal cannot appear in the sources
	// being scanned and accidentally satisfy its own negative control.
	absent := "Test" + "AbsentAuthority" + "NegativeControl"
	for pkg, dir := range pwAuthorityDirs() {
		if pwAuthorityExists(t, dir, absent) {
			t.Fatalf("package %s appears to declare %s, so the citation check cannot detect a stale "+
				"citation", pkg, absent)
		}
		if !pwAuthorityExists(t, dir, "TestResidualLedgerCoversEveryDeferral") &&
			!pwAuthorityExists(t, dir, "TestServingOracleBoundaryLock") &&
			!pwAuthorityExists(t, dir, "TestStockAssertIsNotACheck") {
			t.Errorf("the citation check found NO known test in %s, so it would report every row in "+
				"that package as stale rather than checking them", pkg)
		}
	}
	// And a missing FILE authority must be detectable too, since one row cites one.
	if pwAuthorityExists(t, "../checkedwire", "definitely-not-a-file.md") {
		t.Fatal("the file-authority check accepts a file that does not exist")
	}
	found := false
	for _, r := range rows {
		if strings.HasPrefix(r.authority, "predicatewire:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no row cites a test in this package, so the citation check never runs")
	}
}
