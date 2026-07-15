//go:build nanollm_integration

package shadow

// De-BAML cutover Slice 5 SAME-response comparator suite. Gated by
// `nanollm_integration` (the comparator's response leg links nanollm through
// admission.NewResponseClient + TranslateResponse). It proves, entirely WITHOUT a
// socket or a second provider request:
//
//   - the happy path records all seven response_compare facets as `match`
//     (translate/assistant/structured/order/raw/reasoning/error);
//   - a native-vs-BAML STRUCTURED drift on the same body is caught as a
//     `structured` (and `order`) mismatch, never a silent pass;
//   - a BAML-only-parse error is recorded as an `error` mismatch and stops the
//     comparison rather than fabricating facet results;
//   - compareStructured's semantic-vs-order split: a value change is a structured
//     mismatch, a class-field reorder self-heals (order match), and a nested-object
//     key-order drift is an order mismatch while staying semantically equal.

import (
	"context"
	stdjson "encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
)

// openAIBody builds a minimal openai chat 2xx body whose assistant content is the
// given (already-JSON) string.
func openAIBody(content string) []byte {
	quoted, _ := stdjson.Marshal(content)
	return []byte(`{"choices":[{"message":{"role":"assistant","content":` + string(quoted) + `}}]}`)
}

func responseCompareVal(t *testing.T, reg *prometheus.Registry, result, field string) float64 {
	return counterVal(t, reg, "baml_rest_debaml_response_compare_total", map[string]string{"result": result, "field": field})
}

// newResponseComparator builds a Comparator over a fresh private registry so a
// test can read response_compare, plus a counting exact transport to prove the
// response comparison opens zero sockets.
func newResponseComparator(t *testing.T) (*Comparator, *prometheus.Registry, *countingTransport) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))
	return c, reg, ct
}

// responseReq is a NativeShadowRequest carrying the SAME registry + schema the
// happy-path plan comparison admitted, plus a BAML-only parse closure the test
// controls.
func responseReq(bamlOnly func(ctx context.Context, raw string) ([]byte, error)) bamlutils.NativeShadowRequest {
	req := shadowRequest(nil)
	req.BAMLOnlyParse = bamlOnly
	return req
}

// TestCompareResponse_AllMatch pins the happy path: native TranslateResponse + SAP
// over BAML's fetched body agrees with the independent BAML-only parse on every
// facet, so all seven response_compare series record `match` and none `mismatch`.
func TestCompareResponse_AllMatch(t *testing.T) {
	c, reg, ct := newResponseComparator(t)
	body := openAIBody(`{"answer":"ok"}`)
	req := responseReq(func(context.Context, string) ([]byte, error) {
		return []byte(`{"answer":"ok"}`), nil
	})

	c.compareResponse(context.Background(), req, 200, body)

	assertNoSocket(t, ct)
	for _, field := range []string{"translate", "assistant", "structured", "order", "raw", "reasoning", "error"} {
		if got := responseCompareVal(t, reg, "match", field); got != 1 {
			t.Errorf("response_compare{match,%s} = %v, want 1", field, got)
		}
		if got := responseCompareVal(t, reg, "mismatch", field); got != 0 {
			t.Errorf("response_compare{mismatch,%s} = %v, want 0 (zero-tolerance)", field, got)
		}
	}
}

// TestCompareResponse_StructuredMismatchCaught pins that a native-vs-BAML
// structured drift on the SAME body is caught: the BAML-only parse returns a
// different value than native SAP, so structured (and order) record a mismatch
// while the extractor-channel facets still match and error stays a match.
func TestCompareResponse_StructuredMismatchCaught(t *testing.T) {
	c, reg, ct := newResponseComparator(t)
	body := openAIBody(`{"answer":"ok"}`)
	req := responseReq(func(context.Context, string) ([]byte, error) {
		return []byte(`{"answer":"DIFFERENT"}`), nil
	})

	c.compareResponse(context.Background(), req, 200, body)

	assertNoSocket(t, ct)
	if got := responseCompareVal(t, reg, "mismatch", "structured"); got != 1 {
		t.Errorf("response_compare{mismatch,structured} = %v, want 1 (drift must be caught)", got)
	}
	if got := responseCompareVal(t, reg, "mismatch", "order"); got != 1 {
		t.Errorf("response_compare{mismatch,order} = %v, want 1 (value drift also shifts bytes)", got)
	}
	// translate/assistant/raw/reasoning still match; error is a clean run.
	if got := responseCompareVal(t, reg, "match", "assistant"); got != 1 {
		t.Errorf("response_compare{match,assistant} = %v, want 1", got)
	}
	if got := responseCompareVal(t, reg, "match", "error"); got != 1 {
		t.Errorf("response_compare{match,error} = %v, want 1 (no pipeline error)", got)
	}
}

