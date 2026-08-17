//go:build integration && nanollm_integration

// Package opharness is the shared live-socket harness for the de-BAML Slice 7.2c-3
// ISOLATED OPERATOR fixtures.
//
// # Why the operator proofs are separate test PACKAGES
//
// The cutover admits six direct comparisons on the two PRODUCTION-PINNED class names
// `StaticCheckedAnswer` / `StaticAssertAnswer`. One BAML project cannot declare a
// class twice, and the 7.2c scope forbids renaming the classes to make the variants
// coexist — so each operator has its own generated project
// (internal/nativeprompt/testdata/staticserve_op_fixtures/<op>).
//
// That is necessary but not sufficient, because of a second, harder constraint:
// `baml_go`'s TYPE MAP is PROCESS-GLOBAL and keyed by class NAME. Every generated
// client's InitRuntime calls `baml.SetTypeMap`, so two clients that both declare
// `StaticCheckedAnswer` would overwrite each other's entry — and the flag-OFF BAML
// leg, which is the differential's stock half, decodes THROUGH that map. Linking two
// operator fixtures into one test binary would therefore make one of them decode into
// the other's Go type.
//
// A Go test binary is per PACKAGE, so the isolation that fixes it is one package per
// operator. Each links exactly one generated client, calls exactly one InitRuntime,
// and owns exactly one type-map registration. This package holds everything those
// packages share, so the five of them are wiring rather than five copies of a proof.
//
// Each operator project also bakes its OWN loopback port, so the packages can run
// concurrently (which `go test ./...` does) and a captured request is attributable to
// the project that received it.
package opharness

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve"
)

// Spy wraps the serve func from the PUBLIC nativeserve.NewStaticServe, counting
// generated-seam invocations and exposing the metrics registry so a test can read
// native_sockets{flag=on} to distinguish a native send from a BAML send.
//
// It is the same shape the main staticserve package's spy has, and deliberately so:
// the operator rows have to be judged by the same evidence the `>` rows are.
type Spy struct {
	fn         bamlutils.NativeStaticServeFunc
	calls      atomic.Int64
	reg        *prometheus.Registry
	lastStage  atomic.Value // string
	lastReason atomic.Value // string
	lastDisp   atomic.Int64
}

// Serve is the callback the generated /call seam installs.
func (s *Spy) Serve(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
	s.calls.Add(1)
	res := s.fn(ctx, inv)
	s.lastStage.Store(res.Stage)
	s.lastReason.Store(res.Reason)
	s.lastDisp.Store(int64(res.Disposition))
	return res
}

// Calls is how many times the generated seam invoked the serve func.
func (s *Spy) Calls() int64 { return s.calls.Load() }

// LastDecline is the last serve result's disposition, stage and reason — reported in
// a failure message so a zero-socket row says WHY it declined rather than only that
// it did.
func (s *Spy) LastDecline() (int64, string, string) {
	st, _ := s.lastStage.Load().(string)
	rs, _ := s.lastReason.Load().(string)
	return s.lastDisp.Load(), st, rs
}

// NewSpy builds a spy around a fresh serve func and its own metrics registry.
func NewSpy(t *testing.T) *Spy {
	t.Helper()
	reg := prometheus.NewRegistry()
	fn, err := nativeserve.NewStaticServe(reg)
	if err != nil {
		t.Fatalf("nativeserve.NewStaticServe: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.NewStaticServe returned a nil serve func")
	}
	return &Spy{fn: fn, reg: reg}
}

