package worker

import (
	"context"
	"fmt"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// workerParseInput wraps the input for parse requests.
type workerParseInput struct {
	Raw string `json:"raw"`
	// Stream selects the parse-STREAM (partial) path: when true, Parse drives
	// the method's StreamImpl (BAML ParseStream) instead of the final Impl.
	// Absent/false on every production /parse input, so the default path is
	// unchanged; only the dynamic parse-stream oracle sets it.
	Stream  bool                   `json:"stream,omitempty"`
	Options *bamlutils.BamlOptions `json:"__baml_options__,omitempty"`
}

// Parse executes a BAML parse method against a raw response string.
// Parse intentionally does not thread a round-robin advancer — parsing
// is a local CPU operation and never dispatches against a baml-roundrobin
// client.
func (h *Handler) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	method, ok := h.runtime.ParseMethod(methodName)
	if !ok {
		return nil, fmt.Errorf("parse method %q not found", methodName)
	}

	// Parse input
	var input workerParseInput
	if err := sonic.Unmarshal(inputJSON, &input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	if input.Raw == "" {
		return nil, fmt.Errorf("missing required field 'raw'")
	}

	// Create adapter and apply options
	adapter := h.runtime.MakeAdapter(ctx)
	h.configureAdapter(adapter)
	adapter.SetLogger(h.logger)
	if input.Options != nil {
		opts := workerBamlOptions{Options: input.Options}
		if err := opts.apply(adapter, h.clientDefaults, h.baseURLRewrites); err != nil {
			return nil, fmt.Errorf("failed to apply options: %w", err)
		}
	}

	// Select the final or parse-stream implementation. Stream=true drives
	// BAML's ParseStream over the accumulated prefix (the parse-stream
	// oracle); a method with no StreamImpl cannot service a Stream request.
	// This is a real error (the method does not expose parse-stream), not a
	// native fallback: native de-BAML stream parsing is not wired at this
	// seam.
	impl := method.Impl
	if input.Stream {
		if method.StreamImpl == nil {
			return nil, fmt.Errorf("parse method %q does not support stream parse", methodName)
		}
		impl = method.StreamImpl
	}

	// Call the selected parse implementation.
	result, err := impl(adapter, input.Raw)
	if err != nil {
		// Wrap with any typed classification so the gRPC layer's
		// errors.As against GetCode()/GetDetails() picks it up
		// (workerplugin/grpc.go:220+). The /parse host endpoint also
		// has a fallback rewrite from worker_error to parse_error, so
		// leaving the code empty is safe; wrapping just lets typed
		// surfaces (e.g. an underlying *llmhttp.HTTPError surfaced
		// through a BAML adapter that propagates the wrap chain) land
		// as the more specific code.
		if code, details := classifyBAMLError(err); code != "" {
			return nil, workerplugin.NewErrorWithMetadata(err, "", code, details)
		}
		return nil, err
	}

	// Marshal the result to JSON
	data, err := sonic.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal parse result: %w", err)
	}

	return &workerplugin.ParseResult{Data: data}, nil
}
