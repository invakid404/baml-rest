//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	stdjson "encoding/json"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/workerplugin"
)

// dynclientGate centralises the BAML-version gate the HTTP dynamic tests
// use: dynamic call/stream paths still rely on the BuildRequest API which
// requires BAML >= 0.219.0 in absence of a local BAML source override.
// The dynclient public surface follows the same constraint because it
// drives the same generated BAML adapter.
func dynclientGate(t *testing.T) {
	t.Helper()
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
}

// dynclientCallGate restricts the test to BAML versions where dynamic
// call/stream is known-good. Older BAML versions ship a bug where the
// streaming API doesn't propagate dynamic classes to the parser; the
// existing HTTP tests skip with the same predicate.
func dynclientCallGate(t *testing.T) {
	t.Helper()
	dynclientGate(t)
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}
}

// newDynclient constructs a public dynclient.Client wired with a base-
// URL rewrite that maps the container-internal mockllm URL to the
// host-reachable URL the test process can hit. Without this rewrite the
// public client would try to dial `http://mockllm:8080`, which is only
// resolvable inside the baml-rest container.
func newDynclient(t *testing.T, opts ...dynclient.Option) *dynclient.Client {
	t.Helper()
	rewrites := []dynclient.BaseURLRewriteRule{
		{From: TestEnv.MockLLMInternal, To: TestEnv.MockLLMURL},
	}
	all := append([]dynclient.Option{dynclient.WithBaseURLRewrites(rewrites)}, opts...)
	c, err := dynclient.New(all...)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	return c
}

// httpRegistry wraps the existing testutil helper and reshapes it into a
// dynclient.ClientRegistry value. Both flavours produce identical wire
// JSON, but the Go types differ between packages, so callers building a
// parity test need a converter rather than two parallel builders.
func dynRegistry(reg *testutil.ClientRegistry) *dynclient.ClientRegistry {
	if reg == nil {
		return nil
	}
	out := &dynclient.ClientRegistry{Primary: reg.Primary}
	for _, c := range reg.Clients {
		if c == nil {
			continue
		}
		provider := ""
		if c.Provider != nil {
			provider = *c.Provider
		}
		cp := &dynclient.ClientProperty{
			Name:        c.Name,
			Provider:    provider,
			RetryPolicy: c.RetryPolicy,
			Options:     c.Options,
		}
		out.Clients = append(out.Clients, cp)
	}
	return out
}

// simpleAnswerSchema returns matching output schemas for the HTTP and
// library calls. The schemas are intentionally identical so any
// divergence in the test surfaces a real flattening / parsing mismatch
// rather than a fixture difference.
func simpleAnswerSchema() (*testutil.DynamicOutputSchema, *dynclient.OutputSchema) {
	httpSchema := &testutil.DynamicOutputSchema{
		Properties: map[string]*testutil.DynamicProperty{
			"answer": {Type: "string"},
		},
	}
	libSchema := &dynclient.OutputSchema{
		Properties: dynclientFromMap(map[string]*dynclient.Property{
			"answer": {Type: "string"},
		}),
	}
	return httpSchema, libSchema
}

// jsonEqual compares two JSON blobs for semantic equality. Map ordering
// is not stable across producers, so byte equality would yield spurious
// failures.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := sonic.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v (raw=%s)", err, string(a))
	}
	if err := sonic.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v (raw=%s)", err, string(b))
	}
	return reflect.DeepEqual(av, bv)
}

