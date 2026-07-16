// Package parity holds the de-BAML cutover's PURE native-vs-BAML comparison
// primitives, shared by the one-send SHADOW comparator (which keeps BAML serving)
// and the Slice-6 native SERVE implementation (which serves native). Factoring
// them here keeps a SINGLE comparison policy — the S4 request-plan comparison and
// the S5 same-response structured/order comparison — so the serving path can
// never silently diverge from the shadow oracle. Nothing here opens a socket,
// sends a request, or imports nanollm; it compares already-built plans and
// already-parsed outputs, and every diagnostic is redacted/secret-free — bodies
// surface only a bounded byte length, never a content-derived hash/digest.
package parity

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// PlanComparison is the per-field native-vs-BAML request-plan comparison result.
// Each bool is true when that facet MATCHES. Diffs is a sorted, secret-free,
// REDACTED list of human-readable differences (empty on a full match) suitable
// for optional structured logging — it never carries a header value, body byte,
// prompt text, api key, bearer token, or a content-derived body hash/digest
// (bodies surface a bounded byte length only).
type PlanComparison struct {
	Method  bool
	Target  bool
	Host    bool
	Headers bool
	Body    bool
	// MetaMismatch is set when a STRUCTURAL problem prevented the header facet
	// from being compared normally — a duplicate/multi-value header made one
	// side un-normalizable, so the headers facet fails closed. It is recorded as
	// an ADDITIONAL `meta` mismatch so a structural failure stays distinguishable
	// from a semantic header-value drift; the method/target/host/body facets are
	// still compared from the raw inputs and reported truthfully.
	MetaMismatch bool
	Diffs        []string
}

// AllMatch reports whether every compared field matched.
func (c PlanComparison) AllMatch() bool {
	return c.Method && c.Target && c.Host && c.Headers && c.Body
}

// ComparePlans compares BAML's built request plan against the native prepared
// plan across method, request target (path+query), effective host, semantic
// headers, and raw body bytes. Neither plan is sent; both are built-but-unsent.
//
// It reuses the pure testutil normalizer/redactor so the header policy matches
// the Phase 5/6 differential exactly: header names are case-insensitive,
// duplicate/multi-value headers fail closed, BAML's internal `baml-original-url`
// transport header is excluded from the BAML side only, and every non-allowlisted
// header value is redacted in a diagnostic. Bodies are compared byte-for-byte but
// NEVER printed and NEVER hashed — only a bounded byte LENGTH is surfaced (a
// content-derived digest, even truncated, is avoided so a diagnostic can never
// confirm/correlate provider body content).
func ComparePlans(bamlReq *llmhttp.Request, native *llmhttp.ExactAttemptRequest) PlanComparison {
	var diffs []string
	res := PlanComparison{}

	// Method, request target, effective host, and raw body do NOT depend on
	// header normalization — compare them directly from the raw plan inputs. A
	// duplicate/multi-value header that fails the header facet closed below must
	// not spuriously mark these unaffected facets as mismatched (the raw method /
	// URL / body are exactly what NewSnapshot would have stored unchanged).
	res.Method = bamlReq.Method == native.Method
	if !res.Method {
		diffs = append(diffs, fmt.Sprintf("method: baml=%q native=%q", bamlReq.Method, native.Method))
	}

	bamlHost, bamlTarget := hostAndTarget(bamlReq.URL)
	nativeHost, nativeTarget := hostAndTarget(native.URL)
	res.Host = bamlHost == nativeHost
	if !res.Host {
		diffs = append(diffs, "host differs")
	}
	res.Target = bamlTarget == nativeTarget
	if !res.Target {
		diffs = append(diffs, "target differs")
	}

	bamlBody := []byte(bamlReq.Body)
	res.Body = bytes.Equal(bamlBody, native.Body)
	if !res.Body {
		// Bounded LENGTH ONLY — no content-derived value (not even a truncated
		// hash), so the diagnostic can never confirm/correlate body content.
		diffs = append(diffs, fmt.Sprintf("body: baml=%dB native=%dB", len(bamlBody), len(native.Body)))
	}

	// Headers require normalization (case-fold + fail closed on duplicate/multi-
	// value). If EITHER side cannot normalize, preserve the fail-closed intent for
	// the HEADERS facet ONLY: mark headers a mismatch with the structural, redacted
	// reason and flag a meta mismatch so the structural failure is observably
	// distinct from a semantic header-value drift. The facets above keep their real
	// result — a header-only fault no longer fabricates method/target/host/body
	// mismatches.
	bamlHeaders, bErr := testutil.NormalizeHeaders(testutil.PairsFromStringMap(bamlReq.Headers))
	nativeHeaders, nErr := testutil.NormalizeHeaders(pairsFromHeaderFields(native.Headers))
	if bErr != nil || nErr != nil {
		res.Headers = false
		res.MetaMismatch = true
		if bErr != nil {
			diffs = append(diffs, "baml plan headers not normalizable (duplicate/multi-value)")
		}
		if nErr != nil {
			diffs = append(diffs, "native plan headers not normalizable (duplicate/multi-value)")
		}
	} else {
		headerDiffs := headerSemanticDiffs(bamlHeaders, nativeHeaders)
		res.Headers = len(headerDiffs) == 0
		diffs = append(diffs, headerDiffs...)
	}

	sort.Strings(diffs)
	res.Diffs = diffs
	return res
}

