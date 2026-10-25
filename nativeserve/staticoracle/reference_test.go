package staticoracle

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

// openAIBody is an OpenAI chat completion carrying content and reasoning_content verbatim.
func openAIBody(content, reasoning string) []byte {
	return []byte(`{"choices":[{"message":{"role":"assistant","content":` + quote(content) + `,"reasoning_content":` + quote(reasoning) + `}}]}`)
}

func quote(s string) string {
	b := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"', '\\':
			b = append(b, '\\', byte(r))
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, string(r)...)
		}
	}
	return string(append(b, '"'))
}

// ref is the production reference (the real BAML-path extractor) over parse.
func ref(parse BAMLOnlyParse) Reference {
	return Reference{Provider: "openai", IncludeReasoning: true, Extract: buildrequest.ExtractResponseContentBytes, Parse: parse}
}

// nativeStructured is a clean native structured claim whose channels equal (content, reasoning)
// and whose canonical JSON is structured, over the given ORIGINAL body.
func nativeStructured(body []byte, content, reasoning, structured string) *execute.AttemptResult {
	return &execute.AttemptResult{
		Outcome: execute.OutcomeStructured, ProviderStatus: 200, ProviderBody: body,
		AssistantText: content, Raw: content, Reasoning: reasoning,
		Structured: []byte(structured), SAPInvoked: true,
	}
}

// TestResolveReference_FullMatchServesNative: every facet agrees -> native owns the envelope.
func TestResolveReference_FullMatchServesNative(t *testing.T) {
	const content, reasoning = `{"k":1}`, "why"
	res := nativeStructured(openAIBody(content, reasoning), content, reasoning, content)
	entered := false
	r := ResolveReference(context.Background(), nil, res, nil, ref(parse(content, nil)), func() { entered = true })
	if !entered {
		t.Error("onOracle did not fire")
	}
	if !r.Served || r.Winner != bamlutils.NativeStaticServeEngineNative || r.Fallback {
		t.Fatalf("result = %+v, want a native win", r)
	}
	if !r.ChannelsCompared || !r.AssistantMatch || !r.RawMatch || !r.ReasoningMatch || !r.StructuredMatch || !r.OrderMatch || !r.CanonicalBytesMatch {
		t.Errorf("facets = %+v, want all matched", r)
	}
	if string(r.FinalJSON) != content || r.Raw != content || r.Reasoning != reasoning {
		t.Errorf("served (%s, %q, %q)", r.FinalJSON, r.Raw, r.Reasoning)
	}
}

// TestResolveReference_ReferenceComesFromTheOriginalBody: native's channels claim one text
// while the ORIGINAL body carries another. BAML must parse the original body's text, and the
// served raw/reasoning must be the original body's — never native's channels.
func TestResolveReference_ReferenceComesFromTheOriginalBody(t *testing.T) {
	res := nativeStructured(openAIBody(`{"k":2}`, "original reasoning"), `{"k":1}`, "native reasoning", `{"k":1}`)
	var captured string
	r := ResolveReference(context.Background(), nil, res, nil, ref(parse(`{"k":2}`, &captured)), nil)
	if captured != `{"k":2}` {
		t.Fatalf("BAML parsed %q, want the ORIGINAL body's text (never res.AssistantText)", captured)
	}
	if r.Winner != bamlutils.NativeStaticServeEngineBAMLParse || r.AssistantMatch || r.RawMatch || r.ReasoningMatch {
		t.Fatalf("result = %+v, want a reference win with all three channel facets mismatched", r)
	}
	if string(r.FinalJSON) != `{"k":2}` || r.Raw != `{"k":2}` || r.Reasoning != "original reasoning" {
		t.Errorf("served (%s, %q, %q), want the whole ORIGINAL-body reference envelope", r.FinalJSON, r.Raw, r.Reasoning)
	}
}

