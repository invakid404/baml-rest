package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML: native-first DYNAMIC direct parse (final mode) with a same-input BAML
// TRANSITION ORACLE.
//
// # What this is
//
// `/parse/{method}` was the one serving surface with no native claim of any kind:
// the dynamic method's generated parse entry point could reach the native parser,
// but nothing held that native answer against BAML's, so a native result was
// served on trust. This bridge moves the decision into the worker and makes it
// EARNED per request:
//
//  1. run the injected native parser (bamlutils.DeBAMLParseFunc — internal/debaml.Parse
//     in every real build) against the carried dynamic OutputSchema and the exact
//     raw string;
//  2. run BAML's parse on the SAME raw input, with the de-BAML flag turned OFF for
//     that call so it is a genuine BAML parse and not a second trip through the
//     native seam;
//  3. serve native's bytes ONLY when they are byte-identical to BAML's. On ANY
//     disagreement — drift, a claimed native error, an unsupported shape, an
//     unusable native result — BAML's answer is what the caller gets.
//
// # Why the comparison is on raw bytes
//
// The comparison is `bytes.Equal` over the WORKER-BOUNDARY payload: the exact
// `ParseResult.Data` each leg would return. Nothing downstream (the flatten,
// absent-optional injection and order/sort passes in the host and dynclient)
// re-canonicalizes string escaping or number spelling, so two payloads that are
// merely "semantically equal" can still reach the wire as different bytes. Holding
// native to the bytes BAML produced is therefore the only comparison that supports
// the claim this bridge makes: a native win changes NOTHING a client can observe.
// The native leg is re-encoded through the same order-preserving decode + sonic
// encode the BAML envelope goes through so the two are compared in one encoding
// space; a native result that survives that and still differs is a real
// disagreement and declines.
//
// # What stays on BAML, unconditionally
//
// Static `/parse` methods, parse-STREAM (`Stream=true`), a request carrying no
// dynamic output schema, a build with no native parser injected, and every
// flag-off build: none of them enter this bridge at all, so they run the exact
// BAML path they ran before. The native parser's own cut-line (constraints, media,
// recursive shapes, general unions, un-lowerable schemas) is enforced inside it and
// surfaces here as bamlutils.ErrDeBAMLParseUnsupported — an ordinary decline.
//
// # Never out-claim
//
// That is the invariant this file exists to hold, and it is structural rather than
// aspirational: the ONLY path that returns native bytes requires those bytes to
// equal BAML's, which the same request just computed. Weakening the native parser
// to claim more would not help it here — a wrong answer simply fails the byte
// comparison and BAML serves.

// nativeDirectParse is one request's native leg, computed BEFORE the BAML oracle
// leg runs. It holds everything needed to settle the disposition once BAML's answer
// is known, and nothing else; it is created per request and never shared.
type nativeDirectParse struct {
	h *Handler

	// data is the worker-boundary payload native would return: its flattened parse
	// output re-encoded and wrapped in the dynamic-output envelope. Non-nil only
	// when the native parser reported success AND that output could be re-encoded.
	data []byte

	// err is the native parser's error, or nil on success.
	// bamlutils.ErrDeBAMLParseUnsupported means "outside the native cut-line";
	// anything else is a CLAIMED native parse failure.
	err error

	// unusable records that native reported success but its JSON could not be
	// re-encoded into the envelope this surface returns. Distinct from err (native
	// did not claim a failure) and from a drift (nothing comparable was produced).
	unusable bool

	// unusableErr is why the re-encode failed, kept so the disposition is
	// diagnosable: the counter says a native result was unusable, this says whether
	// it was undecodable or simply not a JSON object. Set iff unusable.
	unusableErr error
}

