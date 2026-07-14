//go:build nanollm_integration

package admission

// De-BAML cutover admission/mapper suite. Gated by `nanollm_integration`
// (the opt-in tag) because the production admission code links nanollm through
// nanollm.New/Prepare/Close; it imports NO BAML runtime, so it needs no CFFI.
//
// It proves, entirely WITHOUT a socket:
//   - POSITIVE: a fully-formed dynamic `_dynamic` call is admitted up to — but
//     NOT including — the exact RoundTrip; the mapper builds a request-scoped
//     one-openai-client nanollm config (separate alias, no ambient env), Prepare
//     produces a plan whose meta/expiry/headers/body all revalidate, and the
//     admitted plan is returned ready-to-send but unsent.
//   - NEGATIVE: every fixed-enum decline stage fires with its stable reason.
//   - the bounded declines/attempts metrics record the routing decisions.
//   - ZERO RoundTrips occur on any path (a counting exact transport, proven to
//     observe a real Execute by a positive control, stays at zero across the
//     whole suite).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/planassert"
	"github.com/invakid404/baml-rest/worker"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterVal reads one labeled counter value from a gathered registry (no
// testutil dependency). Returns 0 when the series is absent.
func counterVal(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			if labelsMatch(mm.GetLabel(), labels) {
				return mm.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// familySeriesCount reports how many series a metric family currently carries.
func familySeriesCount(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

// The literal auth/environment fence — the SAME single source of truth the
// Phase-5 differential legs share (../planassert). Fake, non-routable values: the
// `.invalid` base can never resolve, and no socket is ever opened, so neither the
// key nor the base leaves the process.
const (
	fenceModel   = planassert.FenceModel
	fenceBaseURL = planassert.FenceBaseURL
	fenceAPIKey  = planassert.FenceAPIKey
	fenceAlias   = planassert.FenceAlias
	wantURL      = planassert.WantURL
)

func sp(s string) *string { return &s }

func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

// validRegistry is the effective one-openai-client registry with the literal
// fence transport trio and nothing else.
func validRegistry() *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "TestClient",
			Provider: "openai",
			Options: map[string]any{
				"model":    fenceModel,
				"base_url": fenceBaseURL,
				"api_key":  fenceAPIKey,
			},
		}},
	}
}

// validInput is the fully-formed admitted unary `_dynamic` call. Negative cases
// mutate one field of it so exactly one stage fails.
func validInput() Input {
	return Input{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		Method:              "Baml_Rest_Dynamic",
		Mode:                ModeCall,
		SingleLeaf:          true,
		ResolvedProvider:    "openai",
		Registry:            validRegistry(),
		Alias:               fenceAlias,
		Messages: []bamlutils.DynamicMessage{
			{Role: "system", TextContent: sp("You are concise.")},
			{Role: "user", TextContent: sp("What is 2+2?")},
		},
		OutputSchema: simpleSchema(),
	}
}

// countingTransport is an http.RoundTripper that COUNTS RoundTrips and returns a
// canned response without dialing. Admission never routes through it; the suite
// asserts its count stays zero (proving zero sockets), and TestExactCounter proves
// the counter actually observes a real Execute so that zero is meaningful.
type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// assertNoSocket fails if the counting transport observed any RoundTrip.
func assertNoSocket(t *testing.T, ct *countingTransport) {
	t.Helper()
	if n := ct.n.Load(); n != 0 {
		t.Fatalf("admission opened %d socket(s); the no-send predicate must open zero", n)
	}
}