// TestResolveReference_CanonicalBytesGate: a semantically equal but byte-different BAML parse
// is recorded as a canonical-byte mismatch and refuses native. (Without a Bundle the order
// facet itself falls back to byte equality; the live spine matrix isolates the canonical
// gate against a real Bundle, where order matches.)
func TestResolveReference_CanonicalBytesGate(t *testing.T) {
	const content = `{"k":1}`
	res := nativeStructured(openAIBody(content, ""), content, "", content)
	r := ResolveReference(context.Background(), nil, res, nil, ref(parse(`{"k": 1}`, nil)), nil)
	if !r.StructuredMatch || r.CanonicalBytesMatch {
		t.Fatalf("facets structured=%v bytes=%v, want semantic match + byte mismatch", r.StructuredMatch, r.CanonicalBytesMatch)
	}
	if r.Winner != bamlutils.NativeStaticServeEngineBAMLParse || string(r.FinalJSON) != `{"k": 1}` {
		t.Errorf("winner=%q final=%s, want BAML's exact bytes", r.Winner, r.FinalJSON)
	}
}

// TestResolveReference_NativePostResponseFailuresRecover: every native failure after a
// complete 2xx response — a claimed parse error (channels extracted), a translate failure
// (none extracted), a malformed-2xx classification, and a translated non-2xx over a 2xx
// original — serves the independently established reference envelope, and records the
// error facet as a MISMATCH. The claimed-error row gives native DIFFERENT channels, so
// serving any native channel on this branch fails the served-envelope check.
func TestResolveReference_NativePostResponseFailuresRecover(t *testing.T) {
	const content, reasoning = `{"k":1}`, "r"
	body := openAIBody(content, reasoning)
	cases := []struct {
		name         string
		res          *execute.AttemptResult
		aerr         error
		wantChannels bool
	}{
		{"claimed native parse error", &execute.AttemptResult{ProviderStatus: 200, ProviderBody: body, AssistantText: "native text", Raw: "native raw", Reasoning: "native reasoning", SAPInvoked: true}, errors.New("native sap: boom"), true},
		{"translate failure", &execute.AttemptResult{ProviderStatus: 200, ProviderBody: body}, errors.New("nativeserve: TranslateResponse: bad"), false},
		{"malformed 2xx classification", &execute.AttemptResult{Outcome: execute.OutcomeInvalidBody, ProviderStatus: 200, ProviderBody: body}, nil, false},
		{"translated non-2xx over a 2xx original", &execute.AttemptResult{Outcome: execute.OutcomeProviderError, ProviderStatus: 200, ProviderBody: body}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ResolveReference(context.Background(), nil, tc.res, tc.aerr, ref(parse(content, nil)), nil)
			if !r.Served || r.Winner != bamlutils.NativeStaticServeEngineBAMLParse || !r.NativeFailureRecovered || !r.Fallback {
				t.Fatalf("result = %+v (err %v), want the reference envelope served as a recovered native failure", r, r.Err)
			}
			if r.Outcome != OutcomeSuccess {
				t.Errorf("outcome = %v, want success", r.Outcome)
			}
			if r.ChannelsCompared != tc.wantChannels {
				t.Errorf("ChannelsCompared = %v, want %v", r.ChannelsCompared, tc.wantChannels)
			}
			if !r.ErrorCompareRecorded || r.ErrorCompareMatch {
				t.Errorf("error facet recorded=%v match=%v, want a recorded MISMATCH (the native leg errored)", r.ErrorCompareRecorded, r.ErrorCompareMatch)
			}
			if string(r.FinalJSON) != content || r.Raw != content || r.Reasoning != reasoning {
				t.Errorf("served (%s, %q, %q)", r.FinalJSON, r.Raw, r.Reasoning)
			}
		})
	}
}