// beginNativeDirectParse runs the native leg for an eligible dynamic final parse,
// or returns nil when the request is not eligible — in which case Parse behaves
// exactly as it did before this bridge existed.
//
// Eligibility is the whole gate, and every clause is load-bearing:
//
//   - the umbrella flag must be on (a flag-off build does zero native parse work);
//   - a native parser must be injected (the BAML-only worker injects none);
//   - the method must be the DYNAMIC one — static parse methods have no dynamic
//     output schema and the native parser cannot service them;
//   - Stream must be false — parse-STREAM keeps BAML's partial semantics, which
//     this final-mode bridge does not model;
//   - the request must carry the dynamic output schema, which is what the native
//     parser coerces against.
//
// The native call happens HERE, before BAML runs, so "native-first" is literal:
// BAML is the oracle the native answer is checked against, not the path native is
// bolted onto. It is a local CPU operation with no I/O, no provider contact and no
// mutation of the adapter, so running it first costs the request nothing but the
// parse itself.
func (h *Handler) beginNativeDirectParse(ctx context.Context, methodName string, input *workerParseInput) *nativeDirectParse {
	if !h.deBAML.Enabled || h.deBAMLParse == nil {
		return nil
	}
	if input.Stream || methodName != bamlutils.DynamicMethodName {
		return nil
	}
	if input.Options == nil || input.Options.OutputSchema == nil {
		return nil
	}

	n := &nativeDirectParse{h: h}
	// A panic inside the native parser is deliberately NOT contained. The generated
	// dynamic parse seam already drove this same parser over this same untrusted raw
	// input before this bridge existed, so containment here would change no exposure
	// — it would only convert a native bug into a silent BAML fallback, which is the
	// one failure mode the seam contract (bamlutils.DeBAMLParseFunc) refuses.
	res, err := h.deBAMLParse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          input.Raw,
		OutputSchema: input.Options.OutputSchema,
	})
	if err != nil {
		n.err = err
		return n
	}
	data, encErr := encodeNativeDynamicParseResult(res.JSON)
	if encErr != nil {
		n.unusable = true
		n.unusableErr = encErr
		return n
	}
	n.data = data
	return n
}

// settle decides the disposition once BAML's own payload is known, records it, and
// reports whether the native payload should be served.
//
// It returns (nativeBytes, true) ONLY when those bytes equal bamlData exactly. In
// every other case it returns (nil, false) and the caller serves BAML's payload —
// which is what the caller already holds, so a decline costs nothing and changes
// nothing.
func (n *nativeDirectParse) settle(bamlData []byte) ([]byte, bool) {
	switch {
	case n.err != nil:
		if errors.Is(n.err, bamlutils.ErrDeBAMLParseUnsupported) {
			n.record(directParseEngineBAML, directParseReasonNativeUnsupported)
			return nil, false
		}
		// Native CLAIMED a parse failure where BAML succeeded — an out-claim the
		// oracle just prevented. Logged, not just counted: a claimed failure against
		// a BAML success is the shape most worth a human noticing.
		n.record(directParseEngineBAML, directParseReasonNativeErrorBAMLOK)
		n.warn("de-BAML native direct parse claimed a failure BAML did not; serving BAML", n.err)
		return nil, false
	case n.unusable:
		// Native reported success and produced something this surface cannot
		// return. That is a parser bug, not a disagreement, and it is worth the
		// same bounded line the out-claim shapes get — otherwise the counter
		// records the disposition with no way to tell WHICH failure it was.
		n.record(directParseEngineBAML, directParseReasonNativeResultUnusable)
		n.warn("de-BAML native direct parse produced an unusable result; serving BAML", n.unusableErr)
		return nil, false
	case bytes.Equal(n.data, bamlData):
		n.record(directParseEngineNative, directParseReasonAgreement)
		return n.data, true
	default:
		n.record(directParseEngineBAML, directParseReasonResultDrift)
		return nil, false
	}
}

// settleBAMLResultUnusable records the disposition for a request BAML parsed but
// whose result could not be serialized. There is no BAML payload to hold native
// against, so native cannot win and the request fails as it always did; the
// disposition is recorded so every request that enters the bridge accounts for
// itself.
func (n *nativeDirectParse) settleBAMLResultUnusable() {
	n.record(directParseEngineBAML, directParseReasonBAMLResultUnusable)
}

