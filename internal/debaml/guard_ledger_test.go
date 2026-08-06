package debaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The GUARD-REMOVAL LEDGER, and the drift guard that keeps it honest.
//
// testdata/guard_ledger/ledger.json is the single source: guard_ledger.md is
// RENDERED from it and byte-compared here, so the human-readable inventory can
// never disagree with the machine-readable one. The witness rows it cites are
// checked against the live stock-CFFI corpus by
// internal/debaml/guardledger.TestGuardLedgerCoversEveryLedgerRecord, which is
// where the evidence actually lives — this file only proves the two files agree
// and that every record is complete.
//
// The record struct is duplicated in the harness package rather than exported
// from here: the ledger is proof material for a TEST-ONLY slice, and a
// production symbol carrying it would put a test concern in the customer build.

// nonBAMLName marks a callable BAML's feature set does not have at all, so it
// has no signature to look up — [withdrawNonBAMLBuiltins] replaces it with an
// unknown-name stub.
const nonBAMLName = "\x00non-baml"

const (
	ledgerJSONPath     = "testdata/guard_ledger/ledger.json"
	ledgerMarkdownPath = "guard_ledger.md"
	// ledgerWriteEnv rewrites guard_ledger.md from the JSON instead of comparing.
	ledgerWriteEnv = "GUARD_LEDGER_MARKDOWN_WRITE"
)

// guardLedgerCallable is ONE CALLABLE's inventory entry. Scope §1 requires every
// name listed in the two broad default-decline tables — and every wrapper
// application over them — to be its own record rather than a table-level one, so
// the manifest can prove completeness and attach the deferral to each retained
// decline individually.
//
// The admission SHAPE is not stored here: it is rendered from the live
// [provenSignatures] / [withdrawnBuiltins] tables when the markdown is built, so
// it cannot drift from what the profile actually enforces.
type guardLedgerCallable struct {
	// Callable is the identity checkCallParity uses, prefixed by its kind:
	// "filter:upper", "test:is odd", "global:range".
	Callable string `json:"callable"`
	// Tables lists which broad table(s) name it.
	Tables []string `json:"tables"`
	// Wrapper is the guard applied to every call of it.
	Wrapper string `json:"wrapper"`
	// Admission is the pinned rendering of its admitted call shape, re-derived
	// from the live tables and byte-compared (see renderCallableAdmission).
	Admission      string   `json:"admission"`
	Disposition    string   `json:"disposition"`
	WitnessRows    []string `json:"witnessRows"`
	DeferralRecord string   `json:"deferralRecord"`
	Notes          string   `json:"notes"`
}

type guardLedgerRecord struct {
	Key               string   `json:"key"`
	Name              string   `json:"name"`
	File              string   `json:"file"`
	Class             string   `json:"class"`
	Disposition       string   `json:"disposition"`
	ForkCapability    string   `json:"forkCapability"`
	WitnessRows       []string `json:"witnessRows"`
	StockEnvelope     string   `json:"stockEnvelope"`
	NativeEnvelope    string   `json:"nativeEnvelope"`
	Effect            string   `json:"effect"`
	SubsumedBy        string   `json:"subsumedBy"`
	Rollback          string   `json:"rollback"`
	SubprocessWitness string   `json:"subprocessWitness"`
	// LivenessProof names the in-package test(s) that prove a KEPT guard exists
	// and executes at the seam it sits on, for a guard no witness row can reach.
	LivenessProof  string `json:"livenessProof"`
	DeferralRecord string `json:"deferralRecord"`
	Notes          string `json:"notes"`
}

