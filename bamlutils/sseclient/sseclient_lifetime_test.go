package sseclient

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestEventDataByteStableAfterAdvance is the core safety test for the
// scanner.Bytes() parser: each Event.Data must be an owned copy, not a view
// into the scanner's pooled buffer (which is reused/overwritten on the next
// Scan). We emit many events whose payloads are all the same length but with
// distinct content, so that any aliasing into the reused scanner buffer would
// corrupt earlier events' Data once the parser advances. We collect every
// event first, let the parser run to completion, and only then verify the
// retained Data values are unchanged.
func TestEventDataByteStableAfterAdvance(t *testing.T) {
	const n = 500
	const width = 64 // fixed-width payloads so aliasing would overwrite in place

	var in bytes.Buffer
	want := make([]string, n)
	for i := 0; i < n; i++ {
		// Distinct, fixed-width payload per event.
		p := fmt.Sprintf("payload-%0*d", width-len("payload-"), i)
		if len(p) != width {
			t.Fatalf("payload width mismatch: got %d want %d", len(p), width)
		}
		want[i] = p
		in.WriteString("data: ")
		in.WriteString(p)
		in.WriteString("\n\n")
	}

	events, err := collectEvents(t, context.Background(), bytes.NewReader(in.Bytes()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != n {
		t.Fatalf("expected %d events, got %d", n, len(events))
	}

	// All events are now retained; the parser has finished and its scanner
	// buffer has been recycled. If any Data aliased the scanner buffer, these
	// comparisons would fail.
	for i, ev := range events {
		if ev.Data != want[i] {
			t.Fatalf("event[%d].Data corrupted: got %q want %q", i, ev.Data, want[i])
		}
	}
}

// TestMultilineDataByteStableAfterAdvance verifies that multi-line data:
// frames are still joined with newlines AND remain byte-stable after the
// parser advances well past them.
func TestMultilineDataByteStableAfterAdvance(t *testing.T) {
	const n = 200
	const linesPerEvent = 4

	var in bytes.Buffer
	want := make([]string, n)
	for i := 0; i < n; i++ {
		lines := make([]string, linesPerEvent)
		for j := 0; j < linesPerEvent; j++ {
			line := fmt.Sprintf("evt%05d-line%d-%s", i, j, strings.Repeat("z", 32))
			lines[j] = line
			in.WriteString("data: ")
			in.WriteString(line)
			in.WriteByte('\n')
		}
		in.WriteByte('\n')
		want[i] = strings.Join(lines, "\n")
	}

	events, err := collectEvents(t, context.Background(), bytes.NewReader(in.Bytes()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != n {
		t.Fatalf("expected %d events, got %d", n, len(events))
	}
	for i, ev := range events {
		if ev.Data != want[i] {
			t.Fatalf("event[%d].Data mismatch:\n got %q\nwant %q", i, ev.Data, want[i])
		}
	}
}

// TestSlowConsumerExceedsChannelBuffer drives a deliberately slow consumer so
// the parser fills the 16-deep channel and runs ahead, then validates every
// event's Data. Run under -race, this catches both scanner-buffer aliasing and
// any data race between the producer goroutine and the consumer. Distinct
// fixed-width payloads ensure aliasing would be observable as corruption.
func TestSlowConsumerExceedsChannelBuffer(t *testing.T) {
	const n = 64 // > channel buffer (16) so the producer must run ahead
	const width = 48

	var in bytes.Buffer
	want := make([]string, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("slow-%0*d", width-len("slow-"), i)
		want[i] = p
		in.WriteString("event: tok\n")
		in.WriteString("id: ")
		in.WriteString(fmt.Sprintf("id-%05d", i))
		in.WriteByte('\n')
		in.WriteString("data: ")
		in.WriteString(p)
		in.WriteString("\n\n")
	}

	events, errc := Stream(context.Background(), bytes.NewReader(in.Bytes()))

	var got []Event
	for ev := range events {
		// Slow consumer: let the parser get ahead and fill the channel.
		time.Sleep(time.Millisecond)
		got = append(got, ev)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != n {
		t.Fatalf("expected %d events, got %d", n, len(got))
	}
	for i, ev := range got {
		if ev.Data != want[i] {
			t.Fatalf("event[%d].Data corrupted: got %q want %q", i, ev.Data, want[i])
		}
		if ev.Type != "tok" {
			t.Fatalf("event[%d].Type corrupted: got %q want %q", i, ev.Type, "tok")
		}
		wantID := fmt.Sprintf("id-%05d", i)
		if ev.ID != wantID {
			t.Fatalf("event[%d].ID corrupted: got %q want %q", i, ev.ID, wantID)
		}
	}
}
