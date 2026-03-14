package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

type fakeStreamResult struct {
	kind     bamlutils.StreamResultKind
	stream   any
	final    any
	err      error
	raw      string
	reset    bool
	release  sync.Once
	released chan struct{}
}

func newFakeStreamResult(kind bamlutils.StreamResultKind) *fakeStreamResult {
	return &fakeStreamResult{
		kind:     kind,
		released: make(chan struct{}),
	}
}

func (r *fakeStreamResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *fakeStreamResult) Stream() any                      { return r.stream }
func (r *fakeStreamResult) Final() any                       { return r.final }
func (r *fakeStreamResult) Error() error                     { return r.err }
func (r *fakeStreamResult) Raw() string                      { return r.raw }
func (r *fakeStreamResult) Reset() bool                      { return r.reset }
func (r *fakeStreamResult) Release() {
	r.release.Do(func() {
		close(r.released)
	})
}

func TestBridgeStreamResultsCancelsWhileUpstreamBlocked(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan bamlutils.StreamResult)
	out := bridgeStreamResults(ctx, in)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}
}

func TestBridgeStreamResultsForwardsFinalResult(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindFinal)
	fake.final = map[string]string{"message": "done"}
	fake.raw = "raw-output"
	fake.reset = true
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in)

	select {
	case <-fake.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected upstream result to be released")
	}

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindFinal {
			t.Fatalf("expected final result kind, got %v", got.Kind)
		}
		if string(got.Data) != `{"message":"done"}` {
			t.Fatalf("unexpected bridged payload: %s", got.Data)
		}
		if got.Raw != "raw-output" {
			t.Fatalf("unexpected raw output: %q", got.Raw)
		}
		if !got.Reset {
			t.Fatal("expected reset flag to propagate")
		}
		if got.Error != nil {
			t.Fatalf("unexpected bridged error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged result")
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after upstream closes")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged output channel to close")
	}
}

func TestBridgeStreamResultsForwardsStreamResult(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	fake.stream = map[string]string{"delta": "hi"}
	fake.raw = "partial-raw"
	fake.reset = true
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged stream result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindStream {
			t.Fatalf("expected stream result kind, got %v", got.Kind)
		}
		if string(got.Data) != `{"delta":"hi"}` {
			t.Fatalf("unexpected bridged payload: %s", got.Data)
		}
		if got.Raw != "partial-raw" {
			t.Fatalf("unexpected raw output: %q", got.Raw)
		}
		if !got.Reset {
			t.Fatal("expected reset flag to propagate")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged stream result")
	}
}

func TestBridgeStreamResultsForwardsHeartbeat(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindHeartbeat)
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged heartbeat result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindHeartbeat {
			t.Fatalf("expected heartbeat result kind, got %v", got.Kind)
		}
		if len(got.Data) != 0 {
			t.Fatalf("expected no heartbeat payload, got %q", string(got.Data))
		}
		if got.Error != nil {
			t.Fatalf("unexpected heartbeat error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged heartbeat result")
	}
}

func TestBridgeStreamResultsPropagatesErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindError)
	fake.err = errors.New("boom")
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged error result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindError {
			t.Fatalf("expected error result kind, got %v", got.Kind)
		}
		if got.Error == nil || got.Error.Error() != "boom" {
			t.Fatalf("unexpected bridged error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged error result")
	}
}

func TestBridgeStreamResultsCancelsDuringDownstreamSend(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	fake.stream = map[string]string{"delta": "hi"}
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in)

	select {
	case <-fake.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected upstream result to be released before downstream send")
	}

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}
}
