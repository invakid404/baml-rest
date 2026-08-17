//go:build integration && nanollm_integration

package opharness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve"
)

// The /stream half of the route boundary, live, per operator.
//
// The 7.2c scope keeps `/stream` a ZERO-SOCKET decline for the admitted fingerprint,
// and the cutover widened the SHAPE without touching the route set. That is asserted
// at the gate for all 24 rows in internal/debaml (every route gate, both levels), and
// here it is MEASURED on the real generated route for each newly admitted operator.

// StreamSpy wraps the serve func from the PUBLIC nativeserve.NewStaticStream.
type StreamSpy struct {
	fn         bamlutils.NativeStaticStreamServeFunc
	calls      atomic.Int64
	lastDisp   atomic.Int64
	lastStage  atomic.Value // string
	lastReason atomic.Value // string
}

// Serve is the callback the generated stream seam installs.
func (s *StreamSpy) Serve(ctx context.Context, inv bamlutils.NativeStaticStreamInvocation) bamlutils.NativeStreamServeResult {
	s.calls.Add(1)
	res := s.fn(ctx, inv)
	s.lastDisp.Store(int64(res.Disposition))
	s.lastStage.Store(res.Stage)
	s.lastReason.Store(res.Reason)
	return res
}

// Calls is how many times the generated stream seam invoked the serve func.
func (s *StreamSpy) Calls() int64 { return s.calls.Load() }

// LastDecline is the last stream result's disposition, stage and reason.
func (s *StreamSpy) LastDecline() (int64, string, string) {
	st, _ := s.lastStage.Load().(string)
	rs, _ := s.lastReason.Load().(string)
	return s.lastDisp.Load(), st, rs
}

// NativeSocketsZero reports whether the native stream lane opened ZERO provider
// sockets for the last request: either the seam was never installed (callback not
// invoked) OR the callback ran and returned NativeStreamDeclined — a PRE-TRANSPORT
// decline, whose contract is no socket and no EmitDelta. (A claim opens a socket and
// reports Completed/FailedAfterClaim.)
func (s *StreamSpy) NativeSocketsZero() bool {
	return s.calls.Load() == 0 || s.lastDisp.Load() == int64(bamlutils.NativeStreamDeclined)
}

// NewStreamSpy builds a stream spy around a fresh serve func.
func NewStreamSpy(t *testing.T) *StreamSpy {
	t.Helper()
	fn, err := nativeserve.NewStaticStream(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("nativeserve.NewStaticStream: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.NewStaticStream returned a nil serve func")
	}
	return &StreamSpy{fn: fn}
}

// NewSSEServer binds the fixture's FIXED loopback port and returns each event in order
// as one `data: …\n\n` frame, counting accepted requests.
func NewSSEServer(t *testing.T, addr string, events []string) *Server {
	t.Helper()
	ln := listen(t, addr)
	fs := &Server{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fs.count.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	fs.srv = srv
	return fs
}

// StreamConfigurable is the subset of the generated framework adapter the stream row
// needs. Like [Configurable] it is an interface because each fixture's adapter type is
// its own.
type StreamConfigurable interface {
	SetStreamMode(mode bamlutils.StreamMode)
	SetDeBAMLConfig(cfg bamlutils.DeBAMLConfig)
	SetDeBAMLParser(fn bamlutils.DeBAMLParseFunc)
	SetNativeStaticStreamServeComparator(fn bamlutils.NativeStaticStreamServeFunc)
	SetHTTPClient(c *llmhttp.Client)
}

// BuildStreamAdapter configures one fixture's generated adapter for the /stream lane.
func BuildStreamAdapter(
	t *testing.T,
	spy *StreamSpy,
	initRuntime func(),
	makeAdapter func(context.Context) bamlutils.Adapter,
) bamlutils.Adapter {
	t.Helper()
	initRuntime()
	a := makeAdapter(context.Background())
	cfg, ok := a.(StreamConfigurable)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, which is not stream-configurable", a)
	}
	cfg.SetStreamMode(bamlutils.StreamModeStream)
	cfg.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: true})
	cfg.SetDeBAMLParser(debaml.Parse)
	cfg.SetNativeStaticStreamServeComparator(spy.Serve)
	cfg.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// contentSSE renders assistant content chunks as OpenAI streaming frames, terminated
// with [DONE].
func contentSSE(chunks []string) []string {
	out := make([]string, 0, len(chunks)+1)
	for _, c := range chunks {
		out = append(out, `{"choices":[{"delta":{"content":`+quoteJSON(c)+`}}]}`)
	}
	return append(out, "[DONE]")
}

