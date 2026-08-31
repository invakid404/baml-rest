package nativespinefixture

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/worker"
	"github.com/invakid404/baml-rest/workerplugin"
)

// The fixture runtime satisfies the production worker.Runtime contract.
var _ worker.Runtime = (*NativeRuntime)(nil)

// The fake executor satisfies the neutral executor contract the emitted BuildMethod
// drives.
var _ bamlutils.NativeSpineUnaryExecutor = (*fakeExecutor)(nil)

// fakeExecutor is the neutral injected executor for the fixture — no BAML
// request/parse, no socket. It records what it was called with and returns canned
// tri-state results (a canned callErr surfaces as a terminal failed-after-claim,
// which the worker bridge frames as an error either way).
type fakeExecutor struct {
	callInput   any
	callResult  any
	callErr     error
	parseRaw    string
	parseResult any
	parseErr    error
	callCount   int
	parseCount  int
}

func (e *fakeExecutor) Call(_ context.Context, _ string, input any) bamlutils.NativeSpineUnaryResult {
	e.callCount++
	e.callInput = input
	if e.callErr != nil {
		return bamlutils.FailedAfterClaimSpineResult(e.callErr, "test", "call_err")
	}
	return bamlutils.SucceededSpineResult(e.callResult)
}

func (e *fakeExecutor) Parse(_ context.Context, _ string, raw string) (any, error) {
	e.parseCount++
	e.parseRaw = raw
	if e.parseErr != nil {
		return nil, e.parseErr
	}
	return e.parseResult, nil
}

func drain(ch <-chan *workerplugin.StreamResult) []*workerplugin.StreamResult {
	var out []*workerplugin.StreamResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// TestNativeRuntimeWorkerContract drives the full worker.Runtime contract through
// worker.Handler with fake executor + adapter — no socket, no pool, no parser
// ownership: boot, registration, unknown-method error, typed JSON input, unary
// executor invocation, output envelope, error envelope, stream-mode decline, and
// final-parse registration/invocation.
func TestNativeRuntimeWorkerContract(t *testing.T) {
	exec := &fakeExecutor{}
	rt := NewNativeRuntime(exec)

	// Boot: pure-Go init, no shared library, no panic.
	rt.InitRuntime()

	// Registration.
	if _, ok := rt.Method(MethodName); !ok {
		t.Fatalf("method %q not registered", MethodName)
	}
	if _, ok := rt.Method("Nope"); ok {
		t.Fatal("unknown method unexpectedly registered")
	}
	if _, ok := rt.ParseMethod(MethodName); !ok {
		t.Fatalf("parse method %q not registered", MethodName)
	}

	h, err := worker.New(worker.Config{Runtime: rt})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	ctx := context.Background()

	// Unknown-method error — the handler's exact contract string.
	if _, err := h.CallStream(ctx, "Nope", []byte(`{}`), bamlutils.StreamModeCall); err == nil ||
		!strings.Contains(err.Error(), `method "Nope" not found`) {
		t.Fatalf("unknown-method error = %v, want contains 'method \"Nope\" not found'", err)
	}

	// Unary call: typed JSON input creation + executor invocation + output envelope.
	exec.callResult = &OutputGreeting{Text: "hi bob", Formal: true}
	ch, err := h.CallStream(ctx, MethodName, []byte(`{"name":"bob","formal":true}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream(call): %v", err)
	}
	results := drain(ch)
	if len(results) != 1 {
		t.Fatalf("want 1 stream result, got %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("unexpected error frame: %v", results[0].Error)
	}
	var gotOut OutputGreeting
	if err := json.Unmarshal(results[0].Data, &gotOut); err != nil {
		t.Fatalf("output envelope not JSON: %v (%s)", err, results[0].Data)
	}
	if gotOut != (OutputGreeting{Text: "hi bob", Formal: true}) {
		t.Fatalf("output envelope = %+v", gotOut)
	}
	in, ok := exec.callInput.(*GreetInput)
	if !ok {
		t.Fatalf("executor input type = %T, want *GreetInput (typed input creation)", exec.callInput)
	}
	if in.Name != "bob" || !in.Formal {
		t.Fatalf("typed input = %+v, want {Name:bob Formal:true}", in)
	}
	if exec.callCount != 1 {
		t.Fatalf("executor Call count = %d, want 1", exec.callCount)
	}

	// Error envelope: an executor error becomes an error frame the bridge carries.
	exec.callErr = errors.New("provider boom")
	ch, err = h.CallStream(ctx, MethodName, []byte(`{"name":"x","formal":false}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream(error case) returned dispatch error: %v", err)
	}
	results = drain(ch)
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("want one error frame, got %+v", results)
	}
	if !strings.Contains(results[0].Error.Error(), "provider boom") {
		t.Fatalf("error envelope = %v", results[0].Error)
	}
	exec.callErr = nil

	// Stream-mode decline: only unary final-call is admitted.
	if _, err := h.CallStream(ctx, MethodName, []byte(`{"name":"x","formal":false}`), bamlutils.StreamModeStream); err == nil ||
		!strings.Contains(err.Error(), "unary final-call") {
		t.Fatalf("stream-mode decline error = %v, want the stable unsupported-mode error", err)
	}

	// Final-parse registration + invocation + output envelope.
	exec.parseResult = &OutputGreeting{Text: "parsed", Formal: false}
	pres, err := h.Parse(ctx, MethodName, []byte(`{"raw":"some raw text"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var pOut OutputGreeting
	if err := json.Unmarshal(pres.Data, &pOut); err != nil {
		t.Fatalf("parse output not JSON: %v (%s)", err, pres.Data)
	}
	if pOut.Text != "parsed" {
		t.Fatalf("parse output = %+v", pOut)
	}
	if exec.parseRaw != "some raw text" {
		t.Fatalf("executor Parse raw = %q, want 'some raw text'", exec.parseRaw)
	}
	if exec.parseCount != 1 {
		t.Fatalf("executor Parse count = %d, want 1", exec.parseCount)
	}
}
