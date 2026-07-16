package buildrequest

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// mustNotBuild is a build closure that fails the test if the orchestrator ever
// falls through to a BAML build/send — used to prove a claimed native outcome
// (success or failure) never triggers a hidden same-child BAML send.
func mustNotBuild(t *testing.T) BuildCallRequestFunc {
	return func(context.Context, string) (*llmhttp.Request, error) {
		t.Helper()
		t.Fatal("buildRequest must not run after a claimed native outcome (no hidden resend)")
		return nil, nil
	}
}

// TestNativeSeam_FailWithRawPropagatesDetailsRaw proves the Slice-6 neutral-seam
// extension #1: a FAILED native outcome carrying an owned raw diagnostic surfaces
// that raw as the error frame's details.raw (via newRawError -> rawFromError),
// exactly as the BAML extraction/parse-failure path does — so a post-claim
// translate/extract/SAP failure never loses the raw body. The typed error is
// preserved for the outer classifier (errors.Is still matches).
func TestNativeSeam_FailWithRawPropagatesDetailsRaw(t *testing.T) {
	sentinel := errors.New("native parse boom")
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return FailNativeCallWithRaw(sentinel, "the raw provider body")
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	if err := RunCallOrchestration(
		context.Background(), out, config, nil,
		mustNotBuild(t),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	); err != nil {
		t.Fatalf("errors surface as frames, not a Go error: %v", err)
	}
	close(out)

	final := nativeDrain(out)
	last := final[len(final)-1]
	if last.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error frame, got %v", last.Kind())
	}
	if last.Raw() != "the raw provider body" {
		t.Errorf("expected details.raw carried from the failed outcome, got %q", last.Raw())
	}
	if !errors.Is(last.Error(), sentinel) {
		t.Errorf("expected the typed error preserved for the outer classifier, got %v", last.Error())
	}
}

// TestNativeSeam_FailNilErrWithRawRetainsRaw proves a directly-constructed
// NativeCallFailed outcome with a NIL Err but a non-empty Raw still surfaces the
// owned raw as details.raw: the orchestrator normalizes the nil Err to the
// sentinel FIRST, then routes through newRawError, so the raw is never lost to an
// early sentinel return. (NativeCallOutcome is public + directly constructible.)
func TestNativeSeam_FailNilErrWithRawRetainsRaw(t *testing.T) {
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		// Malformed direct literal: NativeCallFailed, NIL Err, but raw set.
		return NativeCallOutcome{Disposition: NativeCallFailed, Raw: "the owned raw diagnostic"}
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	if err := RunCallOrchestration(
		context.Background(), out, config, nil,
		mustNotBuild(t),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	); err != nil {
		t.Fatalf("errors surface as frames, not a Go error: %v", err)
	}
	close(out)

	last := nativeDrain(out)
	frame := last[len(last)-1]
	if frame.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error frame, got %v", frame.Kind())
	}
	if frame.Raw() != "the owned raw diagnostic" {
		t.Errorf("details.raw = %q, want the owned raw retained even with a nil Err", frame.Raw())
	}
	if !errors.Is(frame.Error(), errNativeCallFailedNil) {
		t.Errorf("a nil-Err failure must normalize to the sentinel, got %v", frame.Error())
	}
}

// TestNativeSeam_FailWithEmptyRawStaysBare proves an empty raw on a failed
// outcome is byte-identical to FailNativeCall: the error frame carries the typed
// error and an EMPTY details.raw (newRawError no-ops on empty raw).
func TestNativeSeam_FailWithEmptyRawStaysBare(t *testing.T) {
	sentinel := errors.New("native transport boom")
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return FailNativeCallWithRaw(sentinel, "")
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	if err := RunCallOrchestration(
		context.Background(), out, config, nil,
		mustNotBuild(t),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	); err != nil {
		t.Fatalf("errors surface as frames, not a Go error: %v", err)
	}
	close(out)

	last := nativeDrain(out)
	frame := last[len(last)-1]
	if frame.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error frame, got %v", frame.Kind())
	}
	if frame.Raw() != "" {
		t.Errorf("expected empty details.raw for an empty-raw failure, got %q", frame.Raw())
	}
	if !errors.Is(frame.Error(), sentinel) {
		t.Errorf("expected the typed error preserved, got %v", frame.Error())
	}
}

// TestNativeSeam_WinnerEngineOverride proves the Slice-6 bounded success engine
// token: a succeeded outcome may override the default "native" marker (e.g.
// "native_baml_parse" when native owned the socket but BAML parse-only produced
// the final), and the token reaches the outcome metadata's winner_engine.
func TestNativeSeam_WinnerEngineOverride(t *testing.T) {
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		o := SucceedNativeCall("native_baml_parse final", "", "")
		o.WinnerEngine = "native_baml_parse"
		return o
	}}
	plan := &bamlutils.Metadata{Path: "buildrequest", Client: "OnlyClient", Provider: "openai"}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		MetadataPlan:         plan,
		NewMetadataResult:    newTestMetadataResult,
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	if err := RunCallOrchestration(
		context.Background(), out, config, nil,
		mustNotBuild(t),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	results := nativeDrain(out)
	_, outcome, _, _ := collectMetadataFrom(results, t)
	if outcome == nil {
		t.Fatal("expected an outcome metadata frame")
	}
	if outcome.WinnerEngine != "native_baml_parse" {
		t.Errorf("expected WinnerEngine=native_baml_parse, got %q", outcome.WinnerEngine)
	}
}

// TestNativeSeam_PlannedEngine proves planned_engine on the outcome frame: it is
// "native" when the serve installer set CallConfig.PlannedEngine and OMITTED
// (empty) otherwise, so the flag-off / default / shadow / BAML outcome JSON stays
// byte-identical to today.
func TestNativeSeam_PlannedEngine(t *testing.T) {
	run := func(t *testing.T, planned string) *bamlutils.Metadata {
		t.Helper()
		rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
			return SucceedNativeCall("native final", "", "")
		}}
		plan := &bamlutils.Metadata{Path: "buildrequest", Client: "OnlyClient", Provider: "openai"}
		out := make(chan bamlutils.StreamResult, 100)
		config := &CallConfig{
			Provider:             "openai",
			MetadataPlan:         plan,
			NewMetadataResult:    newTestMetadataResult,
			NativeAttempt:        rec.callback(),
			NativeAttemptEnabled: true,
			PlannedEngine:        planned,
		}
		if err := RunCallOrchestration(
			context.Background(), out, config, nil,
			mustNotBuild(t),
			identityParseFinal,
			ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
		); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		close(out)
		_, outcome, _, _ := collectMetadataFrom(nativeDrain(out), t)
		if outcome == nil {
			t.Fatal("expected an outcome metadata frame")
		}
		return outcome
	}

	t.Run("serve installer sets native", func(t *testing.T) {
		if got := run(t, "native").PlannedEngine; got != "native" {
			t.Errorf("expected PlannedEngine=native, got %q", got)
		}
	})
	t.Run("default path omits it", func(t *testing.T) {
		if got := run(t, "").PlannedEngine; got != "" {
			t.Errorf("expected PlannedEngine omitted on the default path, got %q", got)
		}
	})
}
