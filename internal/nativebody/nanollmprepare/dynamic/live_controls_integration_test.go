//go:build integration && nanollm_integration

package dynamic

// De-BAML Slice 6c negative + error controls for the BAML-as-served live
// differential (live_differential_integration_test.go). Each control must FAIL
// as a negative — or route an error case to the intended non-SAP outcome — so the
// positive parity above stays meaningful:
//
//   - flag-off: an explicit BAML_REST_USE_DEBAML falsy value makes the native leg
//     perform ZERO sends (the umbrella switch, no new flag);
//   - mutated method / URL / header / body: the wire comparator flags each, and
//     no fake secret leaks into the redacted diagnostic;
//   - mutated response: serving DIFFERENT bodies to the two legs makes the final
//     structured output diverge (each leg still sends exactly once);
//   - non-2xx (429): both legs fail as a provider error with the same status
//     class, native SAP is NOT invoked, and neither leg makes a second request;
//   - invalid-2xx (200 + non-JSON): the native leg maps it to the InvalidBody
//     provider outcome (SAP NOT invoked), the oracle leg fails to parse, and
//     neither leg makes a second request.

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/execute"
)

// runBothLegs is the shared control driver: it programs the server response, runs
// the oracle leg then the native leg against ONE loopback capture server, and
// returns both observations plus the per-leg request counts. It asserts nothing
// itself — each control interprets the results.
type bothLegs struct {
	server           *liveCaptureServer
	oracleSpy        *liveParseSpy
	oracleData       []byte
	oracleErr        error
	oracleRec        recordedLive
	countAfterOracle int

	res              *execute.AttemptResult
	spy              *liveParseSpy
	ran              bool
	nativeErr        error
	nativeRec        recordedLive
	countAfterNative int
}

func runBothLegs(t *testing.T, status int, body []byte, tc dynFixture) *bothLegs {
	t.Helper()
	server := newLiveCaptureServer(t)
	server.setResponse(status, body)

	b := &bothLegs{server: server}
	b.oracleSpy = &liveParseSpy{fn: debaml.Parse}
	oracle := newLiveOracleClient(t, loopbackOracleHTTPClient(), b.oracleSpy)
	nano := newLiveNativeClient(t, server.base())

	b.oracleData, b.oracleErr = runOracleLeg(t, oracle, server.base(), tc)
	b.countAfterOracle = server.count()
	if b.countAfterOracle >= 1 {
		b.oracleRec = server.rec(0)
	}

	b.res, b.spy, b.ran, b.nativeErr = runNativeLeg(t, nano, server.base(), tc)
	b.countAfterNative = server.count()
	if b.countAfterNative >= 2 {
		b.nativeRec = server.rec(1)
	}
	return b
}

// TestLiveFlagOffZeroNativeSends: with the umbrella switch explicitly falsy the
// native leg makes ZERO sends, while the oracle leg (de-BAML false regardless)
// still sends exactly once.
func TestLiveFlagOffZeroNativeSends(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "off")

	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	oracleSpy := &liveParseSpy{fn: debaml.Parse}
	oracle := newLiveOracleClient(t, loopbackOracleHTTPClient(), oracleSpy)
	nano := newLiveNativeClient(t, server.base())
	tc := dynFixtureByName(t, "single_user_message")

	if _, oerr := runOracleLeg(t, oracle, server.base(), tc); oerr != nil {
		t.Fatalf("oracle leg failed: %v", oerr)
	}
	if got := server.count(); got != 1 {
		t.Fatalf("oracle leg made %d requests, want 1", got)
	}

	res, spy, ran, nerr := runNativeLeg(t, nano, server.base(), tc)
	if ran {
		t.Fatal("native leg ran with BAML_REST_USE_DEBAML off; it must produce zero native sends")
	}
	if res != nil || spy != nil || nerr != nil {
		t.Errorf("flag-off native leg returned res=%v spy=%v err=%v, want all nil", res != nil, spy != nil, nerr)
	}
	if got := server.count(); got != 1 {
		t.Errorf("server saw %d requests after a flag-off native leg, want 1 (no native send)", got)
	}
	if oracleSpy.calls != 0 {
		t.Errorf("oracle native-parser spy fired %d times, want 0", oracleSpy.calls)
	}
}

