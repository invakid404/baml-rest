//go:build integration && nanollm_integration

package listserve

// Scope §3.A proofs 2 and 3 for the required scalar-LIST input widening.
//
//	proof 2 — the stock v0.223 NO-SEND `Request` / `StreamRequest` differential over
//	          exactly the typed cross-product: native's own prepared plan must equal
//	          BAML's, which is exactly what the production pre-claim comparator
//	          decides, and the request that actually reaches the provider must carry
//	          BAML's bytes.
//	proof 3 — the unary serve differential: identical captured OpenAI responses
//	          through the REAL standard composite; NATIVE must WIN on every positive
//	          row, exactly one provider request, and the public final bytes must equal
//	          BAML's own `Parse` of the same bytes.
//
// A row that passed only because the request fell back to BAML would show
// claims=0/sockets=0 and a non-native winner, and every positive test here fails on
// that — which is the point: a fallback-green fixture is not admission evidence.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinelistfixture"
	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// unaryInv assembles the exact-cohort invocation the generated /call seam builds,
// with the BAML closures backed by the stock runtime.
func unaryInv(t *testing.T, leg *bamlLeg, r argRow) bamlutils.NativeStaticInvocation {
	t.Helper()
	return bamlutils.NativeStaticInvocation{
		Method:     nativespine.ListArgsFixtureMethod,
		Args:       r.args(),
		ArgOrder:   argOrder(),
		Values:     r.values(t),
		Mode:       bamlutils.NativeStaticModeFinal,
		Provider:   "openai",
		SingleLeaf: true,
		// The live no-send BAML plan the spine compares against before claiming.
		BuildBAMLRequest: leg.buildRequestFn(t, r, false),
		// BAML's own `Parse.<Method>` over the SAME response bytes, marshalled the
		// way the generated seam marshals it (json.Marshal of the parsed value).
		BAMLOnlyParse: func(ctx context.Context, raw string) ([]byte, error) {
			v, err := leg.parse(ctx, t, r, raw)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		DecodeNativeFinal: nativespinelistfixture.Binding().DecodeFinal,
	}
}

// TestUnaryComposite_NativeWinsOnEveryScalarListRow is the headline unary proof.
//
// For every point of the typed cross-product it drives the REAL standard /call
// composite over a REAL spine executor and a loopback provider, and requires:
//
//   - the pre-claim LIVE plan compare against stock BAML MATCHED (claims=1) — which
//     IS the no-send `Request` differential, decided by the production comparator;
//   - exactly ONE provider request (native's, no BAML resend);
//   - the wire request carries BAML's method, URL and BODY BYTES, and every semantic
//     header BAML's plan declares — an independent reading of the same claim;
//   - NATIVE won the same-response comparison against BAML's own `Parse`;
//   - the public final bytes equal BAML's `Parse` of the identical response.
func TestUnaryComposite_NativeWinsOnEveryScalarListRow(t *testing.T) {
	const content = `[1,"x",true]`
	for _, r := range argRows() {
		t.Run(r.name, func(t *testing.T) {
			server := newJSONServer(t, content)
			leg := newBAMLLeg(t, server.baseURL())
			serve, exec := unaryComposite(t, server.baseURL())

			res := serve(context.Background(), unaryInv(t, leg, r))

			if res.Disposition != bamlutils.NativeStaticServeSucceeded {
				t.Fatalf("disposition = %v (stage=%q reason=%q err=%v); the scalar-list cohort must be SERVED natively, "+
					"and a row that only falls back to BAML is not admission evidence",
					res.Disposition, res.Stage, res.Reason, res.Err)
			}
			if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
				t.Fatalf("winner = %q, want %q — native and BAML disagreed on the same response bytes",
					res.WinnerEngine, bamlutils.NativeStaticServeEngineNative)
			}
			if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 || snap.Declines != 0 {
				t.Fatalf("executor counters = %+v, want claims=1 sockets=1 successes=1 declines=0 "+
					"(a claim is what proves the live no-send BAML plan compare MATCHED)", snap)
			}
			if got := server.hits.Load(); got != 1 {
				t.Fatalf("the provider saw %d request(s), want exactly 1", got)
			}

			// --- the no-send plan differential, read independently of the comparator
			plan := leg.buildRequest(t, r, false)
			wire := server.captured(t)
			if wire.method != plan.Method {
				t.Errorf("method: wire=%q baml=%q", wire.method, plan.Method)
			}
			if wire.body != plan.Body {
				t.Errorf("body bytes differ from stock BAML's no-send plan (wire_len=%d baml_len=%d); "+
					"the host rendering of the scalar lists is not byte-identical", len(wire.body), len(plan.Body))
			}
			if got, want := wire.url, planPath(t, plan.URL); got != want {
				t.Errorf("request path: wire=%q baml=%q", got, want)
			}
			assertSemanticHeadersOnTheWire(t, plan.Headers, wire.headers)

			// --- the public final bytes, compared against BAML's own Parse
			bamlValue, err := leg.parse(context.Background(), t, r, content)
			if err != nil {
				t.Fatalf("stock BAML Parse of the captured response failed: %v", err)
			}
			bamlBytes, err := json.Marshal(bamlValue)
			if err != nil {
				t.Fatalf("marshal BAML's parsed value: %v", err)
			}
			if string(res.FinalJSON) != string(bamlBytes) {
				t.Errorf("public final bytes: native=%s baml=%s", res.FinalJSON, bamlBytes)
			}
		})
	}
}

