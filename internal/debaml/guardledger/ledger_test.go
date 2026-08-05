//go:build integration

package guardledger

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// The ledger, as this harness reads it.
//
// internal/debaml/testdata/guard_ledger/ledger.json is the SINGLE SOURCE of the
// guard-removal ledger: internal/debaml/guard_ledger.md is rendered from it and
// byte-compared by internal/debaml's TestGuardLedgerMarkdownIsRendered, and this
// harness reads the same file to prove the ledger's cited evidence exists.
//
// The struct is duplicated here rather than exported from internal/debaml on
// purpose: the ledger is proof material for a TEST-ONLY slice, and adding a
// production symbol to carry it would put a test concern in the customer build.
// Two ~20-line decoders that must agree are cheaper than that, and they DO agree
// — a field one side ignores would make TestGuardLedgerCoversEveryLedgerRecord
// or the markdown drift test fail.

// ledgerRecord is one guard's inventory entry.
type ledgerRecord struct {
	// Key is the stable guard identifier the corpus's guardRow.Guards cite.
	Key string `json:"key"`
	// Name is the function/behaviour as it appears in the source.
	Name string `json:"name"`
	// File is where it lives.
	File string `json:"file"`
	// Class is the scope's P/A/U classification.
	Class string `json:"class"`
	// Disposition is one of "removed", "kept-inert", "kept-over-decline",
	// "kept-unwitnessable" or "kept-unprovable"; see
	// TestGuardLedgerCoversEveryLedgerRecord for what each one owes as evidence.
	Disposition string `json:"disposition"`
	// ForkCapability names the fork patch(es) that subsume the guard.
	ForkCapability string `json:"forkCapability"`
	// WitnessRows are the corpus row ids that witness it.
	WitnessRows []string `json:"witnessRows"`
	// StockEnvelope / NativeEnvelope summarise what the rows recorded.
	StockEnvelope  string `json:"stockEnvelope"`
	NativeEnvelope string `json:"nativeEnvelope"`
	// Effect is "coverage-only", "semantic", or "none (subsumed)".
	Effect string `json:"effect"`
	// SubsumedBy names the retained guard a removal's refusals fall through to.
	SubsumedBy string `json:"subsumedBy"`
	// Rollback is the condition under which the removal must be reverted.
	Rollback string `json:"rollback"`
	// SubprocessWitness names the isolated-subprocess test that stands in for a
	// row where stock cannot be observed in-process at all.
	SubprocessWitness string `json:"subprocessWitness"`
	// LivenessProof names the in-package test(s) that prove a kept-but-unreachable
	// guard exists and executes.
	LivenessProof string `json:"livenessProof"`
	// DeferralRecord links the tracking entry a kept-as-over-decline guard owes.
	DeferralRecord string `json:"deferralRecord"`
	// Notes is free text.
	Notes string `json:"notes"`
}

// ledgerCallable is one per-callable inventory entry. This package only needs to
// know the entries EXIST and that the rows they cite are real; their
// completeness against the live profile tables is enforced in internal/debaml,
// which can read those tables.
//
// It is declared in full anyway, because the decoder below is schema-STRICT: a
// reader that modelled only the fields it happens to use would reject the real
// document, and one that relaxed the strictness to avoid that would accept a
// partial or misspelled schema instead.
type ledgerCallable struct {
	Callable    string   `json:"callable"`
	Tables      []string `json:"tables"`
	Wrapper     string   `json:"wrapper"`
	Admission   string   `json:"admission"`
	Disposition string   `json:"disposition"`
	WitnessRows []string `json:"witnessRows"`
	Deferral    string   `json:"deferralRecord"`
	Notes       string   `json:"notes"`
}

// ledgerDocument is the WHOLE canonical ledger. Every reader in this package
// decodes this one shape, so none of them can drift into accepting a document
// another would reject.
type ledgerDocument struct {
	Records   []ledgerRecord   `json:"records"`
	Callables []ledgerCallable `json:"callables"`
}

// decodeLedger reads the canonical ledger under the strictness every reader
// shares: unknown fields are refused, and the file must be exactly one complete
// JSON document.
//
// An unknown field means the schema moved and this decoder did not — the thing
// that would otherwise let a renamed or misspelled key go quietly unread, in
// proof material whose whole value is that someone checked it.
func decodeLedger(path string) (ledgerDocument, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledgerDocument{}, err
	}
	var doc ledgerDocument
	dec := stdjson.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return ledgerDocument{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := expectSingleJSONDocument(dec); err != nil {
		return ledgerDocument{}, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func loadLedger(path string) ([]ledgerRecord, error) {
	doc, err := decodeLedger(path)
	if err != nil {
		return nil, err
	}
	if len(doc.Records) == 0 {
		return nil, fmt.Errorf("%s carries no records", path)
	}
	return doc.Records, nil
}

// expectSingleJSONDocument reports whether the decoder consumed the WHOLE input.
//
// encoding/json stops at the end of the first value, so a file holding a valid
// ledger followed by anything else — a second document, a truncation artefact, a
// merge conflict's leftovers — decodes cleanly and the trailing bytes are never
// seen. The canonical evidence ledger has to be exactly one complete document,
// so the reader checks for a second token rather than assuming there is none.
func expectSingleJSONDocument(dec *stdjson.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("carries data after the first JSON document (token err: %v); the ledger must be "+
			"exactly one complete document, or part of it is proof nothing reads", err)
	}
	return nil
}

// loadLedgerCallableRows returns every witness row id the per-callable inventory
// cites, so this package can prove they exist in the corpus.
func loadLedgerCallableRows(path string) (map[string][]string, error) {
	doc, err := decodeLedger(path)
	if err != nil {
		return nil, err
	}
	if len(doc.Callables) == 0 {
		return nil, fmt.Errorf("%s carries no callable inventory", path)
	}
	out := map[string][]string{}
	for _, c := range doc.Callables {
		// A DUPLICATE key is rejected rather than overwritten. This reader claims
		// to return every cited row, and a plain assignment would drop the FIRST
		// entry's witnesses silently — proof material disappearing inside the
		// reader whose whole job is to surface it. The first entry is kept in the
		// returned map so the error names the collision rather than the last
		// writer.
		if _, seen := out[c.Callable]; seen {
			return nil, fmt.Errorf("%s inventories callable %q more than once; a duplicate entry would hide the "+
				"first one's cited witnesses", path, c.Callable)
		}
		out[c.Callable] = c.WitnessRows
	}
	return out, nil
}