// TestDynclientDynamicCallParity runs the same logical /call/_dynamic
// request through the HTTP client and the in-process dynclient.Client
// and asserts the returned JSON payloads are semantically identical.
func TestDynclientDynamicCallParity(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupNonStreamingScenario(t, "test-dynclient-call", `{"answer": "4"}`)
	httpSchema, libSchema := simpleAnswerSchema()

	httpResp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
		Messages:       []testutil.DynamicMessage{{Role: "user", Content: "What is 2+2?"}},
		ClientRegistry: opts.ClientRegistry,
		OutputSchema:   httpSchema,
	})
	if err != nil {
		t.Fatalf("HTTP DynamicCall: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Fatalf("HTTP DynamicCall status %d: %s", httpResp.StatusCode, httpResp.Error)
	}

	hello := "What is 2+2?"
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}
	client := newDynclient(t)
	libResp, err := client.DynamicCall(ctx, libReq)
	if err != nil {
		t.Fatalf("Library DynamicCall: %v", err)
	}

	if !jsonEqual(t, httpResp.Body, libResp.Data) {
		t.Errorf("payload mismatch:\n http=%s\n lib =%s", string(httpResp.Body), string(libResp.Data))
	}
	if strings.Contains(string(libResp.Data), "DynamicProperties") {
		t.Errorf("library result still wraps payload: %s", string(libResp.Data))
	}
}

// TestDynclientDynamicCallRawParity asserts that /call-with-raw/_dynamic
// and DynamicCallRaw agree on the structured data, the raw upstream
// text, and the (empty in this scenario) reasoning text.
func TestDynclientDynamicCallRawParity(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupNonStreamingScenario(t, "test-dynclient-call-raw", `{"answer": "4"}`)
	httpSchema, libSchema := simpleAnswerSchema()

	httpResp, err := BAMLClient.DynamicCallWithRaw(ctx, testutil.DynamicRequest{
		Messages:       []testutil.DynamicMessage{{Role: "user", Content: "What is 2+2?"}},
		ClientRegistry: opts.ClientRegistry,
		OutputSchema:   httpSchema,
	})
	if err != nil {
		t.Fatalf("HTTP DynamicCallWithRaw: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Fatalf("HTTP DynamicCallWithRaw status %d: %s", httpResp.StatusCode, httpResp.Error)
	}

	hello := "What is 2+2?"
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}
	client := newDynclient(t)
	libResp, err := client.DynamicCallRaw(ctx, libReq)
	if err != nil {
		t.Fatalf("Library DynamicCallRaw: %v", err)
	}

	if !jsonEqual(t, httpResp.Data, libResp.Data) {
		t.Errorf("data mismatch:\n http=%s\n lib =%s", string(httpResp.Data), string(libResp.Data))
	}
	if httpResp.Raw != libResp.Raw {
		t.Errorf("raw mismatch:\n http=%q\n lib =%q", httpResp.Raw, libResp.Raw)
	}
	if httpResp.Reasoning != libResp.Reasoning {
		t.Errorf("reasoning mismatch:\n http=%q\n lib =%q", httpResp.Reasoning, libResp.Reasoning)
	}
}

// TestDynclientDynamicStreamParity drains both stream paths and asserts
// the final flattened payload is identical and that the library reported
// at least one partial event (the mock LLM emits chunked output).
func TestDynclientDynamicStreamParity(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupScenario(t, "test-dynclient-stream", `{"answer": "4"}`)
	httpSchema, libSchema := simpleAnswerSchema()

	httpEvents, httpErrs := BAMLClient.DynamicStream(ctx, testutil.DynamicRequest{
		Messages:       []testutil.DynamicMessage{{Role: "user", Content: "What is 2+2?"}},
		ClientRegistry: opts.ClientRegistry,
		OutputSchema:   httpSchema,
	})
	httpFinal := drainHTTPStreamFinal(t, httpEvents, httpErrs)

	hello := "What is 2+2?"
	client := newDynclient(t)
	stream, err := client.DynamicStream(ctx, dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	})
	if err != nil {
		t.Fatalf("Library DynamicStream: %v", err)
	}
	defer stream.Close()

	libFinal, partials := drainLibStream(t, stream)
	if partials == 0 {
		t.Error("library stream produced no partial events for a chunked scenario")
	}
	if !jsonEqual(t, httpFinal, libFinal) {
		t.Errorf("stream final payload mismatch:\n http=%s\n lib =%s", string(httpFinal), string(libFinal))
	}
}

