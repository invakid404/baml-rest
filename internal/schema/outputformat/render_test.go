package outputformat

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/invakid404/baml-rest/internal/schema"
)

// updateCorpus regenerates the on-disk testdata corpus from the in-code
// fixtures. Run `go test ./internal/schema/outputformat -update` after adding
// or changing a fixture. The want.txt files it writes are BAML ground truth
// only because the in-code `want` strings are copied from BAML's own render
// tests; the renderer's own output is never used as the oracle.
var updateCorpus = flag.Bool("update", false, "regenerate testdata/outputformat corpus from fixtures")

const corpusDir = "testdata/outputformat"

// TestRenderGolden is the primary parity gate: every fixture must render
// byte-for-byte equal to BAML's output.
func TestRenderGolden(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.bundle(), tc.opts)
			if err != nil {
				t.Fatalf("Render error: %v", err)
			}
			if got != tc.want {
				t.Errorf("render mismatch\n--- got ----\n%q\n--- want ---\n%q\n\n--- got (raw) ---\n%s\n--- want (raw) ---\n%s",
					got, tc.want, got, tc.want)
			}
		})
	}
}

// TestCorpusRoundTrip exercises the production decode path: a bundle is
// serialized to JSON (mirroring testdata/<case>/bundle.json), decoded back —
// which leaves the unexported indexes empty — and rendered. Render calls
// ValidateOutput, which rebuilds the indexes, so the decoded bundle must
// render identically to the in-memory one. With -update it also writes the
// corpus files described in the issue.
func TestCorpusRoundTrip(t *testing.T) {
	if *updateCorpus {
		if err := os.RemoveAll(corpusDir); err != nil {
			t.Fatalf("clean corpus dir: %v", err)
		}
	}
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.MarshalIndent(tc.bundle(), "", "  ")
			if err != nil {
				t.Fatalf("marshal bundle: %v", err)
			}
			var decoded schema.Bundle
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal bundle: %v", err)
			}
			got, err := Render(&decoded, tc.opts)
			if err != nil {
				t.Fatalf("Render(decoded) error: %v", err)
			}
			if got != tc.want {
				t.Errorf("decoded render mismatch\n got: %q\nwant: %q", got, tc.want)
			}

			if *updateCorpus {
				writeCorpusCase(t, tc, raw)
			}
		})
	}
}