func loadGuardLedger(t *testing.T) []guardLedgerRecord {
	t.Helper()
	raw, err := os.ReadFile(ledgerJSONPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerJSONPath, err)
	}
	var doc struct {
		Records   []guardLedgerRecord   `json:"records"`
		Callables []guardLedgerCallable `json:"callables"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// An unknown field means the schema moved and this reader did not; refusing
	// it is what keeps the markdown a faithful projection of the JSON.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", ledgerJSONPath, err)
	}
	if err := expectSingleJSONDocument(dec); err != nil {
		t.Fatalf("%s: %v", ledgerJSONPath, err)
	}
	if len(doc.Records) == 0 {
		t.Fatalf("%s carries no records", ledgerJSONPath)
	}
	return doc.Records
}

// expectSingleJSONDocument reports whether the decoder consumed the WHOLE input.
//
// encoding/json stops at the end of the first value, so a file holding a valid
// ledger followed by anything else — a second document, a truncation artefact, a
// merge conflict's leftovers — decodes cleanly and the trailing bytes are never
// seen. The canonical evidence ledger has to be exactly one complete document,
// so the reader checks for a second token rather than assuming there is none.
func expectSingleJSONDocument(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("carries data after the first JSON document (token err: %v); the ledger must be "+
			"exactly one complete document, or part of it is proof nothing reads", err)
	}
	return nil
}

func loadGuardLedgerCallables(t *testing.T) []guardLedgerCallable {
	t.Helper()
	raw, err := os.ReadFile(ledgerJSONPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerJSONPath, err)
	}
	var doc struct {
		Records   []guardLedgerRecord   `json:"records"`
		Callables []guardLedgerCallable `json:"callables"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", ledgerJSONPath, err)
	}
	if err := expectSingleJSONDocument(dec); err != nil {
		t.Fatalf("%s: %v", ledgerJSONPath, err)
	}
	if len(doc.Callables) == 0 {
		t.Fatalf("%s carries no callable inventory", ledgerJSONPath)
	}
	return doc.Callables
}

// callableFacts is everything about a callable that the ledger's inventory
// claims and the live registries can settle: which tables list it, which wrapper
// is applied to it, and the key checkCallParity looks its signature up by.
//
// Deriving all three (rather than only the name) is what makes the inventory's
// provenance columns regression-proof: relabelling a callable's table or wrapper
// no longer stays green.
type callableFacts struct {
	// Key is what checkCallParity looks up, or [nonBAMLName] for a name BAML's
	// feature set does not have.
	Key string
	// Tables is the sorted set of tables that list this callable.
	Tables []string
	// Wrapper is the guard applied to every call of it.
	Wrapper string
}

// Table and wrapper labels. They are constants so the inventory and the
// derivation cannot drift apart by a typo.
const (
	tableProfileGuards     = "installProfileGuards"
	tableWithdrawnBuiltins = "withdrawnBuiltins"
	tableForeignMapping    = "guardForeignMapping"
	tableGlobalWithdrawals = "globalWithdrawals"
	tableNonBAML           = "withdrawNonBAMLBuiltins"

	wrapperFilter       = "guardIntegerResult"
	wrapperTest         = "guardTestInput"
	wrapperDivisibleBy  = "guardTestInput + the divisibleby guard"
	wrapperGlobal       = "none — a global reaches neither wrapper"
	wrapperStub         = "unknown-name stub"
	wrapperUnregistered = "none — the profile never registers it"
)