// TestCompareResponse_BAMLParseErrorRecordsError pins that a BAML-only-parse
// failure is recorded as an `error` mismatch and stops the comparison — the other
// facets are NOT fabricated.
func TestCompareResponse_BAMLParseErrorRecordsError(t *testing.T) {
	c, reg, ct := newResponseComparator(t)
	body := openAIBody(`{"answer":"ok"}`)
	req := responseReq(func(context.Context, string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})

	c.compareResponse(context.Background(), req, 200, body)

	assertNoSocket(t, ct)
	if got := responseCompareVal(t, reg, "mismatch", "error"); got != 1 {
		t.Errorf("response_compare{mismatch,error} = %v, want 1", got)
	}
	// No facet result is fabricated after a pipeline error.
	for _, field := range []string{"translate", "assistant", "structured", "order", "raw", "reasoning"} {
		if got := responseCompareVal(t, reg, "match", field) + responseCompareVal(t, reg, "mismatch", field); got != 0 {
			t.Errorf("response_compare{*,%s} = %v after a pipeline error, want 0 (not fabricated)", field, got)
		}
	}
}

// TestCompareStructured_SemanticVsOrder pins the two-level compare:
//   - identical bytes -> structured + order match;
//   - a changed scalar value -> structured mismatch;
//   - a class-field reorder -> structured match AND order match (self-heals via the
//     schema reorder pass);
//   - a nested-object key-order drift on a non-schema field -> structured match but
//     order MISMATCH (the reorder pass does not touch extra/map content).
func TestCompareStructured_SemanticVsOrder(t *testing.T) {
	// A two-field class schema so the reorder pass has a declared order to apply.
	schema := &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("name", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("age", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}

	t.Run("identical", func(t *testing.T) {
		s, o := compareStructured([]byte(`{"name":"Ada","age":36}`), []byte(`{"name":"Ada","age":36}`), schema)
		if !s || !o {
			t.Errorf("identical: structured=%v order=%v, want both true", s, o)
		}
	})

	t.Run("value drift", func(t *testing.T) {
		s, _ := compareStructured([]byte(`{"name":"Ada","age":36}`), []byte(`{"name":"Bob","age":36}`), schema)
		if s {
			t.Error("value drift: structured=true, want false")
		}
	})

	t.Run("class field reorder self-heals", func(t *testing.T) {
		s, o := compareStructured([]byte(`{"name":"Ada","age":36}`), []byte(`{"age":36,"name":"Ada"}`), schema)
		if !s || !o {
			t.Errorf("class reorder: structured=%v order=%v, want both true (schema reorder self-heals)", s, o)
		}
	})

	t.Run("nested non-schema key order drift", func(t *testing.T) {
		// meta is not a declared property, so the reorder pass appends it in input
		// order without reordering its nested keys — a nested-order drift stays
		// observable as an order mismatch while remaining semantically equal.
		native := []byte(`{"name":"Ada","age":36,"meta":{"b":1,"a":2}}`)
		baml := []byte(`{"name":"Ada","age":36,"meta":{"a":2,"b":1}}`)
		s, o := compareStructured(native, baml, schema)
		if !s {
			t.Error("nested drift: structured=false, want true (semantically equal)")
		}
		if o {
			t.Error("nested drift: order=true, want false (nested key order differs)")
		}
	})
}
