//go:build nanollm_integration

package admission

// De-BAML Phase 7B stream admission proofs. Gated by nanollm_integration (the
// core links nanollm.New/Prepare/Close). It proves, entirely WITHOUT a socket:
//   - a fully-formed streaming _dynamic call is admitted up to — but NOT
//     including — the exact RoundTrip: Prepare yields a plan whose meta carries
//     stream=true + an SSE response format, whose body byte-equals the canonical
//     STREAM body, and whose exact request is ready to send but unsent;
//   - the retained StreamClaim carries the live engine plus the UNARY-body
//     nanollm.Request the executor hands DoStream;
//   - the stream mode gate declines every unary mode;
//   - validateStreamPlanMeta inverts the unary stream/format gates.
// It reuses the admit_test.go harness (validInput/validRegistry/countingTransport).

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/prometheus/client_golang/prometheus"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// streamInput is the fully-formed admitted streaming _dynamic call: validInput
// with the streaming mode and an admitted-for-stream output schema. The Phase 7C
// native-stream SAP schema gate declines validInput's single-string schema
// (allow_as_string diverges), so the stream fixture uses a >=2-field class with a
// last unquoted-scalar field — the admitted §11 matrix shape.
func streamInput() Input {
	in := validInput()
	in.Mode = ModeStream
	in.OutputSchema = admittedStreamSchema()
	return in
}

// admittedStreamSchema is Root{answer:string, count:int} — a >=2-field required-
// field class whose only scalar field is LAST, no unions/optionals, no non-last
// scalar, no scalar map value: the shape SupportsNativeStream admits.
func admittedStreamSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		bamlutils.OrderedKV("count", &bamlutils.DynamicProperty{Type: "int"}),
	)}
}

// canonicalStreamBody rebuilds the zero-nanollm canonical STREAM body for the
// fence fixture, so a test can byte-compare it against the admitted plan.
func canonicalStreamBody(t *testing.T) []byte {
	t.Helper()
	in := streamInput()
	rendered, err := nativeprompt.Render(toNativeMessages(in.Messages), in.OutputSchema)
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	body, err := nativebody.BuildOpenAIChatStream(rendered, nativebody.ClientIntent{
		Provider:    nativebody.ProviderOpenAI,
		TargetModel: fenceModel,
		ModelAlias:  fenceAlias,
		Stream:      true,
	})
	if err != nil {
		t.Fatalf("BuildOpenAIChatStream: %v", err)
	}
	return body.Bytes()
}

// TestAdmitStreamClaimPositive proves a fully-formed streaming call is admitted
// up to — but not including — the RoundTrip, with a streaming plan.
func TestAdmitStreamClaimPositive(t *testing.T) {
	ct := &countingTransport{}
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	a := NewAdmitter(m, llmhttp.NewExactExecutor(ct))

	claim, err := a.AdmitStreamClaim(context.Background(), streamInput())
	if err != nil {
		t.Fatalf("AdmitStreamClaim declined a fully-formed streaming call: %v", err)
	}
	defer claim.Close()
	assertNoSocket(t, ct)

	if claim.Prepared == nil {
		t.Fatal("admitted stream plan is nil")
	}
	if claim.ExactRequest == nil {
		t.Fatal("admitted exact stream request is nil (should be ready to send)")
	}
	if claim.Client() == nil {
		t.Fatal("stream claim engine is nil; it must stay alive for DoStream")
	}
	// Plan meta: an SSE stream, not a unary JSON call.
	if !claim.Prepared.Meta.Stream {
		t.Error("prepared stream plan meta Stream = false, want true")
	}
	if claim.Prepared.ResponseFormat != nanollm.FormatSSE {
		t.Errorf("prepared stream response format = %q, want sse", claim.Prepared.ResponseFormat)
	}
	if claim.Prepared.Meta.MaxRetries != 0 {
		t.Errorf("prepared stream max retries = %d, want 0", claim.Prepared.Meta.MaxRetries)
	}

	// The exact plan the 7A client compares against is the canonical STREAM body
	// (model+messages+stream suffix), byte-for-byte.
	wantBody := canonicalStreamBody(t)
	if !bytes.Equal(claim.ExactRequest.Body, wantBody) {
		// Body is sensitive (Admitted/Claim contract) — report digests, not the bytes.
		t.Errorf("exact stream request body != canonical stream body (got %s, want %s)", bodyDigest(claim.ExactRequest.Body), bodyDigest(wantBody))
	}
	if !bytes.Equal(claim.Prepared.Body, wantBody) {
		t.Errorf("prepared stream plan body != canonical stream body (validatePreparedBody should have caught this)")
	}

	// The Request handed to DoStream carries the UNARY body (no suffix): DoStream
	// re-prepares with Stream=true and the engine injects the suffix.
	req := claim.Request()
	if bytes.Contains(req.Body, []byte(`"stream_options"`)) {
		// Body is sensitive — report only that the forbidden suffix was present.
		t.Errorf("DoStream Request body must be the UNARY body (engine injects the suffix) but carried the stream_options suffix (%s)", bodyDigest(req.Body))
	}
	if req.Model != fenceAlias {
		t.Errorf("DoStream Request model = %q, want the internal alias %q", req.Model, fenceAlias)
	}
}

// TestAdmitStreamClaimDeclinesUnaryMode proves the stream claim declines a unary
// mode before any socket, with the bounded not_stream_mode reason.
func TestAdmitStreamClaimDeclinesUnaryMode(t *testing.T) {
	ct := &countingTransport{}
	a := NewAdmitter(nil, llmhttp.NewExactExecutor(ct))

	in := streamInput()
	in.Mode = ModeCall
	claim, err := a.AdmitStreamClaim(context.Background(), in)
	if claim != nil {
		claim.Close()
		t.Fatal("stream claim admitted a unary mode")
	}
	assertNoSocket(t, ct)
	var d *Decline
	if !errors.As(err, &d) {
		t.Fatalf("want *Decline, got %v", err)
	}
	if d.Stage != StageMode || d.Reason != ReasonNotStreamMode {
		t.Errorf("decline = (%s, %s), want (mode, not_stream_mode)", d.Stage, d.Reason)
	}
}

// TestValidateStreamPlanMeta unit-checks the stream plan-meta inversions without
// a socket: stream=true + SSE admits; stream=false and a JSON format each decline
// with their bounded reason.
func TestValidateStreamPlanMeta(t *testing.T) {
	base := &nanollm.PreparedRequest{
		ResponseFormat: nanollm.FormatSSE,
		Meta: nanollm.PreparedMeta{
			ModelAlias:  fenceAlias,
			TargetModel: fenceModel,
			Provider:    nativebody.ProviderOpenAI,
			RequestType: nanollm.ChatCompletion,
			Stream:      true,
			MaxRetries:  0,
		},
	}
	if d := validateStreamPlanMeta(base, fenceAlias, fenceModel); d != nil {
		t.Fatalf("valid stream plan meta declined: %v", d)
	}

	notStream := *base
	notStream.Meta.Stream = false
	if d := validateStreamPlanMeta(&notStream, fenceAlias, fenceModel); d == nil || d.Reason != ReasonPlanStreamFalse {
		t.Errorf("stream=false decline = %v, want plan_stream_false", d)
	}

	notSSE := *base
	notSSE.ResponseFormat = nanollm.FormatJSON
	if d := validateStreamPlanMeta(&notSSE, fenceAlias, fenceModel); d == nil || d.Reason != ReasonResponseFormatNotSSE {
		t.Errorf("json-format decline = %v, want response_format_not_sse", d)
	}
}