// callableUniverse is every callable the profile governs, derived from the LIVE
// tables rather than restated. It is the set the ledger's callable inventory
// must cover exactly.
//
// Deriving it is the point: a filter added to [profileFilterBuiltins], a test
// added to [profileTestBuiltins], a name added to [withdrawnBuiltins] or a
// global added to [withdrawnGlobals] appears here immediately, and the coverage
// test then fails until it has an inventory entry.
func callableUniverse(t *testing.T) map[string]callableFacts {
	t.Helper()
	foreign := map[string]bool{}
	for _, name := range foreignMappingFilters {
		foreign[name] = true
	}

	out := map[string]callableFacts{}
	// add refuses a duplicate id. Several sources feed this map — the filter
	// table, the test table, the globals, the non-BAML stubs — and a silent
	// overwrite would let one source's facts stand in for another's, which is
	// exactly the provenance the coverage test then compares against.
	add := func(id string, f callableFacts) {
		if prior, seen := out[id]; seen {
			t.Fatalf("callable %q is claimed by two sources (%+v and %+v); the universe must name each callable once",
				id, prior, f)
		}
		out[id] = f
	}
	for name := range profileFilterBuiltins {
		tables := []string{tableProfileGuards}
		if withdrawnBuiltins[name] {
			tables = append(tables, tableWithdrawnBuiltins)
		}
		if foreign[name] {
			tables = append(tables, tableForeignMapping)
		}
		sort.Strings(tables)
		add("filter:"+name, callableFacts{Key: name, Tables: tables, Wrapper: wrapperFilter})
	}
	for name := range profileTestBuiltins {
		key := "is " + name
		tables := []string{tableProfileGuards}
		if withdrawnBuiltins[key] {
			tables = append(tables, tableWithdrawnBuiltins)
		}
		sort.Strings(tables)
		add("test:"+key, callableFacts{Key: key, Tables: tables, Wrapper: wrapperTest})
	}
	// Registered outside the table so it can carry its own guard.
	add("test:is divisibleby", callableFacts{
		Key: "is divisibleby", Tables: []string{tableProfileGuards}, Wrapper: wrapperDivisibleBy})
	add("global:range", callableFacts{
		Key: "range", Tables: []string{tableGlobalWithdrawals}, Wrapper: wrapperGlobal})
	for _, name := range withdrawnGlobals {
		add("global:"+name, callableFacts{
			Key: name, Tables: []string{tableGlobalWithdrawals}, Wrapper: wrapperGlobal})
	}
	// The five names BAML's feature set does not have. They are replaced by
	// unknown-name stubs rather than registered, so they are in no other table.
	stub := func(id string) {
		add(id, callableFacts{Key: nonBAMLName, Tables: []string{tableNonBAML}, Wrapper: wrapperStub})
	}
	stub("filter:" + nonBAMLFilter)
	stub("test:is " + nonBAMLTest)
	for _, name := range nonBAMLGlobals {
		stub("global:" + name)
	}
	// A name may be WITHDRAWN without ever being registered — `truncate` is one —
	// and an inventory that dropped it could not prove completeness. Such a name
	// carries no wrapper, because nothing wraps a filter that does not exist.
	for name := range withdrawnBuiltins {
		id := "filter:" + name
		key := name
		if strings.HasPrefix(name, "is ") {
			id, key = "test:"+name, name
		}
		if _, registered := out[id]; registered {
			continue
		}
		add(id, callableFacts{Key: key, Tables: []string{tableWithdrawnBuiltins}, Wrapper: wrapperUnregistered})
	}
	return out
}

// renderCallableAdmission states a callable's admitted call shape from the live
// tables: what checkCallParity will accept, in the terms scope §1 asks for
// (subject kind, arity, positional kinds, kwargs, special values).
func renderCallableAdmission(key string) string {
	if key == nonBAMLName {
		return "not part of BAML's feature set: raises an unknown-name error in every shape"
	}
	if withdrawnBuiltins[key] {
		return "withdrawn: declines in every shape"
	}
	sig, ok := provenSignatures[key]
	if !ok {
		return "no proven signature: declines in every shape"
	}
	parts := []string{
		fmt.Sprintf("subject {%s}", kindSetNames(sig.subject)),
		fmt.Sprintf("arity %d..%d", sig.minArgs, sig.maxArgs),
	}
	if len(sig.args) == 0 {
		parts = append(parts, "no positional arguments")
	} else {
		var as []string
		for _, a := range sig.args {
			as = append(as, "{"+kindSetNames(a)+"}")
		}
		parts = append(parts, "positional ["+strings.Join(as, ", ")+"]")
	}
	parts = append(parts, "kwargs rejected")
	if countDefaultingFilters[key] {
		parts = append(parts, "a count below 1 is refused")
	}
	if coercingNumericArg[key] {
		parts = append(parts, "a numeric argument is coerced rather than type-checked")
	} else if len(sig.args) > 0 {
		parts = append(parts, "a non-integral numeric argument is refused")
	}
	return strings.Join(parts, "; ")
}

// kindSetNames spells a kindSet for the manifest.
func kindSetNames(k kindSet) string {
	all := []struct {
		bit  kindSet
		name string
	}{
		{kUndefined, "undefined"}, {kNone, "none"}, {kBool, "bool"}, {kNumber, "number"},
		{kString, "string"}, {kBytes, "bytes"}, {kSeq, "seq"}, {kMap, "map"}, {kIterable, "iterable"},
	}
	var out []string
	for _, a := range all {
		if k&a.bit != 0 {
			out = append(out, a.name)
		}
	}
	if len(out) == 0 {
		return "nothing"
	}
	return strings.Join(out, "|")
}

