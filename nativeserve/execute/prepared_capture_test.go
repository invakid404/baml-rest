//go:build nanollm_integration

package execute

// De-BAML Slice 6b PreparedRequest-adapter proofs against the baml-rest-owned
// loopback CAPTURE server (prepared_attempt_test.go). These are the primary
// wire-fidelity + response-side differential:
//
//	nanollm.PreparedRequest (fresh, Phase-5-admitted)
//	  -> llmhttp ExactAttemptRequest (ordered headers, raw bytes)
//	  -> ONE exact RoundTrip vs the loopback capture server
//	  -> ExactAttemptResponse
//	  -> nanollm.TranslateResponse(prep.Meta.ModelAlias, status, rawBody)
//	  -> buildrequest.ExtractResponseContentBytes("openai", ...)  [2xx only]
//	  -> internal/debaml.Parse (native SAP)                       [2xx only]
//
// There is NO Do/DoStream, NO fallback, and NO same-model retry: exactly one
// transport attempt per call, pinned by the capture server's request counter.
// Every credential is a literal fake; the exact transport dials loopback only.
//
// Tests are SERIAL (no t.Parallel): each starts its own capture server.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// TestPreparedExactPlanSentAndStructured is the core 6b proof: the byte-exact
// PreparedRequest hits the wire ONCE, the attempted alias drives translation, and
// a clean OpenAI 2xx reaches native structured output.
func TestPreparedExactPlanSentAndStructured(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	// Deterministic OpenAI success whose assistant content is JSON the parser
	// cleanly claims against personSchema6b.
	cs.setResponse(200, openAISuccessBody(`{"name":"Ada","age":36}`))

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}

	// (1) exactly one request, byte-identical to the plan.
	if got := cs.count(); got != 1 {
		t.Fatalf("capture count = %d, want exactly 1 (no retry/fallback)", got)
	}
	rec := cs.only()
	assertExactPlanSent(t, prep, rec)

	// The fake Bearer auth is on the wire verbatim (asserted directly, redacted in
	// any diagnostic).
	if got := rec.Header.Get("Authorization"); got != "Bearer "+p6bAPIKey {
		t.Errorf("wire Authorization mismatch (redacted): got <redacted> want <redacted> (present=%v)", got != "")
	}

	// (2) attempted alias retained + used for translation (distinct from target).
	if res.AttemptedAlias != p6bAlias {
		t.Errorf("attempted alias = %q, want %q", res.AttemptedAlias, p6bAlias)
	}

	// (3) structured output reached; SAP invoked exactly once.
	if res.Outcome != OutcomeStructured {
		t.Fatalf("outcome = %s, want structured", res.Outcome)
	}
	if !res.SAPInvoked {
		t.Errorf("SAPInvoked = false, want true on a 2xx claim")
	}
	if spy.calls != 1 {
		t.Errorf("parser calls = %d, want 1", spy.calls)
	}
	if res.AssistantText != `{"name":"Ada","age":36}` {
		t.Errorf("assistant text mismatch (%s), want the structured JSON", bodyDigest([]byte(res.AssistantText)))
	}
	if !jsonSemEqual(t, res.Structured, []byte(`{"name":"Ada","age":36}`)) {
		t.Errorf("structured output %s != want {name,age}", bodyDigest(res.Structured))
	}

	// (4) usage retained from the translated response.
	if res.Usage == nil {
		t.Fatalf("Usage is nil, want retained")
	}
	if res.Usage.PromptTokens != 5 || res.Usage.CompletionTokens != 3 {
		t.Errorf("usage prompt=%d completion=%d, want prompt=5 completion=3", res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
	// The raw upstream 2xx status is preserved alongside the translation.
	if res.ProviderStatus != 200 {
		t.Errorf("provider status = %d, want 200", res.ProviderStatus)
	}
}

// TestPreparedOrdinaryNon2xxSkipsSAP: an ordinary provider non-2xx becomes the
// uniform envelope provider outcome and NEVER reaches SAP; exactly one request.
func TestPreparedOrdinaryNon2xxSkipsSAP(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	cs.setResponse(429, openAIErrorBody("slow down", "rate_limit_error"))

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err != nil {
		t.Fatalf("RunAttempt returned an error, want a provider outcome: %v", err)
	}

	if res.Outcome != OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider-error", res.Outcome)
	}
	if res.SAPInvoked || spy.calls != 0 {
		t.Errorf("SAP was invoked on a non-2xx (SAPInvoked=%v calls=%d); it must be skipped", res.SAPInvoked, spy.calls)
	}
	if res.Structured != nil {
		t.Errorf("structured output present on a non-2xx (%s), want none", bodyDigest(res.Structured))
	}
	// The translated envelope carries the real status + is JSON.
	if res.Translated == nil || res.Translated.Status != 429 || !res.Translated.BodyIsJSON {
		t.Fatalf("translated %s, want status 429 + BodyIsJSON", translatedSummary(res.Translated))
	}
	if res.ProviderStatus != 429 {
		t.Errorf("provider status = %d, want 429", res.ProviderStatus)
	}
	if got := cs.count(); got != 1 {
		t.Errorf("capture count = %d, want exactly 1", got)
	}
}

