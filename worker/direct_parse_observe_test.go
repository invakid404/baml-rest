package worker

// De-BAML serving cutover S1 — the direct-parse observation seam, from the worker
// side.
//
// `/parse/{method}` is the one cutover surface that never reaches native admission,
// so the parse route reports each request to an optional observer and a
// native-capable worker turns that into the surface's telemetry. These tests pin the
// two properties the seam lives or dies by: it is CALLED for real requests, and it
// can never affect the parse it observes.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func parseOKRuntime(called *bool) *fakeRuntime {
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, raw string) (any, error) {
			if called != nil {
				*called = true
			}
			return map[string]any{"got": raw}, nil
		},
	}
	return &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{"parse-ok": method}}
}

// TestParseReportsToTheDirectParseObserver proves the surface produces a real
// per-request observation — once per request, carrying the parse shape and nothing
// else — and that BAML still parses the request.
func TestParseReportsToTheDirectParseObserver(t *testing.T) {
	t.Parallel()

	var observed atomic.Int64
	var lastStream atomic.Bool
	parsed := false
	h := newTestHandler(t, Config{
		Runtime: parseOKRuntime(&parsed),
		NativeDirectParseObserver: func(_ context.Context, obs bamlutils.NativeDirectParseObservation) {
			observed.Add(1)
			lastStream.Store(obs.Stream)
		},
	})

	res, err := h.Parse(context.Background(), "parse-ok", []byte(`{"raw":"hello"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := observed.Load(); got != 1 {
		t.Fatalf("observer called %d times, want exactly 1", got)
	}
	if lastStream.Load() {
		t.Error("a final parse was reported as a stream parse")
	}
	if !parsed {
		t.Error("BAML's parse implementation did not run")
	}
	if !strings.Contains(string(res.Data), "hello") {
		t.Errorf("parse payload = %s, want BAML's own result", string(res.Data))
	}
}

// TestParseWithoutAnObserverIsUnchanged is the flag-off / default-build control: no
// observer is installed, the route calls nothing, and the parse behaves exactly as
// before. This is the zero-native-observation property the other four lanes have when
// the umbrella flag is off.
func TestParseWithoutAnObserverIsUnchanged(t *testing.T) {
	t.Parallel()

	parsed := false
	h := newTestHandler(t, Config{Runtime: parseOKRuntime(&parsed)})
	res, err := h.Parse(context.Background(), "parse-ok", []byte(`{"raw":"hello"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed || !strings.Contains(string(res.Data), "hello") {
		t.Fatalf("the observer-free parse changed: parsed=%v data=%s", parsed, string(res.Data))
	}
}

// TestPanickingDirectParseObserverCannotFailTheParse is the seam's safety contract.
// The observer is advisory; a bug in a telemetry sink must never turn a working parse
// into a failed request, so the route contains the panic and serves BAML's result.
func TestPanickingDirectParseObserverCannotFailTheParse(t *testing.T) {
	t.Parallel()

	parsed := false
	h := newTestHandler(t, Config{
		Runtime: parseOKRuntime(&parsed),
		NativeDirectParseObserver: func(context.Context, bamlutils.NativeDirectParseObservation) {
			panic("de-BAML test: observer blew up")
		},
	})

	res, err := h.Parse(context.Background(), "parse-ok", []byte(`{"raw":"hello"}`))
	if err != nil {
		t.Fatalf("a panicking observer failed the parse: %v", err)
	}
	if !parsed {
		t.Error("BAML's parse implementation did not run after the observer panicked")
	}
	if !strings.Contains(string(res.Data), "hello") {
		t.Errorf("parse payload = %s, want BAML's own result", string(res.Data))
	}
}

// TestDirectParseObserverSeesTheStreamShape pins the one bounded fact the observation
// carries, on the parse-stream path.
func TestDirectParseObserverSeesTheStreamShape(t *testing.T) {
	t.Parallel()

	var sawStream atomic.Bool
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &map[string]any{} },
		Impl:       func(bamlutils.Adapter, string) (any, error) { return map[string]any{}, nil },
		StreamImpl: func(bamlutils.Adapter, string) (any, error) { return map[string]any{"partial": true}, nil },
	}
	h := newTestHandler(t, Config{
		Runtime: &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{"parse-ok": method}},
		NativeDirectParseObserver: func(_ context.Context, obs bamlutils.NativeDirectParseObservation) {
			sawStream.Store(obs.Stream)
		},
	})

	if _, err := h.Parse(context.Background(), "parse-ok", []byte(`{"raw":"hello","stream":true}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !sawStream.Load() {
		t.Fatal("a parse-stream request was not reported as one")
	}
}