// scopeGuards is every guard and behaviour the 7.2a scope's §1 inventory names,
// including both broad default-decline tables. The ledger must carry a record
// for each, whether or not it moved — an inventory with a hole in it is not an
// inventory.
var scopeGuards = []string{
	"withdrawNonBAMLBuiltins",
	"displayString",
	"numericProfile",
	"integerResultWrappers",
	"lengthGuard",
	"splitWithdrawal",
	"lastMappingGuard",
	"itemsTojsonMappingGuards",
	"mappingDualRender",
	"guardForeignMapping",
	"rangeWithdrawal",
	"globalWithdrawals",
	"operatorShapeIsProven",
	"installProfileGuardsTable",
	"withdrawnBuiltinsTable",
	"checkCallParitySignatures",
	"hasMedia",
	"divisibleByZero",
	"divisibleByNonIntegral",
	"pycompatUnknownMethod",
}

// TestGuardLedgerCoversTheWholeScopeInventory fails if a guard the scope names
// has no record, or if the ledger grew one the scope does not name.
func TestGuardLedgerCoversTheWholeScopeInventory(t *testing.T) {
	have := map[string]bool{}
	for _, r := range loadGuardLedger(t) {
		if have[r.Key] {
			t.Errorf("duplicate ledger record %q", r.Key)
		}
		have[r.Key] = true
	}
	want := map[string]bool{}
	for _, k := range scopeGuards {
		want[k] = true
		if !have[k] {
			t.Errorf("scope §1 names guard %q but the ledger carries no record for it", k)
		}
	}
	keys := make([]string, 0, len(have))
	for k := range have {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !want[k] {
			t.Errorf("the ledger carries a record for %q, which is not in the scope §1 inventory", k)
		}
	}
}

// TestGuardLedgerCoversEveryCallable is the answer to "the ledger must inventory
// every listed callable, not the table".
//
// It enumerates the AUTHORITATIVE tables — the live [profileFilterBuiltins],
// [profileTestBuiltins], [withdrawnBuiltins] and [withdrawnGlobals] — and
// requires one inventory entry per callable, in both directions. A filter added
// to the profile without a ledger entry fails here, and so does an entry for a
// callable the profile no longer governs.
//
// It also re-derives each entry's admitted call SHAPE from the same tables and
// byte-compares it, so the manifest cannot claim an arity, subject set, kwargs
// rule or special-value rule the profile does not actually enforce.
func TestGuardLedgerCoversEveryCallable(t *testing.T) {
	universe := callableUniverse(t)
	if len(universe) == 0 {
		t.Fatal("the profile governs no callable; this coverage check would pass over an empty universe")
	}
	have := map[string]guardLedgerCallable{}
	for _, c := range loadGuardLedgerCallables(t) {
		if _, dup := have[c.Callable]; dup {
			t.Errorf("duplicate callable inventory entry %q", c.Callable)
		}
		have[c.Callable] = c
	}

	ids := make([]string, 0, len(universe))
	for id := range universe {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		facts := universe[id]
		c, ok := have[id]
		if !ok {
			t.Errorf("the profile governs callable %q but the ledger has no inventory entry for it", id)
			continue
		}
		if want := renderCallableAdmission(facts.Key); c.Admission != want {
			t.Errorf("callable %q records the admitted shape %q, but the live tables enforce %q",
				id, c.Admission, want)
		}
		// PROVENANCE, compared exactly rather than for non-emptiness. Relabelling
		// a callable's table or wrapper is a claim about where it is governed
		// from, and the live registries settle it.
		got := append([]string(nil), c.Tables...)
		sort.Strings(got)
		if !slices.Equal(got, facts.Tables) {
			t.Errorf("callable %q claims to be listed by %v, but the live tables list it in %v",
				id, got, facts.Tables)
		}
		if c.Wrapper != facts.Wrapper {
			t.Errorf("callable %q claims wrapper %q, but the profile applies %q", id, c.Wrapper, facts.Wrapper)
		}
		if c.Disposition != "kept" {
			t.Errorf("callable %q carries disposition %q; 7.2a-1 removes no callable from either table", id, c.Disposition)
		}
		if c.Notes == "" {
			t.Errorf("callable %q carries no note", id)
		}
		// Every RETAINED DECLINE owes the deferral link. A callable that declines
		// in every shape is one; a proven-signature callable that still answers
		// inside its shape is not, and neither are the five non-BAML names, whose
		// rows agree with stock. The exception is derived from the LIVE registry
		// (the callable's key is the non-BAML sentinel), not from the entry's own
		// label, so it cannot be claimed by relabelling.
		declinesOutright := strings.Contains(c.Admission, "declines in every shape")
		nonBAML := facts.Key == nonBAMLName
		if declinesOutright && !nonBAML && c.DeferralRecord == "" {
			t.Errorf("callable %q declines in every shape but links no deferral record", id)
		}
		if nonBAML && c.DeferralRecord != "" {
			t.Errorf("callable %q is a non-BAML name whose rows agree, so it costs no coverage and owes no deferral", id)
		}
	}
	for id := range have {
		if _, ok := universe[id]; !ok {
			t.Errorf("the ledger inventories callable %q, which the profile does not govern", id)
		}
	}
}

