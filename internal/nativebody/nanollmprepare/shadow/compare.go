// Package shadow is the de-BAML cutover Slice 4 one-send SHADOW comparator: for
// an admitted unary `_dynamic` call it builds the native request plan (via the
// S3 admission predicate), obtains BAML's built request plan for the SAME
// selected child WITHOUT sending, compares the two field by field, records a
// bounded plan_compare result (NO values), and then DECLINES so the orchestrator
// serves BAML unchanged. Native NEVER opens a socket / RoundTrips here.
//
// It lives in the out-of-go.work nanollmprepare module and links nanollm (through
// the admission package), so nothing here can enter the host/root link graph —
// the host stays zero-nanollm and CGO-free. A SHADOW deploy profile's worker
// injects it as a neutral bamlutils.NativeShadowFunc; every default build leaves
// it nil, so the orchestrator's native child-attempt callback stays hard-off.
package shadow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/testutil"
)

// planComparison is the per-field native-vs-BAML request-plan comparison result.
// Each bool is true when that facet MATCHES. Diffs is a sorted, secret-free,
// REDACTED list of human-readable differences (empty on a full match) suitable
// for optional structured logging — it never carries a header value, body byte,
// prompt text, api key, or bearer token.
type planComparison struct {
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

// allMatch reports whether every compared field matched.
func (c planComparison) allMatch() bool {
	return c.Method && c.Target && c.Host && c.Headers && c.Body
}

// comparePlans compares BAML's built request plan against the native prepared
// plan across method, request target (path+query), effective host, semantic
// headers, and raw body bytes. Neither plan is sent; both are built-but-unsent.
//
// It reuses the pure testutil normalizer/redactor so the header policy matches
// the Phase 5/6 differential exactly: header names are case-insensitive,
// duplicate/multi-value headers fail closed, BAML's internal `baml-original-url`
// transport header is excluded from the BAML side only, and every non-allowlisted
// header value is redacted in a diagnostic. Bodies are compared byte-for-byte but
// NEVER printed — only a length+hash digest is surfaced.
func comparePlans(bamlReq *llmhttp.Request, native *llmhttp.ExactAttemptRequest) planComparison {
	var diffs []string
	res := planComparison{}

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
	res.Body = bytesEqual(bamlBody, native.Body)
	if !res.Body {
		diffs = append(diffs, fmt.Sprintf("body: baml=%s native=%s", bodyDigest(bamlBody), bodyDigest(native.Body)))
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
// comparePlans emits field-only URL diagnostics, never a URL or query value.
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

// bytesEqual reports byte-for-byte equality.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bodyDigest renders a body as "<n>B sha256:<12hex>" — never raw bytes — so a
// diagnostic holds no prompt text or credential.
func bodyDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%dB sha256:%s", len(b), hex.EncodeToString(sum[:])[:12])
}