// TestDynclientDynamicStreamRawParity asserts /stream-with-raw and
// DynamicStreamRaw agree on the final data and the final raw text, and
// that the library accumulates raw across partial events rather than
// emitting just the latest delta.
func TestDynclientDynamicStreamRawParity(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupScenario(t, "test-dynclient-stream-raw", `{"answer": "4"}`)
	httpSchema, libSchema := simpleAnswerSchema()

	httpEvents, httpErrs := BAMLClient.DynamicStreamWithRaw(ctx, testutil.DynamicRequest{
		Messages:       []testutil.DynamicMessage{{Role: "user", Content: "What is 2+2?"}},
		ClientRegistry: opts.ClientRegistry,
		OutputSchema:   httpSchema,
	})
	httpFinalData, httpFinalRaw := drainHTTPStreamFinalRaw(t, httpEvents, httpErrs)

	hello := "What is 2+2?"
	client := newDynclient(t)
	stream, err := client.DynamicStreamRaw(ctx, dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	})
	if err != nil {
		t.Fatalf("Library DynamicStreamRaw: %v", err)
	}
	defer stream.Close()

	var (
		libFinalData stdjson.RawMessage
		libFinalRaw  string
		rawSeen      []string
	)
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("library stream Next: %v", err)
		}
		switch ev.Kind {
		case dynclient.EventPartial:
			rawSeen = append(rawSeen, ev.Raw)
		case dynclient.EventFinal:
			libFinalData = ev.Data
			libFinalRaw = ev.Raw
		}
	}

	if !jsonEqual(t, httpFinalData, libFinalData) {
		t.Errorf("raw-stream final data mismatch:\n http=%s\n lib =%s", string(httpFinalData), string(libFinalData))
	}
	if httpFinalRaw != libFinalRaw {
		t.Errorf("raw-stream final raw mismatch:\n http=%q\n lib =%q", httpFinalRaw, libFinalRaw)
	}
	// Accumulation check: if more than one partial fired, consecutive
	// raw values must be non-decreasing in length (the accumulator grows
	// monotonically until a reset arrives, and this scenario has none).
	for i := 1; i < len(rawSeen); i++ {
		if len(rawSeen[i]) < len(rawSeen[i-1]) {
			t.Errorf("partial raw shrunk between events %d->%d: %q -> %q (expected accumulation)",
				i-1, i, rawSeen[i-1], rawSeen[i])
			break
		}
	}
}

// TestDynclientDynamicParseParity exercises the /parse/_dynamic path and
// asserts the library and HTTP paths produce identical flattened JSON.
func TestDynclientDynamicParseParity(t *testing.T) {
	dynclientGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw := `{"answer": "4"}`
	httpSchema, libSchema := simpleAnswerSchema()

	httpResp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
		Raw:          raw,
		OutputSchema: httpSchema,
	})
	if err != nil {
		t.Fatalf("HTTP DynamicParse: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Fatalf("HTTP DynamicParse status %d: %s", httpResp.StatusCode, httpResp.Error)
	}

	client := newDynclient(t)
	libResp, err := client.DynamicParse(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: libSchema})
	if err != nil {
		t.Fatalf("Library DynamicParse: %v", err)
	}
	if !jsonEqual(t, httpResp.Data, libResp.Data) {
		t.Errorf("parse payload mismatch:\n http=%s\n lib =%s", string(httpResp.Data), string(libResp.Data))
	}
	if strings.Contains(string(libResp.Data), "DynamicProperties") {
		t.Errorf("library parse result still wraps payload: %s", string(libResp.Data))
	}
}