// TestResolveReference_ParseDeclineServesReference: a native decline serves the reference
// and is classified parse_decline. Native's channels deliberately DIFFER from the
// reference's, so serving any native channel on the decline branch fails; a decline is
// not a pipeline error, so the error facet stays a match.
func TestResolveReference_ParseDeclineServesReference(t *testing.T) {
	body := openAIBody("hello", "ref reasoning")
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, ProviderStatus: 200, ProviderBody: body,
		AssistantText: "native hello", Raw: "native raw", Reasoning: "native reasoning", SAPInvoked: true}
	r := ResolveReference(context.Background(), nil, res, nil, ref(parse(`"hello"`, nil)), nil)
	if !r.Served || !r.ParseDeclineServed || r.Outcome != OutcomeParseDecline || r.Winner != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Fatalf("result = %+v, want a served parse-decline", r)
	}
	if string(r.FinalJSON) != `"hello"` || r.Raw != "hello" || r.Reasoning != "ref reasoning" {
		t.Errorf("served (%s, %q, %q), want the whole reference envelope", r.FinalJSON, r.Raw, r.Reasoning)
	}
	if !r.ChannelsCompared || r.AssistantMatch || r.RawMatch || r.ReasoningMatch {
		t.Errorf("channel facets = %+v, want compared and mismatched", r)
	}
	if !r.ErrorCompareRecorded || !r.ErrorCompareMatch {
		t.Errorf("error facet recorded=%v match=%v, want a recorded match", r.ErrorCompareRecorded, r.ErrorCompareMatch)
	}
}

// TestResolveReference_ReferenceFailuresAreTerminal: BAML's own extraction or parse failing
// on the original body is terminal with BAML's serving-path envelope.
func TestResolveReference_ReferenceFailuresAreTerminal(t *testing.T) {
	t.Run("extraction", func(t *testing.T) {
		res := &execute.AttemptResult{Outcome: execute.OutcomeInvalidBody, ProviderStatus: 200, ProviderBody: []byte("not json")}
		r := ResolveReference(context.Background(), nil, res, nil, ref(parse(`1`, nil)), nil)
		if r.Served || r.Outcome != OutcomeTranslateError {
			t.Fatalf("result = %+v, want a terminal translate error", r)
		}
		if got, want := r.Err.Error(), "buildrequest: failed to extract response content: provider openai: response body is not valid JSON"; got != want {
			t.Errorf("err = %q, want %q", got, want)
		}
		if r.RawDiagnostic != "not json" || !r.ErrorCompareRecorded || r.ErrorCompareMatch {
			t.Errorf("raw=%q errorCompare=%v/%v", r.RawDiagnostic, r.ErrorCompareRecorded, r.ErrorCompareMatch)
		}
	})
	t.Run("parse", func(t *testing.T) {
		res := nativeStructured(openAIBody(`{"k":1}`, ""), `{"k":1}`, "", `{"k":1}`)
		bad := func(context.Context, string) ([]byte, error) { return nil, errors.New("coerce failed") }
		r := ResolveReference(context.Background(), nil, res, nil, ref(bad), nil)
		if r.Served || r.Outcome != OutcomeParseError || !errors.Is(r.Err, buildrequest.ErrOutputParse) {
			t.Fatalf("result = %+v (err %v), want a terminal ErrOutputParse", r, r.Err)
		}
		if got, want := r.Err.Error(), "buildrequest: failed to parse final result: coerce failed"; got != want {
			t.Errorf("err = %q, want %q", got, want)
		}
		if r.RawDiagnostic != `{"k":1}` {
			t.Errorf("raw = %q, want the reference raw", r.RawDiagnostic)
		}
	})
	t.Run("missing closures", func(t *testing.T) {
		res := nativeStructured(openAIBody(`1`, ""), `1`, "", `1`)
		if r := ResolveReference(context.Background(), nil, res, nil, Reference{Provider: "openai", Extract: buildrequest.ExtractResponseContentBytes}, nil); r.Served || !errors.Is(r.Err, ErrNoBAMLOnlyParse) {
			t.Errorf("no parser: result = %+v, want ErrNoBAMLOnlyParse", r)
		}
		if r := ResolveReference(context.Background(), nil, res, nil, Reference{Provider: "openai", Parse: parse(`1`, nil)}, nil); r.Served || !errors.Is(r.Err, ErrNoReferenceExtract) {
			t.Errorf("no extractor: result = %+v, want ErrNoReferenceExtract", r)
		}
	})
}

