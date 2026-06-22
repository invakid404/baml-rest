package sseclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// collectViaStream drains the channel API into a slice of owned Events.
func collectViaStream(t *testing.T, body string) ([]Event, error) {
	t.Helper()
	events, errc := Stream(context.Background(), strings.NewReader(body))
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	return got, <-errc
}

// collectViaScanEvents drains the synchronous callback API into a slice of
// owned Events. Because EventBytes views are only valid during the callback,
// each field is copied out with string(...) — exactly what a real consumer
// that needs to retain data must do.
func collectViaScanEvents(t *testing.T, body string) ([]Event, error) {
	t.Helper()
	var got []Event
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev EventBytes) error {
		got = append(got, Event{
			Type: string(ev.Type),
			Data: string(ev.Data),
			ID:   string(ev.ID),
		})
		return nil
	})
	return got, err
}

// sseParityFixtures exercises the SSE features both parsers must handle
// identically: default/typed events, ids, multi-line data joined with \n,
// comment/keepalive lines, CRLF line endings, a trailing event with no final
// blank line, and the [DONE] sentinel.
var sseParityFixtures = map[string]string{
	"single_event":        "data: hello\n\n",
	"multi_event":         "data: a\n\ndata: b\n\ndata: c\n\n",
	"typed_event":         "event: content_block_delta\ndata: {\"x\":1}\n\n",
	"with_id":             "id: 42\ndata: payload\n\n",
	"multiline_data":      "data: line1\ndata: line2\ndata: line3\n\n",
	"comment_keepalive":   ": keepalive\ndata: real\n\n",
	"crlf_endings":        "event: e\r\ndata: v\r\n\r\n",
	"trailing_no_blank":   "data: last",
	"done_sentinel":       "data: {\"a\":1}\n\ndata: [DONE]\n\n",
	"empty_data":          "data:\n\n",
	"id_with_null_skip":   "id: a\x00b\ndata: x\n\n",
	"event_last_wins":     "event: first\nevent: second\ndata: x\n\n",
	"interleaved_fields":  "event: t\nid: 7\ndata: d1\ndata: d2\n\n",
	"blank_lines_between": "\n\ndata: x\n\n\n\ndata: y\n\n",
}

func TestScanEventsMatchesStream(t *testing.T) {
	for name, body := range sseParityFixtures {
		t.Run(name, func(t *testing.T) {
			streamEvents, streamErr := collectViaStream(t, body)
			scanEvents, scanErr := collectViaScanEvents(t, body)

			if (streamErr == nil) != (scanErr == nil) {
				t.Fatalf("terminal error mismatch: stream=%v scan=%v", streamErr, scanErr)
			}
			if len(streamEvents) != len(scanEvents) {
				t.Fatalf("event count mismatch: stream=%d scan=%d\nstream=%#v\nscan=%#v",
					len(streamEvents), len(scanEvents), streamEvents, scanEvents)
			}
			for i := range streamEvents {
				if streamEvents[i] != scanEvents[i] {
					t.Errorf("event %d mismatch:\n stream=%#v\n   scan=%#v", i, streamEvents[i], scanEvents[i])
				}
			}
		})
	}
}

// TestScanEventsErrStopScan verifies that returning ErrStopScan halts parsing
// cleanly: ScanEvents returns nil and no further events are delivered.
func TestScanEventsErrStopScan(t *testing.T) {
	body := "data: a\n\ndata: b\n\ndata: c\n\n"
	var seen []string
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev EventBytes) error {
		seen = append(seen, string(ev.Data))
		if string(ev.Data) == "b" {
			return ErrStopScan
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ErrStopScan should yield nil, got %v", err)
	}
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("expected to stop after [a b], got %v", seen)
	}
}

