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
		if err := opts.apply(adapter, h.clientDefaults, h.baseURLRewrites, h.trustedClients); err != nil {
			return nil, fmt.Errorf("failed to apply options: %w", err)
		}
	}

	// De-BAML serving cutover S1: report this request to the DIRECT-PARSE observer,
	// if a native-capable worker installed one. Reported here — after the input is
	// decoded, before BAML runs — so the observation corresponds to a real, well-formed
	// parse request rather than to a malformed one that never reached a parser.
	//
	// This is a telemetry sink and nothing else: it returns nothing, its result is not
	// consulted, and the BAML call below is byte-identical whether or not it is
	// installed. A panic inside it is contained rather than allowed to fail a parse the
	// observer has no business failing — an observability seam must never be able to
	// break the request it observes.
	h.observeDirectParse(ctx, input.Stream)

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

	// De-BAML native-first DYNAMIC direct parse (final mode). Runs the injected
	// native parser against the carried output schema and this exact raw string
	// BEFORE BAML, and returns nil for everything outside that narrow lane —
	// static methods, parse-stream, a schema-less request, a build with no native
	// parser, and every flag-off build — leaving the path below byte-identical to
	// what it has always been. See direct_parse_native.go.
	native := h.beginNativeDirectParse(ctx, methodName, &input)
	if native != nil {
		// The oracle leg must be a GENUINE BAML parse of the same input, so turn
		// the de-BAML flag OFF on this adapter for the call below. The generated
		// dynamic parse entry point consults it first, so without this the "BAML"
		// leg would take the generated native seam and the comparison would be
		// native against itself. The adapter is per-request and is not used after
		// this parse, so nothing else observes the change.
		adapter.SetDeBAMLConfig(bamlutils.DeBAMLConfig{})
	}

	// Call the selected parse implementation.
	result, err := impl(adapter, input.Raw)
	if err != nil {
		// The transition oracle: BAML rejected this input, so BAML's error is what
		// the caller gets. Record what native had claimed for it.
		if native != nil {
			native.settleBAMLError()
		}
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
		// BAML recovered a value with no JSON form (a non-finite float). There is
		// no BAML payload for the oracle to hold native against, so the request
		// fails exactly as it always did — but the disposition is still recorded,
		// so every request the bridge handled accounts for itself.
		if native != nil {
			native.settleBAMLResultUnusable()
		}
		return nil, fmt.Errorf("failed to marshal parse result: %w", err)
	}

	// The transition oracle: native's payload is served ONLY when it is
	// byte-identical to the BAML payload just produced for the same input. Any
	// disagreement — drift, a claimed native error, an unsupported shape — falls
	// through to `data` below, so BAML's answer stands.
	if native != nil {
		if nativeData, ok := native.settle(data); ok {
			return &workerplugin.ParseResult{Data: nativeData}, nil
		}
	}

	return &workerplugin.ParseResult{Data: data}, nil
}

// observeDirectParse reports one direct `/parse/{method}` request to the installed
// observer, or does nothing when none is installed (every default and flag-off
// build). It contains a panic from the observer: the sink is advisory, so a bug in
// it must not turn a working BAML parse into a failed request.
func (h *Handler) observeDirectParse(ctx context.Context, stream bool) {
	obs := h.nativeDirectParseObserver
	if obs == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil && h.logger != nil {
			// Bounded, secret-free: the observation carries no method name, input or
			// output, so there is nothing here to redact.
			h.logger.Error("de-BAML direct-parse observer panicked; the parse itself is unaffected")
		}
	}()
	obs(ctx, bamlutils.NativeDirectParseObservation{Stream: stream})
}