// TestResolveReference_NoResponseAndCancellationAreTerminal covers the transport-class
// terminals: no result, a context error racing the pipeline, a context canceled before or
// during the reference parse, and a parser returning its own context error.
func TestResolveReference_NoResponseAndCancellationAreTerminal(t *testing.T) {
	boom := errors.New("dial tcp: refused")
	if r := ResolveReference(context.Background(), nil, nil, boom, ref(parse(`1`, nil)), nil); r.Served || r.Outcome != OutcomeTransportError || !errors.Is(r.Err, boom) {
		t.Errorf("nil result: %+v", r)
	}
	if r := ResolveReference(context.Background(), nil, nil, nil, ref(parse(`1`, nil)), nil); r.Served || r.Outcome != OutcomeTransportError {
		t.Errorf("nil result + nil error: %+v", r)
	}
	body := openAIBody(`1`, "")
	withRes := func() *execute.AttemptResult { return nativeStructured(body, `1`, "", `1`) }
	if r := ResolveReference(context.Background(), nil, withRes(), context.Canceled, ref(parse(`1`, nil)), nil); r.Served || !errors.Is(r.Err, context.Canceled) {
		t.Errorf("ctx error with a result: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := ResolveReference(ctx, nil, withRes(), nil, ref(parse(`1`, nil)), nil); r.Served || !errors.Is(r.Err, context.Canceled) {
		t.Errorf("canceled before the reference: %+v", r)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancelling := func(context.Context, string) ([]byte, error) { cancel2(); return []byte(`1`), nil }
	if r := ResolveReference(ctx2, nil, withRes(), nil, ref(cancelling), nil); r.Served || !errors.Is(r.Err, context.Canceled) || !r.SameResponseOracleRan {
		t.Errorf("canceled during the parse: %+v", r)
	}
	ownDeadline := func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded }
	if r := ResolveReference(context.Background(), nil, withRes(), nil, ref(ownDeadline), nil); r.Served || r.Outcome != OutcomeTransportError || !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Errorf("parser's own deadline: %+v", r)
	}
}

// TestResolveReference_OriginalNon2xxIsProviderError: the ORIGINAL status classifies, and the
// reference leg never runs on a non-2xx.
func TestResolveReference_OriginalNon2xxIsProviderError(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.OutcomeProviderError, ProviderStatus: 429, ProviderBody: []byte(`{"error":"x"}`)}
	entered := false
	var captured string
	r := ResolveReference(context.Background(), nil, res, nil, ref(parse(`1`, &captured)), func() { entered = true })
	var httpErr *llmhttp.HTTPError
	if r.Served || r.Outcome != OutcomeProviderError || !errors.As(r.Err, &httpErr) || httpErr.StatusCode != 429 || httpErr.Body != `{"error":"x"}` {
		t.Fatalf("result = %+v (err %v), want *HTTPError{429}", r, r.Err)
	}
	if entered || captured != "" {
		t.Errorf("the reference leg ran on a non-2xx (entered=%v captured=%q)", entered, captured)
	}
}

// TestResolveReference_HookFiresBeforeExtractorAndParserPanics: the phase hook precedes both
// the extractor and the parser, so a panic in either keeps the same-response phase.
func TestResolveReference_HookFiresBeforeExtractorAndParserPanics(t *testing.T) {
	res := nativeStructured(openAIBody(`1`, ""), `1`, "", `1`)
	for name, rf := range map[string]Reference{
		"extractor": {Provider: "openai", Extract: func(string, []byte, bool) (string, string, string, error) { panic("x") }, Parse: parse(`1`, nil)},
		"parser":    ref(func(context.Context, string) ([]byte, error) { panic("y") }),
	} {
		t.Run(name, func(t *testing.T) {
			entered := false
			func() {
				defer func() { _ = recover() }()
				ResolveReference(context.Background(), nil, res, nil, rf, func() { entered = true })
				t.Error("ResolveReference returned despite a panic")
			}()
			if !entered {
				t.Error("the phase hook did not fire before the panic")
			}
		})
	}
}
