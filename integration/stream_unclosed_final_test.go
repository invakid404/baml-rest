//go:build integration

package integration

// De-BAML Phase 7C — committed byte-by-byte proof that the native-only stream FINAL
// closure reproduces BAML's non-stream Parse EOF recovery of a COMPLETE-BUT-UNCLOSED
// object. An ordinary completed stream (finish_reason:stop + [DONE]) whose accumulated
// model text lost its closing bracket is a BAML success (its stream final runs the
// non-stream Parse), so ParseNativeStreamFinal must complete it byte-exact — this is
// the round-4 P1 case. For EVERY prefix of a complete admitted response (as if [DONE]
// arrived there), native ParseNativeStreamFinal == BAML non-stream Parse.

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

func ucSch(kv ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(kv...)}
}
func ucP(k, ty string) bamlutils.OrderedEntry[*bamlutils.DynamicProperty] {
	return bamlutils.OrderedKV(k, &bamlutils.DynamicProperty{Type: ty})
}
func ucList(k, elem string) bamlutils.OrderedEntry[*bamlutils.DynamicProperty] {
	return bamlutils.OrderedKV(k, &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: elem}})
}

func TestStream7CUnclosedFinalParity(t *testing.T) {
	dynclientCallGate(t)
	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	preserve := true
	norm := func(s *bamlutils.DynamicOutputSchema, flat []byte) string {
		out, e := bamlutils.FlattenDynamicOutput(flat)
		if e != nil {
			out = flat
		}
		if out, e = bamlutils.InjectAbsentOptionals(out, s); e != nil {
			return string(flat)
		}
		if out, e = bamlutils.ReorderDynamicOutputBySchema(out, s); e != nil {
			return string(flat)
		}
		return string(out)
	}
	baml := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, e := dyn.DynamicParse(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: s, PreserveSchemaOrder: &preserve, Stream: false})
		if e != nil {
			return "", false
		}
		return norm(s, res.Data), true
	}
	// The PRODUCTION native-only FINAL closure (Parse + completeUnclosedFinal on decline).
	nat := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		j, e := debaml.ParseNativeStreamFinal(context.Background(), s, raw)
		if e != nil {
			return "", false
		}
		return norm(s, j), true
	}
	nested := &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("label", &bamlutils.DynamicProperty{Type: "string"}), bamlutils.OrderedKV("inner", &bamlutils.DynamicProperty{Ref: "Inner"})),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Inner", &bamlutils.DynamicClass{Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("x", &bamlutils.DynamicProperty{Type: "string"}), bamlutils.OrderedKV("y", &bamlutils.DynamicProperty{Type: "int"}))})),
	}
	// Admitted shapes covering every last-field type. (list<class> with an INCOMPLETE
	// element is excluded here: native's list coercion conservatively declines a
	// valObject missing a required field where BAML drops it — a PRE-EXISTING gap that
	// affects CLOSED inputs identically and is orthogonal to EOF completion.)
	cases := []struct {
		name string
		s    *bamlutils.DynamicOutputSchema
		full string
	}{
		{"str_int", ucSch(ucP("name", "string"), ucP("age", "int")), `{"name":"Ada","age":36}`},
		{"str_int_spaced", ucSch(ucP("name", "string"), ucP("age", "int")), `{"name": "Ada", "age": 36}`},
		{"str_negint", ucSch(ucP("name", "string"), ucP("age", "int")), `{"name":"Ada","age":-42}`},
		// bool/float/map fields are DECLINED pre-transport (streaming-cadence /
		// overwrite-on-duplicate divergences), so the admitted EOF matrix uses
		// string/int/list<string|int>/nested-class shapes.
		{"str_listint", ucSch(ucP("name", "string"), ucList("nums", "int")), `{"name":"Ada","nums":[1,2,3]}`},
		{"str_liststr", ucSch(ucP("name", "string"), ucList("tags", "string")), `{"name":"Ada","tags":["x","y","z"]}`},
		{"liststr_str", ucSch(ucList("tags", "string"), ucP("name", "string")), `{"tags":["x","y"],"name":"Bob"}`},
		{"str_str", ucSch(ucP("a", "string"), ucP("b", "string")), `{"a":"hello","b":"world"}`},
		{"liststr_str_int", ucSch(ucP("name", "string"), ucList("tags", "string"), ucP("age", "int")), `{"name":"Ada","tags":["p","q"],"age":7}`},
		{"nested_class", nested, `{"label":"L","inner":{"x":"v","y":9}}`},
		// round 13 (CodeRabbit discussion_r3609109500): SINGLE-quoted keys/values/elements
		// and JSONish UNQUOTED keys are recovered at EOF byte-exact vs BAML too — every
		// prefix, not just the completed form. (No backslash appears in these fixtures: a
		// backslash inside a single-quoted string is the #583 fixer deferral, an
		// under-approximation asserted separately, not an EOF-completion concern.)
		{"sq_str_int", ucSch(ucP("name", "string"), ucP("age", "int")), `{'name':'Ada','age':36}`},
		{"uq_str_int", ucSch(ucP("name", "string"), ucP("age", "int")), `{name:"Ada",age:36}`},
		{"sq_liststr", ucSch(ucP("name", "string"), ucList("tags", "string")), `{'name':'Ada','tags':['x','y']}`},
		{"uq_listint", ucSch(ucP("name", "string"), ucList("nums", "int")), `{name:"Ada",nums:[1,2,3]}`},
		{"uq_liststr", ucSch(ucP("name", "string"), ucList("tags", "string")), `{name:"Ada",tags:["x","y"]}`},
	}
	total, mism := 0, 0
	for _, c := range cases {
		if err := debaml.SupportsNativeStream(c.s); err != nil {
			t.Errorf("case %q: expected ADMITTED, got %v", c.name, err)
			continue
		}
		for i := 1; i <= len(c.full); i++ {
			p := c.full[:i]
			b, bok := baml(c.s, p)
			n, nok := nat(c.s, p)
			total++
			if bok != nok || (bok && nok && b != n) {
				mism++
				t.Errorf("[%s @%d] raw=%q | BAML(ok=%v)=%s | NATIVE(ok=%v)=%s", c.name, i, p, bok, b, nok, n)
			}
		}
	}
	t.Logf("UNCLOSED-FINAL PARITY: %d prefix finals, %d mismatches", total, mism)
}
