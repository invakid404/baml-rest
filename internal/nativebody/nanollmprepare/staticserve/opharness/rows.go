//go:build integration && nanollm_integration

package opharness

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// The four LIVE rows one operator contributes to the Slice 7.2c-3 served manifest,
// and the assertions each has to pass.
//
// The whole manifest is 6 operators x 4 outcomes = 24 rows. `>` is proven by the main
// staticserve package (TestConstraintRoutes_FlagOnServesNative, unchanged); the other
// five are proven by the five packages that call [RunServedRows], one per isolated
// project.

// Project is one isolated operator fixture, as the shared runner needs it.
//
// Everything package-specific is a function value, because each fixture's generated
// client is a DIFFERENT Go package with its own types: its own `MakeAdapter`, its own
// `InitRuntime`, and its own `StaticCheckedConfidence`/`StaticAssertConfidence`
// signatures. What is shared is the proof.
type Project struct {
	// OpID is the operator's capability ID (`ge`, `lt`, `le`, `eq`, `ne`), which is
	// also the key its stock capture is filed under.
	OpID string
	// Addr is this project's OWN baked loopback `host:port`. Distinct per project so
	// the five packages can run concurrently and a captured request is attributable to
	// the project that received it.
	Addr string
	// InitRuntime is the fixture's Once-guarded baml_client.InitRuntime.
	InitRuntime func()
	// MakeAdapter is the fixture's generated MakeAdapter.
	MakeAdapter func(ctx context.Context) bamlutils.Adapter
	// DriveCheck / DriveAssert invoke this project's generated /call for the check and
	// assert families and drain the stream.
	DriveCheck  func(t *testing.T, a bamlutils.Adapter) Outcome
	DriveAssert func(t *testing.T, a bamlutils.Adapter) Outcome
}

// transportParseErrPrefix is the ONE wrapper the generated /call adds to a final-parse
// failure before a caller sees it.
//
// It belongs to buildrequest, not to stock, and it is present on BOTH legs of the
// differential — so it is pinned here and stripped exactly, which keeps the comparison
// against stock's capture byte-exact on the part stock actually owns. If the wrapper
// ever changes, this constant is where it is noticed rather than a place where a looser
// match quietly absorbed it.
const transportParseErrPrefix = "buildrequest: failed to parse final result: "

// Row is one of the four serving-shaped outcomes.
type Row struct {
	Name string
	// Confidence is the value the provider returns, taken from the operator's own
	// stock capture — NOT a constant, because the six operators hold at different
	// values and a fixed 9 would silently turn `this < 0` into a different outcome.
	Confidence int64
	// Assert selects the @assert family.
	Assert bool
	// WantErr records that this row's predicate is a FALSE @assert, which stock
	// rejects with no value at all.
	WantErr bool
	// WantBytes / WantErrText are stock's own output for this row.
	WantBytes   string
	WantErrText string
}

// Rows is the four rows of one operator, built from its stock capture.
//
// Deriving them from the capture is what keeps `Confidence` and `WantBytes` in step: a
// row cannot claim stock's bytes while driving a value stock was never given.
func Rows(opID string) []Row {
	c := Captures[opID]
	return []Row{
		{Name: "check_pass", Confidence: c.TrueVal, WantBytes: c.CheckTrue},
		{Name: "check_fail", Confidence: c.FalseVal, WantBytes: c.CheckFalse},
		{Name: "assert_pass", Confidence: c.TrueVal, Assert: true, WantBytes: c.AssertTrue},
		{Name: "assert_fail", Confidence: c.FalseVal, Assert: true, WantErr: true, WantErrText: c.AssertFail},
	}
}

