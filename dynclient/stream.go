package dynclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// errUninitializedStream is returned by Stream.Next when called on a
// zero-value or partially-initialised Stream. Distinct from errNilClient
// because the failure mode is structural (missing channel/cancel), not
// a nil *Client receiver.
var errUninitializedStream = errors.New("dynclient: uninitialized Stream")

// EventKind identifies the variant of a streaming event delivered by
// Stream.Next.
type EventKind string

const (
	// EventPartial signals an incremental update to the structured Data
	// payload. In raw mode Raw and Reasoning carry the accumulated
	// upstream text so far, matching the HTTP /stream-with-raw shape.
	EventPartial EventKind = "partial"
	// EventFinal signals the terminal payload of the stream. Raw and
	// Reasoning, when set, hold the full final upstream text rather than
	// the accumulator's running total.
	EventFinal EventKind = "final"
	// EventReset signals that the orchestrator retried; the caller
	// should discard previously emitted Partial events for this request.
	EventReset EventKind = "reset"
	// EventMetadata carries a routing/retry Metadata payload emitted by
	// the orchestrator alongside the call.
	EventMetadata EventKind = "metadata"
)

// Event is a single streaming event delivered by Stream.Next.
type Event struct {
	Kind      EventKind
	Data      json.RawMessage
	Raw       string
	Reasoning string
	Metadata  *Metadata
}

// Stream is a single dynamic streaming call. Callers must consume to
// io.EOF or invoke Close so the worker's request scope and any pooled
// results are released.
type Stream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	dropScope func()
	results   <-chan *workerplugin.StreamResult
	errLabel  string

	needRaw       bool
	rawAccum      string
	reasonAccum   string
	pending       []Event
	closeOnce     sync.Once
	cleanupOnce   sync.Once
	terminated    bool
	terminatedErr error
}

// newStream wires a Stream over the worker result channel returned by
// CallStream. cancel cancels the per-stream context; dropScope drops
// the shared-state scope when the stream terminates.
func newStream(ctx context.Context, cancel context.CancelFunc, dropScope func(), results <-chan *workerplugin.StreamResult, needRaw bool, errLabel string) *Stream {
	return &Stream{
		ctx:       ctx,
		cancel:    cancel,
		dropScope: dropScope,
		results:   results,
		errLabel:  errLabel,
		needRaw:   needRaw,
	}
}

// Next returns the next event in the stream. io.EOF marks clean
// completion. Mid-stream worker errors surface as Go errors rather than
// EventKind values. Calling Next after EOF or an error returns the same
// terminal value and runs cleanup at most once.
func (s *Stream) Next() (*Event, error) {
	if s == nil || s.results == nil || s.cancel == nil {
		return nil, errUninitializedStream
	}
	if s.terminated {
		if s.terminatedErr != nil {
			return nil, s.terminatedErr
		}
		return nil, io.EOF
	}

	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return &ev, nil
	}

	for {
		result, ok := <-s.results
		if !ok {
			return nil, s.terminate(io.EOF)
		}
		ev, extra, err := s.translate(result)
		workerplugin.ReleaseStreamResult(result)
		if err != nil {
			return nil, s.terminate(err)
		}
		if extra != nil {
			s.pending = append(s.pending, *extra)
		}
		if ev != nil {
			return ev, nil
		}
		// Heartbeat or empty frame — keep waiting.
	}
}

// Close cancels the stream context, drains any remaining worker
// results, and drops the shared-state scope. Safe to call multiple
// times and from any goroutine.
func (s *Stream) Close() error {
	if s == nil || s.results == nil || s.cancel == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.cancel()
		go func() {
			for r := range s.results {
				workerplugin.ReleaseStreamResult(r)
			}
			s.cleanup()
		}()
	})
	return nil
}