// TestDynclientInvalidRequestValidation pins that the library rejects
// the same shapes the HTTP endpoint rejects, returning a Go error
// rather than a status-coded response envelope but with semantically
// equivalent guidance.
func TestDynclientInvalidRequestValidation(t *testing.T) {
	dynclientGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bogus := testutil.DynamicRequest{
		Messages:       nil,
		ClientRegistry: nil,
		OutputSchema:   nil,
	}
	httpResp, err := BAMLClient.DynamicCall(ctx, bogus)
	if err != nil {
		t.Fatalf("HTTP DynamicCall: %v", err)
	}
	if httpResp.StatusCode != 400 {
		t.Fatalf("HTTP DynamicCall: expected 400, got %d: %s", httpResp.StatusCode, httpResp.Error)
	}

	client := newDynclient(t)
	_, libErr := client.DynamicCall(ctx, dynclient.Request{})
	if libErr == nil {
		t.Fatal("library accepted an empty request; expected validation error")
	}
	if !strings.Contains(libErr.Error(), "messages") {
		t.Errorf("library validation error %q does not mention the failing field 'messages'", libErr.Error())
	}
}

// TestDynclientNestedDynamicTypesUnwrapped mirrors the HTTP
// regression test for nested class+enum schemas to ensure the library
// produces the same flattened representation (no Name/Fields/Value
// wrappers).
func TestDynclientNestedDynamicTypesUnwrapped(t *testing.T) {
	dynclientGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw := `{"name": "John", "location": {"street": "123 Main St", "city": "Boston"}, "status": "ACTIVE"}`
	httpSchema := &testutil.DynamicOutputSchema{
		Classes: map[string]*testutil.DynamicClass{
			"DynclientLocation": {
				Properties: map[string]*testutil.DynamicProperty{
					"street": {Type: "string"},
					"city":   {Type: "string"},
				},
			},
		},
		Enums: map[string]*testutil.DynamicEnum{
			"DynclientStatus": {
				Values: []*testutil.DynamicEnumValue{{Name: "ACTIVE"}, {Name: "INACTIVE"}},
			},
		},
		Properties: map[string]*testutil.DynamicProperty{
			"name":     {Type: "string"},
			"location": {Ref: "DynclientLocation"},
			"status":   {Ref: "DynclientStatus"},
		},
	}
	libSchema := &dynclient.OutputSchema{
		Classes: dynclientFromMap(map[string]*dynclient.Class{
			"DynclientLocation": {
				Properties: dynclientFromMap(map[string]*dynclient.Property{
					"street": {Type: "string"},
					"city":   {Type: "string"},
				}),
			},
		}),
		Enums: dynclientFromMap(map[string]*dynclient.Enum{
			"DynclientStatus": {
				Values: []*dynclient.EnumValue{{Name: "ACTIVE"}, {Name: "INACTIVE"}},
			},
		}),
		Properties: dynclientFromMap(map[string]*dynclient.Property{
			"name":     {Type: "string"},
			"location": {Ref: "DynclientLocation"},
			"status":   {Ref: "DynclientStatus"},
		}),
	}

	httpResp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{Raw: raw, OutputSchema: httpSchema})
	if err != nil {
		t.Fatalf("HTTP DynamicParse: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Fatalf("HTTP DynamicParse status %d: %s", httpResp.StatusCode, httpResp.Error)
	}

	client := newDynclient(t)
	libResp, err := client.DynamicParse(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: libSchema})
	if err != nil {
		t.Fatalf("Library DynamicParse: %v", err)
	}

	if !jsonEqual(t, httpResp.Data, libResp.Data) {
		t.Errorf("nested parse mismatch:\n http=%s\n lib =%s", string(httpResp.Data), string(libResp.Data))
	}
	body := string(libResp.Data)
	for _, leak := range []string{`"Name":`, `"Fields":`, `"Value":`, `"DynamicProperties":`} {
		if strings.Contains(body, leak) {
			t.Errorf("library payload leaks internal wrapper %s: %s", leak, body)
		}
	}
}

