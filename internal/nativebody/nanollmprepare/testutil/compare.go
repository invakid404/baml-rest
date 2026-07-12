// Package testutil is the BAML-free, nanollm-free snapshot / normalization /
// comparison helper for the de-BAML Phase 5.1 OpenAI prepared-request
// differential. Both gated test binaries (the dynamic patched-BAML leg and the
// static stock-BAML leg) and the pure ungated unit tests share exactly ONE
// comparator through this package.
//
// It deliberately imports NEITHER BAML runtime (patched dynclient or stock
// github.com/boundaryml/baml) NOR nanollm. Its inputs are plain Go values —
// ordered header pairs, string maps, multi-value maps, and raw bytes — so it can
// be exercised without CGO, without either CFFI runtime, and without the private
// nanollm FFI. The two legs adapt their runtime-specific captures into these
// neutral shapes before comparing.
//
// # Header policy (scope "Exact field policy")
//
// Header NAMES are case-insensitive: both sides are folded to an ASCII-lowercase
// unique map. A duplicate name or a multi-value header is REJECTED (fail closed),
// never silently merged — [NormalizeHeaders]/[NewSnapshot] return an error.
//
// The comparison is over the SEMANTIC header set only. BAML's request additionally
// carries its own internal transport/routing header `baml-original-url` (the base
// URL it later rewrites) which nanollm never emits; the scope EXPLICITLY DECLINES
// parity on BAML transport-added headers, so that named header is partitioned off
// before the set/value comparison ([SplitSemantic]). This is fail closed: any
// OTHER unexpected BAML header stays in the semantic set and breaks the set
// comparison — only the one named, documented transport header is excluded.
//
// # Redaction (scope: "Redact every sensitive header value")
//
// Diagnostics never print a raw secret. Only an allowlist of safe header names
// (mirroring nanollm's own §11.3 allowlist) prints its value; every other value —
// notably `authorization` — renders as "<redacted>". Fixture bodies are safe to
// print (they carry no real credential), so a body mismatch shows both bodies.
//
// # What stays with the caller
//
// This package intentionally does NOT assert the nanollm-only fields
// (ResponseFormat, Meta.*, Expired()); those reference the nanollm type and are
// checked directly in each leg's test. This package also never compares HEADER
// ORDER or CASING across legs — that claim is explicitly declined; nanollm's own
// emitted order is asserted independently by the leg, never against BAML.
package testutil

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Snapshot is a runtime-neutral view of one captured HTTP request — from a BAML
// oracle leg (dynamic net/http or static HTTPRequest) or from a nanollm
// PreparedRequest. Headers is already normalized: a unique map keyed by
// ASCII-lowercase name with exactly one value each.
type Snapshot struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// bamlTransportOnlyHeaders is the named allowlist of BAML-internal transport
// headers that Phase 5.1 EXPLICITLY DECLINES to compare (scope Declines: "BAML
// ... transport-added headers"). BAML v0.223 emits `baml-original-url` (the base
// URL, for its own downstream URL rewrite) on both the dynamic and static legs;
// nanollm never emits it. It is excluded from the semantic comparison — and ONLY
// it: any other unexpected header remains in the semantic set and fails the test.
var bamlTransportOnlyHeaders = map[string]bool{
	"baml-original-url": true,
}

// safeHeaderNames are the header names whose values are safe to print in a
// diagnostic. Mirrors nanollm's own redaction allowlist (FFI_DESIGN.md §11.3):
// everything else — including `authorization` — redacts by default.
var safeHeaderNames = map[string]bool{
	"content-type":      true,
	"accept":            true,
	"anthropic-version": true,
}

// NewSnapshot builds a [Snapshot], normalizing the ordered header pairs into a
// unique ASCII-lowercase map and FAILING CLOSED on a duplicate name / multi-value
// header. The header pairs may come from nanollm's [][2]string directly, from
// [PairsFromStringMap] (static BAML map), or from [PairsFromMultiMap] (dynamic
// net/http header) — a multi-value header becomes repeated pairs and is rejected
// here as a duplicate name.
func NewSnapshot(method, url string, headerPairs [][2]string, body []byte) (Snapshot, error) {
	headers, err := NormalizeHeaders(headerPairs)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Method: method, URL: url, Headers: headers, Body: body}, nil
}

// NormalizeHeaders folds ordered header pairs into a unique map keyed by
// ASCII-lowercase name. It returns an error on any duplicate name (which also
// catches a multi-value header once flattened to repeated pairs) — the scope
// requires rejecting duplicates/multi-values rather than merging them.
func NormalizeHeaders(pairs [][2]string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		name := strings.ToLower(p[0])
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("testutil: duplicate/multi-value header %q rejected (names are compared case-insensitively)", name)
		}
		out[name] = p[1]
	}
	return out, nil
}

