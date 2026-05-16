//go:build inprocess

package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/workerplugin"
)

// recoverTestLogger captures error-level messages so tests can assert
// the bridge-panic helper logged before publishing the terminal frame.
type recoverTestLogger struct {
	mu     sync.Mutex
	errors []string
}

func (l *recoverTestLogger) Debug(string, ...interface{}) {}
func (l *recoverTestLogger) Info(string, ...interface{})  {}
func (l *recoverTestLogger) Warn(string, ...interface{})  {}
func (l *recoverTestLogger) Error(msg string, _ ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}
func (l *recoverTestLogger) errorMessages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]string, len(l.errors))
	copy(cp, l.errors)
	return cp
}

func TestRecoverBridgePanicPublishesTerminalFrame(t *testing.T) {
	logger := &recoverTestLogger{}
	out := make(chan *workerplugin.StreamResult, 1)
	done := make(chan struct{})

	go func() {
		// Mirror bridgeStreamResults' defer order: close last, recover first.
		defer close(done)
		defer close(out)
		defer recoverBridgePanic(context.Background(), out, logger)
		panic("synthetic bridge panic")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not return after panic")
	}

	frame, ok := <-out
	if !ok {
		t.Fatal("expected one terminal error frame before channel close")
	}
	if frame.Kind != workerplugin.StreamResultKindError {
		t.Errorf("frame.Kind = %v, want StreamResultKindError", frame.Kind)
	}
	if frame.ErrorCode != streamPanicErrorCode {
		t.Errorf("frame.ErrorCode = %q, want %q", frame.ErrorCode, streamPanicErrorCode)
	}
	if frame.Stacktrace == "" {
		t.Error("expected non-empty stacktrace on terminal error frame")
	}
	if frame.Error == nil || !strings.Contains(frame.Error.Error(), "synthetic bridge panic") {
		t.Errorf("frame.Error = %v, want substring %q", frame.Error, "synthetic bridge panic")
	}

	if _, more := <-out; more {
		t.Error("expected channel to be closed after terminal frame")
	}

	msgs := logger.errorMessages()
	if len(msgs) == 0 {
		t.Error("expected an error-level log entry for the recovered panic")
	}
}

// TestRecoverBridgePanicDropsFrameWhenContextCancelled verifies the
// helper skips sending when the caller has already gone away. The
// alternative — blocking on an unbuffered channel whose consumer has
// stopped reading — would pin the bridge goroutine indefinitely.
func TestRecoverBridgePanicDropsFrameWhenContextCancelled(t *testing.T) {
	logger := &recoverTestLogger{}
	out := make(chan *workerplugin.StreamResult) // unbuffered: would block if helper sends
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	go func() {
		defer close(done)
		defer close(out)
		defer recoverBridgePanic(ctx, out, logger)
		panic("synthetic post-cancel panic")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not return after panic — helper likely blocked on send")
	}

	if frame, ok := <-out; ok {
		t.Errorf("expected channel closed with no frame; got %+v", frame)
	}

	if len(logger.errorMessages()) == 0 {
		t.Error("expected log entry even when frame is dropped")
	}
}

func TestRecoverBridgePanicNoOpWhenNoPanic(t *testing.T) {
	logger := &recoverTestLogger{}
	out := make(chan *workerplugin.StreamResult, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(out)
		defer recoverBridgePanic(context.Background(), out, logger)
		// no panic
	}()

	<-done
	if frame, ok := <-out; ok {
		t.Errorf("expected no frame on clean exit; got %+v", frame)
	}
	if msgs := logger.errorMessages(); len(msgs) != 0 {
		t.Errorf("expected no error logs on clean exit, got %v", msgs)
	}
}