// TestDynclientBuildRequestOptionsSmoke verifies that the WithUseBuildRequest
// option threads through to the worker handler — the call must succeed
// against the mock LLM with the option enabled. The HTTP path's
// equivalent is gated on env, so this test only validates that the
// option doesn't break the happy path.
func TestDynclientBuildRequestOptionsSmoke(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupNonStreamingScenario(t, "test-dynclient-buildrequest", `{"answer": "4"}`)
	_, libSchema := simpleAnswerSchema()

	hello := "What is 2+2?"
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}

	client := newDynclient(t,
		dynclient.WithUseBuildRequest(ActuallyBuildRequest()),
	)
	libResp, err := client.DynamicCall(ctx, libReq)
	if err != nil {
		t.Fatalf("Library DynamicCall with build-request option: %v", err)
	}
	var decoded map[string]any
	if err := sonic.Unmarshal(libResp.Data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["answer"] != "4" {
		t.Errorf("answer = %v, want %q", decoded["answer"], "4")
	}
}

// TestDynclientSharedStateRoundRobinSmoke is a thin sanity check that
// a SharedStateStore-equipped client can issue back-to-back dynamic
// calls without tripping the missing-request-id warning path or leaking
// scope entries. It does not assert RR distribution — the full RR
// behaviour is exercised by the existing roundrobin_test.go suite.
func TestDynclientSharedStateRoundRobinSmoke(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupNonStreamingScenario(t, "test-dynclient-sharedstate", `{"answer": "4"}`)
	_, libSchema := simpleAnswerSchema()

	store := workerplugin.NewSharedStateStore(func(string) uint64 { return 0 })
	t.Cleanup(store.Close)

	client := newDynclient(t, dynclient.WithSharedStateStore(store))

	hello := "What is 2+2?"
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}

	for i := 0; i < 2; i++ {
		resp, err := client.DynamicCall(ctx, libReq)
		if err != nil {
			t.Fatalf("DynamicCall #%d: %v", i, err)
		}
		if len(resp.Data) == 0 {
			t.Fatalf("DynamicCall #%d returned empty data", i)
		}
	}
}

// drainHTTPStreamFinal drains an SSE stream from BAMLClient.DynamicStream
// and returns the bytes of the final event's Data field. Errors from the
// stream are reported via t.Fatalf.
func drainHTTPStreamFinal(t *testing.T, events <-chan testutil.StreamEvent, errs <-chan error) []byte {
	t.Helper()
	var final stdjson.RawMessage
	for ev := range events {
		if ev.IsFinal() || (ev.IsData() && final == nil) {
			final = ev.Data
		}
		if ev.IsFinal() {
			break
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("HTTP stream error: %v", err)
	}
	if final == nil {
		t.Fatal("HTTP stream produced no data events")
	}
	return final
}

// drainHTTPStreamFinalRaw is the with-raw counterpart of
// drainHTTPStreamFinal. Returns the final data bytes and the final raw
// upstream text.
func drainHTTPStreamFinalRaw(t *testing.T, events <-chan testutil.StreamEvent, errs <-chan error) ([]byte, string) {
	t.Helper()
	var (
		finalData stdjson.RawMessage
		finalRaw  string
	)
	for ev := range events {
		if ev.IsFinal() {
			finalData = ev.Data
			finalRaw = ev.Raw
			break
		}
		if ev.IsData() {
			finalData = ev.Data
			finalRaw = ev.Raw
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("HTTP stream error: %v", err)
	}
	if finalData == nil {
		t.Fatal("HTTP stream produced no data events")
	}
	return finalData, finalRaw
}

// drainLibStream consumes a dynclient.Stream to io.EOF and returns the
// last final-event data along with the number of partial events seen.
func drainLibStream(t *testing.T, s *dynclient.Stream) (stdjson.RawMessage, int) {
	t.Helper()
	var (
		final    stdjson.RawMessage
		partials int
	)
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Library stream Next: %v", err)
		}
		switch ev.Kind {
		case dynclient.EventPartial:
			partials++
		case dynclient.EventFinal:
			final = ev.Data
		}
	}
	if final == nil {
		t.Fatal("Library stream produced no final event")
	}
	return final, partials
}