// TestExactCounterObservesRoundTrip is the positive control that makes every
// other test's zero-socket assertion meaningful: a real Execute through the
// counting transport moves the counter to one (no real network — the transport
// short-circuits with a canned response).
func TestExactCounterObservesRoundTrip(t *testing.T) {
	ct := &countingTransport{}
	exec := llmhttp.NewExactExecutor(ct)
	_, err := exec.Execute(context.Background(), &llmhttp.ExactAttemptRequest{
		Method:      "POST",
		URL:         wantURL,
		Headers:     []llmhttp.HeaderField{{Name: "Content-Type", Value: "application/json"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	})
	if err != nil {
		t.Fatalf("control Execute failed: %v", err)
	}
	if got := ct.n.Load(); got != 1 {
		t.Fatalf("counting transport observed %d RoundTrips, want 1 (zero-socket assertions would be vacuous otherwise)", got)
	}
}

// TestAdmitPositive proves a fully-formed call is admitted up to — but not
// including — the RoundTrip.
func TestAdmitPositive(t *testing.T) {
	ct := &countingTransport{}
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	// The Admitter preflights through THIS executor; if Admit ever RoundTripped,
	// the counting transport would observe it. assertNoSocket then proves Admit
	// opened zero sockets on the actual admission path.
	a := NewAdmitter(m, llmhttp.NewExactExecutor(ct))

	admitted, err := a.Admit(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Admit declined a fully-formed call: %v", err)
	}
	assertNoSocket(t, ct)

	if admitted.Prepared == nil {
		t.Fatal("admitted plan is nil")
	}
	if admitted.ExactRequest == nil {
		t.Fatal("admitted exact request is nil (should be ready to send)")
	}
	if admitted.Alias != fenceAlias {
		t.Errorf("admitted alias = %q, want %q", admitted.Alias, fenceAlias)
	}
	if admitted.Target != fenceModel {
		t.Errorf("admitted target = %q, want %q", admitted.Target, fenceModel)
	}
	if admitted.Provider != "openai" {
		t.Errorf("admitted provider = %q, want openai", admitted.Provider)
	}
	// The exact request carries the plan's fields verbatim, ready to send.
	if admitted.ExactRequest.Method != "POST" {
		t.Errorf("exact method = %q, want POST", admitted.ExactRequest.Method)
	}
	if admitted.ExactRequest.URL != wantURL {
		t.Errorf("exact url = %q, want %q", admitted.ExactRequest.URL, wantURL)
	}
	if !admitted.ExactRequest.BodyPresent || len(admitted.ExactRequest.Body) == 0 {
		t.Error("exact request body must be present and non-empty")
	}
	// The plan targets the non-routable fence base; it is never dialed.
	if !strings.Contains(admitted.Prepared.URL, ".invalid") {
		t.Errorf("plan URL %q lost the non-routable fence base", admitted.Prepared.URL)
	}
	// Body byte-equality with the plan is what validatePreparedBody already
	// enforced; re-assert the plan body is non-empty JSON carrying the target.
	if !strings.Contains(string(admitted.Prepared.Body), fenceModel) {
		t.Error("plan body does not carry the literal target model")
	}

	// Metrics: exactly one admitted attempt, zero declines.
	if got := counterVal(t, reg, "baml_rest_debaml_attempts_total", map[string]string{"mode": "call", "engine": "native", "provider": "openai", "outcome": "admitted"}); got != 1 {
		t.Errorf("attempts{admitted} = %v, want 1", got)
	}
	if got := familySeriesCount(t, reg, "baml_rest_debaml_declines_total"); got != 0 {
		t.Errorf("declines family has %d series, want 0 on a clean admit", got)
	}
}

// TestMetricsRegisterOnWorkerRegistry proves the collectors register cleanly on
// the worker's private Prometheus registry (the real construction the worker
// uses), coexisting with the Go/process collectors already on it.
func TestMetricsRegisterOnWorkerRegistry(t *testing.T) {
	if _, err := NewMetrics(worker.NewMetricsRegistry()); err != nil {
		t.Fatalf("de-BAML metrics failed to register on the worker registry: %v", err)
	}
}

// TestAdmitDeclineMatrix drives every fixed-enum decline stage through the FULL
// predicate (each row mutates one field of the valid input), asserting the
// stable stage+reason, zero sockets, and a recorded declines-metric increment.
func TestAdmitDeclineMatrix(t *testing.T) {
	dup := "TestClient"
	rp := "SomePolicy"
	cases := []struct {
		name           string
		mutate         func(in *Input)
		stage          Stage
		reason         Reason
		metricProvider string
	}{
		// --- layer 1: build / flag / route ---
		{"worker_incapable", func(in *Input) { in.WorkerCapable = false }, StageCapability, ReasonWorkerNotCapable, "openai"},
		{"request_api_absent", func(in *Input) { in.RequestAPIPresent = false }, StageCapability, ReasonRequestAPIAbsent, "openai"},
		{"not_buildrequest_route", func(in *Input) { in.OnBuildRequestRoute = false }, StageCapability, ReasonNotBuildReqRoute, "openai"},
		{"flag_off", func(in *Input) { in.FlagEnabled = false }, StageFlag, ReasonFlagDisabled, "openai"},
		{"wrong_method", func(in *Input) { in.Method = "Baml_Rest_Other" }, StageMethod, ReasonNotDynamicMethod, "openai"},
		{"with_raw_mode", func(in *Input) { in.Mode = ModeCallWithRaw }, StageMode, ReasonWithRawUnproven, "openai"},
		{"stream_mode", func(in *Input) { in.Mode = ModeStream }, StageMode, ReasonStreamingUnproven, "openai"},
		{"unknown_mode", func(in *Input) { in.Mode = Mode("weird") }, StageMode, ReasonModeUnknown, "openai"},
		{"schema_absent", func(in *Input) { in.OutputSchema = nil }, StagePrompt, ReasonOutputSchemaAbsent, "openai"},
		// --- layer 2: whole orchestration plan ---
		{"not_single_leaf", func(in *Input) { in.SingleLeaf = false }, StageStrategy, ReasonNotSingleLeaf, "openai"},
		{"fallback_chain", func(in *Input) { in.HasFallbackChain = true }, StageStrategy, ReasonFallbackChain, "openai"},
		{"round_robin", func(in *Input) { in.HasRoundRobin = true }, StageStrategy, ReasonRoundRobin, "openai"},
		{"legacy_child", func(in *Input) { in.IsLegacyChild = true }, StageStrategy, ReasonLegacyChild, "openai"},
		{"request_retry_override", func(in *Input) { in.HasRequestRetryOverride = true }, StageStrategy, ReasonRequestRetryOverride, "openai"},
		{"url_rewrite_or_proxy", func(in *Input) { in.HasURLRewriteOrProxy = true }, StageStrategy, ReasonURLRewriteOrProxy, "openai"},
		{"resolved_provider_not_openai", func(in *Input) { in.ResolvedProvider = "anthropic" }, StageProvider, ReasonProviderNotOpenAI, "other"},
		// --- layer 3: effective dynamic client ---
		{"nil_registry", func(in *Input) { in.Registry = nil }, StageClientSelection, ReasonNoRegistry, "openai"},
		{"registry_invalid_duplicate", func(in *Input) {
			in.Registry.Clients = append(in.Registry.Clients, &bamlutils.ClientProperty{Name: dup, Provider: "openai", Options: map[string]any{"model": fenceModel, "base_url": fenceBaseURL, "api_key": fenceAPIKey}})
		}, StageClientSelection, ReasonRegistryInvalid, "openai"},
		{"no_clients", func(in *Input) { in.Registry = &bamlutils.ClientRegistry{} }, StageClientSelection, ReasonNoClients, "openai"},
		{"ambiguous_no_primary", func(in *Input) {
			in.Registry = &bamlutils.ClientRegistry{Clients: []*bamlutils.ClientProperty{
				{Name: "A", Provider: "openai", Options: map[string]any{"model": fenceModel, "base_url": fenceBaseURL, "api_key": fenceAPIKey}},
				{Name: "B", Provider: "openai", Options: map[string]any{"model": fenceModel, "base_url": fenceBaseURL, "api_key": fenceAPIKey}},
			}}
		}, StageClientSelection, ReasonAmbiguousSelection, "openai"},
		{"primary_missing", func(in *Input) { in.Registry.Primary = sp("Ghost") }, StageClientSelection, ReasonPrimaryMissing, "openai"},
		{"model_absent", func(in *Input) { delete(in.Registry.Clients[0].Options, "model") }, StageClientSelection, ReasonModelAbsent, "openai"},
		{"model_not_literal", func(in *Input) { in.Registry.Clients[0].Options["model"] = 42 }, StageClientSelection, ReasonModelNotLiteral, "openai"},
		{"client_retry_policy", func(in *Input) { in.Registry.Clients[0].RetryPolicy = &rp }, StageStrategy, ReasonClientRetryPolicy, "openai"},
		{"client_provider_not_openai", func(in *Input) { in.Registry.Clients[0].Provider = "anthropic" }, StageProvider, ReasonProviderNotOpenAI, "openai"},
		// --- separate internal alias (must be non-empty, distinct from target + client name) ---
		{"alias_empty", func(in *Input) { in.Alias = "" }, StageClientSelection, ReasonInvalidAlias, "openai"},
		{"alias_equals_target", func(in *Input) { in.Alias = fenceModel }, StageClientSelection, ReasonInvalidAlias, "openai"},
		{"alias_equals_client_name", func(in *Input) { in.Alias = "TestClient" }, StageClientSelection, ReasonInvalidAlias, "openai"},
		// --- client_option ---
		{"headers_option", func(in *Input) { in.Registry.Clients[0].Options["headers"] = map[string]any{"X": "y"} }, StageClientOption, ReasonHeadersOption, "openai"},
		{"tools_option", func(in *Input) { in.Registry.Clients[0].Options["tools"] = []any{} }, StageClientOption, ReasonToolsOption, "openai"},
		{"response_format_option", func(in *Input) { in.Registry.Clients[0].Options["response_format"] = map[string]any{} }, StageClientOption, ReasonResponseFormatOption, "openai"},
		{"request_body_option", func(in *Input) { in.Registry.Clients[0].Options["request_body"] = map[string]any{} }, StageClientOption, ReasonRequestBodyOption, "openai"},
		{"unproven_option", func(in *Input) { in.Registry.Clients[0].Options["temperature"] = 0.7 }, StageClientOption, ReasonUnprovenClientOption, "openai"},
		// --- credential_source ---
		{"base_url_absent", func(in *Input) { delete(in.Registry.Clients[0].Options, "base_url") }, StageCredentialSource, ReasonBaseURLAbsent, "openai"},
		{"api_key_absent", func(in *Input) { delete(in.Registry.Clients[0].Options, "api_key") }, StageCredentialSource, ReasonAPIKeyAbsent, "openai"},
		{"api_key_empty", func(in *Input) { in.Registry.Clients[0].Options["api_key"] = "" }, StageCredentialSource, ReasonAPIKeyAbsent, "openai"},
		// --- layer 4: message ---
		{"empty_messages", func(in *Input) { in.Messages = nil }, StageMessage, ReasonEmptyMessages, "openai"},
		{"bad_role", func(in *Input) { in.Messages[0].Role = "tool" }, StageMessage, ReasonRoleUnsupported, "openai"},
		{"message_metadata_populated", func(in *Input) {
			in.Messages[0].Metadata = &bamlutils.MessageMetadata{CacheControl: &bamlutils.CacheControl{Type: "ephemeral"}}
		}, StageMessage, ReasonMessageMetadata, "openai"},
		{"message_metadata_empty_object", func(in *Input) {
			in.Messages[0].Metadata = &bamlutils.MessageMetadata{}
		}, StageMessage, ReasonMessageMetadata, "openai"},
		{"empty_message", func(in *Input) { in.Messages[0] = bamlutils.DynamicMessage{Role: "user"} }, StageMessage, ReasonEmptyMessage, "openai"},
		{"media_part", func(in *Input) {
			in.Messages[0] = bamlutils.DynamicMessage{Role: "user", PartsContent: []bamlutils.DynamicContentPart{{Type: "image", Image: &bamlutils.MediaInput{URL: sp("http://x/y.png")}}}}
		}, StageMessage, ReasonMediaPart, "openai"},
		{"unknown_part", func(in *Input) {
			in.Messages[0] = bamlutils.DynamicMessage{Role: "user", PartsContent: []bamlutils.DynamicContentPart{{Type: "mystery"}}}
		}, StageMessage, ReasonUnknownPart, "openai"},
		{"text_part_with_media_payload", func(in *Input) {
			in.Messages[0] = bamlutils.DynamicMessage{Role: "user", PartsContent: []bamlutils.DynamicContentPart{{Type: "text", Text: sp("hi"), Image: &bamlutils.MediaInput{URL: sp("http://x/y.png")}}}}
		}, StageMessage, ReasonMixedPayload, "openai"},
		{"output_format_with_payload", func(in *Input) {
			in.Messages[0] = bamlutils.DynamicMessage{Role: "user", PartsContent: []bamlutils.DynamicContentPart{{Type: "output_format", Text: sp("leak")}}}
		}, StageMessage, ReasonMixedPayload, "openai"},
		{"invalid_utf8", func(in *Input) { in.Messages[0].TextContent = sp("bad\xff\xfeutf8") }, StageMessage, ReasonInvalidUTF8, "openai"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := &countingTransport{}
			reg := prometheus.NewRegistry()
			m, err := NewMetrics(reg)
			if err != nil {
				t.Fatalf("NewMetrics: %v", err)
			}
			a := NewAdmitter(m, llmhttp.NewExactExecutor(ct))

			in := validInput()
			tc.mutate(&in)

			admitted, err := a.Admit(context.Background(), in)
			assertNoSocket(t, ct)
			if admitted != nil {
				t.Fatalf("expected a decline, got an admitted plan")
			}
			var d *Decline
			if !errors.As(err, &d) {
				t.Fatalf("expected a *Decline, got %T: %v", err, err)
			}
			if !errors.Is(err, ErrDeclined) {
				t.Errorf("decline does not unwrap to ErrDeclined")
			}
			if d.Stage != tc.stage || d.Reason != tc.reason {
				t.Fatalf("decline = (%s, %s), want (%s, %s)", d.Stage, d.Reason, tc.stage, tc.reason)
			}
			// The decline is recorded once with its stage/reason, and once on the
			// attempts family with the decline outcome.
			if got := counterVal(t, reg, "baml_rest_debaml_declines_total", map[string]string{"stage": string(tc.stage), "reason": string(tc.reason)}); got != 1 {
				t.Errorf("declines{%s,%s} = %v, want 1", tc.stage, tc.reason, got)
			}
			// The attempts mode/provider labels reflect the (normalized) request
			// under evaluation, bounded to the fixed enums.
			wantMode := string(normalizeMode(in.Mode))
			if got := counterVal(t, reg, "baml_rest_debaml_attempts_total", map[string]string{"mode": wantMode, "engine": "native", "provider": tc.metricProvider, "outcome": "decline"}); got != 1 {
				t.Errorf("attempts{mode=%s,decline} = %v, want 1", wantMode, got)
			}
		})
	}
}

// TestModeNormalization keeps the attempts metric bounded: an unrecognized mode
// is folded to the single "unknown" label, never a free string.
func TestModeNormalization(t *testing.T) {
	for _, m := range []Mode{ModeCall, ModeCallWithRaw, ModeStream, ModeStreamWithRaw} {
		if normalizeMode(m) != m {
			t.Errorf("normalizeMode(%q) changed an admitted-enum value", m)
		}
	}
	if got := normalizeMode(Mode("anything-else")); got != ModeUnknown {
		t.Errorf("normalizeMode(unknown) = %q, want %q", got, ModeUnknown)
	}
}
