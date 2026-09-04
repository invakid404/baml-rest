package nativespinejsonfixture

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/schema"
)

// transcript_test.go OWNS the checked-in expected public stream transcript for the exact
// five-arm `JSON` alias, and proves it is exactly what the ROOT-OWNED native
// static-stream parsers plus the emitted carriers produce for a fixed content-delta
// sequence.
//
// It exists so the two stream consumers that cannot compute it themselves — the gated
// spine SSE fault matrix (nativeserve, a different module) and the booted native-only
// worker e2e (nanollmprepare, another module that cannot import root-internal packages)
// — assert against ONE table rather than three hand-written guesses. Both read the JSON
// file this test regenerates; neither imports BAML to compute an expectation at runtime.
//
// The values themselves are stock-BAML-v0.223 behaviour by construction: the emit/no-emit
// decision and the partial bytes come from debaml.ParseStaticStreamPartial, whose
// byte/event-exactness against v0.223's ParseStream is what the strict per-prefix
// differential and the SSE-replay differential already prove for this family.

var updateTranscript = flag.Bool("update-stream-transcript", false,
	"rewrite the committed stream transcript fixture (testdata/stream_transcript.json)")

// TranscriptDelta is one provider content delta and the public event it must produce.
type TranscriptDelta struct {
	// Content is the assistant content delta the provider sends for this chunk.
	Content string `json:"content"`
	// Emit reports whether this prefix yields a STRUCTURED partial event.
	Emit bool `json:"emit"`
	// Partial is the exact marshaled bytes of the emitted partial carrier (empty when
	// Emit is false).
	Partial string `json:"partial,omitempty"`
}

// Transcript is the whole expected public transcript for one stream.
type Transcript struct {
	// Deltas are the ordered content deltas and their expected events.
	Deltas []TranscriptDelta `json:"deltas"`
	// Final is the exact marshaled bytes of the final value carrier.
	Final string `json:"final"`
	// Accumulated is the full assistant text the deltas concatenate to.
	Accumulated string `json:"accumulated"`
}

// StructuredCount returns how many deltas produce a structured partial.
func (tr Transcript) StructuredCount() int {
	n := 0
	for _, d := range tr.Deltas {
		if d.Emit {
			n++
		}
	}
	return n
}

// transcriptContents is the FIXED content-delta sequence the transcript is computed
// over. It is chosen to exercise the interesting prefix classes: a whitespace-only
// preamble that yields NO partial (the sentinel row), an opening brace, a mid-key split,
// a completed scalar member, a mid-string split, a mid-array split, and the close.
var transcriptContents = []string{
	"\n",
	`{"`,
	`k":1`,
	`,"s":"h`,
	`i"`,
	`,"l":[1`,
	`,2]`,
	`}`,
}

// TranscriptPath is the repo-relative path of the committed fixture, so a consumer in
// another module can locate it from the repo root.
const TranscriptPath = "internal/nativespinejsonfixture/testdata/stream_transcript.json"

func transcriptFile(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "stream_transcript.json")
}

// jsonAliasBundle lowers the exact five-arm JSON alias method's Return the same way
// admission and registration do.
func jsonAliasBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	for _, m := range proj.Methods {
		if m.Name != MethodName {
			continue
		}
		b, err := schema.FromStaticDescriptor(m.Return)
		if err != nil {
			t.Fatalf("FromStaticDescriptor: %v", err)
		}
		return b
	}
	t.Fatalf("%s not admitted", MethodName)
	return nil
}

// computeTranscript replays transcriptContents through the ROOT-OWNED parsers and the
// EMITTED decoders, exactly as the spine stream executor's cadence does.
func computeTranscript(t *testing.T) Transcript {
	t.Helper()
	bundle := jsonAliasBundle(t)
	ctx := context.Background()

	tr := Transcript{}
	var acc strings.Builder
	for _, content := range transcriptContents {
		acc.WriteString(content)
		d := TranscriptDelta{Content: content}
		parsed, err := debaml.ParseStaticStreamPartial(ctx, bundle, acc.String())
		switch {
		case err == nil:
			carrier, derr := decodePartial(parsed.JSON)
			if derr != nil {
				t.Fatalf("decodePartial(%s): %v", parsed.JSON, derr)
			}
			b, merr := json.Marshal(carrier)
			if merr != nil {
				t.Fatalf("marshal partial carrier: %v", merr)
			}
			d.Emit = true
			d.Partial = string(b)
		case errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported):
			// The documented "no parseable partial for this prefix yet" sentinel: a
			// benign no-event on the claimed lane.
		default:
			t.Fatalf("ParseStaticStreamPartial(%q): unexpected non-sentinel error %v", acc.String(), err)
		}
		tr.Deltas = append(tr.Deltas, d)
	}
	tr.Accumulated = acc.String()

	final, err := debaml.ParseStaticStreamFinal(ctx, bundle, tr.Accumulated)
	if err != nil {
		t.Fatalf("ParseStaticStreamFinal: %v", err)
	}
	carrier, derr := decodeFinal(final.JSON)
	if derr != nil {
		t.Fatalf("decodeFinal: %v", derr)
	}
	b, merr := json.Marshal(carrier)
	if merr != nil {
		t.Fatalf("marshal final carrier: %v", merr)
	}
	tr.Final = string(b)
	return tr
}

// TestStreamTranscriptFixtureIsFaithful proves the committed transcript equals what the
// root-owned parsers + emitted decoders produce. With -update-stream-transcript it
// regenerates the file.
func TestStreamTranscriptFixtureIsFaithful(t *testing.T) {
	got := computeTranscript(t)

	// Non-vacuity: the sequence must actually exercise BOTH outcomes, or the consumers
	// asserting against it would prove nothing about emit/no-emit.
	if got.StructuredCount() == 0 {
		t.Fatal("no delta produces a structured partial; the transcript would not test emission")
	}
	if got.StructuredCount() == len(got.Deltas) {
		t.Fatal("every delta produces a structured partial; the transcript would not test the no-emit sentinel")
	}
	if got.Final == "" {
		t.Fatal("the transcript has no final")
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	encoded = append(encoded, '\n')

	path := transcriptFile(t)
	if *updateTranscript {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", path, len(encoded))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed transcript: %v", err)
	}
	if string(want) != string(encoded) {
		t.Fatalf("committed %s is stale — re-run with -update-stream-transcript.\nwant:\n%s\ngot:\n%s", path, want, encoded)
	}
}

// TestStreamTranscriptFinalMatchesTheUnaryFinal proves the STREAM final is byte-identical
// to what the SAME accumulated text produces through the unary final route — the property
// that lets the stream lane reuse DecodeFinal (the value carrier) rather than the stream
// decoder for its final.
func TestStreamTranscriptFinalMatchesTheUnaryFinal(t *testing.T) {
	tr := computeTranscript(t)
	bundle := jsonAliasBundle(t)
	unary, err := debaml.ParseStaticBundleUnaryCall(context.Background(), bundle, tr.Accumulated)
	if err != nil {
		t.Fatalf("ParseStaticBundleUnaryCall: %v", err)
	}
	carrier, derr := decodeFinal(unary.JSON)
	if derr != nil {
		t.Fatalf("decodeFinal: %v", derr)
	}
	b, merr := json.Marshal(carrier)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	if string(b) != tr.Final {
		t.Fatalf("stream final %s != unary final %s for the same completed text", tr.Final, b)
	}
}