// TestScanEventsCallbackErrorPropagates verifies a non-ErrStopScan error from
// the callback is returned verbatim.
func TestScanEventsCallbackErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	err := ScanEvents(context.Background(), strings.NewReader("data: a\n\ndata: b\n\n"), func(ev EventBytes) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestScanEventsContextCancellation verifies ctx cancellation surfaces
// ctx.Err() — matching the channel path's between-line cancellation check.
func TestScanEventsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A multi-line body so the loop has a chance to observe Done between lines.
	err := ScanEvents(ctx, strings.NewReader("data: a\n\ndata: b\n\n"), func(ev EventBytes) error {
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestScanEventsBufferReuse proves the parser reuses one backing array for
// Data across events (no per-event allocation) and that the bytes handed to a
// PRIOR callback are overwritten by a LATER event — the exact reason the
// callback must consume them synchronously and copy out anything it retains.
func TestScanEventsBufferReuse(t *testing.T) {
	body := "data: AAAA\n\ndata: BBBB\n\n"

	var (
		firstCap      int
		secondCap     int
		retainedView  []byte // deliberately retained (unsafe in real code)
		copiedOut     string
		afterRetained string
	)
	idx := 0
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev EventBytes) error {
		switch idx {
		case 0:
			firstCap = cap(ev.Data)
			retainedView = ev.Data // capture the slice header (aliases scratch buf)
			copiedOut = string(ev.Data)
		case 1:
			secondCap = cap(ev.Data)
			// The retained view now observes the second event's bytes,
			// because the scratch buffer was reused. This is the hazard the
			// design avoids by consuming synchronously.
			afterRetained = string(retainedView)
		}
		idx++
		return nil
	})
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if firstCap == 0 || secondCap == 0 {
		t.Fatalf("expected non-zero caps, got %d %d", firstCap, secondCap)
	}
	// Same backing array reused → identical capacity, no growth for equal-size payloads.
	if firstCap != secondCap {
		t.Errorf("expected reused backing array (equal cap), got %d then %d", firstCap, secondCap)
	}
	if copiedOut != "AAAA" {
		t.Errorf("copied-out first event = %q, want AAAA", copiedOut)
	}
	// The retained []byte view was overwritten by event 2 — demonstrating why
	// copying out (copiedOut) is mandatory and a retained view is not safe.
	if afterRetained != "BBBB" {
		t.Errorf("retained view after second event = %q, want BBBB (proves reuse)", afterRetained)
	}
}

// TestScanEventsReaderError surfaces a mid-stream reader error verbatim
// (parity with the channel path's scanner.Err()).
func TestScanEventsReaderError(t *testing.T) {
	boom := errors.New("read failure")
	r := io.MultiReader(strings.NewReader("data: a\n\n"), &errReader{err: boom})
	var seen int
	err := ScanEvents(context.Background(), r, func(ev EventBytes) error {
		seen++
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected reader error, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected one delivered event before error, got %d", seen)
	}
}

type errReader struct{ err error }

func (e *errReader) Read(p []byte) (int, error) { return 0, e.err }

// cancelOnEOFReader delivers data once, then cancels the context on the
// follow-up Read that the scanner makes to detect EOF, before returning EOF.
// This deterministically reproduces the dispatch-time cancellation gap: the
// cancel lands during the Scan() that returns false (so the loop exits via the
// for-condition and the loop-body's top-of-iteration ctx check never runs),
// leaving the post-loop trailing-event dispatch as the only thing that could
// observe the cancellation.
type cancelOnEOFReader struct {
	data   []byte
	off    int
	cancel context.CancelFunc
	done   bool
}

func (r *cancelOnEOFReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	if !r.done {
		r.done = true
		r.cancel()
	}
	return 0, io.EOF
}

// TestScanEventsCtxCheckedBeforeTrailingDispatch is the regression test for the
// cubic P2 on PR #493: a context cancelled at end-of-stream must NOT dispatch
// the trailing event. The data line "data: b\n" sets hasData, then the EOF
// Read cancels the context; without the dispatch-time ctx check the trailing
// emit would still invoke the callback for "b" and ScanEvents would return nil.
// With the fix it returns context.Canceled and the callback is never entered.
func TestScanEventsCtxCheckedBeforeTrailingDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelOnEOFReader{data: []byte("data: b\n"), cancel: cancel}

	var dispatched []string
	err := ScanEvents(ctx, r, func(ev EventBytes) error {
		dispatched = append(dispatched, string(ev.Data))
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(dispatched) != 0 {
		t.Fatalf("trailing event dispatched after cancel: %v", dispatched)
	}
}

// TestScanEventsNoDispatchAfterCallbackCancel verifies that when the callback
// cancels the context mid-stream, no further event is dispatched and
// ScanEvents returns the ctx error. Deterministic: all bytes are available up
// front, so the parser would otherwise run straight through every event.
func TestScanEventsNoDispatchAfterCallbackCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := "data: a\n\ndata: b\n\ndata: c\n\n"

	var dispatched []string
	err := ScanEvents(ctx, strings.NewReader(body), func(ev EventBytes) error {
		dispatched = append(dispatched, string(ev.Data))
		if string(ev.Data) == "a" {
			cancel() // cancel mid-stream after the first event
		}
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(dispatched) != 1 || dispatched[0] != "a" {
		t.Fatalf("expected only [a] dispatched before cancel, got %v", dispatched)
	}
}

// BenchmarkScanEventsParallel mirrors BenchmarkStreamParallel for the
// synchronous callback path. The callback parses nothing durable — it only
// touches the bytes — so the steady-state per-event allocation that Stream
// pays for Event.Data should not appear here.
func BenchmarkScanEventsParallel(b *testing.B) {
	smallPayload := strings.Repeat("x", 128)
	body := buildSSEStream(1000, smallPayload)
	b.SetParallelism(16)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var sink int
		for pb.Next() {
			err := ScanEvents(context.Background(), bytes.NewReader(body), func(ev EventBytes) error {
				sink += len(ev.Data) // touch the bytes without retaining them
				return nil
			})
			if err != nil {
				b.Fatalf("scan error: %v", err)
			}
		}
		_ = sink
	})
}
