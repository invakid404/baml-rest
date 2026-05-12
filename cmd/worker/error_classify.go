package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// Legacy CallStream+OnTick BAML errors arrive across the Go FFI as plain
// strings (language_client_go's BamlError carries Message only — its
// typed BamlClientHttpError is defined but never constructed). BAML's
// public display envelopes are stable enough to anchor a conservative
// classifier on the exact prefixes below; broader substrings like
// "Failed to parse LLM response:" appear in provider response handlers
// and internal paths too and are deliberately not matched.
//
// Source: engine/baml-runtime/src/errors.rs Display impls for
// ValidationError / ClientHttpError / TimeoutError.
const (
	legacyParseErrorPrefix       = "Parsing error: "
	legacyLLMClientPrefix        = `LLM client "`
	legacyLLMClientStatusMarker  = `" failed with status code: `
	legacyLLMClientTimeoutMarker = `" timed out:`
)

// legacyStatusCodeRE extracts the numeric status code from BAML
// v0.219's ClientHttpError envelope. BAML renders the ErrorCode enum
// via Display before the "\nMessage: " body, producing one of these
// shapes (engine/baml-runtime/.../internal/llm_client/mod.rs:249-259):
//
//	InvalidAuthentication (401)  — named enum, parenthesized digits
//	NotSupported (403)             (same shape for all named variants:
//	RateLimited (429)              from_status maps known HTTP codes
//	ServerError (500)              into this allowlist)
//	ServiceUnavailable (503)
//	Timeout (408)
//	BadResponse <code>           — UnsupportedResponse(u16) variant
//	Unspecified error code: <N>  — Other(u16) variant for any code
//	                               BAML didn't recognize
//
// Earlier/future BAML versions could emit bare leading digits, so the
// first alternative keeps that path for compatibility.
//
// The whole alternation is wrapped under a single `^` anchor — Go's
// regexp distributes a top-level `^` only to the first alternative,
// so without the non-capturing group an unrecognized future enum
// display like `SomeNewEnum (599)` would match the parenthesized
// branch mid-segment and leak the wrong number. Pinning the
// parenthesized branch to BAML's exact enum spellings means any new
// named variant added upstream falls through to provider_error with
// nil details — under-classify rather than misclassify. Bumping the
// BAML pin should re-check this allowlist against the current
// ErrorCode Display impl.
var legacyStatusCodeRE = regexp.MustCompile(
	`^(?:(\d+)|(?:InvalidAuthentication|NotSupported|RateLimited|ServerError|ServiceUnavailable|Timeout) \((\d+)\)|(?:BadResponse|Unspecified error code:)\s+(\d+))`,
)

// classifyBAMLError inspects a worker-side error from a BAML call /
// BuildRequest path and returns the apierror.Code that best fits, plus
// optional JSON-encoded details. Returns ("", nil) when no surface
// matched, in which case the host's classifyWorkerError preserves any
// existing pluginResult.ErrorCode or defaults to worker_error.
//
// Typed BuildRequest surfaces are checked first: the typed parse
// wrapper, llmhttp's *HTTPError, and the transport-flake umbrella
// sentinel. Legacy CallStream+OnTick errors fall through to exact-
// prefix matching against BAML's stable public error envelopes. The
// classifier intentionally fails closed (returns "") rather than
// stretching a substring match — under-classifying lets the host's
// worker_error default fire; mis-classifying leaks the wrong code into
// the public taxonomy.
//
// errors.As is used (not a direct type assertion) so wrappers like
// fmt.Errorf("buildrequest: %w", &llmhttp.HTTPError{...}) still match.
//
// Returned details bytes are produced via json.Marshal on a fixed-shape
// struct, so the resulting payload is always well-formed JSON. The
// returned slice is owned by the caller; assigning it directly to
// workerplugin.StreamResult.ErrorDetails is safe.
func classifyBAMLError(err error) (code string, details []byte) {
	if err == nil {
		return "", nil
	}

	if errors.Is(err, buildrequest.ErrOutputParse) {
		return string(apierror.CodeParseError), nil
	}

	var httpErr *llmhttp.HTTPError
	if errors.As(err, &httpErr) {
		return string(apierror.CodeProviderError), statusCodeDetails(httpErr.StatusCode)
	}

	if errors.Is(err, llmhttp.ErrTransportFlake) {
		return string(apierror.CodeProviderError), nil
	}

	// Legacy FFI string surfaces — checked only after every typed branch
	// has been ruled out. Matching against a wrapper's Error() is
	// acceptable here because BAML's prefixes are anchored at the very
	// start of the wrapped chain (the FFI String is the root error) and
	// we never use Contains for the prefix itself.
	msg := err.Error()

	if strings.HasPrefix(msg, legacyParseErrorPrefix) {
		return string(apierror.CodeParseError), nil
	}

	if strings.HasPrefix(msg, legacyLLMClientPrefix) {
		if idx := strings.Index(msg, legacyLLMClientStatusMarker); idx != -1 {
			if status, ok := parseLegacyStatusCode(msg[idx+len(legacyLLMClientStatusMarker):]); ok {
				return string(apierror.CodeProviderError), statusCodeDetails(status)
			}
			// Status code couldn't be parsed — still a provider failure;
			// emit the code without details rather than dropping the
			// classification altogether.
			return string(apierror.CodeProviderError), nil
		}
		if strings.Contains(msg, legacyLLMClientTimeoutMarker) {
			return string(apierror.CodeProviderError), nil
		}
	}

	return "", nil
}

// statusCodeDetails marshals a {"status_code": N} payload. The response
// body and other provider context are deliberately not forwarded —
// upstream bodies can contain partial PII, raw prompts echoed back, or
// large diagnostic payloads, and the brief constrains baml-rest to a
// minimal structured surface. Marshal of a single-int-field struct
// can't realistically fail; if it ever does, fall back to no details
// rather than dropping the classification.
func statusCodeDetails(status int) []byte {
	payload := struct {
		StatusCode int `json:"status_code"`
	}{StatusCode: status}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return data
}

// parseLegacyStatusCode extracts the numeric status code from the
// segment of a BAML ClientHttpError envelope that follows
// `failed with status code: `. The segment is bounded to the first
// newline (BAML appends "\nMessage: <body>") before scanning so a
// digit run inside the trailing provider body can't be misread as the
// status code. Returns (0, false) when no anchored form matches —
// callers keep the provider_error classification and drop details.
func parseLegacyStatusCode(segment string) (int, bool) {
	if nl := strings.IndexByte(segment, '\n'); nl != -1 {
		segment = segment[:nl]
	}
	matches := legacyStatusCodeRE.FindStringSubmatch(segment)
	if matches == nil {
		return 0, false
	}
	// Alternation captures land in different groups depending on which
	// alternative matched; the first non-empty group is the digits.
	for _, group := range matches[1:] {
		if group == "" {
			continue
		}
		status, err := strconv.Atoi(group)
		if err != nil {
			return 0, false
		}
		return status, true
	}
	return 0, false
}
