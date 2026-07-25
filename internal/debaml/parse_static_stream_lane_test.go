package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// TestParse_StaticStreamLaneSelection pins the DeBAMLParseRequest lane contract for a
// StaticStreamDescriptor request (parse.go): StreamFinal → the native STREAM-FINAL parse
// (five-arm stream-family gate + EOF completion), Stream → the stream PARTIAL parse, and
// NEITHER flag → the ORDINARY non-stream final parse via ParseStaticBundle (final-family
// gate, NO stream-EOF completion). It guards against the regression where the neither-flags
// branch was wired to ParseStaticStreamFinal, which would apply the WRONG (narrow
// stream-family) gate and EOF-completion semantics to an ordinary final request.
func TestParse_StaticStreamLaneSelection(t *testing.T) {
	// StaticAnswer is a proven FINAL-family shape (8C flat class) but is NOT the five-arm
	// stream family: the two lanes diverge on it — ParseStaticBundle ADMITS it, while
	// ParseStaticStreamFinal (SupportsNativeStaticStreamBundle) DECLINES it. That divergence
	// is exactly what distinguishes the ordinary-final lane from the stream-final lane.
	fn := &promptdescriptor.Function{Return: staticAnswerDescriptor()}
	const raw = `{"answer":"hi","confidence":5}`

	// NEITHER flag → ordinary final (ParseStaticBundle): the final-family shape is ADMITTED.
	neither, nErr := Parse(context.Background(), bamlutils.DeBAMLParseRequest{
		StaticStreamDescriptor: fn,
		Raw:                    raw,
	})
	if nErr != nil {
		t.Fatalf("neither-flags request must route to the ordinary final lane (ParseStaticBundle) and ADMIT the final-family StaticAnswer shape; got err=%v", nErr)
	}
	// ...and it must be byte-identical to calling ParseStaticBundle directly (proving it is
	// literally the ordinary-final entrypoint, not the stream-final one).
	direct, dErr := ParseStaticBundle(context.Background(), lowerOrFatal(t, staticAnswerDescriptor()), raw)
	if dErr != nil {
		t.Fatalf("ParseStaticBundle(StaticAnswer) baseline failed: %v", dErr)
	}
	if string(neither.JSON) != string(direct.JSON) {
		t.Fatalf("neither-flags result %q != ParseStaticBundle result %q — the neither-flags branch is not the ordinary-final lane", neither.JSON, direct.JSON)
	}

	// StreamFinal → the STREAM-final lane (ParseStaticStreamFinal): its narrow stream-family
	// gate DECLINES the non-alias StaticAnswer shape with the unsupported sentinel. If the
	// neither-flags branch shared this lane it would have declined above too.
	_, sfErr := Parse(context.Background(), bamlutils.DeBAMLParseRequest{
		StaticStreamDescriptor: fn,
		StreamFinal:            true,
		Raw:                    raw,
	})
	if sfErr == nil || !errors.Is(sfErr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("StreamFinal request must route to ParseStaticStreamFinal and DECLINE the non-stream-family StaticAnswer shape with ErrDeBAMLParseUnsupported; got err=%v", sfErr)
	}

	// Stream → the PARTIAL lane (ParseStaticStreamPartial): its stream-family support gate
	// DECLINES the non-alias StaticAnswer shape with the unsupported sentinel, whereas the
	// ordinary-final lane ADMITS it (asserted above). A decline here proves the Stream flag
	// routes to ParseStaticStreamPartial and does NOT fall through to ParseStaticBundle — the
	// middle branch most easily mis-wired.
	_, spErr := Parse(context.Background(), bamlutils.DeBAMLParseRequest{
		StaticStreamDescriptor: fn,
		Stream:                 true,
		Raw:                    raw,
	})
	if spErr == nil || !errors.Is(spErr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("Stream request must route to ParseStaticStreamPartial and DECLINE the non-stream-family StaticAnswer shape with ErrDeBAMLParseUnsupported; got err=%v", spErr)
	}
}

// TestStaticFinalLanes_EOFCompletionDiverges proves the two final entrypoints diverge on an
// unclosed-but-otherwise-final input: ParseStaticStreamFinal applies BAML's stream EOF
// object-completion (completeUnclosedFinal) and SUCCEEDS, while the ordinary ParseStaticBundle
// does NOT complete and DECLINES. This is why routing the neither-flags (ordinary-final)
// request through ParseStaticStreamFinal would wrongly ACCEPT an EOF completion an ordinary
// final parse never performs.
func TestStaticFinalLanes_EOFCompletionDiverges(t *testing.T) {
	bundle := jsonAliasBundle(t) // the admitted five-arm stream family (so the stream gate opens)
	const unclosed = `[1,2`      // a complete-but-UNCLOSED list: valid only under EOF completion

	// STREAM final: EOF-completes `[1,2` → `[1,2]` and succeeds.
	sf, sfErr := ParseStaticStreamFinal(context.Background(), bundle, unclosed)
	if sfErr != nil {
		t.Fatalf("ParseStaticStreamFinal(%q) must EOF-complete and succeed; got err=%v", unclosed, sfErr)
	}
	if string(sf.JSON) != `[1,2]` {
		t.Fatalf("ParseStaticStreamFinal(%q) = %q, want the EOF-completed %q", unclosed, sf.JSON, `[1,2]`)
	}

	// ORDINARY final: no EOF completion → the unclosed candidate declines.
	_, obErr := ParseStaticBundle(context.Background(), bundle, unclosed)
	if obErr == nil {
		t.Fatalf("ParseStaticBundle(%q) must DECLINE the unclosed input (no stream-EOF completion); got success", unclosed)
	}
	if !errors.Is(obErr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("ParseStaticBundle(%q) decline must be the unsupported sentinel; got %v", unclosed, obErr)
	}
}
