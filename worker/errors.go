package worker

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Worker-facing apierror code strings, duplicated as unexported
// constants so this nested module does not need to import the parent
// root module's internal/apierror package just to spell the values.
// These mirror the public apierror.Code constants the host serializes
// onto the wire (parse_error / provider_error / internal_error); they
// must stay in sync with internal/apierror/error.go.
const (
	workerCodeParseError    = "parse_error"
	workerCodeProviderError = "provider_error"
	workerCodeInternalError = "internal_error"
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

// providerErrorDetails is the structured payload attached to
// provider_error responses. baml-rest is a developer tool — the
// operator running it is the consumer of these details and is the
// right place to decide what to do with upstream response bodies. So
// we forward what we have: the HTTP status code when known, the raw
// upstream body, and (on legacy envelopes) the BAML client name that
// failed. Empty fields are omitted so the envelope stays terse for
// arms that genuinely don't have that signal (e.g. transport flakes
// carry neither status nor body).
type providerErrorDetails struct {
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	ClientName string `json:"client_name,omitempty"`
	// ErrorCode/ErrorMessage carry the AWS event-stream
	// :error-code / :error-message header values when a Bedrock
	// stream surfaces a transport-level error frame.
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// ExceptionType / ExceptionMessage carry the modeled
	// exception's :exception-type header value and the parsed
	// `message` field from the exception payload — the in-band
	// failure shape Bedrock uses when the model itself refuses or
	// the service produces a domain-level error mid-stream
	// (ModelStreamErrorException, ThrottlingException,
	// ValidationException, etc.). Distinct from
	// ErrorCode/ErrorMessage so wire consumers can tell a torn
	// transport apart from a modeled exception even when both map
	// to provider_error.
	ExceptionType    string `json:"exception_type,omitempty"`
	ExceptionMessage string `json:"exception_message,omitempty"`
}

// marshal returns the JSON-encoded bytes, or nil when every field is
// at its zero value (in which case the caller drops details entirely
// rather than emitting a bare `{}`). Marshal of a fixed-shape struct
// can't realistically fail; if it ever does, fall back to no details
// rather than dropping the classification.
func (d providerErrorDetails) marshal() []byte {
	if d.StatusCode == 0 && d.Body == "" && d.ClientName == "" &&
		d.ErrorCode == "" && d.ErrorMessage == "" &&
		d.ExceptionType == "" && d.ExceptionMessage == "" {
		return nil
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	return data
}

// mergeRawDetail merges the accumulated raw text from a legacy
// CallStream+OnTick failure (see #256) into the JSON-encoded details
// object returned by classifyBAMLError. The forwarding is not gated on
// classification: parse_error, provider_error, and unclassified errors
// all get details.raw when the extractor accumulated bytes before the
// failure. Consumers reading the apierror envelope no longer need to
// regex-scrape "\nRaw Response: ..." out of BAML's free-form error
// string — same posture as provider_error's details.body (#248).
//
// Behavior:
//   - raw == "": returns details unchanged (the helper is a no-op so
//     omitempty stays in force on the worker→host wire).
//   - details == nil: returns {"raw": raw} as a fresh JSON object.
//   - details is a JSON object: unmarshals, sets "raw", marshals back.
//
// All existing details fields (status_code, body, client_name, etc.)
// are preserved. The marshal-back step uses map[string]any rather than
// the typed struct so we don't drop unrecognized fields a future
// classifier may add.
//
// Defensive fallback: if details is non-empty but isn't a JSON object,
// the helper returns a fresh {"raw": raw} object rather than dropping
// raw on the floor. Today every classifyBAMLError path returns either
// nil or a marshalled providerErrorDetails (always object-shaped), so
// this branch is dead — kept so a future classifier returning
// non-object details doesn't silently lose the diagnostic.
func mergeRawDetail(details []byte, raw string) []byte {
	if raw == "" {
		return details
	}
	if len(details) == 0 {
		out, err := json.Marshal(map[string]any{"raw": raw})
		if err != nil {
			return details
		}
		return out
	}
	var obj map[string]any
	if err := json.Unmarshal(details, &obj); err != nil || obj == nil {
		out, marshalErr := json.Marshal(map[string]any{"raw": raw})
		if marshalErr != nil {
			return details
		}
		return out
	}
	obj["raw"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return details
	}
	return out
}

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
		return workerCodeParseError, nil
	}

	var httpErr *llmhttp.HTTPError
	if errors.As(err, &httpErr) {
		return workerCodeProviderError, providerErrorDetails{
			StatusCode: httpErr.StatusCode,
			Body:       httpErr.Body,
		}.marshal()
	}

	if errors.Is(err, llmhttp.ErrTransportFlake) {
		return workerCodeProviderError, nil
	}

	// AWS event-stream transport errors arrive in-band on the wire as
	// :message-type=error frames. awsstream.Decoder surfaces these as
	// *TransportError with the AWS-specific :error-code /
	// :error-message header values; map them to provider_error so the
	// taxonomy treats them the same as an HTTP-layer 5xx. The AWS
	// codes (e.g. "InternalServerError", "ThrottlingException") and
	// message ride along in details for caller diagnostics.
	var awsTransportErr *awsstream.TransportError
	if errors.As(err, &awsTransportErr) {
		return workerCodeProviderError, providerErrorDetails{
			ErrorCode:    awsTransportErr.Code,
			ErrorMessage: awsTransportErr.Message,
		}.marshal()
	}

	// Bedrock modeled exceptions arrive in-band as
	// :message-type=exception frames; the orchestrator wraps them
	// as *buildrequest.BedrockStreamException with the modeled
	// shape name in ExceptionType and the operator-facing message
	// reachable via Message(). They're a provider-side failure
	// class — model refusal, throttling, validation — so they map
	// to provider_error alongside transport errors but with their
	// own detail fields so consumers can tell the two failure
	// modes apart.
	var bedrockExc *buildrequest.BedrockStreamException
	if errors.As(err, &bedrockExc) {
		return workerCodeProviderError, providerErrorDetails{
			ExceptionType:    bedrockExc.ExceptionType,
			ExceptionMessage: bedrockExc.Message(),
		}.marshal()
	}

	// Legacy FFI string surfaces — checked only after every typed branch
	// has been ruled out. Matching against a wrapper's Error() is
	// acceptable here because BAML's prefixes are anchored at the very
	// start of the wrapped chain (the FFI String is the root error) and
	// we never use Contains for the prefix itself.
	msg := err.Error()

	// Marker detection is scoped to the envelope's first line. BAML's
	// ClientHttpError/TimeoutError Display impls put the recognized
	// markers on the public envelope line and the upstream body after
	// "\nMessage:" — so a malformed or future BAML string whose first
	// line starts with `LLM client "...` but lacks the recognized
	// marker must not be misclassified just because the message body
	// happens to contain `failed with status code:` or `timed out:`
	// verbatim. The split here also yields the body forwarded to
	// callers via providerErrorDetails.Body.
	firstLine := msg
	body := ""
	if nl := strings.IndexByte(msg, '\n'); nl != -1 {
		firstLine = msg[:nl]
		body = msg[nl+1:]
	}

	if strings.HasPrefix(firstLine, legacyParseErrorPrefix) {
		return workerCodeParseError, nil
	}

	if name, suffix, ok := splitLegacyLLMClientEnvelope(firstLine); ok {
		// Markers must begin immediately at the closing client-name
		// quote — both legacyLLMClientStatusMarker and
		// legacyLLMClientTimeoutMarker start with `"`, and the suffix
		// is sliced to include that quote. This forecloses an intra-
		// line false positive where the marker text appears somewhere
		// in firstLine but not at the post-quote anchor (e.g. a client
		// name with embedded quotes whose later content happens to
		// match the marker substring).
		if strings.HasPrefix(suffix, legacyLLMClientStatusMarker) {
			d := providerErrorDetails{Body: body, ClientName: name}
			if status, ok := parseLegacyStatusCode(suffix[len(legacyLLMClientStatusMarker):]); ok {
				d.StatusCode = status
			}
			return workerCodeProviderError, d.marshal()
		}
		if strings.HasPrefix(suffix, legacyLLMClientTimeoutMarker) {
			return workerCodeProviderError, providerErrorDetails{
				Body:       body,
				ClientName: name,
			}.marshal()
		}
	}

	return "", nil
}