// TestStreamRequestPlanMatchesOnEveryScalarListRow is proof 2's STREAM half: the
// stock `StreamRequest` no-send plan must equal native's prepared STREAM plan for
// the same typed values. It is decided by the production pre-claim comparator — a
// claim on the stream lane is only reachable when the plans matched — and read
// independently from the wire.
//
// It is kept separate from the streaming behaviour proof so a plan divergence is
// attributable to the plan rather than to a cadence or parse difference.
func TestStreamRequestPlanMatchesOnEveryScalarListRow(t *testing.T) {
	for _, r := range argRows() {
		t.Run(r.name, func(t *testing.T) {
			server := newSSEServer(t, listStreamCorpus())
			leg := newBAMLLeg(t, server.baseURL())
			serve, exec := streamComposite(t, server.baseURL())

			collector := &eventCollector{}
			res := serve(context.Background(), streamInv(t, leg, r, bamlutils.NativeStreamModeStream), collector.emit)
			if res.Disposition != bamlutils.NativeStaticStreamOracleSucceeded {
				t.Fatalf("disposition = %v (stage=%q reason=%q err=%v); the stream plan compare must MATCH for the scalar-list cohort",
					res.Disposition, res.Stage, res.Reason, res.Err)
			}
			if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 {
				t.Fatalf("executor counters = %+v, want claims=1 sockets=1", snap)
			}

			plan := leg.buildRequest(t, r, true)
			wire := server.captured(t)
			if wire.method != plan.Method {
				t.Errorf("method: wire=%q baml=%q", wire.method, plan.Method)
			}
			if wire.body != plan.Body {
				t.Errorf("stream body bytes differ from stock BAML's no-send StreamRequest plan (wire_len=%d baml_len=%d)",
					len(wire.body), len(plan.Body))
			}
			if got, want := wire.url, planPath(t, plan.URL); got != want {
				t.Errorf("request path: wire=%q baml=%q", got, want)
			}
			assertSemanticHeadersOnTheWire(t, plan.Headers, wire.headers)

			// The two plans are NOT the same request: the streaming one must ask the
			// provider to stream. Without this the test would pass while comparing the
			// unary plan to itself.
			unary := leg.buildRequest(t, r, false)
			if unary.Body == plan.Body {
				t.Fatal("stock BAML built an identical body for Request and StreamRequest; the stream half of this differential would be vacuous")
			}
			if !strings.Contains(plan.Body, `"stream":true`) {
				t.Errorf("the StreamRequest body does not ask the provider to stream (len=%d)", len(plan.Body))
			}
		})
	}
}

// TestNilAndEmptyListsRenderIdenticallyInStockBAML pins the nil-vs-empty answer at
// the RENDERING boundary, where the projectors' shared "a required nil slice is an
// empty list" decision becomes observable: two rows whose only difference is
// `nil` vs `[]` must produce the same stock BAML request body once the differing
// scalar is held equal.
//
// The projector-level half of this claim lives in the composed cross-product
// differential; this is the half that says stock BAML agrees.
func TestNilAndEmptyListsRenderIdenticallyInStockBAML(t *testing.T) {
	server := newJSONServer(t, `[1]`)
	leg := newBAMLLeg(t, server.baseURL())

	empty := argRow{name: "empty", topic: "same", tags: []string{}, counts: []int64{}, flags: []bool{}}
	nilled := argRow{name: "nil", topic: "same"}

	a := leg.buildRequest(t, empty, false)
	b := leg.buildRequest(t, nilled, false)
	if a.Body != b.Body {
		t.Fatalf("stock BAML rendered an empty slice and a nil slice differently (empty_len=%d nil_len=%d); "+
			"the projectors collapse both to an empty StaticList, so the two legs would diverge",
			len(a.Body), len(b.Body))
	}
	// NON-VACUITY: a body that carried neither list would also compare equal.
	nonEmpty := argRow{name: "nonempty", topic: "same", tags: []string{"t"}, counts: []int64{1}, flags: []bool{true}}
	if c := leg.buildRequest(t, nonEmpty, false); c.Body == a.Body {
		t.Fatal("a populated list produced the same body as an empty one; the list values are not reaching the rendered prompt")
	}
}

// planPath is BAML's plan URL reduced to the path+query a captured server request
// reports, so the two are comparable without asserting the loopback host twice.
func planPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse BAML plan URL: %v", err)
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// assertSemanticHeadersOnTheWire requires every SEMANTIC header of BAML's plan to
// reach the provider with the same value.
//
// It is deliberately one-directional. testutil.Diff's header comparison is a set
// equality between two PLANS; the wire additionally carries the Go transport's own
// headers (Host, User-Agent, Content-Length, Accept-Encoding), so a set equality
// against it would fail for reasons that have nothing to do with parity. The
// BAML-internal `baml-original-url` exemption is applied through the same
// testutil.SplitSemantic the plan comparator uses, so the exemption is not
// re-stated here.
func assertSemanticHeadersOnTheWire(t *testing.T, planHeaders, wire map[string]string) {
	t.Helper()
	normalized, err := testutil.NormalizeHeaders(testutil.PairsFromStringMap(planHeaders))
	if err != nil {
		t.Fatalf("normalize BAML plan headers: %v", err)
	}
	semantic, _ := testutil.SplitSemantic(normalized)
	if len(semantic) == 0 {
		t.Fatal("BAML's plan carried no semantic headers; the header comparison would be vacuous")
	}
	for name, want := range semantic {
		got, ok := wire[name]
		if !ok {
			t.Errorf("header %q: present on BAML's plan (value %s) but absent from the wire", name, testutil.RedactValue(name, want))
			continue
		}
		if got != want {
			t.Errorf("header %q: value differs (baml=%s wire=%s)", name, testutil.RedactValue(name, want), testutil.RedactValue(name, got))
		}
	}
	if auth, ok := testutil.Authorization(semantic); !ok || auth == "" {
		t.Error("BAML's plan carried no authorization header; the credential half of the comparison would be vacuous")
	}
}
