package bamlfuzz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Parse recovery status tokens. A case's expected outcome is one of
// these: BAML either recovered typed JSON from the (often JSONish) raw
// text, or it rejected the input. The harness gates on this success/error
// parity, not on exact error strings.
const (
	ParseStatusSuccess = "success"
	ParseStatusError   = "error"
)

// ParseExpected is the checked-in expected outcome for a parse-recovery
// case (or one of its streaming prefixes). Status is "success" or "error".
// JSON is the expected typed JSON when Status is "success"; it is empty
// for an "error" expectation. These values are BAML's *observed* behavior
// — BAML is the oracle and the corpus records what it actually does, not
// what JSON-the-spec would prescribe.
type ParseExpected struct {
	Status string          `json:"status"`
	JSON   json.RawMessage `json:"json,omitempty"`
}

// IsSuccess reports whether the expectation is a successful parse.
func (e ParseExpected) IsSuccess() bool { return e.Status == ParseStatusSuccess }

// ParseRecoveryPrefix is one accumulated raw prefix in a streaming
// recovery case. Raw is the full accumulated text at this step (not a
// delta), so replay is independent of any chunk-size code. Want is the
// expected stream-parse outcome for that prefix.
type ParseRecoveryPrefix struct {
	Name string        `json:"name"`
	Raw  string        `json:"raw"`
	Want ParseExpected `json:"want"`
}

// ParseRecoveryCase is one JSONish recovery corpus entry. A case is
// either a final-parse case (Raw + Want) or a streaming case (Prefixes),
// or both. The schema gives the parsers a typed target; PreserveSchemaOrder
// selects whether the key-order check runs when the case is diffed against
// a registered native parser.
type ParseRecoveryCase struct {
	Name                string                `json:"name"`
	Description         string                `json:"description,omitempty"`
	PreserveSchemaOrder bool                  `json:"preserve_schema_order"`
	Schema              FuzzSchema            `json:"schema"`
	Raw                 string                `json:"raw,omitempty"`
	Want                ParseExpected         `json:"want,omitempty"`
	Prefixes            []ParseRecoveryPrefix `json:"prefixes,omitempty"`
}

// HasFinal reports whether the case carries a final-parse leg (a Want
// with a non-empty Status). A streaming-only case has no final leg.
func (c ParseRecoveryCase) HasFinal() bool { return c.Want.Status != "" }

// HasPrefixes reports whether the case carries a streaming-prefix leg.
func (c ParseRecoveryCase) HasPrefixes() bool { return len(c.Prefixes) > 0 }

// PrefixRaws returns the accumulated raw strings of the streaming
// prefixes in order, the slice DiffParserPrefixes consumes.
func (c ParseRecoveryCase) PrefixRaws() []string {
	out := make([]string, len(c.Prefixes))
	for i, p := range c.Prefixes {
		out[i] = p.Raw
	}
	return out
}

// LoadParseRecoveryCorpus reads every `*.json` file under dir and decodes
// each as a ParseRecoveryCase, returning them sorted by filename so test
// subtests run in a stable, lexicographic order. A case whose Name field
// is empty inherits the filename (sans `.json`). The `_artifacts` output
// directory and any other subdirectory are skipped.
func LoadParseRecoveryCorpus(dir string) ([]ParseRecoveryCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]ParseRecoveryCase, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var c ParseRecoveryCase
		// Decode strictly: a typo'd corpus key must fail fast rather than
		// being silently dropped (plain json.Unmarshal accepts unknown
		// fields). Mirror json.Unmarshal's single-document, no-trailing-data
		// contract by confirming the stream is at EOF after the one case.
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("%s: unexpected trailing JSON after case object", name)
			}
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if c.Name == "" {
			c.Name = strings.TrimSuffix(name, ".json")
		}
		out = append(out, c)
	}
	return out, nil
}