func writeCorpusCase(t *testing.T, tc goldenCase, bundleJSON []byte) {
	t.Helper()
	dir := filepath.Join(corpusDir, tc.name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), append(bundleJSON, '\n'), 0o644); err != nil {
		t.Fatalf("write bundle.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "want.txt"), []byte(tc.want), 0o644); err != nil {
		t.Fatalf("write want.txt: %v", err)
	}
	if len(describeOptions(tc.opts)) > 0 {
		optsJSON, err := json.MarshalIndent(describeOptions(tc.opts), "", "  ")
		if err != nil {
			t.Fatalf("marshal options: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "options.json"), append(optsJSON, '\n'), 0o644); err != nil {
			t.Fatalf("write options.json: %v", err)
		}
	}
}

// TestCorpusFiles renders every persisted corpus case from its bundle.json and
// checks it against want.txt, proving the on-disk fixtures stay in sync with
// the renderer independently of the in-code fixtures.
//
// It first cross-checks the on-disk directory set against the expected case
// names, failing on any missing or extra directory, so a stale corpus (a case
// added or removed in code without rerunning `-update`) is caught rather than
// silently skipped. It also requires an options.json for every non-default
// case and forbids one for default cases, keeping the persisted corpus
// self-describing. Rendering still uses the in-code options table: options.json
// is a human-readable description (the unexported setting fields don't
// round-trip through JSON), so it documents the knobs rather than driving them.
func TestCorpusFiles(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("corpus not generated; run with -update")
		}
		t.Fatalf("read corpus dir: %v", err)
	}

	// Expected case set (name -> options) from the single source of truth.
	expected := make(map[string]Options)
	for _, tc := range goldenCases() {
		expected[tc.name] = tc.opts
	}

	// On-disk case set.
	onDisk := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			onDisk[e.Name()] = struct{}{}
		}
	}

	// Fail on drift in either direction so the corpus can never silently
	// diverge from goldenCases().
	for name := range expected {
		if _, ok := onDisk[name]; !ok {
			t.Errorf("corpus missing directory %q (run `go test ./internal/schema/outputformat -update`)", name)
		}
	}
	for name := range onDisk {
		if _, ok := expected[name]; !ok {
			t.Errorf("corpus has stale directory %q with no matching fixture (run `-update`)", name)
		}
	}

	for name, opts := range expected {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(corpusDir, name)
			raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			if err != nil {
				t.Fatalf("read bundle.json: %v", err)
			}
			want, err := os.ReadFile(filepath.Join(dir, "want.txt"))
			if err != nil {
				t.Fatalf("read want.txt: %v", err)
			}

			// The persisted corpus must be self-describing: a non-default case
			// carries an options.json whose CONTENTS match the expected knobs,
			// a default case must not carry one at all. Validating the contents
			// (not just presence) catches a wrong or stale options.json.
			optsPath := filepath.Join(dir, "options.json")
			optsRaw, optsErr := os.ReadFile(optsPath)
			if len(describeOptions(opts)) > 0 {
				if optsErr != nil {
					t.Errorf("case %q has non-default options but no readable options.json: %v", name, optsErr)
				} else {
					wantOpts, err := json.MarshalIndent(describeOptions(opts), "", "  ")
					if err != nil {
						t.Fatalf("marshal expected options: %v", err)
					}
					if got := string(bytes.TrimRight(optsRaw, "\n")); got != string(wantOpts) {
						t.Errorf("case %q options.json mismatch\n got: %s\nwant: %s", name, got, string(wantOpts))
					}
				}
			} else if optsErr == nil {
				t.Errorf("case %q has default options but an unexpected options.json", name)
			}

			var b schema.Bundle
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatalf("unmarshal bundle.json: %v", err)
			}
			got, err := Render(&b, opts)
			if err != nil {
				t.Fatalf("Render error: %v", err)
			}
			if got != string(want) {
				t.Errorf("corpus render mismatch for %s\n got: %q\nwant: %q", name, got, string(want))
			}
		})
	}
}

// describeOptions produces a stable, human-readable JSON description of
// non-default options for the corpus (the unexported setting fields don't
// serialize on their own). It is documentation only; the test reads options
// from the in-code fixtures, not from this file.
func describeOptions(o Options) map[string]any {
	m := map[string]any{}
	if s, ok := o.Prefix.alwaysNonEmpty(); ok {
		m["prefix"] = s
	} else if o.Prefix.mode == settingNever {
		m["prefix"] = nil
	}
	if o.OrSplitter.set {
		m["or_splitter"] = o.OrSplitter.val
	}
	if s, ok := o.HoistedClassPrefix.alwaysNonEmpty(); ok {
		m["hoisted_class_prefix"] = s
	}
	if o.AlwaysHoistEnums.isTrue() {
		m["always_hoist_enums"] = true
	}
	if o.QuoteClassFields {
		m["quote_class_fields"] = true
	}
	if o.MapStyle == MapStyleObject {
		m["map_style"] = "object"
	}
	switch o.HoistClasses.Mode {
	case HoistAll:
		m["hoist_classes"] = "all"
	case HoistSubset:
		m["hoist_classes"] = o.HoistClasses.Subset
	}
	if o.EnumValuePrefix.mode == settingAlways {
		m["enum_value_prefix"] = o.EnumValuePrefix.val
	} else if o.EnumValuePrefix.mode == settingNever {
		m["enum_value_prefix"] = nil
	}
	return m
}