// headerSemanticDiffs returns the redacted semantic-header differences between
// BAML's and the native plan's headers. BAML's `baml-original-url` transport
// header is excluded (BAML emits it; nanollm never does) via testutil.SplitSemantic;
// the native side is left unfiltered so a native-only leak would still surface.
// Values are redacted unless allowlisted, so a diagnostic never leaks a token.
func headerSemanticDiffs(bamlHeaders, nativeHeaders map[string]string) []string {
	bamlSem, _ := testutil.SplitSemantic(bamlHeaders)
	nativeSem := nativeHeaders

	var diffs []string
	for name, bv := range bamlSem {
		nv, ok := nativeSem[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("header %q: present on baml only (value %s)", name, testutil.RedactValue(name, bv)))
			continue
		}
		if bv != nv {
			diffs = append(diffs, fmt.Sprintf("header %q: value differs (baml=%s native=%s)", name, testutil.RedactValue(name, bv), testutil.RedactValue(name, nv)))
		}
	}
	for name, nv := range nativeSem {
		if _, ok := bamlSem[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("header %q: present on native only (value %s)", name, testutil.RedactValue(name, nv)))
		}
	}
	return diffs
}

// pairsFromHeaderFields flattens ordered native header fields into the neutral
// pair form testutil normalizes. Repeated names become repeated pairs, which
// testutil rejects — matching the "no duplicate/multi-value" policy.
func pairsFromHeaderFields(fields []llmhttp.HeaderField) [][2]string {
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, [2]string{f.Name, f.Value})
	}
	return out
}

// hostAndTarget splits a full URL into (effective host, request target). The
// "host" facet is a scheme-inclusive ORIGIN (scheme://host), NOT the bare host:
// the transport scheme is part of the effective destination, so http:// vs
// https:// route to different ports/transports and must compare UNEQUAL. Without
// the scheme a scheme drift would be silently recorded as a full match while BAML
// actually sends over a different transport (the exact/native lane does no URL
// rewrite, so a rewrite that only flipped the scheme would go undetected). A URL
// that does not parse or carries userinfo falls back to comparing the raw string
// on both facets so an invalid/credentialed plan still yields a deterministic
// mismatch rather than a panic. Those raw values are used only for equality;
// ComparePlans emits field-only URL diagnostics, never a URL or query value.
func hostAndTarget(rawURL string) (host, target string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil {
		return rawURL, rawURL
	}
	// HTTP hostnames are case-insensitive. Preserve the scheme (which is part
	// of the transport destination) and request target exactly, but normalize
	// the authority before comparing the host facet so equivalent DNS casing
	// cannot create a spurious plan mismatch.
	return u.Scheme + "://" + strings.ToLower(u.Host), u.RequestURI()
}

