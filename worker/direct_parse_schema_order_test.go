package worker

// De-BAML native-first direct parse — the FIELD-ORDER half of the agreement.
//
// The transition oracle compares BYTES, so two legs that agree on every field's
// value still decline if they emit those fields in a different order. BAML's order
// for a dynamic request is the order applyDynamicTypes populates the TypeBuilder in:
// the caller's declared order under preserve_order, alphabetical without it. These
// tests pin that the bridge hands the native parser a schema declared in exactly
// that order, and — the biting direction — that a native leg which ignores it and
// answers in wire order is still DECLINED rather than served.

import (
	"context"
	stdjson "encoding/json"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
)

// bookSchema is Root{title string, pages int} declared title-first — the shape the
// corpus's class-union fixtures use, and the one whose wire order is NOT
// alphabetical, so sorting is observable.
func bookSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("title", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("pages", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

// schemaWithNestedClass is Root{u: C} with C{title string, pages int}, so the
// nested class's own field order is exercised alongside the root's.
func schemaWithNestedClass() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("u", &bamlutils.DynamicProperty{Ref: "C"}),
		),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
				Properties: bamlutils.MustOrderedMap(
					bamlutils.OrderedKV("title", &bamlutils.DynamicProperty{Type: "string"}),
					bamlutils.OrderedKV("pages", &bamlutils.DynamicProperty{Type: "int"}),
				),
			}),
		),
	}
}