// TestPreparedInvalidBodySkipsSAP: a provider 2xx with an unparseable body
// surfaces nanollm's typed *InvalidBodyError, mapped to the 502/uniform-envelope
// provider outcome — never a network error, never fed to SAP.
func TestPreparedInvalidBodySkipsSAP(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	// A 2xx whose body is not JSON: TranslateResponse -> *InvalidBodyError(502).
	cs.setResponse(200, []byte("<html>not json</html>"))

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err != nil {
		t.Fatalf("RunAttempt returned an error, want the 502 provider outcome: %v", err)
	}

	if res.Outcome != OutcomeInvalidBody {
		t.Fatalf("outcome = %s, want invalid-body", res.Outcome)
	}
	if res.SAPInvoked || spy.calls != 0 {
		t.Errorf("SAP was invoked on an invalid-2xx (SAPInvoked=%v calls=%d); it must be skipped", res.SAPInvoked, spy.calls)
	}
	if res.Translated == nil || res.Translated.Status != 502 || !res.Translated.BodyIsJSON {
		t.Fatalf("translated %s, want the 502 envelope + BodyIsJSON", translatedSummary(res.Translated))
	}
	if !strings.Contains(string(res.Translated.Body), "invalid_provider_body") {
		t.Errorf("envelope (%s) does not carry invalid_provider_body", bodyDigest(res.Translated.Body))
	}
	// The RAW upstream status (200) is preserved distinct from the caller-facing
	// 502 the envelope reports.
	if res.ProviderStatus != 200 {
		t.Errorf("provider status = %d, want the raw upstream 200", res.ProviderStatus)
	}
	if got := cs.count(); got != 1 {
		t.Errorf("capture count = %d, want exactly 1", got)
	}
}

// TestPreparedExpiryRejectedPreAttempt: an expired plan is rejected BEFORE any
// socket — no request reaches the server and SAP is never invoked.
func TestPreparedExpiryRejectedPreAttempt(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())

	// A synthetic, otherwise-supported plan whose signature window has passed.
	// OpenAI plans are unsigned, so expiry is fabricated to exercise the guard.
	past := time.Now().Add(-time.Hour)
	prep := &nanollm.PreparedRequest{
		Method:         "POST",
		URL:            cs.base() + "/v1/chat/completions",
		Headers:        [][2]string{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + p6bAPIKey}},
		Body:           []byte(`{"model":"` + p6bTarget + `","messages":[]}`),
		ResponseFormat: nanollm.FormatJSON,
		Meta: nanollm.PreparedMeta{
			ModelAlias:  p6bAlias,
			TargetModel: p6bTarget,
			Provider:    "openai",
			RequestType: nanollm.ChatCompletion,
			ExpiresAt:   &past,
		},
	}

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("err = %v, want ErrPlanExpired", err)
	}
	if res != nil {
		t.Errorf("result = %s, want nil on a pre-attempt rejection", resultSummary(res))
	}
	if got := cs.count(); got != 0 {
		t.Errorf("capture count = %d, want 0 (no socket opened)", got)
	}
	if spy.calls != 0 {
		t.Errorf("parser calls = %d, want 0", spy.calls)
	}
}