// RunServedRows is the LIVE admission proof for one operator: all four of its
// serving-shaped outcomes, flag ON and flag OFF, over the same provider bytes.
//
// Per row it requires:
//
//	FLAG OFF  the serve callback is NEVER invoked, ZERO native sockets, and the
//	          provider sees exactly ONE request — BAML alone builds, sends and parses.
//	          This leg is the differential's stock half.
//	FLAG ON   the serve callback runs exactly ONCE, planned_engine == "native", exactly
//	          ONE native socket, the provider sees exactly ONE request (no hidden BAML
//	          resend), and the bytes (or the error text) EQUAL the flag-off leg's.
//	BOTH      what the two legs agree on is what STOCK produced: the bytes are compared
//	          against the 7.2c-1 CFFI capture as well, so an error that moved both legs
//	          cannot cancel out.
func RunServedRows(t *testing.T, p Project) {
	t.Helper()
	if _, ok := Captures[p.OpID]; !ok {
		t.Fatalf("operator %q has no stock capture; its live rows would rest on nothing", p.OpID)
	}
	expr := Expression(p.OpID)
	served, claimed := 0, 0
	for _, row := range Rows(p.OpID) {
		t.Run(row.Name, func(t *testing.T) {
			body := OpenAIStaticAnswer("sunny", row.Confidence)
			drive := p.DriveCheck
			if row.Assert {
				drive = p.DriveAssert
			}

			// ---- FLAG OFF: the unchanged full-BAML route, no native call at all ----
			offSpy := NewSpy(t)
			offServer := NewServer(t, p.Addr, 200, body)
			off := drive(t, BuildAdapter(t, offSpy, false, bamlutils.StreamModeCall, p.InitRuntime, p.MakeAdapter))
			offServer.Close()
			if got := offSpy.Calls(); got != 0 {
				t.Fatalf("flag OFF invoked the serve func %d times, want 0 (hard-off); this leg is the "+
					"stock half of the differential and must not involve native at all", got)
			}
			if got := offSpy.NativeSockets(t, "on"); got != 0 {
				t.Fatalf("flag OFF opened %v native sockets, want 0", got)
			}
			if got := offServer.Count(); got != 1 {
				t.Fatalf("flag OFF: provider saw %d requests, want exactly 1 (BAML serves)", got)
			}
			if off.Planned != "" {
				t.Errorf("flag OFF planned_engine = %q, want empty", off.Planned)
			}

			// ---- FLAG ON: native admits, opens ONE socket and serves ----
			onSpy := NewSpy(t)
			onServer := NewServer(t, p.Addr, 200, body)
			on := drive(t, BuildAdapter(t, onSpy, true, bamlutils.StreamModeCall, p.InitRuntime, p.MakeAdapter))
			onServer.Close()
			if got := onSpy.Calls(); got != 1 {
				t.Fatalf("the native serve func was invoked %d times, want exactly 1", got)
			}
			// planned_engine rides the OUTCOME metadata, which a claimed FAILURE never
			// reaches (the error short-circuits the emission) — for that row the socket
			// count below is what proves native claimed the attempt, and it is a stronger
			// witness than a metadata token anyway.
			if !row.WantErr && on.Planned != "native" {
				t.Errorf("planned_engine = %q, want native", on.Planned)
			}
			disp, stage, reason := onSpy.LastDecline()
			if got := onSpy.NativeSockets(t, "on"); got != 1 {
				t.Fatalf("native_sockets{flag=on} = %v, want 1 (the admitted %q fingerprint must CLAIM "+
					"a socket); serve disposition=%d stage=%q reason=%q", got, expr, disp, stage, reason)
			}
			if got := onServer.Count(); got != 1 {
				t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
			}

			// ---- The differential itself ----
			if row.WantErr {
				if on.Err == nil {
					t.Fatalf("a FALSE @assert SERVED %s; stock emits no value at all", on.FinalJSON)
				}
				if off.Err == nil {
					t.Fatalf("the flag-off (BAML) leg did not reject the FALSE @assert; it returned %s, "+
						"so the comparison below would have no stock half", off.FinalJSON)
				}
				if on.Err.Error() != off.Err.Error() {
					t.Fatalf("native and BAML disagree on the assertion error bytes:\n native %q\n BAML   %q",
						on.Err.Error(), off.Err.Error())
				}
				if on.FinalJSON != "" {
					t.Fatalf("the failed assert produced BOTH an error and a final: %s", on.FinalJSON)
				}
				// And what they agree on is STOCK's, byte for byte — under the ONE
				// wrapper the transport adds.
				//
				// The generated /call surfaces a final-parse failure through
				// buildrequest, which prefixes its own bounded token. That prefix is the
				// TRANSPORT's, not stock's, and it is on BOTH legs (the equality above
				// already proved the two error strings are identical), so it is stripped
				// by an exact prefix match rather than by a substring search — a
				// contains() check would accept the capture appearing anywhere, including
				// inside a longer error that had also changed.
				got := on.Err.Error()
				if !strings.HasPrefix(got, transportParseErrPrefix) {
					t.Fatalf("the served assertion error does not carry the expected transport prefix "+
						"%s:\n got %s", strconv.Quote(transportParseErrPrefix), strconv.Quote(got))
				}
				if inner := strings.TrimPrefix(got, transportParseErrPrefix); inner != row.WantErrText {
					t.Fatalf("the served assertion error is not the stock CFFI capture:\n got  %s\n want %s",
						strconv.Quote(inner), strconv.Quote(row.WantErrText))
				}
				// A CLAIMED failure, not a hidden resend: exactly one provider request was
				// already asserted above, and the winner must not be native's success token.
				if on.Winner == bamlutils.NativeStaticServeEngineNative {
					t.Errorf("winner_engine = %q on a claimed parse failure", on.Winner)
				}
				claimed++
				return
			}

			if on.Err != nil {
				t.Fatalf("the admitted route errored: %v", on.Err)
			}
			if off.Err != nil {
				t.Fatalf("the flag-off (BAML) leg errored: %v; the comparison would have no stock half", off.Err)
			}
			if on.FinalJSON == "" {
				t.Fatal("the admitted route produced no final")
			}
			if on.FinalJSON != off.FinalJSON {
				t.Fatalf("native and BAML disagree on the served bytes:\n native %s\n BAML   %s",
					on.FinalJSON, off.FinalJSON)
			}
			if on.FinalJSON != row.WantBytes {
				t.Fatalf("the served bytes are not the stock CFFI capture:\n got  %s\n want %s",
					on.FinalJSON, row.WantBytes)
			}
			// NATIVE really produced them. `native_baml_parse` would mean native claimed
			// the socket and then fell back to BAML's parse of the same bytes — a real
			// outcome, and not the one this row is about.
			if on.Winner != bamlutils.NativeStaticServeEngineNative {
				t.Errorf("winner_engine = %q, want %q (a structured native win)",
					on.Winner, bamlutils.NativeStaticServeEngineNative)
			}
			// The predicate that reached the wire is THIS project's, not another's. The
			// assert_pass row carries no expression at all (a passing assert leaves no
			// trace), so it is exempt — and that exemption is the capture's own shape,
			// not a weakened assertion.
			if row.Name != "assert_pass" && !strings.Contains(on.FinalJSON, expr) {
				t.Errorf("the served bytes do not quote this project's predicate %q: %s", expr, on.FinalJSON)
			}
			served++
		})
	}
	if served != 3 || claimed != 1 {
		t.Fatalf("operator %q produced %d served rows and %d claimed failures, want 3 and 1",
			p.OpID, served, claimed)
	}
	t.Logf("LIVE served manifest for %q (%s): 4 rows — %d served with ONE native socket each, "+
		"%d claimed assertion failure; every row equals its flag-off BAML leg AND the 7.2c-1 stock "+
		"CFFI capture", p.OpID, expr, served, claimed)
}