// TestLiveWireMutationControls proves the wire comparator is sensitive to each
// mutated field and never leaks a secret into diagnostics. Mutations are applied
// to COPIES of a genuine captured pair, so a real parity pass is the baseline.
func TestLiveWireMutationControls(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "1")
	b := runBothLegs(t, http.StatusOK, openAISuccess(`{"answer":"ok"}`), dynFixtureByName(t, "single_user_message"))
	if b.oracleErr != nil || b.nativeErr != nil || !b.ran {
		t.Fatalf("baseline legs failed: oracleErr=%v nativeErr=%v ran=%v", b.oracleErr, b.nativeErr, b.ran)
	}
	if b.countAfterOracle != 1 || b.countAfterNative != 2 {
		t.Fatalf("baseline request counts: oracle=%d total=%d, want 1 and 2", b.countAfterOracle, b.countAfterNative)
	}
	// Baseline parity must hold.
	if diffs := liveWireDiffs(b.oracleRec, b.nativeRec); len(diffs) != 0 {
		t.Fatalf("baseline wire parity unexpectedly differs: %v", diffs)
	}

	t.Run("method", func(t *testing.T) {
		bad := b.nativeRec
		bad.Method = http.MethodGet
		if len(liveWireDiffs(b.oracleRec, bad)) == 0 {
			t.Fatal("a mutated method was not detected")
		}
	})
	t.Run("target", func(t *testing.T) {
		bad := b.nativeRec
		bad.RequestURI = b.nativeRec.RequestURI + "/extra"
		if len(liveWireDiffs(b.oracleRec, bad)) == 0 {
			t.Fatal("a mutated request target was not detected")
		}
	})
	t.Run("header", func(t *testing.T) {
		bad := b.nativeRec
		bad.Header = b.nativeRec.Header.Clone()
		bad.Header.Set("Authorization", "Bearer tampered-key")
		diffs := liveWireDiffs(b.oracleRec, bad)
		if len(diffs) == 0 {
			t.Fatal("a mutated authorization value was not detected")
		}
		for _, s := range diffs {
			if bytes.Contains([]byte(s), []byte("tampered-key")) || bytes.Contains([]byte(s), []byte(fenceAPIKey)) {
				t.Fatalf("authorization value leaked into diagnostics: %q", s)
			}
		}
	})
	t.Run("body", func(t *testing.T) {
		bad := b.nativeRec
		bad.Body = append([]byte(nil), b.nativeRec.Body...)
		bad.Body[len(bad.Body)/2] ^= 0x20
		if len(liveWireDiffs(b.oracleRec, bad)) == 0 {
			t.Fatal("a one-byte body mutation was not detected")
		}
	})
}

// TestLiveResponseMutationControl proves the final-output comparison is sensitive:
// when the server serves DIFFERENT bodies to the two legs, the structured outputs
// diverge. Each leg still sends exactly one request.
func TestLiveResponseMutationControl(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "1")

	server := newLiveCaptureServer(t)
	oracleSpy := &liveParseSpy{fn: debaml.Parse}
	oracle := newLiveOracleClient(t, loopbackOracleHTTPClient(), oracleSpy)
	nano := newLiveNativeClient(t, server.base())
	tc := dynFixtureByName(t, "single_user_message")

	// Oracle sees one answer, the native leg a DIFFERENT one.
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"one"}`))
	oracleData, oerr := runOracleLeg(t, oracle, server.base(), tc)
	if oerr != nil {
		t.Fatalf("oracle leg failed: %v", oerr)
	}
	if got := server.count(); got != 1 {
		t.Fatalf("oracle leg made %d requests, want 1", got)
	}

	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"two"}`))
	res, spy, ran, nerr := runNativeLeg(t, nano, server.base(), tc)
	if !ran || nerr != nil {
		t.Fatalf("native leg failed: ran=%v err=%v", ran, nerr)
	}
	if got := server.count(); got != 2 {
		t.Fatalf("native leg made %d total requests, want 2", got)
	}
	if res.Outcome != execute.OutcomeStructured || spy.calls != 1 {
		t.Fatalf("native outcome=%s calls=%d, want structured + 1", res.Outcome, spy.calls)
	}
	// The mutated response makes the final structured outputs differ.
	if liveJSONSemEqual(t, oracleData, res.Structured) {
		t.Fatalf("mutated response was not detected: oracle=%s native=%s",
			liveBodyDigest(oracleData), liveBodyDigest(res.Structured))
	}
}