// CompareStructured compares the native SAP flattened JSON against the BAML-only
// flattened JSON. Both are normalized identically before comparison so a
// difference is attributable to the PARSE, not to representation:
//
//   - structured: absent optionals injected on both (so an omitted-optional
//     asymmetry does not read as a value drift), then a key-order-insensitive
//     deep-equal;
//   - order: additionally reordered to schema field order on both and compared
//     byte-for-byte, so a class field reorder or a nested-map key-order drift is
//     caught (the reorder pass normalizes class order, so a residual byte diff is
//     a real map-order/content divergence).
//
// Both flattened inputs are SENSITIVE parsed provider outputs (they carry the
// model's full structured response); they are NEVER surfaced/logged/returned.
// Only the BOUNDED comparison booleans this function returns — structuredMatch /
// orderMatch — are safe to record.
func CompareStructured(nativeFlat, bamlFlat []byte, schema *bamlutils.DynamicOutputSchema) (structuredMatch, orderMatch bool) {
	nInj, e1 := bamlutils.InjectAbsentOptionals(nativeFlat, schema)
	bInj, e2 := bamlutils.InjectAbsentOptionals(bamlFlat, schema)
	if e1 != nil || e2 != nil {
		return false, false
	}
	structuredMatch = jsonSemEqual(nInj, bInj)

	nOrd, e3 := bamlutils.ReorderDynamicOutputBySchema(nInj, schema)
	bOrd, e4 := bamlutils.ReorderDynamicOutputBySchema(bInj, schema)
	if e3 != nil || e4 != nil {
		return structuredMatch, false
	}
	orderMatch = bytes.Equal(nOrd, bOrd)
	return structuredMatch, orderMatch
}

// jsonSemEqual reports whether two JSON documents are semantically equal —
// object key order ignored, arrays order-sensitive, scalars by value. It mirrors
// the Phase 6c differential's liveJSONSemEqual so the shadow oracle, the serving
// path, and the gated differential all agree on what "structurally equal" means.
func jsonSemEqual(a, b []byte) bool {
	av, aok := decodeJSONNumber(a)
	bv, bok := decodeJSONNumber(b)
	if !aok || !bok {
		return false
	}
	return jsonDeepEqual(av, bv)
}

// decodeJSONNumber decodes JSON with UseNumber so numeric values become
// json.Number (string-backed) rather than float64. This makes the structured
// comparison EXACT for large/precise integers — two documents that differ only
// in a value beyond float64's 53-bit mantissa (e.g. a 64-bit id) would otherwise
// compare equal after lossy float64 coercion. Preserves the false-on-decode-error
// contract.
func decodeJSONNumber(data []byte) (any, bool) {
	dec := stdjson.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// Require EOF after the first value: a payload with a TRAILING JSON value
	// (e.g. {"x":1}{"ignored":2}) or trailing non-whitespace is malformed input,
	// not a single comparable document. Feeding only the first value to the
	// caller would violate the false-on-bad-input contract (and could report a
	// spurious structured match). Any non-EOF here — a decoded second value
	// (err == nil) or a syntax error — fails closed.
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, false
	}
	return v, true
}

// jsonDeepEqual recursively compares two decoded JSON values: object key order is
// ignored, array order is significant, numbers compare by exact NUMERIC VALUE
// (json.Number via jsonNumberEqual, not lexical spelling), and other scalars
// (string/bool/nil) compare by value.
func jsonDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok || !jsonDeepEqual(va, vb) {
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
			if !jsonDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case stdjson.Number:
		// A number is equal ONLY to another number, compared by exact numeric value
		// (decodeJSONNumber uses UseNumber, so all JSON numbers arrive as
		// json.Number). Never lexical — 1.50 and 1.5 are equal.
		bn, ok := b.(stdjson.Number)
		return ok && jsonNumberEqual(av, bn)
	default:
		return a == b
	}
}

// Bounds on a JSON number token BEFORE it is handed to big.Rat.SetString, so a
// provider-controlled compact token (e.g. "1e1000000", which SetString would
// expand into a ~1M-digit rational) cannot amplify allocation/CPU. The caps are
// far above any legitimate value (a 256-bit integer is ~78 digits; real
// float exponents are tiny), so bounded numbers still compare EXACTLY.
const (
	maxNumberMantissaDigits = 1000
	maxNumberExponent       = 1000
)