// TestGuardLedgerRecordsAreComplete checks the per-record obligations that do
// not need the stock corpus. The evidence obligations that DO need it — a
// removal's rows being green, an over-decline actually costing coverage — are
// checked in internal/debaml/guardledger against the live legs.
func TestGuardLedgerRecordsAreComplete(t *testing.T) {
	validClass := map[string]bool{"P": true, "A": true, "U": true,
		"P per operator family": true, "P per grammar family": true,
		"P after lifecycle rows": true, "P for ordered ConstraintValue maps": true,
		"P for small bounded ranges only": true, "A for non-string keys": true}
	validDisposition := map[string]bool{
		"removed": true, "kept-inert": true, "kept-over-decline": true,
		"kept-unwitnessable": true, "kept-unprovable": true,
	}
	for _, r := range loadGuardLedger(t) {
		for _, f := range []struct{ name, value string }{
			{"name", r.Name}, {"file", r.File}, {"class", r.Class},
			{"disposition", r.Disposition}, {"forkCapability", r.ForkCapability},
			{"stockEnvelope", r.StockEnvelope}, {"nativeEnvelope", r.NativeEnvelope},
			{"effect", r.Effect}, {"rollback", r.Rollback}, {"notes", r.Notes},
		} {
			if strings.TrimSpace(f.value) == "" {
				t.Errorf("ledger record %q has an empty %s", r.Key, f.name)
			}
		}
		if !validClass[r.Class] {
			t.Errorf("ledger record %q carries class %q, which is not one of the scope's P/A/U classifications", r.Key, r.Class)
		}
		if !validDisposition[r.Disposition] {
			t.Errorf("ledger record %q carries an unknown disposition %q", r.Key, r.Disposition)
		}
		switch r.Disposition {
		case "removed":
			if r.SubsumedBy == "" {
				t.Errorf("ledger record %q is REMOVED but names no guard that now carries its refusals", r.Key)
			}
			if len(r.WitnessRows) == 0 {
				t.Errorf("ledger record %q is REMOVED but cites no witness row", r.Key)
			}
			if r.DeferralRecord != "" {
				t.Errorf("ledger record %q is REMOVED but links a deferral record", r.Key)
			}
		case "kept-unprovable":
			if r.DeferralRecord == "" {
				t.Errorf("ledger record %q is kept because its removal is unprovable but links no deferral record", r.Key)
			}
			// A guard no ROW can reach still has to be shown to EXIST and to
			// EXECUTE, or "kept" is indistinguishable from "silently deleted".
			if r.LivenessProof == "" {
				t.Errorf("ledger record %q is kept although no witness row can observe it, so it owes an in-package "+
					"liveness proof; without one, deleting the guard would leave every test green", r.Key)
			}
		case "kept-over-decline", "kept-unwitnessable":
			if r.DeferralRecord == "" {
				// Name the record's OWN disposition: the two share an obligation
				// but not a reason, and a red run that called an unwitnessable
				// guard an over-decline would send the reader looking for a
				// coverage cost that does not exist.
				t.Errorf("ledger record %q is kept (%s) but links no deferral record; the standing rule is that a "+
					"parity-affecting deferral is logged the moment it is made", r.Key, r.Disposition)
			}
		case "kept-inert":
			if r.DeferralRecord != "" {
				t.Errorf("ledger record %q is kept as INERT but links a deferral record; an inert guard costs no coverage", r.Key)
			}
			// Inertness is a MEASURED claim — "its rows agree with stock" — so a
			// record making it owes at least one row. With none, the greenness
			// check downstream iterates an empty list and passes vacuously,
			// which is indistinguishable from a guard nobody ever witnessed.
			if len(r.WitnessRows) == 0 {
				t.Errorf("ledger record %q is kept as INERT but cites no witness row; inertness is a measured "+
					"claim and an empty list proves it vacuously", r.Key)
			}
		}
		// A guard that is not removed must NOT claim a subsuming guard: the
		// removal argument and the retention argument are different claims and
		// mixing them is how a retained guard quietly reads as removable.
		if r.Disposition != "removed" && r.SubsumedBy != "" {
			t.Errorf("ledger record %q is not removed but names a subsuming guard %q", r.Key, r.SubsumedBy)
		}
	}
}

