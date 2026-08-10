//go:build integration

package constraintoracle

// The TEST-ONLY BRIDGE that hands this package's 719-case corpus to the Slice
// 7.2a-3 serving-shaped oracle.
//
// WHY A FILE AND NOT AN IMPORT. Both corpora live in _test.go files — this one in
// package constraintoracle, the serving oracle's in package debaml — and Go cannot
// import test code across packages. Promoting either into a non-test package would
// add shared production-visible code, which the TEST-ONLY invariant forbids and
// which would make the production diff non-empty. A checked-in JSON artifact is the
// one carrier that crosses the boundary without either cost.
//
// WHY IT CANNOT GO STALE. The file is a pure function of the live corpus:
// TestConstraintCorpusBridgeExport re-renders it on every run and byte-compares.
// A case added, removed, re-expressed or re-pinned here fails this test until the
// artifact is regenerated, and the serving oracle then consumes the new content.
// So the 719 cases are a REGRESSION SOURCE for the serving oracle rather than a
// separate suite that merely happens to be green.
//
// Regenerate with:
//
//	BAML_CONSTRAINT_BRIDGE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/debaml/constraintoracle -run TestConstraintCorpusBridgeExport

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	// bridgeExportPath is the artifact the serving oracle reads. It deliberately
	// lives under the SERVING oracle's testdata: it is that oracle's input, and
	// keeping it there makes the direction of the dependency obvious.
	bridgeExportPath = "../testdata/serving_oracle/constraint_corpus.json"
	// bridgeWriteEnv rewrites the artifact instead of comparing.
	bridgeWriteEnv = "BAML_CONSTRAINT_BRIDGE_WRITE"
	// bridgeVersion is bumped when the artifact's SHAPE changes. The consumer
	// pins it, so a shape change cannot be read as a content change.
	bridgeVersion = 1
)

// bridgeExport is the artifact's top level.
//
// Field names are explicit and the consumer decodes with DisallowUnknownFields, so
// a field added here without teaching the consumer about it is a hard failure on
// the other side rather than a silently ignored half-migration.
type bridgeExport struct {
	Version int           `json:"version"`
	Prelude string        `json:"prelude"`
	Groups  []bridgeGroup `json:"groups"`
	Cases   []bridgeCase  `json:"cases"`
}

// bridgeGroup is one `this` value: the BAML field type that carries the
// attributes and the assistant text that produces the value.
type bridgeGroup struct {
	Name     string `json:"name"`
	BAMLType string `json:"baml_type"`
	Input    string `json:"input"`
}

// bridgeCase is one (expression, `this`) observation with both pinned outcomes.
type bridgeCase struct {
	Label string `json:"label"`
	Group string `json:"group"`
	Expr  string `json:"expr"`
	// Retained is the JinjaExpression BAML evaluates, when it differs from Expr
	// (the attribute lexer doubles backslashes). Empty means identical.
	Retained string `json:"retained,omitempty"`
	Stock    string `json:"stock"`
	Native   string `json:"native"`
}

// renderBridgeExport builds the artifact from the LIVE corpus.
func renderBridgeExport() ([]byte, error) {
	out := bridgeExport{Version: bridgeVersion, Prelude: bamlPrelude}
	for _, g := range constraintGroups {
		out.Groups = append(out.Groups, bridgeGroup{Name: g.Name, BAMLType: g.BAMLType, Input: g.Input})
	}
	for _, c := range constraintCases {
		out.Cases = append(out.Cases, bridgeCase{
			Label: c.Label, Group: c.Group, Expr: c.Expr, Retained: c.Retained,
			Stock: string(c.Stock), Native: string(c.Native),
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping OFF: the corpus is full of `<`, `>` and `&` inside expressions,
	// and escaping them would make the artifact unreadable in review for no benefit
	// — the consumer decodes it as JSON either way.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TestConstraintCorpusBridgeExport pins the bridge artifact as a pure function of
// this package's corpus.
//
// Without it the serving oracle would consume a snapshot that could drift away from
// the 719 cases it claims to be a regression source for.
func TestConstraintCorpusBridgeExport(t *testing.T) {
	want, err := renderBridgeExport()
	if err != nil {
		t.Fatalf("render the bridge export: %v", err)
	}
	if os.Getenv(bridgeWriteEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(bridgeExportPath), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(bridgeExportPath), err)
		}
		if err := os.WriteFile(bridgeExportPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", bridgeExportPath, err)
		}
		t.Logf("%s rewritten from the live corpus (%d groups, %d cases)",
			bridgeExportPath, len(constraintGroups), len(constraintCases))
		return
	}
	got, err := os.ReadFile(bridgeExportPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with %s=1)", bridgeExportPath, err, bridgeWriteEnv)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: it no longer matches the live corpus. The Slice 7.2a-3 serving oracle "+
			"consumes it as a regression source, so it must be regenerated with %s=1 whenever a case "+
			"changes.\n  on disk %d bytes, rendered %d bytes",
			bridgeExportPath, bridgeWriteEnv, len(got), len(want))
	}
	// Non-vacuity: the artifact must actually carry the corpus.
	if len(constraintCases) == 0 || len(constraintGroups) == 0 {
		t.Fatal("the corpus is empty; the exported artifact would carry nothing")
	}
	t.Logf("bridge export current: %d groups, %d cases", len(constraintGroups), len(constraintCases))
}
