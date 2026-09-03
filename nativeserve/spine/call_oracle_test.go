package spine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// TestUnaryExecutorSatisfiesOracleInterface pins that the production executor is the
// optional oracle-capable contract the ExecBridge-U1c standard composite drives, so a
// build-time break is caught here rather than at composite wiring.
func TestUnaryExecutorSatisfiesOracleInterface(t *testing.T) {
	e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
	if err != nil {
		t.Fatalf("newExec: %v", err)
	}
	var _ bamlutils.NativeSpineUnaryOracleExecutor = e
}

// okBuildBAMLRequest / okBAMLOnlyParse are non-nil placeholder oracle callbacks for the
// PRE-SOCKET decline table: every case below declines before admission ever calls them,
// so their bodies are never reached — they exist only so the nil-callback guards do not
// fire for cases meant to decline for a different reason.
func okBuildBAMLRequest(context.Context) (*llmhttp.Request, error) { return nil, nil }
func okBAMLOnlyParse(context.Context, string) ([]byte, error)      { return nil, nil }

// TestCallWithOracle_PreSocketDeclines drives every pre-socket decline arm of the
// oracle Call that does NOT need a real socket/FFI: a registry miss, the request-scoped
// exact-cohort declines (client registry / dynamic schema), the two mandatory
// oracle-callback guards, and a cancelled context. Each MUST be
// NativeSpineDeclinedPreSocket, open ZERO sockets, and carry the bounded stage/reason —
// the fallback-legal outcome the outer composite returns to BAML.
func TestCallWithOracle_PreSocketDeclines(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name      string
		ctx       context.Context
		inv       bamlutils.NativeStaticInvocation
		wantStage string
	}{
		{
			name: "unregistered method",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:           "NoSuchMethod",
				BuildBAMLRequest: okBuildBAMLRequest,
				BAMLOnlyParse:    okBAMLOnlyParse,
			},
			wantStage: "registry",
		},
		{
			name: "client registry override",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:                    jsonAliasMethod,
				HasClientRegistryOverride: true,
				BuildBAMLRequest:          okBuildBAMLRequest,
				BAMLOnlyParse:             okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "dynamic output schema",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:                 jsonAliasMethod,
				HasDynamicOutputSchema: true,
				BuildBAMLRequest:       okBuildBAMLRequest,
				BAMLOnlyParse:          okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "missing BAML plan closure",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:        jsonAliasMethod,
				BAMLOnlyParse: okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "missing BAML parse closure",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:           jsonAliasMethod,
				BuildBAMLRequest: okBuildBAMLRequest,
			},
			wantStage: "admission",
		},
		{
			name: "cancelled context",
			ctx:  cancelled,
			inv: bamlutils.NativeStaticInvocation{
				Method:           jsonAliasMethod,
				BuildBAMLRequest: okBuildBAMLRequest,
				BAMLOnlyParse:    okBAMLOnlyParse,
			},
			wantStage: "preflight",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
			if err != nil {
				t.Fatalf("newExec: %v", err)
			}
			var oracle bamlutils.NativeSpineUnaryOracleExecutor = e
			res := oracle.CallWithOracle(tc.ctx, tc.inv)
			if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
				t.Fatalf("disposition = %v (err %v), want declined_pre_socket", res.Disposition, res.Err)
			}
			if res.Err == nil {
				t.Fatal("declined result carries no typed error")
			}
			if res.Stage != tc.wantStage {
				t.Errorf("stage = %q, want %q (reason %q)", res.Stage, tc.wantStage, res.Reason)
			}
			// A declined oracle result MUST certify zero RoundTrips: no claim, no socket.
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Errorf("declined path opened a socket: sockets=%d claims=%d", snap.Sockets, snap.Claims)
			}
		})
	}
}

// TestCallWithOracle_UnregisteredMethodTypedDecline pins that a registry miss surfaces
// the typed capability-decline (the same value Parse returns), never an ordinary error.
func TestCallWithOracle_UnregisteredMethodTypedDecline(t *testing.T) {
	e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
	if err != nil {
		t.Fatalf("newExec: %v", err)
	}
	res := e.CallWithOracle(context.Background(), bamlutils.NativeStaticInvocation{
		Method:           "NoSuchMethod",
		BuildBAMLRequest: okBuildBAMLRequest,
		BAMLOnlyParse:    okBAMLOnlyParse,
	})
	var unsupported *bamlutils.NativeSpineUnsupportedMethodError
	if !errors.As(res.Err, &unsupported) {
		t.Fatalf("err = %v (%T), want *NativeSpineUnsupportedMethodError", res.Err, res.Err)
	}
}