// splitLegacyLLMClientEnvelope splits a BAML envelope first line into
// the client name and the suffix that begins at the closing client-
// name quote. The suffix includes that quote so it can be matched
// directly against legacyLLMClientStatusMarker /
// legacyLLMClientTimeoutMarker (both of which start with `"`).
//
// Returns ok=false when firstLine doesn't start with `LLM client "`,
// has no closing quote, or has an empty client name — three malformed
// envelope shapes baml-rest can't classify safely. The empty-name
// case is gated here (rather than at the caller) so the marker check
// only ever runs against a well-formed envelope.
func splitLegacyLLMClientEnvelope(firstLine string) (name, suffix string, ok bool) {
	if !strings.HasPrefix(firstLine, legacyLLMClientPrefix) {
		return "", "", false
	}
	rest := firstLine[len(legacyLLMClientPrefix):]
	q := strings.IndexByte(rest, '"')
	if q <= 0 {
		// q == -1: missing closing quote; q == 0: empty client name.
		// Both are malformed envelopes — fail closed.
		return "", "", false
	}
	return rest[:q], rest[q:], true
}

// extractLegacyClientName returns just the client-name part of a BAML
// envelope first line. Thin wrapper around splitLegacyLLMClientEnvelope
// for call sites that only need the name; the underlying helper also
// produces the suffix the marker checks anchor against.
func extractLegacyClientName(firstLine string) string {
	name, _, _ := splitLegacyLLMClientEnvelope(firstLine)
	return name
}

// parseLegacyStatusCode extracts the numeric status code from the
// segment of a BAML ClientHttpError envelope that follows
// `failed with status code: `. The segment is bounded to the first
// newline (BAML appends "\nMessage: <body>") before scanning so a
// digit run inside the trailing provider body can't be misread as the
// status code. Returns (0, false) when no anchored form matches —
// callers keep the provider_error classification with body/client_name
// details but drop the status_code field.
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
