package execute

// De-BAML Slice 6b adapter: the BAML-free PreparedRequest -> exact transport ->
// TranslateResponse -> native structured-output (SAP) pipeline for ONE unary
// OpenAI attempt.
//
// This is the ONLY thing in the de-BAML stack that knows about nanollm aliases
// AND about baml-rest's exact transport at the same time: it wires a fresh,
// Phase-5-admitted nanollm.PreparedRequest through the merged Slice-6a exact
// transport primitive (bamlutils/llmhttp ExactExecutor — ordered headers, raw
// bytes, one RoundTrip) and then back through nanollm.TranslateResponse and the
// native final parser. The Slice-6a primitive stays entirely nanollm-agnostic;
// this adapter is where the two meet.
//
// Deliberate non-goals, matching the Phase 6 scope's Option B:
//
//   - NO nanollm Do/DoStream. Exactly ONE transport attempt per RunAttempt call,
//     performed by the exact executor. There is no fallback advance, no
//     same-model retry, and no expiry re-prepare hidden in the transport — the
//     one owner of request-attempt policy is baml-rest.
//   - The exact attempted alias (prep.Meta.ModelAlias) — never a primary alias —
//     is what TranslateResponse is called with.
//   - Non-2xx and invalid-2xx are PROVIDER OUTCOMES returned as typed data, never
//     fed to the structured-output parser (SAP). A 2xx that the parser merely
//     DECLINES (ErrDeBAMLParseUnsupported) is a parity-decline recorded as data;
//     any OTHER (claimed) parse error propagates unchanged so a fallback can
//     never masquerade as a native success.
//   - This changes NO serving behavior. As of de-BAML cutover Slice 2 this
//     bridge is UNTAGGED production code (it links into the BAML+nanollm worker
//     built from this out-of-go.work module), but it is still UNREACHABLE from
//     production routing: the worker stores a native capability yet the
//     orchestrator's native child-attempt callback stays nil/hard-off. The
//     expensive Phase-5/6 oracle + go-mocklm suites that exercise it remain
//     `nanollm_integration`-gated. A later slice wires it behind the callback.

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// ErrPlanExpired is returned when a prepared plan's signature window has passed
// BEFORE any socket is opened. Re-preparing belongs to the outer planner, never
// to this transport adapter, so an expired plan is rejected rather than silently
// re-signed. (OpenAI plans are unsigned and never expire; this guards the seam
// for the signed-plan providers a later phase adds.)
var ErrPlanExpired = errors.New("nanollmprepare: prepared plan expired before attempt")

// ErrAttemptUnsupported is the umbrella sentinel for a parity-decline: the
// native support decision refused a plan BEFORE opening a socket, so native
// never out-claims BAML on a shape it has not proven. Recover the stable
// stage/reason via errors.As(&UnsupportedError{}).
var ErrAttemptUnsupported = errors.New("nanollmprepare: prepared plan not supported by the native unary attempt")

// UnsupportedError carries a stable, secret-free stage/reason for a
// parity-decline (the "deferral record" the scope asks every decline to leave).
// It unwraps to ErrAttemptUnsupported.
type UnsupportedError struct {
	Stage  string
	Reason string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("nanollmprepare: unsupported attempt (%s): %s", e.Stage, e.Reason)
}

func (e *UnsupportedError) Unwrap() error { return ErrAttemptUnsupported }

// Outcome classifies the terminal disposition of one native unary attempt.
type Outcome int

const (
	// OutcomeStructured: a provider 2xx translated to OpenAI JSON, whose
	// assistant text the native parser cleanly CLAIMED — the structured output
	// is in AttemptResult.Structured.
	OutcomeStructured Outcome = iota
	// OutcomeParseDeclined: a provider 2xx whose assistant text the native
	// parser DECLINED (ErrDeBAMLParseUnsupported). In a later production-routing
	// phase this rejoins the BAML parse fallback; here it is recorded as a
	// parity-decline (SAP WAS invoked; it simply declined).
	OutcomeParseDeclined
	// OutcomeProviderError: an ordinary provider non-2xx, translated to the
	// uniform JSON error envelope. SAP is NEVER invoked. Translated carries the
	// envelope (real status, BodyIsJSON true).
	OutcomeProviderError
	// OutcomeInvalidBody: a provider 2xx whose body TranslateResponse could not
	// parse (*nanollm.InvalidBodyError), mapped to the same 502/uniform-envelope
	// provider outcome nanollm's own Do returns. SAP is NEVER invoked.
	OutcomeInvalidBody
)