// TestLiveNon2xxSkipsSAP: a deterministic 429 makes both legs fail as a provider
// error of the same status class, native SAP is NOT invoked, and neither leg
// makes a second request.
func TestLiveNon2xxSkipsSAP(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "1")
	b := runBothLegs(t, http.StatusTooManyRequests, openAIError("slow down", "rate_limit_error"), dynFixtureByName(t, "single_user_message"))

	// Oracle: a 429 surfaces as an llmhttp.HTTPError carrying the real status;
	// exactly one request; native SAP never touched. Diagnostics print only the
	// error TYPE and the status scalar — never the wrapped HTTPError.Error(),
	// which embeds the (bounded) provider response body the redaction fence keeps
	// out of failure logs.
	if b.oracleErr == nil {
		t.Fatalf("oracle leg must fail on a 429, got structured data %s", liveBodyDigest(b.oracleData))
	}
	var he *llmhttp.HTTPError
	if !errors.As(b.oracleErr, &he) {
		t.Fatalf("oracle error is not an *llmhttp.HTTPError (got %T)", b.oracleErr)
	}
	if he.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("oracle HTTPError status = %d, want 429", he.StatusCode)
	}
	if b.countAfterOracle != 1 {
		t.Errorf("oracle leg made %d requests, want 1 (no retry)", b.countAfterOracle)
	}

	// Native: provider-error outcome, no SAP, exactly one more request.
	if !b.ran || b.nativeErr != nil {
		t.Fatalf("native leg failed: ran=%v err=%v", b.ran, b.nativeErr)
	}
	if b.res.Outcome != execute.OutcomeProviderError {
		t.Fatalf("native outcome = %s, want provider-error", b.res.Outcome)
	}
	if b.res.SAPInvoked || b.spy.calls != 0 {
		t.Errorf("native SAP invoked on a 429 (SAPInvoked=%v calls=%d); it must be skipped", b.res.SAPInvoked, b.spy.calls)
	}
	if b.res.ProviderStatus != http.StatusTooManyRequests {
		t.Errorf("native provider status = %d, want 429", b.res.ProviderStatus)
	}
	if b.res.Translated == nil || b.res.Translated.Status != http.StatusTooManyRequests || !b.res.Translated.BodyIsJSON {
		t.Errorf("native translated envelope not a 429 JSON body")
	}
	if b.countAfterNative != 2 {
		t.Errorf("native leg made a second request (total %d), want 2", b.countAfterNative)
	}
	if b.oracleSpy.calls != 0 {
		t.Errorf("oracle native-parser spy fired %d times, want 0", b.oracleSpy.calls)
	}
}

// TestLiveInvalidBodySkipsSAP: a 200 with a non-JSON body maps to the native
// InvalidBody provider outcome (SAP NOT invoked) and fails the oracle parse; each
// leg makes exactly one request.
func TestLiveInvalidBodySkipsSAP(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "1")
	b := runBothLegs(t, http.StatusOK, []byte("<html>not json</html>"), dynFixtureByName(t, "single_user_message"))

	// Oracle: BAML-as-served sends, then baml-rest's own response extraction
	// rejects the non-JSON 2xx with a STABLE, body-free classifier — pinning this
	// as the expected invalid-2xx extraction path (not some other post-send
	// failure), exactly as the 429 control pins its HTTPError status. The
	// classifier names only the provider, never the raw body, and the facts below
	// are constants, so neither the assertion nor a failure diagnostic echoes the
	// provider body — the redaction fence stays intact. One request; no native SAP.
	if b.oracleErr == nil {
		t.Fatalf("oracle leg must fail on an invalid 2xx body, got data %s", liveBodyDigest(b.oracleData))
	}
	oerrMsg := b.oracleErr.Error()
	for _, fact := range []string{
		"failed to extract response content",
		"provider openai",
		"response body is not valid JSON",
	} {
		if !strings.Contains(oerrMsg, fact) {
			t.Errorf("oracle invalid-2xx error missing expected extraction fact %q (redacted: the classifier never echoes the provider body)", fact)
		}
	}
	if b.countAfterOracle != 1 {
		t.Errorf("oracle leg made %d requests, want 1", b.countAfterOracle)
	}

	// Native: InvalidBody outcome, no SAP, exactly one more request.
	if !b.ran || b.nativeErr != nil {
		t.Fatalf("native leg failed: ran=%v err=%v", b.ran, b.nativeErr)
	}
	if b.res.Outcome != execute.OutcomeInvalidBody {
		t.Fatalf("native outcome = %s, want invalid-body", b.res.Outcome)
	}
	if b.res.SAPInvoked || b.spy.calls != 0 {
		t.Errorf("native SAP invoked on an invalid 2xx (SAPInvoked=%v calls=%d); it must be skipped", b.res.SAPInvoked, b.spy.calls)
	}
	if b.res.ProviderStatus != http.StatusOK {
		t.Errorf("native provider status = %d, want the raw upstream 200", b.res.ProviderStatus)
	}
	if b.res.Translated == nil || b.res.Translated.Status != http.StatusBadGateway {
		t.Errorf("native translated status = %v, want the 502 InvalidBody envelope", b.res.Translated)
	}
	if b.countAfterNative != 2 {
		t.Errorf("native leg made a second request (total %d), want 2", b.countAfterNative)
	}
	if b.oracleSpy.calls != 0 {
		t.Errorf("oracle native-parser spy fired %d times, want 0", b.oracleSpy.calls)
	}
}
