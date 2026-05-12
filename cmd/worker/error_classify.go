package main

import (
	"errors"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// classifyBAMLError inspects a worker-side error from a BAML call /
// BuildRequest path and returns the apierror.Code that best fits, plus
// optional JSON-encoded details. Returns ("", nil) when no typed surface
// matched, in which case the host's classifyWorkerError preserves any
// existing pluginResult.ErrorCode or defaults to worker_error.
//
// The mapping intentionally only covers typed surfaces — typed parse
// wrappers from BuildRequest, typed HTTP errors from llmhttp, and the
// transport-flake umbrella sentinel. Legacy CallStream+OnTick BAML
// errors arrive as opaque strings and are NOT classified here; PR 3 of
// #245 adds conservative prefix matching at the same boundary.
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
		// status_code is the single piece of structured upstream context
		// worth surfacing today. The response body is deliberately not
		// forwarded — provider error bodies can contain partial PII,
		// raw prompts echoed back, or large diagnostic payloads, and
		// the brief constrains baml-rest to a minimal structured surface.
		payload := struct {
			StatusCode int `json:"status_code"`
		}{StatusCode: httpErr.StatusCode}
		data, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			// Marshal of a single-int-field struct can't realistically
			// fail; if it ever does, fall back to the code without details
			// rather than dropping the classification altogether.
			return string(apierror.CodeProviderError), nil
		}
		return string(apierror.CodeProviderError), data
	}

	if errors.Is(err, llmhttp.ErrTransportFlake) {
		return string(apierror.CodeProviderError), nil
	}

	return "", nil
}