// quoteJSON is a minimal JSON string quoter for the SSE chunks above. The chunks are
// fragments of a JSON document (`{"answer":`, `"sunny",`), so they carry quotes and
// braces but never a control byte — anything outside that is a fixture bug and panics
// rather than being silently escaped into something else.
func quoteJSON(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			out = append(out, '\\', '"')
		case c == '\\':
			out = append(out, '\\', '\\')
		case c < 0x20 || c > 0x7e:
			panic("opharness: SSE chunk carries a byte this quoter does not model: " + s)
		default:
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}

// RunStreamDecline is the /stream half of the route boundary for one operator: the
// EXACT admitted return schema, on the /stream route, opens NO native socket and BAML
// produces the stream.
//
// # The non-vacuity control, and its honest bound
//
// A zero is only evidence if the harness could have produced a one. The main
// staticserve package's stream test uses an ADMITTED STREAM family (the recursive alias
// route) as its control, and these isolated projects have none — they declare exactly
// the two pinned classes and nothing else, which is the property that makes them
// isolated in the first place.
//
// So the control here is the one this package can honestly make: the SAME route, in
// StreamModeCall, claims exactly ONE native socket (that is [RunServedRows]'s subject
// and it is re-measured here so the two are not separated by a test boundary). The
// difference between the two rows is the MODE, so a zero below is the route decision
// rather than a fixture native can never reach. What it does NOT prove is that the
// STREAM seam itself is reachable in this binary — that is proven, once, by the main
// package against an admitted stream family, and it is a property of the seam rather
// than of the predicate.
func RunStreamDecline(t *testing.T, p Project) {
	t.Helper()
	c := Captures[p.OpID]

	// CONTROL: the same route in /call mode claims exactly one socket.
	t.Run("control_call_mode_claims_a_socket", func(t *testing.T) {
		spy := NewSpy(t)
		server := NewServer(t, p.Addr, 200, OpenAIStaticAnswer("sunny", c.TrueVal))
		got := p.DriveCheck(t, BuildAdapter(t, spy, true, bamlutils.StreamModeCall, p.InitRuntime, p.MakeAdapter))
		server.Close()
		if got.Err != nil {
			t.Fatalf("control route: %v", got.Err)
		}
		if n := spy.NativeSockets(t, "on"); n != 1 {
			t.Fatalf("control route native_sockets{on}=%v, want EXACTLY 1; the zero below could not "+
				"distinguish a route decision from a fixture native never reaches", n)
		}
	})

	events := contentSSE([]string{`{"answer":`, `"sunny",`, fmt.Sprintf(`"confidence":%d}`, c.TrueVal)})
	for name, drive := range map[string]func(*testing.T, bamlutils.Adapter) Outcome{
		"check":  p.DriveCheck,
		"assert": p.DriveAssert,
	} {
		t.Run(name, func(t *testing.T) {
			spy := NewStreamSpy(t)
			server := NewSSEServer(t, p.Addr, events)
			got := drive(t, BuildStreamAdapter(t, spy, p.InitRuntime, p.MakeAdapter))
			server.Close()
			if got.Err != nil {
				t.Fatalf("the /stream route errored: %v", got.Err)
			}
			disp, stage, reason := spy.LastDecline()
			if !spy.NativeSocketsZero() {
				t.Fatalf("the /stream route opened a native socket for the admitted %q fingerprint "+
					"(calls=%d disposition=%d stage=%q reason=%q); the 7.2c scope keeps /stream a "+
					"ZERO-SOCKET decline", Expression(p.OpID), spy.Calls(), disp, stage, reason)
			}
			if got.Winner == bamlutils.NativeServeEngineNative {
				t.Errorf("winner_engine=%q on a /stream route that must fall back to BAML", got.Winner)
			}
			// BAML really produced the stream, rather than the route quietly producing
			// nothing at all — which would satisfy "no native socket" for the wrong
			// reason.
			if got.FinalJSON == "" {
				t.Errorf("the declined /stream route produced no final; BAML must supply the result")
			}
			if n := server.Count(); n != 1 {
				t.Errorf("provider saw %d requests, want 1 (one ordinary BAML stream)", n)
			}
		})
	}
	t.Logf("/stream boundary for %q (%s): both families are zero-socket declines; the same routes "+
		"claim one socket each in /call mode", p.OpID, Expression(p.OpID))
}
