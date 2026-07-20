//go:build integration

package integration

// De-BAML Phase 7C — committed, CORPUS-EXHAUSTIVE acceptance probe for the admitted
// native-stream schema matrix (the §11 surface SupportsNativeStream enforces). It
// iterates the ENTIRE bamlfuzz parse-recovery corpus and proves, against LIVE BAML
// v0.223, that:
//
//   - for EVERY corpus fixture whose schema SupportsNativeStream ADMITS, the
//     native-only FINAL closure reproduces BAML's final byte-for-byte (and every
//     streaming PREFIX partial matches BAML's parse-stream) — so no admitted fixture
//     can be a BAML-success / native-fallback on the native-only lane (I6 / §5.9);
//   - the §11-narrowed shapes (single string-absorbing root, non-string map key,
//     union/optional, scalar map value) DECLINE pre-transport (#555 Slice 2 admits
//     non-ASCII literal/enum/name, field @description, and class/enum @alias/@description;
//     #583 teardown admits a NON-colliding field @alias too — native's rendered-name-only
//     matcher is byte-exact vs static BAML v0.223's alias-only jsonish coercer — while a
//     FUZZY alias/canonical rendered-name collision stays a #583 residual decline);
//   - comment-bearing FINALS — including UNTERMINATED (to-EOF) block and line
//     comments after a closed object — match BAML byte-exact (the P1/P2-c cases).
//
// Zero mismatches is the gate. This replaces the earlier hand-selected five-shape
// probe the re-review flagged as narrower than the code's admitted matrix.

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

func admStr() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "string"} }
func admInt() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "int"} }
func admSchema(kv ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(kv...)}
}