// PairsFromStringMap converts a BAML static `map[string]string` header set into
// ordered pairs (order is irrelevant — [NormalizeHeaders] keys by name).
func PairsFromStringMap(m map[string]string) [][2]string {
	out := make([][2]string, 0, len(m))
	for k, v := range m {
		out = append(out, [2]string{k, v})
	}
	return out
}

// PairsFromMultiMap flattens a dynamic net/http `map[string][]string` header set
// (http.Header's underlying type) into ordered pairs, one per value. A header
// carrying more than one value therefore yields repeated pairs of the same name,
// which [NormalizeHeaders] rejects — exactly the "reject multi-values" policy.
func PairsFromMultiMap(m map[string][]string) [][2]string {
	var out [][2]string
	for k, vs := range m {
		for _, v := range vs {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// SplitSemantic partitions a normalized header map into the semantic subset
// (compared across legs) and the declined BAML-transport-only subset
// (`baml-original-url`). The input is not mutated.
func SplitSemantic(headers map[string]string) (semantic, transport map[string]string) {
	semantic = make(map[string]string, len(headers))
	transport = map[string]string{}
	for name, value := range headers {
		if bamlTransportOnlyHeaders[name] {
			transport[name] = value
			continue
		}
		semantic[name] = value
	}
	return semantic, transport
}

// Authorization returns the single authorization header value and whether it is
// present. The map is unique by name, so there is at most one.
func Authorization(headers map[string]string) (string, bool) {
	v, ok := headers["authorization"]
	return v, ok
}

// RedactValue returns value if name is on the safe allowlist, else "<redacted>".
func RedactValue(name, value string) string {
	if safeHeaderNames[strings.ToLower(name)] {
		return value
	}
	return "<redacted>"
}

// Diff performs the field-by-field prepared-request differential and returns a
// sorted list of human-readable, REDACTED differences; an empty slice means the
// two snapshots are equal across method, URL, semantic headers, and raw body.
//
// The `a` snapshot is the BAML oracle capture and `b` is the nanollm plan
// (labeled "baml"/"nanollm" in messages). Header comparison is over the semantic
// set only; header VALUES that differ are reported redacted unless the name is on
// the safe allowlist. Header order and casing are never compared. Bodies are
// printed in full on mismatch (fixtures carry no real credential).
func Diff(a, b Snapshot) []string {
	var diffs []string

	if a.Method != b.Method {
		diffs = append(diffs, fmt.Sprintf("method: baml=%q nanollm=%q", a.Method, b.Method))
	}
	if a.URL != b.URL {
		diffs = append(diffs, fmt.Sprintf("url: baml=%q nanollm=%q", a.URL, b.URL))
	}
	if !bytes.Equal(a.Body, b.Body) {
		diffs = append(diffs, fmt.Sprintf("body:\n--- baml ---\n%s\n--- nanollm ---\n%s", a.Body, b.Body))
	}
	diffs = append(diffs, diffHeaders(a.Headers, b.Headers)...)

	sort.Strings(diffs)
	return diffs
}

// diffHeaders compares the header sets (names + values) of the two snapshots,
// redacting non-safe values. It reports names present on only one side and names
// whose values differ.
//
// The BAML-transport-header exemption (`baml-original-url`) is applied to the
// ORACLE side (`a`) ONLY: BAML emits it and nanollm must not, so it is dropped
// from the oracle set before comparison but the nanollm set (`b`) is left
// UNFILTERED. If nanollm ever emitted `baml-original-url` it would appear only in
// `b`, be reported as "present on nanollm only", and correctly FAIL parity — a
// two-sided exemption would silently hide that. Both maps are already normalized
// (unique ASCII-lowercase names, duplicates/multi-values rejected at snapshot
// construction); this only removes the one-sided oracle exemption from `b`.
func diffHeaders(aHeaders, bHeaders map[string]string) []string {
	aSem, _ := SplitSemantic(aHeaders) // oracle side: exempt baml-original-url
	bSem := bHeaders                   // nanollm side: UNFILTERED (no exemption)

	var diffs []string
	for name, av := range aSem {
		bv, ok := bSem[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("header %q: present on baml only (value %s)", name, RedactValue(name, av)))
			continue
		}
		if av != bv {
			diffs = append(diffs, fmt.Sprintf("header %q: value differs (baml=%s nanollm=%s)", name, RedactValue(name, av), RedactValue(name, bv)))
		}
	}
	for name, bv := range bSem {
		if _, ok := aSem[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("header %q: present on nanollm only (value %s)", name, RedactValue(name, bv)))
		}
	}
	return diffs
}