// jsonNumberEqual compares two JSON numbers by exact NUMERIC VALUE (not lexical
// spelling) with NO float64 precision loss, via math/big.Rat parsed from each
// json.Number string. Equivalent spellings (1.50 == 1.5, 1e2 == 100, trailing
// zeros) compare equal, while two genuinely-distinct large integers beyond
// float64's 53-bit mantissa stay UNEQUAL. An unparseable number returns false
// (never panics), preserving the false-on-bad-input contract.
//
// DoS-hardened: (1) byte-IDENTICAL tokens are equal WITHOUT parsing (handles a
// huge-but-identical token cheaply); (2) an oversized token — more than
// maxNumberMantissaDigits significant digits or |exponent| over
// maxNumberExponent — is NEVER parsed: if either operand is out of budget and the
// tokens are not identical (already handled), FAIL CLOSED as a mismatch. Bounded
// operands keep exact big.Rat comparison.
func jsonNumberEqual(a, b stdjson.Number) bool {
	// (1) Identical-token fast path — equal WITHOUT parsing, but only for a
	// WELL-FORMED number: an invalid identical token (e.g. "not-a-number") falls
	// through so the normal path yields the documented false-on-unparseable
	// result. A valid-but-huge identical token still returns true here cheaply
	// (validJSONNumber is an O(len) scan, no big.Rat blowup).
	if a == b {
		return validJSONNumber(string(a))
	}
	// (2) Bounded-digit policy — refuse to parse an oversized rational; a non-
	// identical out-of-budget operand fails closed (mismatch), never big.Rat.
	if !numberWithinBudget(a.String()) || !numberWithinBudget(b.String()) {
		return false
	}
	ar, aok := new(big.Rat).SetString(a.String())
	br, bok := new(big.Rat).SetString(b.String())
	if !aok || !bok {
		return false
	}
	return ar.Cmp(br) == 0
}

// validJSONNumber reports whether s is a syntactically well-formed JSON number
// (RFC 8259 grammar: optional '-', int, optional frac, optional exp) via an
// O(len) scan that never allocates. Used to gate the identical-token fast path so
// an unparseable token is never accepted as equal, while a valid-but-oversized
// token (e.g. "1e1000000") is accepted cheaply without big.Rat.
func validJSONNumber(s string) bool {
	i, n := 0, len(s)
	if n == 0 {
		return false
	}
	if s[i] == '-' {
		i++
	}
	// int: '0' | [1-9] digit*
	if i >= n {
		return false
	}
	if s[i] == '0' {
		i++
	} else if s[i] >= '1' && s[i] <= '9' {
		i++
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	// frac: '.' digit+
	if i < n && s[i] == '.' {
		i++
		start := i
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false // '.' with no digits
		}
	}
	// exp: (e|E) (+|-)? digit+
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < n && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false // 'e' with no digits
		}
	}
	return i == n
}

// numberWithinBudget reports whether a JSON number token stays within the
// mantissa-digit and exponent-magnitude bounds — a cheap O(len) scan that never
// allocates a rational. It counts significant (mantissa) digits and bounds the
// |exponent| so an oversized/pathological token is rejected BEFORE big.Rat sees
// it. The exponent magnitude is accumulated DIGIT-BY-DIGIT with a capped
// accumulator (never strconv.Atoi + signed negation, which has a signed-min-int
// overflow bypass): any non-digit or exceeding the cap is out of budget (fail
// closed). big.Rat.SetString still fully validates a bounded token afterward.
func numberWithinBudget(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	mantissaDigits := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c == 'e' || c == 'E' {
			j := i + 1
			if j < len(s) && (s[j] == '+' || s[j] == '-') {
				j++
			}
			if j >= len(s) {
				return false // 'e' with no exponent digits -> out of budget
			}
			exp := 0
			for ; j < len(s); j++ {
				d := s[j]
				if d < '0' || d > '9' {
					return false // non-digit in the exponent -> out of budget
				}
				exp = exp*10 + int(d-'0')
				if exp > maxNumberExponent {
					return false // |exponent| over the cap -> out of budget
				}
			}
			return mantissaDigits <= maxNumberMantissaDigits
		}
		if c >= '0' && c <= '9' {
			mantissaDigits++
			if mantissaDigits > maxNumberMantissaDigits {
				return false // short-circuit a giant mantissa without scanning it all
			}
		}
		// '.' (and any stray char) is ignored here; big.Rat.SetString still
		// validates the full token for the bounded path.
	}
	return mantissaDigits <= maxNumberMantissaDigits
}
