//go:build inprocess

package pool

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// panicWorker implements workerplugin.Worker by panicking on every
// method. The panic value is distinct per method so tests can assert
// the recover wrapper propagated the correct value into the error
// envelope.
type panicWorker struct{}

func (panicWorker) CallStream(context.Context, string, []byte, bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	panic(errors.New("stream boom"))
}

func (panicWorker) Health(context.Context) (bool, error) {
	panic(errors.New("health boom"))
}

func (panicWorker) GetMetrics(context.Context) ([][]byte, error) {
	panic(errors.New("metrics boom"))
}

func (panicWorker) TriggerGC(context.Context) (*workerplugin.GCResult, error) {
	panic(errors.New("gc boom"))
}

func (panicWorker) Parse(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
	panic(errors.New("parse boom"))
}

func (panicWorker) GetGoroutines(context.Context, string) (*workerplugin.GoroutinesResult, error) {
	panic(errors.New("goroutines boom"))
}

func newRecoveringPanicWorker() workerplugin.Worker {
	return newRecoveringWorker(panicWorker{}, zerolog.Nop())
}

func assertErrorWithStack(t *testing.T, err error, wantSubstr string) *workerplugin.ErrorWithStack {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ews *workerplugin.ErrorWithStack
	if !errors.As(err, &ews) {
		t.Fatalf("expected *workerplugin.ErrorWithStack, got %T: %v", err, err)
	}
	if ews.GetCode() != panicErrorCode {
		t.Errorf("ErrorCode = %q, want %q", ews.GetCode(), panicErrorCode)
	}
	if ews.GetStacktrace() == "" {
		t.Error("expected non-empty stacktrace")
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q missing substring %q", err.Error(), wantSubstr)
	}
	return ews
}

func TestRecoveringWorkerCallStreamPanicYieldsErrorFrame(t *testing.T) {
	w := newRecoveringPanicWorker()
	ch, err := w.CallStream(context.Background(), "Foo", []byte(`{}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream returned method error %v; expected nil so retry policies do not see a transport failure", err)
	}
	if ch == nil {
		t.Fatal("CallStream returned nil channel")
	}
	frame, ok := <-ch
	if !ok {
		t.Fatal("channel closed without yielding error frame")
	}
	if frame.Kind != workerplugin.StreamResultKindError {
		t.Errorf("frame.Kind = %v, want StreamResultKindError", frame.Kind)
	}
	if frame.ErrorCode != panicErrorCode {
		t.Errorf("frame.ErrorCode = %q, want %q", frame.ErrorCode, panicErrorCode)
	}
	if frame.Stacktrace == "" {
		t.Error("expected non-empty stacktrace on error frame")
	}
	if frame.Error == nil || !strings.Contains(frame.Error.Error(), "stream boom") {
		t.Errorf("frame.Error = %v, want substring %q", frame.Error, "stream boom")
	}
	if _, more := <-ch; more {
		t.Error("expected channel to be closed after terminal error frame")
	}
}

func TestRecoveringWorkerParsePanicYieldsStructuredError(t *testing.T) {
	w := newRecoveringPanicWorker()
	res, err := w.Parse(context.Background(), "Foo", []byte(`{}`))
	if res != nil {
		t.Errorf("Parse returned non-nil result on panic: %+v", res)
	}
	assertErrorWithStack(t, err, "parse boom")
}

func TestRecoveringWorkerAdminMethodsRecoverPanics(t *testing.T) {
	tests := []struct {
		name string
		want string
		call func(workerplugin.Worker) error
	}{
		{
			name: "Health",
			want: "health boom",
			call: func(w workerplugin.Worker) error {
				ok, err := w.Health(context.Background())
				if ok {
					t.Errorf("Health returned ok=true on panic")
				}
				return err
			},
		},
		{
			name: "GetMetrics",
			want: "metrics boom",
			call: func(w workerplugin.Worker) error {
				res, err := w.GetMetrics(context.Background())
				if res != nil {
					t.Errorf("GetMetrics returned %v, want nil", res)
				}
				return err
			},
		},
		{
			name: "TriggerGC",
			want: "gc boom",
			call: func(w workerplugin.Worker) error {
				res, err := w.TriggerGC(context.Background())
				if res != nil {
					t.Errorf("TriggerGC returned %+v, want nil", res)
				}
				return err
			},
		},
		{
			name: "GetGoroutines",
			want: "goroutines boom",
			call: func(w workerplugin.Worker) error {
				res, err := w.GetGoroutines(context.Background(), "")
				if res != nil {
					t.Errorf("GetGoroutines returned %+v, want nil", res)
				}
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newRecoveringPanicWorker()
			err := tc.call(w)
			assertErrorWithStack(t, err, tc.want)
		})
	}
}

// TestInProcessPoolWrapsFactoryWorkerWithRecover constructs a real
// inprocess pool whose factory hands back a panicking worker, then
// drives Parse through the pool to confirm the recover wrapper is
// installed on the factory path (not just exercised in isolation).
// Without the wrapper, p.Parse would propagate the goroutine panic
// and crash the test binary.
func TestInProcessPoolWrapsFactoryWorkerWithRecover(t *testing.T) {
	var called atomic.Bool

	factory := func(WorkerFactoryConfig) (workerplugin.Worker, error) {
		called.Store(true)
		return panicWorker{}, nil
	}

	cfg := DefaultConfig()
	cfg.WorkerFactory = factory
	cfg.HealthCheckInterval = 0

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer p.Close()

	if !called.Load() {
		t.Fatal("expected WorkerFactory to be invoked during pool fill")
	}

	res, err := p.Parse(context.Background(), "Foo", []byte(`{}`))
	if res != nil {
		t.Errorf("Parse returned %+v, want nil", res)
	}
	if err == nil {
		t.Fatal("expected Parse to return an error rather than panic")
	}
	if !strings.Contains(err.Error(), "parse boom") {
		t.Errorf("Parse error %q missing substring %q", err.Error(), "parse boom")
	}
}
