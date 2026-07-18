//go:build nanollm_integration

package execute

// De-BAML cutover Slice 5 error-envelope + raw/reasoning golden suite for the
// FACTORED same-response consumer (ConsumeResponse). Unlike RunAttempt, this
// consumer owns NO transport: it takes an ALREADY-COMPLETED (status, body) and
// runs TranslateResponse -> assistant/raw/reasoning extraction -> native SAP. The
// tests drive it with synthetic status+body pairs — no executor, no socket, no
// second provider request — and pin the exact public-compatibility mappings the
// de-BAML shadow oracle (and a later canary) rely on:
//
//   - a clean 2xx -> OutcomeStructured, with the /call-with-raw raw + reasoning
//     channels carried alongside the structured output;
//   - an ordinary non-2xx -> OutcomeProviderError, SAP NEVER invoked;
//   - an invalid-2xx -> OutcomeInvalidBody (nanollm's synthesized 502/uniform
//     envelope), SAP NEVER invoked;
//   - a SAP decline (ErrDeBAMLParseUnsupported) -> OutcomeParseDeclined (nil err);
//   - a CLAIMED SAP error -> propagated Go error (never a masked success).

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// TestConsumeResponseCleanStructured pins the happy path: a 2xx openai body
// translates, extracts, and SAP-claims into structured output, with raw carrying
// the assistant text. No executor is supplied — the consumer never transports.
func TestConsumeResponseCleanStructured(t *testing.T) {
	nano := newPreparedClient(t, "http://127.0.0.1:1")
	spy := &parseSpy{fn: debaml.Parse}

	res, err := ConsumeResponse(context.Background(), ConsumeConfig{
		Client:       nano,
		Alias:        p6bAlias,
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	}, 200, openAISuccessBody(`{"name":"Ada","age":36}`))
	if err != nil {
		t.Fatalf("ConsumeResponse: %v", err)
	}
	if res.Outcome != OutcomeStructured {
		t.Fatalf("outcome = %s, want structured", res.Outcome)
	}
	if res.AttemptedAlias != p6bAlias {
		t.Errorf("attempted alias = %q, want %q", res.AttemptedAlias, p6bAlias)
	}
	if !res.SAPInvoked || spy.calls != 1 {
		t.Errorf("SAP invocation: SAPInvoked=%v calls=%d, want invoked once", res.SAPInvoked, spy.calls)
	}
	if !jsonSemEqual(t, res.Structured, []byte(`{"name":"Ada","age":36}`)) {
		t.Errorf("structured = %s, want {\"name\":\"Ada\",\"age\":36}", bodyDigest(res.Structured))
	}
	// raw carries the text-only assistant channel — the /call-with-raw Raw().
	if res.Raw != `{"name":"Ada","age":36}` {
		t.Errorf("raw = %q, want the assistant text", strDigest(res.Raw))
	}
	if res.AssistantText != res.Raw {
		t.Errorf("assistant text %q and raw %q must match on the text-only channel", strDigest(res.AssistantText), strDigest(res.Raw))
	}
}

// TestConsumeResponseNon2xxProviderError pins that an ordinary non-2xx is a
// provider outcome that NEVER reaches SAP, with no raw/reasoning surfaced.
func TestConsumeResponseNon2xxProviderError(t *testing.T) {
	nano := newPreparedClient(t, "http://127.0.0.1:1")
	spy := &parseSpy{fn: debaml.Parse}

	res, err := ConsumeResponse(context.Background(), ConsumeConfig{
		Client:       nano,
		Alias:        p6bAlias,
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	}, 429, openAIErrorBody("slow down", "rate_limit_error"))
	if err != nil {
		t.Fatalf("ConsumeResponse returned an error, want the provider outcome: %v", err)
	}
	if res.Outcome != OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider-error", res.Outcome)
	}
	if res.SAPInvoked || spy.calls != 0 {
		t.Errorf("SAP invoked on a non-2xx (SAPInvoked=%v calls=%d); it must be skipped", res.SAPInvoked, spy.calls)
	}
	if res.Raw != "" || res.Reasoning != "" {
		t.Errorf("raw/reasoning surfaced on a provider error (raw=%q reasoning=%q), want empty", strDigest(res.Raw), strDigest(res.Reasoning))
	}
}