// TestPreparedUnsupportedDeclinedPreAttempt: a plan outside the admitted unary
// chat surface declines with the parity-decline sentinel + a stable stage/reason,
// before any socket. The provider requirement was REMOVED from supportDecision
// (de-BAML mprov S1 §7 — admission's mapper/verification policy already decides
// provider support, and TranslateResponse normalizes every provider to OpenAI
// JSON), so this exercises a still-enforced provider-NEUTRAL shape gate: a
// streaming plan, which the one-shot unary transport never sends.
func TestPreparedUnsupportedDeclinedPreAttempt(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	// A non-openai provider (e.g. "anthropic") is NO LONGER a pre-attempt decline
	// — the provider gate is gone. Mutate a provider-neutral unsupported shape
	// instead: a streaming plan. The support decision must decline before a socket.
	prep.Meta.Provider = "anthropic"
	prep.Meta.Stream = true

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if !errors.Is(err, ErrAttemptUnsupported) {
		t.Fatalf("err = %v, want ErrAttemptUnsupported", err)
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) || ue.Stage != "stream" {
		t.Fatalf("err = %v, want an UnsupportedError with stage=stream", err)
	}
	if res != nil {
		t.Errorf("result = %s, want nil on a pre-attempt decline", resultSummary(res))
	}
	if got := cs.count(); got != 0 {
		t.Errorf("capture count = %d, want 0 (no socket opened)", got)
	}
	if spy.calls != 0 {
		t.Errorf("parser calls = %d, want 0", spy.calls)
	}
}

// TestPreparedSuccessBodyCapExceeded: a 2xx body over the 16 MiB success cap
// fails the attempt (the exact executor rejects it) without reaching SAP.
func TestPreparedSuccessBodyCapExceeded(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	// One byte over the 16 MiB success cap. The executor reads limit+1 and errors.
	oversized := bytes.Repeat([]byte("a"), llmhttp.MaxResponseBodyBytes+1)
	cs.setResponse(200, oversized)

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err == nil {
		t.Fatalf("RunAttempt succeeded, want a success-cap error; res=%s", resultSummary(res))
	}
	if !strings.Contains(err.Error(), "maximum size") {
		t.Errorf("err = %v, want a body-size-cap error", err)
	}
	if res != nil {
		t.Errorf("result = %s, want nil on a transport failure", resultSummary(res))
	}
	if spy.calls != 0 {
		t.Errorf("parser calls = %d, want 0 (SAP never reached)", spy.calls)
	}
	if got := cs.count(); got != 1 {
		t.Errorf("capture count = %d, want exactly 1", got)
	}
}

// TestPreparedErrorBodyCapTruncated: a non-2xx body over the 64 KiB error cap is
// truncated to exactly the cap and returned as data (still a provider outcome,
// still no SAP).
func TestPreparedErrorBodyCapTruncated(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

	oversized := bytes.Repeat([]byte("a"), llmhttp.MaxExactErrorBodyBytes+128)
	cs.setResponse(503, oversized)

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err != nil {
		t.Fatalf("RunAttempt returned an error, want a truncated provider outcome: %v", err)
	}
	if res.Outcome != OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider-error", res.Outcome)
	}
	if len(res.ProviderBody) != llmhttp.MaxExactErrorBodyBytes {
		t.Errorf("provider body = %d bytes, want exactly the 64 KiB cap (%d)", len(res.ProviderBody), llmhttp.MaxExactErrorBodyBytes)
	}
	if res.ProviderStatus != 503 {
		t.Errorf("provider status = %d, want 503", res.ProviderStatus)
	}
	if res.SAPInvoked || spy.calls != 0 {
		t.Errorf("SAP was invoked on a non-2xx (SAPInvoked=%v calls=%d)", res.SAPInvoked, spy.calls)
	}
	if got := cs.count(); got != 1 {
		t.Errorf("capture count = %d, want exactly 1", got)
	}
}

// TestPreparedCancellationNoSend: a context cancelled before the attempt yields a
// context error, no request, and no SAP.
func TestPreparedCancellationNoSend(t *testing.T) {
	cs := newCaptureServer(t)
	nano := newPreparedClient(t, cs.base())
	prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))
	cs.setResponse(200, openAISuccessBody(`{"name":"Ada","age":36}`))

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the attempt

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Errorf("result = %s, want nil on cancellation", resultSummary(res))
	}
	if got := cs.count(); got != 0 {
		t.Errorf("capture count = %d, want 0 (cancelled before send)", got)
	}
	if spy.calls != 0 {
		t.Errorf("parser calls = %d, want 0", spy.calls)
	}
}

