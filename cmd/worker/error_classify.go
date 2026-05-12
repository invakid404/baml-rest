package main

import (
	"errors"
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
			tail := msg[idx+len(legacyLLMClientStatusMarker):]
			// Trim everything past the status-code digits (BAML appends
			// the response body and other context after the number).
			end := 0
			for end < len(tail) && tail[end] >= '0' && tail[end] <= '9' {
				end++
			}
			if end > 0 {
				if status, parseErr := strconv.Atoi(tail[:end]); parseErr == nil {
					return string(apierror.CodeProviderError), statusCodeDetails(status)
				}
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