// SeamProbe reports what one fixture's INTROSPECTED artifact says about a route: did
// the descriptor extractor emit a V3 descriptor and an argument projector for it, and
// did it record a build-time decline?
//
// It is a function rather than the introspected package itself because each fixture has
// its own `introspected` package with its own package-level tables — five distinct Go
// packages with the same names.
type SeamProbe func(route string) (hasDescriptor, hasProjector bool, declineReason string, declined bool)

// RequireSeamEmitted is the BUILD-TIME half of the live proof, and it is what makes the
// socket counts mean something.
//
// If a route carried no descriptor, its live result would witness an UN-EMITTED SEAM
// rather than an admission decision — a zero that says nothing and a one that could not
// happen. Every route of an isolated operator project must therefore carry a descriptor
// AND a projector AND no build-time decline, so admission is the only thing left to
// decide it.
//
// It also re-states the codegen contract the cutover leaves untouched: codegen is
// SCHEMA-BLIND. It emitted the seam for these routes without knowing anything about
// their predicates, exactly as it does for every other static method, and the widened
// manifest changed nothing here. Slice 7.2c-3 adds NO codegen admission gate.
func RequireSeamEmitted(t *testing.T, opID string, routes []string, probe SeamProbe) {
	t.Helper()
	if len(routes) == 0 {
		t.Fatal("no routes were named; the build-time half would be vacuous")
	}
	for _, route := range routes {
		hasDescriptor, hasProjector, reason, declined := probe(route)
		if !hasDescriptor {
			t.Errorf("%s: NO V3 descriptor was emitted; its live result would witness an un-emitted "+
				"seam rather than an admission decision", route)
		}
		if !hasProjector {
			t.Errorf("%s: no argument projector was emitted", route)
		}
		if declined {
			t.Errorf("%s: a BUILD-TIME decline was recorded (%q); this route must reach admission",
				route, reason)
		}
	}
	t.Logf("seam for %q: %d routes carry a V3 descriptor and a projector with no build-time decline, "+
		"so admission alone decides them", opID, len(routes))
}
