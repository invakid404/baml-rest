package bamlfuzz

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseRecoveryCorpusDir is the on-disk JSONish recovery corpus, relative
// to this package directory. The integration harness loads the same
// directory via its repo-root-relative path.
const parseRecoveryCorpusDir = "../testdata/bamlfuzz/parse_recovery"

// TestParseRecoveryCorpusWellFormed loads the checked-in corpus and pins
// the structural invariants the integration harness relies on, without
// needing BAML or Docker: every case names a root class, a final-parse
// case's want is internally consistent (success ⟺ JSON present), and a
// streaming case's prefixes grow monotonically. This is the local guard
// that a hand-edited corpus file stays loadable and replayable.
func TestParseRecoveryCorpusWellFormed(t *testing.T) {
	corpus, err := LoadParseRecoveryCorpus(parseRecoveryCorpusDir)
	if err != nil {
		t.Fatalf("load parse recovery corpus: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatalf("parse recovery corpus at %s is empty", parseRecoveryCorpusDir)
	}

	for _, c := range corpus {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Schema.RootClass == "" && c.Schema.RootType == nil {
				t.Errorf("case %q: schema has neither root_class nor root_type", c.Name)
			}
			if c.Schema.RootClass != "" {
				if _, ok := c.Schema.FindClass(c.Schema.RootClass); !ok {
					t.Errorf("case %q: root_class %q not present in schema", c.Name, c.Schema.RootClass)
				}
			}
			if !c.HasFinal() && !c.HasPrefixes() {
				t.Errorf("case %q: neither a final (raw+want) nor a streaming (prefixes) leg", c.Name)
			}

			if c.HasFinal() {
				validateExpected(t, c.Name+".want", c.Want)
				if c.Raw == "" {
					t.Errorf("case %q: final leg present but raw is empty", c.Name)
				}
			}

			if c.HasPrefixes() {
				validatePrefixGrowth(t, c)
				for i, p := range c.Prefixes {
					if p.Raw == "" {
						t.Errorf("case %q prefix[%d] %q: empty raw", c.Name, i, p.Name)
					}
					validateExpected(t, c.Name+".prefix["+p.Name+"].want", p.Want)
				}
			}
		})
	}
}

// validateExpected asserts a ParseExpected's status/JSON are consistent:
// status must be one of the two tokens, a success carries decodable JSON,
// and an error carries none.
func validateExpected(t *testing.T, label string, e ParseExpected) {
	t.Helper()
	switch e.Status {
	case ParseStatusSuccess:
		if len(e.JSON) == 0 {
			t.Errorf("%s: status=success but json is empty", label)
			return
		}
		var v any
		if err := json.Unmarshal(e.JSON, &v); err != nil {
			t.Errorf("%s: status=success but json does not decode: %v", label, err)
		}
	case ParseStatusError:
		if len(e.JSON) != 0 {
			t.Errorf("%s: status=error must not carry json", label)
		}
	default:
		t.Errorf("%s: unknown status %q (want %q or %q)", label, e.Status, ParseStatusSuccess, ParseStatusError)
	}
}

// validatePrefixGrowth asserts each accumulated prefix extends the prior
// one — the same monotonicity DiffParserPrefixes enforces at runtime.
func validatePrefixGrowth(t *testing.T, c ParseRecoveryCase) {
	t.Helper()
	for i := 1; i < len(c.Prefixes); i++ {
		if !strings.HasPrefix(c.Prefixes[i].Raw, c.Prefixes[i-1].Raw) {
			t.Errorf("case %q: prefix[%d] %q does not extend prefix[%d] %q",
				c.Name, i, c.Prefixes[i].Name, i-1, c.Prefixes[i-1].Name)
		}
	}
}