// NativeSockets reads native_sockets{flag=<flag>} out of this spy's own registry.
//
// It is the decisive measurement for every row here: a SERVED row must show exactly
// one, and a DECLINED row exactly zero. Reading it from a per-spy registry rather
// than a global one is what makes the count attributable to this call.
func (s *Spy) NativeSockets(t *testing.T, flag string) float64 {
	t.Helper()
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != "baml_rest_debaml_native_sockets_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "flag" && lp.GetValue() == flag {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// Server is the loopback capture server bound to a fixture's baked base_url.
//
// The caller MUST Close() it before binding the same fixed port again (each row binds
// one), so it does NOT auto-register a t.Cleanup.
type Server struct {
	srv    *httptest.Server
	count  atomic.Int64
	closed atomic.Bool
}

// Count is how many provider requests the server saw.
func (fs *Server) Count() int64 { return fs.count.Load() }

// Close shuts the server down and frees the fixed port. Idempotent.
func (fs *Server) Close() {
	if fs != nil && fs.srv != nil && fs.closed.CompareAndSwap(false, true) {
		fs.srv.Close()
	}
}

// NewServer binds the fixture's FIXED loopback port and serves one canned response.
//
// Bind failure is FATAL, never a silent Skip: these are the pivotal live proofs of the
// cutover and must not green-by-skip on a busy runner. A brief retry absorbs a
// lingering TIME_WAIT from the previous row, so only a genuine external collision
// fails here.
func NewServer(t *testing.T, addr string, status int, body []byte) *Server {
	t.Helper()
	ln := listen(t, addr)
	fs := &Server{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fs.count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	fs.srv = srv
	return fs
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cannot bind fixed fixture loopback %s after retries: %v (a genuine external collision, "+
		"not a skip)", addr, lastErr)
	return nil
}

// OpenAIStaticAnswer returns an OpenAI-shaped 2xx whose assistant content is the
// flattened `{answer, confidence}` JSON the pinned classes decode.
func OpenAIStaticAnswer(answer string, confidence int64) []byte {
	inner, _ := json.Marshal(map[string]any{"answer": answer, "confidence": confidence})
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": string(inner)}}},
	})
	return env
}

// Configurable is the subset of the generated framework adapter [BuildAdapter] needs.
//
// It is an interface because every fixture's `generated/adapter` package declares its
// OWN `*BamlAdapter` type — five distinct Go types with the same method set — so a
// concrete type assertion cannot be shared. The methods are named individually rather
// than accepting `any` and reflecting, so a generated adapter that lost one of them
// fails to compile here instead of failing to configure at run time.
type Configurable interface {
	SetStreamMode(mode bamlutils.StreamMode)
	SetDeBAMLConfig(cfg bamlutils.DeBAMLConfig)
	SetNativeStaticServeComparator(fn bamlutils.NativeStaticServeFunc)
	SetHTTPClient(c *llmhttp.Client)
}

// BuildAdapter configures one fixture's generated framework adapter for a live row:
// the flag, the installed serve callback, a loopback-allowing HTTP client (for BAML's
// flag-off / decline-path send), and the stream mode.
//
// initRuntime is the fixture's own Once-guarded `baml_client.InitRuntime`. Calling it
// here rather than in each package's test body keeps the ONE type-map registration
// this binary performs in one place — see the package doc for why that matters.
func BuildAdapter(
	t *testing.T,
	spy *Spy,
	flagOn bool,
	mode bamlutils.StreamMode,
	initRuntime func(),
	makeAdapter func(context.Context) bamlutils.Adapter,
) bamlutils.Adapter {
	t.Helper()
	initRuntime()
	a := makeAdapter(context.Background())
	cfg, ok := a.(Configurable)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, which is not configurable", a)
	}
	cfg.SetStreamMode(mode)
	cfg.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	cfg.SetNativeStaticServeComparator(spy.Serve)
	// No runtime client registry: the request uses the descriptor's BAKED default
	// client (this project's StaticOracleClient, on its own loopback port), so it is a
	// single default-client request with NO client override — which the narrow static
	// surface admits. A runtime registry, even one identical to the default, is
	// surfaced as an unproven override and declines.
	cfg.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// Outcome is one drained /call: the marshalled final, the outcome tokens and any
// error.
type Outcome struct {
	FinalJSON       string
	Winner, Planned string
	Err             error
}