// TestPreparedDeclineVsClaimedParseError asserts the two 2xx parse dispositions
// SEPARATELY so a fallback can never masquerade as a native success: a DECLINE
// (ErrDeBAMLParseUnsupported) is a nil-error parity outcome; a CLAIMED parse
// error propagates as a non-sentinel Go error. Both invoke SAP exactly once.
func TestPreparedDeclineVsClaimedParseError(t *testing.T) {
	t.Run("decline", func(t *testing.T) {
		cs := newCaptureServer(t)
		nano := newPreparedClient(t, cs.base())
		prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

		// Assistant text with no cleanly-claimable JSON candidate -> DECLINE.
		cs.setResponse(200, openAISuccessBody("there is no json in this reply at all"))

		spy := &parseSpy{fn: debaml.Parse}
		ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
		defer cancel()

		res, err := RunAttempt(ctx, AttemptConfig{
			Client:       nano,
			Prepared:     prep,
			Executor:     exactLoopbackExecutor(),
			Parse:        spy.Parse,
			OutputSchema: personSchema6b(),
		})
		if err != nil {
			t.Fatalf("RunAttempt returned an error, want a decline outcome: %v", err)
		}
		if res.Outcome != OutcomeParseDeclined {
			t.Fatalf("outcome = %s, want parse-declined", res.Outcome)
		}
		if !res.SAPInvoked || spy.calls != 1 {
			t.Errorf("SAP invocation: SAPInvoked=%v calls=%d, want invoked once", res.SAPInvoked, spy.calls)
		}
		if res.Structured != nil {
			t.Errorf("structured output present on a decline (%s), want none", bodyDigest(res.Structured))
		}
		if res.DeclineReason == "" {
			t.Errorf("DeclineReason empty, want the parser's decline explanation")
		}
	})

	t.Run("claimed_error", func(t *testing.T) {
		cs := newCaptureServer(t)
		nano := newPreparedClient(t, cs.base())
		prep := preparePlan(t, nano, cs.base(), p6bNativeBody(t))

		// A value that substring-ties both enum values -> CLAIMED parse error.
		cs.setResponse(200, openAISuccessBody(`{"animal":"cat dog"}`))

		spy := &parseSpy{fn: debaml.Parse}
		ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
		defer cancel()

		res, err := RunAttempt(ctx, AttemptConfig{
			Client:       nano,
			Prepared:     prep,
			Executor:     exactLoopbackExecutor(),
			Parse:        spy.Parse,
			OutputSchema: enumTieSchema6b(),
		})
		if err == nil {
			t.Fatalf("RunAttempt succeeded, want a claimed parse error; res=%s", resultSummary(res))
		}
		if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("err is the decline sentinel, want a CLAIMED parse error: %v", err)
		}
		// Post-response error contract (de-BAML cutover Slice 6): a CLAIMED parse
		// error propagates alongside the RETAINED response context (non-nil result)
		// so the serving mapper can attach the extracted assistant text as
		// details.raw and classify the failure without parsing error strings.
		if res == nil {
			t.Fatalf("result = nil, want the retained response context on a claimed parse error")
		}
		if !res.SAPInvoked {
			t.Errorf("SAPInvoked = false, want true (SAP ran, then claimed)")
		}
		if res.ProviderStatus != 200 {
			t.Errorf("ProviderStatus = %d, want 200 (response context retained)", res.ProviderStatus)
		}
		if len(res.ProviderBody) == 0 {
			t.Errorf("ProviderBody empty, want the raw provider body retained")
		}
		// res.Raw is the extracted assistant text — the value the serving mapper
		// attaches as details.raw on a claimed parse_error. It must be the assistant
		// JSON from the 2xx body, not empty.
		if res.Raw != `{"animal":"cat dog"}` {
			t.Errorf("res.Raw = %q, want the extracted assistant text %q (details.raw source)", res.Raw, `{"animal":"cat dog"}`)
		}
		if spy.calls != 1 {
			t.Errorf("parser calls = %d, want 1 (SAP invoked, then claimed)", spy.calls)
		}
	})
}