// settleBAMLError records the disposition for a request BAML itself rejected. BAML's
// error is always what the caller gets: the bytes a client sees for a failed parse
// are BAML's message and classification, and native's message is not proven to
// match them, so native never wins an error.
func (n *nativeDirectParse) settleBAMLError() {
	// Same three-arm shape, in the same order, as settle: the native leg's own
	// failure modes are classified FIRST, and only a native leg that actually
	// produced a servable payload reaches the out-claim bucket.
	switch {
	case n.err != nil:
		if errors.Is(n.err, bamlutils.ErrDeBAMLParseUnsupported) {
			n.record(directParseEngineBAML, directParseReasonNativeUnsupported)
			return
		}
		n.record(directParseEngineBAML, directParseReasonBothError)
	case n.unusable:
		// Native reported success but produced nothing this surface could return,
		// so there was no out-claim to prevent — only a parser bug, which the
		// unusable bucket names. Classifying it as native_ok_baml_error would put a
		// non-out-claim in the bucket that exists to count out-claims, and warn
		// that native "claimed a result" it could never have served.
		n.record(directParseEngineBAML, directParseReasonNativeResultUnusable)
		n.warn("de-BAML native direct parse produced an unusable result; serving BAML's error", n.unusableErr)
	default:
		// Native produced a servable RESULT for input BAML rejected. This is the
		// most dangerous out-claim shape — data where stock BAML errors — so it is
		// both counted and logged.
		n.record(directParseEngineBAML, directParseReasonNativeOKBAMLError)
		n.warn("de-BAML native direct parse claimed a result BAML rejected; serving BAML's error", nil)
	}
}

// record forwards one disposition to the handler's counter.
func (n *nativeDirectParse) record(engine, reason string) {
	n.h.directParseMetrics.record(engine, reason)
}

// warn emits a bounded, secret-free line for the two prevented-out-claim shapes.
// It names neither the raw input nor the parsed output — only which side claimed
// what, plus the native error text when there is one (native error strings are
// schema/shape descriptions, not caller data). Best-effort: no logger, no line.
func (n *nativeDirectParse) warn(msg string, err error) {
	if n.h.logger == nil {
		return
	}
	if err != nil {
		n.h.logger.Warn(msg, "surface", directParseSurface, "err", err.Error())
		return
	}
	n.h.logger.Warn(msg, "surface", directParseSurface)
}

// dynamicOutputEnvelopePrefix opens the generated dynamic-output envelope
// (`Baml_Rest_DynamicOutput`, whose sole field is DynamicProperties). BAML's leg
// reaches the worker boundary as this envelope marshalled by sonic; the native leg
// is wrapped in the identical shape so the two are compared — and served — as the
// same kind of payload. The worker module cannot import the generated types (they
// live in the built adapter), so the envelope is assembled from bytes; the byte
// comparison against BAML's own payload is what proves the shape agrees.
const dynamicOutputEnvelopePrefix = `{"DynamicProperties":`

// encodeNativeDynamicParseResult re-encodes the native parser's flattened output
// and wraps it in the dynamic-output envelope, producing the exact bytes native
// would return from the worker.
//
// The re-encode is the NORMALIZATION half of the oracle. The native parser and
// BAML's Go value take different routes to JSON, so comparing their raw texts would
// reject on formatting incidentals rather than on meaning. Decoding through
// bamlutils.DecodeOrderedAny and re-marshalling with sonic puts native's output in
// the same encoding space as the BAML envelope while preserving the two things that
// ARE semantic here: object key ORDER (DecodeOrderedAny yields order-preserving
// carriers, so a map's observable key order survives) and NUMBER SPELLING
// (DecodeOrderedAny keeps the exact numeric token, so 1 and 1.0 stay distinct).
//
// A native result that is not a JSON object cannot be a dynamic output and is
// rejected here rather than wrapped into a malformed envelope.
func encodeNativeDynamicParseResult(flat []byte) ([]byte, error) {
	decoded, err := bamlutils.DecodeOrderedAny(flat)
	if err != nil {
		return nil, fmt.Errorf("decode native de-BAML parse result: %w", err)
	}
	if _, ok := decoded.(bamlutils.OrderedMap[any]); !ok {
		return nil, fmt.Errorf("native de-BAML parse result is not a JSON object")
	}
	body, err := sonic.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("re-encode native de-BAML parse result: %w", err)
	}
	out := make([]byte, 0, len(dynamicOutputEnvelopePrefix)+len(body)+1)
	out = append(out, dynamicOutputEnvelopePrefix...)
	out = append(out, body...)
	return append(out, '}'), nil
}