func (o Outcome) String() string {
	switch o {
	case OutcomeStructured:
		return "structured"
	case OutcomeParseDeclined:
		return "parse-declined"
	case OutcomeProviderError:
		return "provider-error"
	case OutcomeInvalidBody:
		return "invalid-body"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// AttemptResult is the typed internal result of one native unary attempt. It
// preserves enough data for the later production-routing mapping (status, raw
// provider body, translated response, usage) without finalizing any public
// serving error envelope — no serving route changes in this phase.
type AttemptResult struct {
	Outcome Outcome

	// AttemptedAlias is prep.Meta.ModelAlias — the exact alias whose Prepare
	// produced the executed plan and which TranslateResponse was called with.
	AttemptedAlias string

	// ProviderStatus / ProviderBody are the RAW exact-transport observation for
	// every status: the real upstream status and the bounded raw body (16 MiB
	// success cap / 64 KiB non-2xx cap, both enforced by the exact executor).
	ProviderStatus int
	ProviderBody   []byte

	// Translated is nanollm's translated response: OpenAI JSON on 2xx, the
	// uniform error envelope on a translated non-2xx, or the synthesized
	// 502/envelope Response for an invalid-2xx. Nil only never happens on a
	// returned (nil-error) result.
	Translated *nanollm.Response

	// Usage is retained from Translated even though Phase 6 surfaces no new
	// public metadata (scope: do not discard it in an API that will need
	// redesign immediately afterward).
	Usage *nanollm.Usage

	// AssistantText is the OpenAI assistant text extracted from a 2xx body and
	// fed to SAP (empty for provider outcomes, where extraction never runs). It
	// is the text-only parseable channel — the same value the BAML final parser
	// consumes — never reasoning.
	AssistantText string

	// Raw / Reasoning are the /call-with-raw extractor channels split from the
	// SAME 2xx translated body AssistantText comes from: Raw is the text-only
	// assistant channel exposed on /call-with-raw's Raw(); Reasoning carries the
	// provider reasoning/thinking text when the caller opted in (cfg.IncludeReasoning).
	// Both are empty on provider outcomes (non-2xx / invalid-2xx), where extraction
	// never runs. They are retained so the same-response oracle can compare the
	// FULL native-vs-BAML /call-with-raw envelope (structured + raw + reasoning),
	// not just structured output (extends the Phase 6c differential).
	Raw       string
	Reasoning string

	// Structured is the flattened native structured output on OutcomeStructured
	// (nil otherwise) — the same flattened shape the generated wrapper consumes.
	Structured stdjson.RawMessage

	// DeclineReason is the parser's decline explanation on OutcomeParseDeclined.
	DeclineReason string

	// SAPInvoked records whether the native parser was called at all. It is the
	// affirmative proof that provider outcomes (non-2xx / invalid-2xx) never
	// reach SAP: those results carry SAPInvoked == false.
	SAPInvoked bool
}

// AttemptConfig is the input to RunAttempt: the nanollm client (for
// TranslateResponse with the attempted alias), the fresh Phase-5-admitted plan,
// the exact transport executor (built by the caller over a loopback-guarded
// RoundTripper — the adapter stays transport-agnostic), and the native parser +
// carried output schema.
type AttemptConfig struct {
	Client           *nanollm.Client
	Prepared         *nanollm.PreparedRequest
	Executor         *llmhttp.ExactExecutor
	Parse            bamlutils.DeBAMLParseFunc
	OutputSchema     *bamlutils.DynamicOutputSchema
	IncludeReasoning bool

	// SendHeartbeat, when non-nil, is the first-2xx liveness signal forwarded to
	// the exact executor (ExecuteWithHeartbeat): it fires the instant the provider
	// returns 2xx headers, before the body is buffered, so a pool hung-detector
	// sees liveness on a slow body. It is a pre-canary compatibility hook — the
	// native transport twin of the onSuccess callback the BAML send path gets — and
	// never mutates the request. Nil in the no-heartbeat callers (the gated 6b/6c
	// oracles), where it is a no-op.
	SendHeartbeat func()
}

// ConsumeConfig is the input to ConsumeResponse: the SAME-response consumer that
// runs native TranslateResponse -> assistant/raw/reasoning extraction -> native
// SAP over an ALREADY-COMPLETED (status, body) that some OTHER agent fetched —
// with NO transport, NO RoundTrip, and NO second provider request. It is the
// response side of RunAttempt factored out of the transport, so the de-BAML
// same-response shadow oracle can feed it BAML's already-fetched status+body
// while BAML remains the sole provider send.
//
// Client + Alias are the request-scoped nanollm client and the EXACT attempted
// alias TranslateResponse is called with (never a primary alias); Parse is the
// native SAP; OutputSchema is the carried dynamic output schema; IncludeReasoning
// gates the reasoning extraction channel.
type ConsumeConfig struct {
	Client           *nanollm.Client
	Alias            string
	Parse            bamlutils.DeBAMLParseFunc
	OutputSchema     *bamlutils.DynamicOutputSchema
	IncludeReasoning bool
}

// RunAttempt performs exactly one native unary attempt and returns the typed
// outcome.
//
// Error vs. data contract:
//
//   - Expired plan, parity-decline, and any invalid config are rejected BEFORE
//     the socket (Go error; no request is sent).
//   - Transport failures (construction, cancellation/timeout, transport, read),
//     a translate error OTHER than *InvalidBodyError, an assistant-extraction
//     failure, and a CLAIMED native parse error all propagate as Go errors.
//   - An ordinary non-2xx (OutcomeProviderError), an invalid-2xx
//     (OutcomeInvalidBody), a clean structured claim (OutcomeStructured), and a
//     parser decline (OutcomeParseDeclined) are returned as (*AttemptResult,
//     nil) — provider/parse OUTCOMES, not errors.
func RunAttempt(ctx context.Context, cfg AttemptConfig) (*AttemptResult, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("nanollmprepare: nil nanollm client")
	}
	if cfg.Prepared == nil {
		return nil, fmt.Errorf("nanollmprepare: nil prepared request")
	}
	if cfg.Executor == nil {
		return nil, fmt.Errorf("nanollmprepare: nil exact executor")
	}
	if cfg.Parse == nil {
		return nil, fmt.Errorf("nanollmprepare: nil parse function")
	}
	if cfg.OutputSchema == nil {
		return nil, fmt.Errorf("nanollmprepare: nil output schema")
	}

	prep := cfg.Prepared

	// (1) Reject an expired plan BEFORE the attempt — no re-prepare in the
	// transport. Checked first, before the support decision, so an expired plan
	// never even reaches support classification.
	if prep.Expired() {
		return nil, ErrPlanExpired
	}

	// (2) Native support decision — the parity-decline gate — runs BEFORE any
	// socket is opened. The Phase 6 admitted surface is exactly a unary,
	// non-streaming OpenAI ChatCompletion with a JSON response format and a
	// present, non-empty body; anything else declines rather than out-claiming
	// BAML on an unproven shape.
	if err := supportDecision(prep); err != nil {
		return nil, err
	}

	// (3) Convert the plan's exact fields into the neutral exact-attempt carrier
	// (ordered headers, raw body). The body is present and non-empty (support
	// decision guaranteed it), so BodyPresent is true.
	exactReq := &llmhttp.ExactAttemptRequest{
		Method:      prep.Method,
		URL:         prep.URL,
		Headers:     headerFields(prep.Headers),
		Body:        prep.Body,
		BodyPresent: true,
	}

	// (4) Exactly one exact-transport attempt. The executor performs a single
	// RoundTrip and returns status + raw body as data for EVERY status; a Go
	// error here is a non-response failure (transport-controlled-header decline,
	// request construction, cancellation/timeout, transport, or body-read).
	// cfg.SendHeartbeat (nil in the gated oracles) is the first-2xx liveness hook
	// the exact lane fires before buffering the body — never mutating the request.
	resp, err := cfg.Executor.ExecuteWithHeartbeat(ctx, exactReq, cfg.SendHeartbeat)
	if err != nil {
		return nil, err
	}

	// (5-7) The transport is done; the response side owns NO transport. Hand the
	// already-completed status+body to the same-response consumer with the EXACT
	// attempted alias — the identical pipeline the de-BAML shadow oracle drives
	// over BAML's already-fetched response.
	return ConsumeResponse(ctx, ConsumeConfig{
		Client:           cfg.Client,
		Alias:            prep.Meta.ModelAlias,
		Parse:            cfg.Parse,
		OutputSchema:     cfg.OutputSchema,
		IncludeReasoning: cfg.IncludeReasoning,
	}, resp.StatusCode, resp.Body)
}

// ConsumeResponse runs the SAME-response native pipeline —
// TranslateResponse(attempted alias) -> assistant/raw/reasoning extraction ->
// native SAP — over an ALREADY-COMPLETED (status, body) that some OTHER agent
// fetched. It owns NO transport: it opens no socket, issues no RoundTrip, and
// makes no second provider request. RunAttempt calls it after its own single
// exact send; the de-BAML same-response shadow oracle calls it with BAML's
// already-fetched status+body so BAML stays the sole provider send.
//
// The error-vs-data contract is identical to RunAttempt's response side:
//
//   - An invalid-2xx (nanollm *InvalidBodyError) maps to OutcomeInvalidBody with
//     the synthesized 502/uniform-envelope Response — SAP is NEVER invoked.
//   - An ordinary non-2xx maps to OutcomeProviderError (uniform envelope) — SAP is
//     NEVER invoked.
//   - A clean 2xx whose assistant text SAP CLAIMS is OutcomeStructured; a 2xx SAP
//     DECLINES (ErrDeBAMLParseUnsupported) is OutcomeParseDeclined — both returned
//     as (*AttemptResult, nil).
//   - A translate error other than *InvalidBodyError, a non-JSON 2xx, an
//     assistant-extraction failure, and a CLAIMED (non-decline) SAP error all
//     propagate as Go errors.
func ConsumeResponse(ctx context.Context, cfg ConsumeConfig, status int, body []byte) (*AttemptResult, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("nanollmprepare: nil nanollm client")
	}
	if cfg.Parse == nil {
		return nil, fmt.Errorf("nanollmprepare: nil parse function")
	}
	if cfg.OutputSchema == nil {
		return nil, fmt.Errorf("nanollmprepare: nil output schema")
	}

	res := &AttemptResult{
		AttemptedAlias: cfg.Alias,
		ProviderStatus: status,
		ProviderBody:   body,
	}

	// (5) Translate with the EXACT attempted alias.
	translated, terr := cfg.Client.TranslateResponse(cfg.Alias, status, body)
	if terr != nil {
		// An invalid-2xx surfaces as the typed *InvalidBodyError carrying the
		// (caller-facing status, uniform envelope) pair. Map it into the SAME
		// response-like provider outcome Do would return — never a misclassified
		// network error, and never feed the malformed body to SAP.
		var ibe *nanollm.InvalidBodyError
		if errors.As(terr, &ibe) {
			res.Outcome = OutcomeInvalidBody
			res.Translated = &nanollm.Response{
				Status:     ibe.Status,
				Body:       ibe.Envelope,
				BodyIsJSON: true,
			}
			return res, nil
		}
		// Any other translate failure is a genuine error (not a provider
		// outcome, not SAP-eligible) and propagates.
		return nil, fmt.Errorf("nanollmprepare: TranslateResponse(%s): %w", cfg.Alias, terr)
	}

	res.Translated = translated
	res.Usage = translated.Usage

	// (6) Ordinary non-2xx: TranslateResponse normalized it into the uniform
	// JSON error envelope (real status, BodyIsJSON true). It is a provider
	// outcome; SAP is NEVER invoked.
	if translated.Status < 200 || translated.Status >= 300 {
		res.Outcome = OutcomeProviderError
		return res, nil
	}

	// (7) Unary 2xx: require a valid JSON body, extract the OpenAI assistant
	// text + raw + reasoning (translated.Body is OpenAI-format regardless of the
	// original provider), and feed the assistant text to the native parser. Raw
	// and reasoning are retained on the result so the same-response oracle can
	// compare the full /call-with-raw envelope, not just structured output.
	if !translated.BodyIsJSON {
		return nil, fmt.Errorf("nanollmprepare: 2xx translated response is not JSON (status %d)", translated.Status)
	}
	parseable, raw, reasoning, xerr := buildrequest.ExtractResponseContentBytes("openai", translated.Body, cfg.IncludeReasoning)
	if xerr != nil {
		return nil, fmt.Errorf("nanollmprepare: extracting assistant text: %w", xerr)
	}
	res.AssistantText = parseable
	res.Raw = raw
	res.Reasoning = reasoning

	// SAP is invoked exactly here and nowhere else on the 2xx path.
	res.SAPInvoked = true
	parsed, perr := cfg.Parse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          parseable,
		OutputSchema: cfg.OutputSchema,
		Stream:       false,
	})
	if perr != nil {
		// A clean decline is a parity-decline recorded as data (no fallback
		// rejoined here — no serving change in this phase). Any OTHER parse
		// error is a CLAIMED native failure and propagates UNCHANGED so the
		// differential can catch native-vs-BAML drift.
		if errors.Is(perr, bamlutils.ErrDeBAMLParseUnsupported) {
			res.Outcome = OutcomeParseDeclined
			res.DeclineReason = perr.Error()
			return res, nil
		}
		return nil, perr
	}

	res.Outcome = OutcomeStructured
	res.Structured = parsed.JSON
	return res, nil
}