// TestConsumeResponseInvalidBody502 pins that a 2xx whose body is not JSON maps
// to nanollm's synthesized 502/uniform-envelope provider outcome, and SAP is
// never fed the malformed body.
func TestConsumeResponseInvalidBody502(t *testing.T) {
	nano := newPreparedClient(t, "http://127.0.0.1:1")
	spy := &parseSpy{fn: debaml.Parse}

	res, err := ConsumeResponse(context.Background(), ConsumeConfig{
		Client:       nano,
		Alias:        p6bAlias,
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	}, 200, []byte("this is not json at all"))
	if err != nil {
		t.Fatalf("ConsumeResponse returned an error, want the 502 provider outcome: %v", err)
	}
	if res.Outcome != OutcomeInvalidBody {
		t.Fatalf("outcome = %s, want invalid-body", res.Outcome)
	}
	if res.SAPInvoked || spy.calls != 0 {
		t.Errorf("SAP invoked on an invalid-2xx (SAPInvoked=%v calls=%d); it must be skipped", res.SAPInvoked, spy.calls)
	}
	if res.Translated == nil || res.Translated.Status != 502 || !res.Translated.BodyIsJSON {
		t.Fatalf("translated %s, want the 502 envelope + BodyIsJSON", translatedSummary(res.Translated))
	}
}

// TestConsumeResponseParseDeclineVsClaimedError pins the two 2xx parse
// dispositions SEPARATELY: a DECLINE (ErrDeBAMLParseUnsupported) is a nil-error
// parity outcome; a CLAIMED parse error propagates unchanged so a fallback can
// never masquerade as a native success.
func TestConsumeResponseParseDeclineVsClaimedError(t *testing.T) {
	body := openAISuccessBody(`{"name":"Ada","age":36}`)

	t.Run("decline", func(t *testing.T) {
		nano := newPreparedClient(t, "http://127.0.0.1:1")
		res, err := ConsumeResponse(context.Background(), ConsumeConfig{
			Client: nano,
			Alias:  p6bAlias,
			Parse: func(context.Context, bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
				return bamlutils.DeBAMLParseResult{}, bamlutils.ErrDeBAMLParseUnsupported
			},
			OutputSchema: personSchema6b(),
		}, 200, body)
		if err != nil {
			t.Fatalf("a decline must be a nil-error outcome, got %v", err)
		}
		if res.Outcome != OutcomeParseDeclined {
			t.Fatalf("outcome = %s, want parse-declined", res.Outcome)
		}
		if res.AssistantText == "" {
			t.Error("assistant text must be retained on a decline")
		}
	})

	t.Run("claimed error", func(t *testing.T) {
		nano := newPreparedClient(t, "http://127.0.0.1:1")
		claimed := errors.New("claimed native parse failure")
		_, err := ConsumeResponse(context.Background(), ConsumeConfig{
			Client: nano,
			Alias:  p6bAlias,
			Parse: func(context.Context, bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
				return bamlutils.DeBAMLParseResult{}, claimed
			},
			OutputSchema: personSchema6b(),
		}, 200, body)
		if !errors.Is(err, claimed) {
			t.Fatalf("a claimed parse error must propagate unchanged, got %v", err)
		}
	})
}

// TestConsumeResponseReasoningChannel pins the /call-with-raw reasoning channel:
// with reasoning ON it is extracted from the translated body; with reasoning OFF
// it stays empty while raw (text-only) is unaffected.
func TestConsumeResponseReasoningChannel(t *testing.T) {
	// An openai chat body carrying reasoning_content alongside the content.
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"Ada\",\"age\":36}","reasoning_content":"weighing the options"}}]}`)

	t.Run("reasoning on", func(t *testing.T) {
		nano := newPreparedClient(t, "http://127.0.0.1:1")
		res, err := ConsumeResponse(context.Background(), ConsumeConfig{
			Client:           nano,
			Alias:            p6bAlias,
			Parse:            debaml.Parse,
			OutputSchema:     personSchema6b(),
			IncludeReasoning: true,
		}, 200, body)
		if err != nil {
			t.Fatalf("ConsumeResponse: %v", err)
		}
		if res.Reasoning != "weighing the options" {
			t.Errorf("reasoning = %q, want the reasoning_content text", strDigest(res.Reasoning))
		}
		if res.Raw != `{"name":"Ada","age":36}` {
			t.Errorf("raw = %q, want the text-only assistant content (never reasoning)", strDigest(res.Raw))
		}
	})

	t.Run("reasoning off", func(t *testing.T) {
		nano := newPreparedClient(t, "http://127.0.0.1:1")
		res, err := ConsumeResponse(context.Background(), ConsumeConfig{
			Client:           nano,
			Alias:            p6bAlias,
			Parse:            debaml.Parse,
			OutputSchema:     personSchema6b(),
			IncludeReasoning: false,
		}, 200, body)
		if err != nil {
			t.Fatalf("ConsumeResponse: %v", err)
		}
		if res.Reasoning != "" {
			t.Errorf("reasoning = %q, want empty when reasoning is off", strDigest(res.Reasoning))
		}
		if res.Raw != `{"name":"Ada","age":36}` {
			t.Errorf("raw = %q, want the text-only assistant content", strDigest(res.Raw))
		}
	})
}