// TestGuardLedgerMarkdownIsRendered pins guard_ledger.md as a pure function of
// ledger.json. Rewrite it with GUARD_LEDGER_MARKDOWN_WRITE=1.
func TestGuardLedgerMarkdownIsRendered(t *testing.T) {
	want := renderGuardLedgerMarkdown(loadGuardLedger(t), loadGuardLedgerCallables(t))
	if os.Getenv(ledgerWriteEnv) != "" {
		if err := os.WriteFile(ledgerMarkdownPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", ledgerMarkdownPath, err)
		}
		t.Logf("%s rewritten from %s", ledgerMarkdownPath, ledgerJSONPath)
		return
	}
	got, err := os.ReadFile(ledgerMarkdownPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerMarkdownPath, err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale: it no longer matches %s.\nRegenerate with %s=1.",
			ledgerMarkdownPath, ledgerJSONPath, ledgerWriteEnv)
	}
}

// renderGuardLedgerMarkdown projects the records into the in-repo manifest.
func renderGuardLedgerMarkdown(records []guardLedgerRecord, callables []guardLedgerCallable) string {
	var b strings.Builder
	b.WriteString(`<!-- GENERATED from testdata/guard_ledger/ledger.json — do not edit by hand.
     Regenerate with: GUARD_LEDGER_MARKDOWN_WRITE=1 go test ./internal/debaml -run TestGuardLedgerMarkdownIsRendered -->

# de-BAML constraint-evaluator guard-removal ledger

One entry per compensation guard in the native constraint evaluator, whether or
not it moved.

The evaluator was written against the UPSTREAM pure-Go minijinja port and now
compiles against the BAML-exact fork ` + "`github.com/invakid404/minijinja-go/v2`" + `.
The fork's PATCHES.md says what the ENGINE does; it does not say what stock BAML
v0.223 does with a given value, so it is capability evidence and never removal
authority. Every entry below therefore cites **witness rows**: generated ` + "`.baml`" + `
methods plus exact raw model JSON, driven through the real stock CFFI by
` + "`internal/debaml/guardledger`" + `, which records the stock outcome envelope FIRST
and then compares the native leg against that recording.

A guard is removed only when all of its rows are **green** — stock and native
envelopes agree, either because both decided the same thing or because both
refused. A row where stock decides and native refuses is a measured **coverage
cost**, not a removal licence.

Nothing here changes admission. Every constraint-bearing bundle still declines at
` + "`checkSupported`" + `, so a removal widens only the internal evaluator's ANSWER
surface, which production does not reach.

## Summary

| Guard | File | Class | Disposition | Rows | Effect |
|---|---|---|---|---|---|
`)
	for _, r := range records {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %d | %s |\n",
			cell("`"+r.Key+"`"), cell(shortFile(r.File)), cell(r.Class),
			cell("**"+r.Disposition+"**"), len(r.WitnessRows), cell(r.Effect)))
	}

	b.WriteString(`
## Per-callable inventory

Scope §1 treats each name in the two broad default-decline tables — and each
wrapper application over them — as its own inventory record rather than as part
of one table-level entry, so completeness is provable and the deferral attaches
to each retained decline individually.

The rows below are derived from the LIVE tables (` + "`profileFilterBuiltins`, `profileTestBuiltins`, `withdrawnBuiltins`, `withdrawnGlobals`" + `),
and the admitted call shape is re-derived from ` + "`provenSignatures`" + ` when this file
is rendered. A callable added to the profile without an entry here fails
` + "`TestGuardLedgerCoversEveryCallable`" + `, and an entry whose shape disagrees with the
live table fails it too.

Nothing in this table is removed by 7.2a-1. "declines in every shape" is a
retained over-decline and links the deferral record; a callable with a proven
signature still answers inside that shape.

| Callable | Listed by | Wrapper | Admitted call shape | Rows |
|---|---|---|---|---|
`)
	for _, c := range callables {
		rows := "—"
		if len(c.WitnessRows) > 0 {
			rows = strings.Join(backtickAll(c.WitnessRows), " ")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			cell("`"+c.Callable+"`"), cell(strings.Join(backtickAll(c.Tables), ", ")),
			cell(c.Wrapper), cell(c.Admission), cell(rows))
	}

	b.WriteString("\n## Entries\n")
	for _, r := range records {
		fmt.Fprintf(&b, "\n### `%s` — %s\n\n", r.Key, r.Disposition)
		fmt.Fprintf(&b, "- **What it is:** %s\n", r.Name)
		fmt.Fprintf(&b, "- **Where:** %s\n", r.File)
		fmt.Fprintf(&b, "- **Classification:** %s\n", r.Class)
		fmt.Fprintf(&b, "- **Fork capability that bears on it:** %s\n", r.ForkCapability)
		if len(r.WitnessRows) > 0 {
			fmt.Fprintf(&b, "- **Witness rows:** %s\n", strings.Join(backtickAll(r.WitnessRows), ", "))
		} else {
			b.WriteString("- **Witness rows:** none — see the notes for why no in-process row can be constructed\n")
		}
		if r.SubprocessWitness != "" {
			fmt.Fprintf(&b, "- **Subprocess witness:** `%s`\n", r.SubprocessWitness)
		}
		if r.LivenessProof != "" {
			fmt.Fprintf(&b, "- **Liveness proof (no row can reach it):** %s\n", r.LivenessProof)
		}
		fmt.Fprintf(&b, "- **Recorded stock envelope:** %s\n", r.StockEnvelope)
		fmt.Fprintf(&b, "- **Recorded native envelope:** %s\n", r.NativeEnvelope)
		fmt.Fprintf(&b, "- **Change this makes:** %s\n", r.Effect)
		if r.SubsumedBy != "" {
			fmt.Fprintf(&b, "- **Now carried by:** %s\n", r.SubsumedBy)
		}
		fmt.Fprintf(&b, "- **Rollback condition:** %s\n", r.Rollback)
		if r.DeferralRecord != "" {
			fmt.Fprintf(&b, "- **Deferral record:** %s\n", r.DeferralRecord)
		}
		fmt.Fprintf(&b, "\n%s\n", r.Notes)
	}
	return b.String()
}

// cell escapes a value for a Markdown TABLE CELL.
//
// A pipe inside a cell ends it, and the ledger is full of them: every admitted
// subject-kind set is spelled `{string|seq|iterable|map}`, so an unescaped
// rendering splits those rows across the wrong columns and the human half of the
// evidence becomes unreadable exactly where it is most load-bearing.
func cell(v string) string {
	return strings.ReplaceAll(v, "|", "\\|")
}

func shortFile(path string) string {
	parts := strings.Split(path, ", ")
	for i, p := range parts {
		parts[i] = "`" + strings.TrimPrefix(p, "internal/debaml/") + "`"
	}
	return strings.Join(parts, ", ")
}

func backtickAll(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "`" + id + "`"
	}
	return out
}