// supportDecision is the parity-decline gate. It runs before any socket and
// declines with a stable, secret-free stage/reason for anything outside the
// Phase 6 admitted unary-OpenAI surface. Header VALUES and body bytes are never
// referenced, so a decline diagnostic can never leak a secret.
func supportDecision(prep *nanollm.PreparedRequest) error {
	m := prep.Meta
	if m.Provider != nativebody.ProviderOpenAI {
		return &UnsupportedError{Stage: "provider", Reason: fmt.Sprintf("provider %q is not the admitted openai surface", m.Provider)}
	}
	if m.RequestType != nanollm.ChatCompletion {
		return &UnsupportedError{Stage: "request_type", Reason: fmt.Sprintf("request type %q is not chat_completion", m.RequestType)}
	}
	if m.Stream {
		return &UnsupportedError{Stage: "stream", Reason: "streaming is out of scope for the unary attempt (Phase 7)"}
	}
	if prep.ResponseFormat != nanollm.FormatJSON {
		return &UnsupportedError{Stage: "response_format", Reason: fmt.Sprintf("response format %q is not json", prep.ResponseFormat)}
	}
	if prep.Method != "POST" {
		return &UnsupportedError{Stage: "method", Reason: fmt.Sprintf("method %q is not POST", prep.Method)}
	}
	if len(prep.Body) == 0 {
		return &UnsupportedError{Stage: "body", Reason: "prepared body is empty"}
	}
	return nil
}

// headerFields converts nanollm's ordered [][2]string plan headers into the
// exact carrier's []HeaderField, preserving pair order and repeated names — the
// two properties the exact lane must not lose. Values are copied verbatim; the
// adapter never rewrites, adds, or drops a header.
func headerFields(pairs [][2]string) []llmhttp.HeaderField {
	out := make([]llmhttp.HeaderField, len(pairs))
	for i, p := range pairs {
		out[i] = llmhttp.HeaderField{Name: p[0], Value: p[1]}
	}
	return out
}