// TestStream7CAdmittedMatrixParity is the committed corpus-exhaustive acceptance gate.
func TestStream7CAdmittedMatrixParity(t *testing.T) {
	dynclientCallGate(t)
	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	corpus, err := bamlfuzz.LoadParseRecoveryCorpus(parseRecoveryCorpusDir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}

	// norm applies the SAME flatten+inject+reorder pipeline DynamicParse applies, so
	// the native flattened bytes are compared apples-to-apples with BAML-as-served.
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
	bamlParse := func(s *bamlutils.DynamicOutputSchema, raw string, preserve, stream bool) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, perr := dyn.DynamicParse(ctx, dynclient.ParseRequest{Raw: raw, OutputSchema: s, PreserveSchemaOrder: &preserve, Stream: stream})
		if perr != nil {
			return "", false
		}
		return norm(s, res.Data), true
	}
	nativePartial := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		p, e := debaml.ParseNativeStreamPartial(context.Background(), s, raw)
		if e != nil || p == nil {
			return "", false
		}
		return norm(s, p), true
	}
	nativeFinal := func(s *bamlutils.DynamicOutputSchema, raw string) (string, bool) {
		p, e := debaml.ParseNativeStreamFinal(context.Background(), s, raw)
		if e != nil {
			return "", false
		}
		return norm(s, p), true
	}

	admitted, declined, lowerFail := 0, 0, 0
	total, mism := 0, 0
	declinedNames := map[string]bool{}
	for _, c := range corpus {
		lowered, lerr := bamlfuzz.LowerToDynamicSchema(c.Schema)
		if lerr != nil {
			lowerFail++ // constraints / media / recursive: not native-lowerable, out of scope.
			continue
		}
		s := &lowered
		if debaml.SupportsNativeStream(s) != nil {
			declined++
			declinedNames[c.Name] = true
			continue
		}
		admitted++
		// FINAL parity for the fixture's complete raw (admitted → native MUST equal
		// BAML: both succeed byte-exact, or both do not succeed).
		if c.HasFinal() {
			b, be := bamlParse(s, c.Raw, c.PreserveSchemaOrder, false)
			n, ne := nativeFinal(s, c.Raw)
			total++
			if be != ne || (be && ne && b != n) {
				mism++
				t.Errorf("[FINAL %s] raw=%q | BAML(ok=%v)=%s | NATIVE(ok=%v)=%s", c.Name, c.Raw, be, b, ne, n)
			}
		}
		// PREFIX parity for every streaming prefix.
		for _, p := range c.Prefixes {
			b, be := bamlParse(s, p.Raw, c.PreserveSchemaOrder, true)
			n, ne := nativePartial(s, p.Raw)
			total++
			if be != ne || (be && ne && b != n) {
				mism++
				t.Errorf("[PREFIX %s/%s] raw=%q | BAML(emit=%v)=%s | NATIVE(emit=%v)=%s", c.Name, p.Name, p.Raw, be, b, ne, n)
			}
		}
	}
	// Anti-vacuity guard (catches an accidental all-decline schema-support regression). The
	// round-11 single-nested-class-field root narrow (rootSingleFieldIsClass — BAML single-field-
	// root inferred-object recovery native cannot reproduce) correctly declines a couple of
	// corpus fixtures; #555 Slice 2 (v3) admitted the non-ASCII / metadata shapes, so the
	// admitted count settled at 59; the guard has margin below that.
	if admitted < 45 {
		t.Fatalf("corpus admitted=%d is implausibly low (schema-support regression?)", admitted)
	}

	// COMMENT-bearing FINALS on the admitted str_int schema (the P1 + P2-c cases):
	// every comment position, INCLUDING unterminated (to-EOF) block and line comments
	// after a closed object. BAML strips the comments and succeeds; the native-only
	// final must reproduce it byte-exact.
	person := admSchema(bamlutils.OrderedKV("name", admStr()), bamlutils.OrderedKV("age", admInt()))
	commentFinals := []string{
		`{"name": "Ada", "age": 36, /* note */}`,
		`{"name": "Ada", "age": 36 /* note */}`,
		`{"name": "Ada", /* mid */ "age": 36}`,
		`{/* lead */ "name": "Ada", "age": 36}`,
		"/* pre */ {\"name\": \"Ada\", \"age\": 36}",
		"{\"name\": \"Ada\", // line\n \"age\": 36}",
		"{\"name\": \"Ada\", \"age\": 36 // trailing\n}",
		`{"name": "Ada", "age": 36} // after`,
		`{"name": "Ada", /* a */ /* b */ "age": 36}`,
		`{"name": "a/*b*/c", "age": 36}`, // comment INSIDE string kept
		// P2-c: UNTERMINATED (to-EOF) block/line comments after a CLOSED object.
		`{"name": "Ada", "age": 36} /* dangling to EOF`,
		`{"name": "Ada", "age": 36}/*`,
		`{"name": "Ada", "age": 36} // dangling to EOF`,
		`{"name": "Ada", "age": 36}//`,
	}
	for _, raw := range commentFinals {
		b, be := bamlParse(person, raw, true, false)
		n, ne := nativeFinal(person, raw)
		total++
		if !be {
			t.Errorf("comment-final precondition: BAML did not succeed on %q", raw)
			continue
		}
		if !ne || b != n {
			mism++
			t.Errorf("[COMMENT-FINAL] raw=%q | BAML=%s | NATIVE(ok=%v)=%s", raw, b, ne, n)
		}
	}

	t.Logf("ADMITTED MATRIX ACCEPTANCE: corpus admitted=%d declined=%d lowerFail=%d | %d comparisons, %d mismatches", admitted, declined, lowerFail, total, mism)

	// Hand-built §11-narrowed shapes are declined pre-transport (the enforced
	// admission boundary): a single string-absorbing root, an optional/union field,
	// and a scalar map value.
	handDeclined := map[string]*bamlutils.DynamicOutputSchema{
		"single_string":   admSchema(bamlutils.OrderedKV("answer", admStr())),
		"str_then_optint": admSchema(bamlutils.OrderedKV("name", admStr()), bamlutils.OrderedKV("n", &bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}})),
		"str_then_mapint": admSchema(bamlutils.OrderedKV("name", admStr()), bamlutils.OrderedKV("m", &bamlutils.DynamicProperty{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}})),
	}
	for name, s := range handDeclined {
		if err := debaml.SupportsNativeStream(s); err == nil {
			t.Errorf("declined shape %q: expected SupportsNativeStream to DECLINE, got nil", name)
		}
	}
	// The map-key narrowing is asserted against the REAL corpus fixtures it excludes
	// (BAML succeeds on these finals, native declines, so the native-only lane must
	// reject the SHAPE pre-transport): a non-string map key (enum key 28,
	// union-of-literal key 104). Each must have landed in the declined set above.
	// (The former non-ASCII-literal narrowing is GONE: #555 Slice 2 admits non-ASCII
	// string-literal / enum values and names, so 180/181 now CLAIM — the acceptance
	// loop above proves the admitted non-ASCII fixtures are byte-exact vs live BAML.)
	for _, name := range []string{
		"map_bad_enum_key",
		"map_bad_key_original_order",
	} {
		if !declinedNames[name] {
			t.Errorf("§11 narrowing: corpus fixture %q must be DECLINED pre-transport, but it was not in the declined set", name)
		}
	}
}