// dynamicParseInputWithSchema marshals a worker parse input carrying schema and the
// PreserveOrder flag exactly where the public dynamic parse endpoint puts them: the
// schema on BamlOptions.OutputSchema, the flag on the TypeBuilder's DynamicTypes.
func dynamicParseInputWithSchema(t *testing.T, raw string, schema *bamlutils.DynamicOutputSchema, preserveOrder bool) []byte {
	t.Helper()
	in, err := sonic.Marshal(workerParseInput{
		Raw: raw,
		Options: &bamlutils.BamlOptions{
			OutputSchema: schema,
			TypeBuilder: &bamlutils.TypeBuilder{
				DynamicTypes: &bamlutils.DynamicTypes{PreserveOrder: preserveOrder},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal parse input: %v", err)
	}
	return in
}

// schemaRecordingParser is a native parser that records the OutputSchema it was
// handed, so a test can assert the ORDER the bridge declared it in.
func schemaRecordingParser(seen **bamlutils.DynamicOutputSchema, payload string) bamlutils.DeBAMLParseFunc {
	return func(_ context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
		*seen = req.OutputSchema
		return bamlutils.DeBAMLParseResult{JSON: stdjson.RawMessage(payload)}, nil
	}
}

func TestNativeParseSchemaOrderSortsWhenPreserveOrderIsOff(t *testing.T) {
	t.Parallel()

	in := schemaWithNestedClass()
	got := nativeParseSchemaForOrder(in, false)

	if want := []string{"u"}; !equalStrings(got.Properties.Keys(), want) {
		t.Errorf("root properties = %v, want %v", got.Properties.Keys(), want)
	}
	c, ok := got.Classes.Get("C")
	if !ok {
		t.Fatalf("class C dropped by the order pass: %v", got.Classes.Keys())
	}
	if want := []string{"pages", "title"}; !equalStrings(c.Properties.Keys(), want) {
		t.Errorf("class C fields = %v, want the applyDynamicTypes sorted order %v", c.Properties.Keys(), want)
	}

	// The caller's schema is untouched: the decoded input is still readable in wire
	// order by anything else that consults it.
	origC, _ := in.Classes.Get("C")
	if want := []string{"title", "pages"}; !equalStrings(origC.Properties.Keys(), want) {
		t.Errorf("the pass mutated the caller's schema: class C fields = %v, want %v", origC.Properties.Keys(), want)
	}
}

func TestNativeParseSchemaOrderKeepsDeclaredOrderWhenPreserveOrderIsOn(t *testing.T) {
	t.Parallel()

	in := bookSchema()
	got := nativeParseSchemaForOrder(in, true)
	if got != in {
		t.Errorf("preserve_order must hand the schema through untouched")
	}
	if want := []string{"title", "pages"}; !equalStrings(got.Properties.Keys(), want) {
		t.Errorf("properties = %v, want the declared order %v", got.Properties.Keys(), want)
	}
}

// TestNativeParseSchemaOrderIsIdempotent matters because the CALL path already
// sends a field-sorted schema (DynamicInput.ToWorkerInput's render clone): running
// the pass over one must not perturb it.
func TestNativeParseSchemaOrderIsIdempotent(t *testing.T) {
	t.Parallel()

	once := nativeParseSchemaForOrder(schemaWithNestedClass(), false)
	twice := nativeParseSchemaForOrder(once, false)
	onceC, _ := once.Classes.Get("C")
	twiceC, _ := twice.Classes.Get("C")
	if !equalStrings(onceC.Properties.Keys(), twiceC.Properties.Keys()) {
		t.Errorf("second pass changed the order: %v then %v", onceC.Properties.Keys(), twiceC.Properties.Keys())
	}
}

// TestNativeDirectParseHandsNativeBAMLsFieldOrder is the bridge-level statement:
// the schema the native parser receives is declared in the order BAML's TypeBuilder
// will be populated in for THIS request, which is what lets the two legs' bytes be
// comparable at all.
func TestNativeDirectParseHandsNativeBAMLsFieldOrder(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		preserveOrder bool
		want          []string
	}{
		{"preserve_order off sorts like applyDynamicTypes", false, []string{"pages", "title"}},
		{"preserve_order on keeps the caller's order", true, []string{"title", "pages"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var seen *bamlutils.DynamicOutputSchema
			baml := &oracleLeg{}
			h := newTestHandler(t, Config{
				Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
				DeBAML:      deBAMLOnConfig(),
				DeBAMLParse: schemaRecordingParser(&seen, agreedPayload),
			})

			if _, err := h.Parse(context.Background(), bamlutils.DynamicMethodName,
				dynamicParseInputWithSchema(t, "{...}", bookSchema(), tc.preserveOrder)); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if seen == nil {
				t.Fatal("the native parser was never handed a schema")
			}
			if !equalStrings(seen.Properties.Keys(), tc.want) {
				t.Errorf("native saw fields %v, want %v", seen.Properties.Keys(), tc.want)
			}
		})
	}
}

// TestNativeDirectParseDeclinesANativeLegInWireFieldOrder is the biting case for
// this behavior: a native parser that emits BAML's exact content in the CALLER's
// declared order — what the parser did before the order pass existed — for a
// preserve_order=false request, where BAML emits it sorted. Serving that would be
// an out-claim on key order, so the bridge must decline and record drift.
func TestNativeDirectParseDeclinesANativeLegInWireFieldOrder(t *testing.T) {
	t.Parallel()

	const bamlSorted = `{"pages":300,"title":"Go"}`
	const nativeWireOrder = `{"title":"Go","pages":300}`

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, bamlSorted, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, nativeWireOrder, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName,
		dynamicParseInputWithSchema(t, "{...}", bookSchema(), false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := string(res.Data), `{"DynamicProperties":`+bamlSorted+`}`; got != want {
		t.Fatalf("native out-claimed BAML on field order: served %s, want BAML's %s", got, want)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonResultDrift); got != 1 {
		t.Errorf("baml/result_drift counter = %v, want 1", got)
	}
	if got := directParseCount(t, h, directParseEngineNative, directParseReasonAgreement); got != 0 {
		t.Errorf("a field-order disagreement was recorded as agreement (%v)", got)
	}
}

// TestNativeDirectParseDeclinesANativeLegThatOmitsAnAbsentOptional is the biting
// case for the other half of the worker-boundary agreement: BAML spells an absent
// optional as an explicit null, and a native leg that OMITS the key — what the
// parser did before internal/debaml emitted the null itself — must decline rather
// than be served on the assumption that a downstream pass would re-add it.
func TestNativeDirectParseDeclinesANativeLegThatOmitsAnAbsentOptional(t *testing.T) {
	t.Parallel()

	const bamlWithNull = `{"name":"Ada","nick":null}`
	const nativeOmitting = `{"name":"Ada"}`

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, bamlWithNull, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, nativeOmitting, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := string(res.Data), `{"DynamicProperties":`+bamlWithNull+`}`; got != want {
		t.Fatalf("native out-claimed BAML on an absent optional: served %s, want BAML's %s", got, want)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonResultDrift); got != 1 {
		t.Errorf("baml/result_drift counter = %v, want 1", got)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
