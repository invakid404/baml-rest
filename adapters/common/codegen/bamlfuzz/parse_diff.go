package bamlfuzz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
)

// ParseOutcome records what one parser did with a request: its name, the
// JSON it produced on success, or its error text on failure. Error is
// diagnostic only — the comparator gates on success/error parity, never
// on exact error strings — so a parser is free to put whatever is useful
// for triage there.
type ParseOutcome struct {
	Parser string          `json:"parser"`
	JSON   json.RawMessage `json:"json,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ParseDiffResult is the outcome of comparing the BAML parser against the
// candidate (native) parser for one raw string. A result with an empty
// Failures slice is a pass. SkippedNative is true when the native parser
// returned ErrParserUnavailable, in which case the differential leg
// passes vacuously (BAML's own outcome is still captured for forensics).
type ParseDiffResult struct {
	// SkippedNative is set when the native parser declined the request
	// (ErrParserUnavailable). The leg passes; only BAML ran.
	SkippedNative bool `json:"skipped_native,omitempty"`
	// BAML is the oracle/reference outcome. Always populated unless the
	// BAML parser itself was unavailable (a harness failure).
	BAML ParseOutcome `json:"baml"`
	// Native is the candidate outcome. Zero-valued when SkippedNative.
	Native ParseOutcome `json:"native,omitempty"`
	// SemanticDiff lists the strict JSON disagreements between BAML
	// (reference) and native (actual). Empty unless both succeeded and
	// their JSON differed.
	SemanticDiff []SemanticDiffEntry `json:"semantic_diff,omitempty"`
	// OrderDiff lists schema-aware key-order disagreements, populated only
	// when PreserveSchemaOrder is true and both legs succeeded.
	OrderDiff []SchemaOrderDiffEntry `json:"order_diff,omitempty"`
	// Failures is the human-readable list of why this leg failed. Empty
	// means the differential passed.
	Failures []string `json:"failures,omitempty"`
}

// DiffParsers runs the native-vs-BAML differential for one raw string.
// BAML is the oracle: its outcome is the reference every comparison is
// taken against. The comparison algorithm is:
//
//  1. Call the BAML parser with req. ErrParserUnavailable here is a
//     harness failure (BAML must service the legs it claims to cover).
//  2. Call the native parser with the identical req.
//  3. If native returns ErrParserUnavailable, mark SkippedNative and pass.
//  4. Success/error parity: both error → pass; exactly one errors → fail.
//  5. Both succeed: reject empty JSON on either side, then strict-compare
//     the normalized JSON (no boundaryml/baml#3690 null tolerance — the
//     native parser must match BAML's observed behavior exactly), and when
//     PreserveSchemaOrder is true run a schema-aware key-order check with
//     BAML as expected and native as actual.
//
// choices mirrors CaseMetadata.UnionChoices and is only consulted by the
// order check. The function is pure: it returns the result and never
// fails a test itself — callers decide whether a non-empty Failures slice
// fails and where to write an envelope.
func DiffParsers(ctx context.Context, baml, native Parser, req ParseRequest, choices map[string]UnionChoice) ParseDiffResult {
	var res ParseDiffResult

	bamlRes, bamlErr := baml.Parse(ctx, req)
	res.BAML = parseOutcome(baml.Name(), bamlRes, bamlErr)
	if errors.Is(bamlErr, ErrParserUnavailable) {
		// BAML declining is an integrity failure, not a normal parse
		// error: the harness only drives BAML over legs it covers.
		res.Failures = append(res.Failures, fmt.Sprintf("BAML parser %q unavailable: %v", baml.Name(), bamlErr))
		return res
	}

	natRes, natErr := native.Parse(ctx, req)
	if errors.Is(natErr, ErrParserUnavailable) {
		res.SkippedNative = true
		return res
	}
	res.Native = parseOutcome(native.Name(), natRes, natErr)

	bamlOK := bamlErr == nil
	natOK := natErr == nil
	switch {
	case !bamlOK && !natOK:
		// Both rejected the input — parity holds, nothing more to compare.
		return res
	case bamlOK && !natOK:
		res.Failures = append(res.Failures, fmt.Sprintf("parity: BAML succeeded but native %q errored: %v", native.Name(), natErr))
		return res
	case !bamlOK && natOK:
		res.Failures = append(res.Failures, fmt.Sprintf("parity: BAML errored but native %q succeeded: %v", native.Name(), bamlErr))
		return res
	}

	// Both succeeded: a success must carry real JSON.
	if len(bamlRes.JSON) == 0 {
		res.Failures = append(res.Failures, "BAML reported success with empty JSON")
	}
	if len(natRes.JSON) == 0 {
		res.Failures = append(res.Failures, fmt.Sprintf("native %q reported success with empty JSON", native.Name()))
	}
	if len(res.Failures) > 0 {
		return res
	}

	diff, err := SemanticDiffStrict("baml_vs_native", bamlRes.JSON, natRes.JSON)
	if err != nil {
		res.Failures = append(res.Failures, fmt.Sprintf("baml_vs_native semantic diff: %v", err))
		return res
	}
	if len(diff) > 0 {
		res.SemanticDiff = diff
		res.Failures = append(res.Failures, "baml ≠ native (semantic)")
	}

	if req.PreserveSchemaOrder {
		odiff, oerr := SchemaOrderDiffWithChoices("baml_vs_native", req.Schema, bamlRes.JSON, natRes.JSON, choices)
		switch {
		case errors.Is(oerr, ErrSchemaOrderUnsupported):
			res.Failures = append(res.Failures, fmt.Sprintf("baml_vs_native schema order unsupported: %v", oerr))
		case oerr != nil:
			res.Failures = append(res.Failures, fmt.Sprintf("baml_vs_native schema order: %v", oerr))
		case len(odiff) > 0:
			res.OrderDiff = odiff
			res.Failures = append(res.Failures, "baml ≠ native (order)")
		}
	}

	return res
}

// DiffParserPrefixes runs DiffParsers over a growing sequence of
// accumulated raw prefixes, in stream mode. Every prefix is fed to both
// parsers and compared on its own — early accept/reject behavior is part
// of the spec, so the final prefix is not privileged. Each prefixes[i] is
// expected to extend prefixes[i-1]; a prefix that does not is still diffed
// but its result carries a leading monotonicity failure so a malformed
// corpus is caught rather than silently masking a real divergence.
//
// req is the template: its Raw is overridden per prefix and Stream is
// forced true. choices is passed through to each per-prefix order check.
func DiffParserPrefixes(ctx context.Context, baml, native Parser, req ParseRequest, prefixes []string, choices map[string]UnionChoice) []ParseDiffResult {
	out := make([]ParseDiffResult, 0, len(prefixes))
	for i, prefix := range prefixes {
		preq := req
		preq.Raw = prefix
		preq.Stream = true
		res := DiffParsers(ctx, baml, native, preq, choices)
		if i > 0 && !strings.HasPrefix(prefix, prefixes[i-1]) {
			res.Failures = append([]string{
				fmt.Sprintf("prefix[%d] does not extend prefix[%d]: %q is not prefixed by %q", i, i-1, prefix, prefixes[i-1]),
			}, res.Failures...)
		}
		out = append(out, res)
	}
	return out
}

// NativeOutcome returns a pointer to the candidate's outcome, or nil when
// the native leg was skipped or never ran (its Parser name is unset).
// Envelope constructors use it so an absent native leg is omitted from the
// failure JSON instead of emitting an empty {"parser":""} stub.
func (r ParseDiffResult) NativeOutcome() *ParseOutcome {
	if r.SkippedNative || r.Native.Parser == "" {
		return nil
	}
	n := r.Native
	return &n
}

// parseOutcome packages a parser's (result, error) into a ParseOutcome.
// On error only Error is set (diagnostic text); on success only JSON.
func parseOutcome(name string, res ParseResult, err error) ParseOutcome {
	o := ParseOutcome{Parser: name}
	if err != nil {
		o.Error = err.Error()
		return o
	}
	o.JSON = res.JSON
	return o
}

// SemanticDiffStrict reports the path-level disagreements between two JSON
// blobs with no tolerance whatsoever. Unlike SemanticDiff (which forgives
// boundaryml/baml#3690 leaked null keys on the actual side) and
// SemanticDiffWithSchema (which additionally forgives literal escape-level
// mismatches), the strict comparator treats every structural and scalar
// difference — including an extra or missing null-valued key — as a real
// disagreement.
//
// This is the comparator for native-vs-BAML: BAML is the observed
// behavior the native parser must reproduce exactly, so any divergence,
// including one BAML's own surface tolerates against the walker's
// expected output, must surface here. `side` is copied verbatim onto each
// emitted entry. Either input being empty is a decode error, mirroring
// SemanticDiff's contract.
//
// strictDiffAny / strictDeepEqual are a deliberately separate recursion
// from envelope.go's semanticDiff / diffAny / deepEqualSemantic, not an
// accidental copy. The shared comparator unconditionally applies the
// lenient boundaryml/baml#3690 extra-null tolerance; the native-vs-BAML
// leg requires exact equality and must NOT inherit it. Folding the two
// behind a shared `nullStrict` mode is deferred precisely to avoid
// re-entangling the strict and lenient paths and risking the tolerance
// leaking back into this comparator — the small duplication buys that
// safety property.
func SemanticDiffStrict(side string, a, b json.RawMessage) ([]SemanticDiffEntry, error) {
	av, err := strictDecodeAny(a)
	if err != nil {
		return nil, fmt.Errorf("decode a: %w", err)
	}
	bv, err := strictDecodeAny(b)
	if err != nil {
		return nil, fmt.Errorf("decode b: %w", err)
	}
	var out []SemanticDiffEntry
	strictDiffAny(&out, side, "$", av, bv)
	return out, nil
}

// strictDecodeAny decodes a JSON payload into the generic any-tree the
// strict comparator walks, preserving every number as a json.Number
// instead of collapsing it to float64. Keeping the exact integer token
// lets strictDeepEqual distinguish i64::MAX (9223372036854775807) from
// 9223372036854775808 — a one-off drift float64 rounds away. UseNumber
// applies to the whole decode, so numbers nested inside objects and arrays
// are preserved too.
//
// This is intentionally NOT the shared decodeAny from envelope.go: that
// decoder feeds the lenient comparators, which switch on float64, and the
// strict native-vs-BAML leg keeps its own recursion (see the note on
// strictDiffAny / strictDeepEqual). Empty input is a hard decode error and
// trailing content after the first value is rejected, both mirroring
// decodeAny's json.Unmarshal contract.
func strictDecodeAny(b json.RawMessage) (any, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty JSON payload")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Require the decoder to be fully drained so trailing content is
	// rejected exactly as json.Unmarshal rejects it. dec.More() is NOT a
	// substitute: it reports false before a closing ']' or '}', so tokens
	// like `1]` or `{}]` would slip through. A second Decode must return
	// io.EOF; insignificant trailing whitespace is consumed and still
	// yields io.EOF, while any further value or stray byte surfaces here.
	var scratch json.RawMessage
	if err := dec.Decode(&scratch); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected trailing JSON content after top-level value")
	}
	return v, nil
}

// strictDiffAny appends path-level disagreements between `a` (reference)
// and `b` (actual) to `out`, with no null-key or literal-escape tolerance.
func strictDiffAny(out *[]SemanticDiffEntry, side, path string, a, b any) {
	if strictDeepEqual(a, b) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
			return
		}
		keys := make(map[string]struct{}, len(av)+len(bv))
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		sortedKeys := make([]string, 0, len(keys))
		for k := range keys {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)
		for _, k := range sortedKeys {
			av1, ok1 := av[k]
			bv1, ok2 := bv[k]
			if !ok1 || !ok2 {
				*out = append(*out, SemanticDiffEntry{Side: side, Path: path + "." + k, Got: av1, Want: bv1})
				continue
			}
			strictDiffAny(out, side, path+"."+k, av1, bv1)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
			return
		}
		for i := range av {
			strictDiffAny(out, side, fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
		}
	default:
		*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
	}
}

// strictDeepEqual is structural JSON equality modulo object key ordering
// only — no null-key tolerance. Two objects are equal iff they carry the
// identical key set and every value is strictDeepEqual. Numbers are
// compared via numbersEqual: two integer tokens by arbitrary-precision
// big.Int (so i64-boundary drift is not rounded away) and otherwise by
// float64 value (so 50 and 50.0 still agree).
func strictDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			rv, present := bv[k]
			if !present || !strictDeepEqual(v, rv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !strictDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && numbersEqual(av, bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

// isIntegerToken reports whether a decoded JSON number's source token is an
// integer literal — no fractional point and no exponent. The decision is
// made on the token TEXT, never the value: 1e2 is a non-integer token even
// though it denotes 100, and 9223372036854775808 is an integer token even
// though float64 cannot represent it exactly.
func isIntegerToken(n json.Number) bool {
	return !strings.ContainsAny(string(n), ".eE")
}

// numbersEqual compares two decoded JSON numbers with integer precision at
// the i64 boundary. When BOTH tokens are integer literals they are compared
// as arbitrary-precision big.Int, so 9223372036854775807 (i64::MAX) and
// 9223372036854775808 correctly differ instead of colliding at float64.
// When EITHER token is non-integer (carries a '.', 'e', or 'E') the
// comparison falls back to float64 value equality, preserving intended
// equalities like 50 vs 50.0 and 100 vs 1e2.
func numbersEqual(a, b json.Number) bool {
	if isIntegerToken(a) && isIntegerToken(b) {
		ai, aok := new(big.Int).SetString(string(a), 10)
		bi, bok := new(big.Int).SetString(string(b), 10)
		if aok && bok {
			return ai.Cmp(bi) == 0
		}
		// A token we classified as an integer that big.Int nonetheless
		// rejects is malformed for base 10; fall through to the float
		// comparison rather than silently claiming equality.
	}
	af, aerr := a.Float64()
	bf, berr := b.Float64()
	if aerr != nil || berr != nil {
		// Neither representation parsed as a float; require exact token
		// equality so we never equate two values we cannot compare.
		return string(a) == string(b)
	}
	return af == bf
}