// translate converts a worker stream frame into a public Event. When a
// frame carries both a Reset and a payload, translate returns the
// reset event and stashes the payload event in extra so the caller can
// queue it for the next Next call — preserving the HTTP ordering of
// "reset first, then the new payload".
func (s *Stream) translate(result *workerplugin.StreamResult) (event *Event, extra *Event, err error) {
	if result.Reset {
		s.rawAccum = ""
		s.reasonAccum = ""
	}

	var payload *Event
	switch result.Kind {
	case workerplugin.StreamResultKindError:
		wrapped := workerplugin.NewErrorWithMetadata(result.Error, result.Stacktrace, result.ErrorCode, result.ErrorDetails)
		return nil, nil, fmt.Errorf("dynclient: %s: %w", s.errLabel, wrapped)
	case workerplugin.StreamResultKindHeartbeat:
		// Heartbeats keep the channel alive but carry no payload.
	case workerplugin.StreamResultKindStream:
		ev, derr := s.partialEvent(result)
		if derr != nil {
			return nil, nil, derr
		}
		payload = ev
	case workerplugin.StreamResultKindFinal:
		ev, derr := s.finalEvent(result)
		if derr != nil {
			return nil, nil, derr
		}
		payload = ev
	case workerplugin.StreamResultKindMetadata:
		md, derr := decodeMetadata(result.Data)
		if derr != nil {
			return nil, nil, derr
		}
		if md != nil {
			payload = &Event{Kind: EventMetadata, Metadata: md}
		}
	}

	if result.Reset {
		resetEv := &Event{Kind: EventReset}
		return resetEv, payload, nil
	}
	return payload, nil, nil
}

// partialEvent translates a Stream-kind worker frame into an
// EventPartial. In raw mode the upstream raw/reasoning deltas are
// accumulated to match the HTTP stream-with-raw contract.
func (s *Stream) partialEvent(result *workerplugin.StreamResult) (*Event, error) {
	flattened, err := flattenIfPresent(result.Data)
	if err != nil {
		return nil, err
	}
	if flattened == nil && (!s.needRaw || (result.Raw == "" && result.Reasoning == "")) {
		return nil, nil
	}

	ev := &Event{Kind: EventPartial, Data: flattened}
	if s.needRaw {
		s.rawAccum += result.Raw
		s.reasonAccum += result.Reasoning
		ev.Raw = s.rawAccum
		ev.Reasoning = s.reasonAccum
	}
	return ev, nil
}

// finalEvent translates a Final-kind frame. In raw mode the final raw
// and reasoning fields use the worker's full final values, not the
// running accumulator — the HTTP path emits the final values verbatim
// regardless of how many partials preceded them.
func (s *Stream) finalEvent(result *workerplugin.StreamResult) (*Event, error) {
	flattened, err := flattenIfPresent(result.Data)
	if err != nil {
		return nil, err
	}
	ev := &Event{Kind: EventFinal, Data: flattened}
	if s.needRaw {
		ev.Raw = result.Raw
		ev.Reasoning = result.Reasoning
	}
	return ev, nil
}

// flattenIfPresent copies the pooled byte slice and unwraps any
// DynamicProperties envelope. Empty input yields nil so partial frames
// that carry only metadata (raw deltas) don't surface a bogus payload.
func flattenIfPresent(data []byte) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}
	copied := append(json.RawMessage(nil), data...)
	flattened, err := bamlutils.FlattenDynamicOutput(copied)
	if err != nil {
		return nil, err
	}
	return flattened, nil
}

// terminate latches the stream's terminal value and triggers cleanup.
// Subsequent Next calls observe the same terminal value.
func (s *Stream) terminate(err error) error {
	s.terminated = true
	if !errors.Is(err, io.EOF) {
		s.terminatedErr = err
	}
	s.cancel()
	s.cleanup()
	return err
}

// cleanup runs the shared-state DropScope at most once across the
// natural-EOF and Close paths.
func (s *Stream) cleanup() {
	s.cleanupOnce.Do(func() {
		if s.dropScope != nil {
			s.dropScope()
		}
	})
}

// Compile-time check that the StreamModeStream constant used by the
// public streaming entrypoint still exists in bamlutils. The
// NeedsRaw method is exercised by the real call in client.go, which
// catches any signature change on that method.
var _ = bamlutils.StreamModeStream
